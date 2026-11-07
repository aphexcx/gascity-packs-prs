package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
)

func TestLockStateChecksIdentityImmediatelyAfterAcquiring(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	lock, err := lockState(path, func() {
		if err := os.Rename(path+".lock", path+".lock.old"); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path+".lock", "another owner\n")
	})
	if lock != nil {
		lock.Close()
		t.Fatal("acquired a replaced lock path")
	}
	// Pin the acquisition check itself: RecordSocket's later Check also refuses
	// a replacement, but reports ErrStateLockLost after constructing an owner.
	if err == nil || err.Error() != "state lock path changed or is not a regular file" {
		t.Fatalf("post-acquisition identity check did not refuse: %v", err)
	}
	data, err := os.ReadFile(path + ".lock")
	if err != nil || string(data) != "another owner\n" {
		t.Fatalf("replacement owner metadata changed: %q, %v", data, err)
	}
	old, err := openStateLock(path + ".lock.old")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := tryStateLock(old); err != nil {
		t.Fatalf("failed acquisition leaked the original lock: %v", err)
	}
}

func TestStateHandlerRefusesLostLockBeforeRecovery(t *testing.T) {
	f := newFixture(t, false)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Notify: func(context.Context, string, string) error { return nil }})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, ErrStateLockLost) {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not shut down after lock loss")
		}
	})
	f.health(t, "needs-sign-in", 3*time.Second)
	path := f.cfg.Endpoints.StateFile
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".lock", path+".lock.old"); err != nil {
		t.Fatal(err)
	}
	// Ensure the server's ordinary recovery-state check would allow this request;
	// otherwise its unrelated 409 could hide a missing ownership pre-check.
	f.health(t, "recovery-required", 3*time.Second)
	resp, err := f.client.Post("http://adapter/state", "application/json", strings.NewReader(`{"action":"reset"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// A ledger SaveGuard failure returns 500 after attempting recovery. The
	// handler must reject ownership loss before that path with its own 409.
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("state pre-check status = %d, want 409", resp.StatusCode)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("refused recovery changed the ledger: %v", err)
	}
}

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
