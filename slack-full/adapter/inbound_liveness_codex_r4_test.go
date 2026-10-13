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

// Regression coverage for the lane's own codex round 4 on the gf-d7b delta
// (2 P1, 1 P2; codex-own-packs-r4.out), every one taken.

// P1: a pass that left more messages than the hold set takes had no owner
// for the rest: live copies of every held message released the holds, and
// the held advance carried the watermark past the untracked ones. The
// messages above the newest held are an unread stretch of their own: 301
// missed, the first hundred replayed, 101-200 held; a live copy of 301 and
// then of 101-200 release every hold and the watermark takes the advance,
// and the next probe still replays 201-300 from the stretch, each once.
func TestLivenessMessagesBeyondTheHoldSetAreAnUnreadStretch(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	n := 3*livenessMaxBackfillPerChannel + 1
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != livenessMaxBackfillPerChannel || got[0] != missed[100].TS {
		t.Fatalf("holds = %d from %v, want 101-200", len(got), got[:min(len(got), 1)])
	}
	// The whole range the pass left — the held hundred included — is
	// its own stretch (the lane's codex round 5 made every left message
	// an owner of its own, whatever the hold set does).
	if got := l.pendingHistoryOf("C1"); got == nil || got.From != slackTSBefore(missed[100].TS) || got.Below != slackTSAfter(missed[n-1].TS) {
		t.Fatalf("pending history = %+v, want the stretch of everything left (just below %s, just above %s)", got, missed[100].TS, missed[n-1].TS)
	}
	// Live copies: 301 first, then every held one.
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev301", "C1", missed[n-1].TS, "U1", missed[n-1].Text), ""); got != inboundOriginLive {
		t.Fatalf("live copy of the last = %v, want live", got)
	}
	for i := 100; i < 200; i++ {
		l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev"+missed[i].TS, "C1", missed[i].TS, "U1", missed[i].Text), "")
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the live copies = %v, want none", got)
	}
	if got := l.watermarkOf("C1"); got != missed[n-1].TS {
		t.Fatalf("watermark after the live copies = %s, want the held advance %s taken", got, missed[n-1].TS)
	}
	// The next probes replay 201-300 from the stretch, below the watermark.
	replayUntilDrained(t, l, clock, 4)
	envs := rec.all()
	texts := deliveredTexts(envs)
	for i := 200; i < 300; i++ {
		if texts[missed[i].Text] != 1 {
			t.Fatalf("%q replayed %d times, want once (delivered %d in all)", missed[i].Text, texts[missed[i].Text], len(envs))
		}
	}
	for text, c := range texts {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
	if len(envs) != 2*livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d, want 200: the first hundred and 201-300 (101-200 and 301 landed live)", len(envs))
	}
}

// P1: a listing hold (finding 3m) was keyed by the thread's latest reply
// and released when that one reply landed live, with the older unread
// replies still unread; the watermark then moved past them and the thread
// was never listed again. The hold survives its reply's arrival until a
// probe reads the thread through.
func TestLivenessAListingHoldSurvivesItsLatestReplysLiveArrival(t *testing.T) {
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
	for i, at := range []time.Duration{2 * time.Minute, 4 * time.Minute} {
		r := histMsg(tsAt(base, at), "U2", fmt.Sprintf("reply %d", i))
		r.ThreadTS = parent.TS
		r.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": r.TS, "user": "U2", "text": r.Text, "thread_ts": parent.TS})
		replies = append(replies, r)
	}
	parent.ReplyCount, parent.LatestReply = 2, replies[1].TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != replies[1].TS {
		t.Fatalf("holds = %v, want the listing hold at the latest reply", got)
	}
	// The latest reply lands live (a thread message carries thread_ts).
	raw, _ := json.Marshal(map[string]any{"type": "message", "channel": "C1", "user": "U2", "ts": replies[1].TS, "text": replies[1].Text, "thread_ts": parent.TS})
	env := slackEventEnvelope{Type: "event_callback", TeamID: "T1", APIAppID: "A1", EventID: "EvLatest", Event: raw}
	if got := l.noteInboundEnvelope(env, ""); got != inboundOriginLive {
		t.Fatalf("live latest reply = %v, want live", got)
	}
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != replies[1].TS {
		t.Fatalf("holds after the latest reply landed = %v, want the listing hold still standing", got)
	}
	if got := l.watermarkOf("C1"); got != watermarkBefore {
		t.Fatalf("watermark after the latest reply landed = %s, want still held at %s (the older reply is unread)", got, watermarkBefore)
	}
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if got := deliveredTexts(envs); got["reply 0"] != 1 || got["reply 1"] != 0 {
		t.Fatalf("delivered = %v, want the older reply once and the live one not at all", got)
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the thread was read through = %v, want none", got)
	}
	if got := l.watermarkOf("C1"); got != replies[1].TS {
		t.Fatalf("watermark = %s, want the live reply %s once the hold released", got, replies[1].TS)
	}
}

// P2: a listing hold's cursor the floor passed was clamped to the floor
// silently; the replies between were lost with no record. The crossing is
// dead-lettered once, naming the thread and the cursor, and the cursor
// rides the floor.
func TestLivenessAListingHoldsCursorTheFloorPassesIsDeadLetteredOnce(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := deadLetterTestConfig(t)
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post
	fillPendingThreads(t, l, hist, base)

	watermarkBefore := l.watermarkOf("C1")
	parent, lost := threadWithReply(base, -time.Hour, 2*time.Minute)
	kept := histMsg(tsAt(base, 70*time.Minute), "U2", "reply above the floor")
	kept.ThreadTS = parent.TS
	kept.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": kept.TS, "user": "U2", "text": kept.Text, "thread_ts": parent.TS})
	parent.ReplyCount, parent.LatestReply = 2, kept.TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{lost, kept}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != kept.TS {
		t.Fatalf("holds = %v, want the listing hold at %s", got, kept.TS)
	}
	// 75 minutes on the floor passes the cursor but not the held reply.
	clock.advance(63 * time.Minute)
	l.tick(context.Background())
	recs := readDeadLetter(t, cfg)
	var mine []inboundDeadLetterRecord
	for _, r := range recs {
		if r.Inbound.ReplyToMessageID == parent.TS {
			mine = append(mine, r)
		}
	}
	if len(mine) != 1 || !strings.Contains(mine[0].Reason, "held by its listing has replies above "+watermarkBefore+" that fell below the backfill window's floor") {
		t.Fatalf("dead letter records for the thread = %+v, want one naming the cursor %s", mine, watermarkBefore)
	}
	l.mu.Lock()
	held := l.channels["C1"].PendingMessages[kept.TS]
	l.mu.Unlock()
	if !held.FloorCrossed || held.Cursor != slackTSFromTime(clock.now().Add(-time.Hour)) {
		t.Fatalf("listing hold after the crossing = %+v, want it marked and its cursor at the floor", held)
	}
	awaitDeadLetterAlert(t, alerts)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	n := 0
	for _, r := range readDeadLetter(t, cfg) {
		if r.Inbound.ReplyToMessageID == parent.TS {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("dead letter records for the thread after another probe = %d, want still one", n)
	}
	// The read recovers: the reply above the floor lands, the hold releases.
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := deliveredTexts(rec.all()); got["reply above the floor"] != 1 || got["unread reply"] != 0 {
		t.Fatalf("delivered = %v, want the reply above the floor only", got)
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the read = %v, want none", got)
	}
}
