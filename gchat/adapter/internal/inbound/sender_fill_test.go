package inbound

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

type fillSource struct {
	answer func(context.Context) (*api.User, error)
	calls  []string
}

func (s *fillSource) GetMessage(ctx context.Context, name string) (*api.Message, error) {
	s.calls = append(s.calls, name)
	user, err := s.answer(ctx)
	return &api.Message{Sender: user}, err
}

func (s *fillSource) GetMembership(ctx context.Context, space, id string) (*api.Membership, error) {
	s.calls = append(s.calls, space+" "+id)
	user, err := s.answer(ctx)
	return &api.Membership{Member: user}, err
}

type fillFixture struct {
	*fixture
	source fillSource
	log    bytes.Buffer
	now    time.Time
}

func fillSetup(t *testing.T, enabled bool) *fillFixture {
	t.Helper()
	f := &fillFixture{fixture: setup(t), now: time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)}
	f.l.IdentityDomainID = "example-org"
	// UNPROVEN: both GET methods return these two fields for the same sender.
	f.reply(&api.User{Name: "users/1", DomainId: "example-org", DisplayName: "Example Reader"}, nil)
	f.opts.Now = func() time.Time { return f.now }
	if enabled {
		f.opts.SenderFill = NewSenderFill(&f.source, slog.New(slog.NewTextHandler(&f.log, nil)))
		f.opts.SenderFill.now = f.opts.Now
	}
	var err error
	f.p, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fillFixture) reply(user *api.User, err error) {
	f.source.answer = func(context.Context) (*api.User, error) { return user, err }
}

func fillChange(kind, label, room string, user *api.User) puller.Change {
	// UNPROVEN: live and catch-up events can supply only the sender id.
	m := message()
	m.Name, m.Sender = room+"/messages/"+label, user
	m.Space = &api.Space{Name: room, SpaceType: "DIRECT_MESSAGE"}
	m.Thread = &api.Thread{Name: room + "/threads/topic"}
	m.CreateTime = "2026-01-02T11:00:00Z"
	c := puller.Change{Space: room, Kind: kind, Message: m, PublishTime: time.Date(2026, 1, 2, 11, 1, 0, 0, time.UTC)}
	if kind == "message.updated" {
		m.LastUpdateTime = c.PublishTime.Format(time.RFC3339Nano)
	}
	if strings.HasPrefix(kind, "reaction.") {
		c.Reaction = &api.Reaction{Name: m.Name + "/reactions/example", User: user, Emoji: &api.Emoji{Unicode: "+"}}
		c.Message = nil
	}
	return c
}

func (f *fillFixture) deliver(t *testing.T, c puller.Change, replay bool) string {
	t.Helper()
	before, _ := json.Marshal(c)
	name := ""
	if c.Reaction != nil {
		name = strings.Split(c.Reaction.Name, "/reactions/")[0]
		f.l.Messages[name] = &ledger.MessageState{Mine: true}
	} else {
		name = c.Message.Name
	}
	var out puller.Outcome
	var err error
	if replay {
		out, err = f.p.HandleChange(t.Context(), c)
	} else {
		out, err = f.p.Handle(t.Context(), &puller.Event{Changes: []puller.Change{c}})
	}
	if err != nil || out != puller.Ack {
		t.Fatalf("handle %s: %v %v", c.Kind, out, err)
	}
	after, _ := json.Marshal(c)
	if !bytes.Equal(before, after) {
		t.Fatal("delivered change mutated")
	}
	for _, row := range f.l.Changes {
		if row.Message == name && row.Kind == c.Kind {
			return row.Event
		}
	}
	t.Fatalf("missing %s row for %s", c.Kind, name)
	return ""
}

func assertFillFrame(t *testing.T, raw, name, id, class string, replay bool) {
	t.Helper()
	var w wire.InboundMessage
	if err := json.Unmarshal([]byte(raw), &w); err != nil {
		t.Fatal(err)
	}
	if w.Actor.ID != id || w.Actor.DisplayName != name || !strings.Contains(w.Text, "<"+id+"> | "+class) || strings.HasPrefix(w.Text, "[catch-up]\n") != replay {
		t.Fatalf("actor and frame = %+v %q", w.Actor, w.Text)
	}
	if name != id && !strings.Contains(w.Text, "[from: "+name+" <") {
		t.Fatalf("frame name: %q", w.Text)
	}
}

func TestSenderFillFrames(t *testing.T) {
	for _, tc := range []struct {
		kind        string
		replay, old bool
	}{
		{"message.created", false, false}, {"message.updated", false, false}, {"reaction.created", false, false}, {"reaction.created", true, false}, {"message.created", false, true},
	} {
		t.Run(fmt.Sprintf("%s/replay=%t/old=%t", tc.kind, tc.replay, tc.old), func(t *testing.T) {
			f := fillSetup(t, true)
			if tc.old {
				f.l.HistoryFloor = f.now
			}
			c := fillChange(tc.kind, "one", space, &api.User{Name: "users/1", Type: "HUMAN"})
			assertFillFrame(t, f.deliver(t, c, tc.replay), "Example Reader", "users/1", "organization", tc.replay)
			want := space + "/messages/one"
			if c.Reaction != nil {
				want = space + " users/1"
			}
			if len(f.source.calls) != 1 || f.source.calls[0] != want {
				t.Fatalf("requests = %v", f.source.calls)
			}
		})
	}
	t.Run("keep delivered fields", func(t *testing.T) {
		for _, u := range []*api.User{{Name: "users/1", DisplayName: "Delivered"}, {Name: "users/1", DomainId: "other"}} {
			f := fillSetup(t, true)
			want, class := "Example Reader", "external"
			if u.DisplayName != "" {
				want, class = u.DisplayName, "organization"
			}
			assertFillFrame(t, f.deliver(t, fillChange("message.created", "one", space, u), false), want, u.Name, class, false)
		}
	})
	t.Run("learn identity then classify teammate", func(t *testing.T) {
		f := fillSetup(t, true)
		f.l.IdentityDomainID = ""
		f.reply(&api.User{Name: identity, DomainId: "example-org", DisplayName: "Example Identity"}, nil)
		f.deliver(t, fillChange("message.created", "own", space, &api.User{Name: identity}), false)
		if f.l.IdentityDomainID != "example-org" {
			t.Fatalf("learned domain = %q", f.l.IdentityDomainID)
		}
		f.reply(&api.User{Name: "users/1", DomainId: "example-org", DisplayName: "Example Reader"}, nil)
		assertFillFrame(t, f.deliver(t, fillChange("message.created", "other", space, &api.User{Name: "users/1"}), false), "Example Reader", "users/1", "organization", false)
	})
}

func TestSenderFillTrust(t *testing.T) {
	type row struct {
		name      string
		answer    *api.User
		err       error
		delivered string
	}
	rows := []row{
		{"transport", nil, errors.New("private error"), ""}, {"timeout", nil, context.DeadlineExceeded, ""}, {"missing user", nil, nil, ""},
		{"different user", &api.User{Name: "users/2", DomainId: "example-org", DisplayName: "Other"}, nil, ""},
		{"no domain", &api.User{Name: "users/1", DisplayName: "Example Reader"}, nil, ""},
		{"anonymous", &api.User{Name: "users/1", DomainId: "example-org", IsAnonymous: true}, nil, ""},
		{"bot", &api.User{Name: "users/1", DomainId: "example-org", Type: "BOT"}, nil, ""},
		{"keep name on failure", nil, errors.New("private error"), "Delivered"},
	}
	for _, status := range []int{401, 403, 404, 429, 500} {
		rows = append(rows, row{fmt.Sprint(status), nil, &googleapi.Error{Code: status}, ""})
	}
	for _, kind := range []string{"message.created", "reaction.created"} {
		for _, tc := range rows {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				f, base := fillSetup(t, true), fillSetup(t, false)
				f.reply(tc.answer, tc.err)
				c := fillChange(kind, "one", space, &api.User{Name: "users/1", DisplayName: tc.delivered})
				if got, want := f.deliver(t, c, false), base.deliver(t, c, false); got != want {
					t.Fatalf("failed fill changed turn\ngot %s\nwant %s", got, want)
				}
			})
		}
		for _, test := range []string{"different cached user", "expired entry"} {
			t.Run(kind+"/"+test, func(t *testing.T) {
				f, base := fillSetup(t, true), fillSetup(t, false)
				u := &api.User{Name: "users/1"}
				f.deliver(t, fillChange(kind, "seed", space, u), false)
				if test == "different cached user" {
					u = &api.User{Name: "users/2"}
				} else {
					f.now = f.now.Add(time.Hour)
					base.now = f.now
				}
				f.reply(nil, errors.New("private error"))
				c := fillChange(kind, "next", space, u)
				if got, want := f.deliver(t, c, false), base.deliver(t, c, false); got != want {
					t.Fatalf("cached fields entered failed turn: %s", got)
				}
			})
		}
	}
}

func TestSenderFillExternalDomain(t *testing.T) {
	for _, kind := range []string{"message.created", "reaction.created"} {
		for _, domain := range []string{"different", ""} {
			f := fillSetup(t, true)
			f.l.IdentityDomainID = domain
			u := &api.User{Name: "users/1"}
			assertFillFrame(t, f.deliver(t, fillChange(kind, "one", space, u), false), "Example Reader", u.Name, "external", false)
		}
	}
}

func TestSenderFillCauses(t *testing.T) {
	for _, kind := range []string{"message.created", "reaction.created"} {
		for _, row := range []struct {
			cause string
			user  *api.User
			err   error
		}{
			{"no_user", nil, nil}, {"other_user", &api.User{Name: "users/2"}, nil},
			{"not_a_person", &api.User{Name: "users/1", Type: "BOT"}, nil},
			{"no_domain_id", &api.User{Name: "users/1", DisplayName: "Example Reader"}, nil},
			{"timeout", nil, context.DeadlineExceeded}, {"http_429", nil, errors.Join(context.DeadlineExceeded, &googleapi.Error{Code: 429})},
		} {
			f := fillSetup(t, true)
			f.reply(row.user, row.err)
			f.deliver(t, fillChange(kind, "one", space, &api.User{Name: "users/1"}), false)
			if !strings.Contains(f.log.String(), "cause="+row.cause+"\n") {
				t.Errorf("%s %s: %s", kind, row.cause, f.log.String())
			}
		}
	}
}
