package catchup

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

func patchHistory(t *testing.T, value any, fields map[string]any) {
	t.Helper()
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, value); err != nil {
		t.Fatal(err)
	}
}

func TestCursorlessSpaceStartsWhereItWasFirstSeen(t *testing.T) {
	for _, mode := range []string{"unreadable", "no-interval", "next-listing", "queued", "old-floor", "outage-dm"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			start := frozen.Add(-time.Hour)
			patchHistory(t, f.opts.Ledger, map[string]any{"spaces_listed_at": start})
			f.addSpace("spaces/dm", false)
			f.google.Spaces["spaces/dm"].SpaceType = "DIRECT_MESSAGE"
			for i := range 120 {
				f.addMessage("spaces/dm", i, start.Add(-time.Duration(281+i)*24*time.Hour))
			}
			if mode != "next-listing" && mode != "outage-dm" {
				f.opts.Ledger.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all"}
			}
			if mode == "old-floor" {
				f.opts.Ledger.HistoryFloor = frozen.Add(-40 * 24 * time.Hour)
				for i := range 31 {
					f.addMessage("spaces/dm", 200+i, frozen.Add(-time.Duration(i+1)*24*time.Hour))
				}
			}
			if mode == "queued" {
				s := f.opts.Ledger.Spaces["spaces/dm"]
				s.FrozenUpper = start.Add(-time.Minute)
				s.PageToken = encode(progress{Version: 1, Phase: "done", MessageLower: time.Unix(0, 0).UTC(), EventLower: time.Unix(0, 0).UTC(), Token: "stale"})
				for i := range 3 {
					m := f.google.Messages[fmt.Sprintf("spaces/dm/messages/m%04d", i)]
					at, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
					s.Backlog = append(s.Backlog, encode(queuedChange{Change: puller.Change{Kind: "message.created", Space: "spaces/dm", Message: m}, At: at}))
				}
			}
			if mode == "unreadable" {
				refuse := true
				f.onRequest = func(r *http.Request) error {
					if refuse && r.URL.Path == "/v1/spaces/dm/messages" {
						return &googleapi.Error{Code: 403, Message: "permission denied"}
					}
					return nil
				}
				// Start and retain the interval while a listed space is unavailable.
				f.freeze(t)
				_, _, _ = f.r.Pass(t.Context())
				refuse = false
				s := f.opts.Ledger.Spaces["spaces/dm"]
				p, err := decodeProgress(s.PageToken)
				if err != nil {
					t.Fatal(err)
				}
				p.AccessRetryAfter = frozen
				s.PageToken = encode(p)
			}
			f.addMessage("spaces/dm", 999, start.Add(time.Minute))
			f.freeze(t)
			for range 5 {
				_, _, err := f.r.Pass(t.Context())
				if err != nil {
					t.Fatal(err)
				}
			}
			turns := f.gc.Inbound()
			t.Logf("turns=%d", len(turns))
			if len(turns) != 1 || turns[0].ProviderMessageID != "spaces/dm/messages/m0999" {
				t.Errorf("cursorless replay: %d turns", len(turns))
			}
			for _, m := range turns {
				if !strings.HasPrefix(m.Text, "[catch-up]") {
					t.Error("outage lost catch-up marker")
				}
			}
			if len(f.opts.Ledger.Spaces["spaces/dm"].Backlog) != 0 {
				t.Error("old queue retained")
			}
		})
	}
}

func TestBoundedQueueRetiresSavedDelivery(t *testing.T) {
	for _, kind := range []string{"message.created", "message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
		for _, status := range []string{"intent", "pending", "unresolved"} {
			t.Run(kind+"/"+status, func(t *testing.T) {
				f := setup(t)
				l := f.opts.Ledger
				patchHistory(t, l, map[string]any{"spaces_listed_at": frozen.Add(-time.Hour)})
				f.addSpace("spaces/dm", false)
				f.addMessage("spaces/dm", 0, frozen.Add(-2*time.Hour))
				m := f.google.Messages["spaces/dm/messages/m0000"]
				m.LastUpdateTime = m.CreateTime
				at := frozen.Add(-2 * time.Hour)
				c := puller.Change{Kind: kind, Space: "spaces/dm", Message: m}
				patchHistory(t, &c, map[string]any{"EventTime": at})
				id := m.Name
				if kind == "message.updated" {
					id += "#updated#" + m.LastUpdateTime
				} else if kind == "message.deleted" {
					id += "#deleted"
				} else if strings.HasPrefix(kind, "reaction.") {
					c.Message = nil
					c.Reaction = &api.Reaction{Name: m.Name + "/reactions/one", User: m.Sender}
					id = c.Reaction.Name + "#" + strings.TrimPrefix(kind, "reaction.")
				}
				w := wire.InboundMessage{ProviderMessageID: id, Conversation: wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: "spaces/dm", Kind: "dm"}, Text: "old queued body", ReceivedAt: frozen}
				l.Changes[id] = &ledger.ChangeState{ID: id, Space: "spaces/dm", Message: m.Name, Kind: kind, Status: status, Created: frozen, Event: encode(w)}
				if status == "unresolved" {
					l.Unresolved[id] = l.Changes[id]
				}
				l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all", Backlog: []string{encode(queuedChange{Change: c, At: at, Handled: true})}}
				f.freeze(t)
				if err := f.p.Pass(t.Context()); err != nil {
					t.Fatal(err)
				}
				if len(f.gc.Inbound()) != 0 || len(l.Unresolved) != 0 {
					t.Errorf("turns=%d unresolved=%d", len(f.gc.Inbound()), len(l.Unresolved))
				}
				for _, record := range l.Changes {
					if record.Status != "committed" || record.Reason != "local" || strings.Contains(record.Event, "old queued body") {
						t.Errorf("saved delivery survived: status=%s reason=%s", record.Status, record.Reason)
					}
				}
			})
		}
	}
}

func TestNeverReadSpaceReportsNoLoss(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(fmt.Sprintf("start=%t", started), func(t *testing.T) {
			f := setup(t)
			old := frozen.Add(-40 * 24 * time.Hour)
			f.opts.Ledger.HistoryFloor = old
			f.addSpace("spaces/room", false)
			if started {
				s := &ledger.SpaceState{Kind: "room", MessageCursor: old, EventCursor: old}
				patchHistory(t, s, map[string]any{"history_start": old})
				f.opts.Ledger.Spaces["spaces/room"] = s
			}
			f.freeze(t)
			f.pass(t, 0, 0)
			if f.opts.Ledger.Counters["unrecoverable:spaces/room"] != 0 || strings.Contains(f.logs.String(), "catch-up history unrecoverable") {
				t.Error("never-read space reported lost history")
			}
		})
	}
}

func TestSavedCursorStillDeliversItsOutage(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	for i := range 3 {
		f.addMessage("spaces/room", i, frozen.Add(-30*time.Minute+time.Duration(i)*time.Second))
	}
	f.freeze(t)
	f.pass(t, 3, 0)
	f.freeze(t)
	f.pass(t, 0, 0)
}

func TestCatchupMissingEnvelopeTime(t *testing.T) {
	for _, bounded := range []bool{false, true} {
		for _, kind := range []string{"message.updated", "reaction.created"} {
			t.Run(fmt.Sprintf("%s/bounded=%t", kind, bounded), func(t *testing.T) {
				f := setup(t)
				f.addSpace("spaces/room", true)
				if bounded {
					f.opts.Ledger.HistoryFloor = frozen.Add(-30 * time.Minute)
				}
				m := f.addMessage("spaces/room", 1, frozen.Add(-2*time.Hour))
				m.LastUpdateTime = frozen.Add(-time.Minute).Format(time.RFC3339Nano)
				event := &api.SpaceEvent{Name: "spaces/room/spaceEvents/missing-time", EventType: "google.workspace.chat.message.v1.updated", MessageUpdatedEventData: &api.MessageUpdatedEventData{Message: m}}
				if kind == "reaction.created" {
					m.Sender.Name = "users/123"
					event = &api.SpaceEvent{Name: event.Name, EventType: "google.workspace.chat.reaction.v1.created", ReactionCreatedEventData: &api.ReactionCreatedEventData{Reaction: &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/456"}}}}
				}
				raw, _ := json.Marshal(map[string]any{"spaceEvents": []*api.SpaceEvent{event}})
				var err error
				f.opts.Chat, err = chat.New(t.Context(), &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/spaceEvents") {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}, nil
					}
					return http.DefaultTransport.RoundTrip(r)
				})}, f.opts.Config.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				f.newRunner(t)
				f.freeze(t)
				_, remaining, err := f.r.Pass(t.Context())
				if err != nil || remaining != 0 {
					t.Fatalf("remaining=%d err=%v", remaining, err)
				}
				want := 0
				if bounded && kind == "message.updated" {
					want = 1
				}
				if len(f.gc.Inbound()) != want {
					t.Errorf("turns=%d want=%d", len(f.gc.Inbound()), want)
				}
				if bounded && len(f.opts.Ledger.Unresolved) != 0 {
					t.Error("bounded act with missing time became unresolved")
				}
				if bounded && kind == "reaction.created" && (f.opts.Ledger.Counters["history_time_missing:spaces/room"] != 1 || !strings.Contains(f.logs.String(), "no usable history time")) {
					t.Error("missing time not counted and warned")
				}
				if !bounded && len(f.opts.Ledger.Unresolved) != 1 {
					t.Error("unbounded malformed-event behavior changed")
				}
			})
		}
	}
}
