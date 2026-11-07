package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestAckPairedEventsPickAndPostOnce(t *testing.T) {
	var picks atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		picks.Add(1)
		entered <- struct{}{}
		<-release
		fmt.Fprint(w, chatReply("rocket"))
	}))
	defer model.Close()
	defer close(release)
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	keyCfg := ackTestConfig(t, model.URL, 2*time.Second)
	env := baseSlackEnv()
	env["ACK_REACTION"], env["BUSY_REACTION"] = "model", ""
	env["ACK_MODEL_API_KEY_FILE"], env["ACK_MODEL_BASE_URL"] = keyCfg.ackModelKeyFile, model.URL
	cfg, err := loadConfigFromEnv(stubEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.gcAPIBase, cfg.dispatchSem = gc.URL, defaultTestDispatchSem
	cfg.busyReaction = "" // loadConfigFromEnv cannot represent set-but-empty
	for i, kind := range []string{"message", "app_mention", "message"} {
		ts := "100.000021"
		if i == 2 {
			ts = "100.000022"
		}
		event := botMentionEnvelope(t, kind, fmt.Sprintf("Ev-paired-%d", i), "C1", ts, "", "hello", true)
		processSlackEvent(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil, event, func() {})
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("distinct messages did not reach the picker")
		}
	}
	select {
	case <-entered:
		t.Error("paired message/app_mention triggered a second in-flight pick")
	case <-time.After(100 * time.Millisecond):
	}
	// Release every request, including an erroneous duplicate, before teardown.
	for i := int32(0); i < picks.Load(); i++ {
		release <- struct{}{}
	}
	for i := 0; i < 2; i++ {
		if got := reactions.await(t, time.Second); got.name != "rocket" {
			t.Fatalf("reaction=%+v", got)
		}
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
	if got := picks.Load(); got != 2 {
		t.Fatalf("picks=%d, want one per message timestamp", got)
	}
}
