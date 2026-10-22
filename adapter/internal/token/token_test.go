package token

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"strings"
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
				requests.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `{"error":%q}`, code)
			})
			expired := savedToken("expired")
			expired.Expiry = time.Now().Add(-time.Hour)
			writeJSON(t, tokenFile, expired)
			s := mustSource(t, clientFile, tokenFile, ep)
			for range 3 {
				if _, err := s.Client(t.Context()); !errors.Is(err, ErrNeedsSignIn) {
					t.Errorf("Client error = %v", err)
				}
				if _, err := s.TokenSource(t.Context()).Token(); !errors.Is(err, ErrNeedsSignIn) {
					t.Errorf("Token error = %v", err)
				}
			}
			if s.State() != NeedsSignIn || requests.Load() != 1 {
				t.Fatalf("permanent failure: state=%v requests=%d", s.State(), requests.Load())
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
			if !missing {
				writeJSON(t, tokenFile, savedToken("original"))
			}
			s := mustSource(t, clientFile, tokenFile, ep)
			if missing && s.State() != Missing {
				t.Fatalf("missing state = %v", s.State())
			}
			reloaded := startWatch(t, s)
			replacement := tokenFile + ".new"
			writeJSON(t, replacement, savedToken("replacement"))
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
	validated := make(chan struct{}, 10)
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		validated <- struct{}{}
		w.WriteHeader(http.StatusUnauthorized)
	})
	writeJSON(t, tokenFile, savedToken("original"))
	s := mustSource(t, clientFile, tokenFile, ep)
	reloaded := startWatch(t, s)
	writeJSON(t, tokenFile+".new", savedToken("invalid"))
	if err := os.Rename(tokenFile+".new", tokenFile); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, validated)
	select {
	case <-reloaded:
		t.Fatal("invalid replacement was installed")
	case <-time.After(50 * time.Millisecond):
	}
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "original" || s.State() != Ready {
		t.Fatalf("lost original: token=%+v error=%v state=%v", tok, err, s.State())
	}
	if err := os.WriteFile(tokenFile, []byte(`{invalid`), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-reloaded:
		t.Fatal("malformed replacement was installed")
	case <-time.After(50 * time.Millisecond):
	}
	tok, err = s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "original" {
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
		fmt.Fprintf(w, `{"access_token":"signed-in","refresh_token":"new-refresh","expires_in":3600,"scope":%q}`, strings.Join(testScopes, " "))
	})
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
	if _, err := os.Stat(tokenFile + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file remains: %v", err)
	}
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

func TestRunAuthFailuresPreserveToken(t *testing.T) {
	for _, scenario := range []string{"partial scopes", "missing scope field", "denied", "cancel", "cancel during exchange", "write failure", "no refresh token"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var exchanges atomic.Int32
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				exchanges.Add(1)
				if scenario == "cancel during exchange" {
					cancel()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				response := map[string]any{"access_token": "signed-in", "refresh_token": "new-refresh", "expires_in": 3600, "scope": strings.Join(testScopes, " ")}
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
				if err := os.Mkdir(tokenFile+".tmp", 0700); err != nil {
					t.Fatal(err)
				}
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
			if scenario == "write failure" && !strings.Contains(err.Error(), tokenFile+".tmp") {
				t.Fatalf("write error lacks path: %v", err)
			}
			after, readErr := os.ReadFile(tokenFile)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("old token changed: %v", readErr)
			}
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
	for _, path := range []string{tokenFile, tokenFile + ".tmp"} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cancel created %s: %v", path, err)
		}
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
			writeJSON(t, tokenFile, expired)
			s := mustSource(t, clientFile, tokenFile, ep)
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
	s.reload(t.Context(), reload)
	s.reload(t.Context(), reload)
	tok, err := s.TokenSource(t.Context()).Token()
	if err != nil || tok.AccessToken != "replacement" || reloads != 1 || validations.Load() != 1 {
		t.Fatalf("recovery: token=%+v err=%v reloads=%d refreshes=%d validations=%d", tok, err, reloads, refreshes.Load(), validations.Load())
	}
}

func TestWatchPermanentURLFailureObservedOnce(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected HTTP call") })
	ep.ChatAPIBase = "unsupported://example"
	s := mustSource(t, clientFile, tokenFile, ep)
	validations := 0
	validate := s.validateFn
	s.validateFn = func(ctx context.Context, client *http.Client) error {
		validations++
		return validate(ctx, client)
	}
	writeJSON(t, tokenFile, savedToken("replacement"))
	for range 3 {
		s.reload(t.Context(), func() { t.Error("invalid URL accepted") })
	}
	if validations != 1 {
		t.Fatalf("permanent URL error retried %d times", validations)
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
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"EOF", io.EOF, true},
		{"unexpected EOF", fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF), true},
		{"oauth body EOF", errors.New("oauth2: cannot fetch token: unexpected EOF"), true},
		{"oauth body reset", errors.New("oauth2: cannot fetch token: read tcp: connection reset by peer"), true},
		{"reset", syscall.ECONNRESET, true},
		{"network", &url.Error{Op: "Get", URL: "http://example", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, true},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"permanent URL", &url.Error{Op: "Get", URL: "unsupported://example", Err: errors.New("unsupported protocol scheme")}, false},
		{"malformed JSON", &json.SyntaxError{}, false},
		{"chat 429", validationError(429), true},
		{"chat 503", validationError(503), true},
		{"chat 401", validationError(401), false},
		{"oauth 429", &oauth2.RetrieveError{Response: &http.Response{StatusCode: 429}}, true},
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
