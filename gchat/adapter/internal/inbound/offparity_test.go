package inbound

// THE OFF-PARITY TEST (PR 104 round 2, item 11). With ACK_REACTION unset the
// adapter must behave as it does at the PR's base, c9969436. This file runs
// byte for byte at the base, at the PR head and at the tip. It names only
// symbols that exist at the base; everything wired after the base (the
// arrival service, the stopwatch writer) is reached through ONE hook,
// offparity_hook_test.go, which does nothing at the base. The expected values
// below are what the base produces; the base run proves them.
//
// The only differences allowed, each named with its contract item:
//   - the stopwatch's own files, stopwatch-YYYY-MM.jsonl (item 15);
//   - the stopwatch's own warn-once lines (item 19), which this healthy
//     script never prints;
//   - Google Chat's status field jev_api_key (item 10), which lives in the
//     server package and is not observed here.
//
// The script drives the processor through New with the Options as the
// adapter builds them (app/workers.go), the fake gc, and a fake Google
// Chat behind a transport that counts every request. The mayor's post and
// react verbs run through the same chat.Client calls the server's publish
// and react handlers make: the server package imports this one, so its
// handlers cannot be driven from here; at the head the react handler adds
// only a stopwatch reaction line (server.go:1320-1322), an allowed file.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

type offParityCalls struct {
	mu    sync.Mutex
	calls map[string]int
	next  http.RoundTripper
}

func (c *offParityCalls) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.calls[r.Method+" "+r.URL.Path]++
	c.mu.Unlock()
	return c.next.RoundTrip(r)
}

func (c *offParityCalls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k, v := range c.calls {
		out = append(out, k+" x"+itoa(v))
	}
	sort.Strings(out)
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

type offParityLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *offParityLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *offParityLog) lines(dir string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		if line == "" || strings.Contains(line, "stopwatch:") { // item 19's warn-once lines
			continue
		}
		out = append(out, strings.ReplaceAll(line, dir, "<state>"))
	}
	return out
}

func offParityListing(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." || regexp.MustCompile(`^stopwatch-\d{4}-\d{2}\.jsonl$`).MatchString(rel) { // item 15
			return nil
		}
		if info.IsDir() {
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func offParityCompare(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("OFF-PARITY %s differs from the base:\n got: %q\nwant: %q", what, got, want)
	}
}

func TestOffParityWithTheBase(t *testing.T) {
	g := fakegoogle.New(t, nil)
	gc := fakegc.New(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity"}, Endpoints: config.Endpoints{
		ChatAPIBase: g.URL(), MediaDir: filepath.Join(dir, "media"), MediaMaxBytes: 1024, StateFile: filepath.Join(dir, "state.json"),
	}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "mayor"}}
	cfg.Settings.Defaults.Wake = config.WakeMentions
	calls := &offParityCalls{calls: map[string]int{}, next: http.DefaultTransport}
	c, err := chat.New(context.Background(), &http.Client{Transport: calls}, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	const room, dm, mystery = "spaces/room", "spaces/dm", "spaces/mystery"
	now := time.Now().UTC()
	at := func(i int) string { return now.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano) }
	human := func() *api.User { return &api.User{Name: "users/456", DisplayName: "Reader", Type: "HUMAN"} }
	mine := &api.Message{Name: room + "/messages/mine", Text: "the mayor wrote this", CreateTime: at(0), Space: &api.Space{Name: room, SpaceType: "SPACE"}, Sender: &api.User{Name: identity, Type: "HUMAN"}, Thread: &api.Thread{Name: room + "/threads/mine"}}
	g.Configure(func(s *fakegoogle.Server) {
		s.Spaces[room] = &api.Space{Name: room, SpaceType: "SPACE"}
		s.Spaces[dm] = &api.Space{Name: dm, SpaceType: "DIRECT_MESSAGE"}
		s.Messages[mine.Name] = mine
	})
	var logs offParityLog
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
		if a.Key == slog.TimeKey {
			return slog.Attr{}
		}
		return a
	}}))
	opts := Options{Config: cfg, Ledger: ledger.New(), LedgerMu: &sync.Mutex{}, LedgerPath: cfg.Endpoints.StateFile,
		GC: gcclient.New(gc.URL(), "examplecity"), Chat: c, IdentityUserID: identity, Logger: logger, RetryDelay: time.Millisecond}
	// The hook wires what the adapter's start adds after the base, with
	// ACK_REACTION unset; at the base it does nothing.
	wait := offParityWire(t, &opts, dir, func(s string) { logger.Warn(s) })
	p, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	step := func(id string, changes ...puller.Change) {
		t.Helper()
		if out, err := p.Handle(context.Background(), &puller.Event{PubSubID: id, Changes: changes}); out != puller.Ack || err != nil {
			t.Fatalf("%s: Handle = %v, %v", id, out, err)
		}
	}
	created := func(m *api.Message) puller.Change {
		return puller.Change{Kind: "message.created", Message: m, Space: m.Space.Name}
	}
	reaction := func(name, user string) puller.Change {
		return puller.Change{Kind: "reaction.created", Space: room, Reaction: &api.Reaction{Name: name, User: &api.User{Name: user}, Emoji: &api.Emoji{Unicode: "👍"}}}
	}
	direct := &api.Message{Name: dm + "/messages/one", Text: "a direct message", CreateTime: at(1), Space: &api.Space{Name: dm, SpaceType: "DIRECT_MESSAGE"}, Sender: human(), Thread: &api.Thread{Name: dm + "/threads/one"}}
	mention := &api.Message{Name: room + "/messages/mention", Text: "hello @Example", CreateTime: at(2), Space: &api.Space{Name: room, SpaceType: "SPACE"}, Sender: human(), Thread: &api.Thread{Name: room + "/threads/mention"},
		Annotations: []*api.Annotation{{Type: "USER_MENTION", StartIndex: 6, Length: 8, UserMention: &api.UserMentionMetadata{User: &api.User{Name: identity, DisplayName: "Example"}}}}}
	ambient := &api.Message{Name: room + "/messages/ambient", Text: "no mention here", CreateTime: at(3), Space: &api.Space{Name: room, SpaceType: "SPACE"}, Sender: human(), Thread: &api.Thread{Name: room + "/threads/ambient"}}
	bot := &api.Message{Name: dm + "/messages/bot", Text: "a bot speaks", CreateTime: at(4), Space: &api.Space{Name: dm, SpaceType: "DIRECT_MESSAGE"}, Sender: &api.User{Name: "users/789", DisplayName: "Helper", Type: "BOT"}, Thread: &api.Thread{Name: dm + "/threads/bot"}}
	unknown := &api.Message{Name: mystery + "/messages/one", Text: "where am I", CreateTime: at(5), Space: &api.Space{Name: mystery}, Sender: human(), Thread: &api.Thread{Name: mystery + "/threads/one"}}

	g.Configure(func(s *fakegoogle.Server) {
		for _, m := range []*api.Message{direct, mention, ambient, bot, unknown} {
			s.Messages[m.Name] = m
		}
	})
	step("direct", created(direct))
	step("mention", created(mention))
	step("ambient", created(ambient))
	step("bot", created(bot))
	edited := *direct
	edited.Text, edited.LastUpdateTime = "a direct message, edited", at(10)
	step("edit", puller.Change{Kind: "message.updated", Message: &edited, Space: dm})
	step("delete", puller.Change{Kind: "message.deleted", Message: &api.Message{Name: mention.Name}, Space: room})
	step("human-on-mine", reaction(mine.Name+"/reactions/human", "users/456"))
	step("mine-on-mine", reaction(mine.Name+"/reactions/own", identity)) // case 1a
	step("mine-on-human", reaction(ambient.Name+"/reactions/echo", identity))
	step("unknown-kind", created(unknown)) // case 1h
	// The mayor's post and three react verbs (case 1g's calls).
	if _, _, err := c.CreateMessageWithAttachments(context.Background(), dm, "the mayor answers", direct.Thread.Name, "offparity-post", false, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := c.CreateReaction(context.Background(), direct.Name, "👍"); err != nil {
			t.Fatal(err)
		}
	}
	wait()

	var turns []string
	for _, m := range gc.Inbound() {
		turn := m.Conversation.ConversationID + " | " + m.Conversation.Kind + " | " + m.ProviderMessageID + " | " + m.Text
		turns = append(turns, strings.ReplaceAll(turn, edited.LastUpdateTime, "<edited-at>"))
	}
	opts.LedgerMu.Lock()
	var counters []string
	for k, v := range opts.Ledger.Counters {
		counters = append(counters, k+"="+itoa(int(v)))
	}
	unresolved := len(opts.Ledger.Unresolved)
	opts.LedgerMu.Unlock()
	sort.Strings(counters)
	counters = append(counters, "unresolved="+itoa(unresolved))

	t.Logf("turns=%#v", turns)
	t.Logf("calls=%#v", calls.list())
	t.Logf("counters=%#v", counters)
	t.Logf("logs=%#v", logs.lines(dir))
	t.Logf("listing=%#v", offParityListing(t, dir))
	offParityCompare(t, "turns raised", turns, offParityWantTurns)
	offParityCompare(t, "vendor calls", calls.list(), offParityWantCalls)
	offParityCompare(t, "counters", counters, offParityWantCounters)
	offParityCompare(t, "log lines", logs.lines(dir), offParityWantLogs)
	offParityCompare(t, "state directory", offParityListing(t, dir), offParityWantListing)
}

// What a start accepts with ACK_REACTION unset: an ACK_SCOPE the base never
// reads (case 1c), and an executable that cannot be hashed (case 1d).
func TestOffParityStartsAsTheBase(t *testing.T) {
	for _, tc := range []struct{ name, scope, executable string }{
		{"ACK_SCOPE=everyone (1c)", "everyone", ""},
		{"an executable that cannot be hashed (1d)", "", "no-such-executable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			env := filepath.Join(dir, "env")
			body := "GCHAT_PROJECT_ID=proj\nGCHAT_PUBSUB_TOPIC=projects/proj/topics/t\nGCHAT_PUBSUB_SUBSCRIPTION=projects/proj/subscriptions/s\nGCHAT_OAUTH_CLIENT_FILE=client.json\nGC_CITY_NAME=examplecity\nGC_API_BASE_URL=http://127.0.0.1:9\n"
			if tc.scope != "" {
				body += "ACK_SCOPE=" + tc.scope + "\n"
			}
			for _, key := range []string{"ACK_REACTION", "ACK_SCOPE", "JEV_API_KEY", "JEV_API_BASE_URL", "JEV_MODEL", "GC_SERVICE_STATE_ROOT"} {
				t.Setenv(key, "")
			}
			if err := os.WriteFile(env, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(env, "", dir)
			if err != nil {
				t.Fatalf("config: %v", err)
			}
			executable := ""
			if tc.executable != "" {
				executable = filepath.Join(dir, tc.executable)
			}
			if err := offParityStart(t, cfg, dir, executable, func(string) {}); err != nil {
				t.Errorf("OFF-PARITY start refused where the base starts: %v", err)
			}
		})
	}
}

// The base's observations of the script above (c9969436, base hooks).
var (
	offParityWantTurns = []string{
		"spaces/dm | dm | spaces/dm/messages/one | [from: Reader <users/456> | external | item gchat:spaces/dm/messages/one]\na direct message",
		"spaces/room | room | spaces/room/messages/mention | [from: Reader <users/456> | external | item gchat:spaces/room/messages/mention]\nhello Example",
		"spaces/dm | dm | spaces/dm/messages/bot | [from: Helper <users/789> | external, bot | item gchat:spaces/dm/messages/bot]\na bot speaks",
		"spaces/dm | dm | spaces/dm/messages/one#updated#<edited-at> | [from: Reader <users/456> | external | item gchat:spaces/dm/messages/one]\nedited: a direct message, edited",
		"spaces/room | room | spaces/room/messages/mention#deleted | [from: Reader <users/456> | external | item gchat:spaces/room/messages/mention]\ndeleted",
		"spaces/room | room | spaces/room/messages/mine/reactions/human#created | [from: <users/456> | external]\nreaction created: 👍 on spaces/room/messages/mine",
		// Case 1a: the identity's reaction on its own message is a turn.
		"spaces/room | room | spaces/room/messages/mine/reactions/own#created | [from: <users/123> | external]\nreaction created: 👍 on spaces/room/messages/mine",
	}
	offParityWantCalls = []string{
		// Case 1h: one ListSpaces, the handler's own; none from the arrival path.
		"GET /v1/spaces x1",
		"GET /v1/spaces/room/messages/mine x1",
		"POST /v1/spaces/dm/messages x1",
		"POST /v1/spaces/dm/messages/one/reactions x3",
	}
	offParityWantCounters = []string{"turns:spaces/dm=3", "turns:spaces/room=4", "unavailable:spaces/mystery=1", "unresolved=0"}
	offParityWantLogs     = []string{
		"level=WARN msg=\"inbound change retained\" space=spaces/mystery change=spaces/mystery/messages/one message=spaces/mystery/messages/one cause=\"space unavailable\"",
	}
	offParityWantListing = []string{"state.json"}
)
