package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// No message text, display names, author ids or credentials belong here.
type injectionWindow struct {
	MessageTS string `json:"message_ts"`
	ThreadTS  string `json:"thread_ts,omitempty"`
	Oldest    string `json:"oldest_quoted_ts,omitempty"`
	Newest    string `json:"newest_quoted_ts,omitempty"`
	Collapsed int    `json:"collapsed"`
	Known     bool   `json:"known"`
	SinceTS   string `json:"since_ts,omitempty"`
	Skipped   int    `json:"skipped_before_watermark,omitempty"`
}

type inboundJournal struct {
	mu            sync.Mutex
	dir           string
	retentionDays int
	maxBytes      int64
	now           func() time.Time
}

type inboundJournalRecord struct {
	Phase              string             `json:"phase"`
	TS                 time.Time          `json:"ts"`
	Provider           string             `json:"provider,omitempty"`
	Channel            string             `json:"channel"`
	ThreadTS           string             `json:"thread_ts,omitempty"`
	MessageTS          string             `json:"message_ts"`
	Target             string             `json:"target"`
	TargetSessions     []string           `json:"target_sessions,omitempty"`
	HandoffSessions    []string           `json:"handoff_sessions"`
	CanonicalSessionID string             `json:"canonical_session_id,omitempty"`
	Bytes              int                `json:"bytes"`
	Mode               string             `json:"mode"`
	ReceiptID          string             `json:"receipt_id,omitempty"`
	Status             string             `json:"status"`
	Windows            []injectionWindow  `json:"windows"`
	PayloadSHA256      string             `json:"payload_sha256,omitempty"`
	ProofKind          string             `json:"proof_kind,omitempty"`
	DeliveredAt        time.Time          `json:"delivered_at,omitempty"`
	StartedAt          time.Time          `json:"started_at,omitempty"`
	Evidence           *ingestionEvidence `json:"ingestion_evidence,omitempty"`
	Alerted            bool               `json:"alerted,omitempty"`
	Late               bool               `json:"late,omitempty"`
}

func (j *inboundJournal) record(cfg config, msg externalInboundMessage, receipt deliveryReceipt, phase ...string) {
	if j == nil {
		return
	}
	r := inboundJournalRecord{Channel: msg.Conversation.ConversationID, ThreadTS: msg.ReplyToMessageID, MessageTS: msg.ProviderMessageID, Target: msg.ExplicitTarget, Bytes: len(msg.Text), Mode: cfg.injectionMode, ReceiptID: receipt.id, Status: receipt.status, Windows: msg.injectionWindows, HandoffSessions: []string{}}
	r.StartedAt = msg.injectionStartedAt
	r.Provider = msg.Conversation.Provider
	if r.Provider == "" {
		r.Provider = cfg.provider
	}
	if r.Provider == "" {
		r.Provider = "slack"
	}
	r.ProofKind = "extmsg"
	// Held-receipt callbacks may retain the pre-redaction envelope; hash the
	// same redacted text the extmsg POST actually sent.
	payload := ingestionSanitize(redactInboundMessage(msg).Text)
	if msg.injectionDirect {
		r.ProofKind, payload = "session", msg.Text
	}
	r.PayloadSHA256 = ingestionDigest(payload)
	r.Phase = "accepted"
	if len(phase) > 0 {
		r.Phase = phase[0]
	}
	if receipt.status == receiptFollowUpEnded {
		r.Phase, r.Status = receiptFollowUpEnded, "unverified"
	}
	if r.Mode == "" {
		r.Mode = "full"
	}
	if r.Status == "" {
		r.Status = "accepted_unconfirmed"
	}
	for _, m := range receipt.members {
		if m.SessionID != "" {
			r.TargetSessions = append(r.TargetSessions, m.SessionID)
			// A completed paste may still have an unconfirmed submit. Keep
			// that transport fact separate from actual session ingestion.
			if (receiptStatusPositive(m.Status) && !m.shortfall()) ||
				(m.Status == "" && receiptStatusPositive(r.Status) && !receipt.shortfall()) ||
				(receiptStatusPending(m.Status) && m.landedWhole()) {
				r.HandoffSessions = append(r.HandoffSessions, m.SessionID)
			}
		}
	}
	if len(r.Windows) == 0 {
		r.Windows = []injectionWindow{{MessageTS: r.MessageTS, ThreadTS: r.ThreadTS, Known: cfg.minimalInjection()}}
	}
	if err := j.append(r); err != nil {
		log.Printf("inbound journal: %v", err)
	}
}

// Used for both the initial direct send and its held-receipt callback, so
// delayed confirmation cannot silently switch a reminder to extmsg proof.
func (j *inboundJournal) recordDirect(cfg config, msg externalInboundMessage, receipt deliveryReceipt, sessionID, body string, phase ...string) {
	msg.Text, msg.injectionDirect = body, true
	if len(receipt.members) == 0 {
		receipt.members = []deliveryReceiptMember{{SessionID: sessionID, Status: receipt.status,
			Delivered: receipt.delivered, Expected: receipt.expected,
			DeliveredOK: receipt.deliveredOK, ExpectedOK: receipt.expectedOK,
			DeliveredUnit: receipt.deliveredUnit, ExpectedUnit: receipt.expectedUnit}}
	}
	j.record(cfg, msg, receipt, phase...)
}

func (j *inboundJournal) append(r inboundJournalRecord) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	now := time.Now().UTC()
	if j.now != nil {
		now = j.now().UTC()
	}
	r.TS = now
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(j.dir, 0700); err != nil {
		return err
	}
	day := now.Format("20060102")
	// Rotate busy-day segments rather than dropping records at the cap.
	limit := j.maxBytes
	if limit <= 0 {
		limit = 16 << 20
	}
	path := filepath.Join(j.dir, day+".jsonl")
	if info, err := os.Stat(path); err == nil && info.Size()+int64(len(data)) > limit {
		for n := 1; ; n++ {
			archive := filepath.Join(j.dir, fmt.Sprintf("%s.%06d.jsonl", day, n))
			if _, err := os.Stat(archive); os.IsNotExist(err) {
				if err := os.Rename(path, archive); err != nil {
					return err
				}
				break
			} else if err != nil {
				return err
			}
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	// A crash can leave an unterminated JSON fragment. Isolate it so the
	// next complete record is not swallowed along with the torn line.
	info, err := f.Stat()
	if err == nil && info.Size() > 0 {
		var last [1]byte
		_, err = f.ReadAt(last[:], info.Size()-1)
		if err == nil && last[0] != '\n' {
			data = append([]byte{'\n'}, data...)
		}
	}
	if err != nil {
		f.Close()
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	days := j.retentionDays
	if days <= 0 {
		days = 7
	}
	cutoff := now.AddDate(0, 0, -(days - 1)).Format("20060102")
	entries, err := os.ReadDir(j.dir)
	if err != nil {
		return err
	}
	var expired []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 14 || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if _, err := time.Parse("20060102", name[:8]); err != nil {
			continue
		}
		if name[:8] < cutoff {
			expired = append(expired, name)
		}
	}
	if len(expired) == 0 {
		return nil
	}
	// Retention must not erase the recovery record for a still-unseen
	// delivery or an alert that has not reached the mayor. Preserve its
	// original interval and all later updates until observation settles.
	observations, recordDays, receiptStart, err := j.ingestionStateLocked(now)
	if err != nil {
		return fmt.Errorf("journal pruning deferred: %w", err)
	}
	if !receiptStart.IsZero() {
		if day := receiptStart.UTC().Format("20060102"); day < cutoff {
			cutoff = day
		}
	}
	for _, observation := range observations {
		if ingestionSettled(observation, now) {
			continue
		}
		start := earlierIngestionTime(observation.StartedAt, observation.DeliveredAt)
		if day := start.UTC().Format("20060102"); day < cutoff {
			cutoff = day
		}
	}
	// A settled member's evidence may be in yesterday's file while today's
	// transport conclusion still names that member. Copy only those verdicts
	// that would otherwise be deleted before their newer rows. Preserve all
	// timestamps, proof and alert state; fsync before removing any source.
	var carry []byte
	for key, observation := range observations {
		d := recordDays[key]
		if !ingestionSettled(observation, now) || d.selected >= cutoff || d.latest < cutoff {
			continue
		}
		b, err := json.Marshal(observation)
		if err != nil {
			return err
		}
		carry = append(append(carry, b...), '\n')
	}
	if len(carry) > 0 {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(carry)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, name := range expired {
		if name[:8] < cutoff {
			if err := os.Remove(filepath.Join(j.dir, name)); err != nil {
				return err
			}
		}
	}
	return nil
}

func selectedInjectionWindows(msg externalInboundMessage, full, lean, rendered string) []injectionWindow {
	windows := append([]injectionWindow(nil), msg.injectionWindows...)
	if full != "" && strings.Contains(rendered, full) {
		return windows
	}
	if lean != "" && strings.Contains(rendered, lean) {
		return append([]injectionWindow(nil), msg.injectionLeanWindows...)
	}
	for i := range windows {
		windows[i].Oldest, windows[i].Newest, windows[i].Collapsed = "", "", 0
		// A fitted preamble can retain a suffix or a clipped quote. Text
		// alone cannot recover exact timestamps; do not report an empty
		// known window when some context may have reached the recipient.
		if full != "" {
			windows[i].Known = false
		}
	}
	return windows
}
