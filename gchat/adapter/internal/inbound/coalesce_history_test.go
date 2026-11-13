package inbound

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/provenance"
	"gchat-adapter/internal/wire"
)

func TestWithdrawnRecordKeepsHistorySpeaker(t *testing.T) {
	f := windowSetup(t, "8s")
	f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	owner := f.send(t, 2, "message.created", nil)
	f.gc.CityNotRunning = true
	f.now = f.now.Add(8 * time.Second)
	_ = f.p.Poll(context.Background())
	original := f.l.Changes[owner.Name].OriginalEvent()
	f.handle(t, event("delete-owner", "message.deleted", owner))
	c := f.l.Unresolved[owner.Name]
	if c == nil || c.Event != "" || c.OriginalEvent() != original || c.OriginalKind() != "message.created" {
		t.Fatal("retirement must keep its per-message history and erase its replay payload")
	}
	var before, after wire.InboundMessage
	if err := json.Unmarshal([]byte(original), &before); err != nil {
		t.Fatal(err)
	}
	frame, ok := provenance.FrameOf(before.Text)
	if !ok {
		t.Fatal("fixture lacks a speaker frame")
	}
	f.l.HistoryFloor = f.now.Add(time.Hour)
	RetireSpaceHistory(f.l, f.opts.Config, space, nil)
	c = f.l.Changes[owner.Name]
	if c == nil || json.Unmarshal([]byte(c.Event), &after) != nil || after.Text != frame || after.Actor != before.Actor {
		t.Fatal("history migration lost the retired member's sender")
	}
}

func TestCoalescedKindReadersRetireOldHistory(t *testing.T) {
	for _, mode := range []string{"folded", "merged", "queued_edit"} {
		t.Run(mode, func(t *testing.T) {
			f := windowSetup(t, "8s")
			m := f.send(t, 1, "message.created", nil)
			f.now = f.now.Add(time.Second)
			m.Text, m.LastUpdateTime = "private-old-body", f.now.Format(time.RFC3339Nano)
			f.handle(t, event("edit", "message.updated", m))
			if mode != "folded" {
				f.send(t, 2, "message.created", nil)
			}
			f.gc.CityNotRunning = true
			f.now = f.now.Add(8 * time.Second)
			_ = f.p.Poll(context.Background())
			f.l.HistoryFloor = f.now.Add(time.Hour)
			if mode == "queued_edit" {
				// An update-before-create can occupy its message's natural ID.
				c := f.l.Changes[m.Name]
				c.Kind, c.MemberKind, c.Status = "coalesced", "message.updated", "intent"
				delete(f.l.Changes, m.Name+"#updated#"+m.LastUpdateTime)
				RetireQueuedHistory(f.l, puller.Change{Kind: "message.updated", Space: space, Message: m})
				if f.l.Changes[m.Name] != nil {
					t.Fatal("queued coalesced update was not retired")
				}
				return
			}
			RetireSpaceHistory(f.l, f.opts.Config, space, nil)
			for _, c := range f.l.Changes {
				if c.Status != "committed" || c.Reason != "local" || c.Cause != "before_history_floor" || len(c.Members) != 0 || c.MemberOf != "" {
					t.Errorf("old act not retired: %s status=%s reason=%s", c.ID, c.Status, c.Reason)
				}
				if strings.Contains(c.OriginalEvent(), "private-old-body") || strings.Contains(c.Event, "body-00") {
					t.Errorf("old text retained: %s", c.ID)
				}
			}
			f.gc.CityNotRunning = false
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.gc.Inbound()) != 0 || len(f.l.Unresolved) != 0 {
				t.Fatal("retired history became a turn or recovery")
			}
		})
	}
}

func TestWithdrawalSaveSurvivesRestart(t *testing.T) {
	for _, target := range []int{1, 2} {
		t.Run(map[int]string{1: "member", 2: "owner"}[target], func(t *testing.T) {
			f := windowSetup(t, "8s")
			f.disk()
			a := f.send(t, 1, "message.created", nil)
			f.now = f.now.Add(time.Second)
			b := f.send(t, 2, "message.created", nil)
			if target == 2 {
				a, b = b, a
			}
			f.gc.CityNotRunning = true
			f.now = f.now.Add(8 * time.Second)
			_ = f.p.Poll(context.Background())
			saves := 0
			f.p.opts.Save = func(l *ledger.Ledger) error {
				if l.Messages[a.Name].Deleted {
					saves++
					for _, c := range l.Changes {
						if c.Status == "intent" && len(c.Members) > 0 && strings.Contains(c.Event, a.Text) {
							t.Error("withdrawing act saved with a stale intent")
						}
					}
				}
				return l.Save(f.opts.LedgerPath)
			}
			f.now = f.now.Add(time.Second)
			f.handle(t, event("deleted", "message.deleted", a))
			if saves == 0 {
				t.Fatal("withdrawal not saved")
			}
			l, err := ledger.Load(f.opts.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			f.l, f.opts.Ledger = l, l
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			f.gc.CityNotRunning = false
			f.advance(t, 10*time.Second)
			text := joinTurns(f.turns(t, 2))
			if strings.Contains(text, a.Text) || strings.Count(text, b.Text) != 1 {
				t.Fatal("restart delivered withdrawn or duplicate text")
			}
		})
	}
}
