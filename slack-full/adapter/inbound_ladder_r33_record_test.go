package main

import (
	"path/filepath"
	"testing"
	"time"
)

// The verdict record carries the charged count and the fold orders by
// it (r33 finding 2): the second charge writes its own record, and the
// replay's fold prefers the higher count at equal rank whatever the
// records' order or timestamps. Needs the record's new field, so it does
// not compile at the previous head; TestEveryChargeIsPersisted is its
// RED-provable half.
func TestVerdictRecordCarriesTheRetryCount(t *testing.T) {
	spool := newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
	c := newInboundCoalescer(time.Hour, nil)
	c.spill = spool.spillBatch
	c.recordVerdict = spool.recordVerdict
	c.charge("C1", testPending("C1", "1.0", "unvouched twice"), errDeliveryUnvouched)
	c.mu.Lock()
	restored := append([]pendingChannelInbound(nil), c.pending["C1"]...)
	delete(c.pending, "C1")
	c.mu.Unlock()
	c.charge("C1", restored[0], errDeliveryUnvouched)
	records := readRecords(spool.path)
	var newest *spooledInbound
	counts := 0
	for i := range records {
		e := records[i]
		if e.VerdictTS != "1.0" {
			continue
		}
		counts++
		if newest == nil || supersedes(e, *newest) {
			newest = &records[i]
		}
	}
	if counts != 2 {
		t.Fatalf("each charge writes its own record, got %d record(s)", counts)
	}
	if newest.VerdictAttempts != 2 || verdictOf(*newest).attempts != 2 {
		t.Fatalf("the folded record must carry the ledger's count 2, got %+v", *newest)
	}
	// The fold is by count, not by position or time: an older-looking
	// record with the higher count still wins.
	a := spooledInbound{Channel: "C1", VerdictTS: "1.0", VerdictOutstanding: true, VerdictAttempts: 2, VerdictAt: 10}
	b := spooledInbound{Channel: "C1", VerdictTS: "1.0", VerdictOutstanding: true, VerdictAttempts: 1, VerdictAt: 20}
	if !supersedes(a, b) || supersedes(b, a) {
		t.Fatal("at equal rank the higher charged count supersedes, whatever the timestamps")
	}
}
