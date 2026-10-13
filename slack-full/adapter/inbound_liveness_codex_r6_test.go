package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 6 on the gf-d7b delta
// (2 P1, 2 P2; codex-own-packs-r6.out), every one taken.

// P1: a replay that FAILED for a message read from a stretch — below the
// watermark, with no pull-back since round 5 — was held, but the stretch
// settled past it and the read above the watermark never reached it: held
// until the window closed, then dead-lettered, though a retry would have
// landed. A failed replay owns its stretch like a message the cap left.
func TestLivenessAFailedReplayOfAStretchMessageIsRetried(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusInternalServerError}
	l.deliver = rec.deliver

	from, below := tsAt(base, -30*time.Minute), tsAt(base, -10*time.Minute)
	inStretch := histMsg(tsAt(base, -20*time.Minute), "UFOUNDER", "founder line in the stretch")
	hist.byChan["C1"] = []slackHistoryMessage{inStretch}
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = []livenessPendingHistory{{From: from, Below: below}} })
	clock.advance(12 * time.Minute)
	l.tick(context.Background()) // the replay fails: held
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != inStretch.TS {
		t.Fatalf("holds = %v, want the failed replay", got)
	}
	rec.mu.Lock()
	rec.status = http.StatusOK
	rec.mu.Unlock()
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := l.backfilledTotal.Load(); got != 1 {
		t.Fatalf("backfilled = %d after gc came back, want the founder line delivered by the retry (calls=%v)", got, hist.calls)
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the retry = %v, want none", got)
	}
}

// P1: the hold set gave up its newest held entry to an older message, and
// the newest could be a listing hold — a thread's unread replies from its
// cursor, which no stretch covers. A listing hold is never evicted.
func TestLivenessAListingHoldIsNeverEvicted(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	fillPendingThreads(t, l, hist, base)

	// A thread whose latest reply is newer than every channel message, so
	// its listing hold is the newest held; its read fails.
	parent, reply := threadWithReply(base, -time.Hour, 30*time.Minute)
	msgs := missedTopLevel(base, 2*livenessMaxBackfillPerChannel) // 1-100 replay, 101-200 are held
	hist.byChan["C1"] = append([]slackHistoryMessage{parent}, msgs...)
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	l.mu.Lock()
	held, known := l.channels["C1"].PendingMessages[reply.TS]
	inSet := false
	for _, ts := range l.channels["C1"].PendingTS {
		if ts == reply.TS {
			inSet = true
		}
	}
	l.mu.Unlock()
	if !known || !held.Listing || !inSet {
		t.Fatalf("listing hold = %+v (known=%v, in the set=%v), want it standing beside a hundred capped messages", held, known, inSet)
	}
	delete(hist.errs, "C1|"+parent.TS)
	replayUntilDrained(t, l, clock, 6)
	if got := repliesDelivered(rec.all()); got[parent.TS] != 1 {
		t.Fatalf("thread replies delivered = %d, want the one reply once the read recovered", got[parent.TS])
	}
}

// P2: a thread read page by page under a listing hold got a second hold
// when a newer reply moved its latest_reply, and the lowest cursor of the
// two won every probe: the same page, forever. One hold per thread, its
// cursor carried.
func TestLivenessAThreadKeepsOneListingHoldAsItsLatestReplyMoves(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	hist.pageSize = 10
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	fillPendingThreads(t, l, hist, base)

	parent := histMsg(tsAt(base, -time.Hour), "U1", "old thread")
	parent.ReplyCount = 1
	replies := threadReplies(base, parent, 25)
	parent.LatestReply = replies[24].TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = replies
	clock.advance(12 * time.Minute)
	l.tick(context.Background()) // replies 1-10, the hold at reply 25 from reply 10
	// A newer reply moves the latest_reply while the thread is still paged.
	newer := histMsg(tsAt(base, 40*time.Minute), "U2", "reply 25")
	newer.ThreadTS = parent.TS
	newer.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": newer.TS, "user": "U2", "text": newer.Text, "thread_ts": parent.TS})
	hist.threads["C1|"+parent.TS] = append(replies, newer)
	hist.byChan["C1"][0].LatestReply = newer.TS
	replayUntilDrained(t, l, clock, 6)
	if got := repliesDelivered(rec.all()); got[parent.TS] != 26 {
		t.Fatalf("thread replies delivered = %d, want all 26 (calls=%v)", got[parent.TS], hist.calls)
	}
	for text, c := range deliveredTexts(rec.all()) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
}

// P2: a listing-held thread was read only while the history listing still
// named its parent; newer channel messages push the parent past the page
// the scan reaches, and the thread was never read again. A listing-held
// thread is owed a read like a pending one, whatever the listing names.
func TestLivenessAListingHeldThreadIsReadWhenTheListingNoLongerNamesIt(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	fillPendingThreads(t, l, hist, base)

	parent, reply := threadWithReply(base, -time.Hour, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != reply.TS {
		t.Fatalf("holds = %v, want the listing hold", got)
	}
	// A page's worth of newer bot lines between the parent and the
	// watermark, and the read recovers: the parent is no longer on the
	// page the scan reaches.
	for i := 0; i < livenessHistoryLimit+10; i++ {
		m := histMsg(tsAt(base, -30*time.Minute+time.Duration(i)*time.Second), "", "bot line")
		m.BotID = "B1"
		hist.byChan["C1"] = append(hist.byChan["C1"], m)
	}
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := repliesDelivered(rec.all()); got[parent.TS] != 1 {
		t.Fatalf("thread replies delivered = %d, want the reply read from the hold's cursor (calls=%v)", got[parent.TS], hist.calls)
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the read = %v, want none", got)
	}
}
