package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/server"
	api "google.golang.org/api/chat/v1"
)

// The two sides of this class are deliberately scheduled with channels:
// admission must not overtake a retirement snapshot, and restore must not
// split a watch pass between its snapshot and its externally visible effects.
func TestHistoryNoticeInterleavings(t *testing.T) {
	for _, mode := range []string{"snapshot-before-admission", "track-before-record", "restore-before-poll", "restore-during-poll"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, false)
			now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			id := "spaces/dm/messages/one"
			l := ledger.New()
			l.IdentityUserID = fakegoogle.UserID
			n := newHistoryNoticeSink(t, f.cfg.Endpoints.StateFile+".never-silent.json", now)
			a := &application{l: l, neverSilent: n.engine}
			await := func(done <-chan error) {
				t.Helper()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("operation did not finish")
				}
			}
			if strings.Contains(mode, "admission") || mode == "track-before-record" {
				l.HistoryFloor = now.Add(-time.Minute)
				l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", ChatType: "DIRECT_MESSAGE", Wake: "all", MessageCursor: l.HistoryFloor}
				key := id + "#history#message.created#0001-01-01T00:00:00Z"
				l.Changes[key] = &ledger.ChangeState{ID: key, Message: id, Space: "spaces/dm", Kind: "message.created", Status: "committed", Reason: "local", Cause: "history_time_missing", Created: now.Add(-time.Second)}
				entered, release := make(chan struct{}), make(chan struct{})
				var first atomic.Bool
				if mode == "track-before-record" {
					n.opts.Guard = func() error {
						if !first.Swap(true) {
							close(entered)
							<-release
						}
						return nil
					}
					n.reload(t)
					a.neverSilent = n.engine
				}
				c, err := chat.New(t.Context(), http.DefaultClient, f.cfg.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: l, LedgerMu: &a.mu, LedgerPath: f.cfg.Endpoints.StateFile, Chat: c, GC: gcclient.New(f.gc.URL(), "examplecity"), IdentityUserID: fakegoogle.UserID, NeverSilent: n.engine, Now: func() time.Time { return now }})
				if err != nil {
					t.Fatal(err)
				}
				f.gc.Configure(func(g *fakegc.Server) { g.NextDelivery = "no_route" })
				m := historyMessage("spaces/dm", "one", now)
				m.Space = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
				admitted, reconciled := make(chan error, 1), make(chan error, 1)
				admit := func() {
					_, err := p.Handle(t.Context(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/dm", Message: m}}})
					admitted <- err
				}
				if mode == "snapshot-before-admission" {
					go func() {
						reconciled <- n.engine.WithHistory(func() error {
							a.mu.Lock()
							snapshot := a.l.Clone()
							a.mu.Unlock()
							close(entered)
							<-release
							return inbound.ReconcileNeverSilent(snapshot, n.engine)
						})
					}()
					<-entered
					go admit()
				} else {
					go admit()
					<-entered
					go func() { reconciled <- a.neverSilentGate() }()
				}
				close(release)
				await(admitted)
				await(reconciled)
				n.now = now.Add(4 * time.Minute)
				if err := a.pollNeverSilentOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				if n.posts != 1 || len(n.mails) != 1 || n.messages(t)[id].Removed {
					t.Fatalf("current create silenced: owner=%d mail=%d removed=%t", n.posts, len(n.mails), n.messages(t)[id].Removed)
				}
				return
			}
			if err := n.engine.Track(neversilent.Message{ID: id, Conversation: "spaces/dm", Created: now.Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			if err := n.engine.StopAt(id, true, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			backup := ledger.New()
			backup.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all"}
			old := &ledger.ChangeState{ID: id, Message: id, Space: "spaces/dm", Kind: "message.created", Status: "unresolved", Created: now.Add(-time.Hour)}
			backup.Changes[id], backup.Unresolved[id] = old, old
			path := filepath.Join(t.TempDir(), "backup.json")
			if err := backup.Save(path); err != nil {
				t.Fatal(err)
			}
			s, err := server.New(server.Options{Config: f.cfg, Ledger: l, LedgerMu: &a.mu, LedgerPath: f.cfg.Endpoints.StateFile})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.SetNeverSilent(n.engine)
			s.SetHealth(server.Health{State: "recovery-required"})
			restore := func() {
				t.Helper()
				raw, _ := json.Marshal(map[string]string{"action": "restore", "file": path})
				w := httptest.NewRecorder()
				s.ServeHTTP(w, httptest.NewRequest("POST", "/state", strings.NewReader(string(raw))))
				if w.Code != 200 || len(l.Unresolved) != 0 {
					t.Fatalf("restore=%d", w.Code)
				}
			}
			if mode == "restore-before-poll" {
				a.noticeStateMu.Lock()
				done := make(chan error, 1)
				go func() { done <- a.pollNeverSilentOnce(t.Context()) }()
				restore()
				a.noticeStateMu.Unlock()
				await(done)
				if n.posts != 0 || len(n.mails) != 0 {
					t.Fatalf("retired after restore: owner=%d mail=%d", n.posts, len(n.mails))
				}
			} else {
				entered, release := make(chan struct{}), make(chan struct{})
				n.opts.Post = func(context.Context, neversilent.Message, string) error {
					close(entered)
					<-release
					n.posts++
					return nil
				}
				n.reload(t)
				a.neverSilent = n.engine
				done := make(chan error, 1)
				go func() { done <- a.pollNeverSilentOnce(t.Context()) }()
				<-entered
				// This is the same mutex held by the production POST /state
				// wrapper. If available now, restore can split this pass.
				if a.noticeStateMu.TryLock() {
					restore()
					a.noticeStateMu.Unlock()
					t.Error("restore overtook an in-flight notice decision")
				}
				close(release)
				await(done)
				if n.posts != 1 || len(n.mails) != 1 {
					t.Fatal("pre-restore outage missing")
				}
				a.noticeStateMu.Lock()
				restore()
				a.noticeStateMu.Unlock()
				if err := a.pollNeverSilentOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				if n.posts != 1 || len(n.mails) != 2 || !strings.HasSuffix(n.mails[1].Subject, " cleared") {
					t.Fatalf("post-restore effects: owner=%d mails=%v", n.posts, n.mails)
				}
			}
			if len(n.engine.Status().Outages) != 0 {
				t.Fatal("retired outage remained open")
			}
		})
	}
}
