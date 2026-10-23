package app

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
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
	err = Run(t.Context(), f.cfg)
	var busy *StateLockedError
	if !errors.As(err, &busy) || busy.Socket != f.cfg.Secrets.ServiceSocket {
		t.Fatalf("second service: %v", err)
	}
	after, err := os.Lstat(f.cfg.Secrets.ServiceSocket)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("owner socket changed: %v", err)
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
