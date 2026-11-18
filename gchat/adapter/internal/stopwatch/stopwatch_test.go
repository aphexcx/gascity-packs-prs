package stopwatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMonthlyPrivateJournalAndRestart(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 1, 31, 23, 59, 59, 0, time.UTC)
	w := New(dir, "gchat", "release-example", nil)
	in := Event{Event: "inbound", Conversation: "space-example", Message: "message-example", Thread: "thread-example", MessageAt: at, At: at}
	w.Record(in)
	w.Record(in)
	w.Record(Event{Event: "reaction", Conversation: in.Conversation, Message: in.Message, At: at.Add(time.Second), Actor: "adapter", Choice: "fixed"})
	words := Event{Event: "words", Conversation: in.Conversation, Message: "reply-example", At: at.Add(time.Second)}
	w.Record(words)
	w.Flush()
	w = New(dir, "gchat", "release-example", nil)
	w.Record(words)
	w.Record(in)
	w.Record(Event{Event: "reaction", Conversation: in.Conversation, Message: in.Message, At: at.Add(2 * time.Second), Actor: "agent"})
	w.Flush()
	files, _ := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
	if len(files) != 2 {
		t.Fatal(files)
	}
	total := 0
	for _, name := range files {
		info, _ := os.Stat(name)
		if info.Mode().Perm() != 0600 {
			t.Fatal("not private")
		}
		b, _ := os.ReadFile(name)
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var e Event
			if json.Unmarshal([]byte(line), &e) != nil {
				t.Fatal("invalid line")
			}
			total++
			if e.Channel != "gchat" {
				t.Fatal("missing channel")
			}
			if e.Event == "inbound" && e.Release != "release-example" {
				t.Fatal("missing release")
			}
		}
	}
	if total != 3 {
		t.Fatalf("duplicate events: %d", total)
	}
}

func TestWriteFailureWarnsOnceAndReleaseHashesBytes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("one"), 0600)
	warnings := 0
	w := New(file, "gchat", "release", func(string) { warnings++ })
	for i := 0; i < 3; i++ {
		w.Record(Event{Event: "words", At: time.Now()})
	}
	w.Flush()
	if warnings != 1 {
		t.Fatal(warnings)
	}
	a, _ := Release(file)
	b, _ := Release(file)
	if a != b || len(a) != 12 {
		t.Fatal("unstable release")
	}
	os.WriteFile(file, []byte("two"), 0600)
	c, _ := Release(file)
	if c == a {
		t.Fatal("release ignored bytes")
	}
}
