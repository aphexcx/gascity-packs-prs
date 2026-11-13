package inbound

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
)

type withdrawalCase struct{ Name, Form, Prior, Act string }

func withdrawalCases() []withdrawalCase {
	var rows []withdrawalCase
	for _, form := range []string{"merged", "folded"} {
		for _, prior := range []string{"interrupted", "answer_lost", "redirect", "rejected", "exhausted", "retry_bound", "malformed", "lookup_deadline", "pending_deadline", "receipt_unknown", "held", "unconcluded", "no_receipt", "unknown_status"} {
			rows = append(rows, withdrawalCase{form + "_" + prior, form, prior, "member_deleted"})
		}
		rows = append(rows, withdrawalCase{form + "_pending", form, "pending", "member_deleted"})
	}
	rows = append(rows,
		withdrawalCase{"merged_owner_deleted", "merged", "interrupted", "owner_deleted"},
		withdrawalCase{"merged_member_edited", "merged", "interrupted", "member_edited"},
		withdrawalCase{"folded_edited", "folded", "interrupted", "member_edited"},
		withdrawalCase{"single", "single", "interrupted", "member_deleted"})
	return rows
}

// storedWithdrawal drives the delivery/receipt outcomes through the real
// processor and fakes. Two saved-state injections are explicit: a crash after
// the pre-POST save, and a previously malformed turn whose member was restored.
func storedWithdrawal(t *testing.T, row withdrawalCase) (*windowFixture, string, string, string, string) {
	t.Helper()
	f := windowSetup(t, "8s")
	f.disk()
	a := f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	b := a
	if row.Form == "merged" {
		b = f.send(t, 2, "message.created", nil)
	}
	if row.Form == "folded" {
		a.Text, a.LastUpdateTime = "folded-text", f.now.Format(time.RFC3339Nano)
		f.handle(t, event("fold", "message.updated", a))
	}
	f.gc.CityNotRunning = true
	f.now = f.now.Add(8 * time.Second)
	_ = f.p.Poll(context.Background())
	ownerID := b.Name
	c := f.l.Changes[ownerID]
	if c == nil || c.Status != "intent" {
		t.Fatal("no refused frozen turn")
	}
	f.gc.CityNotRunning = false
	f.now = f.now.Add(time.Second)
	wantReason := ""
	switch row.Prior {
	case "interrupted":
		c.Attempts, c.Reason = 1, ""
		if err := f.l.Save(f.opts.LedgerPath); err != nil {
			t.Fatal(err)
		}
		ownerReload(t, f)
		_ = f.p.Poll(context.Background())
		wantReason = "interrupted POST; delivery unknown"
	case "malformed":
		// A prior malformed-member failure may retain a valid frozen payload;
		// the member can be repaired without resolving the stopped delivery.
		markUnresolved(f.l, ownerID, c, "stored turn is malformed", f.now)
		if err := f.l.Save(f.opts.LedgerPath); err != nil {
			t.Fatal(err)
		}
		wantReason = "stored turn is malformed"
	case "retry_bound":
		f.now = f.now.Add(f.p.opts.RetryBound + time.Second)
		_ = f.p.Poll(context.Background())
		wantReason = reasonRetryBound
	case "exhausted":
		f.gc.Script = []wire.Delivery{{Status: "partial"}, {Status: "failed"}, {Status: "failed"}, {Status: "failed"}}
		_ = f.p.Poll(context.Background())
		wantReason = reasonExhausted
	case "unknown_status":
		f.gc.NextDelivery = "unrecognized"
		_ = f.p.Poll(context.Background())
		wantReason = "unrecognized delivery status"
	case "answer_lost", "redirect", "rejected", "no_receipt":
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch row.Prior {
			case "answer_lost":
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			case "redirect":
				w.WriteHeader(http.StatusSeeOther)
			case "rejected":
				w.WriteHeader(http.StatusForbidden)
			case "no_receipt":
				_ = json.NewEncoder(w).Encode(wire.InboundResponse{Delivery: wire.Delivery{Status: "pending"}})
			}
		}))
		f.p.opts.GC = gcclient.New(srv.URL, "examplecity")
		_ = f.p.Poll(context.Background())
		srv.Close()
		f.p.opts.GC = f.opts.GC
		wantReason = map[string]string{"answer_lost": "POST response unavailable; delivery unknown", "redirect": "gc answered HTTP 303; delivery unknown", "rejected": "gc rejected the POST: HTTP 403", "no_receipt": "pending delivery has no receipt"}[row.Prior]
	default:
		f.gc.NextDelivery = "pending"
		_ = f.p.Poll(context.Background())
		c = f.l.Changes[ownerID]
		if c.Status != "pending" || c.ReceiptID == "" {
			t.Fatal("receipt was not issued")
		}
		switch row.Prior {
		case "lookup_deadline":
			f.gc.CityNotRunning = true
			f.now = f.now.Add(f.p.opts.PendingDeadline + time.Second)
			_ = f.p.Poll(context.Background())
			_ = f.p.Poll(context.Background())
			f.gc.CityNotRunning = false
			wantReason = "receipt lookup failed beyond deadline"
		case "pending_deadline":
			f.now = f.now.Add(f.p.opts.PendingDeadline + time.Second)
			_ = f.p.Poll(context.Background())
			wantReason = "receipt pending beyond deadline"
		case "receipt_unknown":
			f.gc.EvictReceipt(c.ReceiptID)
			_ = f.p.Poll(context.Background())
			wantReason = "receipt unavailable or unknown"
		case "held", "unconcluded":
			f.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "pending", Terminal: row.Prior == "held"}})
			_ = f.p.Poll(context.Background())
			wantReason = reasonUnconcluded
			if row.Prior == "held" {
				wantReason = reasonHeld
			}
		case "pending":
			wantReason = c.Reason
		default:
			t.Fatal("unhandled prior state")
		}
	}
	ownerReload(t, f)
	c = f.l.Changes[ownerID]
	wantStatus := "unresolved"
	if row.Prior == "pending" {
		wantStatus = "pending"
	}
	if c.Status != wantStatus || c.Reason != wantReason {
		t.Fatalf("prior status=%s reason=%q, want %s %q", c.Status, c.Reason, wantStatus, wantReason)
	}
	wantCause := c.Cause
	target, kind := a, "message.deleted"
	if row.Act == "owner_deleted" {
		target = b
	}
	if row.Act == "member_edited" {
		kind = "message.updated"
	}
	withdrawn := target.Text
	f.now = f.now.Add(time.Second)
	target.Text, target.LastUpdateTime = "current-text", f.now.Format(time.RFC3339Nano)
	f.gc.CityNotRunning = true
	ev := event("withdraw-after-stop", kind, target)
	ev.Changes[0].PublishTime, ev.Changes[0].EventTime = f.now, f.now
	_, _ = f.p.Handle(context.Background(), ev)
	ownerReload(t, f)
	return f, ownerID, withdrawn, wantReason, wantCause
}

func TestWithdrawalReachesEveryStoredCopy(t *testing.T) {
	for _, row := range withdrawalCases() {
		t.Run(row.Name, func(t *testing.T) {
			f, id, withdrawn, reason, cause := storedWithdrawal(t, row)
			c := f.l.Changes[id]
			status := "unresolved"
			if row.Prior == "pending" {
				status = "pending"
			}
			if c.Status != status || c.Reason != reason || c.Cause != cause {
				t.Errorf("prior diagnostic changed: status=%s reason=%q cause=%q", c.Status, c.Reason, c.Cause)
			}
			if row.Form != "single" {
				for name, records := range map[string]map[string]*ledger.ChangeState{"changes": f.l.Changes, "unresolved": f.l.Unresolved} {
					for key, record := range records {
						if (record.Status == "intent" || record.Status == "pending" || record.Status == "unresolved") && strings.Contains(record.Event, withdrawn) {
							t.Errorf("%s/%s retains withdrawn text (%s)", name, key, record.Status)
						}
					}
					if copy := records[id]; copy != nil && copy.Event != "" {
						t.Errorf("%s owner remains replayable", name)
					}
				}
			}
			if destination := os.Getenv("GCHAT_WITHDRAWAL_FIXTURES"); destination != "" {
				if err := f.l.Save(filepath.Join(destination, row.Name+".json")); err != nil {
					t.Fatal(err)
				}
				meta := map[string]string{"name": row.Name, "form": row.Form, "prior": row.Prior, "owner": id, "withdrawn": withdrawn, "reason": reason, "cause": cause, "status": status}
				data, _ := json.Marshal(meta)
				if err := os.WriteFile(filepath.Join(destination, row.Name+".meta.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			start := len(f.gc.Inbound())
			if row.Prior == "pending" {
				f.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "failed"}})
			}
			f.gc.CityNotRunning = false
			for i := 0; i < 3; i++ {
				f.now = f.now.Add(10 * time.Second)
				_ = f.p.Poll(context.Background())
			}
			for _, post := range f.gc.Inbound()[start:] {
				if strings.Contains(post.Text, withdrawn) {
					t.Errorf("withdrawn text POSTed after withdrawal: %q", post.Text)
				}
			}
			t.Logf("ROW %s prior=%s reason=%q owner_event_empty=%v replay_copy_empty=%v later_posts=%d", row.Name, status, reason, c.Event == "", f.l.Unresolved[id] == nil || f.l.Unresolved[id].Event == "", len(f.gc.Inbound())-start)
		})
	}
}
