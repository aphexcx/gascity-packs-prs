package inbound

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// A first read retires history under the ledger lock while the processor
// works (catchup's first read: StartFirstRead, then RetireSpaceHistory). The
// retirement keeps a message's record under its own key, committed and
// redacted, and moves a reaction's or an edit's record to a "#history#" key.
// These tests put that retirement between a pass's snapshot and the save that
// follows it: the save must find the records gone or changed and leave them as
// the ledger now has them.

// retireHistoryInside runs the retirement once, under the ledger lock as the
// first read runs it, at the first clock read made inside frame and outside
// any save. Both gaps read the clock between their snapshot and their save.
func retireHistoryInside(f *windowFixture, frame string) *bool {
	fired := false
	f.p.opts.Now = func() time.Time {
		if !fired && onStack(frame) && !onStack(".(*Processor).update") && f.opts.LedgerMu.TryLock() {
			fired = true
			f.l.HistoryFloor = f.now.Add(time.Hour)
			RetireSpaceHistory(f.l, f.opts.Config, space, nil)
			f.opts.LedgerMu.Unlock()
		}
		return f.now
	}
	return &fired
}

func onStack(suffix string) bool {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, suffix) {
			return true
		}
		if !more {
			return false
		}
	}
}

// guardedPoll reports a panic in the pass as a value, with the line that
// panicked, so a row fails with its text instead of ending the test binary.
func guardedPoll(f *windowFixture) (err error, panicked any) {
	defer func() {
		if r := recover(); r != nil {
			panicked = fmt.Sprintf("%v (at %s)", r, panicSite())
		}
	}()
	return f.p.Poll(context.Background()), nil
}

// panicSite names the first two frames below the runtime's panic machinery:
// the line that panicked and its caller. It is called from guardedPoll's
// deferred function, while the panicking frames are still on the stack.
func panicSite() string {
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	var site []string
	for len(site) < 2 {
		frame, more := frames.Next()
		if !strings.HasPrefix(frame.Function, "runtime.") || len(site) > 0 {
			name := strings.TrimPrefix(frame.Function, "gchat-adapter/internal/")
			site = append(site, fmt.Sprintf("%s %s:%d", name, filepath.Base(frame.File), frame.Line))
		}
		if !more {
			break
		}
	}
	return strings.Join(site, ", called from ")
}

// retiredAs returns the retirement's record of an act: under the act's own
// key for a message, under a "#history#" key for a reaction or an edit.
func retiredAs(l *ledger.Ledger, id string) *ledger.ChangeState {
	if c := l.Changes[id]; c != nil {
		return c
	}
	for key, c := range l.Changes {
		if strings.HasPrefix(key, id+"#history#") {
			return c
		}
	}
	return nil
}

// checkRetiredUntouched fails unless every act is in the ledger exactly as the
// retirement left it, no record is still to be delivered, and no record holds
// a retired act's text.
func checkRetiredUntouched(t *testing.T, f *windowFixture, acts map[string]string, texts []string) {
	t.Helper()
	for id, kind := range acts {
		c := retiredAs(f.l, id)
		if c == nil {
			t.Errorf("%s: no record", id)
			continue
		}
		if c.Status != "committed" || c.Reason != "local" || c.Cause != "before_history_floor" || c.Kind != kind || c.MemberOf != "" || len(c.Members) != 0 || c.MemberEvent != "" {
			t.Errorf("%s: not the retirement's record: status=%q reason=%q cause=%q kind=%q member_of=%q members=%v", id, c.Status, c.Reason, c.Cause, c.Kind, c.MemberOf, c.Members)
		}
	}
	for id, c := range f.l.Changes {
		if c.Status == "intent" || c.Status == "pending" {
			t.Errorf("%s: still to be delivered (%s)", id, c.Status)
		}
		for _, text := range texts {
			if strings.Contains(c.Event, text) || strings.Contains(c.MemberEvent, text) {
				t.Errorf("%s: holds retired text %q", id, text)
			}
		}
	}
}

// The window flush's save: the owner it picked, or a member, is gone or is no
// longer held when the save runs. Without the guard the first and third rows
// panic (nil owner, nil member) and the second writes the merged pre-bound
// text back over the redacted history records.
func TestWindowFlushYieldsToAHistoryRetirementBeforeItsSave(t *testing.T) {
	for _, row := range []struct {
		name                string
		messages, reactions int
	}{
		{"reactions_owner_gone", 0, 2},
		{"messages_owner_committed", 2, 0},
		{"message_and_reaction_member_gone", 1, 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			f := windowSetup(t, "8s")
			parent := f.send(t, 1, "message.created", nil)
			f.advance(t, 9*time.Second)
			if c := f.l.Changes[parent.Name]; c == nil || c.Status != "committed" {
				t.Fatalf("parent not delivered: %+v", c)
			}
			// Reactions wake only on the identity's own messages.
			f.l.Messages[parent.Name].Mine = true
			acts := map[string]string{}
			var texts []string
			for i := range row.messages {
				f.now = f.now.Add(time.Second)
				m := f.send(t, 2+i, "message.created", nil)
				acts[m.Name], texts = "message.created", append(texts, m.Text)
			}
			for i := range row.reactions {
				f.now = f.now.Add(time.Second)
				name := fmt.Sprintf("%s/reactions/r%d", parent.Name, i+1)
				f.handle(t, &puller.Event{Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: name, User: &api.User{Name: "users/456"}, Emoji: &api.Emoji{Unicode: "👍"}}}}})
				acts[name+"#created"] = "reaction.created"
			}
			for id := range acts {
				if !isHeld(f.l.Changes[id]) {
					t.Fatalf("%s is not held: %+v", id, f.l.Changes[id])
				}
			}
			posted := len(f.gc.Inbound())
			f.now = f.now.Add(9 * time.Second)
			fired := retireHistoryInside(f, ".(*Processor).flushSpace")
			err, panicked := guardedPoll(f)
			if !*fired {
				t.Fatal("the retirement never ran between the flush's snapshot and its save")
			}
			if panicked != nil {
				t.Fatalf("the pass panicked: %v", panicked)
			}
			if err != nil {
				t.Errorf("the pass failed: %v", err)
			}
			checkRetiredUntouched(t, f, acts, texts)
			// The next pass finds nothing to deliver either.
			if err, panicked = guardedPoll(f); err != nil || panicked != nil {
				t.Errorf("next pass: err=%v panic=%v", err, panicked)
			}
			if got := len(f.gc.Inbound()); got != posted {
				t.Errorf("turns posted after the retirement = %d, want 0", got-posted)
			}
			checkRetiredUntouched(t, f, acts, texts)
		})
	}
}

// The refresh's save before a retry: a merged turn of two edits that gc
// refused four times, with a member's message recorded deleted by a writer
// that did not refresh the turn (the ledgers this refresh exists for,
// coalesce_retry.go's doc comment). The owner is an edit, so the retirement
// moves its record to a history key. Without the guard the save's
// refreshMerged(current, current.Changes[id]) dereferences nil.
func TestRetryRefreshYieldsToAHistoryRetirementBeforeItsSave(t *testing.T) {
	f := windowSetup(t, "8s")
	a := f.send(t, 1, "message.created", nil)
	f.now = f.now.Add(time.Second)
	b := f.send(t, 2, "message.created", nil)
	f.advance(t, 9*time.Second)
	if c := f.l.Changes[b.Name]; c == nil || c.Status != "committed" {
		t.Fatalf("messages not delivered: %+v", c)
	}
	var texts []string
	for i, m := range []*api.Message{a, b} {
		f.now = f.now.Add(time.Second)
		m.Text, m.LastUpdateTime = fmt.Sprintf("edited-%03d", i+1), f.now.Format(time.RFC3339Nano)
		f.handle(t, event("edit-"+m.Name, "message.updated", m))
		texts = append(texts, m.Text)
	}
	f.gc.Script = []wire.Delivery{{Status: "failed"}, {Status: "failed"}, {Status: "failed"}, {Status: "failed"}}
	f.now = f.now.Add(9 * time.Second)
	_ = f.p.Poll(context.Background())
	var owner *ledger.ChangeState
	for _, c := range f.l.Changes {
		if c.Status == "intent" && len(c.Members) == 2 {
			owner = c
		}
	}
	if owner == nil || owner.OriginalKind() != "message.updated" || owner.Reason != reasonRetry || owner.Attempts != firstPOSTs {
		t.Fatalf("no refused merged turn of two edits: %+v", owner)
	}
	acts := map[string]string{}
	for _, id := range owner.Members {
		acts[id] = "message.updated"
	}
	f.l.Messages[a.Name].Deleted = true
	posted := len(f.gc.Inbound())
	f.now = f.now.Add(time.Minute)
	fired := retireHistoryInside(f, ".(*Processor).refreshMerged")
	err, panicked := guardedPoll(f)
	if !*fired {
		t.Fatal("the retirement never ran between the refresh's snapshot and its save")
	}
	if panicked != nil {
		t.Fatalf("the pass panicked: %v", panicked)
	}
	if err != nil {
		t.Errorf("the pass failed: %v", err)
	}
	checkRetiredUntouched(t, f, acts, texts)
	if err, panicked = guardedPoll(f); err != nil || panicked != nil {
		t.Errorf("next pass: err=%v panic=%v", err, panicked)
	}
	if got := len(f.gc.Inbound()); got != posted {
		t.Errorf("turns posted after the retirement = %d, want 0", got-posted)
	}
	checkRetiredUntouched(t, f, acts, texts)
}
