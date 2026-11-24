package ack

import (
	"context"
	"errors"
	"testing"
	"time"
)

// PR 109 round 3: a call waiting for its turn on a reaction ends with its
// caller. A waiter whose context is cancelled, or reaches its deadline,
// returns that error at once, holding nothing; the calls queued before and
// after it go in their order as if it had never come; a caller already gone
// when the turn is free takes nothing (the check after the turn is taken);
// nothing is kept, and a later call takes the turn at once. With the
// acknowledgement off a caller that has gone is never refused.
func TestHoldReactionContextEndsWithItsCaller(t *testing.T) {
	on, err := New(Options{Mode: "eyes", Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	off, err := New(Options{Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Choice: on, Dir: t.TempDir()}
	const conversation, message, emoji = "example-channel", "example-message", "eyes"
	type turn struct {
		release func()
		waited  time.Duration
		err     error
	}
	hold := func(ctx context.Context) <-chan turn {
		got := make(chan turn, 1)
		go func() {
			release, waited, err := s.HoldReactionContext(ctx, conversation, message, emoji)
			got <- turn{release, waited, err}
		}()
		return got
	}
	waiting := func(name string, got <-chan turn) {
		t.Helper()
		select {
		case x := <-got:
			x.release()
			t.Fatalf("%s did not wait for the turn (err=%v)", name, x.err)
		case <-time.After(100 * time.Millisecond):
		}
	}
	next := func(name string, got <-chan turn) turn {
		t.Helper()
		select {
		case x := <-got:
			return x
		case <-time.After(2 * time.Second):
			t.Fatalf("%s was still waiting", name)
		}
		return turn{}
	}
	callers := func() (n int) {
		s.reacting.Lock()
		defer s.reacting.Unlock()
		for _, x := range s.turns {
			n += x.callers
		}
		return n
	}

	holder, _, err := s.HoldReactionContext(context.Background(), conversation, message, emoji)
	if err != nil {
		t.Fatal(err)
	}
	first := hold(context.Background())
	waiting("the first waiter", first)
	ctx, cancel := context.WithCancel(context.Background())
	gone := hold(ctx)
	waiting("the waiter whose caller gives up", gone)
	last := hold(context.Background())
	waiting("the last waiter", last)

	cancel()
	if x := next("the waiter whose caller gave up", gone); !errors.Is(x.err, context.Canceled) {
		x.release()
		t.Fatalf("the waiter whose caller gave up: err=%v, want context.Canceled", x.err)
	} else {
		x.release() // holds nothing: the holder keeps the turn
	}
	if n := callers(); n != 3 {
		t.Errorf("callers=%d after the waiter left, want 3 (the holder and two waiters)", n)
	}
	waiting("the first waiter, once the other left", first)
	holder()
	x := next("the first waiter", first)
	if x.err != nil || x.waited < 300*time.Millisecond {
		t.Errorf("the first waiter: err=%v waited=%s, want its turn after at least 300ms", x.err, x.waited)
	}
	waiting("the last waiter, while the first holds the turn", last)
	x.release()
	if y := next("the last waiter", last); y.err != nil {
		t.Errorf("the last waiter: err=%v", y.err)
	} else {
		y.release()
	}

	holder, _, _ = s.HoldReactionContext(context.Background(), conversation, message, emoji)
	deadline, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	start := time.Now()
	if x := next("the waiter whose deadline passed", hold(deadline)); !errors.Is(x.err, context.DeadlineExceeded) {
		x.release()
		t.Errorf("the waiter whose deadline passed: err=%v, want context.DeadlineExceeded", x.err)
	} else if took := time.Since(start); took < 150*time.Millisecond {
		t.Errorf("the waiter whose deadline passed returned after %s, before its deadline", took)
	}
	holder()

	gone2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	release, waited, err := s.HoldReactionContext(gone2, conversation, message, emoji)
	release()
	if !errors.Is(err, context.Canceled) || waited != 0 {
		t.Errorf("a caller already gone, the turn free: err=%v waited=%s, want context.Canceled and no wait", err, waited)
	}
	if n := callers(); n != 0 || len(s.turns) != 0 {
		t.Errorf("turns=%d callers=%d after every caller left, want none", len(s.turns), n)
	}
	release, waited, err = s.HoldReactionContext(context.Background(), conversation, message, emoji)
	if err != nil || waited != 0 {
		t.Errorf("a later call: err=%v waited=%s, want the turn at once", err, waited)
	}
	release()
	if len(s.turns) != 0 {
		t.Errorf("turns=%d kept after every caller released, want 0", len(s.turns))
	}

	quiet := &Service{Choice: off, Dir: t.TempDir()}
	release, waited, err = quiet.HoldReactionContext(gone2, conversation, message, emoji)
	release()
	if err != nil || waited != 0 || quiet.turns != nil {
		t.Errorf("with the acknowledgement off, a caller already gone: err=%v waited=%s turns=%v, want the base path", err, waited, quiet.turns)
	}
	var none *Service
	release, waited, err = none.HoldReactionContext(gone2, conversation, message, emoji)
	release()
	if err != nil || waited != 0 {
		t.Errorf("a nil service: err=%v waited=%s", err, waited)
	}
}
