package main

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Regression coverage for gf-d7b finding 3o (the gf-c7n vs-upstream gate's
// Major at inbound_liveness.go:1742): the channel history read took one
// conversations.history page (livenessHistoryLimit = 200, newest-first) and
// dropped has_more. With more missed messages than one page inside the
// recovery window, the newest 200 replayed and the watermark moved past the
// rest. The read now pages on has_more while the pages are still above the
// watermark (Slack's latest, exclusive, set to the oldest ts of the page
// before); pages a probe cannot finish (livenessMaxHistoryPages) are pending
// on the channel — the stretch above the watermark and below the oldest ts
// read — read first by the next probe and persisted with the state, and the
// window closing on an unread stretch is logged and posted, never silent.

// pendingHistoryOf returns the channel's lowest unread stretch, nil when none.
func (l *inboundLiveness) pendingHistoryOf(channel string) *livenessPendingHistory {
	all := l.pendingHistoriesOf(channel)
	if len(all) == 0 {
		return nil
	}
	return &all[0]
}

func (l *inboundLiveness) pendingHistoriesOf(channel string) []livenessPendingHistory {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ch := l.channels[channel]; ch != nil {
		return slices.Clone(ch.PendingHistory)
	}
	return nil
}

// replayUntilDrained runs retry probes a minute apart under live traffic
// until nothing is held or n probes ran, and returns the probes run.
func replayUntilDrained(t *testing.T, l *inboundLiveness, clock *fakeClock, n int) int {
	t.Helper()
	for i := 1; i <= n; i++ {
		clock.advance(3 * time.Minute)
		l.tick(context.Background())
		if len(l.pendingTSOf("C1")) == 0 && l.pendingHistoryOf("C1") == nil {
			return i
		}
	}
	return n
}

// 201 missed messages inside the window: the probe reads two pages (the
// second below the first's oldest ts), finds all 201, replays the cap's
// hundred and holds the rest; a live message above them does not lose any;
// the retry probes replay every one once. At 8373ee56 the read took one page
// and the oldest message was never seen again.
func TestLivenessBackfillPagesPastTheFirstHistoryPage(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver

	n := livenessHistoryLimit + 1
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.missedTotal.Load(); got != uint64(n) {
		t.Fatalf("missed = %d after the first probe, want all %d across two pages (calls=%v)", got, n, hist.calls)
	}
	pages := 0
	for _, c := range hist.calls {
		if strings.HasPrefix(c, "history:C1:") {
			pages++
		}
	}
	if pages != 2 || !strings.Contains(strings.Join(hist.calls, " "), ":latest="+missed[1].TS) {
		t.Fatalf("history calls = %v, want two pages, the second below %s", hist.calls, missed[1].TS)
	}
	if envs := rec.all(); len(envs) != livenessMaxBackfillPerChannel {
		t.Fatalf("delivered %d after the first probe, want the cap %d", len(envs), livenessMaxBackfillPerChannel)
	}
	// Everything the pass left (101-201) is an unread stretch of its own
	// (the lane's codex rounds 4 and 5), the holds beside it.
	if got := l.pendingHistoryOf("C1"); got == nil || got.From != slackTSBefore(missed[100].TS) || got.Below != slackTSAfter(missed[n-1].TS) {
		t.Fatalf("pending history = %+v, want the stretch of everything left (just below %s, just above %s)", got, missed[100].TS, missed[n-1].TS)
	}
	live := tsAt(base, 20*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	replayUntilDrained(t, l, clock, 5)
	envs := rec.all()
	if len(envs) != n {
		t.Fatalf("delivered %d, want all %d", len(envs), n)
	}
	for text, c := range deliveredTexts(envs) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
	if got := l.watermarkOf("C1"); got != live {
		t.Fatalf("watermark = %s, want the live advance %s", got, live)
	}
}

// More pages than a probe reads (the bound lowered to two): the probe reads
// the newest two, records the stretch below them as pending history, replays
// what the cap allows and holds the rest; the stretch survives a live
// message and the state file; the reloaded tracker reads the stretch first
// and the later probes replay every one of the 401 messages once.
func TestLivenessHistoryPagesAProbeCannotFinishArePendingAndReadFirstByTheNext(t *testing.T) {
	prev := livenessMaxHistoryPages
	livenessMaxHistoryPages = 2
	t.Cleanup(func() { livenessMaxHistoryPages = prev })
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
	n := 2*livenessHistoryLimit + 1
	missed := missedTopLevel(base, n)
	hist.byChan["C1"] = missed
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.missedTotal.Load(); got != uint64(n-1) {
		t.Fatalf("missed = %d after the first probe, want the two pages' %d", got, n-1)
	}
	if got := l.pendingHistoryOf("C1"); got == nil || got.From != watermarkBefore || got.Below != missed[1].TS {
		t.Fatalf("pending history = %+v, want (%s, %s): the stretch the bound left", got, watermarkBefore, missed[1].TS)
	}
	if !strings.Contains(read(), "has more than 2 pages of history above its watermark") {
		t.Errorf("the pending stretch is not logged: %s", read())
	}
	live := tsAt(base, 20*time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C1", live, "U1", "live"), ""); got != inboundOriginLive {
		t.Fatalf("live message = %v, want live", got)
	}
	hist.byChan["C1"] = append(hist.byChan["C1"], histMsg(live, "U1", "live"))
	// The stretch and the delivered record survive the state file.
	l.statePath = filepath.Join(t.TempDir(), "liveness.json")
	l.saveState()
	lcfg.livenessStatePath = l.statePath
	reloaded := newTestLiveness(t, lcfg, hist, clock)
	reloaded.deliver = rec.deliver
	if got := reloaded.pendingHistoryOf("C1"); got == nil || got.From != watermarkBefore || got.Below != missed[1].TS {
		t.Fatalf("reloaded pending history = %+v, want the stretch", got)
	}
	clock.advance(3 * time.Minute)
	reloaded.tick(context.Background())
	// The one-message stretch the bound left was read through and is
	// gone; what stands is the range the replays left (held beside it).
	for _, st := range reloaded.pendingHistoriesOf("C1") {
		if st.From == watermarkBefore {
			t.Fatalf("pending history after the next probe = %+v, want the bound's stretch (%s, %s) read through and gone (calls=%v)", st, watermarkBefore, missed[1].TS, hist.calls)
		}
	}
	if !strings.Contains(read(), "read the unread history pages") {
		t.Errorf("the drained stretch is not logged: %s", read())
	}
	replayUntilDrained(t, reloaded, clock, 8)
	envs := rec.all()
	if len(envs) != n {
		t.Fatalf("delivered %d, want all %d (calls=%v)", len(envs), n, hist.calls)
	}
	for text, c := range deliveredTexts(envs) {
		if c != 1 {
			t.Errorf("%q replayed %d times, want once", text, c)
		}
	}
	if got := reloaded.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds = %v, want none", got)
	}
	if got := reloaded.watermarkOf("C1"); got != live {
		t.Fatalf("watermark = %s, want the live advance %s", got, live)
	}
}

// A pending stretch the backfill window closes on can never be read (the
// history reads keep failing meanwhile): what is below the floor is cut
// from the stretch as the floor rises, and once the floor passes the whole
// stretch it is retired with a loud log line and a post to the operator
// channel.
func TestLivenessUnreadHistoryPagesTheWindowClosesOnAreRetiredLoudly(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	hist.errs["C1"] = fmt.Errorf("conversations.history not ok: ratelimited")
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	from, below := tsAt(base, -30*time.Minute), tsAt(base, 30*time.Minute)
	l.mu.Lock()
	l.channels["C1"].PendingHistory = []livenessPendingHistory{{From: from, Below: below}}
	l.mu.Unlock()
	// 45 minutes on, the floor (now - 1 h) passes From: the stretch shrinks
	// to the floor; the read fails, so it stands.
	clock.advance(45 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingHistoryOf("C1"); got == nil || got.From != slackTSFromTime(clock.now().Add(-time.Hour)) || got.Below != below {
		t.Fatalf("pending history after the floor passed From = %+v, want (%s, %s): shrunk to the floor", got, slackTSFromTime(clock.now().Add(-time.Hour)), below)
	}
	// 95 minutes on, the floor passes Below: retired, loudly.
	clock.advance(50 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingHistoryOf("C1"); got != nil {
		t.Fatalf("pending history after the window closed = %+v, want none", got)
	}
	if !strings.Contains(read(), "NEVER READ") {
		t.Errorf("the retired stretch is not logged loudly: %s", read())
	}
	awaitCondition(t, func() bool {
		for _, a := range alerts.all() {
			if strings.Contains(a, "was never read within the backfill window") {
				return true
			}
		}
		return false
	}, "the operator post about the unread stretch")
}
