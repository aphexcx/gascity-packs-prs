package inbound

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestHeldActSavePrecedesAcknowledgment(t *testing.T) {
	f := windowSetup(t, "8s")
	f.p.opts.Save = func(*ledger.Ledger) error { return errors.New("disk unavailable") }
	out, err := f.p.Handle(context.Background(), event("unsaved", "message.created", message()))
	if err == nil || out != puller.Nack || len(f.l.Changes) != 0 || f.l.Transport.Seen("pubsub:unsaved") {
		t.Fatal("unsaved act acknowledged")
	}
	f.turns(t, 0)
}

func TestMentioningEditTakesHeldChatter(t *testing.T) {
	f := windowSetup(t, "8s")
	m := f.send(t, 1, "message.created", nil)
	f.send(t, 2, "message.created", nil)
	m.LastUpdateTime = f.now.Add(time.Second).Format(time.RFC3339)
	m.Text = "urgent edit"
	m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity}}}}
	handle(t, f.fixture, event("urgent-edit", "message.updated", m))
	w := f.turns(t, 1)[0]
	if w.ProviderMessageID != m.Name || !strings.Contains(w.Text, "urgent edit") || !strings.Contains(w.Text, "body-002") || strings.Contains(w.Text, "body-001") || strings.Contains(w.Text, "edited:") {
		t.Fatal(w.Text)
	}
}

func TestMergedRedeliveryWhilePendingOrDelivered(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivered", true: "pending"}[pending], func(t *testing.T) {
			f := windowSetup(t, "8s")
			if pending {
				f.gc.NextDelivery = "pending"
			}
			first := f.send(t, 1, "message.created", nil)
			last := f.send(t, 2, "message.created", nil)
			f.advance(t, 8*time.Second)
			f.turns(t, 1)
			for _, m := range []*api.Message{first, last} {
				handle(t, f.fixture, event("redelivery-"+m.Name, "message.created", m))
			}
			f.advance(t, 8*time.Second)
			f.turns(t, 1)
			if pending {
				for _, c := range f.l.Changes {
					if c.Status == "pending" {
						f.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "delivered"}})
					}
				}
				f.advance(t, time.Second)
				f.turns(t, 1)
			}
		})
	}
}

func TestLateAndCatchUpHeaderBudget(t *testing.T) {
	for _, catchUp := range []bool{false, true} {
		t.Run(map[bool]string{false: "late", true: "catchup"}[catchUp], func(t *testing.T) {
			f := windowSetup(t, "8s")
			var changes []puller.Change
			words, frames := 0, 0
			for _, m := range budgetMessages(false) {
				changes = append(changes, puller.Change{Kind: "message.created", Space: m.Space.Name, Message: m})
				words += len(m.Text)
			}
			if catchUp {
				f.now = f.now.Add(2 * time.Minute)
				if out, err := f.p.HandleChanges(context.Background(), changes); out != puller.Ack || err != nil {
					t.Fatal(out, err)
				}
			} else {
				for _, c := range changes {
					handle(t, f.fixture, &puller.Event{Changes: []puller.Change{c}})
				}
				f.advance(t, 2*time.Minute)
			}
			w := f.turns(t, 1)[0]
			for _, line := range strings.Split(w.Text, "\n") {
				if strings.HasPrefix(line, "[from: ") {
					frames += len(line)
				}
			}
			if len(w.Text)-words > 140+frames+2*len(changes) || !strings.Contains(w.Text, "2026-11-01 10:00") || !strings.Contains(w.Text, "UTC") {
				t.Fatal("late header lost time or exceeded budget")
			}
			if catchUp && !strings.HasPrefix(w.Text, "[catch-up:") {
				t.Fatal("catchup marker missing")
			}
		})
	}
}
