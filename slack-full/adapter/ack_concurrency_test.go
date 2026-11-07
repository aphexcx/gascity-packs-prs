package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAckConcurrencyBoundQueuesEveryMessage(t *testing.T) {
	const messages, bound = 9, 4
	var active, peak atomic.Int32
	entered := make(chan struct{}, messages)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		<-release
		active.Add(-1)
		fmt.Fprint(w, chatReply("rocket"))
	}))
	defer model.Close()
	defer unblock()
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	capture := &inboundCapture{}
	gc := httptest.NewServer(capture.handler())
	defer gc.Close()
	keyCfg := ackTestConfig(t, model.URL, 3*time.Second)
	env := baseSlackEnv()
	env["ACK_REACTION"], env["BUSY_REACTION"] = "model", ""
	env["ACK_MODEL_API_KEY_FILE"], env["ACK_MODEL_BASE_URL"] = keyCfg.ackModelKeyFile, model.URL
	env["SLACK_DISPATCH_CONCURRENCY"] = "1"
	cfg, err := loadConfigFromEnv(stubEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.gcAPIBase = gc.URL
	cfg.dispatchSem = make(chan struct{}, cfg.dispatchConcurrency)
	cfg.eventWG = &sync.WaitGroup{}
	cfg.ackModelTimeout = 10 * time.Second
	aliases := newTestHandleAliasRegistry(t)
	handler := handleSlackEvents(cfg, aliases, nil, nil, nil, nil)
	defer func() {
		unblock()
		if !awaitWaitGroup(cfg.eventWG, 2*time.Second) {
			t.Error("dispatch did not finish after model release")
		}
	}()
	for i := 0; i < messages; i++ {
		event := botMentionEnvelope(t, "message", fmt.Sprintf("Ev-bound-%d", i), "C1",
			fmt.Sprintf("100.%06d", 100+i), "", "hello", true)
		body, ts, sig := signedBody(t, cfg.slackSigningKey, event)
		req := httptest.NewRequest(http.MethodPost, "/slack/events", bytes.NewReader(body))
		req.Header.Set("X-Slack-Request-Timestamp", ts)
		req.Header.Set("X-Slack-Signature", sig)
		w := httptest.NewRecorder()
		returned := make(chan struct{})
		go func() { handler(w, req); close(returned) }()
		select {
		case <-returned:
		case <-time.After(time.Second):
			t.Fatal("HTTP admission waited on the held model")
		}
		if w.Code != http.StatusOK {
			t.Fatalf("HTTP admission %d: status=%d", i+1, w.Code)
		}
		// Allow normal dispatch to finish before the next HTTP admission.
		// On the regression, message five holds the only slot; keep posting
		// so the test also witnesses the 200-and-drop of messages six to nine.
		awaitWaitGroup(cfg.eventWG, 100*time.Millisecond)
		if i < bound {
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("ack slots did not fill")
			}
		}
	}
	if got := len(capture.snapshot()); got != messages {
		t.Errorf("while model held: %d/%d messages forwarded after nine HTTP 200s", got, messages)
	}
	select {
	case <-entered:
		t.Error("more than four model requests ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if !awaitWaitGroup(cfg.eventWG, 2*time.Second) {
		t.Fatal("dispatch did not finish")
	}
	if got := len(capture.snapshot()); got != messages {
		t.Fatalf("after model release: %d/%d messages forwarded; HTTP admission dropped messages", got, messages)
	}
	seen := make(map[string]bool)
	for i := 0; i < messages; i++ {
		got := reactions.await(t, 2*time.Second)
		if got.name != "rocket" || seen[got.timestamp] {
			t.Fatalf("missing or duplicate queued ack: %+v", got)
		}
		seen[got.timestamp] = true
	}
	if peak.Load() > bound {
		t.Fatalf("peak concurrency=%d, want <=%d", peak.Load(), bound)
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
	t.Logf("HTTP admission: %d/9 deliveries, %d/9 unique acknowledgments; peak model requests=%d (bound=%d)", len(capture.snapshot()), len(seen), peak.Load(), bound)
}

func TestAckFilteredMessagesDoNotAcquireSlot(t *testing.T) {
	slack, reactions := newReactionRecordingSlackStub(t)
	withSlackAPIStub(t, slack)
	for _, msg := range []slackMessageEvent{
		{User: "U_BOT", BotID: "B_BOT", Text: "thanks"},
		{User: "U_BOT", Subtype: "bot_message", Text: "thanks"},
		{User: "U_HUMAN", Subtype: "channel_join", Text: "joined"},
	} {
		t.Run(msg.BotID+msg.Subtype, func(t *testing.T) {
			cfg := config{ackReaction: "auto", ackSem: make(chan struct{}, 1)}
			cfg.ackSem <- struct{}{} // all slots busy: a filtered message must still finish
			defer func() { <-cfg.ackSem }()
			done := make(chan struct{})
			go func() {
				sendAckReaction(cfg, slack.Client(), "xoxb-test", msg)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("filtered message waited for an ack slot")
			}
		})
	}
	reactions.assertNoCall(t, 100*time.Millisecond)
}
