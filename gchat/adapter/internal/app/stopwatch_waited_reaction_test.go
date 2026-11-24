package app

// PR 109 round 3, Google Chat, a GUARD: a session reaction that waited for
// its turn keeps two times. never-silent judges the session's act by the
// moment it asked (the call dated back by its wait, round 2); the stopwatch
// records when the reaction landed. Google Chat already keeps the two apart
// (server.go's react); Slack's copy of the test is red before its fix.
//
// The stopwatch keeps the first reaction line per message, so the reaction
// that waits is the first the adapter makes on its message: a reaction of the
// session's identity is put on the fake directly, the session's removal finds
// it and its answer is held, and the session's eyes waits behind it. No
// acknowledgement runs (A is never admitted).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	api "google.golang.org/api/chat/v1"
)

// stopwatchReactionAt is the agent's reaction line for message in the stopwatch.
func (r *heldChat) stopwatchReactionAt(message string) time.Time {
	r.t.Helper()
	r.watch.Flush()
	var at []time.Time
	_ = filepath.Walk(r.state, func(path string, info os.FileInfo, err error) error {
		if err != nil || !strings.HasPrefix(info.Name(), "stopwatch-") {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			var e struct {
				Event, Message, Actor string
				At                    time.Time
			}
			if json.Unmarshal([]byte(line), &e) == nil && e.Event == "reaction" && e.Actor == "agent" && e.Message == message {
				at = append(at, e.At)
			}
		}
		return nil
	})
	if len(at) != 1 {
		r.t.Fatalf("stopwatch reaction lines for the message: %d, want 1", len(at))
	}
	return at[0]
}

// noticeActAt is the one session act never-silent holds.
func (r *heldChat) noticeActAt() time.Time {
	r.t.Helper()
	var state struct {
		Acts []struct {
			At time.Time `json:"at"`
		} `json:"acts"`
	}
	data, _ := os.ReadFile(r.opts.Path)
	_ = json.Unmarshal(data, &state)
	if len(state.Acts) != 1 {
		r.t.Fatalf("never-silent acts: %d, want 1", len(state.Acts))
	}
	return state.Acts[0].At
}

func TestReactVerbStopwatchKeepsTheRealTimeOfAWaitedReaction(t *testing.T) {
	r := newHeldChat(t)
	r.start("eyes")
	a := r.sessionFirstNext()
	r.google.Configure(func(g *fakegoogle.Server) {
		name := a + "/reactions/example-seed"
		g.Reactions[name] = &api.Reaction{Name: name, User: &api.User{Name: fakegoogle.UserID, Type: "HUMAN"}, Emoji: &api.Emoji{Unicode: round4Eyes}}
	})
	hold := r.arm("delete")
	var r0, s1 int
	r0Done := r.send(context.Background(), a, round4Eyes, true, &r0)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatal("the removal never reached Google")
	}
	asked := time.Now()
	s1Done := r.send(context.Background(), a, round4Eyes, false, &s1)
	if waitDone(s1Done, 900*time.Millisecond) {
		t.Fatalf("the session's eyes did not wait for the removal's turn: %d", s1)
	}
	released := time.Now()
	hold.free()
	if !waitDone(r0Done, 10*time.Second) || !waitDone(s1Done, 10*time.Second) {
		t.Fatalf("a call never returned; Google applied %s", r.front.record())
	}
	returned := time.Now()
	if got, want := r.front.record(), "delete:200 | create:200"; got != want {
		t.Fatalf("Google applied %s, want %s", got, want)
	}
	if s1 != 200 {
		t.Fatalf("the session's eyes: %d", s1)
	}
	watched, judged := r.stopwatchReactionAt(a), r.noticeActAt()
	t.Logf("from the ask: released +%s, returned +%s; stopwatch +%s, never-silent +%s",
		released.Sub(asked).Round(time.Millisecond), returned.Sub(asked).Round(time.Millisecond),
		watched.Sub(asked).Round(time.Millisecond), judged.Sub(asked).Round(time.Millisecond))
	if watched.Before(released) || watched.After(returned) {
		t.Errorf("the stopwatch line is at +%s from the ask, want the reaction's real completion, between the release (+%s) and the return (+%s)",
			watched.Sub(asked).Round(time.Millisecond), released.Sub(asked).Round(time.Millisecond), returned.Sub(asked).Round(time.Millisecond))
	}
	if judged.Before(asked.Add(-5*time.Millisecond)) || !judged.Before(released) {
		t.Errorf("never-silent's act is at +%s from the ask, want it dated back before the release (+%s)", judged.Sub(asked).Round(time.Millisecond), released.Sub(asked).Round(time.Millisecond))
	}
}
