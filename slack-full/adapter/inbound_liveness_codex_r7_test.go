package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 7 on the gf-d7b delta
// (2 P1, 1 P2; codex-own-packs-r7.out), every one taken.

// P1: the read above the watermark left the same range again every probe
// (the bound, the watermark unmoved), and that re-registration was taken
// for work added into the stretch, so the settle restored the whole range
// and undid the read's progress: a human line behind pages of bot lines
// was never reached. Only messages a pass leaves count as added work.
func TestLivenessAReRegisteredStretchKeepsItsProgress(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 2
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	// One human line, then 999 bot lines above it: five pages, two per
	// probe; nothing moves the watermark.
	human := histMsg(tsAt(base, 2*time.Minute), "UFOUNDER", "founder line under the bot lines")
	msgs := []slackHistoryMessage{human}
	for i := 1; i < 1000; i++ {
		m := histMsg(tsAt(base, 2*time.Minute+time.Duration(i)*time.Second), "", "bot line")
		m.BotID = "B1"
		msgs = append(msgs, m)
	}
	hist.byChan["C1"] = msgs
	clock.advance(12 * time.Minute)
	for i := 0; i < 5; i++ {
		l.tick(context.Background())
		if deliveredTexts(rec.all())[human.Text] == 1 {
			return
		}
		clock.advance(3 * time.Minute)
	}
	t.Fatalf("the founder line under the bot lines was not delivered in five probes: delivered %v, pending history %+v", deliveredTexts(rec.all()), l.pendingHistoriesOf("C1"))
}

// P1: a listing-held thread the cap cut once a pending slot was free became
// pending from the probe's reading, above the hold's cursor; the read from
// the hold's cursor delivered a page below the new entry's cursor, the
// entry would not go lower, the hold released, and the replies between
// were skipped. The pending entry starts from the hold's cursor.
func TestLivenessACutThreadBecomesPendingFromItsListingCursor(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	hist.pageSize = 1
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	fillPendingThreads(t, l, hist, base)

	// The thread: replies at +1m and +2m, its latest at +4m; a channel
	// message at +3m moves the watermark between them; its read fails.
	parent := histMsg(tsAt(base, -time.Hour), "U1", "old thread")
	var replies []slackHistoryMessage
	for i, at := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		r := histMsg(tsAt(base, at), "U2", fmt.Sprintf("reply %d", i))
		r.ThreadTS = parent.TS
		r.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": r.TS, "user": "U2", "text": r.Text, "thread_ts": parent.TS})
		replies = append(replies, r)
	}
	parent.ReplyCount, parent.LatestReply = 3, replies[2].TS
	channelMsg := histMsg(tsAt(base, 3*time.Minute), "U1", "channel message")
	hist.byChan["C1"] = []slackHistoryMessage{parent, channelMsg}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != replies[2].TS {
		t.Fatalf("holds = %v, want the listing hold", got)
	}
	// A pending slot frees (one of the full set's threads is retired by
	// the scan window), ten fresher threads push the held one past the
	// cap, and its read recovers.
	l.setChannelLocked("C1", func(ch *livenessChannel) { ch.PendingThreads = ch.PendingThreads[1:] })
	for i := 0; i < livenessMaxThreadsPerChannel; i++ {
		p, r := threadWithReply(base, -50*time.Minute+time.Duration(i)*time.Minute, 5*time.Minute+time.Duration(i)*time.Minute)
		hist.byChan["C1"] = append(hist.byChan["C1"], p)
		hist.threads["C1|"+p.TS] = []slackHistoryMessage{r}
	}
	delete(hist.errs, "C1|"+parent.TS)
	for i := 0; i < 5; i++ { // one reply a page, a probe each
		clock.advance(3 * time.Minute)
		l.tick(context.Background())
	}
	if got := repliesDelivered(rec.all()); got[parent.TS] != 3 {
		t.Fatalf("thread replies delivered = %d, want all three (calls=%v)", got[parent.TS], hist.calls)
	}
}

// P2: the entry the owed loop synthesized for a listing-held thread whose
// parent the listing no longer named carried no latest reply, so a read
// that delivered a page and found more could not move the hold's cursor
// and the next probe read the same page again. The entry carries the
// hold's key.
func TestLivenessAnOwedListingHeldThreadPagesPastItsFirstPage(t *testing.T) {
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
	replies := threadReplies(base, parent, 15)
	parent.LatestReply = replies[14].TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = replies
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background()) // the listing hold
	// The parent falls off the listing; the read recovers.
	for i := 0; i < livenessHistoryLimit+10; i++ {
		m := histMsg(tsAt(base, -30*time.Minute+time.Duration(i)*time.Second), "", "bot line")
		m.BotID = "B1"
		hist.byChan["C1"] = append(hist.byChan["C1"], m)
	}
	delete(hist.errs, "C1|"+parent.TS)
	replayUntilDrained(t, l, clock, 6)
	if got := repliesDelivered(rec.all()); got[parent.TS] != 15 {
		t.Fatalf("thread replies delivered = %d, want all 15 across the pages (calls=%v)", got[parent.TS], hist.calls)
	}
	for text, c := range deliveredTexts(rec.all()) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
}
