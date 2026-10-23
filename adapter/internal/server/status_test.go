package server

import (
	"bytes"
	"testing"

	"gchat-adapter/internal/ledger"
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
