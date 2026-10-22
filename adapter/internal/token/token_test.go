package token

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gchat-adapter/internal/config"
	"golang.org/x/oauth2"
)

var testScopes = []string{"chat.messages", "chat.spaces.readonly", "chat.memberships.readonly", "pubsub"}

func fixture(t *testing.T, handler http.HandlerFunc) (string, string, config.Endpoints) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	clientFile := filepath.Join(dir, "client.json")
	writeJSON(t, clientFile, map[string]any{"installed": map[string]any{
		"client_id": "example", "client_secret": "example-secret",
		"auth_uri": server.URL + "/authorize", "token_uri": server.URL + "/token",
	}})
	return clientFile, filepath.Join(dir, "token.json"), config.Endpoints{
		OAuthAuthURL: server.URL + "/authorize", OAuthTokenURL: server.URL + "/token", ChatAPIBase: server.URL,
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func savedToken(access string) *oauth2.Token {
	return &oauth2.Token{AccessToken: access, RefreshToken: "example-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
}

func mustSource(t *testing.T, clientFile, tokenFile string, ep config.Endpoints) *Source {
	t.Helper()
	s, err := New(clientFile, tokenFile, ep, testScopes)
	if err != nil {
		t.Fatal(err)
	}
	s.refreshDelay = time.Millisecond
	return s
}

func TestRefresh(t *testing.T) {
	var refreshes atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "example-refresh" || r.Form.Get("client_id") != "example" {
				t.Errorf("unexpected refresh form: %v", r.Form)
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{}`)
	})
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	s := mustSource(t, clientFile, tokenFile, ep)
	client, err := s.Client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res, err := client.Get(ep.ChatAPIBase + "/resource")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "fresh" || tok.RefreshToken != "example-refresh" || s.State() != Ready || refreshes.Load() != 1 {
		t.Fatalf("refresh: token=%+v error=%v state=%v calls=%d", tok, err, s.State(), refreshes.Load())
	}
	// Callers must not be able to mutate the shared token.
	tok.AccessToken = "changed by caller"
	tok, err = s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "fresh" {
		t.Fatalf("token leaked mutable state: %+v, %v", tok, err)
	}
}

func TestPermanentOAuthErrorsNeedSignIn(t *testing.T) {
	for _, code := range []string{
		"invalid_grant", "invalid_client", "unauthorized_client", "invalid_request",
		"invalid_scope", "unsupported_grant_type", "access_denied", "admin_policy_enforced",
		"invalid_rapt", "disabled_client", "deleted_client", "org_internal", "restricted_client",
	} {
		t.Run(code, func(t *testing.T) {
			var requests atomic.Int32
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" {
					fmt.Fprint(w, `{"spaces":[]}`)
					return
				}
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q,"error_description":"token-secret","error_uri":"token-secret"}`, code)
			})
			expired := savedToken("expired")
			expired.Expiry = time.Now().Add(-time.Hour)
			writeJSON(t, tokenFile, expired)
			s := mustSource(t, clientFile, tokenFile, ep)
			for range 3 {
				if _, err := s.Client(t.Context()); !errors.Is(err, ErrNeedsSignIn) {
					t.Errorf("Client error = %v", err)
				} else if strings.Contains(err.Error(), "token-secret") {
					t.Errorf("Client error contains credential response text: %v", err)
				}
				if _, err := s.TokenSource(t.Context()).Token(); !errors.Is(err, ErrNeedsSignIn) {
					t.Errorf("Token error = %v", err)
				}
			}
			if s.State() != NeedsSignIn || requests.Load() != 1 {
				t.Fatalf("permanent failure: state=%v requests=%d", s.State(), requests.Load())
			}
			if code == "admin_policy_enforced" {
				writeJSON(t, tokenFile+".new", savedToken("repaired"))
				if err := os.Rename(tokenFile+".new", tokenFile); err != nil {
					t.Fatal(err)
				}
				reloads := 0
				s.reload(t.Context(), func() { reloads++ })
				tok, err := s.TokenSource(t.Context()).Token()
				if err != nil || tok.AccessToken != "repaired" || s.State() != Ready || reloads != 1 || requests.Load() != 1 {
					t.Fatalf("policy recovery: token=%+v error=%v state=%v reloads=%d requests=%d", tok, err, s.State(), reloads, requests.Load())
				}
			}
		})
	}
}

func TestRefreshTransientRetry(t *testing.T) {
	var requests atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if requests.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":"temporarily_unavailable"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`)
	})
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	s := mustSource(t, clientFile, tokenFile, ep)
	if _, err := s.Client(t.Context()); err != nil || requests.Load() != 3 || s.State() != Ready {
		t.Fatalf("retry: error=%v calls=%d state=%v", err, requests.Load(), s.State())
	}
}

func TestClientUnauthorizedRefreshesRevokedToken(t *testing.T) {
	var refreshes, requests atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":"invalid_grant"}`)
			return
		}
		requests.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, "revoked")
	})
	writeJSON(t, tokenFile, savedToken("revoked"))
	s := mustSource(t, clientFile, tokenFile, ep)
	client, err := s.Client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Get(ep.ChatAPIBase + "/resource")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusUnauthorized || string(body) != "revoked" || refreshes.Load() != 0 || requests.Load() != 1 {
		t.Fatalf("401 was altered or retried: response=%v body=%q err=%v refreshes=%d requests=%d", res, body, err, refreshes.Load(), requests.Load())
	}
	res, err = client.Get(ep.ChatAPIBase + "/resource")
	if res != nil {
		res.Body.Close()
	}
	if !errors.Is(err, ErrNeedsSignIn) || s.State() != NeedsSignIn || refreshes.Load() != 1 || requests.Load() != 1 {
		t.Fatalf("revocation: err=%v state=%v refreshes=%d requests=%d", err, s.State(), refreshes.Load(), requests.Load())
	}
}

func TestClientStaleUnauthorizedKeepsRefreshedToken(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	var refreshes atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer old" {
			t.Errorf("stale request authorization = %q", r.Header.Get("Authorization"))
		}
		close(started)
		<-release
		w.WriteHeader(http.StatusUnauthorized)
	})
	writeJSON(t, tokenFile, savedToken("old"))
	s := mustSource(t, clientFile, tokenFile, ep)
	client, err := s.Client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		res, err := client.Get(ep.ChatAPIBase + "/resource")
		if res != nil {
			res.Body.Close()
		}
		done <- err
	}()
	waitSignal(t, started)
	s.mu.Lock()
	expired := *s.current
	expired.Expiry = time.Now().Add(-time.Hour)
	s.current = &expired
	s.mu.Unlock()
	if _, err := s.TokenSource(t.Context()).Token(); err != nil {
		t.Fatal(err)
	}
	finish()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "fresh" || !tok.Valid() || refreshes.Load() != 1 || s.State() != Ready {
		t.Fatalf("stale 401 invalidated replacement: token=%+v err=%v refreshes=%d state=%v", tok, err, refreshes.Load(), s.State())
	}
}

func TestSourceInvalidate(t *testing.T) {
	var refreshes atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
	})
	s := mustSource(t, clientFile, tokenFile, ep)
	s.Invalidate() // Missing credentials remain missing.
	if s.State() != Missing {
		t.Fatalf("missing credential state = %v", s.State())
	}
	writeJSON(t, tokenFile, savedToken("cached"))
	s = mustSource(t, clientFile, tokenFile, ep)
	s.Invalidate()
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "fresh" || refreshes.Load() != 1 || s.State() != Ready {
		t.Fatalf("invalidation: token=%+v err=%v refreshes=%d state=%v", tok, err, refreshes.Load(), s.State())
	}
}

func TestClientRequestBoundsRefresh(t *testing.T) {
	for _, scenario := range []string{"stalled headers", "stalled body", "deadline before refresh"} {
		t.Run(scenario, func(t *testing.T) {
			started := make(chan struct{})
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(started)
				if scenario == "stalled body" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			})
			writeJSON(t, tokenFile, savedToken("cached"))
			s := mustSource(t, clientFile, tokenFile, ep)
			construction, stop := context.WithCancel(t.Context())
			defer stop()
			client, err := s.Client(construction)
			if err != nil {
				t.Fatal(err)
			}
			s.current.Expiry = time.Now().Add(-time.Hour)
			timeout := 100 * time.Millisecond
			if scenario == "deadline before refresh" {
				timeout = -time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep.ChatAPIBase+"/resource", nil)
			done := make(chan error, 1)
			start := time.Now()
			go func() { _, err := client.Do(req); done <- err }()
			timer := time.NewTimer(500 * time.Millisecond)
			defer timer.Stop()
			var requestErr error
			completed := false
			select {
			case <-started:
			case requestErr = <-done:
				completed = true // The deadline can expire before the handler starts.
			case <-timer.C:
				cancel()
				<-done
				t.Fatal("request deadline did not interrupt refresh")
			}
			if !completed {
				select {
				case requestErr = <-done:
				case <-timer.C:
					cancel()
					<-done
					t.Fatal("request deadline did not interrupt refresh")
				}
			}
			if !errors.Is(requestErr, context.DeadlineExceeded) || time.Since(start) > 500*time.Millisecond {
				t.Fatalf("request deadline: elapsed=%v error=%v", time.Since(start), requestErr)
			}
		})
	}
}

type waitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestRefreshWaiterCanCancel(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
	})
	defer close(release)
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	s := mustSource(t, clientFile, tokenFile, ep)
	first := make(chan error, 1)
	go func() { _, err := s.TokenSource(t.Context()).Token(); first <- err }()
	waitSignal(t, started)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	waiter := &waitContext{Context: ctx, waiting: make(chan struct{})}
	second := make(chan error, 1)
	go func() { _, err := s.TokenSource(waiter).Token(); second <- err }()
	waitSignal(t, waiter.waiting)
	cancel()
	select {
	case err := <-second:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter cancellation = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled waiter is blocked by another refresh")
	}
	select {
	case err := <-first:
		t.Fatalf("waiter cancellation interrupted the first refresh: %v", err)
	default:
	}
	release <- struct{}{}
	if err := <-first; err != nil || calls.Load() != 1 {
		t.Fatalf("first refresh: error=%v requests=%d", err, calls.Load())
	}
}

func TestTokenCancellationDuringRefresh(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		cancel()
		<-r.Context().Done()
	})
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	s := mustSource(t, clientFile, tokenFile, ep)
	if _, err := s.TokenSource(ctx).Token(); !errors.Is(err, context.Canceled) {
		t.Fatalf("refresh cancellation = %v", err)
	}
}

func TestClientOutlivesConstructionContext(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cached" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
	})
	writeJSON(t, tokenFile, savedToken("cached"))
	s := mustSource(t, clientFile, tokenFile, ep)
	ctx, cancel := context.WithCancel(t.Context())
	client, err := s.Client(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.ChatAPIBase, nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if req.Header.Get("Authorization") != "" {
		t.Fatal("transport modified the caller's headers")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type closeTrackingBody struct {
	io.Reader
	closed bool
}

func (b *closeTrackingBody) Close() error { b.closed = true; return nil }

func TestTokenFailureClosesRequestBody(t *testing.T) {
	body := &closeTrackingBody{Reader: strings.NewReader("payload")}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://example/resource", body)
	transport := &tokenTransport{
		token: func(context.Context) (*oauth2.Token, error) { return nil, ErrNeedsSignIn },
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Error("request sent without a token")
			return nil, errors.New("unexpected request")
		}),
	}
	res, err := transport.RoundTrip(req)
	if res != nil || !errors.Is(err, ErrNeedsSignIn) || !body.closed {
		t.Fatalf("token failure: response=%v err=%v bodyClosed=%v", res, err, body.closed)
	}
}

func TestClientRequestRefreshUsesOAuthHTTPClient(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request bypassed the custom HTTP client")
		w.WriteHeader(http.StatusBadRequest)
	})
	writeJSON(t, tokenFile, savedToken("cached"))
	s := mustSource(t, clientFile, tokenFile, ep)
	refreshes, requests := 0, 0
	custom := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Body != nil {
			defer req.Body.Close()
		}
		body := `{}`
		if req.URL.Path == "/token" {
			refreshes++
			body = `{"access_token":"fresh","expires_in":3600}`
		} else {
			requests++
			if req.Header.Get("Authorization") != "Bearer fresh" {
				t.Errorf("request token = %q", req.Header.Get("Authorization"))
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), oauth2.HTTPClient, custom))
	client, err := s.Client(ctx)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	s.Invalidate()
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.ChatAPIBase+"/resource", nil)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if refreshes != 1 || requests != 1 {
		t.Fatalf("custom transport: refreshes=%d requests=%d", refreshes, requests)
	}
}

func TestRunAuthExchangeContextSentinels(t *testing.T) {
	for _, sentinel := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("exchange bypassed the custom HTTP client")
			})
			custom := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				req.Body.Close()
				return nil, sentinel
			})}
			ctx := context.WithValue(t.Context(), oauth2.HTTPClient, custom)
			err := RunAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard,
				func(raw string) error { return browserCallback(raw, "example-code", "", "") })
			if !errors.Is(err, sentinel) || ctx.Err() != nil {
				t.Fatalf("exchange sentinel: err=%v ctx.Err=%v", err, ctx.Err())
			}
		})
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

func TestRefreshRestoresContextErrorAfterLastBodyFailure(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("refresh bypassed the custom HTTP client")
	})
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	s := mustSource(t, clientFile, tokenFile, ep)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	custom := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.Body.Close()
		calls++
		status := http.StatusServiceUnavailable
		body := io.NopCloser(strings.NewReader(`{"error":"temporarily_unavailable"}`))
		if calls == 3 {
			status = http.StatusOK
			body = io.NopCloser(readerFunc(func([]byte) (int, error) {
				cancel()
				return 0, context.Canceled
			}))
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: body}, nil
	})}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, custom)
	_, err := s.TokenSource(ctx).Token()
	if !errors.Is(err, context.Canceled) || calls != 3 {
		t.Fatalf("last body failure: err=%v calls=%d", err, calls)
	}
}

func TestConcurrentClientsShareOneRefresh(t *testing.T) {
	var calls atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"fresh","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
	})
	writeJSON(t, tokenFile, savedToken("cached"))
	s := mustSource(t, clientFile, tokenFile, ep)
	client, err := s.Client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.current.Expiry = time.Now().Add(-time.Hour)
	start, done := make(chan struct{}), make(chan error, 32)
	for range 32 {
		go func() {
			<-start
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.ChatAPIBase, nil)
			res, err := client.Do(req)
			if err == nil {
				res.Body.Close()
			}
			done <- err
		}()
	}
	close(start)
	for range 32 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", calls.Load())
	}
}

func TestRefreshErrorsDoNotExposeResponseBodies(t *testing.T) {
	for _, operation := range []string{"Token", "Client"} {
		t.Run(operation, func(t *testing.T) {
			var calls atomic.Int32
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"error_description":"response-body-secret","access_token":"token-secret"}`)
			})
			expired := savedToken("expired")
			expired.Expiry = time.Now().Add(-time.Hour)
			writeJSON(t, tokenFile, expired)
			s := mustSource(t, clientFile, tokenFile, ep)
			for range 2 {
				var err error
				if operation == "Client" {
					_, err = s.Client(t.Context())
				} else {
					_, err = s.TokenSource(t.Context()).Token()
				}
				if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "error_description") {
					t.Errorf("unsafe refresh error: %v", err)
				}
				if s.State() != Ready || errors.Is(err, ErrNeedsSignIn) {
					t.Fatalf("transient refresh changed state: %v, %v", s.State(), err)
				}
			}
			if calls.Load() != 6 {
				t.Fatalf("transient refresh retries = %d, want 6", calls.Load())
			}
		})
	}
}

func startWatch(t *testing.T, s *Source) <-chan struct{} {
	t.Helper()
	s.watchInterval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done, reloaded := make(chan struct{}), make(chan struct{}, 10)
	go func() {
		defer close(done)
		s.Watch(ctx, func() { reloaded <- struct{}{} })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("watch did not stop")
		}
	})
	return reloaded
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for watch")
	}
}

func TestWatchAtomicReplacementAndCreation(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%v", missing), func(t *testing.T) {
			var validations atomic.Int32
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/spaces" || r.URL.Query().Get("pageSize") != "1" || r.Header.Get("Authorization") != "Bearer replacement" {
					t.Errorf("unexpected validation request: %s %s %q", r.Method, r.URL, r.Header.Get("Authorization"))
				}
				validations.Add(1)
				fmt.Fprint(w, `{"spaces":[]}`)
			})
			original := savedToken("original000")
			replacementToken := savedToken("replacement")
			replacementToken.Expiry = original.Expiry
			if !missing {
				writeJSON(t, tokenFile, original)
			}
			s := mustSource(t, clientFile, tokenFile, ep)
			if missing && s.State() != Missing {
				t.Fatalf("missing state = %v", s.State())
			}
			reloaded := startWatch(t, s)
			replacement := tokenFile + ".new"
			writeJSON(t, replacement, replacementToken)
			if info, err := os.Stat(tokenFile); err == nil {
				// A rename must be detected even if the writer preserves mtime.
				if err := os.Chtimes(replacement, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Rename(replacement, tokenFile); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, reloaded)
			tok, err := s.TokenSource(t.Context()).Token()
			if err != nil || tok.AccessToken != "replacement" || s.State() != Ready {
				t.Fatalf("replacement: token=%+v error=%v state=%v", tok, err, s.State())
			}
			select {
			case <-reloaded:
				t.Fatal("duplicate reload")
			case <-time.After(50 * time.Millisecond):
			}
			if validations.Load() != 1 {
				t.Fatalf("validation calls = %d", validations.Load())
			}
		})
	}
}

func TestWatchInvalidReplacementKeepsOldToken(t *testing.T) {
	var validations atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		validations.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	writeJSON(t, tokenFile, savedToken("original"))
	s := mustSource(t, clientFile, tokenFile, ep)
	writeJSON(t, tokenFile+".new", savedToken("invalid"))
	if err := os.Rename(tokenFile+".new", tokenFile); err != nil {
		t.Fatal(err)
	}
	reload := func() { t.Error("invalid replacement was installed") }
	s.reload(t.Context(), reload)
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "original" || s.State() != Ready || validations.Load() != 1 {
		t.Fatalf("lost original: token=%+v error=%v state=%v validations=%d", tok, err, s.State(), validations.Load())
	}
	if err := os.WriteFile(tokenFile, []byte(`{invalid`), 0600); err != nil {
		t.Fatal(err)
	}
	s.reload(t.Context(), reload)
	tok, err = s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "original" || validations.Load() != 1 {
		t.Fatalf("malformed replacement lost original: %+v, %v", tok, err)
	}
}

func TestRunAuth(t *testing.T) {
	var authQuery url.Values
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" || r.Method != http.MethodPost {
			t.Errorf("unexpected token request: %s %s", r.Method, r.URL)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != authQuery.Get("code_challenge") || r.Form.Get("code_verifier") == "" {
			t.Error("PKCE verifier does not match challenge")
		}
		if r.Form.Get("code") != "example-code" || r.Form.Get("redirect_uri") != authQuery.Get("redirect_uri") || r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("unexpected exchange: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"signed-in","refresh_token":"new-refresh","expires_in":3600,"scope":%q,"id_token":"e30.eyJzdWIiOiIxMjM0NTYifQ.signature"}`, strings.Join(testScopes, " "))
	})
	writeJSON(t, clientFile, map[string]any{"installed": map[string]any{
		"client_id": "example", "client_secret": "example-secret",
		"auth_uri": "unsupported://example/authorize", "token_uri": "unsupported://example/token",
	}})
	writeJSON(t, tokenFile, savedToken("original"))
	var stdout bytes.Buffer
	err := RunAuth(t.Context(), clientFile, tokenFile, ep, testScopes, &stdout, func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil {
			return err
		}
		authQuery = u.Query()
		if !strings.HasPrefix(raw, ep.OAuthAuthURL) || authQuery.Get("scope") != strings.Join(testScopes, " ") || authQuery.Get("state") == "" || authQuery.Get("code_challenge_method") != "S256" || authQuery.Get("access_type") != "offline" || authQuery.Get("prompt") != "consent" {
			t.Errorf("unexpected authorization URL: %s", raw)
		}
		redirect, err := url.Parse(authQuery.Get("redirect_uri"))
		if err != nil {
			return err
		}
		if redirect.Hostname() != "127.0.0.1" || redirect.Port() == "" || redirect.Port() == "0" {
			t.Errorf("redirect is not an allocated loopback port: %s", redirect)
		}
		if err := browserCallback(raw, "wrong-code", "", "wrong-state"); err != nil {
			return err
		}
		return browserCallback(raw, "example-code", "", "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), ep.OAuthAuthURL) {
		t.Fatalf("URL not printed: %s", stdout.String())
	}
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil || tok.AccessToken != "signed-in" || tok.RefreshToken != "new-refresh" || !tok.Valid() {
		t.Fatalf("saved token = %+v error=%v", tok, err)
	}
	info, err := os.Stat(tokenFile)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode: info=%v error=%v", info, err)
	}
	assertNoAuthTemps(t, tokenFile)
}

func browserCallback(raw, code, denied, stateOverride string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	redirect, err := url.Parse(u.Query().Get("redirect_uri"))
	if err != nil {
		return err
	}
	state := u.Query().Get("state")
	if stateOverride != "" {
		state = stateOverride
	}
	redirect.RawQuery = url.Values{"code": {code}, "state": {state}, "error": {denied}}.Encode()
	client := &http.Client{Timeout: 3 * time.Second}
	res, err := client.Get(redirect.String())
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, res.Body)
	return err
}

type delayedWriteListener struct {
	net.Listener
	started chan struct{}
	release <-chan struct{}
	closed  chan struct{}
}

func (l *delayedWriteListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &delayedWriteConn{Conn: conn, listener: l}, nil
}

type delayedWriteConn struct {
	net.Conn
	listener *delayedWriteListener
	write    sync.Once
	close    sync.Once
}

func (c *delayedWriteConn) Write(p []byte) (int, error) {
	c.write.Do(func() { close(c.listener.started) })
	<-c.listener.release
	return c.Conn.Write(p)
}

func (c *delayedWriteConn) Close() error {
	c.close.Do(func() { close(c.listener.closed) })
	return c.Conn.Close()
}

func TestRunAuthFinishesCallbackPageBeforeShutdown(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("denied consent reached token exchange")
	})
	started, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	listen := func(ctx context.Context, network, address string) (net.Listener, error) {
		listener, err := (&net.ListenConfig{}).Listen(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &delayedWriteListener{Listener: listener, started: started, release: release, closed: closed}, nil
	}
	page := make(chan error, 1)
	auth := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	go func() {
		auth <- runAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard, func(raw string) error {
			go func() {
				u, _ := url.Parse(raw)
				redirect, _ := url.Parse(u.Query().Get("redirect_uri"))
				redirect.RawQuery = url.Values{"state": {u.Query().Get("state")}, "error": {"access_denied"}}.Encode()
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, redirect.String(), nil)
				res, err := http.DefaultClient.Do(req)
				if err == nil {
					var body []byte
					body, err = io.ReadAll(res.Body)
					res.Body.Close()
					if err == nil && string(body) != "Consent received. You can close this window and return to the terminal.\n" {
						err = fmt.Errorf("callback page = %q", body)
					}
				}
				page <- err
			}()
			return nil
		}, listen)
	}()
	waitSignal(t, started)
	select {
	case <-closed:
		t.Error("callback connection closed before its page was written")
	case <-time.After(50 * time.Millisecond):
	}
	finish()
	if err := <-page; err != nil {
		t.Errorf("callback page failed: %v", err)
	}
	if err := <-auth; err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Errorf("auth denial = %v", err)
	}
}

func TestRunAuthFailuresPreserveToken(t *testing.T) {
	for _, scenario := range []string{"partial scopes", "missing scope field", "denied", "cancel", "cancel during exchange", "write failure", "no refresh token"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var exchanges atomic.Int32
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				if scenario == "cancel during exchange" {
					// The server starts watching for a disconnect after the request
					// body is consumed. Drain it before waiting on cancellation.
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Error(err)
					}
					cancel()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				response := map[string]any{"access_token": "signed-in", "refresh_token": "new-refresh", "expires_in": 3600, "scope": strings.Join(testScopes, " "), "id_token": "e30.eyJzdWIiOiIxMjM0NTYifQ.signature"}
				if scenario == "partial scopes" {
					response["scope"] = testScopes[0]
				}
				if scenario == "missing scope field" {
					delete(response, "scope")
				}
				if scenario == "no refresh token" {
					delete(response, "refresh_token")
				}
				if err := json.NewEncoder(w).Encode(response); err != nil {
					t.Error(err)
				}
			})
			writeJSON(t, tokenFile, savedToken("original"))
			before, _ := os.ReadFile(tokenFile)
			if scenario == "write failure" {
				if os.Geteuid() == 0 {
					t.Skip("root bypasses the directory permission failure")
				}
				if err := os.Chmod(filepath.Dir(tokenFile), 0500); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(filepath.Dir(tokenFile), 0700) })
			}
			err := RunAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard, func(raw string) error {
				if scenario == "cancel" {
					cancel()
					return nil
				}
				if scenario == "denied" {
					return browserCallback(raw, "", "access_denied", "")
				}
				return browserCallback(raw, "example-code", "", "")
			})
			if err == nil {
				t.Fatal("auth unexpectedly succeeded")
			}
			if strings.Contains(scenario, "scopes") && (!strings.Contains(err.Error(), "chat.spaces.readonly") || !strings.Contains(err.Error(), "pubsub")) {
				t.Fatalf("missing scopes not named: %v", err)
			}
			if strings.HasPrefix(scenario, "cancel") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel error = %v", err)
			}
			if scenario == "denied" && (!strings.Contains(err.Error(), "access_denied") || exchanges.Load() != 0) {
				t.Fatalf("denial: error=%v exchanges=%d", err, exchanges.Load())
			}
			if scenario == "write failure" && !strings.Contains(err.Error(), tokenFile) {
				t.Fatalf("write error lacks path: %v", err)
			}
			after, readErr := os.ReadFile(tokenFile)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("old token changed: %v", readErr)
			}
			assertNoAuthTemps(t, tokenFile)
		})
	}
}

func TestRunAuthCancelCreatesNothing(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected exchange") })
	ctx, cancel := context.WithCancel(t.Context())
	err := RunAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard, func(string) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel created %s: %v", tokenFile, err)
	}
	assertNoAuthTemps(t, tokenFile)
}

func TestLoopbackBindFailureIsActionable(t *testing.T) {
	listener, err := listenLoopback(t.Context(), func(ctx context.Context, network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:0" {
			t.Fatalf("listen address = %s %s", network, address)
		}
		return nil, syscall.EADDRNOTAVAIL
	})
	if listener != nil || !errors.Is(err, syscall.EADDRNOTAVAIL) {
		t.Fatalf("bind result: listener=%v err=%v", listener, err)
	}
	for _, hint := range []string{"127.0.0.1:0", "loopback interface", "ephemeral ports", "firewall"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("bind error %q lacks %q", err, hint)
		}
	}
	if strings.Contains(err.Error(), "another port") {
		t.Errorf("bind error suggests an unavailable option: %v", err)
	}
}

func TestRunAuthExchangeErrorSanitized(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   string
	}{
		{500, "", "OAuth unrecognized (HTTP 500)"},
		{400, "admin_policy_enforced", "OAuth admin_policy_enforced (HTTP 400)"},
		{400, "response-body-secret", "OAuth unrecognized (HTTP 400)"},
	} {
		t.Run(tc.want+tc.code, func(t *testing.T) {
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":%q,"error_description":"response-body-secret","access_token":"token-secret"}`, tc.code)
			})
			err := RunAuth(t.Context(), clientFile, tokenFile, ep, testScopes, io.Discard,
				func(raw string) error { return browserCallback(raw, "example-code", "", "") })
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("exchange error = %v, want sanitized %q", err, tc.want)
			}
			if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed exchange wrote token: %v", err)
			}
		})
	}
}

func TestRunAuthExchangeBodyDeadline(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := RunAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard,
		func(raw string) error { return browserCallback(raw, "example-code", "", "") })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange body deadline = %v", err)
	}
}

func TestRunAuthPlainHTTPBehindHTTPS(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("TLS request unexpectedly reached the HTTP handler")
	})
	ep.OAuthTokenURL = strings.Replace(ep.OAuthTokenURL, "http://", "https://", 1)
	err := RunAuth(t.Context(), clientFile, tokenFile, ep, testScopes, io.Discard,
		func(raw string) error { return browserCallback(raw, "example-code", "", "") })
	if err == nil || !strings.Contains(err.Error(), "plain HTTP server behind an https URL") {
		t.Fatalf("exchange error = %v", err)
	}
}

func TestWatchRetriesTransientValidation(t *testing.T) {
	var calls atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"spaces":[]}`)
	})
	s := mustSource(t, clientFile, tokenFile, ep)
	reloaded := startWatch(t, s)
	writeJSON(t, tokenFile, savedToken("replacement"))
	waitSignal(t, reloaded)
	if s.State() != Ready || calls.Load() != 2 {
		t.Fatalf("transient validation prevented recovery: state=%v calls=%d", s.State(), calls.Load())
	}
}

func TestWatchCancellationDuringValidationDoesNotWarn(t *testing.T) {
	started := make(chan struct{})
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})
	writeJSON(t, tokenFile, savedToken("original"))
	s := mustSource(t, clientFile, tokenFile, ep)
	writeJSON(t, tokenFile+".new", savedToken("replacement"))
	if err := os.Rename(tokenFile+".new", tokenFile); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.Watch(ctx, func() { t.Error("canceled validation installed a token") })
	}()
	defer func() { cancel(); waitSignal(t, done) }()
	waitSignal(t, started)
	cancel()
	waitSignal(t, done)
	if logs.Len() != 0 {
		t.Fatalf("shutdown logged credential rejection: %s", logs.String())
	}
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "original" || s.State() != Ready {
		t.Fatalf("shutdown changed credential: token=%+v err=%v state=%v", tok, err, s.State())
	}
}

func TestWatchRecoveryWinsOverOldRefresh(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoked=%v", revoked), func(t *testing.T) {
			refreshStarted, finishRefresh := make(chan struct{}), make(chan struct{})
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					close(refreshStarted)
					select {
					case <-finishRefresh:
					case <-r.Context().Done():
						return
					}
					w.Header().Set("Content-Type", "application/json")
					if revoked {
						w.WriteHeader(http.StatusBadRequest)
						fmt.Fprint(w, `{"error":"invalid_grant"}`)
					} else {
						fmt.Fprint(w, `{"access_token":"old-refreshed","expires_in":3600}`)
					}
					return
				}
				fmt.Fprint(w, `{"spaces":[]}`)
			})
			// This cleanup also releases the server if the test fails early.
			defer close(finishRefresh)
			expired := savedToken("expired")
			expired.Expiry = time.Now().Add(-time.Hour)
			writeJSON(t, tokenFile, savedToken("cached"))
			s := mustSource(t, clientFile, tokenFile, ep)
			client, err := s.Client(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s.current = expired
			completed := make(chan error, 1)
			go func() {
				tok, err := s.TokenSource(t.Context()).Token()
				if err == nil && tok.AccessToken != "replacement" {
					err = fmt.Errorf("old refresh replaced new credential: %s", tok.AccessToken)
				}
				completed <- err
			}()
			waitSignal(t, refreshStarted)
			reloaded := startWatch(t, s)
			writeJSON(t, tokenFile+".new", savedToken("replacement"))
			if err := os.Rename(tokenFile+".new", tokenFile); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, reloaded)
			// A request can use the validated replacement even while the old
			// credential's refresh still holds the refresh gate.
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ep.ChatAPIBase+"/resource", nil)
			res, err := client.Do(req)
			if err != nil {
				t.Fatalf("valid cached token waited for stalled refresh: %v", err)
			}
			res.Body.Close()
			finishRefresh <- struct{}{}
			select {
			case err := <-completed:
				if err != nil || s.State() != Ready {
					t.Fatalf("recovery: error=%v state=%v", err, s.State())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("refresh did not finish")
			}
		})
	}
}

func TestWatchRetriesInterruptedOAuthResponse(t *testing.T) {
	var interrupted atomic.Bool
	interrupted.Store(true)
	var refreshes, validations atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			refreshes.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if interrupted.Load() {
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				fmt.Fprint(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 1000\r\n\r\n{\"access_token\":")
				if err := rw.Flush(); err != nil {
					t.Error(err)
				}
				return
			}
			fmt.Fprint(w, `{"access_token":"replacement","expires_in":3600}`)
			return
		}
		validations.Add(1)
		fmt.Fprint(w, `{"spaces":[]}`)
	})
	writeJSON(t, tokenFile, savedToken("original"))
	s := mustSource(t, clientFile, tokenFile, ep)
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	reloads := 0
	reload := func() { reloads++ }
	s.reload(t.Context(), reload)
	if reloads != 0 {
		t.Fatal("interrupted replacement installed")
	}
	interrupted.Store(false)
	s.now = func() time.Time { return time.Now().Add(s.watchInterval) }
	s.reload(t.Context(), reload)
	s.reload(t.Context(), reload)
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "replacement" || reloads != 1 || validations.Load() != 1 {
		t.Fatalf("recovery: token=%+v err=%v reloads=%d refreshes=%d validations=%d", tok, err, reloads, refreshes.Load(), validations.Load())
	}
}

func TestWatchPermanentURLFailureObservedOnce(t *testing.T) {
	for _, endpoint := range []string{"unsupported://example", "http://127.0.0.1:99999"} {
		t.Run(endpoint, func(t *testing.T) {
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP call") })
			ep.ChatAPIBase = endpoint
			s := mustSource(t, clientFile, tokenFile, ep)
			now := time.Now()
			s.now = func() time.Time { return now }
			validations := 0
			validate := s.validateFn
			s.validateFn = func(ctx context.Context, client *http.Client) error {
				validations++
				return validate(ctx, client)
			}
			writeJSON(t, tokenFile, savedToken("replacement"))
			for range 3 {
				now = now.Add(time.Hour) // Outlast even the maximum transient backoff.
				s.reload(t.Context(), func() { t.Error("invalid URL accepted") })
			}
			if validations != 1 {
				t.Fatalf("permanent URL error retried %d times", validations)
			}
		})
	}
}

func TestWatchRetriesStalledOAuthBody(t *testing.T) {
	var refreshes, validations atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			if refreshes.Add(1) == 1 {
				_, _ = io.Copy(io.Discard, r.Body)
				fmt.Fprint(w, `{"access_token":`)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			fmt.Fprint(w, `{"access_token":"replacement","expires_in":3600}`)
			return
		}
		validations.Add(1)
		fmt.Fprint(w, `{"spaces":[]}`)
	})
	writeJSON(t, tokenFile, savedToken("original"))
	s := mustSource(t, clientFile, tokenFile, ep)
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	now := time.Now()
	s.now = func() time.Time { return now }
	reloads := 0
	deadline, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	s.reload(deadline, func() { reloads++ })
	if refreshes.Load() != 1 || validations.Load() != 0 || reloads != 0 {
		t.Fatalf("stalled response: refreshes=%d validations=%d reloads=%d", refreshes.Load(), validations.Load(), reloads)
	}
	now = now.Add(s.watchInterval)
	s.reload(t.Context(), func() { reloads++ })
	s.reload(t.Context(), func() { reloads++ })
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "replacement" || refreshes.Load() != 2 || validations.Load() != 1 || reloads != 1 {
		t.Fatalf("recovery: token=%+v error=%v refreshes=%d validations=%d reloads=%d", tok, err, refreshes.Load(), validations.Load(), reloads)
	}
}

func TestWatchRetriesHTTP2StreamReset(t *testing.T) {
	var validations atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("protocol = %s, want HTTP/2", r.Proto)
		}
		if validations.Add(1) == 1 {
			panic(http.ErrAbortHandler)
		}
		fmt.Fprint(w, `{"spaces":[]}`)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected OAuth call") })
	ep.ChatAPIBase = server.URL
	s := mustSource(t, clientFile, tokenFile, ep)
	writeJSON(t, tokenFile, savedToken("replacement"))
	now := time.Now()
	s.now = func() time.Time { return now }
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, server.Client())
	reloads := 0
	s.reload(ctx, func() { reloads++ })
	if validations.Load() != 1 || reloads != 0 {
		t.Fatalf("reset response: validations=%d reloads=%d", validations.Load(), reloads)
	}
	now = now.Add(s.watchInterval)
	s.reload(ctx, func() { reloads++ })
	s.reload(ctx, func() { reloads++ })
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "replacement" || validations.Load() != 2 || reloads != 1 {
		t.Fatalf("recovery: token=%+v error=%v validations=%d reloads=%d", tok, err, validations.Load(), reloads)
	}
}

func TestNewRejectsEmptyChatAPIBase(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP call") })
	ep.ChatAPIBase = ""
	if _, err := New(clientFile, tokenFile, ep, testScopes); err == nil || !strings.Contains(err.Error(), "ChatAPIBase") {
		t.Fatalf("missing ChatAPIBase error = %v", err)
	}
}

func TestTransientClassification(t *testing.T) {
	emptyFile := filepath.Join(t.TempDir(), "token.json")
	writeJSON(t, emptyFile, map[string]any{})
	_, _, emptyTokenErr := readToken(emptyFile)
	if emptyTokenErr == nil {
		t.Fatal("empty token file was accepted")
	}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"EOF", io.EOF, true},
		{"unexpected EOF", fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF), true},
		{"oauth body EOF", errors.New("oauth2: cannot fetch token: unexpected EOF"), true},
		{"oauth body deadline", errors.New("oauth2: cannot fetch token: context deadline exceeded"), true},
		{"unknown transport failure", errors.New("new transport failure"), true},
		{"oauth body reset", errors.New("oauth2: cannot fetch token: read tcp: connection reset by peer"), true},
		{"reset", syscall.ECONNRESET, true},
		{"network", &url.Error{Op: "Get", URL: "http://example", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"permanent URL", &url.Error{Op: "Get", URL: "unsupported://example", Err: errors.New("unsupported protocol scheme")}, false},
		{"invalid port", &url.Error{Op: "Get", URL: "http://example:99999", Err: &net.OpError{Op: "dial", Err: &net.AddrError{Err: "invalid port", Addr: "99999"}}}, false},
		{"invalid TLS certificate", &url.Error{Op: "Get", URL: "https://example", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
		{"plain HTTP behind HTTPS", &url.Error{Op: "Post", URL: "https://example", Err: tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}}, false},
		{"HTTP scheme mismatch", &url.Error{Op: "Post", URL: "https://example", Err: http.ErrSchemeMismatch}, false},
		{"missing file", &os.PathError{Op: "open", Path: "token.json", Err: os.ErrNotExist}, false},
		{"malformed JSON", &json.SyntaxError{}, false},
		{"empty token file", emptyTokenErr, false},
		{"chat 429", validationError(429), true},
		{"chat 408", validationError(408), true},
		{"chat 503", validationError(503), true},
		{"chat 401", validationError(401), false},
		{"oauth 429", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 429}}, true},
		{"oauth 408", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 408}}, true},
		{"oauth 404", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 404}}, false},
		{"oauth unknown code 400", &oauth2.RetrieveError{ErrorCode: "unexpected", Response: &http.Response{StatusCode: 400}}, false},
		{"oauth temporary code 400", &oauth2.RetrieveError{ErrorCode: "temporarily_unavailable", Response: &http.Response{StatusCode: 400}}, true},
		{"oauth 503", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 503}}, true},
		{"oauth policy", &oauth2.RetrieveError{ErrorCode: "admin_policy_enforced", Response: &http.Response{StatusCode: 503}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := transient(tc.err); got != tc.want {
				t.Errorf("transient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWatchTokenEndpoint404ObservedOnce(t *testing.T) {
	var calls atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "token-secret")
	})
	s := mustSource(t, clientFile, tokenFile, ep)
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	now := time.Now()
	s.now = func() time.Time { return now }
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for range 3 {
		s.reload(t.Context(), func() { t.Error("invalid token accepted") })
		now = now.Add(time.Hour)
	}
	if calls.Load() != 1 || strings.Count(logs.String(), "WARN") != 1 ||
		!strings.Contains(logs.String(), "OAuth unrecognized (HTTP 404)") ||
		strings.Contains(logs.String(), "temporarily") || strings.Contains(logs.String(), "token-secret") {
		t.Fatalf("permanent endpoint failure: calls=%d logs=%s", calls.Load(), logs.String())
	}
}

func TestClientAllowsSlowBody(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "first ")
		w.(http.Flusher).Flush()
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			fmt.Fprint(w, "last")
		case <-r.Context().Done():
		}
	})
	writeJSON(t, tokenFile, savedToken("original"))
	s := mustSource(t, clientFile, tokenFile, ep)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 100 * time.Millisecond
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: transport})
	client, err := s.Client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 0 {
		t.Errorf("client has whole-body timeout %v", client.Timeout)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.ChatAPIBase+"/media", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil || string(body) != "first last" {
		t.Fatalf("slow body = %q, error = %v", body, err)
	}
}

func TestWatchLogsSanitizedCause(t *testing.T) {
	for _, scenario := range []struct{ name, want string }{
		{"unauthorized", "HTTP 401"},
		{"oauth policy", "admin_policy_enforced"},
		{"oauth body", "HTTP 400"},
		{"malformed", "malformed JSON"},
		{"bad expiry", "malformed JSON"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					if scenario.name == "oauth policy" {
						fmt.Fprint(w, `{"error":"admin_policy_enforced","error_description":"token-secret"}`)
					} else {
						fmt.Fprint(w, `{"error_description":"token-secret"}`)
					}
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, "token-secret")
			})
			s := mustSource(t, clientFile, tokenFile, ep)
			tok := savedToken("token-secret")
			if strings.HasPrefix(scenario.name, "oauth") {
				tok.Expiry = time.Now().Add(-time.Hour)
			}
			writeJSON(t, tokenFile, tok)
			if scenario.name == "malformed" {
				if err := os.WriteFile(tokenFile, []byte(`{"access_token":"token-secret",`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario.name == "bad expiry" {
				writeJSON(t, tokenFile, map[string]string{"access_token": "token-secret", "expiry": "token-secret"})
			}
			s.reload(t.Context(), func() { t.Error("invalid token accepted") })
			if !strings.Contains(logs.String(), scenario.want) || strings.Contains(logs.String(), "token-secret") {
				t.Fatalf("rejection log = %q, want sanitized %q", logs.String(), scenario.want)
			}
		})
	}
}

func TestWatchBacksOffTransientValidation(t *testing.T) {
	var calls atomic.Int32
	var healthy atomic.Bool
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	s := mustSource(t, clientFile, tokenFile, ep)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.watchInterval = time.Second
	writeJSON(t, tokenFile, savedToken("replacement"))
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	for range 3 {
		s.reload(t.Context(), nil)
	}
	if calls.Load() != 1 || strings.Count(logs.String(), "WARN") != 1 {
		t.Fatalf("polls bypassed backoff: requests=%d warnings=%d", calls.Load(), strings.Count(logs.String(), "WARN"))
	}
	// First retry is one poll later; subsequent retries double up to a minute.
	wantCalls := int32(1)
	for _, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second, time.Minute, time.Minute} {
		now = now.Add(delay - time.Nanosecond)
		s.reload(t.Context(), nil)
		if calls.Load() != wantCalls {
			t.Fatalf("retried before %v elapsed", delay)
		}
		now = now.Add(time.Nanosecond)
		s.reload(t.Context(), nil)
		wantCalls++
		if calls.Load() != wantCalls || strings.Count(logs.String(), "WARN") != int(wantCalls) {
			t.Fatalf("retry after %v: requests=%d warnings=%d want=%d", delay, calls.Load(), strings.Count(logs.String(), "WARN"), wantCalls)
		}
	}
	// A new revision bypasses the one-minute delay and resets it to one poll.
	writeJSON(t, tokenFile+".new", savedToken("new revision"))
	if err := os.Rename(tokenFile+".new", tokenFile); err != nil {
		t.Fatal(err)
	}
	s.reload(t.Context(), nil)
	wantCalls++
	if calls.Load() != wantCalls {
		t.Fatal("new file did not reset backoff")
	}
	healthy.Store(true)
	now = now.Add(time.Second)
	reloads := 0
	s.reload(t.Context(), func() { reloads++ })
	wantCalls++
	if calls.Load() != wantCalls || reloads != 1 {
		t.Fatalf("new revision did not retry after one poll: calls=%d want=%d reloads=%d", calls.Load(), wantCalls, reloads)
	}
	now = now.Add(time.Hour)
	s.reload(t.Context(), func() { reloads++ })
	if calls.Load() != wantCalls || reloads != 1 || s.State() != Ready || s.retryDelay != 0 {
		t.Fatalf("successful validation did not reset retries: calls=%d reloads=%d state=%v delay=%v", calls.Load(), reloads, s.State(), s.retryDelay)
	}
}

func TestRunAuthHandlesStaleTemporaryPath(t *testing.T) {
	for _, kind := range []string{"regular", "symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"access_token":"signed-in","refresh_token":"new-refresh","expires_in":3600,"scope":%q,"id_token":"e30.eyJzdWIiOiIxMjM0NTYifQ.signature"}`, strings.Join(testScopes, " "))
			})
			writeJSON(t, tokenFile, savedToken("original"))
			tmp := tokenFile + ".tmp"
			switch kind {
			case "regular":
				writeJSON(t, tmp, savedToken("stale-secret"))
			case "symlink":
				if err := os.Symlink(tokenFile, tmp); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(tmp, 0700); err != nil {
					t.Fatal(err)
				}
			}
			staleInfo, err := os.Lstat(tmp)
			if err != nil {
				t.Fatal(err)
			}
			err = RunAuth(t.Context(), clientFile, tokenFile, ep, testScopes, io.Discard, func(raw string) error { return browserCallback(raw, "example-code", "", "") })
			if err != nil {
				t.Fatalf("unrelated temp path blocked auth: %v", err)
			}
			tok, info, err := readToken(tokenFile)
			if err != nil || tok.AccessToken != "signed-in" || info.Mode().Perm() != 0600 {
				t.Fatalf("replacement token: %+v, %v, %v", tok, info, err)
			}
			info, err = os.Lstat(tmp)
			if err != nil || !os.SameFile(staleInfo, info) || staleInfo.Mode() != info.Mode() {
				t.Fatalf("unrelated temp changed: %v, %v", info, err)
			}
			if kind == "regular" {
				stale, _, err := readToken(tmp)
				if err != nil || stale.AccessToken != "stale-secret" {
					t.Fatalf("stale file changed: %+v, %v", stale, err)
				}
			}
		})
	}
}

func TestRunAuthRefusesUnsafeTokenPath(t *testing.T) {
	for _, kind := range []string{"symlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				t.Error("unsafe token path reached the exchange endpoint")
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"access_token":"signed-in","refresh_token":"new-refresh","scope":%q,"id_token":"e30.eyJzdWIiOiIxMjM0NTYifQ.signature"}`, strings.Join(testScopes, " "))
			})
			target := tokenFile + ".original"
			writeJSON(t, target, savedToken("original"))
			if kind == "symlink" {
				if err := os.Symlink(target, tokenFile); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(tokenFile, 0700); err != nil {
				t.Fatal(err)
			}
			var stdout bytes.Buffer
			err := RunAuth(t.Context(), clientFile, tokenFile, ep, testScopes, &stdout, func(raw string) error {
				t.Error("unsafe token path reached browser consent")
				return browserCallback(raw, "example-code", "", "")
			})
			if stdout.Len() != 0 {
				t.Errorf("unsafe token path printed a consent URL: %s", stdout.String())
			}
			if err == nil || !strings.Contains(err.Error(), tokenFile) || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("unsafe token error = %v", err)
			}
			info, err := os.Lstat(tokenFile)
			if err != nil || (kind == "symlink" && info.Mode()&os.ModeSymlink == 0) || (kind == "directory" && !info.IsDir()) {
				t.Fatalf("unsafe token path changed: %v, %v", info, err)
			}
			tok, _, err := readToken(target)
			if err != nil || tok.AccessToken != "original" {
				t.Fatalf("original changed: %+v, %v", tok, err)
			}
			assertNoAuthTemps(t, tokenFile)
		})
	}
}

// Pause after a complete temp file exists, at writeToken's pre-rename context
// check. This forces overlapping commits without timing-dependent sleeps.
type authCommitContext struct {
	context.Context
	check func()
}

func (c authCommitContext) Err() error {
	c.check()
	return c.Context.Err()
}

func assertNoAuthTemps(t *testing.T, tokenFile string) {
	t.Helper()
	paths, err := filepath.Glob(tokenFile + "*.tmp")
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary files remain: %v, %v", paths, err)
	}
}

func TestWriteTokenFailureCleansOwnTemporaryFile(t *testing.T) {
	for _, scenario := range []string{"encode", "cancel before rename"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token.json")
			writeJSON(t, path, savedToken("original"))
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tok := savedToken("replacement")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			writeCtx := authCommitContext{Context: ctx, check: func() {
				if scenario == "cancel before rename" {
					paths, _ := filepath.Glob(path + "*.tmp")
					if len(paths) > 0 {
						cancel()
					}
				}
			}}
			if scenario == "encode" {
				tok.Expiry = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) // JSON time encoding rejects this year.
			}
			err = writeToken(writeCtx, path, tok)
			if err == nil || (scenario == "cancel before rename" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("write error = %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("old token changed: %v", err)
			}
			assertNoAuthTemps(t, path)
		})
	}
}

func TestWriteTokenSyncsDirectoryAfterRename(t *testing.T) {
	for _, tc := range []struct {
		name    string
		syncErr error
	}{
		{"success", nil},
		{"unsupported directory sync", syscall.ENOTSUP},
		{"unsupported Linux directory sync", syscall.EINVAL},
		{"failed directory sync", syscall.EIO},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "token.json")
			writeJSON(t, path, savedToken("old"))
			fileSyncs, directorySyncs := 0, 0
			var directory *os.File
			err := writeTokenWithSync(t.Context(), path, savedToken("replacement"), func(f *os.File) error {
				info, err := f.Stat()
				if err != nil {
					return err
				}
				if !info.IsDir() {
					fileSyncs++
					return f.Sync()
				}
				directorySyncs++
				directory = f
				if f.Name() != filepath.Dir(path) || fileSyncs != 1 {
					t.Errorf("directory sync out of order: path=%q fileSyncs=%d", f.Name(), fileSyncs)
				}
				tok, _, err := readToken(path)
				if err != nil || tok.AccessToken != "replacement" {
					t.Fatalf("directory synced before rename: token=%+v err=%v", tok, err)
				}
				assertNoAuthTemps(t, path)
				if tc.syncErr != nil {
					return tc.syncErr
				}
				return f.Sync()
			})
			if tc.syncErr == syscall.EIO {
				if !errors.Is(err, syscall.EIO) || !strings.Contains(err.Error(), fmt.Sprintf("token written to %s but its directory could not be synced", path)) {
					t.Errorf("directory sync failure = %v", err)
				}
			} else if err != nil {
				t.Error(err)
			}
			if fileSyncs != 1 || directorySyncs != 1 {
				t.Fatalf("syncs: file=%d directory=%d", fileSyncs, directorySyncs)
			}
			if _, err := directory.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Errorf("directory left open: %v", err)
			}
		})
	}
}

func TestRunAuthOverlappingWritesAreAtomic(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"new-refresh","expires_in":3600,"scope":%q,"id_token":"e30.eyJzdWIiOiIxMjM0NTYifQ.signature"}`, r.Form.Get("code"), strings.Join(testScopes, " "))
	})
	writeJSON(t, tokenFile, savedToken("original"))
	paused, resume := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	var pausedOnce atomic.Bool
	ctx := authCommitContext{Context: t.Context(), check: func() {
		paths, _ := filepath.Glob(tokenFile + "*.tmp")
		for _, path := range paths {
			tok, _, err := readToken(path)
			if err == nil && tok.AccessToken == "first" && pausedOnce.CompareAndSwap(false, true) {
				close(paused)
				select {
				case <-resume:
				case <-t.Context().Done():
				}
			}
		}
	}}
	// Observe the destination continuously while both runs exchange and commit.
	stop, observed := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			tok, info, err := readToken(tokenFile)
			if !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					observed <- err
					return
				}
				if !tok.Valid() || tok.RefreshToken == "" || info.Mode().Perm() != 0600 ||
					(tok.AccessToken != "original" && tok.AccessToken != "first" && tok.AccessToken != "second") {
					observed <- fmt.Errorf("partial or unexpected token: %+v mode=%v", tok, info.Mode())
					return
				}
			}
			select {
			case <-stop:
				observed <- nil
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	defer func() {
		close(stop)
		if err := <-observed; err != nil {
			t.Error(err)
		}
	}()
	first := make(chan error, 1)
	go func() {
		first <- RunAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard, func(raw string) error { return browserCallback(raw, "first", "", "") })
	}()
	waitSignal(t, paused)
	if err := RunAuth(t.Context(), clientFile, tokenFile, ep, testScopes, io.Discard, func(raw string) error { return browserCallback(raw, "second", "", "") }); err != nil {
		t.Errorf("second auth: %v", err)
	}
	release()
	if err := <-first; err != nil {
		t.Errorf("first auth: %v", err)
	}
	tok, _, err := readToken(tokenFile)
	if err != nil || !tok.Valid() || (tok.AccessToken != "first" && tok.AccessToken != "second") {
		t.Fatalf("final token = %+v, %v", tok, err)
	}
	assertNoAuthTemps(t, tokenFile)
}
