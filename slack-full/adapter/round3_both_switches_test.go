package main

// PR 104 round 3, item 4: ACK_REACTION and NEVER_SILENT both on.
//
// R4: the reaction the adapter places by itself (the acknowledgement) never
// counts as words or a reaction from the addressed session. With the session
// silent and main's never-silent conditions for session_down met, the owner
// line is posted.
// R5: the owner line is never the mayor's words. Its echo writes no stopwatch
// words line and gets no acknowledgement reaction. The switchboard posts the
// line with its own token, so the echo is the switchboard bot's message. On a
// company route the line is posted with the target agent's own bot token
// (never_silent.go, newNeverSilent's Post), so its echo comes back through
// company ingress as that agent's bot post: the shape of a confirmed company
// reply, which the stopwatch times as words (stopwatch_company.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/ack"
	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
	"github.com/sjarmak/gc-slack-adapter/internal/stopwatch"
)

// bothSwitchesSlack stands in for Slack's API: it records every reactions.add.
type bothSwitchesSlack struct {
	mu        sync.Mutex
	reactions []string
}

func (s *bothSwitchesSlack) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if strings.HasSuffix(r.URL.Path, "/reactions.add") {
		var req slackReactionsAddReq
		_ = json.Unmarshal(body, &req)
		s.mu.Lock()
		s.reactions = append(s.reactions, req.Channel+"/"+req.Timestamp+":"+req.Name)
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (s *bothSwitchesSlack) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reactions...)
}

func bothSwitchesEvents(t *testing.T, watch *stopwatch.Writer, dir string) []stopwatch.Event {
	t.Helper()
	watch.Flush()
	files, _ := filepath.Glob(filepath.Join(dir, "stopwatch-*.jsonl"))
	var out []stopwatch.Event
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var e stopwatch.Event
			if line != "" && json.Unmarshal([]byte(line), &e) == nil {
				out = append(out, e)
			}
		}
	}
	return out
}

func TestBothSwitchesOnAckIsNoAnswerAndNoticeIsNoWords(t *testing.T) {
	for _, route := range []string{"switchboard", "room", "dm", "mpim"} {
		t.Run(route, func(t *testing.T) {
			slack := &bothSwitchesSlack{}
			stub := httptest.NewServer(http.HandlerFunc(slack.serve))
			defer stub.Close()
			withSlackAPIStub(t, stub)

			now := time.Now()
			dir := t.TempDir()
			health := filepath.Join(dir, "health.json")
			type post struct {
				m    neversilent.Message
				line string
			}
			var posts []post
			e, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "never-silent.json"), HealthFile: health, Now: func() time.Time { return now },
				Post: func(_ context.Context, m neversilent.Message, line string) error {
					posts = append(posts, post{m, line})
					return nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			choice, err := ack.New(ack.Options{Mode: "eyes", Catalog: ack.Catalog})
			if err != nil {
				t.Fatal(err)
			}
			watchDir := filepath.Join(dir, "state")
			watch := stopwatch.New(watchDir, "slack", "release-example", nil)
			t.Cleanup(watch.Close)
			service := &ack.Service{Choice: choice, Dir: watchDir,
				Inbound: func(in ack.Input) {
					watch.Record(stopwatch.Event{Event: "inbound", Conversation: in.Conversation, Message: in.Message, Thread: in.Thread, MessageAt: in.Time, At: in.Seen})
				},
				Reaction: func(in ack.Input, how string, at time.Time) {
					watch.Record(stopwatch.Event{Event: "reaction", Conversation: in.Conversation, Message: in.Message, At: at, Actor: "adapter", Choice: how})
				},
			}

			human := slackTimestamp(now)
			var channel string
			var admitHuman, admitEcho func(ts, thread, text string)
			var settle, ackEcho func()
			switch route {
			case "switchboard":
				channel = "DEXAMPLE"
				gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"delivery":{"status":"delivered","members":[{"session_id":"example-session","status":"delivered"}]}}`)
				}))
				defer gc.Close()
				cfg := busyTestConfig(gc.URL)
				cfg.busyReaction = ""
				cfg.neverSilent = e
				cfg.inboundJournal = &inboundJournal{watch: watch}
				cfg.arrivalStarted = time.Now().UTC()
				service.Send = slackArrivalSender(cfg.slackBotToken)
				cfg.arrival = service
				aliases := newTestHandleAliasRegistry(t)
				event := func(id string, ev slackMessageEvent) {
					raw, _ := json.Marshal(ev)
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", EventID: id, Event: raw, receivedAt: time.Now(),
						Authorizations: []slackEventAuthorization{{UserID: "U0BOTSELF", IsBot: true}}}, func() {})
				}
				admitHuman = func(ts, _, text string) {
					event("event-human", slackMessageEvent{Type: "message", ChannelType: "im", Channel: channel, User: "U0AAAAAA1", TS: ts, Text: text})
				}
				admitEcho = func(ts, thread, text string) {
					event("event-echo", slackMessageEvent{Type: "message", Subtype: "bot_message", ChannelType: "im", Channel: channel, User: "U0BOTSELF", BotID: "B0SELF", TS: ts, ThreadTS: thread, Text: text})
				}
				// Slack sends the acknowledgement back as reaction_added by
				// the bot. It is not the session's reaction either.
				ackEcho = func() {
					raw := fmt.Sprintf(`{"type":"reaction_added","user":"U0BOTSELF","reaction":"eyes","item":{"type":"message","channel":%q,"ts":%q},"event_ts":%q}`, channel, human, slackTimestamp(time.Now()))
					processSlackEvent(cfg, aliases, nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", EventID: "event-ack-echo", Event: []byte(raw), receivedAt: time.Now(),
						Authorizations: []slackEventAuthorization{{UserID: "U0BOTSELF", IsBot: true}}}, func() {})
				}
				settle = func() { time.Sleep(300 * time.Millisecond); service.Wait() }
			default:
				h, _, _, _ := setupMpim(t)
				bf := baseBindingsFile()
				bf.Bindings[0].Session = "mayor"
				bindings, _, err := ParseCompanyBindings(marshalBindings(t, bf), h.dirStore.Snapshot())
				if err != nil {
					t.Fatal(err)
				}
				h.bindStore.set(bindings)
				h.gw.dmBindStore = loadDMBindingsStore(t, h, dmBindingsFile{SchemaVersion: 1, DMBindings: []DMBinding{{Agent: "ollie", Session: "mayor"}}})
				h.gw.now = func() time.Time { return now }
				h.gw.cfg.neverSilent = e
				h.gw.cfg.inboundJournal = &inboundJournal{watch: watch}
				h.gw.cfg.arrivalStarted = time.Now().UTC()
				h.gw.cfg.arrival = service
				// The agent's bot, as TestCompanyConfirmedPostsAreTimed resolves it.
				h.gw.authors = fakeAuthorResolver{byBot: map[string]companyBotInfo{"bot-example": {UserID: "U0AAAAAA1", AppID: ollieAppID}}}
				admit := func(ev slackMessageEvent) {
					switch route {
					case "dm":
						ev.Channel, ev.ChannelType = testDMChannel, "im"
						admitDMViaHandler(t, h, ev, ollieAppID, 0)
					case "mpim":
						ev.Channel, ev.ChannelType = testMpimChannel, "mpim"
						admitMpimViaHandler(t, h, ev, ollieAppID, 0)
					default:
						ev.Channel = testChannelID
						h.admitViaHandler(t, ev, 0)
					}
					h.wait()
				}
				channel = map[string]string{"room": testChannelID, "dm": testDMChannel, "mpim": testMpimChannel}[route]
				admitHuman = func(ts, _, text string) {
					admit(slackMessageEvent{Type: "message", User: "U0HUMAN01", TS: ts, Text: text})
				}
				admitEcho = func(ts, thread, text string) {
					admit(slackMessageEvent{Type: "message", Subtype: "bot_message", BotID: "bot-example", User: "U0AAAAAA1", AppID: ollieAppID, TS: ts, ThreadTS: thread, Text: text})
				}
				settle = func() { time.Sleep(300 * time.Millisecond); service.Wait() }
			}

			// (a) A delivered, addressed message gets the acknowledgement.
			admitHuman(human, "", "<@U0AAAAAA1> example request")
			deadline := time.Now().Add(5 * time.Second)
			for len(slack.list()) == 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			settle()
			if got, want := slack.list(), []string{channel + "/" + human + ":eyes"}; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("acknowledgements=%q, want %q", got, want)
			}
			if ackEcho != nil {
				ackEcho()
				settle()
			}
			// The session sends no words and no reaction. Three minutes pass
			// and fresh health says session_down: the owner line is posted.
			now = now.Add(3 * time.Minute)
			stamp := now.Format(time.RFC3339)
			if err := os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := e.Poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(posts) != 1 || posts[0].line != neversilent.DownLine || posts[0].m.Conversation != channel {
				t.Fatalf("owner lines=%d (R4: the acknowledgement must not count as the session's answer)", len(posts))
			}

			// (b) The line's echo: no words line, no acknowledgement.
			thread := ""
			if route == "room" {
				thread = posts[0].m.Thread
			}
			echo := slackTimestamp(time.Now())
			admitEcho(echo, thread, posts[0].line)
			settle()
			events := bothSwitchesEvents(t, watch, watchDir)
			var words, reactions []string
			for _, ev := range events {
				switch ev.Event {
				case "words":
					words = append(words, ev.Message)
				case "reaction":
					reactions = append(reactions, fmt.Sprintf("%s:%s", ev.Message, ev.Actor))
				}
			}
			if len(words) != 0 {
				t.Errorf("R5: the owner line's echo wrote stopwatch words lines %q", words)
			}
			if got := slack.list(); len(got) != 1 {
				t.Errorf("R5: acknowledgements=%q, want only the human message's", got)
			}
			if strings.Join(reactions, ",") != human+":adapter" {
				t.Errorf("reaction lines=%q, want the acknowledgement's only", reactions)
			}
			if len(posts) != 1 {
				t.Errorf("owner lines=%d after the echo", len(posts))
			}
		})
	}
}
