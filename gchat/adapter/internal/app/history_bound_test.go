package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/events"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"github.com/BurntSushi/toml"
	api "google.golang.org/api/chat/v1"
	workspace "google.golang.org/api/workspaceevents/v1"
)

func boundStart(t *testing.T, f *fixture, notices *atomic.Int32) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryDelay: time.Millisecond, Notify: func(context.Context, string, string) error { notices.Add(1); return nil }})
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
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("shutdown timed out")
		}
		f.client.CloseIdleConnections()
	}
	t.Cleanup(stop)
	f.health(t, "ready", 30*time.Second)
	return stop
}

// The wire fixture deliberately uses Decode's public input, so the same test
// compiles before occurrence times are carried through the processor.
func boundAct(t *testing.T, f *fixture, kind string, batch bool, at time.Time, tag string) (*pubsub.Message, []*api.Message) {
	t.Helper()
	parts := strings.Split(kind, ".")
	resource, action := parts[0], parts[1]
	n := 1
	if batch {
		n = 2
		action = "batch" + strings.ToUpper(action[:1]) + action[1:]
	}
	var items []map[string]any
	var messages []*api.Message
	for i := range n {
		m := historyMessage("spaces/dm", fmt.Sprintf("%s-%d", tag, i), at.Add(-time.Hour))
		m.Space = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
		if kind == "message.created" {
			m.CreateTime = at.Format(time.RFC3339Nano)
		}
		m.LastUpdateTime = at.Format(time.RFC3339Nano)
		var value any = m
		if resource == "reaction" {
			m.Sender.Name = fakegoogle.UserID
			value = &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/222"}, Emoji: &api.Emoji{Unicode: "+"}}
		}
		items = append(items, map[string]any{resource: value})
		messages = append(messages, m)
	}
	var payload any = items[0]
	if batch {
		payload = map[string]any{resource + "s": items}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return &pubsub.Message{Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat." + resource + ".v1." + action, "ce-id": tag + kind, "ce-time": at.Format(time.RFC3339Nano)}}, messages
}

func boundPublish(t *testing.T, f *fixture, msg *pubsub.Message) {
	t.Helper()
	if _, err := f.pub.Publish(t.Context(), msg).Get(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool {
		l, _ := historyState(t, f)
		return l.Transport.Seen("cloudevent:" + msg.Attributes["ce-id"])
	})
}

func boundZero(t *testing.T, f *fixture, notices *atomic.Int32) {
	t.Helper()
	l, _ := historyState(t, f)
	t.Logf("turns=%d notices=%d changes=%d", len(f.gc.Inbound()), notices.Load(), len(l.Changes))
	if len(f.gc.Inbound()) != 0 || notices.Load() != 0 {
		t.Errorf("old act escaped: turns=%d notices=%d", len(f.gc.Inbound()), notices.Load())
	}
	for _, s := range l.Spaces {
		if len(s.Backlog) != 0 {
			t.Error("old backlog remains")
		}
	}
	for _, c := range l.Changes {
		if c.Status != "committed" {
			t.Errorf("old act remains %s", c.Status)
		}
	}
	if len(l.Unresolved) != 0 {
		t.Error("old unresolved remains")
	}
}

func TestNoOldActIsEverATurn(t *testing.T) {
	for _, kind := range []string{"message.created", "message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
		for _, batch := range []bool{false, true} {
			for _, path := range []string{"L", "R", "C", "Q", "O", "P"} {
				t.Run(fmt.Sprintf("%s/batch=%t/%s", kind, batch, path), func(t *testing.T) {
					f := newFixture(t, false)
					f.replaceIdentityToken(t, fakegoogle.UserID)
					f.google.Configure(func(g *fakegoogle.Server) {
						g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
						g.Subscriptions["subscriptions/adopted"] = &workspace.Subscription{Name: "subscriptions/adopted", State: "ACTIVE", TargetResource: events.Target, EventTypes: append([]string(nil), events.EventTypes...), ExpireTime: time.Now().Add(4 * time.Hour).Format(time.RFC3339Nano), PayloadOptions: &workspace.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &workspace.NotificationEndpoint{PubsubTopic: f.cfg.Secrets.Topic}}
					})
					var notices atomic.Int32
					stop := boundStart(t, f, &notices)
					l, floor := historyState(t, f)
					if l.Subscription.Name != "subscriptions/adopted" {
						t.Fatal("subscription was not adopted")
					}
					at := floor.Add(-time.Hour)
					msg, messages := boundAct(t, f, kind, batch, at, "old")
					f.google.Configure(func(g *fakegoogle.Server) {
						for _, m := range messages {
							g.Messages[m.Name] = m
						}
					})
					if path == "C" || path == "Q" {
						stop()
						l, _ = historyState(t, f)
						if path == "Q" {
							ev, err := puller.Decode(msg)
							if err != nil {
								t.Fatal(err)
							}
							s := l.Spaces["spaces/dm"]
							s.FrozenUpper = time.Now().UTC()
							p, _ := json.Marshal(map[string]any{"version": 1, "phase": "done", "message_lower": at.Add(-time.Hour), "event_lower": at.Add(-time.Hour)})
							s.PageToken = string(p)
							for _, c := range ev.Changes {
								raw, _ := json.Marshal(map[string]any{"change": c, "at": at})
								s.Backlog = append(s.Backlog, string(raw))
							}
							if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
								t.Fatal(err)
							}
						} else if kind != "message.created" {
							parts := strings.Split(strings.TrimPrefix(msg.Attributes["ce-type"], "google.workspace.chat."), ".v1.")
							var payload any
							_ = json.Unmarshal(msg.Data, &payload)
							raw, _ := json.Marshal(map[string]any{"name": "spaces/dm/spaceEvents/old", "eventType": msg.Attributes["ce-type"], "eventTime": at.Format(time.RFC3339Nano), parts[0] + strings.ToUpper(parts[1][:1]) + parts[1][1:] + "EventData": payload})
							var event api.SpaceEvent
							if err := json.Unmarshal(raw, &event); err != nil {
								t.Fatal(err)
							}
							f.google.Configure(func(g *fakegoogle.Server) { g.SpaceEvents["spaces/dm"] = []*api.SpaceEvent{&event} })
						}
						stop = boundStart(t, f, &notices)
					} else {
						boundPublish(t, f, msg)
					}
					if path == "R" {
						boundPublish(t, f, msg)
						stop()
						stop = boundStart(t, f, &notices)
						boundPublish(t, f, msg)
					}
					if path == "O" {
						l, _ = historyState(t, f)
						for id := range l.Changes {
							raw, _ := json.Marshal(map[string]string{"id": id, "action": "replay"})
							r, err := f.client.Post("http://adapter/resolve", "application/json", strings.NewReader(string(raw)))
							if err != nil {
								t.Fatal(err)
							}
							r.Body.Close()
							if r.StatusCode != 400 {
								t.Errorf("replay status=%d", r.StatusCode)
							}
						}
					}
					stop()
					if path == "P" {
						l, _ = historyState(t, f)
						c, err := chat.New(t.Context(), http.DefaultClient, f.cfg.Endpoints)
						if err != nil {
							t.Fatal(err)
						}
						p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: l, LedgerMu: &f.mu, LedgerPath: f.cfg.Endpoints.StateFile, Chat: c, GC: gcclient.New(f.gc.URL(), "examplecity"), IdentityUserID: fakegoogle.UserID, Raise: func(context.Context, inbound.Raise) error { notices.Add(1); return nil }})
						if err != nil {
							t.Fatal(err)
						}
						for range 3 {
							if err := p.Poll(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
					}
					boundZero(t, f, &notices)
				})
			}
		}
	}
}

func TestOldActOrdersAndNewActs(t *testing.T) {
	for _, order := range [][]string{{"message.updated"}, {"message.created", "message.updated"}, {"reaction.created", "reaction.deleted"}, {"message.updated", "message.deleted"}} {
		t.Run("old/"+strings.Join(order, "-"), func(t *testing.T) {
			f := newFixture(t, false)
			f.replaceIdentityToken(t, fakegoogle.UserID)
			f.google.Configure(func(g *fakegoogle.Server) {
				g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
			})
			var notices atomic.Int32
			stop := boundStart(t, f, &notices)
			_, floor := historyState(t, f)
			for _, kind := range order {
				msg, ms := boundAct(t, f, kind, false, floor.Add(-time.Hour), "sequence")
				f.google.Configure(func(g *fakegoogle.Server) {
					for _, m := range ms {
						g.Messages[m.Name] = m
					}
				})
				boundPublish(t, f, msg)
			}
			stop()
			boundZero(t, f, &notices)
		})
	}
	for _, kind := range []string{"message.created", "message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
		t.Run("new/"+kind, func(t *testing.T) {
			f := newFixture(t, false)
			f.replaceIdentityToken(t, fakegoogle.UserID)
			f.google.Configure(func(g *fakegoogle.Server) {
				g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
			})
			var notices atomic.Int32
			stop := boundStart(t, f, &notices)
			msg, ms := boundAct(t, f, kind, false, time.Now(), "new")
			f.google.Configure(func(g *fakegoogle.Server) {
				for _, m := range ms {
					g.Messages[m.Name] = m
				}
			})
			before := 0
			if kind == "message.deleted" {
				edit, _ := boundAct(t, f, "message.updated", false, time.Now(), "new")
				boundPublish(t, f, edit)
				before = len(f.gc.Inbound())
			}
			boundPublish(t, f, msg)
			stop()
			turns := f.gc.Inbound()
			if len(turns) != before+1 {
				t.Fatalf("new act turns=%d want=%d", len(turns), before+1)
			}
			w := turns[len(turns)-1]
			if w.Actor.ID != "users/222" || !strings.Contains(w.Text, "[from:") {
				t.Error("new act lost sender/frame")
			}
		})
	}
}

func TestFloorFollowsTheFirstSuccessfulConnection(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		code                   int
		create, reload, window bool
	}{{"503", 503, false, false, false}, {"403", 403, false, false, false}, {"create", 503, true, false, false}, {"reload", 503, false, true, false}, {"24h", 503, false, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, false)
			if !tc.reload {
				f.replaceIdentityToken(t, fakegoogle.UserID)
			}
			if tc.window {
				if _, err := toml.Decode("first_start_history = \"24h\"", &f.cfg.Settings); err != nil {
					t.Fatal(err)
				}
			}
			f.google.Configure(func(g *fakegoogle.Server) {
				g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
			})
			var refusing atomic.Bool
			var refused atomic.Int32
			refusing.Store(true)
			u, _ := url.Parse(f.cfg.Endpoints.ChatAPIBase)
			proxy := httputil.NewSingleHostReverseProxy(u)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.create && r.Method == "POST" && r.URL.Path == "/v1/subscriptions" {
					fail := refusing.Load()
					f.google.Configure(func(g *fakegoogle.Server) {
						g.OperationError = nil
						if fail {
							g.OperationError = &workspace.Status{Code: 13, Message: "create failed"}
						}
					})
					if fail {
						refused.Add(1)
					}
				}
				if !tc.create && refusing.Load() && strings.HasPrefix(r.URL.Path, "/v1/subscriptions") {
					refused.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.code)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"setup refused","status":"PERMISSION_DENIED"}}`, tc.code)
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			f.cfg.Endpoints.EventsAPIBase = server.URL
			f.start(t)
			if tc.reload {
				f.health(t, "needs-sign-in", 5*time.Second)
				f.replaceIdentityToken(t, fakegoogle.UserID)
			}
			waitFor(t, 10*time.Second, func() bool { return refused.Load() > 0 })
			written := time.Now().UTC()
			age := time.Duration(0)
			if tc.window {
				age = 24 * time.Hour
			}
			m := historyMessage("spaces/dm", "during-refusal", written.Add(-age))
			f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
			time.Sleep(100 * time.Millisecond)
			allowed := time.Now().UTC()
			refusing.Store(false)
			f.health(t, "ready", 30*time.Second)
			_, floor := historyState(t, f)
			if floor.Before(allowed.Add(-age)) {
				t.Errorf("floor predates successful attempt: floor=%s allowed=%s", floor, allowed.Add(-age))
			}
			if len(f.gc.Inbound()) != 0 {
				t.Errorf("before-connection turns=%d", len(f.gc.Inbound()))
			}
			fresh := historyMessage("spaces/dm", "after-connection", time.Now())
			publishHistoryMessage(t, f, fresh)
			waitFor(t, 5*time.Second, func() bool { l, _ := historyState(t, f); return l.Changes[fresh.Name] != nil })
			f.replaceIdentityToken(t, fakegoogle.UserID)
			waitFor(t, 10*time.Second, func() bool { l, _ := historyState(t, f); return l.Subscription.Name != "" })
			_, later := historyState(t, f)
			if !later.Equal(floor) {
				t.Error("later generation moved floor")
			}
		})
	}
}

func TestReleasedLedgerCursorlessSpacesStartAtLoad(t *testing.T) {
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("service-restore=%t", restore), func(t *testing.T) {
			f := newFixture(t, false)
			raw, err := os.ReadFile("../ledger/testdata/ledger-0.5.0.json")
			if err != nil {
				t.Fatal(err)
			}
			var l ledger.Ledger
			if err := json.Unmarshal(raw, &l); err != nil {
				t.Fatal(err)
			}
			l.Spaces = map[string]*ledger.SpaceState{"spaces/dm": {Kind: "dm", Wake: "all"}, "spaces/room": {Kind: "room", Wake: "all", MessageCursor: time.Now().Add(-time.Hour), EventCursor: time.Now().Add(-time.Hour)}}
			backup := f.cfg.Endpoints.StateFile + ".backup"
			if err := l.Save(backup); err != nil {
				t.Fatal(err)
			}
			if restore {
				writeFile(t, f.cfg.Endpoints.StateFile, "invalid")
			} else {
				raw, _ = os.ReadFile(backup)
				writeFile(t, f.cfg.Endpoints.StateFile, string(raw))
			}
			before := time.Now().UTC()
			f.start(t)
			if restore {
				f.health(t, "recovery-required", 5*time.Second)
				body, _ := json.Marshal(map[string]string{"action": "restore", "file": backup})
				r, err := f.client.Post("http://adapter/state", "application/json", strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				r.Body.Close()
				if r.StatusCode != 200 {
					t.Fatal(r.StatusCode)
				}
			}
			f.health(t, "needs-sign-in", 5*time.Second)
			loaded, _ := historyState(t, f)
			if !loaded.HistoryFloor.IsZero() {
				t.Error("released ledger gained a floor")
			}
			if loaded.Spaces["spaces/dm"].MessageCursor.Before(before) {
				t.Error("cursorless space did not start at first load")
			}
			if !loaded.Spaces["spaces/room"].MessageCursor.Equal(l.Spaces["spaces/room"].MessageCursor) {
				t.Error("outage cursor changed")
			}
		})
	}
}
