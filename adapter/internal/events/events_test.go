package events

import (
	"bytes"
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

func TestLostRenewalResponseThenDiscovery(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	f.clock.advance(3 * time.Hour)
	f.server.Configure(func(s *fakegoogle.Server) { s.DropNextResponse = true; s.OperationDelay = time.Minute })
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("lost renewal response must be reported")
	}
	// A successful GET of the old expiry does not prove the pending renewal
	// failed. Allow it to settle before another mutation.
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("pending renewal must retry with backoff")
	}
	if f.count("PATCH", "/v1/"+first.Name) != 1 {
		t.Fatal("ambiguous renewal was repeated before discovery could settle")
	}
	f.clock.advance(time.Minute)
	got := f.reconcile()
	if got.Expiry != first.Expiry.Add(3*time.Hour) || f.count("PATCH", "/v1/"+first.Name) != 1 {
		t.Fatalf("lost renewal discovery = %+v", got)
	}
}

func TestLostDelayedCreateDoesNotRepeatWhilePending(t *testing.T) {
	f := setup(t)
	f.server.DropNextResponse = true
	f.server.OperationDelay = time.Minute
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("expected lost response")
	}
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("expected retryable ambiguity")
	}
	if f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("repeated pending create")
	}
	f.clock.advance(time.Minute)
	f.reconcile()
	if f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("did not discover delayed create")
	}
}

func TestAlreadyExistsWithoutVisibleSubscriptionBacksOff(t *testing.T) {
	f := setup(t)
	f.server.CreateReturnsAlreadyExists = true
	for range 2 {
		if _, err := f.manager.Reconcile(context.Background()); err == nil {
			t.Fatal("expected not yet discoverable")
		}
	}
	if f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("repeated create while ALREADY_EXISTS is unresolved")
	}
}

func TestOperationTimeoutResumesKnownOperation(t *testing.T) {
	f := setup(t)
	start := f.clock.Now()
	f.server.OperationDelay = 3 * time.Minute
	if _, err := f.manager.Reconcile(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout = %v", err)
	}
	if !f.clock.Now().Equal(start.Add(2*time.Minute)) || f.ledger.Subscription.Name != "" {
		t.Fatal("timeout must bound polling without persisting a pending result")
	}
	// Resumed reconciles do one bounded poll; the caller supplies backoff.
	f.clock.advance(time.Minute)
	got := f.reconcile()
	if got.Expiry != start.Add(4*time.Hour) || f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatalf("resumed result = %+v", got)
	}
}

func TestOperationSuspendedResultRemainsRetryable(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	f.clock.advance(3 * time.Hour)
	f.server.OperationDelay = 10 * time.Second
	f.clock.onSleep = func(time.Duration) error { f.server.SuspendSubscription(first.Name); return nil }
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("renewal completed suspended; must retry reconciliation instead of sleeping until next renewal")
	}
	if f.ledger.Subscription.State != "SUSPENDED" {
		t.Fatal("authoritative suspended state not persisted")
	}
}

func TestPersistenceFailureDiscoveredOnRetry(t *testing.T) {
	f := setup(t)
	validPath := f.manager.path
	f.manager.path = filepath.Join(t.TempDir(), "missing", "state.json")
	if _, err := f.manager.Reconcile(context.Background()); err == nil {
		t.Fatal("expected persistence failure")
	}
	if f.ledger.Subscription.Name != "" {
		t.Fatal("unsaved state leaked into ledger")
	}
	f.manager.path = validPath
	f.reconcile()
	if f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("persistence failure caused duplicate create")
	}
}

func TestSavedMissingNameDiscoversReplacement(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	f.ledger.Subscription.Name = "subscriptions/missing"
	got := f.reconcile()
	if got.Name != first.Name || f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("stale ledger should discover existing replacement")
	}
}

func TestDiscoveryReadsAllPagesAndRejectsIncompatibleSubscriptions(t *testing.T) {
	for _, wrong := range []string{"topic", "events", "payload", "field mask", "expiry", "multiple"} {
		t.Run(wrong, func(t *testing.T) {
			f := setup(t)
			first := f.reconcile()
			f.ledger.Subscription = ledger.SubscriptionState{}
			f.server.Configure(func(s *fakegoogle.Server) {
				s.PageSize = 1
				sub := s.Subscriptions[first.Name]
				switch wrong {
				case "topic":
					sub.NotificationEndpoint.PubsubTopic = "projects/example/topics/other"
				case "events":
					sub.EventTypes = []string{EventTypes[0]}
				case "payload":
					sub.PayloadOptions.IncludeResource = false
				case "field mask":
					sub.PayloadOptions.FieldMask = "message.name"
				case "expiry":
					sub.ExpireTime = "invalid"
				case "multiple":
					other := *sub
					other.Name = "subscriptions/second"
					s.Subscriptions[other.Name] = &other
				}
			})
			if _, err := f.manager.Reconcile(context.Background()); err == nil {
				t.Fatal("unsafe subscription adoption")
			}
			if f.count("POST", "/v1/subscriptions") != 1 {
				t.Fatal("incompatible subscription is not absence")
			}
			if wrong == "multiple" && f.requests[len(f.requests)-1].URL.Query().Get("pageToken") == "" {
				t.Fatal("did not inspect subsequent discovery page")
			}
		})
	}
}

func TestRenewLoopClockJumps(t *testing.T) {
	for _, jump := range []time.Duration{-time.Hour, 2 * time.Hour} {
		t.Run(jump.String(), func(t *testing.T) {
			f := setup(t)
			first := f.reconcile()
			start := f.clock.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			jumped := false
			f.clock.onSleep = func(d time.Duration) error {
				if !jumped {
					f.clock.advance(jump)
					jumped = true
				}
				if f.count("PATCH", "/v1/"+first.Name) > 0 {
					cancel()
					return ctx.Err()
				}
				if f.clock.Now().After(start.Add(3 * time.Hour)) {
					t.Fatal("missed renewal after clock jump")
				}
				return nil
			}
			f.hook = func(r *http.Request) (*http.Response, error) {
				if r.Method == "PATCH" && f.clock.Now() != start.Add(3*time.Hour) {
					t.Errorf("renewal at %v", f.clock.Now())
				}
				return http.DefaultTransport.RoundTrip(r)
			}
			f.manager.RenewLoop(ctx)
		})
	}
}

func TestRenewLoopRetriesWithBoundedBackoff(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.hook = func(*http.Request) (*http.Response, error) { return nil, errors.New("unavailable") }
	f.clock.onSleep = func(time.Duration) error {
		if len(f.clock.sleeps) == 10 {
			cancel()
			return ctx.Err()
		}
		return nil
	}
	f.manager.RenewLoop(ctx)
	if !reflect.DeepEqual(f.clock.sleeps, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, 64 * time.Second, 2 * time.Minute, 2 * time.Minute, 2 * time.Minute}) {
		t.Fatalf("backoff = %v", f.clock.sleeps)
	}
	if f.count("POST", "/v1/subscriptions") != 0 {
		t.Fatal("created on lookup failure")
	}
}

func TestCancellationAndUnknownLifecycle(t *testing.T) {
	f := setup(t)
	if err := f.manager.HandleLifecycle(context.Background(), "google.workspace.chat.message.v1.created"); err != nil || len(f.requests) != 0 {
		t.Fatal("unrelated event triggered reconciliation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.manager.Reconcile(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	f.manager.RenewLoop(ctx)
	if len(f.requests) != 0 {
		t.Fatal("canceled work made requests")
	}
	f.server.OperationDelay = time.Minute
	f.clock.onSleep = func(time.Duration) error { return context.Canceled }
	if _, err := f.manager.Reconcile(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("poll cancel = %v", err)
	}
}

func TestResumedOperationCannotOverwriteNewerSuspension(t *testing.T) {
	f := setup(t)
	f.server.OperationDelay = 3 * time.Minute
	if _, err := f.manager.Reconcile(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	f.clock.advance(time.Minute)
	f.server.Advance(0) // complete the operation before the later suspension
	f.server.SuspendSubscription()
	f.server.Configure(func(s *fakegoogle.Server) { s.OperationDelay = 0 })
	got := f.reconcile()
	if f.count("POST", "/v1/"+got.Name+":reactivate") != 1 {
		t.Fatal("stale operation result hid an authoritative suspension")
	}
}

func TestExpiredOperationRecordDoesNotPreventReconciliation(t *testing.T) {
	f := setup(t)
	f.server.OperationDelay = 3 * time.Minute
	if _, err := f.manager.Reconcile(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	f.clock.advance(time.Minute)
	f.hook = func(r *http.Request) (*http.Response, error) {
		if strings.HasPrefix(r.URL.Path, "/v1/operations/") {
			return &http.Response{StatusCode: 404, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":404,"message":"operation record expired"}}`))}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	}
	_, _ = f.manager.Reconcile(context.Background())
	got := f.reconcile()
	if got.Name == "" || f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("missing operation record prevented discovery")
	}
}

func TestConcurrentReconcilesCreateOnce(t *testing.T) {
	f := setup(t)
	errs := make(chan error, 12)
	for range 12 {
		go func() { _, err := f.manager.Reconcile(context.Background()); errs <- err }()
	}
	for range 12 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("concurrent reconciliation created more than once")
	}
}

func TestRenewalUsesReturnedTTL(t *testing.T) {
	f := setup(t)
	start := f.clock.Now()
	f.hook = func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err != nil || r.Method != "POST" || r.URL.Path != "/v1/subscriptions" {
			return response, err
		}
		var op api.Operation
		if err := json.NewDecoder(response.Body).Decode(&op); err != nil {
			return nil, err
		}
		response.Body.Close()
		var sub api.Subscription
		if err := json.Unmarshal(op.Response, &sub); err != nil {
			return nil, err
		}
		sub.ExpireTime = start.Add(2 * time.Hour).Format(time.RFC3339Nano)
		f.server.Configure(func(s *fakegoogle.Server) { s.Subscriptions[sub.Name].ExpireTime = sub.ExpireTime })
		data, err := json.Marshal(sub)
		if err != nil {
			return nil, err
		}
		op.Response = data
		data, err = json.Marshal(op)
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(strings.NewReader(string(data)))
		return response, nil
	}
	first := f.reconcile()
	if first.Expiry != start.Add(2*time.Hour) {
		t.Fatal("computed expiry instead of preserving returned expiry")
	}
	f.clock.advance(90*time.Minute - time.Nanosecond)
	f.reconcile()
	if f.count("PATCH", "/v1/"+first.Name) != 0 {
		t.Fatal("renewed using a fixed maximum instead of returned TTL")
	}
	f.clock.advance(time.Nanosecond)
	f.reconcile()
	if f.count("PATCH", "/v1/"+first.Name) != 1 {
		t.Fatal("did not renew at 75% of returned TTL")
	}
}

func TestRecoveryNotificationsFollowDurableCreationAndReactivation(t *testing.T) {
	f := setup(t)
	assertRecovery := func(want bool) {
		t.Helper()
		select {
		case <-f.manager.RecoveryNeeded():
			if !want {
				t.Fatal("healthy reconciliation requested catch-up")
			}
			disk, err := ledger.Load(f.path, true)
			if err != nil || disk.Subscription.State != "ACTIVE" {
				t.Fatalf("recovery signaled before durable active state: %v", err)
			}
		default:
			if want {
				t.Fatal("recovery did not request catch-up")
			}
		}
	}
	first := f.reconcile()
	assertRecovery(true)
	f.reconcile()
	assertRecovery(false)
	f.clock.advance(3 * time.Hour)
	f.reconcile()
	assertRecovery(false)
	f.server.SuspendSubscription(first.Name)
	f.reconcile()
	assertRecovery(true)
	f.clock.advance(4 * time.Hour)
	f.reconcile()
	assertRecovery(true)
	// Recovery obligations coalesce and never stall reconciliation if the app
	// is already catching up. Startup independently catches up after a crash.
	f.server.SuspendSubscription()
	f.reconcile()
	f.server.SuspendSubscription()
	f.reconcile()
	assertRecovery(true)
	assertRecovery(false)
}

func jsonResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestStuckMutationRecreatesAfterConfirmedAbsence(t *testing.T) {
	for _, kind := range []string{"renew", "reactivate"} {
		t.Run(kind, func(t *testing.T) {
			f := setup(t)
			first := f.reconcile()
			<-f.manager.RecoveryNeeded()
			if kind == "renew" {
				f.clock.advance(3 * time.Hour)
			} else {
				f.server.SuspendSubscription(first.Name)
			}
			f.hook = func(r *http.Request) (*http.Response, error) {
				if r.Method == "PATCH" || strings.HasSuffix(r.URL.Path, ":reactivate") || r.URL.Path == "/v1/operations/stuck" {
					return jsonResponse(200, `{"name":"operations/stuck","done":false}`), nil
				}
				return http.DefaultTransport.RoundTrip(r)
			}
			if _, err := f.manager.Reconcile(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("initial poll = %v", err)
			}
			f.clock.advance(4 * time.Hour)
			f.server.Configure(func(s *fakegoogle.Server) { delete(s.Subscriptions, first.Name) })
			got := f.reconcile()
			if got.Name == first.Name || f.count("POST", "/v1/subscriptions") != 2 {
				t.Fatalf("stuck %s prevented one recreation: %+v", kind, got)
			}
			select {
			case <-f.manager.RecoveryNeeded():
			default:
				t.Fatal("recreation did not request recovery")
			}
			f.reconcile()
			if f.count("POST", "/v1/subscriptions") != 2 {
				t.Fatal("recreated twice")
			}
		})
	}
}

func TestResumedCreatePollIsBoundedWithoutDuplicateCreation(t *testing.T) {
	f := setup(t)
	f.server.OperationDelay = time.Hour
	if _, err := f.manager.Reconcile(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	for range 5 {
		before := len(f.requests)
		start := f.clock.Now()
		if _, err := f.manager.Reconcile(context.Background()); err == nil {
			t.Fatal("pending operation must be reported")
		}
		if len(f.requests)-before != 1 || !strings.HasPrefix(f.requests[before].URL.Path, "/v1/operations/") || !f.clock.Now().Equal(start) {
			t.Fatalf("resume should only poll once without sleeping; requests=%d elapsed=%s", len(f.requests)-before, f.clock.Now().Sub(start))
		}
	}
	if f.count("POST", "/v1/subscriptions") != 1 {
		t.Fatal("pending create must not be replaced")
	}
}

func TestSuspensionReasonReportedInErrorAndLog(t *testing.T) {
	f := setup(t)
	first := f.reconcile()
	var logs bytes.Buffer
	f.manager.log = slog.New(slog.NewTextHandler(&logs, nil))
	f.server.Configure(func(s *fakegoogle.Server) {
		s.Subscriptions[first.Name].State = "SUSPENDED"
		s.Subscriptions[first.Name].SuspensionReason = "ENDPOINT_PERMISSION_DENIED"
	})
	f.hook = func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, ":reactivate") {
			return jsonResponse(403, `{"error":{"code":403,"message":"permission denied"}}`), nil
		}
		return http.DefaultTransport.RoundTrip(r)
	}
	for range 2 { // The ambiguity hold must keep the actionable reason, too.
		_, err := f.manager.Reconcile(context.Background())
		if err == nil || !strings.Contains(err.Error(), "ENDPOINT_PERMISSION_DENIED") || !strings.Contains(err.Error(), first.Name) {
			t.Fatalf("suspension error = %v", err)
		}
	}
	if !strings.Contains(logs.String(), "ENDPOINT_PERMISSION_DENIED") || !strings.Contains(logs.String(), first.Name) {
		t.Fatalf("suspension log = %q", logs.String())
	}
}

func TestScopeRevocationWaitsForTokenReload(t *testing.T) {
	for _, reason := range []string{"USER_SCOPE_REVOKED", "APP_SCOPE_REVOKED"} {
		t.Run(reason, func(t *testing.T) {
			f := setup(t)
			first := f.reconcile()
			<-f.manager.RecoveryNeeded()
			f.server.Configure(func(s *fakegoogle.Server) {
				s.Subscriptions[first.Name].State = "SUSPENDED"
				s.Subscriptions[first.Name].SuspensionReason = reason
			})
			for range 2 {
				_, err := f.manager.Reconcile(context.Background())
				if err == nil || !strings.Contains(err.Error(), reason) {
					t.Fatalf("scope suspension error = %v", err)
				}
				f.clock.advance(time.Minute)
			}
			if f.count("POST", "/v1/"+first.Name+":reactivate") != 0 {
				t.Fatal("reactivation attempted before credential reload")
			}
			reloader, ok := any(f.manager).(interface {
				TokenReloaded(context.Context) (ledger.SubscriptionState, error)
			})
			if !ok {
				t.Fatal("missing token-reload entry point")
			}
			got, err := reloader.TokenReloaded(context.Background())
			if err != nil || got.State != "ACTIVE" || f.count("POST", "/v1/"+first.Name+":reactivate") != 1 {
				t.Fatalf("reload state=%+v error=%v", got, err)
			}
			select {
			case <-f.manager.RecoveryNeeded():
			default:
				t.Fatal("reactivation after reload did not request recovery")
			}
			f.server.Configure(func(s *fakegoogle.Server) {
				s.Subscriptions[first.Name].State = "SUSPENDED"
				s.Subscriptions[first.Name].SuspensionReason = reason
			})
			if _, err := f.manager.Reconcile(context.Background()); err == nil {
				t.Fatal("a later scope revocation needs a new reload")
			}
		})
	}
}
