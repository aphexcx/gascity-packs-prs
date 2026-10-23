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
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
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
	google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
		g.Messages["spaces/room/messages/inbound"] = &api.Message{
			Name: "spaces/room/messages/inbound", Text: "hello from Chat",
			Thread: &api.Thread{Name: "spaces/room/threads/example"},
		}
	})
	cfg := &config.Config{
		Secrets:    config.Secrets{CityName: "examplecity", GCAPIBase: gc.base},
		SecretsDir: gc.root,
		Endpoints:  config.Endpoints{ChatAPIBase: google.URL(), StateFile: filepath.Join(gc.root, "state.json")},
	}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "example"}}
	chatClient, err := chat.New(t.Context(), gc.client, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err := chatClient.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	adapter, err := server.New(server.Options{
		Config: cfg, Ledger: ledger.New(), LedgerMu: new(sync.Mutex),
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

	reg := wire.Registration{
		Provider: "gchat", AccountID: "example", CallbackURL: handler.URL,
		Capabilities:      wire.Capabilities{SupportsAttachments: true, MaxMessageLength: 32000},
		ReplyInstructions: "gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>",
	}
	registered := decode[struct {
		Status    string `json:"status"`
		Provider  string `json:"provider"`
		AccountID string `json:"account_id"`
	}](t, gc.post(t, "adapters", reg, http.StatusCreated))
	if registered.Status != "registered" || registered.Provider != reg.Provider || registered.AccountID != reg.AccountID {
		t.Fatalf("registration was not accepted: %+v", registered)
	}
	t.Log("registration: HTTP 201 with wire.Registration and PascalCase capabilities")

	conversation := wire.Conversation{
		ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: "spaces/room", Kind: "room",
	}
	inbound := wire.InboundEnvelope{Message: wire.InboundMessage{
		ProviderMessageID: "spaces/room/messages/inbound", Conversation: conversation,
		Actor: wire.Actor{ID: "users/example", DisplayName: "Example"}, Text: "hello from Chat",
		ReplyToMessageID: "spaces/room/threads/example", ReceivedAt: time.Now().UTC(),
	}}
	// No default route or running agent is needed to prove acceptance. gc
	// reports the definite no_route delivery outcome and a concluded receipt.
	accepted := decode[wire.InboundResponse](t, gc.post(t, "inbound", inbound, http.StatusOK))
	if accepted.Delivery.Status != "no_route" || accepted.Delivery.ReceiptID == "" {
		t.Fatalf("inbound delivery object: %+v", accepted.Delivery)
	}
	receipt := decode[wire.ReceiptResponse](t, gc.get(t, "inbound/receipts/"+accepted.Delivery.ReceiptID))
	if receipt.State != "concluded" || receipt.Delivery.Status != "no_route" {
		t.Fatalf("inbound receipt: %+v", receipt)
	}
	t.Log("inbound: HTTP 200, delivery=no_route, receipt=concluded")

	// gc supports a pure session-ID binding without starting an agent. This
	// exercises real outbound authorization and transcript persistence while
	// avoiding any inference process or provider credentials.
	const sessionID = "example-session"
	gc.post(t, "bind", struct {
		Conversation wire.Conversation `json:"conversation"`
		SessionID    string            `json:"session_id"`
	}{conversation, sessionID}, http.StatusOK)
	outbound := wire.OutboundRequest{
		SessionID: sessionID, Conversation: conversation, Text: "hello from gc",
		ReplyToMessageID: inbound.Message.ReplyToMessageID, IdempotencyKey: "example-publish",
	}
	resultBody := gc.post(t, "outbound", outbound, http.StatusOK)
	result := decode[wire.OutboundResult](t, resultBody)
	if !result.Receipt.Delivered || result.Receipt.MessageID == "" || result.TranscriptEntry == nil {
		t.Fatalf("outbound receipt/transcript not decoded: %s", resultBody)
	}
	// OutboundResult only models fields the command client uses. Independently
	// inspect gc's returned receipt to pin its string-valued metadata contract.
	full := decode[struct {
		Receipt struct {
			Conversation wire.Conversation `json:"Conversation"`
			Metadata     map[string]string `json:"Metadata"`
		} `json:"Receipt"`
	}](t, resultBody)
	if full.Receipt.Conversation != conversation || !reflect.DeepEqual(full.Receipt.Metadata, map[string]string{
		"thread": outbound.ReplyToMessageID, "parts": "1",
	}) {
		t.Fatalf("gc lost receipt scope or string metadata: %s", resultBody)
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
			t.Errorf("expected one Google reply alongside the inbound fixture, got %+v", g.Messages)
		}
	})
	t.Log("outbound: real /publish callback with X-GC-Request; Receipt.Delivered=true; scope, message ID, thread and parts=\"1\" preserved; transcript recorded")
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(gcHome, "supervisor.toml"), fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\nallow_mutations = true\n", port))
	writeFile(t, filepath.Join(root, "gitconfig"), "[user]\nname = example\nemail = example@example.invalid\n[beads]\nrole = maintainer\n")
	// Deliberately do not inherit GC_*, BEADS_*, credentials, proxies, git
	// overrides or session identity from the lane running this test.
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"GC_HOME=" + gcHome, "XDG_RUNTIME_DIR=" + runtimeDir,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(root, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1",
		"DOLT_ROOT_PATH=" + root, "GC_BEADS=file", "GC_DOLT=skip", "GC_SESSION=fake", "GC_SUPERVISOR_LOG_TEE=0",
	}
	configPath := filepath.Join(root, "city-input.toml")
	writeFile(t, configPath, "[workspace]\nname = \"examplecity\"\n[beads]\nprovider = \"file\"\n")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	init := exec.CommandContext(ctx, bin, "init", "--file", configPath, "--no-start", city)
	init.Dir, init.Env, init.WaitDelay = root, env, 2*time.Second
	if output, err := init.CombinedOutput(); err != nil {
		t.Fatalf("gc init: %v\n%s", err, output)
	}
	// --no-start avoids launchd/systemd and their ambient environment. The
	// isolated supervisor reads only this one-city registry at startup.
	writeFile(t, filepath.Join(gcHome, "cities.toml"), fmt.Sprintf("[[cities]]\npath = %q\nname = \"examplecity\"\n", city))
	ctx, stop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "supervisor", "run")
	cmd.Dir, cmd.Env = root, env
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
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
			t.Fatalf("supervisor exited before readiness: %v\n%s", waitErr, log.String())
		case <-deadline.C:
			t.Fatalf("temporary city did not become ready\n%s", log.String())
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-ticker.C:
			resp, err := gc.client.Get(gc.base + "/v0/city/examplecity/extmsg/adapters")
			if err == nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
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
	req, err := http.NewRequestWithContext(t.Context(), method, gc.base+"/v0/city/examplecity/extmsg/"+path, bytes.NewReader(data))
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
