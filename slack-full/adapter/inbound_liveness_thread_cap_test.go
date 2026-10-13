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

// Regression coverage for gf-d7b finding 3m (the gf-c7n vs-main gate's Major
// at inbound_liveness.go:1117): with fresh replies in more threads than
// livenessMaxThreadsPerChannel, the probe sorted the threads by latest_reply
// and read only the newest ten; the replies replayed from those, and the
// live traffic, moved the watermark past the cut threads' latest_reply, and
// the listing — which names a thread by a latest_reply above the watermark —
// never named them again. Every thread the cap cuts is now PENDING from the
// probe's reading before the cut, read from its cursor by the next probe; a
// thread the pending set cannot take is read this probe (the cap is soft for
// it), and one whose read then fails holds the watermark below its latest
// reply so the listing keeps naming it.

// elevenThreads lays out n old parents, each with one fresh reply in the
// window, the i-th reply at +2m+10i s (so the first parent's reply is the
// oldest latest_reply and the one the cap cuts), and returns the parents
// and the replies by parent ts.
func threadsWithOneReplyEach(base time.Time, n int) ([]slackHistoryMessage, map[string]slackHistoryMessage) {
	var parents []slackHistoryMessage
	replies := map[string]slackHistoryMessage{}
	for i := 0; i < n; i++ {
		parent, reply := threadWithReply(base, -time.Hour+time.Duration(i)*time.Minute, 2*time.Minute+time.Duration(i)*10*time.Second)
		reply.Text = fmt.Sprintf("reply in thread %d", i)
		reply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": reply.TS, "user": "U2", "text": reply.Text, "thread_ts": parent.TS})
		parents = append(parents, parent)
		replies[parent.TS] = reply
	}
	return parents, replies
}

func (l *inboundLiveness) pendingTSOf(channel string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.channels[channel]; ch != nil {
		return append([]string(nil), ch.PendingTS...)
	}
	return nil
}

// repliesDelivered returns, by thread, how many replays landed for each.
func repliesDelivered(envs []slackEventEnvelope) map[string]int {
	out := map[string]int{}
	for _, env := range envs {
		var inner map[string]any
		_ = json.Unmarshal(env.Event, &inner)
		if th, _ := inner["thread_ts"].(string); th != "" {
			out[th]++
		}
	}
	return out
}

// Eleven threads with fresh replies in one window: the first probe reads
// the ten newest and replays their replies, which carry the watermark past
// the eleventh's latest_reply; a live message carries it further. The
// eleventh is pending from the probe's reading, and the next probe — under
// live traffic, by the retry timer — reads it from that cursor and replays
// its reply: eleven delivered, none twice, none lost, the watermark at the
// live message. At 0e5bd58a the eleventh was cut and never read again.
func TestLivenessThreadsTheCapCutsArePendingAndReadByTheNextProbe(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	n := livenessMaxThreadsPerChannel + 1
	watermarkBefore := l.watermarkOf("C1")
	parents, replies := threadsWithOneReplyEach(base, n)
	hist.byChan["C1"] = parents
	for _, p := range parents {
		hist.threads["C1|"+p.TS] = []slackHistoryMessage{replies[p.TS]}
	}
	cut := parents[0] // the oldest latest_reply: the one the cap cuts

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxThreadsPerChannel {
		t.Fatalf("delivered %d after the first probe, want the cap %d (calls=%v)", len(envs), livenessMaxThreadsPerChannel, hist.calls)
	}
	if got := repliesDelivered(rec.all()); got[cut.TS] != 0 {
		t.Fatalf("the cut thread's reply was delivered on the first probe: %v", got)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || got[0].ParentTS != cut.TS || got[0].Oldest != watermarkBefore {
		t.Fatalf("pending threads after the first probe = %+v, want the cut thread %s from the probe's reading %s", got, cut.TS, watermarkBefore)
	}
	if wm := l.watermarkOf("C1"); compareSlackTS(wm, replies[cut.TS].TS) <= 0 {
		t.Fatalf("watermark after the first probe = %s, want past the cut thread's latest reply %s (the replayed replies move it; nothing holds it)", wm, replies[cut.TS].TS)
	}
	if !strings.Contains(read(), "1 pending from "+watermarkBefore+" for the next") {
		t.Errorf("the cut thread is not logged as pending: %s", read())
	}
	// Live traffic moves the watermark further and keeps the watchdog quiet.
	live := tsAt(base, 20*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the live message = %s, want %s", got, live)
	}
	for i := 1; i <= 3; i++ {
		clock.advance(time.Minute)
		l.tick(context.Background())
	}
	envs := rec.all()
	if len(envs) != n {
		t.Fatalf("delivered %d after the retry probe, want all %d replies (calls=%v)", len(envs), n, hist.calls)
	}
	byThread := repliesDelivered(envs)
	for _, p := range parents {
		if byThread[p.TS] != 1 {
			t.Errorf("thread %s: %d replays, want exactly one", p.TS, byThread[p.TS])
		}
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the cut thread was read = %+v, want none", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark = %s, want the live message %s", got, live)
	}
}

// fillPendingThreads puts livenessMaxPendingThreads erroring threads on the
// channel so the pending set is full; their reads fail every probe and they
// stay pending (inside the thread scan window).
func fillPendingThreads(t *testing.T, l *inboundLiveness, hist *fakeHistory, base time.Time) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	ch := l.channels["C1"]
	for i := 0; i < livenessMaxPendingThreads; i++ {
		parent := tsAt(base, -2*time.Hour+time.Duration(i)*time.Second)
		ch.PendingThreads = append(ch.PendingThreads, livenessPendingThread{ParentTS: parent, Oldest: ch.WatermarkTS})
		hist.errs["C1|"+parent] = fmt.Errorf("conversations.replies not ok: ratelimited")
	}
}

// With the pending set full, a thread the cap cuts cannot be pending: it is
// read this probe instead (the cap is soft for it), so eleven threads'
// replies all land on the first probe and nothing waits on a listing the
// watermark will outrun.
func TestLivenessAThreadThePendingSetCannotTakeIsReadThisProbe(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	fillPendingThreads(t, l, hist, base)

	n := livenessMaxThreadsPerChannel + 1
	parents, replies := threadsWithOneReplyEach(base, n)
	hist.byChan["C1"] = parents
	for _, p := range parents {
		hist.threads["C1|"+p.TS] = []slackHistoryMessage{replies[p.TS]}
	}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if len(envs) != n {
		t.Fatalf("delivered %d, want all %d replies read this probe (calls=%v)", len(envs), n, hist.calls)
	}
	if got := repliesDelivered(envs); got[parents[0].TS] != 1 {
		t.Fatalf("the cut thread's reply: %d replays, want one", got[parents[0].TS])
	}
	if !strings.Contains(read(), "1 read now because the pending set is full") {
		t.Errorf("the soft cap is not logged: %s", read())
	}
	if got := l.pendingThreadsOf("C1"); len(got) != livenessMaxPendingThreads {
		t.Fatalf("pending threads = %d, want the full set %d untouched", len(got), livenessMaxPendingThreads)
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds = %v, want none: every thread was read through", got)
	}
}

// With the pending set full, a listed thread whose read FAILS cannot be
// pending either: the channel's watermark holds below its latest reply, so
// the replayed channel message and a live message do not carry it past the
// unread reply and the listing names the thread again; the next probe's
// read succeeds, the reply lands, the hold releases and the watermark takes
// the live advance.
func TestLivenessAFailedReadThePendingSetCannotTakeHoldsTheWatermarkBelowItsLatestReply(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	fillPendingThreads(t, l, hist, base)

	watermarkBefore := l.watermarkOf("C1")
	parent, reply := threadWithReply(base, -time.Hour, 2*time.Minute)
	channelMsg := histMsg(tsAt(base, 4*time.Minute), "U1", "channel message")
	hist.byChan["C1"] = []slackHistoryMessage{parent, channelMsg}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 1 {
		t.Fatalf("delivered %d, want the channel message only", len(envs))
	}
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != reply.TS {
		t.Fatalf("holds = %v, want the thread's latest reply %s", got, reply.TS)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark = %s, want held at the probe's reading %s below the unread reply", got, watermarkBefore)
	}
	if !strings.Contains(read(), "the watermark holds below its latest reply "+reply.TS) {
		t.Errorf("the listing hold is not logged: %s", read())
	}
	live := tsAt(base, 20*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark after the live message = %s, want still held at %s", got, watermarkBefore)
	}
	delete(hist.errs, "C1|"+parent.TS)
	for i := 1; i <= 3; i++ {
		clock.advance(time.Minute)
		l.tick(context.Background())
	}
	envs := rec.all()
	if len(envs) != 2 {
		t.Fatalf("delivered %d after the read recovered, want 2 (the reply once; calls=%v)", len(envs), hist.calls)
	}
	if got := repliesDelivered(envs); got[parent.TS] != 1 {
		t.Fatalf("the thread's reply: %d replays, want one", got[parent.TS])
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the reply landed = %v, want none", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark = %s, want the live advance %s", got, live)
	}
}
