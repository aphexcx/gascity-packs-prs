package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the lane's own codex round 9 on the gf-d7b delta
// (1 P2; codex-own-packs-r9.out), taken.

// P2: the backfill window's floor entered a pending stretch between the
// probe's retire pass and that channel's read (earlier channels took that
// long): the read started at the newer floor, the settle at the stretch's
// From, and a read-through dropped the unread prefix under the floor as
// though read, with no report. The prefix stands, and the next probe's
// retire pass reports it.
func TestLivenessAPrefixTheFloorExcludesMidProbeIsReported(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	cfg.livenessChannels = []string{"C1", "C2"}
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	// C2's stretch (-50m, -30m) holds a human line at -45m. C1 is probed
	// first (pinned order) and its read takes twenty minutes: by the time
	// C2 is read, the floor (now - 1 h) is at -28m, past the whole stretch.
	human := histMsg(tsAt(base, -45*time.Minute), "UFOUNDER", "founder line under the floor")
	hist.byChan["C2"] = []slackHistoryMessage{human}
	l.setChannelLocked("C2", func(ch *livenessChannel) {
		ch.PendingHistory = []livenessPendingHistory{{From: tsAt(base, -50*time.Minute), Below: tsAt(base, -30*time.Minute)}}
	})
	hist.onHistory = func(channel string) {
		if channel == "C1" {
			clock.advance(20 * time.Minute)
		}
	}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("delivered %d, want nothing: the line is under the floor by the time C2 is read", len(got))
	}
	if got := l.pendingHistoriesOf("C2"); len(got) == 0 {
		t.Fatalf("pending history of C2 = none after the probe, want the excluded prefix standing for the retire pass")
	}
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if !strings.Contains(read(), "channel=C2 retired the unread history pages") {
		t.Fatalf("the excluded prefix was not reported by the retire pass: %s", read())
	}
	awaitCondition(t, func() bool {
		for _, a := range alerts.all() {
			if strings.Contains(a, "<#C2>") && strings.Contains(a, "was never read within the backfill window") {
				return true
			}
		}
		return false
	}, "the operator post about C2's excluded prefix")
	if got := l.pendingHistoriesOf("C2"); len(got) != 0 {
		t.Fatalf("pending history of C2 after the retire pass = %+v, want none", got)
	}
}
