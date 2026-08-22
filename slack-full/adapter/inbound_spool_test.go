package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests for the durable shutdown spool (gp-9e7 fix round 2a'/2b').
// Context: the liveness watermark advances at ADMISSION time, so a
// buffered item the shutdown drain cannot deliver is invisible to both
// Slack redelivery (Slack got its 200) and the startup watermark
// backfill (the item sits at or below the persisted watermark). The
// spool is that item's only redelivery path.

func TestInboundSpoolRoundTripSkipsCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s := newInboundSpool(path)
	reaction := testPending("C1", "2.0", "a reaction")
	reaction.reaction = true
	s.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "a message"), reaction})
	s.spillBatch("C2", []pendingChannelInbound{testPending("C2", "3.0", "another channel")})
	// Simulate a process death mid-append: a truncated final line.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open spool for tail corruption: %v", err)
	}
	if _, err := f.WriteString(`{"channel":"C3","inbound":{"provider_message`); err != nil {
		t.Fatalf("write corrupt tail: %v", err)
	}
	_ = f.Close()

	entries := s.consume()
	if len(entries) != 3 {
		t.Fatalf("consumed %d entries, want 3 (corrupt tail skipped): %+v", len(entries), entries)
	}
	byTS := map[string]spooledInbound{}
	for _, e := range entries {
		byTS[e.Inbound.ProviderMessageID] = e
	}
	if e := byTS["1.0"]; e.Channel != "C1" || e.Reaction {
		t.Errorf("entry 1.0 = %+v, want C1 message", e)
	}
	if e := byTS["2.0"]; e.Channel != "C1" || !e.Reaction {
		t.Errorf("entry 2.0 = %+v, want C1 reaction (flag preserved)", e)
	}
	if e := byTS["3.0"]; e.Channel != "C2" || e.Reaction {
		t.Errorf("entry 3.0 = %+v, want C2 message", e)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool file still present after consume (err=%v) — a later restart would replay duplicates", err)
	}
	if again := s.consume(); len(again) != 0 {
		t.Fatalf("second consume returned %d entries, want 0", len(again))
	}
}

func TestInboundSpoolNilAndEmptyPathSafe(t *testing.T) {
	if sp := newInboundSpool(""); sp != nil {
		t.Fatal("empty path must disable the spool (nil)")
	}
	var s *inboundSpool
	s.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}) // must not panic
	if got := s.consume(); len(got) != 0 {
		t.Fatalf("nil spool consumed %d entries", len(got))
	}
	if n := s.replayInto(newInboundCoalescer(time.Hour, nil)); n != 0 {
		t.Fatalf("nil spool replayed %d entries", n)
	}
}

// The finding-(A) headline: a batch the shutdown drain cannot deliver
// (gc down through every retry pass) is re-delivered after a restart
// via the spool replay — messages re-enter the coalesce window and
// deliver on its timer, carrying spooled reactions in the same batch
// (no solo reaction wake).
func TestInboundSpoolShutdownResidueRedeliveredAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")

	// First process: gc unreachable for the whole shutdown drain.
	spool := newInboundSpool(path)
	c1 := newInboundCoalescer(time.Hour, nil)
	c1.deliver = func(string, []pendingChannelInbound) bool { return false }
	c1.spill = spool.spillBatch
	c1.enqueue("C1", testPending("C1", "1.0", "acked to Slack; gc down at shutdown"))
	if !c1.admitReaction("C1", testPending("C1", "2.0", "reaction in the side lane"), false) {
		t.Fatal("admitReaction refused")
	}
	c1.flushAll()

	// Second process: fresh coalescer, gc reachable again.
	c2 := newInboundCoalescer(30*time.Millisecond, nil)
	deliver, got := collectingDeliver()
	c2.deliver = deliver
	if n := newInboundSpool(path).replayInto(c2); n != 2 {
		t.Fatalf("replayed %d entries, want 2", n)
	}
	select {
	case batch := <-got:
		byTS := map[string]bool{}
		for _, p := range batch {
			byTS[p.inbound.ProviderMessageID] = p.reaction
		}
		if r, ok := byTS["1.0"]; !ok || r {
			t.Errorf("replayed batch: message 1.0 present=%v reaction=%v", ok, r)
		}
		if r, ok := byTS["2.0"]; !ok || !r {
			t.Errorf("replayed batch: reaction 2.0 present=%v reaction=%v (flag must survive the spool)", ok, r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replayed batch never delivered")
	}

	// Third process: the spool was consumed — nothing replays twice.
	if n := newInboundSpool(path).replayInto(newInboundCoalescer(time.Hour, nil)); n != 0 {
		t.Fatalf("third-process replay returned %d entries, want 0", n)
	}
}
