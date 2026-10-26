package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/token"
	"gchat-adapter/internal/wire"
)

func generationFixture(t *testing.T, f *fixture) (*application, *token.Source) {
	t.Helper()
	a := &application{cfg: f.cfg, l: ledger.New(), log: slog.New(slog.NewTextHandler(io.Discard, nil)), notices: make(chan notification, 8), notified: map[string]bool{}}
	var err error
	a.s, err = server.New(server.Options{Config: f.cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: f.cfg.Endpoints.StateFile, Logger: a.log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.s.Close)
	a.s.SetClients(nil, gcclient.New(f.cfg.Secrets.GCAPIBase, f.cfg.Secrets.CityName))
	source, err := token.New(f.cfg.Secrets.OAuthClientFile, f.cfg.Identity().TokenFile, f.cfg.Endpoints, token.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	return a, source
}

func TestGenerationRegistersBeforeInstallingChat(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/123456")
	entered, release := make(chan struct{}, 1), make(chan struct{})
	f.intercept = func(r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/extmsg/adapters") {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}
	a, source := generationFixture(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.generation(ctx, source, false, new(atomic.Bool)) }()
	t.Cleanup(func() { cancel(); close(release); <-done })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("no registration")
	}
	// Probe the installed clients directly through a route. The outer app's
	// initializing health would otherwise hide an incorrectly early install.
	a.s.SetHealth(server.Health{State: "ready"})
	rec := httptest.NewRecorder()
	a.s.ServeHTTP(rec, httptest.NewRequest("GET", "/spaces", nil))
	if rec.Code != 503 {
		t.Fatalf("Chat installed before registration completed: %d %s", rec.Code, rec.Body)
	}
}

func TestGenerationDoesNotHoldLedgerAcrossTokenNetwork(t *testing.T) {
	f := newFixture(t, true)
	writeFile(t, f.cfg.Secrets.TokenFile, `{"access_token":"expired","refresh_token":"example-refresh","expiry":"2000-01-01T00:00:00Z","identity_user_id":"users/123456"}`)
	f.cfg.Endpoints.OAuthTokenURL = f.cfg.Endpoints.ChatAPIBase + "/token"
	entered, release := make(chan struct{}, 1), make(chan struct{})
	f.intercept = func(r *http.Request) {
		if r.URL.Path == "/token" {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}
	a, source := generationFixture(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.generation(ctx, source, false, new(atomic.Bool)) }()
	t.Cleanup(func() { cancel(); close(release); <-done })
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("token refresh did not start")
	}
	if !a.mu.TryLock() {
		t.Fatal("ledger mutex held across token network call")
	}
	a.mu.Unlock()
}

func TestRecoveryRequiredPersistsWithoutOperatorAction(t *testing.T) {
	f := newFixture(t, true)
	writeFile(t, f.cfg.Endpoints.StateFile, "invalid ledger")
	f.start(t)
	f.health(t, "recovery-required", time.Second)
	f.replaceIdentityToken(t, "users/123456")
	for deadline := time.Now().Add(2200 * time.Millisecond); time.Now().Before(deadline); {
		r, err := f.client.Get("http://adapter/healthz")
		if err != nil {
			t.Fatal(err)
		}
		var h server.Health
		json.NewDecoder(r.Body).Decode(&h)
		r.Body.Close()
		if h.State != "recovery-required" {
			t.Fatalf("automatic recovery without operator: %+v", h)
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, err := os.ReadFile(f.cfg.Endpoints.StateFile)
	if err != nil || string(b) != "invalid ledger" {
		t.Fatalf("corrupt state changed: %q %v", b, err)
	}
}

func TestRetryPendingWaitsForRegistrationAndReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	synctest.Test(t, func(t *testing.T) {
		var requests atomic.Int32
		previous := http.DefaultTransport
		http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests.Add(1)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"TranscriptEntry":{"Text":"recorded"},"Receipt":{"delivered":true}}`)), Request: r}, nil
		})
		defer func() { http.DefaultTransport = previous }()
		cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity", GCAPIBase: "https://example.invalid"}, Endpoints: config.Endpoints{StateFile: path, ChatAPIBase: "https://example.invalid"}}
		a := &application{cfg: cfg, l: ledger.New(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		p := wire.PublishRequest{SessionID: "example", IdempotencyKey: "pending", Conversation: wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "default", ConversationID: "spaces/room", Kind: "room"}}
		a.l.Sends[p.IdempotencyKey] = &ledger.SendState{Key: p.IdempotencyKey, Space: p.Conversation.ConversationID, Request: &p, RecordPending: true, Receipt: &wire.PublishReceipt{Delivered: true, MessageID: "spaces/room/messages/one", Conversation: p.Conversation}}
		c, err := chat.New(t.Context(), http.DefaultClient, cfg.Endpoints)
		if err != nil {
			t.Fatal(err)
		}
		a.s, err = server.New(server.Options{Config: cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: path, Chat: c, GC: gcclient.New(cfg.Secrets.GCAPIBase, cfg.Secrets.CityName), Logger: a.log})
		if err != nil {
			t.Fatal(err)
		}
		defer a.s.Close()
		a.s.SetHealth(server.Health{State: "ready"})
		registered := new(atomic.Bool)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); a.retryPending(ctx, registered) }()
		defer func() { cancel(); <-done }()
		time.Sleep(6 * time.Second)
		synctest.Wait()
		if requests.Load() != 0 {
			t.Fatal("sweep ran before registration")
		}
		a.s.SetHealth(server.Health{State: "initializing"})
		registered.Store(true)
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if requests.Load() != 0 {
			t.Fatal("sweep ran before readiness")
		}
		a.s.SetHealth(server.Health{State: "ready"})
		time.Sleep(5 * time.Second)
		synctest.Wait()
		if requests.Load() != 1 {
			t.Fatalf("ready sweep requests=%d", requests.Load())
		}
	})
}
