package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression coverage for gf-d7b finding 3n (the gf-c7n vs-main gate's Major
// at inbound_liveness.go:1291): with more missed top-level messages than
// livenessMaxBackfillPerChannel, the backfill replayed the first hundred and
// only set the channel incomplete for the rest. That flag kept a pending
// thread pending but created no hold for a top-level message: a live message
// that advanced the watermark past the unreplayed ones excluded them from
// every later probe for good. A message the cap or a cancellation leaves is
// now held pending on its channel exactly as a failed replay is — the
// watermark holds below it, the retry timer re-reads it under live traffic —
// and the next probe replays it.

func missedTopLevel(base time.Time, n int) []slackHistoryMessage {
	var out []slackHistoryMessage
	for i := 0; i < n; i++ {
		out = append(out, histMsg(tsAt(base, 2*time.Minute+time.Duration(i)*time.Second), "U1", fmt.Sprintf("missed %d", i)))
	}
	return out
}

func deliveredTexts(envs []slackEventEnvelope) map[string]int {
	out := map[string]int{}
	for _, env := range envs {
		var inner map[string]any
		_ = json.Unmarshal(env.Event, &inner)
		if text, _ := inner["text"].(string); text != "" {
			out[text]++
		}
	}
	return out
}

// 101 missed top-level messages: the first probe replays the cap's hundred
// and HOLDS the one it left; a live message above every one of them does not
// carry the watermark past it; the retry probe under live traffic replays it.
// All 101 delivered, each once, the watermark at the live message, no hold
// left. At 4d4b4b49 the 101st was left un-held and the live message put it
// below the watermark for good.
func TestLivenessMessagesTheCapLeavesAreHeldAndReplayedByTheNextProbe(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	n := livenessMaxBackfillPerChannel + 1
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	last := missed[n-1]

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d after the first probe, want the cap %d", len(envs), livenessMaxBackfillPerChannel)
	}
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != last.TS {
		t.Fatalf("holds after the first probe = %v, want the one message the cap left, %s", got, last.TS)
	}
	if got := l.watermarkOf("C1"); got != missed[n-2].TS {
		t.Fatalf("watermark after the first probe = %s, want the last delivered %s", got, missed[n-2].TS)
	}
	if !strings.Contains(read(), "left 1 missed message(s) unreplayed this pass (the per-channel cap of 100) — held pending") {
		t.Errorf("the cap's leftover is not logged as held: %s", read())
	}
	l.mu.Lock()
	held, known := l.channels["C1"].PendingMessages[last.TS]
	l.mu.Unlock()
	if !known || held.Attempts != 0 || held.Msg.Text != last.Text {
		t.Fatalf("held replay = %+v (known=%v), want the message with its text and no attempt", held, known)
	}
	// Live traffic above everything: the watermark does not cross the held
	// message, and the watchdog stays quiet.
	live := tsAt(base, 20*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	if got := l.watermarkOf("C1"); got != missed[n-2].TS {
		t.Fatalf("watermark after the live message = %s, want held at %s below the unreplayed message", got, missed[n-2].TS)
	}
	alarms := l.alarmsTotal.Load()
	for i := 1; i <= 3; i++ {
		clock.advance(time.Minute)
		l.tick(context.Background())
	}
	envs := rec.all()
	if len(envs) != n {
		t.Fatalf("delivered %d after the retry probe, want all %d (calls=%v)", len(envs), n, hist.calls)
	}
	for text, c := range deliveredTexts(envs) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the retry = %v, want none", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the retry = %s, want the live advance %s", got, live)
	}
	if got := l.alarmsTotal.Load(); got != alarms {
		t.Fatalf("alarms = %d, want unchanged (%d): the retry probe is not a stall", got, alarms)
	}
}

// A cancellation mid-pass leaves the rest held the same way: the deliver
// hook cancels the probe's context after the first replay; the other
// hundred are held (the pending set's cap, exactly), the watermark holds
// below them, and the next two probes replay them all.
func TestLivenessMessagesACancellationLeavesAreHeldAndReplayed(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	ctx, cancel := context.WithCancel(context.Background())
	l.deliver = func(c context.Context, env slackEventEnvelope) backfillDelivery {
		out := rec.deliver(c, env)
		cancel()
		return out
	}
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	n := livenessMaxBackfillPerChannel + 1
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(ctx)
	if envs := rec.all(); len(envs) != 1 {
		t.Fatalf("delivered %d before the cancellation, want 1", len(envs))
	}
	if got := l.pendingTSOf("C1"); len(got) != livenessMaxBackfillPerChannel || got[0] != missed[1].TS {
		t.Fatalf("holds after the cancellation = %d from %v, want the %d left, from %s", len(got), got[:min(len(got), 1)], livenessMaxBackfillPerChannel, missed[1].TS)
	}
	if got := l.watermarkOf("C1"); got != missed[0].TS {
		t.Fatalf("watermark = %s, want the one delivered %s", got, missed[0].TS)
	}
	if !strings.Contains(read(), "left 100 missed message(s) unreplayed this pass (a cancellation) — held pending") {
		t.Errorf("the cancellation's leftovers are not logged as held: %s", read())
	}
	l.deliver = rec.deliver
	for i := 1; i <= 2; i++ {
		clock.advance(12 * time.Minute)
		l.tick(context.Background())
	}
	if envs := rec.all(); len(envs) != n {
		t.Fatalf("delivered %d after two more probes, want all %d", len(envs), n)
	}
	for text, c := range deliveredTexts(rec.all()) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds = %v, want none", got)
	}
}
