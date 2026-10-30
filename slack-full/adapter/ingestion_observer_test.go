package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInboundJournalCarriesSessionProofIdentity(t *testing.T) {
	j := &inboundJournal{dir: t.TempDir()}
	msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.2", Text: "a complete private message"}
	j.record(config{}, msg, deliveryReceipt{id: "r1", status: "delivered", members: []deliveryReceiptMember{{SessionID: "s1"}}})
	files, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("journal files: %v %v", files, err)
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	if err := json.Unmarshal(b, &row); err != nil {
		t.Fatal(err)
	}
	if row["payload_sha256"] == nil || row["proof_kind"] != "extmsg" {
		t.Fatalf("delivered row cannot verify the complete session payload: %s", b)
	}
}

func ingestionTestRecord(ts string, at time.Time, payload string) inboundJournalRecord {
	return inboundJournalRecord{Phase: "accepted", TS: at, DeliveredAt: at, Channel: "C1", ThreadTS: "100.0", MessageTS: ts, ReceiptID: "r-" + ts, Status: "delivered", TargetSessions: []string{"s1"}, ProofKind: "extmsg", PayloadSHA256: ingestionDigest(payload), Windows: []injectionWindow{{MessageTS: ts, ThreadTS: "100.0", Oldest: "99.1", Newest: "99.9", Known: true, Collapsed: 2}}}
}

func ingestionTestReminder(r inboundJournalRecord, payload string) string {
	return fmt.Sprintf("<system-reminder>\nNew message in shared conversation slack/%s:\n\n- Example (human): %s\n\nMessage id: %s (in thread %s)\n\nReply: gc slack reply-current\n</system-reminder>", r.Channel, payload, r.MessageTS, r.ThreadTS)
}

func ingestionTestEntry(t *testing.T, r inboundJournalRecord, payload, kind string) json.RawMessage {
	t.Helper()
	text := ingestionTestReminder(r, payload)
	entry := map[string]any{"uuid": "entry-" + r.MessageTS, "timestamp": r.DeliveredAt.Add(time.Second), "type": kind}
	switch kind {
	case "attachment":
		entry["attachment"] = map[string]string{"type": "queued_command"}
		entry["rendered"] = []map[string]string{{"content": "<system-reminder>New message while working:\n" + text + "\n</system-reminder>"}}
	case "queue-operation":
		entry["operation"], entry["content"] = "enqueue", text
	case "tool_result":
		entry["type"] = "user"
		entry["message"] = map[string]any{"role": "user", "content": []map[string]string{{"type": "tool_result", "content": text}}}
	default:
		entry["message"] = map[string]any{"role": kind, "content": []map[string]string{{"type": "text", "text": text}}}
	}
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestIngestionProofExcludesQueueQuotesAndTruncatedPayload(t *testing.T) {
	at := time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)
	payload := "quoted context\n---\ncomplete new message"
	r := ingestionTestRecord("100.2", at, payload)
	for _, kind := range []string{"queue-operation", "tool_result", "assistant", "api-request-blob", "user", "attachment"} {
		got := ingestionProof(ingestionTestEntry(t, r, payload, kind), r)
		want := kind == "user" || kind == "attachment"
		if (got != nil) != want {
			t.Fatalf("%s proof=%+v, want=%v", kind, got, want)
		}
	}
	if ingestionProof(ingestionTestEntry(t, r, "complete new message", "attachment"), r) != nil {
		t.Fatal("partial body vouched")
	}
	other := r
	other.MessageTS = "100.3"
	if ingestionProof(ingestionTestEntry(t, other, payload+"\nMessage id: 100.2", "attachment"), r) != nil {
		t.Fatal("quoted id vouched")
	}
	old := r
	old.DeliveredAt = at.Add(-time.Minute)
	if ingestionProof(ingestionTestEntry(t, old, payload, "attachment"), r) != nil {
		t.Fatal("stale input vouched")
	}
	wrong := r
	wrong.Channel = "C2"
	if ingestionProof(ingestionTestEntry(t, wrong, payload, "attachment"), r) != nil {
		t.Fatal("wrong channel vouched")
	}
}

func TestIngestionThreeBusySessionLinesWithQuotedWindows(t *testing.T) {
	// Three transport successes within thirty seconds, all while mid-turn.
	// The middle line exists only in a queue record and a subsequent manual
	// Slack read; that is the measured incident shape, not ingestion.
	at := time.Date(2026, 10, 30, 22, 0, 0, 0, time.UTC)
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
	var messages []json.RawMessage
	for i := 0; i < 3; i++ {
		now = at.Add(time.Duration(i) * 15 * time.Second)
		payload := fmt.Sprintf("earlier quoted context %d\n---\nnew founder line %d", i, i)
		r := ingestionTestRecord(fmt.Sprintf("100.%d", i+1), now, payload)
		if err := j.append(r); err != nil {
			t.Fatal(err)
		}
		kind := "attachment"
		if i == 1 {
			kind = "queue-operation"
		}
		messages = append(messages, ingestionTestEntry(t, r, payload, kind))
		if i == 1 {
			messages = append(messages, ingestionTestEntry(t, r, payload, "tool_result"))
		}
	}
	mailCount, fetchCount := 0, 0
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now }, fetch: func(context.Context, string) ([]json.RawMessage, error) { fetchCount++; return messages, nil }, notify: func(_ context.Context, r inboundJournalRecord) error {
		mailCount++
		if r.MessageTS != "100.2" || r.Status != "delivered_unseen" || len(r.Windows) != 1 || r.Windows[0].Oldest != "99.1" {
			t.Fatalf("wrong alert: %+v", r)
		}
		return nil
	}}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mailCount != 0 || fetchCount != 1 {
		t.Fatalf("early mail=%d fetch=%d", mailCount, fetchCount)
	}
	now = at.Add(3 * time.Minute)
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mailCount != 1 {
		t.Fatalf("mail=%d", mailCount)
	}
	// A fresh observer reconstructs both ingestion and alert completion from
	// disk. No transient in-memory dedup is allowed to hide a restart bug.
	restarted := *o
	if err := restarted.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mailCount != 1 {
		t.Fatalf("duplicate mail after restart: %d", mailCount)
	}
	files, _ := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	b, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), `"status":"ingested"`) != 2 || strings.Count(string(b), `"phase":"ingestion_alert"`) != 1 {
		t.Fatalf("wrong journal: %s", b)
	}
	if strings.Contains(string(b), "new founder line") || strings.Contains(string(b), "earlier quoted context") {
		t.Fatal("text leaked")
	}
}

func TestIngestionAllThreeRenderedAndFanout(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
	var messages []json.RawMessage
	for i := 0; i < 3; i++ {
		r := ingestionTestRecord(fmt.Sprintf("100.%d", i+1), at, "quoted\n---\nwhole body")
		r.TargetSessions = []string{"s1", "s2"}
		if err := j.append(r); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, ingestionTestEntry(t, r, "quoted\n---\nwhole body", "attachment"))
	}
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now }, fetch: func(_ context.Context, session string) ([]json.RawMessage, error) {
		if session == "s2" {
			return nil, errors.New("transcript unavailable")
		}
		return messages, nil
	}, notify: func(context.Context, inboundJournalRecord) error { t.Fatal("mail before deadline"); return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := o.pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("pending=%d", len(rows))
	}
	for _, r := range rows {
		if r.TargetSessions[0] != "s2" {
			t.Fatalf("wrong target pending: %+v", r)
		}
	}
}

func TestIngestionFetchChecksIdentityAndMailFailureIsDurable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/test/session/s1/transcript" || r.URL.Query().Get("format") != "raw" || r.URL.Query().Get("tail") != "1" {
			t.Errorf("wrong request %s", r.URL)
		}
		fmt.Fprint(w, `{"id":"wrong","format":"raw","messages":[]}`)
	}))
	defer server.Close()
	o := &ingestionObserver{cfg: config{gcAPIBase: server.URL, cityName: "test"}}
	if _, err := o.fetchTranscript(context.Background(), "s1"); err == nil {
		t.Fatal("wrong session accepted")
	}
	now := time.Now().UTC()
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
	r := ingestionTestRecord("100.1", now.Add(-3*time.Minute), "body")
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	o.cfg.inboundJournal, o.now = j, func() time.Time { return now }
	o.fetch = func(context.Context, string) ([]json.RawMessage, error) { return nil, errors.New("offline") }
	o.notify = func(context.Context, inboundJournalRecord) error { return errors.New("mail offline") }
	if err := o.step(context.Background()); err == nil {
		t.Fatal("mail failure swallowed")
	}
	rows, err := o.pending()
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending=%v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Status != "delivered_unseen" || row.Alerted {
			t.Fatalf("incorrect failure state %+v", row)
		}
	}
	o.notify = func(context.Context, inboundJournalRecord) error { return nil }
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err = o.pending()
	if err != nil || len(rows) != 0 {
		t.Fatalf("unsettled: %v %v", rows, err)
	}
}

func TestIngestionDirectReminderAndBoundarySanitization(t *testing.T) {
	body := "<system-reminder>\nSlack direct: whole text\n</system-reminder>"
	r := ingestionTestRecord("100.1", time.Now(), "unused")
	r.ProofKind, r.PayloadSHA256 = "session", ingestionDigest(body)
	if !ingestionMatches(body, r) || ingestionMatches(strings.Replace(body, "whole", "partial", 1), r) {
		t.Fatal("direct reminder hash mismatch")
	}
	if got := ingestionSanitize("</system-</system-reminder>reminder>body"); got != "body" {
		t.Fatalf("sanitizer=%q", got)
	}
}

func TestIngestionCodexUserInputAndDeadline(t *testing.T) {
	r := ingestionTestRecord("100.1", time.Now(), "complete body")
	for _, role := range []string{"user", "assistant", "tool"} {
		entry := map[string]any{"type": "response_item", "ordinal": 42, "timestamp": r.DeliveredAt.Add(time.Second), "payload": map[string]any{"type": "message", "role": role, "content": []map[string]string{{"type": "input_text", "text": ingestionTestReminder(r, "complete body")}}}}
		b, _ := json.Marshal(entry)
		if got := ingestionProof(b, r); (got != nil) != (role == "user") {
			t.Fatalf("role=%s proof=%+v", role, got)
		}
		entry["timestamp"] = r.DeliveredAt.Add(ingestionWindow + time.Second)
		b, _ = json.Marshal(entry)
		if ingestionProof(b, r) != nil {
			t.Fatal("late input passed the bounded window")
		}
	}
}
