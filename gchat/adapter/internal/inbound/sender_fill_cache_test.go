package inbound

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

func TestSenderFillBound(t *testing.T) {
	f, base := fillSetup(t, true), fillSetup(t, false)
	if f.opts.SenderFill.bound != 3*time.Second {
		t.Fatalf("default bound = %v", f.opts.SenderFill.bound)
	}
	const short = 30 * time.Millisecond
	f.opts.SenderFill.bound = short
	var bounded bool
	f.source.answer = func(ctx context.Context) (*api.User, error) {
		deadline, ok := ctx.Deadline()
		bounded = ok && time.Until(deadline) <= short
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
			return nil, errors.New("test timeout: no fill deadline")
		}
	}
	for _, label := range []string{"first", "cached"} {
		c := fillChange("message.created", label, space, &api.User{Name: "users/1"})
		if got, want := f.deliver(t, c, false), base.deliver(t, c, false); got != want || len(f.gc.Inbound()) != len(base.gc.Inbound()) || len(f.gc.Inbound()) == 0 {
			t.Fatal("bounded fill changed delivery")
		}
	}
	if !bounded || len(f.source.calls) != 1 || !strings.Contains(f.log.String(), "cause=timeout") {
		t.Fatalf("bounded=%t requests=%d log=%s", bounded, len(f.source.calls), f.log.String())
	}
}

func TestSenderFillCallerCanceled(t *testing.T) {
	f := fillSetup(t, true)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.source.answer = func(call context.Context) (*api.User, error) { cancel(); <-call.Done(); return nil, call.Err() }
	c := fillChange("message.created", "one", space, &api.User{Name: "users/1"})
	out, err := f.p.Handle(ctx, &puller.Event{Changes: []puller.Change{c}})
	if out != puller.Nack || !errors.Is(err, context.Canceled) || f.log.Len() != 0 {
		t.Fatalf("interrupted Handle = %v, %v; log %s", out, err, f.log.String())
	}
	f.reply(&api.User{Name: "users/1", DomainId: "example-org", DisplayName: "Example Reader"}, nil)
	assertFillFrame(t, f.deliver(t, c, false), "Example Reader", "users/1", "organization", false)
	if len(f.source.calls) != 2 {
		t.Fatalf("requests = %v", f.source.calls)
	}
}

func TestSenderFillCache(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed=%t", failed), func(t *testing.T) {
			f := fillSetup(t, true)
			life := time.Hour
			if failed {
				life = 10 * time.Minute
				f.reply(nil, errors.New("fixture failure"))
			}
			start := f.now
			for i, row := range []struct {
				elapsed    time.Duration
				kind, room string
				calls      int
			}{
				{0, "message.created", space, 1}, {life - time.Minute, "reaction.created", "spaces/second", 1}, {life + time.Minute, "message.created", space, 2},
			} {
				f.now = start.Add(row.elapsed)
				// The reaction's parent is already in the ledger, and this space is known.
				f.l.Spaces[row.room] = &ledger.SpaceState{Kind: "dm", Wake: "all", JoinedAt: start.Add(-24 * time.Hour)}
				c := fillChange(row.kind, fmt.Sprint(i), row.room, &api.User{Name: "users/1"})
				raw := f.deliver(t, c, false)
				if !failed {
					assertFillFrame(t, raw, "Example Reader", "users/1", "organization", false)
				}
				if len(f.source.calls) != row.calls {
					t.Fatalf("at %s calls=%v", row.elapsed, f.source.calls)
				}
			}
		})
	}
	f := fillSetup(t, true)
	for i, change := range []string{"initial", "identity", "domain", "restart"} {
		switch change {
		case "identity":
			f.p.opts.IdentityUserID = "users/99"
		case "domain":
			f.l.IdentityDomainID = "different"
		case "restart":
			f.p.opts.SenderFill = NewSenderFill(&f.source, f.opts.Logger)
		}
		f.deliver(t, fillChange("message.created", fmt.Sprint(i), space, &api.User{Name: "users/1"}), false)
		if len(f.source.calls) != i+1 {
			t.Fatalf("%s: %v", change, f.source.calls)
		}
	}
}

func TestSenderFillEligibility(t *testing.T) {
	for _, user := range []*api.User{
		{Name: identity}, {Name: "users/made-up"}, {Name: "users/2", Type: "BOT"}, {Name: "users/2", IsAnonymous: true},
		{Name: "users/2", DisplayName: "Delivered", DomainId: "example-org"},
	} {
		f, base := fillSetup(t, true), fillSetup(t, false)
		c := fillChange("message.created", "one", space, user)
		if got, want := f.deliver(t, c, false), base.deliver(t, c, false); got != want || len(f.source.calls) != 0 || f.log.Len() != 0 {
			t.Fatalf("ineligible user %+v changed behavior", user)
		}
	}
}

func TestSenderFillDeletion(t *testing.T) {
	for _, history := range []bool{false, true} {
		for _, cache := range []string{"cold", "warm", "expired"} {
			t.Run(fmt.Sprintf("history=%t/%s", history, cache), func(t *testing.T) {
				f, base := fillSetup(t, true), fillSetup(t, false)
				// Keep an existing message whose original frame has only the id.
				create := fillChange("message.created", "target", space, &api.User{Name: "users/1"})
				option := f.p.opts.SenderFill
				f.p.opts.SenderFill = nil
				f.deliver(t, create, false)
				f.p.opts.SenderFill = option
				base.deliver(t, create, false)
				if cache != "cold" {
					f.deliver(t, fillChange("message.created", "seed", space, &api.User{Name: "users/1"}), false)
				}
				if cache == "expired" {
					f.now = f.now.Add(time.Hour)
					base.now = f.now
				}
				if history {
					f.l.HistoryFloor = f.now
					base.l.HistoryFloor = base.now
				}
				c := fillChange("message.deleted", "target", space, &api.User{Name: "users/1"})
				before := len(f.source.calls)
				got := f.deliver(t, c, false)
				if cache == "warm" {
					assertFillFrame(t, got, "Example Reader", "users/1", "organization", false)
				} else if want := base.deliver(t, c, false); got != want {
					t.Fatalf("cache miss changed deletion: %s", got)
				}
				if len(f.source.calls) != before || f.log.Len() != 0 {
					t.Fatalf("deletion requested or logged a lookup: %v %s", f.source.calls, f.log.String())
				}
			})
		}
	}
}

func TestSenderFillLog(t *testing.T) {
	f := fillSetup(t, true)
	f.reply(nil, &googleapi.Error{Code: 403, Message: "https://example.invalid/private"})
	for i := range 20 {
		f.now = f.now.Add(11 * time.Minute)
		f.deliver(t, fillChange("message.created", fmt.Sprint(i), space, &api.User{Name: fmt.Sprintf("users/%d", 1+i%5)}), false)
	}
	f.l.IdentityDomainID = "another"
	f.deliver(t, fillChange("reaction.created", "second", space, &api.User{Name: "users/6"}), false)
	f.reply(nil, errors.New("example@example.com"))
	f.deliver(t, fillChange("message.created", "third", space, &api.User{Name: "users/7"}), false)
	for _, want := range []string{"source=message cause=http_403", "source=member cause=http_403", "source=message cause=error"} {
		if strings.Count(f.log.String(), want) != 1 {
			t.Errorf("missing/duplicate %q in %s", want, f.log.String())
		}
	}
	if strings.Count(f.log.String(), "level=WARN") != 3 {
		t.Errorf("log = %s", f.log.String())
	}
	for _, private := range []string{"users/", "spaces/", "Example Reader", "example.com", "https://"} {
		if strings.Contains(f.log.String(), private) {
			t.Fatalf("log includes %q", private)
		}
	}
}

func TestSenderFillMissingDisplayName(t *testing.T) {
	for _, kind := range []string{"message.created", "reaction.created"} {
		for _, domain := range []string{"example-org", "other"} {
			f := fillSetup(t, true)
			class := "organization"
			if domain != "example-org" {
				class = "external"
			}
			for i, id := range []string{"users/1", "users/2", "users/1"} {
				// UNPROVEN: a GET can supply the verified domain without a display name.
				f.reply(&api.User{Name: id, DomainId: domain}, nil)
				assertFillFrame(t, f.deliver(t, fillChange(kind, fmt.Sprint(i), space, &api.User{Name: id}), false), id, id, class, false)
			}
			source := "message"
			if kind == "reaction.created" {
				source = "member"
			}
			if len(f.source.calls) != 2 || strings.Count(f.log.String(), "level=WARN") != 1 || !strings.Contains(f.log.String(), "source="+source+" cause=no_display_name") {
				t.Errorf("requests=%v log=%s", f.source.calls, f.log.String())
			}
		}
	}
}
