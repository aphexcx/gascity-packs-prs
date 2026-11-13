package stopwatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Record decides (dedup, thread normalization) under its lock and hands the
// file work to the writer; a held disk never delays the caller.
func TestRecordNeverWaitsOnTheDisk(t *testing.T) {
	dir := t.TempDir()
	w := New(dir, "slack", "release-a", nil)
	defer w.Close()
	hold, entered := make(chan struct{}), make(chan struct{}, 4)
	w.beforeWrite = func() { entered <- struct{}{}; <-hold }
	at := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)
	start := time.Now()
	w.Record(Event{Event: "inbound", Conversation: "DEXAMPLE", Message: "1700000000.000100", MessageAt: at, At: at})
	<-entered
	w.Record(Event{Event: "inbound", Conversation: "DEXAMPLE", Message: "1700000000.000100", MessageAt: at, At: at})
	w.Record(Event{Event: "words", Conversation: "DEXAMPLE", Message: "1700000000.000200", Thread: "1700000000.000100", At: at})
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Fatalf("Record waited %s on a held disk write", elapsed)
	}
	close(hold)
	w.Flush()
	data, err := os.ReadFile(filepath.Join(dir, "stopwatch-2026-11.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"event":"inbound"`) || !strings.Contains(lines[1], `"event":"words"`) {
		t.Fatalf("lines after Flush: %q", lines)
	}
}

// A release that cannot be hashed is recorded as "unknown" with one warning,
// and the stopwatch still starts and records.
func TestReleaseUnknownStartsWithOneWarning(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	log := func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }
	missing := filepath.Join(t.TempDir(), "no-such-executable")
	if got := ReleaseOf(func() (string, error) { return missing, nil }, log); got != "unknown" {
		t.Fatalf("release of a missing file = %q, want unknown", got)
	}
	if got := ReleaseOf(func() (string, error) { return "", errors.New("no executable") }, log); got != "unknown" {
		t.Fatalf("release without an executable = %q, want unknown", got)
	}
	exe := filepath.Join(t.TempDir(), "adapter")
	if err := os.WriteFile(exe, []byte("bytes"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := ReleaseOf(func() (string, error) { return exe, nil }, log); len(got) != 12 || got == "unknown" {
		t.Fatalf("release of a readable file = %q", got)
	}
	mu.Lock()
	if len(logs) != 2 || logs[0] != "stopwatch: release unavailable; recorded as unknown" || logs[1] != logs[0] {
		t.Fatalf("logs = %q, want one warning per failed start", logs)
	}
	mu.Unlock()
	dir := t.TempDir()
	w := New(dir, "gchat", ReleaseOf(func() (string, error) { return missing, nil }, nil), nil)
	at := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)
	w.Record(Event{Event: "inbound", Conversation: "spaces/example", Message: "spaces/example/messages/one", MessageAt: at, At: at})
	w.Close()
	data, err := os.ReadFile(filepath.Join(dir, "stopwatch-2026-11.jsonl"))
	if err != nil || !strings.Contains(string(data), `"release":"unknown"`) {
		t.Fatalf("inbound line: %q, %v", data, err)
	}
}
