package ack

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// An acknowledgement marker matters only within MaxAge of its message, so a
// start (and the daily pass) removes markers older than one day. Only empty
// files named by 64 hex characters are markers; nothing else is touched.
func TestOldAckMarkersArePruned(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ack-attempts")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)
	old, fresh := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for name, age := range map[string]time.Duration{old: 25 * time.Hour, fresh: 23 * time.Hour, "stray": 48 * time.Hour, strings.Repeat("c", 64): 48 * time.Hour} {
		path := filepath.Join(root, name)
		body := []byte(nil)
		if name == strings.Repeat("c", 64) {
			body = []byte("not empty")
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	watch := filepath.Join(dir, "stopwatch-2026-10.jsonl")
	if err := os.WriteFile(watch, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(watch, now.Add(-60*24*time.Hour), now.Add(-60*24*time.Hour)); err != nil {
		t.Fatal(err)
	}

	PruneAttempts(dir, now)

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	sort.Strings(left)
	want := []string{fresh, strings.Repeat("c", 64), "stray"}
	sort.Strings(want)
	if strings.Join(left, ",") != strings.Join(want, ",") {
		t.Fatalf("after a start: %v, want %v", left, want)
	}
	if _, err := os.Stat(watch); err != nil {
		t.Fatalf("stopwatch file touched: %v", err)
	}
	// A state directory without markers is not an error and is not created.
	PruneAttempts(t.TempDir(), now)
}
