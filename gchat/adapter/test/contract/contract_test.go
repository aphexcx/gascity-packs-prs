package contract_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

// Run with GC_BIN=/absolute/path/to/gc go test ./test/contract -v -count=1.
// Only Google's API and the agent runtime are faked: gc's HTTP validation,
// adapter registry, bindings, transcripts and receipt decoder are real.
func TestRealGCContract(t *testing.T) {
	bin := os.Getenv("GC_BIN")
	if bin == "" {
		t.Skip("set GC_BIN to a real gc binary to run the contract test")
	}
	gc := startGC(t, bin)
	google := fakegoogle.New(t, nil)
	target, err := url.Parse(google.URL())
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var quota atomic.Bool
	googleAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if quota.Load() && r.Method == http.MethodPost && r.URL.Path == "/v1/spaces/room/messages" {
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(googleAPI.Close)
	google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		g.Messages["spaces/room/messages/inbound"] = &api.Message{
			Name: "spaces/room/messages/inbound", Text: "hello from Chat",
			Space:      &api.Space{Name: "spaces/room", SpaceType: "SPACE"},
			Sender:     &api.User{Name: "users/example", DisplayName: "Example", Type: "HUMAN"},
			CreateTime: time.Now().UTC().Format(time.RFC3339Nano),
			Thread:     &api.Thread{Name: "spaces/room/threads/example"},
		}
	})
	cfg := &config.Config{
		Secrets:    config.Secrets{CityName: "examplecity", GCAPIBase: gc.base},
		SecretsDir: gc.root,
		Endpoints:  config.Endpoints{ChatAPIBase: googleAPI.URL, StateFile: filepath.Join(gc.root, "state.json")},
	}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "example"}}
	chatClient, err := chat.New(t.Context(), gc.client, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err := chatClient.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	state, stateMu := ledger.New(), new(sync.Mutex)
	client := gcclient.New(gc.base, "examplecity")
	adapter, err := server.New(server.Options{
		Config: cfg, Ledger: state, LedgerMu: stateMu,
		LedgerPath: cfg.Endpoints.StateFile, Chat: chatClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(adapter.Close)
	adapter.SetHealth(server.Health{State: "ready"})
	type callback struct {
		header string
		body   wire.PublishRequest
	}
	callbacks := make(chan callback, 2)
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/publish" {
			t.Errorf("unexpected callback: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Error(err)
			http.Error(w, "read callback", http.StatusBadRequest)
			return
		}
		var request wire.PublishRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			http.Error(w, "decode callback", http.StatusBadRequest)
			return
		}
		select {
		case callbacks <- callback{r.Header.Get(wire.RequestHeader), request}:
		default:
			t.Error("unexpected extra publish callback")
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		adapter.ServeHTTP(w, r)
	}))
	t.Cleanup(handler.Close)

	reg, err := server.Registration(cfg, handler.URL)
	if err != nil {
		t.Fatal(err)
	}
	processor, err := inbound.New(inbound.Options{Config: cfg, Ledger: state, LedgerMu: stateMu,
		LedgerPath: cfg.Endpoints.StateFile, GC: client, Chat: chatClient, IdentityUserID: fakegoogle.UserID})
	if err != nil {
		t.Fatal(err)
	}
	var fixture *api.Message
	google.Configure(func(g *fakegoogle.Server) { copy := *g.Messages["spaces/room/messages/inbound"]; fixture = &copy })
	outcome, err := processor.Handle(t.Context(), &puller.Event{PubSubID: "example-event", Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: fixture}}})
	if err != nil || outcome != puller.Ack {
		t.Fatalf("production inbound mapping: %v %v", outcome, err)
	}
	change := state.Changes[fixture.Name]
	if change == nil || change.Status != "committed" || change.ReceiptID == "" {
		t.Fatalf("no_route not committed: %+v", change)
	}
	mapped := decode[wire.InboundMessage](t, []byte(change.Event))
	conversation := mapped.Conversation
	if conversation.Kind != "room" || conversation.ScopeID != "examplecity" {
		t.Fatalf("mapped conversation: %+v", conversation)
	}
	receipt, err := client.Receipt(t.Context(), change.ReceiptID)
	if err != nil || receipt.State != "concluded" || receipt.Delivery.Status != "no_route" {
		t.Fatalf("inbound receipt: %+v %v", receipt, err)
	}
	// A real unknown receipt must remain an unresolved delivery obligation.
	state.Changes["unknown"] = &ledger.ChangeState{ID: "unknown", Status: "pending", ReceiptID: "absent-receipt", Created: time.Now()}
	if err := processor.Poll(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state.Changes["unknown"].Status != "unresolved" {
		t.Fatalf("unknown receipt lost: %+v", state.Changes["unknown"])
	}
	t.Log("inbound: production mapper/client accepted; no_route concluded; unknown receipt retained unresolved")

	// A real session under the isolated fake runtime survives reconciliation.
	gc.request(t, http.MethodPost, "/v0/city/examplecity/sessions", map[string]string{
		"kind": "provider", "name": "example", "alias": "example-contract",
	}, http.StatusAccepted)
	sessionID := ""
	until := time.NewTimer(10 * time.Second)
	defer until.Stop()
	for sessionID == "" {
		sessions := decode[struct {
			Items []struct {
				ID    string `json:"id"`
				Alias string `json:"alias"`
				State string `json:"state"`
			} `json:"items"`
		}](t, gc.get(t, "/v0/city/examplecity/sessions"))
		for _, candidate := range sessions.Items {
			if candidate.Alias == "example-contract" && candidate.State != "creating" && candidate.State != "closed" {
				sessionID = candidate.ID
			}
		}
		if sessionID != "" {
			break
		}
		select {
		case <-until.C:
			t.Fatalf("session did not become available: %+v", sessions)
		case <-time.After(50 * time.Millisecond):
		}
	}

	gc.post(t, "bind", struct {
		Conversation wire.Conversation `json:"conversation"`
		SessionID    string            `json:"session_id"`
	}{conversation, sessionID}, http.StatusOK)
	outbound := wire.OutboundRequest{
		SessionID: sessionID, Conversation: conversation, Text: "hello from gc",
		ReplyToMessageID: mapped.ReplyToMessageID, IdempotencyKey: "example-publish",
	}

	if _, err := client.Outbound(t.Context(), outbound); !gcclient.IsNoAdapter(err, "example") {
		t.Fatalf("before registration: %v", err)
	}
	if err := gcclient.New(gc.base, "example-missing-city").Register(t.Context(), reg); !gcclient.IsCityNotRunning(err) {
		t.Fatalf("missing city: %v", err)
	}
	// Observe the real response without replacing the production registration
	// builder or client: Register deliberately accepts any successful 2xx.
	gcURL, err := url.Parse(gc.base)
	if err != nil {
		t.Fatal(err)
	}
	registrationProxy := httputil.NewSingleHostReverseProxy(gcURL)
	var registrationStatus atomic.Int32
	registrationProxy.ModifyResponse = func(resp *http.Response) error {
		registrationStatus.Store(int32(resp.StatusCode))
		return nil
	}
	registrationObserver := httptest.NewServer(registrationProxy)
	t.Cleanup(registrationObserver.Close)
	if err := gcclient.New(registrationObserver.URL, "examplecity").Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	if got := registrationStatus.Load(); got != http.StatusCreated {
		t.Fatalf("registration status = %d, want 201", got)
	}
	t.Log("registration: production builder/client accepted by real gc with HTTP 201")
	result, err := client.Outbound(t.Context(), outbound)
	if err != nil || !result.Receipt.Delivered || result.Receipt.MessageID == "" || result.TranscriptEntry == nil {
		t.Fatalf("outbound result: %+v %v", result, err)
	}
	if result.Receipt.Conversation != conversation || !reflect.DeepEqual(result.Receipt.Metadata, map[string]string{"thread": outbound.ReplyToMessageID, "parts": "1"}) {
		t.Fatalf("receipt scope/metadata: %+v", result)
	}

	select {
	case got := <-callbacks:
		if got.header == "" {
			t.Error("gc callback omitted X-GC-Request")
		}
		want := wire.PublishRequest{
			SessionID: sessionID, Conversation: conversation, Text: outbound.Text,
			ReplyToMessageID: outbound.ReplyToMessageID, IdempotencyKey: outbound.IdempotencyKey,
		}
		if !reflect.DeepEqual(got.body, want) {
			t.Errorf("callback = %+v, want %+v", got.body, want)
		}
	default:
		t.Fatal("gc did not call the adapter's /publish")
	}
	google.Configure(func(g *fakegoogle.Server) {
		message := g.Messages[result.Receipt.MessageID]
		if len(g.Messages) != 2 || message == nil || message.Text != outbound.Text || message.Thread == nil || message.Thread.Name != outbound.ReplyToMessageID {
			t.Fatalf("expected one Google reply alongside the inbound fixture, got %+v", g.Messages)
		}
	})
	t.Log("outbound: real /publish callback with X-GC-Request; Receipt.Delivered=true; scope, message ID, thread and parts=\"1\" preserved; transcript recorded")
	quota.Store(true)
	outbound.IdempotencyKey = "example-quota"
	result, err = client.Outbound(t.Context(), outbound)
	if err != nil || result.Receipt.Delivered || result.Receipt.FailureKind != "rate_limited" || result.Receipt.RetryAfter != int64(60*time.Second) {
		t.Fatalf("real gc failed receipt decode: %+v %v", result, err)
	}
	t.Log("outbound: real gc decoded rate_limited receipt and 60-second retry hint")

}

type gcProcess struct {
	root, base string
	client     *http.Client
}

func startGC(t *testing.T, bin string) *gcProcess {
	t.Helper()
	bin, err := exec.LookPath(bin)
	if err != nil {
		t.Fatal(err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	// macOS Unix sockets cannot fit Go's long default test directory names.
	root, err := os.MkdirTemp("/tmp", "gchat-contract-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	gcHome, runtimeDir, city := filepath.Join(root, "gc"), filepath.Join(root, "run"), filepath.Join(root, "examplecity")
	for _, dir := range []string{gcHome, runtimeDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(root, "gitconfig"), "[user]\nname = example\nemail = example@example.invalid\n[beads]\nrole = maintainer\n")
	// Deliberately do not inherit GC_*, BEADS_*, credentials, proxies, git
	// overrides or session identity from the lane running this test.
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TMPDIR=" + root,
		"GC_HOME=" + gcHome, "XDG_RUNTIME_DIR=" + runtimeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"DOLT_ROOT_PATH=" + root, "GC_BEADS=file", "GC_DOLT=skip", "GC_SESSION=fake", "GC_SUPERVISOR_LOG_TEE=0",
	}
	configPath := filepath.Join(root, "city-input.toml")
	writeFile(t, configPath, `[workspace]
name = "examplecity"
provider = "example"
[providers.example]
base = "builtin:claude"
command = "/usr/bin/true"
resume_command = "/usr/bin/true"
[beads]
provider = "file"
`)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	init := exec.CommandContext(ctx, bin, "init", "--file", configPath, "--no-start", city)
	init.Dir, init.Env, init.WaitDelay = root, env, 2*time.Second
	initLog := new(boundedLog)
	init.Stdout, init.Stderr = initLog, initLog
	if err := init.Run(); err != nil {
		t.Fatalf("gc init: %v\n%s", err, initLog.String())
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gcHome, "supervisor.toml"), fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\nallow_mutations = true\n", port))
	// --no-start avoids launchd/systemd and their ambient environment. The
	// isolated supervisor reads only this one-city registry at startup.
	writeFile(t, filepath.Join(gcHome, "cities.toml"), fmt.Sprintf("[[cities]]\npath = %q\nname = \"examplecity\"\n", city))
	ctx, stop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "supervisor", "run")
	cmd.Dir, cmd.Env = root, env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	log := new(boundedLog)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		stop()
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		stop()
		<-done
		t.Logf("supervisor exit: %v", waitErr)
		if cmd.ProcessState == nil || !cmd.ProcessState.Success() {
			t.Errorf("supervisor stop: %v", waitErr)
		}
		if t.Failed() {
			t.Logf("supervisor output:\n%s", log.String())
		}
	})
	transport := &http.Transport{Proxy: nil}
	t.Cleanup(transport.CloseIdleConnections)
	gc := &gcProcess{root: root, base: fmt.Sprintf("http://127.0.0.1:%d", port), client: &http.Client{Transport: transport, Timeout: 10 * time.Second}}
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			t.Fatalf("supervisor exited before readiness: %v", waitErr)
		case <-deadline.C:
			t.Fatal("temporary city did not become ready")
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-ticker.C:
			resp, err := gc.client.Get(gc.base + "/v0/city/examplecity/extmsg/adapters")
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					select {
					case <-done:
						t.Fatalf("supervisor exited at readiness: %v", waitErr)
					default:
					}
					return gc
				}
			}
		}
	}
}

func (gc *gcProcess) post(t *testing.T, path string, body any, status int) []byte {
	t.Helper()
	return gc.request(t, http.MethodPost, path, body, status)
}

func (gc *gcProcess) get(t *testing.T, path string) []byte {
	t.Helper()
	return gc.request(t, http.MethodGet, path, nil, http.StatusOK)
}

func (gc *gcProcess) request(t *testing.T, method, path string, body any, status int) []byte {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	endpoint := gc.base + "/v0/city/examplecity/extmsg/" + path
	if strings.HasPrefix(path, "/") {
		endpoint = gc.base + path
	}
	req, err := http.NewRequestWithContext(t.Context(), method, endpoint, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(wire.RequestHeader, "contract-test")
	resp, err := gc.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, resp.StatusCode, status, data)
	}
	return data
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return value
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Bound diagnostic storage even if a broken supervisor logs in a tight loop.
type boundedLog struct {
	mu   sync.Mutex
	data []byte
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	const limit = 32 << 10
	n := len(p)
	if len(p) > limit {
		p = p[len(p)-limit:]
	}
	b.data = append(b.data, p...)
	if len(b.data) > limit {
		b.data = b.data[len(b.data)-limit:]
	}
	return n, nil
}

func (b *boundedLog) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
