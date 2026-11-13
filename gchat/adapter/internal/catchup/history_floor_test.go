package catchup

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

func setHistoryFloor(t *testing.T, l *ledger.Ledger, floor time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"history_floor": floor})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, l); err != nil {
		t.Fatal(err)
	}
}

func TestFloorBoundsEveryNewSpace(t *testing.T) {
	f := setup(t)
	floor := frozen.Add(-time.Minute)
	setHistoryFloor(t, f.opts.Ledger, floor)
	for _, space := range []string{"spaces/room", "spaces/dm"} {
		f.addSpace(space, false)
		if space == "spaces/dm" {
			f.google.Spaces[space].SpaceType = "DIRECT_MESSAGE"
		}
		f.addMessage(space, 1, floor.Add(-time.Hour))
		f.addMessage(space, 2, floor.Add(-30*time.Second))
		f.addMessage(space, 3, floor.Add(time.Second))
	}
	f.freeze(t)
	for name, s := range f.opts.Ledger.Spaces {
		if !s.MessageCursor.Equal(floor) || !s.EventCursor.Equal(floor) || !s.JoinedAt.IsZero() {
			t.Errorf("%s cursors=%s/%s join=%s", name, s.MessageCursor, s.EventCursor, s.JoinedAt)
		}
		p, err := decodeProgress(s.PageToken)
		if err != nil || !p.MessageLower.Equal(floor) || !p.EventLower.Equal(floor) {
			t.Errorf("bounds: %+v %v", p, err)
		}
	}
	f.pass(t, 2, 0)
	// A later inventory must apply the original floor to newly discovered spaces.
	f.addSpace("spaces/later", false)
	f.addMessage("spaces/later", 1, floor.Add(-time.Hour))
	f.addMessage("spaces/later", 2, floor.Add(time.Second))
	f.freeze(t)
	f.pass(t, 1, 0)
	for name, count := range f.opts.Ledger.Counters {
		if strings.HasPrefix(name, "unrecoverable:") && count != 0 {
			t.Fatalf("history loss reported: %s=%d", name, count)
		}
	}
	if strings.Contains(f.logs.String(), "WARN") {
		t.Fatalf("history warning: %s", &f.logs)
	}
}

func TestJoinTimeAndFirstStartAreOneRule(t *testing.T) {
	f := setup(t)
	floor, joined := frozen.Add(-3*time.Minute), frozen.Add(-time.Minute)
	setHistoryFloor(t, f.opts.Ledger, floor)
	f.addSpace("spaces/room", false)
	f.addMessage("spaces/room", 1, floor.Add(-time.Second))
	f.addMessage("spaces/room", 2, floor.Add(time.Second))
	f.addMessage("spaces/room", 3, joined.Add(time.Second))
	_, err := f.p.HandleChange(t.Context(), puller.Change{Kind: "membership.created", Space: "spaces/room", Membership: &api.Membership{Name: "spaces/room/members/123", State: "JOINED", CreateTime: joined.Format(time.RFC3339Nano), Member: &api.User{Name: "users/123"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := f.opts.Ledger.Spaces["spaces/room"]
	if !s.JoinedAt.Equal(joined) || !s.MessageCursor.Equal(joined) || !s.EventCursor.Equal(joined) {
		t.Fatalf("join cursors: %+v", s)
	}
	f.freeze(t)
	f.pass(t, 1, 0)
	if got := f.gc.Inbound()[0].ProviderMessageID; got != "spaces/room/messages/m0003" {
		t.Fatal(got)
	}
}

func TestHistoryFloorSurvivesRetentionFallback(t *testing.T) {
	for _, joined := range []bool{false, true} {
		for _, kind := range []string{"message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
			for _, batch := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/batch=%v/joined=%v", kind, batch, joined), func(t *testing.T) {
					f := setup(t)
					f.pageSize = 1 // The rejected page must not stop pagination.
					floor := frozen.Add(-28*24*time.Hour + 2*time.Minute)
					setHistoryFloor(t, f.opts.Ledger, floor)
					f.opts.Ledger.Subscription.Name = "subscriptions/existing"
					f.addSpace("spaces/room", true)
					s := f.opts.Ledger.Spaces["spaces/room"]
					bound := floor
					if joined {
						bound = floor.Add(time.Minute)
						s.JoinedAt = bound
					}
					s.MessageCursor, s.EventCursor = bound.Add(10*time.Second), bound.Add(10*time.Second)
					parts := strings.Split(kind, ".")
					resource, action := parts[0], parts[1]
					count := 1
					if batch {
						count = 2
						action = "batch" + strings.ToUpper(action[:1]) + action[1:]
					}
					for eventIndex, label := range []string{"old", "new"} {
						at := bound.Add(time.Duration(2*eventIndex-1) * 30 * time.Second)
						var items []map[string]any
						for n := 0; n < count; n++ {
							m := f.addMessage("spaces/room", eventIndex*10+n, floor.Add(-10*time.Minute))
							m.LastUpdateTime = at.Format(time.RFC3339Nano)
							var value any = m
							if resource == "reaction" {
								m.Sender.Name = "users/123"
								value = &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/456"}, Emoji: &api.Emoji{Unicode: "👍"}}
							}
							items = append(items, map[string]any{resource: value})
						}
						var payload any = items[0]
						if batch {
							payload = map[string]any{resource + "s": items}
						}
						raw, err := json.Marshal(map[string]any{
							"name": "spaces/room/spaceEvents/" + label, "eventTime": at.Format(time.RFC3339Nano),
							"eventType": "google.workspace.chat." + resource + ".v1." + action,
							resource + strings.ToUpper(action[:1]) + action[1:] + "EventData": payload,
						})
						if err != nil {
							t.Fatal(err)
						}
						var event api.SpaceEvent
						if err := json.Unmarshal(raw, &event); err != nil {
							t.Fatal(err)
						}
						f.google.SpaceEvents["spaces/room"] = append(f.google.SpaceEvents["spaces/room"], &event)
					}
					f.freeze(t)
					delivered, remaining, err := f.r.Pass(t.Context())
					want := count
					if kind == "message.deleted" {
						want = 0 // A previously unseen deletion only records its tombstone.
					}
					if err != nil || delivered != want || remaining != 0 {
						t.Errorf("Pass = (%d, %d, %v), want (%d, 0, nil)", delivered, remaining, err, want)
					}
					for n := 0; n < count; n++ {
						if f.opts.Ledger.Messages[fmt.Sprintf("spaces/room/messages/m%04d", n)] != nil {
							t.Error("pre-bound event reached message state")
						}
						if f.opts.Ledger.Messages[fmt.Sprintf("spaces/room/messages/m%04d", 10+n)] == nil {
							t.Error("post-bound event was lost")
						}
					}
					if len(f.opts.Ledger.Changes) != count || len(f.opts.Ledger.Spaces["spaces/room"].Backlog) != 0 {
						t.Errorf("retained changes=%d, want %d; backlog=%d", len(f.opts.Ledger.Changes), count, len(f.opts.Ledger.Spaces["spaces/room"].Backlog))
					}
				})
			}
		}
	}
}
