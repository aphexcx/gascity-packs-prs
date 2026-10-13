package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 10 on the gf-d7b delta
// (1 P1, 1 P2; codex-own-packs-r10.out), both taken.

// P1: with the pending set full and the hold set full of message holds
// older than a thread's latest reply, the thread's listing hold was refused
// (its ts newest, nothing newer to give way) and ignored: the thread had no
// owner at all. A listing hold always gets in; the newest message hold
// gives way (its message has its stretch).
func TestLivenessAListingHoldGetsInPastAFullHoldSet(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	fillPendingThreads(t, l, hist, base)

	// 200 channel messages: a hundred replay, a hundred are held; the
	// thread's latest reply is newer than all of them, and its read fails.
	msgs := missedTopLevel(base, 2*livenessMaxBackfillPerChannel)
	parent, reply := threadWithReply(base, -time.Hour, 30*time.Minute)
	hist.byChan["C1"] = append([]slackHistoryMessage{parent}, msgs...)
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	l.mu.Lock()
	held, known := l.channels["C1"].PendingMessages[reply.TS]
	n := len(l.channels["C1"].PendingTS)
	l.mu.Unlock()
	if !known || !held.Listing {
		t.Fatalf("listing hold = %+v (known=%v) with %d holds, want the thread held past the full set (log: %s)", held, known, n, read())
	}
	if n != livenessMaxBackfillPerChannel {
		t.Fatalf("holds = %d, want the set still at its cap", n)
	}
	delete(hist.errs, "C1|"+parent.TS)
	replayUntilDrained(t, l, clock, 6)
	if got := repliesDelivered(rec.all()); got[parent.TS] != 1 {
		t.Fatalf("thread replies delivered = %d, want the reply once the read recovered", got[parent.TS])
	}
	if strings.Contains(read(), "cannot be held either") {
		t.Errorf("the listing hold was refused: %s", read())
	}
}

// P2: an unread-history page that came back empty with has_more marked the
// stretch read through, and the settle dropped it with pages unread. An
// empty page with more to come leaves the stretch as read, and the next
// probe goes on.
func TestLivenessAnEmptyPageWithMoreDoesNotReadAStretchThrough(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	from, below := tsAt(base, -30*time.Minute), tsAt(base, -10*time.Minute)
	human := histMsg(tsAt(base, -20*time.Minute), "UFOUNDER", "founder line in the stretch")
	hist.byChan["C1"] = []slackHistoryMessage{human}
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingHistory = []livenessPendingHistory{{From: from, Below: below}} })
	hist.emptyHistoryCall = 1 // the stretch's first page: nothing usable, more to come
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 0 {
		t.Fatalf("delivered %d after the empty page, want nothing yet", len(envs))
	}
	if got := l.pendingHistoryOf("C1"); got == nil || got.From != from || got.Below != below {
		t.Fatalf("pending history after the empty page = %+v, want the stretch standing (%s, %s)", got, from, below)
	}
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := deliveredTexts(rec.all())[human.Text]; got != 1 {
		t.Fatalf("the founder line was delivered %d times after the next probe, want once", got)
	}
	if got := l.pendingHistoryOf("C1"); got != nil {
		t.Fatalf("pending history after the stretch was read = %+v, want none", got)
	}
}
