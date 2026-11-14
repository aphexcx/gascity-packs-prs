package main

// PR 104 follow-up (gp-yhd5), Slack: the SESSION-FIRST order beside round
// 4's acknowledgement-first one. The rule is round 4's: with ACK_REACTION and
// NEVER_SILENT both on, a session's reaction counts for never-silent exactly
// as it would with the acknowledgement off; the acknowledgement never adds an
// answer and never removes one.
//
// The order: the owner's message A arrives; Jev holds the acknowledgement's
// choice, and inside that hold the session's eyes on A completes (Slack: ok,
// counted). Jev answers eyes; the acknowledgement writes its placement
// record and calls reactions.add, which Slack answers already_reacted (the
// session's eyes is there). That answer is lost, or still in flight, so the
// record stands. The session then repeats its eyes on A: Slack answers
// already_reacted again, and that repeat is no new answer.
//
// Each subtest runs twice: with ACK_REACTION=jev, and with it unset as the
// control (main's answer: the same steps with no acknowledgement and no Jev).
// The rig is round 4's (round4SlackRig: the production start, the react verb,
// the never-silent engine on a test clock) with a Slack stub that can lose or
// hold an already_reacted answer, a Jev stub that holds the choice, and a gc
// stub that answers 503 for message B.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

// sessionFirstSlack keeps one reaction per token, message and name, as Slack
// does. loseDup: Slack decides the next already_reacted and its answer never
// arrives. duringDup runs before the next already_reacted answer is sent.
type sessionFirstSlack struct {
	mu        sync.Mutex
	placed    map[string]bool
	loseDup   bool
	duringDup func()
	log       []string
}

func (s *sessionFirstSlack) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	var req slackReactionsAddReq
	_ = json.Unmarshal(body, &req)
	key := r.Header.Get("Authorization") + "\x00" + req.Channel + "\x00" + req.Timestamp + "\x00" + req.Name
	s.mu.Lock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/reactions.remove"):
		if !s.placed[key] {
			s.log = append(s.log, req.Timestamp+":"+req.Name+":remove:no_reaction")
			s.mu.Unlock()
			_, _ = io.WriteString(w, `{"ok":false,"error":"no_reaction"}`)
			return
		}
		delete(s.placed, key)
		s.log = append(s.log, req.Timestamp+":"+req.Name+":remove:ok")
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	case !strings.HasSuffix(r.URL.Path, "/reactions.add"):
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	if !s.placed[key] {
		s.placed[key] = true
		s.log = append(s.log, req.Timestamp+":"+req.Name+":ok")
		s.mu.Unlock()
		_, _ = io.WriteString(w, `{"ok":true}`)
		return
	}
	lose, during := s.loseDup, s.duringDup
	s.loseDup, s.duringDup = false, nil
	if lose {
		s.log = append(s.log, req.Timestamp+":"+req.Name+":already_reacted(lost)")
	} else {
		s.log = append(s.log, req.Timestamp+":"+req.Name+":already_reacted")
	}
	s.mu.Unlock()
	if during != nil {
		during()
	}
	if lose {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return
	}
	_, _ = io.WriteString(w, `{"ok":false,"error":"already_reacted"}`)
}

func (s *sessionFirstSlack) record() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.log, " | ")
}

type sessionFirstRig struct {
	*round4SlackRig
	slack  *sessionFirstSlack
	jevURL string
	// onJev runs inside the first Jev request, before Jev answers: the
	// acknowledgement's choice is held there.
	onJev   func()
	jevRan  atomic.Bool
	jevOnce sync.Once
	failMu  sync.Mutex
	failTS  string // gc answers 503 for the inbound that carries this ts
}

func newSessionFirstRig(t *testing.T) *sessionFirstRig {
	t.Helper()
	r := &sessionFirstRig{round4SlackRig: newRound4SlackRig(t), slack: &sessionFirstSlack{placed: map[string]bool{}}}
	stub := httptest.NewServer(http.HandlerFunc(r.slack.serve))
	t.Cleanup(stub.Close)
	withSlackAPIStub(t, stub)
	jev := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		r.jevOnce.Do(func() {
			r.jevRan.Store(true)
			if r.onJev != nil {
				r.onJev()
			}
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"ack":{"choice":"option_001"}}}`) // eyes
	}))
	t.Cleanup(jev.Close)
	r.jevURL = jev.URL
	gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		w.Header().Set("Content-Type", "application/json")
		r.failMu.Lock()
		fail := r.failTS != "" && strings.Contains(string(body), r.failTS)
		r.failMu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"unavailable"}`)
			return
		}
		_, _ = io.WriteString(w, `{"delivery":{"status":"delivered","members":[{"session_id":"example-session","status":"delivered"}]}}`)
	}))
	t.Cleanup(gc.Close)
	r.gc = gc.URL
	return r
}

// start is round4SlackRig.start with the Jev settings ACK_REACTION=jev reads.
func (r *sessionFirstRig) start(mode string) {
	r.t.Helper()
	if r.cfg.stopArrival != nil {
		r.cfg.stopArrival()
	}
	cfg := busyTestConfig(r.gc)
	cfg.busyReaction = ""
	env := map[string]string{"ACK_REACTION": mode, "GC_SERVICE_STATE_ROOT": r.state, "JEV_API_BASE_URL": r.jevURL, "JEV_API_KEY": "example-key"}
	if err := configureArrival(&cfg, func(k string) string { return env[k] }); err != nil {
		r.t.Fatal(err)
	}
	cfg.inboundJournal = &inboundJournal{dir: r.journal}
	if err := startArrival(&cfg); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(cfg.stopArrival)
	e, err := neversilent.New(r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	cfg.neverSilent = e
	if err := cfg.reconcileNeverSilent(); err != nil {
		r.t.Fatal(err)
	}
	r.cfg = cfg
}

// unreact is the session's removal of its reaction through the react verb.
func (r *sessionFirstRig) unreact(ts, emoji string) reactReceipt {
	r.t.Helper()
	w := httptest.NewRecorder()
	handleReact(r.cfg)(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(`{"conversation":{"conversation_id":"DEXAMPLE"},"message_id":"`+ts+`","emoji":"`+emoji+`","remove":true,"session_id":"example-session"}`)))
	var receipt reactReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		r.t.Fatalf("receipt %q: %v", w.Body.String(), err)
	}
	return receipt
}

// failing admits message B in the same conversation with gc answering 503,
// and waits until never-silent holds B as failing.
func (r *sessionFirstRig) failing(ts string) {
	r.t.Helper()
	r.failMu.Lock()
	r.failTS = ts
	r.failMu.Unlock()
	raw, _ := json.Marshal(slackMessageEvent{Type: "message", ChannelType: "im", Channel: "DEXAMPLE", User: "U0AAAAAA1", TS: ts, Text: "second fixture words"})
	go processSlackEvent(r.cfg, r.aliases, nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", EventID: "event-" + ts, Event: raw, receivedAt: time.Now(),
		Authorizations: []slackEventAuthorization{{UserID: "U0BOTSELF", IsBot: true}}}, func() {})
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state struct {
			Messages map[string]struct {
				FailureSince time.Time `json:"failure_since"`
				Stopped      time.Time `json:"stopped"`
			} `json:"messages"`
		}
		data, _ := os.ReadFile(r.opts.Path)
		_ = json.Unmarshal(data, &state)
		if m, ok := state.Messages[neverSilentID("DEXAMPLE", ts)]; ok && (!m.FailureSince.IsZero() || !m.Stopped.IsZero()) {
			return
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("B %s was never held as failing: %s", ts, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// later is a Slack ts after prev, on the wall clock the react verb's acts use.
func (r *sessionFirstRig) later(prev string) string {
	for {
		time.Sleep(5 * time.Millisecond)
		if ts := slackTimestamp(time.Now()); ts != prev {
			return ts
		}
	}
}

// placed reports whether a placement record of the acknowledgement is on disk.
func (r *sessionFirstRig) placed() bool {
	for _, f := range r.ackFiles() {
		if strings.HasPrefix(f, "ack-placed/") {
			return true
		}
	}
	return false
}

// firstEyes is the session-first order up to the repeat on A
// (slackTimestamp(r.now)). With mode set, the session's eyes on A completes
// inside the Jev hold and then steps runs, still inside it; unset, the
// session's eyes and steps follow A's delivery (main: no acknowledgement, no
// Jev). It returns A's ts and the acts after the first eyes.
func (r *sessionFirstRig) firstEyes(mode string, steps func(a string)) (string, int) {
	r.t.Helper()
	a := slackTimestamp(r.now)
	var actsAfterFirst atomic.Int64
	actsAfterFirst.Store(-1)
	first := func() {
		if receipt := r.react(a, "eyes"); !receipt.Delivered {
			r.t.Errorf("the session's first eyes: receipt=%+v, want delivered", receipt)
		}
		actsAfterFirst.Store(int64(r.acts()))
		if steps != nil {
			steps(a)
		}
	}
	if mode != "" {
		r.onJev = first
	}
	r.human(a)
	if mode == "" {
		first()
	} else if !r.jevRan.Load() {
		r.t.Fatal("Jev was never asked: the acknowledgement's choice was not held")
	}
	return a, int(actsAfterFirst.Load())
}

func TestSessionFirstReactionCountsAsWithAckOff(t *testing.T) {
	for _, mode := range []string{"jev", ""} {
		name := "ack-" + mode
		if mode == "" {
			name = "control-ack-unset"
		}

		// The session's eyes completes first; the acknowledgement's
		// already_reacted answer is lost, so its record stands; a restart;
		// the session repeats eyes. The first counts once, the repeat adds
		// no act.
		t.Run("session-first/ack-answer-lost/"+name, func(t *testing.T) {
			r := newSessionFirstRig(t)
			r.start(mode)
			// The session's first eyes is placed (ok): the next
			// already_reacted is the acknowledgement's.
			r.slack.loseDup = mode != ""
			a, actsAfterFirst := r.firstEyes(mode, nil)
			if mode != "" && (!strings.Contains(r.slack.record(), ":eyes:already_reacted(lost)") || !r.placed()) {
				t.Fatalf("the acknowledgement's answer was not lost with its record standing: reactions.add %s; files %q", r.slack.record(), r.ackFiles())
			}
			r.start(mode)
			if receipt := r.react(a, "eyes"); !receipt.Delivered {
				t.Fatalf("the session's repeat: receipt=%+v, want delivered", receipt)
			}
			t.Logf("reactions.add: %s", r.slack.record())
			if actsAfterFirst != 1 {
				t.Errorf("after the session's first eyes: acts=%d, want 1", actsAfterFirst)
			}
			if got := r.acts(); got != 1 {
				t.Errorf("after the session's repeat: acts=%d, want 1 (a repeat is no new answer, as with ACK_REACTION unset)", got)
			}
			if got, want := strings.Join(r.journaled(), ","), "eyes:counted,eyes:not counted"; got != want {
				t.Errorf("journal session_react rows=%q, want %q", got, want)
			}
		})

		// The same, with the acknowledgement's already_reacted still in flight
		// when the session repeats (no fault at all).
		t.Run("session-first/ack-answer-pending/"+name, func(t *testing.T) {
			r := newSessionFirstRig(t)
			r.start(mode)
			var actsAfterRepeat atomic.Int64
			actsAfterRepeat.Store(-1)
			repeat := func(a string) {
				if receipt := r.react(a, "eyes"); !receipt.Delivered {
					t.Errorf("the session's repeat: receipt=%+v, want delivered", receipt)
				}
				actsAfterRepeat.Store(int64(r.acts()))
			}
			if mode != "" {
				a := slackTimestamp(r.now)
				r.slack.duringDup = func() {
					if !r.placed() {
						t.Errorf("no placement record while the acknowledgement's answer is in flight: %q", r.ackFiles())
					}
					repeat(a)
				}
			}
			a, actsAfterFirst := r.firstEyes(mode, nil)
			if mode == "" {
				repeat(a)
			}
			if actsAfterRepeat.Load() < 0 {
				t.Fatalf("the session never repeated inside the acknowledgement's call: reactions.add %s", r.slack.record())
			}
			t.Logf("reactions.add: %s", r.slack.record())
			if actsAfterFirst != 1 {
				t.Errorf("after the session's first eyes: acts=%d, want 1", actsAfterFirst)
			}
			if got := actsAfterRepeat.Load(); got != 1 {
				t.Errorf("after the session's repeat with the acknowledgement's answer in flight: acts=%d, want 1", got)
			}
			if got, want := strings.Join(r.journaled(), ","), "eyes:counted,eyes:not counted"; got != want {
				t.Errorf("journal session_react rows=%q, want %q", got, want)
			}
		})

		// THE CONSEQUENCE: message B in the same conversation fails delivery
		// (gc 503). The session repeats eyes on A; three minutes on, B's
		// not_delivered line IS posted (an act answers every message of its
		// conversation, so a counted repeat would answer B).
		t.Run("session-first/later-message-notice/"+name, func(t *testing.T) {
			r := newSessionFirstRig(t)
			r.start(mode)
			r.slack.loseDup = mode != ""
			a, _ := r.firstEyes(mode, nil)
			r.start(mode)
			r.failing(r.later(a))
			if receipt := r.react(a, "eyes"); !receipt.Delivered {
				t.Fatalf("the session's repeat: receipt=%+v, want delivered", receipt)
			}
			r.cfg.arrival.Wait()
			if posts := r.due(); posts != 1 {
				t.Errorf("owner lines=%d, want 1: B's not_delivered line (as with ACK_REACTION unset); acts=%d, journal=%q", posts, r.acts(), strings.Join(r.journaled(), ","))
			}
		})

		// A GUARD, green at the base: the session's eyes, then its removal,
		// both inside the hold; the acknowledgement then places eyes (ok). The
		// session's next eyes is answered already_reacted, and it counts: with
		// ACK_REACTION unset Slack would have answered it ok.
		t.Run("session-first/removed-then-acknowledged/"+name, func(t *testing.T) {
			r := newSessionFirstRig(t)
			r.start(mode)
			a, actsAfterFirst := r.firstEyes(mode, func(a string) {
				if receipt := r.unreact(a, "eyes"); !receipt.Delivered {
					t.Errorf("the session's removal: receipt=%+v, want delivered", receipt)
				}
			})
			if receipt := r.react(a, "eyes"); !receipt.Delivered {
				t.Fatalf("the session's eyes after its removal: receipt=%+v, want delivered", receipt)
			}
			t.Logf("reactions: %s", r.slack.record())
			if actsAfterFirst != 1 {
				t.Errorf("after the session's first eyes: acts=%d, want 1", actsAfterFirst)
			}
			if got := r.acts(); got != 2 {
				t.Errorf("after the session's eyes that followed its removal: acts=%d, want 2 (as with ACK_REACTION unset)", got)
			}
			if got, want := strings.Join(r.journaled(), ","), "eyes:counted,eyes:counted"; got != want {
				t.Errorf("journal session_react rows=%q, want %q", got, want)
			}
		})
	}
}
