package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/events"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	"github.com/BurntSushi/toml"
	api "google.golang.org/api/chat/v1"
	workspace "google.golang.org/api/workspaceevents/v1"
)

// Read the durable schema so these regressions also run against the old adapter.
func historyState(t *testing.T, f *fixture) (*ledger.Ledger, time.Time) {
	t.Helper()
	raw, err := os.ReadFile(f.cfg.Endpoints.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var l ledger.Ledger
	var fields struct {
		Floor time.Time `json:"history_floor"`
	}
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return &l, fields.Floor
}

func historyMessage(space, id string, at time.Time) *api.Message {
	return &api.Message{Name: space + "/messages/" + id, Text: id, CreateTime: at.UTC().Format(time.RFC3339Nano), Sender: &api.User{Name: "users/222", Type: "HUMAN"}, Thread: &api.Thread{Name: space + "/threads/" + id}}
}

func publishHistoryMessage(t *testing.T, f *fixture, m *api.Message) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"message": m})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pub.Publish(t.Context(), &pubsub.Message{Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": m.Name}}).Get(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func readHistory(t *testing.T, f *fixture, space string) []wire.InboundMessage {
	t.Helper()
	r, err := f.client.Get("http://adapter/read?space=" + space)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("read HTTP %d", r.StatusCode)
	}
	var messages []wire.InboundMessage
	if err := json.NewDecoder(r.Body).Decode(&messages); err != nil {
		t.Fatal(err)
	}
	return messages
}

func TestFirstStartReplaysNothing(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	old := []*api.Message{}
	now := time.Now().UTC()
	f.google.Configure(func(g *fakegoogle.Server) {
		for i, space := range []string{"spaces/room", "spaces/team", "spaces/dm", "spaces/direct"} {
			kind := "SPACE"
			if i >= 2 {
				kind = "DIRECT_MESSAGE"
			}
			g.Spaces[space] = &api.Space{Name: space, SpaceType: kind}
			for j, age := range []time.Duration{48 * time.Hour, 400 * 24 * time.Hour} {
				m := historyMessage(space, fmt.Sprintf("old%d", j), now.Add(-age))
				if i == 3 && j == 1 {
					m.Sender.Name = fakegoogle.UserID
				}
				g.Messages[m.Name] = m
				old = append(old, m)
			}
		}
		edit := *old[0]
		edit.LastUpdateTime = now.Add(-time.Hour).Format(time.RFC3339Nano)
		reaction := &api.Reaction{Name: old[len(old)-1].Name + "/reactions/one", User: &api.User{Name: "users/222"}, Emoji: &api.Emoji{Unicode: "👍"}}
		g.SpaceEvents["spaces/room"] = []*api.SpaceEvent{{Name: "spaces/room/spaceEvents/edit", EventType: "google.workspace.chat.message.v1.updated", EventTime: edit.LastUpdateTime, MessageUpdatedEventData: &api.MessageUpdatedEventData{Message: &edit}}}
		g.SpaceEvents["spaces/direct"] = []*api.SpaceEvent{{Name: "spaces/direct/spaceEvents/reaction", EventType: "google.workspace.chat.reaction.v1.created", EventTime: now.Add(-time.Hour).Format(time.RFC3339Nano), ReactionCreatedEventData: &api.ReactionCreatedEventData{Reaction: reaction}}}
	})
	var notices atomic.Int32
	f.startOptions(t, Options{Notify: func(context.Context, string, string) error { notices.Add(1); return nil }})
	f.health(t, "ready", 10*time.Second)
	fresh := historyMessage("spaces/dm", "new", time.Now())
	publishHistoryMessage(t, f, fresh)
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range f.gc.Inbound() {
			if m.ProviderMessageID == fresh.Name {
				return true
			}
		}
		return false
	})
	turns := f.gc.Inbound()
	t.Logf("first start: %d turns, %d old messages, %d notifications", len(turns), len(old), notices.Load())
	if len(turns) != 1 || turns[0].ProviderMessageID != fresh.Name {
		t.Errorf("first start delivered %d turns; want only the new direct message", len(turns))
	}
	l, _ := historyState(t, f)
	for name, s := range l.Spaces {
		if len(s.Backlog) != 0 {
			t.Errorf("%s backlog = %d", name, len(s.Backlog))
		}
	}
	for _, m := range old {
		if l.Messages[m.Name] != nil {
			t.Errorf("old message touched: %s", m.Name)
		}
		for _, c := range l.Changes {
			if c.Message == m.Name {
				t.Errorf("old change recorded: %s", c.ID)
			}
		}
		space := strings.Split(m.Name, "/messages/")[0]
		found := false
		for _, got := range readHistory(t, f, space) {
			if got.ProviderMessageID == m.Name {
				found = true
			}
		}
		if !found {
			t.Errorf("old message missing from read: %s", m.Name)
		}
	}
	if notices.Load() != 0 {
		t.Errorf("notifications = %d", notices.Load())
	}
}

func TestFirstStartWritesTheFloorBeforeTheMarker(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(fmt.Sprint("saved=", saved), func(t *testing.T) {
			f := newFixture(t, false)
			if saved {
				raw, err := os.ReadFile("../ledger/testdata/ledger-0.5.0.json")
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, f.cfg.Endpoints.StateFile, string(raw))
			}
			before := time.Now().UTC()
			f.start(t)
			f.health(t, "needs-sign-in", 5*time.Second)
			_, floor := historyState(t, f)
			if !ledger.Established(f.cfg.SecretsDir) {
				t.Fatal("marker absent")
			}
			if saved && !floor.IsZero() {
				t.Fatal("old ledger gained a floor")
			}
			if !saved && (floor.Before(before) || floor.After(time.Now())) {
				t.Fatalf("first saved floor = %s", floor)
			}
		})
	}
}

func TestFloorMovesUntilTheFirstSubscription(t *testing.T) {
	f := newFixture(t, false)
	f.start(t)
	f.health(t, "needs-sign-in", 5*time.Second)
	_, initial := historyState(t, f)
	connected := time.Now().UTC()
	f.replaceIdentityToken(t, fakegoogle.UserID)
	f.health(t, "ready", 15*time.Second)
	l, floor := historyState(t, f)
	if initial.IsZero() || floor.Before(connected) || !floor.After(initial) || l.Subscription.Name == "" {
		t.Fatalf("initial=%s connected=%s floor=%s subscription=%s", initial, connected, floor, l.Subscription.Name)
	}
	subscriptionReads := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, request := range f.requests {
			if strings.HasPrefix(request, "google GET /v1/subscriptions") {
				n++
			}
		}
		return n
	}
	requests := subscriptionReads()
	f.replaceIdentityToken(t, fakegoogle.UserID)
	waitFor(t, 15*time.Second, func() bool { return subscriptionReads() > requests })
	f.health(t, "ready", 15*time.Second)
	_, later := historyState(t, f)
	if !later.Equal(floor) {
		t.Fatalf("recorded subscription moved floor: %s -> %s", floor, later)
	}
}

func TestFirstSavedFloorSurvivesMarkerFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	f := newFixture(t, false)
	f.cfg.SecretsDir = filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(f.cfg.SecretsDir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.cfg.SecretsDir, 0700) })
	before := time.Now().UTC()
	err := RunWithOptions(t.Context(), f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
	if err == nil || !strings.Contains(err.Error(), "mark ledger established") {
		t.Fatalf("marker failure=%v", err)
	}
	_, floor := historyState(t, f)
	if floor.Before(before) || floor.After(time.Now()) {
		t.Fatalf("first saved floor=%s", floor)
	}
	if ledger.Established(f.cfg.SecretsDir) {
		t.Fatal("failed marker write established state")
	}
}

func TestLiveMessageBeforeTheFloorIsNotATurn(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
		g.Subscriptions["subscriptions/adopted"] = &workspace.Subscription{Name: "subscriptions/adopted", State: "ACTIVE", TargetResource: events.Target, EventTypes: append([]string(nil), events.EventTypes...), ExpireTime: time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339Nano), PayloadOptions: &workspace.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &workspace.NotificationEndpoint{PubsubTopic: f.cfg.Secrets.Topic}}
	})
	var notices atomic.Int32
	f.startOptions(t, Options{Notify: func(context.Context, string, string) error { notices.Add(1); return nil }})
	f.health(t, "ready", 5*time.Second)
	l, _ := historyState(t, f)
	if l.Subscription.Name != "subscriptions/adopted" {
		t.Fatalf("subscription was not adopted: %s", l.Subscription.Name)
	}
	old := historyMessage("spaces/dm", "old-live", time.Now().Add(-365*24*time.Hour))
	publishHistoryMessage(t, f, old)
	waitFor(t, 5*time.Second, func() bool {
		l, _ := historyState(t, f)
		return l.Changes[old.Name] != nil && l.Changes[old.Name].Status == "committed"
	})
	l, _ = historyState(t, f)
	if got := l.Changes[old.Name]; got.Reason != "local" || got.Cause != "before_history_floor" {
		t.Errorf("old live reason = %q; posts = %d", got.Reason, len(f.gc.Inbound()))
	}
	if len(f.gc.Inbound()) != 0 || notices.Load() != 0 {
		t.Errorf("old live message: posts=%d notices=%d", len(f.gc.Inbound()), notices.Load())
	}
	fresh := historyMessage("spaces/dm", "fresh-live", time.Now())
	publishHistoryMessage(t, f, fresh)
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range f.gc.Inbound() {
			if m.ProviderMessageID == fresh.Name {
				return true
			}
		}
		return false
	})
}

func TestFirstStartHistoryIsReadableAndNeverATurn(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	m := historyMessage("spaces/room", "old", time.Now().Add(-time.Hour))
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		g.Messages[m.Name] = m
	})
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	got := readHistory(t, f, "spaces/room")
	if len(got) != 1 || got[0].ProviderMessageID != m.Name {
		t.Fatalf("read: %+v", got)
	}
	l, _ := historyState(t, f)
	if l.Changes[m.Name] != nil || len(f.gc.Inbound()) != 0 {
		t.Fatal("readable history became a turn")
	}
}

func runHistoryFixture(t *testing.T, f *fixture) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }, RetryDelay: time.Millisecond})
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("stop timed out")
		}
	}
	t.Cleanup(stop)
	f.health(t, "ready", 5*time.Second)
	return stop
}

func TestRestartWithALedgerStillDeliversTheOutage(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	stop := runHistoryFixture(t, f)
	stop()
	f.google.Configure(func(g *fakegoogle.Server) {
		for i := range 3 {
			m := historyMessage("spaces/room", fmt.Sprintf("outage%d", i), time.Now())
			g.Messages[m.Name] = m
		}
	})
	runHistoryFixture(t, f)
	turns := f.gc.Inbound()
	if len(turns) != 3 {
		t.Fatalf("outage turns=%d", len(turns))
	}
	seen := map[string]bool{}
	for _, m := range turns {
		if seen[m.ProviderMessageID] || !strings.Contains(m.Text, "[catch-up]") {
			t.Errorf("outage turn=%+v", m)
		}
		seen[m.ProviderMessageID] = true
	}
}

func TestFirstStartWindowReachesBack(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	if _, err := toml.Decode("first_start_history = \"24h\"", &f.cfg.Settings); err != nil {
		t.Fatal(err)
	}
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		for _, hours := range []int{23, 25} {
			m := historyMessage("spaces/room", fmt.Sprintf("age%d", hours), time.Now().Add(-time.Duration(hours)*time.Hour))
			g.Messages[m.Name] = m
		}
	})
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	got := f.gc.Inbound()
	if len(got) != 1 || got[0].ProviderMessageID != "spaces/room/messages/age23" || !strings.Contains(got[0].Text, "[catch-up]") {
		t.Fatalf("window delivered %d turns: %+v", len(got), got)
	}
}

func TestResetTakesTheFirstStartBound(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	old := historyMessage("spaces/room", "before-reset", time.Now().Add(-time.Hour))
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		g.Messages[old.Name] = old
	})
	writeFile(t, f.cfg.Endpoints.StateFile, "broken ledger")
	f.start(t)
	f.health(t, "recovery-required", 5*time.Second)
	r, err := f.client.Post("http://adapter/state", "application/json", strings.NewReader(`{"action":"reset"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("reset HTTP %d", r.StatusCode)
	}
	f.health(t, "ready", 5*time.Second)
	fresh := historyMessage("spaces/room", "after-reset", time.Now())
	publishHistoryMessage(t, f, fresh)
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range f.gc.Inbound() {
			if m.ProviderMessageID == fresh.Name {
				return true
			}
		}
		return false
	})
	l, floor := historyState(t, f)
	if floor.IsZero() || l.Changes[old.Name] != nil || len(f.gc.Inbound()) != 1 {
		t.Fatalf("reset floor=%s turns=%d old=%+v", floor, len(f.gc.Inbound()), l.Changes[old.Name])
	}
}
