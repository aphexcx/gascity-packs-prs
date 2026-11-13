package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func timedAct(t *testing.T, kind string, m *api.Message, at time.Time, id string) *puller.Event {
	t.Helper()
	parts := strings.Split(kind, ".")
	var value any = m
	if parts[0] == "reaction" {
		value = &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/222"}, Emoji: &api.Emoji{Unicode: "+"}}
	}
	raw, _ := json.Marshal(map[string]any{parts[0]: value})
	ev, err := puller.Decode(&pubsub.Message{ID: id, Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat." + parts[0] + ".v1." + parts[1], "ce-time": at.Format(time.RFC3339Nano)}})
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestLiveActsBeforeTheFloorAreNotTurns(t *testing.T) {
	for _, order := range [][]string{{"message.created"}, {"message.updated"}, {"message.deleted"}, {"reaction.created"}, {"reaction.deleted"}, {"message.created", "message.updated"}, {"reaction.created", "reaction.deleted"}, {"message.updated", "message.deleted"}} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			f := setup(t)
			floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			f.l.HistoryFloor = floor
			var raises int
			f.opts.Raise = func(context.Context, Raise) error { raises++; return nil }
			f.p, _ = New(f.opts)
			m := message()
			m.LastUpdateTime = floor.Add(-time.Hour).Format(time.RFC3339Nano)
			if strings.HasPrefix(order[0], "reaction") {
				m.Sender.Name = identity
			}
			f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
			for i, kind := range order {
				ev := timedAct(t, kind, m, floor.Add(-time.Hour), fmt.Sprint(i))
				handle(t, f, ev)
				handle(t, f, ev)
			}
			for range 3 {
				if err := f.p.Poll(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if n := len(f.gc.Inbound()); n != 0 {
				t.Errorf("old acts became %d turns", n)
			}
			if raises != 0 || len(f.l.Unresolved) != 0 {
				t.Error("old acts raised or unresolved")
			}
			for _, c := range f.l.Changes {
				if c.Status != "committed" {
					t.Error("old act not settled")
				}
				if strings.Contains(c.Event, m.Text) {
					t.Error("old body retained")
				}
			}
		})
	}
}

func TestSkippedActReadsAsNotDeliveredAfterRollback(t *testing.T) {
	for _, kind := range []string{"message.created", "message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			f.l.HistoryFloor = floor
			m := message()
			m.LastUpdateTime = floor.Add(-time.Hour).Format(time.RFC3339Nano)
			if strings.HasPrefix(kind, "reaction") {
				m.Sender.Name = identity
				f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
			}
			handle(t, f, timedAct(t, kind, m, floor.Add(-time.Hour), "old"))
			if len(f.l.Changes) != 1 {
				t.Fatalf("skip records=%d", len(f.l.Changes))
			}
			for _, c := range f.l.Changes {
				switch c.Reason {
				case "local", "own", "wake_policy", "space unavailable":
				default:
					t.Errorf("rollback sees reason %q as delivered", c.Reason)
				}
				b, _ := json.Marshal(c)
				if !strings.Contains(string(b), "before_history_floor") {
					t.Error("skip cause missing")
				}
			}
		})
	}
}

func TestNewActsOnOldMessagesKeepTheirFrame(t *testing.T) {
	for _, kind := range []string{"message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			f.l.HistoryFloor = floor
			m := message()
			old := floor.Add(-time.Hour)
			fresh := floor.Add(time.Hour)
			if strings.HasPrefix(kind, "reaction") {
				m.Sender.Name = identity
				f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
			}
			// An old act of the very same kind must not consume the later act.
			m.LastUpdateTime = old.Format(time.RFC3339Nano)
			handle(t, f, timedAct(t, kind, m, old, "old"))
			before := len(f.gc.Inbound())
			if kind == "message.deleted" {
				m.LastUpdateTime = fresh.Add(-time.Minute).Format(time.RFC3339Nano)
				handle(t, f, timedAct(t, "message.updated", m, fresh.Add(-time.Minute), "edit"))
				before = len(f.gc.Inbound())
			}
			m.LastUpdateTime = fresh.Format(time.RFC3339Nano)
			handle(t, f, timedAct(t, kind, m, fresh, "new"))
			turns := f.gc.Inbound()
			if len(turns) != before+1 {
				t.Fatalf("new act turns=%d want %d", len(turns), before+1)
			}
			last := turns[len(turns)-1]
			if last.Actor.ID == "" || !strings.Contains(last.Text, "[from:") {
				t.Errorf("sender/frame lost: %+v", last)
			}
		})
	}
}

func TestBoundedActWithoutTimeUsesPublishTime(t *testing.T) {
	for _, published := range []string{"old", "new", "none"} {
		t.Run(published, func(t *testing.T) {
			f := setup(t)
			floor := time.Now().Add(-time.Hour).UTC()
			f.l.HistoryFloor = floor
			m := message()
			m.Sender.Name = identity
			f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
			raw, _ := json.Marshal(map[string]any{"reaction": &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/222"}}})
			msg := &pubsub.Message{ID: "timeless", Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.reaction.v1.created"}}
			if published == "old" {
				msg.PublishTime = floor.Add(-time.Second)
			}
			if published == "new" {
				msg.PublishTime = floor.Add(time.Second)
			}
			ev, err := puller.Decode(msg)
			if err != nil {
				t.Fatal(err)
			}
			handle(t, f, ev)
			want := 0
			if published == "new" {
				want = 1
			}
			if len(f.gc.Inbound()) != want {
				t.Errorf("turns=%d want %d", len(f.gc.Inbound()), want)
			}
			if published == "none" && f.l.Counters["history_time_missing:"+space] != 1 {
				t.Error("missing-time counter absent")
			}
		})
	}
}

// This test's writer can be overlaid with the new writer and its reader run
// from the released source. The ledger exchange itself is ordinary JSON.
func rollbackNoTurn(t *testing.T, f *fixture) {
	t.Helper()
	f.l.HistoryFloor = time.Time{}
	f.opts.Config.Settings.Defaults.Wake = "mentions"
	for _, s := range f.l.Spaces {
		s.Wake = "mentions"
	}
	m := message()
	m.LastUpdateTime = "2026-10-22T14:00:00Z"
	handle(t, f, timedAct(t, "message.updated", m, time.Date(2026, 10, 22, 14, 0, 0, 0, time.UTC), "rollback-edit"))
	handle(t, f, timedAct(t, "message.deleted", &api.Message{Name: m.Name}, time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC), "rollback-delete"))
	if len(f.gc.Inbound()) != 0 {
		t.Fatalf("rollback turns=%d", len(f.gc.Inbound()))
	}
}

func TestRollbackSkippedCreateDoesNotWake(t *testing.T) {
	f := setup(t)
	m := message()
	floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.l.HistoryFloor = floor
	handle(t, f, timedAct(t, "message.created", m, floor.Add(-time.Hour), "old"))
	raw, _ := json.Marshal(f.l)
	var restored ledger.Ledger
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	*f.l = restored
	rollbackNoTurn(t, f)
	for _, c := range f.l.Changes {
		var w wire.InboundMessage
		_ = json.Unmarshal([]byte(c.Event), &w)
		if c.Status != "committed" {
			t.Error("rollback retained pending act")
		}
	}
}

func TestHistoryBatchUsesEachResourceTime(t *testing.T) {
	for _, kind := range []string{"created", "updated"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			f.l.HistoryFloor = floor
			var items []map[string]any
			for i := range 2 {
				m := message()
				m.Name += fmt.Sprint(i)
				at := floor.Add(time.Duration(2*i-1) * time.Hour)
				if kind == "created" {
					m.CreateTime = at.Format(time.RFC3339Nano)
				} else {
					m.LastUpdateTime = at.Format(time.RFC3339Nano)
				}
				items = append(items, map[string]any{"message": m})
			}
			raw, _ := json.Marshal(map[string]any{"messages": items})
			ev, err := puller.Decode(&pubsub.Message{ID: "batch", Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.batch" + strings.ToUpper(kind[:1]) + kind[1:], "ce-time": floor.Add(-time.Hour).Format(time.RFC3339Nano)}})
			if err != nil {
				t.Fatal(err)
			}
			handle(t, f, ev)
			turns := f.gc.Inbound()
			if len(turns) != 1 || !strings.HasPrefix(turns[0].ProviderMessageID, messageName+"1") {
				t.Fatalf("mixed batch turns=%d", len(turns))
			}
		})
	}
}

func TestMessageWithoutActTimeUsesPublishTime(t *testing.T) {
	for _, kind := range []string{"created", "updated"} {
		for _, stamp := range []string{"old", "new", "none"} {
			t.Run(kind+"/"+stamp, func(t *testing.T) {
				f := setup(t)
				floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
				f.l.HistoryFloor = floor
				m := message()
				m.CreateTime = ""
				m.LastUpdateTime = ""
				raw, _ := json.Marshal(map[string]any{"message": m})
				msg := &pubsub.Message{ID: "missing-time", Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1." + kind}}
				if stamp == "old" {
					msg.PublishTime = floor.Add(-time.Hour)
				}
				if stamp == "new" {
					msg.PublishTime = floor.Add(time.Hour)
				}
				ev, err := puller.Decode(msg)
				if err != nil {
					t.Fatal(err)
				}
				handle(t, f, ev)
				want := 0
				if stamp == "new" {
					want = 1
				}
				if len(f.gc.Inbound()) != want {
					t.Errorf("turns=%d want %d", len(f.gc.Inbound()), want)
				}
				if stamp == "none" && f.l.Counters["history_time_missing:"+space] != 1 {
					t.Error("missing-time counter absent")
				}
			})
		}
	}
}

func TestNewReactionRemovalKeepsSkippedAttribution(t *testing.T) {
	f := setup(t)
	floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.l.HistoryFloor = floor
	m := message()
	m.Sender.Name = identity
	f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
	handle(t, f, timedAct(t, "reaction.created", m, floor.Add(-time.Hour), "old-add"))
	raw, _ := json.Marshal(map[string]any{"reaction": &api.Reaction{Name: m.Name + "/reactions/one"}})
	ev, err := puller.Decode(&pubsub.Message{ID: "new-remove", Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat.reaction.v1.deleted", "ce-time": floor.Add(time.Hour).Format(time.RFC3339Nano)}})
	if err != nil {
		t.Fatal(err)
	}
	handle(t, f, ev)
	turns := f.gc.Inbound()
	if len(turns) != 1 {
		t.Fatalf("turns=%d", len(turns))
	}
	if turns[0].Actor.ID != "users/222" || !strings.Contains(turns[0].Text, "<users/222>") || !strings.Contains(turns[0].Text, "reaction deleted:") {
		t.Errorf("removal lost attribution: %+v", turns[0])
	}
}

func TestOldActNeverDownloadsAttachments(t *testing.T) {
	for _, kind := range []string{"message.created", "message.updated"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			floor := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			f.l.HistoryFloor = floor
			var requests atomic.Int32
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(404) }))
			t.Cleanup(s.Close)
			f.opts.Config.Endpoints.ChatAPIBase = s.URL
			var err error
			f.opts.Chat, err = chat.New(t.Context(), http.DefaultClient, f.opts.Config.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.p, err = New(f.opts)
			if err != nil {
				t.Fatal(err)
			}
			m := message()
			m.LastUpdateTime = floor.Add(-time.Hour).Format(time.RFC3339Nano)
			m.Attachment = []*api.Attachment{{Name: m.Name + "/attachments/file", ContentName: "old-report.txt", ContentType: "text/plain", Source: "UPLOADED_CONTENT", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/file"}}}
			handle(t, f, timedAct(t, kind, m, floor.Add(-time.Hour), "old-attachment"))
			if requests.Load() != 0 || len(f.gc.Inbound()) != 0 {
				t.Errorf("old attachment requests=%d turns=%d", requests.Load(), len(f.gc.Inbound()))
			}
			for _, c := range f.l.Changes {
				var w wire.InboundMessage
				_ = json.Unmarshal([]byte(c.Event), &w)
				if len(w.Attachments) != 0 || strings.Contains(w.Text, "old-report") || strings.Contains(w.Text, m.Text) {
					t.Error("old content retained")
				}
			}
		})
	}
}
