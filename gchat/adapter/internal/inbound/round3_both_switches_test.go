package inbound

// PR 104 round 3, item 4 (Google Chat): ACK_REACTION and NEVER_SILENT both on.
//
// R4: the reaction the adapter places by itself (the acknowledgement) never
// counts as words or a reaction from the addressed session. With the session
// silent and main's never-silent conditions for session_down met, the owner
// line is posted.
// R5: the owner line is never the mayor's words. It writes no stopwatch words
// line, and its echo (Google delivers the identity's own message back) gets no
// acknowledgement reaction.
//
// The arrival service and the stopwatch are built and wired as the start does
// (arrival.New, arrival.Wire, Options.Ack; see offparity_hook_test.go). The
// notice is posted as server.PostNoticeWithClient posts it: CreateNotice on the
// client the adapter shares (the server package imports this one, so its
// method cannot be called from here).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ack"
	"gchat-adapter/internal/arrival"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/stopwatch"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestBothSwitchesOnAckIsNoAnswerAndNoticeIsNoWords(t *testing.T) {
	rf := retrySetup(t)
	dir := t.TempDir()
	health := filepath.Join(dir, "health.json")
	var notices []string
	ns, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(dir, "never-silent.json"), HealthFile: health, City: "example-city", Channel: "gchat", Now: rf.clock.Now,
		Post: func(ctx context.Context, m neversilent.Message, line string) error {
			name, _, err := rf.opts.Chat.CreateNotice(ctx, m.Conversation, line, m.Thread, false, m.CanPost)
			if err == nil {
				notices = append(notices, name)
			}
			return err
		}})
	if err != nil {
		t.Fatal(err)
	}
	rf.opts.NeverSilent = ns
	c := rf.opts.Chat
	state := filepath.Join(dir, "state")
	service, watch, err := arrival.New(arrival.Options{Acknowledgement: ack.Options{Mode: "eyes", Catalog: ack.Catalog}, StateDir: state, Log: func(string) {},
		Chat: func() *chat.Client { return c }})
	if err != nil {
		t.Fatal(err)
	}
	arrival.Wire(c, service, watch)
	t.Cleanup(watch.Close)
	rf.opts.Ack = service
	rf.rebuild(t)
	settle := func() {
		// The space lookup runs in a goroutine the service does not track.
		time.Sleep(200 * time.Millisecond)
		service.Wait()
		watch.Flush()
	}
	reactions := func() []string {
		var out []string
		rf.google.Configure(func(s *fakegoogle.Server) {
			for name, r := range s.Reactions {
				out = append(out, strings.Split(name, "/reactions/")[0]+":"+r.Emoji.Unicode)
			}
		})
		return out
	}

	// (a) A new direct message from a human, delivered, gets the
	// acknowledgement.
	m := message()
	m.Space.SpaceType = "DIRECT_MESSAGE"
	m.CreateTime = rf.clock.Now().Format(time.RFC3339Nano)
	rf.google.Configure(func(s *fakegoogle.Server) { s.Spaces[space] = m.Space; s.Messages[m.Name] = m })
	rf.script(wire.Delivery{Status: "delivered", Members: []wire.DeliveryMember{{SessionID: "example-session", Status: "delivered"}}})
	handle(t, rf.fixture, event("example-event", "message.created", m))
	settle()
	if got, want := reactions(), []string{messageName + ":\U0001F440"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("acknowledgements=%q, want %q", got, want)
	}
	// Google sends the acknowledgement back as a reaction event by the
	// identity. It is not the session's reaction either.
	var ackName string
	rf.google.Configure(func(s *fakegoogle.Server) {
		for name := range s.Reactions {
			ackName = name
		}
	})
	handle(t, rf.fixture, &puller.Event{PubSubID: "example-ack-echo", Changes: []puller.Change{{Kind: "reaction.created", Space: space,
		Reaction: &api.Reaction{Name: ackName, User: &api.User{Name: rf.p.identity()}, Emoji: &api.Emoji{Unicode: "\U0001F440"}}}}})
	settle()
	// The session sends no words and no reaction. Three minutes pass and
	// fresh health says session_down: the owner line is posted.
	rf.clock.Add(3 * time.Minute)
	stamp := rf.clock.Now().Format(time.RFC3339)
	if err := os.WriteFile(health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ns.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 {
		t.Fatalf("owner lines=%d (R4: the acknowledgement must not count as the session's answer)", len(notices))
	}
	var notice *api.Message
	rf.google.Configure(func(s *fakegoogle.Server) {
		if n := s.Messages[notices[0]]; n != nil {
			copied := *n
			notice = &copied
		}
	})
	if notice == nil || notice.Text != neversilent.DownLine {
		t.Fatalf("notice=%+v", notice)
	}

	// (b) Google delivers the identity's notice back as a new message.
	notice.Sender = &api.User{Name: rf.p.identity(), Type: "HUMAN"}
	notice.Space = &api.Space{Name: space, SpaceType: "DIRECT_MESSAGE"}
	handle(t, rf.fixture, event("example-echo", "message.created", notice))
	settle()
	if got := reactions(); len(got) != 1 {
		t.Errorf("R5: acknowledgements=%q, want only the human message's", got)
	}
	files, _ := filepath.Glob(filepath.Join(state, "stopwatch-*.jsonl"))
	var words, reacted []string
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			var e stopwatch.Event
			if line == "" || json.Unmarshal([]byte(line), &e) != nil {
				continue
			}
			switch e.Event {
			case "words":
				words = append(words, e.Message)
			case "reaction":
				reacted = append(reacted, e.Message+":"+e.Actor)
			}
		}
	}
	if len(words) != 0 {
		t.Errorf("R5: the owner line wrote stopwatch words lines %q", words)
	}
	if strings.Join(reacted, ",") != messageName+":adapter" {
		t.Errorf("reaction lines=%q, want the acknowledgement's only", reacted)
	}
	if len(notices) != 1 {
		t.Errorf("owner lines=%d after the echo", len(notices))
	}
}
