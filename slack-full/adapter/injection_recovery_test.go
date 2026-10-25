package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Exercise the real event path: the failed peer event is committed, so its
// recovery must ride a later delivery rather than rely on Slack redelivery.
func TestMinimalFailedPeerForwardRecovery(t *testing.T) {
	for _, next := range []string{"human", "peer", "batch"} {
		t.Run(next, func(t *testing.T) {
			capture := &inboundCapture{}
			var reject atomic.Bool
			reject.Store(true)
			gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reject.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				capture.handler().ServeHTTP(w, r)
			}))
			defer gc.Close()
			cfg := peerTestConfig(gc.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), citadelResolver())
			cfg.injectionMode = "minimal"
			cfg.reminderTextBudget = 3500
			aliases := newTestHandleAliasRegistry(t)
			committed := false
			processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E_FAILED", "C_IMM", "100.000001", "", "recover this failed post", peerTestPeerBotID, ""), func() { committed = true })
			if !committed || len(capture.snapshot()) != 0 {
				t.Fatal("first peer forward must fail with its event claim committed")
			}
			// Ambient traffic must neither evict recovery nor appear in minimal.
			for i := 0; i < maxPeerContextPerChannel+1; i++ {
				cfg.peerContext.add(peerContextItem{Channel: "C_IMM", TS: "100.000002", Text: "ambient-only context"})
			}
			reject.Store(false)
			switch next {
			case "human":
				processSlackEvent(cfg, aliases, nil, nil, nil, nil, humanEnvelope(t, "E_NEXT", "C_IMM", "100.000003", "next human post"), func() {})
			case "peer":
				processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E_NEXT", "C_IMM", "100.000003", "", "next peer post", peerTestPeerBotID, ""), func() {})
			case "batch":
				msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C_IMM"}, Actor: externalActor{ID: "U_TEST"}, ProviderMessageID: "100.000003", Text: "next batched post"}
				if err := deliverCoalescedBatch(cfg, "C_IMM", []pendingChannelInbound{{inbound: msg, body: msg.Text}}); err != nil {
					t.Fatal(err)
				}
			}
			got := capture.snapshot()
			if len(got) != 1 {
				t.Fatalf("successful deliveries = %d, want 1", len(got))
			}
			for _, want := range []string{"recover this failed post", "peer-bot citadel", "100.000001", "read-only", "not addressed to you"} {
				if !strings.Contains(got[0].Text, want) {
					t.Errorf("recovered delivery missing %q", want)
				}
			}
			if strings.Contains(got[0].Text, "ambient-only context") || len(got[0].Text) > cfg.reminderTextBudget {
				t.Fatal("ambient context leaked or reminder budget exceeded")
			}
			if pending, _ := cfg.peerContext.flush("C_IMM"); len(pending) != 0 {
				b, _ := json.Marshal(pending)
				t.Fatalf("delivered recovery still queued: %s", b)
			}
		})
	}
}

func TestMinimalPeerRecoveryBudgetAndRetry(t *testing.T) {
	for _, next := range []string{"human", "peer", "batch"} {
		t.Run(next, func(t *testing.T) {
			capture := &inboundCapture{}
			var reject atomic.Bool
			gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if reject.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				capture.handler().ServeHTTP(w, r)
			}))
			defer gc.Close()
			cfg := peerTestConfig(gc.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), citadelResolver())
			cfg.injectionMode = "minimal"
			cfg.reminderTextBudget = 700
			cfg.peerContext.add(peerContextItem{Channel: "C_IMM", Label: "first", TS: "100.000001", Text: strings.Repeat("a", 200), FailedForward: true})
			cfg.peerContext.add(peerContextItem{Channel: "C_IMM", Label: "second", TS: "100.000002", Text: strings.Repeat("b", 200), FailedForward: true})
			aliases := newTestHandleAliasRegistry(t)
			deliver := func(n int, body string) {
				ts := fmt.Sprintf("100.%06d", n)
				switch next {
				case "human":
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, humanEnvelope(t, "E"+ts, "C_IMM", ts, body), func() {})
				case "peer":
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, peerBotEnvelope(t, "message", "E"+ts, "C_IMM", ts, "", body, peerTestPeerBotID, ""), func() {})
				case "batch":
					msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C_IMM"}, Actor: externalActor{ID: "U_TEST"}, ProviderMessageID: ts, Text: body}
					_ = deliverCoalescedBatch(cfg, "C_IMM", []pendingChannelInbound{{inbound: msg, body: body}})
				}
			}
			// A large message defers both recoveries. A failed small delivery
			// restores the first ahead of the second; later small deliveries
			// drain them separately instead of stranding an over-budget block.
			deliver(3, strings.Repeat("large synthetic body ", 30))
			if strings.Contains(capture.snapshot()[0].Text, "peer-bot first") {
				t.Fatal("recovery ignored the reminder budget")
			}
			reject.Store(true)
			deliver(4, "small failed delivery")
			reject.Store(false)
			deliver(5, "small successful delivery")
			deliver(6, "another successful delivery")
			got := capture.snapshot()
			if len(got) != 3 || !strings.Contains(got[1].Text, "peer-bot first") || strings.Contains(got[1].Text, "peer-bot second") || !strings.Contains(got[2].Text, "peer-bot second") {
				t.Fatalf("budget deferral or failure lost/reordered recovery: %+v", got)
			}
			for _, msg := range got[1:] {
				if len(msg.Text) > cfg.reminderTextBudget {
					t.Fatalf("recovery exceeds budget: %d", len(msg.Text))
				}
			}
		})
	}
}

func TestMinimalPeerRecoveryOversizedHead(t *testing.T) {
	cfg := config{injectionMode: "minimal", reminderTextBudget: 700, peerContext: newPeerContextBuffer()}
	first := peerContextItem{Channel: "C1", Label: "first", TS: "100.000001", Text: strings.Repeat("large recovery body ", 60), FailedForward: true}
	second := peerContextItem{Channel: "C1", Label: "second", TS: "100.000002", Text: "small recovery", FailedForward: true}
	cfg.peerContext.add(first)
	cfg.peerContext.add(second)
	items, _ := cfg.peerContext.flush("C1")
	text, sent := minimalPeerRecovery(cfg, "C1", "small inbound", items)
	if len(sent) != 1 || sent[0].Text != first.Text || !strings.Contains(text, "peer-bot first") || !strings.Contains(text, "truncated; read channel") || len(text) > cfg.reminderTextBudget {
		t.Fatalf("oversized recovery did not make bounded progress: text=%q sent=%+v", text, sent)
	}
	// Retry must restore the ORIGINAL body, not the budget-clipped rendering.
	cfg.peerContext.restore("C1", sent, 0)
	items, _ = cfg.peerContext.flush("C1")
	if len(items) != 2 || items[0].Text != first.Text {
		t.Fatal("recovery retry lost original body or queue ordering")
	}
	_, sent = minimalPeerRecovery(cfg, "C1", "small inbound", items)
	if len(sent) != 1 {
		t.Fatal("oversized head still blocks recovery")
	}
	items, _ = cfg.peerContext.flush("C1")
	text, sent = minimalPeerRecovery(cfg, "C1", "small inbound", items)
	if len(sent) != 1 || !strings.Contains(text, "small recovery") {
		t.Fatal("later small recovery remains blocked")
	}
}
