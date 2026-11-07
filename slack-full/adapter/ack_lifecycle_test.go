package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type ackLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *ackLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *ackLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func captureAckLogs(t *testing.T) *ackLogBuffer {
	t.Helper()
	b := &ackLogBuffer{}
	prev := log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

func TestAckTargetedKeepsReceiptAfterReply(t *testing.T) {
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	cfg := busyTestConfig(gc.URL)
	cfg.ackReaction = "auto"
	env := botMentionEnvelope(t, "message", "EvAck", "C1", "100.000010", "",
		"<@"+testBotUserID+"> thanks", true)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
	// Both are asynchronous; the receipt starts before routing, but the
	// independent HTTP requests can arrive in either order.
	adds := map[string]int{}
	for i := 0; i < 2; i++ {
		got := reactions.await(t, 2*time.Second)
		if got.op != "add" || got.channel != "C1" || got.timestamp != "100.000010" {
			t.Fatalf("unexpected add: %+v", got)
		}
		adds[got.name]++
	}
	if adds["pray"] != 1 || adds["hourglass"] != 1 {
		t.Fatalf("adds = %v, want pray and hourglass", adds)
	}
	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader(publishBody("C1", "100.000010")))
	rec := httptest.NewRecorder()
	handlePublish(cfg, nil, nil, newPublishDedupCache(publishDedupTTL))(rec, req)
	var receipt publishReceipt
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil || rec.Code != http.StatusOK || !receipt.Delivered {
		t.Fatalf("reply not delivered: status=%d err=%v", rec.Code, err)
	}
	got := reactions.await(t, 2*time.Second)
	if got.op != "remove" || got.name != "hourglass" || got.timestamp != "100.000010" {
		t.Fatalf("remove = %+v, want only the hourglass", got)
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
}

func TestAckAmbientOnly(t *testing.T) {
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	cfg := busyTestConfig(gc.URL)
	cfg.ackReaction = "auto"
	env := botMentionEnvelope(t, "message", "EvAmbient", "C1", "100.000011", "", "thanks", true)
	processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
	if got := reactions.await(t, time.Second); got.op != "add" || got.name != "pray" {
		t.Fatalf("ambient reaction = %+v", got)
	}
	if cfg.busyMarks.size() != 0 {
		t.Fatal("ambient message created a busy mark")
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
}

func TestAckBotMessagesGetNeitherReaction(t *testing.T) {
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	cfg := busyTestConfig(gc.URL)
	cfg.ackReaction = "auto"
	for _, tc := range []struct{ user, bot, subtype string }{
		{testBotUserID, "B_SELF", ""},
		{"U_OTHER_BOT", "B_OTHER", ""},
		{"U_LEGACY_BOT", "", "bot_message"},
	} {
		raw, err := json.Marshal(slackMessageEvent{Type: "message", User: tc.user,
			BotID: tc.bot, Subtype: tc.subtype, Channel: "C1", TS: "100.000012", Text: "@mayor thanks"})
		if err != nil {
			t.Fatal(err)
		}
		processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil,
			slackEventEnvelope{Type: "event_callback", Event: raw}, func() {})
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
}

func TestAckRedeliveryAlreadyReactedIsBenign(t *testing.T) {
	logs := captureAckLogs(t)
	reactions := &reactionRecorder{ch: make(chan recordedReaction, 16)}
	seen := false
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/reactions.add" {
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		var req slackReactionsAddReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if seen {
			fmt.Fprint(w, `{"ok":false,"error":"already_reacted"}`)
		} else {
			fmt.Fprint(w, `{"ok":true}`)
		}
		seen = true
		reactions.record(recordedReaction{op: "add", name: req.Name, channel: req.Channel, timestamp: req.Timestamp})
	}))
	defer slack.Close()
	withSlackAPIStub(t, slack)
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	cfg := busyTestConfig(gc.URL)
	cfg.ackReaction = "auto"
	env := botMentionEnvelope(t, "message", "", "C1", "100.000013", "", "thanks", false)
	for i := 0; i < 2; i++ {
		processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, env, func() {})
		if got := reactions.await(t, time.Second); got.name != "pray" {
			t.Fatalf("reaction = %+v", got)
		}
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
	if got := logs.String(); strings.Contains(got, "ack reaction") || strings.Contains(got, "already_reacted") {
		t.Fatalf("duplicate reaction took an error/log path: %s", got)
	}
}
