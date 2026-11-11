package inbound

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// Round 3 (gp-t542): the two Majors of the mayor's codex gate r2 at 136dbae1.

func partialAnswer() wire.Delivery {
	return wire.Delivery{Status: "partial", DeliveredBytes: 40, ExpectedBytes: 120, Members: []wire.DeliveryMember{{Status: "partial", DeliveredBytes: 40, ExpectedBytes: 120}}}
}

func heldPartialAnswer() wire.Delivery {
	return wire.Delivery{Status: "held", DeliveredBytes: 40, ExpectedBytes: 240, Members: []wire.DeliveryMember{
		{Status: "held", ExpectedBytes: 120}, {Status: "partial", DeliveredBytes: 40, ExpectedBytes: 120}}}
}

// M1: a fragment of the message reached the terminal on one POST, so the
// message keeps the four-POST bound whatever gc answers to its other POSTs.
// At 136dbae1 the bound read only the latest answer: partial, then three
// failures, put the change back in intent with a next attempt, a fifth POST
// followed, and the space's later messages could wait behind it for 24 hours.
// A restart between the first and the second answer must not forget the
// fragment. A message that never delivered a fragment retries as before.
func TestFragmentOnAnyAnswerKeepsTheFourPOSTBound(t *testing.T) {
	failed := failedWith(draftDeferred)
	for _, tc := range []struct {
		name    string
		answers []wire.Delivery
		restart bool
		bounded bool
	}{
		{"partial then three failures", []wire.Delivery{partialAnswer(), failed, failed, failed}, false, true},
		{"partial then three failures, restart after the first answer", []wire.Delivery{partialAnswer(), failed, failed, failed}, true, true},
		{"failure, partial, two failures", []wire.Delivery{failed, partialAnswer(), failed, failed}, false, true},
		{"held with a partial member then three failures", []wire.Delivery{heldPartialAnswer(), failed, failed, failed}, false, true},
		{"held with a partial member then three failures, restart after the first answer", []wire.Delivery{heldPartialAnswer(), failed, failed, failed}, true, true},
		{"never a fragment", []wire.Delivery{failed, failed, failed, failed}, false, false},
		{"never a fragment, restart after the first answer", []wire.Delivery{failed, failed, failed, failed}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf := retrySetup(t)
			// Answers for a fifth and later POST, should one be made.
			rf.script(append(tc.answers, repeat(400, failed)...)...)
			if tc.restart {
				// The process stops in the wait before the second POST.
				rf.p.wait = func(context.Context, time.Duration) error { return context.Canceled }
				if out, err := rf.p.Handle(context.Background(), event("fragment", "message.created", message())); err == nil {
					t.Fatalf("setup: the first POST's wait was not interrupted: %v", out)
				}
				if c := rf.change(t, messageName); c.Status != "intent" || c.Reason != reasonRetry || c.Attempts != 1 || len(rf.gc.Inbound()) != 1 {
					t.Fatalf("setup: after the first answer: %s %q, %d attempts, %d POSTs", c.Status, c.Reason, c.Attempts, len(rf.gc.Inbound()))
				}
				rf.restart(t)
				rf.poll(t)
			} else {
				handle(t, rf.fixture, event("fragment", "message.created", message()))
			}
			if got := len(rf.gc.Inbound()); got != 4 {
				t.Fatalf("first phase: %d POSTs, want 4", got)
			}
			// Two hours of polls: far past every wait of a retrying change.
			for range 120 {
				rf.clock.Add(time.Minute)
				rf.poll(t)
			}
			c := rf.change(t, messageName)
			posts := len(rf.gc.Inbound())
			raw, err := os.ReadFile(rf.opts.LedgerPath)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.bounded {
				if posts <= 4 || c.Status != "intent" || c.Reason != reasonRetry || c.NextAttempt.IsZero() {
					t.Fatalf("a message that never delivered a fragment stopped retrying: %d POSTs, %s %q", posts, c.Status, c.Reason)
				}
				if strings.Contains(string(raw), `"fragment":`) {
					t.Fatalf("a change with no fragment writes the mark: %s", raw)
				}
				return
			}
			if posts != 4 {
				t.Fatalf("a message that delivered a fragment was POSTed %d times, want 4", posts)
			}
			if c.Status != "unresolved" || c.Reason != reasonExhausted || !c.NextAttempt.IsZero() || rf.l.Unresolved[messageName] == nil {
				t.Fatalf("after four POSTs with a fragment: %s %q, next attempt %s", c.Status, c.Reason, c.NextAttempt)
			}
			if !strings.Contains(string(raw), `"fragment":true`) {
				t.Fatalf("the ledger does not keep the fragment mark: %s", raw)
			}
			// The end mail does not say the fourth answer carried the fragment.
			got := mailsFor(rf, messageName)
			if len(got) != 1 || !got[0].Ended || strings.Contains(got[0].Body, "fourth or a later POST said") || !strings.Contains(got[0].Body, "fragment") {
				t.Fatalf("end mail of a fragment message (%d mails):\n%+v", len(got), got)
			}
		})
	}
}

// M1: the space's later message is no longer held for hours behind a message
// that delivered a fragment and then met "not delivered": the head ends at
// its fourth POST, and the message behind it is POSTed at once.
func TestFragmentMessageDoesNotHoldItsSpace(t *testing.T) {
	rf := retrySetup(t)
	failed := failedWith(draftDeferred)
	rf.script(append([]wire.Delivery{partialAnswer(), failed, failed, failed, {Status: "delivered"}}, repeat(40, failed)...)...)
	handle(t, rf.fixture, event("head", "message.created", message()))
	rf.clock.Add(time.Second)
	second := secondMessage()
	handle(t, rf.fixture, event("behind", "message.created", second))
	if c := rf.change(t, second.Name); c.Status != "committed" {
		t.Fatalf("the message behind the fragment head: %s %s %q after %d POSTs", c.Status, c.Cause, c.Reason, len(rf.gc.Inbound()))
	}
}

// M1 with the replay: a replay starts a new life whose four in-gate POSTs
// count from zero, so it clears the mark with the rest of the retry state; a
// fragment in the replay's own life marks it again.
func TestReplayClearsTheFragmentMark(t *testing.T) {
	rf := retrySetup(t)
	failed := failedWith(draftDeferred)
	rf.script(partialAnswer(), failed, failed, failed)
	handle(t, rf.fixture, event("first-life", "message.created", message()))
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != reasonExhausted {
		t.Fatalf("first life: %s %q", c.Status, c.Reason)
	}
	rf.clock.Add(time.Hour)
	replay(t, rf, messageName)
	rf.script(repeat(40, failed)...)
	rf.poll(t)
	for range 10 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	if c := rf.change(t, messageName); c.Status != "intent" || c.Reason != reasonRetry || len(rf.gc.Inbound()) <= 8 {
		t.Fatalf("a replay with no fragment of its own stopped retrying: %s %q after %d POSTs", c.Status, c.Reason, len(rf.gc.Inbound()))
	}
	rf.script(partialAnswer())
	rf.clock.Set(rf.change(t, messageName).NextAttempt)
	rf.poll(t)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != reasonExhausted {
		t.Fatalf("a fragment in the replay's life: %s %q", c.Status, c.Reason)
	}
}

// replay is gc gchat resolve --replay as the server does it (server.go): the
// record moves back to Changes in intent with its retry state cleared and its
// mail marks kept, recorded at the current time.
func replay(t *testing.T, rf *retryFixture, id string) {
	t.Helper()
	rf.opts.LedgerMu.Lock()
	defer rf.opts.LedgerMu.Unlock()
	c := rf.l.Unresolved[id]
	if c == nil {
		t.Fatalf("replay: %s is not unresolved", id)
	}
	c.Status, c.Attempts, c.ReceiptID, c.Reason = "intent", 0, "", "operator replay"
	c.Created = rf.clock.Now().UTC()
	c.NextAttempt, c.Posted, c.Cause = time.Time{}, time.Time{}, ""
	c.Fragment = false
	rf.l.Changes[id] = c
	delete(rf.l.Unresolved, id)
}

// blockedRaise holds the first mail inside the Raise call until release is
// closed, as a slow gc mail send does; every mail is logged.
type blockedRaise struct {
	raiseLog
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (b *blockedRaise) raise(ctx context.Context, r Raise) error {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.started)
		<-b.release
	}
	return b.raiseLog.raise(ctx, r)
}

// M2: the mail of a change's first life completes while gc gchat resolve
// --replay starts its second. At 136dbae1 the stamp landed on the replayed
// record, later than its new recorded time: its waiting mail read as sent,
// and it covered the messages behind it, so none of them was mailed while
// the replay kept failing. Wanted: nothing is stamped on the replayed record,
// and it is mailed on its own terms, once, naming the message behind it.
func TestRaiseCompletingAcrossAReplayStampsNothing(t *testing.T) {
	rf := retrySetup(t)
	b := &blockedRaise{started: make(chan struct{}), release: make(chan struct{})}
	rf.opts.Raise, rf.raises = b.raise, &b.raiseLog
	rf.rebuild(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "surprise" })
	handle(t, rf.fixture, event("first-life", "message.created", message()))
	if c := rf.change(t, messageName); c.Status != "unresolved" || !c.Raised.IsZero() {
		t.Fatalf("setup: %s, raised %s", c.Status, c.Raised)
	}
	done := make(chan error, 1)
	go func() { done <- rf.p.Poll(context.Background()) }()
	select {
	case <-b.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the end mail was not sent")
	}
	// While the end mail of the first life is in flight: the operator's replay
	// and its pass, which meets a busy session.
	rf.clock.Add(time.Minute)
	replay(t, rf, messageName)
	rf.script(repeat(400, failedWith(draftDeferred))...)
	if err := rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c := rf.change(t, messageName)
	if c.Status != "intent" || !c.Raised.IsZero() || !c.RaisedEnd.IsZero() {
		t.Fatalf("the replayed record after the first life's mail: %s, raised %s, raised_end %s", c.Status, c.Raised, c.RaisedEnd)
	}
	if got := mailsFor(rf, messageName); len(got) != 1 || !got[0].Ended {
		t.Fatalf("the first life's end mail: %+v", got)
	}
	rf.clock.Add(time.Second)
	second := secondMessage()
	handle(t, rf.fixture, event("behind-replay", "message.created", second))
	if c := rf.change(t, second.Name); c.Status != "intent" || c.Cause != causeBehindEarlier {
		t.Fatalf("setup: the second message does not wait behind the replay: %s %s", c.Status, c.Cause)
	}
	for range 10 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	got := mailsFor(rf, messageName)
	if len(got) != 2 || got[1].Ended || !strings.Contains(got[1].Body, "Waiting behind it: 1 later message of this space") {
		t.Fatalf("the replayed message's own waiting mail (%d mails for it):\n%+v", len(got), got)
	}
	if n := len(mailsFor(rf, second.Name)); n != 0 {
		t.Fatalf("the message behind the mailed head was mailed on its own: %d", n)
	}
	if c := rf.change(t, messageName); c.Raised.IsZero() || c.Raised.Before(c.Created) {
		t.Fatalf("the replayed record's own waiting mail is not marked: raised %s, created %s", c.Raised, c.Created)
	}
}

// M2, the same life: a waiting mail in flight while its message ends is still
// marked, so the end gets its one second mail (ruling E) and not a first one.
func TestRaiseCompletingAcrossAnEndInTheSameLifeIsMarked(t *testing.T) {
	rf := retrySetup(t)
	b := &blockedRaise{started: make(chan struct{}), release: make(chan struct{})}
	rf.opts.Raise, rf.raises = b.raise, &b.raiseLog
	rf.rebuild(t)
	rf.script(repeat(40, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("same-life", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	done := make(chan error, 1)
	go func() { done <- rf.p.Poll(context.Background()) }()
	select {
	case <-b.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the waiting mail was not sent")
	}
	// The sender deletes the message while its waiting mail is in flight.
	handle(t, rf.fixture, event("delete", "message.deleted", &api.Message{Name: messageName}))
	rf.clock.Add(time.Second)
	if err := rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c := rf.change(t, messageName)
	if c.Status != "unresolved" || c.Cause != causeWithdrawn || c.Raised.IsZero() {
		t.Fatalf("the waiting mail of this life is not marked: %s %s, raised %s", c.Status, c.Cause, c.Raised)
	}
	for range 3 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	got := mailsFor(rf, messageName)
	if len(got) != 2 || got[0].Ended || !got[1].Ended || !strings.Contains(got[1].Body, "withdrew") {
		t.Fatalf("withdrawn after a waiting mail in flight: %d mails: %+v", len(got), got)
	}
}
