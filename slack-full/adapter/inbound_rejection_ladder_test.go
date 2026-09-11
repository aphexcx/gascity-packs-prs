package main

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- gp-sgu7: a 4xx is never retried as-is; transient failures back off ------
//
// Incident 2026-09-08 00:14Z → 2026-09-11 (citadel, C0AP0KV9S9E): the live
// adapter predated gp-xnc and retried one 422-refused batch every 8 s,
// 34,000+ times, holding 43 later messages behind it. gp-xnc bounded
// that to three identical retries per poisoned message. This file pins
// the mayor's tighter contract for gp-sgu7: a payload refusal (4xx gc
// answers the same way every time) is retried at most ONCE and only
// with a DIFFERENT payload — the attachments stripped and their local
// paths named in the text — then dead-lettered; a message with nothing
// to strip is dead-lettered at the first refusal. Transient failures
// (network, 5xx) keep retry-forever durability but on a per-channel
// doubling backoff with a cap instead of the fixed window forever.

func testPendingWithAttachment(channel, ts, text, path string) pendingChannelInbound {
	p := testPending(channel, ts, text)
	p.inbound.Attachments = []externalAttachment{{ProviderID: "F1", URL: "file://" + path, MIMEType: "audio/mp4"}}
	return p
}

// The ladder IS the spec: one row per (entry shape, cause) the rounds
// found, with the verdict and why.
func TestNextRejectionStepTable(t *testing.T) {
	withAtt := testPendingWithAttachment("C1", "1.0", "voice memo", "/tmp/store/C1/1.0-audio.m4a")
	noAtt := testPending("C1", "2.0", "plain text")
	stripped := withholdAttachments(withAtt, permanent422())
	deleted := withAtt
	applyDeletion(&deleted)
	unvouched0 := noAtt
	unvouched1 := noAtt
	unvouched1.attempts = 1
	unvouchedAtCap := noAtt
	unvouchedAtCap.attempts = maxCoalesceDeliveryAttempts - 1
	unvouchedWithAtt := withAtt
	unvouchedWithAttAtCap := withAtt
	unvouchedWithAttAtCap.attempts = maxCoalesceDeliveryAttempts - 1
	reaction := noAtt
	reaction.reaction = true

	cases := []struct {
		name  string
		entry pendingChannelInbound
		cause error
		want  rejectionStep
	}{
		{"422 with attachments: retry once without them", withAtt, permanent422(), stepRetryWithoutAttachments},
		{"400 with attachments: same ladder as 422", withAtt, &inboundPostError{Status: 400}, stepRetryWithoutAttachments},
		{"413 with attachments: stripping is the one change that can help", withAtt, &inboundPostError{Status: 413}, stepRetryWithoutAttachments},
		{"422 without attachments: nothing to change, never retried as-is", noAtt, permanent422(), stepDeadLetter},
		{"422 after the attachment-less retry: dead-letter", stripped, permanent422(), stepDeadLetter},
		{"422 on a deletion notice (attachments already dropped): dead-letter", deleted, permanent422(), stepDeadLetter},
		{"422 on a reaction: dead-letter", reaction, permanent422(), stepDeadLetter},
		{"unvouched, first charge: bounded same-payload retry (gp-32q)", unvouched0, errDeliveryUnvouched, stepRetrySame},
		{"unvouched, second charge: still under the cap", unvouched1, errDeliveryUnvouched, stepRetrySame},
		{"unvouched at the cap: dead-letter", unvouchedAtCap, errDeliveryUnvouched, stepDeadLetter},
		// The stripping operand is the REFUSAL, not the attachment: an
		// accepted-but-unvouched payload keeps its attachments (codex r4
		// finding 3 — moving the attachment check outside
		// permanentDeliveryFailure would strip these and still pass the
		// rows above).
		{"unvouched with attachments, under the cap: same payload again, attachments kept", unvouchedWithAtt, errDeliveryUnvouched, stepRetrySame},
		{"unvouched with attachments at the cap: dead-letter, never stripped", unvouchedWithAttAtCap, errDeliveryUnvouched, stepDeadLetter},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextRejectionStep(tc.entry, tc.cause); got != tc.want {
				t.Fatalf("nextRejectionStep = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithholdAttachmentsNamesPathsInText(t *testing.T) {
	cause := permanent422()
	t.Run("parts entry keeps Text == foldedText", func(t *testing.T) {
		p := testPendingWithAttachment("C1", "1.0", "", "/tmp/store/C1/1.0-Audio Clip.m4a")
		p.body = "listen to this"
		p.files = "[1 Slack file attached]\n- Audio Clip.m4a (audio/mp4) → /tmp/store/C1/1.0-Audio Clip.m4a"
		p.inbound.Text = p.foldedText()
		got := withholdAttachments(p, cause)
		if len(got.inbound.Attachments) != 0 {
			t.Fatalf("attachments must be stripped, got %+v", got.inbound.Attachments)
		}
		if got.inbound.Text != got.foldedText() {
			t.Fatalf("Text must stay the fold of the parts:\n text=%q\n fold=%q", got.inbound.Text, got.foldedText())
		}
		for _, want := range []string{"withheld", "/tmp/store/C1/1.0-Audio Clip.m4a", "422 Unprocessable Entity"} {
			if !strings.Contains(got.inbound.Text, want) {
				t.Fatalf("text must name %q:\n%s", want, got.inbound.Text)
			}
		}
		if !strings.HasPrefix(got.inbound.Text, "listen to this") {
			t.Fatalf("body must stay first:\n%s", got.inbound.Text)
		}
		if strings.Contains(got.inbound.Text, cause.(*inboundPostError).Body) {
			t.Fatalf("the notice must carry the status line, not gc's JSON body:\n%s", got.inbound.Text)
		}
	})
	t.Run("legacy entry (no parts) is promoted: text becomes the body, the notice the files part", func(t *testing.T) {
		p := testPendingWithAttachment("C1", "1.0", "old spool line", "/tmp/store/C1/1.0-audio.m4a")
		got := withholdAttachments(p, cause)
		if len(got.inbound.Attachments) != 0 || !got.hasReminderParts() {
			t.Fatalf("legacy entry must lose its attachments and gain parts (codex r1 finding 4): %+v", got)
		}
		if got.body != "old spool line" || !strings.Contains(got.files, "/tmp/store/C1/1.0-audio.m4a") {
			t.Fatalf("body/files split wrong: body=%q files=%q", got.body, got.files)
		}
		// The folded Text reads exactly as the old flat form did.
		if !strings.HasPrefix(got.inbound.Text, "old spool line\n\n[") || !strings.Contains(got.inbound.Text, "/tmp/store/C1/1.0-audio.m4a") {
			t.Fatalf("unexpected text:\n%s", got.inbound.Text)
		}
	})
	t.Run("a second call strips nothing and adds nothing", func(t *testing.T) {
		p := testPendingWithAttachment("C1", "1.0", "x", "/tmp/a.m4a")
		once := withholdAttachments(p, cause)
		twice := withholdAttachments(once, cause)
		if twice.inbound.Text != once.inbound.Text {
			t.Fatalf("idempotent expected:\n once=%q\ntwice=%q", once.inbound.Text, twice.inbound.Text)
		}
	})
}

// The incident, end to end: a voice memo gc refuses WITH its attachment
// is accepted WITHOUT it — the text (naming the file's path) reaches the
// session on the second POST and nothing is dead-lettered.
func TestCoalescer422RetriesOnceWithoutAttachmentsThenDelivers(t *testing.T) {
	var mu sync.Mutex
	var posted [][]pendingChannelInbound
	delivered := make(chan []pendingChannelInbound, 4)
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		posted = append(posted, batch)
		mu.Unlock()
		for _, p := range batch {
			if len(p.inbound.Attachments) > 0 {
				return permanent422()
			}
		}
		delivered <- batch
		return nil
	}
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPendingWithAttachment("C1", "1.0", "voice memo", "/tmp/store/C1/1.0-Audio Clip.m4a"))

	select {
	case batch := <-delivered:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("unexpected delivery %+v", batch)
		}
		if len(batch[0].inbound.Attachments) != 0 || !strings.Contains(batch[0].inbound.Text, "/tmp/store/C1/1.0-Audio Clip.m4a") {
			t.Fatalf("second POST must carry no attachments and name the path: %+v", batch[0].inbound)
		}
		if batch[0].attempts != 1 {
			t.Fatalf("attempts = %d, want 1 (one refusal)", batch[0].attempts)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stripped retry never delivered")
	}
	select {
	case call := <-dead:
		t.Fatalf("nothing may be dead-lettered when the stripped retry succeeds: %+v", call.batch)
	case <-time.After(80 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 2 {
		t.Fatalf("want exactly 2 POSTs (refused with attachments, accepted without), got %d", len(posted))
	}
	if c.pendingContains("C1", "1.0") {
		t.Fatal("delivered message must not remain pending")
	}
}

// Refused twice (with, then without attachments): dead-lettered after
// exactly two POSTs, and a later message in the channel delivers.
func TestCoalescer422DeadLettersAfterAttachmentlessRetryAndSparesLater(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		for _, p := range batch {
			if p.inbound.ProviderMessageID == "1.0" {
				return permanent422()
			}
		}
		return nil
	})
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPendingWithAttachment("C1", "1.0", "poison", "/tmp/store/C1/1.0-a.m4a"))
	var call deadLetterCall
	select {
	case call = <-dead:
	case <-time.After(3 * time.Second):
		t.Fatal("poison was never dead-lettered")
	}
	if len(call.batch) != 1 || call.batch[0].inbound.ProviderMessageID != "1.0" || call.batch[0].attempts != 2 {
		t.Fatalf("dead-letter must carry the poison after 2 refusals, got %+v", call.batch)
	}
	if len(call.batch[0].inbound.Attachments) != 0 || !strings.Contains(call.batch[0].inbound.Text, "/tmp/store/C1/1.0-a.m4a") {
		t.Fatalf("the record is the stripped envelope, naming the file path: %+v", call.batch[0].inbound)
	}
	c.enqueue("C1", testPending("C1", "2.0", "later message"))
	waitForCalls(t, calls, []string{"1.0", "1.0", "2.0"})
	time.Sleep(60 * time.Millisecond)
	if got := calls(); len(got) != 3 {
		t.Fatalf("want exactly 3 POSTs, got %v", got)
	}
	if c.pendingContains("C1", "1.0") || c.pendingContains("C1", "2.0") {
		t.Fatal("nothing should remain pending")
	}
}

// Nothing to strip: one refusal, one dead-letter, no second POST.
func TestCoalescer422WithoutAttachmentsDeadLettersAtOnce(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error { return permanent422() })
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(15*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "text gc refuses"))
	select {
	case call := <-dead:
		if call.batch[0].attempts != 1 {
			t.Fatalf("attempts = %d, want 1", call.batch[0].attempts)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("never dead-lettered")
	}
	time.Sleep(60 * time.Millisecond)
	if got := calls(); len(got) != 1 {
		t.Fatalf("a 4xx is never retried as-is: want 1 POST, got %v", got)
	}
}

// Delay after the k-th consecutive transient failure: the window
// doubled k-1 times, never below the channel's window, never above the
// cap. Zero failures is the plain window.
func TestTransientRetryDelayTable(t *testing.T) {
	w := 8 * time.Second
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, 8 * time.Second}, {1, 8 * time.Second}, {2, 16 * time.Second}, {3, 32 * time.Second},
		{4, 64 * time.Second}, {5, 128 * time.Second}, {6, 256 * time.Second},
		{7, maxTransientRetryDelay}, {30, maxTransientRetryDelay}, {1000, maxTransientRetryDelay},
	}
	for _, tc := range cases {
		if got := transientRetryDelay(w, tc.failures); got != tc.want {
			t.Fatalf("transientRetryDelay(%s, %d) = %s, want %s", w, tc.failures, got, tc.want)
		}
	}
	// A digest interval longer than the backoff wins: the operator's
	// cadence is the floor.
	if got := transientRetryDelay(2*time.Hour, 3); got != 2*time.Hour {
		t.Fatalf("digest floor: got %s", got)
	}
	// A zero window (coalescing disabled: spool replays and dead-letter
	// write retries still run through here) backs off from a positive
	// base, never at request-completion speed (codex r4 finding 2).
	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{{0, 0}, {1, time.Second}, {2, 2 * time.Second}, {3, 4 * time.Second}, {20, maxTransientRetryDelay}} {
		if got := transientRetryDelay(0, tc.failures); got != tc.want {
			t.Fatalf("transientRetryDelay(0, %d) = %s, want %s", tc.failures, got, tc.want)
		}
	}
}

// --- codex r4 on gp-sgu7 ------------------------------------------------------

// A refused batch whose probes come back UNVOUCHED (accepted, not
// vouched for) must resume as single probes, never recombine into the
// refused batch (codex r4 finding 1).
func TestUnvouchedProbesNeverRecombineIntoTheRefusedBatch(t *testing.T) {
	var mu sync.Mutex
	unvouchedOnce := map[string]bool{"1.0": true, "2.0": true}
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch) > 1 {
			return permanent422()
		}
		mu.Lock()
		defer mu.Unlock()
		ts := batch[0].inbound.ProviderMessageID
		if unvouchedOnce[ts] {
			unvouchedOnce[ts] = false
			return errDeliveryUnvouched
		}
		return nil
	})
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(string, []pendingChannelInbound, error) bool {
		t.Error("nothing may dead-letter here")
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "a"))
	c.enqueue("C1", testPending("C1", "2.0", "b"))
	// Batch refused → probes unvouched (charged, same payload again) →
	// next window: single probes, accepted.
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0", "2.0", "1.0", "2.0"})
	if got := calls(); len(got) != 5 {
		t.Fatalf("unexpected extra deliveries: %v", got)
	}
	for _, call := range calls()[1:] {
		if strings.Contains(call, ",") {
			t.Fatalf("the refused batch was recombined and re-posted: %v", calls())
		}
	}
}

// A zero-window coalescer (coalescing disabled) still backs off a
// transient failure on a replayed entry instead of retrying at
// request-completion speed.
func TestZeroWindowTransientFailureBacksOff(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(0, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "replayed"))
	waitForCalls(t, calls, []string{"1.0"})
	time.Sleep(300 * time.Millisecond)
	if got := calls(); len(got) > 2 {
		t.Fatalf("a zero-window transient failure must back off (≥1 s), got %d POSTs in 300 ms: %v", len(got), got)
	}
	c.mu.Lock()
	inBackoff := c.inBackoffLocked("C1")
	c.mu.Unlock()
	if !inBackoff {
		t.Fatal("channel must be in backoff after the transient failure")
	}
}

// Real timers: the gap between consecutive transient retries at least
// doubles (Go timers never fire early), and a success resets the run.
func TestCoalescerTransientFailureBacksOffThenResets(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	fails := 3
	delivered := make(chan []pendingChannelInbound, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if fails > 0 {
			fails--
			return errors.New("dial tcp 127.0.0.1:8372: connect: connection refused")
		}
		delivered <- batch
		return nil
	}
	c.enqueue("C1", testPending("C1", "1.0", "x"))
	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("never delivered")
	}
	mu.Lock()
	times := append([]time.Time(nil), at...)
	mu.Unlock()
	if len(times) != 4 {
		t.Fatalf("want 4 POSTs (3 failures + success), got %d", len(times))
	}
	gaps := []time.Duration{times[1].Sub(times[0]), times[2].Sub(times[1]), times[3].Sub(times[2])}
	wantMin := []time.Duration{20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond}
	for i, g := range gaps {
		if g < wantMin[i] {
			t.Fatalf("retry %d fired after %s, want ≥ %s (gaps %v)", i+1, g, wantMin[i], gaps)
		}
	}
	c.mu.Lock()
	n := c.transientFailures["C1"]
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("a successful delivery must reset the run, got %d", n)
	}
}

// A channel waiting out its backoff is not swept into another
// channel's flush: the sweep would be the fixed-cadence retry the
// backoff exists to end.
func TestSweepSkipsChannelInBackoff(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(string, []pendingChannelInbound) error { return nil }
	c.mu.Lock()
	c.pending["C1"] = []pendingChannelInbound{testPending("C1", "1.0", "x")}
	c.pending["C2"] = []pendingChannelInbound{testPending("C2", "2.0", "y")}
	c.retryNotBefore["C1"] = time.Now().Add(time.Hour)
	swept := c.takeSweepsLocked("C0")
	c.mu.Unlock()
	if len(swept) != 1 || swept[0].channel != "C2" {
		t.Fatalf("only C2 may be swept, got %+v", swept)
	}
	for _, s := range swept {
		s.mu.Unlock()
		c.endDelivery()
	}
}

// --- codex r1 on gp-sgu7: the backoff deadline holds on every early-flush path

// A channel waiting out a transient-failure backoff keeps an over-cap
// buffer instead of POSTing it on every enqueue, and a SIGHUP reconcile
// keeps the deadline instead of collapsing it to the one-second cap
// cadence (codex r1 finding 2). State is set as restore() leaves it: a
// failure run, a deadline, and the armed backoff timer.
func TestOverCapEnqueueAndReconcileHonorBackoffDeadline(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 10)
	for i := 0; i < maxCoalescePerChannel+5; i++ {
		c.enqueue("C1", testPending("C1", fmt.Sprintf("%d.0", i+1), "burst during outage"))
	}
	c.reconcileTimers()
	time.Sleep(120 * time.Millisecond)
	if got := calls(); len(got) != 0 {
		t.Fatalf("an over-cap buffer in backoff must not POST early (enqueue or reconcile): %v", got)
	}
	c.mu.Lock()
	pending := len(c.pending["C1"])
	c.mu.Unlock()
	if pending != maxCoalescePerChannel+5 {
		t.Fatalf("buffer must keep every item through the backoff, got %d", pending)
	}
}

// A reconcile on a channel whose backed-off retry is already DUE fires
// it promptly (the cap cadence) rather than waiting a full backed-off
// delay again.
func TestReconcileFiresAnOverdueBackoffRetryPromptly(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "a"))
	c.mu.Lock()
	c.transientFailures["C1"] = 10                        // a 5-minute backoff...
	c.retryNotBefore["C1"] = time.Now().Add(-time.Second) // ...whose deadline has passed
	c.mu.Unlock()
	c.reconcileTimers()
	waitForCalls(t, calls, []string{"1.0"})
}

// A member flagged for isolation survives the spool: a restart resumes
// the single probes instead of re-posting the refused batch.
func TestSpoolCarriesIsolateFlag(t *testing.T) {
	dir := t.TempDir()
	s := newInboundSpool(dir + "/spool.jsonl")
	p := testPending("C1", "1.0", "refused member")
	p.isolate = true
	if !s.spillBatch("C1", []pendingChannelInbound{p, testPending("C1", "2.0", "plain")}) {
		t.Fatal("spill must confirm")
	}
	entries, done := s.consume()
	defer done()
	if len(entries) != 2 {
		t.Fatalf("want 2 spooled entries, got %d", len(entries))
	}
	if !entries[0].Isolate || entries[1].Isolate {
		t.Fatalf("isolate flag must round-trip per entry: %+v / %+v", entries[0].Isolate, entries[1].Isolate)
	}
}

// A stripped retry posts alone: a batch-mate never takes its blame and a
// second refusal charges IT (the incident's second window, with a newer
// message buffered beside it).
func TestStrippedRetryPostsAloneBesideNewerMessages(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		for _, p := range batch {
			if p.inbound.ProviderMessageID == "1.0" {
				return permanent422()
			}
		}
		return nil
	})
	dead := make(chan deadLetterCall, 4)
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	c.enqueue("C1", testPendingWithAttachment("C1", "1.0", "voice memo", "/tmp/x/memo.m4a"))
	waitForCalls(t, calls, []string{"1.0"})
	c.enqueue("C1", testPending("C1", "2.0", "newer"))
	// The stripped 1.0 probes alone (refused → dead-letter), then 2.0
	// posts as its own batch and delivers.
	waitForCalls(t, calls, []string{"1.0", "1.0", "2.0"})
	select {
	case call := <-dead:
		if len(call.batch) != 1 || call.batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("dead letter must be 1.0 alone: %+v", call.batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stripped retry refused again must dead-letter")
	}
	if c.pendingContains("C1", "2.0") {
		t.Fatal("the newer message must have delivered")
	}
}

// --- codex r1 finding 4: a legacy entry keeps the withheld paths --------------

// A partless legacy/spool-replayed entry is promoted to parts by the
// withholding so the notice lives in the protected files part: a long
// body is tail-trimmed by the head-protected composer, the notice is
// not.
func TestWithholdAttachmentsPromotesLegacyEntryAndSurvivesTrim(t *testing.T) {
	long := strings.Repeat("founder words ", 300) // ~4200 chars
	p := pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: "10.0",
		ReplyToMessageID:  "9.0",
		Text:              long + "\n",
		Attachments:       []externalAttachment{{ProviderID: "F1", URL: "file:///tmp/x/memo.m4a", MIMEType: "audio/mp4"}},
	}}
	q := withholdAttachments(p, permanent422())
	if !q.hasReminderParts() {
		t.Fatal("legacy entry must be promoted to parts")
	}
	if q.body != long {
		t.Fatalf("body must be the original text, got %d chars", len(q.body))
	}
	if !strings.Contains(q.files, "/tmp/x/memo.m4a") || q.threadAnchor == "" {
		t.Fatalf("files part must name the path and the thread anchor must be synthesized: files=%q anchor=%q", q.files, q.threadAnchor)
	}
	if q.inbound.Text != q.foldedText() {
		t.Fatal("Text must be the re-folded parts")
	}
	parts, ok := q.reminderParts("C1")
	if !ok {
		t.Fatal("promoted entry must expose parts to the composer")
	}
	composed, _, _, _, trimmed := composeChannelReminderText(parts, "", "", 1000)
	if !trimmed {
		t.Fatal("fixture must overflow the budget so the body is trimmed")
	}
	if !strings.Contains(composed, "/tmp/x/memo.m4a") {
		t.Fatalf("the withheld-attachment notice must survive the trim; composed=%q", composed)
	}
	// Idempotent: a second call adds nothing.
	if again := withholdAttachments(q, permanent422()); again.files != q.files || again.inbound.Text != q.inbound.Text {
		t.Fatal("withholding must be idempotent")
	}
}

// --- codex r1 finding 5: one log line per state change -----------------------

func TestBackoffLogWorthyTable(t *testing.T) {
	base := 8 * time.Second
	rows := []struct {
		n      int
		delay  time.Duration
		worthy bool
	}{
		{1, 8 * time.Second, true},    // first failure
		{2, 16 * time.Second, true},   // doubled
		{3, 32 * time.Second, true},   // doubled
		{6, 256 * time.Second, true},  // doubled
		{7, 5 * time.Minute, true},    // reached the cap: logged once...
		{8, 5 * time.Minute, false},   // ...then silent at the cap
		{100, 5 * time.Minute, false}, // still silent
	}
	for _, r := range rows {
		delay, worthy := backoffLogWorthy(base, r.n)
		if delay != r.delay || worthy != r.worthy {
			t.Fatalf("n=%d: got (%s, %v), want (%s, %v)", r.n, delay, worthy, r.delay, r.worthy)
		}
	}
	// A digest interval above the cap: one line, then silence.
	if _, worthy := backoffLogWorthy(time.Hour, 2); worthy {
		t.Fatal("a cadence that cannot change must not log again")
	}
}

func TestRepeatedFailureLogLogsOncePerDistinctFailure(t *testing.T) {
	var r *repeatedFailureLog
	if !r.changed("C1", "x") {
		t.Fatal("nil receiver must report every failure")
	}
	r = newRepeatedFailureLog()
	if !r.changed("C1", "422 refused") {
		t.Fatal("first failure logs")
	}
	if r.changed("C1", "422 refused") {
		t.Fatal("the same failure again is silent")
	}
	if !r.changed("C1", "dial tcp: refused") {
		t.Fatal("a different failure logs")
	}
	if !r.changed("C2", "dial tcp: refused") {
		t.Fatal("channels are independent")
	}
	r.clear("C1")
	if !r.changed("C1", "dial tcp: refused") {
		t.Fatal("a success clears the memory so the next failure logs")
	}
}

// --- codex r2 on gp-sgu7 ------------------------------------------------------

// A parked entry spooled at shutdown carries its refusal; the startup
// replay parks it straight back for the dead-letter write and never
// re-posts it (codex r2 finding 1).
func TestSpoolReplayParksRefusedEntriesWithoutRepost(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	deliver1, calls1 := recordingDeliver(func([]pendingChannelInbound) error { return permanent422() })
	c1 := newInboundCoalescer(time.Hour, nil)
	c1.deliver = deliver1
	c1.deadLetter = func(string, []pendingChannelInbound, error) bool { return false } // disk full until restart
	c1.spill = spool.spillBatch
	c1.enqueue("C1", testPending("C1", "1.0", "poison"))
	c1.flushAll()
	waitForCalls(t, calls1, []string{"1.0"})

	// Restart: a fresh coalescer, the disk writable again.
	var mu sync.Mutex
	var written []deadLetterCall
	deliver2, calls2 := recordingDeliver(func([]pendingChannelInbound) error { t.Error("refused entry re-posted after restart"); return nil })
	c2 := newInboundCoalescer(20*time.Millisecond, nil)
	c2.deliver = deliver2
	c2.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		mu.Lock()
		defer mu.Unlock()
		written = append(written, deadLetterCall{channel, batch, cause})
		return true
	}
	if n := spool.replayInto(c2); n != 1 {
		t.Fatalf("replay admitted %d entries, want 1", n)
	}
	waitFor(t, "replayed entry dead-lettered by the write retry", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(written) == 1
	})
	mu.Lock()
	w := written[0]
	mu.Unlock()
	if w.batch[0].inbound.ProviderMessageID != "1.0" || w.batch[0].attempts != 1 || !strings.Contains(w.cause.Error(), "422") {
		t.Fatalf("record must carry the entry, its attempt count and the original refusal: ts=%s attempts=%d cause=%v",
			w.batch[0].inbound.ProviderMessageID, w.batch[0].attempts, w.cause)
	}
	if got := calls2(); len(got) != 0 {
		t.Fatalf("the replay must not deliver a refused entry: %v", got)
	}
	if c2.pendingContains("C1", "1.0") {
		t.Fatal("a replayed refused entry must not enter the pending buffer")
	}
}

// Two stripped batch-mates resume their probes oldest first (codex r2
// finding 3): restore() prepends, so without a sort the second stripped
// entry would probe before the first.
func TestStrippedProbesResumeInChronologicalOrder(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch) > 1 {
			return permanent422()
		}
		if len(batch[0].inbound.Attachments) > 0 {
			return permanent422() // refused WITH the attachment, accepted without
		}
		return nil
	})
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPendingWithAttachment("C1", "1.0", "first", "/tmp/a.m4a"))
	c.enqueue("C1", testPendingWithAttachment("C1", "2.0", "second", "/tmp/b.m4a"))
	// Batch refused → probes 1.0, 2.0 refused with attachments → both
	// stripped → the next window probes them again, oldest first.
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0", "2.0", "1.0", "2.0"})
	if got := calls(); len(got) != 5 {
		t.Fatalf("unexpected extra deliveries: %v", got)
	}
}

// A reactions-only transient failure sets the backoff deadline even
// though it arms no timer (codex r2 finding 2), so the reaction
// overflow flush honors it.
func TestReactionsOnlyTransientFailureSetsBackoffDeadline(t *testing.T) {
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.noteTransientFailure("C1", errors.New("dial tcp: connection refused"), "test failure")
	r := testPending("C1", "1.0", "reaction")
	r.reaction = true
	c.restore("C1", []pendingChannelInbound{r})
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.inBackoffLocked("C1") {
		t.Fatal("reactions-only failure must leave the channel in backoff")
	}
	if _, armed := c.timers["C1"]; armed {
		t.Fatal("reactions must still arm no timer (never a solo wake)")
	}
	if len(c.reactions["C1"]) != 1 {
		t.Fatal("the reaction must be back in its side lane")
	}
}

// --- codex r3 on gp-sgu7 ------------------------------------------------------

// A reactions-only transient failure must hold the deadline against
// real messages too (codex r3 finding 1): one enqueued afterwards must
// not arm a plain window under a five-minute deadline, and a plain
// timer armed BEFORE the failure moves out to the deadline.
func TestReactionsOnlyBackoffHoldsAgainstRealMessages(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	// A plain timer armed by a real message BEFORE the reaction failure.
	c.enqueue("C1", testPending("C1", "1.0", "before"))
	for i := 0; i < 10; i++ {
		c.noteTransientFailure("C1", errors.New("dial tcp: connection refused"), "test failure") // a capped run
	}
	r := testPending("C1", "0.5", "reaction")
	r.reaction = true
	c.restore("C1", []pendingChannelInbound{r})
	// A real message enqueued AFTER it.
	c.enqueue("C1", testPending("C1", "2.0", "after"))
	time.Sleep(150 * time.Millisecond)
	if got := calls(); len(got) != 0 {
		t.Fatalf("no POST may happen under the backoff deadline: %v", got)
	}
	c.mu.Lock()
	_, armed := c.timers["C1"]
	pending := len(c.pending["C1"])
	c.mu.Unlock()
	if !armed || pending != 2 {
		t.Fatalf("the backed-off timer must cover both buffered messages: armed=%v pending=%d", armed, pending)
	}
}

// Delivery stays chronological across flagged and plain members (codex
// r3 finding 2): an older plain message that arrived late (slow file
// download) delivers before a newer message's stripped retry.
func TestOlderPlainMessageDeliversBeforeNewerStrippedRetry(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		for _, p := range batch {
			if len(p.inbound.Attachments) > 0 {
				return permanent422()
			}
		}
		return nil
	})
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPendingWithAttachment("C1", "2.0", "newer, refused with its file", "/tmp/b.m4a"))
	waitForCalls(t, calls, []string{"2.0"}) // refused → stripped, flagged, restored
	c.enqueue("C1", testPending("C1", "1.0", "older, its download finished late"))
	waitForCalls(t, calls, []string{"2.0", "1.0", "2.0"})
	if got := calls(); len(got) != 3 {
		t.Fatalf("unexpected extra deliveries: %v", got)
	}
	for _, ts := range []string{"1.0", "2.0"} {
		if c.pendingContains("C1", ts) {
			t.Fatalf("%s still pending", ts)
		}
	}
}

// --- codex r5 on gp-sgu7 ------------------------------------------------------

// HTTP level: the delivery hook collapses same-ts duplicates before
// posting, so a two-entry segment can reach gc as ONE payload. A 422 on
// it must be charged as a single — exactly one POST, then dead-letter —
// never "isolated" by re-posting that single unchanged (codex r5
// finding 1).
func TestRefusedCollapsedDuplicateIsChargedAsSubmitted(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"code":"validation-failed"}`))
	}))
	t.Cleanup(gc.Close)
	cfg := coalescingTestConfig(gc.URL, 20*time.Millisecond)
	dead := make(chan deadLetterCall, 4)
	cfg.coalescer.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		dead <- deadLetterCall{channel, batch, cause}
		return true
	}
	dup := func() pendingChannelInbound {
		return pendingChannelInbound{inbound: externalInboundMessage{ProviderMessageID: "100.000010", Text: "same message, two event ids",
			Conversation: conversationRef{ConversationID: "C1", Kind: "room"}}}
	}
	cfg.coalescer.enqueue("C1", dup())
	cfg.coalescer.enqueue("C1", dup())
	select {
	case call := <-dead:
		if len(call.batch) != 1 || call.batch[0].inbound.ProviderMessageID != "100.000010" {
			t.Fatalf("dead letter must be the one submitted entry: %+v", call.batch)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("refused single never dead-lettered")
	}
	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	got := posts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("gc must see the refused payload exactly once, got %d POSTs", got)
	}
}

// After a batch refusal whose message probes all deliver, a TRANSIENT
// failure on the reaction-group probe puts the channel in backoff
// (codex r5 finding 2) instead of leaving the overflow flush free to
// re-POST on the next reaction.
func TestReactionGroupTransientFailureAfterIsolationSetsBackoff(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		real := false
		for _, p := range batch {
			real = real || !p.reaction
		}
		switch {
		case len(batch) > 1 && real:
			return permanent422()
		case !real:
			return errors.New("dial tcp: connection refused")
		}
		return nil
	})
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "a"))
	c.enqueue("C1", testPending("C1", "2.0", "b"))
	r := testPending("C1", "1.5", "reaction")
	if !c.admitReaction("C1", r, true) {
		t.Fatal("reaction must ride the armed window")
	}
	waitForCalls(t, calls, []string{"1.0,2.0,1.5(r)", "1.0", "2.0", "1.5(r)"}) // take order: pending, then the side lane
	waitFor(t, "channel in backoff after the reaction probe failed transiently", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.inBackoffLocked("C1") && len(c.reactions["C1"]) == 1
	})
}

// --- codex r6 on gp-sgu7 ------------------------------------------------------

// An urgent (mention) arriving while the channel waits out a backoff
// must not flush the buffered batch ahead of it: the urgent message
// proceeds alone, the buffer keeps its deadline and timer.
func TestUrgentFlushAheadHonorsBackoff(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 10) // a capped run: the deadline is five minutes out
	c.enqueue("C1", testPending("C1", "1.0", "buffered"))
	c.mu.Lock()
	g := c.gen["C1"]
	c.mu.Unlock()
	for i := 0; i < 5; i++ { // a mention stream
		if withheld := c.flushAheadOf("C1", "9.0"); len(withheld) != 0 {
			t.Fatalf("nothing withheld when nothing is taken, got %d", len(withheld))
		}
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("the buffered batch must not be re-posted ahead of an urgent message during backoff: %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pendingContainsLocked("C1", "1.0") || c.gen["C1"] != g || c.inflight != 0 {
		t.Fatalf("buffer/timer must be untouched: pending=%v gen=%d inflight=%d", c.pendingContainsLocked("C1", "1.0"), c.gen["C1"], c.inflight)
	}
}

// An urgent waiter queued behind an in-flight POST that then fails
// transiently wakes into a channel in backoff and must not re-post the
// restored batch either.
func TestUrgentWaiterBehindFailingPostHonorsBackoff(t *testing.T) {
	release := make(chan struct{})
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error {
		<-release
		return errors.New("dial tcp: connection refused")
	})
	c := newInboundCoalescer(10*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "buffered"))
	waitFor(t, "timer flush in flight", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.inflight == 1
	})
	done := make(chan struct{})
	go func() {
		c.flushAheadOf("C1", "9.0") // blocks behind the in-flight delivery
		close(done)
	}()
	waitFor(t, "urgent waiter reserved", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.urgentWaiting["C1"] == 1
	})
	// Put the channel deep into a run first, so the failure below backs
	// off to the cap and no timer retry competes with the assertion.
	c.mu.Lock()
	c.transientFailures["C1"] = 10
	c.mu.Unlock()
	close(release) // the in-flight POST fails → backoff → the waiter wakes
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("urgent waiter never returned")
	}
	time.Sleep(50 * time.Millisecond)
	if got := calls(); len(got) != 1 {
		t.Fatalf("only the in-flight POST may have happened, got %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.inBackoffLocked("C1") || !c.pendingContainsLocked("C1", "1.0") || c.urgentWaiting["C1"] != 0 {
		t.Fatalf("channel must stay in backoff with the batch buffered and no waiter left: backoff=%v pending=%v waiting=%d",
			c.inBackoffLocked("C1"), c.pendingContainsLocked("C1", "1.0"), c.urgentWaiting["C1"])
	}
}

// --- codex r7 on gp-sgu7 ------------------------------------------------------

// A deferred flush-ahead still withholds the urgent message's buffered
// twin (finding 1): left buffered, a timer firing during the urgent
// POST would deliver it beside the urgent copy. The rest of the buffer
// keeps its timer generation and deadline.
func TestDeferredFlushAheadStillWithholdsTheTwin(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 10)
	c.enqueue("C1", testPending("C1", "1.0", "older"))
	c.enqueue("C1", testPending("C1", "9.0", "the urgent message's buffered twin"))
	c.mu.Lock()
	g := c.gen["C1"]
	c.mu.Unlock()
	withheld := c.flushAheadOf("C1", "9.0")
	if len(withheld) != 1 || withheld[0].inbound.ProviderMessageID != "9.0" {
		t.Fatalf("the twin must be withheld and returned to the caller, got %+v", withheld)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("no POST under backoff: %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingContainsLocked("C1", "9.0") || !c.pendingContainsLocked("C1", "1.0") || c.gen["C1"] != g {
		t.Fatalf("twin must leave the buffer, the rest stays with its timer: twin=%v older=%v gen=%d want %d",
			c.pendingContainsLocked("C1", "9.0"), c.pendingContainsLocked("C1", "1.0"), c.gen["C1"], g)
	}
}

// The post-urgent reaction drain honors the backoff deadline (finding
// 2): a successful mention during an outage must not POST the side
// lane at mention cadence.
func TestBufferedReactionDrainHonorsBackoff(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	r := testPending("C1", "1.0", "reaction")
	if !c.admitReaction("C1", r, false) {
		t.Fatal("admit")
	}
	enterBackoff(c, "C1", 10)
	for i := 0; i < 3; i++ {
		c.deliverBufferedReactions("C1")
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("the reaction drain must wait out the backoff: %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reactions["C1"]) != 1 {
		t.Fatalf("reaction must stay in its side lane, got %d", len(c.reactions["C1"]))
	}
}

// Deferral logging is keyed by the effective backoff delay (finding 3):
// consecutive capped failures with mentions between them log once.
func TestUrgentDeferLogWorthyKeyedByDelay(t *testing.T) {
	c := newInboundCoalescer(8*time.Second, nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.transientFailures["C1"] = 1
	if !c.urgentDeferLogWorthyLocked("C1") {
		t.Fatal("first deferral logs")
	}
	if c.urgentDeferLogWorthyLocked("C1") {
		t.Fatal("same state again is silent")
	}
	c.transientFailures["C1"] = 2
	if !c.urgentDeferLogWorthyLocked("C1") {
		t.Fatal("a doubled delay is a new state")
	}
	c.transientFailures["C1"] = 10 // at the cap
	if !c.urgentDeferLogWorthyLocked("C1") {
		t.Fatal("reaching the cap is a new state")
	}
	for n := 11; n < 20; n++ {
		c.transientFailures["C1"] = n
		if c.urgentDeferLogWorthyLocked("C1") {
			t.Fatalf("failure #%d at the cap must not log again", n)
		}
	}
}

// --- codex r8 on gp-sgu7: one scheduler, one deadline writer -----------------
//
// Rounds 1–8 each found another path that armed a timer or moved the
// deadline on its own. The shape changed (mayor's rule after repeated
// gate failures on one predicate): scheduleLocked is the only place a
// timer is armed, moved or disarmed, noteTransientFailure the only
// writer of the deadline, and timerWorthyLocked the only judge of
// whether a channel may POST on a timer at all. TestScheduleTable is
// that contract's spec; the tests after it are the r8 findings through
// the real event path.

// enterBackoff puts the channel into a run of n transient failures
// through the production path — noteTransientFailure sets the
// deadline, scheduleLocked places whatever is buffered on it — so a
// test never hand-arms a timer the scheduler does not know about.
func enterBackoff(c *inboundCoalescer, channel string, n int) {
	for i := 0; i < n; i++ {
		c.noteTransientFailure(channel, errors.New("dial tcp: connection refused"), "test failure")
	}
	c.mu.Lock()
	c.scheduleLocked(channel, time.Time{})
	c.mu.Unlock()
}

func TestScheduleTable(t *testing.T) {
	const window = time.Minute
	real := testPending("C1", "1.0", "real")
	riding := testPending("C1", "0.5", "reaction riding an armed window")
	riding.reaction = true
	rows := []struct {
		name        string
		pending     []pendingChannelInbound
		reactions   int           // side-lane depth
		armedIn     time.Duration // an existing timer's target from now; 0 = none
		deadlineIn  time.Duration // retryNotBefore from now; 0 = none
		wantIn      time.Duration // the caller's `at` from now; 0 = zero want
		wantArmed   bool
		wantTarget  time.Duration // the armed target from now (±1 s)
		wantChanged bool
		wantLane    int // side-lane depth afterwards, when the row moves riders (0 = not checked)
	}{
		{name: "empty buffer with a want: nothing to flush, no timer", wantIn: window},
		{name: "reactions below the cap: no timer, ever", reactions: 5, wantIn: window},
		{name: "real message, nothing armed, no deadline: the window", pending: []pendingChannelInbound{real}, wantIn: window, wantArmed: true, wantTarget: window, wantChanged: true},
		{name: "real message, timer armed, a later want: unchanged (the window belongs to the first message)", pending: []pendingChannelInbound{real}, armedIn: 30 * time.Second, wantIn: window, wantArmed: true, wantTarget: 30 * time.Second},
		{name: "real message, digest timer armed, a short want: pulled in (the poll behind an in-flight delivery)", pending: []pendingChannelInbound{real}, armedIn: 2 * time.Hour, wantIn: time.Second, wantArmed: true, wantTarget: time.Second, wantChanged: true},
		{name: "real message, plain timer under a deadline, zero want: moved out to the deadline (r3 f1)", pending: []pendingChannelInbound{real}, armedIn: 8 * time.Second, deadlineIn: 5 * time.Minute, wantArmed: true, wantTarget: 5 * time.Minute, wantChanged: true},
		{name: "real message, timer at the deadline, zero want: unchanged (a restore with no new failure keeps the deadline, r8 f1)", pending: []pendingChannelInbound{real}, armedIn: 5 * time.Minute, deadlineIn: 5 * time.Minute, wantArmed: true, wantTarget: 5 * time.Minute},
		{name: "real message, timer at the deadline, a short want: still the deadline (a poll or reconcile cannot shorten it, r1 f2)", pending: []pendingChannelInbound{real}, armedIn: 5 * time.Minute, deadlineIn: 5 * time.Minute, wantIn: time.Second, wantArmed: true, wantTarget: 5 * time.Minute},
		{name: "real message, nothing armed, a deadline: the deadline (enqueued during a reactions-only backoff, r3 f1)", pending: []pendingChannelInbound{real}, deadlineIn: 5 * time.Minute, wantIn: window, wantArmed: true, wantTarget: 5 * time.Minute, wantChanged: true},
		{name: "overflowed reaction lane, a deadline, nothing armed: the deadline (r8 f2)", reactions: maxBufferedReactionsPerChannel, deadlineIn: 5 * time.Minute, wantArmed: true, wantTarget: 5 * time.Minute, wantChanged: true},
		{name: "overflowed reaction lane, no deadline, zero want: the window", reactions: maxBufferedReactionsPerChannel, wantArmed: true, wantTarget: window, wantChanged: true},
		{name: "only riding reactions left in pending under an armed timer: disarmed, the riders back in the side lane (r8 f3, r11 f3)", pending: []pendingChannelInbound{riding}, armedIn: 30 * time.Second, wantChanged: true, wantLane: 1},
		{name: "only riding reactions left in pending, nothing armed: no timer, the riders back in the side lane where the cap counts them (r11 f3)", pending: []pendingChannelInbound{riding}, wantLane: 1},
		{name: "riding reactions left beside an overflowed lane: they join it, the overflow timer stands", pending: []pendingChannelInbound{riding}, reactions: maxBufferedReactionsPerChannel, wantArmed: true, wantTarget: window, wantChanged: true, wantLane: maxBufferedReactionsPerChannel + 1},
		{name: "a deadline already passed does not bind", pending: []pendingChannelInbound{real}, deadlineIn: -time.Minute, wantIn: window, wantArmed: true, wantTarget: window, wantChanged: true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := newInboundCoalescer(window, nil)
			c.mu.Lock()
			defer c.mu.Unlock()
			now := time.Now()
			c.pending["C1"] = append([]pendingChannelInbound(nil), row.pending...)
			for i := 0; i < row.reactions; i++ {
				c.reactions["C1"] = append(c.reactions["C1"], riding)
			}
			if row.armedIn != 0 {
				c.due["C1"] = now.Add(row.armedIn)
				c.timers["C1"] = time.AfterFunc(time.Hour, func() {})
			}
			if row.deadlineIn != 0 {
				c.retryNotBefore["C1"] = now.Add(row.deadlineIn)
			}
			var at time.Time
			if row.wantIn != 0 {
				at = now.Add(row.wantIn)
			}
			target, changed := c.scheduleLocked("C1", at)
			tm, armed := c.timers["C1"]
			if armed {
				defer tm.Stop()
			}
			if armed != row.wantArmed || changed != row.wantChanged {
				t.Fatalf("armed=%v changed=%v, want armed=%v changed=%v", armed, changed, row.wantArmed, row.wantChanged)
			}
			if row.wantLane > 0 && (len(c.pending["C1"]) != 0 || len(c.reactions["C1"]) != row.wantLane) {
				t.Fatalf("pending=%d lane=%d, want pending=0 lane=%d (a buffer with no real message holds no riders)", len(c.pending["C1"]), len(c.reactions["C1"]), row.wantLane)
			}
			if !armed {
				if _, ok := c.due["C1"]; ok {
					t.Fatal("no target may be recorded without a timer")
				}
				return
			}
			if got := target.Sub(now); got < row.wantTarget-time.Second || got > row.wantTarget+time.Second {
				t.Fatalf("target in %s, want %s", got.Round(time.Millisecond), row.wantTarget)
			}
			if !c.due["C1"].Equal(target) {
				t.Fatalf("recorded target %v differs from the returned one %v", c.due["C1"], target)
			}
		})
	}
}

// A withheld twin handed back after its urgent copy failed keeps the
// channel's deadline exactly where it was (r8 finding 1): a stream of
// failed mentions can no longer postpone the buffered messages' retry.
func TestReturnedTwinKeepsTheDeadline(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return errors.New("dial tcp: connection refused") })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 10)
	c.enqueue("C1", testPending("C1", "1.0", "older"))
	c.mu.Lock()
	deadline := c.retryNotBefore["C1"]
	g := c.gen["C1"]
	c.mu.Unlock()
	for i := 0; i < 5; i++ { // a mention stream: each twin buffered, withheld, and handed back by a failed mention
		ts := fmt.Sprintf("%d.0", i+2)
		c.enqueue("C1", testPending("C1", ts, "twin"))
		withheld := c.flushAheadOf("C1", ts)
		if len(withheld) != 1 || withheld[0].inbound.ProviderMessageID != ts {
			t.Fatalf("twin %s must be withheld and returned, got %+v", ts, withheld)
		}
		c.restore("C1", withheld) // the urgent copy failed: main hands the twin back
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("no POST under the deadline: %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.retryNotBefore["C1"].Equal(deadline) {
		t.Fatalf("the deadline moved without a new failure: %v → %v", deadline, c.retryNotBefore["C1"])
	}
	if !c.due["C1"].Equal(deadline) || c.gen["C1"] != g {
		t.Fatalf("the timer must still aim at the deadline unchanged: due=%v gen %d→%d", c.due["C1"], g, c.gen["C1"])
	}
	if n := len(c.pending["C1"]); n != 6 {
		t.Fatalf("all six messages must be buffered for the retry, got %d", n)
	}
}

// A reaction lane that overflows during a reactions-only backoff is
// timer-worthy: the scheduler places its flush AT the deadline, and it
// delivers with no further traffic (r8 finding 2).
func TestReactionOverflowDuringBackoffDeliversAtDeadline(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c := newInboundCoalescer(50*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 3) // deadline 200 ms out
	c.mu.Lock()
	deadline := c.retryNotBefore["C1"]
	c.mu.Unlock()
	for i := 0; i < maxBufferedReactionsPerChannel; i++ {
		r := testPending("C1", fmt.Sprintf("%d.0", i+1), "reaction")
		r.reaction = true
		c.admitReaction("C1", r, false)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("no POST under the deadline: %v", got)
	}
	c.mu.Lock()
	due, armed := c.due["C1"]
	c.mu.Unlock()
	if !armed || !due.Equal(deadline) {
		t.Fatalf("the overflow flush must be armed at the deadline: armed=%v due=%v deadline=%v", armed, due, deadline)
	}
	waitFor(t, "overflow delivered at the deadline", func() bool { return len(calls()) == 1 })
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reactions["C1"]) != 0 || c.inBackoffLocked("C1") {
		t.Fatalf("the lane must be drained and the run over: reactions=%d backoff=%v", len(c.reactions["C1"]), c.inBackoffLocked("C1"))
	}
}

// Withholding the only real message leaves nothing timer-worthy: the
// timer is disarmed, and a below-cap reaction never POSTs alone at the
// old expiry (r8 finding 3). Handing the twin back re-arms it — at the
// deadline — through the same scheduler.
func TestWithholdingTheSoleTwinDisarmsTheTimer(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 3) // deadline 80 ms out
	c.enqueue("C1", testPending("C1", "9.0", "the urgent message's buffered twin"))
	r := testPending("C1", "1.0", "reaction")
	r.reaction = true
	c.admitReaction("C1", r, false)
	withheld := c.flushAheadOf("C1", "9.0")
	if len(withheld) != 1 {
		t.Fatalf("the twin must be withheld, got %d", len(withheld))
	}
	c.mu.Lock()
	_, armed := c.timers["C1"]
	_, due := c.due["C1"]
	c.mu.Unlock()
	if armed || due {
		t.Fatalf("nothing timer-worthy remains: armed=%v due=%v", armed, due)
	}
	time.Sleep(250 * time.Millisecond)
	if got := calls(); len(got) != 0 {
		t.Fatalf("a below-cap reaction must never POST alone after the twin left the buffer, got %v", got)
	}
	c.restore("C1", withheld) // the urgent copy failed
	waitFor(t, "twin and reaction delivered together at the deadline", func() bool { return len(calls()) == 1 })
}

// A successful delivery lifts the deadline AND pulls the buffered
// retry in to the plain window: gc just proved itself reachable, so
// the buffer does not wait out a five-minute deadline.
func TestRecoveryPullsTheBufferedRetryIn(t *testing.T) {
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	enterBackoff(c, "C1", 10)
	c.enqueue("C1", testPending("C1", "1.0", "buffered during the outage"))
	c.deliveredOK("C1") // an urgent mention on the channel just succeeded
	waitForCalls(t, calls, []string{"1.0"})
}

// --- codex r9 on gp-sgu7 ------------------------------------------------------

// The scheduler's zero-want fallback is the deadline when one is ahead
// (r9 finding 2): a restore or an overflow arriving with the deadline
// less than a window away retries AT the deadline, not a window past
// it; a fresh message still waits its own window. A reconcile keeps
// the nearer of the established target and the new policy's window.
func TestScheduleFallbackAndReconcileKeepTheNearerTarget(t *testing.T) {
	const window = time.Minute
	real := testPending("C1", "1.0", "real")
	riding := testPending("C1", "0.5", "reaction")
	riding.reaction = true
	within := func(got time.Time, now time.Time, want time.Duration) bool {
		d := got.Sub(now)
		return d >= want-time.Second && d <= want+time.Second
	}
	t.Run("zero want under a near deadline lands on the deadline", func(t *testing.T) {
		c := newInboundCoalescer(window, nil)
		c.mu.Lock()
		defer c.mu.Unlock()
		now := time.Now()
		c.pending["C1"] = []pendingChannelInbound{real}
		c.retryNotBefore["C1"] = now.Add(30 * time.Second)
		target, _ := c.scheduleLocked("C1", time.Time{})
		defer c.timers["C1"].Stop()
		if !within(target, now, 30*time.Second) {
			t.Fatalf("target in %s, want the 30 s deadline", target.Sub(now).Round(time.Millisecond))
		}
	})
	t.Run("an overflowed lane under a near deadline lands on the deadline", func(t *testing.T) {
		c := newInboundCoalescer(window, nil)
		c.mu.Lock()
		defer c.mu.Unlock()
		now := time.Now()
		for i := 0; i < maxBufferedReactionsPerChannel; i++ {
			c.reactions["C1"] = append(c.reactions["C1"], riding)
		}
		c.retryNotBefore["C1"] = now.Add(30 * time.Second)
		target, _ := c.scheduleLocked("C1", time.Time{})
		defer c.timers["C1"].Stop()
		if !within(target, now, 30*time.Second) {
			t.Fatalf("target in %s, want the 30 s deadline", target.Sub(now).Round(time.Millisecond))
		}
	})
	t.Run("a fresh message under a near deadline still waits its window", func(t *testing.T) {
		c := newInboundCoalescer(window, nil)
		c.mu.Lock()
		defer c.mu.Unlock()
		now := time.Now()
		c.pending["C1"] = []pendingChannelInbound{real}
		c.retryNotBefore["C1"] = now.Add(30 * time.Second)
		target, _ := c.scheduleLocked("C1", now.Add(window))
		defer c.timers["C1"].Stop()
		if !within(target, now, window) {
			t.Fatalf("target in %s, want the window", target.Sub(now).Round(time.Millisecond))
		}
	})
	t.Run("reconcile keeps a nearer established target", func(t *testing.T) {
		c := newInboundCoalescer(window, nil)
		c.mu.Lock()
		now := time.Now()
		c.pending["C1"] = []pendingChannelInbound{real}
		c.due["C1"] = now.Add(8 * time.Second)
		c.timers["C1"] = time.AfterFunc(time.Hour, func() {})
		c.mu.Unlock()
		c.reconcileTimers()
		c.mu.Lock()
		defer c.mu.Unlock()
		defer c.timers["C1"].Stop()
		if !within(c.due["C1"], now, 8*time.Second) {
			t.Fatalf("reconcile moved an 8 s retry to %s", c.due["C1"].Sub(now).Round(time.Millisecond))
		}
	})
	t.Run("reconcile pulls a stale digest target in to the new window", func(t *testing.T) {
		c := newInboundCoalescer(window, nil)
		c.mu.Lock()
		now := time.Now()
		c.pending["C1"] = []pendingChannelInbound{real}
		c.due["C1"] = now.Add(2 * time.Hour)
		c.timers["C1"] = time.AfterFunc(time.Hour, func() {})
		c.mu.Unlock()
		c.reconcileTimers()
		c.mu.Lock()
		defer c.mu.Unlock()
		defer c.timers["C1"].Stop()
		if !within(c.due["C1"], now, window) {
			t.Fatalf("reconcile left a two-hour target at %s", c.due["C1"].Sub(now).Round(time.Millisecond))
		}
	})
}

// A same-ts duplicate admitted while the original waits out a backoff
// flagged for isolation must not post the refused bytes again once the
// original is dead-lettered (r9 finding 1): the take collapses same-ts
// copies to the one whose rejection state has progressed furthest.
func TestDuplicateAdmittedDuringBackoffNeverRepostsRefusedBytes(t *testing.T) {
	var mu sync.Mutex
	probesOfA := 0
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch) == 1 && batch[0].inbound.ProviderMessageID == "1.0" {
			mu.Lock()
			probesOfA++
			n := probesOfA
			mu.Unlock()
			if n == 1 {
				return errors.New("dial tcp: connection refused") // the first probe pauses isolation
			}
			return permanent422()
		}
		for _, p := range batch {
			if p.inbound.ProviderMessageID == "1.0" {
				return permanent422() // any batch carrying A is refused
			}
		}
		return nil
	})
	c := newInboundCoalescer(150*time.Millisecond, nil)
	c.deliver = deliver
	var dead []string
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range batch {
			dead = append(dead, p.inbound.ProviderMessageID)
		}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "A"))
	c.enqueue("C1", testPending("C1", "2.0", "B"))
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0"}) // batch refused, A's probe paused transiently
	c.enqueue("C1", testPending("C1", "1.0", "A again — a same-ts duplicate during the backoff"))
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0", "1.0", "2.0"})
	time.Sleep(400 * time.Millisecond) // two more windows: nothing may follow
	if got := calls(); len(got) != 4 {
		t.Fatalf("the refused bytes of A were posted again after its dead-letter: %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dead) != 1 || dead[0] != "1.0" {
		t.Fatalf("exactly one dead-letter record for A, got %v", dead)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending["C1"]) != 0 {
		t.Fatalf("nothing may stay buffered, got %d", len(c.pending["C1"]))
	}
}

// A parked dead-letter write carries a deletion that landed while the
// write was owed (r9 finding 3): the retried record is the notice, not
// the deleted text.
func TestParkedDeadLetterWriteCarriesADeletion(t *testing.T) {
	var mu sync.Mutex
	var written []string
	writes := 0
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		mu.Lock()
		defer mu.Unlock()
		writes++
		if writes == 1 {
			return false // the first write is not confirmed: the entry parks
		}
		for _, p := range batch {
			written = append(written, p.inbound.Text)
		}
		return true
	}
	c.charge("C1", testPending("C1", "1.0", "the founder's deleted words"), permanent422())
	c.markDeleted("C1", "1.0") // deleted while the write is owed
	waitFor(t, "parked write retried", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(written) == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if written[0] != deletedBySenderNotice {
		t.Fatalf("the parked write must carry the deletion notice, got %q", written[0])
	}
}

// --- codex r10 on gp-sgu7: the verdict travels with the message ---------------

// A duplicate copy enqueued while the original's POST is in flight is
// dropped when gc refuses the original and the ladder dead-letters it
// (r10 finding 1): the refused bytes never go out under the copy's
// name next window.
func TestDuplicateEnqueuedDuringRefusedPostIsDroppedWithTheVerdict(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		first := false
		once.Do(func() { first = true })
		if first {
			entered <- struct{}{}
			<-release
			return permanent422()
		}
		return nil
	})
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	var dead []string
	var mu sync.Mutex
	c.deadLetter = func(channel string, batch []pendingChannelInbound, cause error) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range batch {
			dead = append(dead, p.inbound.ProviderMessageID)
		}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "A"))
	<-entered // A's POST is blocked in flight
	c.enqueue("C1", testPending("C1", "1.0", "A again — a duplicate while the POST is in flight"))
	c.enqueue("C1", testPending("C1", "2.0", "B, a later message"))
	close(release) // gc refuses A: no attachments to strip → dead-letter at once
	waitForCalls(t, calls, []string{"1.0", "2.0"})
	time.Sleep(100 * time.Millisecond)
	if got := calls(); len(got) != 2 {
		t.Fatalf("the duplicate copy of A must never POST: %v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(dead) != 1 || dead[0] != "1.0" {
		t.Fatalf("exactly one dead-letter record for A, got %v", dead)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending["C1"]) != 0 {
		t.Fatalf("nothing may stay buffered, got %d", len(c.pending["C1"]))
	}
}

// A copy admitted after the ladder dead-lettered its ts is dropped at
// admission; a copy admitted while the stripped retry is owed enters AS
// the stripped, isolated retry (r10 finding 1, the admission side).
func TestAdmissionAdoptsTheLadderVerdict(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	// Retired: the ladder dead-lettered ts 1.0 (nothing to strip).
	c.charge("C1", testPending("C1", "1.0", "plain, refused"), permanent422())
	c.enqueue("C1", testPending("C1", "1.0", "a late duplicate of the dead-lettered message"))
	// Stripped: ts 2.0 was refused with an attachment; its retry is owed.
	c.charge("C1", testPendingWithAttachment("C1", "2.0", "with a file", "/tmp/x/memo.m4a"), permanent422())
	c.enqueue("C1", testPendingWithAttachment("C1", "2.0", "with a file", "/tmp/x/memo.m4a"))
	c.mu.Lock()
	retiredBuffered := c.pendingContainsLocked("C1", "1.0")
	copies := 0
	var bad string
	for _, p := range c.pending["C1"] {
		if p.inbound.ProviderMessageID != "2.0" {
			continue
		}
		copies++
		if len(p.inbound.Attachments) != 0 || !p.isolate || p.attempts < 1 {
			bad = fmt.Sprintf("attachments=%d isolate=%v attempts=%d", len(p.inbound.Attachments), p.isolate, p.attempts)
		}
	}
	c.mu.Unlock()
	if retiredBuffered {
		t.Fatal("a copy of a dead-lettered ts must not enter the buffer")
	}
	if bad != "" {
		t.Fatalf("every buffered copy of a stripped ts must be the stripped, isolated retry: %s", bad)
	}
	if copies != 2 {
		t.Fatalf("both copies of 2.0 buffered as stripped retries (the take collapses them), got %d", copies)
	}
	if !c.onLadder("C1", "1.0") || !c.onLadder("C1", "2.0") || c.onLadder("C1", "3.0") {
		t.Fatal("onLadder must report both verdicts and nothing else")
	}
}

// A delayed urgent twin of a message whose buffered copy is already on
// the ladder does not withhold that copy, and the urgent path is told
// the ladder owns the message (r10 finding 2): the stripped retry — not
// the fresh event's attachments — is what reaches gc.
func TestUrgentTwinDefersToTheLadder(t *testing.T) {
	var mu sync.Mutex
	probesOfB := 0
	var strippedPosts []int // attachment counts of every single-A POST after the refusal
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch) == 2 {
			return permanent422() // the batch A+B is refused
		}
		mu.Lock()
		defer mu.Unlock()
		switch batch[0].inbound.ProviderMessageID {
		case "1.0":
			if len(batch[0].inbound.Attachments) > 0 {
				return permanent422() // A with its attachment is refused
			}
			strippedPosts = append(strippedPosts, len(batch[0].inbound.Attachments))
			return nil
		case "2.0":
			probesOfB++
			if probesOfB == 1 {
				return errors.New("dial tcp: connection refused") // B's probe pauses: A's stripped copy stays buffered in backoff
			}
		}
		return nil
	})
	c := newInboundCoalescer(100*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPendingWithAttachment("C1", "1.0", "A with a voice memo", "/tmp/x/memo.m4a"))
	c.enqueue("C1", testPending("C1", "2.0", "B"))
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0", "2.0"}) // refused; A stripped; B paused → backoff
	c.mu.Lock()
	inBackoff := c.inBackoffLocked("C1")
	c.mu.Unlock()
	if !inBackoff {
		t.Fatal("the channel must be in backoff with A's stripped copy buffered")
	}
	// The delayed app_mention twin of A arrives on the urgent path.
	if withheld := c.flushAheadOf("C1", "1.0"); len(withheld) != 0 {
		t.Fatalf("a copy on the ladder is never withheld for an urgent twin, got %d", len(withheld))
	}
	if !c.onLadder("C1", "1.0") {
		t.Fatal("the urgent path must be told the ladder owns this message")
	}
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0", "2.0", "1.0", "2.0"})
	mu.Lock()
	defer mu.Unlock()
	if len(strippedPosts) != 1 || strippedPosts[0] != 0 {
		t.Fatalf("exactly one stripped POST of A (no attachments), got %v", strippedPosts)
	}
	if !c.onLadder("C1", "1.0") {
		t.Fatal("a message delivered without its attachments stays the ladder's (r11 finding 1): its urgent copy carries the refused bytes and must still skip")
	}
}

// A withheld urgent twin handed back after the buffered copy's ts was
// retired meanwhile is dropped, not re-queued.
func TestReturnedTwinOfARetiredMessageIsDropped(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	twin := testPending("C1", "1.0", "the twin")
	c.charge("C1", testPending("C1", "1.0", "plain, refused"), permanent422())
	c.restore("C1", []pendingChannelInbound{twin})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pendingContainsLocked("C1", "1.0") {
		t.Fatal("a handed-back copy of a dead-lettered ts must not re-enter the buffer")
	}
	if _, armed := c.timers["C1"]; armed {
		t.Fatal("nothing buffered, nothing armed")
	}
}

// --- codex r11 ---------------------------------------------------------------

// A message the ladder delivered WITHOUT its attachments stays the
// ladder's (r11 finding 1): clearing the verdict on that success let
// an urgent twin, built from the fresh event with the original
// attachments, post the refused bytes right after the flush-ahead
// delivered the stripped copy — onLadder said no, so main.go posted.
// The verdict now lands as "delivered without attachments": onLadder
// stays true, and a fresh copy admitted or handed back is dropped as a
// duplicate of a delivered message, never posted.
func TestStrippedDeliveryKeepsTheLadderVerdict(t *testing.T) {
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch[0].inbound.Attachments) > 0 {
			return permanent422()
		}
		return nil
	})
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	fresh := func() pendingChannelInbound {
		return testPendingWithAttachment("C1", "1.0", "A with a voice memo", "/tmp/x/memo.m4a")
	}
	// The ladder's shape after the refusal: the stripped retry is owed and buffered.
	c.charge("C1", fresh(), permanent422())
	// The delayed app_mention twin's flush-ahead delivers the stripped copy.
	if withheld := c.flushAheadOf("C1", "1.0"); len(withheld) != 0 {
		t.Fatalf("a copy on the ladder is never withheld, got %d", len(withheld))
	}
	waitForCalls(t, calls, []string{"1.0"})
	if !c.onLadder("C1", "1.0") {
		t.Fatal("after the stripped delivery the ladder must still own the message — the urgent copy asks next and carries the refused bytes")
	}
	// Every later copy of the message — a redelivery admitted, an urgent
	// twin handed back — is a duplicate of a delivered message.
	c.enqueue("C1", fresh())
	c.restore("C1", []pendingChannelInbound{fresh()})
	if c.pendingContains("C1", "1.0") {
		t.Fatal("a copy of a message delivered without its attachments must not re-enter the buffer")
	}
	if got := calls(); len(got) != 1 {
		t.Fatalf("the refused bytes must never go out again: %v", got)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, armed := c.timers["C1"]; armed {
		t.Fatal("nothing buffered, nothing armed")
	}
}

// The urgent path's ladder skip RELEASES the (channel, ts) claim it
// holds (r11 finding 2): begin() had granted this goroutine the claim
// before flushAheadOf, and skipping the POST without concluding it
// left it open — a later same-ts copy parked on it until the bounded
// wait gave up. Released, not committed: nothing this copy did reached
// gc, so the claim vouches for nothing; the next same-ts copy asks the
// ladder itself and gets the same answer. No busy mark is taken (no
// reply is promised: the stripped retry may land later, the
// dead-letter file never answers), and gc sees nothing from this copy.
func TestUrgentLadderSkipReleasesTheClaim(t *testing.T) {
	stub := &flakyInboundStub{}
	gcSrv := httptest.NewServer(stub.handler())
	t.Cleanup(gcSrv.Close)

	cfg := coalescingTestConfig(gcSrv.URL, time.Hour)
	cfg.channelClaims = newEventDedupCache(eventDedupTTL)
	cfg.coalescer.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	// The ladder retired the message (a plain refusal: dead-lettered at once).
	cfg.coalescer.charge("C1", testPending("C1", "100.000300", "refused"), permanent422())

	aliasReg := newTestHandleAliasRegistry(t)
	env := botMentionEnvelope(t, "app_mention", "Ev1", "C1", "100.000300", "",
		"<@"+testBotUserID+"> the delayed twin", true)
	processSlackEvent(cfg, aliasReg, nil, nil, nil, nil, env, func() {})

	if got := stub.snapshot(); len(got) != 0 {
		t.Fatalf("the urgent copy of a message the ladder owns must not POST, gc saw %d", len(got))
	}
	proceed, wait := cfg.channelClaims.begin(channelDeliveryClaimKey("C1", "100.000300"))
	if !proceed {
		t.Fatalf("the claim must be RELEASED by the ladder skip (the next copy asks the ladder itself), got proceed=%v open=%v", proceed, wait != nil)
	}
	if cfg.deliveredIDs.seen("", "C1", "100.000300") {
		t.Error("a skipped copy vouches for nothing — the ts must not be recorded as delivered")
	}
}

// Retiring the last real message of a buffer returns the reactions
// that were riding its armed window to the no-wake side lane (r11
// finding 3): left in pending, the scheduler — which counts only the
// side lane toward the overflow cap — neither timed them nor counted
// them, so they sat until the channel's next real message. Shape: a
// duplicate of A admitted while A's POST is in flight arms a window; a
// founder ack rides it; A is refused and retired, the duplicate leaves
// with the verdict; the ack must be back in the lane and count.
func TestRetiringTheLastRealMessageReturnsRidingReactionsToTheLane(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var once sync.Once
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		first := false
		once.Do(func() { first = true })
		if first {
			entered <- struct{}{}
			<-release
			return permanent422()
		}
		return nil
	})
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c.enqueue("C1", testPending("C1", "1.0", "A"))
	<-entered // A's POST is blocked in flight
	c.enqueue("C1", testPending("C1", "1.0", "A again — a duplicate while the POST is in flight; arms a window"))
	if !c.admitReaction("C1", testPending("C1", "1.5", "a founder ack"), true) {
		t.Fatal("admission refused")
	}
	c.mu.Lock()
	riding := len(c.pending["C1"]) == 2
	c.mu.Unlock()
	if !riding {
		t.Fatal("the ack must be riding the duplicate's armed window")
	}
	close(release) // gc refuses A: nothing to strip → retired; the duplicate leaves with the verdict
	waitForCalls(t, calls, []string{"1.0"})
	waitFor(t, "the rider back in the side lane", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.pending["C1"]) == 0 && len(c.reactions["C1"]) == 1
	})
	c.mu.Lock()
	_, armed := c.timers["C1"]
	c.mu.Unlock()
	if armed {
		t.Fatal("a below-cap lane arms no timer")
	}
	// It counts: the lane overflows at the cap, and the flush carries it.
	for i := 1; i < maxBufferedReactionsPerChannel; i++ {
		c.admitReaction("C1", testPending("C1", fmt.Sprintf("2.%03d", i), "r"), false)
	}
	waitFor(t, "the overflow flush", func() bool { return len(calls()) == 2 })
	if got := calls()[1]; !strings.HasPrefix(got, "1.5(r),") || strings.Count(got, "(r)") != maxBufferedReactionsPerChannel {
		t.Fatalf("the overflow flush must carry the returned rider with the lane: %s", got)
	}
}

// The spool replay's re-park restores the retired verdict (r11 finding
// 4): the ledger is memory, the park is the ladder's retirement decided
// before the restart, and without the verdict a redelivery of the
// refused message admitted after the restart entered delivery as a
// plain copy and posted the refused bytes.
func TestSpoolReplayRestoresTheRetiredVerdict(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	deliver1, calls1 := recordingDeliver(func([]pendingChannelInbound) error { return permanent422() })
	c1 := newInboundCoalescer(time.Hour, nil)
	c1.deliver = deliver1
	c1.deadLetter = func(string, []pendingChannelInbound, error) bool { return false } // disk full until restart
	c1.spill = spool.spillBatch
	c1.enqueue("C1", testPending("C1", "1.0", "poison"))
	c1.flushAll()
	waitForCalls(t, calls1, []string{"1.0"})

	deliver2, calls2 := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c2 := newInboundCoalescer(time.Hour, nil)
	c2.deliver = deliver2
	c2.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	if n := spool.replayInto(c2); n != 1 {
		t.Fatalf("replay admitted %d entries, want 1", n)
	}
	if !c2.onLadder("C1", "1.0") {
		t.Fatal("the re-parked entry's retirement must be the ledger's verdict after the restart")
	}
	c2.enqueue("C1", testPending("C1", "1.0", "a redelivery after the restart"))
	c2.restore("C1", []pendingChannelInbound{testPending("C1", "1.0", "a twin handed back after the restart")})
	if c2.pendingContains("C1", "1.0") {
		t.Fatal("a copy of a message retired before the restart must not enter the buffer")
	}
	if got := calls2(); len(got) != 0 {
		t.Fatalf("the refused bytes must never go out after the restart: %v", got)
	}
}

// --- codex r12 ---------------------------------------------------------------

// A stripped retry spooled at shutdown carries its verdict across the
// restart (r12 finding 1): the spool kept the isolate flag but not the
// disposition, so after the replayed stripped copy landed, landVerdicts
// found no verdict to settle, onLadder said no, and an urgent twin
// posted the original attachments. The disposition now travels on the
// entry (stripped) and the replay seeds the verdict before admission.
func TestSpoolReplayRestoresTheStrippedVerdict(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	deliver1, calls1 := recordingDeliver(func(batch []pendingChannelInbound) error {
		if len(batch[0].inbound.Attachments) > 0 {
			return permanent422()
		}
		return errors.New("dial tcp: connection refused") // gc is down before the stripped retry lands
	})
	c1 := newInboundCoalescer(20*time.Millisecond, nil)
	c1.deliver = deliver1
	c1.spill = spool.spillBatch
	c1.enqueue("C1", testPendingWithAttachment("C1", "1.0", "A with a voice memo", "/tmp/x/memo.m4a"))
	waitForCalls(t, calls1, []string{"1.0", "1.0"}) // refused with the file; the stripped retry fails transiently
	c1.flushAll()                                   // the stripped retry is spooled, its disposition with it

	deliver2, calls2 := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c2 := newInboundCoalescer(20*time.Millisecond, nil)
	c2.deliver = deliver2
	if n := spool.replayInto(c2); n != 1 {
		t.Fatalf("replay admitted %d entries, want 1", n)
	}
	waitForCalls(t, calls2, []string{"1.0"}) // the replayed stripped copy lands
	if !c2.onLadder("C1", "1.0") {
		t.Fatal("after the replayed stripped copy lands the ladder must still own the message — the urgent twin asks next, original attachments and all")
	}
	c2.enqueue("C1", testPendingWithAttachment("C1", "1.0", "A with a voice memo", "/tmp/x/memo.m4a"))
	time.Sleep(60 * time.Millisecond)
	if got := calls2(); len(got) != 1 {
		t.Fatalf("the refused bytes must never go out after the restart: %v", got)
	}
}

// Verdict retention (r12 finding 2): an outstanding stripped retry never
// expires by time — the retry is the message's delivery, however long
// the outage; a terminal verdict (dead-lettered, delivered without
// attachments) is kept for Slack's whole redelivery window (delayed
// events and retries for up to 24 hours), so a late redelivery of a
// refused message is still a duplicate, not fresh bytes.
func TestVerdictRetentionTable(t *testing.T) {
	rows := []struct {
		name string
		v    ladderVerdict
		age  time.Duration
		want bool
	}{
		{"an outstanding stripped retry never expires by time", ladderVerdict{}, 30 * time.Hour, true},
		{"a dead-letter inside Slack's 24 h redelivery window", ladderVerdict{retired: true}, 23 * time.Hour, true},
		{"a stripped delivery inside the window", ladderVerdict{retired: true, delivered: true}, 23 * time.Hour, true},
		{"a dead-letter past the window", ladderVerdict{retired: true}, 26 * time.Hour, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			c := newInboundCoalescer(time.Hour, nil)
			c.mu.Lock()
			defer c.mu.Unlock()
			now := time.Now()
			c.recordVerdictLocked("C1", "1.0", row.v, now.Add(-row.age))
			if _, ok := c.verdictLocked("C1", "1.0", now); ok != row.want {
				t.Fatalf("live=%v, want %v", ok, row.want)
			}
		})
	}
}

// A terminal verdict is a durable spool record (r12 finding 2, across
// the restart the ledger does not survive): dead-lettering a message
// writes its verdict beside the deletion records, the replay seeds it
// and writes it again for the next restart, and a delayed redelivery
// after either restart is dropped as a duplicate.
func TestTerminalVerdictSurvivesRestartViaTheSpool(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	deliver1, _ := recordingDeliver(func([]pendingChannelInbound) error { return permanent422() })
	c1 := newInboundCoalescer(20*time.Millisecond, nil)
	c1.deliver = deliver1
	c1.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c1.recordVerdict = spool.recordVerdict
	c1.enqueue("C1", testPending("C1", "1.0", "poison"))
	waitFor(t, "the dead-letter", func() bool { return c1.onLadder("C1", "1.0") })
	for restart := 1; restart <= 2; restart++ {
		deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
		c := newInboundCoalescer(20*time.Millisecond, nil)
		c.deliver = deliver
		c.recordVerdict = spool.recordVerdict
		spool.replayInto(c)
		if !c.onLadder("C1", "1.0") {
			t.Fatalf("restart %d: the dead-letter verdict must survive the restart", restart)
		}
		c.enqueue("C1", testPending("C1", "1.0", "a delayed redelivery after the restart"))
		time.Sleep(60 * time.Millisecond)
		if got := calls(); len(got) != 0 {
			t.Fatalf("restart %d: the refused bytes must never go out: %v", restart, got)
		}
	}
}

// --- codex r13 ---------------------------------------------------------------

// A verdict only progresses (r13 finding 1): a terminal verdict is never
// downgraded to an outstanding one, and recording the same verdict
// again changes nothing — so no second durable record is written and a
// re-seeded record keeps its original decision time.
func TestVerdictOnlyProgresses(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.recordVerdictLocked("C1", "1.0", ladderVerdict{cause: permanent422()}, now) {
		t.Fatal("a first verdict is a change")
	}
	if !c.recordVerdictLocked("C1", "1.0", ladderVerdict{retired: true, cause: permanent422()}, now) {
		t.Fatal("outstanding → terminal is a change")
	}
	if c.recordVerdictLocked("C1", "1.0", ladderVerdict{cause: permanent422()}, now) {
		t.Fatal("terminal → outstanding must be refused")
	}
	if c.recordVerdictLocked("C1", "1.0", ladderVerdict{retired: true, cause: permanent422()}, now.Add(time.Hour)) {
		t.Fatal("the same terminal verdict again is not a change")
	}
	v, ok := c.verdictLocked("C1", "1.0", now)
	if !ok || !v.retired || !v.at.Equal(now) {
		t.Fatalf("the terminal verdict must stand with its original time: live=%v retired=%v at=%v", ok, v.retired, v.at)
	}
	decided := now.Add(-2 * time.Hour)
	if !c.recordVerdictLocked("C1", "2.0", ladderVerdict{retired: true, at: decided}, now) {
		t.Fatal("a seeded record is a change")
	}
	if v, _ := c.verdictLocked("C1", "2.0", now); !v.at.Equal(decided) {
		t.Fatalf("a seeded record keeps its decision time (retention counts from the decision): %v", v.at)
	}
}

// A stale stripped entry staged beside its own terminal verdict (a
// crash after the replayed retry was dead-lettered but before cleanup
// — r13 finding 1) never reverses that verdict on the next replay,
// whichever order the two lines have: the terminal verdict is seeded
// first and wins, and the stale copy is dropped at admission.
func TestReplayNeverDowngradesATerminalVerdict(t *testing.T) {
	for _, order := range []string{"record then entry", "entry then record"} {
		t.Run(order, func(t *testing.T) {
			spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
			stale := testPending("C1", "1.0", "the stripped retry, dead-lettered after this line was staged")
			stale.isolate, stale.attempts, stale.stripped = true, 1, "422 Unprocessable Entity"
			record := func() {
				if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
					t.Fatal("record must confirm")
				}
			}
			entry := func() {
				if !spool.spillBatch("C1", []pendingChannelInbound{stale}) {
					t.Fatal("spill must confirm")
				}
			}
			if order == "record then entry" {
				record()
				entry()
			} else {
				entry()
				record()
			}
			deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
			c := newInboundCoalescer(20*time.Millisecond, nil)
			c.deliver = deliver
			c.recordVerdict = spool.recordVerdict
			spool.replayInto(c)
			time.Sleep(80 * time.Millisecond)
			c.mu.Lock()
			v, ok := c.verdictLocked("C1", "1.0", time.Now())
			c.mu.Unlock()
			if !ok || !v.retired {
				t.Fatalf("the terminal verdict must win over the stale stripped entry: live=%v retired=%v", ok, v.retired)
			}
			if c.pendingContains("C1", "1.0") {
				t.Fatal("the stale stripped copy must not enter the buffer")
			}
			if got := calls(); len(got) != 0 {
				t.Fatalf("the stale stripped retry must never POST: %v", got)
			}
		})
	}
}

// The staged spool is retained until every re-recorded verdict is
// durable (r13 finding 2): with the replacement spool unwritable, the
// replay must not remove the only durable copy of the records; the
// next startup merges the retained file and re-records them.
func TestReplayKeepsTheStagingFileUntilTheRecordsAreDurable(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
		t.Fatal("record must confirm")
	}
	c1 := newInboundCoalescer(time.Hour, nil)
	c1.recordVerdict = func(string, string, ladderVerdict) bool { return false } // the replacement spool cannot be written
	spool.replayInto(c1)
	if !c1.onLadder("C1", "1.0") {
		t.Fatal("the verdict is seeded in memory regardless")
	}
	if _, err := os.Stat(spool.replayingPath()); err != nil {
		t.Fatalf("the staged file must be retained while its records are not durable elsewhere: %v", err)
	}
	// Next startup, the disk writable again.
	c2 := newInboundCoalescer(time.Hour, nil)
	c2.recordVerdict = spool.recordVerdict
	spool.replayInto(c2)
	if !c2.onLadder("C1", "1.0") {
		t.Fatal("the verdict must survive the failed re-record through the retained file")
	}
	if _, err := os.Stat(spool.replayingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is removed once the records are durable again: %v", err)
	}
	c3 := newInboundCoalescer(time.Hour, nil)
	c3.recordVerdict = spool.recordVerdict
	spool.replayInto(c3)
	if !c3.onLadder("C1", "1.0") {
		t.Fatal("the re-recorded verdict must be in the new spool")
	}
}

func countVerdictLines(t *testing.T, path, ts string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		t.Fatal(err)
	}
	return bytes.Count(data, []byte(`"verdict_ts":"`+ts+`"`))
}

// Verdict records are bounded (r13 finding 3): a message writes one
// record per terminal decision (a retire followed by its park writes
// one line, not two); a replay seeds each (channel, ts) once and drops
// expired records, so every restart compacts the journal; and the
// replay streams the file line by line, so an oversized line is
// dropped as loss on its own while everything around it — and a file
// larger than any whole-file cap — still replays.
func TestVerdictRecordsAreCompactedAndReplayStreams(t *testing.T) {
	dir := t.TempDir()
	spool := newInboundSpool(dir + "/spool.jsonl")
	deliver1, _ := recordingDeliver(func([]pendingChannelInbound) error { return permanent422() })
	c1 := newInboundCoalescer(20*time.Millisecond, nil)
	c1.deliver = deliver1
	c1.deadLetter = func(string, []pendingChannelInbound, error) bool { return false } // the write fails: retire, then park
	c1.recordVerdict = spool.recordVerdict
	c1.enqueue("C1", testPending("C1", "1.0", "poison"))
	waitFor(t, "the retirement's record", func() bool { return countVerdictLines(t, spool.path, "1.0") >= 1 })
	time.Sleep(50 * time.Millisecond) // the park follows the retire; it must not write a second record
	if n := countVerdictLines(t, spool.path, "1.0"); n != 1 {
		t.Fatalf("one record per terminal decision (retire + park), got %d", n)
	}
	// Duplicate and expired records accumulate between restarts...
	for i := 0; i < 4; i++ {
		spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()})
	}
	spool.recordVerdict("C1", "2.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now().Add(-30 * time.Hour)})
	// ...beside an oversized line and enough bulk to pass any whole-file cap.
	f, err := os.OpenFile(spool.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	huge := bytes.Repeat([]byte("x"), maxInboundSpoolLineBytes+1)
	f.Write(append(append([]byte(`{"channel":"C1","verdict_ts":"3.0","verdict":"`), huge...), []byte("\"}\n")...))
	pad := append(append([]byte(fmt.Sprintf(`{"channel":"C1","verdict_ts":"4.0","verdict_at":%d,"verdict":"`, time.Now().Unix())), bytes.Repeat([]byte("y"), 256<<10)...), []byte("\"}\n")...)
	for written := 0; written <= 17<<20; written += len(pad) {
		f.Write(pad)
	}
	f.Close()
	if !spool.spillBatch("C1", []pendingChannelInbound{testPending("C1", "5.0", "a message spooled beside the records")}) {
		t.Fatal("spill must confirm")
	}
	deliver2, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c2 := newInboundCoalescer(20*time.Millisecond, nil)
	c2.deliver = deliver2
	c2.recordVerdict = spool.recordVerdict
	if n := spool.replayInto(c2); n != 1 {
		t.Fatalf("the message beside the records must replay (streamed past the oversized line and the bulk), admitted %d", n)
	}
	waitForCalls(t, calls, []string{"5.0"})
	if c2.onLadder("C1", "2.0") || c2.onLadder("C1", "3.0") {
		t.Fatal("an expired record is not seeded; an oversized line is dropped")
	}
	if !c2.onLadder("C1", "1.0") || !c2.onLadder("C1", "4.0") {
		t.Fatal("live records are seeded")
	}
	if n := countVerdictLines(t, spool.path, "1.0"); n != 1 {
		t.Fatalf("the replay compacts duplicates to one record, got %d", n)
	}
	if n := countVerdictLines(t, spool.path, "4.0"); n != 1 {
		t.Fatalf("the bulk compacts to one record, got %d", n)
	}
	if n := countVerdictLines(t, spool.path, "2.0") + countVerdictLines(t, spool.path, "3.0"); n != 0 {
		t.Fatalf("expired and dropped records are not re-written, got %d", n)
	}
	if st, err := os.Stat(spool.path); err != nil || st.Size() > 1<<20 {
		t.Fatalf("the compacted spool is small again: %v %d", err, st.Size())
	}
}

// --- codex r14 ---------------------------------------------------------------

// A copy the ladder owns lands as a terminal verdict whatever put it on
// the ladder (r14 finding 1). Only the STRIPPED copy's landing was
// recorded: an unvouched A+B came back charged (attempts=1, unstripped),
// A's delayed app_mention twin arrived, the flush-ahead posted the
// charged copies (a copy on the ladder is never withheld) and gc vouched
// this time — no verdict was recorded, onLadder said "no", and main.go
// posted urgent A: the same message twice under two dedup keys.
func TestUrgentTwinDefersToTheLadderAfterAChargedCopyLands(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		posts++
		if posts == 1 {
			return errDeliveryUnvouched // gc accepted A+B but never vouched: both come back charged
		}
		return nil
	})
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "A"))
	c.enqueue("C1", testPending("C1", "2.0", "B"))
	c.flushAheadOf("C1", "") // the window's flush: unvouched
	waitForCalls(t, calls, []string{"1.0,2.0"})
	c.mu.Lock()
	charged := 0
	for _, p := range c.pending["C1"] {
		if p.attempts > 0 && !p.isolate && p.stripped == "" {
			charged++
		}
	}
	c.mu.Unlock()
	if charged != 2 {
		t.Fatalf("both members must be back in the buffer charged and unstripped, got %d", charged)
	}
	// The delayed app_mention twin of A arrives on the urgent path: the
	// flush-ahead posts the charged copies, and gc vouches this time.
	if withheld := c.flushAheadOf("C1", "1.0"); len(withheld) != 0 {
		t.Fatalf("a copy on the ladder is never withheld for an urgent twin, got %d", len(withheld))
	}
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0,2.0"})
	if !c.onLadder("C1", "1.0") {
		t.Fatal("the ladder's copy of A just delivered: the urgent path must be told the ladder owns the message, or urgent A posts the same message again under its own dedup key")
	}
	if !c.onLadder("C1", "2.0") {
		t.Fatal("B landed as the ladder's copy too")
	}
	// Every later copy is a duplicate of a delivered message.
	c.enqueue("C1", testPending("C1", "1.0", "A, redelivered"))
	c.restore("C1", []pendingChannelInbound{testPending("C1", "1.0", "A, handed back")})
	if c.pendingContains("C1", "1.0") {
		t.Fatal("a copy of a message the ladder delivered must not re-enter the buffer")
	}
	if got := calls(); len(got) != 2 {
		t.Fatalf("the message went out exactly twice (unvouched, then vouched), never a third time: %v", got)
	}
}

// A reaction the ladder retired is a verdict about that reaction EVENT
// (r14 finding 2): its copies — buffered in the side lane when the
// verdict fell, redelivered by Slack, handed back by a failed delivery,
// replayed from the spool after a restart — are duplicates of a
// dead-lettered event and never ride another delivery. Reactions carry
// their own event ts as their id (reaction_events.go), so the ledger
// keys them the same way it keys messages.
func TestReactionCopiesHonourTheLadderVerdict(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	c := newInboundCoalescer(time.Hour, nil)
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c.recordVerdict = spool.recordVerdict
	reaction := func(id, text string) pendingChannelInbound {
		p := testPending("C1", id, text)
		p.reaction = true
		return p
	}
	sideLane := func(c *inboundCoalescer) int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.reactions["C1"])
	}
	// A copy waits in the side lane when the ladder retires the event.
	c.admitReaction("C1", reaction("1.5", "thumbsup, buffered copy"), false)
	c.charge("C1", reaction("1.5", "thumbsup"), permanent422()) // nothing to strip: dead-lettered
	if n := sideLane(c); n != 0 {
		t.Fatalf("the retired reaction's buffered copy must leave the side lane, got %d", n)
	}
	// A Slack redelivery, and a copy handed back from a failed delivery.
	if !c.admitReaction("C1", reaction("1.5", "thumbsup, redelivered"), false) {
		t.Fatal("admission stays final handling of the event")
	}
	c.restore("C1", []pendingChannelInbound{reaction("1.5", "thumbsup, handed back")})
	if n := sideLane(c); n != 0 {
		t.Fatalf("a copy of a dead-lettered reaction must never re-enter the side lane, got %d", n)
	}
	// After a restart the record restores the verdict before the spooled copy is admitted.
	if !spool.spillBatch("C1", []pendingChannelInbound{reaction("1.5", "thumbsup, spooled copy")}) {
		t.Fatal("spill must confirm")
	}
	c2 := newInboundCoalescer(time.Hour, nil)
	c2.recordVerdict = spool.recordVerdict
	spool.replayInto(c2)
	c2.admitReaction("C1", reaction("1.5", "thumbsup, late redelivery"), false)
	if n := sideLane(c2); n != 0 {
		t.Fatalf("the replayed and redelivered copies of a dead-lettered reaction are dropped at admission, got %d", n)
	}
	// An unrelated reaction buffers as before.
	c2.admitReaction("C1", reaction("2.5", "tada"), false)
	if n := sideLane(c2); n != 1 {
		t.Fatalf("an unrelated reaction still buffers, got %d", n)
	}
}

// A Refused entry's spool line is its retirement's only durable state
// (r14 finding 3): the replay re-parks it and writes the verdict record,
// and when that record cannot be written the staged file must be
// RETAINED — otherwise the parked write succeeds, the staged file is
// gone, and the next restart has no verdict: a delayed redelivery of
// the refused message is admitted as fresh bytes. A duplicate Refused
// line in one replay parks the message once.
func TestReplayKeepsTheStagingFileWhenAReparkedRefusalCannotBeRecorded(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	refused := testPending("C1", "1.0", "poison")
	refused.refused, refused.attempts = "422 Unprocessable Entity", 2
	if !spool.spillBatch("C1", []pendingChannelInbound{refused, refused}) {
		t.Fatal("spill must confirm")
	}
	parked := func(c *inboundCoalescer) int {
		c.mu.Lock()
		defer c.mu.Unlock()
		return len(c.parkedDeadLetters["C1"])
	}
	// Startup 1: the dead-letter write succeeds, the verdict record cannot be written.
	c1 := newInboundCoalescer(20*time.Millisecond, nil)
	c1.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c1.recordVerdict = func(string, string, ladderVerdict) bool { return false }
	spool.replayInto(c1)
	if n := parked(c1); n != 1 {
		t.Fatalf("a duplicate Refused line parks the message once, got %d parked", n)
	}
	if _, err := os.Stat(spool.replayingPath()); err != nil {
		t.Fatalf("the staged file must be retained while the re-parked refusal's record is not durable: %v", err)
	}
	waitFor(t, "the parked dead-letter write", func() bool { return parked(c1) == 0 })
	// Startup 2: the disk writable again — the retained file re-parks
	// the refusal, its record lands, and the staged file goes.
	c2 := newInboundCoalescer(20*time.Millisecond, nil)
	c2.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c2.recordVerdict = spool.recordVerdict
	spool.replayInto(c2)
	if !c2.onLadder("C1", "1.0") {
		t.Fatal("the refusal is the ladder's verdict after the restart")
	}
	if _, err := os.Stat(spool.replayingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the staged file is removed once the record is durable: %v", err)
	}
	waitFor(t, "the second startup's parked write", func() bool { return parked(c2) == 0 })
	// Startup 3: only the record remains; a redelivery is a duplicate.
	c3 := newInboundCoalescer(time.Hour, nil)
	c3.recordVerdict = spool.recordVerdict
	spool.replayInto(c3)
	if !c3.onLadder("C1", "1.0") {
		t.Fatal("the verdict must survive through the record in the new spool")
	}
	if n := parked(c3); n != 0 {
		t.Fatalf("nothing is owed a write any more, got %d parked", n)
	}
	c3.enqueue("C1", testPending("C1", "1.0", "poison, redelivered"))
	if c3.pendingContains("C1", "1.0") {
		t.Fatal("a redelivery of the refused message is a duplicate of a dead-lettered message")
	}
}

// The spool's line cap is the producer's bound too (r14 finding 4): what
// spillBatch confirms, the replay reads. A legal entry above the old
// 4 MiB reader cap round-trips; an entry over the cap is refused at the
// spill (the caller logs the loss then, and its batch-mates are still
// durable); the boundary is measured on the line without its newline
// on both sides, so a line exactly at the cap replays.
func TestSpoolLineCapIsTheProducersBound(t *testing.T) {
	dir := t.TempDir()
	spool := newInboundSpool(dir + "/spool.jsonl")
	bigText := strings.Repeat("あ", (5<<20)/3) // ~5 MiB of UTF-8, over the old 4 MiB cap
	if !spool.spillBatch("C1", []pendingChannelInbound{testPending("C1", "1.0", bigText)}) {
		t.Fatal("a legal entry spills")
	}
	var mu sync.Mutex
	var gotText string
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		gotText = batch[0].inbound.Text
		return nil
	})
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	if n := spool.replayInto(c); n != 1 {
		t.Fatalf("what spillBatch confirmed must replay, admitted %d", n)
	}
	waitForCalls(t, calls, []string{"1.0"})
	mu.Lock()
	intact := gotText == bigText
	mu.Unlock()
	if !intact {
		t.Fatal("the replayed entry must carry its text intact")
	}
	// Over the cap: refused where durability is promised, batch-mates kept.
	read, cleanup := captureLog(t)
	defer cleanup()
	huge := testPending("C1", "2.0", strings.Repeat("x", maxInboundSpoolLineBytes))
	if spool.spillBatch("C1", []pendingChannelInbound{huge, testPending("C1", "3.0", "batch-mate")}) {
		t.Fatal("spillBatch must not confirm a batch the replay cannot fully read")
	}
	if logged := read(); !strings.Contains(logged, "ts=2.0 is ") || !strings.Contains(logged, "REFUSED") {
		t.Fatalf("the refusal names the entry and its size: %q", logged)
	}
	deliver2, calls2 := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c2 := newInboundCoalescer(20*time.Millisecond, nil)
	c2.deliver = deliver2
	if n := spool.replayInto(c2); n != 1 {
		t.Fatalf("the batch-mate is durable and replays alone, admitted %d", n)
	}
	waitForCalls(t, calls2, []string{"3.0"})
	// The boundary: exactly at the cap replays, one byte more is dropped.
	line := func(ts string, extra int) []byte {
		prefix := `{"channel":"C1","inbound":{"provider_message_id":"` + ts + `","text":"`
		suffix := `"}}`
		pad := maxInboundSpoolLineBytes - len(prefix) - len(suffix) + extra
		return append(append(append([]byte(prefix), bytes.Repeat([]byte("p"), pad)...), suffix...), '\n')
	}
	edge := dir + "/edge.jsonl"
	if err := os.WriteFile(edge, append(line("4.0", 0), line("5.0", 1)...), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := readSpoolLines(edge, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Inbound.ProviderMessageID != "4.0" {
		got := make([]string, 0, len(entries))
		for _, e := range entries {
			got = append(got, e.Inbound.ProviderMessageID)
		}
		t.Fatalf("a line exactly at the cap replays and one byte more is dropped, got %v", got)
	}
}

// --- codex r15 ---------------------------------------------------------------

// The ladder's ownership of a message is a ledger fact, not a property
// of where its copy sits (r15 finding 1): onLadder used to scan the
// pending buffer for a charged or isolated copy, so a charged copy the
// timer had just DETACHED — in flight, in neither the ledger nor the
// buffer — was nobody's, and the urgent twin posted beside it. Every
// owned copy entering or re-entering the buffer records an outstanding
// verdict (ownLocked at enqueue and restore), and onLadder asks only
// the ledger.
func TestDetachedChargedCopyStaysTheLadders(t *testing.T) {
	var mu sync.Mutex
	posts := 0
	deliver, calls := recordingDeliver(func(batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		posts++
		if posts == 1 {
			return errDeliveryUnvouched
		}
		return nil
	})
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "A"))
	c.enqueue("C1", testPending("C1", "2.0", "B"))
	c.flushAheadOf("C1", "") // unvouched: both come back charged
	waitForCalls(t, calls, []string{"1.0,2.0"})
	// The timer's take detaches the charged batch; its POST is in flight.
	c.mu.Lock()
	batch, chanMu, ok := c.takeLocked("C1")
	c.mu.Unlock()
	if !ok || len(batch) != 2 {
		t.Fatalf("the charged batch must be taken (ok=%v, %d entries)", ok, len(batch))
	}
	if !c.onLadder("C1", "1.0") {
		t.Fatal("a charged copy in flight is still the ladder's: the urgent twin arriving now must skip, or it posts beside the re-post")
	}
	// The in-flight delivery lands.
	c.deliverBatch("C1", batch, chanMu)
	waitForCalls(t, calls, []string{"1.0,2.0", "1.0,2.0"})
	if !c.onLadder("C1", "1.0") || !c.onLadder("C1", "2.0") {
		t.Fatal("the landing is terminal for both members")
	}
	if got := calls(); len(got) != 2 {
		t.Fatalf("exactly two POSTs: %v", got)
	}
	// An isolation pause leaves the same fact: a probe flagged isolate
	// but never charged is the ladder's while restored and while detached.
	c2 := newInboundCoalescer(time.Hour, nil)
	probe := testPending("C1", "3.0", "probe")
	probe.isolate = true
	c2.restore("C1", []pendingChannelInbound{probe})
	c2.mu.Lock()
	_, chanMu2, ok2 := c2.takeLocked("C1")
	c2.mu.Unlock()
	if !ok2 {
		t.Fatal("take")
	}
	defer func() { chanMu2.Unlock(); c2.endDelivery() }()
	if !c2.onLadder("C1", "3.0") {
		t.Fatal("a detached isolated probe is still the ladder's")
	}
	// A fresh copy admitted meanwhile adopts the ladder's isolated copy.
	c2.enqueue("C1", testPending("C1", "3.0", "probe, redelivered"))
	c2.mu.Lock()
	defer c2.mu.Unlock()
	for _, p := range c2.pending["C1"] {
		if p.inbound.ProviderMessageID == "3.0" && !p.isolate {
			t.Fatal("a copy admitted under the ladder's outstanding verdict enters as its isolated copy, never as a plain member")
		}
	}
}

// A terminal verdict's record is owed until it lands (r15 finding 2):
// charge() used to log a failed write and forget it, and the park that
// followed saw an unchanged ledger and assumed durability. The ledger
// now tracks the record's durability per verdict, persistVerdict writes
// whatever is owed, the channel's durable-write retry timer (the parked
// dead-letter timer) retries it, and the shutdown flush gives it a last
// try.
func TestOwedVerdictRecordsAreRetried(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	var mu sync.Mutex
	failing := true
	recordCalls := 0
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = func(string, []pendingChannelInbound) error { return permanent422() }
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c.recordVerdict = func(channel, ts string, v ladderVerdict) bool {
		mu.Lock()
		defer mu.Unlock()
		recordCalls++
		if failing {
			return false
		}
		return spool.recordVerdict(channel, ts, v)
	}
	c.enqueue("C1", testPending("C1", "1.0", "poison"))
	waitFor(t, "the first (failed) record write", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return recordCalls >= 1
	})
	if !c.onLadder("C1", "1.0") {
		t.Fatal("dead-lettered in memory regardless")
	}
	waitFor(t, "a retry of the owed record", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return recordCalls >= 2
	})
	mu.Lock()
	failing = false
	mu.Unlock()
	waitFor(t, "the owed record to land once the disk recovers", func() bool { return countVerdictLines(t, spool.path, "1.0") == 1 })
	time.Sleep(60 * time.Millisecond) // the retries stop once nothing is owed
	if n := countVerdictLines(t, spool.path, "1.0"); n != 1 {
		t.Fatalf("one record, written once it could be, got %d", n)
	}
	c.mu.Lock()
	_, armed := c.deadLetterTimers["C1"]
	c.mu.Unlock()
	if armed {
		t.Fatal("nothing owed, no retry armed")
	}
	// The shutdown flush is the last try for a record still owed.
	spool2 := newInboundSpool(t.TempDir() + "/spool.jsonl")
	c2 := newInboundCoalescer(time.Hour, nil) // the retry would be an hour out
	c2.deliver = func(string, []pendingChannelInbound) error { return permanent422() }
	c2.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	failing2 := true
	c2.recordVerdict = func(channel, ts string, v ladderVerdict) bool {
		mu.Lock()
		f := failing2
		mu.Unlock()
		if f {
			return false
		}
		return spool2.recordVerdict(channel, ts, v)
	}
	c2.charge("C1", testPending("C1", "2.0", "poison"), permanent422())
	if n := countVerdictLines(t, spool2.path, "2.0"); n != 0 {
		t.Fatalf("the write failed, got %d records", n)
	}
	mu.Lock()
	failing2 = false
	mu.Unlock()
	c2.flushAll()
	if n := countVerdictLines(t, spool2.path, "2.0"); n != 1 {
		t.Fatalf("the shutdown flush writes the owed record, got %d", n)
	}
}

// An in-place replay (the staging rename failed — here on a basename
// whose .replaying suffix exceeds NAME_MAX) appends its re-written
// verdict records to the very file it is replaying, so removing that
// file after re-admission deleted the records it had just reported
// durable (r15 finding 3). The file is compacted to its live records
// instead: the messages do not replay twice, the verdicts survive.
func TestInPlaceReplayKeepsItsRecords(t *testing.T) {
	dir := t.TempDir()
	base := strings.Repeat("s", 250) // + ".replaying" = 260 > NAME_MAX (255)
	spool := newInboundSpool(filepath.Join(dir, base))
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
		t.Fatal("record must confirm")
	}
	if !spool.spillBatch("C1", []pendingChannelInbound{testPending("C1", "2.0", "a message spooled beside it")}) {
		t.Fatal("spill must confirm")
	}
	read, cleanup := captureLog(t)
	defer cleanup()
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c1 := newInboundCoalescer(20*time.Millisecond, nil)
	c1.deliver = deliver
	c1.recordVerdict = spool.recordVerdict
	if n := spool.replayInto(c1); n != 1 {
		t.Fatalf("the message replays, admitted %d", n)
	}
	if !strings.Contains(read(), "replaying in place") {
		t.Fatalf("the test must exercise the in-place fallback: %q", read())
	}
	waitForCalls(t, calls, []string{"2.0"})
	if !c1.onLadder("C1", "1.0") {
		t.Fatal("the verdict is seeded")
	}
	// Next startup: the record is still there, the message is not.
	c2 := newInboundCoalescer(20*time.Millisecond, nil)
	c2.deliver = deliver
	c2.recordVerdict = spool.recordVerdict
	if n := spool.replayInto(c2); n != 0 {
		t.Fatalf("the replayed message must not replay twice, admitted %d", n)
	}
	if !c2.onLadder("C1", "1.0") {
		t.Fatal("the in-place replay must keep the verdict record it re-wrote")
	}
	if n := countVerdictLines(t, spool.path, "1.0"); n != 1 {
		t.Fatalf("compacted to one record, got %d", n)
	}
}

// --- codex r16 ---------------------------------------------------------------

// A message retired WHILE the durable-write retry pass runs, whose
// record write fails, sees the timer armed and arms nothing; the pass's
// tail must recount the owed records under the lock (r16 finding 2) —
// trusting its own tally, it removed the timer and exited with nothing
// parked, and the new record waited for the next failure or shutdown.
func TestOwedRecordRetiredDuringTheRetryPassIsNotStranded(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	var mu sync.Mutex
	calls := 0
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c.recordVerdict = func(channel, ts string, v ladderVerdict) bool {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		switch n {
		case 1:
			return false // 1.0's first write fails: the retry timer is armed
		case 2:
			// The retry pass is writing 1.0. Meanwhile 2.0 is retired and its
			// write (call 3) fails: the timer is armed, so it arms nothing.
			c.charge("C1", testPending("C1", "2.0", "poison too"), permanent422())
			return spool.recordVerdict(channel, ts, v)
		case 3:
			return false
		}
		return spool.recordVerdict(channel, ts, v)
	}
	c.charge("C1", testPending("C1", "1.0", "poison"), permanent422())
	waitFor(t, "1.0's record on the retry", func() bool { return countVerdictLines(t, spool.path, "1.0") == 1 })
	waitFor(t, "2.0's record — the pass's tail must see the record owed by the message retired during it", func() bool {
		return countVerdictLines(t, spool.path, "2.0") == 1
	})
	time.Sleep(60 * time.Millisecond)
	c.mu.Lock()
	_, armed := c.deadLetterTimers["C1"]
	owed := 0
	for _, v := range c.verdicts["C1"] {
		if v.retired && !v.durable {
			owed++
		}
	}
	c.mu.Unlock()
	if armed || owed != 0 {
		t.Fatalf("caught up: nothing owed (%d), no retry armed (%v)", owed, armed)
	}
}

// --- codex r17 ---------------------------------------------------------------

// Every disposition the spool carries is seeded before any entry is
// admitted (r17 finding 1): a retained staged file can hold a PLAIN copy
// of a message ahead of the newer spool's stripped or isolated copy, and
// admitting the plain copy first let a cap flush post the refused bytes
// before the disposition line was reached. Seeded first, the plain copy
// adopts the ladder's copy at its own admission.
func TestReplaySeedsEveryPersistedDispositionBeforeAdmitting(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	plainA := testPendingWithAttachment("C1", "1.0", "A with a voice memo", "/tmp/x/memo.m4a")
	strippedA := withholdAttachments(plainA, permanent422())
	strippedA.isolate, strippedA.attempts = true, 1
	plainC := testPending("C1", "3.0", "C, a probe's twin")
	isolatedC := plainC
	isolatedC.isolate = true
	// The older staged file (plain copies) replays ahead of the newer
	// spool (the dispositions), oldest first.
	if !spool.spillBatch("C1", []pendingChannelInbound{plainA, plainC}) {
		t.Fatal("spill must confirm")
	}
	if err := os.Rename(spool.path, spool.replayingPath()); err != nil {
		t.Fatal(err)
	}
	if !spool.spillBatch("C1", []pendingChannelInbound{strippedA, isolatedC}) {
		t.Fatal("spill must confirm")
	}
	c := newInboundCoalescer(time.Hour, nil)
	c.recordVerdict = spool.recordVerdict
	spool.replayInto(c)
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pending["C1"] {
		switch p.inbound.ProviderMessageID {
		case "1.0":
			if len(p.inbound.Attachments) != 0 || !p.isolate {
				t.Fatalf("a copy of A in the buffer still carries its attachments (%d) or is not isolated — the plain copy was admitted before A's stripped disposition was seeded", len(p.inbound.Attachments))
			}
		case "3.0":
			if !p.isolate {
				t.Fatal("a copy of C in the buffer is plain — admitted before C's isolated disposition was seeded")
			}
		}
	}
	if !c.pendingContainsLocked("C1", "1.0") || !c.pendingContainsLocked("C1", "3.0") {
		t.Fatal("both messages are still owed their delivery")
	}
}

// A write that died mid-line leaves an unterminated prefix; the next
// append must start on a record boundary (r17 finding 2), or the retry
// of a verdict record fuses with the prefix into one corrupt line —
// confirmed durable, lost at startup, and the refused bytes admitted
// again.
func TestAppendSealsAPartialLineBeforeWriting(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
		t.Fatal("record must confirm")
	}
	f, err := os.OpenFile(spool.path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"channel":"C1","verdict_ts":"2.0","verd`) // ENOSPC mid-record
	f.Close()
	read, cleanup := captureLog(t)
	defer cleanup()
	if !spool.recordVerdict("C1", "3.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
		t.Fatal("the retry must confirm")
	}
	if !strings.Contains(read(), "ends mid-line") {
		t.Fatalf("the seal is logged: %q", read())
	}
	c := newInboundCoalescer(time.Hour, nil)
	c.recordVerdict = spool.recordVerdict
	spool.replayInto(c)
	if !c.onLadder("C1", "1.0") {
		t.Fatal("the record before the partial line replays")
	}
	if !c.onLadder("C1", "3.0") {
		t.Fatal("the record appended after a partial line must replay — it was confirmed durable")
	}
	if c.onLadder("C1", "2.0") {
		t.Fatal("the partial record is the loss, dropped as its own corrupt line")
	}
	if !strings.Contains(read(), "CORRUPT LINE") {
		t.Fatalf("the partial line is dropped loudly: %q", read())
	}
}

// A read error during the in-place compaction aborts it with the
// journal untouched (r17 finding 3): the salvage reader swallowed the
// error and returned nothing, and an empty replacement was renamed over
// the journal.
func TestCompactionAbortsOnAReadError(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}) {
		t.Fatal("record must confirm")
	}
	if err := os.Chmod(spool.path, 0o000); err != nil {
		t.Fatal(err)
	}
	read, cleanup := captureLog(t)
	defer cleanup()
	spool.mu.Lock()
	spool.compactToRecordsLocked()
	spool.mu.Unlock()
	if err := os.Chmod(spool.path, 0o600); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(read(), "FAILED (read:") {
		t.Fatalf("the abort is logged: %q", read())
	}
	if n := countVerdictLines(t, spool.path, "1.0"); n != 1 {
		t.Fatalf("the journal must be untouched after a read error, got %d record(s)", n)
	}
}

// --- codex r18 ---------------------------------------------------------------

// A refused REACTION's verdict is seeded before any admission too (r18
// finding 1): the pre-pass skipped reaction entries, so a plain copy of
// the reaction earlier in the file entered the side lane, and with 99
// other reactions ahead of the refused entry the 100th admission's
// overflow POST carried the refused bytes before the park retired them.
func TestReplaySeedsARefusedReactionBeforeAdmitting(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	reaction := func(id, text string) pendingChannelInbound {
		p := testPending("C1", id, text)
		p.reaction = true
		return p
	}
	batch := []pendingChannelInbound{reaction("1.5", "thumbsup, a plain copy of the refused reaction")}
	for i := 0; i < maxBufferedReactionsPerChannel-1; i++ {
		batch = append(batch, reaction(fmt.Sprintf("2.%03d", i), "another reaction"))
	}
	refused := reaction("1.5", "thumbsup")
	refused.refused, refused.attempts = "422 Unprocessable Entity", 1
	batch = append(batch, refused)
	if !spool.spillBatch("C1", batch) {
		t.Fatal("spill must confirm")
	}
	deliver, calls := recordingDeliver(func([]pendingChannelInbound) error { return nil })
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	c.deadLetter = func(string, []pendingChannelInbound, error) bool { return true }
	c.recordVerdict = spool.recordVerdict
	spool.replayInto(c)
	time.Sleep(60 * time.Millisecond)
	for _, call := range calls() {
		if strings.Contains(","+call+",", ",1.5(r),") { // recordingDeliver renders a reaction as "ts(r)"
			t.Fatalf("the refused reaction's plain copy rode a POST (%q) — its verdict must be seeded before any copy is admitted", call)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.reactions["C1"] {
		if p.inbound.ProviderMessageID == "1.5" {
			t.Fatal("the plain copy of a refused reaction must not sit in the side lane")
		}
	}
}

// When the newer spool cannot be merged behind a retained staged file,
// the salvage must carry the dispositions its ENTRIES hold, not only
// its explicit records (r18 finding 2): with plain A staged and stripped
// A in the un-merged spool, the pre-pass never saw A's stripped
// disposition and the staged copy entered delivery with its original
// attachments.
func TestFailedMergeSalvagesEntryCarriedDispositions(t *testing.T) {
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
	// The merge appends the spool behind the staged file: a read-only
	// staged file makes it fail, and the staged entries replay alone.
	if err := os.Chmod(spool.replayingPath(), 0o400); err != nil {
		t.Fatal(err)
	}
	read, cleanup := captureLog(t)
	defer cleanup()
	c := newInboundCoalescer(time.Hour, nil)
	c.recordVerdict = spool.recordVerdict
	spool.replayInto(c)
	if !strings.Contains(read(), "merging") || !strings.Contains(read(), "FAILED") {
		t.Fatalf("the test must exercise the failed merge: %q", read())
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	found := false
	for _, p := range c.pending["C1"] {
		if p.inbound.ProviderMessageID != "1.0" {
			continue
		}
		found = true
		if len(p.inbound.Attachments) != 0 || !p.isolate {
			t.Fatalf("the staged plain copy of A entered delivery with %d attachment(s), isolate=%v — the un-merged spool's stripped disposition was not salvaged", len(p.inbound.Attachments), p.isolate)
		}
	}
	if !found {
		t.Fatal("the staged copy of A is still owed its delivery")
	}
	if _, err := os.Stat(spool.path); err != nil {
		t.Fatalf("the un-merged spool stays for the next startup: %v", err)
	}
}

// --- codex r19 ---------------------------------------------------------------

// When the newer spool cannot be merged AND cannot be read, the staged
// replay is deferred (r19 finding 1): the staged entries may depend on
// dispositions in the unread spool (a plain copy staged, its stripped
// copy there), so nothing is admitted until both can be read.
func TestUnreadableSalvageDefersTheStagedReplay(t *testing.T) {
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
	if err := os.Chmod(spool.path, 0o000); err != nil { // the merge's read of the spool fails, and so does the salvage
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(spool.path, 0o600) })
	read, cleanup := captureLog(t)
	defer cleanup()
	c := newInboundCoalescer(time.Hour, nil)
	c.recordVerdict = spool.recordVerdict
	if n := spool.replayInto(c); n != 0 {
		t.Fatalf("nothing may be admitted while the spool that may hold its disposition is unreadable, admitted %d", n)
	}
	if !strings.Contains(read(), "DEFERRED") {
		t.Fatalf("the deferral is logged: %q", read())
	}
	if c.pendingContains("C1", "1.0") {
		t.Fatal("the staged plain copy must not enter delivery")
	}
	for _, path := range []string{spool.path, spool.replayingPath()} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("both files stay for the next startup: %v", err)
		}
	}
}

// An expired terminal record still applies to a copy of its message
// staged beside it (r19 finding 2): a stripped retry dead-lettered
// during a replay that crashed before its cleanup, restarted after the
// retention window, was seeded as outstanding and posted again. The
// record is renewed while its message is in the spool.
func TestExpiredRecordStillAppliesToItsStagedEntry(t *testing.T) {
	spool := newInboundSpool(t.TempDir() + "/spool.jsonl")
	if !spool.recordVerdict("C1", "1.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now().Add(-30 * time.Hour)}) {
		t.Fatal("record must confirm")
	}
	stale := withholdAttachments(testPendingWithAttachment("C1", "1.0", "A", "/tmp/x/memo.m4a"), permanent422())
	stale.isolate, stale.attempts = true, 1
	if !spool.spillBatch("C1", []pendingChannelInbound{stale, testPending("C1", "2.0", "an unrelated message")}) {
		t.Fatal("spill must confirm")
	}
	spool.recordVerdict("C1", "9.0", ladderVerdict{retired: true, cause: permanent422(), at: time.Now().Add(-30 * time.Hour)}) // expired, no copy present: compacted away
	c := newInboundCoalescer(time.Hour, nil)
	c.recordVerdict = spool.recordVerdict
	spool.replayInto(c)
	if c.pendingContains("C1", "1.0") {
		t.Fatal("the exhausted stripped retry must not be posted again: the expired record beside it still says dead-lettered")
	}
	if !c.pendingContains("C1", "2.0") {
		t.Fatal("the unrelated message replays")
	}
	if !c.onLadder("C1", "1.0") {
		t.Fatal("the renewed verdict stands")
	}
	if n := countVerdictLines(t, spool.path, "1.0"); n != 1 {
		t.Fatalf("the renewed record is written once, got %d", n)
	}
	if n := countVerdictLines(t, spool.path, "9.0"); n != 0 {
		t.Fatalf("an expired record with no copy present is compacted away, got %d", n)
	}
}

// A failed verdict-record write logs once per distinct failure and once
// on recovery (r19 minor), not once per retry.
func TestVerdictRecordWriteFailureLogsOncePerDistinctFailure(t *testing.T) {
	dir := t.TempDir() + "/locked"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	spool := newInboundSpool(dir + "/spool.jsonl")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	read, cleanup := captureLog(t)
	defer cleanup()
	v := ladderVerdict{retired: true, cause: permanent422(), at: time.Now()}
	for i := 0; i < 3; i++ {
		if spool.recordVerdict("C1", "1.0", v) {
			t.Fatal("the write must fail in a read-only directory")
		}
	}
	if n := strings.Count(read(), "verdict record write FAILED"); n != 1 {
		t.Fatalf("one line per distinct failure, got %d: %q", n, read())
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !spool.recordVerdict("C1", "1.0", v) {
		t.Fatal("the write succeeds once the directory is writable")
	}
	if n := strings.Count(read(), "verdict record writes recovered"); n != 1 {
		t.Fatalf("one recovery line, got %d: %q", n, read())
	}
}
