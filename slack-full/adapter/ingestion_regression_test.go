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
