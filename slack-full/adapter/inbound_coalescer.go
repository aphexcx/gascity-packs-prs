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

// pendingChannelInbound is one buffered channel inbound awaiting
// coalesced delivery. The envelope's Text is final per-message text
// (thread preamble, mention rewrite, and files block already applied).
type pendingChannelInbound struct {
	inbound externalInboundMessage
}

// inboundCoalescer owns the per-channel buffers and flush timers.
// Nil-safe throughout: a nil coalescer disables coalescing and every
// inbound takes the immediate path, byte-for-byte pre-gp-729 behavior.
type inboundCoalescer struct {
	mu      sync.Mutex
	pending map[string][]pendingChannelInbound
	gen     map[string]uint64
	timers  map[string]*time.Timer
	flushMu map[string]*sync.Mutex
	window  time.Duration
	policy  *deliveryPolicyRegistry
	// deliver posts one batch as a single inbound; false = failed (the
	// caller restores the batch). Wired in main() to deliverCoalescedBatch
	// with the final cfg; tests inject their own.
	deliver func(channel string, batch []pendingChannelInbound) bool
}

func newInboundCoalescer(window time.Duration, policy *deliveryPolicyRegistry) *inboundCoalescer {
	return &inboundCoalescer{
		pending: make(map[string][]pendingChannelInbound),
		gen:     make(map[string]uint64),
		timers:  make(map[string]*time.Timer),
		flushMu: make(map[string]*sync.Mutex),
		window:  window,
		policy:  policy,
	}
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

// enqueue buffers one inbound and arms the channel's flush timer if it
// isn't already running. A buffer reaching the cap flushes immediately
// (early flush, nothing evicted) — synchronously, in the caller's
// dispatch goroutine, like flushAheadOf.
func (c *inboundCoalescer) enqueue(channel string, p pendingChannelInbound) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.pending[channel] = append(c.pending[channel], p)
	pendingLen := len(c.pending[channel])
	if pendingLen >= maxCoalescePerChannel {
		batch, mu := c.takeLocked(channel)
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s buffer full (%d) — early flush", channel, pendingLen)
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

// takeLocked drains the channel's buffer, disarms its timer, and bumps
// the generation so any armed callback for the old state no-ops.
// Called with c.mu held; returns the batch and the channel's delivery
// mutex (NOT held).
func (c *inboundCoalescer) takeLocked(channel string) ([]pendingChannelInbound, *sync.Mutex) {
	c.gen[channel]++
	if t, ok := c.timers[channel]; ok {
		t.Stop()
		delete(c.timers, channel)
	}
	batch := c.pending[channel]
	delete(c.pending, channel)
	return batch, c.flushMuFor(channel)
}

// deliverBatch serializes on the channel's delivery mutex and posts the
// batch, restoring it for a timer retry on failure. Safe with an empty
// batch (lock-and-release only — the "wait for in-flight flush" case).
func (c *inboundCoalescer) deliverBatch(channel string, batch []pendingChannelInbound, mu *sync.Mutex) {
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
// early flush rather than anything being dropped.
func (c *inboundCoalescer) restore(channel string, batch []pendingChannelInbound) {
	if c == nil || len(batch) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pending[channel] = append(append([]pendingChannelInbound{}, batch...), c.pending[channel]...)
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
	c.mu.Unlock()
	c.deliverBatch(channel, batch, mu)
}

// flushAheadOf synchronously delivers any pending batch for channel so
// an urgent (targeted / bot-mentioned) message cannot overtake buffered
// chatter. It acquires the channel's delivery mutex even when the
// buffer is empty, so an in-flight coalesced POST completes before the
// caller proceeds. A failed delivery restores the batch — the urgent
// message proceeds regardless (logged), and the retry timer covers the
// buffered ones.
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
	mu.Lock()
	defer mu.Unlock()
	if len(batch) == 0 || c.deliver == nil {
		return withheld
	}
	if !c.deliver(channel, batch) {
		log.Printf("coalesce: chan=%s flush-ahead failed; batch restored for timer retry (urgent message proceeds out of order)", channel)
		c.restore(channel, batch)
	}
	return withheld
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

// flushAll synchronously drains every channel's buffer — the normal-
// shutdown path, so already-acked buffered messages are not lost to a
// SIGTERM landing inside a window.
func (c *inboundCoalescer) flushAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	channels := make([]string, 0, len(c.pending))
	for channel := range c.pending {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	c.mu.Unlock()
	for _, channel := range channels {
		c.mu.Lock()
		batch, mu := c.takeLocked(channel)
		c.mu.Unlock()
		c.deliverBatch(channel, batch, mu)
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
	last := batch[len(batch)-1].inbound
	var b strings.Builder
	// --turn-ts resolves via the transcript, which records ONE entry for
	// the whole batch (under the newest ts) — so the header steers
	// replies to older members through --reply-to/--no-thread instead.
	fmt.Fprintf(&b, "[%d messages in %s, coalesced. Reply with --turn-ts %s to answer the newest; to answer an older one use --reply-to <its thread ts> or --no-thread instead (--turn-ts resolves only the newest)]\n",
		len(batch), neutralizeMarkupBoundaries(channelDisplay(cfg, channel)), neutralizeMarkupBoundaries(last.ProviderMessageID))
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
	// the in-batch variant of pc_c920ff5fe90c.
	kept := batch[:0]
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
	// enqueue-time check when the urgent POST is still in flight). The
	// check runs here, at delivery time under the flush mutex, precisely
	// because an urgent delivery that FAILED never records its ts: the
	// buffered copy then still delivers. Fail open to a duplicate in the
	// residual in-flight window, never to loss.
	unseen := batch[:0]
	for _, p := range batch {
		if cfg.deliveredIDs.seen("", channel, p.inbound.ProviderMessageID) {
			log.Printf("coalesce: chan=%s ts=%s already delivered to channel audience — dropped from batch", channel, p.inbound.ProviderMessageID)
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
		cfg.peerContext.restore(channel, peerItems, peerDropped)
		if firstHelp {
			cfg.replyHelp.unmark(channel)
		}
		return false
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
