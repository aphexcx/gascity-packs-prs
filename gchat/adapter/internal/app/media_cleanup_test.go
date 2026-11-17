package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
)

func TestStartupCleanupDoesNotWaitForRemoteServices(t *testing.T) {
	for _, unavailable := range []string{"credentials", "gc", "subscription"} {
		t.Run(unavailable, func(t *testing.T) {
			f := newFixture(t, unavailable != "credentials")
			if unavailable == "gc" {
				blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
				t.Cleanup(blocked.Close)
				f.cfg.Secrets.GCAPIBase = blocked.URL
			}
			if unavailable == "subscription" {
				f.google.Configure(func(g *fakegoogle.Server) { g.OperationDelay = time.Hour })
			}
			name := "spaces/room/messages/deleted"
			dir := filepath.Join(f.cfg.Endpoints.MediaDir, "room", "deleted")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "saved.txt")
			if err := os.WriteFile(path, []byte("fixture bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			l := ledger.New()
			l.Messages[name] = &ledger.MessageState{Deleted: true}
			l.Media = map[string]*ledger.MediaState{name: {DeletePending: true}}
			if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			state := "initializing"
			if unavailable == "credentials" {
				state = "needs-sign-in"
			}
			f.health(t, state, 2*time.Second)
			waitFor(t, 2*time.Second, func() bool { _, err := os.Stat(path); return errors.Is(err, os.ErrNotExist) })
			f.health(t, state, time.Second)
		})
	}
}
