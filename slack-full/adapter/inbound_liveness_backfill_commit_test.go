package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A channel's backfill halts at its first failed message: the newer missed
// messages behind it are not replayed in that probe, so the watermark cannot
// move past the failure, and the next probe replays them all in order.
func TestLivenessBackfillHaltsTheChannelAtTheFirstFailedForward(t *testing.T) {
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
	first, second := tsAt(base, 2*time.Minute), tsAt(base, 3*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(second, "U1", "second"), histMsg(first, "U1", "first")}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 1)
	if got := l.backfilledTotal.Load(); got != 0 {
		t.Fatalf("backfilled = %d, want 0: the channel halts at the first failure", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark = %s after a halted channel, want unchanged %s", got, watermarkBefore)
	}
	for _, ts := range []string{first, second} {
		if got := l.originSource("C1", ts); got != inboundOriginUnknown {
			t.Fatalf("origin %s = %v after a halted channel, want unknown", ts, got)
		}
	}

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 3)
	if got := l.backfilledTotal.Load(); got != 2 {
		t.Fatalf("backfilled = %d after the retry, want 2", got)
	}
	if got := l.watermarkOf("C1"); got != second {
		t.Fatalf("watermark after the retry = %s, want %s", got, second)
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
