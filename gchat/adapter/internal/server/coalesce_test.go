package server

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	"github.com/BurntSushi/toml"
	api "google.golang.org/api/chat/v1"
)

func TestCoalescingStatus(t *testing.T) {
	f := setup(t)
	if _, err := toml.Decode(`coalesce_window = "8s"`, &f.cfg.Settings); err != nil {
		t.Fatal(err)
	}
	f.s.SetConfig(*f.cfg)
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC, IdentityUserID: "users/123"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		m := &api.Message{Name: "spaces/room/messages/" + id, Text: "example", CreateTime: time.Now().UTC().Format(time.RFC3339Nano), Sender: &api.User{Name: "users/456"}, Space: &api.Space{Name: "spaces/room", SpaceType: "SPACE"}}
		if out, err := p.Handle(context.Background(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: m}}}); out != puller.Ack || err != nil {
			t.Fatal(out, err)
		}
	}
	code, raw, _ := f.call(t, "/status", nil)
	var s struct {
		Window  string `json:"coalesce_window"`
		Held    int    `json:"held_acts"`
		Stuck   int    `json:"stuck"`
		Pending int    `json:"pending"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	if code != 200 || s.Window != "8s" || s.Held != 2 || s.Stuck != 0 || s.Pending != 0 {
		t.Fatalf("status=%s", raw)
	}
}

func TestMergedResolveAndHistoryReaders(t *testing.T) {
	for _, action := range []string{"replay", "accept"} {
		t.Run(action, func(t *testing.T) {
			f := setup(t)
			_, _ = toml.Decode(`coalesce_window = "8s"`, &f.cfg.Settings)
			f.s.SetConfig(*f.cfg)
			now := time.Now().UTC()
			p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC, IdentityUserID: "users/123", Now: func() time.Time { return now }, RetryDelay: time.Nanosecond})
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetPoll(p.Pass)
			f.gc.Script = []wire.Delivery{{Status: "partial"}, {Status: "partial"}, {Status: "partial"}, {Status: "partial"}}
			var messages []*api.Message
			for i := 0; i < 2; i++ {
				m := &api.Message{Name: fmt.Sprintf("spaces/room/messages/m%d", i), Text: fmt.Sprintf("own-text-%d", i), CreateTime: now.Format(time.RFC3339Nano), Sender: &api.User{Name: "users/456"}, Space: &api.Space{Name: "spaces/room", SpaceType: "SPACE"}}
				messages = append(messages, m)
				if out, err := p.Handle(context.Background(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: m}}}); out != puller.Ack || err != nil {
					t.Fatal(out, err)
				}
			}
			now = now.Add(8 * time.Second)
			if err := p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.l.Unresolved) != 1 {
				t.Fatalf("unresolved=%d", len(f.l.Unresolved))
			}
			id := ""
			for key := range f.l.Unresolved {
				id = key
			}
			code, raw, _ := f.call(t, "/status", nil)
			var status struct {
				Deliveries []struct {
					Members int `json:"members"`
				} `json:"deliveries"`
			}
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatal(err)
			}
			if code != 200 || len(status.Deliveries) != 1 || status.Deliveries[0].Members != 2 {
				t.Fatalf("status=%s", raw)
			}
			for _, m := range messages {
				got, ok := historyContent(f.l, m)
				if !ok || got.Text != m.Text {
					t.Fatalf("history %s=%q, %v", m.Name, got.Text, ok)
				}
			}
			posts := f.gc.Inbound()
			if len(posts) != 4 {
				t.Fatalf("posts=%d", len(posts))
			}
			code, raw, _ = f.call(t, "/resolve", map[string]string{"id": id, "action": action})
			if code != 200 || len(f.l.Unresolved) != 0 {
				t.Fatalf("resolve=%d %s", code, raw)
			}
			want := 4
			if action == "replay" {
				want = 5
			}
			posts = f.gc.Inbound()
			if len(posts) != want {
				t.Fatalf("posts=%d", len(posts))
			}
			if action == "replay" && (posts[4].Text != posts[0].Text || posts[4].ProviderMessageID != posts[0].ProviderMessageID) {
				t.Fatal("replay changed bytes")
			}
			for _, c := range f.l.Changes {
				if f.l.Delivery(c).Status != "committed" {
					t.Fatal("member not settled")
				}
			}
		})
	}
}
