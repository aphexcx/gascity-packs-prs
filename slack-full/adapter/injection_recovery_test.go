package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func assertNoPeerInstructions(t *testing.T, text string) {
	t.Helper()
	for _, retired := range []string{"read-only", "not addressed to you", "do not reply", "do not act", "peer-bot context", "end peer-bot context"} {
		if strings.Contains(text, retired) {
			t.Errorf("retired instruction %q in delivery", retired)
		}
	}
}

func TestFailedPeerForwardRecovery(t *testing.T) {
	for _, mode := range []string{"full", "minimal"} {
		for _, next := range []string{"human", "peer", "batch"} {
			t.Run(mode+"/"+next, func(t *testing.T) {
				capture := &inboundCapture{}
				var reject atomic.Bool
				reject.Store(true)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if reject.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					capture.handler().ServeHTTP(w, r)
				}))
				defer server.Close()
				cfg := peerTestConfig(server.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
				cfg.injectionMode, cfg.reminderTextBudget = mode, 3500
				cfg.deliveredIDs = newDeliveredIDs()
				aliases := newTestHandleAliasRegistry(t)
				whole := strings.Repeat("报告 ", 320)
				committed := false
				processSlackEvent(cfg, aliases, nil, nil, nil, nil,
					peerBotEnvelope(t, "message", "E_FAIL", "C1", "100.000001", "", whole, peerTestPeerBotID, ""), func() { committed = true })
				if !committed || len(capture.snapshot()) != 0 {
					t.Fatal("failed event did not transfer to recovery")
				}
				reject.Store(false)
				switch next {
				case "human":
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, humanEnvelope(t, "E_NEXT", "C1", "100.000002", "new human post"), func() {})
				case "peer":
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E_NEXT", "C1", "100.000002", "", "new peer post", peerTestPeerBotID, ""), func() {})
				case "batch":
					msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.000002", Text: "new batched post"}
					if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{{inbound: msg, body: msg.Text}}); err != nil {
						t.Fatal(err)
					}
				}
				got := capture.snapshot()
				if len(got) != 1 || !strings.Contains(got[0].Text, whole) || !strings.Contains(got[0].Text, "100.000001") || len(got[0].Text) > cfg.reminderTextBudget {
					t.Fatalf("whole recovery missing/over budget: %+v", got)
				}
				assertNoPeerInstructions(t, got[0].Text)
				if !cfg.deliveredIDs.seen("", "C1", "100.000001") {
					t.Fatal("recovery not marked delivered")
				}
				if pending, _ := cfg.peerContext.flush("C1"); len(pending) != 0 {
					t.Fatal("successful recovery still pending")
				}
			})
		}
	}
}

func TestPeerRecoveryWholeFIFOCountAndRetry(t *testing.T) {
	for _, mode := range []string{"full", "minimal"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config{injectionMode: mode, peerContext: newPeerContextBuffer()}
			items := []peerContextItem{
				{Channel: "C1", Label: "first", TS: "100.1", Text: strings.Repeat("é", 200)},
				{Channel: "C1", Label: "second", TS: "100.2", Text: strings.Repeat("界", 200)},
			}
			body := "new post"
			cfg.reminderTextBudget = len(formatPeerContextBlock(items[:1], 0) + "\n\n" + body + "\n[1 omitted — awaiting delivery]")
			text, sent := peerRecovery(cfg, "C1", body, items)
			if len(sent) != 1 || !strings.Contains(text, items[0].Text) || strings.Contains(text, items[1].Text) || !strings.Contains(text, "[1 omitted — awaiting delivery]") || len(text) > cfg.reminderTextBudget {
				t.Fatal("FIFO prefix or deferred count incorrect")
			}
			cfg.peerContext.restore("C1", sent, 0)
			retry, _ := cfg.peerContext.flush("C1")
			if !reflect.DeepEqual(retry, items) {
				t.Fatal("failed delivery lost/reordered queued posts")
			}
			text, sent = peerRecovery(cfg, "C1", strings.Repeat("large", 1000), retry)
			if len(sent) != 1 || !strings.Contains(text, items[0].Text) || !strings.HasSuffix(text, "[1 omitted — awaiting delivery]") {
				t.Fatal("oversized new message blocked recovery head")
			}
			cfg.peerContext.restore("C1", sent, 0)
			cfg.reminderTextBudget = 0
			retry, _ = cfg.peerContext.flush("C1")
			text, sent = peerRecovery(cfg, "C1", body, retry)
			if len(sent) != 2 || strings.Index(text, items[0].Text) > strings.Index(text, items[1].Text) || strings.Contains(text, "omitted") {
				t.Fatal("retry did not recover the whole FIFO")
			}
			assertNoPeerInstructions(t, text)
		})
	}
}

func TestPeerRecoveryOversizedHeadRemainsVisibleAndWhole(t *testing.T) {
	var logs bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })
	cfg := config{reminderTextBudget: 500, peerContext: newPeerContextBuffer()}
	for i := 0; i < 25; i++ {
		cfg.peerContext.add(peerContextItem{Channel: "C1", TS: fmt.Sprint(i), Text: strings.Repeat("whole ", 1000)})
	}
	items, _ := cfg.peerContext.flush("C1")
	text, sent := peerRecovery(cfg, "C1", "new post", items)
	if len(sent) != 1 || !strings.Contains(text, items[0].Text) || !strings.Contains(text, "24 omitted") || len(text) <= cfg.reminderTextBudget {
		t.Fatal("oversized recovery was clipped or silently dropped")
	}
	queued, _ := cfg.peerContext.flush("C1")
	if !reflect.DeepEqual(queued, items[1:]) {
		t.Fatal("recovery queue lost whole messages")
	}
	wantLog := fmt.Sprintf("oversized recovery head chan=C1 item_bytes=%d budget=500", len(formatPeerContextBlock(items[:1], 0)))
	if strings.Count(logs.String(), wantLog) != 1 {
		t.Fatalf("missing single overflow log: %s", logs.String())
	}
}

func TestPeerRecoverySecondOversizedHeadRidesNextDelivery(t *testing.T) {
	for _, mode := range []string{"full", "minimal"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config{injectionMode: mode, reminderTextBudget: 500, peerContext: newPeerContextBuffer()}
			items := []peerContextItem{
				{Channel: "C1", TS: "100.1", Text: strings.Repeat("first ", 1000)},
				{Channel: "C1", TS: "100.2", Text: strings.Repeat("second ", 1000)},
			}
			for i := range items {
				pending := items
				if i > 0 {
					pending, _ = cfg.peerContext.flush("C1")
				}
				text, sent := peerRecovery(cfg, "C1", "new post", pending)
				if len(sent) != 1 || sent[0].TS != items[i].TS || !strings.Contains(text, items[i].Text) {
					t.Fatalf("delivery %d did not carry its whole head", i)
				}
			}
			if cfg.peerContext.count("C1") != 0 {
				t.Fatal("queue did not drain")
			}
		})
	}
}

func TestFailedPeerAttachmentRecovery(t *testing.T) {
	testAllowAnyURL(t)
	fileServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("synthetic file contents"))
	}))
	t.Cleanup(fileServer.Close)
	for _, mode := range []string{"full", "minimal"} {
		for _, next := range []string{"human", "peer", "batch"} {
			t.Run(mode+"/"+next, func(t *testing.T) {
				var reject atomic.Bool
				reject.Store(true)
				capture := &inboundCapture{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if reject.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					capture.handler().ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				cfg := peerTestConfig(server.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
				cfg.injectionMode, cfg.inboundFileStore = mode, t.TempDir()
				cfg.coalescer = newInboundCoalescer(time.Hour, nil)
				cfg.deliveredIDs = newDeliveredIDs()
				aliases := newTestHandleAliasRegistry(t)
				env := peerBotEnvelope(t, "message", "E_FILE", "C1", "1792436306.401379", "", "peer file", peerTestPeerBotID, "")
				var event slackMessageEvent
				if err := json.Unmarshal(env.Event, &event); err != nil {
					t.Fatal(err)
				}
				event.Files = []slackFile{{ID: "F1", Name: strings.Repeat("report", 8) + ".txt", URLPrivate: fileServer.URL, MIMEType: "text/plain"}}
				var err error
				env.Event, err = json.Marshal(event)
				if err != nil {
					t.Fatal(err)
				}
				processSlackEvent(cfg, aliases, nil, nil, nil, nil, env, func() {})
				queued, _ := cfg.peerContext.flush("C1")
				if len(queued) != 1 || len(queued[0].inbound.Attachments) != 1 {
					t.Fatal("failed file-bearing peer did not retain its attachment")
				}
				want := queued[0].inbound.Attachments[0]
				path := strings.TrimPrefix(want.URL, "file://")
				if redactInboundText(path) == path {
					t.Fatal("fixture must require a trusted path exemption")
				}
				cfg.peerContext.restore("C1", queued, 0)
				reject.Store(false)
				switch next {
				case "human":
					// A mention takes the immediate human channel path with the coalescer enabled.
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, botMentionEnvelope(t, "message", "E_HUMAN", "C1", "1792436307.000002", "", "<@"+testBotUserID+"> next", true), func() {})
				case "peer":
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E_NEXT", "C1", "1792436307.000002", "", "next peer", peerTestPeerBotID, ""), func() {})
				case "batch":
					msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "1792436307.000002", Text: "next batch"}
					if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{{inbound: msg, body: msg.Text}}); err != nil {
						t.Fatal(err)
					}
				}
				got := capture.snapshot()
				var recovered *externalInboundMessage
				for i := range got {
					if got[i].Conversation.ConversationID == "C1" {
						recovered = &got[i]
					}
				}
				if recovered == nil || len(recovered.Attachments) != 1 || recovered.Attachments[0] != want || !strings.Contains(recovered.Text, path) {
					t.Fatalf("recovery lost attachment/path: %+v", recovered)
				}
			})
		}
	}
}

func TestHeldPeerRecoveryLossPreservesEveryEntry(t *testing.T) {
	for _, mode := range []string{"full", "minimal"} {
		for _, next := range []string{"peer", "batch"} {
			t.Run(mode+"/"+next, func(t *testing.T) {
				var posts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte(`{"receipt_id":"ir-peer","state":"unknown"}`))
						return
					}
					if posts.Add(1) == 2 {
						_, _ = w.Write([]byte(`{"TargetSessionID":"fixture","delivery":{"receipt_id":"ir-peer","status":"pending"}}`))
						return
					}
					w.WriteHeader(http.StatusServiceUnavailable)
				}))
				t.Cleanup(server.Close)
				cfg := peerTestConfig(server.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
				cfg.injectionMode, cfg.deliveryReceiptGate = mode, true
				cfg.inboundDeadLetterDir = t.TempDir()
				cfg.coalescer = newInboundCoalescer(time.Hour, nil)
				cfg.deliveredIDs = newDeliveredIDs()
				cfg.receiptFollowUps = fastFollowUps(cfg)
				aliases := newTestHandleAliasRegistry(t)
				whole := strings.Repeat("whole peer ", 200)
				processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E_FAIL", "C1", "100.000001", "", whole, peerTestPeerBotID, ""), func() {})
				if next == "peer" {
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E_HELD", "C1", "100.000002", "", "next peer", peerTestPeerBotID, ""), func() {})
				} else {
					msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, ProviderMessageID: "100.000002", Text: "next batch"}
					if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{{inbound: msg, body: msg.Text}}); err != nil {
						t.Fatal(err)
					}
				}
				if !cfg.receiptFollowUps.wait(3 * time.Second) {
					t.Fatal("held follow-up did not finish")
				}
				if posts.Load() != 3 {
					t.Fatalf("got %d POSTs, want failure, hold, failed repost", posts.Load())
				}
				records := readDeadLetterRecords(t, cfg.inboundDeadLetterDir)
				if len(records) != 2 {
					t.Fatalf("dead-lettered %d entries, want both messages", len(records))
				}
				found := false
				for _, rec := range records {
					if rec.Inbound.ProviderMessageID == "100.000001" {
						found = rec.Inbound.Text == whole && rec.Inbound.Actor.IsBot && strings.Contains(rec.Reason, "lost")
					}
				}
				if !found {
					t.Fatal("held loss discarded recovered peer provenance or whole body")
				}
			})
		}
	}
}

func TestRejectedPeerRecoveryAttachmentCannotPoisonSuccessor(t *testing.T) {
	for _, mode := range []string{"full", "minimal"} {
		for _, firstStatus := range []int{http.StatusServiceUnavailable, http.StatusBadRequest} {
			t.Run(fmt.Sprintf("%s/initial-%d", mode, firstStatus), func(t *testing.T) {
				var posts atomic.Int32
				capture := &inboundCapture{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var req struct {
						Message externalInboundMessage `json:"message"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Error(err)
					}
					if posts.Add(1) == 1 {
						w.WriteHeader(firstStatus)
						return
					}
					if len(req.Message.Attachments) > 0 {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					capture.mu.Lock()
					capture.messages = append(capture.messages, req.Message)
					capture.mu.Unlock()
					_, _ = w.Write([]byte(`{"TargetSessionID":"fixture"}`))
				}))
				t.Cleanup(server.Close)
				cfg := peerTestConfig(server.URL, newTestPeerBotsRegistry(t, peerTestAllowlist), citadelResolver())
				cfg.injectionMode = mode
				cfg.deliveredIDs = newDeliveredIDs()
				cfg.coalescer = newInboundCoalescer(time.Hour, nil)
				cfg.coalescer.deliver = func(channel string, batch []pendingChannelInbound) error {
					return deliverCoalescedBatch(cfg, channel, batch)
				}
				dead := 0
				cfg.coalescer.deadLetter = func(_ string, batch []pendingChannelInbound, _ error) bool {
					dead += len(batch)
					return true
				}
				const path = "/tmp/gc-slack-adapter/inbound/C1234567890/1792436306.401379-image.png"
				msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"},
					ProviderMessageID: "100.000001", Text: "whole peer post", Actor: externalActor{IsBot: true},
					Attachments: []externalAttachment{{ProviderID: "F1", URL: "file://" + path, MIMEType: "image/png"}}}
				item := peerContextItem{Label: "peer", Channel: "C1", TS: msg.ProviderMessageID, Text: msg.Text, inbound: msg}
				// Mirror the immediate peer transfer, then exercise the actual
				// coalescer rejection ladder for the unrelated successors.
				if err := deliverCoalescedBatch(cfg, "C1", []pendingChannelInbound{{inbound: msg, body: msg.Text}}); err != nil {
					cfg.peerContext.add(item.withholdRejectedAttachments(err))
				} else {
					t.Fatal("initial fixture did not fail")
				}
				for _, ts := range []string{"100.000002", "100.000003"} {
					release := cfg.coalescer.holdDelivery("C1")
					err := cfg.coalescer.post("C1", []pendingChannelInbound{testPending("C1", ts, "unrelated successor")})
					release()
					if err != nil {
						t.Fatal(err)
					}
				}
				got := capture.snapshot()
				if dead != 0 || len(got) != 2 || cfg.peerContext.count("C1") != 0 {
					t.Fatalf("rider poisoned successor: dead=%d delivered=%d pending=%d", dead, len(got), cfg.peerContext.count("C1"))
				}
				joined := got[0].Text + got[1].Text
				if !strings.Contains(joined, msg.Text) || !strings.Contains(joined, path) || !strings.Contains(joined, "attachment withheld") {
					t.Fatal("attachment fallback lost peer text, trusted path, or withholding notice")
				}
				if firstStatus == http.StatusServiceUnavailable && !strings.Contains(got[0].Text, "[1 omitted — awaiting delivery]") {
					t.Fatal("successor did not disclose its deferred rider")
				}
			})
		}
	}
}
