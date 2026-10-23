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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/app"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
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
			if source == "home" {
				settings = filepath.Join(filepath.Dir(want), "config.toml")
			}
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
		{[]string{"serve", "extra"}, 2, "", "usage:"},
		{[]string{"state"}, 2, "", "usage:"},
		{[]string{"state", "restore"}, 2, "", "usage:"},
		{[]string{"state", "reset", "extra"}, 2, "", "usage:"},
		{[]string{"state", "reset", "--unknown"}, 2, "", "usage:"},
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

func TestServeLoadsConfigurationByDefault(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing-env")
			_, want := config.Load(path, "", filepath.Dir(path))
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), args, &stdout, &stderr, envAt(path), nil)
			if code != 1 || stdout.Len() != 0 || stderr.String() != want.Error()+"\n" {
				t.Fatalf("code=%d stdout=%q stderr=%q, want %v", code, &stdout, &stderr, want)
			}
		})
	}
}

func TestServeRunsApplicationWithResolvedFiles(t *testing.T) {
	for _, args := range [][]string{nil, {"serve"}} {
		for _, appErr := range []error{nil, errors.New("startup failed")} {
			t.Run(fmt.Sprintf("%v/%v", args, appErr), func(t *testing.T) {
				env, _ := authFixture(t, "http://127.0.0.1:1")
				settings := filepath.Join(filepath.Dir(env), "..", "config.toml")
				writeFile(t, settings, "[defaults]\nwake='mentions'\n")
				ctx := t.Context()
				called := false
				var stdout, stderr bytes.Buffer
				code := runWithServe(ctx, args, &stdout, &stderr, envAt(env), nil,
					func(got context.Context, cfg *config.Config, options app.Options) error {
						called = true
						if got != ctx || cfg.Secrets.CityName != "examplecity" || cfg.Settings.Defaults.Wake != config.WakeMentions || options.SettingsPath != settings || options.EnvFile != env {
							t.Fatalf("incorrect application input: cfg=%+v options=%+v", cfg, options)
						}
						return appErr
					})
				if !called || stdout.Len() != 0 || (code == 0) != (appErr == nil) {
					t.Fatalf("called=%v code=%d stdout=%q stderr=%q", called, code, &stdout, &stderr)
				}
				if appErr != nil && stderr.String() != appErr.Error()+"\n" {
					t.Fatalf("application failure not reported: %q", &stderr)
				}
			})
		}
	}
}

func TestServePassesMissingSettingsPathToApplication(t *testing.T) {
	for _, layout := range []string{"home", "service"} {
		t.Run(layout, func(t *testing.T) {
			fixtureEnv, _ := authFixture(t, "http://127.0.0.1:1")
			contents, err := os.ReadFile(fixtureEnv)
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			secrets := filepath.Join(home, ".config", "gc-gchat-adapter")
			getenv := func(key string) string {
				if key == "HOME" {
					return home
				}
				if layout == "service" && key == "GC_SERVICE_SECRETS_DIR" {
					return secrets
				}
				return ""
			}
			if layout == "service" {
				secrets = filepath.Join(home, "service", "secrets")
			}
			env := filepath.Join(secrets, "env")
			writeFile(t, env, string(contents))
			wantSettings := filepath.Join(secrets, "config.toml")
			if layout == "service" {
				wantSettings = filepath.Join(secrets, "..", "config.toml")
			}
			called := false
			var stdout, stderr bytes.Buffer
			code := runWithServe(t.Context(), nil, &stdout, &stderr, getenv, nil,
				func(_ context.Context, cfg *config.Config, options app.Options) error {
					called = true
					if options.SettingsPath != wantSettings || options.EnvFile != env || cfg.Wake("spaces/example") != config.WakeAll {
						t.Fatalf("missing settings target or defaults: settings=%q env=%q wake=%q", options.SettingsPath, options.EnvFile, cfg.Wake("spaces/example"))
					}
					return nil
				})
			if !called || code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("called=%v exit=%d stdout=%q stderr=%q", called, code, &stdout, &stderr)
			}
		})
	}
}

func TestAuthCheck(t *testing.T) {
	for _, tc := range []struct {
		name, tokenError, want string
		missing                bool
	}{
		{name: "ready", want: "identity_user_id: users/123456\n"},
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
				writeFile(t, tokenPath, strings.TrimSuffix(string(b), "}")+`,"identity_user_id":"users/123456"}`)
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
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "example-access", "refresh_token": "example-refresh", "expires_in": 3600, "token_type": "Bearer", "scope": consentScopes, "id_token": "e30.eyJzdWIiOiIxMjM0NTYifQ.signature"})
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
				wantScopes := "https://www.googleapis.com/auth/chat.messages https://www.googleapis.com/auth/chat.spaces.readonly https://www.googleapis.com/auth/chat.memberships.readonly https://www.googleapis.com/auth/pubsub openid"
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
				return errors.New("browser launcher failed after opening")
			})
			wantOutput := opened + "\nCould not open browser (browser launcher failed after opening); open the URL above manually.\ntoken file " + tokenPath + ": ready\n"
			if code != 0 || stderr.Len() != 0 || opened == "" || stdout.String() != wantOutput || exchanges != 1 {
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
	stubDir := t.TempDir()
	stub := filepath.Join(stubDir, "gc")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", stub)
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := filepath.Join(t.TempDir(), "secrets")
	clientFile := filepath.Join(dir, "client.json")
	writeFile(t, clientFile, `{"installed":{"client_id":"example-client","client_secret":"example-secret"}}`)
	env := filepath.Join(dir, "env")
	writeFile(t, env, "GCHAT_PROJECT_ID=example\nGCHAT_PUBSUB_TOPIC=projects/example/topics/chat\nGCHAT_PUBSUB_SUBSCRIPTION=projects/example/subscriptions/chat\nGCHAT_OAUTH_CLIENT_FILE="+clientFile+"\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL="+fakegc.New(t).URL()+"\nGCHAT_OAUTH_AUTH_URL="+endpoint+"/authorize\nGCHAT_OAUTH_TOKEN_URL="+endpoint+"/token\n")
	t.Setenv("GCHAT_CHAT_API_BASE", endpoint)
	t.Setenv("GCHAT_OAUTH_AUTH_URL", "")
	t.Setenv("GCHAT_OAUTH_TOKEN_URL", "")
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

func TestRunScript(t *testing.T) {
	for _, layout := range []string{"explicit", "service", "home"} {
		t.Run(layout, func(t *testing.T) {
			dir, script := launcherFixture(t)
			home := filepath.Join(dir, "home")
			envPath := filepath.Join(home, ".config", "gc-gchat-adapter", "env")
			environment := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
			switch layout {
			case "explicit":
				envPath = filepath.Join(dir, "explicit env")
				environment = append(environment, "GC_GCHAT_ADAPTER_ENV="+envPath, "GC_SERVICE_SECRETS_DIR="+filepath.Join(dir, "ignored"))
			case "service":
				envPath = filepath.Join(dir, "service secrets", "env")
				environment = append(environment, "GC_SERVICE_SECRETS_DIR="+filepath.Dir(envPath))
			}
			writeFile(t, envPath, "LAUNCHER_VALUE='exported value'\n")
			execute := func() string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "sh", script)
				cmd.Env = environment
				cmd.Dir = dir
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("run.sh: %v: %s", err, output)
				}
				return string(output)
			}
			if output := execute(); output != "serve|exported value\n" {
				t.Fatalf("launcher output=%q", output)
			}
			builds := filepath.Join(filepath.Dir(script), "builds")
			if output := execute(); output != "serve|exported value\n" {
				t.Fatalf("repeat output=%q", output)
			}
			assertBuilds := func(want string) {
				t.Helper()
				got, err := os.ReadFile(builds)
				if err != nil || string(got) != want {
					t.Fatalf("builds=%q err=%v, want %q", got, err, want)
				}
			}
			assertBuilds("build\n")
			future := time.Now().Add(time.Hour)
			if err := os.Chtimes(filepath.Join(filepath.Dir(script), "internal", "source.go"), future, future); err != nil {
				t.Fatal(err)
			}
			execute()
			assertBuilds("build\nbuild\n")
		})
	}
}

func TestRunScriptMissingEnv(t *testing.T) {
	dir, script := launcherFixture(t)
	envPath := filepath.Join(dir, "missing-env")
	cmd := exec.CommandContext(t.Context(), "sh", script)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GC_GCHAT_ADAPTER_ENV=" + envPath}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("missing env succeeded: %s", output)
	}
	for _, want := range []string{envPath, "GCHAT_PROJECT_ID", "GCHAT_PUBSUB_TOPIC", "GCHAT_PUBSUB_SUBSCRIPTION", "GCHAT_OAUTH_CLIENT_FILE", "GC_CITY_NAME", "GC_API_BASE_URL"} {
		if !strings.Contains(string(output), want) {
			t.Errorf("missing %q in %s", want, output)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(script), "builds")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing env invoked build: %v", err)
	}
}

func launcherFixture(t *testing.T) (dir, script string) {
	t.Helper()
	dir = t.TempDir()
	script = filepath.Join(dir, "adapter", "run.sh")
	contents, err := os.ReadFile("../../run.sh")
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	writeFile(t, script, string(contents))
	writeFile(t, filepath.Join(filepath.Dir(script), "internal", "source.go"), "package internal\n")
	build := filepath.Join(filepath.Dir(script), "build.sh")
	writeFile(t, build, "#!/bin/sh\nset -eu\nprintf 'build\\n' >> builds\ncat > gc-gchat-adapter <<'BINARY'\n#!/bin/sh\nprintf '%s|%s\\n' \"$*\" \"$LAUNCHER_VALUE\"\nBINARY\nchmod +x gc-gchat-adapter\n")
	if err := os.Chmod(build, 0700); err != nil {
		t.Fatal(err)
	}
	return
}
