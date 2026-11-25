package app

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	api "google.golang.org/api/chat/v1"
)

func TestSenderFillWiring(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	const room = "spaces/example"
	f.google.Configure(func(g *fakegoogle.Server) { g.Spaces[room] = &api.Space{Name: room, SpaceType: "DIRECT_MESSAGE"} })
	boundStart(t, f, &atomic.Int32{})
	for _, row := range []struct{ id, label, name string }{
		{fakegoogle.UserID, "self", "Example Identity"},
		{"users/1", "other", "Example Reader"},
	} {
		// UNPROVEN: the live sender is bare, while messages.get returns both fields.
		m := historyMessage(room, row.label, time.Now())
		m.Space = &api.Space{Name: room, SpaceType: "DIRECT_MESSAGE"}
		m.Sender = &api.User{Name: row.id, Type: "HUMAN"}
		answer := *m
		answer.Sender = &api.User{Name: row.id, DisplayName: row.name, DomainId: "example-org", Type: "HUMAN"}
		f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = &answer })
		publishHistoryMessage(t, f, m)
		waitFor(t, 10*time.Second, func() bool { l, _ := historyState(t, f); return l.Transport.Seen("cloudevent:" + m.Name) })
		if row.label == "self" {
			l, _ := historyState(t, f)
			if l.IdentityDomainID != "example-org" {
				t.Errorf("own message taught domain %q", l.IdentityDomainID)
			}
		}
	}
	turns := f.gc.Inbound()
	if len(turns) != 1 || turns[0].Actor.DisplayName != "Example Reader" || !strings.Contains(turns[0].Text, "[from: Example Reader <users/1> | organization") {
		t.Fatalf("turns = %+v", turns)
	}
}
