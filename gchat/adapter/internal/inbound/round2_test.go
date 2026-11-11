package inbound

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// Round 2 of PR 88 (bead gp-dl26): the read's probes made durable.

// burstMessage is the i-th message of one space, recorded i-1 seconds after
// retryStart in the tests that use it.
func burstMessage(i int) string { return fmt.Sprintf("%s/messages/burst%d", space, i) }

func handleBurst(t *testing.T, rf *retryFixture, n int) []string {
	t.Helper()
	var names []string
	for i := 1; i <= n; i++ {
		m := message()
		m.Name = burstMessage(i)
		m.CreateTime = fmt.Sprintf("2026-10-22T10:00:%02dZ", i)
		handle(t, rf.fixture, event(fmt.Sprintf("burst-%d", i), "message.created", m))
		names = append(names, m.Name)
		rf.clock.Add(time.Second)
	}
	return names
}

// F2 (ruling B): a pending change blocks nothing. The read's probe: five
// messages of one space to a busy agent, gc answers "pending", each receipt
// concludes "delivered" 35 s after its POST, a poll every 5 s. At d4a95db the
// fifth POST came at +2m20s and two "stuck" mails went out for a burst that
// flowed. Wanted: five POSTs in arrival order within seconds, no mail.
func TestBusyAgentBurstFlowsInArrivalOrder(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(5, wire.Delivery{Status: "pending"})...)
	names := handleBurst(t, rf, 5)
	for elapsed := 5 * time.Second; elapsed <= 3*time.Minute; elapsed += 5 * time.Second {
		rf.clock.Set(retryStart.Add(elapsed))
		for _, name := range names {
			if c := rf.change(t, name); c.Status == "pending" && !rf.clock.Now().Before(c.Posted.Add(35*time.Second)) {
				r := wire.ReceiptResponse{State: "concluded"}
				r.Delivery.Status = "delivered"
				rf.gc.SetReceipt(c.ReceiptID, r)
			}
		}
		rf.poll(t)
	}
	var offsets []string
	late := false
	for _, name := range names {
		c := rf.change(t, name)
		offsets = append(offsets, c.Status+" +"+c.Posted.Sub(retryStart).String())
		late = late || c.Status != "committed" || c.Posted.Sub(retryStart) > 4*time.Second
	}
	if got := postedIDs(rf.fixture); late || !slices.Equal(got, names) || len(rf.raises.list()) != 0 {
		t.Fatalf("burst: POSTs %v at %v, %d mails; want five in arrival order by +4s and no mail", got, offsets, len(rf.raises.list()))
	}
}

// F2 (ruling B), the rewrite of TestPendingChangeHoldsItsSpaceForABoundedTime:
// a later message is POSTed at once behind a pending one, and the pending
// change is still mailed on its own at StuckAfter. The accepted cost, named
// on the card: when the earlier receipt then concludes "failed", gc sees the
// earlier message again after the later one (A, B, A), as adapter 0.5.0 does.
func TestPendingChangeDoesNotHoldItsSpace(t *testing.T) {
	rf := retrySetup(t)
	rf.script(wire.Delivery{Status: "pending"}, wire.Delivery{Status: "pending"})
	handle(t, rf.fixture, event("first", "message.created", message()))
	second := secondMessage()
	rf.clock.Add(time.Second)
	handle(t, rf.fixture, event("second", "message.created", second))
	if got := postedIDs(rf.fixture); !slices.Equal(got, []string{messageName, second.Name}) {
		t.Fatalf("the later message waited behind a pending one: %v", got)
	}
	if c := rf.change(t, second.Name); c.Status != "pending" || c.Cause != "" {
		t.Fatalf("second: %+v", c)
	}
	rf.clock.Set(retryStart.Add(2*time.Minute - time.Second))
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 0 {
		t.Fatalf("raised before StuckAfter: %+v", got)
	}
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 1 || got[0].ID != messageName {
		t.Fatalf("the pending change was not mailed on its own: %+v", got)
	}
	r := wire.ReceiptResponse{State: "concluded"}
	r.Delivery.Status = "delivered"
	rf.gc.SetReceipt(rf.change(t, second.Name).ReceiptID, r)
	r.Delivery = failedWith(draftDeferred)
	rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, r)
	rf.script(wire.Delivery{Status: "delivered"})
	rf.poll(t)
	if got := postedIDs(rf.fixture); !slices.Equal(got, []string{messageName, second.Name, messageName}) {
		t.Fatalf("POSTs %v, want the accepted A, B, A", got)
	}
	for _, name := range []string{messageName, second.Name} {
		if c := rf.change(t, name); c.Status != "committed" {
			t.Fatalf("%s: %+v", name, c)
		}
	}
}

// crashWindow is the record adapter 0.5.0 leaves when it stops between the
// save that puts a fourth "not delivered" back in intent and the save that
// ends it: intent, reason "retry", four POSTs, no cause.
const crashWindow = "spaces/example/messages/crashwindow"

func crashWindowShape(c *ledger.ChangeState) bool {
	return c.Status == "intent" && c.Reason == "retry" && c.Attempts >= firstPOSTs && c.Cause == ""
}

// F4 (ruling C): the read's ledger-file probe. A ledger written by 0.5.0's
// own Save holds the crash-window record. At d4a95db the first poll POSTed it
// a fifth time. Wanted: never POSTed, unresolved as 0.5.0 would have ended it,
// one mail that says gc's last answer is not known.
func TestLedger050CrashWindowRecordIsNeverPosted(t *testing.T) {
	rf := retrySetup(t)
	fixture, err := os.ReadFile("../ledger/testdata/ledger-0.5.0.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rf.opts.LedgerPath, fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	rf.restart(t)
	if c := rf.change(t, crashWindow); !crashWindowShape(&c) {
		t.Fatalf("fixture record: %+v", c)
	}
	r := wire.ReceiptResponse{State: "concluded"}
	r.Delivery.Status = "delivered"
	rf.gc.SetReceipt("ir-1-1", r)
	for range 3 {
		rf.poll(t)
		rf.clock.Add(time.Minute)
	}
	if got := postedIDs(rf.fixture); slices.Contains(got, crashWindow) {
		t.Fatalf("the 0.5.0 crash-window record was POSTed: %v", got)
	}
	c := rf.change(t, crashWindow)
	if c.Status != "unresolved" || c.Reason != "delivery retries exhausted" || c.Cause != "unknown" || rf.l.Unresolved[crashWindow] == nil {
		t.Fatalf("crash-window record: %+v", c)
	}
	var mails []Raise
	for _, x := range rf.raises.list() {
		if x.ID == crashWindow {
			mails = append(mails, x)
		}
	}
	if len(mails) != 1 {
		t.Fatalf("mails for the crash-window record: %d", len(mails))
	}
	if body := mails[0].Body; !strings.Contains(body, "older version") || strings.Contains(body, "partial") {
		t.Fatalf("the mail claims an answer that is not known:\n%s", body)
	}
}

// F4 (ruling C) is complete only if this code never writes the crash-window
// shape itself: a record of this code that is retrying always carries a
// cause. The read's walk: failed four times, gc down, pending, a receipt that
// concludes failed, held, failed, delivered, with a second message behind.
func TestNoSaveWritesTheCrashWindowShape(t *testing.T) {
	saves, bad := 0, 0
	rf := retrySetup(t, func(o *Options) {
		o.Save = func(l *ledger.Ledger) error {
			saves++
			for _, c := range l.Changes {
				if crashWindowShape(c) {
					bad++
				}
			}
			return l.Save(o.LedgerPath)
		}
	})
	rf.script(repeat(4, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("walk", "message.created", message()))
	second := secondMessage()
	rf.clock.Add(time.Second)
	handle(t, rf.fixture, event("walk-second", "message.created", second))
	due := func() { rf.clock.Set(rf.change(t, messageName).NextAttempt) }
	due()
	rf.gc.Configure(func(s *fakegc.Server) { s.CityNotRunning = true })
	if err := rf.p.Poll(context.Background()); err == nil {
		t.Fatal("gc down not reported")
	}
	rf.gc.Configure(func(s *fakegc.Server) { s.CityNotRunning = false })
	rf.script(wire.Delivery{Status: "pending"})
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "pending" {
		t.Fatalf("walk: %+v", c)
	}
	r := wire.ReceiptResponse{State: "concluded", Delivery: failedWith("")}
	rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, r)
	rf.poll(t)
	rf.script(wire.Delivery{Status: "held"}, failedWith(""), wire.Delivery{Status: "delivered"})
	for range 3 {
		due()
		rf.poll(t)
	}
	for _, name := range []string{messageName, second.Name} {
		if c := rf.change(t, name); c.Status != "committed" {
			t.Fatalf("%s at the end of the walk: %+v", name, c)
		}
	}
	if saves < 15 || bad != 0 {
		t.Fatalf("saves %d, of them with the crash-window shape %d", saves, bad)
	}
}

// F5 (ruling F): gc puts "held" above "partial" in its summary, so a fan-out
// with one held member and one partial member answers "held". Each retry can
// paste another fragment into the partial member. At d4a95db the read's
// probe made 24 POSTs in 20 minutes. Wanted: the four-POST bound of "partial".
func TestHeldWithAPartialMemberKeepsThePartialBound(t *testing.T) {
	rf := retrySetup(t)
	heldPartial := wire.Delivery{Status: "held", DeliveredBytes: 40, ExpectedBytes: 240, Members: []wire.DeliveryMember{
		{Status: "held", ExpectedBytes: 120}, {Status: "partial", DeliveredBytes: 40, ExpectedBytes: 120}}}
	rf.script(repeat(40, heldPartial)...)
	handle(t, rf.fixture, event("held-partial", "message.created", message()))
	for range 240 {
		rf.clock.Add(5 * time.Second)
		rf.poll(t)
	}
	c := rf.change(t, messageName)
	if posts := len(rf.gc.Inbound()); posts != 4 || c.Status != "unresolved" || c.Reason != "delivery retries exhausted" {
		t.Fatalf("held with a partial member: %d POSTs in 20 minutes, %s %q after %d", posts, c.Status, c.Reason, c.Attempts)
	}
}

// mailsFor returns the mails sent for one change.
func mailsFor(rf *retryFixture, id string) []Raise {
	var out []Raise
	for _, x := range rf.raises.list() {
		if x.ID == id {
			out = append(out, x)
		}
	}
	return out
}

// F3 (ruling D): the read's probe, one stuck head and three messages behind
// it, gave four mails in one pass at d4a95db. Wanted: one mail, for the head,
// naming the three behind it at that time; not sent again when the count
// changes. Then the head lands and the next one sticks: its own mail. At the
// bound each held-back message is mailed on its own as unresolved, and the
// head, mailed while waiting, gets no second mail for the bound its mail named.
func TestStuckHeadIsMailedOnceNamingTheMessagesBehindIt(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(400, failedWith(draftDeferred))...)
	names := handleBurst(t, rf, 4)
	head, behind := names[0], names[1:]
	// The head's in-gate waits took 7 s: the last is recorded at +10 s.
	if c := rf.change(t, names[3]); !c.Created.Equal(retryStart.Add(10 * time.Second)) {
		t.Fatalf("last recorded at %s", c.Created)
	}
	rf.clock.Set(retryStart.Add(2*time.Minute + 10*time.Second))
	rf.poll(t)
	got := rf.raises.list()
	if len(got) != 1 || got[0].ID != head {
		t.Fatalf("one stuck head and three behind gave %d mails, want one for %s", len(got), head)
	}
	if !strings.Contains(got[0].Body, "Waiting behind it: 3 later messages of this space") || !strings.Contains(got[0].Body, "gc gchat status") {
		t.Fatalf("the head's mail does not name the three behind it:\n%s", got[0].Body)
	}
	m := message()
	m.Name, m.CreateTime = burstMessage(5), "2026-10-22T10:00:05Z"
	handle(t, rf.fixture, event("burst-5", "message.created", m))
	for range 10 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	if got := rf.raises.list(); len(got) != 1 {
		t.Fatalf("mailed again while the head waits or the count changed: %+v", got)
	}

	// The head lands; the next one is refused in turn and is the stuck head.
	rf.script(append([]wire.Delivery{{Status: "delivered"}}, repeat(400, failedWith(draftDeferred))...)...)
	rf.clock.Set(rf.change(t, head).NextAttempt)
	rf.poll(t)
	if c := rf.change(t, head); c.Status != "committed" {
		t.Fatalf("head: %s %q", c.Status, c.Reason)
	}
	rf.poll(t)
	got = mailsFor(rf, behind[0])
	if len(got) != 1 || !strings.Contains(got[0].Body, "Waiting behind it: 3 later messages") || len(rf.raises.list()) != 2 {
		t.Fatalf("the next stuck head was not mailed on its own once: %d mails in all, %+v", len(rf.raises.list()), got)
	}

	// At the bound: the head (behind[0]) ends at the bound its mail named and
	// gets no second mail; each message behind it gets one unresolved mail.
	rf.clock.Set(retryStart.Add(25 * time.Hour))
	for range 3 {
		rf.poll(t)
	}
	if got := mailsFor(rf, behind[0]); len(got) != 1 {
		t.Fatalf("the head was mailed again at the bound its mail named: %d", len(got))
	}
	for _, name := range append(behind[1:], burstMessage(5)) {
		got := mailsFor(rf, name)
		if c := rf.change(t, name); c.Status != "unresolved" || c.Reason != "not delivered within the retry bound" || len(got) != 1 || !strings.Contains(got[0].Body, "stopped trying") {
			t.Fatalf("%s at its bound: %s %q, %d mails", name, c.Status, c.Reason, len(got))
		}
	}
}

// F6 (ruling E): the read's sequence. A message mailed at two minutes as
// waiting went on retrying, got "pending", and its receipt concluded pending
// without member rows: unresolved, and at d4a95db one mail in all, which said
// the adapter keeps trying. Wanted: exactly one second mail that says it has
// ended and will not arrive, once across a restart too.
func TestWaitingMessageThatEndsEarlyGetsOneSecondMail(t *testing.T) {
	for _, restart := range []string{"none", "before the second mail", "after the second mail"} {
		t.Run(restart, func(t *testing.T) {
			rf := retrySetup(t)
			rf.script(repeat(6, failedWith(draftDeferred))...)
			handle(t, rf.fixture, event("ends-early", "message.created", message()))
			rf.clock.Set(retryStart.Add(2 * time.Minute))
			rf.poll(t)
			if got := mailsFor(rf, messageName); len(got) != 1 || !strings.Contains(got[0].Body, "keeps trying") {
				t.Fatalf("waiting mail: %+v", got)
			}
			rf.script(wire.Delivery{Status: "pending"})
			rf.clock.Set(rf.change(t, messageName).NextAttempt)
			rf.poll(t)
			rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "pending"}})
			if restart == "before the second mail" {
				if err := rf.p.Pass(context.Background()); err != nil {
					t.Fatal(err)
				}
				rf.restart(t)
			}
			rf.poll(t)
			if restart == "after the second mail" {
				rf.restart(t)
			}
			for range 5 {
				rf.clock.Add(time.Hour)
				rf.poll(t)
			}
			c := rf.change(t, messageName)
			if c.Status != "unresolved" || c.Reason != "receipt concluded with pending delivery" {
				t.Fatalf("ended: %s %q", c.Status, c.Reason)
			}
			// A restart starts a new mail log: count the mails after the first.
			got := mailsFor(rf, messageName)
			if restart == "none" {
				got = got[1:]
			}
			if len(got) != 1 && restart != "after the second mail" {
				t.Fatalf("second mails: %d, want exactly one", len(got))
			}
			if restart == "after the second mail" {
				if len(got) != 0 {
					t.Fatalf("mailed again after a restart: %d", len(got))
				}
				return
			}
			body := got[0].Body
			if !got[0].Ended || strings.Contains(got[0].Body, "will not arrive") {
				t.Fatalf("second mail: ended %v, claims the message will not arrive:\n%s", got[0].Ended, got[0].Body)
			}
			for _, want := range []string{"stopped trying", "receipt concluded with pending delivery", "earlier mail", "will not deliver it again on its own", "gc gchat resolve " + messageName} {
				if !strings.Contains(body, want) {
					t.Fatalf("second mail lacks %q:\n%s", want, body)
				}
			}
		})
	}
}

// F7 (ruling F): the waiting mail says the message will still arrive, and that
// an agent that acts on it from history must not act on it again when it does.
func TestWaitingMailSaysTheMessageStillArrives(t *testing.T) {
	for _, state := range []string{"intent", "pending"} {
		t.Run(state, func(t *testing.T) {
			rf := retrySetup(t)
			if state == "intent" {
				rf.script(repeat(20, failedWith(draftDeferred))...)
			} else {
				rf.script(wire.Delivery{Status: "pending"})
			}
			handle(t, rf.fixture, event("still-arrives", "message.created", message()))
			rf.clock.Set(retryStart.Add(2 * time.Minute))
			rf.poll(t)
			got := mailsFor(rf, messageName)
			if c := rf.change(t, messageName); len(got) != 1 || c.Status != state {
				t.Fatalf("%s: %d mails", c.Status, len(got))
			}
			if got[0].Ended || strings.Contains(got[0].Body, "0 later messages") {
				t.Fatalf("waiting mail: ended %v or a zero count:\n%s", got[0].Ended, got[0].Body)
			}
			for _, want := range []string{"will still be delivered to the agent when its session clears", "must not act on it again when it arrives", messageName} {
				if !strings.Contains(got[0].Body, want) {
					t.Fatalf("waiting mail lacks %q:\n%s", want, got[0].Body)
				}
			}
		})
	}
}

// R3-1 counts mails per life: replay renews the end mail allowance too.
func TestMessageFirstMailedAsEndedGetsANewEndMailAfterReplay(t *testing.T) {
	rf := retrySetup(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "surprise" })
	handle(t, rf.fixture, event("ended-first", "message.created", message()))
	rf.poll(t)
	c := rf.change(t, messageName)
	if got := mailsFor(rf, messageName); len(got) != 1 || !got[0].Ended || c.Raised.IsZero() || c.RaisedEnd.IsZero() || rf.l.Unresolved[messageName].RaisedEnd.IsZero() {
		t.Fatalf("first mail as ended: %d mails, raised %s, raised_end %s", len(got), c.Raised, c.RaisedEnd)
	}
	rf.clock.Add(time.Second)
	replay(t, rf, messageName)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "surprise" })
	for range 3 {
		rf.poll(t)
		rf.clock.Add(time.Hour)
	}
	if c := rf.change(t, messageName); c.Status != "unresolved" || len(mailsFor(rf, messageName)) != 2 {
		t.Fatalf("after the replay ended: %s, %d mails", c.Status, len(mailsFor(rf, messageName)))
	}
}

// gcMail models gc mail send --dedup as the app drives it, with the key the
// app sends (Raise.DedupKey: one per life of the change and kind of mail); a
// send whose key is still live is suppressed and exits 0. lose makes the next
// send land at gc but report a failure (the 10 s limit killed it after gc
// took it).
type gcMail struct {
	mu        sync.Mutex
	live      map[string]bool
	delivered []Raise
	lose      bool
}

func (g *gcMail) raise(_ context.Context, r Raise) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := r.DedupKey()
	if g.live[key] {
		return nil
	}
	g.live[key] = true
	g.delivered = append(g.delivered, r)
	if g.lose {
		g.lose = false
		return errors.New("signal: killed")
	}
	return nil
}

// Self-review r1 MINOR-1 (probe P2): the waiting mail reaches gc but its
// result is lost; the message then ends unresolved within the minute. The end
// mail must not carry the waiting mail's key, or gc drops it and the agent
// keeps only "will still be delivered".
func TestEndMailIsNotShadowedByALostWaitingMail(t *testing.T) {
	g := &gcMail{live: map[string]bool{}, lose: true}
	rf := retrySetup(t, func(o *Options) { o.Raise = g.raise })
	rf.script(repeat(6, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("shadow", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	if c := rf.change(t, messageName); !c.Raised.IsZero() || len(g.delivered) != 1 {
		t.Fatalf("setup: the waiting mail's result is lost: raised %s, delivered %d", c.Raised, len(g.delivered))
	}
	rf.script(wire.Delivery{Status: "pending"})
	rf.clock.Set(rf.change(t, messageName).NextAttempt)
	rf.poll(t)
	rf.gc.SetReceipt(rf.change(t, messageName).ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "pending"}})
	rf.poll(t)
	rf.clock.Add(2 * time.Minute)
	rf.poll(t)
	c := rf.change(t, messageName)
	if c.Status != "unresolved" || c.RaisedEnd.IsZero() {
		t.Fatalf("ended: %s %q, raised_end %s", c.Status, c.Reason, c.RaisedEnd)
	}
	if len(g.delivered) != 2 || !g.delivered[1].Ended {
		t.Fatalf("the agent got %d mails; the end mail was dropped as a duplicate of the waiting mail", len(g.delivered))
	}
}

// Self-review r1 MINOR-3 (probe P3), ruling E: a message mailed as waiting and
// then withdrawn by its sender gets its one end mail: the waiting mail said it
// would still arrive. It does not advise a replay. A withdrawn message never
// mailed while waiting is still never mailed (round 1's exception;
// TestWithdrawnAndOldUnresolvedChangesAreNotRaised).
func TestWithdrawnAfterAWaitingMailGetsItsEndMail(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(40, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("withdrawn", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	handle(t, rf.fixture, event("delete", "message.deleted", &api.Message{Name: messageName}))
	for range 3 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	c := rf.change(t, messageName)
	got := mailsFor(rf, messageName)
	if c.Status != "unresolved" || c.Cause != "withdrawn" || len(got) != 2 || !got[1].Ended {
		t.Fatalf("withdrawn after a waiting mail: %s %s, %d mails", c.Status, c.Cause, len(got))
	}
	if body := got[1].Body; !strings.Contains(body, "withdrew") || strings.Contains(body, "--replay") {
		t.Fatalf("withdrawn end mail:\n%s", body)
	}
}

// R3-1: a replay's new waiting mail names the messages behind it and its
// current retry bound, so those messages need no separate waiting mail.
func TestReplayedHeadMailsItsNewWaitAndCoversTheMessagesBehindIt(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(4000, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("replayed", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	rf.poll(t)
	rf.clock.Set(retryStart.Add(25 * time.Hour))
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != reasonRetryBound || len(mailsFor(rf, messageName)) != 1 {
		t.Fatalf("first life: %s %q, %d mails", c.Status, c.Reason, len(mailsFor(rf, messageName)))
	}
	replay(t, rf, messageName)
	rf.poll(t)
	rf.clock.Add(time.Second)
	second := secondMessage()
	handle(t, rf.fixture, event("behind-replay", "message.created", second))
	rf.clock.Add(2 * time.Minute)
	rf.poll(t)
	if c := rf.change(t, second.Name); c.Status != "intent" || c.Cause != "behind_earlier" || len(mailsFor(rf, second.Name)) != 0 {
		t.Fatalf("behind the replayed head: %s %s, %d mails", c.Status, c.Cause, len(mailsFor(rf, second.Name)))
	}
	if got := mailsFor(rf, messageName); len(got) != 2 || got[1].Ended || !strings.Contains(got[1].Body, "Waiting behind it: 1 later message") {
		t.Fatalf("the replayed head must mail its new wait and the message behind it: %+v", got)
	}
	rf.clock.Set(rf.change(t, messageName).Created.Add(24*time.Hour + time.Second))
	rf.poll(t)
	got := mailsFor(rf, messageName)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != reasonRetryBound || len(got) != 2 || got[1].Ended {
		t.Fatalf("the replayed message at its new bound: %s %q, %d mails", c.Status, c.Reason, len(got))
	}
}

// Self-review r1 MINOR-4 (probe P5), ruling D: the head is decided by the live
// order test, never by the stored cause word. A is the mailed head and B
// waits behind it; A is withdrawn while gc is down; B is released, its POST
// meets gc down, and B keeps the stale word behind_earlier as the new head.
// B is mailed, and its mail does not say it waits behind an earlier message.
func TestHeadIsTheLiveOrderTestNotTheCauseWord(t *testing.T) {
	rf := retrySetup(t)
	rf.script(repeat(400, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("a", "message.created", message()))
	rf.clock.Add(time.Second)
	b := secondMessage()
	handle(t, rf.fixture, event("b", "message.created", b))
	rf.clock.Set(retryStart.Add(10 * time.Minute))
	rf.poll(t)
	if got := rf.raises.list(); len(got) != 1 || got[0].ID != messageName {
		t.Fatalf("head mail: %d", len(got))
	}
	rf.gc.Configure(func(s *fakegc.Server) { s.CityNotRunning = true })
	_, _ = rf.p.Handle(context.Background(), event("delete-a", "message.deleted", &api.Message{Name: messageName}))
	rf.clock.Add(time.Second)
	_ = rf.p.Poll(context.Background())
	if c := rf.change(t, b.Name); c.Status != "intent" || c.Cause != "behind_earlier" {
		t.Fatalf("setup: B is not the head with a stale word: %s %s", c.Status, c.Cause)
	}
	got := mailsFor(rf, b.Name)
	if len(got) != 1 {
		t.Fatalf("the new head with a stale behind_earlier word was not mailed: %d", len(got))
	}
	if strings.Contains(got[0].Body, "an earlier message of the same space has not landed yet") {
		t.Fatalf("the head's mail says it waits behind an earlier message:\n%s", got[0].Body)
	}
}
