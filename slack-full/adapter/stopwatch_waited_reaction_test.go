package main

// PR 109 round 3, Slack: a session reaction that waited for its turn keeps
// two times. never-silent judges the session's act by the moment it asked
// (the call dated back by its wait, round 2); the stopwatch records when the
// reaction landed. Both were fed one value, so the stopwatch line came early
// by the whole wait (the gate r2's MINOR). Google Chat keeps the two apart.
//
// The stopwatch keeps the first reaction line per message, so the reaction
// that waits is the first on its message: the session's removal of eyes
// (nothing there) is answered no_reaction and held, and the session's first
// eyes waits behind it. No acknowledgement runs (A is never admitted).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// stopwatchReactionAt is the agent's reaction line for ts in the stopwatch.
func (r *heldSlackRig) stopwatchReactionAt(ts string) time.Time {
	r.t.Helper()
	r.cfg.inboundJournal.watch.Flush()
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
			if json.Unmarshal([]byte(line), &e) == nil && e.Event == "reaction" && e.Actor == "agent" && e.Message == ts {
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

// journalActAt is the time the journal's session_react row for ts carries:
// what a restart's reconcile replays into never-silent.
func (r *heldSlackRig) journalActAt(ts string) time.Time {
	r.t.Helper()
	paths, _ := filepath.Glob(filepath.Join(r.journal, "*.jsonl"))
	sort.Strings(paths)
	var at []time.Time
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			var row inboundJournalRecord
			if json.Unmarshal([]byte(line), &row) == nil && row.SessionAct != nil && row.SessionAct.Source == "session_react" && row.SessionAct.MessageTS == ts {
				at = append(at, row.SessionAct.Timestamp)
			}
		}
	}
	if len(at) != 1 {
		r.t.Fatalf("journal session_react rows for the message: %d, want 1", len(at))
	}
	return at[0]
}

// noticeActAt is the one session act never-silent holds.
func (r *heldSlackRig) noticeActAt() time.Time {
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
	t.Run("waited", func(t *testing.T) {
		r := newHeldSlackRig(t)
		r.start("eyes")
		a := slackTimestamp(r.now)
		hold := r.arm("remove")
		var r0, s1 string
		r0Done := r.send(a, "eyes", true, &r0)
		if !waitDone(hold.applied, 10*time.Second) {
			t.Fatal("the removal never reached Slack")
		}
		asked := time.Now()
		s1Done := r.send(a, "eyes", false, &s1)
		if waitDone(s1Done, 700*time.Millisecond) {
			t.Fatalf("the session's eyes did not wait for the removal's turn: %s", s1)
		}
		released := time.Now()
		hold.free()
		if !waitDone(r0Done, 10*time.Second) || !waitDone(s1Done, 10*time.Second) {
			t.Fatalf("a call never returned; Slack applied %s", r.front.record())
		}
		returned := time.Now()
		if got, want := r.front.record(), "remove:no_reaction | add:ok"; got != want {
			t.Fatalf("Slack applied %s, want %s", got, want)
		}
		if s1 != `{"delivered":true}` {
			t.Fatalf("the session's eyes: %s", s1)
		}
		watched, journaled, judged := r.stopwatchReactionAt(a), r.journalActAt(a), r.noticeActAt()
		t.Logf("from the ask: released +%s, returned +%s; stopwatch +%s, journal +%s, never-silent +%s",
			released.Sub(asked).Round(time.Millisecond), returned.Sub(asked).Round(time.Millisecond),
			watched.Sub(asked).Round(time.Millisecond), journaled.Sub(asked).Round(time.Millisecond), judged.Sub(asked).Round(time.Millisecond))
		if watched.Before(released) || watched.After(returned) {
			t.Errorf("the stopwatch line is at +%s from the ask, want the reaction's real completion, between the release (+%s) and the return (+%s)",
				watched.Sub(asked).Round(time.Millisecond), released.Sub(asked).Round(time.Millisecond), returned.Sub(asked).Round(time.Millisecond))
		}
		if judged.Before(asked.Add(-5*time.Millisecond)) || !judged.Before(released) {
			t.Errorf("never-silent's act is at +%s from the ask, want it dated back before the release (+%s)", judged.Sub(asked).Round(time.Millisecond), released.Sub(asked).Round(time.Millisecond))
		}
		if !journaled.Equal(judged) {
			t.Errorf("the journal row carries %s and never-silent %s: a restart would replay another time", journaled, judged)
		}
	})
	t.Run("guard/did-not-wait", func(t *testing.T) {
		r := newHeldSlackRig(t)
		r.start("eyes")
		a := slackTimestamp(r.now)
		var s1 string
		if !waitDone(r.send(a, "eyes", false, &s1), 10*time.Second) || s1 != `{"delivered":true}` {
			t.Fatalf("the session's eyes: %s", s1)
		}
		watched, journaled, judged := r.stopwatchReactionAt(a), r.journalActAt(a), r.noticeActAt()
		if !watched.Equal(judged) || !journaled.Equal(judged) {
			t.Errorf("a call that did not wait: stopwatch %s, journal %s, never-silent %s, want one instant", watched, journaled, judged)
		}
	})
}
