package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 8 on the gf-d7b delta
// (2 P2; codex-own-packs-r8.out), both taken.

// P2: a bot-only backlog above an unmoved watermark had the read above the
// watermark re-register its whole older prefix every probe — drained pages
// included — so the stretches never finished, every probe spent its pages
// again, and already-read history could be reported lost. The watermark
// moves up to the newest message read once the backfill settled, so the
// next probe starts above what this one read.
func TestLivenessAReadThroughBacklogMovesTheWatermarkAndIsNotReadAgain(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 2
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	human := histMsg(tsAt(base, 2*time.Minute), "UFOUNDER", "founder line under the bot lines")
	msgs := []slackHistoryMessage{human}
	for i := 1; i < 1000; i++ {
		m := histMsg(tsAt(base, 2*time.Minute+time.Duration(i)*time.Second), "", "bot line")
		m.BotID = "B1"
		msgs = append(msgs, m)
	}
	hist.byChan["C1"] = msgs
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.watermarkOf("C1"); got != msgs[999].TS {
		t.Fatalf("watermark after the first probe = %s, want the newest message read %s", got, msgs[999].TS)
	}
	calls := len(hist.calls)
	for i := 0; i < 3; i++ {
		clock.advance(3 * time.Minute)
		l.tick(context.Background())
	}
	if got := deliveredTexts(rec.all())[human.Text]; got != 1 {
		t.Fatalf("the founder line was delivered %d times, want once (pending history %+v)", got, l.pendingHistoriesOf("C1"))
	}
	if got := l.pendingHistoriesOf("C1"); len(got) != 0 {
		t.Fatalf("pending history after the backlog was read through = %+v, want none", got)
	}
	// Three probes: the stretch's two pages, its last page, then the one
	// scan page each probe above the watermark — never the backlog again.
	if n := len(hist.calls) - calls; n > 3+3 {
		t.Fatalf("history calls over three probes = %d, want at most 6: the drained pages were read again (%v)", n, hist.calls[calls:])
	}
	if strings.Contains(read(), "NEVER READ") {
		t.Errorf("already-read history was reported lost: %s", read())
	}
}

// P2: after a capped replay the messages left were read twice by the next
// probe — from their stretch and above the held watermark — and the
// duplicate was charged to the cap and counted as missed: 201 missed, and
// the second probe delivered fifty. One message, counted and replayed once.
func TestLivenessAMessageReadTwiceInOneProbeIsOneMissedMessage(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	n := 2*livenessMaxBackfillPerChannel + 1
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.missedTotal.Load(); got != uint64(n) {
		t.Fatalf("missed after the first probe = %d, want %d", got, n)
	}
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if envs := rec.all(); len(envs) != 2*livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d after the second probe, want %d: a full cap of unique messages", len(envs), 2*livenessMaxBackfillPerChannel)
	}
	if got := l.missedTotal.Load(); got != uint64(n+n-livenessMaxBackfillPerChannel) {
		t.Fatalf("missed after the second probe = %d, want %d: the %d left counted once", got, n+n-livenessMaxBackfillPerChannel, n-livenessMaxBackfillPerChannel)
	}
	for text, c := range deliveredTexts(rec.all()) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
}
