package token

import (
	"context"
	"errors"
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
	t.Run("required/resume_incomplete/308/PUT", func(t *testing.T) {
		checkRefusedRedirect(t, "required", http.MethodPut, "", http.StatusPermanentRedirect)
	})
}

func checkRefusedRedirect(t *testing.T, clientName, method, location string, status int) {
	t.Helper()
	const first = "https://first.invalid/resource"
	calls := 0
	responseText := `{"error":{"code":404}}`
	if location == "" {
		responseText = "upload incomplete"
	}
	wireBody := &closeTrackingBody{Reader: strings.NewReader(responseText)}
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Body != nil {
			defer r.Body.Close()
		}
		if calls != 1 || r.URL.String() != first || r.Method != method {
			return nil, fmt.Errorf("unexpected request %d to %s", calls, r.URL)
		}
		if method != http.MethodGet {
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != "fixture body" {
				t.Fatal("first request did not carry the expected body")
			}
		}
		header := make(http.Header)
		header.Set("Retry-After", "60")
		if location == "" {
			header.Set("Range", "bytes=0-11")
		} else {
			header.Set("Location", location)
		}
		return &http.Response{StatusCode: status, Header: header, Body: wireBody, ContentLength: int64(len(responseText))}, nil
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
	// Stop any attempted follow-up before it reaches the authenticated transport.
	authenticated := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != first {
			if r.Body != nil {
				r.Body.Close()
			}
			return nil, errors.New("client attempted a redirect")
		}
		return authenticated.RoundTrip(r)
	})
	var body io.Reader
	if method != http.MethodGet {
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
	if resp.Header.Get("Retry-After") != "60" {
		t.Fatal("lost Retry-After")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if location == "" {
		if resp.Header.Get("Range") != "bytes=0-11" || string(got) != responseText || wireBody.closed {
			t.Fatal("changed a resumable upload response")
		}
	} else if len(got) != 0 || !wireBody.closed || resp.ContentLength != 0 {
		t.Fatal("refused response must have an empty body and close the wire body")
	}
}
