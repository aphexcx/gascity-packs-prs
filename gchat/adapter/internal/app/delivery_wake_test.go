package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// UNPROVEN: the fake models the mail result, deduplication on retry and
// nudge's exit status. Its files count actual invocations and mail creations.
func deliveryCommands(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	t.Setenv("WAKE_TEST_DIR", dir)
	script := `#!/bin/sh
cd "$WAKE_TEST_DIR" || exit 1
printf '%s\n' "$@" >> args
case "$1" in
mail)
  echo mail >> calls
  [ -f mail ] || echo made >> mail
  read -r mode < mode
  case "$mode" in
    mail_timeout) echo duplicate > mode; exec sleep 10 ;;
    mail_exit) echo secret-output >&2; exit 1 ;;
    mail_no_result) echo secret-output; exit 0 ;;
    empty) exit 0 ;;
    false) echo '{"ok":false}'; exit 0 ;;
    slow) sleep 0.5 ;;
    hold) while [ ! -f release ]; do sleep 0.02; done ;;
    missing|wake_timeout|wake_exit) echo '{"ok":true}'; exit 0 ;;
    duplicate) echo '{"ok":true,"already_done":true}'; exit 0 ;;
  esac
  echo '{"ok":true,"notified":true}'
  ;;
session)
  echo wake >> calls
  read -r mode < mode
  case "$mode" in
    wake_timeout) echo duplicate > mode; exec sleep 10 ;;
    wake_exit) echo duplicate > mode; echo secret-output >&2; exit 1 ;;
  esac
  ;;
*) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDeliveryWakeSlow(t *testing.T) {
	for _, tc := range []struct {
		mode, cause string
		calls       []string
	}{
		{"slow", "", []string{"mail"}}, {"missing", "", []string{"mail", "wake"}},
		{"mail_timeout", "mail_timeout", []string{"mail", "mail", "wake"}},
		{"wake_timeout", "wake_timeout", []string{"mail", "wake", "mail", "wake"}},
		{"wake_exit", "wake_exit", []string{"mail", "wake", "mail", "wake"}},
		{"empty", "mail_no_result", []string{"mail"}}, {"false", "mail_no_result", []string{"mail"}},
		{"mail_no_result", "mail_no_result", []string{"mail"}}, {"mail_exit", "mail_exit", []string{"mail"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir := deliveryCommands(t)
			writeFile(t, filepath.Join(dir, "mode"), tc.mode)
			opts := Options{mailTimeout: 5 * time.Second, wakeTimeout: 5 * time.Second}
			if tc.mode == "mail_timeout" {
				opts.mailTimeout = 2 * time.Second
			}
			if tc.mode == "wake_timeout" {
				opts.wakeTimeout = 2 * time.Second
			}
			a := raiseApp(opts)
			r := inbound.Raise{ID: "example-change", Body: "example body"}
			ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
			defer cancel()
			err := a.raiseDelivery(ctx, r)
			if (tc.cause == "") != (err == nil) || (err != nil && err.Error() != tc.cause) {
				t.Fatalf("failure cause=%v want %q", err, tc.cause)
			}
			if len(tc.calls) > 2 {
				if err := a.raiseDelivery(t.Context(), r); err != nil {
					t.Fatal(err)
				}
			}
			read := func(name string) string {
				b, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			if got := strings.Fields(read("calls")); !slices.Equal(got, tc.calls) {
				t.Fatalf("calls=%v want %v", got, tc.calls)
			}
			if got := read("mail"); got != "made\n" {
				t.Fatalf("mail creations=%q", got)
			}
			args := read("args")
			want := "mail\nsend\nsupport\n--dedup\ngchat:examplecity:example:" + r.DedupKey() + "\n--notify\n--json\n-s\ngchat: delivery\n-m\nexample body\n"
			if !strings.HasPrefix(args, want) {
				t.Fatalf("mail arguments=%q", args)
			}
			if slices.Contains(tc.calls, "wake") && !strings.Contains(args, "session\nnudge\nsupport\ngchat: a delivery notice is waiting in your mail\n") {
				t.Fatalf("wake arguments=%q", args)
			}
		})
	}
}

func TestDeliveryWakeInheritedOutput(t *testing.T) {
	for _, mode := range []string{"timeout", "confirmed"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("WAKE_TEST_DIR", dir)
			t.Setenv("WAKE_TEST_PARENT", mode)
			bin := filepath.Join(dir, "gc")
			t.Setenv("GC_BIN", bin)
			script := `#!/bin/sh
cd "$WAKE_TEST_DIR" || exit 1
(while [ ! -f release ] && [ -d "$WAKE_TEST_DIR" ]; do sleep 0.05; done; : > closed) &
: > opened
if [ "$WAKE_TEST_PARENT" = timeout ]; then exec sleep 20; fi
echo '{"ok":true,"notified":true}'
`
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			app := raiseApp(Options{mailTimeout: 3 * time.Second})
			result := make(chan error, 1)
			go func() { result <- app.raiseDelivery(t.Context(), inbound.Raise{ID: "example"}) }()
			defer func() {
				writeFile(t, filepath.Join(dir, "release"), "")
				waitFor(t, 5*time.Second, func() bool {
					_, err := os.Stat(filepath.Join(dir, "closed"))
					return err == nil
				})
			}()
			waitFor(t, 3*time.Second, func() bool {
				_, err := os.Stat(filepath.Join(dir, "opened"))
				return err == nil
			})
			select {
			case err := <-result:
				if mode == "confirmed" && err != nil || mode == "timeout" && err != inbound.RaiseFailure("mail_timeout") {
					t.Fatalf("mail result = %v", err)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("mail still waits for output from a descendant")
			}
		})
	}
}

func deliveryApplication(t *testing.T, dir string, now func() time.Time) (*fixture, func(string)) {
	t.Helper()
	f := newFixture(t, true)
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	f.cfg.Secrets.GCAPIBase = f.gc.URL()
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
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryDelay: time.Millisecond, pollInterval: 50 * time.Millisecond, inboundNow: now})
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
	return f, func(name string) {
		data := fmt.Sprintf(`{"message":{"name":"spaces/room/messages/%s","text":"hello","sender":{"name":"users/222","type":"HUMAN"},"createTime":%q,"thread":{"name":"spaces/room/threads/one"}}}`, name, time.Now().UTC().Format(time.RFC3339Nano))
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err := f.pub.Publish(ctx, &pubsub.Message{Data: []byte(data), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1.created", "ce-id": name}}).Get(ctx)
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeliveryWakeWire(t *testing.T) {
	dir := deliveryCommands(t)
	f, send := deliveryApplication(t, dir, nil)
	f.gc.Configure(func(s *fakegc.Server) {
		s.Health = func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"uptime_sec":3600}`) }
	})
	send("sign")
	waitFor(t, 3*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	f.gc.Configure(func(s *fakegc.Server) { s.Posts = []fakegc.PostStep{{Drop: true, HTTPStatus: 500}, {Drop: true}} })
	send("lost")
	waitFor(t, 3*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "args"))
		return strings.Contains(string(b), "--notify\n--json\n")
	})
	if n := len(f.gc.Inbound()); n != 3 {
		t.Fatalf("total POSTs=%d want 3", n)
	}
	waitFor(t, 3*time.Second, func() bool {
		l, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
		if err != nil {
			return false
		}
		c := l.Unresolved["spaces/room/messages/lost"]
		return c != nil && c.Attempts == 2 && c.Status == "unresolved" && c.Cause == "unknown" && c.Reason == "re-post not confirmed (no_answer); delivery unknown" && !c.RaisedEnd.IsZero()
	})
}

func TestDeliveryWakeApart(t *testing.T) {
	dir := deliveryCommands(t)
	start := time.Now()
	var advance atomic.Int64
	f, send := deliveryApplication(t, dir, func() time.Time { return start.Add(time.Duration(advance.Load())) })
	opened, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	f.gc.Configure(func(s *fakegc.Server) {
		s.Posts = []fakegc.PostStep{{After: func(*httptest.ResponseRecorder) { close(opened); <-release }}}
	})
	send("held")
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		t.Fatal("held POST did not start")
	}
	// This change only becomes due while its own POST holds the gate.
	advance.Store(int64(3 * time.Minute))
	waitFor(t, 2*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "args"))
		return strings.Contains(string(b), "--notify")
	})
}

func TestDeliveryWakeReceiptWhileMailIsHeld(t *testing.T) {
	dir := deliveryCommands(t)
	f, send := deliveryApplication(t, dir, nil)
	writeFile(t, filepath.Join(dir, "mode"), "hold")
	defer writeFile(t, filepath.Join(dir, "release"), "")
	f.gc.Configure(func(s *fakegc.Server) {
		s.Script = []wire.Delivery{{Status: "pending"}}
		s.Posts = []fakegc.PostStep{{}, {Drop: true}}
	})
	send("receipt")
	waitFor(t, 3*time.Second, func() bool { return len(f.gc.Inbound()) == 1 })
	send("lost")
	waitFor(t, 3*time.Second, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "calls"))
		return strings.Contains(string(b), "mail")
	})
	l, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
	if err != nil {
		t.Fatal(err)
	}
	c := l.Changes["spaces/room/messages/receipt"]
	if c == nil || c.Status != "pending" || c.ReceiptID == "" {
		t.Fatal("pending receipt missing")
	}
	f.gc.SetReceipt(c.ReceiptID, wire.ReceiptResponse{State: "concluded", Delivery: wire.Delivery{Status: "delivered"}})
	waitFor(t, 2*time.Second, func() bool {
		l, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
		return err == nil && l.Changes[c.ID] != nil && l.Changes[c.ID].Status == "committed"
	})
}
