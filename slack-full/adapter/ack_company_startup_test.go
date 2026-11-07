package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Exercise main's config-copy order, not a hand-built gateway config that
// could accidentally supply the cache which production startup forgot.
func TestAckCompanyStartupIncludesChannelName(t *testing.T) {
	requests := make(chan ackChatRequest, 1)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ackChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		requests <- req
		fmt.Fprint(w, chatReply("rocket"))
	}))
	defer model.Close()
	acked := make(chan slackReactionsAddReq, 1)
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/conversations.info":
			if got := r.Header.Get("Authorization"); got != "Bearer xoxb-SWITCHBOARD" {
				t.Errorf("channel lookup used unexpected token")
			}
			fmt.Fprint(w, `{"ok":true,"channel":{"name":"company-launch"}}`)
		case "/conversations.history", "/conversations.replies":
			fmt.Fprint(w, `{"ok":true,"messages":[{"type":"message","user":"U0HUMAN01","text":"thanks","ts":"1700000000.000952"}]}`)
		case "/reactions.add":
			var req slackReactionsAddReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			acked <- req
			fmt.Fprint(w, `{"ok":true}`)
		default:
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer slack.Close()
	gc := newFakeGC(t)
	df, bf := baseDirectoryFile(), baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 1)
	modelCfg := ackTestConfig(t, model.URL, time.Second)
	p := startRegistrationProcess(t, gc.server.URL,
		"SLACK_TEST_API_BASE="+slack.URL,
		"SLACK_WORKSPACE_ID="+testTeamID,
		"SLACK_BOT_TOKEN=xoxb-SWITCHBOARD", "SLACK_APP_ID="+switchAppID,
		"SLACK_COMPANY_DIRECTORY_PATH="+h.dirPath,
		"SLACK_COMPANY_BINDINGS_PATH="+h.bindPath,
		"SLACK_COMPANY_VERIFY_SESSIONS=0", "SLACK_COMPANY_VISIBLE_ACKS=1",
		"ACK_REACTION=model", "ACK_MODEL_BASE_URL="+model.URL,
		"ACK_MODEL_API_KEY_FILE="+modelCfg.ackModelKeyFile)
	p.waitHealth(t, "ok", 5*time.Second)
	ev := slackMessageEvent{Type: "message", User: "U0HUMAN01", Channel: testChannelID,
		TS: "1700000000.000952", Text: "thanks"}
	body, ts, sig := signedBody(t, "test-secret", map[string]any{
		"type": "event_callback", "team_id": testTeamID, "api_app_id": switchAppID,
		"event_id": "Ev-startup-company-ack", "event": ev,
	})
	req, err := http.NewRequest(http.MethodPost, p.public+"/slack/events", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", sig)
	resp, err := (&http.Client{Timeout: time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HTTP admission: %d", resp.StatusCode)
	}
	select {
	case req := <-requests:
		var content string
		for _, msg := range req.Messages {
			if msg.Role == "user" {
				content = msg.Content
			}
		}
		if !strings.Contains(content, "Channel: #company-launch\n") {
			t.Errorf("company model request missing channel name: %q", content)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("company model request did not arrive")
	}
	select {
	case req := <-acked:
		if req.Channel != ev.Channel || req.Timestamp != ev.TS || req.Name != "rocket" {
			t.Fatalf("company ack: %+v", req)
		}
	case <-time.After(time.Second):
		t.Fatal("company ack did not post")
	}
	waitUntil(t, func() bool { return len(gc.sessionCalls()) > 0 })
}
