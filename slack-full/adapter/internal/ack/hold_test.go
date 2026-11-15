package ack

import (
	"testing"
	"time"
)

// PR 109 round 2: the react verb's turn on one reaction. A second session
// call on the same reaction, in any spelling, waits until the first releases
// its turn, and waiting calls go one at a time; a call on another emoji,
// another message or another conversation does not wait; a turn goes with
// its last caller; with the acknowledgement off nothing waits and nothing is
// kept.
func TestHoldReactionIsOneTurnPerReaction(t *testing.T) {
	on, err := New(Options{Mode: "eyes", Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	off, err := New(Options{Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Choice: on, Dir: t.TempDir()}
	hold := func(s *Service, conversation, message, emoji string) <-chan func() {
		got := make(chan func(), 1)
		go func() { got <- s.HoldReaction(conversation, message, emoji) }()
		return got
	}
	prompt := func(name string, got <-chan func()) {
		t.Helper()
		select {
		case release := <-got:
			release()
		case <-time.After(5 * time.Second):
			t.Fatalf("%s waited behind another reaction's turn", name)
		}
	}
	waiting := func(name string, got <-chan func()) {
		t.Helper()
		select {
		case release := <-got:
			release()
			t.Fatalf("%s did not wait for the turn held on its reaction", name)
		case <-time.After(100 * time.Millisecond):
		}
	}

	first := s.HoldReaction("example-channel", "example-message", "eyes")
	prompt("another emoji", hold(s, "example-channel", "example-message", "thumbsup"))
	prompt("another message", hold(s, "example-channel", "example-message-2", "eyes"))
	prompt("another conversation", hold(s, "example-channel-2", "example-message", "eyes"))
	colons := hold(s, "example-channel", "example-message", ":eyes:")
	selector := hold(s, "example-channel", "example-message", "eyes️")
	waiting("the same reaction with colons", colons)
	waiting("the same reaction with the presentation selector", selector)

	first()
	first() // a second release changes nothing
	var next func()
	var still <-chan func()
	select {
	case next = <-colons:
		still = selector
	case next = <-selector:
		still = colons
	case <-time.After(5 * time.Second):
		t.Fatal("no waiting call had its turn after the first was released")
	}
	waiting("the other waiting call", still)
	next()
	prompt("the last waiting call", still)
	s.reacting.Lock()
	kept := len(s.turns)
	s.reacting.Unlock()
	if kept != 0 {
		t.Errorf("%d turns kept after every caller released, want 0", kept)
	}

	quiet := &Service{Choice: off, Dir: t.TempDir()}
	held := quiet.HoldReaction("example-channel", "example-message", "eyes")
	prompt("with the acknowledgement off, the same reaction", hold(quiet, "example-channel", "example-message", "eyes"))
	held()
	if quiet.turns != nil {
		t.Errorf("with the acknowledgement off, turns=%v, want none", quiet.turns)
	}
	var none *Service
	none.HoldReaction("example-channel", "example-message", "eyes")()
}
