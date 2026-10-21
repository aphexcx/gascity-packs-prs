package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCompactAlertsConfig(t *testing.T) {
	for _, value := range []string{"", "off", "false", "0", "no", "on", "true", "1", "yes", "invalid"} {
		t.Run("value="+value, func(t *testing.T) {
			env := baseSlackEnv()
			env["SLACK_COMPACT_ALERTS"] = value
			cfg, err := loadConfigFromEnv(stubEnv(env))
			if value == "invalid" {
				if err == nil || !strings.Contains(err.Error(), "SLACK_COMPACT_ALERTS") {
					t.Fatalf("invalid flag accepted: %v", err)
				}
				return
			}
			want := value == "on" || value == "true" || value == "1" || value == "yes"
			if err != nil || cfg.compactAlerts != want {
				t.Fatalf("compact=%v, err=%v; want %v", cfg.compactAlerts, err, want)
			}
		})
	}
}

func TestCompactAlertsRegistration(t *testing.T) {
	for _, compact := range []bool{false, true} {
		var got adapterRegisterRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusOK)
		}))
		cfg := config{gcAPIBase: srv.URL, cityName: "test-city", provider: "slack", accountID: "T1", compactAlerts: compact}
		err := registerAdapter(cfg)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "Reply: gc slack reply-current --conversation-id {conversation_id} --reply-to {thread_ts} --body-file <file>"
		if compact {
			want = "Reply IDs: channel={conversation_id} thread={thread_ts}"
		}
		if got.ReplyInstructions != want {
			t.Fatalf("compact=%v: registration = %q, want %q", compact, got.ReplyInstructions, want)
		}
	}
}

// Exercise the real event/coalescer -> HTTP inbound seam. gc owns the outer
// system-reminder renderer; its routing fields and the registered placeholders
// must survive independently of presentation. Fixtures contain no transcript text.
func TestCompactAlertsEnvelopeShapes(t *testing.T) {
	for _, shape := range []string{"human", "thread", "peer", "batch"} {
		t.Run(shape, func(t *testing.T) {
			var legacy externalInboundMessage
			for _, compact := range []bool{false, true} {
				stub := &gcRouterStub{}
				srv := httptest.NewServer(stub.handler())
				t.Cleanup(srv.Close)
				cfg := coalescingTestConfig(srv.URL, 0)
				cfg.compactAlerts = compact
				cfg.replyHelp = nil // Isolate per-message formatting from first-channel help.
				channel, ts, thread, author := "C1", "100.000002", "", "U_ALICE"
				var payloads []string
				switch shape {
				case "human", "thread":
					env := humanEnvelope(t, "Ev1", channel, ts, "message placeholder 🌱\nsecond line")
					if shape == "thread" {
						thread = "100.000001"
						var msg slackMessageEvent
						if err := json.Unmarshal(env.Event, &msg); err != nil {
							t.Fatal(err)
						}
						msg.ThreadTS = thread
						env.Event, _ = json.Marshal(msg)
					}
					processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
					payloads = []string{"message placeholder 🌱\nsecond line"}
				case "peer":
					channel, thread, author = "C_IMM", "100.000001", peerTestPeerBotUserID
					cfg = peerTestConfig(srv.URL, newTestPeerBotsRegistry(t, peerTestAllowlistImmediate), citadelResolver())
					cfg.compactAlerts = compact
					env := peerBotEnvelope(t, "message", "Ev1", channel, ts, thread, "peer placeholder", peerTestPeerBotID, peerTestPeerAppID)
					processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
					payloads = []string{"peer-bot citadel", "peer placeholder", peerContextFooter}
				case "batch":
					author, thread = "U1", "99.000001"
					first := testPending(channel, "100.000001", "first placeholder")
					last := testPending(channel, ts, "second placeholder")
					last.inbound.ReplyToMessageID = thread
					if err := deliverCoalescedBatch(cfg, channel, []pendingChannelInbound{last, first}); err != nil {
						t.Fatal(err)
					}
					payloads = []string{"[100.000001] Afik: first placeholder", "[100.000002] Afik (in thread 99.000001): second placeholder"}
				}
				msgs := stub.snapshotInbounds()
				if len(msgs) != 1 {
					t.Fatalf("compact=%v: got %d inbounds", compact, len(msgs))
				}
				got := msgs[0]
				if got.Conversation.ConversationID != channel || got.ProviderMessageID != ts || got.ReplyToMessageID != thread || got.Actor.ID != author {
					t.Fatalf("lost channel/ts/thread/author: %+v", got)
				}
				for _, text := range payloads {
					if !strings.Contains(got.Text, text) {
						t.Fatalf("missing %q in %q", text, got.Text)
					}
				}
				if !compact {
					legacy = got
					continue
				}
				if shape == "human" || shape == "thread" {
					if got.Text != legacy.Text {
						t.Fatal("human message/context changed")
					}
				} else if len(got.Text) >= len(legacy.Text) {
					t.Fatal("compact text did not shrink")
				}
				if shape == "peer" && (!got.Actor.IsBot || got.ExplicitTarget != "" || !strings.HasPrefix(got.Text, compactPeerContextHeader)) {
					t.Fatal("peer provenance or read-only instruction lost")
				}
				got.Text, legacy.Text = "", ""
				got.ReceivedAt = legacy.ReceivedAt
				if !reflect.DeepEqual(got, legacy) {
					t.Fatalf("compact changed structured envelope: %+v vs %+v", got, legacy)
				}
			}
		})
	}
}

func TestCompactAlertsBufferedPeerBoundary(t *testing.T) {
	items := []peerContextItem{{Label: "peer", Channel: "C1", TS: "100.1", Text: "peer placeholder"}}
	legacy := formatPeerContextBlock(items, 2)
	if got := formatPeerContextBlockForConfig(config{}, items, 2); got != legacy {
		t.Fatal("flag off changed legacy bytes")
	}
	compact := formatPeerContextBlockForConfig(config{compactAlerts: true}, items, 2)
	if strings.TrimPrefix(compact, compactPeerContextHeader) != strings.TrimPrefix(legacy, peerContextHeader) {
		t.Fatal("peer content, omitted count, or closing boundary changed")
	}
	for _, enabled := range []bool{false, true} {
		if got := formatPeerContextBlockForConfig(config{compactAlerts: enabled}, nil, 0); got != "" {
			t.Fatal("empty peer buffer emitted a wrapper")
		}
	}
}
