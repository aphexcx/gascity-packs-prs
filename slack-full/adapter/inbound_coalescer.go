package main

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// --- inbound burst coalescing (gp-729 items 1, 3, 6) ------------------------
//
// Rapid-fire human messages in a bound channel are usually one thought
// split across sends, but each historically produced its own full
// system-reminder in the bound session — N wrappers, N reply
// boilerplates, N wake-ups. The coalescer buffers untargeted,
// non-bot-mentioned ROOM inbounds for a short window
// (SLACK_COALESCE_WINDOW, default 8s; 0 disables) and delivers them as
// ONE inbound whose text block carries every message verbatim. A
// channel flipped to digest mode in delivery_policy.json uses its
// configured interval instead (item 6). DMs and MPIMs never buffer —
// a direct conversation stays snappy.
//
// Messages that carry an explicit target or an @-mention of the
// adapter's own bot user NEVER buffer: they keep today's exact
// busy-reaction / alias-dispatch / dedup flow, and any pending buffer
// for the channel is flushed AHEAD of them so ordering holds. This is
// the same ride-ahead contract as peerContextBuffer, whose
// flush/restore semantics this generalizes.
//
// Nothing is ever evicted: a buffer reaching maxCoalescePerChannel
// flushes early instead of dropping — the digest contract is
// "verbatim, nothing dropped or summarized", so the cap bounds memory
// by bounding latency, not content. The window is fixed from the first
// buffered message (not a sliding debounce): steady chatter therefore
// flushes every window instead of being deferred indefinitely, at the
// cost of occasionally splitting a burst that straddles a boundary.
//
// Cross-channel coalescing (gp-9e7 item 2): a firing flush timer also
// drains every other buffered non-digest channel, so one idle wake
// delivers ALL channels' window traffic as aligned back-to-back
// per-channel batches (each formatted exactly as before) instead of a
// string of wakes seconds apart. Reaction notifications additionally
// buffer in a no-wake side lane (gp-9e7 item 1, admitReaction) that
// only ever delivers by merging into a real batch take.
//
// Concurrency contract: per-channel state is generation-stamped — every
// drain bumps the generation, and a timer callback armed for an older
// generation no-ops, so a stale timer can never steal a newer batch. A
// per-channel flush mutex serializes deliveries; flushAheadOf acquires
// it even when the buffer is empty, so an urgent message cannot
// overtake a coalesced POST already in flight.
//
// In-memory like peerContextBuffer: messages admitted to the buffer
// were already acked to Slack, so an adapter CRASH inside the window
// loses that window's chatter — the acceptance the peer buffer
// documents, bounded here by the window. A normal shutdown drains
// every buffer first (flushAll). A failed flush restores the batch and
// retries on the next timer.

// defaultCoalesceWindow is the debounce for burst coalescing; inside
// the 5-15s band the Aug-17 plan named.
const defaultCoalesceWindow = 8 * time.Second

// maxCoalescePerChannel is the early-flush threshold: a buffer this
// full delivers immediately rather than waiting out its window.
const maxCoalescePerChannel = 50

// maxBufferedReactionsPerChannel bounds the no-wake reaction
// side-buffer (gp-9e7 item 1). Reactions must never drop, so overflow
// delivers the channel's whole take immediately instead of evicting —
// the one case a reaction is allowed to wake a session solo, and it
// takes a pathological reaction volume with zero real traffic to reach.
const maxBufferedReactionsPerChannel = 100

// maxFlushAllPasses bounds the shutdown drain's fixpoint loop: each
// pass re-takes whatever a failed delivery restored, so a gc outage
// during shutdown cannot spin the drain forever. Three passes is one
// initial drain plus two retries of persistent failures.
const maxFlushAllPasses = 3

// pendingChannelInbound is one buffered channel inbound awaiting
// coalesced delivery. The envelope's Text is final per-message text
// (thread preamble, mention rewrite, and files block already applied).
type pendingChannelInbound struct {
	inbound externalInboundMessage
	// reaction marks a no-wake reaction notification (gp-9e7 item 1):
	// admitted via admitReaction, merged into the channel's batch at
	// take time, and returned to the reaction side-buffer — never
	// re-arming a timer — when a failed delivery restores the batch.
	reaction bool
}

// inboundCoalescer owns the per-channel buffers and flush timers.
// Nil-safe throughout: a nil coalescer disables coalescing and every
// inbound takes the immediate path, byte-for-byte pre-gp-729 behavior.
type inboundCoalescer struct {
	mu      sync.Mutex
	pending map[string][]pendingChannelInbound
	// reactions is the no-wake side-buffer (gp-9e7 item 1): entries
	// here arm no timer and deliver only by merging into a batch taken
	// for the channel — a timer flush armed by real messages, the
	// flush-ahead of an urgent/DM delivery, or shutdown's flushAll.
	reactions map[string][]pendingChannelInbound
	gen       map[string]uint64
	timers    map[string]*time.Timer
	flushMu   map[string]*sync.Mutex
	window    time.Duration
	policy    *deliveryPolicyRegistry
	// inflight counts batches taken out of the maps but not yet
	// delivered (or restored by a failed delivery). Guarded by mu;
	// settled broadcasts every decrement. flushAll's fixpoint drain
	// needs the pair: a firing timer detaches its batch before the
	// network call, so "maps empty" alone does not mean "nothing left
	// to lose" — a detached batch restored on failure after a single
	// snapshot pass would be dropped on exit (gp-9e7 fix round 1a/2b).
	inflight int
	settled  *sync.Cond
	// closed flips on exactly once, inside flushAll, atomically (under
	// mu) with the drain's final verdict — the shutdown admission
	// barrier (gp-9e7 fix round 2b'). Once set, nothing enters the maps
	// again: an already-acked straggler event (an event goroutine that
	// outlived main's bounded eventWG wait) routes to spill instead of
	// being stranded in memory past the final snapshot.
	closed bool
	// spill receives batches the coalescer can no longer deliver in
	// this process lifetime: the leftovers of flushAll's bounded retry
	// loop, and post-close straggler admissions. Wired in main() to the
	// durable inbound spool (replayed at next startup); nil means such
	// batches are LOST, and every spill site logs loudly either way.
	// Called without mu held (it does file I/O).
	spill func(channel string, batch []pendingChannelInbound)
	// deliver posts one batch as a single inbound; false = failed (the
	// caller restores the batch). Wired in main() to deliverCoalescedBatch
	// with the final cfg; tests inject their own.
	deliver func(channel string, batch []pendingChannelInbound) bool
}

func newInboundCoalescer(window time.Duration, policy *deliveryPolicyRegistry) *inboundCoalescer {
	c := &inboundCoalescer{
		pending:   make(map[string][]pendingChannelInbound),
		reactions: make(map[string][]pendingChannelInbound),
		gen:       make(map[string]uint64),
		timers:    make(map[string]*time.Timer),
		flushMu:   make(map[string]*sync.Mutex),
		window:    window,
		policy:    policy,
	}
	c.settled = sync.NewCond(&c.mu)
	return c
}

// enabled reports whether buffering is active. A nil coalescer or a
// zero window means every message forwards immediately.
func (c *inboundCoalescer) enabled() bool {
	return c != nil && c.window > 0
}

// windowFor picks the channel's accumulation window: the operator's
// digest interval when configured, the burst debounce otherwise.
func (c *inboundCoalescer) windowFor(channel string) time.Duration {
	if d, ok := c.policy.digestInterval(channel); ok {
		return d
	}
	return c.window
}

// flushMuFor returns the channel's delivery mutex, creating it under
// c.mu. Entries are never removed — the population is the set of
// channels seen this adapter lifetime.
func (c *inboundCoalescer) flushMuFor(channel string) *sync.Mutex {
	if m, ok := c.flushMu[channel]; ok {
		return m
	}
	m := &sync.Mutex{}
	c.flushMu[channel] = m
	return m
}

// spillLateLocked routes one post-close admission out of the process:
// to the durable spool when wired (the next startup replays it), else
// to a loud loss log. It never touches the maps — the admission
// barrier's whole point is that nothing lands in memory after
// flushAll's final snapshot. Called with c.mu HELD; unlocks it.
func (c *inboundCoalescer) spillLateLocked(channel string, batch []pendingChannelInbound) {
	spill := c.spill
	c.mu.Unlock()
	if spill == nil {
		log.Printf("coalesce: chan=%s %d item(s) admitted after the shutdown drain with no spool wired — LOST (already acked to Slack)", channel, len(batch))
		return
	}
	log.Printf("coalesce: chan=%s %d item(s) admitted after the shutdown drain — spooled for startup replay", channel, len(batch))
	spill(channel, batch)
}

// enqueue buffers one inbound and arms the channel's flush timer if it
// isn't already running. A buffer reaching the cap flushes immediately
// (early flush, nothing evicted) — synchronously, in the caller's
// dispatch goroutine, like flushAheadOf.
func (c *inboundCoalescer) enqueue(channel string, p pendingChannelInbound) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closed {
		c.spillLateLocked(channel, []pendingChannelInbound{p})
		return
	}
	c.pending[channel] = append(c.pending[channel], p)
	pendingLen := len(c.pending[channel])
	if pendingLen >= maxCoalescePerChannel {
		batch, mu := c.takeLocked(channel)
		swept := c.takeSweepsLocked(channel)
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s buffer full (%d) — early flush", channel, pendingLen)
		// Swept mutexes are held from take time (gp-9e7 fix round 2c);
		// launching their goroutines before the triggering channel's
		// own synchronous POST keeps the one-wake fold (round 2a).
		c.deliverSwept(swept)
		c.deliverBatch(channel, batch, mu)
		return
	}
	if _, ok := c.timers[channel]; !ok {
		window := c.windowFor(channel)
		g := c.gen[channel]
		c.timers[channel] = time.AfterFunc(window, func() { c.flushTimer(channel, g) })
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s buffered ts=%s (window %s armed)", channel, p.inbound.ProviderMessageID, window)
		return
	}
	c.mu.Unlock()
	log.Printf("coalesce: chan=%s buffered ts=%s (pending=%d)", channel, p.inbound.ProviderMessageID, pendingLen)
}

// admitReaction buffers one reaction notification (gp-9e7 item 1).
// Reactions must never wake a session solo, so admission NEVER arms a
// timer: the entry sits in the no-wake side-buffer and piggybacks on
// the channel's next real delivery (a timer flush armed by messages,
// the flush-ahead of an urgent/DM inbound, shutdown's flushAll). The
// one exception: ridesArmedWindow=true — a reaction ON one of the
// adapter's own outbound messages (a founder ack) — may join a coalesce
// window that is ALREADY armed, delivering with that batch; with no
// armed window it buffers like any other reaction, whatever the
// target's age, so the exception can never create a wake.
//
// Buffering applies regardless of enabled(): a zero-window deployment
// still routes every real inbound through flushAheadOf, which drains
// the side-buffer. Admission is final handling of the event — the
// caller's dedup claim commits, and a failed batch delivery returns
// the entry here via restore (no retry timer of its own). Returns
// false only on a nil coalescer (bare test configs); the caller then
// falls back to an immediate forward rather than dropping.
func (c *inboundCoalescer) admitReaction(channel string, p pendingChannelInbound, ridesArmedWindow bool) bool {
	if c == nil {
		return false
	}
	p.reaction = true
	c.mu.Lock()
	if c.closed {
		c.spillLateLocked(channel, []pendingChannelInbound{p})
		return true
	}
	if ridesArmedWindow {
		if _, armed := c.timers[channel]; armed {
			c.pending[channel] = append(c.pending[channel], p)
			pendingLen := len(c.pending[channel])
			if pendingLen >= maxCoalescePerChannel {
				batch, mu := c.takeLocked(channel)
				swept := c.takeSweepsLocked(channel)
				c.mu.Unlock()
				log.Printf("coalesce: chan=%s buffer full (%d) — early flush", channel, pendingLen)
				// Swept-before-own ordering: see enqueue (gp-9e7 2a/2c).
				c.deliverSwept(swept)
				c.deliverBatch(channel, batch, mu)
				return true
			}
			c.mu.Unlock()
			log.Printf("coalesce: chan=%s reaction ts=%s riding armed window (pending=%d)", channel, p.inbound.ProviderMessageID, pendingLen)
			return true
		}
	}
	c.reactions[channel] = append(c.reactions[channel], p)
	n := len(c.reactions[channel])
	if n >= maxBufferedReactionsPerChannel {
		// Overflow: deliver rather than evict — reactions never drop.
		// The solo wake this costs takes a pathological reaction volume
		// with zero real traffic; the cap bounds memory, not content.
		batch, mu := c.takeLocked(channel)
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s reaction buffer full (%d) — overflow flush", channel, n)
		c.deliverBatch(channel, batch, mu)
		return true
	}
	c.mu.Unlock()
	log.Printf("coalesce: chan=%s reaction ts=%s buffered no-wake (reactions=%d)", channel, p.inbound.ProviderMessageID, n)
	return true
}

// takeLocked drains the channel's buffer, disarms its timer, and bumps
// the generation so any armed callback for the old state no-ops.
// Buffered no-wake reactions merge into the taken batch (gp-9e7 item
// 1): every take is a real delivery moment for the channel, so the
// side-buffer piggybacks here — deliverCoalescedBatch's ts sort slots
// them into the timeline. Called with c.mu held; returns the batch and
// the channel's delivery mutex (NOT held).
//
// Every take opens an in-flight delivery window (c.inflight): the
// caller MUST route the batch through exactly one deliverBatch call —
// or, for flushAheadOf's inline delivery, call endDelivery itself — so
// flushAll can wait out detached batches (gp-9e7 fix round 1a/2b).
func (c *inboundCoalescer) takeLocked(channel string) ([]pendingChannelInbound, *sync.Mutex) {
	c.gen[channel]++
	c.inflight++
	if t, ok := c.timers[channel]; ok {
		t.Stop()
		delete(c.timers, channel)
	}
	batch := c.pending[channel]
	delete(c.pending, channel)
	if rs := c.reactions[channel]; len(rs) > 0 {
		batch = append(batch, rs...)
		delete(c.reactions, channel)
	}
	return batch, c.flushMuFor(channel)
}

// endDelivery closes one in-flight delivery window opened by a take:
// the batch has either been delivered or restored to the maps, so
// flushAll's fixpoint check can trust "maps empty && inflight zero" =
// nothing left to lose.
func (c *inboundCoalescer) endDelivery() {
	c.mu.Lock()
	c.inflight--
	c.settled.Broadcast()
	c.mu.Unlock()
}

// sweptBatch pairs one swept channel's drained buffer with its
// delivery mutex for the cross-channel flush (gp-9e7 item 2). mu is
// LOCKED — acquired inside takeSweepsLocked's critical section,
// atomically with the take — and the delivery goroutine unlocks it
// after delivering (gp-9e7 fix round 2c).
type sweptBatch struct {
	channel string
	batch   []pendingChannelInbound
	mu      *sync.Mutex
}

// takeSweepsLocked drains every OTHER channel with buffered messages so
// the flush that is about to happen carries ALL channels' window
// traffic in one session wake (gp-9e7 item 2) — each channel still
// delivers as its own per-channel batch, byte-identical formatting, and
// the back-to-back POSTs land in a single wake exactly the way mid-turn
// delivery already batches multi-channel traffic. Excluded:
//   - digest-mode channels: the operator bought their longer latency
//     deliberately (gp-729 item 6); a neighbor's 8s burst window must
//     not undo it. They flush only on their own timer (and shutdown).
//   - channels holding only no-wake reactions: delivering those alone
//     would be a solo reaction wake for THAT channel's bound session,
//     the exact thing item 1 forbids.
//   - channels whose delivery mutex is currently held (a delivery in
//     flight): see below — their own armed timer covers them.
//
// Each swept channel's delivery mutex is acquired HERE, inside the
// caller's c.mu critical section, atomically with the take (gp-9e7 fix
// round 2c): from the instant a batch leaves the maps its mutex is
// held, so an urgent flushAheadOf for that channel — which takes under
// c.mu and then blocks on the same mutex — serializes strictly behind
// the older swept batch by construction; the take-to-lock gap round 2a
// only narrowed is gone. The acquisition is TryLock, never a blocking
// Lock: a blocking Lock under c.mu would deadlock against a failing
// in-flight delivery (which holds the channel mutex and needs c.mu to
// restore) and would stall every enqueue behind a slow POST. A failed
// TryLock means a delivery for that channel is in flight RIGHT NOW; the
// channel is skipped, keeping its buffer and its armed timer (pending
// non-empty ⇒ timer armed — enqueue and restore both guarantee it), so
// it flushes on its own schedule with ordering intact.
//
// Called with c.mu held; the caller hands the returned batches — each
// with its LOCKED mutex — to deliverSwept after releasing it (sorted
// for deterministic order across channels; order WITHIN each channel is
// the buffer order, untouched).
func (c *inboundCoalescer) takeSweepsLocked(except string) []sweptBatch {
	channels := make([]string, 0, len(c.pending))
	for ch, pend := range c.pending {
		if ch == except || len(pend) == 0 {
			continue
		}
		if _, digest := c.policy.digestInterval(ch); digest {
			continue
		}
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	swept := make([]sweptBatch, 0, len(channels))
	for _, ch := range channels {
		m := c.flushMuFor(ch)
		if !m.TryLock() {
			log.Printf("coalesce: chan=%s sweep skipped — delivery in flight; its own timer covers it", ch)
			continue
		}
		b, _ := c.takeLocked(ch)
		swept = append(swept, sweptBatch{channel: ch, batch: b, mu: m})
	}
	return swept
}

// deliverSwept posts each swept channel's batch through per-channel
// delivery goroutines (failure restores that channel's batch alone).
// One goroutine per channel: sequential POSTs would hold later swept
// channels' batches un-delivered for the sum of the earlier POSTs, and
// the near-simultaneous POSTs are what lets gc fold them into one wake.
// Each batch arrives with its delivery mutex ALREADY HELD — acquired at
// take time inside takeSweepsLocked (gp-9e7 fix round 2c) — so an
// urgent flushAheadOf can never slip in between take and delivery.
func (c *inboundCoalescer) deliverSwept(swept []sweptBatch) {
	for _, s := range swept {
		s := s
		go c.deliverTakenLocked(s.channel, s.batch, s.mu)
	}
}

// deliverTakenLocked posts one batch whose delivery mutex was acquired
// at take time by takeSweepsLocked, unlocking it after the delivery
// settles. The lock was taken on the flushTimer (or early-flush
// caller's) goroutine and is released here on the delivery goroutine:
// sync.Mutex is not owner-tracked, so a cross-goroutine unlock is legal
// Go — the handoff of a LOCKED mutex is the point, it is what makes
// "batch out of the maps ⇒ mutex held" an invariant with no gap.
func (c *inboundCoalescer) deliverTakenLocked(channel string, batch []pendingChannelInbound, mu *sync.Mutex) {
	defer c.endDelivery()
	defer mu.Unlock()
	if len(batch) == 0 || c.deliver == nil {
		return
	}
	if !c.deliver(channel, batch) {
		c.restore(channel, batch)
	}
}

// deliverBatch serializes on the channel's delivery mutex and posts the
// batch, restoring it for a timer retry on failure. Safe with an empty
// batch (lock-and-release only — the "wait for in-flight flush" case).
// Closes the in-flight window its take opened, so every taken batch
// must reach exactly one deliverBatch call (see takeLocked).
func (c *inboundCoalescer) deliverBatch(channel string, batch []pendingChannelInbound, mu *sync.Mutex) {
	defer c.endDelivery()
	mu.Lock()
	defer mu.Unlock()
	if len(batch) == 0 || c.deliver == nil {
		return
	}
	if !c.deliver(channel, batch) {
		c.restore(channel, batch)
	}
}

// restore re-queues a batch whose delivery failed, ahead of anything
// buffered meanwhile, and re-arms the timer so the flush retries. The
// re-queued batch may exceed the cap; the next enqueue then triggers an
// early flush rather than anything being dropped. Reaction entries
// split back into the no-wake side-buffer instead (gp-9e7 item 1): a
// reactions-only failure must not arm a retry timer, or the retry
// would be the solo reaction wake the buffer exists to prevent — they
// wait for the channel's next real delivery like any buffered reaction.
func (c *inboundCoalescer) restore(channel string, batch []pendingChannelInbound) {
	if c == nil || len(batch) == 0 {
		return
	}
	var msgs, reactions []pendingChannelInbound
	for _, p := range batch {
		if p.reaction {
			reactions = append(reactions, p)
		} else {
			msgs = append(msgs, p)
		}
	}
	c.mu.Lock()
	if c.closed {
		// Post-barrier restore (defensive — no take can follow the
		// barrier, but a map entry here would sit past the final
		// snapshot forever): spill instead of re-queueing.
		c.spillLateLocked(channel, batch)
		return
	}
	defer c.mu.Unlock()
	if len(reactions) > 0 {
		c.reactions[channel] = append(reactions, c.reactions[channel]...)
	}
	if len(msgs) == 0 {
		return
	}
	c.pending[channel] = append(msgs, c.pending[channel]...)
	if _, ok := c.timers[channel]; !ok {
		window := c.windowFor(channel)
		g := c.gen[channel]
		c.timers[channel] = time.AfterFunc(window, func() { c.flushTimer(channel, g) })
	}
}

// flushTimer is the timer callback. The generation check makes a stale
// callback — one whose arming state was drained by flushAheadOf, an
// early flush, or a reconcile — a strict no-op, so it can never steal
// or reorder a newer batch.
//
// A firing timer is an idle-wake moment, so it sweeps every other
// buffered non-digest channel along with it (gp-9e7 item 2): the
// aligned back-to-back deliveries reach the bound session(s) as ONE
// wake instead of a string of per-channel wakes seconds apart. The
// urgent path's flushAheadOf deliberately does NOT sweep — the session
// is waking for the urgent message anyway, and any other channel's
// window firing during that turn already lands mid-turn without a wake
// of its own.
func (c *inboundCoalescer) flushTimer(channel string, g uint64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.gen[channel] != g {
		c.mu.Unlock()
		return
	}
	batch, mu := c.takeLocked(channel)
	swept := c.takeSweepsLocked(channel)
	c.mu.Unlock()
	// Ordering safety no longer depends on launch order: every swept
	// batch's delivery mutex was acquired at take time inside the
	// critical section above (gp-9e7 fix round 2c), so an urgent
	// flushAheadOf for a swept channel serializes behind the older
	// batch by construction. Launching the swept goroutines before this
	// channel's own synchronous POST remains for the one-wake fold:
	// POSTs seconds apart would defeat the very fold the sweep exists
	// for (round 2a).
	c.deliverSwept(swept)
	c.deliverBatch(channel, batch, mu)
}

// flushAheadOf synchronously delivers any pending batch for channel so
// an urgent (targeted / bot-mentioned) message cannot overtake buffered
// chatter. Buffered no-wake reactions ride in the same take (gp-9e7
// item 1) ONLY when a real message rides with them: a take left with
// nothing but reactions (or emptied entirely by the twin filter below)
// must not POST here — the urgent message's own delivery has not
// happened yet, can be skipped (skipChannelPost twin), and can fail,
// and in every one of those cases the reaction batch would have been
// the only inbound: the solo reaction wake the side-buffer exists to
// prevent (gp-9e7 fix round 1b). Reaction entries from such a take go
// back to the side-buffer instead, and the CALLER delivers them via
// deliverBufferedReactions after its real POST commits — with the real
// delivery, covered by its wake; this is also how DM and zero-window
// deployments drain the reaction side-buffer. It acquires the
// channel's delivery mutex even when nothing delivers, so an in-flight
// coalesced POST completes before the caller proceeds. A failed
// delivery restores the batch — the urgent message proceeds regardless
// (logged), and the retry timer covers the buffered ones.
//
// excludeTS names the urgent message's own ts. A bot-mention pair
// (message + app_mention, same ts, distinct event_ids) can split: when
// the bot user id is unknown, the message twin buffers as plain chatter
// while the app_mention twin takes the urgent path. Delivering the
// buffered twin inside a multi-message batch — whose batch-specific
// dedup key gc cannot correlate with the urgent copy's "slack-<ts>"
// key — hands the session the same message id twice in one turn with
// different decoration (pc_c920ff5fe90c). Matching entries are WITHHELD
// from the batch and returned: the urgent copy is that message's
// delivery, but the twin was already acked to Slack when it entered the
// buffer, so the caller must restore() the returned entries if the
// urgent delivery then fails — dropping them outright would lose the
// message with no redelivery guarantee.
func (c *inboundCoalescer) flushAheadOf(channel, excludeTS string) []pendingChannelInbound {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	batch, mu := c.takeLocked(channel)
	c.mu.Unlock()
	defer c.endDelivery() // inline delivery below; the take is closed on every path
	var withheld []pendingChannelInbound
	if excludeTS != "" {
		kept := batch[:0]
		for _, p := range batch {
			if p.inbound.ProviderMessageID == excludeTS {
				log.Printf("coalesce: chan=%s buffered twin ts=%s withheld (urgent copy delivers it)", channel, excludeTS)
				withheld = append(withheld, p)
				continue
			}
			kept = append(kept, p)
		}
		batch = kept
	}
	real := false
	for _, p := range batch {
		if !p.reaction {
			real = true
			break
		}
	}
	if !real {
		// Reactions-only take (gp-9e7 fix round 1b): never POST — return
		// the entries to the side-buffer (restore arms no timer for
		// reactions) and let the caller piggyback them on its own
		// delivery. Still serialize on the flush mutex so an in-flight
		// coalesced POST completes before the urgent message proceeds.
		if len(batch) > 0 {
			c.restore(channel, batch)
		}
		mu.Lock()
		mu.Unlock() //nolint:staticcheck // empty critical section is the point: wait out an in-flight flush
		return withheld
	}
	mu.Lock()
	defer mu.Unlock()
	if c.deliver == nil {
		return withheld
	}
	if !c.deliver(channel, batch) {
		log.Printf("coalesce: chan=%s flush-ahead failed; batch restored for timer retry (urgent message proceeds out of order)", channel)
		c.restore(channel, batch)
	}
	return withheld
}

// deliverBufferedReactions drains the channel's no-wake reaction
// side-buffer and posts it as its own batch. Callers invoke it ONLY
// immediately after a successful real delivery for the channel (gp-9e7
// fix round 1b): the near-simultaneous POST rides that delivery's wake
// exactly like a swept batch, so the reactions still never wake a
// session solo — and because the drain happens after the real POST
// commits, a skipped or failed real delivery simply leaves the
// side-buffer untouched. A failed reaction POST restores the entries
// to the side-buffer (restore arms no retry timer for reactions).
func (c *inboundCoalescer) deliverBufferedReactions(channel string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	rs := c.reactions[channel]
	if len(rs) == 0 {
		c.mu.Unlock()
		return
	}
	delete(c.reactions, channel)
	c.inflight++ // side-buffer take: closed by deliverBatch below
	mu := c.flushMuFor(channel)
	c.mu.Unlock()
	log.Printf("coalesce: chan=%s delivering %d buffered reaction(s) behind the real delivery", channel, len(rs))
	c.deliverBatch(channel, rs, mu)
}

// pendingContains reports whether ts is currently buffered for channel.
// The thread-context preamble filter treats buffered ids as delivered:
// they are guaranteed to land in the same or an earlier delivery than
// the message whose preamble is being built.
func (c *inboundCoalescer) pendingContains(channel, ts string) bool {
	if c == nil || ts == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.pending[channel] {
		if p.inbound.ProviderMessageID == ts {
			return true
		}
	}
	return false
}

// reconcileTimers re-arms every armed timer against the current policy
// (SIGHUP): a channel flipped digest→immediate must not keep waiting
// out a stale two-hour window. Each affected channel restarts a full
// new window from now.
func (c *inboundCoalescer) reconcileTimers() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for channel, t := range c.timers {
		t.Stop()
		c.gen[channel]++
		window := c.windowFor(channel)
		g := c.gen[channel]
		ch := channel
		c.timers[channel] = time.AfterFunc(window, func() { c.flushTimer(ch, g) })
	}
}

// flushAll synchronously drains every channel's buffer TO A FIXPOINT —
// the normal-shutdown path, so already-acked buffered messages are not
// lost to a SIGTERM landing inside a window. Channels holding only
// no-wake reactions drain too: shutdown is the backstop that keeps
// "every buffered item eventually delivers" true for a channel whose
// real traffic never came (gp-9e7 item 1).
//
// Fixpoint, not a single pass (gp-9e7 fix round 1a/2b): a firing timer
// (and every swept-batch goroutine it spawned) detaches its batch from
// the maps BEFORE the network call, so a one-shot snapshot can find the
// maps empty while a delivery is in flight — and a failed delivery then
// restores the batch after the snapshot, losing it on exit. Each pass
// first waits for every in-flight delivery to settle (c.inflight, whose
// window opens at take time under c.mu), then re-snapshots; the drain
// is done only when the maps are empty with nothing in flight. A
// delivery that keeps failing keeps restoring, so passes are bounded
// (maxFlushAllPasses).
//
// What remains after the bound is NOT accepted as lost (gp-9e7 fix
// round 2a'): the inbound-liveness watermark advances at ADMISSION time
// (noteInboundEnvelope runs before the event is even acked to Slack,
// never at delivery-to-gc time), so the startup watermark backfill can
// NEVER re-fetch an abandoned batch — its members sit at or below the
// persisted watermark, Slack got its 200 long ago, and abandonment
// would be silent permanent loss (the exact opposite of the 8/22
// outage recovery, which worked only because those events were never
// admitted). Undeliverable leftovers are therefore handed to c.spill —
// the durable inbound spool main() wires, replayed through the normal
// buffers at the next startup — with a loud per-channel log either
// way. Only a deployment that explicitly disabled the spool loses
// them, and it is told so per channel.
//
// The drain concludes by flipping c.closed — atomically (under c.mu)
// with the final verdict, on both the clean and the give-up path — so
// nothing can enter the maps after the final snapshot (gp-9e7 fix
// round 2b'): a straggler event goroutine that outlived main's bounded
// eventWG wait finds the barrier down and spills to the spool instead
// of stranding its item in memory. This loop still guards against
// in-flight DELIVERIES via the fixpoint; the barrier is what closes
// the post-final-snapshot admission hole.
func (c *inboundCoalescer) flushAll() {
	if c == nil {
		return
	}
	for pass := 1; ; pass++ {
		c.mu.Lock()
		for c.inflight > 0 {
			c.settled.Wait()
		}
		seen := make(map[string]bool, len(c.pending)+len(c.reactions))
		channels := make([]string, 0, len(c.pending)+len(c.reactions))
		for channel := range c.pending {
			if !seen[channel] {
				seen[channel] = true
				channels = append(channels, channel)
			}
		}
		for channel := range c.reactions {
			if !seen[channel] {
				seen[channel] = true
				channels = append(channels, channel)
			}
		}
		if len(channels) == 0 {
			// Final snapshot is clean: close admission in the same
			// critical section that observed it, so no enqueue can land
			// between the verdict and the barrier.
			c.closed = true
			c.mu.Unlock()
			return
		}
		if pass > maxFlushAllPasses {
			// Give-up path: close admission and take everything still
			// in the maps — inflight is zero (waited above) and closed
			// blocks new entries, so this take is the complete residue.
			c.closed = true
			leftovers := make(map[string][]pendingChannelInbound, len(channels))
			for _, channel := range channels {
				c.gen[channel]++
				if t, ok := c.timers[channel]; ok {
					t.Stop()
					delete(c.timers, channel)
				}
				batch := c.pending[channel]
				delete(c.pending, channel)
				if rs := c.reactions[channel]; len(rs) > 0 {
					batch = append(batch, rs...)
					delete(c.reactions, channel)
				}
				leftovers[channel] = batch
			}
			spill := c.spill
			c.mu.Unlock()
			sort.Strings(channels)
			log.Printf("coalesce: flushAll giving up after %d passes; %d channel(s) still hold undeliverable batches", maxFlushAllPasses, len(channels))
			for _, channel := range channels {
				batch := leftovers[channel]
				if spill == nil {
					log.Printf("coalesce: SHUTDOWN LOSS chan=%s %d undeliverable item(s) with no spool wired — LOST (already acked to Slack; the watermark backfill cannot recover admitted events)", channel, len(batch))
					continue
				}
				log.Printf("coalesce: shutdown chan=%s %d undeliverable item(s) — spooled for startup replay", channel, len(batch))
				spill(channel, batch)
			}
			return
		}
		c.mu.Unlock()
		sort.Strings(channels)
		for _, channel := range channels {
			c.mu.Lock()
			batch, mu := c.takeLocked(channel)
			c.mu.Unlock()
			c.deliverBatch(channel, batch, mu)
		}
	}
}

// coalesceBatchDedupKey derives the gc dedup key for a multi-message
// batch. It must NOT be the newest message's own "slack-<ts>" key: the
// bot-mention twin of that message (message + app_mention, same ts) can
// deliver separately through the urgent path, and sharing its key would
// let gc dedup away the whole batch — losing the earlier messages.
func coalesceBatchDedupKey(batch []pendingChannelInbound) string {
	first := batch[0].inbound.ProviderMessageID
	last := batch[len(batch)-1].inbound.ProviderMessageID
	return fmt.Sprintf("slack-batch-%s-%s-%d", first, last, len(batch))
}

// formatCoalescedBlock renders a multi-message batch as one text block:
// a header naming the channel (name+id when resolvable) and the batch
// shape, then each message verbatim with its sender and ts. Header and
// metadata interpolations are neutralized (cby.17/cby.33 discipline);
// the message bodies themselves are passed through raw, exactly like
// the immediate path — this block is delivered via gc's extmsg
// pipeline, whose reminder formatter sanitizes the whole Text field
// (extmsg.SanitizeForSystemReminder).
func formatCoalescedBlock(cfg config, channel string, batch []pendingChannelInbound) string {
	// The anchor claim names the newest HUMAN message: bot-tagged items
	// (reaction notifications, gp-by3) are excluded from reply-current /
	// react / upload anchoring by the intake helpers, so a batch whose
	// newest entry is one must not claim it as the anchor. All-bot
	// batches fall back to the newest item.
	anchor := batch[len(batch)-1].inbound
	for i := len(batch) - 1; i >= 0; i-- {
		if !batch[i].inbound.Actor.IsBot {
			anchor = batch[i].inbound
			break
		}
	}
	var b strings.Builder
	// --turn-ts resolves via the transcript, which records ONE entry for
	// the whole batch (under the anchor ts — newest HUMAN message, since
	// bot-tagged items are non-anchoring per gp-by3) — so the header
	// steers replies to older members through --reply-to/--no-thread.
	fmt.Fprintf(&b, "[%d messages in %s, coalesced. Reply with --turn-ts %s to answer the newest; to answer an older one use --reply-to <its thread ts> or --no-thread instead (--turn-ts resolves only the newest)]\n",
		len(batch), neutralizeMarkupBoundaries(channelDisplay(cfg, channel)), neutralizeMarkupBoundaries(anchor.ProviderMessageID))
	for _, p := range batch {
		m := p.inbound
		sender := m.Actor.DisplayName
		if sender == "" {
			sender = m.Actor.ID
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "[%s] %s", neutralizeMarkupBoundaries(m.ProviderMessageID), neutralizeMarkupBoundaries(sender))
		if m.ReplyToMessageID != "" && m.ReplyToMessageID != m.ProviderMessageID {
			fmt.Fprintf(&b, " (in thread %s)", neutralizeMarkupBoundaries(m.ReplyToMessageID))
		}
		b.WriteString(": ")
		b.WriteString(m.Text)
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// deliverCoalescedBatch posts one buffered batch as a single inbound.
// The batch is sorted by Slack ts first — enqueue order is handler-
// completion order, and a heavyweight earlier message (file download,
// thread fetch) can finish after a lighter later one. Pending peer-bot
// context rides ahead of the block (same contract as the urgent path)
// and the once-per-channel reply how-to is appended when this is the
// channel's first delivery since adapter start. A single-message batch
// delivers its envelope byte-identical to the immediate path so the
// common case has zero format churn.
func deliverCoalescedBatch(cfg config, channel string, batch []pendingChannelInbound) bool {
	sort.SliceStable(batch, func(i, j int) bool {
		return batch[i].inbound.ProviderMessageID < batch[j].inbound.ProviderMessageID
	})
	// Collapse same-ts entries (first kept): distinct event_ids for one
	// message can each admit to the buffer past the event-dedup cache
	// (eviction, restart), and quoting the id twice inside one block is
	// the in-batch variant of pc_c920ff5fe90c. Filters below build
	// FRESH slices: a failed delivery hands the caller's original batch
	// slice to restore(), and compacting in place (batch[:0]) would
	// corrupt that shared backing array — duplicating tail entries and
	// losing dropped-position ones on the retry (gp-ios).
	kept := make([]pendingChannelInbound, 0, len(batch))
	for _, p := range batch {
		if len(kept) > 0 && kept[len(kept)-1].inbound.ProviderMessageID == p.inbound.ProviderMessageID {
			continue
		}
		kept = append(kept, p)
	}
	batch = kept
	// Delivery-time twin filter (pc_c920ff5fe90c): drop entries whose ts
	// the channel audience has already received — the urgent twin
	// delivered while this copy sat buffered (it can slip past the
	// enqueue-time check when the urgent POST is still in flight). Each
	// surviving member then claims its (channel, ts) delivery (gp-ios):
	// a claim already committed means an urgent twin finished while this
	// batch was assembling — drop the member; a claim still in flight is
	// kept WITHOUT parking (this goroutine holds the channel flush
	// mutex, and if the in-flight owner then failed, a dropped buffered
	// copy would have no redelivery — Slack already got its 200). Fail
	// open to a duplicate in that narrow window, never to loss.
	unseen := make([]pendingChannelInbound, 0, len(batch))
	var ownedClaims []string
	for _, p := range batch {
		ts := p.inbound.ProviderMessageID
		if cfg.deliveredIDs.seen("", channel, ts) {
			log.Printf("coalesce: chan=%s ts=%s already delivered to channel audience — dropped from batch", channel, ts)
			continue
		}
		key := channelDeliveryClaimKey(channel, ts)
		proceed, wait := cfg.channelClaims.begin(key)
		if proceed {
			ownedClaims = append(ownedClaims, key)
		} else if wait == nil {
			log.Printf("coalesce: chan=%s ts=%s delivered by same-ts twin — dropped from batch", channel, ts)
			continue
		}
		unseen = append(unseen, p)
	}
	batch = unseen
	if len(batch) == 0 {
		return true
	}
	env := batch[len(batch)-1].inbound
	text := env.Text
	if len(batch) > 1 {
		text = formatCoalescedBlock(cfg, channel, batch)
		var attachments []externalAttachment
		for _, p := range batch {
			attachments = append(attachments, p.inbound.Attachments...)
		}
		env.Attachments = attachments
		env.DedupKey = coalesceBatchDedupKey(batch)
	}
	peerItems, peerDropped := cfg.peerContext.flush(channel)
	if peerBlock := formatPeerContextBlock(peerItems, peerDropped); peerBlock != "" {
		text = peerBlock + "\n\n" + text
	}
	firstHelp := cfg.replyHelp.first(channel)
	if firstHelp {
		text += "\n\n" + replyHelpBlock(cfg, channel)
	}
	env.Text = text
	if err := postInbound(cfg, env); err != nil {
		log.Printf("coalesced inbound POST failed: chan=%s batch=%d: %v", channel, len(batch), err)
		// Release the member claims this batch owned so a parked
		// same-ts urgent twin — or this batch's own timer retry — can
		// take over the delivery (gp-ios).
		for _, key := range ownedClaims {
			cfg.channelClaims.forget(key)
		}
		cfg.peerContext.restore(channel, peerItems, peerDropped)
		if firstHelp {
			cfg.replyHelp.unmark(channel)
		}
		return false
	}
	for _, key := range ownedClaims {
		cfg.channelClaims.commit(key)
	}
	tss := make([]string, 0, len(batch))
	for _, p := range batch {
		tss = append(tss, p.inbound.ProviderMessageID)
	}
	cfg.deliveredIDs.record("", channel, tss...)
	log.Printf("inbound (coalesced): chan=%s batch=%d newest_ts=%s text=%dch",
		channel, len(batch), env.ProviderMessageID, len(env.Text))
	return true
}

// --- once-per-channel reply how-to (gp-729 item 3) --------------------------
//
// The adapter registers a one-line reply-instruction template with gc
// (see registerAdapter), replacing the generic three-line fallback on
// EVERY inbound reminder. The full how-to — write-to-file mechanics,
// threading semantics, the react verb, and the channel's name+id
// pairing (item 4) — is delivered once per channel per adapter
// lifetime, appended to that channel's first forwarded inbound.

// oncePerChannel remembers which channels received their full reply
// how-to this adapter lifetime. Nil-safe: nil never reports first
// (bare test configs get no help block).
type oncePerChannel struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newOncePerChannel() *oncePerChannel {
	return &oncePerChannel{seen: make(map[string]bool)}
}

// first reports true exactly once per channel; unmark rewinds a claim
// whose delivery failed so the next delivery retries the help block.
func (o *oncePerChannel) first(channel string) bool {
	if o == nil || channel == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.seen[channel] {
		return false
	}
	o.seen[channel] = true
	return true
}

func (o *oncePerChannel) unmark(channel string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.seen, channel)
}

// replyHelpBlock is the full per-channel reply how-to, delivered once
// per channel per adapter lifetime. Keep the command shapes in sync
// with scripts/slack_chat_reply_current.py and slack_chat_react.py —
// tests/test_reply_template_contract.py pins the flags.
func replyHelpBlock(cfg config, channel string) string {
	display := neutralizeMarkupBoundaries(channelDisplay(cfg, channel))
	return fmt.Sprintf(
		"[channel %s — full reply how-to, sent once per channel per adapter session]\n"+
			"To reply: write your reply to a file, then run:\n"+
			"  gc slack reply-current --conversation-id %s --turn-ts <ts of the message you are answering> --body-file <file>\n"+
			"--turn-ts (every delivery names its ts) anchors the reply to that exact inbound — its thread when threaded, top-level otherwise. Without it the reply threads under the LATEST inbound, which interleaved traffic can make the wrong one. --reply-to <thread ts> pins a thread explicitly; --no-thread forces a top-level post.\n"+
			"To react: gc slack react --emoji <name>",
		display, neutralizeMarkupBoundaries(channel))
}

// slackReplyInstructionsTemplate is the one-line per-message reply
// instruction registered with gc (extmsg ReplyInstructionsProvider).
// gc substitutes {conversation_id} and {message_ts} per reminder; the
// full how-to arrives once per channel via replyHelpBlock. Keep the
// command shape in sync with scripts/slack_chat_reply_current.py —
// tests/test_reply_template_contract.py pins the flags.
//
// --turn-ts {message_ts} pins the reply to the exact inbound the
// reminder delivered (gp-6j3): without it, reply-current anchors on the
// LATEST inbound at send time, and coalesced delivery + interleaved
// channel/thread traffic routinely make that a different message —
// replies landed top-level instead of in-thread, and in the wrong
// thread outright, three times in one hour across two cities.
const slackReplyInstructionsTemplate = "Reply: gc slack reply-current --conversation-id {conversation_id} --turn-ts {message_ts} --body-file <file>"
