package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPeerBotDropLogHourlyLimit(t *testing.T) {
	var limiter peerBotDropLogs
	now := time.Unix(100, 0)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if limiter.allow("B_TEST", now) {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 1 || limiter.allow("B_TEST", now.Add(time.Hour-time.Nanosecond)) {
		t.Fatal("repeated bot bypassed hourly limit")
	}
	if !limiter.allow("B_OTHER", now) || !limiter.allow("B_TEST", now.Add(time.Hour)) {
		t.Fatal("independent bot or next hour suppressed")
	}
	if _, exists := limiter.last["B_OTHER"]; exists {
		t.Fatal("expired bot key retained")
	}
}

func TestPeerBotAdmissionDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, registry, app, user, want string
	}{
		{"unlisted", `{"peers":[]}`, "A_TEST", "", "unlisted"},
		{"registry-app", `{"peers":[{"label":"peer","bot_id":"B_TEST","app_id":"A_STALE"}]}`, "A_TEST", "", "contradicted"},
		{"registry-user", `{"peers":[{"label":"peer","app_id":"A_TEST","bot_user_id":"U_STALE"}]}`, "A_TEST", "", "contradicted"},
		{"registry-bot", `{"peers":[{"label":"peer","app_id":"A_TEST","bot_id":"B_STALE"}]}`, "A_TEST", "", "contradicted"},
		{"event-app", `{"peers":[]}`, "A_WRONG", "", "contradicted"},
		{"event-user", `{"peers":[]}`, "A_TEST", "U_WRONG", "contradicted"},
		{"listed", `{"peers":[{"label":"test peer","app_id":"A_TEST"}]}`, "A_TEST", "", `label="test peer"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })
			capture := &inboundCapture{}
			server := httptest.NewServer(capture.handler())
			defer server.Close()
			reg := newTestPeerBotsRegistry(t, tc.registry)
			cfg := peerTestConfig(server.URL, reg, &stubAuthorResolver{info: companyBotInfo{AppID: "A_TEST", UserID: "U_TEST"}, outcome: botResolveOK})
			cfg.companySelfBotUserID = "U_SELF"
			for i := 0; i < 2; i++ {
				maybeDeliverPeerBotMessage(cfg, slackEventEnvelope{}, slackMessageEvent{Type: "message", Channel: "C_TEST", TS: fmt.Sprintf("100.%d", i), BotID: "B_TEST", AppID: tc.app, User: tc.user, Text: "synthetic private payload"})
			}
			got := logs.String()
			if tc.name == "listed" {
				if len(capture.snapshot()) != 2 || strings.Count(got, tc.want) != 2 {
					t.Fatalf("listed delivery lacks label: %s", got)
				}
				return
			}
			if len(capture.snapshot()) != 0 || cfg.peerContext.count("C_TEST") != 0 {
				t.Fatal("dropped bot was delivered or queued")
			}
			if strings.Count(got, "peer-bot: "+tc.want) != 1 || strings.Contains(got, "synthetic private payload") {
				t.Fatalf("expected one diagnostic without text: %s", got)
			}
			for _, field := range []string{`chan="C_TEST"`, `ts="100.0"`, `bot_id="B_TEST"`, "app_id=", "bot_user_id="} {
				if !strings.Contains(got, field) {
					t.Fatalf("missing %s: %s", field, got)
				}
			}
			reg.Commit(&peerBotsSnapshot{})
			reg.logDroppedBot("unlisted", slackMessageEvent{}, "B_TEST", "", "")
			if logs.String() != got {
				t.Fatal("reload reset the rate limit")
			}
		})
	}
}
