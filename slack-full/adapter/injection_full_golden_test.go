package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Fixtures are generated with the 52cbd2f8 source through a Go overlay,
// not with the implementation being tested. They contain synthetic text only.
func TestInjectionFullGoldens(t *testing.T) {
	for _, shape := range []string{"human", "thread", "peer", "batch"} {
		t.Run(shape, func(t *testing.T) {
			capture := &inboundCapture{}
			gc := httptest.NewServer(capture.handler())
			defer gc.Close()
			cfg := coalescingTestConfig(gc.URL, 0)
			cfg.replyHelp.first("C1")
			switch shape {
			case "human":
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, humanEnvelope(t, "Eg1", "C1", "100.000003", "synthetic human text"), func() {})
			case "thread":
				slack, _ := fakeSlackRepliesServer(t, []slackThreadMessage{{TS: "100.000001", User: "U_ROOT", Text: "synthetic parent"}, {TS: "100.000002", User: "U_OTHER", Text: "synthetic prior"}})
				withSlackAPIStub(t, slack)
				cfg.slackBotToken, cfg.threadContextCache = "test-token", newThreadContextCache()
				event, _ := json.Marshal(slackMessageEvent{Type: "message", Channel: "C1", User: "U_HUMAN", TS: "100.000003", ThreadTS: "100.000001", Text: "synthetic reply"})
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", Event: event}, func() {})
			case "peer":
				cfg = peerTestConfig(gc.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), citadelResolver())
				env := peerBotEnvelope(t, "message", "Eg2", "C_IMM", "100.000003", "100.000001", "synthetic peer text", peerTestPeerBotID, "")
				processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
			case "batch":
				var batch []pendingChannelInbound
				for _, ts := range []string{"100.000002", "100.000003"} {
					batch = append(batch, pendingChannelInbound{inbound: externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"}, Actor: externalActor{ID: "U_HUMAN", DisplayName: "Example"}, ProviderMessageID: ts, Text: "synthetic batch member", ReceivedAt: time.Unix(0, 0)}})
				}
				if err := deliverCoalescedBatch(cfg, "C1", batch); err != nil {
					t.Fatal(err)
				}
			}
			msgs := capture.snapshot()
			if len(msgs) != 1 {
				t.Fatalf("deliveries = %d", len(msgs))
			}
			msg := msgs[0]
			msg.ReceivedAt = time.Time{}
			data, err := json.MarshalIndent(msg, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			data = append(data, '\n')
			path := filepath.Join("testdata", "injection-full", shape+".golden")
			if os.Getenv("INJECTION_CAPTURE_BASE") == "1" {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(data) {
				t.Fatalf("full mode differs from Sinan's base:\n%s", data)
			}
		})
	}
}
