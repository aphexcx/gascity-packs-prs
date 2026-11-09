package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/app"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/ledger"
)

// changeState checks cancellation after acquiring ownership and immediately
// before saving. Replace the lock at that deterministic boundary, without a
// timing race or a production-only hook for the offline command.
type stateSaveContext struct {
	context.Context
	beforeSave func()
}

func (c stateSaveContext) Err() error {
	c.beforeSave()
	return c.Context.Err()
}

func TestOfflineStateSaveRefusesLostLock(t *testing.T) {
	for _, action := range []string{"reset", "restore"} {
		for _, replacement := range []bool{false, true} {
			t.Run(action+"/"+map[bool]string{false: "missing", true: "replaced"}[replacement], func(t *testing.T) {
				env, state, _ := stateFixture(t)
				cfg, err := config.Load(env, "", filepath.Dir(env))
				if err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(state)
				if err != nil {
					t.Fatal(err)
				}
				backup := filepath.Join(t.TempDir(), "backup.json")
				if err := ledger.New().Save(backup); err != nil {
					t.Fatal(err)
				}
				called := false
				ctx := stateSaveContext{Context: t.Context(), beforeSave: func() {
					if called {
						return
					}
					called = true
					if err := os.Rename(state+".lock", state+".lock.old"); err != nil {
						t.Fatal(err)
					}
					if replacement {
						writeFile(t, state+".lock", "another owner\n")
					}
				}}
				if err := changeState(ctx, cfg, action, backup, false); !errors.Is(err, app.ErrStateLockLost) {
					t.Fatalf("offline save after lock loss: %v", err)
				}
				if !called {
					t.Fatal("did not reach the save boundary")
				}
				after, err := os.ReadFile(state)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("refused save changed the ledger: %v", err)
				}
			})
		}
	}
}

func TestStateAgainstGCService(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "adapter")
	build := exec.Command("go", "build", "-o", bin, "./cmd/gc-gchat-adapter")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, action := range []string{"reset", "restore"} {
		t.Run(action, func(t *testing.T) {
			env, _, socket := stateFixture(t)
			state := filepath.Join(filepath.Dir(socket), "state.json")
			t.Setenv("GCHAT_STATE_FILE", state)
			t.Setenv("GC_SERVICE_SOCKET", "") // The operator never sees gc's socket.
			t.Setenv("GC_GCHAT_ADAPTER_ENV", env)
			cfg, err := config.Load(env, "", filepath.Dir(env))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Secrets.ServiceSocket = filepath.Join(filepath.Dir(socket), "gchat-random.sock")
			cfg.Secrets.GCAPIBase = fakegc.New(t).URL()
			cfg.Endpoints.MediaDir = filepath.Join(t.TempDir(), "media")
			cfg.Endpoints.UploadRoot = filepath.Join(t.TempDir(), "uploads")
			writeFile(t, state, "corrupt ledger")
			backup := filepath.Join(t.TempDir(), "backup.json")
			l := ledger.New()
			l.Counters["restored"] = 7
			if err := l.Save(backup); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				done <- app.RunWithOptions(ctx, cfg, app.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
			}()
			stopped := false
			t.Cleanup(func() {
				cancel()
				if !stopped {
					<-done
				}
			})
			client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Secrets.ServiceSocket)
			}}}
			defer client.CloseIdleConnections()
			deadline := time.Now().Add(3 * time.Second)
			for {
				resp, err := client.Get("http://adapter/healthz")
				if err == nil {
					var health struct{ State string }
					_ = json.NewDecoder(resp.Body).Decode(&health)
					resp.Body.Close()
					if health.State == "recovery-required" {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("service never entered recovery")
				}
				time.Sleep(time.Millisecond)
			}
			args := []string{"state", action}
			if action == "restore" {
				args = append(args, backup)
			}
			cmd := exec.CommandContext(t.Context(), bin, args...)
			out, err := cmd.CombinedOutput()
			if err == nil || !bytes.Contains(out, []byte(cfg.Secrets.ServiceSocket)) {
				t.Fatalf("live service not refused with its socket: %v %s", err, out)
			}
			cmd = exec.CommandContext(t.Context(), bin, append(args, "--force")...)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("forced recovery: %v %s", err, out)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			stopped = true
			saved, err := ledger.Load(state, true)
			if err != nil || (action == "restore" && saved.Counters["restored"] != 7) {
				t.Fatalf("service did not own recovery: %v %+v", err, saved)
			}
			cmd = exec.CommandContext(t.Context(), bin, "state", "reset")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("stopped service: %v %s", err, out)
			}
		})
	}
}

func TestForcedOfflineResetAfterLockLossSurvivesServiceExit(t *testing.T) {
	env, state, socket := stateFixture(t)
	cfg, err := config.Load(env, "", filepath.Dir(env))
	if err != nil {
		t.Fatal(err)
	}
	// gc's generated socket differs from the operator's fallback socket.
	cfg.Secrets.ServiceSocket = filepath.Join(filepath.Dir(socket), "generated.sock")
	cfg.Endpoints.MediaDir = filepath.Join(t.TempDir(), "media")
	cfg.Endpoints.UploadRoot = filepath.Join(t.TempDir(), "uploads")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- app.RunWithOptions(ctx, cfg, app.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Notify: func(context.Context, string, string) error { return nil }})
	}()
	stopped := false
	t.Cleanup(func() {
		cancel()
		if !stopped {
			<-done
		}
	})
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", cfg.Secrets.ServiceSocket)
	}}}
	defer client.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, e := client.Get("http://adapter/healthz")
		if e == nil {
			var h struct{ State string }
			_ = json.NewDecoder(resp.Body).Decode(&h)
			resp.Body.Close()
			if h.State == "needs-sign-in" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("service did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.Rename(state+".lock", state+".lock.old"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"state", "reset", "--force"}, &out, &errOut, envAt(env), nil); code != 0 {
		t.Fatalf("offline force reset: %d %s", code, &errOut)
	}
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	stopped = true
	after, err := os.ReadFile(state)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("service exit undid forced offline reset: %v", err)
	}
}

func TestStateLockHeldByOfflineCommandHasNoEmptySocketHint(t *testing.T) {
	env, state, _ := stateFixture(t)
	lock, err := app.LockState(state)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"state", "reset", "--force"}, &out, &errOut, envAt(env), nil); code == 0 {
		t.Fatal("concurrent offline reset succeeded")
	}
	if strings.Contains(errOut.String(), "socket") || strings.Contains(errOut.String(), "--force") {
		t.Fatalf("misleading conflict: %s", &errOut)
	}
}

func TestStateIgnoresUnheldLockFile(t *testing.T) {
	env, state, _ := stateFixture(t)
	writeFile(t, state+".lock", "/tmp/old-gchat.sock\n")
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"state", "reset"}, &out, &errOut, envAt(env), nil); code != 0 {
		t.Fatalf("stale lock: %d %s", code, &errOut)
	}
	if strings.Contains(errOut.String(), "old-gchat.sock") {
		t.Fatal("stale owner was used")
	}
}
