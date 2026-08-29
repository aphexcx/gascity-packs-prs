package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- receipt follow-up after a HELD pending (gp-3yg) -------------------------
//
// Every test here starts from the state the mayor's pending=HOLD ruling
// leaves behind: gc answered "pending", the claim concluded, nothing was
// re-posted. The follow-up is what happens NEXT — the adapter asking gc
// afterwards whether the background send ever landed, and acting on a
// definite answer instead of leaving a HELD log line as the only trace.

// scriptedPoll answers successive polls for one receipt id from a script;
// the last entry repeats once the script runs out.
type scriptedPoll struct {
	mu     sync.Mutex
	script map[string][]receiptPollAnswer
	calls  map[string]int
}

type receiptPollAnswer struct {
	state   receiptPollState
	receipt deliveryReceipt
	err     error
}

func newScriptedPoll() *scriptedPoll {
	return &scriptedPoll{script: map[string][]receiptPollAnswer{}, calls: map[string]int{}}
}

func (p *scriptedPoll) on(id string, answers ...receiptPollAnswer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.script[id] = answers
}

func (p *scriptedPoll) poll(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	answers := p.script[id]
	n := p.calls[id]
	p.calls[id] = n + 1
	if len(answers) == 0 {
		return receiptPollUnavailable, deliveryReceipt{}, nil
	}
	if n >= len(answers) {
		n = len(answers) - 1
	}
	a := answers[n]
	return a.state, a.receipt, a.err
}

func (p *scriptedPoll) count(id string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[id]
}

func concludedReceipt(id, status string, delivered, expected int) deliveryReceipt {
	return deliveryReceipt{present: true, id: id, status: status,
		delivered: delivered, deliveredOK: true, deliveredUnit: "bytes",
		expected: expected, expectedOK: true, expectedUnit: "bytes"}
}

func pendingReceipt(id string) deliveryReceipt {
	return deliveryReceipt{present: true, id: id, status: "pending"}
}

// followUpHarness records what the follow-up did with the leg's hooks.
type followUpHarness struct {
	mu          sync.Mutex
	reposts     int
	repostAns   []receiptPollAnswer // receipt+err returned by successive re-posts
	deadLetters []error
}

func (h *followUpHarness) repost() (deliveryReceipt, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.reposts
	h.reposts++
	if n >= len(h.repostAns) {
		return concludedReceipt("rcpt_repost", "delivered", 100, 100), nil
	}
	return h.repostAns[n].receipt, h.repostAns[n].err
}

func (h *followUpHarness) deadLetter(cause error) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.deadLetters = append(h.deadLetters, cause)
	return true
}

func (h *followUpHarness) snapshot() (int, []error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reposts, append([]error(nil), h.deadLetters...)
}

func (h *followUpHarness) held(f *receiptFollowUps, receiptID string) {
	f.note(heldDelivery{
		receiptID:  receiptID,
		leg:        "test",
		channel:    "C1",
		ts:         "1787901226.718729",
		repost:     h.repost,
		deadLetter: h.deadLetter,
	})
}

func testFollowUps(poll func(context.Context, string) (receiptPollState, deliveryReceipt, error), draining *atomic.Bool) *receiptFollowUps {
	f := newReceiptFollowUps(poll, true, draining)
	f.interval = 5 * time.Millisecond
	f.deadline = 2 * time.Second
	return f
}

// pending → landed late. gc concludes delivered after the adapter held:
// the follow-up must conclude quietly — no re-post (a duplicate on a
// busy session is the failure the HOLD ruling exists to prevent), no
// dead letter.
func TestReceiptFollowUp_PendingThenLandedLateConcludesWithoutRepost(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p",
		receiptPollAnswer{state: receiptPollPending},
		receiptPollAnswer{state: receiptPollPending},
		receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "delivered", 790, 790)},
	)
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) after a late-landed delivery — that is the duplicate the HOLD ruling forbids", reposts)
	}
	if len(dead) != 0 {
		t.Fatalf("dead-lettered a delivered message: %v", dead)
	}
	if n := poll.count("rcpt_p"); n < 3 {
		t.Fatalf("polled %d time(s), want at least 3 (kept asking while pending)", n)
	}
}

// pending → lost. gc concludes that nothing landed: the follow-up re-posts
// the SAME envelope once (gc dedups the transcript by provider message id)
// and, when that copy is vouched for, the message is recovered — no dead
// letter, because the agent has it.
func TestReceiptFollowUp_PendingThenLostRepostsAndRecovers(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p",
		receiptPollAnswer{state: receiptPollPending},
		receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)},
	)
	h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: concludedReceipt("rcpt_r", "delivered", 790, 790)}}}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 {
		t.Fatalf("re-posted %d time(s), want exactly 1 recovery copy", reposts)
	}
	if len(dead) != 0 {
		t.Fatalf("dead-lettered a recovered message: %v — a dead letter must mean the agent did NOT get it", dead)
	}
}

// The recovery copy itself fails: now the loss stands, and it must be
// durable — dead letter, not just a log line.
func TestReceiptFollowUp_LostAndRepostUnvouchedDeadLetters(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: concludedReceipt("rcpt_r", "partial", 41, 790)}}}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 {
		t.Fatalf("re-posted %d time(s), want 1", reposts)
	}
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1 after the recovery copy was not vouched for", len(dead))
	}
	if !errors.Is(dead[0], errReceiptFollowUpLost) {
		t.Fatalf("dead-letter cause = %v, want errReceiptFollowUpLost", dead[0])
	}
}

// A recovery copy that itself comes back pending is followed up ONCE more
// (the session may still be busy); if that second receipt is also lost,
// the ladder ends in the dead letter instead of looping.
func TestReceiptFollowUp_RepostHeldIsFollowedOnceThenDeadLetters(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	poll.on("rcpt_r", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_r", "failed", 0, 790)})
	h := &followUpHarness{repostAns: []receiptPollAnswer{
		{receipt: pendingReceipt("rcpt_r")},
		{receipt: pendingReceipt("rcpt_never")},
	}}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 {
		t.Fatalf("re-posted %d time(s), want 1: the ladder allows one recovery copy, then dead-letters", reposts)
	}
	if poll.count("rcpt_r") == 0 {
		t.Fatal("the held recovery copy was never followed up")
	}
	if len(dead) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(dead))
	}
}

// gc holds no record of the receipt (it restarted, taking the fan-out with
// it): nobody is still trying, so this is a definite loss and recovers the
// same way a concluded failure does.
func TestReceiptFollowUp_UnknownReceiptIsDefiniteLoss(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollUnknown})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 || len(dead) != 0 {
		t.Fatalf("reposts=%d dead=%d, want 1 recovery copy and no dead letter once it is vouched", reposts, len(dead))
	}
}

// A gc that does not offer the poll endpoint (pre-gp-3yg) must fail OPEN:
// no re-post, no dead letter — exactly the HOLD ruling's behavior, so a
// pack pinned ahead of the gc rebuild does not start duplicating.
func TestReceiptFollowUp_UnavailableEndpointFailsOpen(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollUnavailable})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 40 * time.Millisecond // unavailable is retried until the deadline
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 || len(dead) != 0 {
		t.Fatalf("reposts=%d dead=%d on a gc without the endpoint, want 0/0 (fail open)", reposts, len(dead))
	}
}

// A poll that cannot reach gc at all is retried on the next tick, not
// read as an answer.
func TestReceiptFollowUp_TransportErrorRetriesThenResolves(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p",
		receiptPollAnswer{err: errors.New("connection refused")},
		receiptPollAnswer{err: errors.New("connection refused")},
		receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "delivered", 10, 10)},
	)
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 || len(dead) != 0 {
		t.Fatalf("reposts=%d dead=%d, want 0/0 after transient poll errors resolved to delivered", reposts, len(dead))
	}
}

// Still pending at the deadline: not a definite loss, so NOT a re-post
// (which could duplicate a send that is merely slow) — but not silence
// either. The dead letter says the state is unknown and needs a hand check.
func TestReceiptFollowUp_StuckPendingPastDeadlineDeadLettersWithoutRepost(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollPending})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 40 * time.Millisecond
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) on a still-pending receipt — a slow send is not a lost one", reposts)
	}
	if len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters = %v, want exactly one errReceiptFollowUpStuck record", dead)
	}
}

// Shutdown while a follow-up is outstanding: one last poll, then the
// unresolved state is made durable before the process exits. No re-post
// during the drain (the same rule the in-place re-post already follows).
func TestReceiptFollowUp_DrainFinalPollThenDeadLettersUnresolved(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollPending})
	h := &followUpHarness{}
	draining := &atomic.Bool{}
	f := testFollowUps(poll.poll, draining)
	f.interval = 20 * time.Millisecond
	h.held(f, "rcpt_p")
	time.Sleep(30 * time.Millisecond)
	draining.Store(true)
	if !f.drain(2 * time.Second) {
		t.Fatal("follow-ups did not drain")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) during the drain", reposts)
	}
	if len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters = %v, want one unresolved-at-shutdown record", dead)
	}
}

// A definite loss discovered during the drain still becomes durable, just
// without the re-post.
func TestReceiptFollowUp_LossDuringDrainDeadLettersWithoutRepost(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	h := &followUpHarness{}
	draining := &atomic.Bool{}
	draining.Store(true)
	f := testFollowUps(poll.poll, draining)
	h.held(f, "rcpt_p")
	if !f.drain(2 * time.Second) {
		t.Fatal("follow-ups did not drain")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 || len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpLost) {
		t.Fatalf("reposts=%d dead=%v, want no re-post and one lost record", reposts, dead)
	}
}

// The same receipt noted twice (a leg retried its bookkeeping) is followed
// up once.
func TestReceiptFollowUp_DuplicateNoteIsFollowedOnce(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	if reposts, _ := h.snapshot(); reposts != 1 {
		t.Fatalf("re-posted %d time(s) for one receipt noted twice, want 1", reposts)
	}
}

// A nil component (bare test configs, or a build without one) is a no-op.
func TestReceiptFollowUp_NilIsNoop(t *testing.T) {
	var f *receiptFollowUps
	f.note(heldDelivery{receiptID: "x"})
	if !f.wait(time.Millisecond) || !f.drain(time.Millisecond) {
		t.Fatal("nil follow-ups must report drained")
	}
}

// --- wire parsing -------------------------------------------------------------

func TestParseReceiptPoll_States(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
		want   receiptPollState
	}{
		"pending":              {200, `{"receipt_id":"ir-1-2","state":"pending"}`, receiptPollPending},
		"concluded":            {200, `{"receipt_id":"ir-1-2","state":"concluded","delivery":{"receipt_id":"ir-1-2","status":"delivered","delivered_bytes":790,"expected_bytes":790}}`, receiptPollConcluded},
		"unknown":              {200, `{"receipt_id":"ir-1-2","state":"unknown"}`, receiptPollUnknown},
		"go-style keys":        {200, `{"ReceiptID":"ir-1-2","State":"Concluded","Delivery":{"ReceiptID":"ir-1-2","Status":"failed","DeliveredBytes":0,"ExpectedBytes":790}}`, receiptPollConcluded},
		"route missing (404)":  {404, `{"title":"Not Found"}`, receiptPollUnavailable},
		"gc down (503)":        {503, ``, receiptPollUnavailable},
		"not json":             {200, `<html>`, receiptPollUnavailable},
		"no state field":       {200, `{"receipt_id":"ir-1-2"}`, receiptPollUnavailable},
		"unknown state word":   {200, `{"receipt_id":"ir-1-2","state":"exploded"}`, receiptPollUnavailable},
		"concluded no block":   {200, `{"receipt_id":"ir-1-2","state":"concluded"}`, receiptPollUnavailable},
		"concluded null block": {200, `{"receipt_id":"ir-1-2","state":"concluded","delivery":null}`, receiptPollUnavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, receipt := parseReceiptPoll("ir-1-2", tc.status, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("state = %v, want %v", got, tc.want)
			}
			if tc.want == receiptPollConcluded && (!receipt.present || receipt.id != "ir-1-2") {
				t.Fatalf("concluded poll did not carry the delivery block: %+v", receipt)
			}
		})
	}
}

// The poll hits the agreed path with the receipt id escaped, and maps the
// response through parseReceiptPoll.
func TestPollInboundReceipt_HitsAgreedPath(t *testing.T) {
	var gotPath, gotHeader string
	gcSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotHeader = r.Header.Get("X-GC-Request")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"receipt_id":"ir-7/1","state":"pending"}`))
	}))
	t.Cleanup(gcSrv.Close)
	cfg := config{gcAPIBase: gcSrv.URL, cityName: "my city"}
	state, _, err := pollInboundReceipt(context.Background(), cfg, "ir-7/1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if state != receiptPollPending {
		t.Fatalf("state = %v, want pending", state)
	}
	if gotPath != "/v0/city/my%20city/extmsg/inbound/receipts/ir-7%2F1" {
		t.Fatalf("path = %q, want the agreed receipts path with both segments escaped", gotPath)
	}
	if gotHeader != "gc-slack-adapter" {
		t.Fatalf("X-GC-Request = %q", gotHeader)
	}
}

// drain must wake a loop asleep between polls: with a 5s cadence and a
// 10s shutdown budget, waiting out the tick would spend half the budget
// on every outstanding follow-up before its final poll.
func TestReceiptFollowUp_DrainWakesSleepingLoopPromptly(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollPending})
	h := &followUpHarness{}
	draining := &atomic.Bool{}
	f := testFollowUps(poll.poll, draining)
	f.interval = 5 * time.Second
	h.held(f, "rcpt_p")
	time.Sleep(20 * time.Millisecond) // first poll taken; loop now asleep
	draining.Store(true)
	start := time.Now()
	if !f.drain(time.Second) {
		t.Fatal("drain did not complete inside 1s — the sleeping loop was not woken")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("drain took %s, want prompt", took)
	}
	if _, dead := h.snapshot(); len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters = %v, want one unresolved-at-shutdown record", dead)
	}
}

// --- codex r1 findings -------------------------------------------------------

// P1 #1: a poll answer must be about the receipt that was asked for. A
// missing or different receipt_id — a stale proxy answer, a mis-routed
// response — must never read as "unknown" and trigger a re-post of a
// message gc may well have delivered.
func TestParseReceiptPoll_MismatchedOrMissingIDIsUnavailable(t *testing.T) {
	cases := map[string]string{
		"different top-level id":           `{"receipt_id":"ir-9-9","state":"unknown"}`,
		"missing top-level id":             `{"state":"unknown"}`,
		"blank top-level id":               `{"receipt_id":"  ","state":"concluded","delivery":{"receipt_id":"ir-1-2","status":"failed"}}`,
		"different nested id on concluded": `{"receipt_id":"ir-1-2","state":"concluded","delivery":{"receipt_id":"ir-9-9","status":"failed","delivered_bytes":0,"expected_bytes":9}}`,
		"missing nested id on concluded":   `{"receipt_id":"ir-1-2","state":"concluded","delivery":{"status":"failed","delivered_bytes":0,"expected_bytes":9}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if got, _ := parseReceiptPoll("ir-1-2", 200, []byte(body)); got != receiptPollUnavailable {
				t.Fatalf("state = %v, want unavailable (fail open) for %s", got, body)
			}
		})
	}
	// And the matching shapes still parse.
	if got, _ := parseReceiptPoll("ir-1-2", 200, []byte(`{"receipt_id":" ir-1-2 ","state":"unknown"}`)); got != receiptPollUnknown {
		t.Fatalf("matching (whitespace-padded) id: state = %v, want unknown", got)
	}
	if got, r := parseReceiptPoll("ir-1-2", 200, []byte(`{"receipt_id":"ir-1-2","state":"concluded","delivery":{"receipt_id":"ir-1-2","status":"failed","delivered_bytes":0,"expected_bytes":9}}`)); got != receiptPollConcluded || r.id != "ir-1-2" {
		t.Fatalf("matching concluded: state = %v receipt = %+v", got, r)
	}
}

// P1 #3: only a VOUCHED recovery copy is a recovery. A copy gc reports as
// reaching nobody (no_route), or accepts without any receipt, leaves the
// original loss standing — dead letter, not RECOVERED.
func TestReceiptFollowUp_RecoveryNoRouteOrReceiptlessDeadLetters(t *testing.T) {
	for name, recovery := range map[string]deliveryReceipt{
		"no_route":     {present: true, id: "rcpt_r", status: "no_route"},
		"receipt-less": {},
	} {
		t.Run(name, func(t *testing.T) {
			poll := newScriptedPoll()
			poll.on("rcpt_p", receiptPollAnswer{state: receiptPollUnknown})
			h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: recovery}}}
			f := testFollowUps(poll.poll, nil)
			h.held(f, "rcpt_p")
			if !f.wait(2 * time.Second) {
				t.Fatal("follow-up did not finish")
			}
			reposts, dead := h.snapshot()
			if reposts != 1 || len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpLost) {
				t.Fatalf("reposts=%d dead=%v, want one re-post and one standing-loss record", reposts, dead)
			}
		})
	}
}

// P1 #2: a hold noted after the drain has begun cannot be followed (the
// process is leaving). It is made durable synchronously, before note
// returns, so a straggler event goroutine's hold is never lost to exit.
func TestReceiptFollowUp_NoteAfterDrainDeadLettersSynchronously(t *testing.T) {
	poll := newScriptedPoll()
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	if !f.drain(time.Second) {
		t.Fatal("empty drain did not complete")
	}
	h.held(f, "rcpt_late")
	_, dead := h.snapshot()
	if len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters after a post-drain note = %v, want one synchronous unresolved record", dead)
	}
	if poll.count("rcpt_late") != 0 {
		t.Fatal("a post-drain note polled gc — nothing may start after the drain")
	}
}

// P1 #2: a poll in flight when the drain begins is cancelled rather than
// waited out — gc's client deadline is 20s and the shutdown budget is
// shorter — and the hold is still made durable before drain returns.
func TestReceiptFollowUp_DrainCancelsInFlightPoll(t *testing.T) {
	entered := make(chan struct{}, 1)
	blockingPoll := func(ctx context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return receiptPollUnavailable, deliveryReceipt{}, ctx.Err()
	}
	h := &followUpHarness{}
	draining := &atomic.Bool{}
	f := newReceiptFollowUps(blockingPoll, true, draining)
	f.interval = 5 * time.Millisecond
	f.deadline = time.Minute
	h.held(f, "rcpt_p")
	<-entered
	draining.Store(true)
	start := time.Now()
	if !f.drain(time.Second) {
		t.Fatal("drain did not complete — the in-flight poll was not cancelled")
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("drain took %s, want prompt", took)
	}
	if _, dead := h.snapshot(); len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters = %v, want one unresolved-at-shutdown record", dead)
	}
}

// P2 #5: a receipt noted again AFTER its follow-up finished is not
// followed a second time — a second recovery copy would be a duplicate.
func TestReceiptFollowUp_DuplicateNoteAfterCompletionIsIgnored(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("second note did not settle")
	}
	if reposts, _ := h.snapshot(); reposts != 1 {
		t.Fatalf("re-posted %d time(s) for one receipt noted twice, want 1", reposts)
	}
}

// P1 #2 / P3: a dead-letter sink that fails leaves NO durable record, and
// that must be said at LOSS grade — it is the one outcome this file exists
// to make impossible to miss.
func TestReceiptFollowUp_DeadLetterSinkFailureIsLoggedAsLoss(t *testing.T) {
	read, cleanup := captureLog(t)
	defer cleanup()
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollUnknown})
	f := testFollowUps(poll.poll, nil)
	f.note(heldDelivery{
		receiptID:  "rcpt_p",
		leg:        "test",
		channel:    "C1",
		ts:         "1.2",
		repost:     func() (deliveryReceipt, error) { return concludedReceipt("rcpt_r", "failed", 0, 9), nil },
		deadLetter: func(error) bool { return false },
	})
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	if out := read(); !strings.Contains(out, "receipt follow-up: LOSS") || !strings.Contains(out, "NO durable record") {
		t.Fatalf("log = %q, want a LOSS line saying there is no durable record", out)
	}
}

// P2 #6: every gc-supplied string that reaches a log line — receipt ids,
// error bodies — is sanitized and bounded, so a hostile or huge response
// cannot forge a second line or flood the log.
func TestReceiptFollowUp_LogsSanitizeAndBoundGCStrings(t *testing.T) {
	read, cleanup := captureLog(t)
	defer cleanup()
	poll := newScriptedPoll()
	hostileID := "rcpt\nFORGED-LINE: inbound: LOSS everything"
	poll.on(hostileID, receiptPollAnswer{state: receiptPollUnknown})
	huge := strings.Repeat("x", 1<<20)
	f := testFollowUps(poll.poll, nil)
	f.note(heldDelivery{
		receiptID: hostileID,
		leg:       "test",
		channel:   "C1",
		ts:        "1.2",
		repost: func() (deliveryReceipt, error) {
			return deliveryReceipt{}, &inboundPostError{Status: 500, StatusText: "500 Internal", Body: huge}
		},
		deadLetter: func(error) bool { return true },
	})
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	out := read()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "FORGED-LINE") {
			t.Fatalf("a gc-supplied receipt id forged a log line: %q", line)
		}
		if len(line) > 4096 {
			t.Fatalf("a log line carried %d bytes of gc response body", len(line))
		}
	}
}

// --- codex r2 findings -------------------------------------------------------

// P1 #1: a HELD recovery copy is still a recovery: when its own follow-up
// concludes anything but vouched — reaching nobody, a gc that stopped
// answering, a receipt this adapter cannot read — the ORIGINAL loss still
// stands and must be recorded, not logged as resolved.
func TestReceiptFollowUp_HeldRecoveryOnlyClearsWhenVouched(t *testing.T) {
	for name, answer := range map[string]receiptPollAnswer{
		"no_route":    {state: receiptPollConcluded, receipt: deliveryReceipt{present: true, id: "rcpt_r", status: "no_route"}},
		"unavailable": {state: receiptPollUnavailable},
		"unreadable":  {state: receiptPollConcluded, receipt: deliveryReceipt{present: true, id: "rcpt_r", status: "exploded"}},
	} {
		t.Run(name, func(t *testing.T) {
			poll := newScriptedPoll()
			poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
			poll.on("rcpt_r", answer)
			h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: pendingReceipt("rcpt_r")}}}
			f := testFollowUps(poll.poll, nil)
			f.deadline = 40 * time.Millisecond // unavailable is retried until the deadline
			h.held(f, "rcpt_p")
			if !f.wait(2 * time.Second) {
				t.Fatal("follow-up did not finish")
			}
			reposts, dead := h.snapshot()
			if reposts != 1 || len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpLost) {
				t.Fatalf("reposts=%d dead=%v, want the standing loss recorded after the held recovery's %s outcome", reposts, dead, name)
			}
		})
	}
	// And the one outcome that does clear it.
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	poll.on("rcpt_r", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_r", "delivered", 790, 790)})
	h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: pendingReceipt("rcpt_r")}}}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	if _, dead := h.snapshot(); len(dead) != 0 {
		t.Fatalf("a held recovery that landed was dead-lettered: %v", dead)
	}
}

// P2 #3: a receipt gc calls pending but gives no id cannot be followed —
// and the leg has already concluded its claim on it. That is an
// untrackable hold and must be recorded, not silently dropped.
func TestReceiptFollowUp_BlankReceiptIDIsRecordedNotDropped(t *testing.T) {
	for _, id := range []string{"", "   ", "\t\n"} {
		poll := newScriptedPoll()
		h := &followUpHarness{}
		f := testFollowUps(poll.poll, nil)
		h.held(f, id)
		if !f.wait(time.Second) {
			t.Fatal("blank-id note did not settle")
		}
		_, dead := h.snapshot()
		if len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
			t.Fatalf("id %q: dead letters = %v, want one untrackable-hold record", id, dead)
		}
	}
}

// P1 #2: a hold noted after the drain closed is written synchronously, and
// its LOSS-grade line is emitted BEFORE the write begins, so a process
// exit that cuts the write short cannot erase both the record and the
// alarm.
func TestReceiptFollowUp_PostDrainNoteLogsLossBeforeWriting(t *testing.T) {
	read, cleanup := captureLog(t)
	defer cleanup()
	poll := newScriptedPoll()
	f := testFollowUps(poll.poll, nil)
	if !f.drain(time.Second) {
		t.Fatal("empty drain did not complete")
	}
	var seenAtWrite string
	f.note(heldDelivery{
		receiptID: "rcpt_late",
		leg:       "test",
		channel:   "C1",
		ts:        "1.2",
		deadLetter: func(error) bool {
			seenAtWrite = read()
			return true
		},
	})
	if !strings.Contains(seenAtWrite, "receipt follow-up: LOSS") || !strings.Contains(seenAtWrite, "rcpt_late") {
		t.Fatalf("log at write time = %q, want a LOSS line naming the receipt before the write started", seenAtWrite)
	}
}

// --- codex r3 finding + core r3 gap ------------------------------------------

// P1: a hold noted after the drain closed, while the drain is still waiting
// on an earlier follow-up, must be JOINED by that drain — its record is
// being written on the caller's goroutine and shutdown must not proceed
// past it. (The WaitGroup this replaced could also panic here: Add after
// Wait had begun returning; that race is removed by construction and is
// not reproducible deterministically.)
func TestReceiptFollowUp_PostCloseStragglerIsJoinedByDrain(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	// Deliberately ignores ctx: this poll stands for a gc round-trip the
	// drain cannot cut short, so the first follow-up is still in flight
	// while the straggler arrives.
	blockingPoll := func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return receiptPollPending, deliveryReceipt{}, nil
	}
	h := &followUpHarness{}
	draining := &atomic.Bool{}
	f := newReceiptFollowUps(blockingPoll, true, draining)
	f.interval = 5 * time.Millisecond
	f.deadline = time.Minute
	h.held(f, "rcpt_first")
	<-entered
	draining.Store(true)

	drained := make(chan bool, 1)
	go func() { drained <- f.drain(2 * time.Second) }()
	// The drain is now closed and waiting on rcpt_first. A straggler
	// arrives; its synchronous record takes a moment.
	time.Sleep(20 * time.Millisecond)
	var stragglerWritten atomic.Bool
	stragglerDone := make(chan struct{})
	go func() {
		defer close(stragglerDone)
		f.note(heldDelivery{
			receiptID: "rcpt_straggler",
			leg:       "test",
			channel:   "C1",
			ts:        "1.2",
			deadLetter: func(error) bool {
				time.Sleep(60 * time.Millisecond)
				stragglerWritten.Store(true)
				return true
			},
		})
	}()
	time.Sleep(10 * time.Millisecond)
	close(release) // rcpt_first finishes; the drain must still wait for the straggler
	ok := <-drained
	if !ok {
		t.Fatal("drain did not complete")
	}
	if !stragglerWritten.Load() {
		t.Fatal("drain returned before the post-close straggler's record was written")
	}
	<-stragglerDone
}

// Core r3 gap: gc answers 404 for the receipt route while a city's Server
// is being replaced (and during a gc restart). "Unavailable" is therefore
// transient until proven otherwise: the follow-up keeps asking until the
// deadline, and a later definite answer is acted on.
func TestReceiptFollowUp_TransientUnavailableIsRetriedUntilDeadline(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p",
		receiptPollAnswer{state: receiptPollUnavailable},
		receiptPollAnswer{state: receiptPollUnavailable},
		receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)},
	)
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 || len(dead) != 0 {
		t.Fatalf("reposts=%d dead=%d, want the loss acted on once gc answered again (transient 404s must not end the follow-up)", reposts, len(dead))
	}
}

// And a recovery copy whose receipt is unavailable until the deadline is
// a standing loss, recorded — not an UNVERIFIED log line.
func TestReceiptFollowUp_HeldRecoveryUnavailableUntilDeadlineDeadLetters(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollConcluded, receipt: concludedReceipt("rcpt_p", "failed", 0, 790)})
	poll.on("rcpt_r", receiptPollAnswer{state: receiptPollUnavailable})
	h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: pendingReceipt("rcpt_r")}}}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 40 * time.Millisecond
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	if _, dead := h.snapshot(); len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpLost) {
		t.Fatalf("dead letters = %v, want the standing loss recorded", dead)
	}
}

// --- codex r4 findings -------------------------------------------------------

// P1 #2: a blank-id record is in-flight work like any other: a drain
// waiting on an ordinary follow-up must not return while that write is
// still running.
func TestReceiptFollowUp_BlankIDRecordIsJoinedByDrain(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	blockingPoll := func(_ context.Context, id string) (receiptPollState, deliveryReceipt, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return receiptPollPending, deliveryReceipt{}, nil
	}
	h := &followUpHarness{}
	draining := &atomic.Bool{}
	f := newReceiptFollowUps(blockingPoll, true, draining)
	f.interval = 5 * time.Millisecond
	f.deadline = time.Minute
	h.held(f, "rcpt_first")
	<-entered

	var blankWritten atomic.Bool
	blankDone := make(chan struct{})
	go func() {
		defer close(blankDone)
		f.note(heldDelivery{
			leg: "test", channel: "C1", ts: "1.2",
			deadLetter: func(error) bool {
				time.Sleep(60 * time.Millisecond)
				blankWritten.Store(true)
				return true
			},
		})
	}()
	time.Sleep(10 * time.Millisecond) // the blank-id write is under way
	draining.Store(true)
	close(release) // rcpt_first finishes at once; the drain must still wait for the blank-id write
	if !f.drain(2 * time.Second) {
		t.Fatal("drain did not complete")
	}
	if !blankWritten.Load() {
		t.Fatal("drain returned before the blank-id record was written")
	}
	<-blankDone
}

// P1 #3: a hold gc once answered "pending" for — proof the endpoint exists
// — that then gets only unavailable answers until the deadline (gc
// restarted, and the restart may have lost the fan-out) is recorded as
// unknown-state, never failed open as if the endpoint had never existed.
func TestReceiptFollowUp_UnavailableAfterUsableAnswerIsRecordedNotFailedOpen(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p",
		receiptPollAnswer{state: receiptPollPending},
		receiptPollAnswer{state: receiptPollUnavailable},
	)
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 40 * time.Millisecond
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) — unavailable is never a loss claim", reposts)
	}
	if len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters = %v, want one unknown-state record (the endpoint had answered before)", dead)
	}
}

// P3: post-close records dedupe like followed ones — the same receipt
// noted twice after the drain closed is written once.
func TestReceiptFollowUp_PostCloseDuplicateNoteIsRecordedOnce(t *testing.T) {
	poll := newScriptedPoll()
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	if !f.drain(time.Second) {
		t.Fatal("empty drain did not complete")
	}
	h.held(f, "rcpt_late")
	h.held(f, "rcpt_late")
	if _, dead := h.snapshot(); len(dead) != 1 {
		t.Fatalf("dead letters = %d for one receipt noted twice after the drain, want 1", len(dead))
	}
}

// The 2026-08-28 08:24Z shape under the gp-2io classification: gc
// CONCLUDED the fan-out, and its last word is a member with status
// pending whose byte counts prove the complete payload is in the pane —
// only the submit went unobserved (a fast turn outruns the busy probe).
// The follow-up must resolve quietly: no re-post (the payload is in the
// pane; a duplicate is the failure the HOLD ruling forbids) and no dead
// letter (a "verify by hand" record for every probe miss is exactly the
// false-alarm noise incident 2 produced).
func TestReceiptFollowUp_ConcludedLandedUnconfirmedResolvesQuietly(t *testing.T) {
	poll := newScriptedPoll()
	landed := concludedReceipt("rcpt_lu", "pending", 1215, 1215)
	landed.members = []deliveryReceiptMember{{
		SessionID: "sess-mayor", Status: "pending",
		Delivered: 1215, DeliveredOK: true, DeliveredUnit: "bytes",
		Expected: 1215, ExpectedOK: true, ExpectedUnit: "bytes",
		Error: "nudge: submit Enter delivered to tmux but not confirmed (busy state never observed)",
	}}
	poll.on("rcpt_lu",
		receiptPollAnswer{state: receiptPollPending},
		receiptPollAnswer{state: receiptPollConcluded, receipt: landed},
	)
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 200 * time.Millisecond
	h.held(f, "rcpt_lu")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) for a payload gc proved is in the pane — that duplicate is the 08:24Z incident again", reposts)
	}
	if len(dead) != 0 {
		t.Fatalf("dead-lettered a landed-but-unconfirmed delivery: %v — dead letters must mean the agent did not get it", dead)
	}
}

// A recovery copy that lands whole with only the submit unconfirmed
// clears the loss the same way a vouched one does: the payload is in the
// pane. No dead letter.
func TestReceiptFollowUp_RecoveryLandedUnconfirmedClears(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollUnknown})
	landed := concludedReceipt("rcpt_r", "pending", 900, 900)
	landed.members = []deliveryReceiptMember{{
		SessionID: "sess-1", Status: "pending",
		Delivered: 900, DeliveredOK: true, DeliveredUnit: "bytes",
		Expected: 900, ExpectedOK: true, ExpectedUnit: "bytes",
	}}
	poll.on("rcpt_r", receiptPollAnswer{state: receiptPollConcluded, receipt: landed})
	h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: pendingReceipt("rcpt_r")}}}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 200 * time.Millisecond
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 {
		t.Fatalf("re-posted %d time(s), want exactly 1", reposts)
	}
	if len(dead) != 0 {
		t.Fatalf("dead-lettered a recovery copy gc proved is in the pane: %v", dead)
	}
}

// Concluded, still pending, and NO landing evidence (a pending member
// without byte counts): gc finished trying and cannot say the payload
// reached the pane. Not a loss claim either — the deadline records it as
// unknown-state, and nothing is re-posted.
func TestReceiptFollowUp_ConcludedPendingWithoutLandingEvidenceStucks(t *testing.T) {
	poll := newScriptedPoll()
	bare := pendingReceipt("rcpt_np")
	bare.members = []deliveryReceiptMember{{SessionID: "sess-1", Status: "pending"}}
	poll.on("rcpt_np", receiptPollAnswer{state: receiptPollConcluded, receipt: bare})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 40 * time.Millisecond
	h.held(f, "rcpt_np")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) without landing evidence going the other way — pending is a HOLD", reposts)
	}
	if len(dead) != 1 || !errors.Is(dead[0], errReceiptFollowUpStuck) {
		t.Fatalf("dead letters = %v, want exactly one unknown-state record", dead)
	}
}

// The spec for landedUnconfirmed: one row per shape the adversary can
// move an operand of. Only a pending or partial summary, with complete,
// byte-unit, per-member evidence on every member — and at least one
// pending member — reads as the gp-2io landed-but-unconfirmed shape.
func TestDeliveryReceipt_LandedUnconfirmedTable(t *testing.T) {
	landed := func(status string, delivered, expected int) deliveryReceiptMember {
		return deliveryReceiptMember{SessionID: "s", Status: status,
			Delivered: delivered, DeliveredOK: true, DeliveredUnit: "bytes",
			Expected: expected, ExpectedOK: true, ExpectedUnit: "bytes"}
	}
	rows := []struct {
		name    string
		status  string
		members []deliveryReceiptMember
		want    bool
	}{
		{"no members: no evidence", "pending", nil, false},
		{"one pending, counts whole", "pending", []deliveryReceiptMember{landed("pending", 1215, 1215)}, true},
		{"pending overshoot (runtime counted more than gc built)", "pending", []deliveryReceiptMember{landed("pending", 1217, 1215)}, true},
		{"pending short: not landed", "pending", []deliveryReceiptMember{landed("pending", 41, 1215)}, false},
		{"pending, no counts: not evidence", "pending", []deliveryReceiptMember{{SessionID: "s", Status: "pending"}}, false},
		{"pending, delivered count only", "pending", []deliveryReceiptMember{{SessionID: "s", Status: "pending", Delivered: 10, DeliveredOK: true, DeliveredUnit: "bytes"}}, false},
		{"pending, unit mismatch", "pending", []deliveryReceiptMember{{SessionID: "s", Status: "pending", Delivered: 10, DeliveredOK: true, DeliveredUnit: "bytes", Expected: 10, ExpectedOK: true, ExpectedUnit: "chars"}}, false},
		{"pending, chars counts: outside the byte contract", "pending", []deliveryReceiptMember{{SessionID: "s", Status: "pending", Delivered: 10, DeliveredOK: true, DeliveredUnit: "chars", Expected: 10, ExpectedOK: true, ExpectedUnit: "chars"}}, false},
		{"pending, unlabeled counts: outside the byte contract", "pending", []deliveryReceiptMember{{SessionID: "s", Status: "pending", Delivered: 10, DeliveredOK: true, Expected: 10, ExpectedOK: true}}, false},
		{"pending, expected zero", "pending", []deliveryReceiptMember{landed("pending", 0, 0)}, false},
		{"partial summary, delivered + pending-landed mix", "partial", []deliveryReceiptMember{landed("delivered", 10, 10), landed("pending", 20, 20)}, true},
		{"failed summary outvotes landed members", "failed", []deliveryReceiptMember{landed("pending", 20, 20)}, false},
		{"no_route summary outvotes landed members", "no_route", []deliveryReceiptMember{landed("pending", 20, 20)}, false},
		{"unknown summary outvotes landed members", "mystery", []deliveryReceiptMember{landed("pending", 20, 20)}, false},
		{"delivered summary: no pending member, not this shape", "delivered", []deliveryReceiptMember{landed("delivered", 10, 10)}, false},
		{"pending summary, delivered-only members: not this shape", "pending", []deliveryReceiptMember{landed("delivered", 10, 10)}, false},
		{"pending-landed + unknown status member", "pending", []deliveryReceiptMember{landed("pending", 20, 20), {SessionID: "s2", Status: "mystery"}}, false},
		{"pending-landed + failed member", "pending", []deliveryReceiptMember{landed("pending", 20, 20), landed("failed", 0, 10)}, false},
		{"delivered but short + pending-landed", "partial", []deliveryReceiptMember{landed("delivered", 5, 10), landed("pending", 20, 20)}, false},
	}
	for _, row := range rows {
		r := deliveryReceipt{present: true, id: "rcpt_t", status: row.status, members: row.members}
		if got := r.landedUnconfirmed(); got != row.want {
			t.Errorf("%s: landedUnconfirmed = %v, want %v", row.name, got, row.want)
		}
	}
}

// A mixed room: one member took the payload, the other had it pasted
// whole with the submit unconfirmed. gc SUMMARIZES that as partial — a
// status the verdict reads as "a retry is clean". The per-member
// evidence says otherwise: the payload is in both panes, and a re-post
// would duplicate it into both. Landing evidence outranks the summary.
func TestReceiptFollowUp_ConcludedPartialAllLandedResolvesQuietly(t *testing.T) {
	poll := newScriptedPoll()
	mixed := concludedReceipt("rcpt_mx", "partial", 30, 30)
	mixed.members = []deliveryReceiptMember{
		{SessionID: "sess-a", Status: "delivered",
			Delivered: 10, DeliveredOK: true, DeliveredUnit: "bytes",
			Expected: 10, ExpectedOK: true, ExpectedUnit: "bytes"},
		{SessionID: "sess-b", Status: "pending",
			Delivered: 20, DeliveredOK: true, DeliveredUnit: "bytes",
			Expected: 20, ExpectedOK: true, ExpectedUnit: "bytes"},
	}
	poll.on("rcpt_mx", receiptPollAnswer{state: receiptPollConcluded, receipt: mixed})
	h := &followUpHarness{}
	f := testFollowUps(poll.poll, nil)
	f.deadline = 200 * time.Millisecond
	h.held(f, "rcpt_mx")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 0 {
		t.Fatalf("re-posted %d time(s) into panes that already hold the payload", reposts)
	}
	if len(dead) != 0 {
		t.Fatalf("dead-lettered a delivery that landed everywhere: %v", dead)
	}
}

// The recovery re-post's own synchronous receipt can already be the
// mixed-room landed shape (summary partial, one member delivered whole,
// one pending with whole counts). The verdict alone would read partial
// as a standing loss and dead-letter a payload that is in both panes —
// the classifier must run on the direct re-post response too.
func TestReceiptFollowUp_RecoveryRepostLandedUnconfirmedClears(t *testing.T) {
	poll := newScriptedPoll()
	poll.on("rcpt_p", receiptPollAnswer{state: receiptPollUnknown})
	mixed := concludedReceipt("rcpt_r", "partial", 30, 30)
	mixed.members = []deliveryReceiptMember{
		{SessionID: "sess-a", Status: "delivered",
			Delivered: 10, DeliveredOK: true, DeliveredUnit: "bytes",
			Expected: 10, ExpectedOK: true, ExpectedUnit: "bytes"},
		{SessionID: "sess-b", Status: "pending",
			Delivered: 20, DeliveredOK: true, DeliveredUnit: "bytes",
			Expected: 20, ExpectedOK: true, ExpectedUnit: "bytes"},
	}
	h := &followUpHarness{repostAns: []receiptPollAnswer{{receipt: mixed}}}
	f := testFollowUps(poll.poll, nil)
	h.held(f, "rcpt_p")
	if !f.wait(2 * time.Second) {
		t.Fatal("follow-up did not finish")
	}
	reposts, dead := h.snapshot()
	if reposts != 1 {
		t.Fatalf("re-posted %d time(s), want exactly 1", reposts)
	}
	if len(dead) != 0 {
		t.Fatalf("dead-lettered a recovery copy whose receipt proves both panes hold the payload: %v", dead)
	}
}

// A drain that times out on a blank-id record (an untrackable hold still
// being written) must say so: it is in flight with no receipt id to
// name, and "0 follow-up(s) outstanding" would hide it.
func TestReceiptFollowUp_DrainTimeoutNamesAnonymousRecords(t *testing.T) {
	read, cleanup := captureLog(t)
	defer cleanup()
	poll := newScriptedPoll()
	f := testFollowUps(poll.poll, nil)
	release := make(chan struct{})
	writing := make(chan struct{})
	go f.note(heldDelivery{
		leg: "test", channel: "C1", ts: "1.9",
		deadLetter: func(error) bool {
			close(writing)
			<-release
			return true
		},
	})
	<-writing
	if f.drain(50 * time.Millisecond) {
		t.Fatal("drain reported success while a record was still being written")
	}
	out := read()
	if !strings.Contains(out, "1 follow-up(s) still outstanding") || !strings.Contains(out, "anonymous=1") {
		t.Fatalf("drain-timeout log = %q, want it to count the anonymous in-flight record", out)
	}
	close(release)
	if !f.wait(2 * time.Second) {
		t.Fatal("record writer never finished")
	}
}
