package inbound

import (
	"testing"
	"time"

	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

func TestHistoryFloorKeepsNewActs(t *testing.T) {
	f := setup(t)
	m := message()
	created, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
	f.l.HistoryFloor = created.Add(time.Hour)
	// An adopted subscription can carry a create payload with a later revision.
	// Suppressing that create must not consume the separate edit event.
	m.LastUpdateTime = f.l.HistoryFloor.Add(time.Minute).Format(time.RFC3339Nano)
	handle(t, f, event("old-create", "message.created", m))
	if len(f.gc.Inbound()) != 0 {
		t.Fatal("old create was a turn")
	}
	handle(t, f, event("new-edit", "message.updated", m))
	if len(f.gc.Inbound()) != 1 {
		t.Fatalf("new edit lost: turns=%d", len(f.gc.Inbound()))
	}
	handle(t, f, event("new-delete", "message.deleted", m))
	if len(f.gc.Inbound()) != 2 {
		t.Fatal("new deletion lost")
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
}
