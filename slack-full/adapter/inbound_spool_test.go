package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests for the durable shutdown spool (gp-9e7 fix round 2a'/2b',
// durability hardened in round 3). Context: the liveness watermark
// advances at ADMISSION time, so a buffered item the shutdown drain
// cannot deliver is invisible to both Slack redelivery (Slack got its
// 200) and the startup watermark backfill (the item sits at or below
// the persisted watermark). The spool is that item's only redelivery
// path — which is why its own failure modes must be crash-safe and
// loudly reported.

// Round 3 (1b + 1c): consume stages the spool to a .replaying file
// instead of deleting it — a crash before re-admission retries — and a
// crash-truncated final line is tolerated but logged LOUDLY as loss.
func TestInboundSpoolConsumeStagesForReplayAndFlagsCorruptTail(t *testing.T) {
	readLog, cleanupLog := captureLog(t)
	defer cleanupLog()
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s := newInboundSpool(path)
	reaction := testPending("C1", "2.0", "a reaction")
	reaction.reaction = true
	if !s.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "a message"), reaction}) {
		t.Fatal("spillBatch reported failure for a healthy spool")
	}
	if !s.spillBatch("C2", []pendingChannelInbound{testPending("C2", "3.0", "another channel")}) {
		t.Fatal("spillBatch reported failure for a healthy spool")
	}
	// Simulate a process death mid-append: a truncated final line.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open spool for tail corruption: %v", err)
	}
	if _, err := f.WriteString(`{"channel":"C3","inbound":{"provider_message`); err != nil {
		t.Fatalf("write corrupt tail: %v", err)
	}
	_ = f.Close()

	entries, done := s.consume()
	if len(entries) != 3 {
		t.Fatalf("consumed %d entries, want 3 (corrupt tail tolerated): %+v", len(entries), entries)
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
	// The truncated line can only mean crash-mid-spill of that batch:
	// it must be reported as LOSS, not quietly skipped (round 3, 1c).
	if logs := readLog(); !strings.Contains(logs, "CORRUPT LINE") || !strings.Contains(logs, "LOSS") {
		t.Errorf("corrupt tail not flagged loudly as loss:\n%s", logs)
	}
	// Crash-safety (round 3, 1b): the spool was RENAMED to the staging
	// file, not deleted — a crash before re-admission finds it there.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool file still present after consume (err=%v) — it must be staged away atomically", err)
	}
	if _, err := os.Stat(s.replayingPath()); err != nil {
		t.Fatalf("staging file missing before cleanup (err=%v) — a crash mid-replay would lose the entries", err)
	}
	if done == nil {
		t.Fatal("consume returned no cleanup for a consumed staging file")
	}
	done()
	if _, err := os.Stat(s.replayingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging file still present after cleanup (err=%v) — a later restart would replay duplicates", err)
	}
	if again, _ := s.consume(); len(again) != 0 {
		t.Fatalf("consume after cleanup returned %d entries, want 0", len(again))
	}
}

// Round 3 (1b) headline: a crash BETWEEN consume and re-admission must
// not lose the replayed events — the next startup finds the .replaying
// file and retries it.
func TestInboundSpoolCrashMidReplayRetriesOnNextStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s1 := newInboundSpool(path)
	if !s1.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "spooled at shutdown")}) {
		t.Fatal("spillBatch failed")
	}

	// First restart: consume stages the file, then the process "crashes"
	// before re-admitting anything (the cleanup is never invoked).
	entries, _ := s1.consume()
	if len(entries) != 1 {
		t.Fatalf("first consume returned %d entries, want 1", len(entries))
	}

	// Second restart: the full replay path re-admits the same entry.
	s2 := newInboundSpool(path)
	c := newInboundCoalescer(30*time.Millisecond, nil)
	deliver, got := collectingDeliver()
	c.deliver = deliver
	if n := s2.replayInto(c); n != 1 {
		t.Fatalf("replay after crash-mid-replay re-buffered %d entries, want 1", n)
	}
	select {
	case batch := <-got:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("replayed batch = %+v, want the spooled 1.0", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replayed batch never delivered")
	}

	// Third restart: the completed replay removed the staging file.
	if n := newInboundSpool(path).replayInto(newInboundCoalescer(time.Hour, nil)); n != 0 {
		t.Fatalf("third-process replay returned %d entries, want 0", n)
	}
}

// A crash-mid-replay leftover AND a newer spool (a later shutdown's
// spill) must both replay, older entries first.
func TestInboundSpoolMergesReplayingLeftoverWithNewerSpool(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s1 := newInboundSpool(path)
	if !s1.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "older, crashed mid-replay")}) {
		t.Fatal("spillBatch failed")
	}
	if e, _ := s1.consume(); len(e) != 1 { // stages 1.0 into .replaying; "crash" here
		t.Fatalf("staged %d entries, want 1", len(e))
	}
	s2 := newInboundSpool(path)
	if !s2.spillBatch("C1", []pendingChannelInbound{testPending("C1", "2.0", "newer spool entry")}) {
		t.Fatal("spillBatch failed")
	}

	entries, done := s2.consume()
	if len(entries) != 2 {
		t.Fatalf("merged consume returned %d entries, want 2: %+v", len(entries), entries)
	}
	if entries[0].Inbound.ProviderMessageID != "1.0" || entries[1].Inbound.ProviderMessageID != "2.0" {
		t.Fatalf("merged order = [%s %s], want oldest first [1.0 2.0]",
			entries[0].Inbound.ProviderMessageID, entries[1].Inbound.ProviderMessageID)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("spool file still present after merge (err=%v)", err)
	}
	if done == nil {
		t.Fatal("consume returned no cleanup after merge")
	}
	done()
	if again, _ := newInboundSpool(path).consume(); len(again) != 0 {
		t.Fatalf("consume after merged replay returned %d entries, want 0", len(again))
	}
}

// Round 3 (1a): spillBatch reports durability truthfully — a failed
// write returns false so the caller logs the loud LOSS verdict instead
// of claiming "spooled for startup replay".
func TestInboundSpoolSpillFailureReportsFalse(t *testing.T) {
	readLog, cleanupLog := captureLog(t)
	defer cleanupLog()
	// The spool path IS a directory: the append open must fail.
	dir := t.TempDir()
	s := newInboundSpool(dir)
	if s.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "doomed")}) {
		t.Fatal("spillBatch reported success for an unwritable spool path")
	}
	if logs := readLog(); !strings.Contains(logs, "write FAILED") {
		t.Errorf("failed spill not logged:\n%s", logs)
	}
}

// Round 3 (1a), coalescer side: when the spill hook reports failure,
// flushAll's give-up path logs the loud per-channel SHUTDOWN LOSS line
// — the explicit last resort — and never claims the batch was spooled.
func TestCoalescerFlushAllSpillFailureLogsLoss(t *testing.T) {
	readLog, cleanupLog := captureLog(t)
	defer cleanupLog()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(string, []pendingChannelInbound) error { return errDeliverFailed }
	c.spill = func(string, []pendingChannelInbound) bool { return false }
	c.enqueue("C1", testPending("C1", "1.0", "doomed"))
	done := make(chan struct{})
	go func() {
		c.flushAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flushAll did not terminate")
	}
	logs := readLog()
	if !strings.Contains(logs, "SHUTDOWN LOSS chan=C1") {
		t.Errorf("spill failure missing the loud SHUTDOWN LOSS line:\n%s", logs)
	}
	if strings.Contains(logs, "spooled for startup replay") {
		t.Errorf("flushAll claimed a failed spill was spooled:\n%s", logs)
	}
}

// Round 3 (2c): every spool write runs entirely under the spool mutex,
// so seal() JOINS a write still in flight — it cannot return while one
// is mid-file — and refuses (loudly, without touching the file) any
// write after it. No spool write can race process exit.
func TestInboundSpoolSealJoinsInFlightWriteAndRefusesLate(t *testing.T) {
	readLog, cleanupLog := captureLog(t)
	defer cleanupLog()
	path := filepath.Join(t.TempDir(), "spool.jsonl")
	s := newInboundSpool(path)

	// Simulate an in-flight write: a spill holds s.mu for its entire
	// write; seal must block until it releases.
	s.mu.Lock()
	sealed := make(chan struct{})
	go func() {
		s.seal()
		close(sealed)
	}()
	select {
	case <-sealed:
		t.Fatal("seal returned while a write held the spool mutex — an in-flight write would race process exit")
	case <-time.After(100 * time.Millisecond):
	}
	s.mu.Unlock()
	select {
	case <-sealed:
	case <-time.After(2 * time.Second):
		t.Fatal("seal never completed after the in-flight write settled")
	}

	// Post-seal writes are refused before touching the file.
	if s.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "late straggler")}) {
		t.Fatal("spillBatch succeeded on a sealed spool")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sealed spool was written to (stat err=%v)", err)
	}
	if logs := readLog(); !strings.Contains(logs, "SEALED") {
		t.Errorf("refused post-seal write not logged:\n%s", logs)
	}
}

func TestInboundSpoolNilAndEmptyPathSafe(t *testing.T) {
	if sp := newInboundSpool(""); sp != nil {
		t.Fatal("empty path must disable the spool (nil)")
	}
	var s *inboundSpool
	if s.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}) { // must not panic
		t.Fatal("nil spool reported a successful spill")
	}
	s.seal() // must not panic
	if got, done := s.consume(); len(got) != 0 || done != nil {
		t.Fatalf("nil spool consumed %d entries (cleanup=%v)", len(got), done != nil)
	}
	if n := s.replayInto(newInboundCoalescer(time.Hour, nil)); n != 0 {
		t.Fatalf("nil spool replayed %d entries", n)
	}
}

// The finding-(A) headline from round 2: a batch the shutdown drain
// cannot deliver (gc down through every retry pass) is re-delivered
// after a restart via the spool replay — messages re-enter the coalesce
// window and deliver on its timer, carrying spooled reactions in the
// same batch (no solo reaction wake).
func TestInboundSpoolShutdownResidueRedeliveredAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spool.jsonl")

	// First process: gc unreachable for the whole shutdown drain.
	spool := newInboundSpool(path)
	c1 := newInboundCoalescer(time.Hour, nil)
	c1.deliver = func(string, []pendingChannelInbound) error { return errDeliverFailed }
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

	// Third process: the completed replay cleaned up — nothing replays twice.
	if n := newInboundSpool(path).replayInto(newInboundCoalescer(time.Hour, nil)); n != 0 {
		t.Fatalf("third-process replay returned %d entries, want 0", n)
	}
}

// Round 5 (finding 2): a .replaying file whose tail is a PARTIAL line —
// a process death mid-append truncated it — must not fuse that tail
// with the first entry of a newer spool being merged behind it. Before
// the boundary seal, appendFileTo concatenated the intact source entry
// directly onto the partial prefix, producing one corrupt fused line:
// the partial tail's loss silently CONSUMED a perfectly intact entry
// (and the merge then removed the source file, so nothing retried it).
func TestInboundSpoolMergeSealsPartialTailNeverConsumesIntactEntry(t *testing.T) {
	readLog, cleanupLog := captureLog(t)
	defer cleanupLog()
	path := filepath.Join(t.TempDir(), "spool.jsonl")

	// A .replaying leftover: one intact entry, then a crash-truncated
	// partial line (no trailing newline).
	s1 := newInboundSpool(path)
	if !s1.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", "intact staged entry")}) {
		t.Fatal("spillBatch failed")
	}
	if e, _ := s1.consume(); len(e) != 1 { // stages into .replaying; "crash" here
		t.Fatalf("staged %d entries, want 1", len(e))
	}
	f, err := os.OpenFile(s1.replayingPath(), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open staging file for tail corruption: %v", err)
	}
	if _, err := f.WriteString(`{"channel":"C1","inbound":{"provider_message`); err != nil {
		t.Fatalf("write partial tail: %v", err)
	}
	_ = f.Close()

	// A newer spool with one intact entry lands behind the leftover.
	s2 := newInboundSpool(path)
	if !s2.spillBatch("C2", []pendingChannelInbound{testPending("C2", "2.0", "intact source entry")}) {
		t.Fatal("spillBatch failed")
	}

	entries, done := s2.consume()
	byTS := map[string]spooledInbound{}
	for _, e := range entries {
		byTS[e.Inbound.ProviderMessageID] = e
	}
	// The intact source entry must SURVIVE the merge — the corrupt
	// boundary may cost only the already-truncated partial line.
	if e, ok := byTS["2.0"]; !ok || e.Channel != "C2" {
		t.Fatalf("intact source entry 2.0 lost across a partial-tail merge (entries=%+v)", entries)
	}
	if e, ok := byTS["1.0"]; !ok || e.Channel != "C1" {
		t.Fatalf("intact staged entry 1.0 lost (entries=%+v)", entries)
	}
	if len(entries) != 2 {
		t.Fatalf("merged consume returned %d entries, want exactly 2", len(entries))
	}
	// The partial line is reported LOUDLY, both when the merge seals it
	// and when replay drops it.
	logs := readLog()
	if !strings.Contains(logs, "CORRUPT LINE") || !strings.Contains(logs, "LOSS") {
		t.Errorf("partial tail not flagged loudly as loss at replay:\n%s", logs)
	}
	if !strings.Contains(logs, "mid-line") {
		t.Errorf("merge did not loudly report sealing the partial tail:\n%s", logs)
	}
	if done != nil {
		done()
	}
}
