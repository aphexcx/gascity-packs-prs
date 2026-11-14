package app

// PR 104 follow-up (gp-yhd5), Google Chat: the SESSION-FIRST order beside
// round 4's acknowledgement-first one. The rule is round 4's: with
// ACK_REACTION and NEVER_SILENT both on, a session's reaction counts for
// never-silent exactly as it would with the acknowledgement off; the
// acknowledgement never adds an answer and never removes one.
//
// The order: the session's eyes on message A is created before the
// acknowledgement writes its placement record. Here the session reacts before
// the adapter admits A, which for the record is the same order as a session
// reacting while the acknowledgement's choice is still pending. The
// acknowledgement then creates eyes on A; Google answers 409 (the identity's
// eyes is there). That answer is lost, or still in flight, so the record
// stands. The session then repeats its eyes on A: Google answers 409 again,
// and that repeat is no new answer (main's react verb: 502, no act).
//
// Each subtest runs twice: with ACK_REACTION=eyes, and with it unset as the
// control (main's answer). The rig is round 4's (round4Chat: the start's own
// pieces, the react verb, the never-silent engine on a test clock).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// sessionFirstNext names the next message human admits and makes it known to
// Google, so the session can react to it before the adapter admits it.
func (r *round4Chat) sessionFirstNext() string {
	name := fmt.Sprintf("%s/messages/m%d", round4Space, r.seq+1)
	r.google.Configure(func(g *fakegoogle.Server) {
		g.Messages[name] = &api.Message{Name: name, Space: &api.Space{Name: round4Space, SpaceType: "DIRECT_MESSAGE"}}
	})
	return name
}

// sessionFirstUnreact is the session's removal of its reaction through the
// react verb.
func (r *round4Chat) sessionFirstUnreact(message, emoji string) (int, string) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"message": message, "emoji": emoji, "remove": true, "session_id": "example-session"})
	w := httptest.NewRecorder()
	r.s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(string(body))))
	return w.Code, strings.TrimSpace(w.Body.String())
}

// sessionFirstFailing admits message B in the same space with gc answering a
// failed delivery, and waits for the acknowledgement's work when it is on.
func (r *round4Chat) sessionFirstFailing() string {
	r.t.Helper()
	r.seq++
	m := &api.Message{Name: fmt.Sprintf("%s/messages/m%d", round4Space, r.seq), Text: "second fixture words", CreateTime: r.clock.Now().Format(time.RFC3339Nano),
		Space: &api.Space{Name: round4Space, SpaceType: "DIRECT_MESSAGE"}, Sender: &api.User{Name: "users/456", DisplayName: "Reader", Type: "HUMAN"}}
	r.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
	r.gc.Configure(func(s *fakegc.Server) {
		s.Script = []wire.Delivery{{Status: "failed", Members: []wire.DeliveryMember{{SessionID: "example-session", Status: "failed"}}}}
	})
	if _, err := r.p.Handle(context.Background(), &puller.Event{PubSubID: "event-" + m.Name, Changes: []puller.Change{{Kind: "message.created", Message: m, Space: round4Space}}}); err != nil {
		r.t.Fatalf("B Handle: %v", err)
	}
	if r.service.Choice.Enabled() {
		r.service.Wait()
	}
	return m.Name
}

// sessionFirstPlaced reports whether a placement record of the
// acknowledgement is on disk.
func (r *round4Chat) sessionFirstPlaced() bool {
	for _, f := range r.ackFiles() {
		if strings.HasPrefix(f, "ack-placed/") {
			return true
		}
	}
	return false
}

// sessionFirstEyes is the session's eyes on A, created before the adapter
// admits A; then steps (the session's own, in both modes); then A's admission
// with its acknowledgement when on. It returns A and the acts after the first
// eyes.
func (r *round4Chat) sessionFirstEyes(steps func(a string)) (string, int) {
	r.t.Helper()
	a := r.sessionFirstNext()
	if code, body := r.react(a, round4Eyes); code != http.StatusOK {
		r.t.Fatalf("the session's first eyes: status=%d %s, want 200", code, body)
	}
	actsAfterFirst := r.acts()
	if steps != nil {
		steps(a)
	}
	if got := r.human(); got != a {
		r.t.Fatalf("admitted %q, want %q", got, a)
	}
	return a, actsAfterFirst
}

func TestSessionFirstReactionCountsAsWithAckOff(t *testing.T) {
	for _, mode := range []string{"eyes", ""} {
		name := "ack-" + mode
		if mode == "" {
			name = "control-ack-unset"
		}

		// The session's eyes is created first; the acknowledgement's 409 is
		// lost, so its record stands; a restart; the session repeats eyes.
		// The first counts once, the repeat adds no act.
		t.Run("session-first/ack-answer-lost/"+name, func(t *testing.T) {
			r := newRound4Chat(t)
			r.start(mode)
			// The session's first eyes is created; the next create is the
			// acknowledgement's, whose 409 is lost.
			a, actsAfterFirst := r.sessionFirstEyes(func(string) { r.lose.Store(mode != "") })
			if mode != "" && (r.lose.Load() || !r.sessionFirstPlaced()) {
				t.Fatalf("the acknowledgement's answer was not lost with its record standing: files %q", r.ackFiles())
			}
			r.start(mode)
			code, body := r.react(a, round4Eyes)
			if actsAfterFirst != 1 {
				t.Errorf("after the session's first eyes: acts=%d, want 1", actsAfterFirst)
			}
			if got := r.acts(); got != 1 {
				t.Errorf("after the session's repeat: acts=%d, want 1 (a repeat is no new answer, as with ACK_REACTION unset)", got)
			}
			if code != http.StatusBadGateway {
				t.Errorf("the session's repeat: status=%d %s, want main's 502", code, body)
			}
		})

		// The same, with the acknowledgement's 409 still in flight when the
		// session repeats.
		t.Run("session-first/ack-answer-pending/"+name, func(t *testing.T) {
			r := newRound4Chat(t)
			r.start(mode)
			var actsAfterRepeat, repeatCode atomic.Int64
			actsAfterRepeat.Store(-1)
			repeat := func(a string) {
				code, _ := r.react(a, round4Eyes)
				repeatCode.Store(int64(code))
				actsAfterRepeat.Store(int64(r.acts()))
			}
			a, actsAfterFirst := r.sessionFirstEyes(func(a string) {
				if mode == "" {
					return
				}
				r.duringMu.Lock()
				r.during = func() {
					if !r.sessionFirstPlaced() {
						t.Errorf("no placement record while the acknowledgement's answer is in flight: %q", r.ackFiles())
					}
					repeat(a)
				}
				r.duringMu.Unlock()
			})
			if mode == "" {
				repeat(a)
			}
			if actsAfterRepeat.Load() < 0 {
				t.Fatal("the session never repeated inside the acknowledgement's call")
			}
			if actsAfterFirst != 1 {
				t.Errorf("after the session's first eyes: acts=%d, want 1", actsAfterFirst)
			}
			if got := actsAfterRepeat.Load(); got != 1 {
				t.Errorf("after the session's repeat with the acknowledgement's answer in flight: acts=%d, want 1", got)
			}
			if got := repeatCode.Load(); got != http.StatusBadGateway {
				t.Errorf("the session's repeat: status=%d, want main's 502", got)
			}
		})

		// THE CONSEQUENCE: message B in the same space fails delivery. The
		// session repeats eyes on A; three minutes on, B's not_delivered line
		// IS posted (an act answers every message of its space, so a counted
		// repeat would answer B).
		t.Run("session-first/later-message-notice/"+name, func(t *testing.T) {
			r := newRound4Chat(t)
			r.start(mode)
			a, _ := r.sessionFirstEyes(func(string) { r.lose.Store(mode != "") })
			r.start(mode)
			// The rig's clock stands at its start; the react verb's acts are
			// on the wall clock. Live, both are the wall clock: bring the rig's
			// up to it so B is created after the session's first eyes.
			r.clock.mu.Lock()
			r.clock.now = time.Now().UTC()
			r.clock.mu.Unlock()
			r.sessionFirstFailing()
			r.react(a, round4Eyes)
			if lines := r.due(); lines != 1 {
				t.Errorf("owner lines=%d, want 1: B's not_delivered line (as with ACK_REACTION unset); acts=%d", lines, r.acts())
			}
		})

		// A GUARD, green at the base: the session's eyes and then its removal,
		// both before the acknowledgement; the acknowledgement then creates
		// eyes. The session's next eyes gets 409, and it counts: with
		// ACK_REACTION unset Google would have created it.
		t.Run("session-first/removed-then-acknowledged/"+name, func(t *testing.T) {
			r := newRound4Chat(t)
			r.start(mode)
			a, actsAfterFirst := r.sessionFirstEyes(func(a string) {
				if code, body := r.sessionFirstUnreact(a, round4Eyes); code != http.StatusOK {
					t.Errorf("the session's removal: status=%d %s, want 200", code, body)
				}
			})
			if mode != "" && len(r.reactions()) != 1 {
				t.Fatalf("reactions=%q, want the acknowledgement's eyes", r.reactions())
			}
			code, body := r.react(a, round4Eyes)
			if code != http.StatusOK {
				t.Errorf("the session's eyes after its removal: status=%d %s, want 200", code, body)
			}
			if actsAfterFirst != 1 {
				t.Errorf("after the session's first eyes: acts=%d, want 1", actsAfterFirst)
			}
			if got := r.acts(); got != 2 {
				t.Errorf("after the session's eyes that followed its removal: acts=%d, want 2 (as with ACK_REACTION unset)", got)
			}
		})
	}
}
