//go:build unix

package puller

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/pubsub/v2"
)

// Descriptor inspection is serial and disables GC so finalizers cannot hide a
// missing explicit Close. HTTP and gRPC handles are ignored by inode identity.
func directoryDescriptors(t *testing.T, path string) []string {
	t.Helper()
	var info unix.Stat_t
	err := unix.Stat(path, &info)
	if err != nil {
		t.Fatal(err)
	}
	fdDir := "/proc/self/fd"
	if _, err := os.Stat(fdDir); os.IsNotExist(err) {
		fdDir = "/dev/fd"
	}
	entries, err := os.ReadDir(fdDir)
	if os.IsNotExist(err) {
		t.Skip("descriptor enumeration unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	var descriptors []string
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		var opened unix.Stat_t
		if unix.Fstat(fd, &opened) == nil && info.Dev == opened.Dev && info.Ino == opened.Ino {
			descriptors = append(descriptors, entry.Name())
		}
	}
	return descriptors
}

type quarantineFailureLogger struct {
	*recordingLogger
	failed chan struct{}
}

func (l *quarantineFailureLogger) Error(message string, args ...any) {
	l.recordingLogger.Error(message, args...)
	if message == "quarantine failed" {
		l.failed <- struct{}{}
	}
}

func TestRunRotationsCloseAndReuseQuarantineHandles(t *testing.T) {
	previousGCPercent := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGCPercent)
	f := newFixture(t)
	base := quarantineTempDir(t)
	t.Chdir(base)
	quarantine := filepath.Join(base, "quarantine")
	recorded := make(chan string, 1)
	log := &quarantineFailureLogger{recordingLogger: &recordingLogger{}, failed: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan error, 1)
	// A failed first delivery is acked on redelivery; it cannot interfere with
	// subsequent descriptor observations or leak an indefinitely nacked fixture.
	failed := map[string]bool{}
	go func() {
		done <- Run(ctx, f.sub, func(_ context.Context, ev *Event) (Outcome, error) {
			if strings.Contains(ev.Changes[0].Message.Name, "/fail-") {
				if failed[ev.PubSubID] {
					return Ack, nil
				}
				failed[ev.PubSubID] = true
				ev.PubSubID = strings.Repeat("x", 300) // rename exceeds NAME_MAX after open succeeds
			}
			return Quarantine, nil
		}, log, RunOptions{MediaDir: "inbound", RecordQuarantine: func(_ context.Context, _ *Event, path string, _ error) error { recorded <- path; return nil }})
	}()
	stopped := false
	defer func() {
		cancel()
		if !stopped {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	}()
	publish := func(name string, succeeds bool) {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"message": map[string]string{"name": "spaces/room/messages/" + name}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.publisher.Publish(ctx, &pubsub.Message{Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created"}}).Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if succeeds {
			select {
			case path := <-recorded:
				data, err := os.ReadFile(path)
				if err != nil || string(data) != string(raw) {
					t.Fatalf("recorded path=%q data=%q err=%v", path, data, err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		} else {
			select {
			case <-log.failed:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	publish("initial", true)
	// Recovery must keep using the startup path after a process cwd change.
	t.Chdir(quarantineTempDir(t))
	rotations := 30
	// Keep the normal/race descriptor stress while preserving the short suite's
	// budget; three rotations still exercise recovery and reuse in short mode.
	if testing.Short() {
		rotations = 3
	}
	for i := 0; i < rotations; i++ {
		retained := filepath.Join(base, fmt.Sprintf("retained-%d", i))
		if err := os.Rename(quarantine, retained); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(quarantine, 0700); err != nil {
			t.Fatal(err)
		}
		publish(fmt.Sprintf("fail-%d", i), false)
		if got := directoryDescriptors(t, quarantine); len(got) != 0 {
			t.Fatalf("failed replacement leaked handles: %v", got)
		}
		if got := directoryDescriptors(t, retained); len(got) != 1 {
			t.Fatalf("failed replacement changed cache: %v", got)
		}
		publish(fmt.Sprintf("recover-%d", i), true)
		if got := directoryDescriptors(t, retained); len(got) != 0 {
			t.Fatalf("superseded handle leaked: %v", got)
		}
		cached := directoryDescriptors(t, quarantine)
		if len(cached) != 1 {
			t.Fatalf("replacement cache handles: %v", cached)
		}
		publish(fmt.Sprintf("reuse-%d", i), true)
		if got := directoryDescriptors(t, quarantine); !reflect.DeepEqual(got, cached) {
			t.Fatalf("second write reopened cache: %v -> %v", cached, got)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	stopped = true
	if got := directoryDescriptors(t, quarantine); len(got) != 0 {
		t.Fatalf("final cached handle leaked: %v", got)
	}
}
