package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	api "google.golang.org/api/chat/v1"
)

func TestShutdownSavesBeforeDrainingStuckPublish(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, fakegoogle.UserID)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	f.intercept = func(r *http.Request) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			entered <- struct{}{}
			<-release
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
	}()
	stopped := false
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		if !stopped {
			<-done
		}
	})
	f.health(t, "ready", 5*time.Second)
	client := *f.client
	client.Timeout = 10 * time.Second
	published := make(chan struct{})
	go func() {
		defer close(published)
		resp, err := client.Post("http://adapter/publish", "application/json", strings.NewReader(`{"idempotency_key":"shutdown-example","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"default","conversation_id":"spaces/room","kind":"room"},"text":"hello"}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("publish did not start")
	}
	before, err := os.Stat(f.cfg.Endpoints.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cancel()
	waitFor(t, 500*time.Millisecond, func() bool {
		current, e := os.Stat(f.cfg.Endpoints.StateFile)
		return e == nil && !os.SameFile(before, current)
	})
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Until(start.Add(1500 * time.Millisecond))):
		t.Fatal("shutdown exceeded 1.5 seconds")
	}
	saved, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
	if err != nil || saved.IdentityUserID != fakegoogle.UserID || saved.Sends["shutdown-example"] == nil {
		t.Fatalf("shutdown ledger lost intent/identity: %v %+v", err, saved)
	}
	releaseOnce.Do(func() { close(release) })
	<-published
}
