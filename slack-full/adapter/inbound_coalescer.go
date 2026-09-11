package main

import (
	"errors"
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
// per-channel flush mutex serializes deliveries, and EVERY take
// acquires it AT TAKE TIME inside the c.mu critical section (TryLock,
// never a blocking Lock under c.mu — gp-9e7 round 3, generalizing the
// round-2c sweep discipline): a detached batch holds its channel's
// mutex until delivered or restored, so any later take/Lock serializes
// behind ALL older detached batches by construction and a newer POST
// can never overtake an older one through a take-to-lock gap. A failed
// TryLock means a delivery is in flight; the taker skips or defers on a
// short retry (the armed timer / restore cycle covers the buffered
// items), and flushAheadOf — whose urgent caller must serialize —
// BLOCKS into the mutex's wait queue and detaches with it held: holding
// the mutex is its reservation (round 5, 3c), so no competing TryLock
// taker can steal the batch while it waits.
//
// In-memory like peerContextBuffer: messages admitted to the buffer
// were already acked to Slack, so an adapter CRASH inside the window
// loses that window's chatter — the acceptance the peer buffer
// documents, bounded here by the window. A normal shutdown drains
// every buffer first (flushAll). A flush that fails TRANSIENTLY
// (network, 5xx, 429) restores the batch and retries on the next timer,
// forever — on a per-channel doubling backoff capped at
// maxTransientRetryDelay, never the fixed window forever (gp-sgu7). A
// flush gc REJECTS as payload (400/413/415/422 — the same payload can
// never be accepted) goes through failed(): a multi-message batch is
// isolated into singles so only the poisoned message is charged, and a
// charged message follows the rejection ladder (nextRejectionStep,
// inbound_dead_letter.go): one retry WITHOUT its attachments when it
// had any, then the dead-letter hook (JSONL file) — never a retry of
// the same bytes, so a refused voice memo costs the channel two
// windows, not forever (gp-xnc 2026-08-26; gp-sgu7 2026-09-08 incident
// where the pre-gp-xnc binary posted one refused batch 34,000 times).

// defaultCoalesceWindow is the debounce for burst coalescing; inside
// the 5-15s band the Aug-17 plan named.
const defaultCoalesceWindow = 8 * time.Second

// maxCoalescePerChannel is the early-flush threshold: a buffer this
// full delivers immediately rather than waiting out its window.
const maxCoalescePerChannel = 50

// maxCoalesceDeliveryAttempts bounds the UNVOUCHED ladder (gp-32q): how
// many times a message gc accepted but would not vouch for may be
// re-posted before it is dead-lettered. A payload REJECTION no longer
// counts toward it — that class is never re-posted as-is at all (see
// nextRejectionStep); transient failures never count toward anything.
const maxCoalesceDeliveryAttempts = 3

// maxTransientRetryDelay caps the per-channel backoff for TRANSIENT
// delivery failures (network error, 5xx, 429): the retry cadence starts
// at the channel's window and doubles per consecutive failure up to
// this, so a gc outage costs one POST per ~5 min per channel instead of
// one per 8 s (gp-sgu7). Reset by the channel's next successful delivery.
const maxTransientRetryDelay = 5 * time.Minute

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

// overCapRetryCeiling caps the retry cadence for a flush deferred
// behind an in-flight delivery (gp-9e7 round 5, 3a/3b). The retry must
// run on COALESCE-WINDOW scale — an over-cap buffer (or an elapsed
// window) needs to flush promptly once the in-flight POST settles —
// never on the channel's digest interval, which can be hours.
const overCapRetryCeiling = time.Second

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
	// attempts counts deliveries gc REJECTED (permanentDeliveryFailure)
	// with this entry charged as the suspect (see failed); at
	// maxCoalesceDeliveryAttempts it is dead-lettered instead of
	// restored (gp-xnc). Not spooled: a replayed entry starts over.
	attempts int
	// isolate marks a member of a batch gc REFUSED (gp-sgu7, codex r1
	// finding 3): it must only ever be POSTed alone — an isolation
	// probe — never inside a batch again, or a transient failure that
	// pauses the probe would restore the members and the next window
	// would re-post the identical refused batch. Set by the probe
	// when it pauses and by charge() on a stripped retry; honored by
	// post(); spooled, so a restart resumes the probes too.
	isolate bool
	// refused is the rejection that RETIRED the entry (the ladder's
	// dead-letter verdict) while its dead-letter write is still owed:
	// set by parkDeadLetter, spooled with the entry so a restart parks
	// it straight back into the write retry instead of replaying it
	// as an inbound and re-posting refused bytes (gp-sgu7, codex r2
	// finding 1). Empty on every entry still in the delivery path.
	refused string
	// stripped is the rejection the ladder answered by withholding this
	// entry's attachments (the stripped retry, owed or in flight): set
	// by withholdAttachments, spooled with the entry so a restart seeds
	// the ledger's verdict before the copy is admitted (codex r12
	// finding 1 — the isolate flag survived the restart, the verdict
	// did not, and the urgent twin posted the original attachments once
	// the replayed stripped copy landed). Empty on a plain entry.
	stripped string
	// threadAnchor/preamble/body/files carry the message unit's parts
	// alongside the folded inbound.Text so a single-entry delivery can
	// re-compose under the head-protection contract exactly like the
	// immediate path (gp-0qw; codex round-1 findings 1+4). Populated
	// at enqueue; empty on legacy/spool-replayed entries, which then
	// deliver inbound.Text as the body. Multi-message batches quote
	// inbound.Text verbatim per member and ignore these.
	threadAnchor string
	preamble     string
	// preambleLean is the preamble without peer-bot quotes, the
	// composer's shed-before-omit fallback (jg-ure5r8; see
	// channelReminderParts.preambleLean).
	preambleLean string
	botQuotes    bool
	body         string
	files        string
}

// hasReminderParts reports whether the entry carries a foldable message
// unit (preamble/body/files). The spool stores such entries WITHOUT
// the redundant folded Text and rebuilds it via foldedText on replay
// (codex round-3 finding 4: persisting both doubled spool lines and
// could push a previously replayable spool past the read cap).
func (p pendingChannelInbound) hasReminderParts() bool {
	return p.preamble != "" || p.body != "" || p.files != ""
}

// foldedText rebuilds the legacy folded Text (preamble + body, files
// block attached) from the parts — byte-identical to what the enqueue
// and spool producers folded, by construction of assemble.
func (p pendingChannelInbound) foldedText() string {
	return channelReminderParts{preamble: p.preamble, body: p.body, files: p.files}.assemble(p.body)
}

// reminderParts returns the entry's channelReminderParts for the
// head-protected composer, reporting false when the entry predates the
// parts fields (legacy spool replays) and the caller must fall back to
// the folded Text.
func (p pendingChannelInbound) reminderParts(channel string) (channelReminderParts, bool) {
	if p.threadAnchor == "" && p.preamble == "" && p.body == "" && p.files == "" {
		return channelReminderParts{}, false
	}
	return channelReminderParts{
		anchor:       p.threadAnchor,
		preamble:     p.preamble,
		preambleLean: p.preambleLean,
		botQuotes:    p.botQuotes,
		body:         p.body,
		files:        p.files,
		ts:           p.inbound.ProviderMessageID,
		channelID:    channel,
	}, true
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
	// due is the moment the channel's armed timer aims at — the
	// scheduler's own bookkeeping (scheduleLocked, the ONE place a
	// timer is armed, moved or disarmed). Present exactly when a timer
	// is armed. Guarded by mu.
	due     map[string]time.Time
	flushMu map[string]*sync.Mutex
	// urgentWaiting counts flushAheadOf callers blocked waiting for the
	// channel's delivery mutex (gp-9e7 round 5, 3c). Guarded by mu. A
	// non-zero count is a RESERVATION: takeLocked (and the direct
	// TryLock in deliverBufferedReactions) refuses the take outright, so
	// no timer/early-flush/sweep taker can slip in between the in-flight
	// delivery's unlock and the urgent waiter's wakeup and steal the
	// batch — the starvation window the old wait-unlock-retake loop had.
	// Deferrable takers already treat a failed take as "delivery in
	// flight" and retry on the short cadence, which covers this case
	// identically.
	urgentWaiting map[string]int
	// deleted holds per-channel deletion tombstones (ts → when) — see
	// tombstoneLocked. Guarded by mu.
	deleted map[string]map[string]time.Time
	// persistDeletion records a deletion durably (wired in main() to
	// inboundSpool.recordDeletion) so it survives a restart and applies
	// to spooled entries on replay whatever the write ordering was
	// (codex round-4 finding 1). Nil-safe; called without mu held.
	persistDeletion func(channel, ts string)
	window          time.Duration
	policy          *deliveryPolicyRegistry
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
	// batches are LOST. It returns true ONLY on confirmed durability
	// (write + fsync verified) — a batch is reported "spooled" solely on
	// that verdict, and every other outcome gets the loud per-channel
	// LOSS log, same contract as spool-disabled (gp-9e7 round 3, 1a).
	// Called without mu held (it does file I/O).
	spill func(channel string, batch []pendingChannelInbound) bool
	// deliver posts one batch as a single inbound; a non-nil error is
	// routed through failed (restore, isolate, or dead-letter by error
	// class). Wired in main() to deliverCoalescedBatch with the final
	// cfg; tests inject their own.
	deliver func(channel string, batch []pendingChannelInbound) error
	// deadLetter receives entries the rejection ladder retires
	// (nextRejectionStep, gp-xnc/gp-sgu7) and returns true ONLY on a
	// confirmed-durable write (the spill contract): on false the entry
	// stays buffered and the write is retried next window. Nil-safe:
	// without a hook the entry is dropped with a LOSS log (the storm
	// still stops). Wired in main() to writeInboundDeadLetter. Called
	// with the channel's delivery mutex held and c.mu NOT held (file I/O).
	deadLetter func(channel string, batch []pendingChannelInbound, cause error) bool
	// transientFailures counts consecutive TRANSIENT delivery failures per
	// channel (gp-sgu7); retryNotBefore is the deadline before which the
	// channel POSTs nothing on a timer. Both guarded by mu; WRITTEN only
	// by noteTransientFailure (a new failure) and cleared only by
	// deliveredOK (a success) — never by a restore, a reconcile or an
	// urgent path (codex r8 finding 1). Read by inBackoffLocked,
	// scheduleLocked and takeSweepsLocked.
	transientFailures map[string]int
	retryNotBefore    map[string]time.Time
	// urgentDeferredAt remembers, per channel, the failure count at
	// which the last "urgent flush-ahead deferred" line was logged, so
	// a mention stream during an outage logs once per backoff step.
	urgentDeferredAt map[string]time.Duration
	// parkedDeadLetters holds entries the rejection ladder retired whose
	// dead-letter write the hook did NOT confirm (gp-sgu7, codex r1
	// finding 1): they sit OUT of the delivery path — never re-posted to
	// gc — while the WRITE retries on its own backoff (deadLetterTimers,
	// deadLetterWriteFailures, all guarded by mu). deadLetterMu
	// serializes the retry callback against the shutdown flush so one
	// entry is never written twice. See parkDeadLetter.
	parkedDeadLetters       map[string][]parkedDeadLetter
	deadLetterTimers        map[string]*time.Timer
	deadLetterWriteFailures map[string]int
	deadLetterMu            sync.Mutex
	// verdicts is the rejection ladder's memory per (channel, ts): what
	// charge() has decided about a MESSAGE so far, so every other copy
	// of it — buffered beside it, admitted later, or arriving as the
	// urgent twin — inherits the decision instead of re-posting the
	// bytes gc refused (codex r10). Written by charge() (the decision),
	// landVerdicts (the stripped retry gc accepted) and parkDeadLetter
	// (a retirement decided before a restart, re-parked by the spool
	// replay — codex r11 finding 4) and the replay's seeding; a
	// terminal verdict is pruned after ladderVerdictRetention, an
	// outstanding one never by time. Guarded by mu.
	verdicts map[string]map[string]ladderVerdict
	// recordVerdict is the durable-record hook for a TERMINAL verdict
	// (the inbound spool's verdict line, beside its deletion records):
	// the ledger is memory, and a delayed Slack redelivery of a refused
	// message after a restart must still be a duplicate, not fresh
	// bytes (codex r12 finding 2). The replay seeds every unexpired
	// record and writes it again for the next restart. Nil in bare
	// test configs (a verdict then lives as long as the process).
	recordVerdict func(channel, ts string, v ladderVerdict) bool
}

// ladderVerdict is the ladder's standing decision about one (channel,
// ts): retired means the message is out of the delivery path for good
// — dead-lettered (or parked for the write), or DELIVERED without its
// attachments (delivered) — and every further copy is dropped;
// otherwise one retry without attachments is owed or in flight, and a
// copy admitted meanwhile adopts that stripped state. A stripped
// delivery retires the message rather than clearing it (codex r11
// finding 1): every other copy still carries the bytes gc refused. The
// same-ts copies of one Slack message (the bot-mention twin pair, a
// redelivery, an urgent copy built from the fresh event) are the same
// message: the ladder's verdict is about the message, not about
// whichever copy happened to be posted.
type ladderVerdict struct {
	retired   bool
	delivered bool
	cause     error
	at        time.Time
}

// disposition names the verdict for a log line.
func (v ladderVerdict) disposition() string {
	switch {
	case v.retired && v.delivered:
		return "delivered this message through the rejection ladder"
	case v.retired:
		return "dead-lettered this message"
	default:
		return "owes this message its retry without attachments"
	}
}

// ladderVerdictRetention bounds the memory of a TERMINAL verdict: Slack
// redelivers an event for up to 24 hours (retries, and delayed events
// after an outage), so a dead-lettered or stripped-delivered message
// stays a duplicate for that whole window plus an hour of slack (codex
// r12 finding 2: at one hour, a late redelivery of a dead-lettered
// message was admitted as fresh bytes and refused — and dead-lettered —
// again). An OUTSTANDING verdict (the stripped retry owed or in
// flight) never expires by time: that retry is the message's delivery,
// however long the outage, and it lands as a terminal verdict. Refused
// messages are rare; the map stays tiny.
const ladderVerdictRetention = 25 * time.Hour

// expired reports whether a verdict has aged out of the ledger: only a
// terminal one ever does.
func (v ladderVerdict) expired(now time.Time) bool {
	return v.retired && now.Sub(v.at) > ladderVerdictRetention
}

func newInboundCoalescer(window time.Duration, policy *deliveryPolicyRegistry) *inboundCoalescer {
	c := &inboundCoalescer{
		pending:           make(map[string][]pendingChannelInbound),
		reactions:         make(map[string][]pendingChannelInbound),
		gen:               make(map[string]uint64),
		timers:            make(map[string]*time.Timer),
		due:               make(map[string]time.Time),
		flushMu:           make(map[string]*sync.Mutex),
		urgentWaiting:     make(map[string]int),
		deleted:           make(map[string]map[string]time.Time),
		transientFailures: make(map[string]int),
		retryNotBefore:    make(map[string]time.Time),
		urgentDeferredAt:  make(map[string]time.Duration),
		window:            window,
		policy:            policy,

		parkedDeadLetters:       make(map[string][]parkedDeadLetter),
		deadLetterTimers:        make(map[string]*time.Timer),
		deadLetterWriteFailures: make(map[string]int),
		verdicts:                make(map[string]map[string]ladderVerdict),
	}
	c.settled = sync.NewCond(&c.mu)
	return c
}

// --- deletion tombstones (gp-0qw item 3) --------------------------------------
//
// A message_deleted event can beat its own message's enqueue (event
// goroutines run concurrently), or land while the message sits in a
// detached in-flight batch whose POST then fails and restores it. A
// rewrite of the pending buffer alone misses both (codex round-1
// finding 3), so the deletion is ALSO remembered as a per-(channel,
// ts) tombstone that enqueue and restore consult. Tombstones expire
// after deletionTombstoneTTL — the buffered window is seconds to
// minutes, and deletions arrive at human rate, so the map stays tiny.

const deletionTombstoneTTL = 15 * time.Minute

// tombstoneLocked records (channel, ts) as deleted and prunes expired
// tombstones for the channel. Caller holds c.mu.
func (c *inboundCoalescer) tombstoneLocked(channel, ts string, now time.Time) {
	if c.deleted == nil {
		c.deleted = make(map[string]map[string]time.Time)
	}
	// Sweep expired tombstones across ALL channels: pruning only the
	// deleting channel leaves channels with no later deletion holding
	// their entries forever (codex round-2 finding 6). Deletions arrive
	// at human rate, so the full sweep stays trivial.
	for ch, m := range c.deleted {
		for k, at := range m {
			if now.Sub(at) > deletionTombstoneTTL {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			delete(c.deleted, ch)
		}
	}
	m := c.deleted[channel]
	if m == nil {
		m = make(map[string]time.Time)
		c.deleted[channel] = m
	}
	m[ts] = now
}

// recordVerdictLocked remembers the ladder's decision about (channel,
// ts) and prunes expired verdicts across all channels (the tombstone
// discipline). A verdict only PROGRESSES (codex r13 finding 1): a
// terminal verdict is never downgraded to an outstanding one — a stale
// stripped entry staged beside its own dead-letter record cannot
// reverse it on replay — and recording the same verdict again changes
// nothing, so the caller writes no second durable record. The decision
// time is kept: a record re-seeded after a restart carries its original
// `at`, and retention counts from the decision, not the replay. Returns
// whether the ledger changed. Caller holds c.mu.
func (c *inboundCoalescer) recordVerdictLocked(channel, ts string, v ladderVerdict, now time.Time) bool {
	if c.verdicts == nil {
		c.verdicts = make(map[string]map[string]ladderVerdict)
	}
	for ch, m := range c.verdicts {
		for k, v := range m {
			if v.expired(now) {
				delete(m, k)
			}
		}
		if len(m) == 0 {
			delete(c.verdicts, ch)
		}
	}
	m := c.verdicts[channel]
	if m == nil {
		m = make(map[string]ladderVerdict)
		c.verdicts[channel] = m
	}
	if cur, ok := m[ts]; ok {
		if cur.retired && !v.retired {
			return false
		}
		if cur.retired == v.retired && cur.delivered == v.delivered {
			return false
		}
	}
	if v.at.IsZero() {
		v.at = now
	}
	m[ts] = v
	return true
}

// retireLocked records a TERMINAL verdict for (channel, ts) and removes
// every buffered copy of the message. Returns how many copies left and
// whether the ledger changed — the caller, once unlocked, calls
// persistVerdict for the durable record only on a change, so a retire
// followed by its park writes ONE record (codex r13 finding 3). Caller
// holds c.mu.
func (c *inboundCoalescer) retireLocked(channel, ts string, v ladderVerdict, now time.Time) (dropped int, changed bool) {
	v.retired = true
	changed = c.recordVerdictLocked(channel, ts, v, now)
	return c.dropBufferedCopiesLocked(channel, ts), changed
}

// persistVerdict writes a terminal verdict's durable record through the
// recordVerdict hook. Called with c.mu NOT held: the record is an
// fsync'd spool line. Returns whether the record is durable (true with
// no hook wired: nothing was promised). A failed write is logged once
// here; the message is still retired in memory for this process's
// lifetime.
func (c *inboundCoalescer) persistVerdict(channel, ts string, v ladderVerdict) bool {
	if c.recordVerdict == nil || !v.retired {
		return true
	}
	if !c.recordVerdict(channel, ts, v) {
		log.Printf("coalesce: chan=%s ts=%s verdict record write FAILED (%s) — after a restart a delayed redelivery of this message could be admitted once as fresh bytes", channel, ts, v.disposition())
		return false
	}
	return true
}

// seedVerdict is the spool replay's entry point: a verdict decided
// before the restart — terminal (a record line) or outstanding (the
// stripped entry's own disposition) — re-enters the ledger before any
// copy of the message is admitted, and a terminal one is written again
// for the next restart. A verdict already standing (a duplicate record,
// a terminal one ahead of a stale stripped entry) changes nothing and
// is not re-written. Returns whether the durable record, if one was
// owed, is confirmed — the replay keeps its staged file otherwise.
func (c *inboundCoalescer) seedVerdict(channel, ts string, v ladderVerdict) bool {
	if c == nil || ts == "" {
		return true
	}
	c.mu.Lock()
	var changed bool
	if v.retired {
		_, changed = c.retireLocked(channel, ts, v, time.Now())
	} else {
		changed = c.recordVerdictLocked(channel, ts, v, time.Now())
	}
	c.mu.Unlock()
	if !changed {
		return true
	}
	return c.persistVerdict(channel, ts, v)
}

// verdictLocked returns the live ladder verdict for (channel, ts), if
// any. Caller holds c.mu.
func (c *inboundCoalescer) verdictLocked(channel, ts string, now time.Time) (ladderVerdict, bool) {
	v, ok := c.verdicts[channel][ts]
	if !ok || v.expired(now) {
		return ladderVerdict{}, false
	}
	return v, true
}

// landVerdicts settles the ladder's verdict for every real member of a
// delivery gc just ACCEPTED. A member the ladder never owned — no
// verdict, never charged, never flagged — was never refused: nothing to
// record, and a later copy is an ordinary duplicate for the dedup key.
// A member the ladder OWNS lands as a terminal verdict (retired,
// delivered): the stripped retry delivered without its attachments,
// and equally a copy that was charged (an unvouched re-post that
// finally vouched) or flagged isolate (a probe resumed after a pause).
// The urgent path never withholds a copy the ladder owns from its
// flush-ahead (onLadderEntry) — the ladder's copy is the message's
// delivery — so the copy's landing MUST leave the ladder's answer
// standing, or main.go's onLadder says "no" right after the flush-ahead
// delivered it and the urgent twin posts the same message again under
// its own dedup key (codex r14 finding 1: an unvouched A+B restored
// charged, A's twin arrives, the flush-ahead delivers A+B vouched, no
// verdict was recorded, urgent A posts). Every other copy of a landed
// message — buffered beside it, admitted later, handed back, the
// urgent twin built from the fresh event — is dropped as a duplicate of
// a delivered message, never posted. Clearing the verdict on a stripped
// landing instead (codex r11 finding 1) opened the same gap for the
// stripped retry.
//
// The stripped copy names its own disposition (pendingChannelInbound.
// stripped, codex r12 finding 1): a replayed stripped retry whose
// ledger entry did not survive the restart still lands as a terminal
// verdict, never as a plain delivery.
func (c *inboundCoalescer) landVerdicts(channel string, delivered []pendingChannelInbound) {
	type landed struct {
		ts string
		v  ladderVerdict
	}
	var terminal []landed
	c.mu.Lock()
	now := time.Now()
	for _, p := range delivered {
		if p.reaction {
			continue
		}
		ts := p.inbound.ProviderMessageID
		v, ok := c.verdictLocked(channel, ts, now)
		switch {
		case ok && v.retired:
			continue
		case !ok && p.stripped == "" && !onLadderEntry(p):
			continue
		case !ok && p.stripped != "":
			v = ladderVerdict{cause: errors.New(p.stripped)}
		case !ok:
			v = ladderVerdict{cause: fmt.Errorf("delivered by the ladder's copy after %d charged re-post(s)", p.attempts)}
		}
		v = ladderVerdict{retired: true, delivered: true, cause: v.cause}
		dropped, changed := c.retireLocked(channel, ts, v, now)
		if dropped > 0 {
			log.Printf("coalesce: chan=%s ts=%s the ladder's copy delivered — %d buffered duplicate copy(ies) dropped, never posted", channel, ts, dropped)
		}
		if changed {
			terminal = append(terminal, landed{ts, v})
		}
	}
	c.mu.Unlock()
	for _, l := range terminal {
		c.persistVerdict(channel, l.ts, l.v)
	}
}

// adoptVerdictLocked applies the ladder's standing verdict for p's ts to
// a copy entering the buffer: a retired ts is dropped (ok=false — the
// dead-letter file, or the stripped copy gc accepted, is that message's
// delivery); a stripped one makes the copy the stripped, isolated retry
// itself, so the bytes gc refused never go out under a fresh copy's
// name (codex r10 finding 1). The verdict is returned for the log line.
// Caller holds c.mu.
func (c *inboundCoalescer) adoptVerdictLocked(channel string, p pendingChannelInbound, now time.Time) (pendingChannelInbound, ladderVerdict, bool) {
	if p.reaction {
		return p, ladderVerdict{}, true
	}
	v, ok := c.verdictLocked(channel, p.inbound.ProviderMessageID, now)
	if !ok {
		return p, v, true
	}
	if v.retired {
		return p, v, false
	}
	p = withholdAttachments(p, v.cause)
	p.isolate = true
	if p.attempts < 1 {
		p.attempts = 1
	}
	return p, v, true
}

// dropBufferedCopiesLocked removes every buffered copy of id from the
// channel — the ladder just decided that entry's fate through another
// copy — and lets the scheduler re-judge the buffer. A real message's
// id is its ts; a reaction entry's id is its own event ts
// (reaction_events.go), so a reaction the ladder retired drops its
// copies from the pending buffer AND the no-wake side lane by that
// identity (codex r14 finding 2: a dead-lettered reaction's copies
// bypassed every verdict check). Returns how many were dropped. Caller
// holds c.mu.
func (c *inboundCoalescer) dropBufferedCopiesLocked(channel, id string) int {
	dropped := 0
	pend := c.pending[channel]
	kept := pend[:0]
	for _, p := range pend {
		if p.inbound.ProviderMessageID == id {
			dropped++
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) != len(pend) {
		if len(kept) == 0 {
			delete(c.pending, channel)
		} else {
			c.pending[channel] = kept
		}
	}
	side := c.reactions[channel]
	keptSide := side[:0]
	for _, p := range side {
		if p.inbound.ProviderMessageID == id {
			dropped++
			continue
		}
		keptSide = append(keptSide, p)
	}
	if len(keptSide) != len(side) {
		if len(keptSide) == 0 {
			delete(c.reactions, channel)
		} else {
			c.reactions[channel] = keptSide
		}
	}
	if dropped == 0 {
		return 0
	}
	c.scheduleLocked(channel, time.Time{})
	return dropped
}

// onLadder reports whether the rejection ladder owns the message
// (channel, ts): a standing verdict (stripped retry owed, delivered by
// the ladder's copy, or dead-lettered), or a buffered copy already
// charged or flagged for isolation. The urgent path consults it after
// flushAheadOf: an urgent copy of a message the ladder owns is NOT
// posted — the ladder's own copy (the stripped retry, the charged
// re-post: delivered or owed) or the dead-letter file is the message's
// delivery (codex r10 finding 2). Every landing of a ladder-owned copy
// leaves a terminal verdict (codex r11 finding 1, r14 finding 1), so
// the answer cannot flip between the flush-ahead and the urgent POST.
// Nil-safe.
func (c *inboundCoalescer) onLadder(channel, ts string) bool {
	if c == nil || ts == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.verdictLocked(channel, ts, time.Now()); ok {
		return true
	}
	for _, p := range c.pending[channel] {
		if !p.reaction && p.inbound.ProviderMessageID == ts && (p.isolate || p.attempts > 0) {
			return true
		}
	}
	return false
}

// isDeletedLocked reports whether (channel, ts) carries a live
// tombstone. Caller holds c.mu.
func (c *inboundCoalescer) isDeletedLocked(channel, ts string, now time.Time) bool {
	at, ok := c.deleted[channel][ts]
	return ok && now.Sub(at) <= deletionTombstoneTTL
}

// applyDeletionTombstones rewrites every non-reaction entry in batch
// whose ts carries a live tombstone. Used by the paths that re-handle
// entries OUTSIDE the pending buffer — permanent-failure isolation and
// dead-lettering (codex round-2 finding 2) — where markDeleted's
// pending rewrite cannot reach.
func (c *inboundCoalescer) applyDeletionTombstones(channel string, batch []pendingChannelInbound) {
	if c == nil || len(batch) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for i := range batch {
		if batch[i].reaction || batch[i].inbound.Text == deletedBySenderNotice {
			continue
		}
		if c.isDeletedLocked(channel, batch[i].inbound.ProviderMessageID, now) {
			applyDeletion(&batch[i])
			log.Printf("coalesce: chan=%s ts=%s deleted while detached — re-handled as a deletion notice", channel, batch[i].inbound.ProviderMessageID)
		}
	}
}

// applyDeletion rewrites one buffered entry into the deletion notice:
// text and parts collapse to the notice, attachments drop (surfacing a
// deleted message's files would defeat the sender's deletion). The
// entry keeps its ts and actor so dedup keys and the batch member line
// stay intact.
func applyDeletion(p *pendingChannelInbound) {
	p.inbound.Text = deletedBySenderNotice
	p.inbound.Attachments = nil
	p.threadAnchor, p.preamble, p.preambleLean, p.body, p.files = "", "", "", "", ""
	p.botQuotes = false
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

// disabledWindowRetryBase is the first retry delay on a channel whose
// accumulation window is zero (coalescing disabled: the coalescer then
// only ever carries spool replays and the dead-letter write retries).
// Without it a transient failure there would retry at request-
// completion speed forever (codex r4 finding 2).
const disabledWindowRetryBase = time.Second

// transientRetryDelay is the retry cadence after `failures` consecutive
// TRANSIENT delivery failures on a channel whose accumulation window is
// `window`: the base doubled failures-1 times, capped at
// maxTransientRetryDelay and floored at the base itself (a digest
// interval longer than the cap stays the operator's cadence). The base
// is the window, or disabledWindowRetryBase when the window is zero.
// Zero failures pass the window through unchanged.
func transientRetryDelay(window time.Duration, failures int) time.Duration {
	if failures <= 0 {
		return window
	}
	base := window
	if base <= 0 {
		base = disabledWindowRetryBase
	}
	d := base
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= maxTransientRetryDelay {
			d = maxTransientRetryDelay
			break
		}
	}
	if d < base {
		return base
	}
	return d
}

// backoffLogWorthy reports whether the n-th consecutive failure on a
// backoff with base `base` is a STATE CHANGE worth a log line (gp-sgu7
// contract 3, codex r1 finding 5): the first failure, and every
// failure whose delay differs from the previous one — so a channel
// logs once per doubling and once more on reaching the cap, then
// stays silent until it recovers. Returns the delay either way.
func backoffLogWorthy(base time.Duration, n int) (time.Duration, bool) {
	delay := transientRetryDelay(base, n)
	if n <= 1 {
		return delay, true
	}
	return delay, delay != transientRetryDelay(base, n-1)
}

// backoffCapSuffix is appended to the log line that announces a
// backoff reaching its cap, so the following silence is explained.
func backoffCapSuffix(delay time.Duration) string {
	if delay >= maxTransientRetryDelay {
		return " (at the cap; further failures at this cadence are not logged until the channel recovers)"
	}
	return ""
}

// retryDelayLocked is the delay for the channel's next timer: its
// window, backed off by its current run of transient failures. Caller
// holds c.mu.
func (c *inboundCoalescer) retryDelayLocked(channel string) time.Duration {
	return transientRetryDelay(c.windowFor(channel), c.transientFailures[channel])
}

// inBackoffLocked reports whether the channel is waiting out a
// transient-failure backoff: its restore armed a retry due at
// retryNotBefore and that moment has not come. Every take that would
// POST the buffer EARLY — the over-cap flush in enqueue/admitReaction,
// the reaction-overflow flush, a SIGHUP reconcile — must honor it
// (codex r1 finding 2): during an outage an over-cap buffer would
// otherwise cost one POST per enqueue and a reconcile would collapse a
// five-minute backoff to the one-second cap cadence. The armed backoff
// timer already covers the buffer. The urgent flush-ahead honors it
// too (codex r6): the urgent message proceeds on its own, the buffered
// batch waits for its retry. Caller holds c.mu.
func (c *inboundCoalescer) inBackoffLocked(channel string) bool {
	nb, ok := c.retryNotBefore[channel]
	return ok && time.Now().Before(nb)
}

// noteTransientFailure records one more consecutive transient failure
// for the channel and moves its retry deadline out to the backed-off
// delay — the ONLY writer of both (gp-sgu7). Rounds 1–8 of the codex
// gate each found a path that set or stretched the deadline on its
// own (a restore with no new failure, a reactions-only restore, an
// urgent twin returned after a failed mention — codex r8 finding 1:
// every restore replaced the deadline with now+delay, so repeated
// failed mentions postponed the buffer forever); now nothing else
// touches it. Logs once per backoff STATE change (contract 3: the
// first failure, each doubling, once more at the cap), naming the
// caller's `what`. Called before restore(), whose scheduleLocked then
// places the retry at the new deadline. Returns the count.
func (c *inboundCoalescer) noteTransientFailure(channel string, cause error, what string) int {
	c.mu.Lock()
	c.transientFailures[channel]++
	n := c.transientFailures[channel]
	delay, worthy := backoffLogWorthy(c.windowFor(channel), n)
	c.retryNotBefore[channel] = time.Now().Add(delay)
	c.mu.Unlock()
	if worthy {
		log.Printf("coalesce: chan=%s %s — transient failure #%d, retry in %s%s: %v", channel, what, n, delay, backoffCapSuffix(delay), cause)
	}
	return n
}

// deliveredOK ends the channel's transient-failure run: the deadline
// is lifted, and whatever real messages still sit in the buffer flush
// at the plain window instead of waiting out a deadline gc has just
// proven unnecessary (the scheduler pulls the timer in; a channel not
// in a run is unchanged). The next failure, if any, starts again at
// the window.
func (c *inboundCoalescer) deliveredOK(channel string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := c.transientFailures[channel]; n > 0 {
		log.Printf("coalesce: chan=%s delivered after %d transient failure(s) — retry cadence back to the window", channel, n)
	}
	delete(c.transientFailures, channel)
	delete(c.retryNotBefore, channel)
	delete(c.urgentDeferredAt, channel)
	c.scheduleLocked(channel, time.Now().Add(c.windowFor(channel)))
}

// hasRealPendingLocked reports whether the channel's pending buffer
// holds at least one real message (not a reaction riding an armed
// window). Caller holds c.mu.
func (c *inboundCoalescer) hasRealPendingLocked(channel string) bool {
	for _, p := range c.pending[channel] {
		if !p.reaction {
			return true
		}
	}
	return false
}

// timerWorthyLocked is the ONE predicate for "may this channel POST on
// a timer": a real message is buffered, or the no-wake reaction side
// lane has overflowed (the one sanctioned solo reaction wake, gp-9e7
// item 1). Reactions below the cap never earn a timer — not by
// admission, not by a restore, not by a withheld twin emptying the
// buffer beside them (codex r8 finding 3) — and an overflowed lane
// always does, even during a backoff (codex r8 finding 2: the deadline
// decides WHEN, never WHETHER). Both the arming side (scheduleLocked)
// and the firing side (flushTimer) consult it. Caller holds c.mu.
func (c *inboundCoalescer) timerWorthyLocked(channel string) bool {
	return c.hasRealPendingLocked(channel) || len(c.reactions[channel]) >= maxBufferedReactionsPerChannel
}

// disarmLocked stops and forgets the channel's timer, bumping the
// generation so an already-fired callback no-ops. Returns whether a
// timer was armed. Caller holds c.mu.
func (c *inboundCoalescer) disarmLocked(channel string) bool {
	delete(c.due, channel)
	t, ok := c.timers[channel]
	if !ok {
		return false
	}
	t.Stop()
	delete(c.timers, channel)
	c.gen[channel]++
	return true
}

// scheduleLocked is the ONE place a channel's flush timer is armed,
// moved or disarmed (gp-sgu7, codex r8). Eight gate rounds each found
// another path that armed its own timer and checked the backoff
// deadline for itself — enqueue, the over-cap short retry, restore
// (twice), a firing timer under the deadline, the reconcile, the
// urgent flush-ahead's twin withhold, the reaction overflow — and
// each patch left the next path uncovered. Now every one of them
// states only what it WANTS and this function derives the timer from
// the channel's state:
//
//   - a buffer left with no real message holds no riders: reactions
//     that were riding its armed window go back to the no-wake side
//     lane FIRST, where the cap counts them (codex r11 finding 3: a
//     retired duplicate left a hundred riders in pending, uncounted
//     and untimed, until the channel's next real message) — whichever
//     path emptied the buffer (a withheld twin, a landed verdict, a
//     dropped copy);
//   - nothing timer-worthy is buffered (timerWorthyLocked) → NO timer,
//     whatever the caller wanted: a take that would POST below-cap
//     reactions alone can never be armed;
//   - otherwise the timer fires at the EARLIER of its current target
//     and `at` — the moment the caller wants (a fresh window on
//     enqueue; a short poll behind an in-flight delivery; the plain
//     window after a recovery; zero = no new want, keep the target) —
//     and NEVER before the channel's transient-failure deadline
//     (retryNotBefore). With no timer and no want, the retry IS the
//     deadline when one is ahead (codex r9 finding 2: an overflow or a
//     restore arriving with the deadline less than a window away must
//     not wait a whole window past it), else a window from now. A
//     restore after a failure therefore lands on the deadline
//     noteTransientFailure just set; a restore WITHOUT a new failure
//     (an urgent twin handed back, reactions returned to their lane)
//     keeps the deadline exactly where it is.
//
// A target that already passed (the timer is about to fire, or a hand
// of the clock) is not moved: the callback's own take is the delivery.
// Re-arming bumps the generation so the replaced callback no-ops.
// Returns the target and whether the timer changed. Caller holds c.mu.
func (c *inboundCoalescer) scheduleLocked(channel string, at time.Time) (time.Time, bool) {
	if riders := c.pending[channel]; len(riders) > 0 && !c.hasRealPendingLocked(channel) {
		c.reactions[channel] = append(append([]pendingChannelInbound(nil), riders...), c.reactions[channel]...)
		delete(c.pending, channel)
	}
	if !c.timerWorthyLocked(channel) {
		return time.Time{}, c.disarmLocked(channel)
	}
	_, armed := c.timers[channel]
	target := time.Time{}
	if armed {
		target = c.due[channel]
	}
	if !at.IsZero() && (target.IsZero() || at.Before(target)) {
		target = at
	}
	now := time.Now()
	nb, backingOff := c.retryNotBefore[channel]
	if target.IsZero() {
		if backingOff && nb.After(now) {
			target = nb
		} else {
			target = now.Add(c.windowFor(channel))
		}
	}
	if backingOff && target.Before(nb) {
		target = nb
	}
	if armed && target.Equal(c.due[channel]) {
		return target, false
	}
	if armed {
		c.timers[channel].Stop()
	}
	c.gen[channel]++
	g := c.gen[channel]
	c.due[channel] = target
	c.timers[channel] = time.AfterFunc(time.Until(target), func() { c.flushTimer(channel, g) })
	return target, true
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

// capRetryDelay is the cadence for retrying a flush that lost its take
// to an in-flight delivery (gp-9e7 round 5, 3a/3b): the burst window,
// bounded above by overCapRetryCeiling — deliberately NOT windowFor,
// whose digest interval would leave an over-cap buffer growing for
// hours. The wait being covered is only "the in-flight POST settles",
// which is POST-scale, so a short poll is correct even on a digest
// channel.
func (c *inboundCoalescer) capRetryDelay() time.Duration {
	if c.window > 0 && c.window < overCapRetryCeiling {
		return c.window
	}
	return overCapRetryCeiling
}

// wantCapRetryLocked asks the scheduler for a SHORT retry — pulling a
// digest-scale target in — for a flush deferred behind an in-flight
// delivery (gp-9e7 round 5, 3a/3b). Every caller attempts the take
// FIRST and asks for this only on a failed TryLock, so repeated asks
// cannot starve the retry: each was itself a fresh flush attempt, and
// the timer only needs to cover the quiet tail after the last one. The
// deadline still binds (an unreachable gc gains nothing from a 1 s
// poll). Called with c.mu held.
func (c *inboundCoalescer) wantCapRetryLocked(channel string) {
	c.scheduleLocked(channel, time.Now().Add(c.capRetryDelay()))
}

// spillLateLocked routes one post-close admission out of the process:
// to the durable spool when wired (the next startup replays it), else
// to a loud loss log. It never touches the maps — the admission
// barrier's whole point is that nothing lands in memory after
// flushAll's final snapshot. Called with c.mu HELD; unlocks it.
func (c *inboundCoalescer) spillLateLocked(channel string, batch []pendingChannelInbound) {
	spill := c.spill
	c.mu.Unlock()
	if spill == nil || !spill(channel, batch) {
		log.Printf("coalesce: LOSS chan=%s %d item(s) admitted after the shutdown drain could not be spooled — LOST (already acked to Slack)", channel, len(batch))
		return
	}
	log.Printf("coalesce: chan=%s %d item(s) admitted after the shutdown drain — spooled for startup replay", channel, len(batch))
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
	// A deletion that raced ahead of this enqueue (gp-0qw item 3).
	// Checked BEFORE the admission barrier: a post-close straggler
	// spills to the durable spool, and tombstones are memory-only, so
	// the spilled line must already carry the notice (codex round-2
	// finding 3).
	now := time.Now()
	if c.isDeletedLocked(channel, p.inbound.ProviderMessageID, now) {
		applyDeletion(&p)
		log.Printf("coalesce: chan=%s ts=%s admitted after its deletion — buffered as a deletion notice", channel, p.inbound.ProviderMessageID)
	}
	// The ladder's verdict about this MESSAGE applies to every copy of
	// it (codex r10 finding 1): a copy of a dead-lettered ts is dropped
	// — before the admission barrier, so a post-close straggler is not
	// spooled either — and a copy of a ts owed its stripped retry enters
	// AS that retry.
	adopted, verdict, ok := c.adoptVerdictLocked(channel, p, now)
	if !ok {
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s ts=%s admitted after the rejection ladder %s — duplicate copy dropped, the same bytes are never re-posted", channel, p.inbound.ProviderMessageID, verdict.disposition())
		return
	}
	if adopted.isolate && !p.isolate {
		log.Printf("coalesce: chan=%s ts=%s admitted while its earlier copy is on the rejection ladder — this copy adopts the stripped, isolated retry", channel, p.inbound.ProviderMessageID)
	}
	p = adopted
	if c.closed {
		c.spillLateLocked(channel, []pendingChannelInbound{p})
		return
	}
	c.pending[channel] = append(c.pending[channel], p)
	pendingLen := len(c.pending[channel])
	// An over-cap buffer on a channel waiting out a transient-failure
	// backoff is NOT flushed early (gp-sgu7): its backoff timer is armed
	// and covers the buffer; every enqueue POSTing the growing buffer at
	// an unreachable gc would be the traffic-driven retry the backoff
	// exists to end. The cap bounds nothing here but memory, and the
	// buffer grows by at most the outage's traffic (codex r1 finding 2).
	if pendingLen >= maxCoalescePerChannel && !c.inBackoffLocked(channel) {
		batch, mu, ok := c.takeLocked(channel)
		if !ok {
			// A delivery for this channel is in flight (round 3): skip the
			// early flush — the buffer keeps its items, and a SHORT retry
			// timer (round 5, 3a: coalesce-window scale, replacing even an
			// armed digest-scale timer) flushes it promptly once the
			// in-flight delivery settles. The cap overshoots for at most
			// one delivery's duration plus one retry delay; nothing is
			// dropped.
			c.wantCapRetryLocked(channel)
			c.mu.Unlock()
			log.Printf("coalesce: chan=%s buffer full (%d) but delivery in flight — early flush deferred to a short retry", channel, pendingLen)
			return
		}
		swept := c.takeSweepsLocked(channel)
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s buffer full (%d) — early flush", channel, pendingLen)
		// Mutexes are held from take time (gp-9e7 fix round 2c/round 3);
		// launching the swept goroutines before the triggering channel's
		// own synchronous POST keeps the one-wake fold (round 2a).
		c.deliverSwept(swept)
		c.deliverBatch(channel, batch, mu)
		return
	}
	// The scheduler decides: a first message arms the window (moved out
	// to the deadline during a backoff — codex r3 finding 1); a later one
	// joins the armed timer unchanged.
	target, armed := c.scheduleLocked(channel, time.Now().Add(c.windowFor(channel)))
	c.mu.Unlock()
	if armed {
		log.Printf("coalesce: chan=%s buffered ts=%s (window %s armed)", channel, p.inbound.ProviderMessageID, time.Until(target).Round(time.Millisecond))
		return
	}
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
	// A copy of a reaction the ladder RETIRED (dead-lettered, by its own
	// event identity) is a duplicate of a handled event, exactly like a
	// retired message's copy at enqueue (codex r14 finding 2): the
	// refused bytes never ride another delivery, however the copy
	// arrives — a Slack redelivery, the spool replay's re-admission.
	// Admission is still final handling of the event.
	if v, ok := c.verdictLocked(channel, p.inbound.ProviderMessageID, time.Now()); ok && v.retired {
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s reaction ts=%s is a copy of a reaction the rejection ladder %s — dropped, never re-posted", channel, p.inbound.ProviderMessageID, v.disposition())
		return true
	}
	if c.closed {
		c.spillLateLocked(channel, []pendingChannelInbound{p})
		return true
	}
	if ridesArmedWindow {
		if _, armed := c.timers[channel]; armed {
			c.pending[channel] = append(c.pending[channel], p)
			pendingLen := len(c.pending[channel])
			if pendingLen >= maxCoalescePerChannel && !c.inBackoffLocked(channel) {
				batch, mu, ok := c.takeLocked(channel)
				if !ok {
					// Delivery in flight (round 3): a short retry timer
					// flushes once it settles (round 5, 3a); see enqueue.
					c.wantCapRetryLocked(channel)
					c.mu.Unlock()
					log.Printf("coalesce: chan=%s buffer full (%d) but delivery in flight — early flush deferred to a short retry", channel, pendingLen)
					return true
				}
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
	if n >= maxBufferedReactionsPerChannel && c.inBackoffLocked(channel) {
		// A channel in transient-failure backoff keeps its overflow too
		// (gp-sgu7): the overflow flush would POST at an unreachable gc.
		// But the overflow is now timer-worthy, and a reactions-only
		// backoff arms no timer of its own — so ask the scheduler, which
		// places the overflow flush AT the deadline (codex r8 finding 2:
		// suppressed without a timer, a lane that overflowed during an
		// outage never drained once traffic stopped). Logged once, when
		// the timer is actually armed.
		if _, armed := c.scheduleLocked(channel, time.Time{}); armed {
			log.Printf("coalesce: chan=%s reaction buffer full (%d) during a transient-failure backoff — overflow flush scheduled at the backoff deadline", channel, n)
		}
		c.mu.Unlock()
		return true
	}
	if n >= maxBufferedReactionsPerChannel {
		// Overflow: deliver rather than evict — reactions never drop.
		// The solo wake this costs takes a pathological reaction volume
		// with zero real traffic; the cap bounds memory, not content.
		batch, mu, ok := c.takeLocked(channel)
		if !ok {
			// Delivery in flight (round 3): the reactions stay in the
			// side-buffer, and a SHORT retry timer (round 5, 3b) delivers
			// them promptly once the in-flight delivery settles — waiting
			// for the next admission or the channel's next real moment
			// would be unbounded with zero further traffic. The timer this
			// arms is the overflow exception deferred, not a new wake
			// class: overflow already crossed the solo-wake line, and any
			// take before the retry fires merges the side-buffer and
			// no-ops the timer via its stale generation. Overshoot is
			// bounded: at most the reactions arriving during one in-flight
			// delivery plus one retry delay past the cap.
			c.wantCapRetryLocked(channel)
			c.mu.Unlock()
			log.Printf("coalesce: chan=%s reaction buffer full (%d) but delivery in flight — overflow flush deferred to a short retry", channel, n)
			return true
		}
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
// them into the timeline. Called with c.mu held.
//
// EVERY take acquires the channel's delivery mutex HERE, inside the
// caller's c.mu critical section, atomically with the take (gp-9e7
// round 3, generalizing round 2c from sweeps to all takes): from the
// instant a batch leaves the maps its mutex is held, so any later
// Lock/TryLock on that mutex genuinely serializes behind ALL older
// detached batches by construction — a newer same-channel take can no
// longer overtake an older one through the take-to-lock gap. The
// acquisition is TryLock, never a blocking Lock (the round-2 ABBA
// constraint: never block on a channel mutex while holding c.mu — a
// failing in-flight delivery holds the channel mutex and needs c.mu to
// restore). A failed TryLock means a delivery for the channel is in
// flight RIGHT NOW: ok=false, NO state changes (no gen bump, no timer
// disarm, no detach, no inflight window), and the caller skips or
// defers — the channel's armed timer / restore cycle covers the
// buffered items.
//
// On ok=true the returned mutex is HELD and an in-flight delivery
// window is open (c.inflight): the caller MUST route the batch through
// exactly one deliverBatch call — or, for flushAheadOf's inline
// delivery, unlock and call endDelivery itself — so flushAll can wait
// out detached batches (gp-9e7 fix round 1a/2b).
func (c *inboundCoalescer) takeLocked(channel string) ([]pendingChannelInbound, *sync.Mutex, bool) {
	mu := c.flushMuFor(channel)
	// Reservation check (round 5, 3c): a flushAheadOf caller is blocked
	// waiting for this channel's mutex — the batch is spoken for. Refuse
	// even to TryLock: a Go mutex in normal mode lets a TryLock barge in
	// between the in-flight delivery's unlock and the queued waiter's
	// wakeup, and that barge is exactly the steal the reservation
	// forbids. Callers treat this like any failed take (a delivery is,
	// from their perspective, in flight — the urgent one, imminently).
	if c.urgentWaiting[channel] > 0 {
		return nil, mu, false
	}
	if !mu.TryLock() {
		return nil, mu, false
	}
	return c.detachLocked(channel), mu, true
}

// detachLocked is the take itself: drain the maps, disarm the timer,
// bump the generation, open the in-flight window. Called with BOTH c.mu
// and the channel's delivery mutex held — takeLocked acquires the
// latter via TryLock; flushAheadOf's reservation path (round 5, 3c)
// acquires it by blocking OUTSIDE c.mu and then detaches here.
func (c *inboundCoalescer) detachLocked(channel string) []pendingChannelInbound {
	c.gen[channel]++
	c.inflight++
	if t, ok := c.timers[channel]; ok {
		t.Stop()
		delete(c.timers, channel)
	}
	delete(c.due, channel)
	batch := c.pending[channel]
	delete(c.pending, channel)
	if rs := c.reactions[channel]; len(rs) > 0 {
		batch = append(batch, rs...)
		delete(c.reactions, channel)
	}
	return batch
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
// LOCKED — acquired inside takeLocked's critical section, atomically
// with the take — and the delivery goroutine unlocks it after
// delivering (gp-9e7 fix round 2c).
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
// Each swept channel's delivery mutex is acquired at take time inside
// takeLocked — the same TryLock-under-c.mu discipline every take now
// follows (gp-9e7 fix round 2c, generalized in round 3): from the
// instant a batch leaves the maps its mutex is held, so an urgent
// flushAheadOf for that channel serializes strictly behind the older
// swept batch by construction. A failed TryLock means a delivery for
// that channel is in flight RIGHT NOW; the channel is skipped, keeping
// its buffer and its armed timer (pending non-empty ⇒ timer armed —
// enqueue and restore both guarantee it), so it flushes on its own
// schedule with ordering intact.
//
// Called with c.mu held; the caller hands the returned batches — each
// with its LOCKED mutex — to deliverSwept after releasing it (sorted
// for deterministic order across channels; order WITHIN each channel is
// the buffer order, untouched).
func (c *inboundCoalescer) takeSweepsLocked(except string) []sweptBatch {
	channels := make([]string, 0, len(c.pending))
	for ch := range c.pending {
		if ch == except || !c.hasRealPendingLocked(ch) {
			continue
		}
		if _, digest := c.policy.digestInterval(ch); digest {
			continue
		}
		if nb, ok := c.retryNotBefore[ch]; ok && time.Now().Before(nb) {
			// Waiting out a transient-failure backoff (gp-sgu7): sweeping
			// it now would be the fixed-cadence retry the backoff ends.
			continue
		}
		channels = append(channels, ch)
	}
	sort.Strings(channels)
	swept := make([]sweptBatch, 0, len(channels))
	for _, ch := range channels {
		b, m, ok := c.takeLocked(ch)
		if !ok {
			log.Printf("coalesce: chan=%s sweep skipped — delivery in flight; its own timer covers it", ch)
			continue
		}
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
// take time inside takeLocked (gp-9e7 fix round 2c) — so an urgent
// flushAheadOf can never slip in between take and delivery.
func (c *inboundCoalescer) deliverSwept(swept []sweptBatch) {
	for _, s := range swept {
		s := s
		go c.deliverBatch(s.channel, s.batch, s.mu)
	}
}

// deliverBatch posts one batch whose delivery mutex was acquired at
// take time by takeLocked, unlocking it after the delivery settles and
// restoring the batch for a timer retry on failure. The lock may have
// been taken on a different goroutine (a sweep's flushTimer) than the
// one delivering: sync.Mutex is not owner-tracked, so a cross-goroutine
// unlock is legal Go — the handoff of a LOCKED mutex is the point, it
// is what makes "batch out of the maps ⇒ mutex held" an invariant with
// no gap (gp-9e7 round 3). Closes the in-flight window its take opened,
// so every taken batch must reach exactly one deliverBatch call (see
// takeLocked).
func (c *inboundCoalescer) deliverBatch(channel string, batch []pendingChannelInbound, mu *sync.Mutex) {
	defer c.endDelivery()
	defer mu.Unlock()
	if len(batch) == 0 || c.deliver == nil {
		return
	}
	c.post(channel, batch)
}

// post delivers one taken batch under the channel's held delivery
// mutex — the ONE place a taken batch meets c.deliver (deliverBatch and
// the urgent flushAheadOf both route here). The batch is walked in ts
// order as SEGMENTS (codex r3 finding 2: an older message whose file
// download finished late must still deliver before a newer one):
// a member flagged isolate — an entry of a batch gc already refused,
// restored when its probe paused on a transient failure or stripped
// for its one retry — is posted ALONE, never re-batched (codex r1
// finding 3); a run of unflagged messages posts as one batch, the
// no-wake reactions riding with the LAST such run (or, with no plain
// run, behind the delivered probes — back to their side lane instead
// when a probe was refused, so a reaction never wakes solo behind a
// charged message). A failed plain segment takes the usual failure
// path and everything later waits for the next window (the take is
// re-sorted then, so restore order is irrelevant). Returns the first
// plain segment's delivery error, nil when everything delivered or a
// probe paused and restored the remainder.
func (c *inboundCoalescer) post(channel string, batch []pendingChannelInbound) error {
	sorted := append([]pendingChannelInbound(nil), batch...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].inbound.ProviderMessageID < sorted[j].inbound.ProviderMessageID
	})
	sorted = collapseSameTS(channel, sorted)
	var reals, reactions []pendingChannelInbound
	for _, p := range sorted {
		if p.reaction {
			reactions = append(reactions, p)
		} else {
			reals = append(reals, p)
		}
	}
	lastPlain := -1
	for i, p := range reals {
		if !p.isolate {
			lastPlain = i
		}
	}
	refused, announced := false, false
	for i := 0; i < len(reals); {
		if reals[i].isolate {
			if !announced {
				announced = true
				c.mu.Lock()
				_, worthy := backoffLogWorthy(c.windowFor(channel), c.transientFailures[channel])
				c.mu.Unlock()
				if worthy { // a resume inside a capped transient run is not a state change
					log.Printf("coalesce: chan=%s resuming isolation from a batch gc refused — flagged members post alone, in ts order, never re-batched", channel)
				}
			}
			rest := append(append([]pendingChannelInbound{}, reals[i+1:]...), reactions...)
			paused, r := c.isolate(channel, reals[i:i+1], rest)
			if paused {
				return nil
			}
			refused = refused || r
			i++
			continue
		}
		j := i
		for j < len(reals) && !reals[j].isolate {
			j++
		}
		seg := append([]pendingChannelInbound{}, reals[i:j]...)
		if j-1 == lastPlain {
			seg = append(seg, reactions...)
			reactions = nil
		}
		err := c.deliver(channel, seg)
		if err == nil {
			c.landVerdicts(channel, seg)
			c.deliveredOK(channel)
			i = j
			continue
		}
		remaining := append(append([]pendingChannelInbound{}, reals[j:]...), reactions...)
		if chargeableDeliveryFailure(err) {
			// Only the entries gc actually saw enter the ladder; a member
			// the hook dropped before posting (a same-ts duplicate, a twin
			// an urgent delivery already carried) was never refused and is
			// simply done (codex r5 finding 1).
			submitted := submittedOf(seg, err)
			if dropped := len(seg) - len(submitted); dropped > 0 {
				log.Printf("coalesce: chan=%s %d member(s) of the refused segment were never posted (duplicates or already delivered) — not charged", channel, dropped)
			}
			c.failed(channel, submitted, err)
			c.restore(channel, remaining)
		} else {
			c.noteTransientFailure(channel, err, fmt.Sprintf("batch of %d failed, %d entries restored", len(seg), len(seg)+len(remaining)))
			c.restore(channel, append(seg, remaining...))
		}
		return err
	}
	if len(reactions) == 0 {
		return nil
	}
	if refused {
		c.restore(channel, reactions)
		return nil
	}
	if err := c.deliver(channel, reactions); err != nil {
		if chargeableDeliveryFailure(err) {
			reactions = submittedOf(reactions, err)
		}
		c.failed(channel, reactions, err)
		return err
	}
	c.deliveredOK(channel)
	return nil
}

// collapseSameTS keeps ONE real entry per ts in a take — the copy whose
// rejection state has progressed furthest (attempts, then the isolate
// flag) — so a same-ts copy admitted while the original waited out a
// backoff flagged for isolation can never post the refused bytes again
// as its own plain segment after the original was dead-lettered
// (codex r9 finding 1: the deliver hook's own same-ts dedup sees one
// segment at a time). Same-ts copies are the same Slack message (the
// bot-mention twin pair, a redelivery), already acked; the surviving
// copy is its delivery, exactly as the hook's dedup and the urgent
// path's twin withhold already treat it. Reactions carry their own
// ids and are left alone. `sorted` is in ts order; positions are kept.
func collapseSameTS(channel string, sorted []pendingChannelInbound) []pendingChannelInbound {
	first := make(map[string]int, len(sorted)) // ts → index in out
	out := make([]pendingChannelInbound, 0, len(sorted))
	dropped := 0
	for _, p := range sorted {
		if p.reaction {
			out = append(out, p)
			continue
		}
		ts := p.inbound.ProviderMessageID
		if i, seen := first[ts]; seen {
			if moreProgressed(p, out[i]) {
				out[i] = p
			}
			dropped++
			continue
		}
		first[ts] = len(out)
		out = append(out, p)
	}
	if dropped > 0 {
		log.Printf("coalesce: chan=%s %d same-ts duplicate(s) collapsed before delivery — the copy furthest along the rejection ladder delivers", channel, dropped)
	}
	return out
}

// moreProgressed reports whether a's rejection state is further along
// than b's: more charged attempts, else flagged for isolation.
func moreProgressed(a, b pendingChannelInbound) bool {
	if a.attempts != b.attempts {
		return a.attempts > b.attempts
	}
	return a.isolate && !b.isolate
}

// restore re-queues a batch whose delivery failed (or was withheld),
// ahead of anything buffered meanwhile, and hands the retry to the
// scheduler. The re-queued batch may exceed the cap; the next enqueue
// then triggers an early flush rather than anything being dropped.
// Reaction entries split back into the no-wake side-buffer instead
// (gp-9e7 item 1): reactions alone earn no timer (the retry would be
// the solo reaction wake the buffer exists to prevent) unless the lane
// has overflowed — they wait for the channel's next real delivery like
// any buffered reaction.
//
// restore itself decides NOTHING about cadence (codex r8 finding 1):
// it does not touch the deadline — only noteTransientFailure, called
// by the failure paths BEFORE restore, moves it — and it arms no timer
// of its own. scheduleLocked places the buffer's timer at the deadline
// when there is one (so a plain window armed by a mid-flight enqueue
// moves out, codex r3 finding 1; a reactions-only failure holds the
// deadline against real messages, codex r2 finding 2) and leaves it
// where it was when this restore carried no new failure — an urgent
// message's withheld twin handed back after the mention failed, or
// reactions returned to their lane, can no longer postpone the
// channel's retry.
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
	// A deletion that landed while these entries were detached in the
	// failed delivery (gp-0qw item 3): the retry — or the post-barrier
	// spill below, whose line must already be the notice because
	// tombstones do not survive a restart (codex round-3 finding 3) —
	// must carry the notice, not the deleted text.
	now := time.Now()
	live := msgs[:0]
	for i := range msgs {
		if c.isDeletedLocked(channel, msgs[i].inbound.ProviderMessageID, now) && msgs[i].inbound.Text != deletedBySenderNotice {
			applyDeletion(&msgs[i])
			log.Printf("coalesce: chan=%s ts=%s deleted during an in-flight delivery — restored as a deletion notice", channel, msgs[i].inbound.ProviderMessageID)
		}
		// A copy of a ts the ladder retired while this one was detached
		// (an urgent twin handed back after the buffered copy was
		// dead-lettered) is a duplicate of a dead-lettered message.
		if v, ok := c.verdictLocked(channel, msgs[i].inbound.ProviderMessageID, now); ok && v.retired {
			log.Printf("coalesce: chan=%s ts=%s handed back after the rejection ladder %s — duplicate copy dropped", channel, msgs[i].inbound.ProviderMessageID, v.disposition())
			continue
		}
		live = append(live, msgs[i])
	}
	msgs = live
	// A reaction copy handed back after the ladder retired its event
	// (codex r14 finding 2) is dropped the same way.
	liveReactions := reactions[:0]
	for _, p := range reactions {
		if v, ok := c.verdictLocked(channel, p.inbound.ProviderMessageID, now); ok && v.retired {
			log.Printf("coalesce: chan=%s reaction ts=%s handed back after the rejection ladder %s — duplicate copy dropped", channel, p.inbound.ProviderMessageID, v.disposition())
			continue
		}
		liveReactions = append(liveReactions, p)
	}
	reactions = liveReactions
	if c.closed {
		// Post-barrier restore (defensive — no take can follow the
		// barrier, but a map entry here would sit past the final
		// snapshot forever): spill instead of re-queueing.
		c.spillLateLocked(channel, append(append([]pendingChannelInbound{}, msgs...), reactions...))
		return
	}
	defer c.mu.Unlock()
	if len(reactions) > 0 {
		c.reactions[channel] = append(reactions, c.reactions[channel]...)
	}
	if len(msgs) > 0 {
		c.pending[channel] = append(msgs, c.pending[channel]...)
	}
	c.scheduleLocked(channel, time.Time{})
}

// failed handles one failed delivery. Called with the channel's
// delivery mutex held (deliverBatch / flushAheadOf) and c.mu NOT held.
//
// A TRANSIENT error (network, 5xx, 429, operational 4xx) restores the
// whole batch for the next timer — the retry-forever durability the
// coalescer always had, so a gc restart never loses buffered chatter —
// on the channel's doubling backoff (noteTransientFailure → restore).
//
// A PAYLOAD rejection (permanentDeliveryFailure: 400/413/415/422 — the
// same payload can never be accepted) means the batch holds a poisoned
// entry, and which one is unknown. The real messages are ISOLATED —
// each re-delivered alone, right now, under the same mutex — so an
// innocent batch-mate delivers instead of waiting behind (or dying
// with) the poison, and only a single gc still rejects is charged
// (charge: the rejection ladder — one retry without attachments, then
// the dead-letter hook; never the same bytes again, gp-sgu7). The
// probe stops at the first TRANSIENT single: gc is failing right now,
// so every further single could cost a full client timeout under the
// flush mutex; that single, every untested message, and the reactions
// are restored together, uncharged, and the next window retries the
// whole batch (codex r1 finding 1). Reaction entries never POST solo
// (the gp-9e7 no-wake contract) and are charged ONLY when their own
// group was submitted and rejected (codex r1 finding 2): a batch that
// WAS the reaction group charges every member; when a message single
// took the blame the reactions return to the side-buffer uncharged;
// when every message single delivered, the reaction group is posted
// next — behind those real deliveries, so never a solo wake — and its
// own verdict decides. Every path makes progress: a rejected batch
// never returns to the buffer unchanged, so the log carries a bounded
// number of rejection lines per poisoned entry, not one per window
// forever (gp-xnc, 2026-08-26 incident).
// chargeableDeliveryFailure reports whether a failure must be COUNTED
// against the batch's retry budget instead of being put back uncharged.
// Two shapes qualify: gc REJECTING the payload (a 4xx it will answer
// the same way next window), and gc accepting the payload but never
// vouching that it reached the session (gp-32q). Both are standing
// conditions, so the uncharged path — which exists for transient faults
// like a connection reset — would retry either one every window
// forever. Charging gives them the bounded ladder gp-xnc built:
// retried under the cap, then dead-lettered for an operator.
//
// This matters most inside the isolation loop below, which re-delivers
// singles AFTER a batch rejection: a single that gc then accepts
// without vouching is an unvouched delivery reached by a different
// route, and classifying it as transient there would reopen the
// unbounded re-notify loop the arm at the top of failed() closes.
func chargeableDeliveryFailure(err error) bool {
	return permanentDeliveryFailure(err) || errors.Is(err, errDeliveryUnvouched)
}

func (c *inboundCoalescer) failed(channel string, batch []pendingChannelInbound, cause error) {
	if len(batch) == 0 {
		return
	}
	if errors.Is(cause, errDeliveryUnvouched) {
		// gc ACCEPTED this batch but would not vouch that it reached the
		// session (gp-32q). Neither of the two existing classes fits: it
		// is not a payload rejection (isolation has no poisoned member to
		// find — the whole delivery is what failed to land), and it must
		// not restore uncharged like a transient fault, because a receipt
		// that never vouches would then re-notify this channel every
		// window forever. Charging every member gives the same bounded
		// ladder gp-xnc built: retried under the cap, then written to the
		// dead-letter file for an operator. Reaction entries ride the
		// same path — charge → restore returns them to their side lane.
		c.applyDeletionTombstones(channel, batch)
		for _, p := range batch {
			c.charge(channel, p, cause)
		}
		return
	}
	if !permanentDeliveryFailure(cause) {
		c.noteTransientFailure(channel, cause, fmt.Sprintf("batch of %d failed, restored", len(batch)))
		c.restore(channel, batch)
		return
	}
	// Isolation re-posts and dead-letters entries WITHOUT passing back
	// through the pending buffer, so a deletion that landed while this
	// batch was detached must be applied here (codex round-2 finding 2).
	c.applyDeletionTombstones(channel, batch)
	var msgs, reactions []pendingChannelInbound
	for _, p := range batch {
		if p.reaction {
			reactions = append(reactions, p)
		} else {
			msgs = append(msgs, p)
		}
	}
	if len(msgs) == 1 && len(reactions) == 0 {
		c.charge(channel, msgs[0], cause)
		return
	}
	if len(msgs) == 0 {
		// The batch WAS the reaction group and gc rejected it.
		for _, p := range reactions {
			c.charge(channel, p, cause)
		}
		return
	}
	log.Printf("coalesce: chan=%s batch of %d rejected by gc — isolating %d message(s) to find the poisoned one: %v",
		channel, len(batch), len(msgs), cause)
	paused, msgRejected := c.isolate(channel, msgs, reactions)
	if paused || len(reactions) == 0 {
		return
	}
	if msgRejected {
		c.restore(channel, reactions)
		return
	}
	// Every message single delivered: the rejection was the batch itself
	// or the reactions. Post the reaction group now — behind the real
	// deliveries that just landed, never solo — and let its verdict decide.
	err := c.deliver(channel, reactions)
	switch {
	case err == nil:
		c.deliveredOK(channel)
	case chargeableDeliveryFailure(err):
		for _, p := range submittedOf(reactions, err) {
			c.charge(channel, p, err)
		}
	default:
		// A transient failure here is a failure like any other: the
		// channel enters its backoff so the reaction overflow honors a
		// deadline (codex r5 finding 2).
		c.noteTransientFailure(channel, err, fmt.Sprintf("reaction group of %d failed behind the isolated singles, returned to the side lane", len(reactions)))
		c.restore(channel, reactions)
	}
}

// isolate is the probe: each of msgs — members of a batch gc refused —
// is POSTed alone, in order, under the channel's held delivery mutex.
// A single gc accepts is delivered; one it refuses (or accepts without
// vouching for) is charged to the rejection ladder. The probe PAUSES at
// the first transient single (codex r1 finding 1 on gp-xnc: gc failing
// right now would cost a client timeout per member): that single and
// every untested one are restored uncharged — flagged isolate, so the
// next window RESUMES the probes instead of re-posting the refused
// batch (codex r1 finding 3 on gp-sgu7) — together with `rest`, the
// entries riding along unchanged (the reactions after a batch refusal;
// the unflagged newer messages on a resume). Returns paused, and
// whether any single was refused and charged.
func (c *inboundCoalescer) isolate(channel string, msgs, rest []pendingChannelInbound) (paused, refused bool) {
	for i := range msgs {
		// A deletion can land between singles while an earlier POST
		// blocks (codex round-3 finding 2); msgs[i] is rewritten in place
		// so the transient-failure rest slice below carries it too.
		c.applyDeletionTombstones(channel, msgs[i:i+1])
		p := msgs[i]
		err := c.deliver(channel, []pendingChannelInbound{p})
		if err == nil {
			// gc is reachable: the channel's transient run, if any, ends
			// here — a later unrelated failure must start at the window,
			// not inherit a capped count (codex r2 finding 4).
			c.landVerdicts(channel, []pendingChannelInbound{p})
			c.deliveredOK(channel)
			continue
		}
		if !chargeableDeliveryFailure(err) {
			untested := append([]pendingChannelInbound{}, msgs[i:]...)
			for j := range untested {
				untested[j].isolate = true
			}
			all := append(untested, rest...)
			c.noteTransientFailure(channel, err, fmt.Sprintf("isolation paused after %d/%d single(s); %d entries restored uncharged (the %d untested resume as single probes, never re-batched)",
				i, len(msgs), len(all), len(untested)))
			c.restore(channel, all)
			return true, refused
		}
		refused = true
		// The member came out of a batch gc REFUSED: whatever the ladder
		// does with it (a stripped retry, an unvouched same-payload
		// retry), it comes back flagged so it posts alone — never inside
		// the refused batch again (codex r4 finding 1).
		p.isolate = true
		c.charge(channel, p, err)
	}
	return false, refused
}

// charge records one rejection against a single entry and acts on the
// rejection ladder's verdict (nextRejectionStep, the ONLY decision
// point — gp-sgu7): a refused entry with attachments is restored ONCE
// without them (withholdAttachments), an unvouched entry under the cap
// is restored unchanged, everything else is handed to the deadLetter
// hook. The entry retires ONLY on the hook's confirmed-durable verdict;
// an unconfirmed write (disk full, bad override, planted symlink) keeps
// it buffered so the next window's rejection retries the write — one
// more rejection line per window beats losing an already-acked message
// (codex r1 finding 3); that re-post is the one deliberate exception
// to "never the same bytes again". With no hook wired (bare test
// configs) the entry is dropped, loudly.
func (c *inboundCoalescer) charge(channel string, p pendingChannelInbound, cause error) {
	// The dead-letter file is durable and tombstones are not: a deleted
	// entry must be written as its notice (codex round-3 finding 2).
	single := []pendingChannelInbound{p}
	c.applyDeletionTombstones(channel, single)
	p = single[0]
	step := nextRejectionStep(p, cause)
	if p.attempts < maxCoalesceDeliveryAttempts {
		p.attempts++ // saturates at the cap: a record written after a failed write recovers still reports a bounded count (codex r2 finding 3)
	}
	ts := p.inbound.ProviderMessageID
	switch step {
	case stepRetryWithoutAttachments:
		n := len(p.inbound.Attachments)
		p = withholdAttachments(p, cause)
		p.isolate = true // its one retry posts alone: a refusal then charges it, not a batch-mate
		// The verdict is about the message: every other buffered copy of
		// this ts leaves (this stripped copy is the retry), and a copy
		// admitted from here on enters as the stripped retry itself
		// (codex r10 finding 1). Reactions carry their own ids.
		c.mu.Lock()
		c.recordVerdictLocked(channel, ts, ladderVerdict{cause: cause}, time.Now())
		dropped := c.dropBufferedCopiesLocked(channel, ts)
		c.mu.Unlock()
		c.restore(channel, []pendingChannelInbound{p})
		log.Printf("coalesce: chan=%s ts=%s refused by gc with %d attachment(s) — retrying ONCE without them (the files stay on disk; the text names their paths)%s: %v",
			channel, ts, n, duplicateCopiesSuffix(dropped), cause)
		return
	case stepRetrySame:
		c.restore(channel, []pendingChannelInbound{p})
		return
	}
	// Retired: dead-lettered, parked for the write, or lost for want of a
	// sink — in every case the message is out of the delivery path for
	// good, and so is every other copy of it, now or later.
	verdict := ladderVerdict{retired: true, cause: cause}
	c.mu.Lock()
	dropped, changed := c.retireLocked(channel, ts, verdict, time.Now())
	c.mu.Unlock()
	if changed {
		c.persistVerdict(channel, ts, verdict)
	}
	if dropped > 0 {
		log.Printf("coalesce: chan=%s ts=%s %d buffered duplicate copy(ies) of the retired message dropped — never re-posted", channel, ts, dropped)
	}
	if c.deadLetter == nil {
		log.Printf("coalesce: LOSS chan=%s ts=%s rejected %d times and no dead-letter sink is wired — dropped: %v",
			channel, ts, p.attempts, cause)
		return
	}
	if !c.deadLetter(channel, []pendingChannelInbound{p}, cause) {
		// The WRITE failed, not the delivery verdict: the entry leaves
		// the delivery path for good and only the write retries
		// (parkDeadLetter) — re-posting it to gc for another refusal
		// was the one remaining way the same refused bytes went out
		// again (codex r1 finding 1).
		c.parkDeadLetter(channel, p, cause)
		return
	}
	log.Printf("coalesce: chan=%s ts=%s dead-lettered after %d rejected deliveries — later messages in this channel no longer wait behind it: %v",
		channel, p.inbound.ProviderMessageID, p.attempts, cause)
}

// duplicateCopiesSuffix names the buffered same-ts copies a verdict
// removed, for the verdict's own log line.
func duplicateCopiesSuffix(dropped int) string {
	if dropped == 0 {
		return ""
	}
	return fmt.Sprintf("; %d buffered duplicate copy(ies) dropped", dropped)
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
	// This timer is consumed; whatever follows takes the buffer or asks
	// the scheduler for a new one.
	delete(c.timers, channel)
	delete(c.due, channel)
	if !c.timerWorthyLocked(channel) {
		// Nothing that may wake the session on a timer is left — the
		// buffer emptied under a still-armed timer (a withheld urgent
		// twin, codex r8 finding 3). A take now would POST below-cap
		// reactions alone; they ride the channel's next real moment.
		c.mu.Unlock()
		return
	}
	if c.inBackoffLocked(channel) {
		// Fired under the channel's backoff deadline (a reconcile race,
		// clock jitter): wait it out rather than POST under it (codex r3
		// finding 1).
		nb := c.retryNotBefore[channel]
		c.scheduleLocked(channel, nb)
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s timer fired under the backoff deadline — re-armed for %s", channel, time.Until(nb).Round(time.Millisecond))
		return
	}
	batch, mu, ok := c.takeLocked(channel)
	if !ok {
		// A delivery for this channel is in flight (round 3) — the timer
		// fired into the gap between another path's take and its POST
		// settling. Ask for the SHORT retry cadence (round 5, 3a): the
		// window already elapsed, so the only wait left is the in-flight
		// POST — a full re-wait (hours, on a digest channel) would strand
		// the batch. A failed delivery's restore reschedules on its own.
		c.wantCapRetryLocked(channel)
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s timer flush deferred — delivery in flight; re-armed on a short retry", channel)
		return
	}
	swept := c.takeSweepsLocked(channel)
	c.mu.Unlock()
	// Ordering safety no longer depends on launch order: every batch's
	// delivery mutex was acquired at take time inside the critical
	// section above (gp-9e7 fix round 2c/round 3), so an urgent
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
// message with no redelivery guarantee. A twin already ON the rejection
// ladder (flagged isolate, or charged) is NOT withheld: gc refused that
// message's bytes, its stripped retry is the delivery, and the urgent
// copy — built from the fresh event, attachments and all — must defer
// to it (the caller checks onLadder; codex r10 finding 2).
func (c *inboundCoalescer) flushAheadOf(channel, excludeTS string) []pendingChannelInbound {
	if c == nil {
		return nil
	}
	// Take-or-reserve (round 3, reservation hardened in round 5, 3c):
	// the take acquires the delivery mutex at take time (TryLock under
	// c.mu, like every take). A failed take means an older delivery is
	// in flight — the urgent message must serialize behind it, so
	// RESERVE the channel (urgentWaiting, which makes every competing
	// take refuse outright), BLOCK into the channel mutex's wait queue
	// WITHOUT holding c.mu (the ABBA constraint), and detach with the
	// mutex KEPT. The old wait-unlock-retake loop reserved nothing —
	// between its Unlock and re-take, any timer/early-flush/sweep taker
	// could TryLock-win the freed mutex, stealing the batch and sending
	// the urgent path back to wait, indefinitely under a steady stream
	// of competitors. With the reservation the win is BOUNDED and the
	// bound is exact: the urgent path acquires the mutex after the
	// in-flight delivery ahead of it (plus any urgent peers queued
	// before it) and nothing else — competing takers cannot enter from
	// the moment the flag is up, and deferrable ones retry on the short
	// cadence after the urgent delivery settles. The settled delivery
	// restores failed items BEFORE releasing the mutex (deliverBatch
	// body precedes its unlock defer), so detaching after the wakeup
	// still picks them up and they flush ahead of the urgent message.
	// Lock order: blocking on mu then taking c.mu is the order restore()
	// already uses under a held delivery mutex; c.mu-then-mu remains
	// TryLock-only everywhere. detachLocked opens the in-flight window,
	// balanced by the deferred endDelivery below.
	c.mu.Lock()
	if c.inBackoffLocked(channel) {
		// The channel is waiting out a transient-failure backoff (codex
		// r6): flushing the buffer ahead of every urgent message would
		// re-post the failed batch at mention cadence under a five-minute
		// deadline. The urgent message proceeds on its own (out of order,
		// as after a failed flush-ahead); the buffer keeps its armed
		// backoff timer, deadline untouched. The urgent message's own
		// buffered twin is still WITHHELD (codex r7 finding 1): left
		// buffered, a timer firing while the urgent POST is in flight
		// would deliver it beside the urgent copy under a different
		// dedup key. The caller restores it if the urgent delivery fails.
		c.deferUrgentFlushLocked(channel)
		withheld := c.withholdTwinLocked(channel, excludeTS)
		c.mu.Unlock()
		return withheld
	}
	batch, mu, ok := c.takeLocked(channel)
	if !ok {
		c.urgentWaiting[channel]++
		c.mu.Unlock()
		mu.Lock()
		c.mu.Lock()
		c.urgentWaiting[channel]--
		if c.inBackoffLocked(channel) {
			// The in-flight delivery this waiter queued behind failed
			// transiently and put the channel in backoff: same verdict as
			// above, nothing detached (no in-flight window opened).
			c.deferUrgentFlushLocked(channel)
			withheld := c.withholdTwinLocked(channel, excludeTS)
			c.mu.Unlock()
			mu.Unlock()
			return withheld
		}
		batch = c.detachLocked(channel)
	}
	c.mu.Unlock()
	defer c.endDelivery() // inline delivery below; the take is closed on every path
	defer mu.Unlock()     // held from take time
	var withheld []pendingChannelInbound
	if excludeTS != "" {
		kept := batch[:0]
		for _, p := range batch {
			if p.inbound.ProviderMessageID == excludeTS && !onLadderEntry(p) {
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
		// the entries to the side-buffer (restore arms no timer for below-cap
		// reactions) and let the caller piggyback them on its own
		// delivery. The take already serialized on the flush mutex, so
		// any in-flight coalesced POST completed before this point.
		if len(batch) > 0 {
			c.restore(channel, batch)
		}
		return withheld
	}
	if c.deliver == nil {
		return withheld
	}
	if err := c.post(channel, batch); err != nil {
		log.Printf("coalesce: chan=%s flush-ahead failed; batch restored for timer retry or isolated/dead-lettered if gc rejected it (urgent message proceeds out of order): %v", channel, err)
	}
	return withheld
}

// deferUrgentFlushLocked logs — once per backoff state, never per
// urgent message — that an urgent flush-ahead left the buffer to its
// backoff timer. Caller holds c.mu.
func (c *inboundCoalescer) deferUrgentFlushLocked(channel string) {
	if !c.urgentDeferLogWorthyLocked(channel) {
		return
	}
	log.Printf("coalesce: chan=%s urgent flush-ahead deferred — channel in transient-failure backoff (#%d, retry at %s); %d buffered message(s) wait for it, the urgent message proceeds alone",
		channel, c.transientFailures[channel], c.retryNotBefore[channel].Format("15:04:05"), len(c.pending[channel]))
}

// urgentDeferLogWorthyLocked reports whether the deferral is a new
// backoff STATE for the channel — keyed by the effective retry delay,
// so consecutive capped failures with mentions between them log once
// (codex r7 finding 3) — and records it. Caller holds c.mu.
func (c *inboundCoalescer) urgentDeferLogWorthyLocked(channel string) bool {
	delay := c.retryDelayLocked(channel)
	if last, ok := c.urgentDeferredAt[channel]; ok && last == delay {
		return false
	}
	c.urgentDeferredAt[channel] = delay
	return true
}

// withholdTwinLocked removes and returns the buffered copies of ts (an
// urgent message's twin) WITHOUT taking the buffer, then lets the
// scheduler judge what is left: real messages keep their timer and
// deadline untouched; a buffer left with no real message loses its
// timer (codex r8 finding 3: armed, it would take and POST below-cap
// reactions alone at expiry, while the urgent POST was still
// unresolved), and the scheduler returns the reactions that were
// riding the armed window to the no-wake side lane so no sweep sees a
// reactions-only buffer. Caller holds c.mu; the caller of flushAheadOf
// owns the returned entries (restore on urgent failure).
func (c *inboundCoalescer) withholdTwinLocked(channel, ts string) []pendingChannelInbound {
	if ts == "" {
		return nil
	}
	var withheld []pendingChannelInbound
	kept := make([]pendingChannelInbound, 0, len(c.pending[channel]))
	for _, p := range c.pending[channel] {
		if p.inbound.ProviderMessageID == ts && !onLadderEntry(p) {
			withheld = append(withheld, p)
			continue
		}
		kept = append(kept, p)
	}
	if len(withheld) == 0 {
		return nil
	}
	if len(kept) == 0 {
		delete(c.pending, channel)
	} else {
		c.pending[channel] = kept
	}
	c.scheduleLocked(channel, time.Time{})
	log.Printf("coalesce: chan=%s buffered twin ts=%s withheld from a deferred flush-ahead (urgent copy delivers it)", channel, ts)
	return withheld
}

// onLadderEntry reports whether a buffered copy is already the
// rejection ladder's: flagged to post alone as a probe, or charged.
func onLadderEntry(p pendingChannelInbound) bool {
	return !p.reaction && (p.isolate || p.attempts > 0)
}

// deliverBufferedReactions drains the channel's no-wake reaction
// side-buffer and posts it as its own batch. Callers invoke it ONLY
// immediately after a successful real delivery for the channel (gp-9e7
// fix round 1b): the near-simultaneous POST rides that delivery's wake
// exactly like a swept batch, so the reactions still never wake a
// session solo — and because the drain happens after the real POST
// commits, a skipped or failed real delivery simply leaves the
// side-buffer untouched. A failed reaction POST restores the entries
// to the side-buffer (the scheduler arms no timer for below-cap reactions).
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
	if c.inBackoffLocked(channel) {
		// A mention that succeeded while the channel backs off must not
		// turn its reaction drain into a retry at mention cadence, whose
		// own failure would push the real messages' deadline out again
		// (codex r7 finding 2). The side lane waits for the backoff
		// timer's take like everything else on the channel.
		worthy := c.urgentDeferLogWorthyLocked(channel)
		c.mu.Unlock()
		if worthy {
			log.Printf("coalesce: chan=%s buffered reaction drain deferred — channel in transient-failure backoff; they ride the backoff retry", channel)
		}
		return
	}
	mu := c.flushMuFor(channel)
	if c.urgentWaiting[channel] > 0 || !mu.TryLock() {
		// Take-time lock discipline (round 3): a delivery for the channel
		// is in flight — or an urgent flushAheadOf holds the channel's
		// reservation (round 5, 3c) and its take will merge the
		// side-buffer itself. Leave the reactions in the side-buffer —
		// they piggyback on the channel's next take/real moment; no
		// timer, no wake, nothing dropped.
		c.mu.Unlock()
		log.Printf("coalesce: chan=%s buffered reaction drain skipped — delivery in flight; they ride the next real moment", channel)
		return
	}
	delete(c.reactions, channel)
	c.inflight++ // side-buffer take: closed by deliverBatch below
	c.mu.Unlock()
	log.Printf("coalesce: chan=%s delivering %d buffered reaction(s) behind the real delivery", channel, len(rs))
	c.deliverBatch(channel, rs, mu)
}

// pendingContains reports whether ts is currently buffered for channel.
// The thread-context preamble filter treats buffered ids as delivered:
// they are guaranteed to land in the same or an earlier delivery than
// the message whose preamble is being built.
// markDeleted records a deletion tombstone for (channel, ts) and
// replaces every still-buffered copy with an explicit deletion notice
// (gp-0qw item 3, pc_b334cff7f9c6): a message deleted on Slack before
// its buffered delivery must reach the session as "message deleted by
// sender", never as a dangling ts whose text resolves to nothing. The
// tombstone catches the copies this rewrite cannot see — one whose
// enqueue the deletion event raced ahead of, and one detached into an
// in-flight delivery that then fails and restores. A copy whose
// in-flight POST succeeds was delivered before the deletion was
// processed; that race is accepted. Returns how many buffered copies
// were replaced right now (0 when the ts is not currently buffered).
func (c *inboundCoalescer) markDeleted(channel, ts string) int {
	if c == nil || channel == "" || ts == "" {
		return 0
	}
	c.mu.Lock()
	c.tombstoneLocked(channel, ts, time.Now())
	n := 0
	for i := range c.pending[channel] {
		if c.pending[channel][i].inbound.ProviderMessageID == ts {
			applyDeletion(&c.pending[channel][i])
			n++
		}
	}
	persist := c.persistDeletion
	c.mu.Unlock()
	// Durable record last, outside mu (file I/O): a spool line written
	// by any producer before OR after this point is rewritten on replay.
	// Two accepted residuals: the dead-letter file — an operator-
	// inspection artifact that never auto-delivers — can capture text
	// deleted in the window between charge()'s tombstone check and its
	// write; and a deletion straggler that reaches this call only after
	// main sealed the spool is refused (loudly), the same best-effort
	// class as any other post-seal straggler (gp-9e7 round 3, 2c).
	if persist != nil {
		persist(channel, ts)
	}
	return n
}

// deletedBySenderNotice is the text delivered in place of a buffered
// message that its sender deleted before delivery.
const deletedBySenderNotice = "[message deleted by sender before delivery]"

func (c *inboundCoalescer) pendingContains(channel, ts string) bool {
	if c == nil || ts == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pendingContainsLocked(channel, ts)
}

// pendingContainsLocked is pendingContains with c.mu already held.
func (c *inboundCoalescer) pendingContainsLocked(channel, ts string) bool {
	for _, p := range c.pending[channel] {
		if p.inbound.ProviderMessageID == ts {
			return true
		}
	}
	return false
}

// reconcileTimers re-judges every armed timer against the current
// policy (SIGHUP): a channel flipped digest→immediate must not keep
// waiting out a stale two-hour window. Each armed channel ASKS the
// scheduler for the new policy's window from now — and keeps its
// established target when that is nearer (codex r9 finding 2: a
// reconcile a second before an eight-second retry must not push it to
// fifteen, nor a digest channel another two hours). A channel sitting
// over either buffer cap, or whose backed-off retry is already due,
// asks for the short over-cap retry instead (gp-9e7 round 5, 3a/3b):
// a window there would leave an over-cap buffer growing for hours
// (round-6 gate finding). Over-cap state is read from the maps, so the
// clamp also self-heals a reconcile that races the retry arming.
func (c *inboundCoalescer) reconcileTimers() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	channels := make([]string, 0, len(c.timers))
	for channel := range c.timers {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	now := time.Now()
	for _, channel := range channels {
		at := now.Add(c.windowFor(channel))
		nb, backingOff := c.retryNotBefore[channel]
		switch {
		case backingOff && !now.Before(nb):
			// The backed-off retry is already due: fire it promptly rather
			// than waiting a whole window again.
			at = now.Add(c.capRetryDelay())
		case len(c.pending[channel]) >= maxCoalescePerChannel ||
			len(c.reactions[channel]) >= maxBufferedReactionsPerChannel:
			at = now.Add(c.capRetryDelay())
		}
		// A deadline still ahead binds inside the scheduler (gp-sgu7,
		// codex r1 finding 2): an over-cap buffer does not shorten it, or
		// a SIGHUP during an outage would turn a five-minute backoff into
		// the one-second cap cadence.
		c.scheduleLocked(channel, at)
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
	c.drainPending()
	// Entries whose dead-letter write never confirmed sit outside the
	// maps the drain above emptied; give the write one last try, else
	// spool them (gp-sgu7, codex r1 finding 1).
	c.flushParkedDeadLetters()
}

// drainPending is flushAll's fixpoint drain of the pending and reaction
// maps; it closes admission (c.closed) on both of its exits.
func (c *inboundCoalescer) drainPending() {
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
				delete(c.due, channel)
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
				// The batch is forgotten from memory either way — the
				// process is exiting — but it is reported as SPOOLED only
				// on the spill's confirmed write+fsync verdict; anything
				// else is the loud last-resort LOSS log, same contract as
				// spool-disabled (round 3, 1a).
				if spill == nil || !spill(channel, batch) {
					log.Printf("coalesce: SHUTDOWN LOSS chan=%s %d undeliverable item(s) could not be spooled — LOST (already acked to Slack; the watermark backfill cannot recover admitted events)", channel, len(batch))
					continue
				}
				log.Printf("coalesce: shutdown chan=%s %d undeliverable item(s) — spooled for startup replay", channel, len(batch))
			}
			return
		}
		c.mu.Unlock()
		sort.Strings(channels)
		for _, channel := range channels {
			c.mu.Lock()
			batch, mu, ok := c.takeLocked(channel)
			c.mu.Unlock()
			if !ok {
				// A delivery for this channel is in flight (round 3): the
				// next pass's inflight wait joins it and re-takes whatever
				// a failure restored — nothing is skipped for good.
				log.Printf("coalesce: shutdown chan=%s drain deferred — delivery in flight; next pass re-takes it", channel)
				continue
			}
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
func deliverCoalescedBatch(cfg config, channel string, batch []pendingChannelInbound) error {
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
		return nil
	}
	env := batch[len(batch)-1].inbound
	peerItems, peerDropped := cfg.peerContext.flush(channel)
	peerBlock := formatPeerContextBlock(peerItems, peerDropped)
	firstHelp := cfg.replyHelp.first(channel)
	helpBlock := ""
	if firstHelp {
		helpBlock = replyHelpBlock(cfg, channel)
	}
	var text string
	if len(batch) == 1 {
		// A single entry delivers under the SAME head-protected contract
		// as the immediate path (gp-0qw): thread anchor with the parent
		// line captured at intake, boilerplate only inside the budget,
		// body tail-trimmed behind the protected head. Entries that
		// predate the parts fields (legacy spool replays) deliver their
		// folded Text as the body with a bare-ts anchor synthesized for
		// thread replies.
		p, ok := batch[0].reminderParts(channel)
		if !ok {
			p = channelReminderParts{body: env.Text, ts: env.ProviderMessageID, channelID: channel}
			// Reaction notifications carry the thread root in
			// ReplyToMessageID but are explicitly not asks — a reply
			// anchor on one would contradict its own "no reply expected"
			// body (codex round-2 finding 5).
			if rt := env.ReplyToMessageID; !batch[0].reaction && rt != "" && rt != env.ProviderMessageID {
				p.anchor = formatThreadReplyAnchor(rt, "", "")
			}
		}
		composed, usedPeer, usedHelp, usedPreamble, trimmed := composeChannelReminderText(p, peerBlock, helpBlock, cfg.reminderTextBudget)
		text = composed
		if firstHelp && !usedHelp {
			cfg.replyHelp.unmark(channel)
			firstHelp = false
			log.Printf("coalesce: chan=%s ts=%s reply how-to withheld — delivery over reminder budget %d; re-arms for a smaller delivery (gp-9gc)",
				channel, env.ProviderMessageID, cfg.reminderTextBudget)
		}
		if peerBlock != "" && !usedPeer {
			cfg.peerContext.restore(channel, peerItems, peerDropped)
			peerItems, peerDropped = nil, 0
			log.Printf("coalesce: chan=%s ts=%s peer context withheld — delivery over reminder budget %d; restored to ride the next delivery",
				channel, env.ProviderMessageID, cfg.reminderTextBudget)
		}
		if p.preamble != "" && !usedPreamble {
			log.Printf("coalesce: chan=%s ts=%s thread-context preamble omitted — delivery over reminder budget %d",
				channel, env.ProviderMessageID, cfg.reminderTextBudget)
		}
		if trimmed {
			log.Printf("coalesce: chan=%s ts=%s body tail trimmed to reminder budget %d — head preserved, marker names the full text",
				channel, env.ProviderMessageID, cfg.reminderTextBudget)
		}
	} else {
		// Multi-message batches stay VERBATIM — the digest contract is
		// "nothing dropped or summarized", each member line carries its
		// sender, ts, and "(in thread <ts>)" — and in practice a batch
		// that overflows the budget is also the delivery shape the
		// transport handles atomically (bracketed paste above 4096
		// bytes). Only the boilerplate is budget-gated here (gp-9gc):
		// the how-to re-arms and rides a later, smaller delivery.
		text = formatCoalescedBlock(cfg, channel, batch)
		var attachments []externalAttachment
		for _, p := range batch {
			attachments = append(attachments, p.inbound.Attachments...)
		}
		env.Attachments = attachments
		env.DedupKey = coalesceBatchDedupKey(batch)
		if peerBlock != "" {
			text = peerBlock + "\n\n" + text
		}
		if firstHelp {
			if cfg.reminderTextBudget > 0 && len(text)+2+len(helpBlock) > cfg.reminderTextBudget {
				cfg.replyHelp.unmark(channel)
				firstHelp = false
				log.Printf("coalesce: chan=%s reply how-to withheld — batch over reminder budget %d; re-arms for a smaller delivery (gp-9gc)",
					channel, cfg.reminderTextBudget)
			} else {
				text += "\n\n" + helpBlock
			}
		}
	}
	env.Text = text
	receipt, err := postInboundWithReceipt(cfg, env)
	verdict := receipt.verdict(cfg.deliveryReceiptGate)
	// Delivery-receipt gate (gp-32q), same contract as the urgent path:
	// a batch gc accepted but would not vouch for is re-posted once in
	// place while the member claims are still held.
	for attempt := 0; err == nil && verdict == receiptUnconfirmed && attempt < deliveryReceiptRepostAttempts && receiptRepostAllowed(cfg); attempt++ {
		log.Printf("coalesce: chan=%s newest_ts=%s gc did not vouch for delivery (%s) — re-posting in place (attempt %d/%d)",
			channel, env.ProviderMessageID, receipt.logField(verdict), attempt+1, deliveryReceiptRepostAttempts)
		receipt, err = postInboundWithReceipt(cfg, env)
		verdict = receipt.verdict(cfg.deliveryReceiptGate)
	}
	if err == nil && verdict == receiptHeld {
		log.Printf("coalesce: HELD chan=%s batch=%d newest_ts=%s gc accepted the batch and has not finished delivering it — not re-posting — %s",
			channel, len(batch), env.ProviderMessageID, receipt.logField(verdict))
	}
	if err == nil && verdict == receiptUnconfirmed {
		// gc took the batch but would not vouch that it reached the
		// session. Routed into the SAME failure path as a rejected POST
		// (codex r1 P2/P3): that path is what releases the member claims
		// for a parked same-ts urgent twin, puts peer context and the
		// once-per-channel reply how-to back, and hands the batch to
		// failed() — which returns reaction entries to their no-wake side
		// lane and charges each message so the retry is BOUNDED and ends
		// in the dead-letter file rather than looping.
		//
		// An earlier revision returned nil here to avoid the coalescer's
		// retry-forever contract turning a never-vouching receipt into an
		// unbounded re-notify loop. That was worse: an ordinary room
		// message has ONE Slack event, so retiring the batch discarded
		// the only copy the adapter still owned — a detected loss with no
		// recovery. errDeliveryUnvouched is chargeable instead (see
		// failed()), which bounds the retry without discarding anything.
		log.Printf("coalesce: UNDELIVERED chan=%s batch=%d newest_ts=%s gc accepted the batch but did not vouch that it reached the session after %d re-post(s) — %s",
			channel, len(batch), env.ProviderMessageID, deliveryReceiptRepostAttempts, receipt.logField(verdict))
		err = errDeliveryUnvouched
	}
	if err != nil {
		// One line per DISTINCT failure per channel, not one per attempt
		// (gp-sgu7 contract 3): the 2026-09-08 incident wrote this exact
		// line 34,000 times. A failure whose text matches the channel's
		// last logged one is silent; the next success clears the memory.
		if cfg.postFailures.changed(channel, err.Error()) {
			log.Printf("coalesced inbound POST failed: chan=%s batch=%d: %v", channel, len(batch), err)
		}
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
		// The ladder judges what was POSTed, not what was handed in
		// (codex r5 finding 1): report the filtered batch with the cause.
		return &submittedDeliveryError{submitted: batch, err: err}
	}
	for _, key := range ownedClaims {
		cfg.channelClaims.commit(key)
	}
	tss := make([]string, 0, len(batch))
	for _, p := range batch {
		tss = append(tss, p.inbound.ProviderMessageID)
	}
	cfg.deliveredIDs.record("", channel, tss...)
	cfg.postFailures.clear(channel)
	log.Printf("inbound (coalesced): chan=%s batch=%d newest_ts=%s text=%dch %s",
		channel, len(batch), env.ProviderMessageID, len(env.Text), receipt.logField(verdict))
	return nil
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
