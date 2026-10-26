// Package e2e exercises the service through its real Unix socket, streaming
// Pub/Sub client and HTTP clients. Only the remote providers are replaced.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"gchat-adapter/internal/app"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/token"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var socketSequence atomic.Int64

type fixture struct {
	t              *testing.T
	cfg            *config.Config
	google         *fakegoogle.Server
	gc             *fakegc.Server
	ps             *pstest.Server
	pub            *pubsub.Publisher
	client         *http.Client
	gateway        string
	cancel         context.CancelFunc
	done           chan error
	seq            int
	commands       int
	inboundEmitted bool
	mu             sync.Mutex
	requests       []string
	emitted        []string
	hook           func(http.ResponseWriter, *http.Request) bool
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(dir, "gc")
	write(t, stub, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$GC_STUB_LOG\"\nexit 0\n")
	if err := os.Chmod(stub, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", stub)
	t.Setenv("GC_STUB_LOG", filepath.Join(dir, "gc-args"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := &fixture{t: t, gc: fakegc.New(t), ps: pstest.NewServer()}
	t.Cleanup(func() { _ = f.ps.Close() })
	pc, err := pubsub.NewClient(t.Context(), "example", option.WithEndpoint(f.ps.Addr), option.WithoutAuthentication(), option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	topic, sub := "projects/example/topics/chat", "projects/example/subscriptions/chat"
	if _, err = pc.TopicAdminClient.CreateTopic(t.Context(), &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	if _, err = pc.SubscriptionAdminClient.CreateSubscription(t.Context(), &pubsubpb.Subscription{Name: sub, Topic: topic, AckDeadlineSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	f.pub = pc.Publisher(topic)
	t.Cleanup(f.pub.Stop)
	f.google = fakegoogle.New(t, f.pub)
	socket := fmt.Sprintf("/tmp/gchat-e2e-%d-%d.sock", os.Getpid(), socketSequence.Add(1))
	t.Cleanup(func() { _ = os.Remove(socket) })
	f.cfg = &config.Config{SecretsDir: dir, Secrets: config.Secrets{ProjectID: "example", Topic: topic, Subscription: sub, CityName: "examplecity", ServiceSocket: socket, ServiceURLPrefix: "/v0/city/examplecity/svc/gchat", OAuthClientFile: filepath.Join(dir, "client.json"), TokenFile: filepath.Join(dir, "token.json")}, Endpoints: config.Endpoints{PubSubEndpoint: f.ps.Addr, MediaDir: filepath.Join(dir, "media"), UploadRoot: filepath.Join(dir, "uploads"), StateFile: filepath.Join(dir, "state.json"), MediaMaxBytes: 1024}}
	f.client = &http.Client{Timeout: 40 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
	t.Cleanup(f.client.CloseIdleConnections)
	googleURL, _ := url.Parse(f.google.URL())
	googleProxy := httputil.NewSingleHostReverseProxy(googleURL)
	googleProxy.ErrorLog = log.New(io.Discard, "", 0)
	gs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !f.intercept("google", w, r) {
			googleProxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(gs.Close)
	f.cfg.Endpoints.ChatAPIBase = gs.URL
	f.cfg.Endpoints.EventsAPIBase = gs.URL
	f.cfg.Endpoints.OAuthTokenURL = gs.URL + "/token"
	gcURL, _ := url.Parse(f.gc.URL())
	gcProxy := httputil.NewSingleHostReverseProxy(gcURL)
	adapterProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "adapter"})
	adapterProxy.Transport = f.client.Transport
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, f.cfg.Secrets.ServiceURLPrefix) {
			r.URL.Path = strings.TrimPrefix(r.URL.Path, f.cfg.Secrets.ServiceURLPrefix)
			adapterProxy.ServeHTTP(w, r)
			return
		}
		if !f.intercept("gc", w, r) {
			gcProxy.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(gateway.Close)
	f.gateway = gateway.URL
	f.cfg.Secrets.GCAPIBase = gateway.URL
	write(t, f.cfg.Secrets.OAuthClientFile, `{"installed":{"client_id":"example","client_secret":"example","auth_uri":"http://127.0.0.1/authorize","token_uri":"`+gs.URL+`/token","redirect_uris":["http://localhost"]}}`)
	f.replaceToken(false)
	f.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces["spaces/dm"] = &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}
		g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	})
	t.Cleanup(func() {
		f.stop()
		if !t.Failed() {
			f.assertStoresConsistent()
		}
	})
	return f
}
func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) intercept(label string, w http.ResponseWriter, r *http.Request) bool {
	f.mu.Lock()
	f.requests = append(f.requests, label+" "+r.Method+" "+r.URL.Path)
	hook := f.hook
	f.mu.Unlock()
	return hook != nil && hook(w, r)
}
func (f *fixture) setHook(h func(http.ResponseWriter, *http.Request) bool) {
	f.mu.Lock()
	f.hook = h
	f.mu.Unlock()
}
func (f *fixture) countRequest(part string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if strings.Contains(r, part) {
			n++
		}
	}
	return n
}
func (f *fixture) replaceToken(expired bool) {
	expiry := "2099-01-01T00:00:00Z"
	if expired {
		expiry = "2000-01-01T00:00:00Z"
	}
	write(f.t, f.cfg.Secrets.TokenFile+".new", `{"access_token":"example","refresh_token":"example-refresh","token_type":"Bearer","expiry":"`+expiry+`","identity_user_id":"`+fakegoogle.UserID+`"}`)
	if err := os.Rename(f.cfg.Secrets.TokenFile+".new", f.cfg.Secrets.TokenFile); err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) start() {
	f.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	f.done = make(chan error, 1)
	go func() {
		f.done <- app.RunWithOptions(ctx, f.cfg, app.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RetryDelay: time.Millisecond})
	}()
}
func (f *fixture) ready() { f.start(); f.health("ready", 10*time.Second) }
func (f *fixture) stop() {
	if f.cancel == nil {
		return
	}
	f.cancel()
	select {
	case err := <-f.done:
		if err != nil {
			f.t.Errorf("app shutdown: %v", err)
		}
	case <-time.After(35 * time.Second):
		f.t.Error("app shutdown timed out")
	}
	f.cancel = nil
	f.client.CloseIdleConnections()
}
func eventually(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true within", d)
}
func (f *fixture) health(want string, d time.Duration) server.Health {
	f.t.Helper()
	var h server.Health
	eventually(f.t, d, func() bool {
		r, e := f.client.Get("http://adapter/healthz")
		if e != nil {
			return false
		}
		defer r.Body.Close()
		_ = json.NewDecoder(r.Body).Decode(&h)
		if r.StatusCode != 200 {
			f.t.Fatalf("health status %d", r.StatusCode)
		}
		if h.State == want {
			header := "degraded"
			if want == "ready" {
				header = "ok"
			}
			if r.Header.Get("X-GC-Health") != header {
				f.t.Fatalf("health header %q", r.Header.Get("X-GC-Health"))
			}
			return true
		}
		return false
	})
	return h
}
func (f *fixture) request(path string, body any, out any) {
	f.t.Helper()
	var r *http.Response
	var err error
	if body == nil {
		r, err = f.client.Get("http://adapter" + path)
	} else {
		data, _ := json.Marshal(body)
		r, err = f.client.Post("http://adapter"+path, "application/json", bytes.NewReader(data))
	}
	if err != nil {
		f.t.Fatal(err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 {
		f.t.Fatalf("%s: %d %s", path, r.StatusCode, b)
	}
	if out != nil {
		if err = json.Unmarshal(b, out); err != nil {
			f.t.Fatalf("%s decode: %v: %s", path, err, b)
		}
	}
}
func (f *fixture) state() *ledger.Ledger {
	f.t.Helper()
	l, e := ledger.Load(f.cfg.Endpoints.StateFile, true)
	if e != nil {
		f.t.Fatal(e)
	}
	return l
}
func (f *fixture) status() map[string]json.RawMessage {
	var s map[string]json.RawMessage
	f.request("/status", nil, &s)
	return s
}
func (f *fixture) committed(id string) {
	f.t.Helper()
	eventually(f.t, 10*time.Second, func() bool { c := f.state().Changes[id]; return c != nil && c.Status == "committed" })
}
func (f *fixture) emit(kind, id string, body any) string {
	f.t.Helper()
	f.inboundEmitted = true
	data, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	pid, e := f.pub.Publish(ctx, &pubsub.Message{Data: data, Attributes: map[string]string{"ce-type": "google.workspace.chat." + kind, "ce-id": id}}).Get(ctx)
	if e != nil {
		f.t.Fatal(e)
	}
	f.emitted = append(f.emitted, pid)
	return pid
}
func message(space, text, sender, id string, at time.Time) *api.Message {
	return &api.Message{Name: space + "/messages/" + id, Text: text, Sender: &api.User{Name: sender, Type: "HUMAN"}, CreateTime: at.UTC().Format(time.RFC3339Nano), Thread: &api.Thread{Name: space + "/threads/" + id}}
}
func (f *fixture) emitMessage(space, text, sender string) *api.Message {
	f.seq++
	m := message(space, text, sender, fmt.Sprint(f.seq), time.Now())
	f.emit("message.v1.created", m.Name, map[string]any{"message": m})
	return m
}
func (f *fixture) expectTurn(t *testing.T, matchText string) wire.InboundMessage {
	t.Helper()
	var got wire.InboundMessage
	eventually(t, 10*time.Second, func() bool {
		for _, m := range f.gc.Inbound() {
			if strings.Contains(m.Text, matchText) {
				got = m
				return true
			}
		}
		return false
	})
	return got
}
func (f *fixture) turns(n int) {
	f.t.Helper()
	eventually(f.t, 10*time.Second, func() bool { return len(f.gc.Inbound()) >= n })
	if got := len(f.gc.Inbound()); got != n {
		f.t.Fatalf("accepted POSTs=%d want %d", got, n)
	}
}
func (f *fixture) messages() []*api.Message {
	var out []*api.Message
	f.google.Configure(func(g *fakegoogle.Server) {
		for _, m := range g.Messages {
			b, _ := json.Marshal(m)
			var c api.Message
			_ = json.Unmarshal(b, &c)
			out = append(out, &c)
		}
	})
	return out
}
func conversation(space string) wire.Conversation {
	kind := "room"
	if space == "spaces/dm" {
		kind = "dm"
	}
	return wire.Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "default", ConversationID: space, Kind: kind}
}
func (f *fixture) publishViaGC(text, thread string) server.CommandResult {
	f.seq++
	return f.command("publish", fmt.Sprint("send-", f.seq), text, thread, "")
}
func (f *fixture) command(mode, key, text, thread, path string) server.CommandResult {
	f.t.Helper()
	f.commands++
	space := "spaces/room"
	if strings.HasPrefix(thread, "spaces/dm/") {
		space = "spaces/dm"
	}
	f.gc.Bind(conversation(space), "session-example")
	var result server.CommandResult
	f.request("/commands/"+mode, map[string]string{"session_id": "session-example", "conversation_id": space, "text": text, "thread": thread, "path": path, "idempotency_key": key}, &result)
	return result
}
func (f *fixture) assertSent(result server.CommandResult, n int) {
	f.t.Helper()
	if !result.Delivered || !result.Recorded {
		f.t.Fatalf("send: %+v", result)
	}
	if len(f.messages()) != n {
		f.t.Fatalf("Google messages=%d want %d", len(f.messages()), n)
	}
	s := f.state().Sends[result.Key]
	if s == nil || !s.Recorded || s.Receipt == nil || !s.Receipt.Delivered {
		f.t.Fatalf("saved send: %+v", s)
	}
}

func TestEndToEnd(t *testing.T) {
	t.Run("DM becomes a turn", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := f.emitMessage("spaces/dm", "first DM", "users/222")
		got := f.expectTurn(t, "first DM")
		f.committed(m.Name)
		if got.Conversation != conversation("spaces/dm") || got.Actor.ID != "users/222" || got.ReplyToMessageID != m.Thread.Name {
			t.Fatalf("turn: %+v", got)
		}
		f.turns(1)
		if len(f.messages()) != 0 {
			t.Fatal("first DM required an outbound send")
		}
		if f.state().IdentityUserID != fakegoogle.UserID {
			t.Fatal("identity not persisted")
		}
	})
	t.Run("reply lands in the thread", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := f.emitMessage("spaces/dm", "reply here", "users/222")
		f.expectTurn(t, "reply here")
		r := f.publishViaGC("answer", m.Thread.Name)
		f.assertSent(r, 1)
		if f.messages()[0].Thread.Name != m.Thread.Name {
			t.Fatal("reply escaped DM thread")
		}
	})
	t.Run("space message under each wake setting", func(t *testing.T) {
		for _, wake := range []config.WakePolicy{config.WakeAll, config.WakeMentions} {
			t.Run(string(wake), func(t *testing.T) {
				f := newFixture(t)
				f.cfg.Settings.Defaults.Wake = wake
				f.ready()
				m := f.emitMessage("spaces/room", "ordinary message", "users/222")
				f.committed(m.Name)
				want := 1
				if wake == config.WakeMentions {
					want = 0
				}
				if len(f.gc.Inbound()) != want {
					t.Fatalf("wake %s accepted %d", wake, len(f.gc.Inbound()))
				}
				if want == 0 && f.state().Changes[m.Name].Reason != "wake_policy" {
					t.Fatal("missing filtered outcome")
				}
			})
		}
	})
	t.Run("mention in a `mentions` space", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Settings.Defaults.Wake = config.WakeMentions
		f.ready()
		m := message("spaces/room", "hello <"+fakegoogle.UserID+">", "users/222", "mention", time.Now())
		m.Annotations = []*api.Annotation{{Type: "USER_MENTION", UserMention: &api.UserMentionMetadata{User: &api.User{Name: fakegoogle.UserID, DisplayName: "Assistant"}, Type: "MENTION"}}}
		f.emit("message.v1.created", "mention", map[string]any{"message": m})
		f.expectTurn(t, "hello")
		f.committed(m.Name)
		f.turns(1)
	})
	t.Run("edit and delete reach the session", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := f.emitMessage("spaces/dm", "original", "users/222")
		f.committed(m.Name)
		m.Text = "replacement"
		m.LastUpdateTime = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
		f.emit("message.v1.updated", "edit", map[string]any{"message": m})
		f.expectTurn(t, "edited: replacement")
		f.emit("message.v1.deleted", "delete", map[string]any{"message": m})
		f.committed(m.Name + "#deleted")
		f.expectTurn(t, "deleted")
		f.turns(3)
		if !f.state().Messages[m.Name].Deleted {
			t.Fatal("missing tombstone")
		}
	})
	t.Run("reaction on own message is a turn, on another's is not", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		own := f.publishViaGC("my message", "")
		f.assertSent(own, 1)
		other := f.emitMessage("spaces/room", "their message", "users/222")
		f.committed(other.Name)
		for i, parent := range []string{own.MessageID, other.Name} {
			r := &api.Reaction{Name: parent + "/reactions/r", User: &api.User{Name: "users/333"}, Emoji: &api.Emoji{Unicode: "👍"}}
			f.emit("reaction.v1.created", fmt.Sprint("react", i), map[string]any{"reaction": r})
			f.committed(r.Name + "#created")
		}
		f.turns(2)
		f.expectTurn(t, "reaction created: 👍")
	})
	t.Run("two users react to one message, and one user removes and re-adds a reaction, each delivered once", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		own := f.publishViaGC("react here", "")
		for i, action := range []string{"created", "created", "deleted", "created"} {
			name, user := "a", "users/222"
			if i == 1 {
				name, user = "b", "users/333"
			}
			if i == 3 {
				name = "c"
			}
			r := &api.Reaction{Name: own.MessageID + "/reactions/" + name, User: &api.User{Name: user}, Emoji: &api.Emoji{Unicode: "👍"}}
			for range 2 {
				f.emit("reaction.v1."+action, fmt.Sprint("react", i), map[string]any{"reaction": r})
			}
			f.committed(r.Name + "#" + action)
		}
		f.barrier()
		f.turns(4)
		seen := map[string]bool{}
		for _, m := range f.gc.Inbound() {
			if seen[m.ProviderMessageID] {
				t.Fatal("duplicate reaction")
			}
			seen[m.ProviderMessageID] = true
		}
	})
	t.Run("an edit then a delete, and an update arriving before its creation, apply in the right order", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := message("spaces/dm", "new version", "users/222", "unordered", time.Now().Add(-time.Hour))
		m.LastUpdateTime = time.Now().UTC().Format(time.RFC3339Nano)
		f.emit("message.v1.updated", "early-update", map[string]any{"message": m})
		f.committed(m.Name)
		m.Text = "old version"
		m.LastUpdateTime = ""
		f.emit("message.v1.created", "late-create", map[string]any{"message": m})
		f.emit("message.v1.deleted", "tombstone", map[string]any{"message": m})
		f.committed(m.Name + "#deleted")
		m.Text = "stale edit"
		m.LastUpdateTime = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		f.emit("message.v1.updated", "stale-edit", map[string]any{"message": m})
		f.barrier()
		f.turns(2)
		if f.gc.Inbound()[0].Text != "new version" || !f.state().Messages[m.Name].Deleted || len(f.state().Unresolved) != 0 {
			t.Fatal("revision order lost")
		}
	})
	t.Run("a batch event with three messages, redelivered after only two reached gc, yields exactly three turns", func(t *testing.T) {
		f := newFixture(t)
		var attempts atomic.Int32
		var blocked atomic.Bool
		blocked.Store(true)
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/inbound") && attempts.Add(1) >= 3 && blocked.Load() {
				http.Error(w, "temporary outage", http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		f.ready()
		items := []any{}
		for i := range 3 {
			items = append(items, map[string]any{"message": message("spaces/dm", fmt.Sprint("batch-", i), "users/222", fmt.Sprint("batch", i), time.Now())})
		}
		body := map[string]any{"messages": items}
		f.emit("message.v1.batchCreated", "batch", body)
		eventually(t, 10*time.Second, func() bool { return attempts.Load() >= 3 })
		f.turns(2)
		if f.state().Transport.Seen("cloudevent:batch") {
			t.Fatal("partial batch prematurely acknowledged")
		}
		blocked.Store(false)
		f.emit("message.v1.batchCreated", "batch", body)
		f.expectTurn(t, "batch-2")
		f.barrier()
		f.turns(3)
	})
	t.Run("file in, saved with its type, and the rendered turn text names its path", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		f.google.Configure(func(g *fakegoogle.Server) { g.Media["attachment/note"] = []byte("hello attachment") })
		m := message("spaces/dm", "see file", "users/222", "file", time.Now())
		m.Attachment = []*api.Attachment{{Name: "attachment/note", ContentName: "note.txt", ContentType: "text/plain", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "attachment/note"}}}
		f.emit("message.v1.created", "file", map[string]any{"message": m})
		got := f.expectTurn(t, "see file")
		f.committed(m.Name)
		if len(got.Attachments) != 1 || got.Attachments[0].MIMEType != "text/plain" {
			t.Fatalf("attachments: %+v", got.Attachments)
		}
		path := strings.TrimPrefix(got.Attachments[0].URL, "file://")
		b, e := os.ReadFile(path)
		if e != nil || string(b) != "hello attachment" || !strings.Contains(got.Text, path) {
			t.Fatalf("file path/text: %q %q %v", path, got.Text, e)
		}
	})
	t.Run("an oversize and a forbidden attachment deliver the message with an unavailable marker", func(t *testing.T) {
		for _, kind := range []string{"oversize", "forbidden"} {
			t.Run(kind, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				f.google.Configure(func(g *fakegoogle.Server) { g.Media["attachment/file"] = bytes.Repeat([]byte("x"), 2048) })
				if kind == "forbidden" {
					f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
						if strings.Contains(r.URL.Path, "media/") {
							http.Error(w, `{"error":{"code":403,"message":"forbidden"}}`, http.StatusForbidden)
							return true
						}
						return false
					})
				}
				m := message("spaces/dm", "attachment message", "users/222", kind, time.Now())
				m.Attachment = []*api.Attachment{{ContentName: "file.bin", ContentType: "application/octet-stream", AttachmentDataRef: &api.AttachmentDataRef{ResourceName: "attachment/file"}}}
				f.emit("message.v1.created", kind, map[string]any{"message": m})
				got := f.expectTurn(t, "unavailable")
				f.committed(m.Name)
				if !strings.Contains(got.Text, "attachment message") {
					t.Fatal("message text lost")
				}
				f.turns(1)
			})
		}
	})
	t.Run("file out, posted in the thread", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		path := filepath.Join(f.cfg.Endpoints.UploadRoot, "note.txt")
		write(t, path, "outbound attachment")
		r := f.command("upload", "upload-one", "file caption", "spaces/dm/threads/root", path)
		f.assertSent(r, 1)
		m := f.messages()[0]
		if m.Thread.Name != "spaces/dm/threads/root" || len(m.Attachment) != 1 {
			t.Fatalf("file message: %+v", m)
		}
		if f.state().Sends[r.Key].Upload.ResourceName == "" {
			t.Fatal("missing durable upload")
		}
	})
	t.Run("a paragraph and a code block larger than the limit split correctly, including multibyte text", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		text := strings.Repeat("界", 12000) + "\n\n```go\n" + strings.Repeat("é", 18000) + "\n```"
		r := f.publishViaGC(text, "")
		f.assertSent(r, len(f.state().Sends[r.Key].Parts))
		send := f.state().Sends[r.Key]
		if len(send.Parts) < 3 {
			t.Fatal("large content did not split")
		}
		for _, m := range f.messages() {
			if strings.Contains(m.Text, "é") && (!strings.Contains(m.Text, "```go\n") || !strings.HasSuffix(strings.TrimSpace(m.Text), "```")) {
				t.Fatal("code part lost its language fence or closing fence")
			}
			if len(m.Text) > 32000 || !utf8.ValidString(m.Text) || strings.Count(m.Text, "```")%2 != 0 || m.Thread.Name != send.Thread {
				t.Fatalf("invalid part bytes=%d thread=%s", len(m.Text), m.Thread.Name)
			}
		}
		var all string
		for _, p := range send.Parts {
			all += p.Text
		}
		if strings.Count(all, "界") != 12000 || strings.Count(all, "é") != 18000 {
			t.Fatal("split lost content")
		}
	})
	t.Run("a vanished thread yields one new thread for all parts", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		r := f.publishViaGC(strings.Repeat("z", 65000), "spaces/room/threads/missing")
		send := f.state().Sends[r.Key]
		f.assertSent(r, 3)
		if send.Thread == "spaces/room/threads/missing" {
			t.Fatal("vanished thread not replaced")
		}
		for _, m := range f.messages() {
			if m.Thread.Name != send.Thread {
				t.Fatal("parts forked into different threads")
			}
		}
	})
	t.Run("a multipart publish that fails after the first part resumes without a duplicate", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		var posts atomic.Int32
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") && posts.Add(1) > 1 {
				http.Error(w, `{"error":{"code":400,"message":"injected part failure"}}`, 400)
				return true
			}
			return false
		})
		text := strings.Repeat("p", 65000)
		first := f.command("publish", "parts-retry", text, "", "")
		if first.Delivered || len(f.messages()) != 1 {
			t.Fatalf("first attempt: %+v messages=%d", first, len(f.messages()))
		}
		firstName := f.messages()[0].Name
		f.setHook(nil)
		second := f.command("publish", "parts-retry", text, "", "")
		f.assertSent(second, 3)
		if f.state().Sends[second.Key].Parts[0].MessageName != firstName {
			t.Fatal("first part duplicated")
		}
	})
	scenariosRecovery(t)
	scenariosDelivery(t)
	scenariosOperations(t)
}

// barrier waits for acknowledgments of every emitted transport message. A
// later event alone is insufficient because Pub/Sub delivery is unordered.
func (f *fixture) barrier() {
	f.t.Helper()
	for _, id := range f.emitted {
		eventually(f.t, 10*time.Second, func() bool { m := f.ps.Message(id); return m != nil && m.Acks > 0 })
	}
}

func (f *fixture) history(n int, space string, at time.Time) {
	f.inboundEmitted = true
	f.google.Configure(func(g *fakegoogle.Server) {
		for i := range n {
			m := message(space, fmt.Sprintf("missed %04d", i), "users/222", fmt.Sprintf("history%04d", i), at)
			g.Messages[m.Name] = m
		}
	})
}
func (f *fixture) lifecycle(kind string) {
	f.t.Helper()
	f.google.Emit("google.workspace.events.subscription.v1."+kind, map[string]any{"name": f.state().Subscription.Name}, "spaces/room")
}
func (f *fixture) expireSoon() {
	f.google.Configure(func(g *fakegoogle.Server) {
		for _, s := range g.Subscriptions {
			s.ExpireTime = time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)
			s.UpdateTime = time.Now().Add(-4 * time.Hour).UTC().Format(time.RFC3339Nano)
		}
	})
}
func scenariosRecovery(t *testing.T) {
	t.Run("subscription renewed before expiry", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		name := f.state().Subscription.Name
		f.stop()
		f.google.Configure(func(g *fakegoogle.Server) {
			sub := g.Subscriptions[name]
			sub.UpdateTime = time.Now().UTC().Format(time.RFC3339Nano)
			sub.ExpireTime = time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339Nano)
		})
		f.ready()
		if f.countRequest("PATCH /v1/"+name) != 0 {
			t.Fatal("renewed during startup before scheduled time")
		}
		eventually(t, 5*time.Second, func() bool {
			return f.countRequest("PATCH /v1/"+name) > 0 && f.state().Subscription.Expiry.After(time.Now().Add(time.Hour))
		})
		if f.state().Subscription.Name != name {
			t.Fatal("renewal created a replacement")
		}
		if len(f.gc.Inbound()) != 0 {
			t.Fatal("lifecycle became a turn")
		}
	})
	t.Run("a lost renewal response, a suspended subscription and `ALREADY_EXISTS` each reconcile correctly", func(t *testing.T) {
		for _, kind := range []string{"lost renewal", "suspended", "ALREADY_EXISTS"} {
			t.Run(kind, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				name := f.state().Subscription.Name
				switch kind {
				case "lost renewal":
					f.stop()
					f.google.Configure(func(g *fakegoogle.Server) {
						sub := g.Subscriptions[name]
						sub.UpdateTime = time.Now().UTC().Format(time.RFC3339Nano)
						sub.ExpireTime = time.Now().Add(4 * time.Second).UTC().Format(time.RFC3339Nano)
					})
					f.ready()
					previous := f.state().Subscription.Expiry
					f.google.Configure(func(g *fakegoogle.Server) { g.DropNextResponse = true })
					eventually(t, 10*time.Second, func() bool {
						return f.countRequest("PATCH /v1/"+name) > 0 && f.state().Subscription.Expiry.After(previous.Add(time.Hour))
					})
					f.mu.Lock()
					requests := append([]string(nil), f.requests...)
					f.mu.Unlock()
					patched, read := false, false
					for _, request := range requests {
						if !patched {
							patched = request == "google PATCH /v1/"+name
							continue
						}
						if request == "google GET /v1/"+name {
							read = true
							break
						}
						if strings.HasPrefix(request, "google PATCH ") || strings.HasPrefix(request, "google POST ") {
							t.Fatal("blind mutation after lost renewal response")
						}
					}
					if !read {
						t.Fatal("lost renewal not reconciled with authoritative GET")
					}

				case "suspended":
					f.google.SuspendSubscription()
					f.lifecycle("suspended")
					eventually(t, 5*time.Second, func() bool { return f.countRequest(":reactivate") > 0 && f.state().Subscription.State == "ACTIVE" })
					f.health("ready", 5*time.Second)
				case "ALREADY_EXISTS":
					f.stop()
					l := f.state()
					l.Subscription = ledger.SubscriptionState{}
					if e := l.Save(f.cfg.Endpoints.StateFile); e != nil {
						t.Fatal(e)
					}
					var hidden atomic.Bool
					f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
						if r.Method == "GET" && r.URL.Path == "/v1/subscriptions" && !hidden.Swap(true) {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"subscriptions":[]}`)
							return true
						}
						return false
					})
					before := f.countRequest("POST /v1/subscriptions")
					f.ready()
					if f.countRequest("POST /v1/subscriptions") != before+1 {
						t.Fatal("did not exercise create conflict")
					}
				}
				if f.state().Subscription.Name != name || f.state().Subscription.State != "ACTIVE" {
					t.Fatal("subscription not reconciled")
				}
				var count int
				f.google.Configure(func(g *fakegoogle.Server) { count = len(g.Subscriptions) })
				if count != 1 {
					t.Fatalf("subscriptions=%d", count)
				}
			})
		}
	})
	t.Run("subscription lapsed, catch-up delivers only what was missed, once, with the cursor advancing only past handled records", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := f.emitMessage("spaces/room", "already seen", "users/222")
		f.committed(m.Name)
		f.barrier()
		old := f.state().Spaces["spaces/room"].MessageCursor
		f.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
		f.history(3, "spaces/room", time.Now().Add(-time.Second))
		f.google.Advance(5 * time.Hour)
		var blocked atomic.Bool
		var recoveryChecks atomic.Int32
		blocked.Store(true)
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if strings.HasSuffix(r.URL.Path, "/inbound") {
				if blocked.Load() {
					http.Error(w, "outage", http.StatusServiceUnavailable)
					return true
				}
				recoveryChecks.Add(1)
				if !f.state().Spaces["spaces/room"].MessageCursor.Equal(old) {
					t.Error("cursor advanced before a recovered record was handled")
				}
			}
			return false
		})
		f.lifecycle("expired")
		eventually(t, 5*time.Second, func() bool { return len(f.state().Spaces["spaces/room"].Backlog) > 0 })
		if !f.state().Spaces["spaces/room"].MessageCursor.Equal(old) {
			t.Fatal("cursor advanced over unhandled records")
		}
		blocked.Store(false)
		eventually(t, 20*time.Second, func() bool {
			state := f.state().Spaces["spaces/room"]
			return len(state.Backlog) == 0 && state.MessageCursor.After(old)
		})
		f.health("ready", 20*time.Second)
		f.turns(4)
		s := f.state().Spaces["spaces/room"]
		if !s.MessageCursor.After(old) || len(s.Backlog) != 0 || recoveryChecks.Load() == 0 {
			t.Fatal("cursor did not settle")
		}
		for _, v := range f.gc.Inbound()[1:] {
			if !strings.HasPrefix(v.Text, "[catch-up]\n") {
				t.Fatal("missing catch-up marker")
			}
		}
		f.stop()
		f.ready()
		f.turns(4)
	})
	t.Run("three thousand missed messages drain over several passes with the backlog visible in status", func(t *testing.T) {
		f := newFixture(t)
		f.history(3000, "spaces/room", time.Now().Add(-time.Minute))
		f.start()
		f.health("catching-up", 10*time.Second)
		var backlog int
		eventually(t, 20*time.Second, func() bool { _ = json.Unmarshal(f.status()["backlog"], &backlog); return backlog > 50 })
		deadline := time.Now().Add(20 * time.Minute)
		if packageDeadline, ok := t.Deadline(); ok {
			// Durable recovery uses the package budget, reserving time for later scenarios and shutdown.
			deadline = packageDeadline.Add(-3 * time.Minute)
		}
		drained := false
		delivered, reported := 0, 0
		for time.Now().Before(deadline) {
			status := f.status()
			_ = json.Unmarshal(status["backlog"], &backlog)
			var counters map[string]int64
			_ = json.Unmarshal(status["counters"], &counters)
			delivered = int(counters["turns:spaces/room"])
			if delivered >= reported+500 {
				t.Logf("catch-up delivered %d/3000; retained backlog %d", delivered, backlog)
				reported = (delivered / 500) * 500
			}
			if backlog == 0 {
				drained = true
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		if !drained {
			t.Fatalf("catch-up did not drain before test budget: delivered %d/3000; retained backlog %d", delivered, backlog)
		}
		f.health("ready", 10*time.Second)
		posts := f.gc.Inbound()
		if len(posts) != 3000 {
			t.Fatalf("turns=%d", len(posts))
		}
		seen := map[string]bool{}
		for _, m := range posts {
			if seen[m.ProviderMessageID] {
				t.Fatal("duplicate backlog turn")
			}
			seen[m.ProviderMessageID] = true
			if !strings.HasPrefix(m.Text, "[catch-up]\n") {
				t.Fatal("unmarked backlog turn")
			}
		}
		s := f.state().Spaces["spaces/room"]
		if len(s.Backlog) != 0 || !s.FrozenUpper.IsZero() || s.MessageCursor.IsZero() || !s.EventCursor.Equal(s.MessageCursor) {
			t.Fatalf("backlog did not finish: %+v", s)
		}
		if f.countRequest("GET /v1/spaces/room/messages") < 2 {
			t.Fatal("history not paginated")
		}
	})
	t.Run("a space joined during downtime is caught up", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		f.stop()
		f.google.Configure(func(g *fakegoogle.Server) {
			g.Spaces["spaces/joined"] = &api.Space{Name: "spaces/joined", SpaceType: "SPACE"}
		})
		f.history(1, "spaces/joined", time.Now().Add(-time.Second))
		f.ready()
		turn := f.expectTurn(t, "missed 0000")
		if turn.Conversation.ConversationID != "spaces/joined" || !strings.HasPrefix(turn.Text, "[catch-up]") {
			t.Fatalf("joined turn: %+v", turn)
		}
		f.turns(1)
		if f.state().Spaces["spaces/joined"].MessageCursor.IsZero() {
			t.Fatal("new space cursor missing")
		}
	})
	t.Run("an old message edited during downtime is recovered through space events", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := f.emitMessage("spaces/room", "original before downtime", "users/222")
		f.committed(m.Name)
		f.stop()
		m.CreateTime = time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
		m.LastUpdateTime = time.Now().Add(-time.Millisecond).UTC().Format(time.RFC3339Nano)
		m.Text = "edited during downtime"
		f.google.Configure(func(g *fakegoogle.Server) {
			g.Messages[m.Name] = m
			g.SpaceEvents["spaces/room"] = []*api.SpaceEvent{{Name: "spaces/room/spaceEvents/edit", EventType: "google.workspace.chat.message.v1.updated", EventTime: m.LastUpdateTime, MessageUpdatedEventData: &api.MessageUpdatedEventData{Message: m}}}
		})
		f.ready()
		f.expectTurn(t, "edited: edited during downtime")
		f.turns(2)
		if f.countRequest("GET /v1/spaces/room/spaceEvents") == 0 {
			t.Fatal("event history never read")
		}
		f.committed(m.Name + "#updated#" + m.LastUpdateTime)
	})
	t.Run("the same event twice yields one turn", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		m := message("spaces/dm", "duplicate", "users/222", "duplicate", time.Now())
		for range 2 {
			f.emit("message.v1.created", "same-cloud-event", map[string]any{"message": m})
		}
		f.committed(m.Name)
		f.barrier()
		f.turns(1)
		if !f.state().Transport.Seen("cloudevent:same-cloud-event") {
			t.Fatal("missing transport dedup")
		}
	})
	t.Run("a live event arriving while catch-up runs does not move the frozen cursors, equal timestamps across a page boundary are not skipped, and a restart in the middle of draining a backlog resumes from the persisted progress", func(t *testing.T) {
		f := newFixture(t)
		f.cfg.Settings.CatchupBudget = 5
		f.history(23, "spaces/room", time.Now().Add(-time.Minute))
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "GET" && r.URL.Path == "/v1/spaces/room/messages" {
				q := r.URL.Query()
				q.Set("pageSize", "7")
				r.URL.RawQuery = q.Encode()
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			return false
		})
		f.start()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("history did not start")
		}
		before := *f.state().Spaces["spaces/room"]
		live := f.emitMessage("spaces/room", "live during recovery", "users/333")
		f.committed(live.Name)
		after := f.state().Spaces["spaces/room"]
		if !after.MessageCursor.Equal(before.MessageCursor) || !after.EventCursor.Equal(before.EventCursor) || !after.FrozenUpper.Equal(before.FrozenUpper) {
			t.Fatal("live delivery moved frozen recovery")
		}
		once.Do(func() { close(release) })
		eventually(t, 10*time.Second, func() bool {
			l := f.state()
			s := l.Spaces["spaces/room"]
			if len(s.Backlog) != 18 || len(l.Changes) != 6 {
				return false
			}
			for _, change := range l.Changes {
				if change.Status != "committed" {
					return false
				}
			}
			return true
		})
		f.stop()
		saved := f.state()
		if len(saved.Spaces["spaces/room"].Backlog) == 0 {
			t.Fatal("restart did not interrupt backlog")
		}
		// Persisted backlog must be sufficient even when remote history is gone.
		f.google.Configure(func(g *fakegoogle.Server) { g.Messages = map[string]*api.Message{} })
		f.setHook(nil)
		f.ready()
		f.turns(24)
		seen := map[string]bool{}
		for _, m := range f.gc.Inbound() {
			if seen[m.ProviderMessageID] {
				t.Fatal("restart duplicated a turn")
			}
			seen[m.ProviderMessageID] = true
		}
		if len(f.state().Spaces["spaces/room"].Backlog) != 0 {
			t.Fatal("restart failed to drain")
		}
	})
}

func dropResponse(t *testing.T, w http.ResponseWriter, r *http.Request, target string) {
	t.Helper()
	u, _ := url.Parse(target)
	copy := r.Clone(r.Context())
	copy.URL.Scheme = u.Scheme
	copy.URL.Host = u.Host
	copy.RequestURI = ""
	resp, e := http.DefaultClient.Do(copy)
	if e != nil {
		t.Errorf("fault forwarding: %v", e)
		http.Error(w, "fault forwarding failed", 500)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	conn, _, e := w.(http.Hijacker).Hijack()
	if e != nil {
		t.Errorf("hijack: %v", e)
		return
	}
	_ = conn.Close()
}
func (f *fixture) unresolved(id string) {
	f.t.Helper()
	eventually(f.t, 12*time.Second, func() bool { return f.state().Unresolved[id] != nil })
}
func (f *fixture) transport(id string) {
	f.t.Helper()
	eventually(f.t, 10*time.Second, func() bool { return f.state().Transport.Seen("cloudevent:" + id) })
}
func scenariosDelivery(t *testing.T) {
	t.Run("gc down, event held, delivered on return", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		f.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = true })
		m := f.emitMessage("spaces/dm", "waiting for gc", "users/222")
		eventually(t, 5*time.Second, func() bool {
			c := f.state().Changes[m.Name]
			return c != nil && c.Status == "intent" && c.Reason == "retry"
		})
		if len(f.gc.Inbound()) != 0 || f.state().Transport.Seen("cloudevent:"+m.Name) {
			t.Fatal("event acknowledged during outage")
		}
		f.gc.Configure(func(g *fakegc.Server) { g.CityNotRunning = false })
		f.expectTurn(t, "waiting for gc")
		f.committed(m.Name)
		f.turns(1)
	})
	t.Run("every gc delivery status (delivered, pending then concluded, partial, failed, no_route) handled as specified", func(t *testing.T) {
		for _, status := range []string{"delivered", "pending", "partial", "failed", "no_route"} {
			t.Run(status, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				f.gc.Configure(func(g *fakegc.Server) { g.NextDelivery = status })
				m := f.emitMessage("spaces/dm", "status "+status, "users/222")
				if status == "pending" {
					eventually(t, 5*time.Second, func() bool { c := f.state().Changes[m.Name]; return c != nil && c.Status == "pending" })
					c := f.state().Changes[m.Name]
					r := wire.ReceiptResponse{State: "concluded"}
					r.Delivery.Status = "delivered"
					f.gc.SetReceipt(c.ReceiptID, r)
				}
				f.committed(m.Name)
				want := 1
				if status == "partial" || status == "failed" {
					want = 2
				}
				f.turns(want)
				c := f.state().Changes[m.Name]
				if c.Attempts != want {
					t.Fatalf("attempts=%d want %d", c.Attempts, want)
				}
				if status == "no_route" && (c.Reason != "no_route" || f.state().Counters["no_route:spaces/dm"] != 1) {
					t.Fatal("no_route not recorded distinctly")
				}
			})
		}
	})
	t.Run("a receipt that ages out, a concluded receipt still pending, and a lost POST response each become an unresolved delivery that is never reposted on its own and that `resolve --accept` and `--replay` settle", func(t *testing.T) {
		for i, kind := range []string{"aged out", "concluded pending", "lost POST"} {
			t.Run(kind, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				if kind == "lost POST" {
					var dropped atomic.Bool
					f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
						if strings.HasSuffix(r.URL.Path, "/inbound") && !dropped.Swap(true) {
							dropResponse(t, w, r, f.gc.URL())
							return true
						}
						return false
					})
				} else {
					f.gc.Configure(func(g *fakegc.Server) { g.NextDelivery = "pending" })
				}
				m := f.emitMessage("spaces/dm", "ambiguous "+kind, "users/222")
				if kind != "lost POST" {
					eventually(t, 5*time.Second, func() bool { c := f.state().Changes[m.Name]; return c != nil && c.Status == "pending" })
					r := wire.ReceiptResponse{State: "concluded"}
					r.Delivery.Status = "pending"
					if kind == "aged out" {
						// The receipt concludes and expires during an outage. Stop
						// polling before exposing its terminal state, so a legitimate
						// poll cannot commit delivery between SetReceipt and Advance.
						f.stop()
						r.Delivery.Status = "delivered"
					}
					f.gc.SetReceipt(f.state().Changes[m.Name].ReceiptID, r)
					if kind == "aged out" {
						f.gc.Advance(time.Hour + time.Second)
						f.ready()
					}
				}
				f.unresolved(m.Name)
				f.stop()
				f.ready()
				f.unresolved(m.Name)
				f.turns(1)
				action := "accept"
				if i == 0 {
					action = "replay"
				}
				f.request("/resolve", map[string]string{"id": m.Name, "action": action}, nil)
				f.committed(m.Name)
				if len(f.state().Unresolved) != 0 {
					t.Fatal("resolution retained unresolved record")
				}
				want := 1
				if action == "replay" {
					want = 2
				}
				f.turns(want)
			})
		}
	})
	t.Run("a crash before gc's acceptance loses nothing and a crash after terminal delivery but before the local commit yields an unresolved delivery, not a silent duplicate", func(t *testing.T) {
		for _, boundary := range []string{"before acceptance", "after terminal before commit"} {
			t.Run(boundary, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				var snapshot []byte
				var mu sync.Mutex
				var blocked atomic.Bool
				blocked.Store(true)
				f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if !strings.HasSuffix(r.URL.Path, "/inbound") {
						return false
					}
					if boundary == "before acceptance" && blocked.Load() {
						http.Error(w, "not accepted", http.StatusServiceUnavailable)
						return true
					}
					mu.Lock()
					snapshot, _ = os.ReadFile(f.cfg.Endpoints.StateFile)
					mu.Unlock()
					return false
				})
				m := f.emitMessage("spaces/dm", "crash boundary", "users/222")
				if boundary == "before acceptance" {
					eventually(t, 5*time.Second, func() bool { c := f.state().Changes[m.Name]; return c != nil && c.Reason == "retry" })
					f.stop()
					if len(f.gc.Inbound()) != 0 {
						t.Fatal("gc accepted before injected crash")
					}
					blocked.Store(false)
					f.setHook(nil)
					f.ready()
					f.committed(m.Name)
					f.turns(1)
				} else {
					f.committed(m.Name)
					f.stop()
					mu.Lock()
					saved := append([]byte(nil), snapshot...)
					mu.Unlock()
					if len(saved) == 0 {
						t.Fatal("did not capture attempted intent")
					}
					write(t, f.cfg.Endpoints.StateFile, string(saved))
					f.setHook(nil)
					f.ready()
					f.unresolved(m.Name)
					f.turns(1)
					if !strings.Contains(f.state().Unresolved[m.Name].Reason, "interrupted POST") {
						t.Fatal("interrupted terminal delivery not identified")
					}
				}
			})
		}
	})
	t.Run("publish retried with the same key yields one message", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		f.gc.ForgetRegistrations()
		registrations := f.countRequest("POST /v0/city/examplecity/extmsg/adapters")
		unavailable := f.command("publish", "same-key", "once", "", "")
		if unavailable.Delivered || unavailable.FailureKind != "transient" || len(f.messages()) != 0 {
			t.Fatalf("no-adapter 422 must be transient and unsent: %+v", unavailable)
		}
		eventually(t, 10*time.Second, func() bool { return f.countRequest("POST /v0/city/examplecity/extmsg/adapters") > registrations })
		first := f.command("publish", "same-key", "once", "", "")
		second := f.command("publish", "same-key", "once", "", "")
		f.assertSent(second, 1)
		if first.MessageID != second.MessageID {
			t.Fatal("retry changed message")
		}
		f.stop()
		f.ready()
		third := f.command("publish", "same-key", "once", "", "")
		f.assertSent(third, 1)
		if third.MessageID != first.MessageID {
			t.Fatal("restart changed idempotency outcome")
		}
	})
	t.Run("a wrapper publish from a bound session travels wrapper, command handler, gc, callback and makes exactly one Google send", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		f.gc.Bind(conversation("spaces/room"), "session-example")
		pack, e := filepath.Abs("../../..")
		if e != nil {
			t.Fatal(e)
		}
		f.commands++
		cmd := exec.CommandContext(t.Context(), "sh", filepath.Join(pack, "commands/publish.sh"), "--session", "session-example", "--conversation-id", "spaces/room", "--body", "wrapper body", "--idempotency-key", "wrapper-key")
		cmd.Env = append(os.Environ(), "GC_PACK_DIR="+pack, "GC_API_BASE_URL="+f.gateway, "GC_CITY_NAME=examplecity", "GC_SESSION_NAME=", "GC_SESSION_ID=")
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("wrapper: %v %s", e, out)
		}
		if !bytes.Contains(out, []byte(`"recorded": true`)) || len(f.gc.Published()) != 1 || len(f.messages()) != 1 {
			t.Fatalf("wrapper outcome: %s callbacks=%d messages=%d", out, len(f.gc.Published()), len(f.messages()))
		}
		if !f.state().Sends["wrapper-key"].Recorded {
			t.Fatal("wrapper did not commit transcript outcome")
		}
	})
	t.Run("an upload crashes between the send and the recording and is repaired from its intent", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		path := filepath.Join(f.cfg.Endpoints.UploadRoot, "crash.txt")
		write(t, path, "small upload")
		var mu sync.Mutex
		var snapshot []byte
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/extmsg/outbound") {
				mu.Lock()
				snapshot, _ = os.ReadFile(f.cfg.Endpoints.StateFile)
				mu.Unlock()
				http.Error(w, "recording interrupted before gc", http.StatusServiceUnavailable)
				return true
			}
			return false
		})
		first := f.command("upload", "upload-crash", "caption", "", path)
		if !first.Delivered || first.Recorded || len(f.messages()) != 1 || len(f.gc.Published()) != 0 {
			t.Fatalf("crash crossed recording boundary: %+v callbacks=%d", first, len(f.gc.Published()))
		}
		f.stop()
		sendsBefore := f.countRequest("POST /v1/spaces/room/messages")
		mu.Lock()
		data := append([]byte(nil), snapshot...)
		mu.Unlock()
		var captured ledger.Ledger
		if json.Unmarshal(data, &captured) != nil || captured.Sends["upload-crash"].Upload.ResourceName == "" || captured.Sends["upload-crash"].Receipt == nil || !captured.Sends["upload-crash"].Receipt.Delivered || !captured.Sends["upload-crash"].RecordPending || captured.Sends["upload-crash"].Recorded {
			t.Fatal("snapshot is not completed-send/before-recording boundary")
		}
		write(t, f.cfg.Endpoints.StateFile, string(data))
		f.setHook(nil)
		f.ready()
		eventually(t, 12*time.Second, func() bool { return f.state().Sends["upload-crash"].Recorded })
		if len(f.messages()) != 1 || f.countRequest("POST /v1/spaces/room/messages") != sendsBefore {
			t.Fatal("recovered upload sent duplicate message")
		}
		if len(f.gc.Published()) != 1 {
			t.Fatalf("recording callbacks=%d", len(f.gc.Published()))
		}
	})
	t.Run("a successful send followed by a gc transcript failure is reported as delivered but unrecorded and recorded on retry", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = true })
		first := f.command("publish", "transcript-retry", "delivered text", "", "")
		if !first.Delivered || first.Recorded || !f.state().Sends[first.Key].RecordPending {
			t.Fatalf("first result: %+v", first)
		}
		f.gc.Configure(func(g *fakegc.Server) { g.FailTranscript = false })
		second := f.command("publish", "transcript-retry", "delivered text", "", "")
		f.assertSent(second, 1)
		if second.MessageID != first.MessageID {
			t.Fatal("transcript repair changed message")
		}
	})
	t.Run("a response lost after Google accepted a message is repaired on retry", func(t *testing.T) {
		f := newFixture(t)
		f.ready()
		var dropped atomic.Bool
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") && !dropped.Swap(true) {
				dropResponse(t, w, r, f.google.URL())
				return true
			}
			return false
		})
		first := f.command("publish", "lost-google", "safe retry", "", "")
		if !dropped.Load() {
			t.Fatal("did not drop send response")
		}
		second := f.command("publish", "lost-google", "safe retry", "", "")
		f.assertSent(second, 1)
		if first.Delivered && first.MessageID != second.MessageID {
			t.Fatal("repaired wrong message")
		}
	})
}

func (f *fixture) outbound(key, text string) wire.OutboundResult {
	f.t.Helper()
	f.commands++
	f.gc.Bind(conversation("spaces/room"), "session-example")
	data, _ := json.Marshal(wire.OutboundRequest{SessionID: "session-example", Conversation: conversation("spaces/room"), Text: text, IdempotencyKey: key})
	r, e := http.NewRequestWithContext(f.t.Context(), "POST", f.gateway+"/v0/city/examplecity/extmsg/outbound", bytes.NewReader(data))
	if e != nil {
		f.t.Fatal(e)
	}
	r.Header.Set(wire.RequestHeader, "e2e")
	r.Header.Set("Content-Type", "application/json")
	resp, e := http.DefaultClient.Do(r)
	if e != nil {
		f.t.Fatal(e)
	}
	defer resp.Body.Close()
	var result wire.OutboundResult
	if e = json.NewDecoder(resp.Body).Decode(&result); e != nil {
		f.t.Fatal(e)
	}
	if resp.StatusCode != 200 {
		f.t.Fatalf("outbound status %d", resp.StatusCode)
	}
	return result
}
func scenariosOperations(t *testing.T) {
	t.Run("quota exhaustion and a queue delay beyond gc's deadline return the specified undelivered receipt with its retry hint decoded by the fake gc", func(t *testing.T) {
		// Task 16 owns the real-gc contract. Here both the production wire receipt
		// and fakegc's strict callback decoder are exercised over actual HTTP.
		for _, kind := range []string{"quota exhaustion", "queue deadline"} {
			t.Run(kind, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				text := "quota"
				if kind == "quota exhaustion" {
					f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
						if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/messages") {
							w.Header().Set("Retry-After", "60")
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(429)
							_, _ = io.WriteString(w, `{"error":{"code":429,"message":"quota exhausted","status":"RESOURCE_EXHAUSTED"}}`)
							return true
						}
						return false
					})
				} else {
					text = strings.Repeat("x", 32*32000)
				}
				started := time.Now()
				result := f.outbound("deadline", text)
				elapsed := time.Since(started)
				if result.Receipt.Delivered || result.TranscriptEntry != nil || result.Receipt.RetryAfter <= 0 {
					t.Fatalf("undelivered receipt: %+v", result)
				}
				if elapsed >= 30*time.Second {
					t.Fatalf("callback missed gc deadline: %s", elapsed)
				}
				if kind == "quota exhaustion" {
					if result.Receipt.FailureKind != "rate_limited" || result.Receipt.RetryAfter != int64(time.Minute) || len(f.messages()) != 0 {
						t.Fatalf("quota hint: %+v", result)
					}
				} else {
					if result.Receipt.FailureKind != "transient" || len(f.messages()) == 0 || len(f.messages()) >= 32 {
						t.Fatalf("queue receipt: %+v messages=%d", result, len(f.messages()))
					}
					send := f.state().Sends["deadline"]
					if send == nil || send.Parts[len(send.Parts)-1].MessageName != "" {
						t.Fatal("queue falsely committed unsent part")
					}
				}
				if len(f.gc.Published()) != 1 {
					t.Fatal("gc retried an undelivered receipt")
				}
			})
		}
	})
	t.Run("a lost response to the first subscription create is recovered by discovery, and an Operation that completes late is awaited", func(t *testing.T) {
		for _, kind := range []string{"lost create", "late Operation"} {
			t.Run(kind, func(t *testing.T) {
				f := newFixture(t)
				f.google.Configure(func(g *fakegoogle.Server) {
					if kind == "lost create" {
						g.DropNextResponse = true
					} else {
						g.OperationDelay = 1500 * time.Millisecond
					}
				})
				f.start()
				if kind == "late Operation" {
					f.health("initializing", time.Second)
					time.Sleep(250 * time.Millisecond)
					if f.state().Subscription.Name != "" {
						t.Fatal("persisted unfinished Operation")
					}
				}
				f.health("ready", 12*time.Second)
				var count int
				f.google.Configure(func(g *fakegoogle.Server) { count = len(g.Subscriptions) })
				if count != 1 || f.state().Subscription.State != "ACTIVE" {
					t.Fatal("subscription creation not reconciled")
				}
				if kind == "lost create" && f.countRequest("GET /v1/subscriptions") < 2 {
					t.Fatal("lost create did not discover subscription")
				}
				if kind == "late Operation" && f.countRequest("GET /v1/operations/") == 0 {
					t.Fatal("Operation was not polled")
				}
			})
		}
	})
	t.Run("each lifecycle event type, and a reactivation that arrives with no event, is handled", func(t *testing.T) {
		for _, kind := range []string{"expirationReminder", "expired", "suspended", "reactivation without event"} {
			t.Run(kind, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				old := f.state().Subscription.Name
				switch kind {
				case "expirationReminder":
					f.expireSoon()
					f.lifecycle(kind)
					eventually(t, 5*time.Second, func() bool { return f.countRequest("PATCH /v1/"+old) > 0 })
				case "expired":
					f.google.Advance(5 * time.Hour)
					f.lifecycle(kind)
					eventually(t, 10*time.Second, func() bool { return f.state().Subscription.Name != old })
				case "suspended":
					f.google.SuspendSubscription()
					f.lifecycle(kind)
					eventually(t, 5*time.Second, func() bool { return f.countRequest(":reactivate") > 0 && f.state().Subscription.State == "ACTIVE" })
				case "reactivation without event":
					f.google.SuspendSubscription()
					f.history(1, "spaces/room", time.Now().Add(-time.Second))
					// Google emits suspension, but never a reactivation event.
					// Recovery and catch-up must happen in this same process.
					f.lifecycle("suspended")
					f.expectTurn(t, "missed 0000")
					if f.countRequest(":reactivate") == 0 {
						t.Fatal("suspended subscription not discovered")
					}
				}
				f.health("ready", 10*time.Second)
				if f.state().Subscription.State != "ACTIVE" {
					t.Fatal("subscription remains inactive")
				}
				want := 0
				if kind == "reactivation without event" {
					want = 1
				}
				if len(f.gc.Inbound()) != want {
					t.Fatal("lifecycle forwarded as a turn")
				}
			})
		}
	})
	t.Run("a corrupt ledger puts the service into `recovery-required`, a restored backup resumes it, and a reset reports the possible loss", func(t *testing.T) {
		for _, action := range []string{"restore", "reset"} {
			t.Run(action, func(t *testing.T) {
				f := newFixture(t)
				f.ready()
				m := f.emitMessage("spaces/dm", "before corruption", "users/222")
				f.committed(m.Name)
				f.barrier()
				f.stop()
				backup := filepath.Join(f.cfg.SecretsDir, "backup.json")
				b, e := os.ReadFile(f.cfg.Endpoints.StateFile)
				if e != nil {
					t.Fatal(e)
				}
				write(t, backup, string(b))
				write(t, f.cfg.Endpoints.StateFile, "broken ledger")
				f.start()
				f.health("recovery-required", time.Second)
				f.status()
				if b, e = os.ReadFile(f.cfg.Endpoints.StateFile); e != nil || string(b) != "broken ledger" {
					t.Fatal("service overwrote corrupt state")
				}
				var result map[string]string
				f.request("/state", map[string]string{"action": action, "file": backup}, &result)
				if action == "reset" && !strings.Contains(result["reason"], "lost or duplicated") {
					t.Fatal("reset omitted possible loss")
				}
				f.health("ready", 10*time.Second)
				if action == "restore" {
					f.emit("message.v1.created", m.Name, map[string]any{"message": m})
					f.transport(m.Name)
					f.turns(1)
				}
				next := f.emitMessage("spaces/dm", "after recovery", "users/222")
				f.committed(next.Name)
				f.turns(2)
			})
		}
	})
	t.Run("revoked token puts the adapter in `needs-sign-in` and health says so", func(t *testing.T) {
		f := newFixture(t)
		f.replaceToken(true)
		f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = "invalid_grant" })
		f.start()
		h := f.health("needs-sign-in", 5*time.Second)
		if !strings.Contains(h.Reason, "auth") {
			t.Fatalf("unactionable health: %+v", h)
		}
		f.status()
		f.emitMessage("spaces/dm", "retained while revoked", "users/222")
		if len(f.gc.Inbound()) != 0 {
			t.Fatal("revoked credential delivered a turn")
		}
		eventually(t, 2*time.Second, func() bool {
			b, _ := os.ReadFile(os.Getenv("GC_STUB_LOG"))
			return strings.Contains(string(b), "mail send")
		})
		f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = "" })
		f.replaceToken(false)
		f.health("ready", 10*time.Second)
		f.expectTurn(t, "retained while revoked")
		f.turns(1)
	})
	t.Run("`auth` run while the service is running with no token, with a revoked token, and while it is stopped each leave it working", func(t *testing.T) {
		for _, state := range []string{"no token", "revoked token", "stopped"} {
			t.Run(state, func(t *testing.T) {
				f := newFixture(t)
				if state == "no token" {
					f.cfg.Secrets.TokenFile = filepath.Join(f.cfg.SecretsDir, "new-token.json")
				} else if state == "revoked token" {
					f.replaceToken(true)
					f.google.Configure(func(g *fakegoogle.Server) { g.TokenError = "invalid_grant" })
				}
				if state != "stopped" {
					f.start()
					f.health("needs-sign-in", 5*time.Second)
				}
				var exchanges atomic.Int32
				f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
					if r.URL.Path != "/token" {
						return false
					}
					exchanges.Add(1)
					payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"123456789"}`))
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "refresh_token": "new-refresh", "token_type": "Bearer", "expires_in": 3600, "scope": strings.Join(token.Scopes, " "), "id_token": "header." + payload + ".signature"})
					return true
				})
				var output bytes.Buffer
				err := token.RunAuth(t.Context(), f.cfg.Secrets.OAuthClientFile, f.cfg.Secrets.TokenFile, f.cfg.Endpoints, token.Scopes, &output, func(authURL string) error {
					u, e := url.Parse(authURL)
					if e != nil {
						return e
					}
					q := u.Query()
					callback, e := url.Parse(q.Get("redirect_uri"))
					if e != nil {
						return e
					}
					callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {"example-code"}}.Encode()
					r, e := http.Get(callback.String())
					if e == nil {
						r.Body.Close()
					}
					return e
				})
				if err != nil {
					t.Fatal(err)
				}
				if exchanges.Load() != 1 {
					t.Fatalf("OAuth exchanges=%d", exchanges.Load())
				}
				if state == "stopped" {
					f.start()
				}
				f.health("ready", 10*time.Second)
				m := f.emitMessage("spaces/dm", "auth recovered "+state, "users/222")
				f.committed(m.Name)
				f.turns(1)
				if f.state().IdentityUserID != fakegoogle.UserID {
					t.Fatal("consent identity not persisted")
				}
			})
		}
	})
	t.Run("initialization that exceeds five seconds under gc supervision keeps the service alive with health degraded", func(t *testing.T) {
		f := newFixture(t)
		f.google.Configure(func(g *fakegoogle.Server) { g.OperationDelay = time.Millisecond })
		release := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f.setHook(func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/operations/") {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			}
			return false
		})
		start := time.Now()
		f.start()
		f.health("initializing", time.Second)
		if time.Since(start) >= time.Second {
			t.Fatal("socket health exceeded one second")
		}
		f.status()
		time.Sleep(5100 * time.Millisecond)
		f.health("initializing", time.Second)
		if f.countRequest("gc POST /v0/city/examplecity/extmsg/adapters") == 0 {
			t.Fatal("service not registered while initializing")
		}
		once.Do(func() { close(release) })
		f.health("ready", 10*time.Second)
		if f.state().Subscription.State != "ACTIVE" {
			t.Fatal("initialization did not finish")
		}
	})
}

// Cross-check every scenario's remote observations against the final durable
// ledger, including absence of unintended sends in inbound-only scenarios.
func (f *fixture) assertStoresConsistent() {
	f.t.Helper()
	l := f.state()
	if !f.inboundEmitted && len(f.gc.Inbound()) != 0 {
		f.t.Error("unexpected gc inbound turn")
	}
	if f.commands == 0 && len(l.Sends) != 0 {
		f.t.Error("inbound-only scenario created an outbound send intent")
	}
	for _, m := range f.gc.Inbound() {
		c := l.Changes[m.ProviderMessageID]
		// A reset explicitly discards old local evidence; its scenario asserts
		// that loss warning and verifies new delivery after the reset separately.
		if c == nil && strings.Contains(f.t.Name(), "/reset") {
			continue
		}
		if c == nil {
			f.t.Errorf("gc accepted %s without durable change", m.ProviderMessageID)
		}
	}
	for _, m := range f.messages() {
		if m.Sender == nil || m.Sender.Name != fakegoogle.UserID {
			continue
		}
		found := false
		for _, send := range l.Sends {
			for _, part := range send.Parts {
				if part.MessageName == m.Name {
					found = true
				}
			}
		}
		if !found {
			f.t.Errorf("Google message %s has no durable send part", m.Name)
		}
	}
}
