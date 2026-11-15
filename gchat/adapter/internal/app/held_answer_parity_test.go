package app

// PR 109 round 2, Google Chat: the session's mark against Google's own order.
//
// THE CLASS: the mark is the adapter's belief that the session's own reaction
// is on the message. The react verb writes and clears it when it handles
// Google's answer, and reads it to judge another call whose answer Google
// decided at a different moment. Whenever the order in which Google applied
// the calls differs from the order in which the adapter handled their
// answers, the mark is stale: it vetoes a reaction that counts with
// ACK_REACTION unset (and the react verb answers 502), or it is missing and
// a repeat counts.
//
// ONE PARITY TEST UNDER HELD ANSWERS. A proxy in front of the fake Google
// can hold the answer of any call after Google applied it: the session's
// create, the session's delete, the acknowledgement's create. Each row is a
// sequence of those calls and of the moments their answers are released. Run
// with ACK_REACTION set, the react verb's status codes and the acts must
// equal the same row run with ACK_REACTION unset (main's answer). The order
// Google applied the calls in is checked too, so a row cannot pass without
// reaching its order.
//
// A call that is not held is waited for until it returns, or for
// heldPending while it has not (a call that waits behind another call on
// the same reaction); the row then goes on. A "prompt" call must return
// while the held answers are still held.
//
// The rig is round 4's (round4Chat: the start's own pieces, the react verb,
// the never-silent engine on a test clock), its client pointed at the proxy.
// The session reacts before the adapter admits the message, as round 1's
// session-first subtests do; "ack" admits the message and waits for the
// acknowledgement (with ACK_REACTION unset it admits the message).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	api "google.golang.org/api/chat/v1"
)

// heldPending is how long a row waits for a call that has not returned
// before it goes on. It is longer than the shared reaction pace's interval.
const heldPending = 750 * time.Millisecond

type heldAnswer struct {
	applied chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldAnswer) free() { h.once.Do(func() { close(h.release) }) }

// heldGoogle stands in front of the fake Google. It applies every call there
// and logs what Google answered to reaction creates and deletes, in the order
// Google applied them; the next call of an armed kind ("create" or "delete")
// has its answer held until released, or until its caller gives up.
type heldGoogle struct {
	target string
	mu     sync.Mutex
	armed  map[string]*heldAnswer
	log    []string
}

func (h *heldGoogle) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	out, err := http.NewRequestWithContext(req.Context(), req.Method, h.target+req.URL.RequestURI(), bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	out.Header = req.Header.Clone()
	kind := ""
	switch {
	case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/reactions"):
		kind = "create"
	case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/reactions/"):
		kind = "delete"
	}
	h.mu.Lock()
	resp, err := http.DefaultTransport.RoundTrip(out)
	if err != nil {
		h.mu.Unlock()
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	hold := h.armed[kind]
	delete(h.armed, kind)
	if kind != "" {
		h.log = append(h.log, kind+":"+strconv.Itoa(resp.StatusCode))
	}
	h.mu.Unlock()
	if hold != nil {
		close(hold.applied)
		select {
		case <-hold.release:
		case <-req.Context().Done():
			return
		}
	}
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(data)
}

func (h *heldGoogle) arm(kind string) *heldAnswer {
	hold := &heldAnswer{applied: make(chan struct{}), release: make(chan struct{})}
	h.mu.Lock()
	h.armed[kind] = hold
	h.mu.Unlock()
	return hold
}

func (h *heldGoogle) record() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.log, " | ")
}

// heldChat is round 4's rig with its client behind the holding proxy.
type heldChat struct {
	*round4Chat
	front *heldGoogle
	holds []*heldAnswer
}

func newHeldChat(t *testing.T) *heldChat {
	t.Helper()
	r := &heldChat{round4Chat: newRound4Chat(t)}
	r.front = &heldGoogle{target: r.google.URL(), armed: map[string]*heldAnswer{}}
	proxy := httptest.NewServer(http.HandlerFunc(r.front.serve))
	t.Cleanup(proxy.Close)
	r.cfg.Endpoints.ChatAPIBase = proxy.URL
	// Runs before proxy.Close: a held answer never outlives its test.
	t.Cleanup(func() {
		for _, h := range r.holds {
			h.free()
		}
	})
	return r
}

func (r *heldChat) arm(kind string) *heldAnswer {
	h := r.front.arm(kind)
	r.holds = append(r.holds, h)
	return h
}

// send is the session's call through the react verb, in a goroutine; done
// closes when it returns, with its status code in *code.
func (r *heldChat) send(ctx context.Context, message, emoji string, remove bool, code *int) chan struct{} {
	body, _ := json.Marshal(map[string]any{"message": message, "emoji": emoji, "remove": remove, "session_id": "example-session"})
	s := r.s
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(string(body))).WithContext(ctx))
		*code = w.Code
	}()
	return done
}

func waitDone(done <-chan struct{}, d time.Duration) bool {
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

type heldRow struct {
	name string
	// steps, separated by "; ":
	//   add ID [thumbsup] [B] [held|prompt]   the session's create (eyes on A unless named)
	//   remove ID [held]                      the session's removal of eyes on A
	//   ack [held]                            admit A; with ACK_REACTION set, its acknowledgement
	//   release ID                            release a held answer (ID "ack": the acknowledgement's)
	//   settle                                wait for every call; note the acts
	steps string
	// The order Google applied the creates and deletes in, ACK_REACTION set
	// and unset.
	googleAck, googleOff string
	// Red at 3dc345a1 (the round's base) or a guard that is green there.
	red bool
}

const heldOther = round4Space + "/messages/other"

// run plays one row with ACK_REACTION set to mode and returns its trace: the
// acts at each settle, then each call's status code in the order the calls
// were made.
func (r *heldChat) run(row heldRow, mode string) []string {
	t := r.t
	t.Helper()
	r.start(mode)
	r.google.Configure(func(g *fakegoogle.Server) {
		g.Messages[heldOther] = &api.Message{Name: heldOther, Space: &api.Space{Name: round4Space, SpaceType: "DIRECT_MESSAGE"}}
	})
	a := r.sessionFirstNext()
	type call struct {
		id   string
		done chan struct{}
		code *int
	}
	var calls []*call
	byID := map[string]*call{}
	holds := map[string]*heldAnswer{}
	var ackDone chan struct{}
	var trace []string
	settle := func() {
		for _, c := range calls {
			if !waitDone(c.done, 15*time.Second) {
				t.Fatalf("%s: the call %s never returned; Google applied %s", row.name, c.id, r.front.record())
			}
		}
		if ackDone != nil && !waitDone(ackDone, 15*time.Second) {
			t.Fatalf("%s: the acknowledgement never finished; Google applied %s", row.name, r.front.record())
		}
		trace = append(trace, "acts="+strconv.Itoa(r.acts()))
	}
	admit := func() {
		if got := r.human(); got != a {
			t.Errorf("%s: admitted %q, want %q", row.name, got, a)
		}
	}
	for _, step := range strings.Split(row.steps, "; ") {
		f := strings.Fields(step)
		has := func(word string) bool {
			for _, x := range f[1:] {
				if x == word {
					return true
				}
			}
			return false
		}
		switch f[0] {
		case "add", "remove":
			message, emoji := a, round4Eyes
			if has("B") {
				message = heldOther
			}
			if has("thumbsup") {
				emoji = round4Thumb
			}
			var hold *heldAnswer
			if has("held") {
				kind := "create"
				if f[0] == "remove" {
					kind = "delete"
				}
				hold = r.arm(kind)
				holds[f[1]] = hold
			}
			c := &call{id: f[1], code: new(int)}
			c.done = r.send(context.Background(), message, emoji, f[0] == "remove", c.code)
			calls = append(calls, c)
			byID[c.id] = c
			switch {
			case hold != nil:
				if !waitDone(hold.applied, 10*time.Second) {
					t.Fatalf("%s: the call %s never reached Google; Google applied %s", row.name, c.id, r.front.record())
				}
			case has("prompt"):
				if !waitDone(c.done, 10*time.Second) {
					t.Errorf("%s: the call %s waited behind another reaction's call", row.name, c.id)
				}
			default:
				if !waitDone(c.done, heldPending) {
					t.Logf("%s: the call %s has not returned after %s; the row goes on", row.name, c.id, heldPending)
				}
			}
		case "ack":
			if mode == "" || !has("held") {
				admit()
				continue
			}
			hold := r.arm("create")
			holds["ack"] = hold
			ackDone = make(chan struct{})
			go func() { defer close(ackDone); admit() }()
			if !waitDone(hold.applied, 10*time.Second) {
				t.Fatalf("%s: the acknowledgement's create never reached Google; Google applied %s", row.name, r.front.record())
			}
		case "release":
			hold := holds[f[1]]
			if hold == nil {
				if f[1] == "ack" && mode == "" {
					continue
				}
				t.Fatalf("%s: nothing held for %s", row.name, f[1])
			}
			hold.free()
			done := ackDone
			if c := byID[f[1]]; c != nil {
				done = c.done
			}
			if !waitDone(done, 10*time.Second) {
				t.Fatalf("%s: %s never returned after its answer was released", row.name, f[1])
			}
		case "settle":
			settle()
		default:
			t.Fatalf("%s: unknown step %q", row.name, step)
		}
	}
	settle()
	r.service.Wait()
	for _, c := range calls {
		trace = append(trace, c.id+"="+strconv.Itoa(*c.code))
	}
	want := row.googleOff
	if mode != "" {
		want = row.googleAck
	}
	if got := r.front.record(); got != want {
		t.Errorf("%s (ACK_REACTION=%q): Google applied %s, want %s: the row did not reach its order", row.name, mode, got, want)
	}
	return trace
}

func heldChatRows() []heldRow {
	return []heldRow{
		{
			// Case 1, the gate's sequence: the session's removal is applied
			// and its answer is pending; the acknowledgement creates eyes; the
			// session's eyes again.
			name:      "case1/removal-applied-answer-pending",
			steps:     "add s1; remove r1 held; ack; add s2; release r1; settle",
			googleAck: "create:200 | delete:200 | create:200 | create:409",
			googleOff: "create:200 | delete:200 | create:200",
			red:       true,
		},
		{
			// Case 3: the session's create and removal overlap; Google applies
			// the create first, the adapter handles the removal's answer first.
			name:      "case3/add-and-removal-answered-in-the-other-order",
			steps:     "add s1 held; remove r1; release s1; settle; ack; add s2",
			googleAck: "create:200 | delete:200 | create:200 | create:409",
			googleOff: "create:200 | delete:200 | create:200",
			red:       true,
		},
		{
			// Case 4, the mirror of case 1 once the held answer lands: a later
			// repeat must count nothing, as with ACK_REACTION unset.
			name:      "case4/repeat-after-the-held-removal-lands",
			steps:     "add s1; remove r1 held; ack; add s2; release r1; settle; add s3",
			googleAck: "create:200 | delete:200 | create:200 | create:409 | create:409",
			googleOff: "create:200 | delete:200 | create:200 | create:409",
			red:       true,
		},
		{
			// The class's over-count direction: the acknowledgement's record
			// stands with no reaction under it (the session removed the
			// acknowledgement's eyes); two identical session creates overlap
			// and the second's 409 is handled before the first's created.
			name:      "overlap/identical-adds-answered-in-the-other-order",
			steps:     "ack; remove r0; add s1 held; add s2; release s1",
			googleAck: "create:200 | delete:200 | create:200 | create:409",
			googleOff: "create:200 | create:409",
			red:       true,
		},
		{
			// The same direction with the acknowledgement's 409 still in
			// flight: its record stands while the session's first created is
			// unhandled.
			name:      "overlap/session-add-pending-while-ack-answer-pending",
			steps:     "add s1 held; ack held; add s2; release s1; release ack",
			googleAck: "create:200 | create:409 | create:409",
			googleOff: "create:200 | create:409",
			red:       true,
		},
		{
			// GUARD: the acknowledgement's call never holds up the session's.
			name:      "guard/session-add-while-ack-answer-held",
			steps:     "ack held; add s1 prompt; release ack; add s2",
			googleAck: "create:200 | create:409 | create:409",
			googleOff: "create:200 | create:409",
		},
		{
			// GUARD: a held call holds up no other emoji and no other message.
			name:      "guard/other-emoji-and-message-not-held",
			steps:     "add s1; remove r1 held; add t1 thumbsup prompt; add b1 B prompt; release r1; ack",
			googleAck: "create:200 | delete:200 | create:200 | create:200 | create:200",
			googleOff: "create:200 | delete:200 | create:200 | create:200",
		},
	}
}

func TestHeldAnswerParityWithAckOff(t *testing.T) {
	for _, row := range heldChatRows() {
		t.Run(row.name, func(t *testing.T) {
			off := newHeldChat(t).run(row, "")
			on := newHeldChat(t).run(row, "eyes")
			t.Logf("ACK_REACTION unset: %s", strings.Join(off, " ; "))
			t.Logf("ACK_REACTION=eyes:  %s", strings.Join(on, " ; "))
			if strings.Join(on, "\n") != strings.Join(off, "\n") {
				t.Errorf("with ACK_REACTION=eyes the row's trace differs from the same row with it unset:\n on: %s\noff: %s", strings.Join(on, " ; "), strings.Join(off, " ; "))
			}
		})
	}
}

// The lock's bound: a removal whose answer never comes holds its reaction only
// until the call ends (here its request's own deadline; live, RemoveReaction's
// 30 s); the session's next call on that reaction then goes through while the
// answer is still held.
func TestReactVerbHoldEndsWithTheCallsTimeout(t *testing.T) {
	r := newHeldChat(t)
	r.start("eyes")
	a := r.sessionFirstNext()
	if code, body := r.react(a, round4Eyes); code != http.StatusOK {
		t.Fatalf("the session's first eyes: status=%d %s", code, body)
	}
	hold := r.arm("delete")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var removal, next int
	removed := r.send(ctx, a, round4Eyes, true, &removal)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatal("the removal never reached Google")
	}
	start := time.Now()
	nextDone := r.send(context.Background(), a, round4Eyes, false, &next)
	if !waitDone(nextDone, 5*time.Second) {
		t.Fatalf("the session's next eyes was still waiting %s after the removal's call ended", 5*time.Second)
	}
	waited := time.Since(start)
	if !waitDone(removed, 5*time.Second) {
		t.Fatal("the removal never returned")
	}
	select {
	case <-hold.release:
		t.Fatal("the removal's answer was released")
	default:
	}
	t.Logf("removal %d; next eyes %d after %s; Google applied %s", removal, next, waited.Round(time.Millisecond), r.front.record())
	if removal != http.StatusBadGateway {
		t.Errorf("removal status %d, want 502 for a call with no answer", removal)
	}
	if next != http.StatusOK {
		t.Errorf("next eyes status %d, want 200", next)
	}
}
