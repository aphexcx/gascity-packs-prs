package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression coverage for gf-c7n finding 3i (the gf-xid vs-upstream gate's
// Major at inbound_liveness.go:741): a thread-history read error was logged
// and skipped while the channel messages the same probe found still replayed
// and advanced the shared watermark past the unread replies. An unread reply
// at ts 110 beside a channel message at 120 was unrecoverable after that
// probe: the next one read above 120, the thread's latest_reply (110) no
// longer cleared the watermark, and the reply was never re-read. A thread a
// probe could not finish reading is now PENDING on the channel, with its own
// cursor (persisted with the state), and is re-read from that cursor every
// probe whether or not the watermark still lists it, until a read finds
// nothing undelivered past the cursor. The watermark is never held for it.

func threadWithReply(base time.Time, parentAt, replyAt time.Duration) (slackHistoryMessage, slackHistoryMessage) {
	parent := histMsg(tsAt(base, parentAt), "U1", "old thread")
	parent.ReplyCount = 1
	parent.LatestReply = tsAt(base, replyAt)
	reply := histMsg(parent.LatestReply, "U2", "unread reply")
	reply.ThreadTS = parent.TS
	reply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": reply.TS, "user": "U2", "text": "unread reply", "thread_ts": parent.TS})
	return parent, reply
}

func threadReplies(base time.Time, parent slackHistoryMessage, n int) []slackHistoryMessage {
	var replies []slackHistoryMessage
	for i := 0; i < n; i++ {
		r := histMsg(tsAt(base, 2*time.Minute+time.Duration(i)*time.Second), "U2", fmt.Sprintf("reply %d", i))
		r.ThreadTS = parent.TS
		r.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": r.TS, "user": "U2", "text": r.Text, "thread_ts": parent.TS})
		replies = append(replies, r)
	}
	return replies
}

func (l *inboundLiveness) pendingThreadsOf(channel string) []livenessPendingThread {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.channels[channel]; ch != nil {
		return append([]livenessPendingThread(nil), ch.PendingThreads...)
	}
	return nil
}

// A thread read error beside a newer channel message: the channel message
// replays and the watermark moves on (through a live message above it and
// a state round trip), the thread stays pending from the probe's reading,
// and the next probe whose read succeeds — on the RELOADED tracker, past a
// watermark its latest_reply no longer clears — replays the reply once and
// drops the thread.
func TestLivenessProbeKeepsAFailedThreadReadPendingPastTheWatermark(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t)
	l := newTestLiveness(t, lcfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	watermarkBefore := l.watermarkOf("C1")
	parent, reply := threadWithReply(base, -time.Hour, 2*time.Minute)
	channelMsg := histMsg(tsAt(base, 4*time.Minute), "U1", "channel message")
	live := tsAt(base, 5*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{parent, channelMsg}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 1 {
		t.Fatalf("delivered %d after the first probe, want the one channel message (calls=%v)", len(envs), hist.calls)
	}
	if got := l.watermarkOf("C1"); got != channelMsg.TS {
		t.Fatalf("watermark after the first probe = %s, want %s: the channel message moves it, the failed read holds nothing", got, channelMsg.TS)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || got[0].ParentTS != parent.TS || got[0].Oldest != watermarkBefore {
		t.Fatalf("pending threads = %+v, want the failed thread from the probe's reading %s", got, watermarkBefore)
	}
	if !strings.Contains(read(), "thread read channel=C1") || !strings.Contains(read(), "pending until a read succeeds") {
		t.Errorf("the failed thread read is not logged as pending: %s", read())
	}
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the live message = %s, want %s", got, live)
	}
	// The pending thread survives the state file; the rest runs on the
	// reloaded tracker, whose watermark (the live message) is past the
	// thread's latest_reply.
	l.statePath = filepath.Join(t.TempDir(), "liveness.json")
	l.saveState()
	lcfg.livenessStatePath = l.statePath
	reloaded := newTestLiveness(t, lcfg, hist, clock)
	reloaded.deliver = rec.deliver
	if got := reloaded.pendingThreadsOf("C1"); len(got) != 1 || got[0].ParentTS != parent.TS || got[0].Oldest != watermarkBefore {
		t.Fatalf("reloaded pending threads = %+v, want the failed thread and its cursor", got)
	}
	delete(hist.errs, "C1|"+parent.TS)
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	clock.advance(12 * time.Minute)
	reloaded.tick(context.Background())
	envs := rec.all()
	if len(envs) != 2 {
		t.Fatalf("delivered %d after the thread read recovered on the reloaded tracker, want 2 (the reply replayed once; calls=%v)", len(envs), hist.calls)
	}
	var inner map[string]any
	_ = json.Unmarshal(envs[1].Event, &inner)
	if inner["thread_ts"] != parent.TS || inner["text"] != "unread reply" {
		t.Fatalf("second replay = %v, want the unread reply", inner)
	}
	if got := reloaded.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the drained read = %+v, want none", got)
	}
	if !strings.Contains(read(), "drained; no longer pending") {
		t.Errorf("the drained thread is not logged: %s", read())
	}
}

// A thread that keeps failing stays pending until its parent leaves the
// thread scan window — the adapter would never list it again anyway — and
// is then retired, logged, its undelivered replies left to a live redelivery.
func TestLivenessPendingThreadRetiresWithTheThreadScanWindow(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	parent, reply := threadWithReply(base, -livenessThreadScanWindow+time.Hour, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 1 {
		t.Fatalf("pending threads = %+v, want the failed thread", got)
	}
	// Still failing half an hour on: pending, re-read, nothing delivered.
	clock.advance(30 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 1 {
		t.Fatalf("pending threads after a second failing probe = %+v, want kept", got)
	}
	if n := len(hist.calls); n < 4 {
		t.Fatalf("history calls = %v, want the thread re-read on the second probe", hist.calls)
	}
	// The parent leaves the scan window: retired.
	clock.advance(time.Hour)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the parent left the scan window = %+v, want retired", got)
	}
	if !strings.Contains(read(), "retired 1 pending thread(s) out of the thread scan window") {
		t.Errorf("the retired thread is not logged: %s", read())
	}
	if envs := rec.all(); len(envs) != 0 {
		t.Fatalf("delivered %d, want nothing: the read never succeeded", len(envs))
	}
}

// A recovered thread with more replies than one pass replays keeps its
// pending cursor moving: the per-channel cap delivers 100, the cursor moves
// past them, and the next probe reads the rest from there — past a watermark
// a live message newer than every reply has already moved.
func TestLivenessPendingThreadCursorAdvancesThroughACappedReplay(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	watermarkBefore := l.watermarkOf("C1")
	parent := histMsg(tsAt(base, -time.Hour), "U1", "old thread")
	parent.ReplyCount = 1
	n := livenessMaxBackfillPerChannel + 20
	replies := threadReplies(base, parent, n)
	parent.LatestReply = replies[n-1].TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || got[0].Oldest != watermarkBefore {
		t.Fatalf("pending threads = %+v, want the failed thread from %s", got, watermarkBefore)
	}
	live := tsAt(base, 10*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))

	// The read recovers: the cap replays the first 100; the cursor moves to
	// the 100th; the thread stays pending.
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d, want the cap %d", len(envs), livenessMaxBackfillPerChannel)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || got[0].Oldest != replies[livenessMaxBackfillPerChannel-1].TS {
		t.Fatalf("pending threads after a capped replay = %+v, want the cursor at the last delivered reply %s", got, replies[livenessMaxBackfillPerChannel-1].TS)
	}
	// The next probe reads from the cursor and replays the rest; drained.
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != n {
		t.Fatalf("delivered %d after the second pass, want all %d replies", len(envs), n)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the complete replay = %+v, want none", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark = %s, want the live advance %s", got, live)
	}
}

// A pending thread with more fresh replies than one page (livenessHistoryLimit)
// is read page by page from its cursor across probes, so a long thread
// behind a failed read is drained whole, not only its first page.
func TestLivenessPendingThreadPagesPastTheFirstPage(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	parent := histMsg(tsAt(base, -time.Hour), "U1", "busy thread")
	parent.ReplyCount = 1
	n := livenessHistoryLimit + 50
	replies := threadReplies(base, parent, n)
	parent.LatestReply = replies[n-1].TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	delete(hist.errs, "C1|"+parent.TS)
	// A live message above every reply, so the watermark never pages for us.
	live := tsAt(base, 10*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	for i := 0; i < 3; i++ {
		clock.advance(12 * time.Minute)
		l.tick(context.Background())
	}
	if envs := rec.all(); len(envs) != n {
		t.Fatalf("delivered %d replies over three probes, want all %d (one page is %d)", len(envs), n, livenessHistoryLimit)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the drain = %+v, want none", got)
	}
}
