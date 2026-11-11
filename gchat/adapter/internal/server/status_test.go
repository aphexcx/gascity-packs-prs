package server

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
)

func TestStatusDeliveryDetailsUseStatusNotReason(t *testing.T) {
	f := setup(t)
	f.mu.Lock()
	f.l.Changes["pending-id"] = &ledger.ChangeState{ID: "pending-id", Status: "pending", Reason: "receipt lookup failed beyond deadline; retry pending", ReceiptID: "receipt-example", Event: "private message body"}
	f.l.Changes["retry-id"] = &ledger.ChangeState{ID: "retry-id", Status: "intent", Reason: "retry"}
	u := &ledger.ChangeState{ID: "unresolved-id", Status: "unresolved", Reason: "receipt pending beyond deadline"}
	f.l.Changes[u.ID], f.l.Unresolved[u.ID] = u, u
	f.l.Changes["retained-id"] = &ledger.ChangeState{ID: "retained-id", Status: "committed", Reason: "parent unavailable: permanent (HTTP 404)"}
	f.l.Counters["unavailable:spaces/room"] = 1
	f.mu.Unlock()
	code, raw, _ := f.call(t, "/status", nil)
	type statusView struct {
		Pending    int `json:"pending"`
		Unresolved int `json:"unresolved"`
		Deliveries []struct {
			ID, Status, Reason string
			ReceiptID          string `json:"receipt_id"`
		} `json:"deliveries"`
	}
	status := decode[statusView](t, raw)
	if code != 200 || status.Pending != 2 || status.Unresolved != 1 || len(status.Deliveries) != 3 {
		t.Fatalf("status details: %s", raw)
	}
	if status.Deliveries[0].ID != "pending-id" || status.Deliveries[0].Status != "pending" || status.Deliveries[0].ReceiptID != "receipt-example" || status.Deliveries[2].ID != "unresolved-id" {
		t.Fatalf("delivery order/status: %s", raw)
	}
	if bytes.Contains(raw, []byte("private message body")) || bytes.Contains(raw, []byte("retained-id")) {
		t.Fatalf("status exposes message content or offers a committed delivery to resolve: %s", raw)
	}
}

func TestStatusDeliveryRowsShowAgeCauseAndStuck(t *testing.T) {
	f := setup(t)
	now := time.Now().UTC()
	next := now.Add(45 * time.Second)
	raised := now.Add(-time.Minute)
	f.mu.Lock()
	f.l.Changes["waiting-id"] = &ledger.ChangeState{ID: "waiting-id", Status: "intent", Reason: "retry", Created: now.Add(-3 * time.Minute), Attempts: 7,
		NextAttempt: next, Posted: now.Add(-15 * time.Second), Cause: "draft_deferred", Raised: raised, Event: "private message body"}
	f.l.Changes["pending-id"] = &ledger.ChangeState{ID: "pending-id", Status: "pending", ReceiptID: "receipt-example", Created: now.Add(-5 * time.Minute), Attempts: 1}
	f.l.Changes["fresh-id"] = &ledger.ChangeState{ID: "fresh-id", Status: "intent", Created: now.Add(-10 * time.Second), Cause: "behind_earlier"}
	f.l.Changes["fresh-post-id"] = &ledger.ChangeState{ID: "fresh-post-id", Status: "pending", Created: now.Add(-5 * time.Minute), Posted: now.Add(-10 * time.Second)}
	u := &ledger.ChangeState{ID: "unresolved-id", Status: "unresolved", Reason: "not delivered within the retry bound", Created: now.Add(-25 * time.Hour), Attempts: 1400, Cause: "draft_deferred", Raised: raised}
	f.l.Changes[u.ID], f.l.Unresolved[u.ID] = u, u
	u.RaisedEnd, u.Fragment = raised, true
	old := &ledger.ChangeState{ID: "old-id", Status: "unresolved", Reason: "delivery retries exhausted", Attempts: 4}
	f.l.Changes[old.ID], f.l.Unresolved[old.ID] = old, old
	f.mu.Unlock()
	code, raw, _ := f.call(t, "/status", nil)
	if code != 200 {
		t.Fatalf("status: %d %s", code, raw)
	}
	var status struct {
		Stuck      *int             `json:"stuck"`
		Deliveries []map[string]any `json:"deliveries"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if status.Stuck == nil || *status.Stuck != 2 {
		t.Fatalf("stuck counts intent and pending rows of two minutes or more: %s", raw)
	}
	rows := map[string]map[string]any{}
	for _, row := range status.Deliveries {
		rows[row["id"].(string)] = row
	}
	waiting := rows["waiting-id"]
	if waiting == nil || waiting["cause"] != "draft_deferred" || waiting["attempts"] != float64(7) || waiting["next_attempt"] != next.Format(time.RFC3339Nano) ||
		waiting["raised"] != raised.Format(time.RFC3339Nano) || waiting["created"] == nil {
		t.Fatalf("waiting row: %v", waiting)
	}
	if age, _ := waiting["age_seconds"].(float64); age < 180 || age > 240 {
		t.Fatalf("waiting age: %v", waiting)
	}
	if rows["fresh-id"]["cause"] != "behind_earlier" || rows["unresolved-id"]["cause"] != "draft_deferred" {
		t.Fatalf("causes: %s", raw)
	}
	if rows["unresolved-id"]["raised_end"] != raised.Format(time.RFC3339Nano) || rows["unresolved-id"]["fragment"] != true {
		t.Fatalf("end mail and fragment marks: %v", rows["unresolved-id"])
	}
	for _, key := range []string{"cause", "next_attempt", "raised", "raised_end", "fragment", "created", "age_seconds"} {
		if _, ok := rows["old-id"][key]; ok {
			t.Fatalf("empty %s not omitted on an old row: %v", key, rows["old-id"])
		}
	}
	for _, key := range []string{"cause", "next_attempt", "raised"} {
		if _, ok := rows["pending-id"][key]; ok {
			t.Fatalf("empty %s not omitted: %v", key, rows["pending-id"])
		}
	}
	if bytes.Contains(raw, []byte("private message body")) {
		t.Fatalf("status exposes message content: %s", raw)
	}
}

func TestReplayClearsRetryStateAndMailMarks(t *testing.T) {
	f := setup(t)
	f.s.SetPoll(func(context.Context) error { return nil })
	id := "spaces/room/messages/replay"
	m := wire.InboundMessage{ProviderMessageID: id, Conversation: conversation("spaces/room"), Actor: wire.Actor{ID: "users/222", DisplayName: "Example"}, Text: "hello", ReceivedAt: time.Now()}
	b, _ := json.Marshal(m)
	raised := time.Now().UTC().Add(-time.Hour)
	c := &ledger.ChangeState{ID: id, Space: "spaces/room", Message: id, Status: "unresolved", Reason: "not delivered within the retry bound", Event: string(b), Attempts: 1400,
		Created: time.Now().Add(-25 * time.Hour), NextAttempt: time.Now().Add(time.Minute), Posted: time.Now().Add(-time.Minute), Cause: "draft_deferred", Fragment: true, Raised: raised, RaisedEnd: raised}
	f.mu.Lock()
	f.l.Changes[id] = c
	copy := *c
	f.l.Unresolved[id] = &copy
	f.mu.Unlock()
	if code, b, _ := f.call(t, "/resolve", map[string]string{"id": id, "action": "replay"}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	got := f.l.Changes[id]
	if got.Status != "intent" || got.Attempts != 0 || !got.NextAttempt.IsZero() || !got.Posted.IsZero() || got.Cause != "" || got.Fragment || !got.Raised.IsZero() || !got.RaisedEnd.IsZero() || time.Since(got.Created) > time.Minute {
		t.Fatalf("replay: %+v", got)
	}
}
