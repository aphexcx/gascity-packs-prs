package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gchat-adapter/internal/config"
)

func TestSenderFillLookupCredential(t *testing.T) {
	for _, code := range []string{"grant_rejected", "invalid_grant"} {
		t.Run(code, func(t *testing.T) {
			holder := newFakeKeyholder(t)
			directory, searches := fakeDirectory(t, "reader@example.com", "people/1", http.StatusOK)
			s := managedSource(t, holder, directory.URL)
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			t.Cleanup(api.Close)
			required, err := s.Client(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			s.Invalidate()
			original, id, generation := s.current, s.IdentityUserID(), s.generation
			calls := holder.mints.Load()
			holder.code.Store(code)
			holder.status.Store(http.StatusBadRequest)
			lookup := s.LookupClient(t.Context())
			if holder.mints.Load() != calls {
				t.Fatal("constructing the lookup client refreshed the token")
			}
			_, err = lookup.Get(api.URL)
			permanent := code == "grant_rejected"
			if err == nil || errors.Is(err, ErrLookupCredential) != permanent || holder.mints.Load() != calls+1 ||
				s.State() != Ready || s.current != original || s.IdentityUserID() != id || s.generation != generation || searches.Load() != 1 {
				t.Fatal("optional refusal changed the credential or retried")
			}
			_, err = required.Get(api.URL)
			if err == nil || errors.Is(err, ErrNeedsSignIn) != permanent || errors.Is(err, ErrLookupCredential) || (s.State() == NeedsSignIn) != permanent {
				t.Fatal("required refusal changed behavior")
			}
			if permanent {
				calls = holder.mints.Load()
				_, err = lookup.Get(api.URL)
				if !errors.Is(err, ErrNeedsSignIn) || holder.mints.Load() != calls || s.State() != NeedsSignIn {
					t.Fatal("lookup escaped the existing hold")
				}
			}
		})
	}
}

func TestSenderFillSuccessfulLookupRefresh(t *testing.T) {
	holder := newFakeKeyholder(t)
	directory, searches := fakeDirectory(t, "reader@example.com", "people/1", http.StatusOK)
	s := managedSource(t, holder, directory.URL)
	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing bearer")
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(resource.Close)
	// No identity is known yet. A successful mint installs its token, even
	// when the optional resource GET then fails. Discovery itself can rebuild
	// the generation and belongs to the watcher or required work.
	resp, err := s.LookupClient(t.Context()).Get(resource.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !s.current.Valid() || s.State() != Ready || s.IdentityUserID() != "" || searches.Load() != 0 {
		t.Fatal("an optional resource refusal expired the new token or initiated identity discovery")
	}
	required, err := s.Client(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	resp, err = required.Get(resource.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if holder.mints.Load() != 1 || s.current.Valid() {
		t.Fatal("required read must reuse the refreshed token and expire it on its own 401")
	}
}

func TestSenderFillDesktopRefusalPreservesFile(t *testing.T) {
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
	}))
	t.Cleanup(issuer.Close)
	dir := t.TempDir()
	client, file := filepath.Join(dir, "client"), filepath.Join(dir, "credential")
	if err := os.WriteFile(client, []byte(`{"installed":{"client_id":"example","client_secret":"example"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	const saved = `{"access_token":"example","refresh_token":"example","identity_user_id":"users/1","expiry":"2000-01-01T00:00:00Z"}`
	if err := os.WriteFile(file, []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(client, file, config.Endpoints{ChatAPIBase: issuer.URL, OAuthAuthURL: issuer.URL, OAuthTokenURL: issuer.URL}, Scopes)
	if err != nil {
		t.Fatal(err)
	}
	original := s.current
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.LookupClient(ctx).Do(req)
	after, readErr := os.ReadFile(file)
	if !errors.Is(err, ErrLookupCredential) || readErr != nil || string(after) != saved || s.current != original || s.IdentityUserID() != "users/1" || s.State() != Ready {
		t.Fatal("refused desktop lookup changed the credential")
	}
}
