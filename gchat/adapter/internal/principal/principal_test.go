package principal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gchat-adapter/internal/provenance"
	api "google.golang.org/api/chat/v1"
)

func TestSpeakerClasses(t *testing.T) {
	directory := map[string]string{"users/1": "Robert@Example.com", "users/2": "alice@example.com", "users/3": "bob@partner.com"}
	lookups := 0
	r := New(Options{Owners: []string{"robert@example.com", "users/4"}, OrganizationDomain: "Example.com", Lookup: func(_ context.Context, id string) (string, error) {
		lookups++
		if a, ok := directory[id]; ok {
			return a, nil
		}
		return "", nil
	}})
	cases := []struct {
		user *api.User
		want provenance.Speaker
	}{
		{&api.User{Name: "users/1", DisplayName: "Robert", Type: "HUMAN"}, provenance.Speaker{Name: "Robert", Address: "Robert@Example.com", Class: provenance.Owner}},
		{&api.User{Name: "users/2", DisplayName: "Alice", Type: "HUMAN"}, provenance.Speaker{Name: "Alice", Address: "alice@example.com", Class: provenance.Organization}},
		{&api.User{Name: "users/3", DisplayName: "Robert", Type: "HUMAN", DomainId: "other"}, provenance.Speaker{Name: "Robert", Address: "bob@partner.com", Class: provenance.External}},
		// An owner configured by id, outside the directory.
		{&api.User{Name: "users/4", DisplayName: "CFO", Type: "HUMAN"}, provenance.Speaker{Name: "CFO", Address: "users/4", Class: provenance.Owner}},
		// Same Workspace domain as the identity, no address: organization.
		{&api.User{Name: "users/5", DisplayName: "Colleague", Type: "HUMAN", DomainId: "dom"}, provenance.Speaker{Name: "Colleague", Address: "users/5", Class: provenance.Organization}},
		{&api.User{Name: "users/6", DisplayName: "Guest", Type: "HUMAN", DomainId: "elsewhere"}, provenance.Speaker{Name: "Guest", Address: "users/6", Class: provenance.External}},
		// Bots and deleted users are never looked up and never principals.
		{&api.User{Name: "users/7", DisplayName: "Deploy", Type: "BOT", DomainId: "dom"}, provenance.Speaker{Name: "Deploy", Address: "users/7", Class: provenance.External, Bot: true}},
		{&api.User{Name: "users/8", Type: "HUMAN", IsAnonymous: true, DomainId: "dom"}, provenance.Speaker{Name: "deleted user", Address: "users/8", Class: provenance.External}},
		{nil, provenance.Speaker{Class: provenance.External}},
	}
	for _, tc := range cases {
		if got := r.Speaker(context.Background(), tc.user, "dom"); got != tc.want {
			t.Errorf("Speaker(%+v) = %+v, want %+v", tc.user, got, tc.want)
		}
	}
	if lookups != 6 {
		t.Fatalf("lookups = %d, want one per human sender", lookups)
	}
	// Answers are cached, including "not in the directory".
	r.Speaker(context.Background(), &api.User{Name: "users/1", Type: "HUMAN"}, "")
	r.Speaker(context.Background(), &api.User{Name: "users/5", Type: "HUMAN"}, "")
	if lookups != 6 {
		t.Fatalf("lookups = %d after cached senders", lookups)
	}
}

func TestUnknownIdentityDomainIsExternal(t *testing.T) {
	r := New(Options{})
	got := r.Speaker(context.Background(), &api.User{Name: "users/5", DisplayName: "Colleague", Type: "HUMAN", DomainId: "dom"}, "")
	if got.Class != provenance.External || got.Address != "users/5" {
		t.Fatalf("Speaker = %+v", got)
	}
}

func TestStalledDirectoryFallsBackWithinInboundBudget(t *testing.T) {
	started := time.Now()
	r := New(Options{Lookup: func(ctx context.Context, _ string) (string, error) {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(started) > 31*time.Second {
			t.Fatal("directory lookup has no 30-second deadline")
		}
		<-ctx.Done()
		return "", ctx.Err()
	}})
	got := r.Speaker(context.Background(), &api.User{Name: "users/2", Type: "HUMAN"}, "")
	if got.Class != provenance.External || got.Address != "users/2" {
		t.Fatalf("timeout fallback = %+v", got)
	}
	if elapsed := time.Since(started); elapsed > 32*time.Second {
		t.Fatalf("directory held delivery for %s", elapsed)
	}
	// The failure cache lets the next delivery proceed without another wait.
	r.Speaker(context.Background(), &api.User{Name: "users/2", Type: "HUMAN"}, "")
	if elapsed := time.Since(started); elapsed > 32*time.Second {
		t.Fatalf("cached failure held delivery for %s", elapsed)
	}
}

func TestFailedLookupRetriesAfterTTL(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	calls := 0
	r := New(Options{Lookup: func(context.Context, string) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("HTTP 503")
		}
		return "alice@example.com", nil
	}, OrganizationDomain: "example.com", Now: func() time.Time { return now }, FailureTTL: time.Minute})
	u := &api.User{Name: "users/2", Type: "HUMAN"}
	if got := r.Speaker(context.Background(), u, ""); got.Class != provenance.External || calls != 1 {
		t.Fatalf("first: %+v, calls %d", got, calls)
	}
	if got := r.Speaker(context.Background(), u, ""); calls != 1 || got.Address != "users/2" {
		t.Fatalf("within TTL: %+v, calls %d", got, calls)
	}
	now = now.Add(2 * time.Minute)
	if got := r.Speaker(context.Background(), u, ""); got.Class != provenance.Organization || calls != 2 {
		t.Fatalf("after TTL: %+v, calls %d", got, calls)
	}
}

func TestDirectoryLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, src := range r.URL.Query()["sources"] {
			if src != "READ_SOURCE_TYPE_PROFILE" && src != "READ_SOURCE_TYPE_CONTACT" && src != "READ_SOURCE_TYPE_DOMAIN_CONTACT" {
				// Google's answer to a DirectorySourceType here.
				w.WriteHeader(400)
				return
			}
		}
		switch r.URL.Path {
		case "/v1/people/2":
			if r.URL.Query().Get("personFields") != "emailAddresses" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"emailAddresses":[{"value":"old@example.com"},{"value":"alice@example.com","metadata":{"primary":true}}]}`))
		case "/v1/people/3":
			w.WriteHeader(404)
		case "/v1/people/4":
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	lookup := DirectoryLookup(srv.Client(), srv.URL+"/")
	ctx := context.Background()
	if a, err := lookup(ctx, "users/2"); err != nil || a != "alice@example.com" {
		t.Fatalf("primary: %q %v", a, err)
	}
	if a, err := lookup(ctx, "users/3"); err != nil || a != "" {
		t.Fatalf("outside the directory: %q %v", a, err)
	}
	if _, err := lookup(ctx, "users/4"); err == nil {
		t.Fatal("no address should be an error")
	}
	if _, err := lookup(ctx, "users/5"); err == nil {
		t.Fatal("503 should be an error")
	}
	if _, err := lookup(ctx, "spaces/x"); err == nil {
		t.Fatal("a non-user id should be refused")
	}
}
