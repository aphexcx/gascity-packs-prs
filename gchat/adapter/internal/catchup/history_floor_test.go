package catchup

import (
	"encoding/json"
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
