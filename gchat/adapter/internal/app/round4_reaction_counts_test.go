package app

// PR 104 round 4, R2 (Google Chat): with ACK_REACTION and NEVER_SILENT both
// on, a session's reaction counts for never-silent exactly as it would with
// the acknowledgement off. The acknowledgement never adds an answer and never
// removes one.
//
// The acknowledgement and the react verb share the adapter's one identity, so
// Google answers the session's reaction of the emoji the acknowledgement
// placed with 409 (what chat.CreateArrivalReaction expects; the fake models
// it only with RepeatedReactionConflicts, which these tests turn on). Main's
// react verb answers any error 502 and records no act.
//
// This package imports both the server and the inbound packages, so the rig
// is the start's own pieces: chat.New, arrival.New and arrival.Wire (as
// workers.go wires every client), server.New with its react verb, inbound.New,
// and a never-silent engine on a test clock whose Post is
// server.PostNoticeWithClient on the shared client, the call never_silent.go
// makes. A restart rebuilds every piece from disk, the ledger included, and
// reconciles never-silent as neverSilentGate does.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/arrival"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/server"
	"gchat-adapter/internal/stopwatch"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

const (
	round4Space = "spaces/dm"
	round4Eyes  = "\U0001F440"
	round4Thumb = "\U0001F44D"
)

type round4Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *round4Clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *round4Clock) Add(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type round4Chat struct {
	t      *testing.T
	google *fakegoogle.Server
	gc     *fakegc.Server
	cfg    *config.Config
	dir    string
	state  string
	health string
	clock  *round4Clock
	opts   neversilent.Options
	refuse atomic.Bool // refuse the next reaction create, placing nothing
	lose   atomic.Bool // let the next reaction create reach Google, then lose its answer
	// during runs after the next reaction create reached Google and before
	// its answer returns: a reaction that arrives while the call is in flight.
	during   func()
	duringMu sync.Mutex
	seq      int
	mu       *sync.Mutex
	l        *ledger.Ledger
	c        *chat.Client
	s        *server.Server
	p        *inbound.Processor
	service  *ack.Service
	watch    *stopwatch.Writer
	engine   *neversilent.Engine
}

func newRound4Chat(t *testing.T) *round4Chat {
	t.Helper()
	dir := t.TempDir()
	r := &round4Chat{t: t, google: fakegoogle.New(t, nil), gc: fakegc.New(t), dir: dir, state: filepath.Join(dir, "state"), health: filepath.Join(dir, "health.json"),
		clock: &round4Clock{now: time.Now().UTC().Truncate(time.Millisecond)}}
	r.google.Configure(func(g *fakegoogle.Server) {
		g.Spaces[round4Space] = &api.Space{Name: round4Space, SpaceType: "DIRECT_MESSAGE"}
		g.RepeatedReactionConflicts = true
	})
	r.cfg = &config.Config{SecretsDir: dir, Secrets: config.Secrets{CityName: "examplecity", GCAPIBase: r.gc.URL()},
		Endpoints: config.Endpoints{ChatAPIBase: r.google.URL(), MediaDir: filepath.Join(dir, "media"), UploadRoot: dir, MediaMaxBytes: 1024, StateFile: filepath.Join(dir, "state.json")}}
	r.cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
	r.opts = neversilent.Options{Enabled: true, Path: filepath.Join(dir, "never-silent.json"), HealthFile: r.health, City: "examplecity", Channel: "gchat", Now: r.clock.Now,
		Post: func(ctx context.Context, m neversilent.Message, line string) error {
			return r.s.PostNoticeWithClient(ctx, r.c, m, line)
		}}
	r.mu = &sync.Mutex{}
	r.l = ledger.New()
	return r
}

// RoundTrip refuses one reaction create when asked, as Google refusing the
// acknowledgement's call, or lets it through and then loses the answer, or
// holds the answer while another reaction runs; everything else reaches the
// fake as it is.
func (r *round4Chat) RoundTrip(req *http.Request) (*http.Response, error) {
	create := req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/reactions")
	if create && r.refuse.CompareAndSwap(true, false) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"refused"}}`)), Request: req}, nil
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if !create || err != nil {
		return resp, err
	}
	if r.lose.CompareAndSwap(true, false) {
		_ = resp.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	r.duringMu.Lock()
	during := r.during
	r.during = nil
	r.duringMu.Unlock()
	if during != nil {
		done := make(chan struct{})
		go func() { defer close(done); during() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
		}
	}
	return resp, nil
}

// start is a process start with ACK_REACTION set to mode ("" leaves it
// unset). Everything but the fakes is rebuilt from disk.
func (r *round4Chat) start(mode string) {
	r.t.Helper()
	if r.s != nil {
		r.s.Close()
		r.watch.Close()
		l, err := ledger.Load(r.cfg.Endpoints.StateFile, true)
		if err != nil {
			r.t.Fatal(err)
		}
		r.l = l
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c, err := chat.New(context.Background(), &http.Client{Transport: r}, r.cfg.Endpoints)
	if err != nil {
		r.t.Fatal(err)
	}
	if err := c.SetIdentityUserID(fakegoogle.UserID); err != nil {
		r.t.Fatal(err)
	}
	service, watch, err := arrival.New(arrival.Options{Acknowledgement: ack.Options{Mode: mode, Catalog: ack.Catalog}, StateDir: r.state, Log: func(string) {},
		Chat: func() *chat.Client { return c }})
	if err != nil {
		r.t.Fatal(err)
	}
	arrival.Wire(c, service, watch)
	r.t.Cleanup(watch.Close)
	engine, err := neversilent.New(r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	g := gcclient.New(r.gc.URL(), "examplecity")
	s, err := server.New(server.Options{Stopwatch: watch, Config: r.cfg, Ledger: r.l, LedgerMu: r.mu, LedgerPath: r.cfg.Endpoints.StateFile,
		SettingsPath: filepath.Join(r.dir, "config.toml"), Chat: c, GC: g, Logger: logger})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(s.Close)
	s.SetHealth(server.Health{State: "ready"})
	s.SetClients(c, g)
	s.SetNeverSilent(engine)
	p, err := inbound.New(inbound.Options{Ack: service, NeverSilent: engine, Config: r.cfg, Ledger: r.l, LedgerMu: r.mu, LedgerPath: r.cfg.Endpoints.StateFile,
		GC: g, Chat: c, IdentityUserID: fakegoogle.UserID, Logger: logger, RetryDelay: time.Millisecond, Now: r.clock.Now})
	if err != nil {
		r.t.Fatal(err)
	}
	r.c, r.s, r.p, r.service, r.watch, r.engine = c, s, p, service, watch, engine
	r.mu.Lock()
	snapshot := r.l.Clone()
	r.mu.Unlock()
	if err := inbound.ReconcileNeverSilent(snapshot, engine); err != nil {
		r.t.Fatal(err)
	}
}

// human admits a new direct message from the owner, delivered to the
// session, and waits until the acknowledgement (when on) has finished.
func (r *round4Chat) human() string {
	r.t.Helper()
	r.seq++
	m := &api.Message{Name: fmt.Sprintf("%s/messages/m%d", round4Space, r.seq), Text: "fixture words", CreateTime: r.clock.Now().Format(time.RFC3339Nano),
		Space: &api.Space{Name: round4Space, SpaceType: "DIRECT_MESSAGE"}, Sender: &api.User{Name: "users/456", DisplayName: "Reader", Type: "HUMAN"}}
	r.google.Configure(func(g *fakegoogle.Server) { g.Messages[m.Name] = m })
	r.gc.Configure(func(s *fakegc.Server) {
		s.Script = []wire.Delivery{{Status: "delivered", Members: []wire.DeliveryMember{{SessionID: "example-session", Status: "delivered"}}}}
	})
	out, err := r.p.Handle(context.Background(), &puller.Event{PubSubID: "event-" + m.Name, Changes: []puller.Change{{Kind: "message.created", Message: m, Space: round4Space}}})
	if out != puller.Ack || err != nil {
		r.t.Fatalf("Handle = %v, %v", out, err)
	}
	if r.service.Choice.Enabled() {
		// The space lookup before the claim runs in a goroutine the service
		// does not track; the claim's marker says it has reached the service.
		marker := filepath.Join(r.state, "ack-attempts", fmt.Sprintf("%x", sha256.Sum256([]byte(round4Space+"\x00"+m.Name))))
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				r.t.Fatal("the acknowledgement never claimed the message")
			}
			time.Sleep(5 * time.Millisecond)
		}
		r.service.Wait()
	}
	return m.Name
}

// react is the session's reaction through the react verb.
func (r *round4Chat) react(message, emoji string) (int, string) {
	r.t.Helper()
	body, _ := json.Marshal(map[string]any{"message": message, "emoji": emoji, "session_id": "example-session"})
	w := httptest.NewRecorder()
	r.s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(string(body))))
	return w.Code, strings.TrimSpace(w.Body.String())
}

// reactions lists the reactions on the fake, as message:emoji.
func (r *round4Chat) reactions() []string {
	var out []string
	r.google.Configure(func(g *fakegoogle.Server) {
		for name, x := range g.Reactions {
			out = append(out, strings.Split(name, "/reactions/")[0]+":"+x.Emoji.Unicode)
		}
	})
	sort.Strings(out)
	return out
}

// acts is the number of session acts never-silent holds.
func (r *round4Chat) acts() int {
	var state struct {
		Acts []json.RawMessage `json:"acts"`
	}
	data, _ := os.ReadFile(r.opts.Path)
	_ = json.Unmarshal(data, &state)
	return len(state.Acts)
}

// ackFiles lists what the acknowledgement keeps on disk under the state directory.
func (r *round4Chat) ackFiles() []string {
	var out []string
	_ = filepath.Walk(r.state, func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() && strings.HasPrefix(filepath.Base(filepath.Dir(path)), "ack-") {
			rel, _ := filepath.Rel(r.state, path)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

// due moves the test clock three minutes on, writes fresh session_down health
// and polls; it returns the owner lines on the fake.
func (r *round4Chat) due() int {
	r.t.Helper()
	r.clock.Add(3 * time.Minute)
	stamp := r.clock.Now().Format(time.RFC3339)
	if err := os.WriteFile(r.health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := r.engine.Poll(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	lines := 0
	r.google.Configure(func(g *fakegoogle.Server) {
		for _, m := range g.Messages {
			if m.Text == neversilent.DownLine {
				lines++
			}
		}
	})
	return lines
}

func TestRound4SessionReactionCountsAsWithAckOff(t *testing.T) {
	// THE CLASS: the acknowledgement places eyes, the session reacts eyes
	// through the react verb (Google: 409), a restart, three minutes,
	// session_down: no owner line, and the react verb answers 200.
	t.Run("class/ack-eyes-session-eyes-restart", func(t *testing.T) {
		r := newRound4Chat(t)
		r.start("eyes")
		message := r.human()
		if got, want := strings.Join(r.reactions(), ","), message+":"+round4Eyes; got != want {
			t.Fatalf("reactions=%q, want the acknowledgement %q", got, want)
		}
		if code, body := r.react(message, round4Eyes); code != http.StatusOK {
			t.Errorf("react status=%d %s, want 200: the acknowledgement's eyes is there, and the session's eyes is its answer", code, body)
		}
		if r.acts() != 1 {
			t.Errorf("never-silent acts=%d after the session's eyes, want 1", r.acts())
		}
		r.start("eyes")
		if lines := r.due(); lines != 0 {
			t.Errorf("owner lines=%d after the session answered with eyes and a restart, want 0", lines)
		}
	})

	// The control: the same acknowledgement and restart with the session
	// silent. The owner line IS posted, through PostNoticeWithClient.
	t.Run("class/silent-session-restart-control", func(t *testing.T) {
		r := newRound4Chat(t)
		r.start("eyes")
		r.human()
		r.start("eyes")
		if lines := r.due(); lines != 1 {
			t.Errorf("owner lines=%d with the session silent, want 1 (the acknowledgement is no answer)", lines)
		}
	})

	// (b) THE SWITCH UNSET: a run with the acknowledgement on leaves its files
	// on disk; the next start has ACK_REACTION unset. The session's eyes gets
	// Google's 409: main's answer, 502 and no act, and the owner line follows.
	// Nothing the acknowledgement left is read or removed.
	t.Run("switch-unset/old-ack-files-on-disk", func(t *testing.T) {
		r := newRound4Chat(t)
		r.start("eyes")
		message := r.human()
		before := r.ackFiles()
		if len(before) == 0 {
			t.Fatal("the acknowledgement left no file on disk to test with")
		}
		r.start("")
		if r.service.Choice.Enabled() {
			t.Fatal("ACK_REACTION is still on")
		}
		if code, body := r.react(message, round4Eyes); code != http.StatusBadGateway {
			t.Errorf("react status=%d %s, want main's 502", code, body)
		}
		if r.acts() != 0 {
			t.Errorf("never-silent acts=%d, want 0 (main's rule with the switch unset)", r.acts())
		}
		if got := r.ackFiles(); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Errorf("acknowledgement files %q became %q with the switch unset", before, got)
		}
		r.start("")
		if lines := r.due(); lines != 1 {
			t.Errorf("owner lines=%d, want 1 (main's rule with the switch unset)", lines)
		}
	})

	// Review M1: Google created the acknowledgement but its answer was lost.
	// The session's eyes still counts, once.
	t.Run("lost-answer/ack-placed-answer-lost-session-eyes-twice", func(t *testing.T) {
		r := newRound4Chat(t)
		r.start("eyes")
		r.lose.Store(true)
		message := r.human()
		if got, want := strings.Join(r.reactions(), ","), message+":"+round4Eyes; got != want {
			t.Fatalf("reactions=%q, want the acknowledgement %q (placed, answer lost)", got, want)
		}
		if code, body := r.react(message, round4Eyes); code != http.StatusOK {
			t.Errorf("first react status=%d %s, want 200 (the eyes already there is the acknowledgement's)", code, body)
		}
		if r.acts() != 1 {
			t.Errorf("after the session's first eyes: acts=%d, want 1", r.acts())
		}
		r.start("eyes")
		if code, body := r.react(message, round4Eyes); code != http.StatusBadGateway {
			t.Errorf("second react status=%d %s, want main's 502 for a repeat", code, body)
		}
		if r.acts() != 1 {
			t.Errorf("after the session's second eyes: acts=%d, want 1 (a repeat is no new answer)", r.acts())
		}
	})

	// Review M1, the same window: the session's eyes reaches Google after the
	// acknowledgement's eyes is created and before Google's answer returns.
	// It counts, once, and the session's next eyes is a repeat.
	t.Run("in-flight/session-eyes-while-ack-in-flight-then-again", func(t *testing.T) {
		r := newRound4Chat(t)
		r.start("eyes")
		next := fmt.Sprintf("%s/messages/m%d", round4Space, r.seq+1)
		var inFlight int
		r.duringMu.Lock()
		r.during = func() {
			body, _ := json.Marshal(map[string]any{"message": next, "emoji": round4Eyes, "session_id": "example-session"})
			w := httptest.NewRecorder()
			r.s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(string(body))))
			inFlight = w.Code
		}
		r.duringMu.Unlock()
		message := r.human()
		if message != next {
			t.Fatalf("message=%q, want %q", message, next)
		}
		if inFlight != http.StatusOK {
			t.Errorf("the in-flight react status=%d, want 200", inFlight)
		}
		if r.acts() != 1 {
			t.Errorf("after the in-flight eyes: acts=%d, want 1", r.acts())
		}
		if code, body := r.react(message, round4Eyes); code != http.StatusBadGateway {
			t.Errorf("second react status=%d %s, want main's 502 for a repeat", code, body)
		}
		if r.acts() != 1 {
			t.Errorf("after the session's second eyes: acts=%d, want 1 (a repeat is no new answer)", r.acts())
		}
	})

	// P1, the repeats: each second reaction is the session's own repeat and
	// counts no more than it would with the acknowledgement off.
	for _, tc := range []struct {
		name, first string
		refuseAck   bool
	}{
		{"repeat/ack-eyes-session-eyes-twice", round4Eyes, false},
		{"repeat/ack-eyes-session-thumbsup-twice", round4Thumb, false},
		{"repeat/ack-refused-session-eyes-twice", round4Eyes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRound4Chat(t)
			r.start("eyes")
			r.refuse.Store(tc.refuseAck)
			message := r.human()
			if tc.refuseAck && len(r.reactions()) != 0 {
				t.Fatalf("the refused acknowledgement placed %q", r.reactions())
			}
			if code, body := r.react(message, tc.first); code != http.StatusOK {
				t.Errorf("first react status=%d %s, want 200", code, body)
			}
			if r.acts() != 1 {
				t.Errorf("after the session's first %s: acts=%d, want 1 (it counts)", tc.first, r.acts())
			}
			r.start("eyes")
			if code, body := r.react(message, tc.first); code != http.StatusBadGateway {
				t.Errorf("second react status=%d %s, want main's 502 for a repeat", code, body)
			}
			if r.acts() != 1 {
				t.Errorf("after the session's second %s: acts=%d, want 1 (a repeat is no new answer)", tc.first, r.acts())
			}
		})
	}
}

// THE GUARD that stays: with no acknowledgement on the message, the
// session's own repeated reaction is main's answer (502) and no second act.
// No test of main pins this on Google Chat; this one does, beside the class.
func TestRound4RepeatedSessionReactionWithoutAckIsNoNewAnswer(t *testing.T) {
	r := newRound4Chat(t)
	r.start("")
	message := r.human()
	if code, body := r.react(message, round4Eyes); code != http.StatusOK {
		t.Fatalf("first react status=%d %s", code, body)
	}
	if r.acts() != 1 {
		t.Fatalf("acts=%d after the first reaction, want 1", r.acts())
	}
	if code, body := r.react(message, round4Eyes); code != http.StatusBadGateway {
		t.Errorf("second react status=%d %s, want main's 502", code, body)
	}
	if r.acts() != 1 {
		t.Errorf("acts=%d after the repeat, want 1", r.acts())
	}
}
