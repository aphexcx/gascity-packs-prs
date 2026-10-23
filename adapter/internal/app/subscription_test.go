package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/events"
	"gchat-adapter/internal/fakes/fakegoogle"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	workspace "google.golang.org/api/workspaceevents/v1"
)

func TestSubscriptionNoticesForIsolatedErrorsAndEachRefusalCause(t *testing.T) {
	f := newFixture(t, false)
	a, _ := generationFixture(t, f)
	for _, err := range []error{errors.New("temporary network failure"), &googleapi.Error{Code: 429}, &googleapi.Error{Code: 500}} {
		a.subscriptionFailure(err)
		a.subscriptionRecovered()
	}
	if len(a.notices) != 0 {
		t.Fatal("transient subscription failure notified operator")
	}
	disabled := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "accessNotConfigured"}}}
	iam := &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "iamPermissionDenied"}}}
	a.subscriptionFailure(disabled)
	a.subscriptionFailure(disabled)
	a.subscriptionFailure(iam)
	if len(a.notices) != 2 {
		t.Fatalf("want one notice per distinct cause; got %d", len(a.notices))
	}
	first, second := <-a.notices, <-a.notices
	if first.dedup == second.dedup {
		t.Fatal("different causes share mail dedup key")
	}
	a.subscriptionRecovered()
	a.subscriptionFailure(disabled)
	if len(a.notices) != 1 {
		t.Fatal("recovery did not reset incident dedup")
	}
}

// Use the real manager's completed reconciliation count, as both the startup
// retry loop and the steady-state monitor do. Requests stay on local fakes.
func subscriptionNoticeFixture(t *testing.T) (*application, *events.Manager, *atomic.Int32) {
	t.Helper()
	f := newFixture(t, false)
	a, _ := generationFixture(t, f)
	code := new(atomic.Int32)
	code.Store(http.StatusTooManyRequests)
	u, err := url.Parse(f.cfg.Endpoints.EventsAPIBase)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := code.Load(); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(status))
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"private-provider-message"}}`, status)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	service, err := workspace.NewService(t.Context(), option.WithHTTPClient(endpoint.Client()), option.WithEndpoint(endpoint.URL+"/"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := events.New(events.Options{Service: service, Topic: f.cfg.Secrets.Topic, Ledger: a.l, LedgerPath: f.cfg.Endpoints.StateFile, LedgerMu: &a.mu, Log: a.log})
	if err != nil {
		t.Fatal(err)
	}
	a.manager.Store(m)
	return a, m, code
}

func TestSubscriptionNoticeAfterThreeConsecutiveFailures(t *testing.T) {
	a, m, _ := subscriptionNoticeFixture(t)
	for attempt := 1; attempt <= 5; attempt++ {
		_, err := m.Reconcile(t.Context())
		if err == nil {
			t.Fatal("quota failure not reported")
		}
		a.subscriptionFailure(err)
		want := 0
		if attempt == 3 {
			want = 1
		}
		if len(a.notices) != want {
			t.Fatalf("attempt %d: got %d notices, want %d", attempt, len(a.notices), want)
		}
		if want == 1 {
			n := <-a.notices
			if n.kind != "subscription" || !strings.Contains(n.message, "HTTP 429") || !strings.Contains(n.message, "README") || strings.Contains(n.message, "private-provider-message") {
				t.Fatalf("incorrect quota notice: %+v", n)
			}
		}
	}
}

func TestSubscriptionFailurePollingDoesNotAdvanceThreshold(t *testing.T) {
	a, m, _ := subscriptionNoticeFixture(t)
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := m.Reconcile(t.Context()); err == nil {
			t.Fatal("quota failure not reported")
		}
		for range 5 {
			last, _, _, _ := m.Status()
			a.subscriptionFailure(last)
		}
		if len(a.notices) != 0 {
			t.Fatalf("polling failure %d triggered a notice", attempt)
		}
	}
	_, err := m.Reconcile(t.Context())
	if err == nil {
		t.Fatal("quota failure not reported")
	}
	a.subscriptionFailure(err)
	if len(a.notices) != 1 {
		t.Fatal("third reconciliation failure did not trigger a notice")
	}
}

func TestSubscriptionRecoveryResetsFailureThresholdAndNoticeDedup(t *testing.T) {
	for _, initialFailures := range []int{1, 3} {
		t.Run(fmt.Sprint(initialFailures), func(t *testing.T) {
			a, m, code := subscriptionNoticeFixture(t)
			for range initialFailures {
				_, err := m.Reconcile(t.Context())
				if err == nil {
					t.Fatal("quota failure not reported")
				}
				a.subscriptionFailure(err)
			}
			if initialFailures == 3 {
				if len(a.notices) != 1 {
					t.Fatal("initial incident did not trigger a notice")
				}
				<-a.notices
			}
			code.Store(0)
			if _, err := m.Reconcile(t.Context()); err != nil {
				t.Fatalf("subscription recovery: %v", err)
			}
			a.subscriptionRecovered()
			_, failures, _, _ := m.Status()
			if failures != 0 {
				t.Fatalf("recovery retained %d consecutive failures", failures)
			}
			code.Store(http.StatusTooManyRequests)
			for attempt := 1; attempt <= 3; attempt++ {
				_, err := m.Reconcile(t.Context())
				if err == nil {
					t.Fatal("quota failure not reported after recovery")
				}
				a.subscriptionFailure(err)
				want := 0
				if attempt == 3 {
					want = 1
				}
				if len(a.notices) != want {
					t.Fatalf("attempt %d after recovery: got %d notices, want %d", attempt, len(a.notices), want)
				}
			}
		})
	}
}

func TestSubscriptionMonitorResetsNoticesWhileGCUnavailable(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/123456")
	var code atomic.Int32
	u, err := url.Parse(f.cfg.Endpoints.EventsAPIBase)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := code.Load(); status != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(int(status))
			fmt.Fprintf(w, `{"error":{"code":%d,"message":"quota temporarily exceeded"}}`, status)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(endpoint.Close)
	f.cfg.Endpoints.EventsAPIBase = endpoint.URL
	a, source := generationFixture(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.generation(ctx, source, false, new(atomic.Bool)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("generation shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("generation did not shut down")
		}
	})
	waitFor(t, 5*time.Second, func() bool { return a.s.Health().State == "ready" })
	m := a.manager.Load()
	a.gcDown.Store(true)
	for incident := 1; incident <= 2; incident++ {
		code.Store(http.StatusTooManyRequests)
		for range 3 {
			if _, err := m.Reconcile(ctx); err == nil {
				t.Fatal("quota failure not reported")
			}
		}
		select {
		case n := <-a.notices:
			if n.kind != "subscription" || !strings.Contains(n.message, "HTTP 429") {
				t.Fatalf("incident %d: incorrect notice: %+v", incident, n)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("incident %d: monitor did not notify after three failures", incident)
		}
		if incident == 1 {
			code.Store(0)
			state, err := m.Reconcile(ctx)
			if err != nil || state.State != "ACTIVE" {
				t.Fatalf("subscription recovery: state=%+v err=%v", state, err)
			}
			// This health transition proves the monitor observed the recovered
			// subscription while overall readiness remained blocked by GC.
			waitFor(t, 3*time.Second, func() bool {
				return a.s.Health().Reason == "gc unavailable; deliveries retained for retry"
			})
		}
	}
}

func TestSubscriptionRecoveryBetweenObservationsResetsNoticeDedup(t *testing.T) {
	a, m, code := subscriptionNoticeFixture(t)
	for incident := 1; incident <= 2; incident++ {
		code.Store(http.StatusTooManyRequests)
		var last error
		for range 3 {
			_, last = m.Reconcile(t.Context())
			if last == nil {
				t.Fatal("quota failure not reported")
			}
		}
		// The app observes only failed status, even though the subscription
		// recovered between these incidents before failing three more times.
		a.subscriptionFailure(last)
		if len(a.notices) != 1 {
			t.Fatalf("incident %d: got %d notices, want 1", incident, len(a.notices))
		}
		<-a.notices
		for range 5 {
			a.subscriptionFailure(last)
		}
		if len(a.notices) != 0 {
			t.Fatalf("incident %d: polling duplicated the notice", incident)
		}
		if incident == 1 {
			code.Store(0)
			state, err := m.Reconcile(t.Context())
			if err != nil || state.State != "ACTIVE" {
				t.Fatalf("subscription recovery: state=%+v err=%v", state, err)
			}
		}
	}
}

func TestSubscriptionReasonsParseCurrentGoogleErrorDetails(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{{"SERVICE_DISABLED", "Workspace Events API not enabled"}, {"IAM_PERMISSION_DENIED", "topic Publisher IAM permission denied"}} {
		t.Run(tc.reason, func(t *testing.T) {
			err := &googleapi.Error{Code: 403, Message: "private-secret", Details: []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": tc.reason, "metadata": map[string]any{"secret": "private-secret"}}}}
			got := subscriptionReason(err)
			if !strings.Contains(got, tc.want) || strings.Contains(got, "private-secret") {
				t.Fatalf("unsafe or imprecise reason: %s", got)
			}
		})
	}
}

func TestSubscriptionLogSanitizesNestedGroups(t *testing.T) {
	var logs logBuffer
	l := slog.New(subscriptionLogHandler{slog.NewJSONHandler(&logs, nil)})
	err := &googleapi.Error{Code: 403, Message: "private-secret"}
	l.WithGroup("outer").With(slog.Group("bound", slog.Group("nested", slog.Any("error", err)))).Warn("reconcile", slog.Group("call", slog.Any("error", err)))
	if strings.Contains(logs.String(), "private-secret") || strings.Count(logs.String(), "subscription refused:") != 2 {
		t.Fatalf("nested error escaped sanitization: %s", logs.String())
	}
}

func TestSubscriptionRefusalRetriesWithoutSignIn(t *testing.T) {
	for _, code := range []int{403, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newFixture(t, false)
			f.replaceIdentityToken(t, "users/123456")
			var fixed atomic.Bool
			var creates atomic.Int32
			var discoveries atomic.Int32
			u, _ := url.Parse(f.cfg.Endpoints.EventsAPIBase)
			proxy := httputil.NewSingleHostReverseProxy(u)
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" && r.URL.Path == "/v1/subscriptions" {
					discoveries.Add(1)
				}
				if r.Method == "POST" && r.URL.Path == "/v1/subscriptions" {
					creates.Add(1)
					if !fixed.Load() {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(code)
						fmt.Fprintf(w, `{"error":{"code":%d,"message":"private-token-secret topic access","status":"PERMISSION_DENIED","errors":[{"reason":"accessNotConfigured"}]}}`, code)
						return
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer endpoint.Close()
			f.cfg.Endpoints.EventsAPIBase = endpoint.URL
			var logs logBuffer
			notices := make(chan string, 4)
			f.startOptions(t, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Notify: func(_ context.Context, subject, body string) error { notices <- subject + ": " + body; return nil }})
			select {
			case notice := <-notices:
				if !strings.Contains(notice, "gchat: subscription") || !strings.Contains(notice, "README") || !strings.Contains(notice, fmt.Sprintf("HTTP %d", code)) || strings.Contains(notice, "private-token-secret") {
					t.Fatalf("incorrect notice: %s", notice)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no subscription notice")
			}
			h := f.health(t, "catching-up", time.Second)
			if !strings.Contains(h.Reason, "subscription refused:") {
				t.Fatalf("health: %+v", h)
			}
			// The subscriber preserves its two-minute ambiguous-create window;
			// retries first discover, avoiding duplicate subscriptions.
			waitFor(t, 4*time.Second, func() bool { return discoveries.Load() >= 2 })
			fixed.Store(true)
			f.google.Configure(func(g *fakegoogle.Server) {
				g.Subscriptions["subscriptions/repaired"] = &workspace.Subscription{Name: "subscriptions/repaired", State: "ACTIVE", TargetResource: events.Target, EventTypes: events.EventTypes, PayloadOptions: &workspace.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &workspace.NotificationEndpoint{PubsubTopic: f.cfg.Secrets.Topic}, ExpireTime: time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339Nano)}
			})
			f.health(t, "ready", 5*time.Second)
			if len(notices) != 0 {
				t.Fatalf("duplicate or credentials notice: %s", <-notices)
			}
			if !strings.Contains(logs.String(), "subscription recovered") || strings.Contains(logs.String(), "private-token-secret") {
				t.Fatalf("missing recovery or unsafe cause: %s", logs.String())
			}
		})
	}
}

func TestSubscriptionRenewalRefusalDoesNotRebuildCredentials(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/123456")
	now := time.Now()
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Subscriptions["subscriptions/renew"] = &workspace.Subscription{Name: "subscriptions/renew", State: "ACTIVE", TargetResource: events.Target, EventTypes: events.EventTypes, PayloadOptions: &workspace.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &workspace.NotificationEndpoint{PubsubTopic: f.cfg.Secrets.Topic}, CreateTime: now.Add(-4 * time.Second).UTC().Format(time.RFC3339Nano), ExpireTime: now.Add(8 * time.Second).UTC().Format(time.RFC3339Nano)}
	})
	var fixed atomic.Bool
	u, _ := url.Parse(f.cfg.Endpoints.EventsAPIBase)
	proxy := httputil.NewSingleHostReverseProxy(u)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" && !fixed.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			fmt.Fprint(w, `{"error":{"code":403,"message":"private-token-secret","status":"PERMISSION_DENIED"}}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	f.cfg.Endpoints.EventsAPIBase = endpoint.URL
	var logs logBuffer
	notices := make(chan string, 4)
	f.startOptions(t, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil)), Notify: func(_ context.Context, subject, body string) error { notices <- subject + ": " + body; return nil }})
	f.health(t, "ready", 5*time.Second)
	select {
	case n := <-notices:
		if !strings.Contains(n, "gchat: subscription") {
			t.Fatalf("wrong notice: %s", n)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("renewal did not notify")
	}
	f.health(t, "catching-up", time.Second)
	fixed.Store(true)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Subscriptions["subscriptions/renew"].ExpireTime = time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339Nano)
	})
	f.health(t, "ready", 5*time.Second)
	if strings.Contains(logs.String(), "private-token-secret") {
		t.Fatalf("renewal logged unsanitized cause: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "subscription recovered") {
		t.Fatalf("missing recovery log: %s", logs.String())
	}
}
