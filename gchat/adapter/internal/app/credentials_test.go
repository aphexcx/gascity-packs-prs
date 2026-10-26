package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
)

func TestCredentialGenerationRefresh(t *testing.T) {
	for _, tc := range []struct {
		name, tokenError    string
		failures, refreshes int32
		hold                bool
	}{
		{name: "refresh recovers", failures: 1, refreshes: 1},
		{name: "invalid grant", failures: 1, refreshes: 1, tokenError: "invalid_grant", hold: true},
		{name: "bounded repeated rejection", failures: 100, refreshes: 2, hold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, true)
			f.replaceIdentityToken(t, "users/123456")
			f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = tc.tokenError })
			var lists, refreshes, notices atomic.Int32
			chatURL, _ := url.Parse(f.cfg.Endpoints.ChatAPIBase)
			proxy := httputil.NewSingleHostReverseProxy(chatURL)
			chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/spaces" && lists.Add(1) <= tc.failures {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"code":401,"message":"credential rejected","status":"UNAUTHENTICATED"}}`))
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer chatServer.Close()
			f.cfg.Endpoints.ChatAPIBase = chatServer.URL
			googleURL, _ := url.Parse(f.google.URL())
			oauthProxy := httputil.NewSingleHostReverseProxy(googleURL)
			oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { refreshes.Add(1); oauthProxy.ServeHTTP(w, r) }))
			defer oauthServer.Close()
			f.cfg.Endpoints.OAuthTokenURL = oauthServer.URL + "/token"
			f.startOptions(t, Options{Notify: func(context.Context, string, string) error { notices.Add(1); return nil }})
			if tc.hold {
				h := f.health(t, "needs-sign-in", 5*time.Second)
				waitFor(t, time.Second, func() bool { return notices.Load() == 1 })
				if tc.tokenError == "" && !strings.Contains(h.Reason, "two consecutive") {
					t.Fatalf("missing bounded reason: %+v", h)
				}
			} else {
				f.health(t, "ready", 5*time.Second)
				if notices.Load() != 0 {
					t.Fatal("successful refresh sent a sign-in notice")
				}
				var counters map[string]int64
				_ = json.Unmarshal(f.status(t)["counters"], &counters)
				if counters["credential_refresh_restarts"] != 1 {
					t.Fatalf("restart not counted: %v", counters)
				}
			}
			if refreshes.Load() != tc.refreshes {
				t.Fatalf("refreshes=%d want %d", refreshes.Load(), tc.refreshes)
			}
		})
	}
}
