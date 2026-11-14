package ack

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// gp-yhd5: the session's reaction placed before the acknowledgement's record
// is marked, so its "already there" repeat is no new answer while that record
// stands; a definite removal clears the mark. With the acknowledgement off
// none of the session verbs reads or writes anything.
func TestSessionMarkMakesTheRepeatNoNewAnswer(t *testing.T) {
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

	// Session first: placed, then the acknowledgement's record, whose own
	// "already there" answer never comes back.
	s.SessionPlaced(in.Conversation, in.Message, ":eyes:")
	if err := s.NotePlacing(in, "eyes"); err != nil {
		t.Fatal(err)
	}
	if s.TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Error("the session's repeat was taken as the acknowledgement's reaction")
	}
	if _, err := os.Stat(placedPath(dir, in.Conversation, in.Message, "eyes")); err != nil {
		t.Errorf("the record went with the repeat: %v", err)
	}

	// The session removes its eyes; the acknowledgement's lands after; the
	// session's next eyes is answered "already there" and counts.
	s.SessionRemoved(in.Conversation, in.Message, "eyes")
	if !s.TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Error("after the session's removal the acknowledgement's reaction was not taken")
	}
	if s.TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Error("taken twice")
	}

	// Another emoji and another message are not marked.
	if err := s.NotePlacing(Input{Conversation: in.Conversation, Message: "other-message"}, "eyes"); err != nil {
		t.Fatal(err)
	}
	if !s.TakePlaced(in.Conversation, "other-message", "eyes") {
		t.Error("a mark on one message hid another's record")
	}

	// The switch unset: nothing written, read or removed.
	mark := sessionPath(dir, in.Conversation, in.Message, "eyes")
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("no mark on disk: %v", err)
	}
	before := ackEntries(t, dir)
	o := &Service{Choice: off, Dir: dir}
	o.SessionPlaced(in.Conversation, "third-message", "eyes")
	o.SessionRemoved(in.Conversation, in.Message, "eyes")
	if o.TakePlaced(in.Conversation, in.Message, "eyes") {
		t.Error("taken with the acknowledgement off")
	}
	if after := ackEntries(t, dir); after != before {
		t.Errorf("with the acknowledgement off the state went from %q to %q", before, after)
	}

	// Pruned after the records: a record younger than the mark (the session
	// first) is gone before the mark goes.
	now := time.Now()
	record := placedPath(dir, in.Conversation, in.Message, "eyes")
	if err := s.NotePlacing(in, "eyes"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(mark, now.Add(-MarkerAge-2*time.Hour), now.Add(-MarkerAge-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(record, now.Add(-MarkerAge-time.Hour), now.Add(-MarkerAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	PruneAttempts(dir, now)
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("a record older than MarkerAge was not pruned: %v", err)
	}
	if _, err := os.Stat(mark); err != nil {
		t.Errorf("the mark went with its younger record: %v", err)
	}
	if err := os.Chtimes(mark, now.Add(-sessionMarkAge-time.Hour), now.Add(-sessionMarkAge-time.Hour)); err != nil {
		t.Fatal(err)
	}
	PruneAttempts(dir, now)
	if _, err := os.Stat(mark); !os.IsNotExist(err) {
		t.Errorf("a mark older than sessionMarkAge was not pruned: %v", err)
	}
}

func ackEntries(t *testing.T, dir string) string {
	t.Helper()
	var out string
	for _, sub := range []string{"ack-placed", "ack-session"} {
		entries, _ := os.ReadDir(filepath.Join(dir, sub))
		for _, e := range entries {
			out += sub + "/" + e.Name() + " "
		}
	}
	return out
}
