package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLedger(t *testing.T, lines ...string) *ledger {
	t.Helper()
	path := filepath.Join(t.TempDir(), "own-writes.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &ledger{path: path}
}

func TestLedgerMatches(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	l := writeLedger(t,
		`{"schema":1,"file":"note.md","sha256":"`+sha+`","ts":"2026-07-29T23:55:00Z"}`,
		`{"schema":1,"file":"gone.md","sha256":"-","ts":"2026-07-29T23:56:00Z"}`,
	)
	consumed := map[string]int{}
	if !l.consume("note.md", sha, consumed) {
		t.Error("recorded own write did not match")
	}
	if !l.consume("gone.md", ledgerTombstone, consumed) {
		t.Error("recorded tombstone did not match")
	}
	if l.consume("note.md", strings.Repeat("cd", 32), consumed) {
		t.Error("different hash matched")
	}
	if l.consume("other.md", sha, consumed) {
		t.Error("same hash under a different file matched")
	}
}

// TestLedgerEntrySuppressesExactlyOneChange is gf-4c9 row 8 at the ledger
// level: an entry is consumed by the change it explains and never matches
// again; two identical entries (two own writes of the same bytes) suppress
// two changes; the counts round-trip through the caller's map.
func TestLedgerEntrySuppressesExactlyOneChange(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	l := writeLedger(t,
		`{"schema":1,"file":"gone.md","sha256":"-","ts":"2026-07-29T23:56:00Z"}`,
		`{"schema":1,"file":"twice.md","sha256":"`+sha+`","ts":"2026-07-29T23:57:00Z"}`,
		`{"schema":1,"file":"twice.md","sha256":"`+sha+`","ts":"2026-07-29T23:57:00Z"}`,
	)
	consumed := map[string]int{}
	if !l.consume("gone.md", ledgerTombstone, consumed) {
		t.Fatal("first match of the tombstone")
	}
	if l.consume("gone.md", ledgerTombstone, consumed) {
		t.Fatal("a consumed tombstone matched a second deletion")
	}
	for i := 1; i <= 2; i++ {
		if !l.consume("twice.md", sha, consumed) {
			t.Fatalf("identical entry %d did not match", i)
		}
	}
	if l.consume("twice.md", sha, consumed) {
		t.Fatal("two identical entries suppressed a third change")
	}
	// A new entry appended later (the write helper's next own write) is
	// unconsumed and matches again.
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema":1,"file":"gone.md","sha256":"-","ts":"2026-07-30T00:00:00Z"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if !l.consume("gone.md", ledgerTombstone, consumed) {
		t.Fatal("a fresh tombstone with a new ts did not match")
	}
}

func TestLedgerToleratesGarbageLines(t *testing.T) {
	sha := strings.Repeat("ef", 32)
	l := writeLedger(t,
		"not json at all",
		"",
		`{"schema":1,"file":"ok.md","sha256":"`+sha+`","ts":"2026-07-29T23:55:00Z"}`,
	)
	if !l.consume("ok.md", sha, map[string]int{}) {
		t.Error("valid line after garbage did not match")
	}
}

func TestLedgerMissingFile(t *testing.T) {
	l := &ledger{path: filepath.Join(t.TempDir(), "absent.jsonl")}
	if l.consume("any.md", strings.Repeat("00", 32), map[string]int{}) {
		t.Error("missing ledger matched something")
	}
}
