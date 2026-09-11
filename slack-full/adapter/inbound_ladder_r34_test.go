package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// --- codex r34 finding 2 ------------------------------------------------------
//
// The replay folds the ledger by rank, then charged count, then time —
// the one order every fold uses — and a stripped disposition seeds its
// count. Either gap let an earlier plain copy adopt a budget one step
// behind and take one identical retry past row 4's bound.

func replayIntoFreshCoalescer(t *testing.T, spool *inboundSpool) *inboundCoalescer {
	t.Helper()
	c := newInboundCoalescer(time.Hour, nil)
	c.spill = spool.spillBatch
	c.recordVerdict = spool.recordVerdict
	c.deliver = func(string, []pendingChannelInbound) error { return nil }
	spool.replayInto(c)
	return c
}

func ledgerCount(t *testing.T, c *inboundCoalescer, channel, ts string) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.verdicts[channel][ts]
	if !ok {
		t.Fatalf("the ledger must own %s/%s after the replay", channel, ts)
	}
	return v.attempts
}

func leastChargedCopy(t *testing.T, c *inboundCoalescer, channel, ts string) int {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	least, copies := 1<<30, 0
	for _, p := range c.pending[channel] {
		if p.inbound.ProviderMessageID != ts {
			continue
		}
		copies++
		if p.attempts < least {
			least = p.attempts
		}
	}
	if copies == 0 {
		t.Fatalf("a copy of %s/%s must be buffered after the replay", channel, ts)
	}
	return least
}

// Two outstanding records of one message: the NEWER carries the lower
// count (its charge was recorded before the second charge's record
// landed in a later spool). The fold must keep the count-2 record,
// and the plain copy admitted after the seeding adopts count 2.
func TestReplayFoldsRecordsByCount(t *testing.T) {
	spool := newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
	now := time.Now()
	for _, v := range []ladderVerdict{
		{cause: errors.New("unvouched"), attempts: 2, at: now.Add(-2 * time.Minute)},
		{cause: errors.New("unvouched"), attempts: 1, at: now.Add(-time.Minute)},
	} {
		if !spool.recordVerdict("C1", "1.0", v) {
			t.Fatal("record must confirm")
		}
	}
	if !spool.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "A")}) {
		t.Fatal("spill must confirm")
	}
	c := replayIntoFreshCoalescer(t, spool)
	if got := ledgerCount(t, c, "C1", "1.0"); got != 2 {
		t.Fatalf("the replay must fold the records by count: ledger count %d, want 2", got)
	}
	if got := leastChargedCopy(t, c, "C1", "1.0"); got != 2 {
		t.Fatalf("the admitted copy must adopt the ledger's count: %d, want 2", got)
	}
}

// A stripped disposition line carries the message's count (2) while
// the only record says 1 (a crash between the second charged copy's
// spill and its record); the plain copy staged AHEAD of the stripped
// line must adopt count 2 at its admission, not 1.
func TestReplaySeedsTheStrippedCount(t *testing.T) {
	spool := newInboundSpool(filepath.Join(t.TempDir(), "spool.jsonl"))
	if !spool.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "A")}) {
		t.Fatal("spill must confirm")
	}
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{stripped: true, cause: errors.New("422"), attempts: 1, at: time.Now()}) {
		t.Fatal("record must confirm")
	}
	stripped := testPending("C1", "1.0", "A (attachments withheld)")
	stripped.stripped, stripped.isolate, stripped.attempts = "422", true, 2
	if !spool.spillBatch("C1", []pendingChannelInbound{stripped}) {
		t.Fatal("spill must confirm")
	}
	c := replayIntoFreshCoalescer(t, spool)
	if got := ledgerCount(t, c, "C1", "1.0"); got != 2 {
		t.Fatalf("the stripped line must seed its count: ledger count %d, want 2", got)
	}
	if got := leastChargedCopy(t, c, "C1", "1.0"); got != 2 {
		t.Fatalf("every admitted copy must carry the ledger's count: least %d, want 2", got)
	}
}
