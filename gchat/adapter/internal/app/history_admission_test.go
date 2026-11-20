package app

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

func TestHalfSavedAdmissionKeepsItsNotices(t *testing.T) {
	for _, mode := range []string{"save-failure", "crash-reload"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, false)
			now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			const id = "spaces/dm/messages/one"
			l := ledger.New()
			l.IdentityUserID = fakegoogle.UserID
			l.HistoryFloor = now.Add(-time.Minute)
			l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", ChatType: "DIRECT_MESSAGE", Wake: "all", MessageCursor: l.HistoryFloor}
			key := id + "#history#message.created#0001-01-01T00:00:00Z"
			l.Changes[key] = &ledger.ChangeState{ID: key, Message: id, Space: "spaces/dm", Kind: "message.created", Status: "committed", Reason: "local", Cause: "history_time_missing", Created: now.Add(-time.Second)}
			if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
				t.Fatal(err)
			}
			n := newHistoryNoticeSink(t, f.cfg.Endpoints.StateFile+".never-silent.json", now)
			a := &application{l: l, neverSilent: n.engine}
			client, err := chat.New(t.Context(), http.DefaultClient, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			failSave, failed := true, 0
			processor := func() *inbound.Processor {
				t.Helper()
				p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: l, LedgerMu: &a.mu, LedgerPath: f.cfg.Endpoints.StateFile,
					Chat: client, GC: gcclient.New(f.gc.URL(), "examplecity"), IdentityUserID: fakegoogle.UserID,
					NeverSilent: n.engine, Now: func() time.Time { return n.now }, Save: func(next *ledger.Ledger) error {
						if failSave && next.Changes[id] != nil {
							failed++
							return errors.New("primary save interrupted")
						}
						return next.Save(f.cfg.Endpoints.StateFile)
					}})
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			p := processor()
			m := historyMessage("spaces/dm", "one", now)
			m.Space = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
			ev := &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/dm", Message: m}}}
			out, err := p.Handle(t.Context(), ev)
			if out != puller.Nack || err == nil || failed != 1 || l.Changes[id] != nil {
				t.Fatalf("partial save: outcome=%v err=%v failures=%d primary=%v", out, err, failed, l.Changes[id] != nil)
			}
			if _, ok := n.messages(t)[id]; !ok {
				t.Fatal("enrollment did not persist before primary save")
			}
			if mode == "crash-reload" {
				// Discard both in-memory stores: the persisted pair is exactly the
				// crash boundary after Track and before the primary ledger save.
				l, err = ledger.Load(f.cfg.Endpoints.StateFile, true)
				if err != nil {
					t.Fatal(err)
				}
				if l.Changes[id] != nil {
					t.Fatal("primary unexpectedly on disk")
				}
				if err := catchup.InitializeHistory(l, f.cfg, now, nil); err != nil {
					t.Fatal(err)
				}
				if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
					t.Fatal(err)
				}
				n.reload(t)
				a = &application{l: l, neverSilent: n.engine}
				p = processor()
			}
			if err := a.neverSilentGate(); err != nil {
				t.Fatal(err)
			}
			failSave = false
			f.gc.Configure(func(g *fakegc.Server) { g.NextDelivery = "no_route" })
			if out, err := p.Handle(t.Context(), ev); out != puller.Ack || err != nil {
				t.Fatalf("redelivery: outcome=%v err=%v", out, err)
			}
			for range 3 {
				n.now = n.now.Add(4 * time.Minute)
				if err := a.pollNeverSilentOnce(t.Context()); err != nil {
					t.Fatal(err)
				}
				if n.posts != 1 || len(n.mails) != 1 || n.messages(t)[id].Removed {
					t.Errorf("current admission: owner=%d mail=%d removed=%t", n.posts, len(n.mails), n.messages(t)[id].Removed)
				}
			}
		})
	}
}
