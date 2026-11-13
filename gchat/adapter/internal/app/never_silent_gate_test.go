package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	api "google.golang.org/api/chat/v1"
)

func noticeMail(t *testing.T) string {
	t.Helper()
	mail := filepath.Join(t.TempDir(), "mail")
	writeFile(t, os.Getenv("GC_BIN"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+mail+"'\n")
	return mail
}

func noticeCounts(f *fixture, mail string) (int, int) {
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

func seedNotice(t *testing.T, f *fixture, at time.Time, stopped bool) {
	t.Helper()
	e, err := neversilent.New(neversilent.Options{Enabled: true, Path: f.cfg.Endpoints.StateFile + ".never-silent.json",
		Now: func() time.Time { return at }, Post: func(context.Context, neversilent.Message, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Track(neversilent.Message{ID: "spaces/example/messages/m1", Conversation: "spaces/example", Created: at}); err != nil {
		t.Fatal(err)
	}
	if stopped {
		err = e.StopAt("spaces/example/messages/m1", false, at)
	} else {
		err = e.Result("spaces/example/messages/m1", "unreachable", nil)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func runNoticeFixture(t *testing.T, f *fixture) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }, RetryDelay: time.Millisecond})
	}()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, ErrStateLockLost) {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not shut down")
		}
	})
}

// The old process must leave a successor's state and effects alone, including
// when no credential generation exists to stop on its behalf.
func TestNeverSilentGateLockLoss(t *testing.T) {
	for _, credential := range []bool{false, true} {
		for _, replaced := range []bool{false, true} {
			name := map[bool]string{false: "no-credentials", true: "credentials-running"}[credential] + "/" + map[bool]string{false: "missing", true: "replaced"}[replaced]
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, credential)
				f.cfg.NeverSilent.Enabled = true
				f.cfg.NeverSilent.OperatorContext = "example-operator"
				f.google.Configure(func(g *fakegoogle.Server) {
					g.Spaces["spaces/example"] = &api.Space{Name: "spaces/example", SpaceType: "DIRECT_MESSAGE"}
				})
				mail := noticeMail(t)
				// The first poll has nothing due; the next two do, after lock loss.
				seedNotice(t, f, time.Now().Add(-3*time.Minute+9*time.Second), false)
				runNoticeFixture(t, f)
				if credential {
					f.health(t, "ready", 5*time.Second)
				} else {
					f.health(t, "needs-sign-in", 3*time.Second)
				}
				time.Sleep(6 * time.Second)
				lock := f.cfg.Endpoints.StateFile + ".lock"
				if err := os.Rename(lock, lock+".old"); err != nil {
					t.Fatal(err)
				}
				if replaced {
					writeFile(t, lock, "replacement")
				}
				f.health(t, "recovery-required", 3*time.Second)
				sidecar := f.cfg.Endpoints.StateFile + ".never-silent.json"
				r, err := neversilent.New(neversilent.Options{Enabled: true, Path: sidecar, Post: func(context.Context, neversilent.Message, string) error { return nil }})
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Track(neversilent.Message{ID: "example-replacement-message", Conversation: "spaces/example", Created: time.Now()}); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(sidecar)
				if err != nil {
					t.Fatal(err)
				}
				beforeInfo, err := os.Stat(sidecar)
				if err != nil {
					t.Fatal(err)
				}
				time.Sleep(11 * time.Second) // cover two complete watch intervals
				after, err := os.ReadFile(sidecar)
				if err != nil {
					t.Fatal(err)
				}
				afterInfo, err := os.Stat(sidecar)
				if err != nil {
					t.Fatal(err)
				}
				posts, mails := noticeCounts(f, mail)
				unchanged := bytes.Equal(before, after) && os.SameFile(beforeInfo, afterInfo)
				t.Logf("replacement unchanged=%v owner lines=%d operator mails=%d", unchanged, posts, mails)
				if !unchanged || posts != 0 || mails != 0 {
					t.Error("watch wrote state or emitted effects after lock loss")
				}
			})
		}
	}
}

// A locally held credential cannot spend the owner's once-only reservation.
// Keep the hold longer than the Chat client's retry budget for an unknown space.
func TestNeverSilentGateCredentialHold(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(map[bool]string{true: "space-known", false: "space-unknown"}[known], func(t *testing.T) {
			f := newFixture(t, true)
			f.replaceIdentityToken(t, "users/123456")
			f.cfg.NeverSilent.Enabled = true
			f.cfg.NeverSilent.OperatorContext = "example-operator"
			f.google.Configure(func(g *fakegoogle.Server) {
				g.TokenError = "invalid_grant"
				g.Spaces["spaces/example"] = &api.Space{Name: "spaces/example", SpaceType: "DIRECT_MESSAGE"}
			})
			if known {
				l := ledger.New()
				l.IdentityUserID = "users/123456"
				l.Spaces["spaces/example"] = &ledger.SpaceState{Kind: "dm", ChatType: "DIRECT_MESSAGE"}
				if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
					t.Fatal(err)
				}
			}
			var lists atomic.Int32
			chatURL, err := url.Parse(f.cfg.Endpoints.ChatAPIBase)
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(chatURL)
			chatServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/spaces" && lists.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"code":401,"message":"credential rejected","status":"UNAUTHENTICATED"}}`))
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			t.Cleanup(chatServer.Close)
			f.cfg.Endpoints.ChatAPIBase = chatServer.URL
			f.cfg.Endpoints.EventsAPIBase = chatServer.URL
			mail := noticeMail(t)
			seedNotice(t, f, time.Now().Add(-4*time.Minute), false)
			runNoticeFixture(t, f)
			f.health(t, "needs-sign-in", 5*time.Second)
			time.Sleep(45 * time.Second)
			b, err := os.ReadFile(f.cfg.Endpoints.StateFile + ".never-silent.json")
			if err != nil {
				t.Fatal(err)
			}
			var state struct {
				Owners   map[string]json.RawMessage `json:"owners"`
				Messages map[string]struct {
					Notified bool `json:"notified"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(b, &state); err != nil {
				t.Fatal(err)
			}
			posts, mails := noticeCounts(f, mail)
			reserved := len(state.Owners) != 0 || state.Messages["spaces/example/messages/m1"].Notified
			t.Logf("credential held: reserved=%v owner lines=%d operator mails=%d", reserved, posts, mails)
			if reserved || posts != 0 || mails != 1 {
				t.Error("credential hold spent a reservation or held operator mail")
			}
			f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = "" })
			f.replaceIdentityToken(t, "users/123456")
			f.health(t, "ready", 12*time.Second)
			time.Sleep(11 * time.Second)
			posts, mails = noticeCounts(f, mail)
			t.Logf("after sign-in: owner lines=%d operator mails=%d", posts, mails)
			if posts != 1 || mails != 1 {
				t.Error("want exactly one owner line after sign-in and one operator mail overall")
			}
		})
	}
}

func TestNeverSilentGateReconcilesBeforeRegistration(t *testing.T) {
	f := newFixture(t, true)
	f.cfg.NeverSilent.Enabled = true
	f.cfg.NeverSilent.OperatorContext = "example-operator"
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/example"] = &api.Space{Name: "spaces/example", SpaceType: "DIRECT_MESSAGE"}
	})
	mail := noticeMail(t)
	old := time.Now().Add(-4 * time.Minute)
	seedNotice(t, f, old, true)
	// Crash between resolve-accept's ledger save and the sidecar's Forget.
	l := ledger.New()
	l.IdentityUserID = fakegoogle.UserID
	l.Changes["spaces/example/messages/m1"] = &ledger.ChangeState{ID: "spaces/example/messages/m1", Space: "spaces/example", Message: "spaces/example/messages/m1",
		Kind: "created", Status: "committed", Reason: "operator accepted", NoticeResolved: true, Created: old}
	if err := l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	var registrations atomic.Int64
	gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/extmsg/adapters") {
			registrations.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(gc.Close)
	f.cfg.Secrets.GCAPIBase = gc.URL
	runNoticeFixture(t, f)
	time.Sleep(12 * time.Second)
	posts, mails := noticeCounts(f, mail)
	b, err := os.ReadFile(f.cfg.Endpoints.StateFile + ".never-silent.json")
	if err != nil {
		t.Fatal(err)
	}
	reconciled := bytes.Contains(b, []byte(`"removed":true`))
	t.Logf("registration refused=%d reconciled=%v owner lines=%d operator mails=%d", registrations.Load(), reconciled, posts, mails)
	if registrations.Load() == 0 || !reconciled || posts != 0 || mails != 0 {
		t.Error("watch must reconcile the resolved message locally before any notice")
	}
}
