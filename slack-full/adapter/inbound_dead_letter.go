package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// --- inbound delivery failure classes + dead letters (gp-xnc) ---------------

// inboundPostError is a non-2xx response from gc's extmsg inbound
// endpoint. It carries the status so the coalescer can tell a
// deterministic payload rejection from everything else; its text keeps
// the historical "<status line>: <body>" form operators grep for.
type inboundPostError struct {
	Status     int
	StatusText string
	Body       string
}

func (e *inboundPostError) Error() string {
	return fmt.Sprintf("%s: %s", e.StatusText, e.Body)
}

// permanentDeliveryFailure reports whether an inbound delivery error is
// a PAYLOAD rejection — gc looked at the message and will never accept
// this exact payload: 400 (malformed), 413 (too large), 415 (media
// type), 422 (validation, the gp-xnc incident). Only those dead-letter.
// Every other outcome keeps the coalescer's retry-forever durability:
// network errors and 5xx (gc restarting), 408/429 (transient by
// definition), AND operational 4xx such as 401/403/404/405 (wrong city
// name, a route missing mid-rollout, auth) — those are deployment
// problems an operator fixes in place, and dead-lettering every
// buffered message after three windows would turn a config mistake
// into a recovery chore (codex r1 finding 4).
func permanentDeliveryFailure(err error) bool {
	var pe *inboundPostError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// submittedDeliveryError is the coalesced-delivery error as
// deliverCoalescedBatch reports it: the cause, plus the entries it
// ACTUALLY POSTed. The hook drops same-ts duplicates and members an
// urgent twin already delivered BEFORE posting, so the batch the
// coalescer handed it and the payload gc refused can differ — and the
// rejection ladder must judge the latter (codex r5 finding 1: a
// two-entry segment collapsed to one refused single must be charged
// as a single, never "isolated" by re-posting that single unchanged).
// errors.Is / errors.As see through it to the cause.
type submittedDeliveryError struct {
	submitted []pendingChannelInbound
	err       error
}

func (e *submittedDeliveryError) Error() string { return e.err.Error() }
func (e *submittedDeliveryError) Unwrap() error { return e.err }

// submittedOf returns the entries the delivery hook actually posted for
// a failed segment: the hook's report when it made one, else the whole
// segment (test hooks post what they are given).
func submittedOf(seg []pendingChannelInbound, err error) []pendingChannelInbound {
	var se *submittedDeliveryError
	if errors.As(err, &se) {
		return se.submitted
	}
	return seg
}

// --- the rejection ladder (gp-sgu7) -------------------------------------------
//
// ONE decision point for a message gc refused (permanentDeliveryFailure)
// or accepted without vouching for (errDeliveryUnvouched, gp-32q):
// nextRejectionStep. Every consumer — charge() is the only one — acts on
// its verdict; nothing else may decide whether an entry is retried.
//
// A 4xx payload refusal is deterministic: the same bytes get the same
// answer, so the entry is NEVER re-posted as-is (the 2026-09-08 incident
// re-posted one refused batch 34,000 times). The one retry allowed is a
// DIFFERENT payload: the attachments stripped and their local paths
// named in the text, because an attachment gc will not validate (a
// missing mime_type, a URL shape, a size) must not cost the founder's
// words; a second refusal, or a refusal with nothing left to strip,
// dead-letters at once. The unvouched class keeps gp-32q's bounded
// same-payload ladder (maxCoalesceDeliveryAttempts): the payload was
// accepted, only the vouch was missing, so re-posting it can succeed.

type rejectionStep int

const (
	// stepRetryWithoutAttachments: 4xx refusal, attachments present —
	// withholdAttachments, then restore for the next window.
	stepRetryWithoutAttachments rejectionStep = iota
	// stepDeadLetter: hand the entry to the dead-letter hook now.
	stepDeadLetter
	// stepRetrySame: unvouched under the cap — restore unchanged.
	stepRetrySame
)

func (s rejectionStep) String() string {
	switch s {
	case stepRetryWithoutAttachments:
		return "retry-without-attachments"
	case stepDeadLetter:
		return "dead-letter"
	case stepRetrySame:
		return "retry-same"
	}
	return fmt.Sprintf("rejectionStep(%d)", int(s))
}

// nextRejectionStep is the ladder. p.attempts counts the charges the
// entry has ALREADY taken (the caller increments after asking).
func nextRejectionStep(p pendingChannelInbound, cause error) rejectionStep {
	if permanentDeliveryFailure(cause) {
		if len(p.inbound.Attachments) > 0 {
			return stepRetryWithoutAttachments
		}
		return stepDeadLetter
	}
	if p.attempts+1 < maxCoalesceDeliveryAttempts {
		return stepRetrySame
	}
	return stepDeadLetter
}

// rejectionStatusLine is the short, operator-readable cause carried into
// the text: the HTTP status line for a gc refusal (its JSON body can be
// hundreds of bytes and quotes the file path itself), a bounded prefix
// of any other error.
func rejectionStatusLine(cause error) string {
	var pe *inboundPostError
	if errors.As(cause, &pe) {
		if pe.StatusText != "" {
			return pe.StatusText
		}
		return fmt.Sprintf("HTTP %d", pe.Status)
	}
	if cause == nil {
		return "refused"
	}
	s := cause.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// withholdAttachments returns p with its attachments removed and a
// notice naming their local paths and the refusal appended to the
// message unit — always into the files part (so the head-protected
// composer keeps it: the files block is never shed) with Text
// re-folded; a partless legacy/spool-replayed entry is promoted to
// parts first. The files themselves stay where downloadSlackFiles put
// them. A p with no attachments is returned unchanged, so a repeat
// call adds nothing.
func withholdAttachments(p pendingChannelInbound, cause error) pendingChannelInbound {
	// The disposition travels on the entry: this copy IS the stripped
	// retry, whatever the ledger remembers after a restart.
	p.stripped = truncateReason(rejectionReasonText(cause))
	if len(p.inbound.Attachments) == 0 {
		return p
	}
	paths := make([]string, 0, len(p.inbound.Attachments))
	for _, a := range p.inbound.Attachments {
		paths = append(paths, neutralizeMarkupBoundaries(strings.TrimPrefix(a.URL, "file://")))
	}
	noun := "attachments"
	if len(paths) == 1 {
		noun = "attachment"
	}
	notice := fmt.Sprintf("[%d %s withheld — gc refused this message with them attached (%s); the files remain at %s]",
		len(paths), noun, neutralizeMarkupBoundaries(rejectionStatusLine(cause)), strings.Join(paths, ", "))
	p.inbound.Attachments = nil
	if !p.hasReminderParts() {
		// A partless legacy/spool-replayed entry is PROMOTED to parts
		// here: its text becomes the body, and a thread anchor is
		// synthesized exactly as the single-entry composer's legacy
		// fallback would. Appending the notice to the flat Text instead
		// would put it at the TAIL of the body, which the head-protected
		// composer trims first on a long message — losing the paths
		// (codex r1 finding 4). In the files part it is never shed.
		if rt := p.inbound.ReplyToMessageID; !p.reaction && rt != "" && rt != p.inbound.ProviderMessageID {
			p.threadAnchor = formatThreadReplyAnchor(rt, "", "")
		}
		p.body = strings.TrimRight(p.inbound.Text, "\n")
	}
	if p.files == "" {
		p.files = notice
	} else {
		p.files += "\n" + notice
	}
	p.inbound.Text = p.foldedText()
	return p
}

// --- dead-letter writes that did not confirm (gp-sgu7, codex r1 finding 1) --
//
// The ladder's verdict (dead-letter) is final: once gc has refused the
// entry's bytes the entry NEVER re-enters the delivery path. When the
// dead-letter hook cannot confirm the write (disk full, a planted
// symlink, a bad override), the entry is PARKED here — held in memory,
// out of the pending maps — and only the WRITE retries, on the same
// doubling backoff as transient deliveries. A restart loses parked
// entries like any in-memory state, so the shutdown drain gives the
// write one last try and spools what still fails WITH their refusal
// (pendingChannelInbound.refused); the replay after restart parks such
// an entry straight back for the write — it is never re-posted.

// parkedDeadLetter is one entry awaiting its dead-letter write, with
// the rejection that retired it (the record's reason).
type parkedDeadLetter struct {
	p     pendingChannelInbound
	cause error
	// spilled: the entry's Refused spool line is on disk — the payload's
	// durable home until the dead-letter write confirms (codex r20
	// finding 1); the shutdown flush spills only what is not.
	spilled bool
}

// deadLetterRetryBase is the first retry delay for a failed dead-letter
// write on a channel: the coalesce window, or one second when
// coalescing is disabled (the coalescer then only sees spool replays).
func (c *inboundCoalescer) deadLetterRetryBase() time.Duration {
	if c.window > 0 {
		return c.window
	}
	return disabledWindowRetryBase
}

// armDeadLetterRetryLocked arms the channel's write-retry timer if none
// is armed, at the backoff for its current run of write failures (at
// least one). Caller holds c.mu. Returns the delay and whether the
// timer was armed by this call.
func (c *inboundCoalescer) armDeadLetterRetryLocked(channel string) (time.Duration, bool) {
	if _, armed := c.deadLetterTimers[channel]; armed || c.closedForWrites {
		return 0, false
	}
	n := c.deadLetterWriteFailures[channel]
	if n < 1 {
		n = 1
	}
	delay := transientRetryDelay(c.deadLetterRetryBase(), n)
	c.deadLetterTimers[channel] = time.AfterFunc(delay, func() { c.retryParkedDeadLetters(channel) })
	return delay, true
}

// parkDeadLetter takes an entry whose dead-letter write the hook did
// not confirm OUT of the delivery path and schedules the write's
// retry. Called by charge() with the channel's delivery mutex held and
// c.mu NOT held, and by the spool replay for a Refused entry. After the
// shutdown drain has flushed the parked entries (c.closed) a straggler
// goes straight to the spool — nothing may sit in memory past the
// final snapshot.
//
// Returns whether the parked payload is durable on disk (its Refused
// spool line, written here; true for a duplicate already parked and for
// a post-close straggler the spool took). The replay keeps its staged
// file on false (codex r14 finding 3, r20 finding 1): the staged line
// was the payload's only durable copy.
func (c *inboundCoalescer) parkDeadLetter(channel string, p pendingChannelInbound, cause error) bool {
	// The terminal disposition travels WITH the entry: any spill from
	// here on (post-close straggler, the shutdown backstop) writes a
	// spool line the replay parks straight back into the write retry.
	p.refused = truncateReason(rejectionReasonText(cause))
	// The park IS the ladder's retirement of this message, whether
	// charge() just decided it (recorded and persisted there already —
	// the second record is idempotent) or the spool replay is re-parking
	// a retirement decided before a restart (codex r11 finding 4: the
	// ledger is memory, and without the verdict a redelivery admitted
	// after the restart entered delivery as a plain copy and posted the
	// refused bytes). A park from the replay persists it too, so the
	// verdict outlives the write's eventual success (codex r12).
	// No record yet (codex r20 finding 1): the verdict stands in memory
	// with its write pending, and the record is written by
	// deadLetterWritten once the payload is in the dead-letter file. A
	// record already standing (the write confirmed before a restart) is
	// left as it is.
	verdict := ladderVerdict{retired: true, cause: cause, pendingWrite: true}
	ts := p.inbound.ProviderMessageID
	c.mu.Lock()
	c.retireLocked(channel, ts, verdict, time.Now())
	if c.closed {
		c.spillLateLocked(channel, []pendingChannelInbound{p})
		return true
	}
	for _, e := range c.parkedDeadLetters[channel] {
		if e.p.inbound.ProviderMessageID == ts {
			// Already parked for its write (a duplicate Refused line in
			// one replay): one record per message in the dead-letter
			// file, not one per copy.
			c.mu.Unlock()
			log.Printf("coalesce: chan=%s ts=%s already parked for its dead-letter write — duplicate copy dropped", channel, ts)
			return true
		}
	}
	spill := c.spill
	c.mu.Unlock()
	// The payload's durable home while the write is owed is its Refused
	// spool line, written NOW — not at shutdown: a crash before then
	// would lose the only copy of an acknowledged message (codex r20
	// finding 1). The next startup re-parks it (or drops it once the
	// record says the write confirmed). Returns whether that copy is on
	// disk — the replay keeps its staged file otherwise.
	durable := spill != nil && spill(channel, []pendingChannelInbound{p})
	if !durable {
		log.Printf("coalesce: chan=%s ts=%s the refused message could not be spooled while its dead-letter write is owed — it lives in memory only until the write or the shutdown spill succeeds", channel, ts)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return durable
	}
	c.parkedDeadLetters[channel] = append(c.parkedDeadLetters[channel], parkedDeadLetter{p: p, cause: cause, spilled: durable})
	parked := len(c.parkedDeadLetters[channel])
	if c.deadLetterWriteFailures[channel] < 1 {
		c.deadLetterWriteFailures[channel] = 1
	}
	delay, armed := c.armDeadLetterRetryLocked(channel)
	c.mu.Unlock()
	if armed {
		log.Printf("coalesce: chan=%s ts=%s dead-letter write NOT confirmed — parked out of the delivery path (%d parked; never re-posted to gc); the write retries in %s",
			channel, ts, parked, delay)
		return durable
	}
	log.Printf("coalesce: chan=%s ts=%s dead-letter write NOT confirmed — parked out of the delivery path (%d parked; never re-posted to gc); a write retry is already scheduled",
		channel, ts, parked)
	return durable
}

// retryParkedDeadLetters is the write-retry timer callback: every
// parked entry of the channel is offered to the hook again; the ones
// it confirms retire, the rest stay parked and the timer re-arms on
// the next backoff step. Serialized by deadLetterMu against the
// shutdown flush so an entry is written at most once.
func (c *inboundCoalescer) retryParkedDeadLetters(channel string) {
	c.deadLetterMu.Lock()
	defer c.deadLetterMu.Unlock()
	c.mu.Lock()
	snapshot := append([]parkedDeadLetter(nil), c.parkedDeadLetters[channel]...)
	hook := c.deadLetter
	c.mu.Unlock()
	var kept []parkedDeadLetter
	for _, e := range snapshot {
		if hook == nil {
			// No sink wired: the entry stays parked (never re-posted) and
			// the owed verdict records below still get their retry.
			kept = append(kept, e)
			continue
		}
		// A deletion that landed while the write was owed must reach the
		// record as its notice, not the deleted text (codex r9 finding
		// 3) — the same tombstone check charge() made before the first
		// attempt.
		single := []pendingChannelInbound{e.p}
		c.applyDeletionTombstones(channel, single)
		e.p = single[0]
		if hook(channel, single, e.cause) {
			c.deadLetterWritten(channel, e.p.inbound.ProviderMessageID) // the payload is in the file: the record follows
			continue
		}
		kept = append(kept, e)
	}
	// The same timer retries the channel's owed verdict records (codex
	// r15 finding 2): the timer entry still stands here, so a failure
	// inside persistVerdict re-arms nothing — the tail below decides.
	c.persistOwedVerdicts(channel)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Recounted UNDER the lock, not the pass's own tally (codex r16
	// finding 2): a message retired while the pass ran, whose record
	// write failed, saw this timer armed and armed nothing — the tail
	// must see it or the record waits for the next failure or shutdown.
	owed := c.owedVerdictsLocked(channel)
	// Entries parked while the hook ran were appended after the
	// snapshot (appends happen only under c.mu, removals only here and
	// in the shutdown flush, which deadLetterMu excludes).
	newer := c.parkedDeadLetters[channel][len(snapshot):]
	remaining := append(kept, newer...)
	delete(c.deadLetterTimers, channel)
	written := len(snapshot) - len(kept)
	if len(remaining) == 0 && owed == 0 {
		delete(c.parkedDeadLetters, channel)
		n := c.deadLetterWriteFailures[channel]
		delete(c.deadLetterWriteFailures, channel)
		log.Printf("coalesce: chan=%s durable writes caught up after %d failed attempt(s): %d parked entr%s dead-lettered, every owed verdict record written", channel, n, written, plural(written, "y", "ies"))
		return
	}
	if len(remaining) == 0 {
		delete(c.parkedDeadLetters, channel)
	} else {
		c.parkedDeadLetters[channel] = remaining
	}
	c.deadLetterWriteFailures[channel]++
	n := c.deadLetterWriteFailures[channel]
	delay, _ := c.armDeadLetterRetryLocked(channel)
	if _, worthy := backoffLogWorthy(c.deadLetterRetryBase(), n); worthy || written > 0 {
		log.Printf("coalesce: chan=%s durable writes still failing (%d written, %d parked, %d verdict record(s) owed) after attempt #%d — next retry in %s%s",
			channel, written, len(remaining), owed, n, delay, backoffCapSuffix(delay))
	}
}

// flushParkedDeadLetters is the shutdown backstop, run by flushAll after
// the pending drain: every parked entry gets one more write; what still
// fails is spooled for startup replay (or logged LOST without a spool).
func (c *inboundCoalescer) flushParkedDeadLetters() {
	c.deadLetterMu.Lock()
	defer c.deadLetterMu.Unlock()
	c.mu.Lock()
	parked := c.parkedDeadLetters
	c.parkedDeadLetters = make(map[string][]parkedDeadLetter)
	for channel, t := range c.deadLetterTimers {
		t.Stop()
		delete(c.deadLetterTimers, channel)
	}
	c.closedForWrites = true
	hook, spill := c.deadLetter, c.spill
	c.mu.Unlock()
	channels := make([]string, 0, len(parked))
	for channel := range parked {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	for _, channel := range channels {
		var failed []pendingChannelInbound
		for _, e := range parked[channel] {
			single := []pendingChannelInbound{e.p}
			c.applyDeletionTombstones(channel, single) // codex r9 finding 3; see retryParkedDeadLetters
			if hook != nil && hook(channel, single, e.cause) {
				c.deadLetterWritten(channel, e.p.inbound.ProviderMessageID)
				continue
			}
			if e.spilled {
				continue // its Refused line is already on disk (parked with a spill, codex r20 finding 1)
			}
			failed = append(failed, single[0])
		}
		if len(failed) == 0 {
			continue
		}
		if spill == nil || !spill(channel, failed) {
			log.Printf("coalesce: SHUTDOWN LOSS chan=%s %d entr%s awaiting a dead-letter write could not be written or spooled — LOST (already acked to Slack)",
				channel, len(failed), plural(len(failed), "y", "ies"))
			continue
		}
		log.Printf("coalesce: shutdown chan=%s %d entr%s awaiting a dead-letter write spooled with their refusal — the startup replay parks them for the write, never re-posts them",
			channel, len(failed), plural(len(failed), "y", "ies"))
	}
	// Owed verdict records get their last try too (codex r15 finding 2);
	// what still fails is loss of the verdict, said so.
	for _, channel := range c.channelsOwingVerdicts() {
		if left := c.persistOwedVerdicts(channel); left > 0 {
			log.Printf("coalesce: SHUTDOWN chan=%s %d terminal verdict record(s) could not be written — after the restart a delayed redelivery of each may be admitted once as fresh bytes (then refused and dead-lettered again)", channel, left)
		}
	}
}

// rejectionReasonText is the cause as the dead-letter record and the
// spool carry it.
func rejectionReasonText(cause error) string {
	if cause == nil {
		return "refused"
	}
	return cause.Error()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// --- one log line per distinct failure (gp-sgu7 contract 3) ---------------

// repeatedFailureLog remembers, per channel, the text of the last
// delivery failure logged, so a standing condition (an outage, a
// refusal) logs once and again only when the failure CHANGES; a
// success clears the channel. Nil-safe: a nil receiver reports every
// failure as changed.
type repeatedFailureLog struct {
	mu   sync.Mutex
	last map[string]string
}

func newRepeatedFailureLog() *repeatedFailureLog {
	return &repeatedFailureLog{last: make(map[string]string)}
}

// changed records text as the channel's latest failure and reports
// whether it differs from the one recorded before (true on the first).
func (r *repeatedFailureLog) changed(channel, text string) bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, seen := r.last[channel]
	r.last[channel] = text
	return !seen || prev != text
}

// clear forgets the channel's last failure (its next one logs again).
func (r *repeatedFailureLog) clear(channel string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.last, channel)
}

// inboundDeadLetterRecord is one JSONL line in a channel's dead-letter
// file: the inbound envelope exactly as the adapter tried to POST it
// (the per-message envelope, without the coalesced wrapper text), so an
// operator can inspect the message and re-post it by hand once the
// rejection cause is fixed.
type inboundDeadLetterRecord struct {
	DeadLetteredAt time.Time              `json:"dead_lettered_at"`
	Channel        string                 `json:"channel"`
	Attempts       int                    `json:"attempts"`
	Reason         string                 `json:"reason"`
	Inbound        externalInboundMessage `json:"inbound"`
}

// maxDeadLetterReasonBytes bounds the stored error text INCLUDING the
// truncation marker — a gc validation body is a few hundred bytes;
// anything larger is cut on a rune boundary.
const maxDeadLetterReasonBytes = 4096

const deadLetterTruncMarker = "…(truncated)"

// truncateReason bounds s to maxDeadLetterReasonBytes without splitting
// a multi-byte rune, reserving room for the marker (codex r1 finding 6).
func truncateReason(s string) string {
	if len(s) <= maxDeadLetterReasonBytes {
		return s
	}
	cut := maxDeadLetterReasonBytes - len(deadLetterTruncMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + deadLetterTruncMarker
}

// writeInboundDeadLetter appends one record per message to
// <dir>/<channel>.jsonl (channel sanitized exactly like the inbound file
// store's path components) and returns the file path. It reports
// success ONLY once the bytes are fsynced and the file is closed —
// the coalescer keeps the entry buffered on any error (codex r1
// finding 3). Store discipline matches inboundFileStore (gc-ywe.6)
// and is ENFORCED, not just requested at create time: the directory is
// chmod'd 0700 even when pre-existing, the file is opened O_NOFOLLOW
// (a planted symlink at <channel>.jsonl fails with ELOOP instead of
// redirecting the append), must be a regular file, and is chmod'd 0600
// (codex r1 finding 5).
func writeInboundDeadLetter(dir, channel string, batch []pendingChannelInbound, cause error) (string, error) {
	created, err := missingAncestors(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("enforce 0700 on %q: %w", dir, err)
	}
	path := filepath.Join(dir, safePathComponent(channel)+".jsonl")
	// O_NONBLOCK: a planted FIFO would otherwise block the open until a
	// reader appears — under the channel flush mutex (codex r2 finding
	// 1). Regular files ignore it; it is cleared again before Go's
	// runtime sees the fd so the *os.File behaves like any other.
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return "", fmt.Errorf("open %q (no-follow, non-blocking): %w", path, err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		return "", fmt.Errorf("clear O_NONBLOCK on %q: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return "", fmt.Errorf("dead-letter file %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("enforce 0600 on %q: %w", path, err)
		}
	}
	reason := ""
	if cause != nil {
		reason = truncateReason(cause.Error())
	}
	now := time.Now().UTC()
	enc := json.NewEncoder(f)
	for _, p := range batch {
		if err := enc.Encode(inboundDeadLetterRecord{
			DeadLetteredAt: now,
			Channel:        channel,
			Attempts:       p.attempts,
			Reason:         reason,
			Inbound:        p.inbound,
		}); err != nil {
			_ = f.Close()
			return path, err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return path, fmt.Errorf("fsync %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return path, err
	}
	// Durability of the directory ENTRIES (codex r2 finding 2, r3
	// finding 1): fsync of the file alone does not persist a newly
	// created name; a power loss after the hook returned true could
	// otherwise erase the record the coalescer just retired. dir and
	// its parent are synced UNCONDITIONALLY on every successful write —
	// not only when this call created dir — so a retry after an attempt
	// that created the directory but failed before its sync still
	// confirms the entry. Every deeper ancestor this call created is
	// synced too. main() pre-creates dir at startup, so at write time
	// the chain is normally already there; the residual gap (a ≥2-level
	// chain created at write time by an attempt that failed mid-sync,
	// then retried) is accepted.
	for _, d := range created {
		if d == dir || d == filepath.Dir(dir) {
			continue
		}
		if err := syncDir(d); err != nil {
			return path, err
		}
	}
	if err := syncDir(dir); err != nil {
		return path, err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return path, err
	}
	return path, nil
}

// missingAncestors lists dir and every ancestor that does not exist
// yet, deepest first, followed by the nearest EXISTING ancestor (whose
// entry for the first created directory must be synced too). A Stat
// error other than not-exist is returned as-is.
func missingAncestors(dir string) ([]string, error) {
	var out []string
	d := dir
	for {
		_, err := os.Stat(d)
		if err == nil {
			if len(out) > 0 {
				out = append(out, d)
			}
			return out, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat %q: %w", d, err)
		}
		out = append(out, d)
		parent := filepath.Dir(d)
		if parent == d {
			return nil, fmt.Errorf("no existing ancestor for %q", dir)
		}
		d = parent
	}
}

// syncDir fsyncs a directory so entries created in it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %q for fsync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("fsync dir %q: %w", dir, err)
	}
	return d.Close()
}
