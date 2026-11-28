package inbound

import (
	"context"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// unansweredMergedTurn freezes a two-member room turn, then saves the state a
// crash leaves after the pre-POST save: one attempt and no answer. A fresh
// processor loads it, as after a restart. The second message owns the turn.
func unansweredMergedTurn(t *testing.T) (*windowFixture, *api.Message, *api.Message) {
	t.Helper()
	f := windowSetup(t, "8s")
	f.disk()
	a := f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	b := f.send(t, 2, "message.created", nil)
	f.gc.CityNotRunning = true
	f.now = f.now.Add(8 * time.Second)
	_ = f.p.Poll(context.Background())
	f.gc.CityNotRunning = false
	owner := f.l.Changes[b.Name]
	if owner == nil || owner.Status != "intent" || len(owner.Members) != 2 || f.l.Changes[a.Name].MemberOf != b.Name {
		t.Fatal("no frozen two-member turn")
	}
	owner.Attempts, owner.Reason, owner.Cause, owner.Posted = 1, "", "", f.now
	if err := f.l.Save(f.opts.LedgerPath); err != nil {
		t.Fatal(err)
	}
	ownerReload(t, f)
	f.mails = nil
	return f, a, b
}

func endNotices(f *windowFixture, id string) int {
	n := 0
	for _, r := range f.mails {
		if r.ID == id && r.Ended {
			n++
		}
	}
	return n
}

func turnTextPOSTs(f *windowFixture) int {
	n := 0
	for _, w := range f.gc.Inbound() {
		if strings.Contains(w.Text, "body-00") {
			n++
		}
	}
	return n
}

// A deletion or an edit that reaches the adapter before its first recovery
// pass must not turn a merged turn's unanswered POST into a silent, re-keyed
// or re-rendered end.
func TestUnansweredMergedTurnIsClassifiedBeforeAWithdrawal(t *testing.T) {
	for _, row := range []string{"member_deleted", "member_edited", "owner_deleted", "all_deleted"} {
		t.Run(row, func(t *testing.T) {
			f, a, b := unansweredMergedTurn(t)
			posted := f.l.Changes[b.Name].Event
			f.now = f.now.Add(time.Second)
			if row == "member_edited" {
				a.Text, a.LastUpdateTime = "current-text", f.now.Format(time.RFC3339Nano)
				f.handle(t, event("edited-a", "message.updated", a))
				if got := f.l.Changes[b.Name]; got == nil || got.Event != posted {
					t.Errorf("the edit's own save re-rendered the attempted turn")
				}
			}
			if row == "member_deleted" || row == "all_deleted" {
				f.handle(t, event("deleted-a", "message.deleted", a))
			}
			if row == "owner_deleted" || row == "all_deleted" {
				f.handle(t, event("deleted-b", "message.deleted", b))
			}
			_ = f.p.Poll(context.Background())
			f.now = f.now.Add(time.Hour)
			_ = f.p.Poll(context.Background())
			owner := f.l.Changes[b.Name]
			if owner == nil || owner.Status != "unresolved" || owner.Cause != causeUnknown || owner.Reason != "interrupted POST; delivery unknown" || owner.Attempts != 1 {
				t.Errorf("owner ended %+v", owner)
			}
			if got := endNotices(f, b.Name); got != 1 {
				t.Errorf("end notices for the attempted turn = %d, want 1 (all mails: %d)", got, len(f.mails))
			}
			if member := f.l.Changes[a.Name]; member == nil || member.Status == "intent" || member.Status == "pending" || f.l.Delivery(member) != owner {
				t.Errorf("member left its attempted turn: %+v", member)
			}
			if got := turnTextPOSTs(f); got != 0 {
				t.Errorf("the turn's text was posted %d more times", got)
			}
			for id, c := range f.l.Changes {
				if c.Status == "unresolved" && c.Reason == reasonWithdrawn && c.Attempts > 0 {
					t.Errorf("%s: an attempt with no answer ended withdrawn", id)
				}
			}
		})
	}
}

// The control: a turn that gc answered "failed" four times has attempts and a
// definite answer. It is refreshed in the withdrawing act's own save and its
// retry posts the surviving text; only an attempt with no answer yields.
func TestRefusedMergedTurnStillRefreshesOnAWithdrawal(t *testing.T) {
	f := windowSetup(t, "8s")
	f.disk()
	a := f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	b := f.send(t, 2, "message.created", nil)
	f.gc.Script = []wire.Delivery{{Status: "failed"}, {Status: "failed"}, {Status: "failed"}, {Status: "failed"}}
	f.now = f.now.Add(8 * time.Second)
	_ = f.p.Poll(context.Background())
	owner := f.l.Changes[b.Name]
	if owner == nil || owner.Status != "intent" || owner.Reason != reasonRetry || owner.Attempts != firstPOSTs || !strings.Contains(owner.Event, a.Text) {
		t.Fatalf("no refused frozen turn: %+v", owner)
	}
	f.now = f.now.Add(time.Second)
	f.handle(t, event("deleted-a", "message.deleted", a))
	owner = f.l.Changes[b.Name]
	if owner == nil || owner.Status != "intent" || owner.Attempts != firstPOSTs || strings.Contains(owner.Event, a.Text) || !strings.Contains(owner.Event, b.Text) {
		t.Fatalf("the withdrawing save did not refresh a refused turn: %+v", owner)
	}
	f.advance(t, time.Minute)
	if owner = f.l.Changes[b.Name]; owner == nil || owner.Status != "committed" || owner.Reason != "delivered" {
		t.Fatalf("the refreshed turn was not retried: %+v", owner)
	}
}

// The history bound meets a merged turn whose POST has no answer: the turn
// keeps its unknown-delivery notice, no member is posted again, and no text
// from before the bound stays in the ledger.
func TestUnansweredMergedTurnKeepsItsNoticeAcrossHistoryRetirement(t *testing.T) {
	for _, row := range []string{"space", "queued_then_space"} {
		t.Run(row, func(t *testing.T) {
			f, a, b := unansweredMergedTurn(t)
			f.l.HistoryFloor = f.now.Add(time.Hour)
			if row == "queued_then_space" {
				RetireQueuedHistory(f.l, f.opts.Config, puller.Change{Kind: "message.created", Space: space, Message: b})
				if member := f.l.Changes[a.Name]; member == nil || member.Status == "intent" {
					t.Fatalf("a member of an attempted turn was released for its own POST: %+v", member)
				}
			}
			RetireSpaceHistory(f.l, f.opts.Config, space, nil)
			owner := f.l.Changes[b.Name]
			if owner == nil || f.l.Unresolved[b.Name] != owner || owner.Status != "unresolved" || owner.Cause != causeUnknown || owner.Reason != "interrupted POST; delivery unknown" {
				t.Fatalf("the attempted turn lost its notice record: %+v", owner)
			}
			for _, records := range []map[string]*ledger.ChangeState{f.l.Changes, f.l.Unresolved} {
				for id, c := range records {
					if strings.Contains(c.Event, "body-00") || strings.Contains(c.MemberEvent, "body-00") {
						t.Errorf("%s keeps text from before the bound (status %s)", id, c.Status)
					}
					if c.Status == "intent" || c.Status == "pending" {
						t.Errorf("%s is still a delivery: %s", id, c.Status)
					}
				}
			}
			if member := f.l.Changes[a.Name]; member == nil || member.Status != "committed" || member.Reason != "local" || member.Cause != "before_history_floor" || member.MemberOf != "" {
				t.Errorf("member was not retired to its own history row: %+v", member)
			}
			if err := f.l.Save(f.opts.LedgerPath); err != nil {
				t.Fatal(err)
			}
			ownerReload(t, f)
			_ = f.p.Poll(context.Background())
			f.now = f.now.Add(time.Hour)
			_ = f.p.Poll(context.Background())
			if got := endNotices(f, b.Name); got != 1 {
				t.Errorf("end notices = %d, want 1 (all mails: %d)", got, len(f.mails))
			}
			if got := len(f.gc.Inbound()); got != 0 {
				t.Errorf("retired history was posted %d times", got)
			}
		})
	}
}

// The control beside it: a merged turn with no attempt has no notice to keep.
// Its owner's queued row releases the members, and the space pass retires all.
func TestUnsentMergedTurnIsRetiredWholeByTheHistoryBound(t *testing.T) {
	f := windowSetup(t, "8s")
	a := f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	b := f.send(t, 2, "message.created", nil)
	f.gc.CityNotRunning = true
	f.now = f.now.Add(8 * time.Second)
	_ = f.p.Poll(context.Background())
	f.gc.CityNotRunning = false
	f.l.HistoryFloor = f.now.Add(time.Hour)
	RetireQueuedHistory(f.l, f.opts.Config, puller.Change{Kind: "message.created", Space: space, Message: b})
	if f.l.Changes[b.Name] != nil {
		t.Fatal("an unsent owner was kept")
	}
	if member := f.l.Changes[a.Name]; member == nil || member.Status != "intent" || member.MemberOf != "" {
		t.Fatalf("the unsent member was not restored: %+v", member)
	}
	RetireSpaceHistory(f.l, f.opts.Config, space, nil)
	if member := f.l.Changes[a.Name]; member == nil || member.Status != "committed" || member.Reason != "local" || len(f.l.Unresolved) != 0 {
		t.Fatalf("the restored member was not retired: %+v", member)
	}
}

// A deletion act can own a merged turn. Its retirement puts the message's
// deleted mark back by the act's own kind, whether or not its notice is kept.
func TestRetiredMergedDeletionPutsTheDeletedMarkBack(t *testing.T) {
	for _, attempts := range []int{0, 1} {
		t.Run(map[int]string{0: "unsent", 1: "unanswered"}[attempts], func(t *testing.T) {
			f := windowSetup(t, "8s")
			m := message()
			m.Name = space + "/messages/m009"
			at := f.now
			f.l.Messages[m.Name] = &ledger.MessageState{Deleted: true}
			f.l.Changes[m.Name+"#deleted"] = &ledger.ChangeState{ID: m.Name + "#deleted", Space: space, Message: m.Name, Kind: "coalesced", MemberKind: "message.deleted",
				Status: "intent", Created: at, Attempts: attempts, Posted: at, Members: []string{m.Name + "#deleted"},
				Event: `{"provider_message_id":"` + m.Name + `#deleted","text":"merged"}`, MemberEvent: `{"provider_message_id":"` + m.Name + `#deleted","text":"deleted"}`}
			f.l.HistoryFloor = f.now.Add(time.Hour)
			RetireSpaceHistory(f.l, f.opts.Config, space, nil)
			if f.l.Messages[m.Name].Deleted {
				t.Fatal("a retired deletion act left the message marked deleted")
			}
			kept := f.l.Unresolved[m.Name+"#deleted"]
			if (attempts == 1) != (kept != nil && kept.Cause == causeUnknown) {
				t.Fatalf("notice record kept=%v for attempts=%d", kept != nil, attempts)
			}
		})
	}
}

// One constructor serves both designs: the proof memory of an unanswered POST
// and the recovery of held acts are both ready before the first pass.
func TestNewKeepsTheUnansweredMemoryAndRecoversHeldActs(t *testing.T) {
	f := windowSetup(t, "8s")
	f.disk()
	m := f.send(t, 1, "message.created", nil)
	if !isHeld(f.l.Changes[m.Name]) {
		t.Fatal("no held act")
	}
	ownerReload(t, f)
	if f.p.unanswered == nil {
		t.Fatal("the unanswered-POST memory is nil: recording a proof would panic")
	}
	if !f.p.recoverHeld[m.Name] || f.p.windowChanged == nil || f.p.windowWait == nil {
		t.Fatal("a held act was not marked for recovery, or the window worker has no wait")
	}
}
