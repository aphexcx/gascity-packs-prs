package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLedgerSettleTakesTheLatestMatchAndRetiresWhatItSupersedes is gf-jr0
// findings 3a/3b at the ledger level: a settle matches the LATEST unconsumed
// entry of its shape and retires every entry for the file recorded before
// it (an older own state the settled one supersedes); a settle that matches
// nothing (a peer's state, a touch) retires every entry for the file on
// record, since the state that settled came after all of them. Entries for
// other files, and entries recorded after the match, are untouched.
func TestLedgerSettleTakesTheLatestMatchAndRetiresWhatItSupersedes(t *testing.T) {
	a := strings.Repeat("aa", 32)
	b := strings.Repeat("bb", 32)
	l := writeLedger(t,
		`{"schema":1,"file":"doc.md","sha256":"-","ts":"2026-07-29T23:50:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:51:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:52:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"`+b+`","ts":"2026-07-29T23:53:00Z"}`,
		`{"schema":1,"file":"other.md","sha256":"`+a+`","ts":"2026-07-29T23:54:00Z"}`,
	)
	consumed := map[string]int{}

	// The settle of A takes the LATER A entry and retires the tombstone and
	// the earlier A; B, recorded after it, stays live and settles next.
	if !l.settle("doc.md", a, consumed, l.boundary()) {
		t.Fatal("own write of A did not match")
	}
	if !l.settle("doc.md", b, consumed, l.boundary()) {
		t.Fatal("B, recorded after the settled A, must still match")
	}
	// The tombstone was retired by A's settle, and the earlier A was retired
	// rather than spent by a change, so a deletion or a further A now is a
	// peer's.
	if l.settle("doc.md", ledgerTombstone, consumed, l.boundary()) {
		t.Fatal("the tombstone superseded by A's settle still matched a deletion")
	}
	if l.settle("doc.md", a, consumed, l.boundary()) {
		t.Fatal("a retired A entry matched a later change")
	}
	// other.md is untouched by doc.md's settles.
	if !l.settle("other.md", a, consumed, l.boundary()) {
		t.Fatal("other.md's own write was retired by doc.md's settles")
	}

	// No match retires everything on record for the file: a peer's state
	// settled after our entries.
	l2 := writeLedger(t,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:51:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"-","ts":"2026-07-29T23:52:00Z"}`,
	)
	consumed2 := map[string]int{}
	if l2.settle("doc.md", b, consumed2, l2.boundary()) {
		t.Fatal("a peer's content matched")
	}
	if l2.settle("doc.md", a, consumed2, l2.boundary()) || l2.settle("doc.md", ledgerTombstone, consumed2, l2.boundary()) {
		t.Fatal("entries superseded by a peer's settled state still matched")
	}
}

// TestLedgerRetireAllCoversEveryEntryOnRecord: the baseline's retirement
// spends every entry present, for every file, and leaves entries appended
// afterwards live.
func TestLedgerRetireAllCoversEveryEntryOnRecord(t *testing.T) {
	a := strings.Repeat("aa", 32)
	l := writeLedger(t,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:51:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:51:00Z"}`,
		`{"schema":1,"file":"gone.md","sha256":"-","ts":"2026-07-29T23:52:00Z"}`,
	)
	consumed := map[string]int{}
	l.retireAll(consumed, l.boundary())
	if l.settle("doc.md", a, consumed, l.boundary()) || l.settle("gone.md", ledgerTombstone, consumed, l.boundary()) {
		t.Fatal("an entry on record at baseline matched a later change")
	}
	appendLedgerLine(t, l, `{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-30T00:00:00Z"}`)
	if !l.settle("doc.md", a, consumed, l.boundary()) {
		t.Fatal("an entry appended after the baseline did not match")
	}
}

func appendLedgerLine(t *testing.T, l *ledger, line string) {
	t.Helper()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

// TestLedgerOffsetsCountCRLF is the lane's codex round 5: a writer that ends
// lines with CRLF must not shift the byte offsets the boundary rule reads,
// or an entry appended after the boundary could appear before it.
func TestLedgerOffsetsCountCRLF(t *testing.T) {
	a := strings.Repeat("aa", 32)
	b := strings.Repeat("bb", 32)
	path := filepath.Join(t.TempDir(), "own-writes.jsonl")
	first := `{"schema":1,"file":"doc.md","sha256":"` + a + `","ts":"2026-07-29T23:51:00Z"}` + "\r\n"
	if err := os.WriteFile(path, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &ledger{path: path}
	boundary := l.boundary()
	appendLedgerLine(t, l, `{"schema":1,"file":"doc.md","sha256":"`+b+`","ts":"2026-07-29T23:52:00Z"}`)
	consumed := map[string]int{}
	// A peer's state settles: the pre-boundary A is retired, the post-boundary
	// B (an own write landing mid-cycle) stays live.
	if l.settle("doc.md", strings.Repeat("cc", 32), consumed, boundary) {
		t.Fatal("a peer's content matched")
	}
	if !l.settle("doc.md", b, consumed, l.boundary()) {
		t.Fatal("the entry appended after a CRLF line was retired: its offset was undercounted")
	}
}

// TestLedgerShrunkSinceBoundaryRetiresNothing: a ledger pruned after the
// boundary was captured no longer maps the boundary to entries, so a settle
// retires nothing rather than the wrong entries.
func TestLedgerShrunkSinceBoundaryRetiresNothing(t *testing.T) {
	a := strings.Repeat("aa", 32)
	l := writeLedger(t,
		`{"schema":1,"file":"old.md","sha256":"`+a+`","ts":"2026-07-29T23:50:00Z"}`,
		`{"schema":1,"file":"old.md","sha256":"`+a+`","ts":"2026-07-29T23:51:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:52:00Z"}`,
	)
	boundary := l.boundary()
	// Pruned: the old entries are gone, doc.md's entry now starts before the
	// captured boundary although it was recorded after the board read.
	if err := os.WriteFile(l.path, []byte(`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:52:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	consumed := map[string]int{}
	if l.settle("doc.md", strings.Repeat("cc", 32), consumed, boundary) {
		t.Fatal("a peer's content matched")
	}
	if !l.settle("doc.md", a, consumed, l.boundary()) {
		t.Fatal("an entry of a pruned ledger was retired against a stale boundary")
	}
}
