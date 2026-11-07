package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAckConfigOffAndColons(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"", ""}, {":eyes:", "eyes"}, {"auto", "auto"}} {
		env := baseSlackEnv()
		if tc.value != "" {
			env["ACK_REACTION"] = tc.value
		}
		cfg, err := loadConfigFromEnv(stubEnv(env))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ackReaction != tc.want {
			t.Errorf("ACK_REACTION=%q gives %q", tc.value, cfg.ackReaction)
		}
	}
	env := baseSlackEnv()
	env["ACK_MODEL_TIMEOUT_MS"] = "9223372036854775807"
	if _, err := loadConfigFromEnv(stubEnv(env)); err == nil {
		t.Fatal("overflowed model timeout accepted")
	}
}

func TestAckUnsetPreservesExistingReactions(t *testing.T) {
	for _, targeted := range []bool{false, true} {
		for _, busy := range []bool{false, true} {
			slack, reactions := newReactionRecordingSlackStub(t)
			withSlackAPIStub(t, slack)
			capture := &inboundCapture{}
			gc := httptest.NewServer(capture.handler())
			defer gc.Close()
			cfg := busyTestConfig(gc.URL)
			if !busy {
				cfg.busyReaction = ""
			}
			text := "thanks"
			if targeted {
				text = "<@" + testBotUserID + "> thanks"
			}
			env := botMentionEnvelope(t, "message", "", "C1", "100.000014", "", text, true)
			processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
			if targeted && busy {
				if got := reactions.await(t, time.Second); got.name != "hourglass" {
					t.Fatalf("existing busy reaction changed: %+v", got)
				}
			}
			reactions.assertNoCall(t, 100*time.Millisecond)
			if msgs := capture.snapshot(); len(msgs) != 1 || msgs[0].Text != text {
				t.Fatalf("off mode changed routing: %+v", msgs)
			}
		}
	}
}

func TestAckMissingKeyFallsBackEveryMessageLogsOnce(t *testing.T) {
	logs := captureAckLogs(t)
	env := baseSlackEnv()
	env["ACK_REACTION"] = "model"
	// A sentinel endpoint: without a key no model request should be attempted.
	env["ACK_MODEL_BASE_URL"] = "http://127.0.0.1:1/v1"
	cfg, err := loadConfigFromEnv(stubEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	cfg.gcAPIBase = gc.URL
	cfg.dispatchSem = defaultTestDispatchSem
	for i, text := range []string{"thanks", "the site is broken", "hello"} {
		env := botMentionEnvelope(t, "message", "", "C1", "100.000015", "", text, true)
		processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
		if got := reactions.await(t, time.Second); got.name != []string{"pray", "wrench", "eyes"}[i] {
			t.Fatalf("missing-key reaction = %+v", got)
		}
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
	got := logs.String()
	if strings.Count(got, "ACK_REACTION=model but ACK_MODEL_API_KEY_FILE is unset") != 1 || strings.Contains(got, "ack reaction model:") {
		t.Fatalf("expected one config warning and no per-message model log: %s", got)
	}
}
