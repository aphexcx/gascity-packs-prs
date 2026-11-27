package inbound

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/provenance"
	"gchat-adapter/internal/token"
	"golang.org/x/oauth2"
	api "google.golang.org/api/chat/v1"
)

func TestSenderFillAuthenticationStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"oauth", 400, "invalid_grant"}, {"managed", 403, "scope_not_allowed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected API request") }))
			defer directory.Close()
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"error":%q}`, tc.code)
			})
			var source oauth2.TokenSource
			if tc.name == "oauth" {
				server := httptest.NewServer(handler)
				defer server.Close()
				cfg := oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL, AuthStyle: oauth2.AuthStyleInParams}}
				source = cfg.TokenSource(t.Context(), &oauth2.Token{RefreshToken: "fixture", Expiry: time.Unix(1, 0)})
			} else {
				dir, err := os.MkdirTemp("", "fill-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				socket := filepath.Join(dir, "s")
				ln, err := net.Listen("unix", socket)
				if err != nil {
					t.Fatal(err)
				}
				server := &http.Server{Handler: handler}
				go server.Serve(ln)
				defer server.Close()
				managed, err := token.NewKeyholder(socket, config.Endpoints{ChatAPIBase: directory.URL}, token.KeyholderScopes)
				if err != nil {
					t.Fatal(err)
				}
				source = managed.TokenSource(t.Context())
			}
			client := oauth2.NewClient(t.Context(), source)
			read := principal.DirectoryLookup(client, directory.URL)
			_, err := read(t.Context(), "users/1")
			if err == nil {
				t.Fatal("expected token refusal")
			}
			// HTTP status wins over a joined deadline, even when an outer
			// diagnostic looks like a different refusal.
			err = fmt.Errorf("key holder: scope_not_allowed (HTTP 499): %w", errors.Join(err, context.DeadlineExceeded))
			f := fillSetup(t, true)
			f.reply(nil, err)
			f.deliver(t, fillChange("reaction.created", "one", space, &api.User{Name: "users/1"}), false)
			var log bytes.Buffer
			warnings := principal.NewWarningLog(slog.New(slog.NewTextHandler(&log, nil)))
			r := principal.New(principal.Options{Logger: warnings, Lookup: func(context.Context, string) (string, error) { return "", err }})
			r.Speaker(t.Context(), &api.User{Name: "users/1"}, "")
			want := fmt.Sprintf("cause=http_%d", tc.status)
			if err := warnings.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, line := range []string{f.log.String(), log.String()} {
				if !strings.Contains(line, want) || strings.Contains(line, tc.code) {
					t.Errorf("expected %s in warning: %s", want, line)
				}
			}
		})
	}
	if status := principal.LookupHTTPStatus(errors.New("key holder: scope_not_allowed (HTTP 403)")); status != 0 {
		t.Fatalf("untyped error supplied HTTP status %d", status)
	}
}

func TestSenderFillResourceStatus(t *testing.T) {
	// A typed error body can describe a different status than the resource
	// actually returned. The warning should use only the HTTP response status.
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":404}}`)
	}))
	defer resource.Close()
	dir := t.TempDir()
	client, file := filepath.Join(dir, "client"), filepath.Join(dir, "credential")
	if err := os.WriteFile(client, []byte(`{"installed":{"client_id":"example","client_secret":"example"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"access_token":"example"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ep := config.Endpoints{ChatAPIBase: resource.URL, OAuthAuthURL: resource.URL, OAuthTokenURL: resource.URL}
	source, err := token.New(client, file, ep, token.Scopes)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := chat.New(t.Context(), source.LookupClient(t.Context()), ep)
	if err != nil {
		t.Fatal(err)
	}
	f := fillSetup(t, true)
	f.opts.SenderFill.source = lookup
	for _, kind := range []string{"message.created", "reaction.created"} {
		for i := 1; i <= 2; i++ {
			id := fmt.Sprintf("users/%d", i)
			f.deliver(t, fillChange(kind, fmt.Sprint(i), space, &api.User{Name: id}), false)
		}
		f.now = f.now.Add(11 * time.Minute)
	}
	for _, source := range []string{"message", "member"} {
		if strings.Count(f.log.String(), "source="+source+" cause=http_403") != 1 {
			t.Errorf("wrong HTTP cause for %s: %s", source, f.log.String())
		}
	}
}

func TestDirectoryCauseTransientMintStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant"}`)
	}))
	defer server.Close()
	dir, err := os.MkdirTemp("", "lookup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	holder := &http.Server{Handler: server.Config.Handler}
	go holder.Serve(ln)
	defer holder.Close()
	s, err := token.NewKeyholder(socket, config.Endpoints{ChatAPIBase: server.URL}, token.KeyholderScopes)
	if err != nil {
		t.Fatal(err)
	}
	lookup := principal.DirectoryLookup(s.LookupClient(t.Context()), server.URL)
	_, err = lookup(t.Context(), "users/1")
	if principal.LookupHTTPStatus(err) != http.StatusBadRequest || s.State() != token.Ready || errors.Is(err, token.ErrLookupCredential) {
		t.Fatal("transient mint lost its HTTP status or entered the permanent refusal path")
	}
}

func TestSenderFillNearExpiryDirectoryClass(t *testing.T) {
	const base = "https://example.invalid"
	const address = "reader@example.com"
	for _, tc := range []struct {
		name, kind     string
		fill, complete bool
		lifetime       time.Duration
		wantSenderGET  int
	}{
		{"no_fill", "message.created", false, false, 41 * time.Second, 0},
		{"message", "message.created", true, false, 41 * time.Second, 1},
		{"member", "reaction.created", true, false, 41 * time.Second, 1},
		{"complete_sender", "message.created", true, true, 39 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				client, file := filepath.Join(dir, "client"), filepath.Join(dir, "credential")
				if err := os.WriteFile(client, []byte(`{"installed":{"client_id":"example","client_secret":"example"}}`), 0600); err != nil {
					t.Fatal(err)
				}
				expiry := time.Now().Add(tc.lifetime).Format(time.RFC3339)
				if err := os.WriteFile(file, []byte(fmt.Sprintf(`{"access_token":"example","expiry":%q}`, expiry)), 0600); err != nil {
					t.Fatal(err)
				}
				ep := config.Endpoints{ChatAPIBase: base, OAuthAuthURL: base, OAuthTokenURL: base}
				s, err := token.New(client, file, ep, token.Scopes)
				if err != nil {
					t.Fatal(err)
				}
				var senderGET, directoryGET int
				transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer example" {
						t.Fatal("expected a GET with the current bearer")
					}
					resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody}
					switch r.URL.Path {
					case "/v1/people/1":
						directoryGET++
						resp.Body = io.NopCloser(strings.NewReader(`{"emailAddresses":[{"value":"reader@example.com"}]}`))
					case "/v1/" + space + "/messages/target", "/v1/" + space + "/members/1":
						senderGET++
						time.Sleep(2 * time.Second)
						resp.StatusCode = http.StatusNotFound
					default:
						t.Fatalf("unexpected request to %s", r.URL.Path)
					}
					return resp, nil
				})
				ctx := context.WithValue(t.Context(), oauth2.HTTPClient, &http.Client{Transport: transport})
				directory := principal.DirectoryLookup(s.LookupClient(ctx), base)
				resolver := principal.New(principal.Options{Owners: []string{address}, Lookup: directory})
				p := Processor{opts: Options{IdentityUserID: "users/99", Principals: resolver}}
				if tc.fill {
					reader, err := chat.New(ctx, s.LookupClient(ctx), ep)
					if err != nil {
						t.Fatal(err)
					}
					p.opts.SenderFill = NewSenderFill(reader, nil)
				}
				sender := &api.User{Name: "users/1"}
				if tc.complete {
					sender.DisplayName, sender.DomainId = "Example Reader", "example-org"
				}
				c, ctx := p.fillSender(ctx, fillChange(tc.kind, "target", space, sender), "")
				got := resolver.Speaker(ctx, historyUser(c), "")
				if senderGET != tc.wantSenderGET || directoryGET != 1 || got.Class != provenance.Owner || got.Address != address {
					t.Fatalf("sender GETs=%d directory GETs=%d class=%s address=%s", senderGET, directoryGET, got.Class, got.Address)
				}
			})
		})
	}
}
