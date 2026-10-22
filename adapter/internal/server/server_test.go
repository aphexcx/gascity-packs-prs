package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
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
	for _, mode := range []string{"bound", "unbound", "upload"} {
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
			if first.Delivered {
				t.Fatalf("expected failure: %+v", first)
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
			if f.count() != 0 || v.RecordPending || v.Receipt == nil || v.Receipt.Delivered || v.Receipt.FailureKind != "rate_limited" || v.Receipt.RetryAfter != int64(time.Minute) {
				t.Fatalf("failure not retained: %+v sends=%d", v, f.count())
			}
			_, raw, _ = f.call(t, path, cmd)
			if got := decode[CommandResult](t, raw); !got.Delivered || f.count() != 1 {
				t.Fatalf("caller retry: %+v", got)
			}
		})
	}
}
