package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

type fixture struct {
	s       *Server
	g       *fakegoogle.Server
	gc      *fakegc.Server
	l       *ledger.Ledger
	cfg     *config.Config
	mu      *sync.Mutex
	url     string
	options Options
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	g := fakegoogle.New(t, nil)
	g.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE", DisplayName: "Example"}
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
		g.Spaces["spaces/group"] = &api.Space{Name: "spaces/group", SpaceType: "GROUP_CHAT"}
	})
	gc := fakegc.New(t)
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity", GCAPIBase: gc.URL(), ServiceSocket: filepath.Join(dir, "adapter.sock")}, SecretsDir: dir,
		Endpoints: config.Endpoints{ChatAPIBase: g.URL(), UploadRoot: dir, MediaMaxBytes: 1024, StateFile: filepath.Join(dir, "state.json")}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
	c, err := chat.New(context.Background(), http.DefaultClient, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	l := ledger.New()
	mu := new(sync.Mutex)
	opts := Options{Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: cfg.Endpoints.StateFile, SettingsPath: filepath.Join(dir, "config.toml"), Chat: c, GC: gcclient.New(gc.URL(), "examplecity")}
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.SetHealth(Health{State: "ready"})
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	reg := wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: h.URL, Capabilities: wire.Capabilities{SupportsAttachments: true, MaxMessageLength: 32000}}
	if err := opts.GC.Register(context.Background(), reg); err != nil {
		t.Fatal(err)
	}
	return &fixture{s, g, gc, l, cfg, mu, h.URL, opts}
}

func conversation(space string) wire.Conversation {
	kind := "room"
	if space == "spaces/dm" {
		kind = "dm"
	}
	return wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: space, Kind: kind}
}
func (f *fixture) call(t *testing.T, path string, value any) (int, []byte, http.Header) {
	t.Helper()
	method := http.MethodGet
	var body io.Reader
	if value != nil {
		method = http.MethodPost
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, f.url+path, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b, resp.Header
}
func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: %v", data, err)
	}
	return v
}
func (f *fixture) count() int {
	n := 0
	f.g.Configure(func(g *fakegoogle.Server) { n = len(g.Messages) })
	return n
}
func (f *fixture) send(t *testing.T, p wire.PublishRequest) wire.PublishReceipt {
	t.Helper()
	code, b, _ := f.call(t, "/publish", p)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	return decode[wire.PublishReceipt](t, b)
}
func command(key string) map[string]any {
	return map[string]any{"session_id": "session", "conversation_id": "spaces/room", "text": "**hello**", "idempotency_key": key}
}

func TestPublishIdempotentAndWire(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "**hello**", IdempotencyKey: "key"}
	a := f.send(t, p)
	b := f.send(t, p)
	if !a.Delivered || a.MessageID != b.MessageID || f.count() != 1 {
		t.Fatalf("%+v %+v count %d", a, b, f.count())
	}
	_, raw, _ := f.call(t, "/publish", p)
	want := `{"message_id":"` + a.MessageID + `","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/room","kind":"room"},"delivered":true,"metadata":{"parts":"1","thread":"` + a.Metadata["thread"] + `"}}`
	if string(bytes.TrimSpace(raw)) != want {
		t.Fatalf("wire: %s", raw)
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		if g.Messages[a.MessageID].Text != "*hello*" {
			t.Error("markdown not converted")
		}
	})
	saved, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Sends["key"].Parts[0].RequestID != "key-1" || saved.Messages[a.MessageID].SendKey != "key" {
		t.Fatalf("ledger: %+v", saved.Sends["key"])
	}
}
func TestPublishDerivedKeyAndMultipartVanishedThread(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: strings.Repeat("x", 33000), ReplyToMessageID: "spaces/room/threads/gone"}
	a := f.send(t, p)
	b := f.send(t, p)
	if !a.Delivered || a.MessageID != b.MessageID || a.Metadata["parts"] != "2" || f.count() != 2 {
		t.Fatalf("%+v %+v", a, b)
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		for _, m := range g.Messages {
			if m.Thread.Name != a.Metadata["thread"] {
				t.Error("parts split across threads")
			}
		}
	})
	for key := range f.l.Sends {
		if !strings.HasPrefix(key, "derived-") {
			t.Fatal(key)
		}
	}
}
func TestPublishUnnamedSpaces(t *testing.T) {
	for _, space := range []string{"spaces/dm", "spaces/group"} {
		t.Run(space, func(t *testing.T) {
			f := setup(t)
			r := f.send(t, wire.PublishRequest{Conversation: conversation(space), Text: "hello", IdempotencyKey: "key"})
			if !r.Delivered {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestPublishRateLimitAndDeadline(t *testing.T) {
	f := setup(t)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			http.Redirect(w, r, f.g.URL()+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":429,"message":"quota"}}`)
	}))
	defer h.Close()
	ep := f.cfg.Endpoints
	ep.ChatAPIBase = h.URL
	c, err := chat.New(context.Background(), http.DefaultClient, ep)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	start := time.Now()
	r := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "key"})
	if r.Delivered || r.FailureKind != "rate_limited" || r.RetryAfter != int64(time.Minute) || time.Since(start) > time.Second {
		t.Fatalf("%+v elapsed %v", r, time.Since(start))
	}
	// A failure must not become a cached final receipt: the same key can recover.
	f.s.SetClients(f.options.Chat, f.options.GC)
	if r = f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "key"}); !r.Delivered {
		t.Fatalf("retry: %+v", r)
	}
}
func TestCommandBoundUnboundAndWrongOwner(t *testing.T) {
	for _, mode := range []string{"bound", "unbound", "wrong-owner", "no-session"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			cmd := command("key")
			if mode == "bound" {
				f.gc.Bind(conversation("spaces/room"), "session")
			}
			if mode == "wrong-owner" {
				f.gc.Bind(conversation("spaces/room"), "someone-else")
			}
			if mode == "no-session" {
				cmd["session_id"] = ""
			}
			code, b, _ := f.call(t, "/commands/publish", cmd)
			r := decode[CommandResult](t, b)
			if code != 200 {
				t.Fatalf("%d %s", code, b)
			}
			if mode == "wrong-owner" {
				if r.Delivered || f.count() != 0 || r.Reason == "" {
					t.Fatalf("%+v", r)
				}
				return
			}
			if !r.Delivered || r.Recorded != (mode == "bound") || f.count() != 1 || r.Key != "key" {
				t.Fatalf("%+v", r)
			}
			if mode != "bound" && !strings.Contains(r.Reason, "no active binding") {
				t.Fatal(r.Reason)
			}
			if len(f.gc.Published()) != map[bool]int{true: 1, false: 0}[mode == "bound"] {
				t.Fatal("unexpected callback count")
			}
		})
	}
}
func TestUploadBoundAndPathValidation(t *testing.T) {
	for _, mode := range []string{"valid", "outside", "symlink", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.gc.Bind(conversation("spaces/room"), "session")
			path := filepath.Join(f.cfg.Endpoints.UploadRoot, "file.txt")
			data := []byte("attachment")
			if mode == "oversize" {
				data = make([]byte, 1025)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "outside" {
				path = filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "symlink" {
				link := filepath.Join(f.cfg.Endpoints.UploadRoot, "link")
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
				path = link
			}
			cmd := command("upload-key")
			cmd["path"] = path
			_, b, _ := f.call(t, "/commands/upload", cmd)
			r := decode[CommandResult](t, b)
			if mode != "valid" {
				if r.Delivered || f.count() != 0 || !strings.Contains(r.Reason, path) {
					t.Fatalf("%+v", r)
				}
				return
			}
			if !r.Delivered || !r.Recorded || f.count() != 1 || len(f.gc.Published()) != 1 || f.gc.Published()[0].IdempotencyKey != "upload-key" {
				t.Fatalf("%+v", r)
			}
			f.g.Configure(func(g *fakegoogle.Server) {
				if len(g.Messages[r.MessageID].Attachment) != 1 {
					t.Error("attachment missing")
				}
			})
		})
	}
}
func TestUnrecordedRetriedAfterRestart(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "upload"}[upload], func(t *testing.T) {
			f := setup(t)
			f.gc.Bind(conversation("spaces/room"), "session")
			f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = true })
			path := "/commands/publish"
			cmd := command("recover-key")
			if upload {
				path = "/commands/upload"
				file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
				if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
				cmd["path"] = file
			}
			_, b, _ := f.call(t, path, cmd)
			r := decode[CommandResult](t, b)
			if !r.Delivered || r.Recorded || r.Reason == "" {
				t.Fatalf("%+v", r)
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			opts := f.options
			opts.Ledger = l
			s, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			s.SetHealth(Health{State: "ready"})
			h := httptest.NewServer(s)
			defer h.Close()
			if err := opts.GC.Register(context.Background(), wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: h.URL}); err != nil {
				t.Fatal(err)
			}
			f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = false })
			if err := s.RetryPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !l.Sends["recover-key"].Recorded || f.count() != 1 {
				t.Fatalf("%+v count %d", l.Sends["recover-key"], f.count())
			}
		})
	}
}
func TestResolveAcceptAndReplay(t *testing.T) {
	f := setup(t)
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC})
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetPoll(p.Poll)
	for _, action := range []string{"accept", "replay"} {
		id := "spaces/room/messages/" + action
		m := wire.InboundMessage{ProviderMessageID: id, Conversation: conversation("spaces/room"), Actor: wire.Actor{ID: "users/222", DisplayName: "Example"}, Text: "hello", ReceivedAt: time.Now()}
		b, _ := json.Marshal(m)
		c := &ledger.ChangeState{ID: id, Space: "spaces/room", Message: id, Status: "unresolved", Event: string(b), Attempts: 4, Created: time.Now().Add(-2 * time.Hour)}
		f.l.Changes[id] = c
		f.l.Unresolved[id] = c
		code, b, _ := f.call(t, "/resolve", map[string]string{"id": id, "action": action})
		if code != 200 {
			t.Fatalf("%d %s", code, b)
		}
		if f.l.Changes[id].Status != "committed" || len(f.l.Unresolved) != 0 {
			t.Fatalf("%+v", f.l.Changes[id])
		}
	}
	if len(f.gc.Inbound()) != 1 {
		t.Fatal("accept reposted or replay failed")
	}
}
func TestHealthDuringInitAndUnixSocket(t *testing.T) {
	f := setup(t)
	// macOS limits sockaddr_un paths to 104 bytes. Keep only the socket at a
	// short temporary pathname; Serve closes and unlinks it before cleanup.
	f.s.mu.Lock()
	f.s.cfg.Secrets.ServiceSocket = filepath.Join(os.TempDir(), fmt.Sprintf("gchat-%d.sock", time.Now().UnixNano()))
	f.s.mu.Unlock()
	f.s.SetHealth(Health{State: "initializing", Reason: "loading"})
	code, b, h := f.call(t, "/healthz", nil)
	if code != 200 || h.Get("X-GC-Health") != "degraded" || decode[Health](t, b).State != "initializing" {
		t.Fatalf("%d %s %v", code, b, h)
	}
	r := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "no", IdempotencyKey: "blocked"})
	if r.Delivered || f.count() != 0 {
		t.Fatalf("%+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln, err := f.s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx, ln) }()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", f.s.configuration().Secrets.ServiceSocket)
	}}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get("http://unix/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInboundInventoryDoesNotConfuseNamedAndGroupSpaces(t *testing.T) {
	f := setup(t)
	for _, space := range []string{"spaces/room", "spaces/group"} {
		// Inbound's shared ledger stores gc kinds, not Google SpaceType values.
		f.l.Spaces[space] = &ledger.SpaceState{Kind: "room"}
		p := wire.PublishRequest{Conversation: conversation(space), Text: strings.Repeat("x", 33000), ReplyToMessageID: space + "/threads/gone", IdempotencyKey: space}
		r := f.send(t, p)
		if !r.Delivered {
			t.Fatalf("%+v", r)
		}
		f.g.Configure(func(g *fakegoogle.Server) {
			for _, m := range g.Messages {
				if m.Space.Name == space && m.Thread.Name != r.Metadata["thread"] {
					t.Error("one publish created multiple threads")
				}
			}
		})
	}
}

func TestRegistrarContractRetriesAndReconnectDoesNotBlock(t *testing.T) {
	f := setup(t)
	var mu sync.Mutex
	attempts := 0
	var registration wire.Registration
	var registrationKey string
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		key := r.Header.Get("Idempotency-Key")
		if key == "" || (registrationKey != "" && key != registrationKey) {
			t.Error("registration retries require one stable idempotency key")
		}
		registrationKey = key
		if r.Header.Get(wire.RequestHeader) == "" {
			t.Error("missing mutation header")
		}
		if attempts == 1 {
			w.WriteHeader(503)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil {
			t.Error(err)
		}
		w.WriteHeader(200)
	}))
	defer h.Close()
	f.s.mu.Lock()
	f.s.cfg.Secrets.GCAPIBase = h.URL
	f.s.cfg.Secrets.ServiceURLPrefix = "/svc/gchat"
	f.s.mu.Unlock()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.s.Register(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	reg := registration
	n := attempts
	mu.Unlock()
	if n != 2 || reg.CallbackURL != h.URL+"/svc/gchat" || reg.AccountID != "example" || !reg.Capabilities.SupportsAttachments || reg.Capabilities.MaxMessageLength != 32000 || reg.ReplyInstructions != "gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>" {
		t.Fatalf("attempts=%d %+v", n, reg)
	}
	// A refused connection schedules one service-lifetime registration without
	// spending a command's 25-second deadline waiting for registration.
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { entered <- struct{}{}; <-block; w.WriteHeader(200) }))
	defer slow.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(slow.URL, "examplecity"))
	f.s.ObserveGCError(syscall.ECONNREFUSED)
	<-entered
	done := make(chan struct{})
	go func() { f.s.ObserveGCError(syscall.ECONNREFUSED); close(done) }()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		close(block)
		t.Fatal("reconnect notification blocked")
	}
	close(block)
}
func TestSpacesWritePreservesOmittedTablesAndResetsWake(t *testing.T) {
	f := setup(t)
	if err := os.WriteFile(f.options.SettingsPath, []byte("[[space]]\nid = \"spaces/room\"\nwake = \"mentions\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.l.Spaces["spaces/room"] = &ledger.SpaceState{Kind: "SPACE", Wake: "mentions", MessageCursor: time.Now()}
	code, b, _ := f.call(t, "/spaces", map[string]string{"id": "spaces/room", "wake": ""})
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	data, err := os.ReadFile(f.options.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "[defaults]") || strings.Contains(string(data), "[media]") || f.l.Spaces["spaces/room"].Wake != "" || f.l.Spaces["spaces/room"].MessageCursor.IsZero() {
		t.Fatalf("%s %+v", data, f.l.Spaces["spaces/room"])
	}
	code, b, _ = f.call(t, "/spaces", nil)
	if code != 200 || !bytes.Contains(b, []byte(`"wake":"all"`)) {
		t.Fatalf("%d %s", code, b)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (fn roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestMultipartFailureResumesPersistedParts(t *testing.T) {
	f := setup(t)
	httpClient := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Query().Get("requestId"), "-2") {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err := chat.New(context.Background(), httpClient, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: strings.Repeat("x", 33000), IdempotencyKey: "multipart", ReplyToMessageID: "spaces/room/threads/missing"}
	r := f.send(t, p)
	if r.Delivered || r.FailureKind != "rate_limited" || f.count() != 1 {
		t.Fatalf("%+v count %d", r, f.count())
	}
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Sends["multipart"].Parts[0].MessageName == "" || l.Sends["multipart"].Parts[1].MessageName != "" {
		t.Fatal("multipart progress not saved")
	}
	opts := f.options
	opts.Ledger = l
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetHealth(Health{State: "ready"})
	if err := s.normalize(&p); err != nil {
		t.Fatal(err)
	}
	r, err = s.publish(context.Background(), p, nil)
	if err != nil || !r.Delivered || f.count() != 2 {
		t.Fatalf("%+v %v count %d", r, err, f.count())
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		for _, m := range g.Messages {
			if m.Thread.Name != r.Metadata["thread"] {
				t.Error("retry opened a new thread")
			}
		}
	})
}

func TestUploadCrashAfterGoogleAcceptanceRecovers(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "session")
	file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			// Simulate failure to commit the message name/receipt after Google
			// stored it. The prior intent and attachment reference remain on disk.
			f.s.opts.LedgerPath = filepath.Join(f.cfg.Endpoints.UploadRoot, "absent", "state.json")
		}
		return resp, err
	})}
	c, err := chat.New(context.Background(), client, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	cmd := command("crash")
	cmd["path"] = file
	_, b, _ := f.call(t, "/commands/upload", cmd)
	r := decode[CommandResult](t, b)
	if r.Recorded || f.count() != 1 {
		t.Fatalf("%+v count %d", r, f.count())
	}
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Sends["crash"].Receipt != nil || l.Sends["crash"].Upload.ResourceName == "" {
		t.Fatalf("%+v", l.Sends["crash"])
	}
	opts := f.options
	opts.Ledger = l
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetHealth(Health{State: "ready"})
	h := httptest.NewServer(s)
	defer h.Close()
	if err := opts.GC.Register(context.Background(), wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: h.URL}); err != nil {
		t.Fatal(err)
	}
	if err := s.RetryPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !l.Sends["crash"].Recorded || f.count() != 1 {
		t.Fatalf("%+v count %d", l.Sends["crash"], f.count())
	}
	f.g.Configure(func(g *fakegoogle.Server) {
		if len(g.Media) != 1 {
			t.Error("attachment uploaded again")
		}
	})
}

func TestPublishConcurrentKeysAndCanceledQueue(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "parallel"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.send(t, p)
			if !r.Delivered {
				t.Errorf("%+v", r)
			}
		}()
	}
	wg.Wait()
	if f.count() != 1 {
		t.Fatal("concurrent duplicates")
	}
	unlock, err := f.s.lock(context.Background(), "send:queued")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	p.IdempotencyKey = "queued"
	data, _ := json.Marshal(p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest("POST", "/publish", bytes.NewReader(data)).WithContext(ctx)
	w := httptest.NewRecorder()
	f.s.ServeHTTP(w, req)
	r := decode[wire.PublishReceipt](t, w.Body.Bytes())
	if w.Code != 200 || r.Delivered || r.FailureKind != "transient" || f.count() != 1 {
		t.Fatalf("%d %+v", w.Code, r)
	}
	if _, ok := f.l.Sends["queued"]; ok {
		t.Fatal("canceled wait wrote intent")
	}
}

func TestPublishAlwaysHas25SecondBudget(t *testing.T) {
	f := setup(t)
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 25*time.Second {
			t.Error("missing bounded publish deadline")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err := chat.New(context.Background(), client, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	if r := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "budget"}); !r.Delivered {
		t.Fatalf("%+v", r)
	}
}

func TestPublishDeadlineWhileLedgerIsLocked(t *testing.T) {
	f := setup(t)
	// The holder outlives the request, but eventually releases even before the
	// fix so this regression fails promptly instead of hanging the test suite.
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		f.mu.Lock()
		close(held)
		select {
		case <-release:
		case <-time.After(500 * time.Millisecond):
		}
		f.mu.Unlock()
	}()
	<-held
	defer close(release)
	p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "locked"}
	data, _ := json.Marshal(p)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	w := httptest.NewRecorder()
	start := time.Now()
	f.s.ServeHTTP(w, httptest.NewRequest("POST", "/publish", bytes.NewReader(data)).WithContext(ctx))
	r := decode[wire.PublishReceipt](t, w.Body.Bytes())
	if time.Since(start) > 200*time.Millisecond || w.Code != 200 || r.Delivered || r.FailureKind != "transient" || r.RetryAfter <= 0 {
		t.Fatalf("deadline not respected: elapsed=%v code=%d receipt=%+v", time.Since(start), w.Code, r)
	}
}

func TestReadReactStatusAndState(t *testing.T) {
	f := setup(t)
	a := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "older", IdempotencyKey: "older"})
	b := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "newer", IdempotencyKey: "newer", ReplyToMessageID: a.Metadata["thread"]})
	code, raw, _ := f.call(t, "/read?space=spaces/room&limit=1&thread="+a.Metadata["thread"], nil)
	msgs := decode[[]wire.InboundMessage](t, raw)
	if code != 200 || len(msgs) != 1 || msgs[0].ProviderMessageID != b.MessageID || msgs[0].Actor.DisplayName != fakegoogle.UserID || msgs[0].Conversation != conversation("spaces/room") {
		t.Fatalf("%d %s", code, raw)
	}
	for _, remove := range []bool{false, true} {
		code, raw, _ = f.call(t, "/react", map[string]any{"message": a.MessageID, "emoji": "👍", "remove": remove})
		if code != 200 {
			t.Fatalf("%d %s", code, raw)
		}
		f.g.Configure(func(g *fakegoogle.Server) {
			want := 1
			if remove {
				want = 0
			}
			if len(g.Reactions) != want {
				t.Errorf("reactions %d want %d", len(g.Reactions), want)
			}
		})
	}
	f.l.Spaces["spaces/room"].Backlog = []string{"one", "two"}
	f.l.Counters["turns:spaces/room"] = 3
	code, raw, _ = f.call(t, "/status", nil)
	status := decode[map[string]any](t, raw)
	if code != 200 || status["backlog"] != float64(2) || status["unresolved"] != float64(0) || status["subscription"] == nil || status["health"] == nil {
		t.Fatalf("%d %s", code, raw)
	}
	backup := filepath.Join(f.cfg.Endpoints.UploadRoot, "backup.json")
	if err := f.l.Save(backup); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"reset", "restore"} {
		f.s.SetHealth(Health{State: "recovery-required", Reason: "corrupt ledger"})
		code, raw, _ = f.call(t, "/state", map[string]string{"action": action, "file": backup})
		if code != 200 || f.s.Health().State != "initializing" {
			t.Fatalf("%d %s", code, raw)
		}
		if action == "reset" {
			if len(f.l.Sends) != 0 || !bytes.Contains(raw, []byte("lost or duplicated")) {
				t.Fatalf("%s", raw)
			}
		} else {
			if len(f.l.Sends) != 2 {
				t.Fatal("restore lost sends")
			}
		}
	}
}

func TestReadNormalizesMentionsAndMediaLikeInbound(t *testing.T) {
	f := setup(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Endpoints.MediaDir = filepath.Join(dir, "media")
	f.s.SetConfig(*f.cfg)
	f.options.Chat, err = chat.New(context.Background(), http.DefaultClient, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.options.Chat.SetIdentityUserID(fakegoogle.UserID); err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(f.options.Chat, f.options.GC)
	m := &api.Message{Name: "spaces/room/messages/history", Text: "Hi @Reader", CreateTime: "2026-10-22T10:00:00Z",
		Space: &api.Space{Name: "spaces/room", SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", DisplayName: "Reader", Type: "HUMAN"}, Thread: &api.Thread{Name: "spaces/room/threads/topic"},
		Annotations: []*api.Annotation{{Type: "USER_MENTION", StartIndex: 3, Length: 7, UserMention: &api.UserMentionMetadata{User: &api.User{Name: "users/456", DisplayName: "Example Reader"}}}},
		Attachment: []*api.Attachment{
			{Name: "drive", ContentName: "document", ContentType: "text/plain", DriveDataRef: &api.DriveDataRef{DriveFileId: "drive-id"}},
			{Name: "upload", ContentName: "report.txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/report"}},
			{Name: "missing", ContentName: "missing.txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "spaces/room/media/missing"}},
		}}
	f.g.Configure(func(g *fakegoogle.Server) {
		g.Messages[m.Name] = m
		g.Media["spaces/room/media/report"] = []byte("report")
	})
	// A message not yet observed on the stream also uses the shared normalizer.
	_, fresh, _ := f.call(t, "/read?space=spaces/room", nil)
	for _, fragment := range []string{"Hi Example Reader", "https://drive.google.com/file/d/drive-id/view", "(text/plain) file://", "missing.txt unavailable: file not found"} {
		if !bytes.Contains(fresh, []byte(fragment)) {
			t.Fatalf("unseen history missing %q: %s", fragment, fresh)
		}
	}
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Handle(context.Background(), &puller.Event{PubSubID: "history", Changes: []puller.Change{{Kind: "message.created", Space: "spaces/room", Message: m}}}); err != nil {
		t.Fatal(err)
	}
	want := f.gc.Inbound()[0]
	code, raw, _ := f.call(t, "/read?space=spaces/room", nil)
	got := decode[[]wire.InboundMessage](t, raw)
	if code != 200 || len(got) != 1 {
		t.Fatalf("%d %s", code, raw)
	}
	// The persisted inbound text also keeps the already-downloaded local path.
	if got[0].Text != want.Text || got[0].Actor != want.Actor || got[0].ReplyToMessageID != want.ReplyToMessageID {
		t.Fatalf("history differs from turn:\n got %+v\nwant %+v", got[0], want)
	}
	if !strings.Contains(got[0].Text, "Hi Example Reader") || !strings.Contains(got[0].Text, "https://drive.google.com/file/d/drive-id/view") || !strings.Contains(got[0].Text, "(text/plain) file://") || !strings.Contains(got[0].Text, "missing.txt unavailable: file not found") {
		t.Fatalf("incomplete normalized content: %s", got[0].Text)
	}
}

func TestRecoveryBlocksSettingsAndFailedSaveDoesNotCommit(t *testing.T) {
	f := setup(t)
	f.s.SetHealth(Health{State: "recovery-required"})
	code, _, _ := f.call(t, "/spaces", map[string]string{"id": "spaces/room", "wake": "all"})
	if code != 503 {
		t.Fatalf("settings during recovery: %d", code)
	}
	f.s.SetHealth(Health{State: "ready"})
	f.s.opts.LedgerPath = filepath.Join(f.cfg.Endpoints.UploadRoot, "absent", "state.json")
	r := f.send(t, wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "fail"})
	if r.Delivered || f.count() != 0 || len(f.l.Sends) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestCommandUsesPersistedTextWhenKeyIsRetried(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "session")
	f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = true })
	cmd := command("stable")
	_, _, _ = f.call(t, "/commands/publish", cmd)
	cmd["text"] = "different retry body"
	f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = false })
	_, raw, _ := f.call(t, "/commands/publish", cmd)
	r := decode[CommandResult](t, raw)
	if !r.Recorded || f.count() != 1 {
		t.Fatalf("%+v", r)
	}
	requests := f.gc.Published()
	if requests[len(requests)-1].Text != "**hello**" {
		t.Fatal("recorded text differs from persisted sent text")
	}
}

func TestRegisterCancel(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.s.Register(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
}

func TestPublishLogsSanitizedFailureAndRecovery(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	f := setup(t)
	client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"secret-response-token"}}`)), Request: r}, nil
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	c, err := chat.New(context.Background(), client, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "private-message-body", IdempotencyKey: "log-key"}
	if f.send(t, p).Delivered {
		t.Fatal("expected failure")
	}
	f.s.SetClients(f.options.Chat, f.options.GC)
	for range 2 {
		if !f.send(t, p).Delivered {
			t.Fatal("expected recovery and cached result")
		}
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one failure and one recovery: %s", logs.String())
	}
	failure := decode[map[string]any](t, []byte(lines[0]))
	recovery := decode[map[string]any](t, []byte(lines[1]))
	if failure["level"] != "WARN" || failure["space"] != "spaces/room" || failure["key"] != "log-key" || failure["cause_kind"] != "rate_limited" || failure["http_status"] != float64(429) || recovery["level"] != "INFO" {
		t.Fatalf("unexpected records: %s", logs.String())
	}
	if strings.Contains(logs.String(), p.Text) || strings.Contains(logs.String(), "secret-response-token") {
		t.Fatal("sensitive error contents logged")
	}
	logs.Reset()
	f.s.stateGate.Lock()
	blocked := f.send(t, p)
	f.s.stateGate.Unlock()
	if blocked.Delivered || !f.send(t, p).Delivered {
		t.Fatal("expected recovery gate rejection then cached delivery")
	}
	lines = strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("recovery gate must log failure and recovery: %s", logs.String())
	}
}

func TestRegistrarLogsSanitizedFailureAndRecovery(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	f := setup(t)
	attempts := 0
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "secret-response-token")
			return
		}
		w.WriteHeader(200)
	}))
	defer h.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := f.s.Register(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Register(ctx); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected one failure and one recovery: %s", logs.String())
	}
	failure := decode[map[string]any](t, []byte(lines[0]))
	recovery := decode[map[string]any](t, []byte(lines[1]))
	if failure["level"] != "WARN" || failure["operation"] != "register" || failure["cause_kind"] != "transient" || failure["http_status"] != float64(503) || recovery["level"] != "INFO" {
		t.Fatalf("unexpected records: %s", logs.String())
	}
	if strings.Contains(logs.String(), "secret-response-token") {
		t.Fatal("sensitive error contents logged")
	}
}

func TestGCRefusalFromInboundAlsoRegisters(t *testing.T) {
	f := setup(t)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	g := gcclient.New(h.URL, "examplecity")
	f.s.SetClients(f.options.Chat, g)
	h.Close()
	_, err := g.Inbound(context.Background(), wire.InboundMessage{})
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("%v", err)
	}
	f.s.mu.RLock()
	registering := f.s.registering
	f.s.mu.RUnlock()
	if !registering {
		t.Fatal("refused inbound call did not schedule registration")
	}
}

func TestCachedReceiptNeedsNeitherCredentialsNorAnotherSave(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "durable"}
	first := f.send(t, p)
	f.s.SetHealth(Health{State: "needs-sign-in"})
	f.s.opts.LedgerPath = filepath.Join(f.cfg.Endpoints.UploadRoot, "absent", "state.json")
	r := f.send(t, p)
	if !r.Delivered || r.MessageID != first.MessageID || f.count() != 1 {
		t.Fatalf("lost durable receipt: %+v", r)
	}
}

func TestCachedCommandOutcomePrecedesReadinessInventoryAndWrites(t *testing.T) {
	for _, route := range []string{"publish", "upload"} {
		for _, mode := range []string{"not-ready", "save-failure", "unrecorded", "unbound"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				f := setup(t)
				if mode != "unbound" {
					f.gc.Bind(conversation("spaces/dm"), "session")
				}
				if mode == "unrecorded" {
					f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = true })
				}
				cmd := command("") // Include derived keys and the DM conversation kind.
				cmd["conversation_id"] = "spaces/dm"
				if route == "upload" {
					file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
					if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
						t.Fatal(err)
					}
					cmd["path"] = file
				}
				_, raw, _ := f.call(t, "/commands/"+route, cmd)
				first := decode[CommandResult](t, raw)
				if !first.Delivered {
					t.Fatalf("first: %+v", first)
				}
				// Cached outcomes must not require inventory or a successful save.
				f.mu.Lock()
				f.l.Spaces = map[string]*ledger.SpaceState{}
				f.mu.Unlock()
				f.g.Configure(func(g *fakegoogle.Server) { g.Spaces = map[string]*api.Space{} })
				f.s.opts.LedgerPath = filepath.Join(f.cfg.Endpoints.UploadRoot, "absent", "state.json")
				if mode != "save-failure" {
					f.s.SetHealth(Health{State: "needs-sign-in"})
				}
				_, raw, _ = f.call(t, "/commands/"+route, cmd)
				got := decode[CommandResult](t, raw)
				if !got.Delivered || got.Recorded != first.Recorded || got.MessageID != first.MessageID || got.Key != first.Key || f.count() != 1 {
					t.Fatalf("lost outcome: first=%+v retry=%+v", first, got)
				}
			})
		}
	}
}

func TestUploadDeliverySurvivesFailedRecordingCallback(t *testing.T) {
	f := setup(t)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"Receipt":{"Delivered":false},"TranscriptEntry":null}`)
	}))
	defer h.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := command("recording-callback")
	cmd["path"] = file
	_, raw, _ := f.call(t, "/commands/upload", cmd)
	r := decode[CommandResult](t, raw)
	if !r.Delivered || r.Recorded || r.MessageID == "" || f.count() != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestCommandDeliverySurvivesLostCallbackResponse(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "session")
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Chat and the ledger have committed; gc cannot decode the lost body.
		f.s.ServeHTTP(httptest.NewRecorder(), r)
		_, _ = io.WriteString(w, `{"delivered":`)
	}))
	defer proxy.Close()
	reg := wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: proxy.URL, Capabilities: wire.Capabilities{SupportsAttachments: true, MaxMessageLength: 32000}}
	if err := f.options.GC.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	_, raw, _ := f.call(t, "/commands/publish", command("lost-callback"))
	first := decode[CommandResult](t, raw)
	if !first.Delivered || first.Recorded || first.MessageID == "" || first.FailureKind != "" || f.count() != 1 {
		t.Fatalf("durable success downgraded: %+v, sends=%d", first, f.count())
	}
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	v := l.Sends[first.Key]
	if v.Receipt == nil || !v.Receipt.Delivered || !v.RecordPending {
		t.Fatalf("lost pending transcript: %+v", v)
	}
	f.mu.Lock()
	*f.l = *l
	f.mu.Unlock()
	reg.CallbackURL = f.url
	if err := f.options.GC.Register(t.Context(), reg); err != nil {
		t.Fatal(err)
	}
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, raw, _ = f.call(t, "/commands/publish", command(first.Key))
	got := decode[CommandResult](t, raw)
	if !got.Delivered || !got.Recorded || got.MessageID != first.MessageID || f.count() != 1 {
		t.Fatalf("transcript recovery: %+v, sends=%d", got, f.count())
	}
}

func TestCommandDeliverySurvivesGCErrorAfterCallback(t *testing.T) {
	for _, mode := range []string{"503", "lost-response"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			f.gc.Bind(conversation("spaces/room"), "session")
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				req, err := http.NewRequestWithContext(r.Context(), r.Method, f.gc.URL()+r.URL.RequestURI(), r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				req.Header = r.Header.Clone()
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if mode == "503" {
					w.WriteHeader(503)
				} else {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				}
			}))
			defer proxy.Close()
			f.s.SetClients(f.options.Chat, gcclient.New(proxy.URL, "examplecity"))
			_, raw, _ := f.call(t, "/commands/publish", command("lost-gc-response"))
			first := decode[CommandResult](t, raw)
			if !first.Delivered || first.Recorded || first.MessageID == "" || first.FailureKind != "" || f.count() != 1 {
				t.Fatalf("gc error downgraded delivery: %+v sends=%d", first, f.count())
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			if !l.Sends[first.Key].RecordPending || !l.Sends[first.Key].Receipt.Delivered {
				t.Fatalf("lost transcript recovery: %+v", l.Sends[first.Key])
			}
			f.s.SetClients(f.options.Chat, f.options.GC)
			if err := f.s.RetryPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			if v := f.l.Sends[first.Key]; !v.Recorded || v.RecordPending || f.count() != 1 {
				t.Fatalf("recording retry: %+v sends=%d", v, f.count())
			}
		})
	}
}

func TestRecordingSuccessRemainsReportedWhenSaveFails(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "session")
	f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = true })
	_, raw, _ := f.call(t, "/commands/publish", command("recorded"))
	first := decode[CommandResult](t, raw)
	if !first.Delivered || first.Recorded {
		t.Fatalf("first: %+v", first)
	}
	f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = false })
	f.s.opts.LedgerPath = filepath.Join(f.cfg.Endpoints.UploadRoot, "absent", "state.json")
	_, raw, _ = f.call(t, "/commands/publish", command("recorded"))
	got := decode[CommandResult](t, raw)
	if !got.Delivered || !got.Recorded || got.MessageID != first.MessageID || got.Reason == "" || f.count() != 1 {
		t.Fatalf("lost known gc outcome: %+v", got)
	}
}

func TestDefiniteRecordingRefusalStopsAutomaticRecovery(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 409, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "refused-recording"}
			if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
				t.Fatal(err)
			}
			first := f.send(t, p)
			if !first.Delivered {
				t.Fatal(first)
			}
			calls := 0
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				respond(w, status, map[string]any{"code": "validation-failed", "detail": "binding belongs to another session"})
			}))
			defer h.Close()
			f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
			_, raw, _ := f.call(t, "/commands/publish", command(p.IdempotencyKey))
			result := decode[CommandResult](t, raw)
			if !result.Delivered || result.Recorded || result.MessageID != first.MessageID || result.Reason != "binding belongs to another session" || result.FailureKind == "" {
				t.Fatalf("recording refusal: %+v", result)
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			if v := l.Sends[p.IdempotencyKey]; v.RecordPending || !v.NotRecordable || v.RecordingFailureKind == "" {
				t.Fatalf("refusal was not durable before sweep: %+v", v)
			}
			_, statusBody, _ := f.call(t, "/status", nil)
			if decode[map[string]any](t, statusBody)["unrecorded"] != float64(0) {
				t.Fatalf("refusal remains pending: %s", statusBody)
			}
			f.mu.Lock()
			*f.l = *l
			f.mu.Unlock()
			for range 2 {
				if err := f.s.RetryPending(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			v := f.l.Sends[p.IdempotencyKey]
			if calls != 1 || v.RecordPending || !v.NotRecordable || !v.Receipt.Delivered || commandOutcome(v) != result {
				t.Fatalf("recording refusal not settled: %+v calls=%d", v, calls)
			}
			if strings.Count(logs.String(), `"level":"WARN"`) != 1 {
				t.Fatalf("expected one refusal warning: %s", logs.String())
			}
			_, raw, _ = f.call(t, "/status", nil)
			if decode[map[string]any](t, raw)["unrecorded"] != float64(0) {
				t.Fatalf("refused recording still pending: %s", raw)
			}
			// A corrected binding and explicit retry can record the same delivery.
			f.s.SetClients(f.options.Chat, f.options.GC)
			f.gc.Bind(p.Conversation, p.SessionID)
			_, raw, _ = f.call(t, "/commands/publish", command(p.IdempotencyKey))
			got := decode[CommandResult](t, raw)
			if !got.Delivered || !got.Recorded || got.MessageID != first.MessageID || got.Reason != "" || got.FailureKind != "" || f.count() != 1 {
				t.Fatalf("explicit recording retry: %+v", got)
			}
			saved, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			if v := saved.Sends[p.IdempotencyKey]; v.NotRecordable || v.RecordingFailureKind != "" {
				t.Fatalf("successful recording retained refusal: %+v", v)
			}
		})
	}
}

func TestDurableDeliverySurvivesNoBindingSaveFailure(t *testing.T) {
	f := setup(t)
	first := f.send(t, wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "saved"})
	if !first.Delivered {
		t.Fatal(first)
	}
	// A crash left a delivery receipt but no determination of recordability.
	f.s.opts.LedgerPath = filepath.Join(f.cfg.Endpoints.UploadRoot, "absent", "state.json")
	_, raw, _ := f.call(t, "/commands/publish", command("saved"))
	got := decode[CommandResult](t, raw)
	if !got.Delivered || got.Recorded || got.MessageID != first.MessageID || f.count() != 1 {
		t.Fatalf("lost saved delivery: %+v", got)
	}
}

func TestHistoryDoesNotReuseAnOlderTurnForALocalRevision(t *testing.T) {
	l := ledger.New()
	m := &api.Message{Name: "spaces/room/messages/edited", CreateTime: "2026-10-22T10:00:00Z", LastUpdateTime: "2026-10-22T11:00:00Z", Text: "current"}
	at, _ := time.Parse(time.RFC3339Nano, m.LastUpdateTime)
	l.Messages[m.Name] = &ledger.MessageState{Revision: at}
	original, _ := json.Marshal(wire.InboundMessage{ProviderMessageID: m.Name, Text: "old"})
	l.Changes[m.Name] = &ledger.ChangeState{Kind: "message.created", Event: string(original)}
	l.Changes[m.Name+"#updated#"+m.LastUpdateTime] = &ledger.ChangeState{Kind: "message.updated", Reason: "own"}
	if got, ok := historyContent(l, m); ok {
		t.Fatalf("used obsolete content: %+v", got)
	}
}

func TestHistoryNormalizesRetainedSpaceFailureWithCause(t *testing.T) {
	f := setup(t)
	m := &api.Message{Name: "spaces/room/messages/retained", CreateTime: "2026-10-22T10:00:00Z", Text: "document",
		Attachment: []*api.Attachment{{Name: "drive", ContentName: "document", ContentType: "text/plain", DriveDataRef: &api.DriveDataRef{DriveFileId: "drive-id"}}}}
	f.g.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
	at, _ := time.Parse(time.RFC3339Nano, m.CreateTime)
	f.l.Messages[m.Name] = &ledger.MessageState{Revision: at}
	raw, _ := json.Marshal(wire.InboundMessage{ProviderMessageID: m.Name, Text: m.Text})
	f.l.Changes[m.Name] = &ledger.ChangeState{Kind: "message.created", Reason: "space unavailable: permanent (HTTP 400)", Event: string(raw)}
	code, raw, _ := f.call(t, "/read?space=spaces/room", nil)
	got := decode[[]wire.InboundMessage](t, raw)
	if code != 200 || len(got) != 1 || !strings.Contains(got[0].Text, "https://drive.google.com/file/d/drive-id/view") || len(got[0].Attachments) != 1 {
		t.Fatalf("retained history was not normalized: %d %s", code, raw)
	}
}

func TestHistoryDoesNotReuseTurnBeforeRemoteEdit(t *testing.T) {
	f := setup(t)
	m := &api.Message{Name: "spaces/room/messages/edited", CreateTime: "2026-10-22T10:00:00Z", LastUpdateTime: "2026-10-22T11:00:00Z", Text: "first edit"}
	oldRevision, _ := time.Parse(time.RFC3339Nano, m.LastUpdateTime)
	f.l.Messages[m.Name] = &ledger.MessageState{Revision: oldRevision}
	id := m.Name + "#updated#" + m.LastUpdateTime
	raw, _ := json.Marshal(wire.InboundMessage{ProviderMessageID: id, Text: "edited: first edit"})
	// An update first observed as a creation uses the base name too. Both
	// records are plausible fallbacks unless the revision guard runs first.
	f.l.Changes[id] = &ledger.ChangeState{Kind: "message.updated", Event: string(raw)}
	raw, _ = json.Marshal(wire.InboundMessage{ProviderMessageID: m.Name, Text: "edited: first edit"})
	f.l.Changes[m.Name] = &ledger.ChangeState{Kind: "message.updated", Event: string(raw)}
	m.LastUpdateTime, m.Text = "2026-10-22T12:00:00Z", "latest remote edit"
	f.g.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
	code, raw, _ := f.call(t, "/read?space=spaces/room", nil)
	got := decode[[]wire.InboundMessage](t, raw)
	if code != 200 || len(got) != 1 || got[0].Text != m.Text {
		t.Fatalf("stale history: %d %s", code, raw)
	}
}

func TestStateResetRefusedThroughoutInFlightDelivery(t *testing.T) {
	f := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	c, err := chat.New(t.Context(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}, f.cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetClients(c, f.options.GC)
	raw, _ := json.Marshal(wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "in flight", IdempotencyKey: "in-flight"})
	done := make(chan *httptest.ResponseRecorder, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		w := httptest.NewRecorder()
		f.s.ServeHTTP(w, httptest.NewRequest("POST", "/publish", bytes.NewReader(raw)).WithContext(ctx))
		done <- w
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("delivery did not start")
	}
	f.s.SetHealth(Health{State: "recovery-required"})
	code, response, _ := f.call(t, "/state", map[string]string{"action": "reset"})
	if code != 409 || !bytes.Contains(response, []byte("delivery is still in flight")) {
		t.Fatalf("reset allowed during delivery: %d %s", code, response)
	}
	unblock()
	w := <-done
	if !decode[wire.PublishReceipt](t, w.Body.Bytes()).Delivered {
		t.Fatalf("delivery: %s", w.Body.String())
	}
	code, response, _ = f.call(t, "/state", map[string]string{"action": "reset"})
	if code != 200 {
		t.Fatalf("reset after delivery: %d %s", code, response)
	}
}

func TestStatusLastEventAndReloadedDefaults(t *testing.T) {
	f := setup(t)
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	f.s.opts.LastEvent = func() time.Time { return at }
	_, raw, _ := f.call(t, "/status", nil)
	status := decode[map[string]any](t, raw)
	if status["last_event_time"] != at.Format(time.RFC3339Nano) {
		t.Fatalf("%s", raw)
	}
	cfg := *f.cfg
	cfg.Settings.Defaults.Wake = config.WakeMentions
	f.s.SetConfig(cfg)
	_, raw, _ = f.call(t, "/spaces", nil)
	if !bytes.Contains(raw, []byte(`"wake":"mentions"`)) {
		t.Fatalf("%s", raw)
	}
}

func TestRejectedCommandNeverRetriesAutomatically(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "someone-else")
	_, raw, _ := f.call(t, "/commands/publish", command("rejected"))
	first := decode[CommandResult](t, raw)
	if first.Delivered || first.Reason == "" {
		t.Fatalf("expected rejection: %+v", first)
	}
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the persisted state, including after the binding changes.
	f.mu.Lock()
	*f.l = *l
	f.mu.Unlock()
	f.gc.Bind(conversation("spaces/room"), "session")
	if err := f.s.RetryPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 || f.l.Sends["rejected"].RecordingReason != first.Reason || f.l.Sends["rejected"].RecordPending {
		t.Fatalf("rejected command was retried or lost: %+v, sends=%d", f.l.Sends["rejected"], f.count())
	}
	// A caller may explicitly choose to retry the same intent and key.
	_, raw, _ = f.call(t, "/commands/publish", command("rejected"))
	if got := decode[CommandResult](t, raw); !got.Delivered || !got.Recorded || f.count() != 1 {
		t.Fatalf("caller retry: %+v", got)
	}
}

func TestUndeliveredCommandNeverRetriesAutomatically(t *testing.T) {
	for _, mode := range []string{"bound", "unbound", "upload", "gc-503", "gc-429", "gc-408", "gc-refused"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			if mode != "unbound" {
				f.gc.Bind(conversation("spaces/room"), "session")
			}
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			c, err := chat.New(context.Background(), client, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetClients(c, f.options.GC)
			wantKind, wantDelay := "rate_limited", int64(time.Minute)
			if strings.HasPrefix(mode, "gc-") {
				status := map[string]int{"gc-503": 503, "gc-429": 429, "gc-408": 408}[mode]
				h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Retry-After", "60")
					w.WriteHeader(status)
				}))
				defer h.Close()
				if mode == "gc-refused" {
					h.Close()
				}
				f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
				if mode != "gc-429" {
					wantKind = "transient"
				}
				if mode == "gc-refused" {
					wantDelay = int64(time.Second)
				}
			}
			path, cmd := "/commands/publish", command("undelivered")
			if mode == "upload" {
				path = "/commands/upload"
				file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
				if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
				cmd["path"] = file
			}
			_, raw, _ := f.call(t, path, cmd)
			first := decode[CommandResult](t, raw)
			if first.Delivered || first.FailureKind != wantKind || first.RetryAfter != wantDelay {
				t.Fatalf("expected typed failure: %+v", first)
			}
			f.s.SetClients(f.options.Chat, f.options.GC)
			if err := f.s.RetryPending(context.Background()); err != nil {
				t.Fatal(err)
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			v := l.Sends["undelivered"]
			if f.count() != 0 || v.RecordPending || v.Receipt == nil || v.Receipt.Delivered || v.Receipt.FailureKind != wantKind || v.Receipt.RetryAfter != wantDelay {
				t.Fatalf("failure not retained: %+v sends=%d", v, f.count())
			}
			_, raw, _ = f.call(t, path, cmd)
			if got := decode[CommandResult](t, raw); !got.Delivered || f.count() != 1 {
				t.Fatalf("caller retry: %+v", got)
			}
		})
	}
}

// Reload the ledger into a fresh server so recovery tests cannot pass using
// state that never reached disk. The old server is idle and remains cleanup-owned.
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	opts := f.options
	opts.Ledger, opts.LedgerMu = l, new(sync.Mutex)
	s, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	s.SetHealth(Health{State: "ready"})
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	if err := opts.GC.Register(t.Context(), wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: h.URL}); err != nil {
		t.Fatal(err)
	}
	f.s, f.l, f.mu, f.url = s, l, opts.LedgerMu, h.URL
}

func (f *fixture) assertPending(t *testing.T, key string, pending bool) {
	t.Helper()
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	v := l.Sends[key]
	if v == nil || v.RecordPending != pending || (pending && v.Receipt != nil) {
		t.Fatalf("persisted recovery state: %+v; want pending=%v, no failed receipt while pending", v, pending)
	}
	_, raw, _ := f.call(t, "/status", nil)
	want := float64(0)
	if pending {
		want = 1
	}
	if status := decode[map[string]any](t, raw); status["unrecorded"] != want {
		t.Fatalf("status pending count: %s", raw)
	}
}

func TestInterruptedCommandRetriesTransientGCFailureAfterRestart(t *testing.T) {
	for _, mode := range []string{"503", "408", "429", "transport", "canceled", "receipt"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "interrupted"}
			f.gc.Bind(p.Conversation, p.SessionID)
			if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
				t.Fatal(err)
			}
			f.restart(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var calls atomic.Int32
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/outbound") && calls.Add(1) == 1 {
					switch mode {
					case "transport":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					case "canceled":
						_, _ = io.Copy(io.Discard, r.Body)
						cancel()
						<-r.Context().Done()
					case "receipt":
						var out wire.OutboundResult
						out.Receipt.FailureKind, out.Receipt.RetryAfter = "transient", int64(time.Second)
						respond(w, 200, out)
					default:
						w.Header().Set("Retry-After", "60")
						w.WriteHeader(map[string]int{"503": 503, "408": 408, "429": 429}[mode])
					}
					return
				}
				proxy.ServeHTTP(w, r)
			}))
			defer h.Close()
			f.options.GC = gcclient.New(h.URL, "examplecity")
			f.s.SetClients(f.options.Chat, f.options.GC)
			if err := f.s.RetryPending(ctx); err == nil {
				t.Error("transient recovery failure was not returned")
			} else if mode == "canceled" && !errors.Is(err, context.Canceled) {
				t.Errorf("cancellation lost: %v", err)
			}
			f.assertPending(t, p.IdempotencyKey, true)
			f.restart(t)
			if err := f.s.RetryPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertPending(t, p.IdempotencyKey, false)
			if got := commandOutcome(f.l.Sends[p.IdempotencyKey]); !got.Delivered || !got.Recorded || f.count() != 1 || calls.Load() != 2 {
				t.Fatalf("recovery: %+v; Google sends=%d gc calls=%d", got, f.count(), calls.Load())
			}
			if err := f.s.RetryPending(t.Context()); err != nil || calls.Load() != 2 {
				t.Fatalf("completed intent retried: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestInterruptedDirectCommandRetriesTransientChatFailureAfterRestart(t *testing.T) {
	for _, mode := range []string{"upload", "unbound", "no-session"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "interrupted"}
			var upload *ledger.SendUpload
			if mode == "upload" {
				f.gc.Bind(p.Conversation, p.SessionID)
				file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
				if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
				upload = &ledger.SendUpload{Path: file}
			} else if mode == "no-session" {
				p.SessionID = ""
			}
			if err := f.s.prepare(t.Context(), p, upload, true); err != nil {
				t.Fatal(err)
			}
			f.restart(t)
			var attempts atomic.Int32
			c, err := chat.New(t.Context(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					attempts.Add(1)
					return &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"quota"}}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetClients(c, f.options.GC)
			if err := f.s.RetryPending(t.Context()); err == nil {
				t.Error("transient Chat recovery failure was not returned")
			}
			if attempts.Load() != 1 || f.count() != 0 {
				t.Fatalf("unexpected attempts=%d sends=%d", attempts.Load(), f.count())
			}
			f.assertPending(t, p.IdempotencyKey, true)
			f.restart(t)
			if err := f.s.RetryPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertPending(t, p.IdempotencyKey, false)
			if got := commandOutcome(f.l.Sends[p.IdempotencyKey]); !got.Delivered || got.Recorded != (mode == "upload") || f.count() != 1 {
				t.Fatalf("recovery: %+v; Google sends=%d", got, f.count())
			}
		})
	}
}

func TestInterruptedCommandDefiniteRejectionStopsAutomaticRecovery(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "rejected-interrupted"}
	f.gc.Bind(p.Conversation, "another-session")
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	f.restart(t)
	if err := f.s.RetryPending(t.Context()); !gcclient.IsWrongOwner(err) {
		t.Fatalf("expected definite rejection: %v", err)
	}
	f.assertPending(t, p.IdempotencyKey, false)
	want := commandOutcome(f.l.Sends[p.IdempotencyKey])
	if want.Delivered || want.Recorded || want.FailureKind != "permanent" || want.Reason == "" {
		t.Fatalf("returned failure: %+v", want)
	}
	f.restart(t)
	f.gc.Bind(p.Conversation, p.SessionID)
	for range 2 {
		if err := f.s.RetryPending(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := commandOutcome(f.l.Sends[p.IdempotencyKey]); got != want || f.count() != 0 {
		t.Fatalf("definite rejection retried or lost: %+v sends=%d", got, f.count())
	}
}

func TestInterruptedCommandPreparationTimeoutRemainsPending(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "interrupted"}
	f.gc.Bind(p.Conversation, p.SessionID)
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	release, err := f.s.lock(t.Context(), "send:"+p.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := f.s.RetryPending(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected preparation timeout: %v", err)
	}
	f.assertPending(t, p.IdempotencyKey, true)
	f.restart(t)
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := commandOutcome(f.l.Sends[p.IdempotencyKey]); !got.Delivered || !got.Recorded || f.count() != 1 {
		t.Fatalf("recovery: %+v sends=%d", got, f.count())
	}
}

func TestLateCallbackRearmsTranscriptRecovery(t *testing.T) {
	f := setup(t)
	f.gc.Bind(conversation("spaces/room"), "session")
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "late"}
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := f.s.stopRetry(t.Context(), p, f.s.failure(p, errors.New("gc unavailable")), "gc unavailable"); err != nil {
		t.Fatal(err)
	}
	// The callback outlived gc's failed response. Its success must re-arm
	// recording even though the caller already received a failure.
	if !f.send(t, p).Delivered {
		t.Fatal("late callback failed")
	}
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Sends[p.IdempotencyKey].Recorded || l.Sends[p.IdempotencyKey].RecordPending || f.count() != 1 {
		t.Fatalf("late success not recorded: %+v", l.Sends[p.IdempotencyKey])
	}
}

func TestCanceledCommandFailureIsCallerOwned(t *testing.T) {
	f := setup(t)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer h.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "canceled"}
	result, err := f.s.command(ctx, p, nil)
	if !errors.Is(err, context.DeadlineExceeded) || result.Delivered || result.FailureKind != "transient" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	f.s.SetClients(f.options.Chat, f.options.GC)
	f.gc.Bind(p.Conversation, p.SessionID)
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.count() != 0 || f.l.Sends[p.IdempotencyKey].RecordPending {
		t.Fatal("canceled command was delivered automatically")
	}
}

func TestExpiredDirectCommandFailureSurvivesRestart(t *testing.T) {
	for _, mode := range []string{"upload", "unbound", "no-session"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			client := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return http.DefaultTransport.RoundTrip(r)
			})}
			c, err := chat.New(t.Context(), client, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetClients(c, f.options.GC)
			path, cmd := "/commands/publish", command("expired")
			if mode == "upload" {
				f.gc.Bind(conversation("spaces/room"), "session")
				path = "/commands/upload"
				file := filepath.Join(f.cfg.Endpoints.UploadRoot, "data.txt")
				if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
				cmd["path"] = file
			} else if mode == "no-session" {
				cmd["session_id"] = ""
			}
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			raw, _ := json.Marshal(cmd)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)).WithContext(ctx)
			w := httptest.NewRecorder()
			f.s.ServeHTTP(w, req)
			result := decode[CommandResult](t, w.Body.Bytes())
			if w.Code != 200 || result.Delivered || result.FailureKind != "transient" || result.RetryAfter == 0 {
				t.Fatalf("deadline result: %d %+v", w.Code, result)
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			opts := f.options
			opts.Ledger, opts.LedgerMu = l, new(sync.Mutex)
			restarted, err := New(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			restarted.SetHealth(Health{State: "ready"})
			h := httptest.NewServer(restarted)
			defer h.Close()
			if err := opts.GC.Register(t.Context(), wire.Registration{Provider: "gchat", AccountID: "example", CallbackURL: h.URL}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := restarted.RetryPending(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			v := l.Sends["expired"]
			if f.count() != 0 || v.RecordPending || v.Receipt == nil || v.Receipt.Delivered || v.Receipt.FailureKind != "transient" || v.RecordingReason != result.Reason {
				t.Fatalf("expired failure not retained across restart: %+v sends=%d", v, f.count())
			}
		})
	}
}

func TestCommandSettlesGCResultAfterRequestCancellation(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprint(delivered), func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "settle"}
			if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
				t.Fatal(err)
			}
			receipt := f.s.failure(p, errors.New("unavailable"))
			var entry *map[string]any
			if delivered {
				receipt = f.send(t, p)
				entry = &map[string]any{}
			}
			// Start the cancellation budget after setup and the optional Google send.
			ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
			defer cancel()
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// gc answers before the deadline, while another ledger consumer
				// holds the mutex until the request expires.
				f.mu.Lock()
				go func() { <-ctx.Done(); f.mu.Unlock() }()
				var out wire.OutboundResult
				out.Receipt.Delivered, out.Receipt.MessageID = receipt.Delivered, receipt.MessageID
				out.Receipt.FailureKind, out.Receipt.RetryAfter = receipt.FailureKind, receipt.RetryAfter
				out.TranscriptEntry = entry
				respond(w, 200, out)
			}))
			defer h.Close()
			f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
			result, err := f.s.command(ctx, p, nil)
			if err != nil || result.Delivered != delivered || result.Recorded != delivered {
				t.Fatalf("settled result: %+v %v", result, err)
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			v := l.Sends[p.IdempotencyKey]
			if v.RecordPending || v.Receipt == nil || v.Receipt.Delivered != delivered || v.Recorded != delivered {
				t.Fatalf("definite gc result not durable: %+v", v)
			}
		})
	}
}

func TestNoBindingAtDeadlineDoesNotLeaveAnAutomaticSend(t *testing.T) {
	f := setup(t)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		go func() { <-ctx.Done(); f.mu.Unlock() }()
		respond(w, 422, map[string]string{"code": "validation-failed", "detail": "no active binding for conversation spaces/room"})
	}))
	defer h.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "binding-deadline"}
	result, err := f.s.command(ctx, p, nil)
	if !errors.Is(err, context.DeadlineExceeded) || result.Delivered || result.FailureKind != "transient" || result.RetryAfter == 0 {
		t.Fatalf("deadline before direct send: %+v %v", result, err)
	}
	l, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	*f.l = *l
	f.mu.Unlock()
	f.s.SetClients(f.options.Chat, f.options.GC)
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := f.l.Sends[p.IdempotencyKey]; v.RecordPending || v.Receipt == nil || v.Receipt.FailureKind != "transient" || f.count() != 0 {
		t.Fatalf("deadline left an automatic send: %+v sends=%d", v, f.count())
	}
}

type cancelAfterIntent struct {
	context.Context
	cancel context.CancelFunc
	ledger *ledger.Ledger
	key    string
}

func (c cancelAfterIntent) Err() error {
	// Inject a caller disconnect immediately after the intent becomes durable,
	// before any network operation. The command runs synchronously in this test.
	if c.ledger.Sends[c.key] != nil {
		c.cancel()
	}
	return c.Context.Err()
}

func TestCommandPreparationFailureSettlesExistingIntent(t *testing.T) {
	for _, mode := range []string{"after-intent", "not-ready", "send-lock"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "preparation"}
			f.gc.Bind(p.Conversation, p.SessionID)
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			if mode == "after-intent" {
				ctx = cancelAfterIntent{Context: ctx, cancel: cancel, ledger: f.l, key: p.IdempotencyKey}
			} else {
				if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "not-ready" {
				f.s.SetHealth(Health{State: "needs-sign-in"})
			}
			release := func() {}
			if mode == "send-lock" {
				var err error
				release, err = f.s.lock(t.Context(), "send:"+p.IdempotencyKey)
				if err != nil {
					t.Fatal(err)
				}
			}
			result, err := f.s.command(ctx, p, nil)
			release()
			if err == nil || result.Delivered || result.FailureKind == "" {
				t.Fatalf("preparation failure: %+v %v", result, err)
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			*f.l = *l
			f.mu.Unlock()
			f.s.SetHealth(Health{State: "ready"})
			if err := f.s.RetryPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			if v := f.l.Sends[p.IdempotencyKey]; v.RecordPending || v.Receipt == nil || v.Receipt.FailureKind != result.FailureKind || f.count() != 0 {
				t.Fatalf("preparation failure retried: %+v sends=%d", v, f.count())
			}
		})
	}
}

func TestUnrecordableReceiptClearsPendingAtCommitAndAfterRestart(t *testing.T) {
	for _, mode := range []string{"receipt-commit", "old-crash-window"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{Conversation: conversation("spaces/room"), Text: "hello", IdempotencyKey: "unrecordable"}
			if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
				t.Fatal(err)
			}
			if err := f.s.update(t.Context(), func(l *ledger.Ledger) error {
				l.Sends[p.IdempotencyKey].NotRecordable = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !f.send(t, p).Delivered {
				t.Fatal("send failed")
			}
			if mode == "old-crash-window" {
				if err := f.s.update(t.Context(), func(l *ledger.Ledger) error {
					l.Sends[p.IdempotencyKey].RecordPending = true
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.s.RetryPending(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			if l.Sends[p.IdempotencyKey].RecordPending || f.count() != 1 || len(f.gc.Published()) != 0 {
				t.Fatalf("unrecordable send still pending: %+v", l.Sends[p.IdempotencyKey])
			}
			_, raw, _ := f.call(t, "/status", nil)
			if decode[map[string]any](t, raw)["unrecorded"] != float64(0) {
				t.Fatalf("status: %s", raw)
			}
		})
	}
}

func TestHistoryOmitsDeliveryMarkers(t *testing.T) {
	f := setup(t)
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC})
	if err != nil {
		t.Fatal(err)
	}
	m := &api.Message{Name: "spaces/room/messages/caught", CreateTime: "2026-10-22T10:00:00Z", Text: "edited: literal content", Space: &api.Space{Name: "spaces/room", SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", Type: "HUMAN"}}
	for _, kind := range []string{"message.created", "message.updated"} {
		if kind == "message.updated" {
			m.LastUpdateTime, m.Text = "2026-10-22T11:00:00Z", "current content"
		}
		f.g.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
		if _, err := p.HandleChange(t.Context(), puller.Change{Kind: kind, Space: "spaces/room", Message: m}); err != nil {
			t.Fatal(err)
		}
		_, raw, _ := f.call(t, "/read?space=spaces/room", nil)
		got := decode[[]wire.InboundMessage](t, raw)
		if len(got) != 1 || got[0].Text != m.Text {
			t.Fatalf("history includes delivery markers for %s: %s", kind, raw)
		}
	}
}

func TestHistoryPreservesLiteralCatchUpHeader(t *testing.T) {
	for _, catchUp := range []bool{false, true} {
		t.Run(fmt.Sprint(catchUp), func(t *testing.T) {
			f := setup(t)
			p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC})
			if err != nil {
				t.Fatal(err)
			}
			m := &api.Message{Name: "spaces/room/messages/literal", CreateTime: "2026-10-22T10:00:00Z", Text: "[catch-up]\nliteral user content", Space: &api.Space{Name: "spaces/room", SpaceType: "SPACE"}, Sender: &api.User{Name: "users/456", Type: "HUMAN"}}
			f.g.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
			c := puller.Change{Kind: "message.created", Space: "spaces/room", Message: m}
			if catchUp {
				_, err = p.HandleChange(t.Context(), c)
			} else {
				_, err = p.Handle(t.Context(), &puller.Event{PubSubID: "live", Changes: []puller.Change{c}})
			}
			if err != nil {
				t.Fatal(err)
			}
			// Exercise persisted provenance rather than only the processor's memory.
			l, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			*f.l = *l
			f.mu.Unlock()
			_, raw, _ := f.call(t, "/read?space=spaces/room", nil)
			got := decode[[]wire.InboundMessage](t, raw)
			if len(got) != 1 || got[0].Text != m.Text {
				t.Fatalf("literal header altered: %s", raw)
			}
		})
	}
}

func TestCommandGCLogsSanitizedFailureAndRecovery(t *testing.T) {
	for _, status := range []int{422, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "private-gc-error-body")
			}))
			defer h.Close()
			f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
			cmd := command("gc-log")
			cmd["text"] = "private-command-text"
			_, _, _ = f.call(t, "/commands/publish", cmd)
			f.s.SetClients(f.options.Chat, f.options.GC)
			f.gc.Bind(conversation("spaces/room"), "session")
			for range 2 {
				_, _, _ = f.call(t, "/commands/publish", cmd)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 2 {
				t.Fatalf("want one warning and one recovery: %s", logs.String())
			}
			bad, good := decode[map[string]any](t, []byte(lines[0])), decode[map[string]any](t, []byte(lines[1]))
			wantKind := "transient"
			if status == 422 {
				wantKind = "permanent"
			}
			if bad["level"] != "WARN" || bad["key"] != "gc-log" || bad["space"] != "spaces/room" || bad["cause_kind"] != wantKind || bad["http_status"] != float64(status) || good["level"] != "INFO" {
				t.Fatalf("unexpected records: %s", logs.String())
			}
			if strings.Contains(logs.String(), "private-") {
				t.Fatal("private command or error body logged")
			}
		})
	}
}

func TestFailureLogRecoveryTrackingIsBounded(t *testing.T) {
	f := setup(t)
	f.s.opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	for i := range 1100 {
		p := wire.PublishRequest{Conversation: conversation("spaces/room"), IdempotencyKey: fmt.Sprint(i)}
		err := errors.New("failure")
		f.s.logPublish(p, f.s.failure(p, err), err)
	}
	if len(f.s.publishFailures) > 1024 {
		t.Fatalf("unbounded recovery tracking: %d entries", len(f.s.publishFailures))
	}
	// A recent failure must still produce its recovery line.
	var logs bytes.Buffer
	f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	f.s.logPublish(wire.PublishRequest{IdempotencyKey: "1099"}, wire.PublishReceipt{Delivered: true}, nil)
	if !strings.Contains(logs.String(), `"level":"INFO"`) {
		t.Fatal("recent recovery was evicted")
	}
}

func TestMissingInventorySpaceReturnsNotFound(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{Conversation: conversation("spaces/gone"), Text: "hello", IdempotencyKey: "gone"}
	got := f.send(t, p)
	if got.Delivered || got.FailureKind != "not_found" || got.RetryAfter != 0 {
		t.Fatalf("missing space receipt: %+v", got)
	}
	cmd := command("gone-command")
	cmd["conversation_id"] = "spaces/gone"
	_, raw, _ := f.call(t, "/commands/publish", cmd)
	result := decode[CommandResult](t, raw)
	if result.Delivered || result.FailureKind != "not_found" || result.RetryAfter != 0 {
		t.Fatalf("missing space command: %+v", result)
	}
}

func TestStateReconciliationCanRetryWithoutReplacingStateAgain(t *testing.T) {
	f := setup(t)
	calls := 0
	f.s.opts.StateChanged = func(ctx context.Context) error {
		calls++
		if f.s.stateGate.TryRLock() {
			f.s.stateGate.RUnlock()
			t.Error("StateChanged must run under the state gate")
		}
		if err := f.s.update(ctx, func(l *ledger.Ledger) error {
			l.Counters["reconcile"]++
			return nil
		}); err != nil {
			return err
		}
		if calls == 1 {
			return errors.New("reconciliation temporarily unavailable")
		}
		f.s.SetHealth(Health{State: "ready"})
		return nil
	}
	f.s.SetHealth(Health{State: "recovery-required"})
	code, raw, _ := f.call(t, "/state", map[string]string{"action": "reset"})
	if code != 503 || f.s.Health().State != "recovery-required" {
		t.Fatalf("failed reconcile cannot be retried: %d %s, health=%+v", code, raw, f.s.Health())
	}
	code, _, _ = f.call(t, "/state", map[string]string{"action": "restore", "file": "different"})
	if code != 409 || calls != 1 {
		t.Fatal("pending recovery accepted a different state replacement")
	}
	code, raw, _ = f.call(t, "/state", map[string]string{"action": "reset"})
	if code != 200 || calls != 2 || f.l.Counters["reconcile"] != 2 || f.s.Health().State != "ready" {
		t.Fatalf("reconcile retry: %d %s, calls=%d counter=%d", code, raw, calls, f.l.Counters["reconcile"])
	}
}

func TestConcurrentRecoveryChecksHealthUnderExclusiveGate(t *testing.T) {
	f := setup(t)
	replacements := 0
	f.s.opts.StateChanged = func(ctx context.Context) error {
		replacements++
		if err := f.s.update(ctx, func(l *ledger.Ledger) error {
			l.Counters["recovered"] = 1
			return nil
		}); err != nil {
			return err
		}
		f.s.SetHealth(Health{State: "ready"})
		return nil
	}
	f.s.SetHealth(Health{State: "recovery-required"})
	// Freeze the health read so both requests overlap at the authorization
	// boundary. One must hold the recovery gate and the other must be refused
	// before either can consume the old recovery-required health value.
	f.s.mu.Lock()
	results := make(chan int, 2)
	for range 2 {
		go func() {
			w := httptest.NewRecorder()
			f.s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/state", strings.NewReader(`{"action":"reset"}`)))
			results <- w.Code
		}()
	}
	var codes []int
	select {
	case code := <-results:
		codes = append(codes, code)
		if code != 409 {
			t.Errorf("overlapping recovery = %d, want 409", code)
		}
	case <-time.After(time.Second):
		t.Error("concurrent recovery read health before taking the exclusive gate")
	}
	f.s.mu.Unlock()
	for len(codes) < 2 {
		select {
		case code := <-results:
			codes = append(codes, code)
		case <-time.After(5 * time.Second):
			t.Fatal("recovery did not finish")
		}
	}
	if codes[0]+codes[1] != 200+409 || replacements != 1 || f.l.Counters["recovered"] != 1 {
		t.Fatalf("recovery replaced state again: statuses=%v replacements=%d state=%+v", codes, replacements, f.l.Counters)
	}
	if code, raw, _ := f.call(t, "/state", map[string]string{"action": "reset"}); code != 409 {
		t.Fatalf("recovered state replaced: %d %s", code, raw)
	}
}

func TestNoAdapterAfterGCRestartRecovers(t *testing.T) {
	testGCRestartRecovers(t, false)
}

func TestCityNotRunningAfterGCRestartRecovers(t *testing.T) {
	testGCRestartRecovers(t, true)
}

func testGCRestartRecovers(t *testing.T, cityNotRunning bool) {
	t.Helper()
	status, code, detail := 422, "validation-failed", "no adapter for gchat/example"
	if cityNotRunning {
		status, code, detail = 404, "city-not-found", "not_found: city not found or not running: examplecity"
	}
	for _, mode := range []string{"interrupted", "caller", "delivered"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			var logs bytes.Buffer
			f.s.opts.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "gc-restart"}
			f.gc.Bind(p.Conversation, p.SessionID)
			if mode != "caller" {
				if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
					t.Fatal(err)
				}
				if mode == "delivered" && !f.send(t, p).Delivered {
					t.Fatal("could not seed delivered send awaiting recording")
				}
			}
			f.gc.ForgetRegistrations()
			f.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = cityNotRunning })

			// Hold re-registration so all assertions observe gc's restart window.
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			entered, release, registered := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}, 1)
			var registrations, outbound atomic.Int32
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/adapters") {
					registrations.Add(1)
					select {
					case entered <- struct{}{}:
					default:
					}
					<-release
					proxy.ServeHTTP(w, r)
					select {
					case registered <- struct{}{}:
					default:
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/outbound") {
					outbound.Add(1)
				}
				proxy.ServeHTTP(w, r)
			}))
			defer h.Close()
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			f.s.mu.Lock()
			f.s.cfg.Secrets.GCAPIBase = f.url
			f.s.mu.Unlock()
			f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var restartErr error
			if mode == "caller" {
				_, raw, _ := f.call(t, "/commands/publish", command(p.IdempotencyKey))
				got := decode[CommandResult](t, raw)
				if got.Delivered || got.FailureKind != "transient" || got.RetryAfter != int64(time.Second) {
					t.Errorf("caller restart failure: %+v", got)
				}
			} else {
				restartErr = f.s.RetryPending(ctx)
				var responseErr *gcclient.HTTPError
				if !errors.As(restartErr, &responseErr) || responseErr.StatusCode != status || responseErr.Problem.Code != code || responseErr.Problem.Detail != detail {
					t.Fatalf("restart error not returned: %v", restartErr)
				}
			}
			cancel() // Registration must outlive the failed attempt's context.
			saved, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			v := saved.Sends[p.IdempotencyKey]
			if v.RecordPending != (mode != "caller") || v.NotRecordable {
				t.Errorf("restart recovery state: %+v", v)
			}
			if mode == "interrupted" && v.Receipt != nil {
				t.Errorf("interrupted send settled: %+v", v.Receipt)
			}
			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 {
				t.Errorf("want one WARN: %s", logs.String())
			} else if line := decode[map[string]any](t, []byte(lines[0])); line["level"] != "WARN" || line["operation"] != "gc_outbound" || line["cause_kind"] != "transient" || line["http_status"] != float64(status) {
				t.Errorf("restart warning: %s", logs.String())
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("restart error did not trigger registration")
			}
			if restartErr == nil {
				restartErr = &gcclient.HTTPError{StatusCode: status, Problem: &gcclient.Problem{Code: code, Detail: detail}}
			}
			for range 10 {
				f.s.ObserveGCError(restartErr)
			}
			f.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = false })
			unblock()
			select {
			case <-registered:
			case <-time.After(time.Second):
				t.Fatal("registration did not survive attempt cancellation")
			}
			if mode == "caller" {
				if err := f.s.RetryPending(t.Context()); err != nil || outbound.Load() != 1 {
					t.Fatalf("caller failure retried automatically: %v calls=%d", err, outbound.Load())
				}
				_, raw, _ := f.call(t, "/commands/publish", command(p.IdempotencyKey))
				if got := decode[CommandResult](t, raw); !got.Delivered || !got.Recorded {
					t.Fatalf("caller recovery: %+v", got)
				}
			} else if err := f.s.RetryPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := commandOutcome(f.l.Sends[p.IdempotencyKey])
			if !got.Delivered || !got.Recorded || f.count() != 1 || outbound.Load() != 2 || registrations.Load() != 1 {
				t.Fatalf("recovery: %+v Google=%d gc=%d registrations=%d", got, f.count(), outbound.Load(), registrations.Load())
			}
			if err := f.s.RetryPending(t.Context()); err != nil || outbound.Load() != 2 {
				t.Fatalf("completed intent retried: %v calls=%d", err, outbound.Load())
			}
		})
	}
}

func TestUnboundTransientRetryRechecksBinding(t *testing.T) {
	for _, mode := range []string{"sweep", "explicit", "still-unbound"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "unbound-transient"}
			const reason = "no active binding for conversation gchat/spaces/room"
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var calls atomic.Int32
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/outbound") && calls.Add(1) == 2 {
					// Inspect the durable re-arm before gc supplies the new outcome.
					saved, err := ledger.Load(f.options.LedgerPath, true)
					if err != nil {
						t.Error(err)
					} else if v := saved.Sends[p.IdempotencyKey]; v.NotRecordable || !v.RecordPending || v.RecordingReason != reason {
						t.Errorf("re-arm lost binding diagnostic or retained direct shortcut: %+v", v)
					}
				}
				proxy.ServeHTTP(w, r)
			}))
			defer h.Close()
			f.options.GC = gcclient.New(h.URL, "examplecity")
			var attempts atomic.Int32
			c, err := chat.New(t.Context(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == "POST" {
					attempts.Add(1)
					return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":503,"message":"temporarily unavailable"}}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetClients(c, f.options.GC)
			if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
				t.Fatal(err)
			}
			if err := f.s.RetryPending(t.Context()); err == nil {
				t.Fatal("transient Chat error not returned")
			}
			f.assertPending(t, p.IdempotencyKey, true)
			v := f.l.Sends[p.IdempotencyKey]
			if !v.NotRecordable || v.RecordingReason != reason || calls.Load() != 1 || attempts.Load() != 1 || f.count() != 0 {
				t.Fatalf("first unbound attempt: %+v; gc=%d attempts=%d Google=%d", v, calls.Load(), attempts.Load(), f.count())
			}
			f.restart(t) // Reload the no-binding decision from disk, with healthy Chat.
			if mode != "still-unbound" {
				f.gc.Bind(p.Conversation, p.SessionID)
			}
			if mode == "explicit" {
				_, raw, _ := f.call(t, "/commands/publish", command(p.IdempotencyKey))
				if got := decode[CommandResult](t, raw); !got.Delivered || !got.Recorded {
					t.Errorf("explicit same-key retry: %+v", got)
				}
			} else if err := f.s.RetryPending(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.assertPending(t, p.IdempotencyKey, false)
			got := commandOutcome(f.l.Sends[p.IdempotencyKey])
			if !got.Delivered || got.Recorded != (mode != "still-unbound") || calls.Load() != 2 || f.count() != 1 {
				t.Fatalf("binding recheck: %+v; gc=%d Google=%d", got, calls.Load(), f.count())
			}
			if mode == "still-unbound" && got.Reason != reason {
				t.Fatalf("no-binding diagnostic changed: %q", got.Reason)
			}
			if err := f.s.RetryPending(t.Context()); err != nil || calls.Load() != 2 || f.count() != 1 {
				t.Fatalf("completed intent retried: %v; gc=%d Google=%d", err, calls.Load(), f.count())
			}
		})
	}
}

func TestFullGCRestartSequenceDeliversOnce(t *testing.T) {
	f := setup(t)
	p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "recover me", IdempotencyKey: "full-restart"}
	f.gc.Bind(p.Conversation, p.SessionID)
	if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
		t.Fatal(err)
	}
	f.gc.ForgetRegistrations()
	f.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = true })

	// Keep the registrar from racing past any of the restart windows.
	f.s.regGate <- struct{}{}
	var gateOnce sync.Once
	releaseGate := func() { gateOnce.Do(func() { <-f.s.regGate }) }
	defer releaseGate()
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()
	f.s.SetClients(f.options.Chat, gcclient.New(closed.URL, "examplecity"))
	if err := f.s.RetryPending(t.Context()); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("refused phase: %v", err)
	}
	f.assertPending(t, p.IdempotencyKey, true)

	target, _ := url.Parse(f.gc.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	failed, registered, release := make(chan int, 1), make(chan struct{}, 1), make(chan struct{})
	var registrations, outbound atomic.Int32
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/adapters") {
			if registrations.Add(1) == 1 {
				recorder := httptest.NewRecorder()
				proxy.ServeHTTP(recorder, r)
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				failed <- recorder.Code
				return
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			proxy.ServeHTTP(w, r)
			select {
			case registered <- struct{}{}:
			default:
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/outbound") {
			outbound.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer h.Close()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f.s.mu.Lock()
	f.s.cfg.Secrets.GCAPIBase = f.url
	f.s.mu.Unlock()
	f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
	releaseGate()
	select {
	case status := <-failed:
		if status != 404 {
			t.Fatalf("registration during city startup: %d", status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("registrar did not try the starting city")
	}
	for _, status := range []int{404, 422} {
		if status == 422 {
			f.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = false })
		}
		err := f.s.RetryPending(t.Context())
		var responseErr *gcclient.HTTPError
		if !errors.As(err, &responseErr) || responseErr.StatusCode != status {
			t.Fatalf("restart phase %d: %v", status, err)
		}
		f.assertPending(t, p.IdempotencyKey, true)
		if f.count() != 0 {
			t.Fatal("delivered before city and registration recovered")
		}
	}
	unblock()
	select {
	case <-registered:
	case <-time.After(5 * time.Second):
		t.Fatal("registration did not recover")
	}
	if err := f.s.RetryPending(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := commandOutcome(f.l.Sends[p.IdempotencyKey]); !got.Delivered || !got.Recorded || f.count() != 1 || outbound.Load() != 3 || registrations.Load() != 2 {
		t.Fatalf("recovery: %+v Google=%d gc=%d registrations=%d", got, f.count(), outbound.Load(), registrations.Load())
	}
	f.assertPending(t, p.IdempotencyKey, false)
	if err := f.s.RetryPending(t.Context()); err != nil || outbound.Load() != 3 || f.count() != 1 {
		t.Fatalf("completed intent retried: %v gc=%d Google=%d", err, outbound.Load(), f.count())
	}
}

func TestCallerDirectRetryReportsBindingReason(t *testing.T) {
	for _, session := range []string{"", "session"} {
		t.Run("session="+session, func(t *testing.T) {
			f := setup(t)
			c, err := chat.New(t.Context(), &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost {
					return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":503,"message":"temporarily unavailable"}}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})}, f.cfg.Endpoints)
			if err != nil {
				t.Fatal(err)
			}
			f.s.SetClients(c, f.options.GC)
			cmd := command("caller-direct-retry")
			cmd["session_id"] = session
			_, raw, _ := f.call(t, "/commands/publish", cmd)
			first := decode[CommandResult](t, raw)
			if first.Delivered || first.FailureKind != "transient" || !strings.Contains(first.Reason, "temporarily unavailable") {
				t.Fatalf("first attempt: %+v", first)
			}
			f.restart(t) // Reload the caller-owned failure, then retry with healthy Chat.
			_, raw, _ = f.call(t, "/commands/publish", cmd)
			got := decode[CommandResult](t, raw)
			want := "no active binding: session_id is empty"
			if session != "" {
				want = "no active binding for conversation gchat/spaces/room"
			}
			if !got.Delivered || got.Recorded || got.Reason != want || f.count() != 1 {
				t.Fatalf("retry: %+v Google=%d; want reason %q", got, f.count(), want)
			}
			saved, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			v := saved.Sends[first.Key]
			if v.RecordingReason != want || !v.NotRecordable || v.RecordPending {
				t.Fatalf("saved binding reason: %+v", v)
			}
		})
	}
}

func TestPreviousAccountIsDefiniteAndDoesNotRegister(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		t.Run(fmt.Sprintf("delivered=%v", delivered), func(t *testing.T) {
			f := setup(t)
			p := wire.PublishRequest{SessionID: "session", Conversation: conversation("spaces/room"), Text: "previous account", IdempotencyKey: "previous-account"}
			f.gc.Bind(p.Conversation, p.SessionID)
			if err := f.s.prepare(t.Context(), p, nil, true); err != nil {
				t.Fatal(err)
			}
			if delivered && !f.send(t, p).Delivered {
				t.Fatal("could not seed delivered send awaiting recording")
			}
			// A configuration rename leaves the saved intent under its old account.
			f.s.mu.Lock()
			f.s.cfg.Settings.Identities = []config.Identity{{Name: "renamed", Agent: "mayor"}}
			f.s.mu.Unlock()
			f.gc.ForgetRegistrations()
			if err := f.options.GC.Register(t.Context(), wire.Registration{Provider: "gchat", AccountID: "renamed", CallbackURL: f.url}); err != nil {
				t.Fatal(err)
			}
			target, _ := url.Parse(f.gc.URL())
			proxy := httputil.NewSingleHostReverseProxy(target)
			var registrations, outbound atomic.Int32
			release := make(chan struct{})
			h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/adapters") {
					registrations.Add(1)
					<-release
				}
				if strings.HasSuffix(r.URL.Path, "/outbound") {
					outbound.Add(1)
				}
				proxy.ServeHTTP(w, r)
			}))
			defer h.Close()
			defer close(release)
			f.s.SetClients(f.options.Chat, gcclient.New(h.URL, "examplecity"))
			err := f.s.RetryPending(t.Context())
			var responseErr *gcclient.HTTPError
			if !errors.As(err, &responseErr) || responseErr.StatusCode != 422 || responseErr.Problem.Detail != "no adapter for gchat/example" {
				t.Fatalf("previous account rejection: %v", err)
			}
			saved, err := ledger.Load(f.options.LedgerPath, true)
			if err != nil {
				t.Fatal(err)
			}
			v := saved.Sends[p.IdempotencyKey]
			got := commandOutcome(v)
			if v.RecordPending || got.FailureKind != "permanent" || got.Delivered != delivered || (delivered && !v.NotRecordable) {
				t.Errorf("previous account retained for retry: %+v outcome=%+v", v, got)
			}
			if err := f.s.RetryPending(t.Context()); err != nil || outbound.Load() != 1 {
				t.Errorf("definite rejection retried: %v gc=%d", err, outbound.Load())
			}
			f.s.mu.RLock()
			registering := f.s.registering
			f.s.mu.RUnlock()
			if registering || registrations.Load() != 0 {
				t.Errorf("previous account triggered registration: registering=%v calls=%d", registering, registrations.Load())
			}
			wantMessages := 0
			if delivered {
				wantMessages = 1
			}
			if f.count() != wantMessages {
				t.Fatalf("previous account sent to Google: %d", f.count())
			}
		})
	}
}

func TestRegistrationBuilder(t *testing.T) {
	f := setup(t)
	reg, err := Registration(f.cfg, f.url)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Provider != "gchat" || reg.AccountID != "example" || reg.CallbackURL != f.url || !reg.Capabilities.SupportsAttachments || reg.Capabilities.MaxMessageLength != 32000 || !strings.Contains(reg.ReplyInstructions, "reply-current") {
		t.Fatalf("registration contract: %+v", reg)
	}
	for _, base := range []string{"/relative", "file:///tmp/example", ""} {
		if _, err := Registration(f.cfg, base); err == nil {
			t.Fatalf("accepted callback %q", base)
		}
	}
}
