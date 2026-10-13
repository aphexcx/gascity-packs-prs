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

// Regression coverage for the lane's own codex round 3 on the gf-d7b delta
// (2 P1, 3 P2; codex-own-packs-r3.out), every one taken. The pending
// history became a list of disjoint stretches: one a probe leaves is
// installed at once, a shrink or a drop waits for the backfill.

// P1: the stretch a probe left existed only in a deferred closure until
// after the backfill; a state save in between (a reconnect probe beside the
// watchdog's persist) after a replay moved the watermark, and a crash, left
// the unread pages behind a moved watermark with no stretch. The stretch is
// installed before the first replay.
func TestLivenessAStretchAProbeLeavesIsInstalledBeforeTheBackfill(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 2
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	var atFirstDelivery []livenessPendingHistory
	l.deliver = func(ctx context.Context, env slackEventEnvelope) backfillDelivery {
		if atFirstDelivery == nil {
			atFirstDelivery = l.pendingHistoriesOf("C1")
		}
		return rec.deliver(ctx, env)
	}
	watermarkBefore := l.watermarkOf("C1")
	missed := missedTopLevel(base, 500)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if len(atFirstDelivery) != 1 || atFirstDelivery[0].From != watermarkBefore || atFirstDelivery[0].Below != missed[100].TS {
		t.Fatalf("pending history at the first delivery = %+v, want the stretch (%s, %s) installed already", atFirstDelivery, watermarkBefore, missed[100].TS)
	}
}

// P1: a thread listed from a stretch reads below the watermark; when that
// read failed with the pending set full, the listing hold took the
// watermark as its cursor and the replies between the stretch's floor and
// the watermark were skipped once the stretch settled. The hold takes the
// read's own cursor, and the thread reads from it.
func TestLivenessAFailedReadFromAStretchHoldsFromItsOwnCursor(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	fillPendingThreads(t, l, hist, base)

	from, below := tsAt(base, -30*time.Minute), tsAt(base, -10*time.Minute)
	parent := histMsg(tsAt(base, -25*time.Minute), "", "old thread")
	parent.BotID = "B1"
	reply := histMsg(tsAt(base, -20*time.Minute), "U2", "reply below the watermark")
	reply.ThreadTS = parent.TS
	reply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": reply.TS, "user": "U2", "text": reply.Text, "thread_ts": parent.TS})
	parent.ReplyCount, parent.LatestReply = 1, reply.TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = []livenessPendingHistory{{From: from, Below: below}} })
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	l.mu.Lock()
	held, known := l.channels["C1"].PendingMessages[reply.TS]
	l.mu.Unlock()
	if !known || !held.Listing || held.Cursor != from {
		t.Fatalf("listing hold = %+v (known=%v), want the thread held from the stretch's floor %s", held, known, from)
	}
	if got := l.pendingHistoryOf("C1"); got != nil {
		t.Fatalf("pending history = %+v, want the stretch read through and gone", got)
	}
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := repliesDelivered(rec.all()); got[parent.TS] != 1 {
		t.Fatalf("thread replies delivered = %d (calls=%v), want the reply below the watermark, read from the hold's cursor", got[parent.TS], hist.calls)
	}
}

// P2: the delivered record the state file carries reached down to the
// watermark, the pending threads' cursors and the stretches, not to a
// listing hold's cursor: delivered replies between that cursor and the
// others' floor were replayed after a restart. The listing cursor joins the
// floor.
func TestLivenessTheDeliveredRecordReachesDownToAListingHoldsCursor(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	l := newTestLiveness(t, livenessTestConfig(t), newFakeHistory(), clock)
	// The watermark sits between the hold's cursor and the held reply, so
	// the hold's own pull-back does not move it: the floor must reach the
	// cursor on its own.
	cursor, delivered, latest := tsAt(base, time.Minute), tsAt(base, 2*time.Minute), tsAt(base, 4*time.Minute)
	l.mu.Lock()
	ch := l.channels["C1"]
	ch.WatermarkTS = tsAt(base, 3*time.Minute)
	l.originsPutLocked(originKey("C1", delivered), inboundOriginBackfilled, clock.now())
	l.holdThreadListingLocked("C1", "T1", tsAt(base, -time.Hour), latest, cursor, false)
	floor, held := ch.originFloorLocked()
	origins, _ := l.heldOriginsLocked("C1", floor)
	l.mu.Unlock()
	if !held || floor != cursor {
		t.Fatalf("origin floor = %s (held=%v), want the listing hold's cursor %s", floor, held, cursor)
	}
	if len(origins) != 1 || origins[0].TS != delivered {
		t.Fatalf("held origins = %+v, want the delivered reply above the listing cursor", origins)
	}
}

// P2: a stretch the floor had entered lost its crossed flag when a probe
// shrank it, and the partial-expiry line posted again on the next probe.
// The flag survives a shrink (and a merge).
func TestLivenessAShrunkStretchKeepsItsCrossedFlag(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 1
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post

	// 650 messages in a stretch inside the window: one page a probe
	// shrinks it three times over without draining it.
	var msgs []slackHistoryMessage
	for i := 0; i < 650; i++ {
		msgs = append(msgs, histMsg(tsAt(base, -28*time.Minute+time.Duration(i)*time.Second), "U1", "in the stretch"))
	}
	hist.byChan["C1"] = msgs
	l.setChannelLocked("C1", func(ch *livenessChannel) {
		ch.PendingHistory = []livenessPendingHistory{{From: tsAt(base, -30*time.Minute), Below: tsAt(base, -10*time.Minute), FloorCrossed: true}}
	})
	for i := 0; i < 3; i++ {
		clock.advance(3 * time.Minute)
		if i == 0 {
			clock.advance(9 * time.Minute)
		}
		l.tick(context.Background())
		// The lowest stretch is the crossed one, shrunk; a delivery below
		// the holds may move the watermark inside it and a later probe
		// leave its own stretch above (held messages, nothing unread).
		got := l.pendingHistoriesOf("C1")
		if len(got) == 0 || !got[0].FloorCrossed || got[0].From != tsAt(base, -30*time.Minute) {
			t.Fatalf("probe %d: pending history = %+v, want the crossed stretch lowest, shrunk, still crossed", i+1, got)
		}
	}
	for _, a := range alerts.all() {
		if strings.Contains(a, "is still unread as the backfill window") {
			t.Fatalf("the partial-expiry line posted again on a shrunk stretch: %v", alerts.all())
		}
	}
}
