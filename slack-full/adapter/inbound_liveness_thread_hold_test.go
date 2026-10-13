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
// longer cleared the watermark, and the reply was never re-read. A failed
// thread read now HOLDS the channel's watermark where the probe read it
// (every advance above it — a committed replay or a live message — is
// remembered as HeldMaxTS, the pending-replay hold's own mechanism) until a
// probe reads every thread of the channel successfully, or the hold ages out
// of the backfill window (the replies it guarded are below the floor by then).

func threadWithReply(base time.Time, parentAt, replyAt time.Duration) (slackHistoryMessage, slackHistoryMessage) {
	parent := histMsg(tsAt(base, parentAt), "U1", "old thread")
	parent.ReplyCount = 1
	parent.LatestReply = tsAt(base, replyAt)
	reply := histMsg(parent.LatestReply, "U2", "unread reply")
	reply.ThreadTS = parent.TS
	reply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": reply.TS, "user": "U2", "text": "unread reply", "thread_ts": parent.TS})
	return parent, reply
}

func (l *inboundLiveness) threadHoldOf(channel string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.channels[channel]; ch != nil {
		return ch.ThreadHoldTS
	}
	return ""
}

// A thread read error beside a newer channel message: the channel message
// replays, the watermark holds at the probe's reading (through a live
// message above it and a state round trip), and the next probe whose
// thread read succeeds replays the reply and releases the watermark to the
// newest message seen meanwhile.
func TestLivenessProbeHoldsTheWatermarkWhileAThreadReadFails(t *testing.T) {
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
	if got := l.originSource("C1", channelMsg.TS); got != inboundOriginBackfilled {
		t.Fatalf("origin of the channel message = %v, want backfilled", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark after a failed thread read = %s, want held at the probe's reading %s", got, watermarkBefore)
	}
	if got := l.threadHoldOf("C1"); got != watermarkBefore {
		t.Fatalf("thread hold = %q, want %s", got, watermarkBefore)
	}
	if !strings.Contains(read(), "thread read channel=C1") || !strings.Contains(read(), "watermark holds") {
		t.Errorf("the failed thread read is not logged with the hold: %s", read())
	}
	// A live message above the hold is seen, but the watermark does not
	// cross the unread replies.
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark = %s after a live message above a thread hold, want held at %s", got, watermarkBefore)
	}
	// The hold survives the state file — and so do the origins the held
	// channel knows above its watermark (the channel message it delivered,
	// the live message it saw): a restart must not replay them (the lane's
	// codex round 4). The rest of the test runs on the RELOADED tracker.
	l.statePath = filepath.Join(t.TempDir(), "liveness.json")
	l.saveState()
	lcfg.livenessStatePath = l.statePath
	reloaded := newTestLiveness(t, lcfg, hist, clock)
	reloaded.deliver = rec.deliver
	if ch := reloaded.channels["C1"]; ch == nil || ch.ThreadHoldTS != watermarkBefore || ch.WatermarkTS != watermarkBefore || ch.HeldMaxTS != live {
		t.Fatalf("reloaded channel = %+v, want the thread hold at %s, the watermark held there and the live advance remembered (%s)", ch, watermarkBefore, live)
	}
	if got := reloaded.originSource("C1", channelMsg.TS); got != inboundOriginBackfilled {
		t.Fatalf("reloaded origin of the delivered channel message = %v, want backfilled", got)
	}
	if got := reloaded.originSource("C1", live); got != inboundOriginLive {
		t.Fatalf("reloaded origin of the live message = %v, want live", got)
	}

	// The thread read recovers: the next probe of the reloaded tracker
	// re-reads from the held watermark, finds the reply, replays it, skips
	// the channel and live messages by their restored origins, and releases
	// the watermark to the newest message seen while held.
	delete(hist.errs, "C1|"+parent.TS)
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	clock.advance(12 * time.Minute)
	reloaded.tick(context.Background())
	envs := rec.all()
	if len(envs) != 2 {
		t.Fatalf("delivered %d after the thread read recovered on the reloaded tracker, want 2 (the reply replayed once, nothing replayed twice; calls=%v)", len(envs), hist.calls)
	}
	var inner map[string]any
	_ = json.Unmarshal(envs[1].Event, &inner)
	if inner["thread_ts"] != parent.TS || inner["text"] != "unread reply" {
		t.Fatalf("second replay = %v, want the unread reply", inner)
	}
	if got := reloaded.threadHoldOf("C1"); got != "" {
		t.Fatalf("thread hold after a clean probe = %q, want released", got)
	}
	if got := reloaded.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the release = %s, want %s (the newest message seen while held)", got, live)
	}
}

// A healthy thread beside a failing one keeps paging: the held watermark
// re-reads the same range every probe, so a thread with more fresh replies
// than one page (livenessHistoryLimit) used to have only its first page
// ever read — the rest unreachable until they aged out (the lane's codex
// round 4). The replies are paged inside the probe now.
func TestLivenessThreadRepliesArePagedUnderAHold(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t)
	l := newTestLiveness(t, lcfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	failing, _ := threadWithReply(base, -2*time.Hour, 2*time.Minute)
	healthy := histMsg(tsAt(base, -time.Hour), "U1", "busy thread")
	healthy.ReplyCount = 1
	n := livenessHistoryLimit + 50
	var replies []slackHistoryMessage
	for i := 0; i < n; i++ {
		r := histMsg(tsAt(base, 2*time.Minute+time.Duration(i)*time.Second), "U2", fmt.Sprintf("reply %d", i))
		r.ThreadTS = healthy.TS
		r.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": r.TS, "user": "U2", "text": r.Text, "thread_ts": healthy.TS})
		replies = append(replies, r)
	}
	healthy.LatestReply = replies[n-1].TS
	hist.byChan["C1"] = []slackHistoryMessage{failing, healthy}
	hist.threads["C1|"+healthy.TS] = replies
	hist.errs["C1|"+failing.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")

	// Three probes under the hold: the per-channel cap delivers 100, 100,
	// then the last 50 — every reply of the healthy thread, not only the
	// first page's.
	for i := 0; i < 3; i++ {
		clock.advance(12 * time.Minute)
		l.tick(context.Background())
	}
	if envs := rec.all(); len(envs) != n {
		t.Fatalf("delivered %d replies of the healthy thread over three probes, want all %d (the first page is %d)", len(envs), n, livenessHistoryLimit)
	}
	if got := l.threadHoldOf("C1"); got == "" {
		t.Fatalf("thread hold = %q, want still held: the other thread's read is still failing", got)
	}
}

// A thread read that keeps failing holds the channel at the backfill
// window's floor, no further: what is below the floor the probe can no
// longer read, what is above it is still unknown (the lane's codex round 3
// on the first cut, which released the whole held advance after one window
// and could skip a reply that arrived, unread, inside it).
func TestLivenessThreadHoldRidesTheBackfillWindowFloor(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t) // backfillMaxWindow = 1h
	l := newTestLiveness(t, lcfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	watermarkBefore := l.watermarkOf("C1")
	parent, reply := threadWithReply(base, -30*time.Minute, 2*time.Minute)
	channelMsg := histMsg(tsAt(base, 4*time.Minute), "U1", "channel message")
	hist.byChan["C1"] = []slackHistoryMessage{parent, channelMsg}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark = %s, want held at %s", got, watermarkBefore)
	}
	// Still failing half an hour later: the hold stays, nothing replays twice.
	clock.advance(30 * time.Minute)
	l.tick(context.Background())
	if got := l.threadHoldOf("C1"); got != watermarkBefore {
		t.Fatalf("thread hold after a second failing probe = %q, want kept at %s", got, watermarkBefore)
	}
	if envs := rec.all(); len(envs) != 1 {
		t.Fatalf("delivered %d, want the channel message once", len(envs))
	}
	// A newer reply lands in the thread, still unread (the read keeps
	// failing), and a live message above everything is seen.
	parent.LatestReply = tsAt(base, 50*time.Minute)
	lateReply := histMsg(parent.LatestReply, "U2", "late reply")
	lateReply.ThreadTS = parent.TS
	lateReply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": lateReply.TS, "user": "U2", "text": "late reply", "thread_ts": parent.TS})
	hist.byChan["C1"] = []slackHistoryMessage{parent, channelMsg}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply, lateReply}
	live := tsAt(base, 55*time.Minute)
	clock.advance(14 * time.Minute) // base+56m
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	// Past the window (base+68m: the watchdog probes once the live message
	// is 10 minutes old) the hold moves up to the floor (base+8m) — not to
	// the live advance at base+55m, which would skip the unread reply at
	// base+50m. The first reply, at base+2m, is below the floor now: left to
	// a live redelivery, as every message out of the window is.
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	floor := slackTSFromTime(clock.now().Add(-lcfg.backfillMaxWindow))
	if got := l.threadHoldOf("C1"); got != floor {
		t.Fatalf("thread hold after the window = %q, want the window's floor %s", got, floor)
	}
	if got := l.watermarkOf("C1"); got != floor {
		t.Fatalf("watermark after the window = %s, want the floor %s (never the held advance %s over the unread reply)", got, floor, live)
	}
	if !strings.Contains(read(), "moved the thread-read hold") {
		t.Errorf("the moved hold is not logged: %s", read())
	}
	// The read recovers: the reply above the floor replays, the hold
	// releases, and the watermark takes the live advance.
	delete(hist.errs, "C1|"+parent.TS)
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if len(envs) != 2 {
		t.Fatalf("delivered %d after the read recovered, want 2 (the channel message, then the reply above the floor)", len(envs))
	}
	var inner map[string]any
	_ = json.Unmarshal(envs[1].Event, &inner)
	if inner["text"] != "late reply" {
		t.Fatalf("second replay = %v, want the late reply", inner)
	}
	if got := l.threadHoldOf("C1"); got != "" {
		t.Fatalf("thread hold after the recovered read = %q, want released", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the release = %s, want %s", got, live)
	}
}

// The release waits for the replay: a recovered thread with more replies
// than the per-channel backfill cap is replayed over several probes, and
// the held advance (a live message newer than every reply) is not taken
// until the last of them has gone (the lane's codex round 3 on the first
// cut, which released at probe time and let the cap strand the rest below
// the released watermark).
func TestLivenessThreadHoldWaitsForTheCappedReplay(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t)
	l := newTestLiveness(t, lcfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	watermarkBefore := l.watermarkOf("C1")
	parent := histMsg(tsAt(base, -time.Hour), "U1", "old thread")
	parent.ReplyCount = 1
	n := livenessMaxBackfillPerChannel + 20
	var replies []slackHistoryMessage
	for i := 0; i < n; i++ {
		r := histMsg(tsAt(base, 2*time.Minute+time.Duration(i)*time.Second), "U2", fmt.Sprintf("reply %d", i))
		r.ThreadTS = parent.TS
		r.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": r.TS, "user": "U2", "text": r.Text, "thread_ts": parent.TS})
		replies = append(replies, r)
	}
	parent.LatestReply = replies[n-1].TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")

	// The hold, then a live message newer than every reply.
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.threadHoldOf("C1"); got != watermarkBefore {
		t.Fatalf("thread hold = %q, want %s", got, watermarkBefore)
	}
	live := tsAt(base, 10*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))

	// The read recovers: the cap replays the first 100, the hold stays, and
	// the watermark stays below the 20 left for the next probe.
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d, want the cap %d", len(envs), livenessMaxBackfillPerChannel)
	}
	if got := l.threadHoldOf("C1"); got != watermarkBefore {
		t.Fatalf("thread hold after a capped replay = %q, want kept at %s", got, watermarkBefore)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark after a capped replay = %s, want held at %s (20 replies are still unreplayed below the live advance)", got, watermarkBefore)
	}
	// The next probe replays the rest; the hold releases to the live advance.
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != n {
		t.Fatalf("delivered %d after the second pass, want all %d replies", len(envs), n)
	}
	if got := l.threadHoldOf("C1"); got != "" {
		t.Fatalf("thread hold after the complete replay = %q, want released", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the complete replay = %s, want the live advance %s", got, live)
	}
}
