package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/neversilent"
	api "google.golang.org/api/chat/v1"
)

func TestNeverSilentStartsBeforeRegistration(t *testing.T) {
	f := newFixture(t, true)
	f.cfg.NeverSilent.Enabled = true
	f.cfg.NeverSilent.OperatorContext = "example-operator"
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/example"] = &api.Space{Name: "spaces/example", SpaceType: "DIRECT_MESSAGE"}
	})
	var registrations atomic.Int64
	gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/extmsg/adapters") {
			registrations.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer gc.Close()
	f.cfg.Secrets.GCAPIBase = gc.URL
	mail := filepath.Join(t.TempDir(), "mail")
	writeFile(t, os.Getenv("GC_BIN"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+mail+"'\n")
	old := time.Now().Add(-4 * time.Minute)
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: f.cfg.Endpoints.StateFile + ".never-silent.json",
		Now: func() time.Time { return old }, Post: func(context.Context, neversilent.Message, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Track(neversilent.Message{ID: "example-message", Conversation: "spaces/example", Created: old}); err != nil {
		t.Fatal(err)
	}
	if err := e.Result("example-message", "unreachable", nil); err != nil {
		t.Fatal(err)
	}
	counts := func() (int, int) {
		posts := 0
		f.google.Configure(func(g *fakegoogle.Server) {
			for _, m := range g.Messages {
				if m.Text == neversilent.DownLine {
					posts++
				}
			}
		})
		b, _ := os.ReadFile(mail)
		return posts, strings.Count(string(b), "--context example-operator")
	}
	for _, restart := range []string{"first-restart", "second-restart"} {
		t.Run(restart, func(t *testing.T) {
			before := registrations.Load()
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
			}()
			defer func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(5 * time.Second):
					t.Error("adapter did not stop")
				}
			}()
			// Wait through a complete production poll interval even on the second
			// restart: persisted marks must prevent both effects from repeating.
			time.Sleep(6 * time.Second)
			deadline := time.Now().Add(6 * time.Second)
			for time.Now().Before(deadline) {
				posts, mails := counts()
				if posts == 1 && mails == 1 {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			posts, mails := counts()
			t.Logf("registration 503s=%d owner lines=%d operator mails=%d", registrations.Load()-before, posts, mails)
			if registrations.Load() == before {
				t.Error("registration was not attempted")
			}
			if posts != 1 || mails != 1 {
				t.Errorf("got owner lines=%d operator mails=%d; want 1, 1", posts, mails)
			}
			f.health(t, "initializing", time.Second)
		})
	}
}
