package main

import (
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
	gc := httptest.NewServer((&inboundCapture{}).handler())
	defer gc.Close()
	keyCfg := ackTestConfig(t, model.URL, 3*time.Second)
	env := baseSlackEnv()
	env["ACK_REACTION"], env["BUSY_REACTION"] = "model", ""
	env["ACK_MODEL_API_KEY_FILE"], env["ACK_MODEL_BASE_URL"] = keyCfg.ackModelKeyFile, model.URL
	cfg, err := loadConfigFromEnv(stubEnv(env))
	if err != nil {
		t.Fatal(err)
	}
	cfg.gcAPIBase, cfg.dispatchSem = gc.URL, defaultTestDispatchSem
	var workers sync.WaitGroup
	for i := 0; i < messages; i++ {
		event := botMentionEnvelope(t, "message", fmt.Sprintf("Ev-bound-%d", i), "C1",
			fmt.Sprintf("100.%06d", 100+i), "", "hello", true)
		aliases := newTestHandleAliasRegistry(t)
		workers.Add(1)
		go func() {
			defer workers.Done()
			processSlackEvent(cfg, aliases, nil, nil, nil, nil, event, func() {})
		}()
	}
	for i := 0; i < bound; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("ack slots did not fill")
		}
	}
	select {
	case <-entered:
		t.Error("more than four model requests ran concurrently")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	workers.Wait()
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
}
