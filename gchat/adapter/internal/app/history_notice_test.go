package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/events"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/server"
	api "google.golang.org/api/chat/v1"
	workspace "google.golang.org/api/workspaceevents/v1"
)

type historyNoticeSink struct {
	now          time.Time
	posts, local int
	mails        []neversilent.Mail
	opts         neversilent.Options
	engine       *neversilent.Engine
}

func newHistoryNoticeSink(t *testing.T, path string, now time.Time) *historyNoticeSink {
	t.Helper()
	n := &historyNoticeSink{now: now}
	n.opts = neversilent.Options{Enabled: true, Path: path, HealthFile: filepath.Join(t.TempDir(), "health.json"), City: "examplecity", Channel: "gchat", OperatorContext: "example-operator",
		Now:       func() time.Time { return n.now },
		Post:      func(context.Context, neversilent.Message, string) error { n.posts++; return nil },
		Send:      func(_ context.Context, m neversilent.Mail) error { n.mails = append(n.mails, m); return nil },
		SendLocal: func(context.Context, neversilent.Message, string) error { n.local++; return nil },
	}
	n.reload(t)
	return n
}

func (n *historyNoticeSink) reload(t *testing.T) {
	t.Helper()
	var err error
	n.engine, err = neversilent.New(n.opts)
	if err != nil {
		t.Fatal(err)
	}
}

func (n *historyNoticeSink) messages(t *testing.T) map[string]neversilent.Message {
	t.Helper()
	b, err := os.ReadFile(n.opts.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Messages map[string]neversilent.Message `json:"messages"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	return saved.Messages
}

func TestOldActStartsNoNotice(t *testing.T) {
	for _, cause := range []string{"before_history_floor", "history_time_missing"} {
		for _, path := range []string{"L", "R", "C", "Q", "O", "P"} {
			t.Run(cause+"/"+path, func(t *testing.T) {
				f := newFixture(t, false)
				f.replaceIdentityToken(t, fakegoogle.UserID)
				f.cfg.NeverSilent.Enabled = true
				f.cfg.NeverSilent.OperatorContext = "example-operator"
				mail := noticeMail(t)
				f.google.Configure(func(g *fakegoogle.Server) {
					g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
					g.Subscriptions["subscriptions/adopted"] = &workspace.Subscription{Name: "subscriptions/adopted", State: "ACTIVE", TargetResource: events.Target, EventTypes: append([]string(nil), events.EventTypes...), ExpireTime: time.Now().Add(4 * time.Hour).Format(time.RFC3339Nano), PayloadOptions: &workspace.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &workspace.NotificationEndpoint{PubsubTopic: f.cfg.Secrets.Topic}}
				})
				var local atomic.Int32
				stop := boundStart(t, f, &local)
				l, floor := historyState(t, f)
				if l.Subscription.Name != "subscriptions/adopted" {
					t.Fatal("subscription not adopted")
				}
				at := floor.Add(-time.Hour)
				msg, messages := boundAct(t, f, "message.created", false, at, "old-notice")
				if cause == "history_time_missing" {
					messages[0].CreateTime, messages[0].LastUpdateTime = "", ""
					msg.Attributes["ce-time"] = ""
					msg.Data, _ = json.Marshal(map[string]any{"message": messages[0]})
					at = time.Time{}
				}
				f.google.Configure(func(g *fakegoogle.Server) { g.Messages[messages[0].Name] = messages[0] })
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
						progress, _ := json.Marshal(map[string]any{"version": 1, "phase": "done", "message_lower": floor, "event_lower": floor})
						s.PageToken = string(progress)
						queued, _ := json.Marshal(map[string]any{"change": ev.Changes[0], "at": at})
						s.Backlog = []string{string(queued)}
						if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
							t.Fatal(err)
						}
					}
					stop = boundStart(t, f, &local)
				} else {
					// Pub/Sub assigns publish time; for a missing-time act the
					// direct decoded form below deliberately has no fallback.
					if cause == "before_history_floor" {
						boundPublish(t, f, msg)
					}
				}
				if path == "R" && cause == "before_history_floor" {
					boundPublish(t, f, msg)
					stop()
					stop = boundStart(t, f, &local)
					boundPublish(t, f, msg)
				}
				stop()
				l, _ = historyState(t, f)
				n := newHistoryNoticeSink(t, f.cfg.Endpoints.StateFile+".never-silent.json", floor.Add(time.Minute))
				c, err := chat.New(t.Context(), http.DefaultClient, f.cfg.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				opts := inbound.Options{Config: f.cfg, Ledger: l, LedgerMu: &f.mu, LedgerPath: f.cfg.Endpoints.StateFile, Chat: c, GC: gcclient.New(f.gc.URL(), "examplecity"), IdentityUserID: fakegoogle.UserID, NeverSilent: n.engine, Now: func() time.Time { return n.now }, Raise: func(context.Context, inbound.Raise) error { local.Add(1); return nil }}
				p, err := inbound.New(opts)
				if err != nil {
					t.Fatal(err)
				}
				if cause == "history_time_missing" && path != "C" && path != "Q" {
					ev, err := puller.Decode(msg)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := p.Handle(t.Context(), ev); err != nil {
						t.Fatal(err)
					}
					if path == "R" {
						if _, err := p.Handle(t.Context(), ev); err != nil {
							t.Fatal(err)
						}
						loaded, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
						if err != nil {
							t.Fatal(err)
						}
						l.Replace(loaded)
						n.reload(t)
						opts.NeverSilent = n.engine
						p, err = inbound.New(opts)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := p.Handle(t.Context(), ev); err != nil {
							t.Fatal(err)
						}
					}
				}
				s, err := server.New(server.Options{Config: f.cfg, Ledger: l, LedgerMu: &f.mu, LedgerPath: f.cfg.Endpoints.StateFile, Chat: c, GC: opts.GC})
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				s.SetNeverSilent(n.engine)
				s.SetPoll(p.Pass)
				s.SetHealth(server.Health{State: "ready"})
				if path == "O" {
					for id := range l.Changes {
						raw, _ := json.Marshal(map[string]string{"id": id, "action": "replay"})
						w := httptest.NewRecorder()
						s.ServeHTTP(w, httptest.NewRequest("POST", "/resolve", strings.NewReader(string(raw))))
						if w.Code != 400 {
							t.Fatalf("replay=%d", w.Code)
						}
					}
				}
				for range 3 {
					if err := p.Pass(t.Context()); err != nil {
						t.Fatal(err)
					}
					n.now = n.now.Add(4 * time.Minute)
					if err := n.engine.Poll(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				boundZero(t, f, &local)
				posts, mails := noticeCounts(f, mail)
				if posts+mails+n.posts+n.local+len(n.mails)+len(n.messages(t)) != 0 {
					t.Fatalf("old act notice: provider=%d mail=%d posts=%d local=%d remote=%d tracked=%d", posts, mails, n.posts, n.local, len(n.mails), len(n.messages(t)))
				}
				w := httptest.NewRecorder()
				s.ServeHTTP(w, httptest.NewRequest("GET", "/status", nil))
				var status struct {
					Notices neversilent.Status `json:"never_silent"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if w.Code != 200 || !status.Notices.Enabled || len(status.Notices.Outages) != 0 {
					t.Fatalf("status=%s", w.Body.String())
				}
				// The same space still owes a newly addressed live message.
				fresh, _ := boundAct(t, f, "message.created", false, n.now, "new-notice")
				ev, err := puller.Decode(fresh)
				if err != nil {
					t.Fatal(err)
				}
				f.gc.Configure(func(g *fakegc.Server) { g.NextDelivery = "no_route" })
				if _, err := p.Handle(t.Context(), ev); err != nil {
					t.Fatal(err)
				}
				for range 3 {
					n.now = n.now.Add(4 * time.Minute)
					if err := p.Pass(t.Context()); err != nil {
						t.Fatal(err)
					}
					if err := n.engine.Poll(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if len(f.gc.Inbound()) != 1 || len(n.messages(t)) != 1 || n.posts != 1 || len(n.mails) != 1 || n.local != 1 {
					t.Fatalf("new act: turns=%d tracked=%d posts=%d mails=%d local=%d", len(f.gc.Inbound()), len(n.messages(t)), n.posts, len(n.mails), n.local)
				}
			})
		}
	}
}

func TestRetiredAttemptEndsItsNotice(t *testing.T) {
	for _, mode := range []string{"unresolved", "intent", "delivered", "queued", "queued-missing-time", "restore", "cursor"} {
		for _, alreadyNoticed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/noticed=%t", mode, alreadyNoticed), func(t *testing.T) {
				f := newFixture(t, false)
				now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
				before := now.Add(-time.Hour)
				id := "spaces/dm/messages/old"
				l := ledger.New()
				l.IdentityUserID = fakegoogle.UserID
				l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all"}
				if mode == "cursor" {
					l.Spaces["spaces/dm"].MessageCursor = before.Add(-time.Hour)
				}
				m := historyMessage("spaces/dm", "old", before)
				c := &ledger.ChangeState{ID: id, Message: id, Space: "spaces/dm", Kind: "message.created", Created: before, Status: "unresolved", Reason: "gc rejected the POST: example", NoticeAt: before}
				if mode == "intent" {
					c.Status, c.Reason = "intent", "retry"
					c.Attempts = 1
				}
				if mode == "delivered" {
					c.NoticeDelivered = true
				}
				l.Changes[id] = c
				if c.Status == "unresolved" {
					l.Unresolved[id] = c
				}
				if strings.HasPrefix(mode, "queued") {
					at := before
					if mode == "queued-missing-time" {
						m.CreateTime, m.LastUpdateTime = "", ""
						at = time.Time{}
					}
					raw, _ := json.Marshal(map[string]any{"change": puller.Change{Kind: "message.created", Space: "spaces/dm", Message: m}, "at": at, "handled": true})
					l.Spaces["spaces/dm"].Backlog = []string{string(raw)}
				}
				if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
					t.Fatal(err)
				}
				n := newHistoryNoticeSink(t, f.cfg.Endpoints.StateFile+".never-silent.json", before)
				if err := n.engine.Track(neversilent.Message{ID: id, Conversation: "spaces/dm", Created: before}); err != nil {
					t.Fatal(err)
				}
				if mode == "intent" {
					if err := n.engine.Result(id, "failed", nil); err != nil {
						t.Fatal(err)
					}
				} else if mode == "delivered" {
					if err := n.engine.Result(id, "delivered", nil); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := n.engine.Stop(id, true); err != nil {
						t.Fatal(err)
					}
				}
				n.now = now
				health := func() {
					raw, _ := json.Marshal(map[string]any{"state": "no_seat", "since": before, "checked_at": n.now})
					if err := os.WriteFile(n.opts.HealthFile, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "delivered" {
					health()
				}
				if alreadyNoticed {
					if err := n.engine.Poll(t.Context()); err != nil {
						t.Fatal(err)
					}
					if n.posts != 1 || len(n.mails) != 1 {
						t.Fatal("pre-load outage not established")
					}
				}
				if mode == "restore" {
					backup := filepath.Join(t.TempDir(), "backup.json")
					if err := l.Save(backup); err != nil {
						t.Fatal(err)
					}
					l.Replace(ledger.New())
					s, err := server.New(server.Options{Config: f.cfg, Ledger: l, LedgerMu: &f.mu, LedgerPath: f.cfg.Endpoints.StateFile})
					if err != nil {
						t.Fatal(err)
					}
					defer s.Close()
					s.SetNeverSilent(n.engine)
					s.SetHealth(server.Health{State: "recovery-required"})
					raw, _ := json.Marshal(map[string]string{"action": "restore", "file": backup})
					w := httptest.NewRecorder()
					s.ServeHTTP(w, httptest.NewRequest("POST", "/state", strings.NewReader(string(raw))))
					if w.Code != 200 {
						t.Fatalf("restore=%d %s", w.Code, w.Body.String())
					}
				} else {
					loaded, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
					if err != nil {
						t.Fatal(err)
					}
					l.Replace(loaded)
					if err := catchup.InitializeHistory(l, f.cfg, now, nil); err != nil {
						t.Fatal(err)
					}
					if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
						t.Fatal(err)
					}
					n.reload(t)
				}
				if mode != "cursor" && (len(l.Unresolved) != 0 || len(l.Spaces["spaces/dm"].Backlog) != 0) {
					t.Fatal("load did not retire attempt")
				}
				// A large untracked history must not grow the notice file.
				for i := range 100 {
					key := fmt.Sprintf("spaces/dm/messages/untracked-%d", i)
					l.Changes[key] = &ledger.ChangeState{ID: key, Message: key, Space: "spaces/dm", Kind: "message.created", Status: "committed", Reason: "local", Cause: "before_history_floor"}
				}
				a := &application{l: l, neverSilent: n.engine}
				for range 3 {
					n.now = n.now.Add(4 * time.Minute)
					if mode == "delivered" {
						health()
					}
					if err := a.neverSilentGate(); err != nil {
						t.Fatal(err)
					}
					if err := n.engine.Poll(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				wantPosts, wantMail, wantOpen := 0, 0, 0
				if alreadyNoticed {
					wantPosts, wantMail = 1, 2
				}
				if mode == "cursor" {
					wantPosts, wantMail, wantOpen = 1, 1, 1
				}
				t.Logf("posts=%d mail=%d local=%d open=%d tracked=%d", n.posts, len(n.mails), n.local, len(n.engine.Status().Outages), len(n.messages(t)))
				if n.posts != wantPosts || len(n.mails) != wantMail || n.local != wantPosts || len(n.engine.Status().Outages) != wantOpen {
					t.Errorf("after load: posts=%d/%d mail=%d/%d local=%d/%d open=%d/%d", n.posts, wantPosts, len(n.mails), wantMail, n.local, wantPosts, len(n.engine.Status().Outages), wantOpen)
				}
				if alreadyNoticed && mode != "cursor" && len(n.mails) == 2 && !strings.HasSuffix(n.mails[1].Subject, " cleared") {
					t.Error("ordinary cleared mail missing")
				}
				if len(n.messages(t)) != 1 {
					t.Errorf("history created %d notice records", len(n.messages(t)))
				}
			})
		}
	}
}
