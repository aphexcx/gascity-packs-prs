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
	// The hold survives the state file.
	l.statePath = filepath.Join(t.TempDir(), "liveness.json")
	l.saveState()
	reloaded := &inboundLiveness{cfg: lcfg, now: clock.now, channels: map[string]*livenessChannel{}, origins: map[string]originEntry{}, statePath: l.statePath}
	reloaded.loadState()
	if ch := reloaded.channels["C1"]; ch == nil || ch.ThreadHoldTS != watermarkBefore || ch.WatermarkTS != watermarkBefore || ch.HeldMaxTS != live {
		t.Fatalf("reloaded channel = %+v, want the thread hold at %s, the watermark held there and the live advance remembered (%s)", ch, watermarkBefore, live)
	}

	// The thread read recovers: the next probe re-reads from the held
	// watermark, finds the reply, replays it, skips the channel and live
	// messages by their origins, and releases the watermark to the newest
	// message seen while held.
	delete(hist.errs, "C1|"+parent.TS)
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if len(envs) != 2 {
		t.Fatalf("delivered %d after the thread read recovered, want 2 (the reply replayed once; calls=%v)", len(envs), hist.calls)
	}
	var inner map[string]any
	_ = json.Unmarshal(envs[1].Event, &inner)
	if inner["thread_ts"] != parent.TS || inner["text"] != "unread reply" {
		t.Fatalf("second replay = %v, want the unread reply", inner)
	}
	if got := l.threadHoldOf("C1"); got != "" {
		t.Fatalf("thread hold after a clean probe = %q, want released", got)
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark after the release = %s, want %s (the newest message seen while held)", got, live)
	}
}

// A thread read that keeps failing cannot hold the channel forever: once the
// hold is older than the backfill window, the replies it guarded are below
// the floor the probe reads from, so the hold retires (left to a live
// redelivery, as every unread message past the window is) and the watermark
// takes the advance it was holding back.
func TestLivenessThreadHoldRetiresOutsideTheBackfillWindow(t *testing.T) {
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
	// Past the window the hold retires and the watermark moves on.
	clock.advance(31 * time.Minute)
	l.tick(context.Background())
	if got := l.threadHoldOf("C1"); got != "" {
		t.Fatalf("thread hold after the window = %q, want retired", got)
	}
	if got := l.watermarkOf("C1"); got != channelMsg.TS {
		t.Fatalf("watermark after the retired hold = %s, want the held advance %s", got, channelMsg.TS)
	}
	if !strings.Contains(read(), "retired the thread-read hold") {
		t.Errorf("the retired hold is not logged: %s", read())
	}
}
