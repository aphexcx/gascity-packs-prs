package app

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
)

func TestStateLockExcludesSecondServiceBeforeSocketProbe(t *testing.T) {
	f := newFixture(t, false)
	lock, err := LockState(f.cfg.Endpoints.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lock.RecordSocket(f.cfg.Secrets.ServiceSocket); err != nil {
		t.Fatal(err)
	}
	// No listener is accepting: even a refused connection must not let another
	// service start against this ledger or remove the owner's socket.
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: f.cfg.Secrets.ServiceSocket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	before, _ := os.Lstat(f.cfg.Secrets.ServiceSocket)
	err = RunWithOptions(t.Context(), f.cfg, Options{Notify: func(context.Context, string, string) error { return nil }})
	var busy *StateLockedError
	if !errors.As(err, &busy) || busy.Socket != f.cfg.Secrets.ServiceSocket {
		t.Fatalf("second service: %v", err)
	}
	if strings.Contains(err.Error(), "--force") {
		t.Fatalf("serve suggested unsupported force: %v", err)
	}
	after, err := os.Lstat(f.cfg.Secrets.ServiceSocket)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("owner socket changed: %v", err)
	}
}

func TestRemoveSocketPreservesDifferentSocketInode(t *testing.T) {
	f := newFixture(t, false)
	path := f.cfg.Secrets.ServiceSocket
	first, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	first.SetUnlinkOnClose(false)
	defer first.Close()
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	second, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := RemoveSocket(path, before); err == nil {
		t.Fatal("different socket inode was removed")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestLostStateLockBlocksShutdownSaveAndStaysDegraded(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "replaced"}[replacement], func(t *testing.T) {
			f := newFixture(t, false)
			var logs logBuffer
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Notify: func(context.Context, string, string) error { return nil }})
			}()
			stopped := false
			t.Cleanup(func() {
				cancel()
				if !stopped {
					<-done
				}
			})
			f.health(t, "needs-sign-in", 3*time.Second)
			path := f.cfg.Endpoints.StateFile + ".lock"
			if err := os.Rename(path, path+".old"); err != nil {
				t.Fatal(err)
			}
			if replacement {
				writeFile(t, path, "replacement")
			}
			h := f.health(t, "recovery-required", time.Second)
			if !strings.Contains(h.Reason, "lock") {
				t.Fatalf("health does not name lost lock: %+v", h)
			}
			// An offline reset after lock loss must survive all later service saves.
			fresh := ledger.New()
			fresh.Counters["offline-reset"] = 9
			if err := fresh.Save(f.cfg.Endpoints.StateFile); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(f.cfg.Endpoints.StateFile)
			// Returning the original inode cannot restore this process's ownership.
			if replacement {
				if err := os.Rename(path, path+".replacement"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(path+".old", path); err != nil {
				t.Fatal(err)
			}
			f.replaceToken(t)
			time.Sleep(250 * time.Millisecond)
			f.health(t, "recovery-required", time.Second)
			cancel()
			<-done
			stopped = true
			after, _ := os.ReadFile(f.cfg.Endpoints.StateFile)
			if string(before) != string(after) {
				t.Fatal("shutdown undid offline reset after lock loss")
			}
			if got := strings.Count(logs.String(), "state lock ownership lost"); got != 1 {
				t.Fatalf("lock-loss logs=%d: %s", got, logs.String())
			}
		})
	}
}

func TestRemoveSocketPreservesReplacement(t *testing.T) {
	f := newFixture(t, false)
	path := f.cfg.Secrets.ServiceSocket
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	defer ln.Close()
	before, _ := os.Lstat(path)
	if err := os.Rename(path, filepath.Join(filepath.Dir(path), "old.sock")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "replacement")
	if err := RemoveSocket(path, before); err == nil {
		t.Fatal("replacement accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement removed: %v", err)
	}
}
