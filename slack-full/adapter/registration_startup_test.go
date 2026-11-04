package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Run the real startup path in an isolated process, including both listeners.
// The explicit environment keeps production credentials and state out of tests.
func TestRegistrationProcessHelper(t *testing.T) {
	if os.Getenv("SLACK_REGISTRATION_TEST_PROCESS") != "1" {
		return
	}
	if os.Getenv("SLACK_REGISTRATION_TEST_SHORT_DEADLINE") == "1" {
		bootRegistrationPolicy.timeout = 100 * time.Millisecond
	}
	main()
}

type registrationProcess struct {
	cmd    *exec.Cmd
	log    string
	public string
	client *http.Client
	done   chan error
}

func registrationUnusedAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func startRegistrationProcess(t *testing.T, gcURL string, extraEnv ...string) *registrationProcess {
	t.Helper()
	// Unix sockets have a short platform path limit (especially on macOS).
	dir, err := os.MkdirTemp("/tmp", "slack-register-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "internal.sock")
	public := registrationUnusedAddress(t)
	logPath := filepath.Join(dir, "adapter.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistrationProcessHelper$")
	cmd.Env = []string{
		"SLACK_REGISTRATION_TEST_PROCESS=1",
		"SLACK_WORKSPACE_ID=TBOOT", "SLACK_BOT_TOKEN=xoxb-test",
		"SLACK_SIGNING_SECRET=test-secret", "GC_CITY_NAME=test-city",
		"GC_CITY_PATH=" + dir, "GC_API_BASE_URL=" + gcURL,
		"GC_SERVICE_SOCKET=" + socket,
		"GC_SERVICE_URL_PREFIX=/v0/city/test-city/svc/slack",
		"LISTEN_PUBLIC=" + public, "SLACK_SOCKET_MODE=off",
		"IDENTITY_STORE_PATH=" + filepath.Join(dir, "identities.json"),
		"HANDLE_ALIAS_STORE_PATH=" + filepath.Join(dir, "aliases.json"),
		"INBOUND_FILE_STORE=" + filepath.Join(dir, "inbound"),
	}
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()
	p := &registrationProcess{cmd: cmd, log: logPath, public: "http://" + public, done: make(chan error, 1)}
	transport := &http.Transport{DialContext: (&net.Dialer{}).DialContext}
	// HTTP over the same UDS gc probes for proxy_process readiness.
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	p.client = &http.Client{Transport: transport, Timeout: time.Second}
	go func() { p.done <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("adapter did not exit after kill")
		}
		transport.CloseIdleConnections()
		b, _ := os.ReadFile(p.log)
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "register") {
				t.Log(line)
			}
		}
	})
	return p
}

func (p *registrationProcess) waitHealth(t *testing.T, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := p.client.Get("http://gc-service/healthz")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			last = string(b)
			if resp.StatusCode == http.StatusOK && strings.HasPrefix(last, want+"\n") {
				if strings.HasPrefix(want, "registering:") && (resp.Header.Get("X-GC-Health") != "degraded" || resp.Header.Get("X-GC-Health-Reason") != want) {
					t.Fatalf("registration health headers: %v", resp.Header)
				}
				if want == "ok" && strings.Contains(resp.Header.Get("X-GC-Health-Reason"), "registering:") {
					t.Fatal("stale registering health header")
				}
				t.Logf("health: %s", want)
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(p.log)
	t.Fatalf("health never became %q; last=%q; log:\n%s", want, last, b)
}

func TestRegistrationStartupRetriesCityNotRunning(t *testing.T) {
	var mu sync.Mutex
	var templates []bool
	fakeGC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/city/test-city/extmsg/adapters" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		templates = append(templates, strings.Contains(string(b), "reply_instructions"))
		if len(templates) <= 2 {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"status":404,"title":"Not Found","detail":"not_found: city not found or not running: test-city"}`)
		}
	}))
	defer fakeGC.Close()
	p := startRegistrationProcess(t, fakeGC.URL)
	p.waitHealth(t, "registering: attempt 1", 2*time.Second)
	// Admission must stay retryable while only the health surface is ready.
	resp, err := (&http.Client{Timeout: time.Second}).Post(p.public+"/slack/events", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("inbound during registration: status=%d, want 503", resp.StatusCode)
	}
	p.waitHealth(t, "registering: attempt 2", 3*time.Second)
	p.waitHealth(t, "ok", 5*time.Second)
	mu.Lock()
	got := fmt.Sprint(templates)
	mu.Unlock()
	if got != "[true true true]" {
		t.Fatalf("registration requests retained template: %s", got)
	}
	b, _ := os.ReadFile(p.log)
	if n := strings.Count(string(b), "register adapter: attempt "); n != 3 {
		t.Fatalf("want one log per attempt (3), got %d:\n%s", n, b)
	}
}

func TestRegistrationStartupReplyInstructionsCompatibility(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var mu sync.Mutex
			var templates []bool
			fakeGC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/city/test-city/extmsg/adapters" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				templates = append(templates, strings.Contains(string(b), "reply_instructions"))
				if len(templates) == 1 {
					w.WriteHeader(status)
					if status == http.StatusBadRequest {
						_, _ = io.WriteString(w, `{"error":"bad_request","message":"json: unknown field \"reply_instructions\""}`)
					} else {
						_, _ = io.WriteString(w, `{"status":422,"code":"validation-failed","errors":[{"message":"unexpected property","location":"body.reply_instructions"}]}`)
					}
				}
			}))
			defer fakeGC.Close()
			p := startRegistrationProcess(t, fakeGC.URL)
			p.waitHealth(t, "ok", 5*time.Second)
			mu.Lock()
			got := fmt.Sprint(templates)
			mu.Unlock()
			if got != "[true false]" {
				t.Fatalf("compatibility requests: %s", got)
			}
		})
	}
}

func TestRegistrationStartupDelayedPort(t *testing.T) {
	if os.Getenv("SLACK_TEST_BOOT_DELAY") != "1" {
		t.Skip("set SLACK_TEST_BOOT_DELAY=1 for the 20-second closed-port boot acceptance run")
	}
	addr := registrationUnusedAddress(t)
	p := startRegistrationProcess(t, "http://"+addr)
	p.waitHealth(t, "registering: attempt 1", 2*time.Second)
	time.Sleep(20 * time.Second)
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	_ = srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	defer srv.Close()
	p.waitHealth(t, "ok", 20*time.Second)
	t.Logf("same adapter PID %d registered after the port opened at +20s", p.cmd.Process.Pid)
}

func TestRegistrationFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		retry, compat bool
	}{
		{"refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true, false},
		{"unready", &registrationHTTPError{404, `{"detail":"not_found: city not found or not running: test-city"}`}, true, false},
		{"missing-route", &registrationHTTPError{404, `{"detail":"route not found"}`}, false, false},
		{"old-gc", &registrationHTTPError{400, `json: unknown field "reply_instructions"`}, false, true},
		{"current-gc", &registrationHTTPError{422, `{"errors":[{"message":"unexpected property","location":"body.reply_instructions"}]}`}, false, true},
		{"wrong-field", &registrationHTTPError{422, `{"errors":[{"message":"unexpected property","location":"body.callback"}]}`}, false, false},
		{"bad-value", &registrationHTTPError{422, `{"errors":[{"message":"expected string","location":"body.reply_instructions"}]}`}, false, false},
		{"not-json", &registrationHTTPError{422, `reply_instructions`}, false, false},
		{"city-code", &registrationHTTPError{404, `{"code":"city-not-found"}`}, true, false},
		{"invalid-account", &registrationHTTPError{400, `account_id is required`}, false, false},
		{"auth", &registrationHTTPError{401, `reply_instructions not authorized`}, false, false},
		{"server", &registrationHTTPError{500, `city not found or not running`}, false, false},
		{"dns", &net.DNSError{Err: "no such host", Name: "invalid"}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if registrationCanRetry(tc.err) != tc.retry || registrationNeedsCompatibility(tc.err) != tc.compat {
				t.Fatalf("classification of %v: want retry=%t compat=%t", tc.err, tc.retry, tc.compat)
			}
		})
	}
}

func TestRegistrationDeadlineRetainsLastStatus(t *testing.T) {
	fakeGC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"not_found: city not found or not running: test-city"}`)
	}))
	defer fakeGC.Close()
	started := time.Now()
	err := registerAdapterOnStart(context.Background(), config{gcAPIBase: fakeGC.URL, cityName: "test-city"}, &startupGate{}, registrationRetryPolicy{time.Millisecond, 5 * time.Millisecond, 40 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "404 Not Found") {
		t.Fatalf("deadline lost last status: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("registration exceeded its total deadline")
	}
}

func TestRegistrationStartupDeadlineExitsFatal(t *testing.T) {
	fakeGC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"detail":"not_found: city not found or not running: test-city"}`)
	}))
	defer fakeGC.Close()
	p := startRegistrationProcess(t, fakeGC.URL, "SLACK_REGISTRATION_TEST_SHORT_DEADLINE=1")
	select {
	case err := <-p.done:
		p.done <- err // Cleanup still owns reaping/closing the test process.
		if err == nil {
			t.Fatal("deadline should exit nonzero")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("adapter did not exit after registration deadline")
	}
	b, _ := os.ReadFile(p.log)
	if !strings.Contains(string(b), "register adapter: registration stopped: context deadline exceeded; last failure: register failed: 404 Not Found") {
		t.Fatalf("fatal did not retain the last status: %s", b)
	}
}

func TestRegistrationDeadlineCancelsRequest(t *testing.T) {
	fakeGC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer fakeGC.Close()
	err := registerAdapterOnStart(context.Background(), config{gcAPIBase: fakeGC.URL, cityName: "test-city"}, &startupGate{}, registrationRetryPolicy{time.Millisecond, time.Millisecond, 40 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline did not cancel hung request: %v", err)
	}
}

func TestRegistrationCompatibilityOnlyOnceAcrossBootRetries(t *testing.T) {
	var mu sync.Mutex
	var templates []bool
	fakeGC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		templates = append(templates, strings.Contains(string(b), "reply_instructions"))
		switch len(templates) {
		case 1, 3:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `json: unknown field "reply_instructions"`)
		case 2:
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `city not found or not running: test-city`)
		}
	}))
	defer fakeGC.Close()
	err := registerAdapterOnStart(context.Background(), config{gcAPIBase: fakeGC.URL, cityName: "test-city"}, &startupGate{}, registrationRetryPolicy{time.Millisecond, time.Millisecond, time.Second})
	mu.Lock()
	defer mu.Unlock()
	if err == nil || fmt.Sprint(templates) != "[true false false]" {
		t.Fatalf("compatibility must fire once: requests=%v err=%v", templates, err)
	}
}
