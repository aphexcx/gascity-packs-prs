package inbound

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// gc's renderer lives outside this repository. This is the envelope formula
// used by the repository's transport fakes, not a live gc measurement.
func budgetEnvelope(w wire.InboundMessage) string {
	return fmt.Sprintf("<system-reminder>\nNew message in shared conversation gchat/%s:\n\n- %s (human): %s\n\nMessage id: %s (in thread %s)\n\ngc gchat reply-current --to %s --body-file <path>\n</system-reminder>",
		w.Conversation.ConversationID, w.Actor.DisplayName, w.Text, w.ProviderMessageID, w.ReplyToMessageID, w.ProviderMessageID)
}

func budgetMessages(single bool) []*api.Message {
	bodies := []string{"Morning all.", "The plumber is confirmed for unit 4.", "Great, thanks.", "I will let the tenant know.", "Perfect."}
	if single {
		bodies = []string{"Can you send me the rent roll for March?"}
	}
	var messages []*api.Message
	for i, body := range bodies {
		id := fmt.Sprintf("m%daaaaaaaaa", i+1)
		if single {
			id = "abcDEF12345"
		}
		user := &api.User{Name: "users/dana", DisplayName: "Dana Example", Type: "HUMAN"}
		if i == 2 || i == 3 {
			user = &api.User{Name: "users/lee", DisplayName: "Lee Sample", Type: "HUMAN"}
		}
		messages = append(messages, &api.Message{Name: "spaces/AAAA_XXXXXX/messages/" + id + "." + id, Text: body,
			CreateTime: fmt.Sprintf("2026-11-01T10:00:%02dZ", i), Sender: user,
			Space:  &api.Space{Name: "spaces/AAAA_XXXXXX", SpaceType: "SPACE"},
			Thread: &api.Thread{Name: "spaces/AAAA_XXXXXX/threads/" + id}})
	}
	return messages
}

func budgetPrincipals() *principal.Resolver {
	return principal.New(principal.Options{Owners: []string{"dana@example.com"}, OrganizationDomain: "example.com",
		Lookup: func(_ context.Context, id string) (string, error) {
			return strings.TrimPrefix(id, "users/") + "@example.com", nil
		}})
}

func TestBaselineTurnBytes(t *testing.T) {
	for _, row := range []struct {
		name       string
		single, dm bool
		fixed      int
	}{{"room", true, false, 452}, {"dm", true, true, 452}, {"five", false, false, 2264}} {
		t.Run(row.name, func(t *testing.T) {
			f := setup(t)
			f.opts.Principals = budgetPrincipals()
			var err error
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			words := 0
			for i, m := range budgetMessages(row.single) {
				if row.dm {
					m.Space.SpaceType = "DIRECT_MESSAGE"
				}
				e := event(fmt.Sprint(i), "message.created", m)
				e.Changes[0].Space = m.Space.Name
				handle(t, f, e)
				words += len(m.Text)
			}
			adapter, total := 0, 0
			for _, w := range f.gc.Inbound() {
				adapter += len(w.Text)
				total += len(budgetEnvelope(w))
				t.Logf("CAPTURE\n%s", budgetEnvelope(w))
			}
			t.Logf("BYTES turns=%d words=%d adapter_fixed=%d whole_fixed=%d total=%d", len(f.gc.Inbound()), words, adapter-words, total-words, total)
			if total-words != row.fixed {
				t.Fatalf("fixed=%d want=%d", total-words, row.fixed)
			}
		})
	}
}

func TestTurnFixedBytesBudget(t *testing.T) {
	for _, n := range []int{1, 2, 5, 50} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := windowSetup(t, "8s")
			messages := budgetMessages(n == 1)
			words, frames := 0, 0
			for i := 0; i < n; i++ {
				m := *messages[i%len(messages)]
				if i >= len(messages) {
					m.Name = fmt.Sprintf("spaces/AAAA_XXXXXX/messages/extra%03d", i)
				}
				words += len(m.Text)
				e := event(fmt.Sprint(i), "message.created", &m)
				e.Changes[0].Space = m.Space.Name
				handle(t, f.fixture, e)
				// The unchanged single frame is the per-member budget allowance.
				frames += len("[from: " + m.Sender.DisplayName + " <" + strings.TrimPrefix(m.Sender.Name, "users/") + "@example.com> | ")
				if m.Sender.Name == "users/dana" {
					frames += len("owner")
				} else {
					frames += len("organization")
				}
				frames += len(" | item gchat:" + m.Name + "]")
			}
			f.advance(t, 8*time.Second)
			w := f.turns(t, 1)[0]
			own := len(w.Text) - words
			fixed := len(budgetEnvelope(w)) - words
			t.Logf("BYTES members=%d words=%d adapter_fixed=%d whole_fixed=%d total=%d", n, words, own, fixed, len(budgetEnvelope(w)))
			t.Logf("CAPTURE\n%s", budgetEnvelope(w))
			if n == 1 {
				if own != frames+1 || fixed > 452 {
					t.Fatal("single grew")
				}
			} else if own > 80+frames+2*n {
				t.Fatal("adapter budget exceeded")
			}
			if n == 5 && fixed > 1000 {
				t.Fatalf("five fixed=%d", fixed)
			}
			if n == 2 && fixed >= 904 {
				t.Fatal("two no smaller than singles")
			}
		})
	}
}
