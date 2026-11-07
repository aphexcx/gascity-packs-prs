package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestAckCompanyUsesConfiguredReceipt(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(fmt.Sprintf("dm=%v", dm), func(t *testing.T) {
			var mu sync.Mutex
			var calls []ackCall
			var tokens []string
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
			}))
			defer slack.Close()
			withSlackAPIStub(t, slack)
			h, _, _ := setupDM(t)
			h.gw.visibleAcks = true
			h.gw.cfg.ackReaction = "auto"
			h.gw.reactHookTok = func(token, method, channel, ts, name string) ackOutcome {
				return slackReact(slack.Client(), token, method, channel, ts, name)
			}
			ev := slackMessageEvent{Type: "message", User: "U0HUMAN01", Channel: testChannelID,
				TS: "1700000000.000950", Text: "thanks"}
			app, secret, wantToken := switchAppID, "env-secret", "Bearer xoxb-SWITCHBOARD"
			if dm {
				ev.Channel, ev.ChannelType = testDMChannel, "im"
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
			mu.Lock()
			defer mu.Unlock()
			want := ackCall{"/reactions.add", ev.Channel, ev.TS, "pray"}
			if len(calls) != 1 || calls[0] != want || tokens[0] != wantToken {
				t.Fatalf("configured receipt: calls=%+v tokens=%v; want %+v with %s", calls, tokens, want, wantToken)
			}
		})
	}
}
