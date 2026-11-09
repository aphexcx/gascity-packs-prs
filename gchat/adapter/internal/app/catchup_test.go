package app

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/events"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
	"google.golang.org/api/option"
	workspace "google.golang.org/api/workspaceevents/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatchupCadence(t *testing.T) {
	for _, held := range []bool{false, true} {
		name := "steady"
		if held {
			name = "held space"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			synctest.Test(t, func(t *testing.T) {
				cfg := &config.Config{Endpoints: config.Endpoints{StateFile: path, ChatAPIBase: "https://example.invalid"}}
				a := &application{cfg: cfg, l: ledger.New(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
				a.l.IdentityUserID = "users/123456"
				var lists, messages, history atomic.Int32
				var denied atomic.Bool
				denied.Store(held)
				hc := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					body, code := "{}", 200
					switch {
					case r.URL.Path == "/v1/subscriptions":
						data, _ := json.Marshal(&workspace.ListSubscriptionsResponse{Subscriptions: []*workspace.Subscription{{Name: "subscriptions/recovered", State: "ACTIVE", TargetResource: events.Target, EventTypes: events.EventTypes, PayloadOptions: &workspace.PayloadOptions{IncludeResource: true}, NotificationEndpoint: &workspace.NotificationEndpoint{PubsubTopic: "projects/example/topics/chat"}, ExpireTime: time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339Nano)}}})
						body = string(data)
					case r.URL.Path == "/v1/spaces":
						lists.Add(1)
						body = `{"spaces":[{"name":"spaces/room","spaceType":"SPACE"}]}`
					case strings.HasSuffix(r.URL.Path, "/messages"):
						messages.Add(1)
						if denied.Load() {
							code = 403
							body = `{"error":{"code":403,"message":"not a member","status":"PERMISSION_DENIED"}}`
						}
					case strings.HasSuffix(r.URL.Path, "/spaceEvents"):
						history.Add(1)
					default:
						t.Errorf("unexpected request: %s", r.URL)
					}
					return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
				c, err := chat.New(t.Context(), hc, cfg.Endpoints)
				if err != nil {
					t.Fatal(err)
				}
				g := gcclient.New("https://example.invalid", "examplecity")
				a.s, err = server.New(server.Options{Config: cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: path, Logger: a.log})
				if err != nil {
					t.Fatal(err)
				}
				defer a.s.Close()
				p, err := inbound.New(inbound.Options{Config: cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: path, Chat: c, GC: g, Logger: a.log})
				if err != nil {
					t.Fatal(err)
				}
				r, err := catchup.New(catchup.Options{Config: cfg, Ledger: a.l, LedgerMu: &a.mu, LedgerPath: path, Chat: c, Processor: p, Logger: a.log})
				if err != nil {
					t.Fatal(err)
				}
				service, err := workspace.NewService(t.Context(), option.WithHTTPClient(hc))
				if err != nil {
					t.Fatal(err)
				}
				m, err := events.New(events.Options{Service: service, Topic: "projects/example/topics/chat", Ledger: a.l, LedgerMu: &a.mu, LedgerPath: path, Log: a.log})
				if err != nil {
					t.Fatal(err)
				}
				if err = r.Freeze(t.Context(), time.Now()); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var caught atomic.Bool
				done := make(chan error, 1)
				go func() { done <- a.catchup(ctx, r, m, &caught) }()
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if !caught.Load() {
					t.Fatal("initial recovery did not settle")
				}
				beforeLists, beforeMessages, beforeHistory := lists.Load(), messages.Load(), history.Load()
				denied.Store(false)
				time.Sleep(10 * time.Minute)
				synctest.Wait()
				if lists.Load() != beforeLists || messages.Load() != beforeMessages || history.Load() != beforeHistory {
					t.Fatalf("steady state polled Chat: lists %d->%d messages %d->%d events %d->%d", beforeLists, lists.Load(), beforeMessages, messages.Load(), beforeHistory, history.Load())
				}
				time.Sleep(50 * time.Minute)
				synctest.Wait()
				if lists.Load() != beforeLists+1 {
					t.Fatalf("hourly inventory calls=%d want 1", lists.Load()-beforeLists)
				}
				intervals := int32(1)
				if held {
					intervals = 2
				} // finish the held interval, then the new sweep interval
				if messages.Load() != beforeMessages+intervals || history.Load() != beforeHistory+intervals {
					t.Fatalf("hourly sweep calls: messages %d->%d events %d->%d", beforeMessages, messages.Load(), beforeHistory, history.Load())
				}
				if a.s.Health().Reason != "hourly history sweep" {
					t.Fatalf("hourly sweep not named in status: %+v", a.s.Health())
				}
				beforeMessages, beforeHistory = messages.Load(), history.Load()
				if _, err := m.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				if messages.Load() != beforeMessages+1 || history.Load() != beforeHistory+1 || !caught.Load() {
					t.Fatal("RecoveryNeeded did not run a complete history pass")
				}
				cancel()
				<-done
			})
		})
	}
}
