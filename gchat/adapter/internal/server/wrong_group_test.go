package server

// An answer's address is its message reference, regardless of other activity
// in the same session. All HTTP traffic stays within the loopback fixtures.

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

const (
	groupA         = "spaces/AAAA"
	groupB         = "spaces/BBBB"
	questionInA    = groupA + "/messages/QUESTION"
	questionThread = groupA + "/threads/QUESTION"
)

func wrongGroupFixture(t *testing.T) *fixture {
	t.Helper()
	f := setup(t)
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Spaces[groupA] = &api.Space{Name: groupA, SpaceType: "SPACE", DisplayName: "Group A"}
		g.Spaces[groupB] = &api.Space{Name: groupB, SpaceType: "SPACE", DisplayName: "Group B"}
		// The question as Google holds it: a message in A, in a thread of A.
		g.Messages[questionInA] = &api.Message{Name: questionInA, Text: "the question", Thread: &api.Thread{Name: questionThread}}
	})
	// gc routes both spaces to the same session, as a city's default route
	// does for its mayor.
	f.gc.Bind(conversation(groupA), "session")
	f.gc.Bind(conversation(groupB), "session")
	// The adapter's own record of the question: it knows the message and its
	// thread belong to A.
	f.mu.Lock()
	f.l.Messages[questionInA] = &ledger.MessageState{Thread: questionThread}
	f.l.Changes[questionInA] = &ledger.ChangeState{ID: questionInA, Space: groupA, Message: questionInA, Kind: "message.created", Status: "committed"}
	f.mu.Unlock()
	return f
}

// landedInB lists what the adapter posted into space B.
func landedInB(f *fixture) []string {
	var found []string
	f.g.Configure(func(g *fakegoogle.Server) {
		for name, m := range g.Messages {
			if strings.HasPrefix(name, groupB+"/messages/") {
				found = append(found, name+" (thread "+m.Thread.Name+")")
			}
		}
	})
	return found
}

func TestWrongGroupReferenceIsTheAddress(t *testing.T) {
	for _, tc := range []struct {
		name, space, thread string
		refuse              bool
	}{
		{"B wrote before A", "", "", false},
		{"B wrote after A", "", "", false},
		{"events newest first", "", "", false},
		{"events oldest first", "", "", false},
		{"bindings end in B", "", "", false},
		{"thread of B", "", groupB + "/threads/OTHER", true},
		{"space of B", groupB, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := wrongGroupFixture(t)
			order := []string{groupA, groupB}
			if tc.name == "B wrote before A" || tc.name == "events oldest first" {
				order = []string{groupB, groupA}
			}
			var events []map[string]any
			for i, space := range order {
				_, err := f.options.GC.Inbound(t.Context(), wire.InboundMessage{ProviderMessageID: space + "/messages/QUESTION", Conversation: conversation(space), Text: "question"})
				if err != nil {
					t.Fatal(err)
				}
				target := "session"
				if strings.HasPrefix(tc.name, "events") && space == groupA {
					target = ""
				}
				seq := i + 1
				if tc.name == "events newest first" {
					seq = len(order) - i
				}
				events = append(events, map[string]any{"seq": seq, "payload": map[string]any{"provider": "gchat", "conversation_id": space, "target_session": target}})
			}
			if tc.name == "bindings end in B" {
				events = nil
			}
			target, err := url.Parse(f.gc.URL())
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(target)
			var lookups atomic.Int32
			gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/events"):
					lookups.Add(1)
					respond(w, 200, map[string]any{"items": events})
				case strings.HasSuffix(r.URL.Path, "/extmsg/bindings"):
					lookups.Add(1)
					respond(w, 200, map[string]any{"items": []map[string]any{
						{"Status": "active", "Conversation": conversation(groupA)},
						{"Status": "active", "Conversation": conversation(groupB)},
					}})
				default:
					proxy.ServeHTTP(w, r)
				}
			}))
			defer gc.Close()
			f.s.SetClients(f.options.Chat, gcclient.New(gc.URL, "examplecity"))
			cmd := map[string]any{"session_id": "session", "to": questionInA, "conversation_id": tc.space,
				"thread": tc.thread, "text": "the answer", "idempotency_key": "reference"}
			code, raw, _ := f.call(t, "/commands/publish", cmd)
			if lookups.Load() != 0 {
				t.Fatal("reference send consulted events or bindings")
			}
			if tc.refuse {
				if code != 400 || f.count() != 1 || !strings.Contains(string(raw), groupB) || !strings.Contains(string(raw), groupA) {
					t.Fatalf("conflicting address was not refused: %d %s", code, raw)
				}
				return
			}
			result := decode[CommandResult](t, raw)
			if code != 200 || !result.Delivered || !result.Recorded || !strings.HasPrefix(result.MessageID, groupA+"/messages/") || len(landedInB(f)) != 0 {
				t.Fatalf("answer did not land in A: %d %s", code, raw)
			}
		})
	}
}
