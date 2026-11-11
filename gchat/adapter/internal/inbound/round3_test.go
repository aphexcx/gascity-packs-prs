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
			// gc's last answer was draft_deferred: the mail says what that means.
			if c.Cause != causeDraftDeferred || !strings.Contains(got[0].Body, "Cause: draft_deferred ("+causeMeaning[causeDraftDeferred]+"; ") {
				t.Fatalf("the cause line does not tell its word (%s):\n%s", c.Cause, got[0].Body)
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

// mailGate holds the first mail inside the Raise call until release is
// closed, as a slow gc mail send does, then hands every mail to next.
type mailGate struct {
	next    func(context.Context, Raise) error
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newMailGate(next func(context.Context, Raise) error) *mailGate {
	return &mailGate{next: next, started: make(chan struct{}), release: make(chan struct{})}
}

func (b *mailGate) raise(ctx context.Context, r Raise) error {
	first := false
	b.once.Do(func() { first = true })
	if first {
		close(b.started)
		<-b.release
	}
	return b.next(ctx, r)
}

// inFlight starts a Poll whose first mail the gate holds, and returns once
// that mail is in the Raise call; finish releases it and waits for the Poll.
func (b *mailGate) inFlight(t *testing.T, rf *retryFixture) (finish func()) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- rf.p.Poll(context.Background()) }()
	select {
	case <-b.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no mail was sent")
	}
	return func() {
		t.Helper()
		close(b.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

// deliveredFor returns the mails gc took for one change, in order.
func (g *gcMail) deliveredFor(id string) []Raise {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Raise
	for _, r := range g.delivered {
		if r.ID == id {
			out = append(out, r)
		}
	}
	return out
}

// dedupMail is a retry fixture whose mails go through gcMail (gc's dedup,
// with the key the app sends) behind a gate.
func dedupMail(t *testing.T) (*retryFixture, *gcMail, *mailGate) {
	t.Helper()
	g := &gcMail{live: map[string]bool{}}
	b := newMailGate(g.raise)
	rf := retrySetup(t, func(o *Options) { o.Raise = b.raise })
	return rf, g, b
}

// M2: the END mail of a change's first life completes while gc gchat resolve
// --replay starts its second. At 136dbae1 the stamp landed on the replayed
// record, later than its new recorded time: its waiting mail read as sent,
// and it covered the messages behind it, so none of them was mailed while
// the replay kept failing. At c1ae854f nothing was stamped, but the mail key
// was the change's alone: the second life's end mail met the first life's
// live key at gc, was dropped, and was marked sent, so the waiting mail's
// promise of one more mail was broken. Wanted: nothing is stamped on the
// replayed record; it is mailed on its own terms, as a message never mailed,
// and gc takes each of its mails.
func TestRaiseCompletingAcrossAReplayStampsNothing(t *testing.T) {
	rf, g, b := dedupMail(t)
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "surprise" })
	handle(t, rf.fixture, event("first-life", "message.created", message()))
	if c := rf.change(t, messageName); c.Status != "unresolved" || !c.Raised.IsZero() {
		t.Fatalf("setup: %s, raised %s", c.Status, c.Raised)
	}
	finish := b.inFlight(t, rf)
	// While the end mail of the first life is in flight: the operator's replay
	// and its pass, which meets a busy session.
	rf.clock.Add(time.Minute)
	replay(t, rf, messageName)
	rf.script(repeat(400, failedWith(draftDeferred))...)
	if err := rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	finish()
	c := rf.change(t, messageName)
	if c.Status != "intent" || !c.Raised.IsZero() || !c.RaisedEnd.IsZero() {
		t.Fatalf("the replayed record after the first life's mail: %s, raised %s, raised_end %s", c.Status, c.Raised, c.RaisedEnd)
	}
	if got := g.deliveredFor(messageName); len(got) != 1 || !got[0].Ended {
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
	got := g.deliveredFor(messageName)
	if len(got) != 2 || got[1].Ended || !strings.Contains(got[1].Body, "Waiting behind it: 1 later message of this space") {
		t.Fatalf("the replayed message's own waiting mail (%d mails for it at gc):\n%+v", len(got), got)
	}
	if n := len(g.deliveredFor(second.Name)); n != 0 {
		t.Fatalf("the message behind the mailed head was mailed on its own: %d", n)
	}
	if c := rf.change(t, messageName); c.Raised.IsZero() || c.Raised.Before(c.Created) {
		t.Fatalf("the replayed record's own waiting mail is not marked: raised %s, created %s", c.Raised, c.Created)
	}
	// The replay ends for a reason other than its bound: the one more mail its
	// waiting mail promised reaches gc.
	rf.script()
	rf.gc.Configure(func(s *fakegc.Server) { s.NextDelivery = "surprise" })
	rf.clock.Set(rf.change(t, messageName).NextAttempt)
	rf.poll(t)
	rf.poll(t)
	got = g.deliveredFor(messageName)
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.RaisedEnd.IsZero() || len(got) != 3 || !got[2].Ended {
		t.Fatalf("the replay's end: %s %q, raised_end %s, %d mails at gc", c.Status, c.Reason, c.RaisedEnd, len(got))
	}
}

// M2, the WAITING mail of the first life in flight (self-review r1 MAJOR,
// probe A): the first life ends at a fragment and is replayed before the mail
// returns. At c1ae854f the second life's waiting mail met the first life's
// live key at gc, was dropped and marked sent, so the replay covered the
// message behind it and neither was mailed. Wanted: gc takes the second
// life's own waiting mail, which names the message behind it.
func TestRaiseCompletingAcrossAReplayOfAWaitingMessage(t *testing.T) {
	rf, g, b := dedupMail(t)
	failed := failedWith(draftDeferred)
	rf.script(failed, failed, failed, failed, failed, partialAnswer())
	handle(t, rf.fixture, event("first-life", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	finish := b.inFlight(t, rf)
	if c := rf.change(t, messageName); c.Status != "intent" || c.Attempts != 5 {
		t.Fatalf("setup: %s after %d POSTs", c.Status, c.Attempts)
	}
	// While the waiting mail is in flight: the sixth POST answers partial, so
	// the first life ends; the operator replays, and the replay meets a busy
	// session.
	rf.clock.Add(time.Minute)
	if err := rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := rf.change(t, messageName); c.Status != "unresolved" || c.Reason != reasonExhausted {
		t.Fatalf("setup: the first life did not end: %s %q", c.Status, c.Reason)
	}
	rf.clock.Add(time.Second)
	replay(t, rf, messageName)
	rf.script(repeat(400, failed)...)
	if err := rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	finish()
	if c := rf.change(t, messageName); c.Status != "intent" || !c.Raised.IsZero() || !c.RaisedEnd.IsZero() {
		t.Fatalf("the replayed record after the first life's waiting mail: %s, raised %s, raised_end %s", c.Status, c.Raised, c.RaisedEnd)
	}
	rf.clock.Add(time.Second)
	second := secondMessage()
	handle(t, rf.fixture, event("behind-replay", "message.created", second))
	for range 10 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	got := g.deliveredFor(messageName)
	if len(got) != 2 || got[0].Ended || got[1].Ended || !strings.Contains(got[1].Body, "Waiting behind it: 1 later message of this space") {
		t.Fatalf("waiting mails at gc for the two lives: %d:\n%+v", len(got), got)
	}
	if n := len(g.deliveredFor(second.Name)); n != 0 {
		t.Fatalf("the message behind the mailed head was mailed on its own: %d", n)
	}
}

// M2, the same life: a waiting mail in flight while its message ends is still
// marked, so the end gets its one second mail (ruling E) and not a first one.
func TestRaiseCompletingAcrossAnEndInTheSameLifeIsMarked(t *testing.T) {
	rf, g, b := dedupMail(t)
	rf.script(repeat(40, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("same-life", "message.created", message()))
	rf.clock.Set(retryStart.Add(2 * time.Minute))
	finish := b.inFlight(t, rf)
	// The sender deletes the message while its waiting mail is in flight.
	handle(t, rf.fixture, event("delete", "message.deleted", &api.Message{Name: messageName}))
	rf.clock.Add(time.Second)
	if err := rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	finish()
	c := rf.change(t, messageName)
	if c.Status != "unresolved" || c.Cause != causeWithdrawn || c.Raised.IsZero() {
		t.Fatalf("the waiting mail of this life is not marked: %s %s, raised %s", c.Status, c.Cause, c.Raised)
	}
	for range 3 {
		rf.clock.Add(time.Minute)
		rf.poll(t)
	}
	got := g.deliveredFor(messageName)
	if len(got) != 2 || got[0].Ended || !got[1].Ended || !strings.Contains(got[1].Body, "withdrew") {
		t.Fatalf("withdrawn after a waiting mail in flight: %d mails at gc: %+v", len(got), got)
	}
}

// A mail's key at gc is one per life of the change and kind of mail: a
// retry of one mail in one life keeps its key, so gc keeps a send whose
// result was lost to one mail; a replay's life has keys of its own.
func TestDedupKeyIsPerLifeAndKind(t *testing.T) {
	first := Raise{ID: messageName, Recorded: retryStart}
	if first.DedupKey() != (Raise{ID: messageName, Recorded: retryStart, Body: "other"}).DedupKey() {
		t.Fatal("one mail of one life has two keys")
	}
	ended := first
	ended.Ended = true
	replayed := Raise{ID: messageName, Recorded: retryStart.Add(time.Hour)}
	keys := map[string]bool{first.DedupKey(): true, ended.DedupKey(): true, replayed.DedupKey(): true}
	if len(keys) != 3 || !strings.HasSuffix(ended.DedupKey(), ":ended") || strings.Contains(first.DedupKey(), messageName) {
		t.Fatalf("keys: waiting %q ended %q replayed %q", first.DedupKey(), ended.DedupKey(), replayed.DedupKey())
	}
}

// Self-review r2 MINOR-2: within one life a mail keeps its key, across a
// restart too, so a mail gc took whose result was lost stays one mail at gc
// when it is tried again. The clock is a wall clock reading with nanoseconds,
// a monotonic reading and a local zone; the ledger is reloaded from its JSON.
func TestLostMailKeepsItsKeyWithinALifeAcrossARestart(t *testing.T) {
	g := &gcMail{live: map[string]bool{}, lose: true}
	rf := retrySetup(t, func(o *Options) { o.Raise = g.raise })
	start := time.Now().In(time.FixedZone("CST", -6*60*60)).Add(-time.Hour)
	if start.Nanosecond() == 0 {
		start = start.Add(123456789)
	}
	rf.clock.Set(start)
	rf.script(repeat(400, failedWith(draftDeferred))...)
	handle(t, rf.fixture, event("lost", "message.created", message()))
	rf.clock.Set(start.Add(2*time.Minute + 30*time.Second))
	rf.poll(t)
	if c := rf.change(t, messageName); !c.Raised.IsZero() || len(g.delivered) != 1 {
		t.Fatalf("setup: the waiting mail's result is lost: raised %s, %d mails at gc", c.Raised, len(g.delivered))
	}
	first := g.delivered[0]
	if c := rf.change(t, messageName); !first.Recorded.Equal(c.Created) {
		t.Fatalf("the mail names another life: recorded %s, created %s", first.Recorded, c.Created)
	}
	rf.restart(t)
	var tried []Raise
	rf.opts.Raise = func(ctx context.Context, r Raise) error { tried = append(tried, r); return g.raise(ctx, r) }
	rf.rebuild(t)
	rf.clock.Add(2 * time.Minute)
	rf.poll(t)
	c := rf.change(t, messageName)
	if len(tried) != 1 || tried[0].DedupKey() != first.DedupKey() || len(g.delivered) != 1 || c.Raised.IsZero() {
		t.Fatalf("the waiting mail tried again after a restart: %d tries, same key %v, %d mails at gc, raised %s",
			len(tried), len(tried) == 1 && tried[0].DedupKey() == first.DedupKey(), len(g.delivered), c.Raised)
	}
}
