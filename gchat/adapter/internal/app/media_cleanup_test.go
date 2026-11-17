package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
	api "google.golang.org/api/chat/v1"
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

func TestCleanupAllowsReloadToCancelDownloads(t *testing.T) {
	f := newFixture(t, true)
	if err := canonicalize(f.cfg); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}))
	defer remote.Close()
	f.cfg.Endpoints.ChatAPIBase = remote.URL
	client, err := chat.New(context.Background(), http.DefaultClient, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	downloadContext, cancelDownload := context.WithCancel(context.Background())
	defer cancelDownload()
	downloadDone := make(chan struct{})
	m := &api.Message{Name: "spaces/room/messages/downloading", Attachment: []*api.Attachment{{
		Name: "spaces/room/messages/downloading/attachments/file", ContentName: "saved.txt",
		AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/file"},
	}}}
	go func() {
		inbound.NormalizeContent(downloadContext, client, f.cfg.Endpoints, m)
		close(downloadDone)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("download did not enter the held request")
	}
	a := &application{cfg: f.cfg, l: ledger.New(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.s, err = server.New(server.Options{Config: f.cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: f.cfg.Endpoints.StateFile})
	if err != nil {
		t.Fatal(err)
	}
	defer a.s.Close()
	a.loaded.Store(true)
	a.l.Media = map[string]*ledger.MediaState{"spaces/room/messages/deleted": {DeletePending: true}}
	snapshot := make(chan struct{}, 1)
	store := &inbound.MediaStore{
		Snapshot: func() (*ledger.Ledger, error) {
			select {
			case snapshot <- struct{}{}:
			default:
			}
			return a.l.Clone(), nil
		},
		Update: func(func(*ledger.Ledger)) error { return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	cleanupDone := make(chan struct{})
	go func() { a.recoverMedia(ctx, f.cfg.Endpoints.MediaDir, store); close(cleanupDone) }()
	defer func() { cancel(); <-cleanupDone }()
	select {
	case <-snapshot:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not reach its local checkpoint")
	}
	// Credential and settings reloads change health before cancelling their
	// generation. That transition must remain possible while cleanup waits.
	reloadDone := make(chan struct{})
	go func() {
		a.health("initializing", "credentials replaced; rebuilding clients")
		cancelDownload()
		close(reloadDone)
	}()
	defer func() { cancel(); <-reloadDone; <-downloadDone }()
	select {
	case <-downloadDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup prevented reload from cancelling its stalled download")
	}
}
