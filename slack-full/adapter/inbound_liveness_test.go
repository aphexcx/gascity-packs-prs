package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the inbound-liveness watchdog + backfill (gp-3og).

// fakeHistory is a scripted historyFetcher.
type fakeHistory struct {
	mu      sync.Mutex
	byChan  map[string][]slackHistoryMessage // parents, any order
	threads map[string][]slackHistoryMessage // key channel|threadTS → replies (parent excluded)
	errs    map[string]error
	calls   []string
}

func newFakeHistory() *fakeHistory {
	return &fakeHistory{byChan: map[string][]slackHistoryMessage{}, threads: map[string][]slackHistoryMessage{}, errs: map[string]error{}}
}

func histMsg(ts, user, text string) slackHistoryMessage {
	m := slackHistoryMessage{Type: "message", TS: ts, User: user, Text: text}
	m.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": ts, "user": user, "text": text})
	return m
}

func (f *fakeHistory) history(_ context.Context, channel, oldest string, _ int) ([]slackHistoryMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "history:"+channel+":"+oldest)
	if err := f.errs[channel]; err != nil {
		return nil, err
	}
	var out []slackHistoryMessage
	for _, m := range f.byChan[channel] {
		if oldest == "" || compareSlackTS(m.TS, oldest) > 0 {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeHistory) replies(_ context.Context, channel, threadTS, oldest string, _ int) ([]slackHistoryMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "replies:"+channel+":"+threadTS+":"+oldest)
	var out []slackHistoryMessage
	for _, m := range f.threads[channel+"|"+threadTS] {
		if oldest == "" || compareSlackTS(m.TS, oldest) > 0 {
			out = append(out, m)
		}
	}
	return out, nil
}

// deliveryRecorder captures synthesized envelopes.
type deliveryRecorder struct {
	mu     sync.Mutex
	envs   []slackEventEnvelope
	status int
}

func (d *deliveryRecorder) deliver(_ context.Context, env slackEventEnvelope) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.envs = append(d.envs, env)
	if d.status == 0 {
		return http.StatusOK
	}
	return d.status
}

func (d *deliveryRecorder) all() []slackEventEnvelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]slackEventEnvelope(nil), d.envs...)
}

type alertRecorder struct {
	mu    sync.Mutex
	texts []string
}

func (a *alertRecorder) post(_ context.Context, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.texts = append(a.texts, text)
	return nil
}

func (a *alertRecorder) all() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.texts...)
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func livenessTestConfig(t *testing.T) config {
	t.Helper()
	return config{
		accountID:          "T1",
		livenessStallAfter: 10 * time.Minute,
		backfillMaxWindow:  time.Hour,
		livenessChannels:   []string{"C1"},
		livenessStatePath:  "",
	}
}

// newTestLiveness builds a tracker on a fake clock starting at base.
func newTestLiveness(t *testing.T, cfg config, fetch historyFetcher, clock *fakeClock) *inboundLiveness {
	t.Helper()
	l := &inboundLiveness{
		cfg:       cfg,
		fetch:     fetch,
		now:       clock.now,
		channels:  make(map[string]*livenessChannel),
		origins:   make(map[string]originEntry),
		statePath: cfg.livenessStatePath,
		teamID:    cfg.accountID,
	}
	l.startedAt = clock.now()
	l.loadState()
	for _, c := range cfg.livenessChannels {
		ch := l.channels[c]
		if ch == nil {
			ch = &livenessChannel{TeamID: cfg.accountID, LastSeenAt: l.startedAt}
			l.channels[c] = ch
		}
		ch.Pinned = true
		if ch.WatermarkTS == "" {
			ch.WatermarkTS = slackTSFromTime(l.startedAt)
		}
	}
	return l
}

func tsAt(base time.Time, d time.Duration) string { return slackTSFromTime(base.Add(d)) }

func liveMessageEnvelope(t *testing.T, eventID, channel, ts, user, text string) slackEventEnvelope {
	t.Helper()
	raw, _ := json.Marshal(slackMessageEvent{Type: "message", Channel: channel, User: user, TS: ts, Text: text})
	return slackEventEnvelope{Type: "event_callback", TeamID: "T1", APIAppID: "A1", EventID: eventID, Event: raw,
		Authorizations: []slackEventAuthorization{{UserID: "UBOT", IsBot: true}}}
}

// Live inbound advances the last-inbound clock, the channel watermark,
// and the origin seen-set; learned channels join the watched set.
func TestLivenessNoteInboundTracksWatermarkAndOrigins(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	l := newTestLiveness(t, livenessTestConfig(t), newFakeHistory(), clock)

	clock.advance(time.Minute)
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev1", "C9", tsAt(base, time.Minute), "U1", "hi"), ""); got != inboundOriginLive {
		t.Fatalf("note = %v, want live", got)
	}
	l.mu.Lock()
	last, ch, bot := l.lastInboundAt, l.channels["C9"], l.botUserID
	l.mu.Unlock()
	if !last.Equal(base.Add(time.Minute)) {
		t.Errorf("lastInboundAt = %s", last)
	}
	if ch == nil || ch.WatermarkTS != tsAt(base, time.Minute) || ch.TeamID != "T1" || ch.Pinned {
		t.Errorf("learned channel = %+v", ch)
	}
	if bot != "UBOT" {
		t.Errorf("botUserID = %q", bot)
	}
	if got := l.originSource("C9", tsAt(base, time.Minute)); got != inboundOriginLive {
		t.Errorf("origin = %v, want live", got)
	}
	// Older ts does not move the watermark backwards.
	l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev0", "C9", tsAt(base, 0), "U1", "late"), "")
	l.mu.Lock()
	wm := l.channels["C9"].WatermarkTS
	l.mu.Unlock()
	if wm != tsAt(base, time.Minute) {
		t.Errorf("watermark regressed to %s", wm)
	}
	// A backfill-transport envelope is not live inbound.
	before := l.now()
	clock.advance(5 * time.Minute)
	l.noteInboundEnvelope(liveMessageEnvelope(t, "backfill:C9:x", "C9", tsAt(base, 2*time.Minute), "U1", "replay"), transportBackfill)
	l.mu.Lock()
	last = l.lastInboundAt
	l.mu.Unlock()
	if !last.Equal(before) { // unchanged at base+1m
		t.Errorf("backfill transport moved lastInboundAt to %s", last)
	}
}

// The watchdog: quiet past the stall threshold + history showing human
// messages the adapter never saw ⇒ alarm, healthz stalled, Slack alert,
// backfill of exactly the missed human messages (bots/seen ones skipped),
// and the live copy of a backfilled message is then dropped. Live
// inbound afterwards logs recovery.
func TestLivenessWatchdogAlarmsAndBackfills(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	// One live message at +1m sets the C1 watermark.
	clock.advance(time.Minute)
	seenTS := tsAt(base, time.Minute)
	l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev1", "C1", seenTS, "U1", "seen live"), "")

	// Then the transport dies: history grows, events don't.
	missed1 := histMsg(tsAt(base, 3*time.Minute), "U2", "merge it")
	missed2 := histMsg(tsAt(base, 4*time.Minute), "U3", "")
	missed2.Files = []slackFile{{ID: "F1", Name: "shot.png"}}
	missed2.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": missed2.TS, "user": "U3", "files": []any{map[string]any{"id": "F1", "name": "shot.png"}}})
	botMsg := histMsg(tsAt(base, 5*time.Minute), "", "bot noise")
	botMsg.BotID = "B1"
	joinMsg := histMsg(tsAt(base, 6*time.Minute), "U4", "joined")
	joinMsg.Subtype = "channel_join"
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(seenTS, "U1", "seen live"), missed1, missed2, botMsg, joinMsg}

	// 5 minutes quiet: under threshold, no probe.
	clock.advance(5 * time.Minute)
	l.tick(context.Background())
	if l.probesTotal.Load() != 0 {
		t.Fatalf("probed before the stall threshold")
	}
	// 11 minutes quiet: probe.
	clock.advance(6 * time.Minute)
	l.tick(context.Background())
	if l.probesTotal.Load() != 1 {
		t.Fatalf("probes = %d, want 1", l.probesTotal.Load())
	}
	envs := rec.all()
	if len(envs) != 2 {
		t.Fatalf("backfilled %d envelopes, want 2 (got %+v)", len(envs), envs)
	}
	if envs[0].EventID != "backfill:C1:"+missed1.TS || envs[1].EventID != "backfill:C1:"+missed2.TS {
		t.Errorf("event ids = %s, %s", envs[0].EventID, envs[1].EventID)
	}
	var inner map[string]any
	_ = json.Unmarshal(envs[0].Event, &inner)
	if inner["channel"] != "C1" || inner["channel_type"] != "channel" || inner["event_ts"] != missed1.TS || inner["text"] != "merge it" || inner["user"] != "U2" {
		t.Errorf("synthesized event = %v", inner)
	}
	if envs[0].TeamID != "T1" || envs[0].APIAppID != "A1" || envs[0].botUserID() != "UBOT" {
		t.Errorf("synthesized envelope ids = team=%s app=%s bot=%s", envs[0].TeamID, envs[0].APIAppID, envs[0].botUserID())
	}
	logs := read()
	if !strings.Contains(logs, "INBOUND LIVENESS ALARM") || !strings.Contains(logs, "2 new human message(s)") {
		t.Errorf("alarm log missing: %s", logs)
	}
	if h := l.healthzDetail(); !strings.Contains(h, "inbound_liveness=stalled") || !strings.Contains(h, "liveness_backfilled_total=2") {
		t.Errorf("healthz = %q", h)
	}
	awaitCondition(t, func() bool { return len(alerts.all()) == 1 }, "alert posted")
	if a := alerts.all()[0]; !strings.Contains(a, "INBOUND LIVENESS ALARM") || !strings.Contains(a, "C1:2") {
		t.Errorf("alert = %q", a)
	}
	// Watermark advanced past the backfilled messages.
	l.mu.Lock()
	wm := l.channels["C1"].WatermarkTS
	l.mu.Unlock()
	if wm != missed2.TS {
		t.Errorf("watermark = %s, want %s", wm, missed2.TS)
	}
	// The live copy of a backfilled message is dropped; an unrelated
	// live message is accepted and clears the stall.
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLate", "C1", missed1.TS, "U2", "merge it"), ""); got != inboundOriginBackfilled {
		t.Errorf("late live copy = %v, want backfilled", got)
	}
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvNew", "C1", tsAt(base, 20*time.Minute), "U5", "back"), ""); got != inboundOriginLive {
		t.Errorf("fresh live = %v, want live", got)
	}
	if !strings.Contains(read(), "RECOVERED") {
		t.Errorf("recovery not logged: %s", read())
	}
	if h := l.healthzDetail(); !strings.Contains(h, "inbound_liveness=ok") {
		t.Errorf("healthz after recovery = %q", h)
	}
	awaitCondition(t, func() bool { return len(alerts.all()) == 2 }, "recovery alert posted")
	// Re-probing finds nothing new: everything is in the seen-set.
	clock.advance(15 * time.Minute)
	rec2 := &deliveryRecorder{}
	l.deliver = rec2.deliver
	l.tick(context.Background())
	if len(rec2.all()) != 0 {
		t.Errorf("second probe re-delivered %d", len(rec2.all()))
	}
}

func awaitCondition(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// With backfill disabled the watchdog still alarms but replays nothing.
func TestLivenessAlarmOnlyWhenBackfillDisabled(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	cfg.backfillMaxWindow = 0
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(tsAt(base, 2*time.Minute), "U1", "lost")}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if len(rec.all()) != 0 {
		t.Errorf("delivered %d with backfill disabled", len(rec.all()))
	}
	if logs := read(); !strings.Contains(logs, "INBOUND LIVENESS ALARM") || !strings.Contains(logs, "backfill disabled") {
		t.Errorf("logs = %s", logs)
	}
}

// A reconnect probe replays the gap without raising the alarm, and
// notes it in the alert channel.
func TestLivenessReconnectBackfillsWithoutAlarm(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(tsAt(base, 2*time.Minute), "U1", "while down")}
	clock.advance(3 * time.Minute)

	l.onTransportConnected(context.Background(), false) // first connect: no probe
	if l.probesTotal.Load() != 0 {
		t.Fatalf("first connect probed")
	}
	l.onTransportConnected(context.Background(), true)
	if len(rec.all()) != 1 {
		t.Fatalf("delivered %d, want 1", len(rec.all()))
	}
	if logs := read(); strings.Contains(logs, "ALARM") || !strings.Contains(logs, "socket reconnect") {
		t.Errorf("logs = %s", logs)
	}
	if h := l.healthzDetail(); strings.Contains(h, "stalled") {
		t.Errorf("reconnect backfill must not mark stalled: %q", h)
	}
	awaitCondition(t, func() bool { return len(alerts.all()) == 1 }, "backfill note posted")
	if a := alerts.all()[0]; !strings.Contains(a, "backfilled 1 message") {
		t.Errorf("alert = %q", a)
	}
}

// Thread replies posted during the gap into an older parent are found
// via the latest_reply scan and replayed.
func TestLivenessProbeFindsFreshThreadReplies(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{}
	l.deliver = rec.deliver

	// Parent from an hour ago (below the watermark), with a reply at +5m.
	parent := histMsg(tsAt(base, -time.Hour), "U1", "old thread")
	parent.ReplyCount = 1
	parent.LatestReply = tsAt(base, 5*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	reply := histMsg(tsAt(base, 5*time.Minute), "U2", "new reply")
	reply.ThreadTS = parent.TS
	reply.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": reply.TS, "user": "U2", "text": "new reply", "thread_ts": parent.TS})
	staleReply := histMsg(tsAt(base, -30*time.Minute), "U2", "old reply")
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{staleReply, reply}

	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if len(envs) != 1 {
		t.Fatalf("delivered %d, want the one fresh reply (calls=%v)", len(envs), hist.calls)
	}
	var inner map[string]any
	_ = json.Unmarshal(envs[0].Event, &inner)
	if inner["thread_ts"] != parent.TS || inner["text"] != "new reply" {
		t.Errorf("synthesized reply = %v", inner)
	}
}

// A channel read error is skipped (logged) and does not block the
// others; a failed delivery un-marks the origin so a live copy can land.
func TestLivenessProbeSkipsErroringChannelAndUnmarksFailedDelivery(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	cfg.livenessChannels = []string{"C1", "C2"}
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusServiceUnavailable}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	hist.errs["C1"] = fmt.Errorf("conversations.history not ok: not_in_channel")
	lost := histMsg(tsAt(base, 2*time.Minute), "U1", "lost")
	hist.byChan["C2"] = []slackHistoryMessage{lost}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if len(rec.all()) != 1 {
		t.Fatalf("delivered %d, want 1 attempt", len(rec.all()))
	}
	if logs := read(); !strings.Contains(logs, "not_in_channel") || !strings.Contains(logs, "left for a live redelivery") {
		t.Errorf("logs = %s", logs)
	}
	if got := l.originSource("C2", lost.TS); got != inboundOriginUnknown {
		t.Errorf("failed delivery left origin marked %v", got)
	}
	if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive", "C2", lost.TS, "U1", "lost"), ""); got != inboundOriginLive {
		t.Errorf("live copy after failed backfill = %v, want live (accepted)", got)
	}
}

// Watermarks and last-inbound survive a restart through the state file.
func TestLivenessStatePersistRoundTrip(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	l := newTestLiveness(t, cfg, newFakeHistory(), clock)
	clock.advance(time.Minute)
	l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev1", "C9", tsAt(base, time.Minute), "U1", "hi"), "")
	l.saveState()

	clock.advance(time.Hour)
	l2 := newTestLiveness(t, cfg, newFakeHistory(), clock)
	l2.mu.Lock()
	defer l2.mu.Unlock()
	if !l2.lastInboundAt.Equal(base.Add(time.Minute)) {
		t.Errorf("lastInboundAt = %s", l2.lastInboundAt)
	}
	if ch := l2.channels["C9"]; ch == nil || ch.WatermarkTS != tsAt(base, time.Minute) || ch.Pinned {
		t.Errorf("C9 = %+v", ch)
	}
	if ch := l2.channels["C1"]; ch == nil || !ch.Pinned {
		t.Errorf("pinned C1 = %+v", ch)
	}
}

// Restart gap: the watchdog's startup probe replays what landed while
// the process was down, and the pre-restart silence does not trip the
// stall alarm.
func TestLivenessStartupProbeReplaysRestartGap(t *testing.T) {
	base := time.Date(2026, 8, 19, 17, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	cfg := livenessTestConfig(t)
	cfg.livenessStatePath = filepath.Join(t.TempDir(), "liveness.json")
	l := newTestLiveness(t, cfg, newFakeHistory(), clock)
	clock.advance(time.Minute)
	l.noteInboundEnvelope(liveMessageEnvelope(t, "Ev1", "C1", tsAt(base, time.Minute), "U1", "before restart"), "")
	l.saveState()

	// Down for 20 minutes; two messages land.
	clock.advance(20 * time.Minute)
	hist := newFakeHistory()
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(tsAt(base, 10*time.Minute), "U2", "during downtime"), histMsg(tsAt(base, 15*time.Minute), "U3", "also")}
	l2 := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{}
	l2.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l2.runWatchdog(ctx); close(done) }()
	awaitCondition(t, func() bool { return len(rec.all()) == 2 }, "startup replay")
	cancel()
	<-done
	if logs := read(); strings.Contains(logs, "ALARM") || !strings.Contains(logs, "startup (persisted watermarks)") {
		t.Errorf("logs = %s", logs)
	}
	// 30s after start the watchdog must not alarm on the old silence.
	clock.advance(30 * time.Second)
	l2.tick(context.Background())
	if l2.alarmsTotal.Load() != 0 {
		t.Errorf("alarm fired on pre-restart silence")
	}
}

func TestHistoryMessageDispatchable(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    slackHistoryMessage
		want bool
	}{
		{"human text", slackHistoryMessage{Type: "message", User: "U1", Text: "hi"}, true},
		{"file only", slackHistoryMessage{Type: "message", User: "U1", Files: []slackFile{{ID: "F"}}}, true},
		{"file_share", slackHistoryMessage{Type: "message", Subtype: "file_share", User: "U1", Text: "x"}, true},
		{"bot", slackHistoryMessage{Type: "message", BotID: "B1", User: "U1", Text: "hi"}, false},
		{"no user", slackHistoryMessage{Type: "message", Text: "hi"}, false},
		{"join", slackHistoryMessage{Type: "message", Subtype: "channel_join", User: "U1", Text: "hi"}, false},
		{"empty", slackHistoryMessage{Type: "message", User: "U1", Text: "   "}, false},
		{"thread_broadcast", slackHistoryMessage{Type: "message", Subtype: "thread_broadcast", User: "U1", Text: "hi"}, false},
	} {
		if got := historyMessageDispatchable(tc.m); got != tc.want {
			t.Errorf("%s: dispatchable=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// End to end through the real events handler: a backfilled message
// forwards into gc once; the late live (signed HTTP) copy of the same
// message is acked 200 but not forwarded again.
func TestLivenessBackfillThroughHandlerDropsLateLiveCopy(t *testing.T) {
	gcStub, hits := countingGCStub(t)
	cfg := dedupTestConfig(t, gcStub.URL)
	base := time.Now()
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	lcfg := livenessTestConfig(t)
	l := newTestLiveness(t, lcfg, hist, clock)
	cfg.inboundLiveness = l
	events := handleSlackEvents(cfg, newTestHandleAliasRegistry(t), nil, nil, nil, nil)
	l.deliver = deliverViaHandler(events)
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	lostTS := tsAt(base, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(lostTS, "U1", "lost then found")}
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	awaitInboundHits(t, hits, 1)
	if l.backfilledTotal.Load() != 1 {
		t.Fatalf("backfilled = %d", l.backfilledTotal.Load())
	}

	// Now the live copy arrives over the wire, signed.
	raw, _ := json.Marshal(slackMessageEvent{Type: "message", Channel: "C1", User: "U1", TS: lostTS, Text: "lost then found"})
	envBody, _ := json.Marshal(slackEventEnvelope{Type: "event_callback", TeamID: "T1", EventID: "EvLive", Event: raw})
	ts := fmt.Sprint(time.Now().Unix())
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(string(envBody)))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", signFor(cfg.slackSigningKey, ts, envBody))
	w := httptest.NewRecorder()
	events(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("live copy status = %d, want 200", w.Code)
	}
	awaitInboundHits(t, hits, 1)
	if !strings.Contains(read(), "dropping live copy of a backfilled message") {
		t.Errorf("drop not logged: %s", read())
	}
}

// The production history client parses conversations.history (newest-
// first) and conversations.replies (parent first) into oldest-first
// slices with Raw retained, and surfaces Slack errors.
func TestSlackHistoryClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer xoxb-t" {
			t.Errorf("auth = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history":
			if r.URL.Query().Get("oldest") != "1.000000" || r.URL.Query().Get("channel") != "C1" {
				t.Errorf("history query = %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"3.000000","user":"U1","text":"c","reply_count":2,"latest_reply":"9.0"},{"type":"message","ts":"2.000000","user":"U1","text":"b"}]}`))
		case "/conversations.replies":
			if r.URL.Query().Get("ts") != "3.000000" {
				t.Errorf("replies query = %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","ts":"3.000000","user":"U1","text":"c"},{"type":"message","ts":"4.000000","user":"U2","text":"r1","thread_ts":"3.000000"}]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":false,"error":"not_in_channel"}`))
		}
	}))
	t.Cleanup(srv.Close)
	prev := slackAPIBase
	slackAPIBase = srv.URL
	t.Cleanup(func() { slackAPIBase = prev })

	c := newSlackHistoryClient("xoxb-t")
	msgs, err := c.history(context.Background(), "C1", "1.000000", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].TS != "2.000000" || msgs[1].TS != "3.000000" || msgs[1].ReplyCount != 2 || msgs[1].LatestReply != "9.0" {
		t.Errorf("history = %+v", msgs)
	}
	if !strings.Contains(string(msgs[0].Raw), `"text":"b"`) {
		t.Errorf("raw not retained: %s", msgs[0].Raw)
	}
	replies, err := c.replies(context.Background(), "C1", "3.000000", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(replies) != 1 || replies[0].TS != "4.000000" || replies[0].ThreadTS != "3.000000" {
		t.Errorf("replies = %+v (parent must be dropped)", replies)
	}
	if _, err := c.get(context.Background(), "/conversations.other", nil, ""); err == nil || !strings.Contains(err.Error(), "not_in_channel") {
		t.Errorf("error surface = %v", err)
	}
}

func TestSlackTSFromTimeAndChannelType(t *testing.T) {
	ts := slackTSFromTime(time.Date(2026, 8, 19, 17, 5, 48, 837329000, time.UTC))
	if ts != "1787159148.837329" {
		t.Errorf("ts = %s", ts)
	}
	for id, want := range map[string]string{"C1": "channel", "D1": "im", "G1": "group", "": "", "X": ""} {
		if got := channelTypeFromID(id); got != want {
			t.Errorf("channelTypeFromID(%q) = %q, want %q", id, got, want)
		}
	}
}
