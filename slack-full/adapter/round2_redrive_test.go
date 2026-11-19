package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/ack"
)

// A stale-roster mention admitted while no dispatch slot is free waits for
// the sweep. When the worker admits its wake later than the reaction budget,
// the worker-started observation still times the message for the stopwatch
// but places no reaction: item 8's two seconds from first sight are gone.
func TestLateRedriveTimesButDoesNotReact(t *testing.T) {
	var mu sync.Mutex
	var reactedAt []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/reactions.add" {
			mu.Lock()
			reactedAt = append(reactedAt, time.Now())
			mu.Unlock()
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	prev := slackAPIBase
	slackAPIBase = srv.URL
	defer func() { slackAPIBase = prev }()

	gc := newFakeGC(t)
	df := baseDirectoryFile()
	df.Rooms[0].MentionWake = []string{"ollie"}
	bf := baseBindingsFile()
	h := newCompanyHarness(t, gc.server.URL, &df, &bf, 0) // no dispatch slot at admission
	setFixedClock(h)
	var probes int32
	h.gw.channelMembersProbe = countingMembersProbe(map[string]bool{botRiley: true}, true, &probes)
	h.gw.slackToken = "test-token"
	env := map[string]string{"ACK_REACTION": "eyes", "GC_SERVICE_STATE_ROOT": t.TempDir()}
	if err := configureArrival(&h.gw.cfg, func(k string) string { return env[k] }); err != nil {
		t.Fatal(err)
	}
	var inbound atomic.Int32
	h.gw.cfg.arrivalStarted = time.Now().UTC()
	h.gw.cfg.arrival = &ack.Service{Choice: h.gw.cfg.arrivalChoice, Dir: h.gw.cfg.arrivalState, Inbound: func(ack.Input) { inbound.Add(1) }}
	h.openBarrier()

	now := time.Now()
	ts := fmt.Sprintf("%d.%06d", now.Unix(), now.Nanosecond()/1000)
	if _, handled := h.admitViaHandler(t, humanMessage(ts, driftMention(botRiley)), 0); !handled {
		t.Fatal("not handled")
	}
	h.wait()
	time.Sleep(2500 * time.Millisecond) // past ack.ReactionBudget
	h.gw.cfg.dispatchSem = make(chan struct{}, 4)
	h.gw.sweepOnce()
	h.wait()
	time.Sleep(300 * time.Millisecond)
	h.gw.cfg.arrival.Wait()
	if n := len(gc.sessionCalls()); n != 1 {
		t.Fatalf("session calls = %d, want the sweep's one delivery", n)
	}
	if inbound.Load() == 0 {
		t.Error("the late-admitted mention has no stopwatch inbound observation")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reactedAt) != 0 {
		t.Errorf("a redrive past the reaction budget placed %d reaction(s), %.2fs after first sight", len(reactedAt), reactedAt[0].Sub(now).Seconds())
	}
}
