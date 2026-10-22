package main

import (
	"bytes"
	"context"
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
	"testing"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"golang.org/x/oauth2"
)

func TestResolveFiles(t *testing.T) {
	for _, source := range []string{"explicit", "service", "home"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", filepath.Join(dir, "home"))
			t.Setenv("GC_SERVICE_SECRETS_DIR", "")
			t.Setenv("GC_GCHAT_ADAPTER_ENV", "")
			want := filepath.Join(os.Getenv("HOME"), ".config/gc-gchat-adapter/env")
			if source != "home" {
				t.Setenv("GC_SERVICE_SECRETS_DIR", filepath.Join(dir, "service/secrets"))
				want = filepath.Join(os.Getenv("GC_SERVICE_SECRETS_DIR"), "env")
			}
			if source == "explicit" {
				want = filepath.Join(dir, "explicit/secrets/custom-env")
				t.Setenv("GC_GCHAT_ADAPTER_ENV", want)
			}
			files, err := resolveFiles(os.Getenv)
			if err != nil || files.env != want || files.secrets != filepath.Dir(want) || files.settings != "" {
				t.Fatalf("resolveFiles = %+v, %v; want %s with no settings", files, err, want)
			}
			settings := filepath.Join(filepath.Dir(want), "..", "config.toml")
			writeFile(t, settings, "[defaults]\nwake='mentions'\n")
			files, err = resolveFiles(os.Getenv)
			if err != nil || files.settings != settings {
				t.Fatalf("resolveFiles = %+v, %v; want settings %s", files, err, settings)
			}
		})
	}
}

func TestCommandDispatch(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		code     int
		out, err string
	}{
		{[]string{"version"}, 0, "gc-gchat-adapter " + Version + "\n", ""},
		{nil, 2, "", "not implemented yet\n"},
		{[]string{"serve"}, 2, "", "not implemented yet\n"},
		{[]string{"state", "reset"}, 2, "", "not implemented yet\n"},
		{[]string{"unknown"}, 2, "", "usage:"},
		{[]string{"auth", "--unknown"}, 2, "", "usage:"},
		{[]string{"auth", "--check", "extra"}, 2, "", "usage:"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tc.args, &stdout, &stderr, func(string) string { t.Fatal("unexpected config lookup"); return "" }, nil)
			if code != tc.code || stdout.String() != tc.out || !strings.HasPrefix(stderr.String(), tc.err) {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}

func TestAuthMissingEnvSurfacesConfigError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-env")
	_, want := config.Load(path, "", filepath.Dir(path))
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"auth"}, &stdout, &stderr, envAt(path), nil)
	if code == 0 || stdout.Len() != 0 || stderr.String() != want.Error()+"\n" {
		t.Fatalf("code=%d stdout=%q stderr=%q, want %v", code, &stdout, &stderr, want)
	}
	for _, key := range []string{path, "GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION", "GCHAT_OAUTH_CLIENT_FILE", "GC_CITY_NAME", "GC_API_BASE_URL"} {
		if !strings.Contains(stderr.String(), key) {
			t.Errorf("missing %s in %q", key, &stderr)
		}
	}
}

func TestAuthCheck(t *testing.T) {
	for _, tc := range []struct {
		name, tokenError, want string
		missing                bool
	}{
		{name: "ready", want: "ready\n"},
		{name: "missing", missing: true, want: "needs-sign-in"},
		{name: "revoked", tokenError: "invalid_grant", want: "needs-sign-in"},
		{name: "refresh failure", tokenError: "temporarily_unavailable", want: "OAuth refresh failed: OAuth temporarily_unavailable (HTTP 400)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := fakegoogle.New(t, nil)
			fake.Configure(func(s *fakegoogle.Server) { s.TokenError = tc.tokenError })
			env, tokenPath := authFixture(t, fake.URL())
			if !tc.missing {
				// A valid cached access token must not bypass the refresh check.
				b, err := json.Marshal(oauth2.Token{AccessToken: "cached", RefreshToken: "example-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
				writeFile(t, tokenPath, string(b))
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"auth", "--check"}, &stdout, &stderr, envAt(env), func(string) error { t.Error("check opened browser"); return nil })
			if (code == 0) != (tc.name == "ready") || !strings.Contains(stdout.String(), tc.want) || stderr.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
		})
	}
}

func TestAuthLoopback(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprintf("settings=%v", configured), func(t *testing.T) {
			var consentScopes string
			exchanges := 0
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/token" || r.Method != http.MethodPost {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "example-code" || r.Form.Get("code_verifier") == "" {
					t.Errorf("unexpected exchange: %v", r.Form)
				}
				exchanges++
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "example-access", "refresh_token": "example-refresh", "expires_in": 3600, "token_type": "Bearer", "scope": consentScopes})
			}))
			t.Cleanup(fake.Close)
			env, tokenPath := authFixture(t, fake.URL)
			if configured {
				tokenPath = filepath.Join(filepath.Dir(env), "identity-token.json")
				writeFile(t, filepath.Join(filepath.Dir(env), "..", "config.toml"), "[[identity]]\nname='example'\nagent='mayor'\ntoken_file='identity-token.json'\n")
			}
			var stdout, stderr bytes.Buffer
			opened := ""
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			code := run(ctx, []string{"auth"}, &stdout, &stderr, envAt(env), func(raw string) error {
				opened = raw
				if stdout.String() != raw+"\n" {
					t.Errorf("URL not printed before open: %q", &stdout)
				}
				u, err := url.Parse(raw)
				if err != nil {
					return err
				}
				q := u.Query()
				consentScopes = q.Get("scope")
				wantScopes := "https://www.googleapis.com/auth/chat.messages https://www.googleapis.com/auth/chat.spaces.readonly https://www.googleapis.com/auth/chat.memberships.readonly https://www.googleapis.com/auth/pubsub"
				if consentScopes != wantScopes {
					t.Errorf("scopes = %q", consentScopes)
				}
				if q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("code_challenge_method") != "S256" {
					t.Errorf("consent query = %v", q)
				}
				callback, err := url.Parse(q.Get("redirect_uri"))
				if err != nil {
					return err
				}
				callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"example-code"}}.Encode()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, callback.String(), nil)
				if err != nil {
					return err
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				_, _ = io.Copy(io.Discard, resp.Body)
				if resp.StatusCode != 200 {
					t.Errorf("callback status = %d", resp.StatusCode)
				}
				return errors.New("browser launcher failed after opening") // ignored by the CLI
			})
			if code != 0 || stderr.Len() != 0 || opened == "" || stdout.String() != opened+"\ntoken file "+tokenPath+": ready\n" || exchanges != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q exchanges=%d", code, &stdout, &stderr, exchanges)
			}
			b, err := os.ReadFile(tokenPath)
			if err != nil {
				t.Fatal(err)
			}
			var saved oauth2.Token
			if err := json.Unmarshal(b, &saved); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(tokenPath)
			if err != nil {
				t.Fatal(err)
			}
			if saved.RefreshToken != "example-refresh" || !saved.Valid() || info.Mode().Perm() != 0o600 {
				t.Fatalf("invalid saved token or mode: %s", info.Mode())
			}
		})
	}
}

func TestAuthCancellation(t *testing.T) {
	fake := fakegoogle.New(t, nil)
	env, tokenPath := authFixture(t, fake.URL())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, []string{"auth"}, &stdout, &stderr, envAt(env), func(string) error { cancel(); return nil })
	if code == 0 || !strings.Contains(stderr.String(), "context canceled") || strings.Contains(stdout.String(), "ready") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
	if _, err := os.Stat(tokenPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token created on cancellation: %v", err)
	}
}

func envAt(path string) func(string) string {
	return func(key string) string {
		if key == "GC_GCHAT_ADAPTER_ENV" {
			return path
		}
		return ""
	}
}

func authFixture(t *testing.T, endpoint string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "secrets")
	clientFile := filepath.Join(dir, "client.json")
	writeFile(t, clientFile, `{"installed":{"client_id":"example-client","client_secret":"example-secret"}}`)
	env := filepath.Join(dir, "env")
	writeFile(t, env, "GCHAT_PROJECT_ID=example\nGCHAT_PUBSUB_TOPIC=projects/example/topics/chat\nGCHAT_PUBSUB_SUBSCRIPTION=projects/example/subscriptions/chat\nGCHAT_OAUTH_CLIENT_FILE="+clientFile+"\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL=http://127.0.0.1:8372\n")
	t.Setenv("GCHAT_CHAT_API_BASE", endpoint)
	t.Setenv("GCHAT_OAUTH_AUTH_URL", endpoint+"/authorize")
	t.Setenv("GCHAT_OAUTH_TOKEN_URL", endpoint+"/token")
	return env, filepath.Join(dir, "token.json")
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
