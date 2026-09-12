package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- codex r33 ---------------------------------------------------------------
//
// The charged count is ledger state (r33 findings 2 and 4): every
// increment is a progression that owes its own record and its copy's
// spool line, the count never regresses, and the journal folds owned
// copies by it. A reaction is never the ladder's (r33 finding 3).

// Every charge is persisted (r33 finding 2): the first charge owned the
// message and wrote its copy with Attempts=1, but the second only
// incremented the count in memory — ownership had not progressed by
// rank, so restore spooled nothing — and a crash restored the earlier
// count, giving the bounded ladder one more identical retry.
func TestEveryChargeIsPersisted(t *testing.T) {
	spool := newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
	c := newInboundCoalescer(time.Hour, nil)
	c.spill = spool.spillBatch
	c.recordVerdict = spool.recordVerdict
	c.charge("C1", testPending("C1", "1.0", "unvouched twice"), errDeliveryUnvouched)
	c.mu.Lock()
	restored := append([]pendingChannelInbound(nil), c.pending["C1"]...)
	delete(c.pending, "C1")
	c.mu.Unlock()
	if len(restored) != 1 || restored[0].attempts != 1 {
		t.Fatalf("setup: the first charge must leave one copy charged once, got %+v", restored)
	}
	c.charge("C1", restored[0], errDeliveryUnvouched)
	entries, err := readSpoolLines(spool.path, false)
	if err != nil {
		t.Fatal(err)
	}
	most := 0
	for _, e := range entries {
		if e.VerdictTS == "" && e.DeletedTS == "" && e.Inbound.ProviderMessageID == "1.0" && e.Attempts > most {
			most = e.Attempts
		}
	}
	if most != 2 {
		t.Fatalf("the second charge must spool the copy with its count: the journal's furthest copy is charged %d time(s), want 2", most)
	}
}

// A charged reaction line seeds no ownership (r33 finding 3): the ladder
// never owns a reaction (onLadderEntry), but a line written with its
// count — a reaction charged by an unvouched batch and spooled at
// shutdown — seeded an outstanding verdict the reaction's admission and
// delivery never settled, so owesDurability stayed true, the staged file
// was retained, and the delivered reaction replayed at every restart.
func TestChargedReactionReplaysUnowned(t *testing.T) {
	spool := newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
	r := testPending("C1", "9.0", "reaction")
	spool.mu.Lock()
	err := spool.appendLinesLocked([]spooledInbound{{Channel: "C1", Reaction: true, Inbound: r.inbound, Attempts: 1}})
	spool.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	c := newInboundCoalescer(time.Hour, nil)
	c.spill = spool.spillBatch
	c.recordVerdict = spool.recordVerdict
	c.deliver = func(string, []pendingChannelInbound) error { return nil }
	spool.replayInto(c)
	if c.onLadder("C1", "9.0") {
		t.Fatal("a reaction is never the rejection ladder's, whatever count its line carries")
	}
	if c.owesDurability() {
		t.Fatal("a replayed charged reaction must not leave the coalescer owing durability for an ownership nothing settles")
	}
	c.mu.Lock()
	n := len(c.reactions["C1"])
	c.mu.Unlock()
	if n != 1 {
		t.Fatalf("the reaction must be admitted to its side lane once, got %d", n)
	}
}

// The in-place compaction keeps the message's FURTHEST copy (r33 finding
// 4): the newest line was kept, so a later, less charged duplicate — a
// plain copy that adopted isolation with a zero count and was appended
// after the charged copy — replaced it, and a crash restored the message
// with its budget reset.
func TestInPlaceCompactionKeepsTheMostProgressedCopy(t *testing.T) {
	spool := newInboundSpool(filepath.Join(t.TempDir(), strings.Repeat("s", 250))) // forces the in-place fallback
	plain := testPending("C1", "1.0", "A")
	charged := plain
	charged.attempts = 1
	probe := plain
	probe.isolate = true
	for _, batch := range [][]pendingChannelInbound{{plain}, {charged}} {
		if !spool.spillBatch("C1", batch) {
			t.Fatal("spill must confirm")
		}
	}
	if !spool.recordVerdict("C1", "1.0", ownershipOf(charged)) {
		t.Fatal("record must confirm")
	}
	if !spool.spillBatch("C1", []pendingChannelInbound{probe}) { // later in the file, less progressed
		t.Fatal("spill must confirm")
	}
	spool.mu.Lock()
	spool.compactToRecordsLocked()
	spool.mu.Unlock()
	entries, err := readSpoolLines(spool.path, false)
	if err != nil {
		t.Fatal(err)
	}
	copies := 0
	for _, e := range entries {
		if e.VerdictTS != "" || e.DeletedTS != "" {
			continue
		}
		copies++
		if e.Attempts != 1 {
			t.Fatalf("the compaction kept a less charged copy (attempts=%d isolate=%v) over the ladder's furthest one", e.Attempts, e.Isolate)
		}
	}
	if copies != 1 {
		t.Fatalf("exactly one copy of the message is kept, got %d", copies)
	}
}
