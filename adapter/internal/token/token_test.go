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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestInvalidGrant(t *testing.T) {
	var requests atomic.Int32
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	})
	expired := savedToken("expired")
	expired.Expiry = time.Now().Add(-time.Hour)
	writeJSON(t, tokenFile, expired)
	s := mustSource(t, clientFile, tokenFile, ep)
	if _, err := s.Client(t.Context()); !errors.Is(err, ErrNeedsSignIn) {
		t.Fatalf("Client error = %v", err)
	}
	if s.State() != NeedsSignIn {
		t.Fatalf("state = %v", s.State())
	}
	if _, err := s.TokenSource(t.Context()).Token(); !errors.Is(err, ErrNeedsSignIn) || requests.Load() != 1 {
		t.Fatalf("revoked token retried: err=%v calls=%d", err, requests.Load())
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
