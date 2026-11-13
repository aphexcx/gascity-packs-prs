package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/puller"
	"github.com/BurntSushi/toml"
	api "google.golang.org/api/chat/v1"
)

func TestCoalescingStatus(t *testing.T) {
	f := setup(t)
	if _, err := toml.Decode(`coalesce_window = "8s"`, &f.cfg.Settings); err != nil {
		t.Fatal(err)
	}
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.opts.LedgerPath, Chat: f.opts.Chat, GC: f.opts.GC, IdentityUserID: "users/123"})
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
