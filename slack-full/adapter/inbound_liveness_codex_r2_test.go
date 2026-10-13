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

// Regression coverage for the lane's own codex round 2 on the gf-d7b delta
// (2 P1, 2 P2; codex-own-packs-r2.out), every one taken.

// P1: a thread under a listing hold (finding 3m: the pending set full, its
// read failed) kept the watermark below its latest reply only; a replayed
// channel message between the thread's cursor and that reply moved the
// watermark past an older unread reply, and the next read started at the
// watermark. The thread reads from the hold's saved cursor: both replies
// land, none lost.
func TestLivenessAListingHeldThreadReadsFromItsSavedCursor(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	fillPendingThreads(t, l, hist, base)

	watermarkBefore := l.watermarkOf("C1")
	parent := histMsg(tsAt(base, -time.Hour), "U1", "old thread")
	var replies []slackHistoryMessage
	for i, at := range []time.Duration{2 * time.Minute, 4 * time.Minute} { // t2, t4
		r := histMsg(tsAt(base, at), "U2", fmt.Sprintf("reply %d", i))
		r.ThreadTS = parent.TS
		r.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": r.TS, "user": "U2", "text": r.Text, "thread_ts": parent.TS})
		replies = append(replies, r)
	}
	parent.ReplyCount, parent.LatestReply = 2, replies[1].TS
	channelMsg := histMsg(tsAt(base, 3*time.Minute), "U1", "channel message at t3")
	hist.byChan["C1"] = []slackHistoryMessage{parent, channelMsg}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != replies[1].TS {
		t.Fatalf("holds = %v, want the listing hold at the latest reply %s", got, replies[1].TS)
	}
	if got := l.watermarkOf("C1"); got != channelMsg.TS {
		t.Fatalf("watermark = %s, want the replayed channel message %s (below the hold, above the older reply)", got, channelMsg.TS)
	}
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if got := repliesDelivered(envs); got[parent.TS] != 2 {
		t.Fatalf("thread replies delivered = %d (calls=%v), want both: the read starts at the hold's cursor %s, not the watermark", got[parent.TS], hist.calls, watermarkBefore)
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the thread was read through = %v, want none", got)
	}
}

// P1: the unread stretch was dropped or shrunk as soon as its pages were
// read, before the backfill delivered or held their messages; a state save
// in between (a reconnect probe beside the watchdog's persist) and a crash
// left those messages, below the watermark, with no recovery owner. The
// stretch settles after the backfill: it still stands while the stretch's
// messages are being delivered, and is gone once they are.
func TestLivenessAnUnreadStretchSettlesOnlyAfterTheBackfill(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	var duringDelivery *livenessPendingHistory
	l.deliver = func(ctx context.Context, env slackEventEnvelope) backfillDelivery {
		if duringDelivery == nil {
			duringDelivery = l.pendingHistoryOf("C1")
		}
		return rec.deliver(ctx, env)
	}
	from, below := tsAt(base, -30*time.Minute), tsAt(base, -10*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(tsAt(base, -20*time.Minute), "U1", "in the stretch")}
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = &livenessPendingHistory{From: from, Below: below} })
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 1 {
		t.Fatalf("delivered %d, want the stretch's message", len(envs))
	}
	if duringDelivery == nil || duringDelivery.From != from || duringDelivery.Below != below {
		t.Fatalf("pending history during the delivery = %+v, want the stretch still standing (%s, %s)", duringDelivery, from, below)
	}
	if got := l.pendingHistoryOf("C1"); got != nil {
		t.Fatalf("pending history after the backfill = %+v, want none", got)
	}
}

// P2: a failure on a later history page discarded every page read and saved
// no progress, so a rate budget that allowed one page and refused the next
// replayed nothing until the window closed. The pages read stand and
// replay, and the pages below are pending for the next probe.
func TestLivenessALaterPagesFailureKeepsThePagesRead(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	watermarkBefore := l.watermarkOf("C1")
	n := livenessHistoryLimit + 50
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	hist.failHistoryCall = 2 // the first page lands, the second fails
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d after the probe whose second page failed, want the cap %d from the page that landed", len(envs), livenessMaxBackfillPerChannel)
	}
	if got := l.pendingHistoryOf("C1"); got == nil || got.From != watermarkBefore || got.Below != missed[50].TS {
		t.Fatalf("pending history = %+v, want the pages below the one read (%s, %s)", got, watermarkBefore, missed[50].TS)
	}
	if !strings.Contains(read(), "history page 2 below "+missed[50].TS) {
		t.Errorf("the failed later page is not logged: %s", read())
	}
	replayUntilDrained(t, l, clock, 6)
	envs := rec.all()
	if len(envs) != n {
		t.Fatalf("delivered %d, want all %d", len(envs), n)
	}
	for text, c := range deliveredTexts(envs) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
}

// P2: the floor entering an unread stretch silently cut its oldest part;
// if the rest was then read through, no notice of the loss was ever given.
// The partial loss is logged and posted once, as the floor enters; the
// full expiry still posts its own line.
func TestLivenessAPartialExpiryOfAnUnreadStretchIsReportedOnce(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	hist.errs["C1"] = fmt.Errorf("conversations.history not ok: ratelimited")
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	from, below := tsAt(base, -30*time.Minute), tsAt(base, 30*time.Minute)
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = &livenessPendingHistory{From: from, Below: below} })
	partial := func() int {
		n := 0
		for _, a := range alerts.all() {
			if strings.Contains(a, "is still unread as the backfill window") {
				n++
			}
		}
		return n
	}
	// 45 minutes on the floor enters the stretch: said once.
	clock.advance(45 * time.Minute)
	l.tick(context.Background())
	awaitCondition(t, func() bool { return partial() == 1 }, "the partial-expiry post")
	if !strings.Contains(read(), "has entered the unread history pages") {
		t.Errorf("the partial expiry is not logged: %s", read())
	}
	if got := l.pendingHistoryOf("C1"); got == nil || !got.FloorCrossed {
		t.Fatalf("pending history = %+v, want it marked as crossed", got)
	}
	// Two more probes as the floor rises: not said again.
	for i := 0; i < 2; i++ {
		clock.advance(3 * time.Minute)
		l.tick(context.Background())
	}
	if got := partial(); got != 1 {
		t.Fatalf("partial-expiry posts = %d after two more probes, want still one", got)
	}
	// The window closes on the whole stretch: its own line.
	clock.advance(50 * time.Minute)
	l.tick(context.Background())
	awaitCondition(t, func() bool {
		for _, a := range alerts.all() {
			if strings.Contains(a, "was never read within the backfill window") {
				return true
			}
		}
		return false
	}, "the full-expiry post")
}
