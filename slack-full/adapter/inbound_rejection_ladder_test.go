package main

import (
	"errors"
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
	t.Run("legacy entry (no parts) appends to Text", func(t *testing.T) {
		p := testPendingWithAttachment("C1", "1.0", "old spool line", "/tmp/store/C1/1.0-audio.m4a")
		got := withholdAttachments(p, cause)
		if len(got.inbound.Attachments) != 0 || got.hasReminderParts() {
			t.Fatalf("legacy entry must stay partless and lose its attachments: %+v", got)
		}
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
	if got := transientRetryDelay(0, 5); got != 0 {
		t.Fatalf("a zero window (coalescing disabled) stays zero, got %s", got)
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
