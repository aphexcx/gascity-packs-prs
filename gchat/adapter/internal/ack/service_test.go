package ack

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcknowledgementExclusionsAndRestart(t *testing.T) {
	c, _ := New(Options{Mode: "eyes", Catalog: []byte(fixture)})
	dir := t.TempDir()
	var sends, inbounds atomic.Int32
	makeService := func() *Service {
		return &Service{Choice: c, Dir: dir, Send: func(context.Context, Input, Row) error { sends.Add(1); return nil }, Inbound: func(Input) { inbounds.Add(1) }}
	}
	in := Input{Conversation: "space-example", Message: "message-example", Text: "synthetic", Time: time.Now(), Seen: time.Now(), Addressed: true, Human: true}
	s := makeService()
	s.Observe(in)
	s.Wait()
	s.Observe(in)
	s.Wait()
	s = makeService()
	s.Observe(in)
	s.Wait()
	if sends.Load() != 1 {
		t.Fatal("restart repeated reaction")
	}
	for i := 0; i < 7; i++ {
		bad := in
		bad.Message += string(rune('a' + i))
		switch i {
		case 0:
			bad.Human = false
		case 1:
			bad.Replay = true
		case 2:
			bad.Edit = true
		case 3:
			bad.Own = true
		case 4:
			bad.Time = bad.Seen.Add(-MaxAge - time.Second)
		case 5:
			bad.Addressed = false
		case 6:
			bad.Time = time.Time{}
		}
		s.Observe(bad)
	}
	s.Wait()
	if sends.Load() != 1 || inbounds.Load() != 3 {
		t.Fatal("excluded event observed")
	}
}

func TestAcknowledgementDeliveryDoesNotWait(t *testing.T) {
	c, _ := New(Options{Mode: "eyes", Scope: "all", Catalog: []byte(fixture)})
	hold := make(chan struct{})
	started := make(chan struct{}, 9)
	s := &Service{Choice: c, Dir: t.TempDir(), Send: func(ctx context.Context, _ Input, _ Row) error { started <- struct{}{}; <-hold; return nil }}
	defer s.Wait()
	defer close(hold)
	for i := 0; i < 9; i++ {
		s.Observe(Input{Conversation: "space-example", Message: string(rune('a' + i)), Human: true, Time: time.Now(), Seen: time.Now()})
	}
	for i := 0; i < 9; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("reaction blocked another delivery")
		}
	}
}

func TestAcknowledgementOffStillTimesInbound(t *testing.T) {
	c, _ := New(Options{})
	var seen atomic.Int32
	s := &Service{Choice: c, Dir: t.TempDir(), Send: func(context.Context, Input, Row) error { t.Error("off made request"); return nil }, Inbound: func(Input) { seen.Add(1) }}
	s.Observe(Input{Message: "example", Human: true, Addressed: true, Time: time.Now(), Seen: time.Now()})
	s.Wait()
	if seen.Load() != 1 {
		t.Fatal("off suppressed stopwatch")
	}
}
