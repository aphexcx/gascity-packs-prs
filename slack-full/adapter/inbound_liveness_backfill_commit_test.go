package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Regression coverage for gf-xid finding 3g (the gf-pu2 vs-upstream gate's
// Major at inbound_liveness.go:756): the backfill committed its recovery
// state — the origin marked backfilled (so a racing live copy is dropped) and
// the channel watermark advanced (so no later probe re-reads the message) —
// on the events handler's IMMEDIATE answer. That answer is the 200 Slack gets
// before the forward to gc runs; the forward itself continues in the
// dispatched goroutine. A replayed message whose gc POST then failed kept its
// backfilled marker and its advanced watermark: the live redelivery was
// dropped as a duplicate and every later backfill skipped it. The recovery
// state now commits only once the event's dispatch CONCLUDED committed —
// delivered to gc, or durably queued with retry ownership — and a failed
// forward un-marks the origin and holds the channel's watermark below the
// message so the next probe (or a live copy) retries it.

// flakyGCStub is a gc stub that answers failures for the first failFirst
// POSTs and 202 afterwards.
func flakyGCStub(t *testing.T, failFirst int32) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n <= failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func (l *inboundLiveness) watermarkOf(channel string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.channels[channel]; ch != nil {
		return ch.WatermarkTS
	}
	return ""
}

// A replayed message whose gc POST fails keeps nothing: the origin stays
// unmarked, the watermark stays below it, and the next probe retries it; the
// retry that reaches gc commits exactly once.
func TestLivenessBackfillCommitsOnlyAfterTheForwardReachesGC(t *testing.T) {
	gcStub, hits := flakyGCStub(t, 1)
	cfg := dedupTestConfig(t, gcStub.URL)
	base := time.Now()
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t)
	l := newTestLiveness(t, lcfg, hist, clock)
	cfg.inboundLiveness = l
	events := handleSlackEvents(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil)
	l.deliver = deliverViaHandler(events)
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	watermarkBefore := l.watermarkOf("C1")
	lostTS := tsAt(base, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(lostTS, "U1", "lost then found")}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 1)
	if got := l.backfilledTotal.Load(); got != 0 {
		t.Fatalf("backfilled = %d after a failed gc POST, want 0", got)
	}
	if got := l.originSource("C1", lostTS); got != inboundOriginUnknown {
		t.Fatalf("origin after a failed forward = %v, want unknown (left for a retry or a live copy)", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark after a failed forward = %s, want unchanged %s", got, watermarkBefore)
	}
	if !strings.Contains(read(), "left for a live redelivery or the next probe") {
		t.Errorf("the failed replay is not logged as left for a retry: %s", read())
	}
	// A live copy arriving now is accepted, not dropped as a duplicate.
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", lostTS, "U1", "lost then found"), ""); got != inboundOriginLive {
		t.Fatalf("live copy after the failed backfill = %v, want live (accepted)", got)
	}
	// Undo the live copy's bookkeeping so the next probe sees the message
	// as still missed (the point under test is the backfill's own retry).
	l.mu.Lock()
	delete(l.origins, originKey("C1", lostTS))
	l.channels["C1"].WatermarkTS = watermarkBefore
	l.mu.Unlock()

	// The next probe retries it; gc is up; the recovery state commits once.
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 2)
	if got := l.backfilledTotal.Load(); got != 1 {
		t.Fatalf("backfilled = %d after the retry reached gc, want 1", got)
	}
	if got := l.originSource("C1", lostTS); got != inboundOriginBackfilled {
		t.Fatalf("origin after the delivered retry = %v, want backfilled", got)
	}
	if got := l.watermarkOf("C1"); got != lostTS {
		t.Fatalf("watermark after the delivered retry = %s, want %s", got, lostTS)
	}
}

// A failed replay holds the channel's watermark below itself while the
// newer missed messages behind it still deliver: nothing delivered or seen
// above the failure is lost, and the failure is re-read by the next probe
// (the lane's codex round 1 on the first cut, which halted the channel and
// could strand the rest once a live message advanced the watermark).
func TestLivenessBackfillHoldsTheWatermarkBelowAFailedReplay(t *testing.T) {
	gcStub, hits := flakyGCStub(t, 1)
	cfg := dedupTestConfig(t, gcStub.URL)
	base := time.Now()
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t)
	l := newTestLiveness(t, lcfg, hist, clock)
	cfg.inboundLiveness = l
	events := handleSlackEvents(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil)
	l.deliver = deliverViaHandler(events)

	watermarkBefore := l.watermarkOf("C1")
	first, second, live := tsAt(base, 2*time.Minute), tsAt(base, 3*time.Minute), tsAt(base, 4*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(second, "U1", "second"), histMsg(first, "U1", "first")}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 2)
	if got := l.backfilledTotal.Load(); got != 1 {
		t.Fatalf("backfilled = %d, want 1: the second message still delivers behind the failed first", got)
	}
	if got := l.originSource("C1", first); got != inboundOriginUnknown {
		t.Fatalf("origin of the failed first = %v, want unknown", got)
	}
	if got := l.originSource("C1", second); got != inboundOriginBackfilled {
		t.Fatalf("origin of the delivered second = %v, want backfilled", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark = %s after a failed first replay, want held at %s", got, watermarkBefore)
	}
	// A live message now: seen, but the watermark still does not cross the
	// pending failure.
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark = %s after a live message above a pending failure, want held at %s", got, watermarkBefore)
	}
	// The hold survives the state file.
	l.statePath = filepath.Join(t.TempDir(), "liveness.json")
	l.saveState()
	reloaded := &inboundLiveness{cfg: lcfg, now: clock.now, channels: map[string]*livenessChannel{}, origins: map[string]originEntry{}, statePath: l.statePath}
	reloaded.loadState()
	if ch := reloaded.channels["C1"]; ch == nil || len(ch.PendingTS) != 1 || ch.PendingTS[0] != first || ch.WatermarkTS != watermarkBefore {
		t.Fatalf("reloaded channel = %+v, want the pending %s and the held watermark", ch, first)
	}

	// The next probe re-reads from the held watermark: the failed first is
	// retried and commits; the delivered second and the live message are
	// skipped by their origins; the watermark jumps to the newest seen.
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 3)
	if got := l.backfilledTotal.Load(); got != 2 {
		t.Fatalf("backfilled = %d after the retry, want 2", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the retry = %s, want %s (the newest message seen while held)", got, live)
	}
	l.mu.Lock()
	pending := append([]string(nil), l.channels["C1"].PendingTS...)
	l.mu.Unlock()
	if len(pending) != 0 {
		t.Fatalf("pending after the retry = %v, want none", pending)
	}
}

// The seam itself: the deliver hook's verdict, not its status, decides the
// commit. A dispatch that concluded committed (delivered, or durably queued
// with retry ownership) commits the recovery state; one that concluded
// forgotten, or that has not concluded within the settle bound, leaves the
// origin unmarked and the watermark where it was.
func TestLivenessBackfillHonorsTheDispatchVerdict(t *testing.T) {
	cases := []struct {
		name      string
		verdict   func() *backfillVerdict
		wantMark  inboundOriginSource
		wantCount uint64
	}{
		{name: "committed dispatch commits", verdict: func() *backfillVerdict {
			cache := newEventDedupCache(time.Minute)
			cache.begin("Ev1")
			v := &backfillVerdict{}
			v.watch(cache, "Ev1")
			cache.commit("Ev1")
			return v
		}, wantMark: inboundOriginBackfilled, wantCount: 1},
		{name: "forgotten dispatch leaves the message for a retry", verdict: func() *backfillVerdict {
			cache := newEventDedupCache(time.Minute)
			cache.begin("Ev1")
			v := &backfillVerdict{}
			v.watch(cache, "Ev1")
			cache.forget("Ev1")
			return v
		}, wantMark: inboundOriginUnknown},
		{name: "a dispatch that never concludes within the bound is not committed", verdict: func() *backfillVerdict {
			cache := newEventDedupCache(time.Minute)
			cache.begin("Ev1")
			v := &backfillVerdict{}
			v.watch(cache, "Ev1")
			return v
		}, wantMark: inboundOriginUnknown},
		{name: "no claim taken: the status is final", verdict: func() *backfillVerdict { return &backfillVerdict{} },
			wantMark: inboundOriginBackfilled, wantCount: 1},
	}
	prev := livenessBackfillSettleWait
	livenessBackfillSettleWait = 50 * time.Millisecond
	t.Cleanup(func() { livenessBackfillSettleWait = prev })
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)
			clock := &fakeClock{t: base}
			hist := newFakeHistory()
			l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
			watermarkBefore := l.watermarkOf("C1")
			lost := histMsg(tsAt(base, 2*time.Minute), "U1", "lost")
			hist.byChan["C1"] = []slackHistoryMessage{lost}
			var attempts int32
			l.deliver = func(_ context.Context, _ slackEventEnvelope) backfillDelivery {
				atomic.AddInt32(&attempts, 1)
				return backfillDelivery{status: http.StatusOK, verdict: tc.verdict()}
			}
			clock.advance(12 * time.Minute)
			l.tick(context.Background())
			if attempts != 1 {
				t.Fatalf("deliver attempts = %d, want 1", attempts)
			}
			if got := l.originSource("C1", lost.TS); got != tc.wantMark {
				t.Fatalf("origin = %v, want %v", got, tc.wantMark)
			}
			if got := l.backfilledTotal.Load(); got != tc.wantCount {
				t.Fatalf("backfilled = %d, want %d", got, tc.wantCount)
			}
			wantWatermark := watermarkBefore
			if tc.wantMark == inboundOriginBackfilled {
				wantWatermark = lost.TS
			}
			if got := l.watermarkOf("C1"); got != wantWatermark {
				t.Fatalf("watermark = %s, want %s", got, wantWatermark)
			}
		})
	}
}

// The persistence snapshot copies the pending set under the lock (the
// lane's codex round 2): saveState encodes after unlocking, and the live
// path and the backfill mutate the same slice meanwhile. Run under -race.
func TestLivenessSaveStateSnapshotsThePendingSetUnderTheLock(t *testing.T) {
	base := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	l := newTestLiveness(t, cfg, newFakeHistory(), clock)
	// A standing pending set keeps the backing array in place, so a
	// delete shifts (writes) the same memory a shallow snapshot reads.
	l.mu.Lock()
	for i := 0; i < 64; i++ {
		l.holdPendingLocked(missedMessage{channel: "C1", teamID: "T1", msg: slackHistoryMessage{TS: tsAt(base, time.Duration(i+1)*time.Minute)}, watermark: l.channels["C1"].WatermarkTS}, true, false)
	}
	l.mu.Unlock()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			ts := tsAt(base, time.Duration(i%64+1)*time.Minute)
			l.mu.Lock()
			l.touchChannelLocked("C1", "T1", ts, clock.now())
			l.mu.Unlock()
			l.mu.Lock()
			l.holdPendingLocked(missedMessage{channel: "C1", teamID: "T1", msg: slackHistoryMessage{TS: ts}, watermark: l.channels["C1"].WatermarkTS}, true, false)
			l.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			l.mu.Lock()
			l.stateDirty = true
			l.mu.Unlock()
			l.saveState()
		}
	}()
	wg.Wait()
}

// A pending hold whose message has aged out of the backfill window can never
// be released by re-reading it (the probe reads above the window's floor), so
// it is retired at the next probe and the held advance taken; the message is
// left to a live redelivery, as before the hold existed (the lane's codex
// round 2).
func TestLivenessRetiresPendingHoldsOutsideTheBackfillWindow(t *testing.T) {
	base := time.Date(2026, 10, 12, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	l := newTestLiveness(t, cfg, hist, clock)
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	pending, held := tsAt(base, 2*time.Minute), tsAt(base, 5*time.Minute)
	l.mu.Lock()
	l.channels["C1"].PendingTS = []string{pending}
	l.channels["C1"].HeldMaxTS = held
	l.mu.Unlock()
	watermarkBefore := l.watermarkOf("C1")

	// Inside the window the hold stands.
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark inside the window = %s, want held at %s", got, watermarkBefore)
	}

	// Past the window (backfillMaxWindow is an hour) it is retired.
	clock.advance(2 * time.Hour)
	l.tick(context.Background())
	if got := l.watermarkOf("C1"); got != held {
		t.Fatalf("watermark after the hold aged out = %s, want the held advance %s", got, held)
	}
	l.mu.Lock()
	left := append([]string(nil), l.channels["C1"].PendingTS...)
	heldLeft := l.channels["C1"].HeldMaxTS
	l.mu.Unlock()
	if len(left) != 0 || heldLeft != "" {
		t.Fatalf("pending = %v held = %q after the window, want none", left, heldLeft)
	}
	if !strings.Contains(read(), "out of the backfill window") {
		t.Errorf("retirement not logged: %s", read())
	}
}
