package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// --- receipt follow-up after a HELD pending (gp-3yg) -------------------------
//
// The pending=HOLD ruling (gp-32q) is correct and stays: gc waits for a
// busy session to reach an idle boundary before pasting, longer than any
// HTTP budget, so "pending" is a busy session's normal receipt and a
// re-post there duplicates the very messages being protected. But a hold
// CONCLUDES the claim, and until this file existed that was the end of
// the story: if gc's background send never landed, the adapter had no
// second copy, no alarm, and no retry — the message was simply gone and
// the HELD log line was the only trace. Taylor's C0B0Y964Q1Z message of
// 2026-08-28 (ts 1787901226) was lost exactly this way and found 19
// hours later when Afik bumped the thread.
//
// gc now records every fan-out's conclusion against its receipt id and
// answers GET /extmsg/inbound/receipts/{id} (gascity gp-3yg). So after a
// hold the adapter ASKS, instead of guessing:
//
//   - pending   → keep asking (bounded by receiptFollowUpDeadline)
//   - concluded → per-member landing evidence first: a receipt whose
//                 members ALL either took the payload or had it pasted
//                 whole with only the submit unconfirmed (gp-2io: status
//                 pending WITH complete byte counts, under a pending or
//                 partial summary) is RESOLVED — the payload is in
//                 every pane, and a re-post and a dead letter would
//                 both be wrong.
//                 Otherwise read the receipt exactly as the synchronous
//                 path would: vouched / no_route → RESOLVED (late); the
//                 hold was right; anything gc names undelivered → LOST
//   - unknown   → LOST. gc holds no record: the fan-out died with its
//                 process (restart) or aged out. gc never reports a
//                 RUNNING fan-out as unknown, so nobody is still trying.
//   - non-200   → UNVERIFIED. This gc has no such endpoint (pre-gp-3yg);
//                 fail open to the plain hold, loudly, so a pack pinned
//                 ahead of the gc rebuild neither duplicates nor lies.
//
// Every answer is checked to be ABOUT the receipt that was asked for: an
// answer echoing a different or missing receipt id is not an answer
// (codex r1 P1 #1) — a stale or mis-routed "unknown" must never re-post
// a message gc delivered.
//
// LOST recovers with ONE re-post of the identical envelope — dedup-keyed:
// gc dedups the transcript by provider message id, so the recovery copy
// costs at most a duplicate notification, the trade this codebase always
// takes over loss. Only a VOUCHED recovery is RECOVERED and leaves no
// dead letter: a dead letter must mean the agent did NOT get the message
// (that is what makes them worth reading, and false ones are what
// incident 2 produced). A recovery copy that is itself held is followed
// up once more; a recovery copy gc reports as reaching nobody, accepts
// without a receipt, or does not vouch for leaves the loss STANDING, and
// it is written to the dead-letter file so it survives the process
// (codex r1 P1 #3). Still-pending at the deadline or at shutdown is not a
// loss and is not re-posted (a slow send is not a lost one) — but it is
// not silence either: a dead-letter record says the state is unknown and
// needs a hand check.

// receiptPollState is what one poll of gc's receipt endpoint told us.
type receiptPollState int

const (
	// receiptPollPending: gc is still trying. Ask again.
	receiptPollPending receiptPollState = iota
	// receiptPollConcluded: the fan-out finished; the receipt carries its
	// outcome and is judged by the same verdict as a synchronous one.
	receiptPollConcluded
	// receiptPollUnknown: gc holds no record of the id. A definite
	// statement — nobody is still trying — and so a definite loss.
	receiptPollUnknown
	// receiptPollUnavailable: no usable answer — the route is missing (a
	// gc that predates gp-3yg answers 404), gc is down, the body is not
	// the agreed shape, or the answer names a different receipt. Fail
	// OPEN: this is the plain hold, said out loud.
	receiptPollUnavailable
)

func (s receiptPollState) String() string {
	switch s {
	case receiptPollPending:
		return "pending"
	case receiptPollConcluded:
		return "concluded"
	case receiptPollUnknown:
		return "unknown"
	default:
		return "unavailable"
	}
}

var (
	// errReceiptFollowUpLost is the dead-letter cause when gc said the
	// background send did not land and no recovery copy was vouched for.
	errReceiptFollowUpLost = errors.New("gc reported the held delivery lost and no recovery re-post was vouched for")
	// errReceiptFollowUpStuck is the dead-letter cause when gc never
	// concluded: the message MAY have landed, and nobody can say. Not a
	// loss claim — a "verify by hand" marker.
	errReceiptFollowUpStuck = errors.New("gc never concluded the held delivery — state unknown, verify by hand")
)

// receiptFollowUpInterval is the poll cadence. gc's fan-out concludes
// within roughly NudgeIdleTimeout (30s) plus one paste, so a few seconds
// between asks resolves the common case in a handful of polls without
// hammering the control plane from every held delivery at once.
const receiptFollowUpInterval = 5 * time.Second

// receiptFollowUpDeadline bounds how long a hold is followed. The
// fan-out's own lifetime is bounded only in principle (a wedged provider
// can hold it), so past this the follow-up stops asking and records the
// unknown state rather than polling forever.
const receiptFollowUpDeadline = 10 * time.Minute

// receiptFollowUpReposts is how many recovery copies a lost delivery gets.
// One: it exists to recover a send that died in the background, and a
// second loss is a standing condition a third copy would not fix.
const receiptFollowUpReposts = 1

// receiptFollowUpTombstones bounds how many FINISHED receipt ids are
// remembered so a late second note for one of them is not followed again
// (a second recovery copy would be a duplicate; codex r1 P2 #5). Holds are
// rare — this is many hours of them.
const receiptFollowUpTombstones = 1024

// shutdownReceiptFollowUpTimeout bounds the wait for outstanding
// follow-ups at shutdown. Polls in flight are cancelled by the drain, so
// what remains is one dead-letter write per follow-up — except a recovery
// re-post that was already in flight when the drain began, which runs to
// gcForwardClient's 20s deadline; the budget covers one of those plus its
// dead-letter write, because a follow-up that exits without a record is
// the loss this file exists to prevent (codex r1 P1 #2).
const shutdownReceiptFollowUpTimeout = 25 * time.Second

// receiptFollowUpLogLimit bounds gc-derived error text in a log line. The
// values come from another process (a rejected re-post carries gc's
// response body, up to deliveryReceiptBodyLimit), so they are truncated
// and control characters are stripped before they reach the log (codex
// r1 P2 #6). The dead-letter record keeps its own, longer, truncation.
const receiptFollowUpLogLimit = 240

// heldDelivery is what a claim-holding leg hands over after a HELD verdict:
// enough to ask gc later, to re-post the identical envelope, and to make a
// standing loss durable.
type heldDelivery struct {
	receiptID string
	// leg names the call site for the log line: inbound | coalesce |
	// alias dispatch.
	leg     string
	channel string
	// ts is the provider message id (newest for a batch) for greps.
	ts string
	// repost re-sends exactly the envelope the held receipt was issued
	// for and returns gc's new receipt, or a transport error.
	repost func() (deliveryReceipt, error)
	// deadLetter durably records the loss; true when the record is on
	// disk. Loss with no durable record is the bug this file fixes, so a
	// false return is logged at LOSS grade.
	deadLetter func(cause error) bool
}

// receiptPollFunc is the gc round-trip. ctx is cancelled by the drain so
// a poll in flight at shutdown returns at once instead of running to the
// client deadline.
type receiptPollFunc func(ctx context.Context, receiptID string) (receiptPollState, deliveryReceipt, error)

// receiptFollowUps runs one follow-up goroutine per held delivery.
type receiptFollowUps struct {
	poll     receiptPollFunc
	gated    bool
	draining *atomic.Bool
	interval time.Duration
	deadline time.Duration

	// ctx is cancelled (and stop closed) by drain: sleeping loops wake for
	// their final poll and in-flight polls abort.
	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}

	mu sync.Mutex
	// inflight counts follow-ups that have not finished making their
	// state durable — goroutines AND post-close synchronous records — and
	// cond wakes wait/drain when it drops. A plain WaitGroup cannot carry
	// this: a post-close note that Adds while a Wait is mid-return is a
	// documented panic, and one that Adds just after is not joined at all
	// (codex r3 P1). Here admission, completion and the drain's zero-test
	// all happen under mu.
	inflight int
	cond     *sync.Cond
	// closed is set by drain under mu; a note after that point is made
	// durable synchronously instead of starting a goroutine nobody will
	// wait for (codex r1 P1 #2).
	closed bool
	active map[string]bool
	// done remembers finished receipt ids (bounded FIFO) so a late
	// duplicate note is ignored.
	done      map[string]bool
	doneOrder []string
}

// newReceiptFollowUps builds the component. gated mirrors
// cfg.deliveryReceiptGate so a concluded receipt is judged by the same
// verdict as a synchronous one; draining is the shutdown flag the loops
// watch (nil means never draining).
func newReceiptFollowUps(poll receiptPollFunc, gated bool, draining *atomic.Bool) *receiptFollowUps {
	ctx, cancel := context.WithCancel(context.Background())
	f := &receiptFollowUps{
		poll:     poll,
		gated:    gated,
		draining: draining,
		interval: receiptFollowUpInterval,
		deadline: receiptFollowUpDeadline,
		ctx:      ctx,
		cancel:   cancel,
		stop:     make(chan struct{}),
		active:   make(map[string]bool),
		done:     make(map[string]bool),
	}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// release marks one follow-up finished and wakes anyone waiting for zero.
func (f *receiptFollowUps) release() {
	f.mu.Lock()
	f.inflight--
	f.cond.Broadcast()
	f.mu.Unlock()
}

// note starts following a held delivery. Nil-safe (bare test configs); a
// receipt already being followed, or already finished, is not followed
// again. After the drain has begun the hold is made durable right here,
// synchronously — the process is leaving and nothing will wait for a
// goroutine started now.
func (f *receiptFollowUps) note(h heldDelivery) {
	if f == nil {
		return
	}
	h.receiptID = strings.TrimSpace(h.receiptID)
	if h.receiptID == "" {
		// gc called it pending but named no receipt: the leg has already
		// concluded its claim on that verdict, and nothing can be asked
		// about it. An untrackable hold is recorded, not dropped (codex
		// r2 P2 #3) — and counted in flight while it is, so a drain joins
		// the write (codex r4 P1 #2).
		f.mu.Lock()
		f.inflight++
		f.mu.Unlock()
		defer f.release()
		f.recordNow(h, fmt.Errorf("%w: gc reported the delivery pending without a receipt id; nothing can follow it", errReceiptFollowUpStuck))
		return
	}
	f.mu.Lock()
	if f.active[h.receiptID] || f.done[h.receiptID] {
		f.mu.Unlock()
		return
	}
	if f.closed {
		// The process is leaving and nothing will wait for a goroutine
		// started now. Counted in flight anyway — under the same lock the
		// drain tests for zero — so a drain still waiting joins this
		// write, and marked active/done like a followed receipt so a
		// duplicate note is written once (codex r2 P1 #2, r3 P1, r4 P3).
		//
		// Residual, stated plainly: a note that arrives AFTER the drain's
		// zero-test returned is joined by nobody, and a process exit can
		// cut its write short. That is the straggler contract this
		// adapter already accepts for spool writes past the seal
		// (gp-9e7): main waits shutdownEventDrainTimeout for event
		// goroutines and proceeds regardless. recordNow therefore emits
		// its LOSS-grade line BEFORE the write, so the alarm survives
		// even when the record does not (codex r4 P1 #1, declined:
		// closing it needs an unbounded producer barrier at shutdown).
		f.active[h.receiptID] = true
		f.inflight++
		f.mu.Unlock()
		defer f.release()
		defer f.finish(h.receiptID)
		f.recordNow(h, fmt.Errorf("%w: receipt=%s noted after the shutdown drain began; nothing could follow it", errReceiptFollowUpStuck, receiptLogID(h.receiptID)))
		return
	}
	f.active[h.receiptID] = true
	f.inflight++
	f.mu.Unlock()
	go func() {
		defer f.release()
		defer f.finish(h.receiptID)
		f.run(h)
	}()
}

// recordNow makes a hold durable synchronously, on the caller's
// goroutine, with the LOSS-grade line emitted BEFORE the write begins: a
// process exit that cuts the write short must not erase both the record
// and the alarm (codex r2 P1 #2).
func (f *receiptFollowUps) recordNow(h heldDelivery, cause error) {
	log.Printf("receipt follow-up: LOSS leg=%s chan=%s ts=%s receipt=%s recording an untrackable hold now — if no dead-letter line follows for this receipt, the process exited before the record landed: %s",
		h.leg, h.channel, h.ts, receiptLogID(h.receiptID), describeReceiptErr(cause))
	f.stuck(h, cause)
}

// finish retires a receipt id from active into the bounded tombstone set.
func (f *receiptFollowUps) finish(receiptID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.active, receiptID)
	if f.done[receiptID] {
		return
	}
	f.done[receiptID] = true
	f.doneOrder = append(f.doneOrder, receiptID)
	for len(f.doneOrder) > receiptFollowUpTombstones {
		delete(f.done, f.doneOrder[0])
		f.doneOrder = f.doneOrder[1:]
	}
}

// wait blocks until every follow-up in flight has finished, or timeout.
func (f *receiptFollowUps) wait(timeout time.Duration) bool {
	if f == nil {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	expired := false
	timer := time.AfterFunc(timeout, func() {
		f.mu.Lock()
		expired = true
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer timer.Stop()
	for f.inflight > 0 && !expired {
		f.cond.Wait()
	}
	return f.inflight == 0
}

// drain is the shutdown path: closes admission, wakes sleeping loops and
// aborts in-flight polls so each loop takes its final decision now, and
// waits for every follow-up to make its state durable. A follow-up still
// outstanding at the timeout is named at LOSS grade — its state has no
// record.
func (f *receiptFollowUps) drain(timeout time.Duration) bool {
	if f == nil {
		return true
	}
	f.mu.Lock()
	if !f.closed {
		f.closed = true
		close(f.stop)
		f.cancel()
	}
	f.mu.Unlock()
	if !f.wait(timeout) {
		inflight, ids := f.outstandingSnapshot()
		log.Printf("receipt follow-up: LOSS %d follow-up(s) still outstanding after %s at shutdown — their state has NO durable record: receipts=%s anonymous=%d",
			inflight, timeout, strings.Join(ids, ","), inflight-len(ids))
		return false
	}
	return true
}

// outstandingSnapshot reports, under one lock, how many follow-ups have
// not made their state durable and the receipt ids of those that HAVE
// ids — a blank-id record (an untrackable hold still being written) is
// in the count but has no id to name, so the drain-timeout line counts
// it as anonymous instead of reporting nothing was outstanding (codex
// final round P3).
func (f *receiptFollowUps) outstandingSnapshot() (inflight int, ids []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids = make([]string, 0, len(f.active))
	for id := range f.active {
		ids = append(ids, receiptLogID(id))
	}
	sort.Strings(ids)
	return f.inflight, ids
}

// isDraining reports whether the process is leaving: the adapter-wide
// drain flag, or this component's own drain.
func (f *receiptFollowUps) isDraining() bool {
	if f.draining != nil && f.draining.Load() {
		return true
	}
	select {
	case <-f.stop:
		return true
	default:
		return false
	}
}

// pause sleeps one poll interval, or less if the drain begins meanwhile.
func (f *receiptFollowUps) pause() {
	select {
	case <-time.After(f.interval):
	case <-f.stop:
	}
}

// run is one held delivery's follow-up, start to finish.
func (f *receiptFollowUps) run(h heldDelivery) {
	f.runFrom(h, receiptFollowUpReposts, false)
}

// runFrom follows h.receiptID with repostsLeft recovery copies in hand. A
// recovery copy that is itself held re-enters here under its own receipt
// id with one copy fewer and recovery=true, so the ladder is bounded by
// receiptFollowUpReposts and ends in a dead letter, never a loop.
//
// recovery changes what the non-vouched outcomes mean (codex r2 P1 #1):
// for the ORIGINAL hold, "reached nobody" and "cannot verify" leave the
// hold standing as it was; for a RECOVERY copy the original loss is
// already established, so anything short of a vouch leaves that loss
// standing and must be recorded.
func (f *receiptFollowUps) runFrom(h heldDelivery, repostsLeft int, recovery bool) {
	receiptID := h.receiptID
	logID := receiptLogID(receiptID)
	started := time.Now()
	where := fmt.Sprintf("leg=%s chan=%s ts=%s", h.leg, h.channel, h.ts)
	if recovery {
		where += " recovery=true"
	}
	unavailableSeen := false
	// lastAnswer is what the most recent poll said and sawUsable whether
	// gc EVER gave a usable answer, for the give-up branches below: a
	// hold that was never answered at all fails open (a gc without the
	// endpoint), while one gc once answered — and then stopped answering,
	// as across a restart that may have lost the fan-out — is recorded as
	// unknown-state (codex r4 P1 #3).
	lastAnswer := receiptPollPending
	sawUsable := false
	for {
		state, receipt, err := f.poll(f.ctx, receiptID)
		draining := f.isDraining()
		if err == nil {
			lastAnswer = state
			if state != receiptPollUnavailable {
				sawUsable = true
			}
		}
		switch {
		case err != nil:
			// Not an answer. Ask again next tick; the deadline bounds it.
			// A cancelled poll is the drain itself, handled below.
			if !draining {
				log.Printf("receipt follow-up: poll failed %s receipt=%s (retrying): %s", where, logID, describeReceiptErr(err))
			}
		case state == receiptPollUnavailable:
			// Transient until the deadline says otherwise: gc answers 404
			// for this route while a city's Server is being replaced and
			// throughout a restart, and a pre-gp-3yg gc answers 404 for
			// good. The two are told apart by whether an answer ever
			// arrives (codex core r3 P2). Said once, not once per tick.
			if !unavailableSeen {
				unavailableSeen = true
				log.Printf("receipt follow-up: %s receipt=%s gc offers no usable receipt status yet (404, not the agreed shape, or a different receipt) — retrying until the deadline", where, logID)
			}
		case state == receiptPollUnknown:
			f.lost(h, repostsLeft, fmt.Errorf("gc holds no record of receipt %s — the background send died with its process or aged out; nobody is still trying", logID))
			return
		case state == receiptPollConcluded:
			// Landing evidence outranks the summary verdict. A busy
			// session's fan-out concludes with the payload pasted whole
			// and only the submit unconfirmed (gp-2io classifies that
			// member pending WITH its real byte counts) — and a mixed
			// room SUMMARIZES that as partial, a status the verdict
			// below reads as "a retry is clean". It is not: the payload
			// is in every pane. Checked for original holds and recovery
			// copies alike, on positive per-member proof under a
			// pending or partial summary only (landedUnconfirmed); the
			// synchronous receipt of a recovery re-post gets the same
			// check in lost().
			if receipt.landedUnconfirmed() {
				if recovery {
					log.Printf("receipt follow-up: RECOVERED %s the recovery copy landed whole; only the submit went unconfirmed — holding, never re-posting (gp-2io) — %s",
						where, receipt.logField(receipt.verdict(f.gated)))
					return
				}
				log.Printf("receipt follow-up: RESOLVED %s the held delivery landed whole after %s; only the submit went unconfirmed — not re-posting, no dead letter (gp-2io) — %s",
					where, time.Since(started).Round(time.Millisecond), receipt.logField(receipt.verdict(f.gated)))
				return
			}
			verdict := receipt.verdict(f.gated)
			switch verdict {
			case receiptVouched:
				if recovery {
					log.Printf("receipt follow-up: RECOVERED %s the held recovery copy landed after %s — %s",
						where, time.Since(started).Round(time.Millisecond), receipt.logField(verdict))
					return
				}
				log.Printf("receipt follow-up: RESOLVED %s the held delivery landed after %s — %s",
					where, time.Since(started).Round(time.Millisecond), receipt.logField(verdict))
				return
			case receiptNoRoute:
				if recovery {
					f.deadLetter(h, fmt.Errorf("%w: the held recovery copy reached nobody — %s", errReceiptFollowUpLost, receipt.logField(verdict)))
					return
				}
				log.Printf("receipt follow-up: RESOLVED %s the held delivery had nobody to reach after %s — %s",
					where, time.Since(started).Round(time.Millisecond), receipt.logField(verdict))
				return
			case receiptHeld:
				// Concluded, still pending, and landedUnconfirmed said the
				// members carry no landing evidence: gc finished trying
				// and cannot say the payload reached the pane. Not a loss
				// claim either — let the deadline decide (STUCK, recorded),
				// never a re-post.
			case receiptUnconfirmed:
				f.lost(h, repostsLeft, fmt.Errorf("gc concluded the held delivery did not land — %s", receipt.logField(verdict)))
				return
			default:
				// A block this adapter cannot read. Not a vouch, not an
				// accusation: for the original hold, fail open, loudly,
				// without a re-post.
				if recovery {
					f.deadLetter(h, fmt.Errorf("%w: the held recovery copy concluded with a receipt this adapter cannot interpret — %s", errReceiptFollowUpLost, receipt.logField(verdict)))
					return
				}
				log.Printf("receipt follow-up: UNVERIFIED %s receipt=%s gc concluded with a receipt this adapter cannot interpret — not re-posting — %s",
					where, logID, receipt.logField(verdict))
				return
			}
		}
		// Still pending, or no usable answer yet. Decide whether to keep
		// asking.
		giveUp := ""
		switch {
		case draining:
			// The process is leaving. One poll was just taken; nothing
			// here can wait out what it did not answer.
			giveUp = fmt.Sprintf("adapter shut down %s after the hold", time.Since(started).Round(time.Second))
		case time.Since(started) >= f.deadline:
			giveUp = fmt.Sprintf("%s after the hold", f.deadline)
		}
		if giveUp != "" {
			switch {
			case lastAnswer == receiptPollUnavailable && recovery:
				// The original loss is established and the recovery
				// copy could never be verified: the loss stands.
				f.deadLetter(h, fmt.Errorf("%w: the held recovery copy's receipt %s could not be verified (gc offered no usable receipt status) %s", errReceiptFollowUpLost, logID, giveUp))
			case lastAnswer == receiptPollUnavailable && !sawUsable:
				// gc NEVER offered a usable status for the original hold:
				// this is the pre-gp-3yg gc (or drift). Fail OPEN to the
				// plain hold, said out loud — never a re-post, never a
				// dead letter for something no evidence calls lost. A hold
				// gc did answer for before going unavailable falls
				// through to the record below: the endpoint exists, and
				// the silence may be a restart that lost the fan-out.
				log.Printf("receipt follow-up: UNVERIFIED %s receipt=%s gc offered no usable receipt status %s (pre-gp-3yg gc, or not the agreed shape) — the hold stands unverified, exactly as before this follow-up existed",
					where, logID, giveUp)
			default:
				f.stuck(h, fmt.Errorf("%w: receipt=%s still pending %s", errReceiptFollowUpStuck, logID, giveUp))
			}
			return
		}
		if state == receiptPollPending && err == nil {
			log.Printf("receipt follow-up: still pending %s receipt=%s after %s", where, logID, time.Since(started).Round(time.Second))
		}
		f.pause()
	}
}

// lost handles a definite loss: loud log, one dedup-keyed recovery copy,
// and a durable record when the loss stands.
func (f *receiptFollowUps) lost(h heldDelivery, repostsLeft int, cause error) {
	where := fmt.Sprintf("leg=%s chan=%s ts=%s", h.leg, h.channel, h.ts)
	logID := receiptLogID(h.receiptID)
	log.Printf("receipt follow-up: LOST %s receipt=%s the held delivery never landed: %s", where, logID, describeReceiptErr(cause))
	switch {
	case repostsLeft <= 0:
		f.deadLetter(h, fmt.Errorf("%w: %v (recovery copies exhausted)", errReceiptFollowUpLost, cause))
		return
	case f.isDraining():
		// Same rule as the in-place re-post: never spend a gc round-trip
		// during the drain. The durable record is the recovery.
		f.deadLetter(h, fmt.Errorf("%w: %v (adapter shutting down; no recovery copy sent)", errReceiptFollowUpLost, cause))
		return
	case h.repost == nil:
		f.deadLetter(h, fmt.Errorf("%w: %v (this leg cannot re-post)", errReceiptFollowUpLost, cause))
		return
	}
	log.Printf("receipt follow-up: re-posting %s receipt=%s the identical envelope (dedup-keyed: gc dedups the transcript by provider message id)", where, logID)
	receipt, err := h.repost()
	if err != nil {
		f.deadLetter(h, fmt.Errorf("%w: %v; recovery re-post failed: %s", errReceiptFollowUpLost, cause, describeReceiptErr(err)))
		return
	}
	if receipt.landedUnconfirmed() {
		// The recovery copy's own synchronous receipt already proves the
		// payload is in every pane — a mixed room folds delivered+pending
		// to partial, which the verdict below would read as a standing
		// loss (codex final round P2 #2). Same classifier, same outcome
		// as the polled path.
		log.Printf("receipt follow-up: RECOVERED %s the recovery copy landed whole; only the submit went unconfirmed — holding, never re-posting (gp-2io) — %s",
			where, receipt.logField(receipt.verdict(f.gated)))
		return
	}
	verdict := receipt.verdict(f.gated)
	switch verdict {
	case receiptVouched:
		log.Printf("receipt follow-up: RECOVERED %s the recovery copy landed — %s", where, receipt.logField(verdict))
	case receiptHeld:
		// The session is still busy; the recovery copy is in the same
		// position the original was. Follow IT, with one fewer copy left.
		if receipt.id == "" {
			f.deadLetter(h, fmt.Errorf("%w: %v; recovery re-post held with no receipt id to follow", errReceiptFollowUpLost, cause))
			return
		}
		log.Printf("receipt follow-up: recovery copy HELD %s new_receipt=%s — following it", where, receiptLogID(receipt.id))
		next := h
		next.receiptID = receipt.id
		f.runFrom(next, repostsLeft-1, true)
	default:
		// no_route (the recovery reached nobody), unsupported (gc took
		// it without vouching), unconfirmed (gc says it did not land):
		// none of these is the agent having the message. The loss
		// stands (codex r1 P1 #3).
		f.deadLetter(h, fmt.Errorf("%w: %v; recovery re-post not vouched: %s", errReceiptFollowUpLost, cause, receipt.logField(verdict)))
	}
}

// stuck handles "gc never concluded": durable record, no re-post.
func (f *receiptFollowUps) stuck(h heldDelivery, cause error) {
	log.Printf("receipt follow-up: STUCK leg=%s chan=%s ts=%s receipt=%s %s — not re-posting (a slow send is not a lost one); recording for a hand check",
		h.leg, h.channel, h.ts, receiptLogID(h.receiptID), describeReceiptErr(cause))
	f.deadLetter(h, cause)
}

// deadLetter makes the state durable and says so at LOSS grade either way,
// so a grep for LOSS finds every message an agent may not have.
func (f *receiptFollowUps) deadLetter(h heldDelivery, cause error) {
	where := fmt.Sprintf("leg=%s chan=%s ts=%s receipt=%s", h.leg, h.channel, h.ts, receiptLogID(h.receiptID))
	if h.deadLetter != nil && h.deadLetter(cause) {
		log.Printf("receipt follow-up: LOSS %s written to the dead-letter file — %s", where, describeReceiptErr(cause))
		return
	}
	log.Printf("receipt follow-up: LOSS %s and the dead-letter write FAILED or is unavailable — this message has NO durable record: %s", where, describeReceiptErr(cause))
}

// receiptLogID makes a gc-supplied receipt id safe for a log line.
func receiptLogID(id string) string {
	return sanitizeReceiptLogValue(id, receiptLogValueLimit)
}

// describeReceiptErr bounds an error for a log line. Errors here can carry
// gc's response body (a rejected re-post) or a receipt id; both come from
// another process and must not forge a line or flood the log.
func describeReceiptErr(err error) string {
	if err == nil {
		return "-"
	}
	return sanitizeReceiptLogValue(err.Error(), receiptFollowUpLogLimit)
}

// landedUnconfirmed reports whether a CONCLUDED receipt describes the
// gp-2io shape: gc finished trying, and what it has to say is that every
// member either took the complete payload (delivered, counts whole) or
// had it pasted whole with only the submit unconfirmed — which gc
// classifies as status pending WITH complete per-member byte counts
// (gascity gp-2io: landed+unconfirmed is "not yet", never "failed").
// That is the busy-session case the pending=HOLD ruling protects: the
// payload is in the pane, so a re-post duplicates it, and a dead letter
// would cry wolf for every fast turn the busy probe missed — the same
// false alarm the 2026-08-28 08:24Z incident turned into six duplicates.
//
// Positive proof on every operand: at least one member, and every member
// individually accounted for. A member with an unrecognized status, or a
// pending member without complete byte evidence, makes this NOT the
// shape — the caller falls through to the deadline (STUCK, recorded),
// never to a re-post.
func (r deliveryReceipt) landedUnconfirmed() bool {
	if len(r.members) == 0 {
		return false
	}
	// The summary must be one gc actually produces for this shape:
	// pending (nothing vouched yet, a member landed-unconfirmed) or
	// partial (a mixed room folds delivered+pending to partial). A
	// contradictory summary — failed, no_route, or a status this
	// adapter has never heard of — is not the gp-2io story, and
	// per-member counts must not outvote it (codex final round P2 #1).
	switch normalizeReceiptStatus(r.status) {
	case "pending", "partial":
	default:
		return false
	}
	pending := 0
	for _, m := range r.members {
		switch {
		case receiptStatusPositive(m.Status) && !m.shortfall():
			// took the complete payload
		case receiptStatusPending(m.Status) && m.landedWhole():
			pending++
		default:
			return false
		}
	}
	return pending > 0
}

// landedWhole is positive proof that THIS member's complete payload
// reached its pane: both counts present, BOTH in bytes — the unit the
// gp-2io contract emits (DeliveredBytes/ExpectedBytes) — expected
// non-zero, delivered covering it. Absent or unparseable counts are not
// evidence (a pre-gp-2io gc, or drift, must not read as "landed"), and
// neither is a producer counting chars, runes or an unlabeled unit:
// that producer is outside the contract this proof is about (codex
// final round P2 #1).
func (m deliveryReceiptMember) landedWhole() bool {
	return m.DeliveredOK && m.ExpectedOK &&
		m.DeliveredUnit == "bytes" && m.ExpectedUnit == "bytes" &&
		m.Expected > 0 && m.Delivered >= m.Expected
}

// --- wire ---------------------------------------------------------------------

// pollInboundReceipt asks gc what became of one receipt. A transport error
// is returned as such (the caller retries); every HTTP answer is folded
// into a receiptPollState by parseReceiptPoll.
func pollInboundReceipt(ctx context.Context, cfg config, receiptID string) (receiptPollState, deliveryReceipt, error) {
	target := fmt.Sprintf("%s/v0/city/%s/extmsg/inbound/receipts/%s",
		cfg.gcAPIBase, url.PathEscape(cfg.cityName), url.PathEscape(receiptID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return receiptPollUnavailable, deliveryReceipt{}, err
	}
	req.Header.Set("X-GC-Request", "gc-slack-adapter")
	resp, err := gcForwardClient.Do(req)
	if err != nil {
		return receiptPollUnavailable, deliveryReceipt{}, err
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, deliveryReceiptBodyLimit))
	if readErr != nil {
		return receiptPollUnavailable, deliveryReceipt{}, readErr
	}
	state, receipt := parseReceiptPoll(receiptID, resp.StatusCode, body)
	return state, receipt, nil
}

// parseReceiptPoll folds gc's answer into a state. The agreed shape
// (gascity gp-3yg, extmsg.InboundReceiptStatus):
//
//	{"receipt_id": "ir-…", "state": "pending" | "concluded" | "unknown",
//	 "delivery": {…the same block the inbound response carries…}}
//
// Every state is a 200 on purpose: "unknown" must be distinguishable from
// "no such route" (404 on a gc that predates the endpoint), because the
// two demand opposite actions. Anything not understood — a non-200, a
// non-JSON body, a missing or unrecognized state, a concluded answer with
// no readable delivery block — is unavailable, the fail-open arm, for the
// same reason the synchronous verdict fails open on drift: misreading a
// producer's schema change as "lost" would re-post every held delivery in
// the workspace.
//
// The answer must also be ABOUT want: the top-level receipt_id, and for a
// concluded answer the delivery block's own id, must both name it. A
// stale, cached or mis-routed answer for another receipt reads as
// unavailable rather than as a verdict on this one (codex r1 P1 #1).
func parseReceiptPoll(want string, status int, body []byte) (receiptPollState, deliveryReceipt) {
	if status != http.StatusOK {
		return receiptPollUnavailable, deliveryReceipt{}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return receiptPollUnavailable, deliveryReceipt{}
	}
	want = strings.TrimSpace(want)
	if want == "" || receiptPollString(top, "receiptid") != want {
		return receiptPollUnavailable, deliveryReceipt{}
	}
	switch normalizeReceiptStatus(receiptPollString(top, "state")) {
	case "pending":
		return receiptPollPending, deliveryReceipt{}
	case "unknown":
		return receiptPollUnknown, deliveryReceipt{}
	case "concluded":
		receipt := parseDeliveryReceipt(body)
		if !receipt.present || strings.TrimSpace(receipt.id) != want {
			return receiptPollUnavailable, deliveryReceipt{}
		}
		return receiptPollConcluded, receipt
	}
	return receiptPollUnavailable, deliveryReceipt{}
}

// receiptPollString reads one top-level string field by normalized key,
// trimmed; "" when absent, ambiguous, or not a string.
func receiptPollString(top map[string]json.RawMessage, key string) string {
	raw, ok := lookupNormalized(top, key)
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// receiptFollowUpsFor wires the component to gc for a live config.
func receiptFollowUpsFor(cfg config) *receiptFollowUps {
	return newReceiptFollowUps(func(ctx context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		return pollInboundReceipt(ctx, cfg, id)
	}, cfg.deliveryReceiptGate, cfg.draining)
}

// deadLetterHeldAliasLoss records a lost address-by-handle injection
// with what a hand recovery needs — the aliased session, the handle and
// the exact rendered reminder — alongside the channel envelope it came
// from. Written directly: the coalescer's hook is shaped for channel
// inbounds, and replaying this record as one would route to the
// channel-bound session, not to the session it was addressed to.
func deadLetterHeldAliasLoss(cfg config, inbound externalInboundMessage, sessionID, handle, body string, cause error) bool {
	if strings.TrimSpace(cfg.inboundDeadLetterDir) == "" {
		return false
	}
	channel := inbound.Conversation.ConversationID
	path, err := writeAliasDeadLetter(cfg.inboundDeadLetterDir, channel, inbound, sessionID, handle, body, cause)
	if err != nil {
		log.Printf("receipt follow-up: chan=%s alias dead-letter write FAILED (dir %q): %v", channel, cfg.inboundDeadLetterDir, err)
		return false
	}
	log.Printf("receipt follow-up: chan=%s lost injection for session=%s handle=%s written to dead-letter file %s — re-inject alias_body by hand",
		channel, sanitizeReceiptLogValue(sessionID, receiptLogValueLimit), sanitizeReceiptLogValue(handle, receiptLogValueLimit), path)
	return true
}

// deadLetterHeldLoss writes the envelope(s) behind a lost hold to the
// dead-letter file, through the coalescer's hook when one is wired (it
// owns the log line naming the path) and directly otherwise. False when
// nothing durable happened.
func deadLetterHeldLoss(cfg config, channel string, batch []pendingChannelInbound, cause error) bool {
	if cfg.coalescer != nil && cfg.coalescer.deadLetter != nil {
		return cfg.coalescer.deadLetter(channel, batch, cause)
	}
	if strings.TrimSpace(cfg.inboundDeadLetterDir) == "" {
		return false
	}
	path, err := writeInboundDeadLetter(cfg.inboundDeadLetterDir, channel, batch, cause)
	if err != nil {
		log.Printf("receipt follow-up: chan=%s dead-letter write FAILED (dir %q): %v", channel, cfg.inboundDeadLetterDir, err)
		return false
	}
	log.Printf("receipt follow-up: chan=%s %d message(s) written to dead-letter file %s — inspect and re-post by hand",
		channel, len(batch), path)
	return true
}
