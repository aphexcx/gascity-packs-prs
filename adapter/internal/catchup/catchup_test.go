package catchup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/googleapi"
)

var frozen = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	r         *Runner
	opts      Options
	p         *inbound.Processor
	google    *fakegoogle.Server
	gc        *fakegc.Server
	logs      bytes.Buffer
	onRequest func(*http.Request) error
	requests  []string
	pageSize  int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{google: fakegoogle.New(t, nil), gc: fakegc.New(t), pageSize: 17}
	f.google.Now = func() time.Time { return frozen }
	dir := t.TempDir()
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity"}, Endpoints: config.Endpoints{ChatAPIBase: f.google.URL(), StateFile: filepath.Join(dir, "state.json")}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
	h := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		f.requests = append(f.requests, r.URL.Path+"?"+r.URL.RawQuery)
		if f.onRequest != nil {
			if err := f.onRequest(r); err != nil {
				return nil, err
			}
		}
		// A service may return fewer than pageSize items. Exercise real HTTP
		// pagination using the shared fake with a small server-side page size.
		copy := r.Clone(r.Context())
		q := copy.URL.Query()
		if q.Has("pageSize") {
			q.Set("pageSize", fmt.Sprint(f.pageSize))
			copy.URL.RawQuery = q.Encode()
		}
		return http.DefaultTransport.RoundTrip(copy)
	})}
	c, err := chat.New(context.Background(), h, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	l := ledger.New()
	mu := &sync.Mutex{}
	logger := slog.New(slog.NewTextHandler(&f.logs, nil))
	f.p, err = inbound.New(inbound.Options{Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: cfg.Endpoints.StateFile, GC: gcclient.New(f.gc.URL(), "examplecity"), Chat: c, IdentityUserID: "users/123", Logger: logger, Now: func() time.Time { return frozen }, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	f.opts = Options{Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: cfg.Endpoints.StateFile, Chat: c, Processor: f.p, Logger: logger, Now: func() time.Time { return frozen }}
	f.newRunner(t)
	return f
}

func (f *fixture) newRunner(t *testing.T) {
	t.Helper()
	var err error
	f.r, err = New(f.opts)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) addSpace(name string, known bool) {
	f.google.Spaces[name] = &api.Space{Name: name, SpaceType: "SPACE"}
	if known {
		f.opts.Ledger.Spaces[name] = &ledger.SpaceState{Kind: "room", Wake: "all", MessageCursor: frozen.Add(-time.Hour), EventCursor: frozen.Add(-time.Hour)}
	}
}

func (f *fixture) addMessage(space string, n int, at time.Time) *api.Message {
	m := &api.Message{Name: fmt.Sprintf("%s/messages/m%04d", space, n), Text: fmt.Sprintf("message %d", n), CreateTime: at.Format(time.RFC3339Nano), Sender: &api.User{Name: "users/456", Type: "HUMAN"}, Thread: &api.Thread{Name: space + "/threads/topic"}, Space: f.google.Spaces[space]}
	f.google.Messages[m.Name] = m
	return m
}

func (f *fixture) freeze(t *testing.T) {
	t.Helper()
	if err := f.r.Freeze(context.Background(), frozen); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) pass(t *testing.T, wantDelivered, wantRemaining int) {
	t.Helper()
	d, n, err := f.r.Pass(context.Background())
	if err != nil || d != wantDelivered || n != wantRemaining {
		t.Fatalf("Pass = (%d, %d, %v), want (%d, %d, nil)", d, n, err, wantDelivered, wantRemaining)
	}
}

func TestOutageDrainsAcrossThreePasses(t *testing.T) {
	f := setup(t)
	for i, count := range []int{110, 10} {
		space := fmt.Sprintf("spaces/room%d", i)
		f.addSpace(space, true)
		for j := 0; j < count; j++ {
			f.addMessage(space, j, frozen.Add(-time.Hour+time.Duration(j+1)*time.Second))
		}
	}
	f.freeze(t)
	f.pass(t, 60, 60)
	if f.opts.Ledger.Spaces["spaces/room0"].MessageCursor.Equal(frozen) {
		t.Fatal("advanced over backlog")
	}
	f.pass(t, 50, 10)
	f.pass(t, 10, 0)
	f.pass(t, 0, 0)
	for space, state := range f.opts.Ledger.Spaces {
		if !state.MessageCursor.Equal(frozen) || !state.EventCursor.Equal(frozen) || len(state.Backlog) != 0 {
			t.Fatalf("%s incomplete: %+v", space, state)
		}
	}
	seen := map[string]bool{}
	last := map[string]string{}
	for _, m := range f.gc.Inbound() {
		if seen[m.ProviderMessageID] || !strings.HasPrefix(m.Text, "[catch-up]\n") {
			t.Fatalf("duplicate or unmarked turn: %+v", m)
		}
		if m.ProviderMessageID < last[m.Conversation.ConversationID] {
			t.Fatal("not oldest first")
		}
		seen[m.ProviderMessageID] = true
		last[m.Conversation.ConversationID] = m.ProviderMessageID
	}
	if len(seen) != 120 {
		t.Fatalf("turns = %d", len(seen))
	}
}

func TestLiveDeliveryDoesNotMoveFrozenCursor(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	f.addMessage("spaces/room", 1, frozen.Add(-time.Minute))
	f.freeze(t)
	live := &api.Message{Name: "spaces/room/messages/live", Text: "live", CreateTime: frozen.Add(time.Minute).Format(time.RFC3339Nano), Space: f.google.Spaces["spaces/room"], Sender: &api.User{Name: "users/456"}}
	f.onRequest = func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			f.onRequest = nil
			out, err := f.p.Handle(r.Context(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Message: live, Space: "spaces/room"}}})
			if err != nil || out != puller.Ack {
				t.Fatalf("live: %v %v", out, err)
			}
			s := f.opts.Ledger.Spaces["spaces/room"]
			if !s.MessageCursor.Equal(frozen.Add(-time.Hour)) || !s.FrozenUpper.Equal(frozen) {
				t.Fatal("live moved frozen interval")
			}
		}
		return nil
	}
	f.pass(t, 1, 0)
	if !f.opts.Ledger.Spaces["spaces/room"].MessageCursor.Equal(frozen) || len(f.gc.Inbound()) != 2 {
		t.Fatal("wrong cursor or delivery count")
	}
}

func TestEqualCreateTimesAcrossPageBoundary(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	for i := 0; i < 35; i++ {
		f.addMessage("spaces/room", i, frozen.Add(-time.Minute))
	}
	f.freeze(t)
	f.pass(t, 35, 0)
	if len(f.gc.Inbound()) != 35 {
		t.Fatal("equal timestamp messages skipped")
	}
}

func TestRestartResumesPersistedPageToken(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	for i := 0; i < 35; i++ {
		f.addMessage("spaces/room", i, frozen.Add(-time.Minute))
	}
	f.freeze(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onRequest = func(r *http.Request) error {
		if r.URL.Query().Get("pageToken") == "17" {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if _, _, err := f.r.Pass(ctx); err == nil {
		t.Fatal("missing interrupted-page error")
	}
	saved, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	state := saved.Spaces["spaces/room"]
	if len(state.Backlog) != 17 || !strings.Contains(state.PageToken, "17") || !state.MessageCursor.Equal(frozen.Add(-time.Hour)) {
		t.Fatalf("lost page: %+v", state)
	}
	*f.opts.Ledger = *saved
	f.onRequest = nil
	f.requests = nil
	f.newRunner(t)
	if err := f.r.Freeze(context.Background(), frozen.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !f.opts.Ledger.Spaces["spaces/room"].FrozenUpper.Equal(frozen) {
		t.Fatal("restart replaced unfinished interval")
	}
	f.pass(t, 35, 0)
	for _, request := range f.requests {
		if strings.Contains(request, "/messages?") {
			if !strings.Contains(request, "pageToken=17") {
				t.Fatalf("restarted at wrong page: %s", request)
			}
			break
		}
	}
}

func TestNewSpaceUsesDefaultWake(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.Defaults.Wake = config.WakeMentions
	f.addSpace("spaces/joined", false)
	m := f.addMessage("spaces/joined", 1, frozen.Add(-time.Minute))
	m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: "users/123"}}}}
	f.freeze(t)
	s := f.opts.Ledger.Spaces["spaces/joined"]
	if s == nil || s.Wake != "mentions" || !s.MessageCursor.IsZero() || !s.EventCursor.IsZero() {
		t.Fatalf("discovered state: %+v", s)
	}
	f.pass(t, 1, 0)
}

func TestOldMessageEditRecoveredFromSpaceEvents(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	m := f.addMessage("spaces/room", 1, frozen.Add(-2*time.Hour))
	out, err := f.p.Handle(context.Background(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: m}}})
	if err != nil || out != puller.Ack {
		t.Fatal(out, err)
	}
	m.Text, m.LastUpdateTime = "changed", frozen.Add(-time.Minute).Format(time.RFC3339Nano)
	f.google.SpaceEvents["spaces/room"] = []*api.SpaceEvent{{Name: "spaces/room/spaceEvents/edit", EventType: "google.workspace.chat.message.v1.updated", EventTime: m.LastUpdateTime, MessageUpdatedEventData: &api.MessageUpdatedEventData{Message: m}}}
	f.freeze(t)
	f.pass(t, 1, 0)
	turns := f.gc.Inbound()
	if len(turns) != 2 || turns[1].ProviderMessageID != m.Name+"#updated#"+m.LastUpdateTime || !strings.Contains(turns[1].Text, "[catch-up]\nedited: changed") {
		t.Fatalf("edit missing: %+v", turns)
	}
}

func TestFortyDayGapReportedUnrecoverable(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	s := f.opts.Ledger.Spaces["spaces/room"]
	s.MessageCursor, s.EventCursor = frozen.Add(-40*24*time.Hour), frozen.Add(-40*24*time.Hour)
	f.addMessage("spaces/room", 1, frozen.Add(-35*24*time.Hour))
	f.freeze(t)
	f.pass(t, 1, 0)
	if f.opts.Ledger.Counters["unrecoverable:spaces/room"] != 1 || !strings.Contains(f.logs.String(), "unrecoverable") {
		t.Fatalf("missing gap report: %v %s", f.opts.Ledger.Counters, f.logs.String())
	}
}

func TestPendingReceiptHoldsCursorUntilPoll(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown=%v", unknown), func(t *testing.T) {
			f := setup(t)
			f.addSpace("spaces/room", true)
			m := f.addMessage("spaces/room", 1, frozen.Add(-time.Minute))
			f.gc.NextDelivery = "pending"
			f.freeze(t)
			f.pass(t, 0, 1)
			f.pass(t, 0, 1)
			if len(f.gc.Inbound()) != 1 || f.opts.Ledger.Spaces["spaces/room"].MessageCursor.Equal(frozen) {
				t.Fatal("pending reposted or cursor advanced")
			}
			id := f.opts.Ledger.Changes[m.Name].ReceiptID
			if unknown {
				f.gc.EvictReceipt(id)
			} else {
				receipt := wire.ReceiptResponse{State: "concluded"}
				receipt.Delivery.Status = "delivered"
				f.gc.SetReceipt(id, receipt)
			}
			if err := f.p.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.pass(t, 0, 0)
			if !f.opts.Ledger.Spaces["spaces/room"].MessageCursor.Equal(frozen) {
				t.Fatal("terminal cursor stuck")
			}
		})
	}
}

func TestFailedSaveCannotSkipAPage(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	for i := 0; i < 20; i++ {
		f.addMessage("spaces/room", i, frozen.Add(-time.Minute))
	}
	f.freeze(t)
	f.opts.Save = func(l *ledger.Ledger) error {
		if len(l.Spaces["spaces/room"].Backlog) > 17 {
			return errors.New("disk full")
		}
		return l.Save(f.opts.LedgerPath)
	}
	f.newRunner(t)
	if _, _, err := f.r.Pass(context.Background()); err == nil {
		t.Fatal("save error swallowed")
	}
	s := f.opts.Ledger.Spaces["spaces/room"]
	// The first, durably saved page may drain even though the next save failed.
	// None of the failed page may be delivered or reflected in the page token.
	if len(s.Backlog) != 0 || len(f.gc.Inbound()) != 17 || !strings.Contains(s.PageToken, "17") || !s.MessageCursor.Equal(frozen.Add(-time.Hour)) {
		t.Fatalf("failed page published in memory: %+v", s)
	}
	f.opts.Save = nil
	f.newRunner(t)
	f.pass(t, 3, 0)
	if len(f.gc.Inbound()) != 20 {
		t.Fatal("lost or duplicated a page after failed save")
	}
}

func TestOverlapAndLocalChangesDoNotConsumeDeliveryBudget(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.CatchupBudget = 1
	f.newRunner(t)
	f.addSpace("spaces/room", true)
	for i := 0; i < 3; i++ {
		f.addMessage("spaces/room", i, frozen.Add(-time.Hour-30*time.Second)).Sender.Name = "users/123"
	}
	f.addMessage("spaces/room", 10, frozen.Add(-time.Hour-10*time.Second))
	f.addMessage("spaces/room", 11, frozen.Add(-time.Minute))
	f.freeze(t)
	f.pass(t, 1, 1)
	f.pass(t, 1, 0)
}

func TestRestartAlsoRecoversThroughTheNewFreezeTime(t *testing.T) {
	f := setup(t)
	f.opts.Config.Settings.CatchupBudget = 1
	f.newRunner(t)
	f.addSpace("spaces/room", true)
	f.addMessage("spaces/room", 1, frozen.Add(-time.Minute))
	f.addMessage("spaces/room", 2, frozen.Add(time.Minute))
	f.freeze(t)
	next := frozen.Add(time.Hour)
	f.google.Now = func() time.Time { return next }
	if err := f.r.Freeze(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	f.pass(t, 1, 1)
	f.pass(t, 1, 0)
	if !f.opts.Ledger.Spaces["spaces/room"].MessageCursor.Equal(next) || len(f.gc.Inbound()) != 2 {
		t.Fatal("restart gap was not recovered")
	}
}

func TestEventPagesResumeAndExpandBatches(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	for i := 0; i < 19; i++ {
		m := f.addMessage("spaces/room", i, frozen.Add(-2*time.Hour))
		out, err := f.p.Handle(context.Background(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: m}}})
		if out != puller.Ack || err != nil {
			t.Fatal(out, err)
		}
		if i < 17 {
			m.LastUpdateTime = frozen.Add(-time.Minute).Format(time.RFC3339Nano)
			f.google.SpaceEvents["spaces/room"] = append(f.google.SpaceEvents["spaces/room"], &api.SpaceEvent{Name: fmt.Sprintf("spaces/room/spaceEvents/e%02d", i), EventTime: m.LastUpdateTime, EventType: "google.workspace.chat.message.v1.updated", MessageUpdatedEventData: &api.MessageUpdatedEventData{Message: m}})
		}
	}
	f.google.SpaceEvents["spaces/room"] = append(f.google.SpaceEvents["spaces/room"], &api.SpaceEvent{Name: "spaces/room/spaceEvents/z", EventTime: frozen.Add(-30 * time.Second).Format(time.RFC3339Nano), EventType: "google.workspace.chat.message.v1.batchDeleted", MessageBatchDeletedEventData: &api.MessageBatchDeletedEventData{Messages: []*api.MessageDeletedEventData{{Message: &api.Message{Name: "spaces/room/messages/m0017"}}, {Message: &api.Message{Name: "spaces/room/messages/m0018"}}}}})
	f.freeze(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.onRequest = func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/spaceEvents") && r.URL.Query().Get("pageToken") == "17" {
			cancel()
			return context.Canceled
		}
		return nil
	}
	if _, _, err := f.r.Pass(ctx); err == nil {
		t.Fatal("expected interrupted event enumeration")
	}
	saved, err := ledger.Load(f.opts.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	*f.opts.Ledger = *saved
	f.onRequest, f.requests = nil, nil
	f.newRunner(t)
	f.pass(t, 19, 0)
	if len(f.requests) != 1 || !strings.Contains(f.requests[0], "/spaceEvents?") || !strings.Contains(f.requests[0], "pageToken=17") {
		t.Fatalf("event restart requests: %v", f.requests)
	}
	if !f.opts.Ledger.Messages["spaces/room/messages/m0017"].Deleted || !f.opts.Ledger.Messages["spaces/room/messages/m0018"].Deleted {
		t.Fatal("batch delete missing")
	}
}

func TestCatchupReactionsAndMalformedHistory(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	m := f.addMessage("spaces/room", 1, frozen.Add(-2*time.Hour))
	m.Sender.Name = "users/123"
	reaction := &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/456"}, Emoji: &api.Emoji{Unicode: "👍"}}
	f.google.SpaceEvents["spaces/room"] = []*api.SpaceEvent{
		{Name: "spaces/room/spaceEvents/r1", EventTime: frozen.Add(-time.Minute).Format(time.RFC3339Nano), EventType: "google.workspace.chat.reaction.v1.created", ReactionCreatedEventData: &api.ReactionCreatedEventData{Reaction: reaction}},
		{Name: "spaces/room/spaceEvents/r2", EventTime: frozen.Add(-30 * time.Second).Format(time.RFC3339Nano), EventType: "google.workspace.chat.reaction.v1.deleted", ReactionDeletedEventData: &api.ReactionDeletedEventData{Reaction: &api.Reaction{Name: reaction.Name}}},
		{Name: "spaces/room/spaceEvents/bad", EventTime: frozen.Add(-20 * time.Second).Format(time.RFC3339Nano), EventType: "google.workspace.chat.message.v1.updated"},
	}
	f.freeze(t)
	f.pass(t, 2, 0)
	turns := f.gc.Inbound()
	if len(turns) != 2 || turns[0].ProviderMessageID != reaction.Name+"#created" || turns[1].ProviderMessageID != reaction.Name+"#deleted" {
		t.Fatalf("reaction turns: %+v", turns)
	}
	if len(f.opts.Ledger.Unresolved) != 1 {
		t.Fatal("malformed history was not retained")
	}
}

func TestUnavailableHistoryReportedWithoutMovingCursor(t *testing.T) {
	f := setup(t)
	f.opts.Ledger.Spaces["spaces/gone"] = &ledger.SpaceState{Kind: "room", Wake: "all", MessageCursor: frozen.Add(-time.Hour), EventCursor: frozen.Add(-time.Hour)}
	f.freeze(t)
	// The shared fake lists an absent space as empty; inject the client's
	// classified error to exercise a genuinely inaccessible history interval.
	f.onRequest = func(r *http.Request) error { return &googleapi.Error{Code: 404, Message: "space unavailable"} }
	for i := 0; i < 2; i++ {
		if _, _, err := f.r.Pass(context.Background()); err == nil {
			t.Fatal("unavailable history succeeded")
		}
	}
	if f.opts.Ledger.Counters["unrecoverable:spaces/gone"] != 1 || !f.opts.Ledger.Spaces["spaces/gone"].MessageCursor.Equal(frozen.Add(-time.Hour)) {
		t.Fatal("history loss not reported or cursor advanced")
	}
}

func TestHistoryErrorDoesNotStarveRetainedMessages(t *testing.T) {
	f := setup(t)
	f.addSpace("spaces/room", true)
	f.addMessage("spaces/room", 1, frozen.Add(-time.Minute))
	f.freeze(t)
	f.onRequest = func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/spaceEvents") {
			return &googleapi.Error{Code: 404, Message: "event history unavailable"}
		}
		return nil
	}
	for i, want := range []int{1, 0} {
		delivered, remaining, err := f.r.Pass(context.Background())
		if err == nil || delivered != want || remaining != 0 {
			t.Fatalf("pass %d: (%d, %d, %v), want (%d, 0, error)", i, delivered, remaining, err, want)
		}
		if !f.opts.Ledger.Spaces["spaces/room"].MessageCursor.Equal(frozen.Add(-time.Hour)) {
			t.Fatal("cursor advanced over unenumerated events")
		}
	}
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("saved message not delivered exactly once")
	}
	f.onRequest = nil
	f.pass(t, 0, 0)
	if !f.opts.Ledger.Spaces["spaces/room"].MessageCursor.Equal(frozen) {
		t.Fatal("cursor did not recover after successful enumeration")
	}
}

func TestPartialHistoryDrainsRetainedRecordsChronologically(t *testing.T) {
	f := setup(t)
	f.pageSize = 1
	f.addSpace("spaces/room", true)
	old := f.addMessage("spaces/room", 1, frozen.Add(-2*time.Hour))
	if out, err := f.p.Handle(context.Background(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: old}}}); out != puller.Ack || err != nil {
		t.Fatal(out, err)
	}
	old.LastUpdateTime = frozen.Add(-20 * time.Minute).Format(time.RFC3339Nano)
	newer := f.addMessage("spaces/room", 2, frozen.Add(-10*time.Minute))
	f.google.SpaceEvents["spaces/room"] = []*api.SpaceEvent{
		{Name: "spaces/room/spaceEvents/edit", EventTime: old.LastUpdateTime, EventType: "google.workspace.chat.message.v1.updated", MessageUpdatedEventData: &api.MessageUpdatedEventData{Message: old}},
		{Name: "spaces/room/spaceEvents/unavailable", EventTime: frozen.Add(-time.Minute).Format(time.RFC3339Nano), EventType: "google.workspace.chat.message.v1.updated"},
	}
	f.freeze(t)
	f.onRequest = func(r *http.Request) error {
		if strings.HasSuffix(r.URL.Path, "/spaceEvents") && r.URL.Query().Get("pageToken") == "1" {
			return &googleapi.Error{Code: 404, Message: "history unavailable"}
		}
		return nil
	}
	if d, n, err := f.r.Pass(context.Background()); err == nil || d != 2 || n != 0 {
		t.Fatal(d, n, err)
	}
	turns := f.gc.Inbound()
	if len(turns) != 3 || turns[1].ProviderMessageID != old.Name+"#updated#"+old.LastUpdateTime || turns[2].ProviderMessageID != newer.Name {
		t.Fatalf("retained history out of order: %+v", turns)
	}
}
