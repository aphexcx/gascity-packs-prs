package app

import (
	"context"
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
	workspace "google.golang.org/api/workspaceevents/v1"
)

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
