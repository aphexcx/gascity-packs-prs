package token

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/config"
)

// fakeKeyholder speaks the key holder's socket protocol with scripted answers.
type fakeKeyholder struct {
	socket  string
	status  atomic.Int32 // 0: mint succeeds
	code    atomic.Value // error code for a failing mint
	mints   atomic.Int32
	subject string
	now     func() time.Time
}

func newFakeKeyholder(t *testing.T) *fakeKeyholder {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gchat-kh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	k := &fakeKeyholder{socket: filepath.Join(dir, "kh.sock"), subject: "agent1@example.com", now: time.Now}
	k.code.Store("")
	ln, err := net.Listen("unix", k.socket)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/token", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Scopes []string `json:"scopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		k.mints.Add(1)
		// A key holder mints exactly the scopes asked for, and only those its
		// delegation grant covers: openid is a consent scope it never has.
		for _, scope := range req.Scopes {
			if scope == "openid" {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "scope_not_allowed", "detail": "not configured for openid"})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if status := int(k.status.Load()); status != 0 {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": k.code.Load().(string), "detail": "scripted"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "kh-token-" + time.Now().Format("150405.000000"), "expires_at": k.now().Add(time.Hour).UnixMilli()})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"subject": k.subject, "scopes": Scopes})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return k
}

// fakeDirectory answers the People directory search for one address.
func fakeDirectory(t *testing.T, email, person string, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/people:searchDirectoryPeople" || r.Header.Get("Authorization") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"people": []map[string]any{
			{"resourceName": "people/999", "emailAddresses": []map[string]string{{"value": "other@example.com"}}},
			{"resourceName": person, "emailAddresses": []map[string]string{{"value": email}}},
		}})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// fakeChat answers the two Chat reads the membership fallback makes: a DM with
// a bot (as Google makes for a new user) whose human member is the subject.
func fakeChat(t *testing.T, email, userID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/spaces":
			_ = json.NewEncoder(w).Encode(map[string]any{"spaces": []map[string]string{{"name": "spaces/room", "spaceType": "SPACE"}, {"name": "spaces/dm", "spaceType": "DIRECT_MESSAGE"}}})
		case "/v1/spaces/dm/members":
			_ = json.NewEncoder(w).Encode(map[string]any{"memberships": []map[string]any{
				{"member": map[string]string{"name": "users/103074035611191657205", "type": "BOT"}},
				{"member": map[string]string{"name": userID, "type": "HUMAN", "email": email}},
			}})
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func managedSource(t *testing.T, k *fakeKeyholder, peopleBase string) *Source {
	t.Helper()
	s, err := NewKeyholder(k.socket, config.Endpoints{ChatAPIBase: "http://127.0.0.1:1", PeopleAPIBase: peopleBase}, KeyholderScopes)
	if err != nil {
		t.Fatal(err)
	}
	s.watchInterval = 20 * time.Millisecond
	return s
}

func TestKeyholderMintsAndLearnsIdentity(t *testing.T) {
	k := newFakeKeyholder(t)
	dir, calls := fakeDirectory(t, "Agent1@Example.com", "people/123456", http.StatusOK)
	s := managedSource(t, k, dir.URL)
	if !s.Managed() || s.State() != Ready {
		t.Fatalf("managed source should start ready: managed=%v state=%s", s.Managed(), s.State())
	}
	tok, err := s.TokenSource(context.Background()).Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken == "" || !tok.Valid() {
		t.Fatalf("token: %+v", tok)
	}
	if s.IdentityUserID() != "users/123456" {
		t.Fatalf("identity: %q", s.IdentityUserID())
	}
	// A valid cached token is reused: one mint, one directory lookup.
	if _, err := s.TokenSource(context.Background()).Token(); err != nil {
		t.Fatal(err)
	}
	if k.mints.Load() != 1 || calls.Load() != 1 {
		t.Fatalf("mints=%d directory calls=%d", k.mints.Load(), calls.Load())
	}
	// Invalidation mints again, and the known identity is not looked up twice.
	s.Invalidate()
	if _, err := s.TokenSource(context.Background()).Token(); err != nil {
		t.Fatal(err)
	}
	if k.mints.Load() != 2 || calls.Load() != 1 {
		t.Fatalf("after invalidate: mints=%d directory calls=%d", k.mints.Load(), calls.Load())
	}
}

func TestKeyholderIdentityFromDirectMessageMembership(t *testing.T) {
	k := newFakeKeyholder(t)
	dir, _ := fakeDirectory(t, "agent1@example.com", "people/123456", http.StatusForbidden)
	chat := fakeChat(t, "Agent1@Example.com", "users/111864201493147506516")
	s, err := NewKeyholder(k.socket, config.Endpoints{ChatAPIBase: chat.URL, PeopleAPIBase: dir.URL}, KeyholderScopes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TokenSource(context.Background()).Token(); err != nil {
		t.Fatal(err)
	}
	if s.IdentityUserID() != "users/111864201493147506516" {
		t.Fatalf("identity from memberships: %q", s.IdentityUserID())
	}
}

func TestKeyholderIdentityFailureKeepsToken(t *testing.T) {
	k := newFakeKeyholder(t)
	dir, _ := fakeDirectory(t, "agent1@example.com", "people/123456", http.StatusForbidden)
	s := managedSource(t, k, dir.URL)
	if _, err := s.TokenSource(context.Background()).Token(); err != nil {
		t.Fatal(err)
	}
	if s.IdentityUserID() != "" {
		t.Fatalf("identity should stay unknown without directory access, got %q", s.IdentityUserID())
	}
	if s.State() != Ready {
		t.Fatalf("state: %s", s.State())
	}
}

func TestKeyholderPermanentRefusalNeedsSignInAndRecovers(t *testing.T) {
	k := newFakeKeyholder(t)
	k.status.Store(http.StatusBadRequest)
	k.code.Store("delegation_not_authorized")
	s := managedSource(t, k, "")
	_, err := s.TokenSource(context.Background()).Token()
	if !errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("want ErrNeedsSignIn, got %v", err)
	}
	if s.State() != NeedsSignIn {
		t.Fatalf("state: %s", s.State())
	}
	// Nothing on disk can repair this; the watch keeps asking the key holder.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reloaded := make(chan struct{}, 1)
	go s.Watch(ctx, func() { reloaded <- struct{}{} })
	time.Sleep(60 * time.Millisecond)
	if s.State() != NeedsSignIn {
		t.Fatal("watch should not recover while the key holder refuses")
	}
	k.status.Store(0)
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not report the recovered credential")
	}
	if s.State() != Ready {
		t.Fatalf("state after recovery: %s", s.State())
	}
	if _, err := s.TokenSource(context.Background()).Token(); err != nil {
		t.Fatalf("token after recovery: %v", err)
	}
}

func TestKeyholderTransientFailureStaysReady(t *testing.T) {
	k := newFakeKeyholder(t)
	k.status.Store(http.StatusBadGateway)
	k.code.Store("endpoint_unavailable")
	s := managedSource(t, k, "")
	s.refreshDelay = time.Millisecond
	_, err := s.TokenSource(context.Background()).Token()
	if err == nil || errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("want a transient error, got %v", err)
	}
	if got := err.Error(); got != "key holder token failed: key holder: endpoint_unavailable (HTTP 502)" {
		t.Fatalf("error text: %s", got)
	}
	if s.State() != Ready {
		t.Fatalf("a transient refusal must not require sign-in: %s", s.State())
	}
	if k.mints.Load() != 3 {
		t.Fatalf("transient failures retry: mints=%d", k.mints.Load())
	}
	k.status.Store(0)
	if _, err := s.TokenSource(context.Background()).Token(); err != nil {
		t.Fatal(err)
	}
}

func TestKeyholderUnreachableSocket(t *testing.T) {
	s, err := NewKeyholder(filepath.Join(t.TempDir(), "missing.sock"), config.Endpoints{ChatAPIBase: "http://127.0.0.1:1"}, KeyholderScopes)
	if err != nil {
		t.Fatal(err)
	}
	s.refreshDelay = time.Millisecond
	_, err = s.TokenSource(context.Background()).Token()
	if err == nil || errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("want a transient error, got %v", err)
	}
	if _, err := NewKeyholder("", config.Endpoints{ChatAPIBase: "x"}, Scopes); err == nil {
		t.Fatal("empty socket accepted")
	}
}
