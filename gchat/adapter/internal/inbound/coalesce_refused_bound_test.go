package inbound

// A history bound that falls between the acts of a merged turn that gc
// refused (gp-e5w3). The bound retires only the acts recorded before it. An
// act recorded at or after it is kept and posted once, and it is posted
// without the text of the retired acts. The arrangement of the between rows
// is the Fable read's probe P9 on packs pull request 135. Two rows put the
// bound between a message's creation and an edit the window folded into it,
// one held alone and one inside a refused turn: the fold keeps the edit's
// text in the creation's record.

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// mergedHeader is the first line of a merged turn (merged in coalesce.go).
var mergedHeader = regexp.MustCompile(`^\[\d+ messages`)

type boundAct struct {
	id, text string
	at       time.Time // when the ledger recorded the act
	m        *api.Message
}

// refusedTurn holds the acts of one room inside an 8 s window. The window's
// end freezes them into one merged turn, and gc answers "failed" to the
// turn's four POSTs. Layouts:
//
//	two_messages: body-002, then body-003. The owner is the second act.
//	message_then_edit: body-002, then an edit of an older message (edit-007).
//	  The owner is the first act.
//	three_acts: body-002, body-003, then edit-007. The owner is the second.
//	three_messages: body-002, body-003, body-004. The owner is the third.
//	fold_then_message: body-002, then its edit rev-002 (folded into it), then
//	  body-003. The owner is the last act.
//	held_fold: body-002 and its folded edit rev-002, still held: no turn and
//	  no refusal.
func refusedTurn(t *testing.T, layout string, restart bool) (*windowFixture, []boundAct) {
	t.Helper()
	f := windowSetup(t, "8s")
	if restart {
		f.disk()
	}
	var acts []boundAct
	members := 0
	held := func(m *api.Message, text string) {
		t.Helper()
		c := f.l.Changes[m.Name]
		if !isHeld(c) {
			t.Fatalf("%s is not held: %+v", m.Name, c)
		}
		acts = append(acts, boundAct{m.Name, text, c.Created, m})
		members++
	}
	f.now = f.now.Add(time.Second)
	first := f.send(t, 2, "message.created", nil)
	held(first, "body-002")
	if layout == "fold_then_message" || layout == "held_fold" {
		f.now = f.now.Add(time.Second)
		edit := f.send(t, 2, "message.updated", func(m *api.Message) {
			m.Text, m.LastUpdateTime = "rev-002", f.now.Format(time.RFC3339Nano)
		})
		c := f.l.Changes[edit.Name+"#updated#"+edit.LastUpdateTime]
		if c == nil || c.Status != "committed" || c.Reason != "coalesced" || c.MemberOf != first.Name {
			t.Fatalf("the edit was not folded into its creation: %+v", c)
		}
		acts = append(acts, boundAct{c.ID, "rev-002", c.Created, edit})
		if layout == "held_fold" {
			return f, acts
		}
	}
	if layout != "message_then_edit" {
		f.now = f.now.Add(time.Second)
		held(f.send(t, 3, "message.created", nil), "body-003")
	}
	if layout == "three_messages" {
		f.now = f.now.Add(time.Second)
		held(f.send(t, 4, "message.created", nil), "body-004")
	}
	if layout == "message_then_edit" || layout == "three_acts" {
		f.now = f.now.Add(time.Second)
		held(f.send(t, 7, "message.updated", func(m *api.Message) {
			m.Text, m.LastUpdateTime = "edit-007", f.now.Format(time.RFC3339Nano)
		}), "edit-007")
	}
	f.gc.Script = []wire.Delivery{{Status: "failed"}, {Status: "failed"}, {Status: "failed"}, {Status: "failed"}}
	f.now = f.now.Add(9 * time.Second)
	_ = f.p.Poll(context.Background())
	owner := boundOwner(f.l)
	if owner == nil || len(owner.Members) != members || owner.Status != "intent" || owner.Reason != reasonRetry || owner.Attempts != firstPOSTs {
		t.Fatalf("the acts were not frozen into one refused turn: %+v", owner)
	}
	if restart {
		l, err := ledger.Load(f.opts.LedgerPath, true)
		if err != nil {
			t.Fatal(err)
		}
		f.l, f.opts.Ledger = l, l
		if f.p, err = New(f.opts); err != nil {
			t.Fatal(err)
		}
	}
	return f, acts
}

func boundOwner(l *ledger.Ledger) *ledger.ChangeState {
	for _, c := range l.Changes {
		if len(c.Members) > 0 {
			return c
		}
	}
	return nil
}

// installBound is what catch-up's first read does: a bound for the room, and
// RetireSpaceHistory, under the ledger lock.
func installBound(f *windowFixture, at time.Time) {
	f.opts.LedgerMu.Lock()
	defer f.opts.LedgerMu.Unlock()
	f.l.HistoryFloor = at
	RetireSpaceHistory(f.l, f.opts.Config, space, nil)
}

// boundRetired finds the record a retirement leaves for an act: under the
// act's own key for a message's creation, under a history key otherwise.
func boundRetired(l *ledger.Ledger, id string) *ledger.ChangeState {
	for key, c := range l.Changes {
		if (key == id || strings.HasPrefix(key, id+"#history#")) && c.Status == "committed" && c.Reason == "local" && c.Cause == "before_history_floor" {
			return c
		}
	}
	return nil
}

func TestHistoryBoundAcrossARefusedMergedTurn(t *testing.T) {
	type row struct {
		name    string
		layout  string
		restart bool
		// The bound, from the recorded times of the acts.
		bound func(acts []boundAct) time.Time
		// before runs after the refusal and before the bound.
		before  func(t *testing.T, f *windowFixture, acts []boundAct)
		retired []int // indexes of the acts the bound retires
		// withdrawn: acts their author deleted. Their record is not judged
		// here; their text must never be posted.
		withdrawn []int
		posted    []int // indexes of the acts posted, once each, after the bound
		// texts: other texts, with the times each is posted after the bound.
		texts map[string]int
		turns int // turns posted after the bound
		// whole: the kept acts are still one merged turn.
		whole bool
		after func(t *testing.T, f *windowFixture, acts []boundAct)
	}
	at := func(i int, d time.Duration) func([]boundAct) time.Time {
		return func(acts []boundAct) time.Time { return acts[i].at.Add(d) }
	}
	rows := []row{
		// The bound is before the first act. Nothing is retired, and the
		// turn is posted whole.
		{name: "before_the_first_act", layout: "two_messages", bound: at(0, -time.Second), posted: []int{0, 1}, turns: 1, whole: true},
		// The bound is at the first act's own time. That act is not before it.
		{name: "at_the_first_act", layout: "two_messages", bound: at(0, 0), posted: []int{0, 1}, turns: 1, whole: true},
		// Between the acts: the first is retired and the second, the owner,
		// is posted alone. At main the owner, judged by the turn's time,
		// was retired as well and never posted.
		{name: "between_the_acts", layout: "two_messages", bound: at(0, 500*time.Millisecond), retired: []int{0}, posted: []int{1}, turns: 1},
		// Probe P9: the bound is at the second act's own time.
		{name: "at_the_second_act", layout: "two_messages", bound: at(1, 0), retired: []int{0}, posted: []int{1}, turns: 1},
		// The bound is after the last act. Every act is retired.
		{name: "after_the_last_act", layout: "two_messages", bound: at(1, time.Second), retired: []int{0, 1}},
		// The owner is the first act and is retired. The later act was its
		// member, and at main it went with its owner and was never posted.
		{name: "owner_before_the_bound", layout: "message_then_edit", bound: at(1, 0), retired: []int{0}, posted: []int{1}, turns: 1},
		// The adapter restarts between the refusal and the bound. The recorded
		// times survive the state file.
		{name: "restart_between_the_refusal_and_the_bound", layout: "two_messages", restart: true, bound: at(1, 0), retired: []int{0}, posted: []int{1}, turns: 1},
		// A state file written before this change has no own recorded time
		// for the turn's owner. The owner reads as recorded at the later of
		// the turn's time and its own act's time, so it is kept here.
		{name: "state_file_without_the_owners_time", layout: "three_acts", bound: at(1, 0), retired: []int{0}, posted: []int{1, 2}, turns: 2,
			before: func(t *testing.T, f *windowFixture, _ []boundAct) {
				// The owner as such a file reads: its record without the
				// member_created key.
				owner := boundOwner(f.l)
				raw, err := json.Marshal(owner)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]any
				if err = json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "member_created")
				if raw, err = json.Marshal(fields); err != nil {
					t.Fatal(err)
				}
				var old ledger.ChangeState
				if err = json.Unmarshal(raw, &old); err != nil {
					t.Fatal(err)
				}
				*owner = old
			}},
		// The owner was deleted after the refusal, so the refresh moved the
		// turn to the last act still in it. That act keeps its own time. The
		// deletion is an act after the bound: its notice is the second turn.
		// The deleted owner's withdrawn record still has the turn's time, as
		// at main, so the bound retires it too; it is never posted either way.
		{name: "refreshed_to_a_later_owner", layout: "three_messages", bound: at(1, 0), retired: []int{0}, withdrawn: []int{2}, posted: []int{1}, turns: 2,
			before: func(t *testing.T, f *windowFixture, acts []boundAct) {
				f.now = f.now.Add(time.Second)
				f.handle(t, event("deleted", "message.deleted", acts[2].m))
				owner := boundOwner(f.l)
				if owner == nil || owner.ID != acts[1].id || len(owner.Members) != 2 || !owner.Created.Equal(acts[0].at) {
					t.Fatalf("the refresh did not move the turn to %s: %+v", acts[1].id, owner)
				}
			}},
		// The first act was deleted after the refusal, so the refresh took it
		// out of the turn, and the turn kept the deleted act's time. Every act
		// left in it is after the bound. The deletion's notice is the third
		// turn.
		{name: "first_act_deleted_after_the_refusal", layout: "three_messages", bound: at(1, 0), withdrawn: []int{0}, posted: []int{1, 2}, turns: 3,
			before: func(t *testing.T, f *windowFixture, acts []boundAct) {
				f.now = f.now.Add(time.Second)
				f.handle(t, event("deleted", "message.deleted", acts[0].m))
				owner := boundOwner(f.l)
				if owner == nil || owner.ID != acts[2].id || len(owner.Members) != 2 || !owner.Created.Equal(acts[0].at) {
					t.Fatalf("the refresh did not take %s out of the turn: %+v", acts[0].id, owner)
				}
			}},
		// The first act was edited after the refusal, so the refresh rebuilt
		// the turn under the same owner. The owner keeps its own time. The
		// edit is an act after the bound and is posted on its own.
		{name: "edit_of_the_first_act_after_the_refusal", layout: "three_messages", bound: at(1, 0), retired: []int{0}, posted: []int{1, 2}, texts: map[string]int{"rev-002": 1}, turns: 3,
			before: func(t *testing.T, f *windowFixture, acts []boundAct) {
				f.now = f.now.Add(time.Second)
				m := *acts[0].m
				m.Text, m.LastUpdateTime = "rev-002", f.now.Format(time.RFC3339Nano)
				f.handle(t, event("edited", "message.updated", &m))
				owner := boundOwner(f.l)
				if owner == nil || owner.ID != acts[2].id || len(owner.Members) != 3 || !strings.Contains(owner.Event, "rev-002") {
					t.Fatalf("the refresh did not rebuild the turn under %s: %+v", acts[2].id, owner)
				}
			}},
		// An edit folded into its message's held creation, the bound between
		// them. The creation is retired; the edit is posted on its own.
		{name: "edit_folded_into_a_held_creation", layout: "held_fold", bound: at(1, 0), retired: []int{0}, posted: []int{1}, turns: 1},
		// The same fold inside a refused turn.
		{name: "edit_folded_into_a_refused_turn", layout: "fold_then_message", bound: at(1, 0), retired: []int{0}, posted: []int{1, 2}, turns: 2},
		// A POST of the turn may have landed: its answer never came. The
		// turn is not split. It is retired as before, into an operator's
		// notice, and none of its text is posted or kept for a replay.
		{name: "post_may_have_landed", layout: "two_messages", bound: at(1, 0), retired: []int{0},
			before: func(t *testing.T, f *windowFixture, _ []boundAct) {
				boundOwner(f.l).Reason = ""
			},
			after: func(t *testing.T, f *windowFixture, acts []boundAct) {
				c := f.l.Changes[acts[1].id]
				if c == nil || c.Status != "unresolved" || c.Reason != "interrupted POST; delivery unknown" {
					t.Errorf("the owner whose POST may have landed is not an operator's notice: %+v", c)
					return
				}
				for _, a := range acts {
					if strings.Contains(c.Event, a.text) || strings.Contains(c.OriginalEvent(), a.text) {
						t.Errorf("the notice keeps %q for a replay", a.text)
					}
				}
			}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f, acts := refusedTurn(t, r.layout, r.restart)
			if r.before != nil {
				r.before(t, f, acts)
			}
			posted := len(f.gc.Inbound())
			installBound(f, r.bound(acts))
			retired, want, withdrawn := map[int]bool{}, map[int]bool{}, map[int]bool{}
			for _, i := range r.retired {
				retired[i] = true
			}
			for _, i := range r.withdrawn {
				withdrawn[i] = true
			}
			for _, i := range r.posted {
				want[i] = true
			}
			for i, a := range acts {
				c := boundRetired(f.l, a.id)
				if withdrawn[i] {
					continue
				}
				if retired[i] && c == nil {
					t.Errorf("%s, recorded %s, before the bound: not retired", a.id, a.at.Format("15:04:05.000"))
				}
				if !retired[i] && c != nil {
					t.Errorf("%s, recorded %s, at or after the bound: retired", a.id, a.at.Format("15:04:05.000"))
				}
				if c != nil && strings.Contains(c.Event, a.text) {
					t.Errorf("%s: the retired record keeps its text", a.id)
				}
			}
			for pass := 0; pass < 3; pass++ {
				f.now = f.now.Add(time.Minute)
				if err := f.p.Poll(context.Background()); err != nil {
					t.Fatalf("pass %d: %v", pass, err)
				}
			}
			turns := f.gc.Inbound()[posted:]
			var lines []string
			for _, w := range turns {
				lines = append(lines, fmt.Sprintf("%s: %q", w.ProviderMessageID, w.Text))
			}
			if len(turns) != r.turns {
				t.Errorf("turns after the bound=%d, want %d: %v", len(turns), r.turns, lines)
			}
			for i, a := range acts {
				n := 0
				for _, w := range turns {
					n += strings.Count(w.Text, a.text)
				}
				switch {
				case want[i] && n != 1:
					t.Errorf("%q was posted %d times, want once: %v", a.text, n, lines)
				case !want[i] && n != 0:
					t.Errorf("%q was posted %d times, want none: %v", a.text, n, lines)
				}
			}
			for text, times := range r.texts {
				n := 0
				for _, w := range turns {
					n += strings.Count(w.Text, text)
				}
				if n != times {
					t.Errorf("%q was posted %d times, want %d: %v", text, n, times, lines)
				}
			}
			for _, w := range turns {
				merged := mergedHeader.MatchString(w.Text)
				if merged != r.whole {
					t.Errorf("turn %s: merged=%v, want %v: %q", w.ProviderMessageID, merged, r.whole, w.Text)
				}
			}
			if r.after != nil {
				r.after(t, f, acts)
			}
		})
	}
}
