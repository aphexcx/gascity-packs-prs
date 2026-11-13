package inbound

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestHistoryFloorKeepsNewActs(t *testing.T) {
	for _, reactionFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "unseen", true: "reaction-first"}[reactionFirst], func(t *testing.T) {
			f := setup(t)
			raises := 0
			f.opts.Raise = func(context.Context, Raise) error { raises++; return nil }
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			created, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
			f.l.HistoryFloor = created.Add(time.Hour)
			// An adopted subscription can carry a create payload with a later revision.
			// Suppressing that create must not consume the separate edit event.
			m.LastUpdateTime = f.l.HistoryFloor.Add(time.Minute).Format(time.RFC3339Nano)
			if reactionFirst {
				f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
				r := &api.Reaction{Name: m.Name + "/reactions/earlier", User: &api.User{Name: "users/222"}, Emoji: &api.Emoji{Unicode: "👍"}}
				handle(t, f, &puller.Event{PubSubID: "earlier-reaction", Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: r}}})
			}
			handle(t, f, event("old-create", "message.created", m))
			if len(f.gc.Inbound()) != 0 {
				t.Fatal("old create was a turn")
			}
			if err := f.p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			retained := f.l.Changes[m.Name]
			if retained.Status != "committed" || retained.Reason != "before_history_floor" || raises != 0 || len(f.gc.Inbound()) != 0 {
				t.Fatalf("suppressed create escaped: status=%s reason=%s raises=%d posts=%d", retained.Status, retained.Reason, raises, len(f.gc.Inbound()))
			}
			for _, s := range f.l.Spaces {
				if len(s.Backlog) != 0 {
					t.Fatal("suppressed create queued")
				}
			}
			var envelope wire.InboundMessage
			if err := json.Unmarshal([]byte(retained.Event), &envelope); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(envelope.Text, m.Text) || len(envelope.Attachments) != 0 {
				t.Fatal("suppressed create retained its old content")
			}
			handle(t, f, event("new-edit", "message.updated", m))
			if len(f.gc.Inbound()) != 1 {
				t.Fatalf("new edit lost: turns=%d", len(f.gc.Inbound()))
			}
			edit := f.gc.Inbound()[0]
			handle(t, f, event("new-delete", "message.deleted", &api.Message{Name: m.Name}))
			if len(f.gc.Inbound()) != 2 {
				t.Fatal("new deletion lost")
			}
			deleted := f.gc.Inbound()[1]
			if deleted.Actor != edit.Actor || strings.Split(deleted.Text, "\n")[0] != strings.Split(edit.Text, "\n")[0] {
				t.Errorf("deletion lost attribution: edit=%+v deletion=%+v", edit.Actor, deleted.Actor)
			}

			own := message()
			own.Name = space + "/messages/own"
			own.Sender.Name = identity
			handle(t, f, event("old-own", "message.created", own))
			reaction := &api.Reaction{Name: own.Name + "/reactions/one", User: &api.User{Name: "users/222"}, Emoji: &api.Emoji{Unicode: "👍"}}
			handle(t, f, &puller.Event{PubSubID: "new-reaction", Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: reaction}}})
			if len(f.gc.Inbound()) != 3 {
				t.Fatal("new reaction on old message lost")
			}
		})
	}
}
