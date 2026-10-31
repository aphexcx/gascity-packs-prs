package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func ingestionJournalRows(t *testing.T, j *inboundJournal) []inboundJournalRecord {
	t.Helper()
	j.mu.Lock()
	defer j.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(j.dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []inboundJournalRecord
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var row inboundJournalRecord
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func TestIngestionConcludedPendingAndMixedFanout(t *testing.T) {
	j := &inboundJournal{dir: t.TempDir()}
	msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.2", Text: "whole body"}
	r := deliveryReceipt{id: "r1", status: "partial", members: []deliveryReceiptMember{
		{SessionID: "s1", Status: "delivered"},
		{SessionID: "s2", Status: "pending", Delivered: 50, Expected: 50, DeliveredOK: true, ExpectedOK: true, DeliveredUnit: "bytes", ExpectedUnit: "bytes"},
		{SessionID: "s3", Status: "pending", Delivered: 10, Expected: 50, DeliveredOK: true, ExpectedOK: true, DeliveredUnit: "bytes", ExpectedUnit: "bytes"},
		{SessionID: "s4", Status: "failed"},
	}}
	j.record(config{}, msg, r, "concluded")
	o := &ingestionObserver{cfg: config{inboundJournal: j}}
	rows, err := o.pending()
	if err != nil {
		t.Fatal(err)
	}
	targets := map[string]bool{}
	for _, row := range rows {
		targets[row.TargetSessions[0]] = true
	}
	if len(targets) != 2 || !targets["s1"] || !targets["s2"] {
		t.Fatalf("completed handoffs must be monitored individually, got %v", targets)
	}
}

func TestIngestionHeldAliasKeepsDirectProof(t *testing.T) {
	stub := &heldRouterStub{}
	base := stub.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/extmsg/inbound/receipts/") {
			fmt.Fprint(w, `{"receipt_id":"ir-9-1","state":"concluded","delivery":{"receipt_id":"ir-9-1","status":"delivered","members":[{"session_id":"sess-mayor-1","status":"delivered"}]}}`)
			return
		}
		base(w, r)
	}))
	defer server.Close()
	cfg := receiptClaimsConfig(server.URL)
	cfg.bindingCheck = newBindingCheckCache()
	cfg.inboundJournal = &inboundJournal{dir: t.TempDir()}
	cfg.receiptFollowUps = fastFollowUps(cfg)
	alias := newTestHandleAliasRegistry(t)
	if err := alias.Set("mayor", "sess-mayor-1"); err != nil {
		t.Fatal(err)
	}
	env := botMentionEnvelope(t, "message", "Ev-ingestion-alias", "C1", "100.000600", "", "@mayor: please handle this", true)
	processSlackEvent(cfg, alias, nil, nil, nil, nil, env, func() {})
	waitFor(t, "concluded alias journal", func() bool {
		for _, row := range ingestionJournalRows(t, cfg.inboundJournal) {
			if row.ReceiptID == "ir-9-1" && row.Phase == "concluded" {
				return true
			}
		}
		return false
	})
	if !cfg.receiptFollowUps.wait(3 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	injected, _ := stub.snapshot()
	if len(injected) != 1 {
		t.Fatalf("alias sent %d times", len(injected))
	}
	for _, row := range ingestionJournalRows(t, cfg.inboundJournal) {
		if row.ReceiptID == "ir-9-1" && row.Phase == "concluded" {
			if row.ProofKind != "session" || !ingestionMatches(injected[0], row) {
				t.Fatalf("concluded alias lost its exact direct proof: kind=%s", row.ProofKind)
			}
		}
	}
}

func TestIngestionNamedSessionPinsCanonicalProof(t *testing.T) {
	now := time.Now().UTC()
	r := ingestionTestRecord("100.1", now, "body")
	r.TargetSessions = []string{"mayor"}
	entry := ingestionTestEntry(t, r, "body", "attachment")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v0/city/test/session/mayor" {
			fmt.Fprint(w, `{"id":"s1","alias":"mayor","session_name":"mayor"}`)
			return
		}
		if !strings.HasSuffix(req.URL.Path, "/transcript") {
			http.NotFound(w, req)
			return
		}
		fmt.Fprintf(w, `{"id":"s1","format":"raw","messages":[%s]}`, entry)
	}))
	defer server.Close()
	j := &inboundJournal{dir: t.TempDir()}
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	o := &ingestionObserver{cfg: config{inboundJournal: j, gcAPIBase: server.URL, cityName: "test"}, now: func() time.Time { return now }}
	o.fetch = o.fetchTranscript
	o.notify = func(context.Context, inboundJournalRecord) error { t.Fatal("unexpected alert"); return nil }
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Status == "ingested" && row.Evidence != nil && row.Evidence.SessionID == "s1" {
			return
		}
	}
	t.Fatal("named-session input has no canonical ingestion proof")
}

func TestIngestionProofBeforeCompactionIsRecovered(t *testing.T) {
	now := time.Now().UTC()
	r := ingestionTestRecord("100.1", now, "body")
	entry := ingestionTestEntry(t, r, "body", "attachment")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests++
		if req.URL.Query().Get("before") == "boundary" {
			fmt.Fprintf(w, `{"id":"s1","format":"raw","messages":[%s],"pagination":{"has_older_messages":false}}`, entry)
			return
		}
		fmt.Fprint(w, `{"id":"s1","format":"raw","messages":[{"uuid":"boundary","type":"system"}],"pagination":{"has_older_messages":true,"truncated_before_message":"boundary"}}`)
	}))
	defer server.Close()
	o := &ingestionObserver{cfg: config{gcAPIBase: server.URL, cityName: "test"}}
	entries, err := o.fetchTranscript(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range entries {
		if ingestionProof(raw, r) != nil && requests == 2 {
			return
		}
	}
	t.Fatalf("proof before compaction not recovered; requests=%d", requests)
}

func TestIngestionNameReassignmentBetweenDeliveries(t *testing.T) {
	at := time.Now().UTC()
	now := at
	first := ingestionTestRecord("100.1", at, "first body")
	second := ingestionTestRecord("100.2", at.Add(10*time.Second), "second body")
	first.TargetSessions, second.TargetSessions = []string{"mayor"}, []string{"mayor"}
	entries := map[string]json.RawMessage{
		"s1": ingestionTestEntry(t, first, "first body", "attachment"),
		"s2": ingestionTestEntry(t, second, "second body", "attachment"),
	}
	var generation atomic.Int32
	generation.Store(1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		id := fmt.Sprintf("s%d", generation.Load())
		if req.URL.Path == "/v0/city/test/session/mayor" {
			fmt.Fprintf(w, `{"id":%q,"alias":"mayor","session_name":"mayor"}`, id)
			return
		}
		if strings.Contains(req.URL.Path, "/session/s1/") {
			id = "s1"
		}
		fmt.Fprintf(w, `{"id":%q,"format":"raw","messages":[%s]}`, id, entries[id])
	}))
	defer server.Close()
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
	o := &ingestionObserver{cfg: config{inboundJournal: j, gcAPIBase: server.URL, cityName: "test"}, now: func() time.Time { return now }}
	o.fetch = o.fetchTranscript
	o.notify = func(context.Context, inboundJournalRecord) error { t.Fatal("unexpected unseen alert"); return nil }
	if err := j.append(first); err != nil {
		t.Fatal(err)
	}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	generation.Store(2)
	now = at.Add(10 * time.Second)
	if err := j.append(second); err != nil {
		t.Fatal(err)
	}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.MessageTS == second.MessageTS && row.Status == "ingested" && row.Evidence.SessionID == "s2" {
			return
		}
	}
	t.Fatal("new delivery followed the old name binding")
}

func TestIngestionRepeatedConclusionsCannotReopenOrDelay(t *testing.T) {
	for _, rendered := range []bool{false, true} {
		t.Run(fmt.Sprint("rendered=", rendered), func(t *testing.T) {
			at := time.Now().UTC()
			now := at
			r := ingestionTestRecord("100.1", at, "body")
			r.DeliveredAt = time.Time{} // transport rows do not set this field
			j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			reference := ingestionTestRecord("100.1", at, "body")
			var entries []json.RawMessage
			if rendered {
				entries = []json.RawMessage{ingestionTestEntry(t, reference, "body", "attachment")}
			}
			alerts := 0
			o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
				fetch:  func(context.Context, string) ([]json.RawMessage, error) { return entries, nil },
				notify: func(context.Context, inboundJournalRecord) error { alerts++; return nil }}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			// A later transport observation is neither a new delivery nor a
			// reason to forget session evidence already recorded on disk.
			now = at.Add(4 * time.Minute)
			r.Phase = "concluded"
			if err := j.append(r); err != nil {
				t.Fatal(err)
			}
			if rendered {
				rows, err := o.pending()
				if err != nil || len(rows) != 0 {
					t.Fatalf("completed ingestion reopened: rows=%d err=%v", len(rows), err)
				}
			} else {
				if err := o.step(context.Background()); err != nil {
					t.Fatal(err)
				}
				if alerts != 1 {
					t.Fatalf("later receipt postponed original deadline: alerts=%d", alerts)
				}
			}
		})
	}
}

func TestIngestionDelayedConclusionRetainsInputInterval(t *testing.T) {
	at := time.Now().UTC()
	now := at
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
	r := ingestionTestRecord("100.1", at, "body")
	r.DeliveredAt, r.Status = time.Time{}, "pending"
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	now = at.Add(2 * time.Minute)
	r.Status, r.Phase = "delivered", "concluded"
	if err := j.append(r); err != nil {
		t.Fatal(err)
	}
	reference := ingestionTestRecord("100.1", at, "body")
	o := &ingestionObserver{cfg: config{inboundJournal: j}, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) {
			return []json.RawMessage{ingestionTestEntry(t, reference, "body", "attachment")}, nil
		},
		notify: func(context.Context, inboundJournalRecord) error { t.Fatal("unexpected alert"); return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Status == "ingested" {
			return
		}
	}
	t.Fatal("delayed transport conclusion excluded the original input")
}

func TestIngestionMentionOnlyDelayedAsyncConfirmation(t *testing.T) {
	at := time.Now().UTC()
	reference := ingestionTestRecord("100.1", at, "body")
	body := ingestionTestReminder(reference, "body")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/session/s1/messages") {
			t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"status":"accepted","request_id":"mo-req-1","event_cursor":"1"}`)
	}))
	defer server.Close()
	// Simulate a confirmation reconnecting two minutes after input arrived.
	// There is no earlier pending journal row on this actual send path.
	now := at.Add(2 * time.Minute)
	j := &inboundJournal{dir: t.TempDir(), now: func() time.Time { now = now.Add(time.Millisecond); return now }}
	cfg := config{gcAPIBase: server.URL, cityName: "test", inboundJournal: j}
	msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: reference.MessageTS}
	_, ok := postMentionOnlyReminder(cfg, "s1", body, func(requestID, cursor string) (string, bool) {
		if requestID != "mo-req-1" || cursor != "1" {
			t.Fatal("wrong confirmation identity")
		}
		return "delivered", true
	}, msg)
	if !ok {
		t.Fatal("async confirmation failed")
	}
	o := &ingestionObserver{cfg: cfg, now: func() time.Time { return now },
		fetch: func(context.Context, string) ([]json.RawMessage, error) {
			return []json.RawMessage{ingestionTestEntry(t, reference, "body", "attachment")}, nil
		},
		notify: func(context.Context, inboundJournalRecord) error { t.Fatal("unexpected alert"); return nil }}
	if err := o.step(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, row := range ingestionJournalRows(t, j) {
		if row.Status == "ingested" {
			return
		}
	}
	t.Fatal("mention-only confirmation lost the send-time proof interval")
}
