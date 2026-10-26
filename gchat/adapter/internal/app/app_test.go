package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/server"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type fixture struct {
	cfg       *config.Config
	google    *fakegoogle.Server
	gc        *fakegc.Server
	client    *http.Client
	pub       *pubsub.Publisher
	mu        sync.Mutex
	requests  []string
	intercept func(*http.Request)
}

func newFixture(t *testing.T, credential bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "gc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", stub)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	socketDir, err := os.MkdirTemp("/tmp", "gchat-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	f := &fixture{google: fakegoogle.New(t, nil), gc: fakegc.New(t)}
	ps := pstest.NewServer()
	t.Cleanup(func() { _ = ps.Close() })
	pc, err := pubsub.NewClient(context.Background(), "example", option.WithEndpoint(ps.Addr), option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	topic := "projects/example/topics/chat"
	_, err = pc.TopicAdminClient.CreateTopic(context.Background(), &pubsubpb.Topic{Name: topic})
	if err != nil {
		t.Fatal(err)
	}
	sub := "projects/example/subscriptions/chat"
	_, err = pc.SubscriptionAdminClient.CreateSubscription(context.Background(), &pubsubpb.Subscription{Name: sub, Topic: topic, AckDeadlineSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	f.pub = pc.Publisher(topic)
	t.Cleanup(f.pub.Stop)
	f.cfg = &config.Config{SecretsDir: dir, Secrets: config.Secrets{ProjectID: "example", Topic: topic, Subscription: sub, CityName: "examplecity", ServiceSocket: filepath.Join(dir, "s.sock"), ServiceURLPrefix: "/svc/gchat", OAuthClientFile: filepath.Join(dir, "client.json"), TokenFile: filepath.Join(dir, "token.json")}, Endpoints: config.Endpoints{PubSubEndpoint: ps.Addr, MediaDir: filepath.Join(dir, "media"), UploadRoot: filepath.Join(dir, "uploads"), StateFile: filepath.Join(dir, "state.json"), MediaMaxBytes: 1 << 20}}
	f.cfg.Endpoints.ChatAPIBase = f.proxy(t, f.google.URL(), "google")
	f.cfg.Secrets.ServiceSocket = filepath.Join(socketDir, "s.sock")
	f.cfg.Endpoints.EventsAPIBase = f.cfg.Endpoints.ChatAPIBase
	f.cfg.Endpoints.OAuthTokenURL = f.google.URL() + "/token"
	f.cfg.Secrets.GCAPIBase = f.proxy(t, f.gc.URL(), "gc")
	writeFile(t, f.cfg.Secrets.OAuthClientFile, `{"installed":{"client_id":"example","client_secret":"example","auth_uri":"http://127.0.0.1/authorize","token_uri":"`+f.google.URL()+`/token","redirect_uris":["http://localhost"]}}`)
	if credential {
		f.replaceToken(t)
	}
	f.client = &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", f.cfg.Secrets.ServiceSocket)
	}}}
	t.Cleanup(func() { f.client.CloseIdleConnections() })
	return f
}

func (f *fixture) proxy(t *testing.T, target, label string) string {
	u, _ := url.Parse(target)
	p := httputil.NewSingleHostReverseProxy(u)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, label+" "+r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if f.intercept != nil {
			f.intercept(r)
		}
		p.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) replaceToken(t *testing.T) {
	t.Helper()
	writeFile(t, f.cfg.Secrets.TokenFile+".new", `{"access_token":"example","refresh_token":"example-refresh","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z"}`)
	if err := os.Rename(f.cfg.Secrets.TokenFile+".new", f.cfg.Secrets.TokenFile); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) start(t *testing.T) {
	f.startOptions(t, Options{})
}

func (f *fixture) startOptions(t *testing.T, opts Options) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Notify == nil {
		opts.Notify = func(context.Context, string, string) error { return nil }
	}
	opts.RetryDelay = time.Millisecond
	go func() { done <- RunWithOptions(ctx, f.cfg, opts) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not shut down")
		}
	})
}

func (f *fixture) health(t *testing.T, want string, timeout time.Duration) server.Health {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last server.Health
	for time.Now().Before(deadline) {
		resp, err := f.client.Get("http://adapter/healthz")
		if err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&last)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("health HTTP %d", resp.StatusCode)
			}
			if last.State == want {
				if (resp.Header.Get("X-GC-Health") == "ok") != (want == "ready") {
					t.Fatal("incorrect health header")
				}
				return last
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("health did not become %s in %s: %+v", want, timeout, last)
	return last
}

func TestStartupHealthDoesNotWaitForGoogle(t *testing.T) {
	f := newFixture(t, true)
	f.google.Configure(func(g *fakegoogle.Server) { g.OperationDelay = 10 * time.Second })
	start := time.Now()
	f.start(t)
	f.health(t, "initializing", 2*time.Second)
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("health took %s", elapsed)
	}
	// The supervisor's entire five-second readiness window must remain served.
	time.Sleep(5 * time.Second)
	f.health(t, "initializing", 2*time.Second)
}

func TestStartupOrderAndReadiness(t *testing.T) {
	f := newFixture(t, true)
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	f.mu.Lock()
	reqs := append([]string(nil), f.requests...)
	f.mu.Unlock()
	registration, reconcile, freeze := -1, -1, -1
	for i, r := range reqs {
		if strings.Contains(r, "/extmsg/adapters") && registration < 0 {
			registration = i
		}
		if r == "google POST /v1/subscriptions" && reconcile < 0 {
			reconcile = i
		}
		if r == "google GET /v1/spaces" && freeze < 0 {
			freeze = i
		}
	}
	if registration < 0 || reconcile <= registration || freeze <= reconcile {
		t.Fatalf("startup order: %v", reqs)
	}
}

func TestMissingTokenReplacementResumes(t *testing.T) {
	f := newFixture(t, false)
	f.start(t)
	f.health(t, "needs-sign-in", time.Second)
	waitFor(t, time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, r := range f.requests {
			if strings.Contains(r, "/extmsg/adapters") {
				return true
			}
		}
		return false
	})
	f.replaceToken(t)
	f.health(t, "ready", 5*time.Second)
}

func waitFor(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func (f *fixture) status(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	resp, err := f.client.Get("http://adapter/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]json.RawMessage
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLivePullAndQuarantine(t *testing.T) {
	f := newFixture(t, true)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	l := ledger.New()
	l.IdentityUserID = fakegoogle.UserID
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	data := `{"message":{"name":"spaces/room/messages/one","text":"hello","sender":{"name":"users/222","type":"HUMAN"},"createTime":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","thread":{"name":"spaces/room/threads/one"}}}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.pub.Publish(ctx, &pubsub.Message{Data: []byte(data), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": "one"}}).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	_, err = f.pub.Publish(ctx, &pubsub.Message{Data: []byte("bad"), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created"}}).Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { var n int; _ = json.Unmarshal(f.status(t)["quarantine"], &n); return n == 1 })
	var state ledger.SubscriptionState
	_ = json.Unmarshal(f.status(t)["subscription"], &state)
	if state.State != "ACTIVE" {
		t.Fatal(state)
	}
	if f.status(t)["subscription_retry"] == nil {
		t.Fatal("missing subscription retry status")
	}
}

func TestSettingsReloadUpdatesWakePolicy(t *testing.T) {
	f := newFixture(t, true)
	settings := filepath.Join(f.cfg.SecretsDir, "config.toml")
	env := filepath.Join(f.cfg.SecretsDir, "env")
	writeFile(t, settings, "[defaults]\nwake = 'all'\n")
	writeFile(t, env, "GCHAT_PROJECT_ID=example\nGCHAT_PUBSUB_TOPIC="+f.cfg.Secrets.Topic+"\nGCHAT_PUBSUB_SUBSCRIPTION="+f.cfg.Secrets.Subscription+"\nGCHAT_OAUTH_CLIENT_FILE="+f.cfg.Secrets.OAuthClientFile+"\nGCHAT_TOKEN_FILE="+f.cfg.Secrets.TokenFile+"\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL="+f.cfg.Secrets.GCAPIBase+"\n")
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	l := ledger.New()
	l.IdentityUserID = fakegoogle.UserID
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	f.startOptions(t, Options{SettingsPath: settings, EnvFile: env})
	f.health(t, "ready", 5*time.Second)
	writeFile(t, settings, "[defaults]\nwake = 'mentions'\n")
	waitFor(t, 5*time.Second, func() bool {
		resp, err := f.client.Get("http://adapter/spaces")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return strings.Contains(string(b), `"wake":"mentions"`)
	})
	waitFor(t, 5*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		registrations := 0
		for _, request := range f.requests {
			if strings.Contains(request, "/extmsg/adapters") {
				registrations++
			}
		}
		return registrations == 2
	})
}

func TestRevokedTokenReplacementResumes(t *testing.T) {
	f := newFixture(t, true)
	writeFile(t, f.cfg.Secrets.TokenFile, `{"access_token":"expired","refresh_token":"example-refresh","expiry":"2000-01-01T00:00:00Z"}`)
	f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = "invalid_grant" })
	f.start(t)
	f.health(t, "needs-sign-in", 5*time.Second)
	f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = "" })
	f.replaceToken(t)
	f.health(t, "ready", 5*time.Second)
}

func TestObservedIdentityReplacesContradictedLedgerIdentity(t *testing.T) {
	f := newFixture(t, true)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	l := ledger.New()
	l.IdentityUserID = "users/999"
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	resp, err := f.client.Post("http://adapter/publish", "application/json", strings.NewReader(`{"idempotency_key":"identity-example","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"default","conversation_id":"spaces/room","kind":"room"},"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"delivered":true`) {
		t.Fatalf("publish %s", b)
	}
	waitFor(t, 5*time.Second, func() bool {
		b, e := os.ReadFile(f.cfg.Endpoints.StateFile)
		if e != nil {
			return false
		}
		var saved ledger.Ledger
		return json.Unmarshal(b, &saved) == nil && saved.IdentityUserID == fakegoogle.UserID
	})
	f.health(t, "ready", 5*time.Second)
}

func TestTokenReloadDrainsPublishBeforeRebuilding(t *testing.T) {
	f := newFixture(t, true)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	f.intercept = func(r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	// Release before the fixture's cleanup waits for application shutdown.
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	client := *f.client
	client.Timeout = 10 * time.Second
	done := make(chan error, 1)
	go func() {
		resp, err := client.Post("http://adapter/publish", "application/json", strings.NewReader(`{"idempotency_key":"drain-example","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"default","conversation_id":"spaces/room","kind":"room"},"text":"hello"}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("publish did not start")
	}
	f.mu.Lock()
	before := len(f.requests)
	f.mu.Unlock()
	f.replaceToken(t)
	f.health(t, "initializing", 4*time.Second)
	time.Sleep(100 * time.Millisecond)
	f.mu.Lock()
	requests := append([]string(nil), f.requests[before:]...)
	f.mu.Unlock()
	for _, r := range requests {
		if strings.Contains(r, "subscriptions") {
			t.Fatalf("reconciled before send drained: %v", requests)
		}
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.health(t, "ready", 5*time.Second)
}

func TestQuarantineRecordIsDurableAndIdempotent(t *testing.T) {
	a := &application{cfg: &config.Config{Endpoints: config.Endpoints{StateFile: filepath.Join(t.TempDir(), "state.json")}}, l: ledger.New()}
	ev := &puller.Event{PubSubID: "example-id"}
	for range 2 {
		if err := a.quarantine(context.Background(), ev, "quarantine/example-id.json", errors.New("malformed event")); err != nil {
			t.Fatal(err)
		}
	}
	saved, err := ledger.Load(a.cfg.Endpoints.StateFile, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Quarantine) != 1 || saved.Quarantine[0].PubSubID != ev.PubSubID {
		t.Fatalf("quarantine: %+v", saved.Quarantine)
	}
	a.cfg.Endpoints.StateFile = filepath.Join(t.TempDir(), "missing", "state.json")
	err = a.quarantine(context.Background(), &puller.Event{PubSubID: "failed-id"}, "path", errors.New("malformed event"))
	if err == nil || len(a.l.Quarantine) != 1 {
		t.Fatalf("failed save became durable: err=%v records=%v", err, a.l.Quarantine)
	}
}

func TestCorruptLedgerServesStatusAndResetResumes(t *testing.T) {
	f := newFixture(t, true)
	writeFile(t, f.cfg.Endpoints.StateFile, "broken ledger")
	f.start(t)
	f.health(t, "recovery-required", time.Second)
	r, err := f.client.Get("http://adapter/status")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatal(r.Status)
	}
	data, err := os.ReadFile(f.cfg.Endpoints.StateFile)
	if err != nil || string(data) != "broken ledger" {
		t.Fatalf("corrupt ledger changed: %q %v", data, err)
	}
	r, err = f.client.Post("http://adapter/state", "application/json", strings.NewReader(`{"action":"reset"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		b, _ := io.ReadAll(r.Body)
		t.Fatalf("reset: %s %s", r.Status, b)
	}
	f.health(t, "ready", 5*time.Second)
	if !ledger.Established(f.cfg.SecretsDir) {
		t.Fatal("missing established marker")
	}
}

func (f *fixture) replaceIdentityToken(t *testing.T, id string) {
	t.Helper()
	writeFile(t, f.cfg.Secrets.TokenFile+".new", `{"access_token":"example","refresh_token":"example-refresh","token_type":"Bearer","expiry":"2099-01-01T00:00:00Z","identity_user_id":"`+id+`"}`)
	if err := os.Rename(f.cfg.Secrets.TokenFile+".new", f.cfg.Secrets.TokenFile); err != nil {
		t.Fatal(err)
	}
}

func TestFreshTokenIdentityDeliversFirstDM(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/123456")
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
	})
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	data := `{"message":{"name":"spaces/dm/messages/first","text":"hello","sender":{"name":"users/222","type":"HUMAN"},"createTime":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","thread":{"name":"spaces/dm/threads/first"}}}`
	_, err := f.pub.Publish(t.Context(), &pubsub.Message{Data: []byte(data), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": "first"}}).Get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests {
		if strings.Contains(request, "POST /v1/spaces/dm/messages") {
			t.Fatal("identity required an outbound send")
		}
	}
}

func TestTokenIdentityReplacementRebuildsOnce(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/123456")
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	f.replaceIdentityToken(t, "users/654321")
	waitFor(t, 5*time.Second, func() bool {
		data, _ := os.ReadFile(f.cfg.Endpoints.StateFile)
		var l ledger.Ledger
		return json.Unmarshal(data, &l) == nil && l.IdentityUserID == "users/654321"
	})
	f.health(t, "ready", 5*time.Second)
	f.mu.Lock()
	before := len(f.requests)
	f.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, request := range f.requests[before:] {
		if strings.Contains(request, "subscriptions") {
			t.Fatalf("identity rebuild loop: %v", f.requests[before:])
		}
	}
}

type logBuffer struct {
	mu   sync.Mutex
	text strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text.Write(p)
}
func (b *logBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.text.String() }

func TestLegacyTokenWarnsOnce(t *testing.T) {
	f := newFixture(t, true)
	var logs logBuffer
	f.startOptions(t, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	f.health(t, "ready", 5*time.Second)
	f.replaceToken(t)
	time.Sleep(2300 * time.Millisecond)
	if n := strings.Count(logs.String(), "legacy token has no identity"); n != 1 || !strings.Contains(logs.String(), "gc gchat auth") {
		t.Fatalf("expected one actionable warning: %s", logs.String())
	}
}
