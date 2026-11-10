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
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("SLACK_INBOUND_REPLAY_MAX_AGE"))); raw != "" {
		if raw == "off" || raw == "0" {
			r.replayMaxAge = 0
		} else {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 {
				return errors.New("SLACK_INBOUND_REPLAY_MAX_AGE must be a positive duration, 0, or off")
			}
			r.replayMaxAge = d
		}
	}
	noticeOn, err := parseHoldNotice(os.Getenv("SLACK_INBOUND_HOLD_NOTICE"))
	if err != nil {
		return err
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
	r.resolveAlias = func(handle string) (string, bool) {
		if cfg.handleAliases == nil {
			return "", false
		}
		return cfg.handleAliases.Get(handle)
	}
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
			if e.BusyReaction && copyCfg.slackBotToken != "" && copyCfg.busyReaction != "" {
				done, displaced := copyCfg.busyMarks.markBoth(channel, e.Inbound.ReplyToMessageID, ts)
				go func() {
					defer close(done)
					_, err := postReactionToSlack(copyCfg.slackBotToken, slackReactionsAddReq{Channel: channel, Name: copyCfg.busyReaction, Timestamp: ts})
					if err != nil {
						log.Printf("inbound HOLD recovered busy mark: %v", err)
					}
				}()
				for _, d := range displaced {
					go removeBusyReaction(copyCfg, channel, d.mark)
				}
			}
		}
		if e.AliasSessionID != "" {
			bindings := copyCfg.mentionOnly.ForChannel(channel)
			if e.DeliveredAt.IsZero() {
				forgetAliasDeliveryForMentionOnly(copyCfg, bindings, e.AliasSessionID, e.Inbound)
			} else {
				e.Inbound.ReceivedAt = e.DeliveredAt
				forgetAliasDeliveryForMentionOnly(copyCfg, bindings, e.AliasSessionID, e.Inbound)
				recordAliasDeliveryForMentionOnly(copyCfg, bindings, e.AliasSessionID, e.Inbound)
			}
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
			bindings := attemptCfg.mentionOnly.ForChannel(e.Inbound.Conversation.ConversationID)
			if e.AliasOriginalSessionID != "" {
				forgetAliasDeliveryForMentionOnly(attemptCfg, bindings, e.AliasOriginalSessionID, e.Inbound)
			}
			e.Inbound.ReceivedAt = r.now()
			forgetAliasDeliveryForMentionOnly(attemptCfg, bindings, e.AliasSessionID, e.Inbound)
			recordAliasDeliveryForMentionOnly(attemptCfg, bindings, e.AliasSessionID, e.Inbound, true)
			var accepted bool
			receipt, accepted = sendAliasDispatch(attemptCfg, e.AliasSessionID, e.AliasHandle, e.AliasBody, e.Inbound)
			if !accepted {
				forgetAliasDeliveryForMentionOnly(attemptCfg, bindings, e.AliasSessionID, e.Inbound)
				err = errors.New("alias redrive not accepted")
				if receipt.postStatus != 0 {
					err = &inboundPostError{Status: receipt.postStatus, StatusText: fmt.Sprint(receipt.postStatus), Body: "alias redrive not accepted"}
				}
			}
		} else if e.Rendered {
			receipt, err = postInboundWithReceipt(attemptCfg, e.Inbound)
		} else {
			err = deliverCoalescedBatch(attemptCfg, e.Inbound.Conversation.ConversationID, batch)
		}
		return receipt, err
	}
	if noticeOn {
		r.notice = inboundHoldPoster(cfg.slackBotToken)
	}
	return nil
}

func parseHoldNotice(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "on", "1", "true", "yes":
		return true, nil
	case "off", "0", "false", "no":
		return false, nil
	default:
		return false, errors.New("SLACK_INBOUND_HOLD_NOTICE must be on/off, 1/0, true/false, or yes/no")
	}
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
var errRecoveryReaction = errors.New("receipt resolved; reaction returns to its no-wake buffer")
var errRecoveryUncertain = errors.New("concluded receipt still has no delivery proof")
var errRecoveryExpired = errors.New("dead letter exceeds the replay age limit")

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
	AliasOriginalSessionID string                 `json:"alias_original_session_id,omitempty"`
	BusyReaction           bool                   `json:"busy_reaction,omitempty"`
	LastVouchAt            time.Time              `json:"last_vouch_at,omitempty"`
	ReplayBlockedReason    string                 `json:"replay_blocked_reason,omitempty"`
	AcknowledgedAt         time.Time              `json:"acknowledged_at,omitempty"`
	LastAttemptAt          time.Time              `json:"last_attempt_at,omitempty"`
	PendingSince           time.Time              `json:"pending_since,omitempty"`
	NextPollAt             time.Time              `json:"next_poll_at,omitempty"`
	RetryStep              int                    `json:"retry_step,omitempty"`
	ManualRecovery         bool                   `json:"manual_recovery,omitempty"`
	TrustedFilePaths       []string               `json:"trusted_file_paths,omitempty"`
	Landed                 bool                   `json:"landed,omitempty"`
	Parts                  *recoveryParts         `json:"parts,omitempty"`
	LegacyUncertain        bool                   `json:"legacy_uncertain,omitempty"`
	Submission             *recoverySubmission    `json:"submission,omitempty"`
	ComposerHold           *deliveryComposerHold  `json:"composer_hold,omitempty"`
	AliasSessionID         string                 `json:"alias_session_id,omitempty"`
	AliasHandle            string                 `json:"alias_handle,omitempty"`
	AliasBody              string                 `json:"alias_body,omitempty"`
	Inbound                externalInboundMessage `json:"inbound"`
	Reaction               bool                   `json:"reaction,omitempty"`
	Rendered               bool                   `json:"rendered,omitempty"`
	OriginalReceiptID      string                 `json:"original_receipt_id,omitempty"`
	PendingReceiptID       string                 `json:"pending_receipt_id,omitempty"`
	PendingReceipt         bool                   `json:"pending_receipt,omitempty"`
	TargetSessions         []string               `json:"target_sessions,omitempty"`
	HeldAt                 time.Time              `json:"held_at"`
	NextAttemptAt          time.Time              `json:"next_attempt_at"`
	Attempts               int                    `json:"attempts"`
	DeadLetteredAt         time.Time              `json:"dead_lettered_at,omitempty"`
	DeliveredAt            time.Time              `json:"delivered_at,omitempty"`
	CompletionPending      bool                   `json:"completion_pending,omitempty"`
	Retiring               bool                   `json:"retiring,omitempty"`
	ReplayNotBefore        time.Time              `json:"replay_not_before,omitempty"`
	Reason                 string                 `json:"reason,omitempty"`
}

type inboundHoldNotice struct {
	NextAttemptAt time.Time `json:"next_attempt_at,omitempty"`
	Failures      int       `json:"failures,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	LoggedErrors  []string  `json:"logged_errors,omitempty"`
	Ended         bool      `json:"ended,omitempty"`
	PreviousTS    []string  `json:"previous_ts,omitempty"`
	TS            string    `json:"ts"`
	ThreadTS      string    `json:"thread_ts"`
	Since         time.Time `json:"since"`
	Resolved      bool      `json:"resolved,omitempty"`
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
	e.AliasBody = ""
	e.PendingReceiptID, e.PendingReceipt = "", false
	return e
}

type inboundRecoveryState struct {
	Entries map[string]inboundRecoveryEntry `json:"entries"`
	Notices map[string]inboundHoldNotice    `json:"notices"`
}

type inboundRecovery struct {
	coalescer     *inboundCoalescer
	mu, runMu     sync.Mutex
	state         inboundRecoveryState
	path          string
	deadDir       string
	ceiling       time.Duration
	bound         time.Duration
	replayMaxAge  time.Duration
	gated         bool
	now           func() time.Time
	send          func(inboundRecoveryEntry) (deliveryReceipt, error)
	poll          receiptPollFunc
	notice        func(channel, thread, ts, text string) (string, error)
	journal       func(inboundRecoveryEntry, deliveryReceipt)
	finished      func(inboundRecoveryEntry)
	resolveAlias  func(string) (string, bool)
	counted       map[string]bool
	missesPending uint64
	dirty         bool
	stopping      func() bool
	// One worker owns retries and redrives. Success observations only enqueue
	// session IDs; they never acquire a channel lock inside a delivery callback.
	wake     chan struct{}
	sessions map[string]bool
	manual   map[string]bool
}

func newInboundRecovery(path, deadDir string) (*inboundRecovery, error) {
	r := &inboundRecovery{path: path, deadDir: deadDir, ceiling: 15 * time.Minute,
		bound: 30 * time.Minute, replayMaxAge: 24 * time.Hour, gated: true, now: time.Now, wake: make(chan struct{}, 1),
		counted:  map[string]bool{},
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
			log.Printf("inbound HOLD: LOSS unreadable state %s: %v; recovering from history only", path, err)
		}
		if loaded != nil {
			if loaded.Entries == nil || loaded.Notices == nil {
				log.Printf("inbound HOLD: LOSS invalid state %s; recovering from history only", path)
			} else {
				r.state = *loaded
			}
			for key, e := range r.state.Entries {
				if e.RetryStep == 0 {
					e.RetryStep = e.Attempts
				}
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

func (r *inboundRecovery) isNotice(channel, ts string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.state.Notices[channel]
	return ts != "" && (n.TS == ts || slices.Contains(n.PreviousTS, ts))
}

// A nil error confirms durability. On write failure recovery still owns the
// payload AND receipt in memory, and tick retries persistence independently.
func (r *inboundRecovery) hold(p pendingChannelInbound, receipt deliveryReceipt, rendered bool, busy ...bool) error {
	if p.reaction && receipt.verdict(r.gated) != receiptHeld {
		if r.coalescer != nil {
			r.coalescer.restore(p.inbound.Conversation.ConversationID, []pendingChannelInbound{p})
		}
		return nil
	}
	return r.holdEntry(inboundRecoveryEntry{Inbound: redactInboundMessage(p.inbound), Parts: captureRecoveryParts(p), Reaction: p.reaction, Rendered: rendered, BusyReaction: len(busy) > 0 && busy[0]}, receipt)
}

func (r *inboundRecovery) holdAlias(msg externalInboundMessage, session, handle, body string, receipt deliveryReceipt, busy ...bool) error {
	return r.holdEntry(inboundRecoveryEntry{Inbound: redactInboundMessage(msg), AliasSessionID: session, AliasHandle: handle, AliasBody: body, BusyReaction: len(busy) > 0 && busy[0]}, receipt)
}

func (r *inboundRecovery) knownAlias(msg externalInboundMessage, session string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.state.Entries[recoveryKey(msg)+"|alias:"+session]
	if !ok {
		for _, e := range r.state.Entries {
			if e.AliasSessionID == session && recoveryKey(e.Inbound) == recoveryKey(msg) {
				return true
			}
		}
	}
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
		e.PendingSince, e.NextPollAt = now, now.Add(5*time.Second)
	}
	r.state.Entries[key] = e
	r.noteMissedLocked(e)
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
		log.Printf("inbound HOLD: LOSS cannot read history %s: %v", r.deadDir, err)
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".jsonl") {
			continue
		}
		f, err := openBeneathFlags(r.deadDir, file.Name(), unix.O_NONBLOCK)
		if err != nil {
			log.Printf("inbound HOLD: LOSS skipped history %s: %v", filepath.Join(r.deadDir, file.Name()), err)
			continue
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			f.Close()
			log.Printf("inbound HOLD: LOSS skipped non-regular history %s", filepath.Join(r.deadDir, file.Name()))
			continue
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
				id := rec.AliasSessionID
				if rec.AliasOriginalSessionID != "" {
					id = rec.AliasOriginalSessionID
				}
				key += "|alias:" + id
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
			if !rec.AcknowledgedAt.IsZero() {
				if !exists {
					cur = inboundRecoveryEntry{Inbound: rec.Inbound, DeadLetteredAt: rec.DeadLetteredAt}
				}
				cur.AcknowledgedAt = rec.AcknowledgedAt
				r.state.Entries[key] = cur
				continue
			}
			if !rec.ReplayedAt.IsZero() {
				if !exists {
					cur = inboundRecoveryEntry{Inbound: rec.Inbound, DeadLetteredAt: rec.DeadLetteredAt}
				}
				cur.DeliveredAt = rec.ReplayedAt
				r.state.Entries[key] = compactRecoveryEntry(cur)
				continue
			}
			if exists {
				if rec.ReplayBlockedReason != "" {
					cur.ReplayBlockedReason = rec.ReplayBlockedReason
					r.state.Entries[key] = cur
				}
				if cur.DeliveredAt.IsZero() && cur.DeadLetteredAt.IsZero() {
					cur.DeadLetteredAt, cur.NextAttemptAt, cur.Retiring = rec.DeadLetteredAt, time.Time{}, false
					cur.Reason = rec.Reason
					r.state.Entries[key] = cur
				}
				continue
			}
			r.state.Entries[key] = inboundRecoveryEntry{Inbound: rec.Inbound, HeldAt: rec.DeadLetteredAt, ManualRecovery: manualRecovery, ReplayBlockedReason: rec.ReplayBlockedReason,
				DeadLetteredAt: rec.DeadLetteredAt, OriginalReceiptID: rec.OriginalReceiptID, Attempts: rec.Attempts, RetryStep: rec.Attempts,
				TargetSessions: rec.TargetSessions, Reason: rec.Reason, PendingReceiptID: rec.PendingReceiptID, PendingReceipt: rec.PendingReceipt, Submission: rec.Submission, Parts: rec.Parts, TrustedFilePaths: rec.TrustedFilePaths,
				AliasSessionID: rec.AliasSessionID, AliasOriginalSessionID: rec.AliasOriginalSessionID, AliasHandle: rec.AliasHandle, AliasBody: rec.AliasBody, Rendered: rec.Rendered, Reaction: rec.Reaction, BusyReaction: rec.BusyReaction}
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
			e := r.state.Entries[key]
			if legacyPayloadRefusal(rec.Reason) {
				e.ReplayBlockedReason = rec.Reason
			}
			r.state.Entries[key] = e
		}
		err = scan.Err()
		f.Close()
		if err != nil {
			log.Printf("inbound HOLD: LOSS skipped unreadable history tail %s: %v", filepath.Join(r.deadDir, file.Name()), err)
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
	torn := false
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
				torn = true
				log.Printf("inbound replay: skipping torn journal line in %s: %v", file.Name(), err)
				continue
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
		if torn {
			break
		}
		e := r.state.Entries[key]
		e.PendingReceiptID = id
		r.state.Entries[key] = e
	}
	complete = true
	return r.saveLocked()
}

func legacyPayloadRefusal(reason string) bool {
	for _, status := range []string{"400", "413", "415", "422"} {
		if strings.HasPrefix(reason, status+" ") || strings.HasPrefix(reason, status+":") {
			return true
		}
	}
	return false
}

func recoveryOrderKeys(e inboundRecoveryEntry) []string {
	keys := make([]string, 0, len(e.TargetSessions))
	for _, id := range e.TargetSessions {
		keys = append(keys, "session:"+id)
	}
	if len(keys) == 0 {
		keys = append(keys, "channel:"+e.Inbound.Conversation.ConversationID)
	}
	return keys
}

func (e inboundRecoveryEntry) inactive() bool {
	return e.ManualRecovery || e.ReplayBlockedReason != "" || !e.AcknowledgedAt.IsZero() || !e.DeliveredAt.IsZero()
}

func (r *inboundRecovery) pollDelay(e inboundRecoveryEntry) time.Duration {
	since := e.PendingSince
	if since.IsZero() {
		since = e.HeldAt
	}
	if r.now().Sub(since) >= 2*time.Minute {
		return 30 * time.Second
	}
	return 5 * time.Second
}

func (r *inboundRecovery) tick(startup bool) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.send == nil {
		return
	}
	if c := r.coalescer; c != nil {
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
	armed, aged := 0, 0
	var oldest time.Time
	for key, e := range r.state.Entries {
		if e.inactive() && !e.CompletionPending && !e.Retiring {
			continue
		}
		// Resolve before matching vouches and before calculating session FIFO.
		if e.AliasHandle != "" && !e.PendingReceipt && r.resolveAlias != nil {
			if id, ok := r.resolveAlias(e.AliasHandle); ok && id != e.AliasSessionID {
				log.Printf("inbound replay: handle=%s session=%s -> %s", e.AliasHandle, e.AliasSessionID, id)
				if e.AliasOriginalSessionID == "" {
					e.AliasOriginalSessionID = e.AliasSessionID
				}
				e.AliasSessionID, e.TargetSessions = id, []string{id}
			}
		}
		vouched := false
		for _, id := range e.TargetSessions {
			vouched = vouched || sessions[id]
		}
		if !e.DeadLetteredAt.IsZero() {
			eligible := startup || manual[e.Inbound.Conversation.ConversationID] || vouched
			tooOld := r.replayMaxAge == 0 || now.Sub(e.DeadLetteredAt) > r.replayMaxAge
			if tooOld && !e.PendingReceipt {
				e.NextAttemptAt = time.Time{}
				if eligible {
					aged++
					if oldest.IsZero() || e.DeadLetteredAt.Before(oldest) {
						oldest = e.DeadLetteredAt
					}
				}
			} else if eligible && !e.PendingReceipt {
				e.NextAttemptAt = now
				if e.ReplayNotBefore.After(now) {
					e.NextAttemptAt = e.ReplayNotBefore
				}
				armed++
			}
		} else if vouched && !e.PendingReceipt && (e.LastVouchAt.IsZero() || now.Sub(e.LastVouchAt) >= time.Minute) {
			e.NextAttemptAt = now
			e.LastVouchAt = now
		}
		r.state.Entries[key] = e
		keys = append(keys, key)
	}
	if armed > 0 || aged > 0 {
		reason := "vouch"
		if startup {
			reason = "startup"
		} else if len(manual) > 0 {
			reason = "verb"
		}
		log.Printf("inbound replay: trigger=%s armed=%d left-unreplayed-for-age=%d oldest=%s", reason, armed, aged, oldest.UTC().Format(time.RFC3339))
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := r.state.Entries[keys[i]], r.state.Entries[keys[j]]
		cmp := compareSlackTS(a.Inbound.ProviderMessageID, b.Inbound.ProviderMessageID)
		return cmp < 0 || (cmp == 0 && keys[i] < keys[j])
	})
	// A follower's rung wakes the oldest entry sharing its destination.
	pressure := map[string]bool{}
	for _, key := range keys {
		e := r.state.Entries[key]
		if e.inactive() || e.Reaction || e.PendingReceipt || !e.DeadLetteredAt.IsZero() || now.Before(e.NextAttemptAt) {
			continue
		}
		for _, k := range recoveryOrderKeys(e) {
			pressure[k] = true
		}
	}
	r.mu.Unlock()
	blocked := map[string]bool{}
	for _, key := range keys {
		if r.stopping != nil && r.stopping() {
			break
		}
		r.mu.Lock()
		e := r.state.Entries[key]
		r.mu.Unlock()
		if e.CompletionPending {
			if err := r.writeDead(e, e.DeliveredAt); err == nil {
				e.CompletionPending = false
				r.put(key, e)
			}
			continue
		}
		if e.inactive() && !e.Retiring {
			continue
		}
		if e.Retiring {
			if !now.Before(e.NextAttemptAt) {
				r.retire(key, e)
			}
			continue
		}
		if e.PendingReceipt && e.PendingReceiptID == "" {
			e.ReplayBlockedReason = "verify by hand: no pollable receipt"
			if e.DeadLetteredAt.IsZero() {
				e.Reason = e.ReplayBlockedReason
				r.retire(key, e)
			} else {
				r.put(key, e)
			}
			continue
		}
		orderKeys := recoveryOrderKeys(e)
		blockedHere, pushed := false, false
		for _, k := range orderKeys {
			blockedHere = blockedHere || blocked[k]
			pushed = pushed || pressure[k]
		}
		block := func() {
			if !e.Reaction {
				for _, k := range orderKeys {
					blocked[k] = true
				}
			}
		}
		atBound := e.DeadLetteredAt.IsZero() && now.Sub(e.HeldAt) >= r.bound
		if blockedHere && !e.PendingReceipt && !e.Landed {
			if atBound {
				e.Reason = "unvouched delivery held for " + r.bound.String() + "; older delivery still outstanding"
				r.retire(key, e)
			} else if e.DeadLetteredAt.IsZero() && !now.Before(e.NextAttemptAt) {
				e.RetryStep++
				e.NextAttemptAt = now.Add(retryLadderDelay(inboundHoldBackoff, e.RetryStep, r.ceiling))
				if deadline := e.HeldAt.Add(r.bound); e.NextAttemptAt.After(deadline) {
					e.NextAttemptAt = deadline
				}
				r.put(key, e)
			}
			if !atBound {
				block()
			}
			continue
		}
		if !e.PendingReceipt && !e.DeadLetteredAt.IsZero() && e.NextAttemptAt.IsZero() {
			if e.ReplayNotBefore.After(now) {
				block()
			}
			continue
		}
		due := atBound || e.Landed
		if e.PendingReceipt {
			due = due || !now.Before(e.NextPollAt)
		} else {
			due = due || !now.Before(e.NextAttemptAt) || pushed
		}
		if !due {
			if e.DeadLetteredAt.IsZero() || !e.PendingReceipt || (!e.PendingSince.IsZero() && now.Sub(e.PendingSince) < r.bound) {
				block()
			}
			continue
		}
		wasPending := e.PendingReceipt
		receipt, err, clearPending := r.attempt(e, !blockedHere)
		// clearPending means attempt crossed into a new POST, except for
		// the explicit order/reaction/uncertain sentinels below.
		sent := clearPending && !errors.Is(err, errRecoveryOrder) && !errors.Is(err, errRecoveryReaction) && !errors.Is(err, errRecoveryUncertain) && !errors.Is(err, errRecoveryExpired)
		if sent {
			e.Attempts++
			e.RetryStep++
			e.LastAttemptAt = now
		}
		if clearPending {
			e.PendingReceiptID, e.PendingReceipt, e.LegacyUncertain = "", false, false
			e.NextPollAt = time.Time{}
		}
		if errors.Is(err, errRecoveryOrder) {
			e.NextAttemptAt = now
			r.put(key, e)
			block()
			continue
		}
		if errors.Is(err, errRecoveryExpired) {
			e.NextAttemptAt = time.Time{}
			r.put(key, e)
			continue
		}
		if errors.Is(err, errRecoveryReaction) {
			if r.coalescer != nil {
				r.coalescer.restore(e.Inbound.Conversation.ConversationID, []pendingChannelInbound{e.pending()})
				r.mu.Lock()
				delete(r.state.Entries, key)
				_ = r.saveLocked()
				r.mu.Unlock()
			}
			continue
		}
		verdict := receipt.verdict(r.gated)
		log.Printf("inbound HOLD attempt: chan=%s ts=%s alias=%s receipt=%s poll=%t outcome=%s error=%v", e.Inbound.Conversation.ConversationID, e.Inbound.ProviderMessageID, e.AliasSessionID, receiptLogID(receipt.id), wasPending, receipt.logField(verdict), err)
		if errors.Is(err, errRecoveryUncertain) || (err == nil && verdict == receiptUnsupported && !receipt.followUpLanded && !receipt.landedUnconfirmed()) {
			e.ReplayBlockedReason = "verify by hand: delivery proof unavailable"
			if !r.gated {
				e.ReplayBlockedReason = "verify by hand: receipt gate is off"
			}
			e.Reason = e.ReplayBlockedReason
			if e.DeadLetteredAt.IsZero() {
				r.retire(key, e)
			} else {
				r.put(key, e)
			}
			continue
		}
		if err == nil && (verdict == receiptVouched || receipt.followUpLanded || receipt.landedUnconfirmed()) {
			e.DeliveredAt = r.now()
			e.Reason = "terminal handoff confirmed"
			if receipt.followUpLanded || receipt.landedUnconfirmed() {
				e.Reason = "terminal paste confirmed; submission unconfirmed"
			}
			if !e.DeadLetteredAt.IsZero() {
				if err := r.writeDead(e, e.DeliveredAt); err != nil {
					log.Printf("inbound replay completion write: %v", err)
					e.CompletionPending = true
				}
			}
			log.Printf("inbound HOLD settled: chan=%s ts=%s receipt=%s by=%s dead-lettered-at=%s proof=%s", e.Inbound.Conversation.ConversationID, e.Inbound.ProviderMessageID, receiptLogID(receipt.id), e.DeliveredAt.UTC().Format(time.RFC3339), e.DeadLetteredAt.UTC().Format(time.RFC3339), e.Reason)
			r.put(key, e)
			if r.finished != nil {
				r.finished(e)
			}
			r.observe(receipt)
			continue
		}
		var postErr *inboundPostError
		refused := permanentDeliveryFailure(err) || (e.AliasSessionID != "" && errors.As(err, &postErr) && postErr.Status >= 400 && postErr.Status < 500)
		if refused {
			p := e.pending()
			if e.AliasSessionID == "" && e.DeadLetteredAt.IsZero() && nextRejectionStep(p, err) == stepRetryWithoutAttachments {
				p = withholdAttachments(p, err)
				e.Inbound, e.Parts = p.inbound, captureRecoveryParts(p)
				e.TrustedFilePaths = append([]string(nil), p.inbound.trustedFilePaths...)
			} else {
				e.ReplayBlockedReason = truncateReason(err.Error())
				e.Reason = e.ReplayBlockedReason
				if e.DeadLetteredAt.IsZero() {
					r.retire(key, e)
				} else {
					r.put(key, e)
					if err := r.writeDead(e, time.Time{}); err != nil {
						log.Printf("inbound HOLD refusal persistence: %v", err)
					}
				}
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
			if !e.PendingReceipt {
				e.PendingSince = now
			}
			e.PendingReceiptID, e.PendingReceipt = receipt.id, true
		} else if err == nil || errors.Is(err, errDeliveryUnvouched) {
			e.PendingReceiptID, e.PendingReceipt = "", false
		}
		if e.OriginalReceiptID == "" {
			e.OriginalReceiptID = receipt.id
		}
		if len(e.TargetSessions) == 0 {
			e.TargetSessions = receiptSessions(receipt)
		}
		if e.PendingReceipt {
			e.NextPollAt = now.Add(r.pollDelay(e))
		}
		if atBound {
			e.Reason = "unvouched delivery held for " + r.bound.String()
			r.retire(key, e)
			continue
		}
		if e.DeadLetteredAt.IsZero() {
			e.NextAttemptAt = now.Add(retryLadderDelay(inboundHoldBackoff, e.RetryStep, r.ceiling))
			if deadline := e.HeldAt.Add(r.bound); e.NextAttemptAt.After(deadline) {
				e.NextAttemptAt = deadline
			}
			block()
		} else if !e.PendingReceipt {
			e.NextAttemptAt = time.Time{}
			e.ReplayNotBefore = now.Add(retryLadderDelay(inboundHoldBackoff, e.RetryStep, r.ceiling))
			block()
		} else if !e.PendingSince.IsZero() && now.Sub(e.PendingSince) < r.bound {
			block()
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
		e.NextPollAt = r.now().Add(r.pollDelay(e))
	}
	if err := r.writeDead(e, time.Time{}); err != nil {
		e.DeadLetteredAt, e.Retiring = time.Time{}, true
		e.RetryStep++
		e.NextAttemptAt = r.now().Add(retryLadderDelay(inboundHoldBackoff, e.RetryStep, r.ceiling))
		log.Printf("inbound HOLD dead-letter write: %v; retrying the write in %s", err, e.NextAttemptAt.Sub(r.now()))
	}
	r.put(key, e)
	if !e.Retiring {
		path := filepath.Join(r.deadDir, safePathComponent(e.Inbound.Conversation.ConversationID)+".jsonl")
		log.Printf("inbound HOLD: LOSS chan=%s ts=%s receipt=%s dead-lettered after %d attempt(s) over %s file=%s", e.Inbound.Conversation.ConversationID, e.Inbound.ProviderMessageID, receiptLogID(e.OriginalReceiptID), e.Attempts, r.now().Sub(e.HeldAt), path)
	}
	if !e.Retiring && r.finished != nil {
		r.finished(e)
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
		// Acceptance is not proof of delivery. With checking disabled,
		// retain uncertainty without another possibly duplicate POST.
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
		if state == receiptPollConcluded && receipt.verdict(r.gated) == receiptHeld && !receipt.landedUnconfirmed() {
			return receipt, errRecoveryUncertain, false
		}
		if err != nil || state == receiptPollUnavailable || state == receiptPollPending {
			return deliveryReceipt{present: true, id: e.PendingReceiptID, status: "pending"}, err, false
		}
		if state == receiptPollConcluded && (receipt.verdict(r.gated) != receiptUnconfirmed || receipt.landedUnconfirmed()) {
			receipt.followUpLanded = receipt.landedUnconfirmed()
			return receipt, nil, false
		}
	}
	if !e.DeadLetteredAt.IsZero() && (r.replayMaxAge == 0 || r.now().Sub(e.DeadLetteredAt) > r.replayMaxAge) {
		return deliveryReceipt{}, errRecoveryExpired, true
	}
	if e.Reaction {
		return deliveryReceipt{}, errRecoveryReaction, true
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
		ReceiptStateKnown:   true,
		ReplayBlockedReason: e.ReplayBlockedReason, AcknowledgedAt: e.AcknowledgedAt,
		ManualRecovery:         e.ManualRecovery,
		BusyReaction:           e.BusyReaction,
		AliasOriginalSessionID: e.AliasOriginalSessionID,
		TrustedFilePaths:       e.TrustedFilePaths,
		PendingReceiptID:       e.PendingReceiptID, PendingReceipt: e.PendingReceipt, Submission: e.Submission, Parts: e.Parts, AliasSessionID: e.AliasSessionID, AliasHandle: e.AliasHandle, AliasBody: e.AliasBody, Rendered: e.Rendered, Reaction: e.Reaction}})
	return err
}

func (r *inboundRecovery) saveNotice(channel string, n inboundHoldNotice) {
	r.mu.Lock()
	r.state.Notices[channel] = n
	if err := r.saveLocked(); err != nil {
		log.Printf("inbound HOLD notice persistence: %v", err)
	}
	r.mu.Unlock()
}

func permanentNoticeError(err error) bool {
	if err == nil {
		return false
	}
	for _, code := range []string{"thread_not_found", "channel_not_found", "not_in_channel", "is_archived", "message_not_found", "cant_update_message", "edit_window_closed"} {
		if strings.Contains(err.Error(), code) {
			return true
		}
	}
	return false
}

func (r *inboundRecovery) noticeFailure(channel string, n inboundHoldNotice, err error) inboundHoldNotice {
	if n.LastError != "" && !slices.Contains(n.LoggedErrors, n.LastError) {
		n.LoggedErrors = append(n.LoggedErrors, n.LastError)
	}
	if !slices.Contains(n.LoggedErrors, err.Error()) {
		log.Printf("inbound HOLD notice: chan=%s: %v", channel, err)
		n.LoggedErrors = append(n.LoggedErrors, err.Error())
	}
	n.LastError = err.Error()
	n.NextAttemptAt = r.now().Add(retryLadderDelay(inboundHoldBackoff, n.Failures, r.ceiling))
	n.Failures++
	n.Ended = permanentNoticeError(err)
	return n
}

func (r *inboundRecovery) updateNotices() {
	if r.notice == nil {
		return
	}
	r.mu.Lock()
	now := r.now()
	active := map[string]inboundRecoveryEntry{}
	entries := make([]inboundRecoveryEntry, 0, len(r.state.Entries))
	for _, e := range r.state.Entries {
		entries = append(entries, e)
		if e.Reaction || e.Inbound.Actor.IsBot || e.inactive() || !e.DeadLetteredAt.IsZero() {
			continue
		}
		channel := e.Inbound.Conversation.ConversationID
		if old, ok := active[channel]; !ok || e.HeldAt.Before(old.HeldAt) || (e.HeldAt.Equal(old.HeldAt) && compareSlackTS(e.Inbound.ProviderMessageID, old.Inbound.ProviderMessageID) < 0) {
			active[channel] = e
		}
	}
	notices := make(map[string]inboundHoldNotice, len(r.state.Notices))
	for channel, n := range r.state.Notices {
		notices[channel] = n
	}
	r.mu.Unlock()
	for channel, n := range notices {
		if _, ok := active[channel]; ok || n.Resolved {
			continue
		}
		if n.Ended || n.TS == "" {
			n.Resolved = true
			r.saveNotice(channel, n)
			continue
		}
		if now.Before(n.NextAttemptAt) && now.Sub(n.Since) < r.bound {
			continue
		}
		var latest time.Time
		dead, unknown, gateOff, paste, acknowledged := false, false, false, false, false
		for _, e := range entries {
			if e.Inbound.Conversation.ConversationID != channel || e.HeldAt.Before(n.Since) || e.Reaction || e.Inbound.Actor.IsBot {
				continue
			}
			if e.DeliveredAt.After(latest) {
				latest = e.DeliveredAt
			}
			if !e.AcknowledgedAt.IsZero() {
				acknowledged = true
				continue
			}
			dead = dead || (!e.DeadLetteredAt.IsZero() && e.DeliveredAt.IsZero() && e.AcknowledgedAt.IsZero())
			unknown = unknown || strings.Contains(e.Reason, "proof unavailable")
			gateOff = gateOff || strings.Contains(e.Reason, "gate is off")
			paste = paste || strings.Contains(e.Reason, "submission unconfirmed")
		}
		text := fmt.Sprintf("Held since %s; handed to the session's terminal by %s, with reading unconfirmed.", n.Since.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
		if paste {
			text = fmt.Sprintf("Held since %s; pasted into the session's terminal by %s, with submission and reading unconfirmed.", n.Since.UTC().Format(time.RFC3339), latest.UTC().Format(time.RFC3339))
		}
		if acknowledged {
			text = "The held incident was acknowledged by hand; this does not confirm terminal delivery or reading."
		}
		if dead {
			text = fmt.Sprintf("Delivery was not confirmed within %s; the recorded line may arrive late when the session accepts deliveries again.", r.bound)
		}
		if unknown {
			text = "Delivery proof is unavailable; the recorded line needs verification by hand."
		}
		if gateOff {
			text = "Receipt checking is off; delivery remains unverified and the recorded line needs verification by hand."
		}
		_, err := r.notice(channel, n.ThreadTS, n.TS, text)
		if err != nil {
			n = r.noticeFailure(channel, n, err)
			if now.Sub(n.Since) >= r.bound {
				n.Ended = true
				n.Resolved = true
			}
		} else {
			n.Resolved = true
			n.LastError = ""
			n.NextAttemptAt = time.Time{}
		}
		r.saveNotice(channel, n)
	}
	for channel, e := range active {
		n, exists := notices[channel]
		if exists && !n.Resolved && (n.TS != "" || n.Ended) {
			continue
		}
		delay := time.Minute
		if e.PendingReceipt {
			delay = 3 * time.Minute
		}
		if now.Sub(e.HeldAt) < delay {
			continue
		}
		if !exists || n.Resolved {
			old := append([]string(nil), n.PreviousTS...)
			if n.TS != "" {
				old = append(old, n.TS)
			}
			n = inboundHoldNotice{Since: e.HeldAt, PreviousTS: old}
			n.ThreadTS = e.Inbound.ReplyToMessageID
			if n.ThreadTS == "" {
				n.ThreadTS = e.Inbound.ProviderMessageID
			}
		}
		if now.Before(n.NextAttemptAt) || n.Ended {
			continue
		}
		action := "retrying"
		if e.PendingReceipt {
			action = "checking the outstanding delivery receipt"
		}
		text := fmt.Sprintf("Delivery has been held since %s; %s for up to %s.", e.HeldAt.UTC().Format(time.RFC3339), action, r.bound)
		ts, err := r.notice(channel, n.ThreadTS, "", text)
		if err != nil {
			n = r.noticeFailure(channel, n, err)
		} else {
			n.TS = ts
			n.LastError = ""
			n.NextAttemptAt = time.Time{}
		}
		r.saveNotice(channel, n)
	}
}

type inboundRecoveryCounts struct{ Held, Dead, ByHand int }

func recoveryCountClass(e inboundRecoveryEntry) int {
	if !e.DeliveredAt.IsZero() || !e.AcknowledgedAt.IsZero() || e.Reaction || e.Inbound.Actor.IsBot {
		return 0
	}
	if e.ManualRecovery || e.ReplayBlockedReason != "" || (e.PendingReceipt && e.PendingReceiptID == "") {
		return 3
	}
	if !e.DeadLetteredAt.IsZero() {
		return 2
	}
	return 1
}

func (r *inboundRecovery) counts() map[string]inboundRecoveryCounts {
	out := map[string]inboundRecoveryCounts{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lines := map[string]int{}
	channels := map[string]string{}
	for _, e := range r.state.Entries {
		key := recoveryKey(e.Inbound)
		if class := recoveryCountClass(e); class > lines[key] {
			lines[key] = class
			channels[key] = e.Inbound.Conversation.ConversationID
		}
	}
	for key, class := range lines {
		channel := channels[key]
		c := out[channel]
		switch class {
		case 1:
			c.Held++
		case 2:
			c.Dead++
		case 3:
			c.ByHand++
		}
		out[channel] = c
	}
	return out
}

// A process counter records each human line once, independently of how often
// liveness observes it or how many channel/alias copies remain outstanding.
func (r *inboundRecovery) noteMissedLocked(e inboundRecoveryEntry) {
	if r.counted == nil {
		r.counted = map[string]bool{}
	}
	key := recoveryKey(e.Inbound)
	if recoveryCountClass(e) > 0 && !r.counted[key] {
		r.counted[key] = true
		r.missesPending++
	}
}

func (r *inboundRecovery) newlyMissed() uint64 {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.state.Entries {
		r.noteMissedLocked(e)
	}
	n := r.missesPending
	r.missesPending = 0
	return n
}

func (r *inboundRecovery) acknowledge(channel, ts string) (int, error) {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	r.mu.Lock()
	var selected []string
	for key, e := range r.state.Entries {
		if e.Inbound.Conversation.ConversationID == channel && e.Inbound.ProviderMessageID == ts && e.DeliveredAt.IsZero() && e.AcknowledgedAt.IsZero() {
			selected = append(selected, key)
		}
	}
	r.mu.Unlock()
	for _, key := range selected {
		r.mu.Lock()
		e := r.state.Entries[key]
		r.mu.Unlock()
		e.AcknowledgedAt = r.now()
		if err := r.writeDead(e, time.Time{}); err != nil {
			return 0, err
		}
		r.put(key, e)
	}
	return len(selected), nil
}

func (r *inboundRecovery) handleRedrive(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ConversationID string `json:"conversation_id"`
		Acknowledge    string `json:"acknowledge,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(req.Body, 4096)).Decode(&body); err != nil || body.ConversationID == "" {
		writeAdminError(w, http.StatusBadRequest, "conversation_id is required")
		return
	}
	if err := r.importDeadLetters(); err != nil {
		writeAdminError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if body.Acknowledge != "" {
		n, err := r.acknowledge(body.ConversationID, body.Acknowledge)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"conversation_id": body.ConversationID, "acknowledged": n})
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
