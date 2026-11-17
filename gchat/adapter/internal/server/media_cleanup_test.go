package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/media"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func cleanupHistory(t *testing.T, client *http.Client) (*fixture, *inbound.Processor, *api.Message) {
	t.Helper()
	f := setup(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Endpoints.MediaDir = filepath.Join(dir, "media")
	f.s.SetConfig(*f.cfg)
	c, err := chat.New(context.Background(), client, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	f.options.Chat = c
	f.s.SetClients(c, f.options.GC)
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: c, GC: f.options.GC})
	if err != nil {
		t.Fatal(err)
	}
	m := &api.Message{Name: "spaces/room/messages/history", Text: "example", CreateTime: "2026-10-22T10:00:00Z", Space: &api.Space{Name: "spaces/room", SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", Type: "HUMAN"}}
	m.Attachment = []*api.Attachment{{Name: m.Name + "/attachments/file", ContentName: "report.txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/report"}}}
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Messages[m.Name] = m
		g.Media["spaces/room/media/report"] = []byte("fixture bytes")
	})
	return f, p, m
}

func TestHistoryReadSavesAreRemovedToo(t *testing.T) {
	for _, mode := range []string{"unseen", "own", "withheld", "edit"} {
		t.Run(mode, func(t *testing.T) {
			f, p, m := cleanupHistory(t, http.DefaultClient)
			if mode == "own" {
				m.Sender.Name = fakegoogle.UserID
			}
			if mode == "withheld" {
				f.cfg.Settings.Defaults.Wake = "mentions"
			}
			if mode == "own" || mode == "withheld" {
				if _, err := p.HandleChange(context.Background(), puller.Change{Kind: "message.created", Space: "spaces/room", Message: m}); err != nil {
					t.Fatal(err)
				}
			}
			var paths []string
			for i := 0; i < 2; i++ {
				code, raw, _ := f.call(t, "/read?space=spaces/room", nil)
				messages := decode[[]wire.InboundMessage](t, raw)
				if code != 200 || len(messages) != 1 || len(messages[0].Attachments) != 1 {
					t.Fatal("history failed")
				}
				u, err := url.Parse(messages[0].Attachments[0].URL)
				if err != nil || u.Scheme != "file" {
					t.Fatal("history did not save")
				}
				paths = append(paths, u.Path)
			}
			if paths[0] == paths[1] {
				t.Fatal("history copy policy changed")
			}
			kind := "message.deleted"
			if mode == "edit" {
				kind = "message.updated"
				m.LastUpdateTime = "2026-10-22T10:01:00Z"
				m.Attachment = nil
			}
			out, err := p.HandleChange(context.Background(), puller.Change{Kind: kind, Space: "spaces/room", Message: m})
			if err != nil || out != puller.Ack {
				t.Fatal("change failed")
			}
			for _, path := range paths {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("history file remains")
				}
			}
		})
	}
}

type heldCleanupBody struct {
	io.ReadCloser
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *heldCleanupBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered); <-b.release })
	return b.ReadCloser.Read(p)
}

func TestSaveRacingADeletionLeavesNoFile(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var held sync.Once
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		response, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && strings.Contains(r.URL.Path, "/media/") {
			held.Do(func() {
				response.Body = &heldCleanupBody{ReadCloser: response.Body, entered: entered, release: release}
			})
		}
		return response, err
	})}
	f, p, m := cleanupHistory(t, client)
	readDone := make(chan error, 1)
	go func() {
		response, err := http.Get(f.url + "/read?space=spaces/room")
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		readDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	deleteDone := make(chan error, 1)
	go func() {
		_, err := p.HandleChange(context.Background(), puller.Change{Kind: "message.deleted", Space: "spaces/room", Message: &api.Message{Name: m.Name}})
		deleteDone <- err
	}()
	// Observe the durable tombstone while the download is held open.
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		state := f.l.Messages[m.Name]
		deleted := state != nil && state.Deleted
		f.mu.Unlock()
		if deleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tombstone was not persisted")
		}
		time.Sleep(time.Millisecond)
	}
	unblock()
	for _, done := range []chan error{readDone, deleteDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("operation stalled")
		}
	}
	if n, err := media.Usage(f.cfg.Endpoints.MediaDir); err != nil || n != 0 {
		t.Fatal("racing save survived deletion")
	}
	// Google history can return a stale snapshot even after the event arrived.
	f.call(t, "/read?space=spaces/room", nil)
	if n, err := media.Usage(f.cfg.Endpoints.MediaDir); err != nil || n != 0 {
		t.Fatal("stale history resurrected deleted media")
	}
}
