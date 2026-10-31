package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIngestionSettledFanoutSurvivesMidnightUntilFollowUpEnds(t *testing.T) {
	for _, ending := range []string{"concluded", "concluded_extmsg", "follow_up_ended", "orphan"} {
		t.Run(ending, func(t *testing.T) {
			at := time.Date(2026, 10, 30, 23, 55, 0, 0, time.UTC)
			now := at
			j := &inboundJournal{dir: t.TempDir(), retentionDays: 1, now: func() time.Time { return now }}
			cfg := config{inboundJournal: j}
			// Extmsg callbacks capture an envelope without StartedAt. Both
			// members' genuine input timestamps precede the later callback.
			msg := externalInboundMessage{Conversation: conversationRef{ConversationID: "C1"},
				ProviderMessageID: "100.2", ReplyToMessageID: "100.0", Text: "midnight fanout"}
			j.record(cfg, msg, deliveryReceipt{id: "fanout", status: "pending", members: []deliveryReceiptMember{
				{SessionID: "s1", Status: "delivered"},
				{SessionID: "s2", Status: "pending"},
			}})
			entry := ingestionTestEntry(t, ingestionTestRecord(msg.ProviderMessageID, at, msg.Text), msg.Text, "attachment")
			alerts := 0
			o := &ingestionObserver{cfg: cfg, now: func() time.Time { return now },
				fetch: func(context.Context, string) ([]json.RawMessage, error) {
					return []json.RawMessage{entry}, nil
				},
				notify: func(context.Context, inboundJournalRecord) error { alerts++; return nil }}
			now = at.Add(ingestionWindow + time.Second)
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			initialAlerts := alerts
			if alerts != 0 {
				t.Fatalf("wrong early verdict: alerts=%d", alerts)
			}
			oldPath := filepath.Join(j.dir, at.Format("20060102")+".jsonl")
			now = at.Add(5*time.Minute + time.Second)
			if err := j.append(inboundJournalRecord{Status: "unrelated"}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(oldPath); err != nil {
				t.Fatalf("member verdict released live receipt evidence at midnight: %v", err)
			}
			// Reconstruct from disk, as after a restart; no in-memory
			// verdict or acceptance time may be needed for correctness.
			j = &inboundJournal{dir: j.dir, retentionDays: 1, now: func() time.Time { return now }}
			cfg.inboundJournal, o.cfg.inboundJournal = j, j
			now = at.Add(6 * time.Minute)
			switch ending {
			case "orphan":
				now = at.Add(ingestionPendingTimingLimit)
				if err := j.append(inboundJournalRecord{Status: "unrelated"}); err != nil {
					t.Fatal(err)
				}
			case "follow_up_ended":
				j.record(cfg, msg, deliveryReceipt{id: "fanout", status: receiptFollowUpEnded}, "concluded")
			default:
				members := []deliveryReceiptMember{{SessionID: "s1", Status: "delivered"}}
				if ending == "concluded_extmsg" {
					members = append(members, deliveryReceiptMember{SessionID: "s2", Status: "delivered"})
				}
				j.record(cfg, msg, deliveryReceipt{id: "fanout", status: "delivered", members: members}, "concluded")
			}
			pending, err := o.pending()
			if err != nil {
				t.Fatal(err)
			}
			wantPending := 0
			if ending == "concluded_extmsg" {
				wantPending = 1
			}
			if len(pending) != wantPending {
				t.Fatalf("settled member re-enrolled after %s: pending=%v", ending, pending)
			}
			for _, row := range pending {
				if row.TargetSessions[0] != "s2" || !row.StartedAt.Equal(at) {
					t.Fatalf("extmsg callback lost original acceptance interval: %+v", row)
				}
			}
			o.fetch = func(_ context.Context, session string) ([]json.RawMessage, error) {
				if session != "s2" {
					t.Fatalf("settled member fetched again: %s", session)
				}
				return []json.RawMessage{entry}, nil
			}
			if err := o.step(context.Background()); err != nil {
				t.Fatal(err)
			}
			pending, err = o.pending()
			if err != nil || len(pending) != 0 || alerts != initialAlerts {
				t.Fatalf("false unseen or reopened member: pending=%v alerts=%d err=%v", pending, alerts, err)
			}
			if ending == "concluded_extmsg" || ending == "concluded" {
				settled := map[string]inboundJournalRecord{}
				for _, row := range ingestionJournalRows(t, j) {
					if row.Status == "ingested" || (row.Status == "delivered_unseen" && row.Alerted) {
						settled[row.TargetSessions[0]] = row
					}
				}
				if settled["s1"].Status != "ingested" || settled["s1"].Evidence == nil {
					t.Fatal("settled evidence lost while newer transport row remains")
				}
				if ending == "concluded_extmsg" && settled["s2"].Status != "ingested" {
					t.Fatal("earlier genuine extmsg input did not match after delayed callback")
				}
			}
			if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
				t.Fatalf("finished follow-up kept expired segment: %v", err)
			}
			now = now.Add(24 * time.Hour)
			if err := j.append(inboundJournalRecord{Status: "unrelated"}); err != nil {
				t.Fatal(err)
			}
			for _, row := range ingestionJournalRows(t, j) {
				if row.ReceiptID == "fanout" {
					t.Fatal("settled receipt retained beyond normal retention")
				}
			}
		})
	}
}
