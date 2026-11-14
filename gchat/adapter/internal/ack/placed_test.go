package ack

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// PR 104 round 4: the acknowledgement's placement record. The react verb
// takes it once, on the session's reaction of the same emoji on the same
// message; while the acknowledgement is off it is never taken, and it is
// pruned with the attempt markers.
func TestPlacementRecordIsTakenOnceAndOnlyWhileOn(t *testing.T) {
	dir := t.TempDir()
	on, err := New(Options{Mode: "eyes", Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	off, err := New(Options{Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Choice: on, Dir: dir}
	in := Input{Conversation: "example-channel", Message: "example-message"}

	if s.TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Fatal("taken before any placement")
	}
	if err := s.NotePlaced(in, "eyes"); err != nil {
		t.Fatal(err)
	}
	for _, other := range [][3]string{{"example-channel", "example-message", "thumbsup"}, {"example-channel", "other-message", "eyes"}, {"other-channel", "example-message", "eyes"}} {
		if s.TakePlaced(other[0], other[1], other[2]) {
			t.Errorf("taken for %q", other)
		}
	}
	// The switch unset: never taken, and the record stays.
	if (&Service{Choice: off, Dir: dir}).TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Error("taken with the acknowledgement off")
	}
	if !s.TakePlaced(in.Conversation, in.Message, ":eyes:") {
		t.Error("the placement was not taken by its own emoji (colons as Slack clients write them)")
	}
	if s.TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Error("taken twice")
	}

	// Google may write the emoji with or without the presentation selector.
	if err := s.NotePlaced(in, "❤️"); err != nil {
		t.Fatal(err)
	}
	if !s.TakePlaced(in.Conversation, in.Message, "❤") {
		t.Error("the placement was not taken without the presentation selector")
	}

	// Pruned with the markers: older than MarkerAge goes, newer stays.
	if err := s.NotePlaced(in, "eyes"); err != nil {
		t.Fatal(err)
	}
	if err := s.NotePlaced(Input{Conversation: "example-channel", Message: "fresh-message"}, "eyes"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := placedPath(dir, in.Conversation, in.Message, "eyes")
	if err := os.Chtimes(old, now.Add(-MarkerAge-time.Hour), now.Add(-MarkerAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	PruneAttempts(dir, now)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an old placement record was not pruned: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "ack-placed"))
	if err != nil || len(entries) != 1 {
		t.Errorf("records left=%d (%v), want the fresh one", len(entries), err)
	}
}
