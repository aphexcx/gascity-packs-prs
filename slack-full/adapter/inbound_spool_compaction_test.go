package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An in-place compaction whose temp file cannot be flushed or fsynced
// must leave the journal as it is (r16 finding 1): a shadowed error let
// the rename proceed with an incomplete temp file, replacing the journal
// and losing its verdicts. An fsync failure here also empties the temp
// file — the bytes are not on disk whatever the write returned.
func TestInPlaceCompactionKeepsTheJournalWhenTheTempFileCannotBeSynced(t *testing.T) {
	prev := syncFile
	syncFile = func(f *os.File) error {
		f.Truncate(0)
		return errors.New("fsync: input/output error")
	}
	t.Cleanup(func() { syncFile = prev })
	dir := t.TempDir()
	spool := newInboundSpool(filepath.Join(dir, strings.Repeat("s", 250))) // forces the in-place fallback
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
		t.Fatal("record must confirm")
	}
	if !spool.spillBatch("C1", []pendingChannelInbound{testPending("C1", "2.0", "beside it")}) {
		t.Fatal("spill must confirm")
	}
	read, cleanup := captureLog(t)
	defer cleanup()
	deliver, _ := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c1 := newInboundCoalescer(time.Hour, nil)
	c1.deliver = deliver
	c1.recordVerdict = spool.recordVerdict
	spool.replayInto(c1)
	if logged := read(); !strings.Contains(logged, "replaying in place") || !strings.Contains(logged, "compacting") || !strings.Contains(logged, "FAILED") {
		t.Fatalf("the compaction must fail loudly on the fsync error: %q", logged)
	}
	if n := countVerdictLines(t, spool.path, "1.0"); n < 1 {
		t.Fatal("the journal must be left whole when the temp file could not be made durable — its verdicts were the only durable copy")
	}
	c2 := newInboundCoalescer(time.Hour, nil)
	c2.recordVerdict = spool.recordVerdict
	spool.replayInto(c2)
	if !c2.onLadder("C1", "1.0") {
		t.Fatal("the verdict survives the failed compaction")
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, ".spool-compact-*")); len(entries) != 0 {
		t.Fatalf("the failed temp file is removed, found %v", entries)
	}
}

// A recovery failure (a spool file the adapter wrote and now cannot
// read) closes the coalescer to admissions and is returned to the
// caller (r20 finding 2): returned as an empty replay, startup went on
// with an empty ledger and a delayed Slack copy of a message whose
// stripped disposition sat in the unreadable file entered delivery with
// its original attachments. main() refuses to start on the error.
func TestRecoveryFailureClosesAdmissions(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	plainA := testPendingWithAttachment("C1", "1.0", "A with a voice memo", "/tmp/x/memo.m4a")
	strippedA := withholdAttachments(plainA, permanent422())
	strippedA.isolate, strippedA.attempts = true, 1
	if !spool.spillBatch("C1", []pendingChannelInbound{plainA}) {
		t.Fatal("spill must confirm")
	}
	if err := os.Rename(spool.path, spool.replayingPath()); err != nil {
		t.Fatal(err)
	}
	if !spool.spillBatch("C1", []pendingChannelInbound{strippedA}) {
		t.Fatal("spill must confirm")
	}
	if err := os.Chmod(spool.path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(spool.path, 0o600) })
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.recordVerdict = spool.recordVerdict
	n, err := spool.replay(c)
	if err == nil || n != 0 {
		t.Fatalf("recovery must fail loudly (n=%d err=%v)", n, err)
	}
	// A delayed Slack copy of A arrives after the failed recovery.
	c.enqueue("C1", plainA)
	c.flushAheadOf("C1", "")
	time.Sleep(60 * time.Millisecond)
	if c.pendingContains("C1", "1.0") {
		t.Fatal("nothing may enter delivery after a failed recovery — its disposition is in the unreadable spool")
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("nothing may POST after a failed recovery: %v", got)
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if !closed {
		t.Fatal("the coalescer is closed to admissions")
	}
}
