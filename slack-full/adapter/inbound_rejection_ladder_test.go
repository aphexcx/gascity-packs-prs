package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
		{name: "only riding reactions left in pending under an armed timer: disarmed (r8 f3)", pending: []pendingChannelInbound{riding}, armedIn: 30 * time.Second, wantChanged: true},
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
