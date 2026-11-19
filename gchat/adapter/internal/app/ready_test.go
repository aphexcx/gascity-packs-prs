package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestLegacyIdentityHoldResumesInterruptedOutbound(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish", true: "upload"}[upload], func(t *testing.T) {
			f := newFixture(t, true) // legacy token: no identity in either credential or ledger
			// Route gc's recording callback back through the running adapter,
			// as the real city's service proxy does for an interrupted command.
			gcTarget, _ := url.Parse(f.cfg.Secrets.GCAPIBase)
			gcProxy := httputil.NewSingleHostReverseProxy(gcTarget)
			adapterTarget, _ := url.Parse("http://adapter")
			adapterProxy := httputil.NewSingleHostReverseProxy(adapterTarget)
			adapterProxy.Transport = f.client.Transport
			prefix := f.cfg.Secrets.ServiceURLPrefix
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, prefix+"/") {
					r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
					adapterProxy.ServeHTTP(w, r)
					return
				}
				gcProxy.ServeHTTP(w, r)
			}))
			defer gateway.Close()
			f.cfg.Secrets.GCAPIBase = gateway.URL
			f.google.Configure(func(g *fakegoogle.Server) {
				g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
			})
			p := wire.PublishRequest{SessionID: "example", IdempotencyKey: "interrupted", Text: "recover me", Conversation: wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "default", ConversationID: "spaces/room", Kind: "room"}}
			f.gc.Bind(p.Conversation, p.SessionID)
			send := &ledger.SendState{Key: p.IdempotencyKey, Space: p.Conversation.ConversationID, Request: &p, RecordPending: true, Parts: []ledger.SendPart{{RequestID: "interrupted-1", Text: p.Text}}}
			if upload {
				if err := os.MkdirAll(f.cfg.Endpoints.UploadRoot, 0700); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.cfg.Endpoints.UploadRoot, "recovered.txt")
				if err := os.WriteFile(path, []byte("recovered file"), 0600); err != nil {
					t.Fatal(err)
				}
				path, err := filepath.EvalSymlinks(path)
				if err != nil {
					t.Fatal(err)
				}
				send.Upload = &ledger.SendUpload{Path: path}
			}
			l := ledger.New()
			l.Sends[p.IdempotencyKey] = send
			if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
				t.Fatal(err)
			}
			f.start(t)
			f.health(t, "catching-up", 5*time.Second)
			waitFor(t, 20*time.Second, func() bool {
				data, err := os.ReadFile(f.cfg.Endpoints.StateFile)
				if err != nil {
					return false
				}
				var saved ledger.Ledger
				if json.Unmarshal(data, &saved) != nil {
					return false
				}
				s := saved.Sends[p.IdempotencyKey]
				if s != nil && !s.RecordPending && !s.Recorded {
					t.Fatalf("recovery ended without recording: receipt=%+v reason=%q", s.Receipt, s.RecordingReason)
				}
				return s != nil && s.Recorded && !s.RecordPending && s.Receipt != nil && s.Receipt.Delivered
			})
			f.health(t, "ready", 10*time.Second)
		})
	}
}

// TestRetryingMessageLeavesHealthReadyAndOutboundRecoveryRunning: a message
// that catch-up recovered and that gc keeps answering "not delivered" is the
// poll's to retry (gp-dl26, ruling A). It must not keep health at catching-up,
// which would also pause outbound recovery (only an identity-discovery hold
// permits recovery before health is ready) for up to the retry bound.
func TestRetryingMessageLeavesHealthReadyAndOutboundRecoveryRunning(t *testing.T) {
	f := newFixture(t, true)
	now := time.Now().UTC()
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		g.Messages["spaces/room/messages/one"] = &api.Message{Name: "spaces/room/messages/one", Text: "hello", CreateTime: now.Add(-time.Minute).Format(time.RFC3339Nano), Sender: &api.User{Name: "users/222", Type: "HUMAN"}, Thread: &api.Thread{Name: "spaces/room/threads/one"}, Space: g.Spaces["spaces/room"]}
	})
	f.gc.Configure(func(s *fakegc.Server) { s.Script = slices.Repeat([]wire.Delivery{{Status: "failed"}}, 1000) })
	l := ledger.New()
	l.IdentityUserID = fakegoogle.UserID
	l.Spaces["spaces/room"] = &ledger.SpaceState{Kind: "room", Wake: "all", MessageCursor: now.Add(-time.Hour), EventCursor: now.Add(-time.Hour)}
	// An outbound reply whose transcript record is still owed: retryPending's work.
	p := wire.PublishRequest{SessionID: "example", IdempotencyKey: "owed", Conversation: wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "default", ConversationID: "spaces/room", Kind: "room"}}
	l.Sends[p.IdempotencyKey] = &ledger.SendState{Key: p.IdempotencyKey, Space: p.Conversation.ConversationID, Request: &p, RecordPending: true, Receipt: &wire.PublishReceipt{Delivered: true, MessageID: "spaces/room/messages/reply", Conversation: p.Conversation}}
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	f.health(t, "ready", 10*time.Second)
	if posts := len(f.gc.Inbound()); posts < 4 {
		t.Fatalf("catch-up POSTed the recovered message %d times, want its first four", posts)
	}
	var deliveries []struct{ ID, Status, Reason string }
	if err := json.Unmarshal(f.status(t)["deliveries"], &deliveries); err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 || deliveries[0].ID != "spaces/room/messages/one" || deliveries[0].Status != "intent" || deliveries[0].Reason != "retry" {
		t.Fatalf("the message is not retrying while health is ready: %+v", deliveries)
	}
	waitFor(t, 15*time.Second, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return slices.ContainsFunc(f.requests, func(r string) bool { return strings.HasSuffix(r, "/extmsg/outbound") })
	})
}
