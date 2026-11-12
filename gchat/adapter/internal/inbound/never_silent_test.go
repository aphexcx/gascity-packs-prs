package inbound

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestNeverSilentReconcilesPrimaryTerminalTransitions(t *testing.T) {
	for _, reason := range []string{reasonExhausted, "no_route", "operator accepted", "operator replay", "delivered", reasonLanded} {
		t.Run(reason, func(t *testing.T) {
			rf := retrySetup(t)
			dir := t.TempDir()
			posts := 0
			opts := neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: filepath.Join(dir, "health.json"), Now: rf.clock.Now, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }}
			ns, err := neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			id := "example-crash"
			if err = ns.Track(neversilent.Message{ID: id, Conversation: space, Created: rf.clock.Now()}); err != nil {
				t.Fatal(err)
			}
			if reason != reasonExhausted {
				if err = ns.Stop(id, false); err != nil {
					t.Fatal(err)
				}
			}
			// Primary write succeeds; the process dies before its sidecar observation.
			status := "committed"
			if reason == reasonExhausted {
				status = "unresolved"
			}
			rf.l.Changes[id] = &ledger.ChangeState{ID: id, Space: space, Status: status, Reason: reason, Created: rf.clock.Now()}
			ns, err = neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			rf.opts.NeverSilent = ns
			rf.rebuild(t)
			if err = rf.p.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			rf.clock.Add(3 * time.Minute)
			if err = ns.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if reason == reasonExhausted || reason == "no_route" {
				want = 1
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d", posts, want)
			}
		})
	}
}

func TestNeverSilentRepairsSuccessfulPublishActivityFromLedger(t *testing.T) {
	rf := retrySetup(t)
	dir := t.TempDir()
	posts := 0
	ns, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), Now: rf.clock.Now, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err = ns.Track(neversilent.Message{ID: "example", Conversation: space, Thread: "original", Targets: []string{"example-session"}, Created: rf.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	if err = ns.Stop("example", false); err != nil {
		t.Fatal(err)
	}
	rf.clock.Add(time.Minute)
	rf.l.Sends["example-reply"] = &ledger.SendState{Space: space, Request: &wire.PublishRequest{SessionID: "example-session"}, Parts: []ledger.SendPart{{Text: "example reply", MessageName: "example-message", SentAt: rf.clock.Now()}}}
	rf.opts.NeverSilent = ns
	rf.rebuild(t)
	if err = rf.p.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	rf.clock.Add(3 * time.Minute)
	if err = ns.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("lost durable session reply")
	}
}

func TestNeverSilentNewAddressedMessagesOnly(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		bot, catchup, ambient, edit, off bool
	}{
		{name: "new dm"}, {name: "bot", bot: true}, {name: "catchup", catchup: true}, {name: "ambient", ambient: true}, {name: "edit", edit: true}, {name: "off", off: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf := retrySetup(t)
			dir := t.TempDir()
			posts := 0
			mails := 0
			ns, err := neversilent.New(neversilent.Options{Enabled: !tc.off, Path: filepath.Join(dir, "state.json"), HealthFile: filepath.Join(dir, "health.json"), City: "example-city", Channel: "gchat", OperatorContext: "example-operator", Now: rf.clock.Now,
				Post: func(ctx context.Context, m neversilent.Message, line string) error {
					posts++
					_, _, err := rf.opts.Chat.CreateNotice(ctx, m.Conversation, line, m.Thread, !tc.ambient)
					return err
				},
				Send: func(context.Context, neversilent.Mail) error { mails++; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			rf.opts.NeverSilent = ns
			rf.rebuild(t)
			m := message()
			kind := "message.created"
			if tc.edit {
				kind = "message.updated"
				m.LastUpdateTime = "2026-10-22T10:00:01Z"
			}
			if tc.bot {
				m.Sender.Type = "BOT"
			}
			if !tc.ambient {
				m.Space.SpaceType = "DIRECT_MESSAGE"
				rf.google.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = m.Space })
			}
			rf.script(repeat(12, failedWith("unavailable"))...)
			if tc.catchup {
				_, err = rf.p.HandleChange(context.Background(), puller.Change{Kind: kind, Message: m, Space: space})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				handle(t, rf.fixture, event("example-event", kind, m))
			}
			rf.clock.Add(3 * time.Minute)
			rf.poll(t)
			want := 0
			if tc.name == "new dm" {
				want = 1
			}
			if posts != want || mails != want {
				t.Fatalf("posts=%d mails=%d want=%d", posts, mails, want)
			}
			if rf.change(t, messageName).Status != "intent" {
				t.Fatal("notice changed retry status")
			}
		})
	}
}

func TestNeverSilentLandedPendingAndBusyPending(t *testing.T) {
	for _, whole := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy", true: "whole paste"}[whole], func(t *testing.T) {
			rf := retrySetup(t)
			dir := t.TempDir()
			health := filepath.Join(dir, "health.json")
			posts := 0
			ns, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: health, Now: rf.clock.Now, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
			if err != nil {
				t.Fatal(err)
			}
			rf.opts.NeverSilent = ns
			rf.rebuild(t)
			m := message()
			m.Space.SpaceType = "DIRECT_MESSAGE"
			rf.google.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = &api.Space{Name: space, SpaceType: "DIRECT_MESSAGE"} })
			rf.script(wire.Delivery{Status: "pending", ReceiptID: "example-receipt"})
			handle(t, rf.fixture, event("example-event", "message.created", m))
			if whole {
				if err = rf.p.concludedPending(messageName, wire.Delivery{Status: "pending", Members: []wire.DeliveryMember{{Status: "pending", ExpectedBytes: 100, DeliveredBytes: 100, SessionID: "example-session"}}}); err != nil {
					t.Fatal(err)
				}
			}
			rf.clock.Add(3 * time.Minute)
			stamp := rf.clock.Now().Format(time.RFC3339)
			if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = ns.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			want := 0
			if whole {
				want = 1
			}
			if posts != want {
				t.Fatalf("posts=%d want=%d", posts, want)
			}
		})
	}
}

func TestNeverSilentTerminalWholePasteIsDeliveredAcrossRestart(t *testing.T) {
	for _, memberTerminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "delivery terminal", true: "member terminal"}[memberTerminal], func(t *testing.T) {
			rf := retrySetup(t)
			dir := t.TempDir()
			posts, mails := 0, 0
			opts := neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: filepath.Join(dir, "health.json"), OperatorContext: "example", Now: rf.clock.Now,
				Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }, Send: func(context.Context, neversilent.Mail) error { mails++; return nil }}
			ns, err := neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			rf.opts.NeverSilent = ns
			rf.rebuild(t)
			m := message()
			m.Space.SpaceType = "DIRECT_MESSAGE"
			rf.google.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = &api.Space{Name: space, SpaceType: "DIRECT_MESSAGE"} })
			rf.script(wire.Delivery{Status: "pending", ReceiptID: "example-receipt"})
			handle(t, rf.fixture, event("example-event", "message.created", m))
			d := wire.Delivery{Status: "pending", Terminal: !memberTerminal, Members: []wire.DeliveryMember{{Status: "pending", Terminal: memberTerminal, ExpectedBytes: 100, DeliveredBytes: 100, SessionID: "mayor"}}}
			if err = rf.p.concludedPending(messageName, d); err != nil {
				t.Fatal(err)
			}
			if rf.change(t, messageName).Status != "unresolved" {
				t.Fatal("primary operator hold changed")
			}
			if err = ns.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if mails != 0 {
				t.Fatal("whole paste opened not-delivered outage")
			}
			ns, err = neversilent.New(opts)
			if err != nil {
				t.Fatal(err)
			}
			rf.opts.NeverSilent = ns
			rf.rebuild(t)
			if err = rf.p.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			rf.clock.Add(3 * time.Minute)
			if err = ns.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts != 0 || mails != 0 {
				t.Fatal("restart restored a false stop")
			}
			stamp := rf.clock.Now().Format(time.RFC3339)
			if err = os.WriteFile(opts.HealthFile, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err = ns.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if posts != 1 {
				t.Fatal("whole paste did not preserve delivered health eligibility")
			}
		})
	}
}

func TestNeverSilentWithdrawalAfterDelivery(t *testing.T) {
	rf := retrySetup(t)
	dir := t.TempDir()
	posts := 0
	health := filepath.Join(dir, "health.json")
	ns, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "state.json"), HealthFile: health, Now: rf.clock.Now, Post: func(context.Context, neversilent.Message, string) error { posts++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	rf.opts.NeverSilent = ns
	rf.rebuild(t)
	m := message()
	m.Space.SpaceType = "DIRECT_MESSAGE"
	rf.google.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = m.Space })
	rf.script(wire.Delivery{Status: "delivered"})
	handle(t, rf.fixture, event("example-new", "message.created", m))
	handle(t, rf.fixture, event("example-deleted", "message.deleted", m))
	rf.clock.Add(3 * time.Minute)
	stamp := rf.clock.Now().Format(time.RFC3339)
	if err = os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = ns.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatal("notice answered a withdrawn message")
	}
}
