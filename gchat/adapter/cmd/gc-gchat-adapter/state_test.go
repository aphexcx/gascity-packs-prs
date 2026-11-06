package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
)

func TestStateOfflineRestoreAndReset(t *testing.T) {
	for _, action := range []string{"restore", "reset"} {
		t.Run(action, func(t *testing.T) {
			env, state, _ := stateFixture(t)
			backup := filepath.Join(t.TempDir(), "backup.json")
			l := ledger.New()
			l.Counters["restored"] = 9
			if err := l.Save(backup); err != nil {
				t.Fatal(err)
			}
			args := []string{"state", action}
			if action == "restore" {
				args = append(args, backup)
			}
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), args, &stdout, &stderr, envAt(env), nil); code != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
			}
			saved, err := ledger.Load(state, true)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if action == "restore" {
				want = 9
			}
			if saved.Counters["restored"] != want || saved.Counters["original"] != 0 || !ledger.Established(filepath.Dir(env)) {
				t.Fatalf("unexpected ledger/marker after %s: %+v", action, saved)
			}
			if action == "reset" && !strings.Contains(stderr.String(), "messages from the gap may be lost or duplicated") {
				t.Fatalf("reset warning missing: %q", &stderr)
			}
			if !strings.Contains(stdout.String(), "state ") {
				t.Fatalf("missing confirmation: %q", &stdout)
			}
		})
	}
}

func TestStateFileOverrideInEnvFile(t *testing.T) {
	env, original, socket := stateFixture(t)
	path := filepath.Join(filepath.Dir(socket), "configured.json")
	l := ledger.New()
	l.Counters["original"] = 9
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(env)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, env, string(contents)+"GCHAT_STATE_FILE="+path+"\n")
	t.Setenv("GCHAT_STATE_FILE", "")
	var out, errOut bytes.Buffer
	if code := run(t.Context(), []string{"state", "reset"}, &out, &errOut, envAt(env), nil); code != 0 {
		t.Fatalf("reset: %d %s", code, &errOut)
	}
	reset, err := ledger.Load(path, true)
	if err != nil || reset.Counters["original"] != 0 {
		t.Fatalf("env-file ledger not reset: %v %+v", err, reset)
	}
	kept, err := ledger.Load(original, true)
	if err != nil || kept.Counters["original"] != 4 {
		t.Fatalf("default ledger changed: %v %+v", err, kept)
	}
}

func TestStateInvalidRestorePreservesCurrent(t *testing.T) {
	for _, contents := range []string{"not json", `{"version":99}`, "missing"} {
		t.Run(contents, func(t *testing.T) {
			env, state, _ := stateFixture(t)
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			backup := filepath.Join(t.TempDir(), "backup.json")
			if contents != "missing" {
				writeFile(t, backup, contents)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"state", "restore", backup}, &stdout, &stderr, envAt(env), nil)
			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if code != 1 || !bytes.Equal(before, after) || ledger.Established(filepath.Dir(env)) {
				t.Fatalf("invalid backup changed state: exit=%d stderr=%q", code, &stderr)
			}
		})
	}
}

func TestStateReclaimsStaleSocket(t *testing.T) {
	for _, action := range []string{"restore", "reset"} {
		for _, force := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/force=%t", action, force), func(t *testing.T) {
				env, state, socket := stateFixture(t)
				ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
				if err != nil {
					t.Fatal(err)
				}
				ln.SetUnlinkOnClose(false)
				if err := ln.Close(); err != nil {
					t.Fatal(err)
				}
				backup := filepath.Join(t.TempDir(), "backup.json")
				l := ledger.New()
				l.Counters["restored"] = 9
				if err := l.Save(backup); err != nil {
					t.Fatal(err)
				}
				args := []string{"state", action}
				if action == "restore" {
					args = append(args, backup)
				}
				if force {
					args = append(args, "--force")
				}
				var stdout, stderr bytes.Buffer
				if code := run(t.Context(), args, &stdout, &stderr, envAt(env), nil); code != 0 {
					t.Fatalf("stale socket blocked %s: exit=%d stderr=%q", action, code, &stderr)
				}
				saved, err := ledger.Load(state, true)
				if err != nil {
					t.Fatal(err)
				}
				want := int64(0)
				if action == "restore" {
					want = 9
				}
				if saved.Counters["restored"] != want || saved.Counters["original"] != 0 || !ledger.Established(filepath.Dir(env)) {
					t.Fatalf("unexpected ledger/marker after %s: %+v", action, saved)
				}
				if _, err := os.Lstat(socket); !os.IsNotExist(err) {
					t.Fatalf("state socket reservation not released: %v", err)
				}
			})
		}
	}
}

func TestStateLiveSocketIsProbedBeforeRefusal(t *testing.T) {
	for _, action := range []string{"restore", "reset"} {
		t.Run(action, func(t *testing.T) {
			env, state, socket := stateFixture(t)
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			info, err := os.Lstat(socket)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"state", action}
			if action == "restore" {
				args = append(args, "test-backup.json")
			}
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), args, &stdout, &stderr, envAt(env), nil); code != 1 || !strings.Contains(stderr.String(), "--force") {
				t.Fatalf("live service not refused: exit=%d stderr=%q", code, &stderr)
			}
			if err := ln.SetDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			conn, err := ln.AcceptUnix()
			if err != nil {
				t.Fatalf("live socket was not probed: %v", err)
			}
			_ = conn.Close()
			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			current, err := os.Lstat(socket)
			if err != nil || !os.SameFile(info, current) || !bytes.Equal(before, after) || ledger.Established(filepath.Dir(env)) {
				t.Fatalf("refusal changed live socket or state: socket error=%v", err)
			}
		})
	}
}

func TestStateRefusesNonSocketPath(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(fmt.Sprintf("force=%t", force), func(t *testing.T) {
			env, state, socket := stateFixture(t)
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, socket, "keep this file")
			args := []string{"state", "reset"}
			if force {
				args = append(args, "--force")
			}
			var stdout, stderr bytes.Buffer
			if code := run(t.Context(), args, &stdout, &stderr, envAt(env), nil); code != 1 || !strings.Contains(stderr.String(), "not a socket") {
				t.Fatalf("non-socket path not refused: exit=%d stderr=%q", code, &stderr)
			}
			contents, err := os.ReadFile(socket)
			if err != nil || string(contents) != "keep this file" {
				t.Fatalf("non-socket path changed: contents=%q error=%v", contents, err)
			}
			after, err := os.ReadFile(state)
			if err != nil || !bytes.Equal(before, after) || ledger.Established(filepath.Dir(env)) {
				t.Fatalf("refusal changed state: %v", err)
			}
		})
	}
}

func TestStateUncertainSocketProbePreservesState(t *testing.T) {
	env, state, socket := stateFixture(t)
	before, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var stdout, stderr bytes.Buffer
	if code := run(ctx, []string{"state", "reset", "--force"}, &stdout, &stderr, envAt(env), nil); code != 1 || !strings.Contains(stderr.String(), "cannot verify") {
		t.Fatalf("uncertain probe not refused: exit=%d stderr=%q", code, &stderr)
	}
	after, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	current, err := os.Lstat(socket)
	if err != nil || !os.SameFile(info, current) || !bytes.Equal(before, after) || ledger.Established(filepath.Dir(env)) {
		t.Fatalf("uncertain probe changed socket or state: socket error=%v", err)
	}
}

func TestStatePreservesReplacedSocketOnCancellation(t *testing.T) {
	for _, replacement := range []string{"file", "socket"} {
		t.Run(replacement, func(t *testing.T) {
			env, state, socket := stateFixture(t)
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			var replaced os.FileInfo
			ctx := stateCancelAtCheck{Context: t.Context(), check: func() {
				// Replace the reservation immediately before the final cancellation
				// check, so cleanup must preserve the new path's owner.
				if err := os.Rename(socket, socket+".old"); err != nil {
					t.Fatal(err)
				}
				if replacement == "file" {
					writeFile(t, socket, "replacement")
				} else {
					ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = ln.Close() })
				}
				var err error
				replaced, err = os.Lstat(socket)
				if err != nil {
					t.Fatal(err)
				}
			}}
			var stdout, stderr bytes.Buffer
			if code := run(ctx, []string{"state", "reset"}, &stdout, &stderr, envAt(env), nil); code != 1 || !strings.Contains(stderr.String(), "context canceled") {
				t.Fatalf("cancellation not honored: exit=%d stderr=%q", code, &stderr)
			}
			current, err := os.Lstat(socket)
			if err != nil || !os.SameFile(replaced, current) {
				t.Fatalf("cleanup removed replacement %s: %v", replacement, err)
			}
			after, err := os.ReadFile(state)
			if err != nil || !bytes.Equal(before, after) || ledger.Established(filepath.Dir(env)) {
				t.Fatalf("cancellation changed state: %v", err)
			}
		})
	}
}

type stateCancelAtCheck struct {
	context.Context
	check func()
}

func (c stateCancelAtCheck) Err() error {
	c.check()
	return context.Canceled
}

func TestStateRunningService(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		force        bool
		status       int
	}{
		{"refuse reset", "reset", false, 0},
		{"refuse restore", "restore", false, 0},
		{"forced reset", "reset", true, 200},
		{"forced restore", "restore", true, 200},
		{"healthy refusal", "reset", true, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, state, socket := stateFixture(t)
			before, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			ln, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			requests := make(chan map[string]string, 2)
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/state" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				var request map[string]string
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				requests <- request
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				if tc.status == 409 {
					fmt.Fprint(w, `{"error":"state restore/reset requires recovery-required health"}`)
				} else {
					fmt.Fprint(w, `{"state":"initializing","reason":"state restored"}`)
				}
			})}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })
			socketBefore, err := os.Lstat(socket)
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"state", tc.action}
			backup, err := filepath.Abs("test-backup.json")
			if err != nil {
				t.Fatal(err)
			}
			if tc.action == "restore" {
				args = append(args, "test-backup.json")
			}
			if tc.force {
				args = append(args, "--force")
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), args, &stdout, &stderr, envAt(env), nil)
			socketAfter, socketErr := os.Lstat(socket)
			if socketErr != nil || !os.SameFile(socketBefore, socketAfter) {
				t.Fatalf("live socket replaced: %v", socketErr)
			}
			after, err := os.ReadFile(state)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) || ledger.Established(filepath.Dir(env)) {
				t.Fatalf("CLI changed running service ledger: exit=%d stderr=%q", code, &stderr)
			}
			if tc.force {
				select {
				case request := <-requests:
					if request["action"] != tc.action || (tc.action == "restore" && request["file"] != backup) {
						t.Fatalf("state request=%v", request)
					}
				default:
					t.Fatalf("force did not contact service: exit=%d stderr=%q", code, &stderr)
				}
				if (code == 0) != (tc.status == 200) || (tc.status == 409 && !strings.Contains(stderr.String(), "recovery-required")) {
					t.Fatalf("exit=%d stdout=%q stderr=%q", code, &stdout, &stderr)
				}
			} else if code != 1 || len(requests) != 0 || !strings.Contains(stderr.String(), "--force") {
				t.Fatalf("running service not refused: exit=%d stderr=%q", code, &stderr)
			}
		})
	}
}

func stateFixture(t *testing.T) (env, state, socket string) {
	t.Helper()
	env, _ = authFixture(t, "http://127.0.0.1:1")
	state = filepath.Join(filepath.Dir(env), "..", "state.json")
	l := ledger.New()
	l.Counters["original"] = 4
	if err := l.Save(state); err != nil {
		t.Fatal(err)
	}
	// Keep Unix socket paths beneath macOS's 104-byte limit.
	dir, err := os.MkdirTemp("/tmp", "gchat-state-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket = filepath.Join(dir, "service.sock")
	t.Setenv("GC_SERVICE_SOCKET", socket)
	t.Setenv("GCHAT_STATE_FILE", state)
	return
}
