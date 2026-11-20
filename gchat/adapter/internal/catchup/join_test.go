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
	"gchat-adapter/internal/wire"
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

func TestFirstReadNeverReachesBelowTheInstallationFloor(t *testing.T) {
	f := setup(t)
	floor := frozen.Add(-2 * time.Minute)
	setHistoryFloor(t, f.opts.Ledger, floor)
	f.addSpace("spaces/joined", false)
	joined := floor.Add(time.Minute)
	mention(f.addMessage("spaces/joined", 1, joined.Add(-3*time.Minute)))
	adding := f.addMessage("spaces/joined", 2, joined.Add(-30*time.Second))
	mention(adding)
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
		if q.At.Before(floor) {
			t.Fatal("history from before the floor was queued")
		}
	}
	f.pass(t, 1, 0)
	historyLower(t, f, floor)
	if got := f.gc.Inbound(); len(got) != 1 || got[0].ProviderMessageID != adding.Name {
		t.Fatalf("expected only the adding mention, got %+v", got)
	}
}

func TestJoinedSpaceHistoryAfterMigration(t *testing.T) {
	const space = "spaces/joined"
	listed := frozen.Add(-3 * time.Hour)
	setupMigrated := func(t *testing.T, restart time.Time, dm bool) *fixture {
		t.Helper()
		f := setup(t)
		l := f.opts.Ledger
		// The released ledger saved last_space_list and had no spaces_listed_at.
		l.LastSpaceList, l.SpacesListedAt = listed, time.Time{}
		if err := l.Save(f.opts.LedgerPath); err != nil {
			t.Fatal(err)
		}
		loaded, err := ledger.Load(f.opts.LedgerPath, true)
		if err != nil {
			t.Fatal(err)
		}
		l.Replace(loaded)
		if err := InitializeHistory(l, f.opts.Config, restart, nil); err != nil {
			t.Fatal(err)
		}
		f.addSpace(space, false)
		if dm {
			f.google.Spaces[space].SpaceType = "DIRECT_MESSAGE"
		}
		return f
	}
	live := func(t *testing.T, f *fixture, m *api.Message) {
		t.Helper()
		c := puller.Change{Kind: "message.created", Space: space, Message: m}
		if out, err := f.p.Handle(t.Context(), &puller.Event{Changes: []puller.Change{c}}); err != nil || out != puller.Ack {
			t.Fatalf("live message: %v %v", out, err)
		}
	}
	checkTurns := func(t *testing.T, f *fixture, want *api.Message) {
		t.Helper()
		f.freeze(t)
		if _, remaining, err := f.r.Pass(t.Context()); err != nil || remaining != 0 {
			t.Fatalf("first read: remaining=%d err=%v", remaining, err)
		}
		var names []string
		for _, turn := range f.gc.Inbound() {
			names = append(names, turn.ProviderMessageID)
		}
		if len(names) != 1 || names[0] != want.Name {
			t.Fatalf("turns = %v, want only %s", names, want.Name)
		}
	}
	t.Run("offline-dm", func(t *testing.T) {
		f := setupMigrated(t, frozen, true)
		opened := listed.Add(time.Hour)
		member := &api.Membership{Name: space + "/members/123", State: "JOINED", Member: &api.User{Name: "users/123"}, CreateTime: opened.Format(time.RFC3339Nano)}
		f.google.Memberships[member.Name] = member
		checkTurns(t, f, f.addMessage(space, 1, opened.Add(time.Minute)))
	})
	t.Run("live-room-before-join", func(t *testing.T) {
		f := setupMigrated(t, listed, false)
		joined := listed.Add(45 * time.Minute)
		f.addMessage(space, 1, joined.Add(-30*time.Minute))
		current := f.addMessage(space, 2, joined.Add(time.Minute))
		live(t, f, current)
		join(t, f, joined)
		checkTurns(t, f, current)
	})
	t.Run("live-dm-after-join", func(t *testing.T) {
		f := setupMigrated(t, listed, true)
		joined := listed.Add(time.Minute)
		f.opts.Ledger.HistoryFloor = listed
		f.addMessage(space, 1, listed.Add(-time.Second))
		first := f.addMessage(space, 2, joined.Add(-2*time.Second))
		join(t, f, joined)
		live(t, f, first)
		checkTurns(t, f, first)
		historyLower(t, f, listed)
	})
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
	// No list has been recorded: the fallback is the time found.
	f.opts.Ledger.SpacesListedAt = time.Time{}
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

func TestJoinReachbackCanBeDisabled(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.JoinReachback = "0s"
	f.addSpace("spaces/joined", false)
	joined := frozen.Add(-time.Minute)
	mention(f.addMessage("spaces/joined", 1, joined.Add(-2*time.Second)))
	f.addMessage("spaces/joined", 2, joined.Add(time.Second))
	join(t, f, joined)
	f.freeze(t)
	f.pass(t, 1, 0)
	historyLower(t, f, joined)
}

func TestMembershipBoundNeverPrecedesRecordedList(t *testing.T) {
	for _, state := range []string{"JOINED", "INVITED"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t)
			f.freeze(t)
			f.addSpace("spaces/joined", false)
			f.google.Memberships["spaces/joined/members/123"] = &api.Membership{State: state, CreateTime: frozen.Add(-time.Hour).Format(time.RFC3339Nano)}
			if err := f.r.Freeze(t.Context(), frozen.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if !f.opts.Ledger.Spaces["spaces/joined"].JoinedAt.Equal(frozen) {
				t.Fatal("membership bypassed list bound")
			}
		})
	}
}

func TestTemporaryMembershipErrorsLeaveFreezeAtomic(t *testing.T) {
	for _, code := range []int{429, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			f := setup(t)
			f.addSpace("spaces/joined", false)
			f.addSpace("spaces/other", false)
			f.google.MembershipErrors["spaces/other/members/123"] = code
			if err := f.r.Freeze(t.Context(), frozen); err == nil {
				t.Fatal("transient failure accepted")
			}
			if len(f.opts.Ledger.Spaces) != 0 || !f.opts.Ledger.LastSpaceList.IsZero() || f.logs.Len() != 0 {
				t.Fatal("failed Freeze published state")
			}
			delete(f.google.MembershipErrors, "spaces/other/members/123")
			f.freeze(t)
			if len(f.opts.Ledger.Spaces) != 2 {
				t.Fatal("retry lost discovery")
			}
		})
	}
}

func TestLegacyOpenReadKeepsItsBounds(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/joined", false)
	p := progress{Version: 1, Phase: "messages", MessageLower: time.Unix(0, 0).UTC(), EventLower: frozen.Add(-24 * time.Hour)}
	f.opts.Ledger.Spaces["spaces/joined"] = &ledger.SpaceState{FrozenUpper: frozen, PageToken: encode(p)}
	f.freeze(t)
	s := f.opts.Ledger.Spaces["spaces/joined"]
	rebound, err := decodeProgress(s.PageToken)
	if err != nil || s.HistoryStart.IsZero() || !rebound.MessageLower.Equal(s.HistoryStart) || !s.JoinedAt.IsZero() || s.FirstRead != nil || len(f.google.MembershipRequests) != 0 {
		t.Fatalf("legacy read changed: %+v", s)
	}
}

func TestFirstReadLogsCountConcludedPendingTurn(t *testing.T) {
	for _, status := range []string{"delivered", "pending"} {
		t.Run(status, func(t *testing.T) { testFirstReadLogsCountConcludedPendingTurn(t, status) })
	}
}

func testFirstReadLogsCountConcludedPendingTurn(t *testing.T, status string) {
	f := setup(t)
	f.addSpace("spaces/joined", false)
	joined := frozen.Add(-time.Minute)
	m := f.addMessage("spaces/joined", 1, joined.Add(-2*time.Second))
	mention(m)
	join(t, f, joined)
	f.gc.NextDelivery = "pending"
	f.freeze(t)
	f.pass(t, 0, 1)
	if strings.Contains(f.logs.String(), "gchat space first read complete") {
		t.Fatal("read logged complete while pending")
	}
	id := f.opts.Ledger.Changes[m.Name].ReceiptID
	receipt := wire.ReceiptResponse{State: "concluded"}
	receipt.Delivery.Status = status
	if status == "pending" {
		receipt.Delivery.Members = []wire.DeliveryMember{{Status: "pending", DeliveredBytes: 120, ExpectedBytes: 120}}
	}
	f.gc.SetReceipt(id, receipt)
	if err := f.p.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.newRunner(t)
	f.pass(t, 0, 0)
	logs := f.logs.String()
	if strings.Count(logs, "gchat space first seen") != 1 || strings.Count(logs, "gchat space first read complete") != 1 || !strings.Contains(logs, "turns=1 pre_join_turns=1") || !strings.Contains(logs, "found_by=event") {
		t.Fatalf("logs: %s", logs)
	}
	if strings.Contains(logs, m.Text) || strings.Contains(logs, "users/") {
		t.Fatal("content or user ID in first-read log")
	}
}

func TestDiscoveryLogsOnceForEachExistingTrigger(t *testing.T) {
	for _, source := range []string{"startup", "sweep", "recovery"} {
		t.Run(source, func(t *testing.T) {
			f := setup(t)
			f.addSpace("spaces/joined", false)
			if err := f.r.FreezeFrom(t.Context(), frozen, source); err != nil {
				t.Fatal(err)
			}
			f.pass(t, 0, 0)
			f.freeze(t)
			f.pass(t, 0, 0)
			logs := f.logs.String()
			if strings.Count(logs, "gchat space first seen") != 1 || strings.Count(logs, "gchat space first read complete") != 1 || !strings.Contains(logs, "found_by="+source) {
				t.Fatalf("logs: %s", logs)
			}
		})
	}
}

func TestFirstReadCountSurvivesSaveFailureAfterDelivery(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/joined", false)
	joined := frozen.Add(-time.Minute)
	m := f.addMessage("spaces/joined", 1, joined.Add(-2*time.Second))
	mention(m)
	join(t, f, joined)
	f.freeze(t)
	f.r.opts.Save = func(l *ledger.Ledger) error {
		if l.Changes[m.Name] != nil && len(l.Spaces["spaces/joined"].Backlog) == 0 {
			return errors.New("scripted disk failure")
		}
		return l.Save(f.opts.LedgerPath)
	}
	if _, _, err := f.r.Pass(t.Context()); err == nil {
		t.Fatal("expected post-delivery save failure")
	}
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("fixture did not deliver")
	}
	f.newRunner(t)
	f.pass(t, 0, 0)
	if len(f.gc.Inbound()) != 1 || !strings.Contains(f.logs.String(), "turns=1 pre_join_turns=1") {
		t.Fatalf("recovered completion: %s", f.logs.String())
	}
}
