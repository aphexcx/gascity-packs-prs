package app

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// A key holder that mints for one subject, with a scripted refusal, and the
// directory that knows that subject's Chat id.
type fakeKeyholder struct {
	socket string
	status atomic.Int32
	code   atomic.Value
	mints  atomic.Int32
}

func newFakeKeyholder(t *testing.T) *fakeKeyholder {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gchat-kh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	k := &fakeKeyholder{socket: filepath.Join(dir, "kh.sock")}
	k.code.Store("")
	ln, err := net.Listen("unix", k.socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/token", func(w http.ResponseWriter, r *http.Request) {
		k.mints.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if status := int(k.status.Load()); status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": k.code.Load().(string), "detail": "scripted"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "example", "expires_at": time.Now().Add(time.Hour).UnixMilli()})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"subject": "agent1@example.com"})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return k
}

func fakeDirectory(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"people": []map[string]any{{"resourceName": "people/123456", "emailAddresses": []map[string]string{{"value": "agent1@example.com"}}}}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// managedFixture is the app fixture with the key holder as its token source:
// no client file, no token file, identity from the directory.
func managedFixture(t *testing.T) (*fixture, *fakeKeyholder) {
	t.Helper()
	f := newFixture(t, false)
	k := newFakeKeyholder(t)
	f.cfg.Secrets.TokenSource = config.TokenSourceKeyholder
	f.cfg.Secrets.KeyholderSocket = k.socket
	f.cfg.Secrets.OAuthClientFile = ""
	f.cfg.Endpoints.PeopleAPIBase = fakeDirectory(t).URL
	return f, k
}

func TestManagedSourceDeliversDM(t *testing.T) {
	f, k := managedFixture(t)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
	})
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	data := `{"message":{"name":"spaces/dm/messages/first","text":"hello","sender":{"name":"users/222","type":"HUMAN"},"createTime":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","thread":{"name":"spaces/dm/threads/first"}}}`
	if _, err := f.pub.Publish(t.Context(), &pubsub.Message{Data: []byte(data), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": "first"}}).Get(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	if got := f.gc.Inbound()[0].ProviderMessageID; got != "spaces/dm/messages/first" {
		t.Fatalf("delivered %q", got)
	}
	if k.mints.Load() == 0 {
		t.Fatal("the key holder was never asked for a token")
	}
	// Ready after an event proves the identity came from the directory: an
	// adapter that does not know its own user stays catching-up once events flow.
	f.health(t, "ready", 5*time.Second)
}

func TestManagedSourceLateIdentityReachesRunningClient(t *testing.T) {
	f, k := managedFixture(t)
	var available atomic.Bool
	var attempts atomic.Int32
	directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if !available.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"people":[{"resourceName":"people/123456","emailAddresses":[{"value":"agent1@example.com"}]}]}`))
	}))
	defer directory.Close()
	f.cfg.Endpoints.PeopleAPIBase = directory.URL
	f.startOptions(t, Options{KeyholderRetry: 50 * time.Millisecond})
	f.health(t, "ready", 5*time.Second)
	// A generation is already running with a valid token and no known identity.
	waitFor(t, 5*time.Second, func() bool { return attempts.Load() >= 2 })
	available.Store(true)
	waitFor(t, 5*time.Second, func() bool {
		b, _ := os.ReadFile(f.cfg.Endpoints.StateFile)
		var state struct {
			IdentityUserID string `json:"identity_user_id"`
		}
		return json.Unmarshal(b, &state) == nil && state.IdentityUserID == "users/123456"
	})
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
	})
	data := `{"message":{"name":"spaces/dm/messages/late-identity","text":"hello","sender":{"name":"users/222","type":"HUMAN"},"createTime":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","thread":{"name":"spaces/dm/threads/late-identity"}}}`
	if _, err := f.pub.Publish(t.Context(), &pubsub.Message{Data: []byte(data), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": "late-identity"}}).Get(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	f.health(t, "ready", 5*time.Second)
	if k.mints.Load() != 1 {
		t.Fatalf("identity recovery should reuse the valid credential; mints=%d", k.mints.Load())
	}
}

func TestManagedSourceChatRefusalRetriesWithoutSignIn(t *testing.T) {
	f, _ := managedFixture(t)
	var lists atomic.Int32
	chatURL, _ := url.Parse(f.cfg.Endpoints.ChatAPIBase)
	proxy := httputil.NewSingleHostReverseProxy(chatURL)
	refusing := atomic.Bool{}
	refusing.Store(true)
	chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/spaces" && refusing.Load() {
			lists.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer chatServer.Close()
	f.cfg.Endpoints.ChatAPIBase = chatServer.URL
	var notices atomic.Int32
	var kinds []string
	f.startOptions(t, Options{Notify: func(_ context.Context, subject, _ string) error {
		notices.Add(1)
		kinds = append(kinds, subject)
		return nil
	}})
	waitFor(t, 10*time.Second, func() bool { return notices.Load() == 1 })
	if kinds[0] != "gchat: chat-access" {
		t.Fatalf("notice kind: %v", kinds)
	}
	h := f.health(t, "initializing", time.Second)
	if !strings.Contains(h.Reason, "Chat refuses") {
		t.Fatalf("reason: %+v", h)
	}
	// It keeps asking Chat, and never parks on a sign-in nobody can do.
	waitFor(t, 10*time.Second, func() bool { return lists.Load() >= 3 })
	if cur := f.health(t, "initializing", time.Second); cur.State == "needs-sign-in" {
		t.Fatal("managed credential must never wait for gc gchat auth")
	}
	refusing.Store(false)
	f.health(t, "ready", 10*time.Second)
	if notices.Load() != 1 {
		t.Fatalf("notices=%d want exactly one", notices.Load())
	}
}

func TestManagedSourceKeyholderRefusalRecovers(t *testing.T) {
	f, k := managedFixture(t)
	k.status.Store(http.StatusBadRequest)
	k.code.Store("delegation_not_authorized")
	var notices atomic.Int32
	f.startOptions(t, Options{KeyholderRetry: 200 * time.Millisecond, Notify: func(context.Context, string, string) error { notices.Add(1); return nil }})
	f.health(t, "needs-sign-in", 5*time.Second)
	waitFor(t, time.Second, func() bool { return notices.Load() == 1 })
	k.status.Store(0)
	// The watch re-asks the key holder (every 30s in production) and the first
	// successful mint is a credential replacement: clients rebuild, work resumes.
	f.health(t, "ready", 10*time.Second)
	if notices.Load() != 1 {
		t.Fatalf("notices=%d want one", notices.Load())
	}
}

func TestDeletedSpaceDoesNotHoldInterruptedSend(t *testing.T) {
	f := newFixture(t, true)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/live"] = &api.Space{Name: "spaces/live", SpaceType: "SPACE"}
	})
	target, _ := url.Parse(f.cfg.Endpoints.ChatAPIBase)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var gone atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/spaces/gone/") {
			gone.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"space deleted"}}`))
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer srv.Close()
	f.cfg.Endpoints.ChatAPIBase = srv.URL
	// Model the city's service proxy so gc's outbound callback reaches the
	// running adapter over its private socket.
	gcTarget, _ := url.Parse(f.cfg.Secrets.GCAPIBase)
	gcProxy := httputil.NewSingleHostReverseProxy(gcTarget)
	adapterTarget, _ := url.Parse("http://adapter")
	adapterProxy := httputil.NewSingleHostReverseProxy(adapterTarget)
	adapterProxy.Transport = f.client.Transport
	prefix := "/v0/city/" + f.cfg.Secrets.CityName + "/svc/gchat"
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, prefix+"/") {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
			adapterProxy.ServeHTTP(w, r)
			return
		}
		gcProxy.ServeHTTP(w, r)
	}))
	defer gateway.Close()
	f.cfg.Secrets.GCAPIBase = gateway.URL
	l := ledger.New()
	l.IdentityUserID = fakegoogle.UserID
	l.Spaces["spaces/gone"] = &ledger.SpaceState{Kind: "room", MessageCursor: time.Now().Add(-time.Hour), EventCursor: time.Now().Add(-time.Hour)}
	p := wire.PublishRequest{SessionID: "example", IdempotencyKey: "interrupted", Text: "recover me", Conversation: wire.Conversation{ScopeID: f.cfg.Secrets.CityName, Provider: "gchat", AccountID: f.cfg.Identity().Name, ConversationID: "spaces/live", Kind: "room"}}
	f.gc.Bind(p.Conversation, p.SessionID)
	l.Sends[p.IdempotencyKey] = &ledger.SendState{Key: p.IdempotencyKey, Space: "spaces/live", Request: &p, RecordPending: true, Parts: []ledger.SendPart{{RequestID: "interrupted-1", Text: p.Text}}}
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	waitFor(t, 12*time.Second, func() bool {
		data, _ := os.ReadFile(f.cfg.Endpoints.StateFile)
		var saved ledger.Ledger
		if json.Unmarshal(data, &saved) != nil {
			return false
		}
		s := saved.Sends[p.IdempotencyKey]
		return saved.Spaces["spaces/gone"] == nil && s != nil && s.Recorded && !s.RecordPending && s.Receipt != nil && s.Receipt.Delivered
	})
	if gone.Load() != 1 {
		t.Fatalf("deleted-space history requests=%d, want one", gone.Load())
	}
}

// Google's all-spaces subscription announced the identity's membership in a
// new space but not the message posted there seconds later (seen live). The
// membership alone must make the adapter read that space's history from the
// join time and deliver the message.
func TestJoinedSpaceHistoryIsSweptWithoutMessageEvents(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	joined := time.Now().UTC().Add(-2 * time.Second)
	before := joined.Add(-30 * time.Second)
	after := joined.Add(time.Second)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/joined"] = &api.Space{Name: "spaces/joined", SpaceType: "SPACE"}
		g.Messages["spaces/joined/messages/old"] = &api.Message{Name: "spaces/joined/messages/old", Text: "before the agent joined", Sender: &api.User{Name: "users/222", Type: "HUMAN"}, CreateTime: before.Format(time.RFC3339Nano), Thread: &api.Thread{Name: "spaces/joined/threads/old"}}
		g.Messages["spaces/joined/messages/hey"] = &api.Message{Name: "spaces/joined/messages/hey", Text: "@agent hey", Sender: &api.User{Name: "users/222", Type: "HUMAN"}, CreateTime: after.Format(time.RFC3339Nano), Thread: &api.Thread{Name: "spaces/joined/threads/hey"}}
	})
	membership := `{"membership":{"name":"spaces/joined/members/123456","state":"JOINED","createTime":"` + joined.Format(time.RFC3339Nano) + `","member":{"name":"` + fakegoogle.UserID + `","type":"HUMAN"}}}`
	if _, err := f.pub.Publish(t.Context(), &pubsub.Message{Data: []byte(membership), Attributes: map[string]string{"ce-type": "google.workspace.chat.membership.v1.created", "ce-id": "joined", "ce-subject": "//chat.googleapis.com/spaces/joined"}}).Get(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	if got := f.gc.Inbound()[0]; got.ProviderMessageID != "spaces/joined/messages/hey" || !strings.Contains(got.Text, "[catch-up]") {
		t.Fatalf("delivered %+v", got)
	}
	f.health(t, "ready", 10*time.Second)
	if len(f.gc.Inbound()) != 1 {
		t.Fatalf("history before the join must stay out: %+v", f.gc.Inbound())
	}
}
