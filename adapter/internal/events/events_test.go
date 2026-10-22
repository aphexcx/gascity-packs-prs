package events

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"google.golang.org/api/option"
	api "google.golang.org/api/workspaceevents/v1"
)

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	sleeps  []time.Duration
	onSleep func(time.Duration) error
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }
func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	c.mu.Unlock()
	if c.onSleep != nil {
		if err := c.onSleep(d); err != nil {
			return err
		}
	}
	c.advance(d)
	return ctx.Err()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	t        *testing.T
	server   *fakegoogle.Server
	clock    *fakeClock
	ledger   *ledger.Ledger
	ledgerMu sync.Mutex
	path     string
	manager  *Manager
	requests []*http.Request
	bodies   []map[string]any
	hook     func(*http.Request) (*http.Response, error)
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, clock: &fakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, ledger: ledger.New(), path: filepath.Join(t.TempDir(), "state.json")}
	f.server = fakegoogle.New(t, nil)
	f.server.Now = f.clock.Now
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.requests = append(f.requests, r.Clone(r.Context()))
		var body map[string]any
		if r.GetBody != nil {
			b, err := r.GetBody()
			if err != nil {
				return nil, err
			}
			defer b.Close()
			_ = json.NewDecoder(b).Decode(&body)
		}
		f.bodies = append(f.bodies, body)
		if f.hook != nil {
			return f.hook(r)
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	service, err := api.NewService(context.Background(), option.WithHTTPClient(client), option.WithEndpoint(f.server.URL()+"/"))
	if err != nil {
		t.Fatal(err)
	}
	f.manager, err = New(Options{Service: service, Topic: "projects/example/topics/events", Ledger: f.ledger, LedgerPath: f.path, LedgerMu: &f.ledgerMu, Clock: f.clock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) reconcile() ledger.SubscriptionState {
	f.t.Helper()
	s, err := f.manager.Reconcile(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	disk, err := ledger.Load(f.path, true)
	if err != nil {
		f.t.Fatal(err)
	}
	if disk.Subscription != s || f.ledger.Subscription != s {
		f.t.Fatalf("returned=%+v disk=%+v memory=%+v", s, disk.Subscription, f.ledger.Subscription)
	}
	return s
}

func (f *fixture) count(method, path string) int {
	n := 0
	for _, r := range f.requests {
		if r.Method == method && r.URL.Path == path {
			n++
		}
	}
	return n
}

func TestFreshCreatePersistsGoogleExpiryAndRequestContract(t *testing.T) {
	f := setup(t)
	wantExpiry := f.clock.Now().Add(4 * time.Hour)
	s := f.reconcile()
	if s.Name == "" || s.State != "ACTIVE" || s.Target != "//chat.googleapis.com/spaces/-" || !s.Expiry.Equal(wantExpiry) {
		t.Fatalf("subscription = %+v", s)
	}
	if f.requests[0].Method != "GET" || !strings.Contains(f.requests[0].URL.Query().Get("filter"), "event_types:") {
		t.Fatal("must discover by target and event types before creating")
	}
	wantTypes := []string{"google.workspace.chat.message.v1.created", "google.workspace.chat.message.v1.updated", "google.workspace.chat.message.v1.deleted", "google.workspace.chat.reaction.v1.created", "google.workspace.chat.reaction.v1.deleted", "google.workspace.chat.membership.v1.created", "google.workspace.chat.membership.v1.updated", "google.workspace.chat.membership.v1.deleted"}
	f.server.Configure(func(server *fakegoogle.Server) {
		sub := server.Subscriptions[s.Name]
		if !reflect.DeepEqual(sub.EventTypes, wantTypes) || sub.PayloadOptions == nil || !sub.PayloadOptions.IncludeResource || sub.NotificationEndpoint.PubsubTopic != "projects/example/topics/events" {
			t.Errorf("create = %+v", sub)
		}
	})
	for i, r := range f.requests {
		if r.Method == "POST" {
			if _, ok := f.bodies[i]["ttl"]; ok {
				t.Error("create must omit ttl")
			}
		}
	}
}

func TestRenewBeforeExpiry(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	f.clock.advance(3*time.Hour - time.Nanosecond)
	f.reconcile()
	if f.count("PATCH", "/v1/"+first.Name) != 0 {
		t.Fatal("renewed early")
	}
	f.clock.advance(time.Nanosecond)
	got := f.reconcile()
	if got.Name != first.Name || !got.Expiry.Equal(f.clock.Now().Add(4*time.Hour)) {
		t.Fatalf("renewal=%+v", got)
	}
	if f.count("POST", "/v1/subscriptions") != 1 || f.count("PATCH", "/v1/"+first.Name) != 1 {
		t.Fatal("expected one create and one renewal")
	}
	for i, r := range f.requests {
		if r.Method == "PATCH" && (r.URL.Query().Get("updateMask") != "ttl" || f.bodies[i]["ttl"] != "0s") {
			t.Fatalf("patch query=%v body=%v", r.URL.Query(), f.bodies[i])
		}
	}
}

func TestLostCreateResponseThenDiscovery(t *testing.T) {
	f := setup(t)
	f.server.DropNextResponse = true
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("lost response must be reported")
	}
	if f.ledger.Subscription.Name != "" {
		t.Fatal("unconfirmed create was persisted")
	}
	got := f.reconcile()
	if got.Name == "" || f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("lost create was repeated instead of discovered")
	}
}

func TestAlreadyExistsAdoptsConcurrentCreate(t *testing.T) {
	f := setup(t)
	f.hook = func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && r.URL.Path == "/v1/subscriptions" {
			f.server.Configure(func(s *fakegoogle.Server) {
				s.Subscriptions["subscriptions/concurrent"] = &api.Subscription{Name: "subscriptions/concurrent", State: "ACTIVE", TargetResource: Target, EventTypes: append([]string(nil), EventTypes...), ExpireTime: f.clock.Now().Add(4 * time.Hour).Format(time.RFC3339Nano), PayloadOptions: &api.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &api.NotificationEndpoint{PubsubTopic: "projects/example/topics/events"}}
			})
		}
		return http.DefaultTransport.RoundTrip(r)
	}
	got := f.reconcile()
	if got.Name != "subscriptions/concurrent" || f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatalf("adoption=%+v", got)
	}
}

func TestSuspendedReactivatedWithoutEvent(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	f.server.SuspendSubscription(first.Name)
	got := f.reconcile()
	if got.Name != first.Name || got.State != "ACTIVE" || got.Expiry != first.Expiry {
		t.Fatalf("reactivation=%+v", got)
	}
	if f.count("POST", "/v1/"+first.Name+":reactivate") != 1 || f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("must reactivate existing subscription")
	}
}

func TestRenewLoopAtThreeQuartersTTL(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	start := f.clock.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.hook = func(r *http.Request) (*http.Response, error) {
		if r.Method == "PATCH" {
			if !f.clock.Now().Equal(start.Add(3 * time.Hour)) {
				t.Errorf("renewed at %v", f.clock.Now())
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	}
	f.clock.onSleep = func(d time.Duration) error {
		if f.count("PATCH", "/v1/"+first.Name) > 0 {
			cancel()
			return ctx.Err()
		}
		if f.clock.Now().After(start.Add(3 * time.Hour)) {
			t.Fatal("loop missed renewal")
		}
		return nil
	}
	f.manager.RenewLoop(ctx)
	if f.count("PATCH", "/v1/"+first.Name) != 1 {
		t.Fatal("loop did not renew")
	}
}

func TestLifecycleEventsReconcileAuthoritativeState(t *testing.T) {
	for _, typ := range []string{"expirationReminder", "expired", "suspended"} {
		t.Run(typ, func(t *testing.T) {
			f := setup(t)
			first := f.reconcile()
			switch typ {
			case "expirationReminder":
				f.clock.advance(3 * time.Hour)
			case "expired":
				f.clock.advance(4 * time.Hour)
			case "suspended":
				f.server.SuspendSubscription(first.Name)
			}
			before := f.count("GET", "/v1/"+first.Name)
			if err := f.manager.HandleLifecycle(context.Background(), "google.workspace.events.subscription.v1."+typ); err != nil {
				t.Fatal(err)
			}
			if f.count("GET", "/v1/"+first.Name) <= before || f.ledger.Subscription.State != "ACTIVE" {
				t.Fatal("lifecycle event did not reconcile authoritative state")
			}
			if (typ == "expired") != (first.Name != f.ledger.Subscription.Name) {
				t.Fatalf("unexpected recreation for %s", typ)
			}
			// A duplicate/late event must observe the healthy subscription and do nothing.
			beforeMutations := f.count("POST", "/v1/subscriptions") + f.count("PATCH", "/v1/"+f.ledger.Subscription.Name) + f.count("POST", "/v1/"+first.Name+":reactivate")
			if err := f.manager.HandleLifecycle(context.Background(), "google.workspace.events.subscription.v1."+typ); err != nil {
				t.Fatal(err)
			}
			afterMutations := f.count("POST", "/v1/subscriptions") + f.count("PATCH", "/v1/"+f.ledger.Subscription.Name) + f.count("POST", "/v1/"+first.Name+":reactivate")
			if beforeMutations != afterMutations {
				t.Fatal("late event caused mutation")
			}
		})
	}
}

func TestOperationsAreAwaitedAndFailuresDoNotPersist(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failure], func(t *testing.T) {
			f := setup(t)
			f.server.OperationDelay = 30 * time.Second
			if failure {
				f.server.OperationError = &api.Status{Code: 7, Message: "permission denied"}
			}
			f.clock.onSleep = func(time.Duration) error {
				if f.ledger.Subscription.Name != "" {
					t.Error("persisted before completion")
				}
				return nil
			}
			got, err := f.manager.Reconcile(context.Background())
			if failure {
				if err == nil || f.ledger.Subscription.Name != "" {
					t.Fatalf("failure state=%+v err=%v", got, err)
				}
			} else if err != nil || got.Name == "" {
				t.Fatalf("success state=%+v err=%v", got, err)
			}
			if len(f.clock.sleeps) < 2 || f.clock.sleeps[1] <= f.clock.sleeps[0] {
				t.Fatal("operation polling must back off")
			}
		})
	}
}

func TestReadFailureNeverCreates(t *testing.T) {
	for _, saved := range []bool{false, true} {
		t.Run(map[bool]string{false: "list", true: "get"}[saved], func(t *testing.T) {
			f := setup(t)
			if saved {
				f.reconcile()
			}
			before := f.count("POST", "/v1/subscriptions")
			f.hook = func(r *http.Request) (*http.Response, error) { return nil, errors.New("transport unavailable") }
			if _, err := f.manager.Reconcile(context.Background()); err == nil {
				t.Fatal("must propagate discovery error")
			}
			if f.count("POST", "/v1/subscriptions") != before {
				t.Fatal("created without confirmed absence")
			}
		})
	}
}
