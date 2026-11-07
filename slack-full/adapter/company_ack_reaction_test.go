package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestAckCompanyUsesConfiguredReceipt(t *testing.T) {
	for _, kind := range []string{"room", "dm", "mpim"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			var calls []ackCall
			var tokens []string
			acked := make(chan struct{}, 4)
			slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req slackReactionsAddReq
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
				}
				mu.Lock()
				calls = append(calls, ackCall{r.URL.Path, req.Channel, req.Timestamp, req.Name})
				tokens = append(tokens, r.Header.Get("Authorization"))
				mu.Unlock()
				fmt.Fprint(w, `{"ok":true}`)
				acked <- struct{}{}
			}))
			defer slack.Close()
			withSlackAPIStub(t, slack)
			h, _, _, _ := setupMpim(t)
			h.gw.visibleAcks = true
			h.gw.cfg.ackReaction = "auto"
			h.gw.reactHookTok = func(token, method, channel, ts, name string) ackOutcome {
				return slackReact(slack.Client(), token, method, channel, ts, name)
			}
			ev := slackMessageEvent{Type: "message", User: "U0HUMAN01", Channel: testChannelID,
				TS: "1700000000.000950", Text: "thanks"}
			app, secret, wantToken := switchAppID, "env-secret", "Bearer xoxb-SWITCHBOARD"
			if kind == "dm" {
				ev.Channel, ev.ChannelType = testDMChannel, "im"
				app, secret, wantToken = ollieAppID, "ollie-secret", "Bearer xoxb-ollie"
			}
			if kind == "mpim" {
				ev.Channel, ev.ChannelType = testMpimChannel, "mpim"
				ev.Text = "thanks <@U0AAAAAA1>"
				app, secret, wantToken = ollieAppID, "ollie-secret", "Bearer xoxb-ollie"
			}
			body, ts, sig := signedBody(t, secret, map[string]any{
				"type": "event_callback", "team_id": testTeamID, "api_app_id": app,
				"event_id": "Ev-configured-ack", "event": ev,
			})
			cfg := h.gw.cfg
			cfg.slackSigningKey, cfg.slackAppID, cfg.companyGateway = "env-secret", switchAppID, h.gw
			req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body))
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			req.Header.Set("X-Slack-Signature", sig)
			w := httptest.NewRecorder()
			handleSlackEvents(cfg, nil, nil, nil, nil, nil)(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP admission: %d %s", w.Code, w.Body.String())
			}
			h.wait()
			if kind == "mpim" {
				r := getMpimReceipt(t, h, ev.TS)
				if r == nil || r.Kind != receiptKindMpim || r.OwnerAppID != ollieAppID || r.Status != ingressStatusDelivered {
					t.Fatalf("mpim receipt: %+v", r)
				}
			}
			select {
			case <-acked:
			case <-time.After(time.Second):
				t.Fatal("configured ack did not post")
			}
			select {
			case <-acked:
				t.Fatal("configured receipt posted more than one ack")
			case <-time.After(100 * time.Millisecond):
			}
			mu.Lock()
			defer mu.Unlock()
			want := ackCall{"/reactions.add", ev.Channel, ev.TS, "pray"}
			if len(calls) != 1 || calls[0] != want || tokens[0] != wantToken {
				t.Fatalf("configured receipt: calls=%+v tokens=%v; want %+v with %s", calls, tokens, want, wantToken)
			}
		})
	}
}

func TestAckCompanyDeliveryDoesNotWaitForModel(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(fmt.Sprintf("dm=%v", dm), func(t *testing.T) {
			entered, release := make(chan struct{}, 1), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				entered <- struct{}{}
				<-release
				fmt.Fprint(w, chatReply("rocket"))
			}))
			defer model.Close()
			defer unblock()
			slack, reactions := newReactionRecordingSlackStub(t)
			withSlackAPIStub(t, slack)
			h, gc, _ := setupDM(t)
			modelCfg := ackTestConfig(t, model.URL, 10*time.Second)
			h.gw.cfg.ackReaction = "model"
			h.gw.cfg.ackModelBaseURL, h.gw.cfg.ackModelKeyFile = model.URL, modelCfg.ackModelKeyFile
			h.gw.cfg.ackModelTimeout, h.gw.cfg.ackModelName = modelCfg.ackModelTimeout, modelCfg.ackModelName
			h.gw.cfg.ackSem = make(chan struct{}, ackConcurrency)
			ev := slackMessageEvent{Type: "message", User: "U0HUMAN01", Channel: testChannelID,
				TS: "1700000000.000951", Text: "thanks"}
			app, secret := switchAppID, "env-secret"
			if dm {
				ev.Channel, ev.ChannelType = testDMChannel, "im"
				app, secret = ollieAppID, "ollie-secret"
			}
			body, ts, sig := signedBody(t, secret, map[string]any{
				"type": "event_callback", "team_id": testTeamID, "api_app_id": app,
				"event_id": "Ev-held-company-ack", "event": ev,
			})
			cfg := h.gw.cfg
			cfg.slackSigningKey, cfg.slackAppID, cfg.companyGateway = "env-secret", switchAppID, h.gw
			req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body))
			req.Header.Set("X-Slack-Request-Timestamp", ts)
			req.Header.Set("X-Slack-Signature", sig)
			w := httptest.NewRecorder()
			handleSlackEvents(cfg, nil, nil, nil, nil, nil)(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("HTTP admission: %d", w.Code)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("model request did not start")
			}
			if !awaitWaitGroup(&h.gw.deliverWG, time.Second) {
				unblock()
				h.wait()
				t.Fatal("company delivery waited for acknowledgment model")
			}
			if got := len(gc.sessionCalls()); got == 0 {
				t.Fatal("company message did not forward while model was held")
			}
			unblock()
			if got := reactions.await(t, time.Second); got.name != "rocket" || got.channel != ev.Channel || got.timestamp != ev.TS {
				t.Fatalf("configured company ack: %+v", got)
			}
			reactions.assertNoCall(t, 100*time.Millisecond)
		})
	}
}
