package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// errDeliverFailed is the generic (untyped ⇒ TRANSIENT) delivery failure
// test hooks return where they used to return false: the coalescer
// restores and retries exactly as before gp-xnc.
var errDeliverFailed = errors.New("deliver failed (test: transient)")

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
func collectingDeliver() (func(string, []pendingChannelInbound) error, chan []pendingChannelInbound) {
	ch := make(chan []pendingChannelInbound, 16)
	return func(channel string, batch []pendingChannelInbound) error {
		ch <- batch
		return nil
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
	_, resetMu, ok := c.takeLocked("C1") // reset buffer state
	c.mu.Unlock()
	if !ok {
		t.Fatal("reset take failed — no delivery is in flight here")
	}
	// The take holds the delivery mutex and opens an in-flight window
	// (round 3 take-time discipline); settle both for the reset.
	resetMu.Unlock()
	c.endDelivery()
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		defer mu.Unlock()
		attempts <- batch
		if fails > 0 {
			fails--
			return errDeliverFailed
		}
		return nil
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		attempts <- batch
		if fail {
			return errDeliverFailed
		}
		return nil
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		attempts <- batch
		if deliverOK {
			return nil
		}
		return errDeliverFailed
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		attempts <- batch
		return nil
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
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
		return nil
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

// Structural close of the sweep reorder race (gp-9e7 fix round 2c): the
// swept channel's delivery mutex is acquired AT TAKE TIME, inside the
// same c.mu critical section that detaches the batch — so there is no
// instant at which the batch is out of the maps with the mutex free.
// The delivery goroutine unlocks it after delivering.
func TestCoalescerSweptMutexHeldFromTakeTime(t *testing.T) {
	delivered := make(chan string, 1)
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		delivered <- channel
		return nil
	}
	c.enqueue("C_SWEPT", testPending("C_SWEPT", "1.0", "older"))
	c.mu.Lock()
	swept := c.takeSweepsLocked("C_TRIGGER")
	c.mu.Unlock()
	if len(swept) != 1 || swept[0].channel != "C_SWEPT" {
		t.Fatalf("swept = %+v, want the one buffered channel", swept)
	}
	if swept[0].mu.TryLock() {
		swept[0].mu.Unlock()
		t.Fatal("swept channel's delivery mutex is FREE after the take — the take-to-lock gap is back")
	}
	c.deliverSwept(swept)
	select {
	case ch := <-delivered:
		if ch != "C_SWEPT" {
			t.Fatalf("delivered %q, want C_SWEPT", ch)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("swept delivery did not run")
	}
	// After the delivery settles the goroutine releases the mutex.
	freed := make(chan struct{})
	go func() {
		swept[0].mu.Lock()
		swept[0].mu.Unlock()
		close(freed)
	}()
	select {
	case <-freed:
	case <-time.After(2 * time.Second):
		t.Fatal("swept delivery goroutine never released the handed-off mutex")
	}
}

// gp-9e7 fix round 2c, the behavioral guarantee: an urgent flushAheadOf
// racing a swept delivery can NEVER deliver before the older swept
// batch. The swept mutex is held from take time, so flushAheadOf —
// which takes its own batch and then blocks on that same mutex —
// serializes strictly behind it by construction. Deterministic: the
// ordering is enforced by the mutex handoff, not by sleeps.
func TestCoalescerUrgentFlushAheadNeverOvertakesSweptBatch(t *testing.T) {
	var omu sync.Mutex
	var order []string
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		omu.Lock()
		order = append(order, batch[0].inbound.ProviderMessageID)
		omu.Unlock()
		return nil
	}
	c.enqueue("C_SWEPT", testPending("C_SWEPT", "1.0", "older swept chatter"))
	// The sweep takes exactly as flushTimer does — under c.mu, mutex
	// acquired with the take.
	c.mu.Lock()
	swept := c.takeSweepsLocked("C_TRIGGER")
	c.mu.Unlock()
	if len(swept) != 1 {
		t.Fatalf("swept = %+v, want one batch", swept)
	}
	// A newer message lands and its urgent flush-ahead races the swept
	// delivery goroutine.
	c.enqueue("C_SWEPT", testPending("C_SWEPT", "2.0", "newer"))
	aheadDone := make(chan struct{})
	go func() {
		c.flushAheadOf("C_SWEPT", "")
		close(aheadDone)
	}()
	c.deliverSwept(swept)
	select {
	case <-aheadDone:
	case <-time.After(3 * time.Second):
		t.Fatal("flushAheadOf never completed")
	}
	omu.Lock()
	defer omu.Unlock()
	if len(order) != 2 || order[0] != "1.0" || order[1] != "2.0" {
		t.Fatalf("delivery order = %v, want [1.0 2.0] — the urgent flush-ahead overtook the older swept batch", order)
	}
}

// The take-time acquisition is TryLock, never a blocking Lock: a
// channel whose delivery mutex is held (a delivery in flight right now)
// is SKIPPED by the sweep — blocking under c.mu would deadlock against
// that delivery's failure-path restore and stall every enqueue — and
// keeps its buffer and armed timer, flushing on its own schedule.
func TestCoalescerSweepSkipsChannelWithDeliveryInFlight(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(string, []pendingChannelInbound) error { return nil }
	c.enqueue("C_BUSY", testPending("C_BUSY", "1.0", "buffered behind an in-flight delivery"))
	c.mu.Lock()
	busyMu := c.flushMuFor("C_BUSY")
	c.mu.Unlock()
	busyMu.Lock() // simulate the in-flight delivery holding the channel
	defer busyMu.Unlock()
	c.mu.Lock()
	swept := c.takeSweepsLocked("C_OTHER")
	pendingN := len(c.pending["C_BUSY"])
	_, timerArmed := c.timers["C_BUSY"]
	c.mu.Unlock()
	if len(swept) != 0 {
		t.Fatalf("swept %d channel(s), want 0 — a contended channel must be skipped, not blocked on", len(swept))
	}
	if pendingN != 1 || !timerArmed {
		t.Fatalf("skipped channel: pending=%d timerArmed=%v, want its buffer and timer intact", pendingN, timerArmed)
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			close(firstAttempt)     // timer fired; batch now detached
			<-release               // hold it in flight while flushAll starts
			return errDeliverFailed // fail → restore lands AFTER a one-shot snapshot
		}
		delivered <- batch
		return nil
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
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		if channel == "C_SWEPT" {
			mu.Lock()
			sweptAttempts++
			n := sweptAttempts
			mu.Unlock()
			if n == 1 {
				close(sweptTaken)
				<-release
				return errDeliverFailed // restore after flushAll's first look
			}
		}
		delivered <- channel
		return nil
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

// A delivery that fails every attempt must not spin flushAll forever —
// the fixpoint loop is bounded — and the residue at the bound must NOT
// be dropped (gp-9e7 fix round 2a'): the liveness watermark advanced
// when these items were ADMITTED, so neither Slack redelivery nor the
// startup watermark backfill can ever recover them — the spill hook
// (the durable spool) is their only redelivery path. This replaces the
// old test that blessed the drop.
func TestCoalescerFlushAllBoundedAndSpillsResidue(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	var mu sync.Mutex
	attempts := 0
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		attempts++
		mu.Unlock()
		return errDeliverFailed
	}
	spilled := map[string][]pendingChannelInbound{}
	c.spill = func(channel string, batch []pendingChannelInbound) bool {
		mu.Lock()
		spilled[channel] = append(spilled[channel], batch...)
		mu.Unlock()
		return true
	}
	c.enqueue("C1", testPending("C1", "1.0", "doomed message"))
	if !c.admitReaction("C1", testPending("C1", "2.0", "doomed reaction"), false) {
		t.Fatal("admitReaction refused")
	}
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
	defer mu.Unlock()
	if attempts < 1 || attempts > maxFlushAllPasses {
		t.Fatalf("attempts = %d, want between 1 and %d", attempts, maxFlushAllPasses)
	}
	got := spilled["C1"]
	if len(got) != 2 {
		t.Fatalf("spilled %d item(s), want the doomed message AND reaction: %+v", len(got), got)
	}
	byTS := map[string]bool{}
	for _, p := range got {
		byTS[p.inbound.ProviderMessageID] = p.reaction
	}
	if r, ok := byTS["1.0"]; !ok || r {
		t.Errorf("message 1.0 spilled=%v reaction=%v, want spilled as a message", ok, r)
	}
	if r, ok := byTS["2.0"]; !ok || !r {
		t.Errorf("reaction 2.0 spilled=%v reaction=%v, want spilled with its reaction flag", ok, r)
	}
	c.mu.Lock()
	pendingN, reactionsN, closed := len(c.pending), len(c.reactions), c.closed
	c.mu.Unlock()
	if pendingN != 0 || reactionsN != 0 || !closed {
		t.Fatalf("after flushAll: pending=%d reactions=%d closed=%v, want empty maps and a closed barrier", pendingN, reactionsN, closed)
	}
}

// Admission barrier (gp-9e7 fix round 2b'): once flushAll's final
// snapshot closes the coalescer, a straggler admission — an event
// goroutine that outlived main's bounded eventWG wait — must never land
// in the maps (it would sit past the final snapshot forever); it routes
// to the spill hook instead, and no timer arms.
func TestCoalescerClosedAfterFlushAllSpillsLateAdmissions(t *testing.T) {
	c := newInboundCoalescer(time.Hour, nil)
	c.deliver = func(string, []pendingChannelInbound) error { return nil }
	var mu sync.Mutex
	spilled := map[string][]pendingChannelInbound{}
	c.spill = func(channel string, batch []pendingChannelInbound) bool {
		mu.Lock()
		spilled[channel] = append(spilled[channel], batch...)
		mu.Unlock()
		return true
	}
	c.flushAll() // empty drain: the barrier closes with the clean verdict
	c.enqueue("C1", testPending("C1", "1.0", "straggler message"))
	if !c.admitReaction("C1", testPending("C1", "2.0", "straggler reaction"), false) {
		t.Fatal("admitReaction refused")
	}
	c.mu.Lock()
	pendingN, reactionsN, timersN := len(c.pending), len(c.reactions), len(c.timers)
	c.mu.Unlock()
	if pendingN != 0 || reactionsN != 0 || timersN != 0 {
		t.Fatalf("post-close admission reached memory: pending=%d reactions=%d timers=%d, want 0/0/0", pendingN, reactionsN, timersN)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(spilled["C1"]) != 2 {
		t.Fatalf("spilled %d item(s), want both straggler admissions", len(spilled["C1"]))
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
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{testPending("C1", "1.0", "hello world")}); err != nil {
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
	if err := deliverCoalescedBatch(cfg, "C1", batch); err != nil {
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
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{testPending("C1", "3.0", "third")}); err != nil {
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
	if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{testPending("C1", "1.0", "x")}); err == nil {
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

// --- gp-9e7 round 3: generalized take-time lock discipline ------------------

// blockableDeliver returns a deliver hook that records every delivery's
// (channel, first-ts) arrival order and blocks the FIRST delivery for
// blockChannel until gate closes. entered signals once that first
// delivery is inside the hook (batch detached, mutex held from take).
func blockableDeliver(blockChannel string, gate, entered chan struct{}) (func(string, []pendingChannelInbound) error, func() []string) {
	var mu sync.Mutex
	var order []string
	var blockedOnce bool
	deliver := func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		first := ""
		if len(batch) > 0 {
			first = batch[0].inbound.ProviderMessageID
		}
		order = append(order, channel+":"+first)
		block := channel == blockChannel && !blockedOnce
		if block {
			blockedOnce = true
		}
		mu.Unlock()
		if block {
			close(entered)
			<-gate
		}
		return nil
	}
	read := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), order...)
	}
	return deliver, read
}

// Round 3 headline invariant: an ORDINARY take (the firing channel's
// own timer) holds the delivery mutex from take time, so while a
// delivery is in flight (a) a competing timer take for the same channel
// DEFERS — the newer batch stays in the maps, is never detached into
// the take-to-lock gap a sweep could overtake — and (b) a sweep skips
// the channel. Pre-round-3, the manual flushTimer below would DETACH
// the newer batch and block, emptying pending while 2.0 was still
// undelivered (and, in the live race, lettable ahead of 1.0 by a sweep).
func TestCoalescerTimerTakeDefersWhileDeliveryInFlight(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	deliver, order := blockableDeliver("C_A", gate, entered)
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver

	// 1.0 flushes on its timer and blocks inside the deliver hook: the
	// batch is detached with its delivery mutex held from take time.
	c.enqueue("C_A", testPending("C_A", "1.0", "older, in flight"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery never started")
	}

	// A newer message buffers while the older delivery is in flight.
	c.enqueue("C_A", testPending("C_A", "2.0", "newer"))
	c.mu.Lock()
	g := c.gen["C_A"]
	c.mu.Unlock()

	// (a) A competing timer take must DEFER, not detach: it returns
	// promptly, 2.0 is still in the maps, and the timer is re-armed.
	timerDone := make(chan struct{})
	go func() {
		c.flushTimer("C_A", g)
		close(timerDone)
	}()
	select {
	case <-timerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("flushTimer blocked behind the in-flight delivery — the take must defer, not detach-and-wait")
	}
	if !c.pendingContains("C_A", "2.0") {
		t.Fatal("newer batch was detached while an older delivery was in flight — the take-to-lock gap is back")
	}
	c.mu.Lock()
	_, timerArmed := c.timers["C_A"]
	c.mu.Unlock()
	if !timerArmed {
		t.Fatal("deferred timer take must re-arm the flush timer")
	}

	// (b) A sweep must skip the channel outright.
	c.mu.Lock()
	swept := c.takeSweepsLocked("C_OTHER")
	c.mu.Unlock()
	if len(swept) != 0 {
		t.Fatalf("sweep took %d batch(es) from a channel with a delivery in flight", len(swept))
	}

	// Release the older delivery; the re-armed timer delivers 2.0 after.
	close(gate)
	deadline := time.After(3 * time.Second)
	for {
		got := order()
		if len(got) >= 2 {
			if got[0] != "C_A:1.0" || got[1] != "C_A:2.0" {
				t.Fatalf("delivery order = %v, want [C_A:1.0 C_A:2.0]", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("2.0 never delivered after the in-flight delivery settled (order=%v)", order())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The cap-triggered early flush follows the same discipline: with a
// delivery in flight, enqueue must NOT detach the over-cap buffer (or
// block the dispatch goroutine behind the in-flight POST, the
// pre-round-3 behavior) — it defers to the armed timer and keeps every
// message buffered.
func TestCoalescerEarlyFlushDefersWhileDeliveryInFlight(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	deliver, order := blockableDeliver("C_A", gate, entered)
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver

	c.enqueue("C_A", testPending("C_A", "000000.0", "older, in flight"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery never started")
	}

	enqueued := make(chan struct{})
	go func() {
		for i := 1; i <= maxCoalescePerChannel; i++ {
			c.enqueue("C_A", testPending("C_A", fmt.Sprintf("%06d.0", i), "burst"))
		}
		close(enqueued)
	}()
	select {
	case <-enqueued:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked behind the in-flight delivery — the early flush must defer, not detach-and-wait")
	}
	c.mu.Lock()
	pendingN := len(c.pending["C_A"])
	_, timerArmed := c.timers["C_A"]
	c.mu.Unlock()
	if pendingN != maxCoalescePerChannel || !timerArmed {
		t.Fatalf("deferred early flush: pending=%d timerArmed=%v, want %d buffered with an armed timer", pendingN, timerArmed, maxCoalescePerChannel)
	}

	close(gate)
	deadline := time.After(3 * time.Second)
	for {
		got := order()
		if len(got) >= 2 {
			if got[0] != "C_A:000000.0" || got[1] != "C_A:000001.0" {
				t.Fatalf("delivery order = %v, want the in-flight batch then the deferred burst", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("deferred burst never delivered (order=%v)", order())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// The reaction-overflow flush and the post-delivery reaction drain
// follow the discipline too: with a delivery in flight they leave the
// side-buffer intact instead of detaching it.
func TestCoalescerReactionTakesDeferWhileDeliveryInFlight(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	deliver, _ := blockableDeliver("C_A", gate, entered)
	defer close(gate)
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver

	c.enqueue("C_A", testPending("C_A", "1.0", "older, in flight"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery never started")
	}

	r := testPending("C_A", "2.0", "reaction")
	r.reaction = true
	if !c.admitReaction("C_A", r, false) {
		t.Fatal("admitReaction refused")
	}
	c.deliverBufferedReactions("C_A")
	c.mu.Lock()
	reactionsN := len(c.reactions["C_A"])
	c.mu.Unlock()
	if reactionsN != 1 {
		t.Fatalf("reaction drain detached %d buffered reaction(s) behind an in-flight delivery, want them kept", 1-reactionsN)
	}
}

// The bead's verification bar: a same-channel ordinary-take vs sweep
// overtake interleaving. Channel A's deliveries are triggered both by
// its own timers/early-flushes and by sweeps from channel B firing
// concurrently; pre-round-3, an ordinary take detached its batch with
// the mutex still free, so a sweep (or any later take) could hand a
// NEWER batch the mutex first and the newer POST overtook the older.
// With the mutex held from take time the per-channel delivery order is
// monotone by construction; this stress run asserts exactly that.
func TestCoalescerSameChannelSweepInterleavingNeverReorders(t *testing.T) {
	var mu sync.Mutex
	firstTS := map[string][]string{}
	c := newInboundCoalescer(2*time.Millisecond, nil)
	c.deliver = func(channel string, batch []pendingChannelInbound) error {
		mu.Lock()
		if len(batch) > 0 {
			firstTS[channel] = append(firstTS[channel], batch[0].inbound.ProviderMessageID)
		}
		mu.Unlock()
		time.Sleep(200 * time.Microsecond) // widen the in-flight window the old gap raced
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // channel A: steady bursts, its own timers + cap flushes
		defer wg.Done()
		for i := 0; i < 400; i++ {
			c.enqueue("C_A", testPending("C_A", fmt.Sprintf("%06d.0", i), "a"))
			if i%7 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	go func() { // channel B: its timer flushes sweep A concurrently
		defer wg.Done()
		for i := 0; i < 400; i++ {
			c.enqueue("C_B", testPending("C_B", fmt.Sprintf("%06d.0", i), "b"))
			if i%5 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	wg.Wait()
	time.Sleep(20 * time.Millisecond)
	c.flushAll()

	mu.Lock()
	defer mu.Unlock()
	for channel, seq := range firstTS {
		for i := 1; i < len(seq); i++ {
			if seq[i] <= seq[i-1] {
				t.Fatalf("chan=%s delivery %d (first ts %s) overtook delivery %d (first ts %s): full order %v",
					channel, i, seq[i], i-1, seq[i-1], seq)
			}
		}
	}
	if len(firstTS["C_A"]) == 0 || len(firstTS["C_B"]) == 0 {
		t.Fatalf("stress produced no deliveries to check: %v", firstTS)
	}
}

// Round 5 (3a): an over-cap buffer whose early flush lost the take to an
// in-flight delivery must retry on COALESCE-WINDOW scale, never the
// channel's digest interval. Before the fix the deferred flush kept the
// armed digest timer — a digest channel's full buffer sat past the cap
// for the remainder of a possibly hours-long window while the in-flight
// delivery had long since settled.
func TestCoalescerOverCapDeferredFlushRetriesOnWindowScaleNotDigest(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"CDIGEST": {"mode": "digest", "interval_minutes": 10}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	gate := make(chan struct{})
	entered := make(chan struct{})
	deliver, order := blockableDeliver("CDIGEST", gate, entered)
	c := newInboundCoalescer(30*time.Millisecond, reg)
	c.deliver = deliver

	// One message buffers (arming the 10m digest timer); a manual timer
	// fire detaches it and blocks inside the deliver hook.
	c.enqueue("CDIGEST", testPending("CDIGEST", "000000.0", "older, in flight"))
	c.mu.Lock()
	g := c.gen["CDIGEST"]
	c.mu.Unlock()
	go c.flushTimer("CDIGEST", g)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery never started")
	}

	// A burst fills the buffer to the cap while the delivery is in
	// flight: the early flush loses the take and must defer — but to a
	// short retry, not the re-armed 10-minute digest window.
	for i := 1; i <= maxCoalescePerChannel; i++ {
		c.enqueue("CDIGEST", testPending("CDIGEST", fmt.Sprintf("%06d.0", i), "burst"))
	}
	c.mu.Lock()
	pendingN := len(c.pending["CDIGEST"])
	_, timerArmed := c.timers["CDIGEST"]
	c.mu.Unlock()
	if pendingN != maxCoalescePerChannel || !timerArmed {
		t.Fatalf("deferred early flush: pending=%d timerArmed=%v, want %d buffered with an armed retry timer", pendingN, timerArmed, maxCoalescePerChannel)
	}

	// Once the in-flight delivery settles, the over-cap buffer must
	// flush promptly — within seconds, not the digest interval.
	close(gate)
	deadline := time.After(3 * time.Second)
	for {
		got := order()
		if len(got) >= 2 {
			if got[0] != "CDIGEST:000000.0" || got[1] != "CDIGEST:000001.0" {
				t.Fatalf("delivery order = %v, want the in-flight batch then the deferred over-cap burst", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("over-cap buffer never flushed promptly after the in-flight delivery settled (order=%v) — deferred to the digest timer", order())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Round-6 gate finding: SIGHUP reconciliation must not downgrade an
// over-cap retry timer back to digest scale. Same shape as the test
// above, with reconcileTimers() fired while the deferred retry is
// armed — before the fix the reconcile unconditionally re-armed
// windowFor (the 10-minute digest interval) and the over-cap buffer
// sat for the full interval once the in-flight delivery settled.
func TestCoalescerReconcileKeepsOverCapRetryShort(t *testing.T) {
	path := writeDeliveryPolicyFile(t, `{"channels": {"CDIGEST": {"mode": "digest", "interval_minutes": 10}}}`)
	reg, err := newDeliveryPolicyRegistry(path)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	gate := make(chan struct{})
	entered := make(chan struct{})
	deliver, order := blockableDeliver("CDIGEST", gate, entered)
	c := newInboundCoalescer(30*time.Millisecond, reg)
	c.deliver = deliver

	c.enqueue("CDIGEST", testPending("CDIGEST", "000000.0", "older, in flight"))
	c.mu.Lock()
	g := c.gen["CDIGEST"]
	c.mu.Unlock()
	go c.flushTimer("CDIGEST", g)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery never started")
	}

	for i := 1; i <= maxCoalescePerChannel; i++ {
		c.enqueue("CDIGEST", testPending("CDIGEST", fmt.Sprintf("%06d.0", i), "burst"))
	}
	c.mu.Lock()
	pendingN := len(c.pending["CDIGEST"])
	_, timerArmed := c.timers["CDIGEST"]
	c.mu.Unlock()
	if pendingN != maxCoalescePerChannel || !timerArmed {
		t.Fatalf("deferred early flush: pending=%d timerArmed=%v, want %d buffered with an armed retry timer", pendingN, timerArmed, maxCoalescePerChannel)
	}

	// The SIGHUP path fires while the retry is armed and the delivery
	// is still in flight: the re-armed timer must stay retry-scale.
	c.reconcileTimers()

	close(gate)
	deadline := time.After(3 * time.Second)
	for {
		got := order()
		if len(got) >= 2 {
			if got[0] != "CDIGEST:000000.0" || got[1] != "CDIGEST:000001.0" {
				t.Fatalf("delivery order = %v, want the in-flight batch then the deferred over-cap burst", got)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("over-cap buffer never flushed promptly after reconcileTimers + delivery settle (order=%v) — SIGHUP downgraded the retry to digest scale", order())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Round 5 (3b): a reaction side-buffer that overflowed its cap while a
// delivery was in flight must still deliver within a bounded time of
// that delivery settling — WITHOUT waiting for another admission or the
// channel's next real moment. Before the fix the deferred overflow
// armed nothing: with zero further traffic the over-cap side-buffer sat
// indefinitely.
func TestCoalescerReactionOverflowDeferredArmsBoundedRetry(t *testing.T) {
	gate := make(chan struct{})
	entered := make(chan struct{})
	deliver, order := blockableDeliver("C_A", gate, entered)
	c := newInboundCoalescer(30*time.Millisecond, nil)
	c.deliver = deliver

	// A real message's timer flush blocks in the deliver hook.
	c.enqueue("C_A", testPending("C_A", "000000.0", "older, in flight"))
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first delivery never started")
	}

	// Reactions overflow the cap while the delivery is in flight. The
	// overflow flush loses the take and defers.
	for i := 1; i <= maxBufferedReactionsPerChannel; i++ {
		r := testPending("C_A", fmt.Sprintf("%06d.0", i), "reaction")
		r.reaction = true
		if !c.admitReaction("C_A", r, false) {
			t.Fatal("admitReaction refused")
		}
	}
	c.mu.Lock()
	reactionsN := len(c.reactions["C_A"])
	c.mu.Unlock()
	if reactionsN != maxBufferedReactionsPerChannel {
		t.Fatalf("side-buffer holds %d reactions, want the full deferred overflow of %d", reactionsN, maxBufferedReactionsPerChannel)
	}

	// After the in-flight delivery settles, the deferred overflow must
	// deliver on its own — no further admission, no real traffic.
	close(gate)
	deadline := time.After(3 * time.Second)
	for {
		got := order()
		if len(got) >= 2 {
			c.mu.Lock()
			left := len(c.reactions["C_A"])
			c.mu.Unlock()
			if left != 0 {
				t.Fatalf("overflowed side-buffer still holds %d reactions after the retry delivery", left)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("deferred reaction overflow never delivered after the in-flight delivery settled (order=%v)", order())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Round 5 (3c): flushAheadOf's wait must be a RESERVATION, not a
// wait-unlock-retake race. The urgent path blocks INTO the channel
// mutex's wait queue and takes with the mutex held, so from the moment
// the in-flight delivery settles no competing taker can TryLock-win the
// channel — the old loop's unlock-to-retake window let a steady stream
// of timer/early-flush takers starve the urgent flush indefinitely (and
// steal its batch). Hot competing takers across repeated cycles must
// record ZERO successful takes.
func TestCoalescerFlushAheadReservationCannotBeStarved(t *testing.T) {
	for cycle := 0; cycle < 25; cycle++ {
		gate := make(chan struct{})
		entered := make(chan struct{})
		deliver, order := blockableDeliver("C_A", gate, entered)
		c := newInboundCoalescer(time.Hour, nil) // no self-firing timers
		c.deliver = deliver

		// Older delivery in flight, holding the channel mutex.
		c.enqueue("C_A", testPending("C_A", "1.0", "older, in flight"))
		c.mu.Lock()
		g := c.gen["C_A"]
		c.mu.Unlock()
		go c.flushTimer("C_A", g)
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("first delivery never started")
		}

		// The batch the urgent path must win.
		c.enqueue("C_A", testPending("C_A", "2.0", "urgent must flush this ahead"))

		urgentDone := make(chan struct{})
		go func() {
			c.flushAheadOf("C_A", "")
			close(urgentDone)
		}()
		// Let the urgent path reach its blocking wait (>1ms also arms
		// Go's starvation-mode mutex handoff for it).
		time.Sleep(10 * time.Millisecond)

		// Hot competing takers: timer/early-flush-style takes hammering
		// the channel until the urgent path completes.
		var steals int32
		stop := make(chan struct{})
		var spinners sync.WaitGroup
		for i := 0; i < 4; i++ {
			spinners.Add(1)
			go func() {
				defer spinners.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					c.mu.Lock()
					batch, mu, ok := c.takeLocked("C_A")
					c.mu.Unlock()
					if ok {
						if len(batch) > 0 {
							atomic.AddInt32(&steals, 1)
						}
						c.deliverBatch("C_A", batch, mu)
					}
				}
			}()
		}

		close(gate) // settle the in-flight delivery
		select {
		case <-urgentDone:
		case <-time.After(5 * time.Second):
			close(stop)
			spinners.Wait()
			t.Fatalf("cycle %d: urgent flushAheadOf starved by competing takers (order=%v)", cycle, order())
		}
		close(stop)
		spinners.Wait()
		if n := atomic.LoadInt32(&steals); n != 0 {
			t.Fatalf("cycle %d: competing takers stole %d batch(es) out from under the waiting urgent flush — the reservation does not hold", cycle, n)
		}
		got := order()
		if len(got) < 2 || got[1] != "C_A:2.0" {
			t.Fatalf("cycle %d: urgent flush did not deliver the buffered batch ahead (order=%v)", cycle, got)
		}
	}
}
