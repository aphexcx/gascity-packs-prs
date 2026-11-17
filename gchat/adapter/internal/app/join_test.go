package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/token"
)

// Pin: a real application start and restart, then a forced standing-source
// refresh, ask only for the existing list. The CLI auth-check pin is separate.
func TestNothingAsksForTheWriteScopeAtStart(t *testing.T) {
	f, k := managedFixture(t)
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }, RetryDelay: time.Millisecond})
		}()
		f.health(t, "ready", 5*time.Second)
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("shutdown timed out")
		}
	}
	s, err := NewSource(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefreshNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.scopes) < 3 {
		t.Fatalf("mint count=%d", len(k.scopes))
	}
	for _, scopes := range k.scopes {
		if !reflect.DeepEqual(scopes, token.KeyholderScopes) {
			t.Fatalf("unexpected scopes: %v", scopes)
		}
	}
}

func TestJoinRefusalDoesNotNotify(t *testing.T) {
	f, k := managedFixture(t)
	var notices atomic.Int32
	f.startOptions(t, Options{Notify: func(context.Context, string, string) error { notices.Add(1); return nil }})
	f.health(t, "ready", 5*time.Second)
	before := notices.Load()
	for _, code := range []string{"delegation_not_authorized", "scope_not_allowed", "invalid_scope", "grant_rejected", "subject_not_found", "key_rejected", "bad_request"} {
		k.mu.Lock()
		k.joinRefusal = code
		k.mu.Unlock()
		res, err := f.client.Post("http://adapter/join", "application/json", strings.NewReader(`{"check":true}`))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		err = json.NewDecoder(res.Body).Decode(&v)
		res.Body.Close()
		if err != nil || res.StatusCode != 200 || v["result"] != "permission_missing" {
			t.Fatalf("refusal %s: HTTP %d %v %v", code, res.StatusCode, v, err)
		}
		f.health(t, "ready", time.Second)
	}
	if notices.Load() != before {
		t.Fatal("join permission refusal generated an operator notice")
	}
}
