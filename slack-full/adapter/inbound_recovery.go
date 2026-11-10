package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Unvouched deliveries are durable holds, separate from payload rejections and
// from the channel's normal buffer. A busy composer must not block new traffic.
var inboundHoldBackoff = []time.Duration{20 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}

func retryLadderDelay(ladder []time.Duration, attempt int, ceiling time.Duration) time.Duration {
	if attempt >= len(ladder) {
		attempt = len(ladder) - 1
	}
	d := ladder[attempt]
	if ceiling > 0 && d > ceiling {
		d = ceiling
	}
	return d
}

func installInboundRecovery(cfg *config) error {
	r, err := newInboundRecovery(filepath.Join(companyStateDirDefault(cfg.cityPath, "inbound-holds"), "state.json"), cfg.inboundDeadLetterDir)
	if err != nil {
		return err
	}
	for name, dst := range map[string]*time.Duration{"SLACK_INBOUND_HOLD_CEILING": &r.ceiling, "SLACK_INBOUND_HOLD_BOUND": &r.bound} {
		if raw := strings.TrimSpace(os.Getenv(name)); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return fmt.Errorf("%s must be a positive duration", name)
			}
			*dst = d
		}
	}
	if err := r.importDeadLetters(); err != nil {
		return err
	}
	if cfg.inboundJournal != nil {
		if err := r.enrichFromJournal(cfg.inboundJournal.dir); err != nil {
			log.Printf("inbound replay: legacy receipt provenance incomplete: %v", err)
		}
	}
	cfg.inboundRecovery, cfg.coalescer.recovery = r, r
	r.coalescer = cfg.coalescer
	r.gated = cfg.deliveryReceiptGate
	if cfg.inboundLiveness != nil {
		cfg.inboundLiveness.cfg.inboundRecovery = r
	}
	copyCfg := *cfg
	copyCfg.inboundRecovery, copyCfg.recoveryReplay = nil, true
	r.poll = func(ctx context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		return pollInboundReceipt(ctx, copyCfg, id)
	}
	r.journal = func(e inboundRecoveryEntry, receipt deliveryReceipt) {
		if e.Submission == nil {
			// Legacy records lack the submitted batch identity. Do not
			// invent a per-message payload hash for a batch receipt.
			if e.AliasSessionID == "" {
				return
			}
		} else {
			e.Inbound = e.Submission.message()
			if e.AliasSessionID != "" {
				e.AliasBody = e.Inbound.Text
			}
		}
		e.Inbound.originalReceiptID = e.OriginalReceiptID
		if e.AliasSessionID != "" {
			copyCfg.inboundJournal.recordDirect(copyCfg, e.Inbound, receipt, e.AliasSessionID, e.AliasBody, "concluded")
		} else {
			copyCfg.inboundJournal.record(copyCfg, e.Inbound, receipt, "concluded")
		}
	}
	r.finished = func(e inboundRecoveryEntry) {
		channel, ts := e.Inbound.Conversation.ConversationID, e.Inbound.ProviderMessageID
		if !e.DeliveredAt.IsZero() {
			copyCfg.deliveredIDs.record(e.AliasHandle, channel, ts)
		}
		if e.AliasSessionID != "" {
			return
		}
		// Isolation may already own a spooled copy when its receipt becomes
		// unvouched. Settle that ownership when recovery finishes it.
		c := copyCfg.coalescer
		c.mu.Lock()
		v, ok := c.verdictLocked(channel, ts, time.Now())
		if ok && !v.retired {
			v.retired, v.delivered, v.pendingWrite = true, !e.DeliveredAt.IsZero(), false
			c.retireLocked(channel, ts, v, time.Now())
		} else {
			ok = false
		}
		c.mu.Unlock()
		if ok {
			c.persistVerdict(channel, ts)
		}
	}
	r.send = func(e inboundRecoveryEntry) (deliveryReceipt, error) {
		copyCfg.coalescer.mu.Lock()
		if copyCfg.coalescer.closed || (copyCfg.draining != nil && copyCfg.draining.Load()) {
			copyCfg.coalescer.mu.Unlock()
			return deliveryReceipt{}, errors.New("recovery paused for shutdown")
		}
		copyCfg.coalescer.inflight++
		copyCfg.coalescer.mu.Unlock()
		defer copyCfg.coalescer.endDelivery()
		release := copyCfg.coalescer.holdDelivery(e.Inbound.Conversation.ConversationID)
		defer release()
		batch := []pendingChannelInbound{e.pending()}
		copyCfg.coalescer.applyDeletionTombstones(e.Inbound.Conversation.ConversationID, batch)
		e.Inbound = batch[0].inbound
		if e.Inbound.Text == deletedBySenderNotice && e.AliasBody != "" {
			e.AliasBody = deletedBySenderNotice
		}
		var receipt deliveryReceipt
		attemptCfg := copyCfg
		attemptCfg.receiptObserver = func(got deliveryReceipt) { receipt = got }
		// A prior pending receipt or terminal ladder record is not proof of
		// delivery. Recovery owns its durable message-ID dedup separately.
		attemptCfg.deliveredIDs = newDeliveredIDs()
		attemptCfg.channelClaims = newEventDedupCache(eventDedupTTL)
		attemptCfg.peerContext = newPeerContextBuffer()
		var err error
		if e.AliasSessionID != "" {
			var accepted bool
			receipt, accepted = sendAliasDispatch(attemptCfg, e.AliasSessionID, e.AliasHandle, e.AliasBody, e.Inbound)
			if !accepted {
				err = errors.New("alias redrive not accepted")
			}
		} else if e.Rendered {
			receipt, err = postInboundWithReceipt(attemptCfg, e.Inbound)
		} else {
			err = deliverCoalescedBatch(attemptCfg, e.Inbound.Conversation.ConversationID, batch)
		}
		return receipt, err
	}
	if !strings.EqualFold(os.Getenv("SLACK_INBOUND_HOLD_NOTICE"), "off") {
		r.notice = inboundHoldPoster(cfg.slackBotToken)
	}
	return nil
}

func (r *inboundRecovery) run(ctx context.Context, cfg config) {
	r.stopping = func() bool { return ctx.Err() != nil || (cfg.draining != nil && cfg.draining.Load()) }
	r.tick(true)
	for waitUnlessDraining(ctx, cfg, time.Second, r.wake) {
		r.tick(false)
	}
}

type unvouchedDeliveryError struct{ receipt deliveryReceipt }

var errRecoveryOrder = errors.New("receipt resolved; new send waits for an older message")

func (e *unvouchedDeliveryError) Error() string { return errDeliveryUnvouched.Error() }
func (e *unvouchedDeliveryError) Unwrap() error { return errDeliveryUnvouched }

// Preserve the exact journal identity behind a receipt across restarts. The
// per-message recovery envelope is different from a rendered coalesced batch.
type recoverySubmission struct {
	TrustedFilePaths []string               `json:"trusted_file_paths,omitempty"`
	Inbound          externalInboundMessage `json:"inbound"`
	Windows          []injectionWindow      `json:"windows,omitempty"`
	Direct           bool                   `json:"direct,omitempty"`
	Started          time.Time              `json:"started,omitempty"`
}

func captureRecoverySubmission(msg externalInboundMessage) *recoverySubmission {
	return &recoverySubmission{Inbound: msg, Windows: msg.injectionWindows, Direct: msg.injectionDirect, Started: msg.injectionStartedAt, TrustedFilePaths: append([]string(nil), msg.trustedFilePaths...)}
}

func (s *recoverySubmission) message() externalInboundMessage {
	msg := s.Inbound
	msg.injectionWindows, msg.injectionDirect, msg.injectionStartedAt = s.Windows, s.Direct, s.Started
	msg.trustedFilePaths = s.TrustedFilePaths
	return msg
}

type inboundRecoveryEntry struct {
	ManualRecovery    bool                   `json:"manual_recovery,omitempty"`
	TrustedFilePaths  []string               `json:"trusted_file_paths,omitempty"`
	Landed            bool                   `json:"landed,omitempty"`
	Parts             *recoveryParts         `json:"parts,omitempty"`
	LegacyUncertain   bool                   `json:"legacy_uncertain,omitempty"`
	Submission        *recoverySubmission    `json:"submission,omitempty"`
	ComposerHold      *deliveryComposerHold  `json:"composer_hold,omitempty"`
	AliasSessionID    string                 `json:"alias_session_id,omitempty"`
	AliasHandle       string                 `json:"alias_handle,omitempty"`
	AliasBody         string                 `json:"alias_body,omitempty"`
	Inbound           externalInboundMessage `json:"inbound"`
	Reaction          bool                   `json:"reaction,omitempty"`
	Rendered          bool                   `json:"rendered,omitempty"`
	OriginalReceiptID string                 `json:"original_receipt_id,omitempty"`
	PendingReceiptID  string                 `json:"pending_receipt_id,omitempty"`
	PendingReceipt    bool                   `json:"pending_receipt,omitempty"`
	TargetSessions    []string               `json:"target_sessions,omitempty"`
	HeldAt            time.Time              `json:"held_at"`
	NextAttemptAt     time.Time              `json:"next_attempt_at"`
	Attempts          int                    `json:"attempts"`
	DeadLetteredAt    time.Time              `json:"dead_lettered_at,omitempty"`
	DeliveredAt       time.Time              `json:"delivered_at,omitempty"`
	CompletionPending bool                   `json:"completion_pending,omitempty"`
	Retiring          bool                   `json:"retiring,omitempty"`
	ReplayNotBefore   time.Time              `json:"replay_not_before,omitempty"`
	Reason            string                 `json:"reason,omitempty"`
}

type inboundHoldNotice struct {
	TS       string    `json:"ts"`
	ThreadTS string    `json:"thread_ts"`
	Since    time.Time `json:"since"`
	Resolved bool      `json:"resolved,omitempty"`
}

type recoveryParts struct {
	Body         string            `json:"body,omitempty"`
	Files        string            `json:"files,omitempty"`
	Preamble     string            `json:"preamble,omitempty"`
	ThreadAnchor string            `json:"thread_anchor,omitempty"`
	PreambleLean string            `json:"preamble_lean,omitempty"`
	BotQuotes    bool              `json:"bot_quotes,omitempty"`
	Windows      []injectionWindow `json:"windows,omitempty"`
}

func captureRecoveryParts(p pendingChannelInbound) *recoveryParts {
	if !p.hasReminderParts() && p.threadAnchor == "" && len(p.inbound.injectionWindows) == 0 {
		return nil
	}
	clean := func(s string) string { return redactAttachmentText(s, p.inbound) }
	return &recoveryParts{Body: clean(p.body), Files: clean(p.files), Preamble: clean(p.preamble), ThreadAnchor: clean(p.threadAnchor), PreambleLean: clean(p.preambleLean), BotQuotes: p.botQuotes, Windows: p.inbound.injectionWindows}
}

func (e inboundRecoveryEntry) pending() pendingChannelInbound {
	p := pendingChannelInbound{inbound: e.Inbound, reaction: e.Reaction}
	p.inbound.trustedFilePaths = e.TrustedFilePaths
	if e.Parts != nil {
		p.body, p.files, p.preamble, p.threadAnchor = e.Parts.Body, e.Parts.Files, e.Parts.Preamble, e.Parts.ThreadAnchor
		p.preambleLean, p.botQuotes, p.inbound.injectionWindows = e.Parts.PreambleLean, e.Parts.BotQuotes, e.Parts.Windows
	}
	return p
}

func compactRecoveryEntry(e inboundRecoveryEntry) inboundRecoveryEntry {
	if e.DeliveredAt.IsZero() || e.CompletionPending {
		return e
	}
	// Keep durable message-ID dedup and notice timestamps, release payloads.
	e.Inbound = externalInboundMessage{ProviderMessageID: e.Inbound.ProviderMessageID, Conversation: e.Inbound.Conversation}
	e.Submission, e.Parts, e.ComposerHold = nil, nil, nil
	e.TrustedFilePaths = nil
	e.AliasBody, e.Reason = "", ""
	e.PendingReceiptID, e.PendingReceipt = "", false
	return e
}

type inboundRecoveryState struct {
	Entries map[string]inboundRecoveryEntry `json:"entries"`
	Notices map[string]inboundHoldNotice    `json:"notices"`
}

type inboundRecovery struct {
	coalescer *inboundCoalescer
	mu, runMu sync.Mutex
	state     inboundRecoveryState
	path      string
	deadDir   string
	ceiling   time.Duration
	bound     time.Duration
	gated     bool
	now       func() time.Time
	send      func(inboundRecoveryEntry) (deliveryReceipt, error)
	poll      receiptPollFunc
	notice    func(channel, thread, ts, text string) (string, error)
	journal   func(inboundRecoveryEntry, deliveryReceipt)
	finished  func(inboundRecoveryEntry)
	dirty     bool
	stopping  func() bool
	// One worker owns retries and redrives. Success observations only enqueue
	// session IDs; they never acquire a channel lock inside a delivery callback.
	wake     chan struct{}
	sessions map[string]bool
	manual   map[string]bool
}

func newInboundRecovery(path, deadDir string) (*inboundRecovery, error) {
	r := &inboundRecovery{path: path, deadDir: deadDir, ceiling: 15 * time.Minute,
		bound: 30 * time.Minute, gated: true, now: time.Now, wake: make(chan struct{}, 1),
		sessions: map[string]bool{}, manual: map[string]bool{},
		state: inboundRecoveryState{Entries: map[string]inboundRecoveryEntry{}, Notices: map[string]inboundHoldNotice{}}}
	if path != "" {
		created, err := missingAncestors(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, err
		}
		for _, dir := range created {
			if err := syncDir(dir); err != nil {
				return nil, err
			}
		}
		loaded, err := readRecoveryState(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if loaded != nil {
			r.state = *loaded
			if r.state.Entries == nil || r.state.Notices == nil {
				return nil, errors.New("invalid inbound hold state")
			}
			for key, e := range r.state.Entries {
				r.state.Entries[key] = compactRecoveryEntry(e)
			}
		}
	}
	return r, nil
}

func recoveryKey(msg externalInboundMessage) string {
	return originKey(msg.Conversation.ConversationID, msg.ProviderMessageID)
}

func readRecoveryState(path string) (*inboundRecoveryState, error) {
	f, err := openBeneathFlags(filepath.Dir(path), filepath.Base(path), unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("inbound recovery state is not a regular file")
	}
	var state inboundRecoveryState
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing data in inbound recovery state")
	}
	return &state, nil
}

func receiptSessions(receipt deliveryReceipt) []string {
	seen := map[string]bool{}
	for _, m := range receipt.members {
		if m.SessionID != "" {
			seen[m.SessionID] = true
		}
	}
	if receipt.targetSessionID != "" {
		seen[receipt.targetSessionID] = true
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (r *inboundRecovery) saveLocked() error {
	if r.path == "" {
		return nil
	}
	data, err := json.Marshal(r.state)
	if err != nil {
		return err
	}
	err = companyWriteFileAtomic(r.path, data)
	r.dirty = err != nil
	return err
}

func (r *inboundRecovery) known(channel, ts string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.state.Entries[originKey(channel, ts)]
	return ok
}

// A nil error confirms durability. On write failure recovery still owns the
// payload AND receipt in memory, and tick retries persistence independently.
func (r *inboundRecovery) hold(p pendingChannelInbound, receipt deliveryReceipt, rendered bool) error {
	return r.holdEntry(inboundRecoveryEntry{Inbound: redactInboundMessage(p.inbound), Parts: captureRecoveryParts(p), Reaction: p.reaction, Rendered: rendered}, receipt)
}

func (r *inboundRecovery) holdAlias(msg externalInboundMessage, session, handle, body string, receipt deliveryReceipt) error {
	return r.holdEntry(inboundRecoveryEntry{Inbound: redactInboundMessage(msg), AliasSessionID: session, AliasHandle: handle, AliasBody: body}, receipt)
}

func (r *inboundRecovery) knownAlias(msg externalInboundMessage, session string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.state.Entries[recoveryKey(msg)+"|alias:"+session]
	return ok
}

// Unvouched riders belong to the submitted envelope too. Even on a write
// failure they stay owned in memory, outside the volatile peer FIFO.
func (r *inboundRecovery) holdPendingPeers(items []peerContextItem, receipt deliveryReceipt) []peerContextItem {
	for _, item := range items {
		if err := r.hold(pendingChannelInbound{inbound: item.inbound, body: item.Text, files: item.Files}, receipt, false); err != nil {
			log.Printf("peer rider HOLD retained in memory; persistence retry needed: %v", err)
		}
	}
	return nil
}

func (r *inboundRecovery) holdEntry(e inboundRecoveryEntry, receipt deliveryReceipt) error {
	// Admission and message_deleted use c.mu -> r.mu. A deletion that raced
	// the HTTP send must reach the persisted retry envelope, even if there
	// was no recovery entry when the deletion event arrived.
	if c := r.coalescer; c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !e.Reaction && c.isDeletedLocked(e.Inbound.Conversation.ConversationID, e.Inbound.ProviderMessageID, time.Now()) {
			p := e.pending()
			applyDeletion(&p)
			e.Inbound, e.Parts = p.inbound, captureRecoveryParts(p)
			if e.AliasBody != "" {
				e.AliasBody = deletedBySenderNotice
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := recoveryKey(e.Inbound)
	if e.AliasSessionID != "" {
		key += "|alias:" + e.AliasSessionID
	}
	if _, ok := r.state.Entries[key]; ok {
		return r.saveLocked()
	}
	now := r.now()
	e.OriginalReceiptID, e.TargetSessions = receipt.id, receiptSessions(receipt)
	e.Submission, e.ComposerHold = receipt.submission, receipt.composerHold()
	e.TrustedFilePaths = append([]string(nil), e.Inbound.trustedFilePaths...)
	e.Landed = receipt.landedUnconfirmed()
	e.HeldAt, e.NextAttemptAt = now, now.Add(retryLadderDelay(inboundHoldBackoff, 0, r.ceiling))
	if deadline := now.Add(r.bound); e.NextAttemptAt.After(deadline) {
		e.NextAttemptAt = deadline
	}
	if receipt.verdict(r.gated) == receiptHeld {
		e.PendingReceiptID = receipt.id
		e.PendingReceipt = true
	}
	r.state.Entries[key] = e
	if err := r.saveLocked(); err != nil {
		r.dirty = true
		return err
	}
	log.Printf("inbound HOLD: chan=%s ts=%s receipt=%s retry_in=%s bound=%s", e.Inbound.Conversation.ConversationID, e.Inbound.ProviderMessageID, receiptLogID(receipt.id), e.NextAttemptAt.Sub(now), r.bound)
	return nil
}

func (receipt deliveryReceipt) composerHold() *deliveryComposerHold {
	for _, member := range receipt.members {
		if member.Held != nil {
			return member.Held
		}
	}
	return nil
}

func (r *inboundRecovery) observe(receipt deliveryReceipt) {
	if r == nil || receipt.verdict(true) != receiptVouched {
		return
	}
	r.mu.Lock()
	for _, id := range receiptSessions(receipt) {
		r.sessions[id] = true
	}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *inboundRecovery) redact(channel, ts string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, e := range r.state.Entries {
		if e.Inbound.Conversation.ConversationID != channel || e.Inbound.ProviderMessageID != ts {
			continue
		}
		e.Inbound.Text, e.Inbound.Attachments = deletedBySenderNotice, nil
		e.TrustedFilePaths, e.Inbound.trustedFilePaths = nil, nil
		e.Parts = nil
		if e.AliasBody != "" {
			e.AliasBody = deletedBySenderNotice
		}
		r.state.Entries[key] = e
	}
	if err := r.saveLocked(); err != nil {
		log.Printf("inbound HOLD deletion persistence: %v", err)
	}
}

// importDeadLetters folds append-only history by message ID. Completion records
// never erase the original payload. Legacy records without a target are eligible
// at startup or an explicit channel redrive, never on an unrelated session's vouch.
func (r *inboundRecovery) importDeadLetters() error {
	inboundDeadLetterWriteMu.Lock()
	defer inboundDeadLetterWriteMu.Unlock()
	if r == nil {
		return nil
	}
	if r.deadDir == "" {
		return nil
	}
	files, err := os.ReadDir(r.deadDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}
		f, err := openBeneathFlags(r.deadDir, file.Name(), unix.O_NONBLOCK)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			return fmt.Errorf("dead-letter history %s is not a regular file", file.Name())
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 8<<20)
		for scan.Scan() {
			var rec inboundDeadLetterRecord
			if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
				// Preserve damaged bytes in the append-only history for a
				// hand check, but do not disable all inbound delivery. A
				// durable retirement intent can finish its write on tick.
				log.Printf("inbound replay: quarantined malformed history record in %s (retained in place): %v", file.Name(), err)
				continue
			}
			if rec.Inbound.ProviderMessageID == "" {
				continue
			}
			key := recoveryKey(rec.Inbound)
			if rec.AliasSessionID != "" {
				key += "|alias:" + rec.AliasSessionID
			}
			// Liveness history is an alarm, not a complete replay payload:
			// text/files may be clipped, or the record may describe unread
			// replies using their parent's timestamp. Keep a separate key
			// so delivering the parent cannot settle those missing replies.
			manualRecovery := rec.ManualRecovery || strings.HasPrefix(rec.Reason, "inbound liveness:")
			if manualRecovery {
				key += "|manual:" + rec.Inbound.DedupKey
			}
			cur, exists := r.state.Entries[key]
			if !rec.ReplayedAt.IsZero() {
				if !exists {
					cur = inboundRecoveryEntry{Inbound: rec.Inbound, DeadLetteredAt: rec.DeadLetteredAt}
				}
				cur.DeliveredAt = rec.ReplayedAt
				r.state.Entries[key] = compactRecoveryEntry(cur)
				continue
			}
			if exists {
				if cur.DeliveredAt.IsZero() && cur.DeadLetteredAt.IsZero() {
					cur.DeadLetteredAt, cur.NextAttemptAt, cur.Retiring = rec.DeadLetteredAt, time.Time{}, false
					cur.Reason = rec.Reason
					r.state.Entries[key] = cur
				}
				continue
			}
			r.state.Entries[key] = inboundRecoveryEntry{Inbound: rec.Inbound, HeldAt: rec.DeadLetteredAt, ManualRecovery: manualRecovery,
				DeadLetteredAt: rec.DeadLetteredAt, OriginalReceiptID: rec.OriginalReceiptID,
				TargetSessions: rec.TargetSessions, Reason: rec.Reason, PendingReceiptID: rec.PendingReceiptID, PendingReceipt: rec.PendingReceipt, Submission: rec.Submission, Parts: rec.Parts, TrustedFilePaths: rec.TrustedFilePaths,
				AliasSessionID: rec.AliasSessionID, AliasHandle: rec.AliasHandle, AliasBody: rec.AliasBody, Rendered: rec.Rendered, Reaction: rec.Reaction}
			if !rec.ReceiptStateKnown && !rec.PendingReceipt && (strings.Contains(rec.Reason, errReceiptFollowUpStuck.Error()) || strings.Contains(rec.Reason, errReceiptFollowUpLost.Error())) {
				e := r.state.Entries[key]
				e.PendingReceipt, e.LegacyUncertain = true, true
				r.state.Entries[key] = e
			}
			if rec.AliasSessionID != "" {
				e := r.state.Entries[key]
				e.TargetSessions = []string{rec.AliasSessionID}
				r.state.Entries[key] = e
			}
		}
		err = scan.Err()
		f.Close()
		if err != nil {
			return err
		}
	}
	return r.saveLocked()
}

// Older dead-letter records did not retain receipts or targets. Recover known
// facts from the text-free journal when available; never invent an original ID.
func (r *inboundRecovery) enrichFromJournal(dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	needed := false
	for _, e := range r.state.Entries {
		if !e.ManualRecovery && e.DeliveredAt.IsZero() && (e.OriginalReceiptID == "" || len(e.TargetSessions) == 0 || e.LegacyUncertain) {
			needed = true
			break
		}
	}
	if !needed {
		return nil
	}
	// Never expose a partial scan's receipt selection. A later unreadable
	// segment could contain a newer, still-active recovery receipt.
	selected := map[string]string{}
	complete := false
	for key, e := range r.state.Entries {
		if e.LegacyUncertain && e.DeliveredAt.IsZero() {
			e.PendingReceiptID, e.PendingReceipt = "", true
			r.state.Entries[key] = e
		}
	}
	defer func() {
		if !complete {
			if err := r.saveLocked(); err != nil {
				log.Printf("inbound replay: persist uncertainty after incomplete journal scan: %v", err)
			}
		}
	}()
	files, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var budget int64 = 64 << 20
	keysByOrigin := map[string][]string{}
	for key, e := range r.state.Entries {
		if e.ManualRecovery {
			continue
		}
		origin := recoveryKey(e.Inbound)
		keysByOrigin[origin] = append(keysByOrigin[origin], key)
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}
		f, err := openBeneathFlags(dir, file.Name(), unix.O_NONBLOCK)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > budget {
			f.Close()
			return errors.New("inbound provenance journal exceeds 64 MiB recovery budget")
		}
		budget -= info.Size()
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 8<<20)
		for scan.Scan() {
			var rec inboundJournalRecord
			if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
				f.Close()
				return err
			}
			if rec.ReceiptID == "" {
				continue
			}
			windows := append([]injectionWindow{{MessageTS: rec.MessageTS}}, rec.Windows...)
			for _, window := range windows {
				for _, key := range keysByOrigin[originKey(rec.Channel, window.MessageTS)] {
					e := r.state.Entries[key]
					if e.AliasSessionID != "" && (rec.ProofKind != "session" || !slices.Contains(rec.TargetSessions, e.AliasSessionID)) {
						continue
					}
					if e.AliasSessionID == "" && rec.ProofKind == "session" {
						continue
					}
					if e.OriginalReceiptID == "" {
						e.OriginalReceiptID = rec.ReceiptID
					}
					if len(e.TargetSessions) == 0 {
						e.TargetSessions = rec.TargetSessions
					}
					// Acceptance order identifies the latest send. A delayed
					// conclusion of the old batch must not replace the recovery
					// copy's outstanding receipt. Missing acceptance stays held.
					if e.LegacyUncertain && e.DeliveredAt.IsZero() && rec.Phase == "accepted" {
						selected[key] = rec.ReceiptID
					}
					r.state.Entries[key] = e
				}
			}
		}
		err = scan.Err()
		f.Close()
		if err != nil {
			return err
		}
	}
	for key, id := range selected {
		e := r.state.Entries[key]
		e.PendingReceiptID = id
		r.state.Entries[key] = e
	}
	complete = true
	return r.saveLocked()
}

func (r *inboundRecovery) tick(startup bool) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.send == nil {
		return
	}
	if c := r.coalescer; c != nil {
		// Shutdown must await the receipt/state writes after a send or
		// poll, not just the transport call. Admission and the drain's
		// final closed snapshot share c.mu so no tick starts after it.
		c.mu.Lock()
		if c.closed || (r.stopping != nil && r.stopping()) {
			c.mu.Unlock()
			return
		}
		c.inflight++
		c.mu.Unlock()
		defer c.endDelivery()
	}
	r.mu.Lock()
	now := r.now()
	sessions, manual := r.sessions, r.manual
	r.sessions, r.manual = map[string]bool{}, map[string]bool{}
	var keys []string
	for key, e := range r.state.Entries {
		if e.ManualRecovery {
			continue
		}
		if !e.DeliveredAt.IsZero() {
			if e.CompletionPending {
				keys = append(keys, key)
			}
			continue
		}
		keys = append(keys, key)
		if e.DeadLetteredAt.IsZero() {
			continue
		}
		eligible := startup || manual[e.Inbound.Conversation.ConversationID]
		for _, id := range e.TargetSessions {
			eligible = eligible || sessions[id]
		}
		if eligible {
			e.NextAttemptAt = now
			if e.ReplayNotBefore.After(now) {
				e.NextAttemptAt = e.ReplayNotBefore
			}
			r.state.Entries[key] = e
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := r.state.Entries[keys[i]], r.state.Entries[keys[j]]
		cmp := compareSlackTS(a.Inbound.ProviderMessageID, b.Inbound.ProviderMessageID)
		return cmp < 0 || (cmp == 0 && keys[i] < keys[j])
	})
	r.mu.Unlock()
	blocked := map[string]bool{}
	for _, key := range keys {
		if r.stopping != nil && r.stopping() {
			break
		}
		r.mu.Lock()
		e := r.state.Entries[key]
		r.mu.Unlock()
		channel := e.Inbound.Conversation.ConversationID
		orderKeys := []string{"channel:" + channel}
		for _, id := range e.TargetSessions {
			orderKeys = append(orderKeys, "session:"+id)
		}
		blockedHere := false
		for _, orderKey := range orderKeys {
			blockedHere = blockedHere || blocked[orderKey]
		}
		block := func() {
			for _, orderKey := range orderKeys {
				blocked[orderKey] = true
			}
		}
		if e.CompletionPending {
			if err := r.writeDead(e, e.DeliveredAt); err == nil {
				e.CompletionPending = false
				r.put(key, e)
			}
			continue
		}
		// Deadlines and receipts belong to individual submitted deliveries.
		// FIFO ordering limits new sends, not independent settlement.
		acceptedByPolicy := !r.gated && (e.PendingReceipt || e.DeadLetteredAt.IsZero())
		if !e.Landed && !acceptedByPolicy && ((e.Retiring && !now.Before(e.NextAttemptAt)) || (!e.Retiring && e.DeadLetteredAt.IsZero() && now.Sub(e.HeldAt) >= r.bound)) {
			if !e.Retiring {
				e.Reason = "unvouched delivery held for " + r.bound.String()
			}
			r.retire(key, e)
			if e.PendingReceipt {
				block()
			}
			continue
		}
		if blockedHere && !e.PendingReceipt && !e.Landed && !acceptedByPolicy {
			continue
		}
		if !e.DeadLetteredAt.IsZero() && e.NextAttemptAt.IsZero() && !e.PendingReceipt && !e.Landed {
			continue
		}
		if !e.Landed && !acceptedByPolicy && now.Before(e.NextAttemptAt) {
			block()
			continue
		}
		receipt, err, clearPending := r.attempt(e, !blockedHere)
		if clearPending {
			e.PendingReceiptID, e.PendingReceipt, e.LegacyUncertain = "", false, false
		}
		if errors.Is(err, errRecoveryOrder) {
			e.NextAttemptAt = now
			r.put(key, e)
			block()
			continue
		}
		verdict := receipt.verdict(r.gated)
		if err == nil && (verdict == receiptVouched || verdict == receiptUnsupported || receipt.followUpLanded || receipt.landedUnconfirmed()) {
			e.DeliveredAt = r.now()
			if !e.DeadLetteredAt.IsZero() {
				if err := r.writeDead(e, e.DeliveredAt); err != nil {
					log.Printf("inbound replay completion write: %v", err)
					e.CompletionPending = true
				}
			}
			r.put(key, e)
			if r.finished != nil {
				r.finished(e)
			}
			r.observe(receipt)
			continue
		}
		block()
		e.Attempts++
		if permanentDeliveryFailure(err) && e.DeadLetteredAt.IsZero() {
			p := e.pending()
			if nextRejectionStep(p, err) == stepRetryWithoutAttachments {
				p = withholdAttachments(p, err)
				e.Inbound, e.Parts = p.inbound, captureRecoveryParts(p)
				e.TrustedFilePaths = append([]string(nil), p.inbound.trustedFilePaths...)
			} else {
				e.Reason = truncateReason(err.Error())
				r.retire(key, e)
				continue
			}
		}
		if receipt.submission != nil {
			e.Submission = receipt.submission
			e.LegacyUncertain = false
		}
		if hold := receipt.composerHold(); hold != nil {
			e.ComposerHold = hold
		}
		if verdict == receiptHeld {
			e.PendingReceiptID = receipt.id
			e.PendingReceipt = true
		} else if err == nil || errors.Is(err, errDeliveryUnvouched) {
			e.PendingReceiptID = ""
			e.PendingReceipt = false
		}
		if e.OriginalReceiptID == "" {
			e.OriginalReceiptID = receipt.id
		}
		if len(e.TargetSessions) == 0 {
			e.TargetSessions = receiptSessions(receipt)
		}
		if e.DeadLetteredAt.IsZero() {
			e.NextAttemptAt = r.now().Add(retryLadderDelay(inboundHoldBackoff, e.Attempts, r.ceiling))
			deadline := e.HeldAt.Add(r.bound)
			if e.NextAttemptAt.After(deadline) {
				e.NextAttemptAt = deadline
			}
		} else {
			e.NextAttemptAt = time.Time{}
			e.ReplayNotBefore = r.now().Add(retryLadderDelay(inboundHoldBackoff, e.Attempts, r.ceiling))
			if e.PendingReceipt {
				// The selected replay is still active. Poll it on the ladder
				// and retain its selected followers; they can continue in the
				// same tick once it completes, without another redrive signal.
				e.NextAttemptAt = e.ReplayNotBefore
			} else {
				r.pauseReplayFollowers(e)
			}
		}
		r.put(key, e)
	}
	r.updateNotices()
	r.mu.Lock()
	if r.dirty {
		if err := r.saveLocked(); err != nil {
			log.Printf("inbound HOLD persistence retry: %v", err)
		}
	}
	r.mu.Unlock()
}

func (r *inboundRecovery) retire(key string, e inboundRecoveryEntry) {
	// Persist the retirement intent before entering a fallible append. A
	// restart after a failed/partial write must retry that write, not the POST.
	e.Retiring = true
	r.put(key, e)
	e.DeadLetteredAt, e.NextAttemptAt, e.Retiring = r.now(), time.Time{}, false
	if e.PendingReceipt {
		e.NextAttemptAt = r.now().Add(retryLadderDelay(inboundHoldBackoff, e.Attempts, r.ceiling))
	}
	if err := r.writeDead(e, time.Time{}); err != nil {
		e.DeadLetteredAt, e.Retiring = time.Time{}, true
		e.Attempts++
		e.NextAttemptAt = r.now().Add(retryLadderDelay(inboundHoldBackoff, e.Attempts, r.ceiling))
		log.Printf("inbound HOLD dead-letter write: %v; retrying the write in %s", err, e.NextAttemptAt.Sub(r.now()))
	}
	r.put(key, e)
	if !e.Retiring && r.finished != nil {
		r.finished(e)
	}
}

// A failed replay stops its selected FIFO beyond this tick as well. A fresh
// vouch (or another explicit redrive) re-arms the whole set from its oldest ID.
func (r *inboundRecovery) pauseReplayFollowers(failed inboundRecoveryEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, e := range r.state.Entries {
		if e.PendingReceipt || e.DeadLetteredAt.IsZero() || !e.DeliveredAt.IsZero() || compareSlackTS(e.Inbound.ProviderMessageID, failed.Inbound.ProviderMessageID) <= 0 {
			continue
		}
		shares := e.Inbound.Conversation.ConversationID == failed.Inbound.Conversation.ConversationID
		for _, a := range e.TargetSessions {
			for _, b := range failed.TargetSessions {
				shares = shares || a == b
			}
		}
		if shares {
			e.NextAttemptAt = time.Time{}
			r.state.Entries[key] = e
		}
	}
}

func (r *inboundRecovery) put(key string, e inboundRecoveryEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A deletion racing a POST still applies to the durable retry copy.
	if cur := r.state.Entries[key]; cur.Inbound.Text == deletedBySenderNotice {
		e.Inbound.Text, e.Inbound.Attachments = deletedBySenderNotice, nil
		e.TrustedFilePaths, e.Inbound.trustedFilePaths = nil, nil
		e.Parts = nil
		if e.AliasBody != "" {
			e.AliasBody = deletedBySenderNotice
		}
	}
	r.state.Entries[key] = compactRecoveryEntry(e)
	if err := r.saveLocked(); err != nil {
		log.Printf("inbound HOLD persist: %v", err)
	}
}

// The final result says the previous pending receipt is resolved. That fact
// survives a replacement POST's rejection or transport failure.
func (r *inboundRecovery) attempt(e inboundRecoveryEntry, allowSend bool) (deliveryReceipt, error, bool) {
	e.Inbound.trustedFilePaths = e.TrustedFilePaths
	if e.Landed {
		return deliveryReceipt{followUpLanded: true}, nil, false
	}
	if !r.gated && (e.PendingReceipt || e.DeadLetteredAt.IsZero()) {
		// The durable pending record already proves acceptance. Disabling
		// the gate settles it without another possibly duplicate POST.
		return deliveryReceipt{}, nil, false
	}
	if e.PendingReceipt && (e.PendingReceiptID == "" || r.poll == nil) {
		return deliveryReceipt{present: true, id: e.PendingReceiptID, status: "pending"}, nil, false
	}
	if e.PendingReceiptID != "" && r.poll != nil {
		state, receipt, err := r.poll(context.Background(), e.PendingReceiptID)
		if state == receiptPollConcluded && r.journal != nil {
			r.journal(e, receipt)
		}
		if err != nil || state == receiptPollUnavailable || state == receiptPollPending {
			return deliveryReceipt{present: true, id: e.PendingReceiptID, status: "pending"}, err, false
		}
		if state == receiptPollConcluded && (receipt.verdict(r.gated) != receiptUnconfirmed || receipt.landedUnconfirmed()) {
			receipt.followUpLanded = receipt.landedUnconfirmed()
			return receipt, nil, false
		}
	}
	if !allowSend {
		return deliveryReceipt{}, errRecoveryOrder, true
	}
	e.Inbound.originalReceiptID = e.OriginalReceiptID
	if !e.DeadLetteredAt.IsZero() {
		marker := "[delivered late: dead-lettered at " + e.DeadLetteredAt.UTC().Format(time.RFC3339) + "]\n"
		e.Inbound.Text = marker + e.Inbound.Text
		if e.Parts != nil && (e.pending().hasReminderParts() || e.Parts.ThreadAnchor != "") {
			parts := *e.Parts
			parts.Body = marker + parts.Body
			e.Parts = &parts
		}
		if e.AliasBody != "" {
			e.AliasBody = marker + e.AliasBody
		}
	}
	receipt, err := r.send(e)
	if (err == nil || errors.Is(err, errDeliveryUnvouched)) && receipt.landedUnconfirmed() {
		err = nil
		if r.journal != nil && receipt.submission != nil {
			e.Submission = receipt.submission
			r.journal(e, receipt)
		}
	}
	return receipt, err, true
}

func (r *inboundRecovery) writeDead(e inboundRecoveryEntry, replayed time.Time) error {
	_, err := writeInboundDeadLetterRecords(r.deadDir, e.Inbound.Conversation.ConversationID, []inboundDeadLetterRecord{{
		DeadLetteredAt: e.DeadLetteredAt, Channel: e.Inbound.Conversation.ConversationID,
		Attempts: e.Attempts, Reason: e.Reason, Inbound: e.Inbound,
		OriginalReceiptID: e.OriginalReceiptID, TargetSessions: e.TargetSessions, ReplayedAt: replayed,
		ReceiptStateKnown: true,
		ManualRecovery:    e.ManualRecovery,
		TrustedFilePaths:  e.TrustedFilePaths,
		PendingReceiptID:  e.PendingReceiptID, PendingReceipt: e.PendingReceipt, Submission: e.Submission, Parts: e.Parts, AliasSessionID: e.AliasSessionID, AliasHandle: e.AliasHandle, AliasBody: e.AliasBody, Rendered: e.Rendered, Reaction: e.Reaction}})
	return err
}

func (r *inboundRecovery) updateNotices() {
	if r.notice == nil {
		return
	}
	r.mu.Lock()
	now := r.now()
	active := map[string]inboundRecoveryEntry{}
	deliveredAt := map[string]time.Time{}
	for _, e := range r.state.Entries {
		if e.ManualRecovery {
			continue
		}
		channel := e.Inbound.Conversation.ConversationID
		if !e.DeliveredAt.IsZero() {
			if e.DeliveredAt.After(deliveredAt[channel]) {
				deliveredAt[channel] = e.DeliveredAt
			}
			continue
		}
		if old, ok := active[channel]; !ok || e.HeldAt.Before(old.HeldAt) || (e.HeldAt.Equal(old.HeldAt) && compareSlackTS(e.Inbound.ProviderMessageID, old.Inbound.ProviderMessageID) < 0) {
			active[channel] = e
		}
	}
	notices := make(map[string]inboundHoldNotice, len(r.state.Notices))
	for channel, notice := range r.state.Notices {
		notices[channel] = notice
	}
	r.mu.Unlock()
	for channel, e := range active {
		n, exists := notices[channel]
		if exists && !n.Resolved || now.Sub(e.HeldAt) < time.Minute {
			continue
		}
		target := strings.Join(e.TargetSessions, ", ")
		if target == "" {
			target = "the bound session"
		}
		text := fmt.Sprintf("Delivery to %s is held: its composer may be busy or hold an unsubmitted draft since %s; retrying for up to %s.", target, e.HeldAt.UTC().Format(time.RFC3339), r.bound)
		if h := e.ComposerHold; h != nil {
			text = fmt.Sprintf("Delivery to %s is held: composer state %s since %s; retrying for up to %s.", target, sanitizeReceiptLogValue(h.State, 80), sanitizeReceiptLogValue(h.Since, 80), r.bound)
		}
		thread := e.Inbound.ReplyToMessageID
		if thread == "" {
			thread = e.Inbound.ProviderMessageID
		}
		ts, err := r.notice(channel, thread, "", text)
		if err != nil {
			log.Printf("inbound HOLD notice: %v", err)
			continue
		}
		r.mu.Lock()
		r.state.Notices[channel] = inboundHoldNotice{TS: ts, ThreadTS: thread, Since: e.HeldAt}
		_ = r.saveLocked()
		r.mu.Unlock()
	}
	for channel, n := range notices {
		if _, ok := active[channel]; ok || n.Resolved {
			continue
		}
		at := deliveredAt[channel]
		if at.IsZero() {
			continue
		}
		if _, err := r.notice(channel, n.ThreadTS, n.TS, "Delivery recovered: delivered late at "+at.UTC().Format(time.RFC3339)+"."); err != nil {
			log.Printf("inbound HOLD notice edit: %v", err)
			continue
		}
		n.Resolved = true
		r.mu.Lock()
		r.state.Notices[channel] = n
		_ = r.saveLocked()
		r.mu.Unlock()
	}
}

type inboundRecoveryCounts struct{ Held, Dead int }

func (r *inboundRecovery) counts() map[string]inboundRecoveryCounts {
	out := map[string]inboundRecoveryCounts{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.state.Entries {
		if !e.DeliveredAt.IsZero() {
			continue
		}
		channel := e.Inbound.Conversation.ConversationID
		c := out[channel]
		if e.DeadLetteredAt.IsZero() {
			c.Held++
		} else {
			c.Dead++
		}
		out[channel] = c
	}
	return out
}

func (r *inboundRecovery) handleRedrive(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&body); err != nil || body.ConversationID == "" {
		writeAdminError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	if err := r.importDeadLetters(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	r.mu.Lock()
	r.manual[body.ConversationID] = true
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	writeAdminJSON(w, http.StatusAccepted, map[string]any{"conversation_id": body.ConversationID, "status": "queued"})
}

// This is an adapter-authored post, deliberately outside session_publish and
// session_react. It produces no ingestionSessionAct and never adds a reaction.
func inboundHoldPoster(token string) func(string, string, string, string) (string, error) {
	return func(channel, thread, ts, text string) (string, error) {
		if ts == "" {
			resp, err := postMessageWithClient(slackAPIClient, token, slackPostMessageReq{Channel: channel, ThreadTS: thread, Text: text})
			if err != nil {
				return "", err
			}
			if !resp.OK {
				return "", fmt.Errorf("Slack hold notice: %s", resp.Error)
			}
			return resp.TS, nil
		}
		body, _ := json.Marshal(map[string]string{"channel": channel, "ts": ts, "text": text})
		req, err := http.NewRequest(http.MethodPost, slackAPIBase+"/chat.update", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		resp, err := slackAPIClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		var result slackPostMessageResp
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
			return "", err
		}
		if !result.OK {
			return "", fmt.Errorf("Slack hold notice edit: %s", result.Error)
		}
		return ts, nil
	}
}
