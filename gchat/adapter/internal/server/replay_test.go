package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
)

// Exercise the public resolve endpoint and the real processor together: each
// replay has its own answer and mail allowance, including after an end mail.
func TestReplayReportsItsPassAndStartsANewMailLife(t *testing.T) {
	for _, answer := range []string{"delivered", "pending", "draft_deferred", "no_route", "outage", "fragment"} {
		t.Run(answer, func(t *testing.T) {
			f := setup(t)
			now := time.Now().UTC()
			var mails []inbound.Raise
			p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu,
				LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC,
				RetryDelay: time.Nanosecond, Now: func() time.Time { return now },
				Raise: func(_ context.Context, r inbound.Raise) error { mails = append(mails, r); return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetPoll(func(ctx context.Context) error { now = time.Now().UTC(); return p.Pass(ctx) })
			id := "spaces/room/messages/replay"
			m := wire.InboundMessage{ProviderMessageID: id, Conversation: conversation("spaces/room"),
				Actor: wire.Actor{ID: "users/222"}, Text: "private replay text", ReceivedAt: now}
			encoded, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			old := now.Add(-25 * time.Hour)
			c := &ledger.ChangeState{ID: id, Space: "spaces/room", Message: id, Status: "unresolved",
				Event: string(encoded), Created: old, Raised: old.Add(time.Minute), RaisedEnd: old.Add(time.Hour), Fragment: true}
			f.l.Changes[id], f.l.Unresolved[id] = c, c
			f.gc.Configure(func(g *fakegc.Server) {
				g.NextDelivery = answer
				if answer == "outage" {
					g.CityNotRunning = true
				}
				if answer == "fragment" {
					for range 4 {
						g.Script = append(g.Script, wire.Delivery{Status: "partial"})
					}
				}
				if answer == "draft_deferred" {
					g.NextDelivery = "failed"
					for range 30 {
						g.Script = append(g.Script, wire.Delivery{Status: "failed", Members: []wire.DeliveryMember{{Status: "failed", Error: "preserved draft deferred delivery"}}})
					}
				}
			})
			code, raw, _ := f.call(t, "/resolve", map[string]string{"id": id, "action": "replay"})
			response := decode[map[string]string](t, raw)
			wantStatus, wantReason := "waiting", "receipt"
			if answer == "delivered" {
				wantStatus, wantReason = "landed", "delivered"
			} else if answer == "draft_deferred" {
				wantReason = "input box"
			} else if answer == "no_route" {
				wantStatus, wantReason = "not_routed", "no route"
			} else if answer == "outage" {
				wantReason = "temporarily unavailable"
			} else if answer == "fragment" {
				wantReason = "clear the fragment first"
			}
			if code != 200 || response["status"] != wantStatus || !strings.Contains(response["reason"], wantReason) {
				t.Errorf("resolve must report %s and %q: %d %s", wantStatus, wantReason, code, raw)
			}
			if strings.Contains(string(raw), m.Text) || len(mails) != 0 {
				t.Fatalf("resolve leaked text or sent mail: %s, %d mails", raw, len(mails))
			}
			if answer == "outage" {
				if response["pass_error"] == "" {
					t.Error("failed pass must be visible")
				}
				return
			}
			if answer == "fragment" {
				return
			}
			f.mu.Lock()
			replayed := *f.l.Changes[id]
			f.mu.Unlock()
			start := replayed.Created
			if replayed.Status == "pending" && replayed.Posted.After(start) {
				start = replayed.Posted
			}
			poll := func() {
				t.Helper()
				if err := p.Poll(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			now = start.Add(inbound.DefaultStuckAfter - time.Nanosecond)
			poll()
			if len(mails) != 0 {
				t.Fatalf("mail before the replay's threshold: %d", len(mails))
			}
			now = start.Add(inbound.DefaultStuckAfter)
			poll()
			poll()
			wantMails := 1
			if answer == "delivered" || answer == "no_route" {
				wantMails = 0
			}
			if len(mails) != wantMails {
				t.Errorf("replay waiting mails = %d, want %d", len(mails), wantMails)
			}
			if answer == "delivered" || answer == "no_route" {
				return
			}
			if answer == "pending" {
				f.gc.SetReceipt(replayed.ReceiptID, wire.ReceiptResponse{State: "unknown"})
			} else {
				f.gc.Configure(func(g *fakegc.Server) { g.Script = nil; g.NextDelivery = "partial" })
				now = now.Add(time.Minute)
			}
			poll()
			poll()
			if len(mails) != 2 || !mails[1].Ended {
				t.Fatalf("replay must get one waiting and one end mail: %+v", mails)
			}
			if answer == "draft_deferred" && !strings.Contains(mails[1].Body, "clear the fragment first") {
				t.Errorf("end mail must say to clear the fragment first: %s", mails[1].Body)
			}
		})
	}
}
