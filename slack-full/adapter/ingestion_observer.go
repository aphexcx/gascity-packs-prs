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
	"syscall"
	"time"
)

const ingestionWindow = 3 * time.Minute

// The measured consolidator tool call lasts 60-90 minutes. Allow two hours
// from handoff for rendered input to appear, while still alerting at 3 minutes.
const ingestionLateWindow = 2 * time.Hour
const ingestionPollInterval = 5 * time.Second
const ingestionReadLimit = 32 << 20

// A crashed process cannot write the follow-up's terminal disposition. Keep
// initial timing through the bounded recovery ladder plus a grace window,
// without allowing an orphan acceptance to pin journal retention forever.
const ingestionPendingTimingLimit = (receiptFollowUpReposts+1)*receiptFollowUpDeadline + ingestionWindow

// These are session-side facts, not a transport receipt or model claim.
// No transcript text is persisted in the journal.
type ingestionEvidence struct {
	SessionID string              `json:"session_id"`
	EntryID   string              `json:"entry_id"`
	Timestamp time.Time           `json:"timestamp"`
	Kind      string              `json:"kind"`
	MessageTS string              `json:"message_ts,omitempty"`
	ThreadTS  string              `json:"thread_ts,omitempty"`
	Members   []ingestionEvidence `json:"members,omitempty"`
}

type ingestionObserver struct {
	cfg             config
	now             func() time.Time
	fetch           func(context.Context, string) ([]json.RawMessage, error)
	notify          func(context.Context, []inboundJournalRecord) error
	resolved        map[string]string
	preparedBatches []ingestionAlertBatch
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

func ingestionObserverDisabled() bool {
	return os.Getenv("SLACK_INGESTION_OBSERVER_DISABLED") == "1"
}

func startIngestionObserver(cfg config) func() {
	if cfg.inboundJournal == nil || ingestionObserverDisabled() {
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
	now := time.Now()
	if o.now != nil {
		now = o.now()
	}
	rows, _, _, err := j.ingestionStateLocked(now)
	o.preparedBatches = nil
	for _, batch := range j.preparedBatches {
		o.preparedBatches = append(o.preparedBatches, batch)
	}
	sort.Slice(o.preparedBatches, func(i, k int) bool {
		a, b := o.preparedBatches[i], o.preparedBatches[k]
		if a.Created.Equal(b.Created) {
			return a.ID < b.ID
		}
		return a.Created.Before(b.Created)
	})
	for key, row := range rows {
		needsCompletion := row.Status == "delivered_unseen" && row.Alerted && row.Phase != "ingestion_alert"
		if ingestionSettled(row, now) && !needsCompletion {
			delete(rows, key)
		}
	}
	return rows, err
}

func ingestionSettled(r inboundJournalRecord, now time.Time) bool {
	// Batch completion proves mail acceptance, but retention must preserve
	// the source until its individual completion row is durable as well.
	return r.Status == "ingested" || (r.Status == "delivered_unseen" && r.Alerted && r.Phase == "ingestion_alert" && !now.Before(r.DeliveredAt.Add(ingestionLateWindow)))
}

type ingestionRecordDays struct {
	selected string // segment containing the selected member verdict
	latest   string // newest segment containing any row for this member
}

// Shared with retention pruning. The caller holds j.mu so pending evidence
// cannot be removed between reconstruction and the pruning decision. Keep
// settled rows too: newer transport rows must not outlive their verdicts.
// receiptStart protects held receipts independently of member observation.
func (j *inboundJournal) ingestionStateLocked(pruningAt time.Time) (map[string]inboundJournalRecord, map[string]ingestionRecordDays, time.Time, error) {
	if ingestionObserverDisabled() {
		// With no observer to settle handoffs, use ordinary journal retention.
		// Do not let unresolved rows pin disabled-mode history indefinitely.
		return nil, nil, time.Time{}, nil
	}
	paths, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	sort.Strings(paths)
	j.sessionActs = make(map[string]ingestionSessionAct)
	j.sessionActsOverflow = false
	j.preparedBatches = make(map[string]ingestionAlertBatch)
	j.coverageDays = make(map[ingestionAlertCoverage]string)
	batchDays := make(map[string]string)
	attemptBefore := make(map[string]bool)
	rows := make(map[string]inboundJournalRecord)
	days := make(map[string]ingestionRecordDays)
	alerted := make(map[string]bool)
	coverage := make(map[string][]ingestionAlertCoverage)
	rememberCoverage := func(members []ingestionAlertCoverage, batchID, sourceDay string) {
		for _, c := range members {
			if c.BatchID == "" {
				c.BatchID = batchID
			}
			if sourceDay > j.coverageDays[c] {
				j.coverageDays[c] = sourceDay
			}
			coverage[c.Receipt] = mergeIngestionCoverage(coverage[c.Receipt], []ingestionAlertCoverage{c})
		}
	}
	started := make(map[string]time.Time)
	held := make(map[string]bool)
	concluded := make(map[string]bool)
	skipped := 0
	defer func() {
		if skipped > 0 {
			log.Printf("ingestion observer: skipped %d malformed journal line(s)", skipped)
		}
	}()
	remaining := int64(64 << 20)
	for _, path := range paths {
		day := strings.SplitN(filepath.Base(path), ".", 2)[0]
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		remaining -= info.Size()
		if remaining < 0 {
			return nil, nil, time.Time{}, fmt.Errorf("journal exceeds 64 MiB observation budget")
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, time.Time{}, err
		}
		s := bufio.NewScanner(f)
		s.Buffer(make([]byte, 4096), 1<<20)
		for s.Scan() {
			var r inboundJournalRecord
			if err := json.Unmarshal(s.Bytes(), &r); err != nil {
				skipped++
				continue
			}
			if r.SessionAct != nil {
				j.cacheSessionActLocked(*r.SessionAct)
				continue
			}
			if r.AlertBatch != nil {
				batch := *r.AlertBatch
				if r.Phase == "ingestion_batch" {
					batch.attempted = batch.Version < ingestionAlertAttemptVersion
					j.preparedBatches[batch.ID] = batch
					batchDays[batch.ID] = day
				} else if r.Phase == "ingestion_batch_attempt" {
					if saved, ok := j.preparedBatches[batch.ID]; ok {
						attemptBefore[batch.ID] = saved.attempted
						saved.attempted = true
						j.preparedBatches[batch.ID] = saved
					}
				} else if r.Phase == "ingestion_batch_refused" {
					if saved, ok := j.preparedBatches[batch.ID]; ok {
						// Refusing this retry cannot disprove an older attempt.
						saved.attempted = attemptBefore[batch.ID]
						j.preparedBatches[batch.ID] = saved
						delete(attemptBefore, batch.ID)
					}
				} else if r.Phase == "ingestion_batch_done" {
					sourceDay := day
					if batch.All {
						batch.Members = j.preparedBatches[batch.ID].Members
						batch.Coverage = j.preparedBatches[batch.ID].Coverage
						// All references the plan's bytes. A surviving completion
						// cannot replace an expiring plan as a coverage source.
						sourceDay = batchDays[batch.ID]
					}
					delete(j.preparedBatches, batch.ID)
					delete(batchDays, batch.ID)
					delete(attemptBefore, batch.ID)
					if batch.Sent || batch.Unconfirmed {
						if batch.Version >= ingestionAlertCoverageVersion {
							rememberCoverage(batch.Coverage, batch.ID, sourceDay)
						} else {
							for _, member := range batch.Members {
								alerted[member] = true
							}
						}
					}
				}
				continue
			}
			if r.Phase == "ingestion_coverage" {
				rememberCoverage(r.AlertCoverage, "", day)
				continue
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
			if r.Phase == "concluded" || r.Phase == receiptFollowUpEnded || receiptStatusPositive(r.Status) {
				concluded[identity] = true
			}
			if r.Status != "delivered" && r.Status != "ingested" && r.Status != "delivered_unseen" && len(r.HandoffSessions) == 0 {
				continue
			}
			rememberCoverage(r.AlertCoverage, r.AlertBatchID, day)
			if r.Alerted && r.AlertVersion < ingestionAlertCoverageVersion {
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
				recordDays := ingestionRecordDays{selected: day, latest: day}
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
					completion := copy.Status == "delivered_unseen" && copy.Alerted && copy.Phase == "ingestion_alert"
					if old.Status == "ingested" ||
						(old.Status == "delivered_unseen" && copy.Status != "ingested" && !completion) ||
						(copy.Status != "ingested" && copy.Status != "delivered_unseen" && old.TS.After(copy.TS)) {
						copy = old
						recordDays.selected = days[key].selected
					}
					copy.DeliveredAt = handoff
					if canonical != "" {
						copy.CanonicalSessionID = canonical
					}
				}
				rows[key] = copy
				days[key] = recordDays
			}
		}
		err = s.Err()
		f.Close()
		if err != nil {
			return nil, nil, time.Time{}, err
		}
	}
	for key, r := range rows {
		r.StartedAt = started[ingestionKey(r, "")]
		r.AlertCoverage = mergeIngestionCoverage(r.AlertCoverage, coverage[ingestionKey(r, "")])
		if len(r.AlertCoverage) > 0 {
			r.AlertVersion = ingestionAlertCoverageVersion
			if ingestionAllMessagesAlerted(r) {
				r.Alerted = true
			}
		}
		if alerted[ingestionKey(r, "")] {
			r.Alerted = true
			// Keep the legacy receipt-wide meaning when retention carries
			// this verdict beyond the original unversioned completion row.
			r.AlertVersion = 0
		}
		rows[key] = r
	}
	var receiptStart time.Time
	for identity := range held {
		if !concluded[identity] && (pruningAt.IsZero() || pruningAt.Before(started[identity].Add(ingestionPendingTimingLimit))) {
			receiptStart = earlierIngestionTime(receiptStart, started[identity])
		}
	}
	return rows, days, receiptStart, nil
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
	batches := make(map[string][]inboundJournalRecord)
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
		if r.Alerted && r.Status == "delivered_unseen" && r.Phase != "ingestion_alert" {
			// A durable batch completion may outlive a crash during its
			// per-member appends. Restore those rows without another mail,
			// including after the normal late-proof horizon has expired.
			r.Phase = "ingestion_alert"
			if err := o.cfg.inboundJournal.append(r); err != nil {
				return err
			}
			if ingestionSettled(r, o.now()) {
				continue
			}
		}
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
		if proof == nil {
			proof, _, err = o.cfg.inboundJournal.sessionActProof(r)
			if err != nil {
				return err
			}
		}
		if proof != nil {
			if err := o.markIngested(r, proof); err != nil {
				return err
			}
			continue
		}
		if o.now().Before(r.DeliveredAt.Add(ingestionWindow)) {
			continue
		}
		if r.Alerted && r.Status == "delivered_unseen" {
			continue // Keep looking for late proof without another alert or row.
		}
		if r.Status != "delivered_unseen" {
			r.Status, r.Phase = "delivered_unseen", "ingestion"
			if err := o.cfg.inboundJournal.append(r); err != nil {
				return err
			}
		}
		if !r.Alerted {
			batches[r.Channel] = append(batches[r.Channel], r)
		}
	}
	channels := make([]string, 0, len(batches))
	for _, batch := range o.preparedBatches {
		if _, exists := batches[batch.Channel]; !exists {
			batches[batch.Channel] = nil
		}
	}
	for channel := range batches {
		channels = append(channels, channel)
	}
	sort.Strings(channels)
	for _, channel := range channels {
		var unseen []inboundJournalRecord
		for _, r := range batches[channel] {
			// Transcript fetches for other targets may have taken time. Observe
			// successful acts arriving during this pass before sending mail.
			proof, missing, err := o.cfg.inboundJournal.sessionActProof(r)
			if err != nil {
				return err
			}
			if proof != nil {
				if err := o.markIngested(r, proof); err != nil {
					return err
				}
				continue
			}
			r.UnseenWindows = make([]injectionWindow, 0, len(missing))
			for _, w := range missing {
				if !ingestionMessageAlerted(r, w) {
					r.UnseenWindows = append(r.UnseenWindows, w)
				}
			}
			if len(r.UnseenWindows) == 0 {
				if err := o.recordAlertResult(r, nil); err != nil {
					return err
				}
				continue
			}
			unseen = append(unseen, r)
		}
		batch, unseen, err := o.prepareAlertBatch(channel, unseen)
		if err != nil {
			failures = append(failures, fmt.Errorf("prepare unseen alert: %w", err))
			continue
		}
		if len(unseen) == 0 {
			continue
		}
		for i := range unseen {
			unseen[i].AlertBatchID = batch.ID
		}
		attempt := ingestionAlertBatch{ID: batch.ID, Version: batch.Version}
		if !batch.recovered {
			if err := o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch_attempt", AlertBatch: &attempt}); err != nil {
				return err
			}
		}
		// An outstanding ambiguous attempt already fences recovery. Reuse it
		// on retries: no additional marker can make that acceptance more certain.
		mailCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = o.notify(mailCtx, ingestionAlertRows(unseen))
		cancel()
		if err != nil {
			if !batch.recovered && ingestionNotifyDefinitelyRefused(err) {
				if recordErr := o.cfg.inboundJournal.append(inboundJournalRecord{Phase: "ingestion_batch_refused", AlertBatch: &attempt}); recordErr != nil {
					return errors.Join(err, recordErr)
				}
			}
			failures = append(failures, fmt.Errorf("unseen alert failed: %w", err))
			continue
		}
		if err := o.finishAlertBatch(batch, unseen, true); err != nil {
			return err
		}
		for _, r := range unseen {
			if err := o.recordAlertResult(r, ingestionCoverageFor([]inboundJournalRecord{r})); err != nil {
				return err
			}
		}
	}
	return errors.Join(failures...)
}

func (o *ingestionObserver) markIngested(r inboundJournalRecord, proof *ingestionEvidence) error {
	if r.CanonicalSessionID != "" {
		proof.SessionID = r.CanonicalSessionID
	}
	r.Late = !o.now().Before(r.DeliveredAt.Add(ingestionWindow))
	r.Status, r.Phase, r.Evidence = "ingested", "ingestion", proof
	if proof.Kind == "reaction" || proof.Kind == "reply" {
		r.ProofKind = proof.Kind
	}
	return o.cfg.inboundJournal.append(r)
}

// One line per receipt, even when several fanout targets lack proof. Sorting
// both receipts and sessions makes the set's mail dedup key deterministic.
func ingestionAlertRows(rows []inboundJournalRecord) []inboundJournalRecord {
	unique := make(map[string]inboundJournalRecord)
	for _, r := range rows {
		key := ingestionKey(r, "")
		if old, ok := unique[key]; ok {
			old.AlertSelection = append(old.AlertSelection, ingestionCoverageFor([]inboundJournalRecord{r})...)
			old.TargetSessions = append(old.TargetSessions, r.TargetSessions...)
			old.UnseenWindows = append(ingestionAlertWindows(old), ingestionAlertWindows(r)...)
			unique[key] = old
		} else {
			r.TargetSessions = append([]string(nil), r.TargetSessions...)
			r.AlertSelection = ingestionCoverageFor([]inboundJournalRecord{r})
			unique[key] = r
		}
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]inboundJournalRecord, 0, len(keys))
	for _, key := range keys {
		r := unique[key]
		sort.Strings(r.TargetSessions)
		result = append(result, r)
	}
	return result
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

func (o *ingestionObserver) mailUnseen(ctx context.Context, rows []inboundJournalRecord) error {
	rows = ingestionAlertRows(rows)
	if len(rows) == 0 {
		return nil
	}
	key := rows[0].AlertBatchID
	if key == "" {
		key = ingestionAlertID(ingestionAlertMembers(rows))
	}
	subject, body := ingestionAlertMessage(rows)
	if len(body) > ingestionAlertBatchBudget {
		return ingestionNotifyRefusal{fmt.Errorf("ingestion alert exceeds %d-byte mail budget", ingestionAlertBatchBudget)}
	}
	if err := ctx.Err(); err != nil {
		return ingestionNotifyRefusal{err} // No child process was started.
	}
	cmd := exec.CommandContext(ctx, "gc", "--city", o.cfg.cityPath, "mail", "send", "mayor", "--dedup", "slack-ingestion:"+key, "-s", subject, "-m", body)
	// Do not log CLI output, which may contain mailbox or provider data.
	// Output retains bounded stderr on ExitError for refusal classification.
	_, err := cmd.Output()
	if ctx.Err() != nil {
		return ctx.Err() // Cancellation after start leaves acceptance ambiguous.
	}
	return err
}

type ingestionNotifyRefusal struct{ error }

func ingestionNotifyDefinitelyRefused(err error) bool {
	var refused ingestionNotifyRefusal
	if errors.As(err, &refused) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true // The connection was never established.
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() < 0 {
			return false // Signal termination may have followed acceptance.
		}
		output := strings.ToLower(string(exit.Stderr))
		for _, uncertain := range []string{"connection reset", "broken pipe", "eof", "timeout", "timed out", "deadline exceeded"} {
			if strings.Contains(output, uncertain) {
				return false // gc may have sent its request before losing the response.
			}
		}
		return true // gc completed and returned a nonzero status.
	}
	var start *exec.Error
	var path *os.PathError
	return errors.As(err, &start) || errors.As(err, &path)
}

func ingestionAlertMessage(rows []inboundJournalRecord) (string, string) {
	subject, body := ingestionAlertMessageWithBudget(rows, 0)
	if len(body) <= ingestionAlertBatchBudget {
		return subject, body
	}
	return ingestionAlertMessageWithBudget(rows, ingestionAlertBatchBudget)
}

// The union across unresolved fanout targets, in deterministic message order.
func ingestionAlertWindows(r inboundJournalRecord) []injectionWindow {
	windows := r.UnseenWindows
	if windows == nil {
		windows = ingestionMessageWindows(r)
	}
	unique := make(map[string]injectionWindow)
	for _, w := range windows {
		unique[w.MessageTS+"\x00"+w.ThreadTS] = w
	}
	keys := make([]string, 0, len(unique))
	for key := range unique {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]injectionWindow, 0, len(keys))
	for _, key := range keys {
		result = append(result, unique[key])
	}
	return result
}

// A zero budget measures a new batch without truncation. Saved legacy batches
// may be larger; summarize those under their original dedup identity instead
// of permanently failing the retry or inventing subset identities.
func ingestionAlertMessageWithBudget(rows []inboundJournalRecord, budget int) (string, string) {
	if len(rows) == 0 {
		return "", ""
	}
	windows := make([][]injectionWindow, len(rows))
	count := 0
	for i, r := range rows {
		windows[i] = ingestionAlertWindows(r)
		count += len(windows[i])
	}
	subject := fmt.Sprintf("Slack delivered_unseen: %d lines in %s", count, rows[0].Channel)
	footer := fmt.Sprintf("No rendered input, explicit session reaction, or session reply was found within %s of hand-off. Read these Slack threads now. Transport delivery is not ingestion; the dead-letter file is unchanged.", ingestionWindow)
	key := rows[0].AlertBatchID
	if key == "" {
		key = ingestionAlertID(ingestionAlertMembers(rows))
	}
	omitted := func(n int) string {
		return fmt.Sprintf("%d additional lines omitted from this mail; full receipt membership is in the inbound journal ingestion_batch id=%s.\n", n, key)
	}
	var body strings.Builder
	body.WriteString(subject + "\n")
	written := 0
writeLines:
	for i, r := range rows {
		// Session fanout can grow after preparation. Only immutable receipt
		// member coordinates belong in the retry payload; targets stay in the journal.
		for _, w := range windows[i] {
			line := fmt.Sprintf("message_ts=%s thread_ts=%s receipt=%s\n", w.MessageTS, w.ThreadTS, r.ReceiptID)
			if budget > 0 && body.Len()+len(line)+len(footer)+len(omitted(count-written)) > budget {
				body.WriteString(omitted(count - written))
				break writeLines
			}
			body.WriteString(line)
			written++
		}
	}
	body.WriteString(footer)
	return subject, body.String()
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
	if entry.Timestamp.Before(start.Add(-30*time.Second)) || entry.Timestamp.After(r.DeliveredAt.Add(ingestionLateWindow)) {
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
