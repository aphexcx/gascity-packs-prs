package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the Opus 5.5 delta read's MINOR beside gf-c7n
// finding 3l (inbound_liveness.go:559-582 with :377-410 at e8b760cf): a held
// replay was retried only by the next history scan — after 10 minutes with
// no inbound at all, or a reconnect or restart — and a hold older than the
// backfill window was retired unreplayed, so on a busy workspace a held
// founder line could still be lost, silently. A held replay is now retried
// on its own timer while live traffic flows (one retry probe per
// livenessHoldRetryInterval, no alarm), and a hold that ages out goes to the
// adapter's inbound dead letter with its text, logged and alerted.

// Live traffic every minute keeps the watchdog quiet; the held replay is
// still retried within livenessHoldRetryInterval and lands once gc is back.
func TestLivenessHeldReplaysRetryOnATimerUnderLiveTraffic(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	l := newTestLiveness(t, livenessTestConfig(t), hist, clock)
	rec := &deliveryRecorder{status: http.StatusInternalServerError}
	l.deliver = rec.deliver
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	held := tsAt(base, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{histMsg(held, "UFOUNDER", "founder line")}
	clock.advance(12 * time.Minute)
	l.tick(context.Background()) // the stall probe: the replay fails, a hold
	alarms := l.alarmsTotal.Load()
	if got := l.backfilledTotal.Load(); got != 0 {
		t.Fatalf("backfilled = %d after the failed replay, want 0", got)
	}
	// gc is back; traffic flows every minute, so no stall probe will run.
	rec.mu.Lock()
	rec.status = http.StatusOK
	rec.mu.Unlock()
	var attempts int
	for i := 1; i <= 5; i++ {
		clock.advance(time.Minute)
		live := tsAt(base, 12*time.Minute+time.Duration(i)*time.Minute)
		if got := l.noteInboundEnvelope(liveMessageEnvelope(t, "EvLive"+live, "C1", live, "U1", "live"), ""); got != inboundOriginLive {
			t.Fatalf("live message = %v, want live", got)
		}
		l.tick(context.Background())
		attempts = len(rec.all())
	}
	if attempts < 2 {
		t.Fatalf("delivery attempts = %d after 5 minutes of live traffic, want the held replay retried", attempts)
	}
	if got := l.backfilledTotal.Load(); got != 1 {
		t.Fatalf("backfilled = %d, want the held founder line delivered by the retry", got)
	}
	if got := l.alarmsTotal.Load(); got != alarms {
		t.Fatalf("alarms = %d, want unchanged (%d): a retry probe is not a stall", got, alarms)
	}
	if !strings.Contains(read(), "held replay retry") {
		t.Errorf("the retry probe is not logged: %s", read())
	}
	l.mu.Lock()
	pending := len(l.channels["C1"].PendingTS)
	msgs := len(l.channels["C1"].PendingMessages)
	l.mu.Unlock()
	if pending != 0 || msgs != 0 {
		t.Fatalf("pending = %d held messages = %d after the retry landed, want none", pending, msgs)
	}
}

// A hold that ages out of the backfill window is never dropped silently: it
// goes to the inbound dead letter with its text and thread, the log says so,
// and the operator channel is told.
func TestLivenessAHoldThatAgesOutGoesToTheInboundDeadLetter(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := livenessTestConfig(t)
	cfg.cityName = "citadel"
	cfg.provider = "slack"
	cfg.inboundDeadLetterDir = filepath.Join(t.TempDir(), "inbound_dead_letter")
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusInternalServerError}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)

	held := tsAt(base, 2*time.Minute)
	parent := tsAt(base, -time.Hour)
	msg := histMsg(held, "UFOUNDER", "founder line in a thread")
	msg.ThreadTS = parent
	hist.byChan["C1"] = []slackHistoryMessage{msg}
	clock.advance(12 * time.Minute)
	l.tick(context.Background()) // the replay fails, a hold
	l.mu.Lock()
	heldRec, known := l.channels["C1"].PendingMessages[held]
	l.mu.Unlock()
	if !known || heldRec.Msg.Text != "founder line in a thread" || heldRec.Attempts != 1 {
		t.Fatalf("held message = %+v (known=%v), want the founder line with 1 attempt", heldRec, known)
	}
	// gc stays down through the window; the hold retries and keeps failing.
	clock.advance(70 * time.Minute)
	l.tick(context.Background())
	path := filepath.Join(cfg.inboundDeadLetterDir, "C1.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("dead letter %s: %v (log: %s)", path, err, read())
	}
	var rec1 inboundDeadLetterRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &rec1); err != nil {
		t.Fatalf("dead letter record: %v: %s", err, data)
	}
	if rec1.Inbound.ProviderMessageID != held || rec1.Inbound.Text != "founder line in a thread" || rec1.Inbound.ReplyToMessageID != parent ||
		rec1.Inbound.Conversation.ConversationID != "C1" || rec1.Inbound.Conversation.ScopeID != "citadel" || rec1.Inbound.Actor.ID != "UFOUNDER" || rec1.Attempts < 1 {
		t.Fatalf("dead letter record = %+v, want the held founder line with its thread, channel and city", rec1)
	}
	if !strings.Contains(rec1.Reason, "aged out of the backfill window") {
		t.Fatalf("dead letter reason = %q", rec1.Reason)
	}
	if !strings.Contains(read(), "dead-lettered to "+path) {
		t.Errorf("the dead letter is not logged: %s", read())
	}
	// The stall alarm of the first probe posts too; the dead-letter line
	// is its own post.
	deadLetterAlerts := func() int {
		n := 0
		for _, a := range alerts.all() {
			if strings.Contains(a, "inbound dead letter") {
				n++
			}
		}
		return n
	}
	deadline := time.Now().Add(2 * time.Second)
	for deadLetterAlerts() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := deadLetterAlerts(); got != 1 {
		t.Fatalf("dead-letter alerts = %d (all: %v), want the one line on the operator channel", got, alerts.all())
	}
	l.mu.Lock()
	pending := len(l.channels["C1"].PendingTS)
	msgs := len(l.channels["C1"].PendingMessages)
	l.mu.Unlock()
	if pending != 0 || msgs != 0 {
		t.Fatalf("pending = %d held messages = %d after the retirement, want none", pending, msgs)
	}
}
