package inbound

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
)

// A pending delivery's wait starts at the later of recording and the latest
// POST, whether the delay came from its space's head, gc downtime or a retry.
func TestPendingMailCountsFromTheLaterOfRecordingAndPOST(t *testing.T) {
	t.Run("five-message burst flows in 35 seconds", func(t *testing.T) {
		rf := retrySetup(t)
		rf.script(repeat(4, failedWith(draftDeferred))...)
		handle(t, rf.fixture, event("head", "message.created", message()))
		var ids []string
		for i := range 4 {
			m := secondMessage()
			m.Name = fmt.Sprintf("%s/messages/behind-%d", space, i)
			ids = append(ids, m.Name)
			rf.clock.Add(time.Second)
			handle(t, rf.fixture, event(fmt.Sprint(i), "message.created", m))
		}
		rf.clock.Set(retryStart.Add(3 * time.Minute))
		rf.script(wire.Delivery{Status: "delivered"}, wire.Delivery{Status: "pending"}, wire.Delivery{Status: "pending"}, wire.Delivery{Status: "pending"}, wire.Delivery{Status: "pending"})
		rf.poll(t)
		if got := len(rf.raises.list()); got != 0 {
			t.Errorf("fresh pending burst sent %d mails, want 0", got)
		}
		rf.clock.Add(35 * time.Second)
		for _, id := range ids {
			c := rf.change(t, id)
			if c.Status != "pending" {
				t.Fatalf("burst message %s: %s", id, c.Status)
			}
			rf.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "delivered"}})
		}
		rf.poll(t)
		for _, id := range append(ids, messageName) {
			if c := rf.change(t, id); c.Status != "committed" {
				t.Errorf("%s: %s", id, c.Status)
			}
		}
		if got := len(rf.raises.list()); got != 0 {
			t.Errorf("flowing burst sent %d mails, want 0", got)
		}
	})
	for _, scenario := range []string{"gc outage", "latest pending POST", "recorded later", "legacy no POST"} {
		t.Run(scenario, func(t *testing.T) {
			rf := retrySetup(t)
			if scenario == "gc outage" {
				rf.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = true })
				if out, err := rf.p.Handle(context.Background(), event("outage", "message.created", message())); out != puller.Nack || err == nil {
					t.Fatalf("outage: %v %v", out, err)
				}
				rf.clock.Add(3 * time.Hour)
				rf.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = false; g.NextDelivery = "pending" })
				rf.poll(t)
			} else {
				rf.script(wire.Delivery{Status: "pending"})
				handle(t, rf.fixture, event("pending", "message.created", message()))
				if scenario == "latest pending POST" {
					c := rf.change(t, messageName)
					rf.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: failedWith("")})
					rf.clock.Add(time.Minute)
					rf.script(wire.Delivery{Status: "pending"})
					rf.poll(t)
				} else {
					rf.opts.LedgerMu.Lock()
					if scenario == "recorded later" {
						rf.l.Changes[messageName].Posted = retryStart.Add(-time.Minute)
					} else {
						rf.l.Changes[messageName].Posted = time.Time{}
					}
					rf.opts.LedgerMu.Unlock()
				}
			}
			c := rf.change(t, messageName)
			start := c.Created
			if c.Posted.After(start) {
				start = c.Posted
			}
			if got := len(rf.raises.list()); got != 0 {
				t.Errorf("fresh pending sent %d mails", got)
			}
			rf.clock.Set(start.Add(DefaultStuckAfter - time.Nanosecond))
			rf.poll(t)
			if got := len(rf.raises.list()); got != 0 {
				t.Errorf("before threshold: %d mails", got)
			}
			rf.clock.Set(start.Add(DefaultStuckAfter))
			rf.poll(t)
			rf.poll(t)
			if got := len(rf.raises.list()); got != 1 {
				t.Errorf("truly stuck pending: %d mails, want 1", got)
			}
			rf.restart(t)
			rf.poll(t)
			if got := len(rf.raises.list()); got != 0 {
				t.Errorf("restart repeated waiting mail: %d", got)
			}
		})
	}
}
