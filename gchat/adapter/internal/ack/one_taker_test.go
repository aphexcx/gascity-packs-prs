package ack

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// gp-yhd5, item 3: two simultaneous identical session reactions, both
// answered "already there", meet one placement record. Exactly one of them
// is the acknowledgement's reaction counted as the session's new answer; the
// other is a repeat. A file removed by two callers at once can report success
// to both (shown on macOS), so the record alone does not make one taker.
func TestPlacementRecordHasOneTaker(t *testing.T) {
	on, err := New(Options{Mode: "eyes", Catalog: Catalog})
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{Choice: on, Dir: t.TempDir()}
	for i := 0; i < 50; i++ {
		in := Input{Conversation: "example-channel", Message: fmt.Sprintf("example-message-%d", i)}
		if err := s.NotePlacing(in, "eyes"); err != nil {
			t.Fatal(err)
		}
		var taken atomic.Int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if s.TakePlaced(in.Conversation, in.Message, "eyes") {
					taken.Add(1)
				}
			}()
		}
		close(start)
		wg.Wait()
		if n := taken.Load(); n != 1 {
			t.Fatalf("message %d: %d takers of one placement record, want exactly 1", i, n)
		}
	}
}
