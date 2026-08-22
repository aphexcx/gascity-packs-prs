package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPending(channel, ts, text string) pendingChannelInbound {
	return pendingChannelInbound{inbound: externalInboundMessage{
		ProviderMessageID: ts,
		Conversation:      conversationRef{ConversationID: channel, Provider: "slack"},
		Actor:             externalActor{ID: "U1", DisplayName: "Afik"},
		Text:              text,
		DedupKey:          "slack-" + ts,
	}}
}

// collectingDeliver returns a deliver func that records batches and a
// channel carrying each delivery.
func collectingDeliver() (func(string, []pendingChannelInbound) bool, chan []pendingChannelInbound) {
	ch := make(chan []pendingChannelInbound, 16)
	return func(channel string, batch []pendingChannelInbound) bool {
		ch <- batch
		return true
	}, ch
}

func TestCoalescerNilAndZeroWindowDisabled(t *testing.T) {
	var nilC *inboundCoalescer
	if nilC.enabled() {
		t.Fatal("nil coalescer must be disabled")
	}
	nilC.enqueue("C1", testPending("C1", "1.0", "x")) // must not panic
	nilC.flushAheadOf("C1", "")                       // must not panic
	nilC.flushAll()                                   // must not panic
	nilC.reconcileTimers()                            // must not panic
	if nilC.pendingContains("C1", "1.0") {
		t.Fatal("nil coalescer contains nothing")
	}

	zero := newInboundCoalescer(0, nil)
	if zero.enabled() {
		t.Fatal("zero-window coalescer must be disabled")
	}
}

func TestCoalescerTimerFlushDeliversOneBatchInOrder(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "first"))
	c.enqueue("C1", testPending("C1", "2.0", "second"))
	c.enqueue("C1", testPending("C1", "3.0", "third"))
	if !c.pendingContains("C1", "2.0") {
		t.Fatal("pendingContains must see buffered ts")
	}

	select {
	case batch := <-got:
		if len(batch) != 3 {
			t.Fatalf("batch len = %d, want 3", len(batch))
		}
		for i, want := range []string{"1.0", "2.0", "3.0"} {
			if batch[i].inbound.ProviderMessageID != want {
				t.Fatalf("batch[%d].ts = %q, want %q", i, batch[i].inbound.ProviderMessageID, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timer flush did not fire")
	}
	select {
	case extra := <-got:
		t.Fatalf("unexpected second delivery: %d messages", len(extra))
	case <-time.After(80 * time.Millisecond):
	}
	if c.pendingContains("C1", "2.0") {
		t.Fatal("flushed ts must leave the pending set")
	}
}

func TestCoalescerChannelsBufferIndependently(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "one"))
	c.enqueue("C2", testPending("C2", "2.0", "two"))

	channels := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case batch := <-got:
			if len(batch) != 1 {
				t.Fatalf("batch len = %d, want 1", len(batch))
			}
			channels[batch[0].inbound.Conversation.ConversationID] = true
		case <-time.After(2 * time.Second):
			t.Fatal("flushes did not fire")
		}
	}
	if !channels["C1"] || !channels["C2"] {
		t.Fatalf("channels flushed = %v, want C1 and C2", channels)
	}
}

func TestCoalescerFlushAheadOfDrainsSynchronously(t *testing.T) {
	deliver, got := collectingDeliver()
	// Hour-long window: only flushAheadOf can drain it inside the test.
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "buffered"))
	c.flushAheadOf("C1", "")
	select {
	case batch := <-got:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("unexpected batch: %+v", batch)
		}
	default:
		t.Fatal("flushAheadOf must deliver synchronously")
	}
	// Empty buffer: no delivery.
	c.flushAheadOf("C1", "")
	select {
	case <-got:
		t.Fatal("empty flushAheadOf must not deliver")
	default:
	}
}

func TestCoalescerFlushAheadOfWithholdsUrgentTwin(t *testing.T) {
	// pc_c920ff5fe90c: the urgent message's buffered twin (same ts,
	// distinct event id) must not ride in the flushed batch — the batch
	// dedup key can't collide with the urgent copy's "slack-<ts>", so
	// gc would deliver the id twice in one turn. The twin is RETURNED,
	// not dropped: the caller restores it if the urgent delivery fails.
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "chatter"))
	c.enqueue("C1", testPending("C1", "2.0", "twin of the urgent mention"))
	withheld := c.flushAheadOf("C1", "2.0")
	select {
	case batch := <-got:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("batch must hold only the non-twin chatter: %+v", batch)
		}
	default:
		t.Fatal("flush-ahead must still deliver the rest of the batch")
	}
	if len(withheld) != 1 || withheld[0].inbound.ProviderMessageID != "2.0" {
		t.Fatalf("withheld = %+v, want the twin entry", withheld)
	}
	if c.pendingContains("C1", "2.0") {
		t.Fatal("withheld twin must leave the buffer while the urgent copy is in flight")
	}
	// Urgent delivery failed → the caller restores; the twin must be
	// buffered again for the timer retry.
	c.restore("C1", withheld)
	if !c.pendingContains("C1", "2.0") {
		t.Fatal("restored twin must re-enter the buffer")
	}

	// A buffer holding ONLY the twin flushes to nothing.
	c.mu.Lock()
	c.takeLocked("C1") // reset buffer state
	c.mu.Unlock()
	c.enqueue("C1", testPending("C1", "3.0", "lone twin"))
	if withheld := c.flushAheadOf("C1", "3.0"); len(withheld) != 1 {
		t.Fatalf("lone twin must be withheld, got %+v", withheld)
	}
	select {
	case batch := <-got:
		t.Fatalf("twin-only batch must not deliver: %+v", batch)
	default:
	}
}

func TestCoalescerStaleTimerCannotStealNewerBatch(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "old"))
	// Capture the generation the armed timer holds, then drain via
	// flushAheadOf (bumps the generation) and enqueue a NEW message.
	c.mu.Lock()
	staleGen := c.gen["C1"]
	c.mu.Unlock()
	c.flushAheadOf("C1", "")
	<-got
	c.enqueue("C1", testPending("C1", "2.0", "new"))
	// Simulate the stale callback firing late: it must no-op, leaving
	// the new message buffered for its own window.
	c.flushTimer("C1", staleGen)
	select {
	case batch := <-got:
		t.Fatalf("stale timer stole a newer batch: %+v", batch)
	default:
	}
	if !c.pendingContains("C1", "2.0") {
		t.Fatal("new message must remain buffered after the stale no-op")
	}
}

func TestCoalescerFailedFlushRestoresAndRetries(t *testing.T) {
	var mu sync.Mutex
	fails := 1
	attempts := make(chan []pendingChannelInbound, 8)
	c := newInboundCoalescer(25*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		mu.Lock()
		defer mu.Unlock()
		attempts <- batch
		if fails > 0 {
			fails--
			return false
		}
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "x"))

	// First attempt fails; the restore re-arms the timer and the retry
	// delivers the same batch.
	for i := 0; i < 2; i++ {
		select {
		case batch := <-attempts:
			if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
				t.Fatalf("attempt %d: unexpected batch %+v", i, batch)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("attempt %d did not fire", i)
		}
	}
}

// TestCoalescerFullBufferFlushesEarlyNothingDropped pins the digest
// contract: the cap triggers an early flush; no message is ever evicted.
func TestCoalescerFullBufferFlushesEarlyNothingDropped(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	for i := 0; i < maxCoalescePerChannel; i++ {
		c.enqueue("C1", testPending("C1", fmt.Sprintf("%03d.0", i), "x"))
	}
	select {
	case batch := <-got:
		if len(batch) != maxCoalescePerChannel {
			t.Fatalf("early flush delivered %d, want all %d", len(batch), maxCoalescePerChannel)
		}
		if batch[0].inbound.ProviderMessageID != "000.0" {
			t.Fatalf("oldest message must survive, got first=%s", batch[0].inbound.ProviderMessageID)
		}
	default:
		t.Fatal("cap-full buffer must flush immediately")
	}
	if c.pendingContains("C1", "000.0") {
		t.Fatal("early flush must drain the buffer")
	}
}

// --- cross-channel coalescing (gp-9e7 item 2) -------------------------------

// A firing timer sweeps every other buffered non-digest channel so one
// idle wake carries all channels' window traffic, each as its own
// per-channel batch with in-channel order preserved.
func TestCoalescerTimerFlushSweepsOtherChannels(t *testing.T) {
	deliver, got := collectingDeliver()
	// C1 gets a short window; C2/C3 buffer under it too and must ride
	// C1's flush instead of waiting out their own timers.
	c := newInboundCoalescer(40*time.Millisecond, nil)
	c.deliver = deliver
	c.enqueue("C2", testPending("C2", "2.0", "c2 first"))
	c.enqueue("C2", testPending("C2", "2.1", "c2 second"))
	c.enqueue("C3", testPending("C3", "3.0", "c3 only"))
	c.enqueue("C1", testPending("C1", "1.0", "c1 only"))

	batches := map[string][]pendingChannelInbound{}
	for i := 0; i < 3; i++ {
		select {
		case batch := <-got:
			batches[batch[0].inbound.Conversation.ConversationID] = batch
		case <-time.After(2 * time.Second):
			t.Fatalf("delivery %d did not fire (swept channels must ride the first flush)", i)
		}
	}
	if len(batches["C2"]) != 2 || batches["C2"][0].inbound.ProviderMessageID != "2.0" {
		t.Fatalf("C2 batch = %+v, want both entries in order", batches["C2"])
	}
	if len(batches["C1"]) != 1 || len(batches["C3"]) != 1 {
		t.Fatalf("C1/C3 batches = %d/%d entries, want 1/1", len(batches["C1"]), len(batches["C3"]))
	}
	// Everything drained: no timer left to fire a second wave.
	select {
	case extra := <-got:
		t.Fatalf("unexpected second delivery: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// Digest-mode channels are exempt from the sweep: the operator bought
// that latency deliberately (gp-729 item 6).
func TestCoalescerSweepSkipsDigestChannels(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"CDIGEST": {"mode": "digest", "interval_minutes": 120}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(30*time.Millisecond, reg)
	c.deliver = deliver
	c.enqueue("CDIGEST", testPending("CDIGEST", "9.0", "digest-held"))
	c.enqueue("C1", testPending("C1", "1.0", "burst"))

	select {
	case batch := <-got:
		if batch[0].inbound.Conversation.ConversationID != "C1" {
			t.Fatalf("first delivery = %+v, want C1's burst", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("burst flush did not fire")
	}
	select {
	case batch := <-got:
		t.Fatalf("digest channel swept by a burst flush: %+v", batch)
	case <-time.After(100 * time.Millisecond):
	}
	if !c.pendingContains("CDIGEST", "9.0") {
		t.Fatal("digest channel's buffer must survive the sweep")
	}
}

// --- no-wake reaction buffering (gp-9e7 item 1) ------------------------------

func TestCoalescerAdmitReactionNeverArmsTimer(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(20*time.Millisecond, nil)
	c.deliver = deliver
	r := testPending("C1", "5.099", "reacted :+1:")
	if !c.admitReaction("C1", r, false) {
		t.Fatal("admitReaction on a live coalescer must admit")
	}
	select {
	case batch := <-got:
		t.Fatalf("buffered reaction delivered on its own: %+v", batch)
	case <-time.After(80 * time.Millisecond):
	}
	// Nil coalescer refuses so the caller can fall back.
	var nilC *inboundCoalescer
	if nilC.admitReaction("C1", r, false) {
		t.Fatal("nil coalescer must refuse admission")
	}
}

// Reactions merge into any real take for the channel: a timer flush
// armed by messages carries them, and a failed delivery returns them to
// the side lane WITHOUT arming a retry timer.
func TestCoalescerReactionsRideRealBatchAndRestoreNoWake(t *testing.T) {
	var fail bool
	attempts := make(chan []pendingChannelInbound, 8)
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		attempts <- batch
		return !fail
	}
	c.admitReaction("C1", testPending("C1", "1.099", "reaction"), false)
	c.enqueue("C1", testPending("C1", "2.0", "message"))
	batch := c.flushAheadOf("C1", "")
	_ = batch
	select {
	case b := <-attempts:
		if len(b) != 2 {
			t.Fatalf("batch = %d entries, want message + reaction", len(b))
		}
	default:
		t.Fatal("flush-ahead did not deliver")
	}

	// Failure path: restore splits the reaction back into the side lane
	// and arms NO timer when only reactions remain.
	fail = true
	c.admitReaction("C1", testPending("C1", "3.099", "reaction2"), false)
	c.enqueue("C1", testPending("C1", "4.0", "message2"))
	c.flushAheadOf("C1", "")
	<-attempts
	c.mu.Lock()
	pendingN := len(c.pending["C1"])
	reactionsN := len(c.reactions["C1"])
	_, timerArmed := c.timers["C1"]
	c.mu.Unlock()
	if pendingN != 1 || reactionsN != 1 {
		t.Fatalf("after failed flush: pending=%d reactions=%d, want 1/1 (split restore)", pendingN, reactionsN)
	}
	if !timerArmed {
		t.Fatal("failed real message must re-arm the retry timer")
	}
}

// A reactions-only take must never POST from flushAheadOf (gp-9e7 fix
// round 1b): the urgent message's own delivery has not happened yet and
// can be skipped or fail — a reaction batch posted ahead of it would be
// the solo reaction wake the side-buffer exists to prevent. The entries
// stay in the side lane and drain via deliverBufferedReactions after
// the caller's real POST commits.
func TestCoalescerFlushAheadReactionsOnlyDefersToRealDelivery(t *testing.T) {
	attempts := make(chan []pendingChannelInbound, 4)
	deliverOK := true
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		attempts <- batch
		return deliverOK
	}
	c.admitReaction("C1", testPending("C1", "1.099", "reaction"), false)
	if withheld := c.flushAheadOf("C1", ""); len(withheld) != 0 {
		t.Fatalf("withheld = %+v, want none", withheld)
	}
	select {
	case b := <-attempts:
		t.Fatalf("reactions-only flush-ahead delivered %+v (solo reaction wake)", b)
	default:
	}
	c.mu.Lock()
	reactionsN := len(c.reactions["C1"])
	_, timerArmed := c.timers["C1"]
	c.mu.Unlock()
	if reactionsN != 1 {
		t.Fatalf("reactions = %d, want the entry kept in the side lane", reactionsN)
	}
	if timerArmed {
		t.Fatal("a deferred reaction must not arm a retry timer (solo wake)")
	}

	// The caller's real POST committed: the drain delivers the batch.
	c.deliverBufferedReactions("C1")
	select {
	case b := <-attempts:
		if len(b) != 1 || !b[0].reaction {
			t.Fatalf("batch = %+v, want the lone reaction entry", b)
		}
	default:
		t.Fatal("deliverBufferedReactions did not deliver the side lane")
	}

	// A failed drain restores to the side lane, still with no timer.
	deliverOK = false
	c.admitReaction("C1", testPending("C1", "2.099", "reaction2"), false)
	c.deliverBufferedReactions("C1")
	<-attempts
	c.mu.Lock()
	reactionsN = len(c.reactions["C1"])
	_, timerArmed = c.timers["C1"]
	c.mu.Unlock()
	if reactionsN != 1 {
		t.Fatalf("reactions = %d, want the failed entry restored to the side lane", reactionsN)
	}
	if timerArmed {
		t.Fatal("a reactions-only restore must not arm a retry timer (solo wake)")
	}
}

// The urgent twin filter can empty a take down to its riding reactions;
// that emptied take must defer them too, not post them alone.
func TestCoalescerFlushAheadTwinOnlyTakeDefersReactions(t *testing.T) {
	attempts := make(chan []pendingChannelInbound, 4)
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		attempts <- batch
		return true
	}
	c.enqueue("C1", testPending("C1", "5.0", "twin"))
	c.admitReaction("C1", testPending("C1", "5.099", "reaction"), false)
	withheld := c.flushAheadOf("C1", "5.0")
	if len(withheld) != 1 || withheld[0].inbound.ProviderMessageID != "5.0" {
		t.Fatalf("withheld = %+v, want the twin", withheld)
	}
	select {
	case b := <-attempts:
		t.Fatalf("twin-emptied flush-ahead delivered %+v (solo reaction wake)", b)
	default:
	}
	c.mu.Lock()
	reactionsN := len(c.reactions["C1"])
	c.mu.Unlock()
	if reactionsN != 1 {
		t.Fatalf("reactions = %d, want the riding reaction returned to the side lane", reactionsN)
	}
}

// The sweep never touches a channel holding only reactions — that
// delivery would be a solo reaction wake for that channel's session.
func TestCoalescerSweepSkipsReactionOnlyChannels(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver
	c.admitReaction("C_QUIET", testPending("C_QUIET", "7.099", "reaction"), false)
	c.enqueue("C1", testPending("C1", "1.0", "burst"))

	select {
	case batch := <-got:
		if batch[0].inbound.Conversation.ConversationID != "C1" {
			t.Fatalf("delivery = %+v, want C1 only", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("burst flush did not fire")
	}
	select {
	case batch := <-got:
		t.Fatalf("reaction-only channel swept: %+v", batch)
	case <-time.After(80 * time.Millisecond):
	}
	c.mu.Lock()
	kept := len(c.reactions["C_QUIET"])
	c.mu.Unlock()
	if kept != 1 {
		t.Fatalf("C_QUIET reactions = %d, want 1 (untouched by the sweep)", kept)
	}
}

// Shutdown's flushAll is the delivery backstop for reaction-only
// channels whose real traffic never came.
func TestCoalescerFlushAllDrainsReactionOnlyChannels(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.admitReaction("C_QUIET", testPending("C_QUIET", "7.099", "reaction"), false)
	c.flushAll()
	select {
	case batch := <-got:
		if len(batch) != 1 || !batch[0].reaction {
			t.Fatalf("flushAll delivered %+v, want the lone reaction", batch)
		}
	default:
		t.Fatal("flushAll must drain reaction-only channels")
	}
}

func TestCoalescerReactionOverflowFlushesNothingDropped(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	for i := 0; i < maxBufferedReactionsPerChannel; i++ {
		c.admitReaction("C1", testPending("C1", fmt.Sprintf("%03d.099", i), "r"), false)
	}
	select {
	case batch := <-got:
		if len(batch) != maxBufferedReactionsPerChannel {
			t.Fatalf("overflow flush delivered %d, want all %d", len(batch), maxBufferedReactionsPerChannel)
		}
	default:
		t.Fatal("cap-full reaction buffer must flush (bounded memory, nothing dropped)")
	}
}

// Swept batches must not wait behind the triggering channel's own POST
// (gp-9e7 fix round 2a): sequenced after it, a swept channel's batch
// sits detached — maps empty, flush mutex free — for the whole network
// call, a window an urgent flushAheadOf overtakes (in-channel reorder),
// and the seconds-apart POSTs defeat the one-wake fold. The swept
// delivery must be able to COMPLETE while the trigger's POST is still
// in flight.
func TestCoalescerSweptDeliveryDoesNotWaitForTriggerPOST(t *testing.T) {
	sweptDone := make(chan struct{})
	overtaken := make(chan bool, 1)
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		switch channel {
		case "C_TRIGGER":
			// The firing channel's POST is slow. The swept channel's
			// delivery must finish during it, not after it.
			select {
			case <-sweptDone:
				overtaken <- false
			case <-time.After(1500 * time.Millisecond):
				overtaken <- true
			}
		case "C_SWEPT":
			close(sweptDone)
		}
		return true
	}
	c.enqueue("C_SWEPT", testPending("C_SWEPT", "2.0", "swept"))
	c.enqueue("C_TRIGGER", testPending("C_TRIGGER", "1.0", "trigger"))
	c.mu.Lock()
	g := c.gen["C_TRIGGER"]
	c.mu.Unlock()
	done := make(chan struct{})
	go func() {
		c.flushTimer("C_TRIGGER", g)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flushTimer did not return")
	}
	if <-overtaken {
		t.Fatal("swept channel's delivery waited for the triggering channel's POST to finish")
	}
}

// Shutdown drain fixpoint (gp-9e7 fix round 1a/2b): flushAll must wait
// out an in-flight timer delivery whose batch is detached from the maps
// — and, when that delivery fails and restores, re-drain it — while a
// concurrent enqueue lands mid-drain. One snapshot pass would find the
// maps empty, return, and lose both to the process exit.
func TestCoalescerFlushAllFixpointUnderConcurrentDeliveryAndEnqueue(t *testing.T) {
	firstAttempt := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan []pendingChannelInbound, 8)
	var mu sync.Mutex
	attempts := 0
	c := newInboundCoalescer(25*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			close(firstAttempt) // timer fired; batch now detached
			<-release           // hold it in flight while flushAll starts
			return false        // fail → restore lands AFTER a one-shot snapshot
		}
		delivered <- batch
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "first"))
	select {
	case <-firstAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("timer flush did not fire")
	}
	flushDone := make(chan struct{})
	go func() {
		c.flushAll()
		close(flushDone)
	}()
	// While the drain is waiting on the in-flight delivery, more traffic
	// arrives (the caller-side event goroutines are only stopped ahead
	// of flushAll in main's shutdown ordering; the coalescer itself must
	// still fold a mid-drain enqueue into the fixpoint).
	c.enqueue("C1", testPending("C1", "2.0", "second"))
	select {
	case <-flushDone:
		t.Fatal("flushAll returned while a taken batch was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-flushDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flushAll did not reach its fixpoint")
	}
	var got []string
	for {
		select {
		case batch := <-delivered:
			for _, p := range batch {
				got = append(got, p.inbound.ProviderMessageID)
			}
			continue
		default:
		}
		break
	}
	want := map[string]bool{"1.0": false, "2.0": false}
	for _, ts := range got {
		want[ts] = true
	}
	if !want["1.0"] || !want["2.0"] {
		t.Fatalf("flushAll delivered %v, want the restored batch AND the mid-drain enqueue", got)
	}
	c.mu.Lock()
	pendingN := len(c.pending["C1"])
	inflight := c.inflight
	c.mu.Unlock()
	if pendingN != 0 || inflight != 0 {
		t.Fatalf("after flushAll: pending=%d inflight=%d, want 0/0", pendingN, inflight)
	}
}

// flushAll must also await UNTRACKED-goroutine swept deliveries
// (gp-9e7 fix round 2b): a SIGTERM landing mid-sweep used to exit with
// swept batches detached and undeliverable — here the swept delivery
// fails while flushAll runs, and the fixpoint re-drains it.
func TestCoalescerFlushAllAwaitsSweptDeliveries(t *testing.T) {
	sweptTaken := make(chan struct{})
	release := make(chan struct{})
	delivered := make(chan string, 8)
	var mu sync.Mutex
	sweptAttempts := 0
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		if channel == "C_SWEPT" {
			mu.Lock()
			sweptAttempts++
			n := sweptAttempts
			mu.Unlock()
			if n == 1 {
				close(sweptTaken)
				<-release
				return false // restore after flushAll's first look
			}
		}
		delivered <- channel
		return true
	}
	c.enqueue("C_SWEPT", testPending("C_SWEPT", "2.0", "swept"))
	c.enqueue("C_TRIGGER", testPending("C_TRIGGER", "1.0", "trigger"))
	c.mu.Lock()
	g := c.gen["C_TRIGGER"]
	c.mu.Unlock()
	go c.flushTimer("C_TRIGGER", g)
	select {
	case <-sweptTaken:
	case <-time.After(2 * time.Second):
		t.Fatal("swept delivery did not start")
	}
	flushDone := make(chan struct{})
	go func() {
		c.flushAll()
		close(flushDone)
	}()
	select {
	case <-flushDone:
		t.Fatal("flushAll returned while the swept goroutine's batch was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-flushDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flushAll did not await the swept delivery's restore + retry")
	}
	seen := map[string]bool{}
	for {
		select {
		case ch := <-delivered:
			seen[ch] = true
			continue
		default:
		}
		break
	}
	if !seen["C_SWEPT"] || !seen["C_TRIGGER"] {
		t.Fatalf("delivered %v, want both the trigger and the restored swept batch", seen)
	}
}

// A delivery that fails every attempt must not spin flushAll forever:
// the fixpoint loop is bounded and gives up loudly.
func TestCoalescerFlushAllBoundedWhenDeliveryAlwaysFails(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	var mu sync.Mutex
	attempts := 0
	c.deliver = func(channel string, batch []pendingChannelInbound) bool {
		mu.Lock()
		attempts++
		mu.Unlock()
		return false
	}
	c.enqueue("C1", testPending("C1", "1.0", "doomed"))
	done := make(chan struct{})
	go func() {
		c.flushAll()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("flushAll did not terminate under persistent delivery failure")
	}
	mu.Lock()
	n := attempts
	mu.Unlock()
	if n < 1 || n > maxFlushAllPasses {
		t.Fatalf("attempts = %d, want between 1 and %d", n, maxFlushAllPasses)
	}
}

func TestCoalescerFlushAllDrainsEveryChannel(t *testing.T) {
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = deliver
	c.enqueue("C1", testPending("C1", "1.0", "one"))
	c.enqueue("C2", testPending("C2", "2.0", "two"))
	c.flushAll()
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case batch := <-got:
			seen[batch[0].inbound.Conversation.ConversationID] = true
		default:
			t.Fatal("flushAll must deliver synchronously")
		}
	}
	if !seen["C1"] || !seen["C2"] {
		t.Fatalf("flushAll drained %v, want C1 and C2", seen)
	}
}

func TestCoalescerReconcileTimersAppliesNewPolicy(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"C1": {"mode": "digest", "interval_minutes": 120}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	deliver, got := collectingDeliver()
	c := newInboundCoalescer(20*time.Millisecond, reg)
	c.deliver = deliver
	// Buffered under a two-hour digest window...
	c.enqueue("C1", testPending("C1", "1.0", "x"))
	// ...operator flips the channel to immediate and SIGHUPs.
	if err := writeFileAndReload(path, `{"channels": {}}`, reg); err != nil {
		t.Fatalf("reload: %v", err)
	}
	c.reconcileTimers()
	select {
	case batch := <-got:
		if len(batch) != 1 || batch[0].inbound.ProviderMessageID != "1.0" {
			t.Fatalf("unexpected batch: %+v", batch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconciled timer did not fire at the new short window")
	}
}

// writeFileAndReload rewrites a registry file and stages+commits it.
func writeFileAndReload(path, content string, reg *deliveryPolicyRegistry) error {
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return err
	}
	snap, err := reg.Stage()
	if err != nil {
		return err
	}
	reg.Commit(snap)
	return nil
}

func TestCoalescerDigestWindowFromPolicy(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"CDIGEST": {"mode": "digest", "interval_minutes": 10}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	c := newInboundCoalescer(8*time.Second, reg)
	if w := c.windowFor("CDIGEST"); w != 10*time.Minute {
		t.Fatalf("windowFor(CDIGEST) = %v, want 10m", w)
	}
	if w := c.windowFor("COTHER"); w != 8*time.Second {
		t.Fatalf("windowFor(COTHER) = %v, want 8s", w)
	}
}

func TestFormatCoalescedBlockShape(t *testing.T) {
	cfg := config{} // nil channelNames → raw channel id
	batch := []pendingChannelInbound{
		testPending("C1", "1.000000", "first line"),
		testPending("C1", "2.000000", "second"),
	}
	batch[1].inbound.ReplyToMessageID = "1.000000"
	got := formatCoalescedBlock(cfg, "C1", batch)

	for _, want := range []string{
		"[2 messages in C1, coalesced. Reply with --turn-ts 2.000000 to answer the newest",
		"--reply-to <its thread ts> or --no-thread",
		"[1.000000] Afik: first line",
		"[2.000000] Afik (in thread 1.000000): second",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("block missing %q:\n%s", want, got)
		}
	}
}

func TestCoalesceBatchDedupKeyDiffersFromMemberKeys(t *testing.T) {
	batch := []pendingChannelInbound{
		testPending("C1", "1.0", "a"),
		testPending("C1", "2.0", "b"),
	}
	key := coalesceBatchDedupKey(batch)
	for _, p := range batch {
		if key == p.inbound.DedupKey {
			t.Fatalf("batch key %q must differ from member key %q — a bot-mention twin delivering separately would dedup the whole batch away", key, p.inbound.DedupKey)
		}
	}
	if key != coalesceBatchDedupKey(batch) {
		t.Fatal("batch key must be deterministic for retry idempotency")
	}
}

// TestDeliverCoalescedBatchSingleMessagePassthrough pins the zero-churn
// contract: a batch of one delivers its envelope text byte-identical to
// the immediate path (no coalesce header), so the common single-message
// case is unchanged on the wire.
func TestDeliverCoalescedBatchSingleMessagePassthrough(t *testing.T) {
	bodyCh := make(chan externalInboundMessage, 1)
	gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&envelope)
		bodyCh <- envelope.Message
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(gcStub.Close)

	cfg := config{
		gcAPIBase:    gcStub.URL,
		cityName:     "test",
		peerContext:  newPeerContextBuffer(),
		deliveredIDs: newDeliveredIDs(),
		// replyHelp deliberately nil: bare configs get no help block.
	}
	if !deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{testPending("C1", "1.0", "hello world")}) {
		t.Fatal("deliver failed")
	}
	got := <-bodyCh
	if got.Text != "hello world" {
		t.Fatalf("single-message text = %q, want passthrough %q", got.Text, "hello world")
	}
	if got.ProviderMessageID != "1.0" {
		t.Fatalf("ProviderMessageID = %q, want 1.0", got.ProviderMessageID)
	}
	if got.DedupKey != "slack-1.0" {
		t.Fatalf("single-message DedupKey = %q, want the member's own", got.DedupKey)
	}
	if !cfg.deliveredIDs.seen("", "C1", "1.0") {
		t.Fatal("delivered ts not recorded")
	}
}

// TestDeliverCoalescedBatchMultiMergesEnvelopeAndRecords pins the
// multi-message envelope: sorted by ts, newest message's identity, a
// batch-specific dedup key, all texts present, every ts recorded, peer
// context riding in front, help block once per channel.
func TestDeliverCoalescedBatchMultiMergesEnvelopeAndRecords(t *testing.T) {
	bodyCh := make(chan externalInboundMessage, 2)
	gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var envelope struct {
			Message externalInboundMessage `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&envelope)
		bodyCh <- envelope.Message
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(gcStub.Close)

	peer := newPeerContextBuffer()
	peer.add(peerContextItem{Label: "sinan", Channel: "C1", TS: "0.5", Text: "peer says hi"})
	cfg := config{
		gcAPIBase:    gcStub.URL,
		cityName:     "test",
		peerContext:  peer,
		deliveredIDs: newDeliveredIDs(),
		replyHelp:    newOncePerChannel(),
	}
	// Deliberately out of order: enqueue order is handler-completion
	// order; delivery must sort by Slack ts.
	batch := []pendingChannelInbound{
		testPending("C1", "2.0", "second"),
		testPending("C1", "1.0", "first"),
	}
	if !deliverCoalescedBatch(cfg, "C1", batch) {
		t.Fatal("deliver failed")
	}
	got := <-bodyCh
	if got.ProviderMessageID != "2.0" {
		t.Fatalf("envelope must carry newest ts, got %q", got.ProviderMessageID)
	}
	if got.DedupKey != "slack-batch-1.0-2.0-2" {
		t.Fatalf("multi-batch DedupKey = %q", got.DedupKey)
	}
	if !strings.Contains(got.Text, "[1.0] Afik: first") || strings.Index(got.Text, "[1.0]") > strings.Index(got.Text, "[2.0]") {
		t.Fatalf("messages must render in ts order:\n%s", got.Text)
	}
	for _, want := range []string{"first", "second", "peer says hi", peerContextHeader,
		"full reply how-to", "gc slack reply-current --conversation-id C1"} {
		if !strings.Contains(got.Text, want) {
			t.Fatalf("delivered text missing %q:\n%s", want, got.Text)
		}
	}
	if !cfg.deliveredIDs.seen("", "C1", "1.0") || !cfg.deliveredIDs.seen("", "C1", "2.0") {
		t.Fatal("all batch ts must be recorded as delivered")
	}
	// Help block is once per channel: second delivery must not repeat it.
	if !deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{testPending("C1", "3.0", "third")}) {
		t.Fatal("second deliver failed")
	}
	second := <-bodyCh
	if strings.Contains(second.Text, "full reply how-to") {
		t.Fatalf("help block must appear once per channel:\n%s", second.Text)
	}
}

// TestDeliverCoalescedBatchFailureRestoresPeerAndHelp pins the failure
// contract: peer context restored, help claim rewound, false returned.
func TestDeliverCoalescedBatchFailureRestoresPeerAndHelp(t *testing.T) {
	gcStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(gcStub.Close)

	peer := newPeerContextBuffer()
	peer.add(peerContextItem{Label: "sinan", Channel: "C1", TS: "0.5", Text: "peer says hi"})
	cfg := config{
		gcAPIBase:   gcStub.URL,
		cityName:    "test",
		peerContext: peer,
		replyHelp:   newOncePerChannel(),
	}
	if deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}) {
		t.Fatal("deliver must report failure")
	}
	if items, _ := peer.flush("C1"); len(items) != 1 {
		t.Fatalf("peer context must be restored, got %d items", len(items))
	}
	if !cfg.replyHelp.first("C1") {
		t.Fatal("help claim must be rewound on failure")
	}
}

func TestOncePerChannel(t *testing.T) {
	var nilO *oncePerChannel
	if nilO.first("C1") {
		t.Fatal("nil tracker must never report first")
	}
	nilO.unmark("C1") // must not panic

	o := newOncePerChannel()
	if !o.first("C1") {
		t.Fatal("first call must report true")
	}
	if o.first("C1") {
		t.Fatal("second call must report false")
	}
	if !o.first("C2") {
		t.Fatal("channels are independent")
	}
	o.unmark("C1")
	if !o.first("C1") {
		t.Fatal("unmark must rewind the claim")
	}
	if o.first("") {
		t.Fatal("empty channel must never claim")
	}
}
