package catchup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

func join(t *testing.T, f *fixture, at time.Time) {
	t.Helper()
	out, err := f.p.Handle(t.Context(), &puller.Event{Changes: []puller.Change{{Kind: "membership.created", Space: "spaces/joined", Membership: &api.Membership{Name: "spaces/joined/members/123", State: "JOINED", CreateTime: at.Format(time.RFC3339Nano), Member: &api.User{Name: "users/123", Type: "HUMAN"}}}}})
	if out != puller.Ack || err != nil {
		t.Fatalf("join: %v %v", out, err)
	}
}

func mention(m *api.Message) {
	m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: "users/123"}}}}
}

func historyLower(t *testing.T, f *fixture, want time.Time) {
	t.Helper()
	if len(f.google.HistoryRequests) == 0 {
		t.Fatal("no history request")
	}
	for _, req := range f.google.HistoryRequests {
		if req.Space == "spaces/joined" && req.Lower.Before(want) {
			t.Fatalf("request lower %v before %v", req.Lower, want)
		}
	}
	if got := f.google.HistoryRequests[0].Lower; !got.Equal(want) {
		t.Fatalf("first lower=%v want %v", got, want)
	}
}

func TestOlderHistoryOfAJoinedRoomIsNeverATurn(t *testing.T) {
	for _, dm := range []bool{false, true} {
		t.Run(map[bool]string{false: "room", true: "dm"}[dm], func(t *testing.T) {
			f := setup(t)
			f.addSpace("spaces/joined", false)
			if dm {
				f.google.Spaces["spaces/joined"].SpaceType = "DIRECT_MESSAGE"
			}
			joined := frozen.Add(-time.Minute)
			f.addMessage("spaces/joined", 1, joined.Add(-365*24*time.Hour))
			mention(f.addMessage("spaces/joined", 2, joined.Add(-6*time.Minute)))
			f.addMessage("spaces/joined", 3, joined.Add(-time.Minute))
			mention(f.addMessage("spaces/joined", 4, joined.Add(-2*time.Second)))
			f.addMessage("spaces/joined", 5, joined.Add(time.Second))
			join(t, f, joined)
			f.freeze(t)
			if err := f.r.enumerate(t.Context(), "spaces/joined"); err != nil {
				t.Fatal(err)
			}
			for _, raw := range f.opts.Ledger.Spaces["spaces/joined"].Backlog {
				var q queuedChange
				if err := json.Unmarshal([]byte(raw), &q); err != nil {
					t.Fatal(err)
				}
				if q.At.Before(joined.Add(-5 * time.Minute)) {
					t.Fatal("older history was queued")
				}
			}
			want := 2
			if dm {
				want = 3
			}
			f.pass(t, want, 0)
			historyLower(t, f, joined.Add(-5*time.Minute))
			if s := f.opts.Ledger.Spaces["spaces/joined"]; !s.JoinedAt.Equal(joined) {
				t.Fatal("join time changed")
			}
			if len(f.gc.Inbound()) != want {
				t.Fatalf("turns=%d", len(f.gc.Inbound()))
			}
		})
	}
}

func TestTheReachBackHappensOnce(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/joined", false)
	joined := frozen.Add(-time.Minute)
	mention(f.addMessage("spaces/joined", 1, joined.Add(-2*time.Second)))
	join(t, f, joined)
	f.freeze(t)
	lower := joined.Add(-5 * time.Minute)
	var saved string
	f.onRequest = func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/spaceEvents") {
			return errors.New("offline")
		}
		return nil
	}
	if _, _, err := f.r.Pass(t.Context()); err == nil {
		t.Fatal("expected interrupted read")
	}
	saved = f.opts.Ledger.Spaces["spaces/joined"].PageToken
	loaded, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	*f.opts.Ledger = *loaded
	f.newRunner(t)
	f.onRequest = nil
	if err := f.r.Freeze(t.Context(), frozen.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	p, err := decodeProgress(saved)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MessageLower.Equal(lower) {
		t.Fatalf("frozen lower=%v want %v", p.MessageLower, lower)
	}
	f.pass(t, 1, 0)
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("duplicate adding turn")
	}
	for i, req := range f.google.HistoryRequests {
		if i > 0 && req.Lower.Before(frozen.Add(-time.Minute)) {
			t.Fatalf("reopened reach-back: %v", req)
		}
	}
	if strings.Count(f.logs.String(), "gchat space first seen") != 1 || strings.Count(f.logs.String(), "gchat space first read complete") != 1 {
		t.Fatalf("first-read logs: %s", f.logs.String())
	}
}

func TestJoinEventOnABareRowSetsTheJoinTime(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/joined", false)
	f.opts.Ledger.Spaces["spaces/joined"] = &ledger.SpaceState{Wake: "mentions"}
	joined := frozen.Add(-time.Minute)
	join(t, f, joined)
	s := f.opts.Ledger.Spaces["spaces/joined"]
	if !s.JoinedAt.Equal(joined) || !s.MessageCursor.Equal(joined) || !s.EventCursor.Equal(joined) || s.Wake != "mentions" {
		t.Fatalf("bare row: %+v", s)
	}
}

func TestAKnownRoomGetsNoReachBack(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/joined", true)
	before := f.opts.Ledger.Spaces["spaces/joined"].MessageCursor
	join(t, f, frozen.Add(-time.Minute))
	f.freeze(t)
	f.pass(t, 0, 0)
	historyLower(t, f, before.Add(-time.Minute))
	if !f.opts.Ledger.Spaces["spaces/joined"].JoinedAt.IsZero() {
		t.Fatal("inferred join for known room")
	}
	if strings.Contains(f.logs.String(), "gchat space first seen") {
		t.Fatal("known room logged as new")
	}
}

func TestRoomFoundWithoutAnEventNeverReadsFromTheEpoch(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "bare"}[bare], func(t *testing.T) {
			f := setup(t)
			f.addSpace("spaces/joined", false)
			if bare {
				f.opts.Ledger.Spaces["spaces/joined"] = &ledger.SpaceState{Wake: "all"}
			}
			joined := frozen.Add(-time.Hour)
			f.google.Memberships["spaces/joined/members/123"] = &api.Membership{State: "JOINED", CreateTime: joined.Format(time.RFC3339Nano)}
			f.addMessage("spaces/joined", 1, joined.Add(-365*24*time.Hour))
			mention(f.addMessage("spaces/joined", 2, joined.Add(-2*time.Second)))
			f.addMessage("spaces/joined", 3, joined.Add(time.Second))
			f.freeze(t)
			f.pass(t, 2, 0)
			historyLower(t, f, joined.Add(-5*time.Minute))
			if len(f.google.MembershipRequests) != 1 {
				t.Fatalf("membership requests=%v", f.google.MembershipRequests)
			}
			if !f.opts.Ledger.Spaces["spaces/joined"].JoinedAt.Equal(joined) {
				t.Fatal("membership bound missing")
			}
			if err := f.r.Freeze(context.Background(), frozen.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			f.pass(t, 0, 0)
			if len(f.google.MembershipRequests) != 1 {
				t.Fatal("membership reread")
			}
		})
	}
}

func TestMembershipWithoutATimeTakesTheRecordedList(t *testing.T) {
	for _, code := range []int{0, 403, 404} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := setup(t)
			f.freeze(t)
			f.addSpace("spaces/joined", false)
			f.google.Memberships["spaces/joined/members/123"] = &api.Membership{State: "JOINED"}
			f.google.MembershipErrors["spaces/joined/members/123"] = code
			if err := f.r.Freeze(t.Context(), frozen.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if got := f.opts.Ledger.Spaces["spaces/joined"].JoinedAt; !got.Equal(frozen) {
				t.Fatalf("bound=%v want recorded list %v", got, frozen)
			}
		})
	}
}

func TestMembershipReadThatMayPassFailsTheFreeze(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/joined", false)
	f.onRequest = func(r *http.Request) error {
		if strings.Contains(r.URL.Path, "/members/") {
			return errors.New("offline")
		}
		return nil
	}
	if err := f.r.Freeze(t.Context(), frozen); err == nil {
		t.Fatal("membership failure accepted")
	}
	if f.opts.Ledger.Spaces["spaces/joined"] != nil {
		t.Fatal("row written on failure")
	}
	f.onRequest = nil
	f.freeze(t)
	if !f.opts.Ledger.Spaces["spaces/joined"].JoinedAt.Equal(frozen) {
		t.Fatal("retry did not use time found")
	}
}
