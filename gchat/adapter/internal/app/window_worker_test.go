package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	api "google.golang.org/api/chat/v1"
)

// The application starts the window worker beside the notice ticker. The poll
// tick is an hour here and no second event arrives, so only that worker can
// post a held room turn at its deadline.
func TestApplicationPostsAHeldTurnAtItsDeadline(t *testing.T) {
	dir := deliveryCommands(t)
	f := newFixture(t, true)
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	f.cfg.Secrets.GCAPIBase = f.gc.URL()
	window := "1s"
	f.cfg.Settings.CoalesceWindow = &window
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	l := ledger.New()
	l.IdentityUserID = fakegoogle.UserID
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "mode"), "ok")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryDelay: time.Millisecond, pollInterval: time.Hour})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("application failed to stop")
		}
	})
	f.health(t, "ready", 5*time.Second)
	data := fmt.Sprintf(`{"message":{"name":"spaces/room/messages/held","text":"hello","sender":{"name":"users/222","type":"HUMAN"},"createTime":%q,"thread":{"name":"spaces/room/threads/one"}}}`, time.Now().UTC().Format(time.RFC3339Nano))
	publish, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	if _, err := f.pub.Publish(publish, &pubsub.Message{Data: []byte(data), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": "held"}}).Get(publish); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 10*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
}
