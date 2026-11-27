package token

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestAuthenticatedClientRefusesRedirect(t *testing.T) {
	for _, client := range []string{"required", "lookup"} {
		for _, target := range []struct{ name, location string }{
			{"other_origin", "https://second.invalid/target"},
			{"same_origin", "/target"},
		} {
			for _, status := range []int{301, 302, 303, 307, 308} {
				name := fmt.Sprintf("%s/%s/%d/", client, target.name, status)
				t.Run(name+"GET", func(t *testing.T) {
					checkRefusedRedirect(t, client, http.MethodGet, target.location, status)
				})
				if status == 307 || status == 308 {
					t.Run(name+"POST", func(t *testing.T) {
						checkRefusedRedirect(t, client, http.MethodPost, target.location, status)
					})
				}
			}
		}
	}
}

func checkRefusedRedirect(t *testing.T, clientName, method, location string, status int) {
	t.Helper()
	const first = "https://first.invalid/resource"
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Body != nil {
			defer r.Body.Close()
		}
		if calls != 1 || r.URL.String() != first || r.Method != method {
			return nil, fmt.Errorf("unexpected request %d to %s", calls, r.URL)
		}
		if method == http.MethodPost {
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != "fixture body" {
				t.Fatal("first request did not carry the expected body")
			}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Location": {location}}, Body: http.NoBody}, nil
	})
	ctx := context.WithValue(t.Context(), oauth2.HTTPClient, &http.Client{Transport: transport})
	source := &Source{state: Ready, current: &oauth2.Token{AccessToken: "fixture"}}
	var client *http.Client
	var err error
	if clientName == "required" {
		client, err = source.Client(ctx)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		client = source.LookupClient(ctx)
	}
	if client.CheckRedirect == nil {
		t.Fatal("authenticated client has no redirect policy")
	}
	var body io.Reader
	if method == http.MethodPost {
		body = strings.NewReader("fixture body")
	}
	req, err := http.NewRequestWithContext(ctx, method, first, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 1 || resp.StatusCode != status || resp.Header.Get("Location") != location {
		t.Fatalf("got %d requests, status %d, Location %q", calls, resp.StatusCode, resp.Header.Get("Location"))
	}
}
