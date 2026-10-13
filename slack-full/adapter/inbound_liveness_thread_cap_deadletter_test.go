package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the Opus 5.5 delta read's MINOR 1 at 0e5bd58a,
// gf-d7b finding 3p (inbound_liveness.go:535): the 33rd pending thread in a
// channel (livenessMaxPendingThreads = 32) was only logged, so its unread
// replies could be lost with no record. With finding 3m a thread the pending
// set cannot take is read this probe, and one whose read fails holds the
// watermark below its latest reply; this file proves the end of that path: a
// hold the window closes on goes to the inbound dead letter naming the
// thread, its latest reply and the cursor its replies were never read above,
// logged and posted. The dead-letter helpers here serve finding 3q's tests
// too (inbound_liveness_pending_thread_deadletter_test.go).

func deadLetterTestConfig(t *testing.T) config {
	t.Helper()
	cfg := livenessTestConfig(t)
	cfg.cityName = "citadel"
	cfg.provider = "slack"
	cfg.inboundDeadLetterDir = filepath.Join(t.TempDir(), "inbound_dead_letter")
	return cfg
}

func readDeadLetter(t *testing.T, cfg config) []inboundDeadLetterRecord {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cfg.inboundDeadLetterDir, "C1.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []inboundDeadLetterRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var rec inboundDeadLetterRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("dead letter record: %v: %s", err, line)
		}
		out = append(out, rec)
	}
	return out
}

func awaitDeadLetterAlert(t *testing.T, alerts *alertRecorder) {
	t.Helper()
	awaitCondition(t, func() bool {
		for _, a := range alerts.all() {
			if strings.Contains(a, "inbound dead letter") {
				return true
			}
		}
		return false
	}, "the dead-letter post on the operator channel")
}

// 3p: the pending set full of failing threads, a 33rd thread's read fails
// too: its listing hold keeps the watermark below its latest reply, and
// when the backfill window closes on it the hold goes to the dead letter
// naming the thread, its latest reply and the cursor its replies were never
// read above — logged and posted, never silent.
func TestLivenessTheThirtyThirdPendingThreadIsNeverLostSilently(t *testing.T) {
	base := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: base}
	hist := newFakeHistory()
	cfg := deadLetterTestConfig(t)
	l := newTestLiveness(t, cfg, hist, clock)
	rec := &deliveryRecorder{status: http.StatusOK}
	l.deliver = rec.deliver
	alerts := &alertRecorder{}
	l.alert = alerts.post
	read, cleanup := captureLog(t)
	t.Cleanup(cleanup)
	fillPendingThreads(t, l, hist, base)

	watermarkBefore := l.watermarkOf("C1")
	parent, reply := threadWithReply(base, -time.Hour, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingTSOf("C1"); len(got) != 1 || got[0] != reply.TS {
		t.Fatalf("holds = %v, want the 33rd thread's latest reply %s", got, reply.TS)
	}
	// The reads keep failing through the window.
	clock.advance(70 * time.Minute)
	l.tick(context.Background())
	var listing []inboundDeadLetterRecord
	for _, r := range readDeadLetter(t, cfg) {
		if r.Inbound.ReplyToMessageID == parent.TS && r.Inbound.ProviderMessageID == reply.TS {
			listing = append(listing, r)
		}
	}
	if len(listing) != 1 {
		t.Fatalf("dead letter records for the 33rd thread's listing hold = %d, want one (log: %s)", len(listing), read())
	}
	if r := listing[0]; !strings.Contains(r.Reason, "listing hold") || !strings.Contains(r.Reason, "thread "+parent.TS+" above "+watermarkBefore) ||
		r.Inbound.Conversation.ConversationID != "C1" || r.Inbound.Conversation.ScopeID != "citadel" {
		t.Fatalf("dead letter record = %+v, want the thread, the cursor %s, the channel and the city in it", r, watermarkBefore)
	}
	if !strings.Contains(read(), "dead-lettered to ") {
		t.Errorf("the dead letter is not logged: %s", read())
	}
	awaitDeadLetterAlert(t, alerts)
	if got := l.pendingTSOf("C1"); len(got) != 0 {
		t.Fatalf("holds after the retirement = %v, want none", got)
	}
}
