package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 5 on the gf-d7b delta
// (4 P1; codex-own-packs-r5.out), every one taken: every message a pass
// leaves gets its owner — an unread stretch — the moment it is left,
// whatever the hold set does; a settle takes only what was read out of the
// stretch that holds it and keeps what was added meanwhile; a listing hold's
// cursor advances after a page it delivered.

// P1: a stretch that yielded more than the cap and the hold set take had
// its overflow registered inside its own bounds, and the settle — the
// stretch read through — dropped it. The left range stays pending: live
// copies release every hold, and the next probes still replay the rest.
func TestLivenessWhatAPassLeavesInsideAReadStretchStaysPending(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	from := tsAt(base, -30*time.Minute)
	var msgs []slackHistoryMessage
	for i := 0; i < 250; i++ {
		msgs = append(msgs, histMsg(tsAt(base, -25*time.Minute+time.Duration(i)*time.Second), "U1", fmt.Sprintf("stretch %d", i)))
	}
	hist.byChan["C1"] = msgs
	l.setChannelLocked("C1", func(ch *livenessChannel) {
		ch.PendingHistory = []livenessPendingHistory{{From: from, Below: slackTSAfter(msgs[249].TS)}}
	})
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d, want the cap", len(envs))
	}
	covered := false
	for _, st := range l.pendingHistoriesOf("C1") {
		if compareSlackTS(st.From, msgs[100].TS) < 0 && compareSlackTS(st.Below, msgs[249].TS) > 0 {
			covered = true
		}
	}
	if !covered {
		t.Fatalf("pending history = %+v, want a stretch covering everything the pass left (%s..%s)", l.pendingHistoriesOf("C1"), msgs[100].TS, msgs[249].TS)
	}
	// Live copies of every held message release the holds.
	for _, ts := range l.pendingTSOf("C1") {
		l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev"+ts, "C1", ts, "U1", "live copy"), "")
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the live copies = %v, want none", got)
	}
	replayUntilDrained(t, l, clock, 4)
	texts := deliveredTexts(rec.all())
	for i := 200; i < 250; i++ {
		if texts[msgs[i].Text] != 1 {
			t.Fatalf("%q replayed %d times, want once: the overflow inside the read stretch was lost", msgs[i].Text, texts[msgs[i].Text])
		}
	}
}

// P1: with the pending set full, a listed thread whose page was delivered
// but had more got a listing hold from the read's floor, so the next probe
// read the same page again, forever. The hold's cursor moves up to what
// was delivered.
func TestLivenessAListingHoldsCursorAdvancesPastADeliveredPage(t *testing.T) {
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
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := repliesDelivered(rec.all()); got[parent.TS] != 10 {
		t.Fatalf("delivered %d replies on the first probe, want the page of 10", got[parent.TS])
	}
	l.mu.Lock()
	held, known := l.channels["C1"].PendingMessages[replies[14].TS]
	l.mu.Unlock()
	if !known || !held.Listing || held.Cursor != replies[9].TS {
		t.Fatalf("listing hold = %+v (known=%v), want the thread held from the last delivered reply %s", held, known, replies[9].TS)
	}
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := repliesDelivered(rec.all()); got[parent.TS] != 15 {
		t.Fatalf("delivered %d replies after the next probe, want all 15 (calls=%v)", got[parent.TS], hist.calls)
	}
	for text, c := range deliveredTexts(rec.all()) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the thread was read through = %v, want none", got)
	}
}

// P1: an older message's hold evicted a newer one's, and the evicted
// message — not in this probe's missed set when the read above the
// watermark failed — had no owner left; live copies released the
// replacement holds and the held advance carried the watermark past it.
// Every held message has its stretch from the pass that left it.
func TestLivenessAnEvictedHoldsMessageKeepsItsStretch(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	newer := missedTopLevel(base, 200) // above the watermark
	hist.byChan["C1"] = newer
	clock.advance(12 * time.Minute)
	l.tick(context.Background()) // 1-100 delivered, 101-200 held
	if got := l.pendingTSOf("C1"); len(got) != 100 || got[0] != newer[100].TS {
		t.Fatalf("holds = %d from %v, want 101-200", len(got), got[:min(len(got), 1)])
	}
	// An older stretch below the watermark, 150 messages; the read above
	// the watermark then fails.
	var older []slackHistoryMessage
	for i := 0; i < 150; i++ {
		older = append(older, histMsg(tsAt(base, -25*time.Minute+time.Duration(i)*time.Second), "U1", fmt.Sprintf("older %d", i)))
	}
	hist.byChan["C1"] = append(older, newer...)
	l.setChannelLocked("C1", func(ch *livenessChannel) {
		ch.PendingHistory = append(ch.PendingHistory, livenessPendingHistory{From: tsAt(base, -30*time.Minute), Below: slackTSAfter(older[149].TS)})
	})
	hist.failHistoryCall = len(hist.calls) + 3 // the two stretch reads land; the read above the watermark fails
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	holds := l.pendingTSOf("C1")
	if len(holds) != 100 || holds[0] != older[100].TS || holds[99] != newer[150-1].TS {
		t.Fatalf("holds after the eviction = %d (%s..%s), want the 50 oldest older ones and 101-150: 151-200 evicted", len(holds), holds[0], holds[len(holds)-1])
	}
	// Live copies release every hold; the held advance is taken.
	for _, ts := range holds {
		l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev"+ts, "C1", ts, "U1", "live copy"), "")
	}
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the live copies = %v, want none", got)
	}
	replayUntilDrained(t, l, clock, 4)
	texts := deliveredTexts(rec.all())
	for i := 150; i < 200; i++ {
		if texts[newer[i].Text] != 1 {
			t.Fatalf("%q replayed %d times, want once: the evicted hold's message was lost (delivered %d in all)", newer[i].Text, texts[newer[i].Text], len(rec.all()))
		}
	}
}

// P1: the overflow was registered after the whole pass, from the holds
// that stood then; live copies during another channel's delivery had
// released them, and the registration skipped the channel. A left message
// gets its stretch the moment it is left.
func TestLivenessALeftMessageIsOwnedBeforeAnotherChannelsDelivery(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	cfg.livenessChannels = []string{"C1", "C2"}
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	n := 2*livenessMaxBackfillPerChannel + 1 // 1-100 replay, 101-200 are held, 201 is left beyond the holds
	c1 := missedTopLevel(base, n)
	c2 := histMsg(tsAt(base, 10*time.Minute), "U1", "the other channel's message")
	above := tsAt(base, 11*time.Minute)
	l.deliver = func(ctx context.Context, env slackEventEnvelope) backfillDelivery {
		var inner map[string]any
		_ = json.Unmarshal(env.Event, &inner)
		if inner["channel"] == "C2" {
			// While C2's message is being delivered, C1's held hundred
			// land live and a newer live message moves C1's watermark.
			for i := 100; i < n-1; i++ {
				l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev"+c1[i].TS, "C1", c1[i].TS, "U1", c1[i].Text), "")
			}
			l.noteInboundEnvelope(liveMessageEnvelope(t, "EvAbove", "C1", above, "U1", "above"), "")
		}
		return rec.deliver(ctx, env)
	}
	hist.byChan["C1"] = c1
	hist.byChan["C2"] = []slackHistoryMessage{c2}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.watermarkOf("C1"); got != above {
		t.Fatalf("C1 watermark = %s, want the live advance %s taken once the holds released", got, above)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(above, "U1", "above"))
	replayUntilDrained(t, l, clock, 4)
	if got := deliveredTexts(rec.all()); got[c1[n-1].Text] != 1 {
		t.Fatalf("the message C1's pass left was replayed %d times, want once: its owner was registered too late", got[c1[n-1].Text])
	}
}
