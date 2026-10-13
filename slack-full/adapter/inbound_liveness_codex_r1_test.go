package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 1 on the gf-d7b delta
// (3 P1, 2 P2; codex-own-packs-r1.out), every one taken.

func (l *inboundLiveness) setChannelLocked(channel string, f func(ch *livenessChannel)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f(l.channels[channel])
}

// P1: with a full pending set, a message read below the watermark (from a
// pending stretch) could not be held and the watermark stayed above it.
// The set keeps the OLDEST outstanding ts: 700 missed messages with the page
// bound at two (400 per probe) and the cap at 100 are all replayed once over
// the probes, the stretch and the holds between them losing nothing.
func TestLivenessTheHoldSetKeepsTheOldestOutstandingWhenFull(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 2
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	n := 700
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingHistoryOf("C1"); got == nil || got.Below != missed[300].TS {
		t.Fatalf("pending history after the first probe = %+v, want the stretch below %s", got, missed[300].TS)
	}
	// The second probe reads the whole stretch (1-300, two pages) and the
	// 300 above the watermark; the cap delivers 1-100 and leaves 101-300 —
	// below the watermark — and the full set of holds (401-500) gives way
	// to them: the oldest outstanding is 101 and the watermark goes below.
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != livenessMaxBackfillPerChannel || got[0] != missed[100].TS {
		t.Fatalf("holds after the second probe = %d from %v, want the full set from the oldest outstanding %s", len(got), got[:min(len(got), 1)], missed[100].TS)
	}
	// The watermark is not pulled back for a message read from a stretch
	// (the stretch owns it; the lane's codex round 5): the holds stand
	// beside the stretch, and the probes below drain both.
	replayUntilDrained(t, l, clock, 14)
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

// P1: the pending stretch's progress was committed before the read above
// the watermark, whose failure then discarded everything the probe had
// collected, the stretch's messages included. The stretch's messages are
// delivered or held before it settles, so nothing is lost either way: the
// first cut kept the stretch untouched and replayed nothing on a probe
// whose read above the watermark failed; since the lane's codex round 3
// the stretch's work stands and replays on such a probe (a budget of one
// request per probe would otherwise re-read the same stretch forever), and
// the stretch settles after that delivery.
func TestLivenessAFailedReadAboveTheWatermarkLosesNothingOfTheUnreadStretch(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	from, below := tsAt(base, -30*time.Minute), tsAt(base, -10*time.Minute)
	inStretch := histMsg(tsAt(base, -20*time.Minute), "U1", "in the stretch")
	above := histMsg(tsAt(base, 2*time.Minute), "U1", "above the watermark")
	hist.byChan["C1"] = []slackHistoryMessage{inStretch, above}
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = []livenessPendingHistory{{From: from, Below: below}} })
	hist.failHistoryCall = 2 // the stretch reads, the read above the watermark fails
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 1 || deliveredTexts(envs)["in the stretch"] != 1 {
		t.Fatalf("delivered %d after the probe whose read above the watermark failed, want the stretch's message once (calls=%v)", len(envs), hist.calls)
	}
	if got := l.pendingHistoryOf("C1"); got != nil {
		t.Fatalf("pending history after the stretch was read through = %+v, want none", got)
	}
	if !strings.Contains(read(), "the channel above its watermark waits for the next probe") {
		t.Errorf("the failed read above the watermark is not logged: %s", read())
	}
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 2 || deliveredTexts(envs)["above the watermark"] != 1 {
		t.Fatalf("delivered %d after the next probe, want the message above the watermark too", len(envs))
	}
}

// P1: a stretch the bound leaves on a probe whose older stretch is still
// pending replaced it only when lower, and otherwise vanished. Nothing is
// lost: the old stretch (shrunk to what the probe read of it), the new one
// above the watermark and the overflow beyond the hold set (the lane's
// codex round 4) are all covered — here by one merged stretch, since the
// overflow spans the gap between them.
func TestLivenessTwoUnreadStretchesAreBothCovered(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 2
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	from := tsAt(base, -50*time.Minute)
	var old, fresh []slackHistoryMessage
	for i := 0; i < 500; i++ {
		old = append(old, histMsg(tsAt(base, -45*time.Minute+time.Duration(i)*time.Second), "U1", "old"))
		fresh = append(fresh, histMsg(tsAt(base, 11*time.Minute+time.Duration(i)*time.Second), "U1", "fresh"))
	}
	hist.byChan["C1"] = append(append([]slackHistoryMessage{}, old...), fresh...)
	watermark := tsAt(base, 10*time.Minute)
	l.setChannelLocked("C1", func(ch *livenessChannel) {
		ch.WatermarkTS = watermark
		ch.PendingHistory = []livenessPendingHistory{{From: from, Below: old[499].TS}}
	})
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	// The window's floor (now - 1 h) has risen past From by two minutes.
	// Read: two pages of the old stretch (old[99..498]) and two of the
	// fresh history (fresh[100..499]); the replay delivered old[99..198]
	// and holds old[199..298]; everything the pass left — old[199] up to
	// fresh[499] — is a stretch of its own (the lane's codex round 5), and
	// the old stretch's unread remainder (floor, old[99]) stands beside
	// it, still crossed; the new one (watermark, fresh[100]) lies inside
	// the left range.
	floor := slackTSFromTime(clock.now().Add(-time.Hour))
	got := l.pendingHistoriesOf("C1")
	if len(got) != 2 || got[0].From != floor || got[0].Below != old[99].TS || !got[0].FloorCrossed ||
		got[1].From != slackTSBefore(old[199].TS) || got[1].Below != slackTSAfter(fresh[499].TS) {
		t.Fatalf("pending history = %+v, want (%s, %s) crossed and (just below %s, just above %s)", got, floor, old[99].TS, old[199].TS, fresh[499].TS)
	}
	if held := l.pendingTSOf("C1"); len(held) != livenessMaxBackfillPerChannel || held[0] != old[199].TS {
		t.Fatalf("holds = %d from %v, want the oldest hundred left from %s", len(held), held[:min(len(held), 1)], old[199].TS)
	}
}

// P2: a thread listed from the pending stretch, cut by the thread cap, was
// made pending from the watermark instead of the stretch's floor, so its
// replies between the two were skipped. The cut thread keeps its discovery
// cursor.
func TestLivenessACutThreadListedFromTheStretchKeepsItsCursor(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	from, below := tsAt(base, -30*time.Minute), tsAt(base, -10*time.Minute)
	n := livenessMaxThreadsPerChannel + 1
	var parents []slackHistoryMessage
	for i := 0; i < n; i++ {
		parent := histMsg(tsAt(base, -25*time.Minute+time.Duration(i)*time.Minute), "", "old thread")
		parent.BotID = "B1" // the parents are not themselves missed messages
		reply := histMsg(tsAt(base, -12*time.Minute+time.Duration(i)*10*time.Second), "U2", "reply below the watermark")
		reply.ThreadTS = parent.TS
		reply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": reply.TS, "user": "U2", "text": reply.Text, "thread_ts": parent.TS})
		parent.ReplyCount, parent.LatestReply = 1, reply.TS
		parents = append(parents, parent)
		hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	}
	hist.byChan["C1"] = parents
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = []livenessPendingHistory{{From: from, Below: below}} })
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxThreadsPerChannel {
		t.Fatalf("delivered %d, want the ten read threads' replies (calls=%v)", len(envs), hist.calls)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || got[0].ParentTS != parents[0].TS || got[0].Oldest != from {
		t.Fatalf("pending threads = %+v, want the cut thread %s from the stretch's floor %s", got, parents[0].TS, from)
	}
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != n {
		t.Fatalf("delivered %d after the next probe, want all %d replies", len(envs), n)
	}
}

// P2: a message the cap left whose live copy landed between the probe's
// read and the replay was held anyway, and nothing could release the hold.
// A known origin is not held.
func TestLivenessACappedMessageSeenLiveMeanwhileIsNotHeld(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	n := livenessMaxBackfillPerChannel + 1
	missed := missedTopLevel(base, n)
	last := missed[n-1]
	l.deliver = func(ctx context.Context, env slackEventEnvelope) backfillDelivery {
		out := rec.deliver(ctx, env)
		if len(rec.all()) == livenessMaxBackfillPerChannel {
			// The live copy of the one the cap is about to leave lands now.
			l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", last.TS, "U1", last.Text), "")
		}
		return out
	}
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds = %v, want none: the capped message was seen live", got)
	}
	if got := l.watermarkOf("C1"); got != last.TS {
		t.Fatalf("watermark = %s, want the live copy %s", got, last.TS)
	}
	if strings.Contains(read(), "held pending") {
		t.Errorf("a hold was logged for a message seen live: %s", read())
	}
}
