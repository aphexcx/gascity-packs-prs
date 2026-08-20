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
	nilC.flushAheadOf("C1", "")                           // must not panic
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
