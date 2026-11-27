package token

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func redirectDestination(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("unexpected request at redirect destination")
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// Only the fixture origin is reachable. The production client must replace
// this test redirect policy while retaining the supplied transport.
func guardedOriginClient(t *testing.T, origin string) *http.Client {
	t.Helper()
	base, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != base.Host {
				if r.Body != nil {
					r.Body.Close()
				}
				t.Error("attempted request outside fixture origin")
				return nil, errors.New("unexpected request")
			}
			return http.DefaultTransport.RoundTrip(r)
		}),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			t.Error("credential request reached the caller's redirect policy")
			return errors.New("unexpected redirect")
		},
	}
}

func tokenFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTokenEndpointRefusesRedirect(t *testing.T) {
	target := redirectDestination(t)
	for _, tc := range []struct {
		action string
		status int
		body   string
	}{
		{"refresh", 301, ""}, {"refresh", 302, ""}, {"refresh", 303, ""},
		{"refresh", 307, ""}, {"refresh", 308, ""},
		{"refresh_permanent_error_body", 302, `{"error":"invalid_grant"}`},
		{"exchange", 307, ""},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.action, tc.status), func(t *testing.T) {
			var calls atomic.Int32
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", target)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			})
			expired := savedToken("expired")
			expired.Expiry = time.Now().Add(-time.Hour)
			writeJSON(t, tokenFile, expired)
			before := tokenFileBytes(t, tokenFile)
			custom := guardedOriginClient(t, ep.OAuthTokenURL)
			ctx := context.WithValue(t.Context(), oauth2.HTTPClient, custom)
			if tc.action == "exchange" {
				err := RunAuth(ctx, clientFile, tokenFile, ep, testScopes, io.Discard,
					func(raw string) error { return browserCallback(raw, "example-code", "", "") })
				if err == nil || calls.Load() != 1 {
					t.Fatalf("expected failed exchange and one attempt, calls=%d", calls.Load())
				}
			} else {
				source := mustSource(t, clientFile, tokenFile, ep)
				_, err := source.Client(ctx)
				if err == nil || errors.Is(err, ErrNeedsSignIn) || source.State() == NeedsSignIn || calls.Load() != 3 {
					t.Fatalf("expected retryable refusal, err=%v state=%s calls=%d", err, source.State(), calls.Load())
				}
				if source.current.AccessToken != expired.AccessToken || source.current.RefreshToken != expired.RefreshToken {
					t.Fatal("refresh changed the current credential")
				}
			}
			if !bytes.Equal(before, tokenFileBytes(t, tokenFile)) {
				t.Fatal("token file changed")
			}
		})
	}
}

func TestManagedIdentityRefusesRedirect(t *testing.T) {
	target := redirectDestination(t)
	differentHost, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	differentHost.Host = net.JoinHostPort("localhost", differentHost.Port())
	for _, path := range []string{"directory", "spaces", "members"} {
		for _, dest := range []struct{ name, location string }{
			{"same_host", target}, {"different_host", differentHost.String()},
		} {
			for _, status := range []int{301, 302, 303, 307, 308} {
				t.Run(fmt.Sprintf("%s/%s/%d", path, dest.name, status), func(t *testing.T) {
					var redirects atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if path == "members" && r.URL.Path == "/v1/spaces" {
							io.WriteString(w, `{"spaces":[{"name":"spaces/example","spaceType":"DIRECT_MESSAGE"}]}`)
							return
						}
						redirects.Add(1)
						w.Header().Set("Location", dest.location)
						w.WriteHeader(status)
					}))
					defer server.Close()
					previous := http.DefaultClient
					http.DefaultClient = guardedOriginClient(t, server.URL)
					defer func() { http.DefaultClient = previous }()
					k := &keyholder{peopleBase: server.URL, chatBase: server.URL}
					identify := k.identifyFromMemberships
					if path == "directory" {
						identify = k.identifyFromDirectory
					}
					_, err := identify(t.Context(), savedToken("fixture"), "user@example.invalid")
					if err == nil || redirects.Load() != 1 {
						t.Fatalf("expected failed identity lookup, calls=%d", redirects.Load())
					}
					if path != "members" && !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
						t.Fatalf("lost identity lookup status: %v", err)
					}
				})
			}
		}
	}
}
