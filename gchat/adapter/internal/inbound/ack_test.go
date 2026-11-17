package inbound

import (
	"context"
	"fmt"
	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestNineArrivalsDeliverWhileJevIsHeld(t *testing.T) {
	f := setup(t)
	var active, maximum atomic.Int32
	stop := make(chan struct{})
	vendor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		n := active.Add(1)
		defer active.Add(-1)
		for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
		}
		select {
		case <-stop:
		case <-r.Context().Done():
		}
	}))
	defer vendor.Close()
	defer close(stop)
	choice, err := ack.New(ack.Options{Mode: "jev", Key: "test-credential", Base: vendor.URL, Catalog: ack.Catalog})
	if err != nil {
		t.Fatal(err)
	}
	reactions := make(chan string, 9)
	service := &ack.Service{Choice: choice, Dir: t.TempDir(), Send: func(_ context.Context, in ack.Input, row ack.Row) error {
		if row.Name != "eyes" {
			t.Error("not default")
		}
		reactions <- in.Message
		return nil
	}}
	f.opts.Ack = service
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		m := message()
		m.Name = fmt.Sprintf("%s/messages/arrival-%d", space, i)
		m.CreateTime = time.Now().Format(time.RFC3339Nano)
		m.Space.SpaceType = "DIRECT_MESSAGE"
		handle(t, f, event(fmt.Sprintf("arrival-transport-%d", i), "message.created", m))
	}
	if len(f.gc.Inbound()) != 9 {
		t.Fatal("held choice blocked delivery")
	}
	seen := map[string]bool{}
	for i := 0; i < 9; i++ {
		select {
		case id := <-reactions:
			if seen[id] {
				t.Fatal("duplicate reaction")
			}
			seen[id] = true
		case <-time.After(3 * time.Second):
			t.Fatal("missing reaction")
		}
	}
	service.Wait()
	if maximum.Load() != 4 {
		t.Fatalf("maximum choices=%d", maximum.Load())
	}
}

func TestArrivalRunsBesideDeliveryAndOwnReactionIsLocal(t *testing.T) {
	f := setup(t)
	arrived := make(chan ack.Input, 1)
	hold := make(chan struct{})
	choice, _ := ack.New(ack.Options{Mode: "eyes", Catalog: ack.Catalog})
	service := &ack.Service{Choice: choice, Dir: t.TempDir(), Send: func(_ context.Context, in ack.Input, _ ack.Row) error { arrived <- in; <-hold; return nil }}
	defer service.Wait()
	defer close(hold)
	f.opts.Ack = service
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	m := message()
	m.CreateTime = time.Now().Format(time.RFC3339Nano)
	m.Space.SpaceType = "DIRECT_MESSAGE"
	handle(t, f, event("transport-example", "message.created", m))
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("delivery blocked")
	}
	select {
	case in := <-arrived:
		if in.Message != m.Name {
			t.Fatal(in.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("no arrival reaction")
	}
	handle(t, f, &puller.Event{PubSubID: "reaction-example", Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: &api.Reaction{Name: m.Name + "/reactions/example", User: &api.User{Name: identity}, Emoji: &api.Emoji{Unicode: "👀"}}}}})
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("own reaction woke agent")
	}
}
