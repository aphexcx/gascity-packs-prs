package token

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestAuthPersistsIdentity(t *testing.T) {
	for _, sub := range []string{"123456", "", "not-a-user"} {
		t.Run(sub, func(t *testing.T) {
			clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				payload, _ := json.Marshal(map[string]string{"sub": sub})
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "example", "refresh_token": "example-refresh", "scope": strings.Join(Scopes, " "), "id_token": "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"})
			})
			writeJSON(t, tokenFile, savedToken("old"))
			before, _ := os.ReadFile(tokenFile)
			err := RunAuth(t.Context(), clientFile, tokenFile, ep, Scopes, io.Discard, func(raw string) error { return browserCallback(raw, "example-code", "", "") })
			after, _ := os.ReadFile(tokenFile)
			if sub != "123456" {
				if err == nil || !strings.Contains(err.Error(), "id_token") || string(before) != string(after) {
					t.Fatalf("invalid sub accepted or old token changed: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var saved map[string]any
			_ = json.Unmarshal(after, &saved)
			if saved["identity_user_id"] != "users/123456" {
				t.Fatalf("identity not persisted: %v", saved["identity_user_id"])
			}
			source := mustSource(t, clientFile, tokenFile, ep)
			identity, ok := any(source).(interface{ IdentityUserID() string })
			if !ok || identity.IdentityUserID() != "users/123456" {
				t.Fatal("source does not expose saved identity")
			}
		})
	}
}

func TestAuthRejectsMissingIDToken(t *testing.T) {
	clientFile, tokenFile, ep := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "example", "refresh_token": "example-refresh", "scope": strings.Join(Scopes, " ")})
	})
	err := RunAuth(t.Context(), clientFile, tokenFile, ep, Scopes, io.Discard, func(raw string) error { return browserCallback(raw, "example-code", "", "") })
	if err == nil || !strings.Contains(err.Error(), "id_token") {
		t.Fatalf("missing identity accepted: %v", err)
	}
}
