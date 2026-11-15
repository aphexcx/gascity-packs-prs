package main

// PR 109 round 2, Slack: the session's mark against Slack's own order.
//
// THE CLASS: the mark is the adapter's belief that the session's own reaction
// is on the message. The react verb writes and clears it when it handles
// Slack's answer, and reads it to judge another call whose answer Slack
// decided at a different moment. Whenever the order in which Slack applied
// the calls differs from the order in which the adapter handled their
// answers, the mark is stale: it vetoes a reaction that counts with
// ACK_REACTION unset, or it is missing and a repeat counts.
//
// ONE PARITY TEST UNDER HELD ANSWERS. The Slack stub can hold the answer of
// any call after applying it: the session's add, the session's removal, the
// acknowledgement's add. Each row is a sequence of those calls and of the
// moments their answers are released. Run with ACK_REACTION set, the
// receipts, the journal's session_react rows and the acts must equal the
// same row run with ACK_REACTION unset (main's answer). The order Slack
// applied the calls in is checked too, so a row cannot pass without reaching
// its order.
//
// A call that is not held is waited for until it returns, or for
// heldPending while it has not (a call that waits behind another call on
// the same reaction); the row then goes on. A "prompt" call must return
// while the held answers are still held.
//
// The rig is round 4's (round4SlackRig: the production start, handleReact,
// the never-silent engine on a test clock) with a front stub that holds
// answers. The session reacts before the adapter admits the message, which
// for the record is the same order as a session reacting while the
// acknowledgement's choice is pending; "ack" admits the message and waits
// for the acknowledgement (with ACK_REACTION unset it admits the message).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldPending is how long a row waits for a call that has not returned
// before it goes on.
const heldPending = 500 * time.Millisecond

type heldAnswer struct {
	applied chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *heldAnswer) free() { h.once.Do(func() { close(h.release) }) }

// heldSlack stands in front of round 4's Slack stub. It applies every call
// there and logs what Slack answered, in the order Slack applied the calls;
// the next call of an armed kind ("add" or "remove") has its answer held
// until released, or until its caller gives up.
type heldSlack struct {
	inner func(http.ResponseWriter, *http.Request)
	mu    sync.Mutex
	armed map[string]*heldAnswer
	log   []string
}

func (h *heldSlack) serve(w http.ResponseWriter, req *http.Request) {
	kind := ""
	switch {
	case strings.HasSuffix(req.URL.Path, "/reactions.add"):
		kind = "add"
	case strings.HasSuffix(req.URL.Path, "/reactions.remove"):
		kind = "remove"
	}
	rec := httptest.NewRecorder()
	h.mu.Lock()
	h.inner(rec, req)
	var answer struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &answer)
	hold := h.armed[kind]
	delete(h.armed, kind)
	if kind != "" {
		word := "ok"
		if !answer.OK {
			word = answer.Error
		}
		h.log = append(h.log, kind+":"+word)
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
	for k, v := range rec.Header() {
		w.Header()[k] = v
	}
	w.WriteHeader(rec.Code)
	_, _ = w.Write(rec.Body.Bytes())
}

func (h *heldSlack) arm(kind string) *heldAnswer {
	hold := &heldAnswer{applied: make(chan struct{}), release: make(chan struct{})}
	h.mu.Lock()
	h.armed[kind] = hold
	h.mu.Unlock()
	return hold
}

func (h *heldSlack) record() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.log, " | ")
}

// heldSlackRig is round 4's rig behind the holding front.
type heldSlackRig struct {
	*round4SlackRig
	front *heldSlack
	holds []*heldAnswer
}

func newHeldSlackRig(t *testing.T) *heldSlackRig {
	t.Helper()
	r := &heldSlackRig{round4SlackRig: newRound4SlackRig(t)}
	r.front = &heldSlack{inner: r.slack.serve, armed: map[string]*heldAnswer{}}
	stub := httptest.NewServer(http.HandlerFunc(r.front.serve))
	t.Cleanup(stub.Close)
	withSlackAPIStub(t, stub)
	// Runs before stub.Close: a held answer never outlives its test.
	t.Cleanup(func() {
		for _, h := range r.holds {
			h.free()
		}
	})
	return r
}

func (r *heldSlackRig) arm(kind string) *heldAnswer {
	h := r.front.arm(kind)
	r.holds = append(r.holds, h)
	return h
}

// send is the session's call through the react verb, in a goroutine; done
// closes when it returns, with its body in *out.
func (r *heldSlackRig) send(ts, emoji string, remove bool, out *string) chan struct{} {
	body := `{"conversation":{"conversation_id":"DEXAMPLE"},"message_id":"` + ts + `","emoji":"` + emoji + `","session_id":"example-session"`
	if remove {
		body += `,"remove":true`
	}
	body += "}"
	cfg := r.cfg
	done := make(chan struct{})
	go func() {
		defer close(done)
		w := httptest.NewRecorder()
		handleReact(cfg)(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(body)))
		*out = strings.TrimSpace(w.Body.String())
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
	//   add ID [thumbsup] [B] [held|prompt]   the session's add (eyes on A unless named)
	//   remove ID [held]                      the session's removal of eyes on A
	//   ack [held]                            admit A; with ACK_REACTION set, its acknowledgement
	//   release ID                            release a held answer (ID "ack": the acknowledgement's)
	//   settle                                wait for every call; note the acts
	steps string
	// The order Slack applied the calls in, ACK_REACTION set and unset.
	slackAck, slackOff string
	// Red at 3dc345a1 (the round's base) or a guard that is green there.
	red bool
}

// run plays one row with ACK_REACTION set to mode and returns its trace: at
// each settle the acts and the journal's session_react rows since the last
// settle, then each call's receipt in the order the calls were made.
func (r *heldSlackRig) run(row heldRow, mode string) []string {
	t := r.t
	t.Helper()
	r.start(mode)
	a := slackTimestamp(r.now)
	b := slackTimestamp(r.now.Add(time.Second))
	type call struct {
		id   string
		done chan struct{}
		body *string
	}
	var calls []*call
	byID := map[string]*call{}
	holds := map[string]*heldAnswer{}
	var ackDone chan struct{}
	var trace []string
	journaled := 0
	settle := func() {
		for _, c := range calls {
			if !waitDone(c.done, 15*time.Second) {
				t.Fatalf("%s: the call %s never returned; Slack applied %s", row.name, c.id, r.front.record())
			}
		}
		if ackDone != nil && !waitDone(ackDone, 15*time.Second) {
			t.Fatalf("%s: the acknowledgement never finished; Slack applied %s", row.name, r.front.record())
		}
		// The journal's rows since the last settle, as a set: calls that
		// overlap may be handled in either order, and only what counted is
		// compared; rows of different settles keep their order.
		rows := r.journaled()
		segment := append([]string(nil), rows[journaled:]...)
		journaled = len(rows)
		sort.Strings(segment)
		trace = append(trace, "acts="+strconv.Itoa(r.acts())+" journal=["+strings.Join(segment, ",")+"]")
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
			ts, emoji := a, "eyes"
			if has("B") {
				ts = b
			}
			if has("thumbsup") {
				emoji = "thumbsup"
			}
			var hold *heldAnswer
			if has("held") {
				hold = r.arm(f[0])
				holds[f[1]] = hold
			}
			c := &call{id: f[1], body: new(string)}
			c.done = r.send(ts, emoji, f[0] == "remove", c.body)
			calls = append(calls, c)
			byID[c.id] = c
			switch {
			case hold != nil:
				if !waitDone(hold.applied, 10*time.Second) {
					t.Fatalf("%s: the call %s never reached Slack; Slack applied %s", row.name, c.id, r.front.record())
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
				r.human(a)
				continue
			}
			hold := r.arm("add")
			holds["ack"] = hold
			ackDone = make(chan struct{})
			go func() { defer close(ackDone); r.human(a) }()
			if !waitDone(hold.applied, 10*time.Second) {
				t.Fatalf("%s: the acknowledgement's add never reached Slack; Slack applied %s", row.name, r.front.record())
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
	r.cfg.arrival.Wait()
	for _, c := range calls {
		trace = append(trace, c.id+"="+*c.body)
	}
	want := row.slackOff
	if mode != "" {
		want = row.slackAck
	}
	if got := r.front.record(); got != want {
		t.Errorf("%s (ACK_REACTION=%q): Slack applied %s, want %s: the row did not reach its order", row.name, mode, got, want)
	}
	return trace
}

func heldSlackRows() []heldRow {
	return []heldRow{
		{
			// Case 1, the gate's sequence: the session's removal is applied
			// and its answer is pending; the acknowledgement places eyes; the
			// session's eyes again.
			name:     "case1/removal-applied-answer-pending",
			steps:    "add s1; remove r1 held; ack; add s2; release r1; settle",
			slackAck: "add:ok | remove:ok | add:ok | add:already_reacted",
			slackOff: "add:ok | remove:ok | add:ok",
			red:      true,
		},
		{
			// Case 3: the session's add and removal overlap; Slack applies the
			// add first, the adapter handles the removal's answer first.
			name:     "case3/add-and-removal-answered-in-the-other-order",
			steps:    "add s1 held; remove r1; release s1; settle; ack; add s2",
			slackAck: "add:ok | remove:ok | add:ok | add:already_reacted",
			slackOff: "add:ok | remove:ok | add:ok",
			red:      true,
		},
		{
			// Case 4, the mirror of case 1 once the held answer lands: a later
			// repeat must count nothing, as with ACK_REACTION unset.
			name:     "case4/repeat-after-the-held-removal-lands",
			steps:    "add s1; remove r1 held; ack; add s2; release r1; settle; add s3",
			slackAck: "add:ok | remove:ok | add:ok | add:already_reacted | add:already_reacted",
			slackOff: "add:ok | remove:ok | add:ok | add:already_reacted",
			red:      true,
		},
		{
			// The class's over-count direction: the acknowledgement's record
			// stands with no reaction under it (the session removed the
			// acknowledgement's eyes); two identical session adds overlap and
			// the second's already_reacted is handled before the first's ok.
			name:     "overlap/identical-adds-answered-in-the-other-order",
			steps:    "ack; remove r0; add s1 held; add s2; release s1",
			slackAck: "add:ok | remove:ok | add:ok | add:already_reacted",
			slackOff: "remove:no_reaction | add:ok | add:already_reacted",
			red:      true,
		},
		{
			// The same direction with the acknowledgement's already_reacted
			// still in flight: its record stands while the session's first ok
			// is unhandled.
			name:     "overlap/session-add-pending-while-ack-answer-pending",
			steps:    "add s1 held; ack held; add s2; release s1; release ack",
			slackAck: "add:ok | add:already_reacted | add:already_reacted",
			slackOff: "add:ok | add:already_reacted",
			red:      true,
		},
		{
			// GUARD: the acknowledgement's call never holds up the session's.
			name:     "guard/session-add-while-ack-answer-held",
			steps:    "ack held; add s1 prompt; release ack; add s2",
			slackAck: "add:ok | add:already_reacted | add:already_reacted",
			slackOff: "add:ok | add:already_reacted",
		},
		{
			// GUARD: a held call holds up no other emoji and no other message.
			name:     "guard/other-emoji-and-message-not-held",
			steps:    "add s1; remove r1 held; add t1 thumbsup prompt; add b1 B prompt; release r1; ack",
			slackAck: "add:ok | remove:ok | add:ok | add:ok | add:ok",
			slackOff: "add:ok | remove:ok | add:ok | add:ok",
		},
	}
}

func TestHeldAnswerParityWithAckOff(t *testing.T) {
	for _, row := range heldSlackRows() {
		t.Run(row.name, func(t *testing.T) {
			off := newHeldSlackRig(t).run(row, "")
			on := newHeldSlackRig(t).run(row, "eyes")
			t.Logf("ACK_REACTION unset: %s", strings.Join(off, " ; "))
			t.Logf("ACK_REACTION=eyes:  %s", strings.Join(on, " ; "))
			if strings.Join(on, "\n") != strings.Join(off, "\n") {
				t.Errorf("with ACK_REACTION=eyes the row's trace differs from the same row with it unset:\n on: %s\noff: %s", strings.Join(on, " ; "), strings.Join(off, " ; "))
			}
		})
	}
}

// The lock's bound: a removal whose answer never comes holds its reaction only
// until the call's own timeout (slackAPIClient's, shortened here); the
// session's next call on that reaction then goes through while the answer is
// still held.
func TestReactVerbHoldEndsWithTheCallsTimeout(t *testing.T) {
	r := newHeldSlackRig(t)
	r.start("eyes")
	a := slackTimestamp(r.now)
	if receipt := r.react(a, "eyes"); !receipt.Delivered {
		t.Fatalf("the session's first eyes: receipt=%+v", receipt)
	}
	prev := slackAPIClient
	slackAPIClient = &http.Client{Timeout: 300 * time.Millisecond}
	t.Cleanup(func() { slackAPIClient = prev })
	hold := r.arm("remove")
	var removal, next string
	removed := r.send(a, "eyes", true, &removal)
	if !waitDone(hold.applied, 10*time.Second) {
		t.Fatal("the removal never reached Slack")
	}
	start := time.Now()
	nextDone := r.send(a, "eyes", false, &next)
	if !waitDone(nextDone, 5*time.Second) {
		t.Fatalf("the session's next eyes was still waiting %s after the removal's call timed out", 5*time.Second)
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
	t.Logf("removal %s; next eyes %s after %s; Slack applied %s", removal, next, waited.Round(time.Millisecond), r.front.record())
	if removal != `{"delivered":false,"failure_kind":"transient"}` {
		t.Errorf("removal receipt %s, want the transient failure of a call with no answer", removal)
	}
	if next != `{"delivered":true}` {
		t.Errorf("next eyes receipt %s, want delivered", next)
	}
}
