//go:build unix

package ledger_test

import (
	"bytes"
	"errors"
	"gchat-adapter/internal/ledger"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCorruptCopyWriteFailureRemovesPartialFile(t *testing.T) {
	if runInSubprocess(t) {
		return
	}
	path := filepath.Join(t.TempDir(), "state.json")
	contents := bytes.Repeat([]byte("invalid JSON "), 32)
	writeFile(t, path, contents)
	limitFileSize(t)
	got, err := ledger.Load(path, true)
	if got != nil || !errors.Is(err, ledger.ErrRecoveryRequired) || !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("load with failed copy write = %v, %v", got, err)
	}
	var writeErr *os.PathError
	if !errors.As(err, &writeErr) || writeErr.Op != "write" || !strings.HasPrefix(writeErr.Path, path+".corrupt-") {
		t.Fatalf("expected a corrupt-copy write failure: %v", err)
	}
	if !strings.Contains(err.Error(), writeErr.Path) {
		t.Errorf("error lacks failed copy path %s: %v", writeErr.Path, err)
	}
	var recovery *ledger.RecoveryError
	if !errors.As(err, &recovery) || recovery.PreservedPath != "" {
		t.Errorf("failed copy claimed preserved evidence: %v", err)
	}
	assertAbsent(t, writeErr.Path)
	if !bytes.Equal(readFile(t, path), contents) {
		t.Fatal("failed copy changed the corrupt source")
	}
}

func TestSaveWriteFailureRemovesPartialFile(t *testing.T) {
	if runInSubprocess(t) {
		return
	}
	path := filepath.Join(t.TempDir(), "state.json")
	l := ledger.New()
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	old := readFile(t, path)
	l.Counters["turns"] = 1
	limitFileSize(t)
	err := l.Save(path)
	var writeErr *os.PathError
	if !errors.Is(err, syscall.EFBIG) || !errors.As(err, &writeErr) || writeErr.Op != "write" || writeErr.Path != path+".tmp" {
		t.Fatalf("expected a temporary-file write failure: %v", err)
	}
	assertAbsent(t, path+".tmp")
	if !bytes.Equal(readFile(t, path), old) {
		t.Fatal("failed write changed the committed file")
	}
}

// File-size limits and signal handling must stay out of the main test process.
func runInSubprocess(t *testing.T) bool {
	t.Helper()
	const env = "GCHAT_LEDGER_WRITE_FAILURE_TEST"
	if os.Getenv(env) == t.Name() {
		return false
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), env+"="+t.Name(), "GORACE="+strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("write-failure subprocess: %v\n%s", err, output)
	}
	return true
}

func limitFileSize(t *testing.T) {
	t.Helper()
	var original syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
		t.Fatal(err)
	}
	// Permit a short write, then force EFBIG instead of terminating the process.
	signal.Ignore(syscall.SIGXFSZ)
	limit := original
	limit.Cur = 16
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &original); err != nil {
			t.Error(err)
		}
	})
}
