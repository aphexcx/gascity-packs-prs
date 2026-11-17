package inbound

import (
	"testing"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

// With no acknowledgement service, a reaction is a turn exactly as at the
// PR's base: the identity's reaction on its own message is a turn, and only a
// reaction on someone else's message (the acknowledgement's echo) stays local.
func TestOwnReactionOnOwnMessageIsATurn(t *testing.T) {
	for _, tc := range []struct {
		name, author, reactor string
		want                  int
	}{
		{"identity reacts on its own message", identity, identity, 1},
		{"a human reacts on the identity's message", identity, "users/456", 1},
		{"the identity reacts on a human's message", "users/456", identity, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			m := message()
			m.Sender.Name = tc.author
			f.google.Configure(func(s *fakegoogle.Server) { s.Messages[m.Name] = m })
			r := &api.Reaction{Name: messageName + "/reactions/example", User: &api.User{Name: tc.reactor}, Emoji: &api.Emoji{Unicode: "👍"}}
			handle(t, f, &puller.Event{PubSubID: "reaction-example", Changes: []puller.Change{{Kind: "reaction.created", Space: space, Reaction: r}}})
			if got := len(f.gc.Inbound()); got != tc.want {
				t.Fatalf("turns POSTed = %d, want %d", got, tc.want)
			}
		})
	}
}
