package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression coverage for the Opus 5.5 delta read's MINOR 2 at 0e5bd58a,
// gf-d7b finding 3q (inbound_liveness.go:1149 and :654): a pending thread's
// cursor below the one-hour backfill floor was silently raised to the floor,
// and its retirement after the 24-hour scan window logged "left to a live
// redelivery", which Slack never does; nothing reached the dead letter or
// the operator channel. Both losses now go to the inbound dead letter with
// the parent ts, the cursor, the channel and a reason, logged and posted.

// 3q: a pending thread whose reads keep failing for over an hour: the
// replies between its cursor and the backfill floor can no longer be read —
// dead-lettered ONCE with the parent, the cursor, the channel and the
// reason, logged and posted; the thread stays pending above the floor, the
// next probe adds no second record, and when the read recovers the replies
// above the floor are delivered.
func TestLivenessAPendingThreadsRepliesTheFloorPassesGoToTheDeadLetterOnce(t *testing.T) {
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

	watermarkBefore := l.watermarkOf("C1")
	parent, lost := threadWithReply(base, -time.Hour, 2*time.Minute)
	kept := histMsg(tsAt(base, 60*time.Minute), "U2", "reply above the floor")
	kept.ThreadTS = parent.TS
	kept.Raw, _ = json.Marshal(map[string]any{"type": "message", "ts": kept.TS, "user": "U2", "text": kept.Text, "thread_ts": parent.TS})
	parent.LatestReply = kept.TS
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{lost, kept}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || got[0].Oldest != watermarkBefore {
		t.Fatalf("pending threads = %+v, want the failing thread from %s", got, watermarkBefore)
	}
	if got := readDeadLetter(t, cfg); len(got) != 0 {
		t.Fatalf("dead letter records before the floor passed the cursor = %d, want none", len(got))
	}
	// 75 minutes on, the floor (now - 1 h) has passed the cursor.
	clock.advance(63 * time.Minute)
	l.tick(context.Background())
	recs := readDeadLetter(t, cfg)
	if len(recs) != 1 {
		t.Fatalf("dead letter records = %d, want one for the thread (log: %s)", len(recs), read())
	}
	if r := recs[0]; r.Inbound.ReplyToMessageID != parent.TS || r.Inbound.ProviderMessageID != parent.TS || !strings.Contains(r.Reason, "above "+watermarkBefore+" fell below the backfill window's floor") ||
		r.Inbound.Conversation.ConversationID != "C1" || r.Inbound.Conversation.ScopeID != "citadel" {
		t.Fatalf("dead letter record = %+v, want the parent %s, the cursor %s, the channel and the city", r, parent.TS, watermarkBefore)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 1 || !got[0].FloorCrossed {
		t.Fatalf("pending threads after the floor passed = %+v, want the thread still pending and marked", got)
	}
	awaitDeadLetterAlert(t, alerts)
	// The next probe: the floor rose again, no second record.
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	if got := readDeadLetter(t, cfg); len(got) != 1 {
		t.Fatalf("dead letter records after another probe = %d, want still one", len(got))
	}
	// The read recovers: the reply above the floor is delivered, the lost
	// one below it is not, and the thread is drained.
	delete(hist.errs, "C1|"+parent.TS)
	clock.advance(3 * time.Minute)
	l.tick(context.Background())
	envs := rec.all()
	if len(envs) != 1 {
		t.Fatalf("delivered %d after the read recovered, want the one reply above the floor", len(envs))
	}
	if got := deliveredTexts(envs); got["reply above the floor"] != 1 {
		t.Fatalf("delivered = %v, want the reply above the floor", got)
	}
	if got := l.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the drained read = %+v, want none", got)
	}
}

// 3q: a pending thread retired after its parent left the thread scan window
// with its reads still failing goes to the dead letter with the parent, the
// cursor, the channel and a reason that says Slack does not redeliver —
// logged and posted, never "left to a live redelivery".
func TestLivenessARetiredPendingThreadGoesToTheDeadLetter(t *testing.T) {
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

	watermarkBefore := l.watermarkOf("C1")
	parent, reply := threadWithReply(base, -time.Hour, 2*time.Minute)
	hist.byChan["C1"] = []slackHistoryMessage{parent}
	hist.threads["C1|"+parent.TS] = []slackHistoryMessage{reply}
	hist.errs["C1|"+parent.TS] = fmt.Errorf("conversations.replies not ok: ratelimited")
	clock.advance(12 * time.Minute)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 1 {
		t.Fatalf("pending threads = %+v, want the failing thread", got)
	}
	// A day on, the parent has left the scan window: retired.
	clock.advance(25 * time.Hour)
	l.tick(context.Background())
	if got := l.pendingThreadsOf("C1"); len(got) != 0 {
		t.Fatalf("pending threads after the scan window = %+v, want none", got)
	}
	recs := readDeadLetter(t, cfg)
	if len(recs) != 1 {
		t.Fatalf("dead letter records = %d, want one for the retired thread (log: %s)", len(recs), read())
	}
	if r := recs[0]; r.Inbound.ReplyToMessageID != parent.TS || !strings.Contains(r.Reason, "retired") || !strings.Contains(r.Reason, "above "+watermarkBefore) ||
		!strings.Contains(r.Reason, "Slack does not redeliver") || r.Inbound.Conversation.ConversationID != "C1" {
		t.Fatalf("dead letter record = %+v, want the retired thread with its cursor %s and the reason", r, watermarkBefore)
	}
	logs := read()
	if strings.Contains(logs, "live redelivery") {
		t.Errorf("the retirement still claims a live redelivery: %s", logs)
	}
	if !strings.Contains(logs, "going to the inbound dead letter") || !strings.Contains(logs, "dead-lettered to ") {
		t.Errorf("the retirement is not logged as dead-lettered: %s", logs)
	}
	awaitDeadLetterAlert(t, alerts)
}
