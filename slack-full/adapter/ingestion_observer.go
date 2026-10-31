package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const ingestionWindow = 3 * time.Minute
const ingestionPollInterval = 5 * time.Second
const ingestionReadLimit = 32 << 20

// These are session-side facts, not a transport receipt or model claim.
// No transcript text is persisted in the journal.
type ingestionEvidence struct {
	SessionID string    `json:"session_id"`
	EntryID   string    `json:"entry_id"`
	Timestamp time.Time `json:"timestamp"`
	Kind      string    `json:"kind"`
}

type ingestionObserver struct {
	cfg      config
	now      func() time.Time
	fetch    func(context.Context, string) ([]json.RawMessage, error)
	notify   func(context.Context, inboundJournalRecord) error
	resolved map[string]string
}

func ingestionDigest(text string) string {
	d := sha256.Sum256([]byte(text))
	return hex.EncodeToString(d[:])
}

// Mirrors gc's promptsafe boundary stripping for the extmsg envelope. A
// future incompatible renderer fails closed as unseen, never as ingested.
func ingestionSanitize(text string) string {
	for {
		next := strings.ReplaceAll(strings.ReplaceAll(text, "</system-reminder>", ""), "<system-reminder>", "")
		if next == text {
			return text
		}
		text = next
	}
}

func startIngestionObserver(cfg config) func() {
	if cfg.inboundJournal == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	o := &ingestionObserver{cfg: cfg, now: time.Now}
	o.fetch, o.notify = o.fetchTranscript, o.mailUnseen
	go func() {
		defer close(done)
		ticker := time.NewTicker(ingestionPollInterval)
		defer ticker.Stop()
		for {
			if err := o.step(ctx); err != nil && ctx.Err() == nil {
				log.Printf("ingestion observer: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}

func ingestionKey(r inboundJournalRecord, session string) string {
	return strings.Join([]string{r.ReceiptID, r.Channel, r.MessageTS, r.PayloadSHA256, session}, "\x00")
}

// Reconstruct from the durable journal every pass: adapter restarts cannot
// forget a delivered receipt or send another successful alert. Scan all
// retained days; bounded reads fail loudly rather than silently skipping rows.
func (o *ingestionObserver) pending() (map[string]inboundJournalRecord, error) {
	j := o.cfg.inboundJournal
	j.mu.Lock()
	defer j.mu.Unlock()
	rows, _, err := j.pendingIngestionLocked()
	return rows, err
}

// Shared with retention pruning. The caller holds j.mu so pending evidence
// cannot be removed between reconstruction and the pruning decision. The
// second result protects initial pending receipts before they become eligible
// for session observation; it never treats an acceptance as a handoff.
func (j *inboundJournal) pendingIngestionLocked() (map[string]inboundJournalRecord, time.Time, error) {
	paths, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil {
		return nil, time.Time{}, err
	}
	sort.Strings(paths)
	rows := make(map[string]inboundJournalRecord)
	alerted := make(map[string]bool)
	started := make(map[string]time.Time)
	held := make(map[string]bool)
	concluded := make(map[string]bool)
	remaining := int64(64 << 20)
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return nil, time.Time{}, err
		}
		remaining -= info.Size()
		if remaining < 0 {
			return nil, time.Time{}, fmt.Errorf("journal exceeds 64 MiB observation budget")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, time.Time{}, err
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 4096), 1<<20)
		for s.Scan() {
			var r inboundJournalRecord
			if err := json.Unmarshal(s.Bytes(), &r); err != nil {
				f.Close()
				return nil, time.Time{}, fmt.Errorf("invalid journal row: %w", err)
			}
			// Old rows lack a complete payload fingerprint. Do not fabricate
			// acknowledgements or flood the mailbox on the first upgrade.
			if r.PayloadSHA256 == "" || r.TS.IsZero() {
				continue
			}
			identity := ingestionKey(r, "")
			start := earlierIngestionTime(r.StartedAt, earlierIngestionTime(r.DeliveredAt, r.TS))
			started[identity] = earlierIngestionTime(started[identity], start)
			if r.Phase == "accepted" && r.ReceiptID != "" && receiptStatusPending(r.Status) {
				held[identity] = true
			}
			if r.Phase == "concluded" || receiptStatusPositive(r.Status) || r.Status == "ingested" || r.Status == "delivered_unseen" {
				concluded[identity] = true
			}
			if r.Status != "delivered" && r.Status != "ingested" && r.Status != "delivered_unseen" && len(r.HandoffSessions) == 0 {
				continue
			}
			if r.Alerted {
				alerted[ingestionKey(r, "")] = true
			}
			targets := r.TargetSessions
			if r.HandoffSessions != nil {
				targets = r.HandoffSessions
			}
			for _, session := range targets {
				if session == "" {
					continue
				}
				key := ingestionKey(r, session)
				copy := r
				copy.TargetSessions = []string{session}
				if len(r.HandoffSessions) > 0 {
					copy.HandoffSessions = []string{session}
				}
				if copy.DeliveredAt.IsZero() {
					copy.DeliveredAt = copy.TS
				}
				if old, ok := rows[key]; ok {
					// Transport polls cannot undo a session-side verdict or
					// restart a handoff's original observation deadline.
					handoff := earlierIngestionTime(old.DeliveredAt, copy.DeliveredAt)
					canonical := old.CanonicalSessionID
					if old.Status == "ingested" ||
						(old.Status == "delivered_unseen" && copy.Status != "ingested") ||
						(copy.Status != "ingested" && copy.Status != "delivered_unseen" && old.TS.After(copy.TS)) {
						copy = old
					}
					copy.DeliveredAt = handoff
					if canonical != "" {
						copy.CanonicalSessionID = canonical
					}
				}
				rows[key] = copy
			}
		}
		err = s.Err()
		f.Close()
		if err != nil {
			return nil, time.Time{}, err
		}
	}
	for key, r := range rows {
		r.StartedAt = started[ingestionKey(r, "")]
		if alerted[ingestionKey(r, "")] {
			r.Alerted = true
		}
		rows[key] = r
		if r.Status == "ingested" || (r.Status == "delivered_unseen" && r.Alerted) {
			delete(rows, key)
		}
	}
	var receiptStart time.Time
	for identity := range held {
		if !concluded[identity] {
			receiptStart = earlierIngestionTime(receiptStart, started[identity])
		}
	}
	return rows, receiptStart, nil
}

func earlierIngestionTime(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

func (o *ingestionObserver) step(ctx context.Context) error {
	rows, err := o.pending()
	if err != nil {
		return err
	}
	// One bounded request per target per pass, not one per Slack message.
	cache := make(map[string][]json.RawMessage)
	// Resolution results describe this pass only. Each delivery's durable
	// CanonicalSessionID remains pinned, but a new delivery may use a name
	// reassigned to a different session since the previous pass.
	o.resolved = make(map[string]string)
	alerted := make(map[string]bool)
	keys := make([]string, 0, len(rows))
	for key := range rows {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var failures []error
	for _, key := range keys {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r := rows[key]
		session := r.TargetSessions[0]
		if r.CanonicalSessionID != "" {
			session = r.CanonicalSessionID
		}
		messages, ok := cache[session]
		if !ok {
			requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			messages, err = o.fetch(requestCtx, session)
			cancel()
			if err != nil {
				log.Printf("ingestion observer: session=%s transcript unavailable: %v", sanitizeReceiptLogValue(session, 100), err)
			}
			cache[session] = messages
		}
		if canonical := o.resolved[session]; canonical != "" && r.CanonicalSessionID == "" {
			r.CanonicalSessionID, r.Phase = canonical, "ingestion_target"
			if err := o.cfg.inboundJournal.append(r); err != nil {
				return err
			}
		}
		var proof *ingestionEvidence
		for _, raw := range messages {
			if proof = ingestionProof(raw, r); proof != nil {
				break
			}
		}
		if proof != nil {
			if r.CanonicalSessionID != "" {
				proof.SessionID = r.CanonicalSessionID
			}
			r.Status, r.Phase, r.Evidence = "ingested", "ingestion", proof
			if err := o.cfg.inboundJournal.append(r); err != nil {
				return err
			}
			continue
		}
		if o.now().Before(r.DeliveredAt.Add(ingestionWindow)) {
			continue
		}
		if r.Status != "delivered_unseen" {
			r.Status, r.Phase = "delivered_unseen", "ingestion"
			if err := o.cfg.inboundJournal.append(r); err != nil {
				return err
			}
		}
		alertKey := ingestionKey(r, "")
		if !r.Alerted && !alerted[alertKey] {
			mailCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = o.notify(mailCtx, r)
			cancel()
			if err != nil {
				failures = append(failures, fmt.Errorf("unseen alert failed: %w", err))
				continue
			}
		}
		alerted[alertKey] = true
		r.Alerted, r.Phase = true, "ingestion_alert"
		if err := o.cfg.inboundJournal.append(r); err != nil {
			return err
		}
	}
	return errors.Join(failures...)
}

func (o *ingestionObserver) fetchTranscript(ctx context.Context, session string) ([]json.RawMessage, error) {
	base := fmt.Sprintf("%s/v0/city/%s/session/", o.cfg.gcAPIBase, url.PathEscape(o.cfg.cityName))
	canonical := session
	var messages []json.RawMessage
	remaining := int64(ingestionReadLimit)
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 64; page++ {
		q := url.Values{"format": {"raw"}, "tail": {"1"}}
		if cursor != "" {
			q.Set("before", cursor)
		}
		b, err := ingestionRead(ctx, base+url.PathEscape(canonical)+"/transcript?"+q.Encode(), remaining)
		if err != nil {
			return messages, err
		}
		remaining -= int64(len(b))
		var result struct {
			ID         string            `json:"id"`
			Format     string            `json:"format"`
			Messages   []json.RawMessage `json:"messages"`
			Pagination struct {
				HasOlder bool   `json:"has_older_messages"`
				Before   string `json:"truncated_before_message"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(b, &result); err != nil {
			return messages, err
		}
		if result.ID != canonical && page == 0 {
			// Name bindings are supported by gc; authenticate their resolution
			// separately, then pin all transcript pages to the canonical ID.
			body, err := ingestionRead(ctx, base+url.PathEscape(session), remaining)
			if err != nil {
				return messages, err
			}
			remaining -= int64(len(body))
			var info struct {
				ID    string `json:"id"`
				Alias string `json:"alias"`
				Name  string `json:"session_name"`
			}
			if json.Unmarshal(body, &info) == nil && info.ID == result.ID && info.ID != "" && (info.ID == session || info.Alias == session || info.Name == session) {
				canonical = info.ID
			}
		}
		if result.ID == "" || result.ID != canonical || result.Format != "raw" {
			return messages, fmt.Errorf("transcript identity/format mismatch")
		}
		if o.resolved == nil {
			o.resolved = map[string]string{}
		}
		o.resolved[session] = canonical
		messages = append(messages, result.Messages...)
		if !result.Pagination.HasOlder {
			return messages, nil
		}
		cursor = result.Pagination.Before
		if cursor == "" || seen[cursor] {
			return messages, fmt.Errorf("transcript older-page cursor missing or repeated")
		}
		seen[cursor] = true
	}
	return messages, fmt.Errorf("transcript exceeds 64-page observation budget")
}

func ingestionRead(ctx context.Context, u string, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("transcript exceeds %d bytes", ingestionReadLimit)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-GC-Request", "gc-slack-ingestion-observer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("transcript HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("transcript exceeds %d bytes", ingestionReadLimit)
	}
	return b, nil
}

func (o *ingestionObserver) mailUnseen(ctx context.Context, r inboundJournalRecord) error {
	// Fanout members share one mailbox alert for this Slack receipt.
	key := ingestionDigest(ingestionKey(r, ""))
	body := fmt.Sprintf("Slack delivered_unseen: channel=%s message_ts=%s thread_ts=%s receipt=%s session=%s. No complete rendered session payload was found within %s of hand-off %s. Read this Slack thread now. Transport delivery is not ingestion; the dead-letter file is unchanged.", r.Channel, r.MessageTS, r.ThreadTS, r.ReceiptID, r.TargetSessions[0], ingestionWindow, r.DeliveredAt.UTC().Format(time.RFC3339))
	cmd := exec.CommandContext(ctx, "gc", "--city", o.cfg.cityPath, "mail", "send", "mayor", "--dedup", "slack-ingestion:"+key, "-s", "Slack delivered_unseen", "-m", body)
	// Do not log CLI output, which may contain mailbox or provider data.
	return cmd.Run()
}

// Accept actual user input and rendered queued-command attachments only.
// Queue enqueue/remove, API snapshots, assistant quotations and tool results
// (including a manual Slack read) are not evidence that an injection surfaced.
func ingestionProof(raw json.RawMessage, r inboundJournalRecord) *ingestionEvidence {
	var entry struct {
		Type        string    `json:"type"`
		UUID        string    `json:"uuid"`
		Timestamp   time.Time `json:"timestamp"`
		IsSidechain bool      `json:"isSidechain"`
		Message     struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		Attachment struct {
			Type string `json:"type"`
		} `json:"attachment"`
		Rendered []struct {
			Content string `json:"content"`
		} `json:"rendered"`
		Payload struct {
			Type    string `json:"type"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"payload"`
	}
	if json.Unmarshal(raw, &entry) != nil || entry.IsSidechain || entry.Timestamp.IsZero() {
		return nil
	}
	// The receipt may wait for gc's bounded fanout before returning. Claude's
	// attachment timestamp is input creation time, a few seconds before it.
	start := r.StartedAt
	if start.IsZero() {
		start = r.DeliveredAt
	}
	if entry.Timestamp.Before(start.Add(-30*time.Second)) || entry.Timestamp.After(r.DeliveredAt.Add(ingestionWindow)) {
		return nil
	}
	var texts []string
	if entry.Type == "response_item" && entry.Payload.Type == "message" && entry.Payload.Role == "user" {
		// Native Codex records have no UUID or ordinal. Hash the complete
		// record after removing insignificant JSON whitespace for a stable
		// evidence identifier across raw transcript formatting changes.
		var compact bytes.Buffer
		if json.Compact(&compact, raw) != nil {
			return nil
		}
		entry.UUID = "codex-sha256:" + ingestionDigest(compact.String())
		for _, b := range entry.Payload.Content {
			if b.Type == "input_text" || b.Type == "text" {
				texts = append(texts, b.Text)
			}
		}
	} else if entry.Type == "attachment" && entry.Attachment.Type == "queued_command" {
		for _, rendered := range entry.Rendered {
			texts = append(texts, rendered.Content)
		}
	} else if entry.Type == "user" && entry.Message.Role == "user" {
		var text string
		if json.Unmarshal(entry.Message.Content, &text) == nil {
			texts = append(texts, text)
		} else {
			var blocks []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(entry.Message.Content, &blocks) != nil {
				return nil
			}
			for _, b := range blocks {
				if b.Type == "text" {
					texts = append(texts, b.Text)
				}
			}
		}
	}
	if entry.UUID == "" {
		return nil
	}
	for _, text := range texts {
		if ingestionMatches(text, r) {
			return &ingestionEvidence{SessionID: r.TargetSessions[0], EntryID: entry.UUID, Timestamp: entry.Timestamp, Kind: entry.Type}
		}
	}
	return nil
}

func ingestionMatches(text string, r inboundJournalRecord) bool {
	const open = "<system-reminder>"
	const close = "</system-reminder>"
	provider := r.Provider
	if provider == "" {
		provider = "slack" // compatibility with journal rows before provider recording
	}
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], open)
		if i < 0 {
			break
		}
		i += offset
		offset = i + len(open) // also examine nested reminder starts
		end := strings.Index(text[offset:], close)
		if end < 0 {
			continue
		}
		block := text[i : offset+end+len(close)]
		if r.ProofKind == "session" && ingestionDigest(block) == r.PayloadSHA256 {
			return true
		}
		if r.ProofKind != "extmsg" {
			continue
		}
		header := open + "\nNew message in shared conversation " + provider + "/" + r.Channel + ":\n\n- "
		if !strings.HasPrefix(block, header) {
			continue
		}
		body := block[len(header):]
		footer := "\n\nMessage id: " + r.MessageTS
		if r.ThreadTS != "" {
			footer += " (in thread " + r.ThreadTS + ")"
		}
		footer += "\n\n"
		last := strings.LastIndex(body, footer)
		if last < 0 {
			continue
		}
		// Display names and message text may themselves contain actor-like
		// separators. The complete payload digest selects the real boundary.
		for _, kind := range []string{"human", "agent"} {
			separator := " (" + kind + "): "
			for from := 0; from < last; {
				n := strings.Index(body[from:last], separator)
				if n < 0 || strings.Contains(body[:from+n], "\n") {
					break
				}
				actorEnd := from + n + len(separator)
				if ingestionDigest(body[actorEnd:last]) == r.PayloadSHA256 {
					return true
				}
				from = actorEnd
			}
		}
	}
	return false
}
