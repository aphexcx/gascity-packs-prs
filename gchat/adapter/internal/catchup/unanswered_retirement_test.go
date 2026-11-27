package catchup

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestUnansweredPostNoticeAcrossLiveFirstRead(t *testing.T) {
	for _, name := range []string{"control", "first_read_before_raise", "first_read_during_post"} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			now := frozen
			// Use the current initializer and writer for every durable record.
			l := ledger.New()
			if _, err := InitializeHistoryWithReport(l, f.opts.Config, now.Add(-time.Minute), f.opts.Logger); err != nil {
				t.Fatal(err)
			}
			f.opts.Ledger = l
			notices := 0
			opts := inbound.Options{
				Config: f.opts.Config, Ledger: l, LedgerMu: f.opts.LedgerMu, LedgerPath: f.opts.LedgerPath,
				GC: gcclient.New(f.gc.URL(), "examplecity"), Chat: f.opts.Chat, IdentityUserID: "users/123",
				Logger: f.opts.Logger, Now: func() time.Time { return now },
				Raise: func(_ context.Context, r inbound.Raise) error {
					if !r.Ended {
						t.Fatal("unexpected waiting notice")
					}
					notices++
					return nil
				},
			}
			p, err := inbound.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			f.opts.Processor, f.opts.Now = p, opts.Now
			f.newRunner(t)
			const space = "spaces/room"
			f.addSpace(space, false)
			m := f.addMessage(space, 1, now)
			// The next list can see a newer membership, e.g. a leave and rejoin.
			joined := now.Add(2 * time.Second)
			member := &api.Membership{Name: space + "/members/123", State: "JOINED", CreateTime: joined.Format(time.RFC3339Nano)}
			f.google.Memberships[member.Name] = member
			swept := make(chan error, 1)
			step := fakegc.PostStep{Drop: true}
			if name == "first_read_during_post" {
				// A late successful reply must not overwrite the retired record.
				step = fakegc.PostStep{After: func(_ *httptest.ResponseRecorder) {
					swept <- f.r.Freeze(t.Context(), joined)
				}}
			}
			f.gc.Configure(func(s *fakegc.Server) { s.Posts = []fakegc.PostStep{step} })
			ev := &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: space, Message: m}}}
			if out, err := p.Handle(t.Context(), ev); err != nil || out != puller.Ack {
				t.Fatalf("live message: %v %v", out, err)
			}
			if name == "first_read_during_post" {
				if err := <-swept; err != nil {
					t.Fatal(err)
				}
			}
			c := l.Changes[m.Name]
			if c == nil || c.Status != "unresolved" || c.Cause != "unknown" || c.Attempts != 1 || !(c.Raised.IsZero() && c.RaisedEnd.IsZero()) {
				t.Fatalf("missing unannounced unknown end: %+v", c)
			}
			if name == "first_read_before_raise" {
				now = joined
				if err := f.r.Freeze(t.Context(), now); err != nil {
					t.Fatal(err)
				}
			}
			saved, err := ledger.Load(opts.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			opts.Ledger = saved
			p, err = inbound.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			if name != "control" {
				for _, record := range []*ledger.ChangeState{saved.Changes[m.Name], saved.Unresolved[m.Name]} {
					var turn wire.InboundMessage
					if record == nil || json.Unmarshal([]byte(record.Event), &turn) != nil || turn.ProviderMessageID != "" ||
						turn.Text == "" || strings.Contains(turn.Text, m.Text) || len(turn.Attachments) != 0 {
						t.Fatal("retirement lost the notice or retained a replayable payload")
					}
				}
			}
			if err := p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Hour)
			if err := p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			c = saved.Changes[m.Name]
			if notices != 1 || c.RaisedEnd.IsZero() || len(f.gc.Inbound()) != 1 || f.gc.Fanouts(m.Name) != 1 {
				t.Fatalf("notices=%d marked=%t posts=%d fan-outs=%d", notices, !c.RaisedEnd.IsZero(), len(f.gc.Inbound()), f.gc.Fanouts(m.Name))
			}
		})
	}
}
