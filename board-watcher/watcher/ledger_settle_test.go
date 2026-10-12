package main

import (
	"os"
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
	if !l.settle("doc.md", a, consumed) {
		t.Fatal("own write of A did not match")
	}
	if !l.settle("doc.md", b, consumed) {
		t.Fatal("B, recorded after the settled A, must still match")
	}
	// The tombstone was retired by A's settle, and the earlier A was retired
	// rather than spent by a change, so a deletion or a further A now is a
	// peer's.
	if l.settle("doc.md", ledgerTombstone, consumed) {
		t.Fatal("the tombstone superseded by A's settle still matched a deletion")
	}
	if l.settle("doc.md", a, consumed) {
		t.Fatal("a retired A entry matched a later change")
	}
	// other.md is untouched by doc.md's settles.
	if !l.settle("other.md", a, consumed) {
		t.Fatal("other.md's own write was retired by doc.md's settles")
	}

	// No match retires everything on record for the file: a peer's state
	// settled after our entries.
	l2 := writeLedger(t,
		`{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-29T23:51:00Z"}`,
		`{"schema":1,"file":"doc.md","sha256":"-","ts":"2026-07-29T23:52:00Z"}`,
	)
	consumed2 := map[string]int{}
	if l2.settle("doc.md", b, consumed2) {
		t.Fatal("a peer's content matched")
	}
	if l2.settle("doc.md", a, consumed2) || l2.settle("doc.md", ledgerTombstone, consumed2) {
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
	l.retireAll(consumed)
	if l.settle("doc.md", a, consumed) || l.settle("gone.md", ledgerTombstone, consumed) {
		t.Fatal("an entry on record at baseline matched a later change")
	}
	appendLedgerLine(t, l, `{"schema":1,"file":"doc.md","sha256":"`+a+`","ts":"2026-07-30T00:00:00Z"}`)
	if !l.settle("doc.md", a, consumed) {
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
