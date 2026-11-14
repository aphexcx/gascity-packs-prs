package main

// PR 104 round 4, R2 (Slack): with ACK_REACTION and NEVER_SILENT both on, a
// session's reaction counts for never-silent exactly as it would with the
// acknowledgement off. The acknowledgement never adds an answer and never
// removes one.
//
// On the switchboard the acknowledgement (arrival.go) and the react verb
// (handleReact) use the same token, so Slack answers the session's reaction
// of the emoji the acknowledgement placed with already_reacted. Main's rule
// takes already_reacted as no new answer (TestNeverSilentRepeatedReactionIsNotNewActivity),
// which is right for the session's own repeat and wrong when the reaction
// already there is the acknowledgement's.
//
// The rig is the production start: configureArrival and startArrival build the
// arrival service, processSlackEvent admits the owner's message, handleReact
// is the react verb. A local Slack stub answers already_reacted to a second
// identical reactions.add by the same token. A restart is a new engine from
// its state file and reconcileNeverSilent, as main's test does.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

// round4Slack stands in for Slack's reactions API: one reaction per token,
// message and name, as Slack keeps them.
type round4Slack struct {
	mu     sync.Mutex
	placed map[string]bool
	refuse bool // refuse the next reactions.add, placing nothing
	adds   []string
}

func (s *round4Slack) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	var req slackReactionsAddReq
	_ = json.Unmarshal(body, &req)
	key := r.Header.Get("Authorization") + "\x00" + req.Channel + "\x00" + req.Timestamp + "\x00" + req.Name
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/reactions.add"):
		s.adds = append(s.adds, req.Name)
		if s.refuse {
			s.refuse = false
			_, _ = io.WriteString(w, `{"ok":false,"error":"too_many_reactions"}`)
			return
		}
		if s.placed[key] {
			_, _ = io.WriteString(w, `{"ok":false,"error":"already_reacted"}`)
			return
		}
		s.placed[key] = true
	case strings.HasSuffix(r.URL.Path, "/reactions.remove"):
		if !s.placed[key] {
			_, _ = io.WriteString(w, `{"ok":false,"error":"no_reaction"}`)
			return
		}
		delete(s.placed, key)
	}
	_, _ = io.WriteString(w, `{"ok":true}`)
}

func (s *round4Slack) addCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.adds)
}

type round4SlackRig struct {
	t       *testing.T
	slack   *round4Slack
	dir     string
	health  string
	now     time.Time
	posts   int
	opts    neversilent.Options
	cfg     config
	aliases *handleAliasRegistry
	gc      string
	state   string
	journal string
}

func newRound4SlackRig(t *testing.T) *round4SlackRig {
	t.Helper()
	r := &round4SlackRig{t: t, slack: &round4Slack{placed: map[string]bool{}}, dir: t.TempDir(), now: time.Now()}
	stub := httptest.NewServer(http.HandlerFunc(r.slack.serve))
	t.Cleanup(stub.Close)
	withSlackAPIStub(t, stub)
	gc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"delivery":{"status":"delivered","members":[{"session_id":"example-session","status":"delivered"}]}}`)
	}))
	t.Cleanup(gc.Close)
	r.gc = gc.URL
	r.health = filepath.Join(r.dir, "health.json")
	r.state = filepath.Join(r.dir, "state")
	r.journal = filepath.Join(r.dir, "journal")
	r.opts = neversilent.Options{Enabled: true, Path: filepath.Join(r.dir, "never-silent.json"), HealthFile: r.health, Now: func() time.Time { return r.now },
		Post: func(context.Context, neversilent.Message, string) error { r.posts++; return nil }}
	r.aliases = newTestHandleAliasRegistry(t)
	prev := arrivalExecutable
	missing := filepath.Join(r.dir, "missing-executable")
	arrivalExecutable = func() (string, error) { return missing, nil }
	t.Cleanup(func() { arrivalExecutable = prev })
	return r
}

// start is a process start with ACK_REACTION set to mode ("" leaves it
// unset): main's configureArrival and startArrival, a never-silent engine from
// its state file, and the reconcile a start runs.
func (r *round4SlackRig) start(mode string) {
	r.t.Helper()
	if r.cfg.stopArrival != nil {
		r.cfg.stopArrival()
	}
	cfg := busyTestConfig(r.gc)
	cfg.busyReaction = ""
	env := map[string]string{"ACK_REACTION": mode, "GC_SERVICE_STATE_ROOT": r.state}
	if err := configureArrival(&cfg, func(k string) string { return env[k] }); err != nil {
		r.t.Fatal(err)
	}
	cfg.inboundJournal = &inboundJournal{dir: r.journal}
	if err := startArrival(&cfg); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(cfg.stopArrival)
	e, err := neversilent.New(r.opts)
	if err != nil {
		r.t.Fatal(err)
	}
	cfg.neverSilent = e
	if err := cfg.reconcileNeverSilent(); err != nil {
		r.t.Fatal(err)
	}
	r.cfg = cfg
}

// human admits an owner's direct message to the switchboard and waits until
// never-silent holds it as delivered and the acknowledgement has finished.
func (r *round4SlackRig) human(ts string) {
	r.t.Helper()
	raw, _ := json.Marshal(slackMessageEvent{Type: "message", ChannelType: "im", Channel: "DEXAMPLE", User: "U0AAAAAA1", TS: ts, Text: "fixture words"})
	processSlackEvent(r.cfg, r.aliases, nil, nil, nil, nil, slackEventEnvelope{Type: "event_callback", EventID: "event-" + ts, Event: raw, receivedAt: time.Now(),
		Authorizations: []slackEventAuthorization{{UserID: "U0BOTSELF", IsBot: true}}}, func() {})
	deadline := time.Now().Add(10 * time.Second)
	for !r.delivered(ts) {
		if time.Now().After(deadline) {
			r.t.Fatalf("the message %s was not delivered", ts)
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.cfg.arrival.Wait()
}

func (r *round4SlackRig) delivered(ts string) bool {
	var state struct {
		Messages map[string]struct {
			Delivered time.Time `json:"delivered"`
		} `json:"messages"`
	}
	data, _ := os.ReadFile(r.opts.Path)
	_ = json.Unmarshal(data, &state)
	m, ok := state.Messages[neverSilentID("DEXAMPLE", ts)]
	return ok && !m.Delivered.IsZero()
}

// react is the session's reaction through the react verb; it returns the receipt.
func (r *round4SlackRig) react(ts, emoji string) reactReceipt {
	r.t.Helper()
	w := httptest.NewRecorder()
	handleReact(r.cfg)(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(`{"conversation":{"conversation_id":"DEXAMPLE"},"message_id":"`+ts+`","emoji":"`+emoji+`","session_id":"example-session"}`)))
	var receipt reactReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		r.t.Fatalf("receipt %q: %v", w.Body.String(), err)
	}
	return receipt
}

// acts is the number of session acts never-silent holds.
func (r *round4SlackRig) acts() int {
	var state struct {
		Acts []json.RawMessage `json:"acts"`
	}
	data, _ := os.ReadFile(r.opts.Path)
	_ = json.Unmarshal(data, &state)
	return len(state.Acts)
}

// journaled lists the journal's session_react rows as emoji:counted, in order.
func (r *round4SlackRig) journaled() []string {
	paths, _ := filepath.Glob(filepath.Join(r.journal, "*.jsonl"))
	sort.Strings(paths)
	var out []string
	for _, path := range paths {
		data, _ := os.ReadFile(path)
		for _, line := range strings.Split(string(data), "\n") {
			var row inboundJournalRecord
			if json.Unmarshal([]byte(line), &row) != nil || row.SessionAct == nil || row.SessionAct.Source != "session_react" {
				continue
			}
			counted := "counted"
			if row.SessionAct.NoticeNoActivity {
				counted = "not counted"
			}
			out = append(out, row.SessionAct.Emoji+":"+counted)
		}
	}
	return out
}

// ackFiles lists what the acknowledgement keeps on disk under the state directory.
func (r *round4SlackRig) ackFiles() []string {
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
// and polls; it returns the owner lines posted so far.
func (r *round4SlackRig) due() int {
	r.t.Helper()
	r.now = r.now.Add(3 * time.Minute)
	stamp := r.now.Format(time.RFC3339)
	if err := os.WriteFile(r.health, []byte(`{"state":"session_down","since":"`+stamp+`","checked_at":"`+stamp+`"}`), 0o600); err != nil {
		r.t.Fatal(err)
	}
	if err := r.cfg.neverSilent.Poll(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	return r.posts
}

func TestRound4SessionReactionCountsAsWithAckOff(t *testing.T) {
	// THE CLASS: the acknowledgement places eyes, the session reacts eyes
	// through the react verb, a restart, three minutes, session_down: no owner
	// line, and the receipt says delivered.
	t.Run("class/ack-eyes-session-eyes-restart", func(t *testing.T) {
		r := newRound4SlackRig(t)
		r.start("eyes")
		human := slackTimestamp(r.now)
		r.human(human)
		if got := r.slack.addCount(); got != 1 {
			t.Fatalf("acknowledgements=%d, want 1", got)
		}
		if receipt := r.react(human, "eyes"); !receipt.Delivered {
			t.Fatalf("receipt=%+v, want delivered (as main)", receipt)
		}
		if r.acts() != 1 {
			t.Errorf("never-silent acts=%d after the session's eyes, want 1: the acknowledgement hid the session's answer", r.acts())
		}
		if got, want := strings.Join(r.journaled(), ","), "eyes:counted"; got != want {
			t.Errorf("journal session_react rows=%q, want %q (a restart's reconcile replays only a counted row)", got, want)
		}
		r.start("eyes")
		if posts := r.due(); posts != 0 {
			t.Errorf("owner lines=%d after the session answered with eyes and a restart, want 0", posts)
		}
	})

	// The control: the same acknowledgement and restart with the session
	// silent. The owner line IS posted (round 3's R4, across a restart).
	t.Run("class/silent-session-restart-control", func(t *testing.T) {
		r := newRound4SlackRig(t)
		r.start("eyes")
		r.human(slackTimestamp(r.now))
		r.start("eyes")
		if posts := r.due(); posts != 1 {
			t.Errorf("owner lines=%d with the session silent, want 1 (the acknowledgement is no answer)", posts)
		}
	})

	// (b) THE SWITCH UNSET: a run with the acknowledgement on leaves its files
	// on disk; the next start has ACK_REACTION unset. The session's eyes is
	// answered already_reacted: main's answer, delivered and no act, and the
	// owner line follows. Nothing the acknowledgement left is read or removed.
	t.Run("switch-unset/old-ack-files-on-disk", func(t *testing.T) {
		r := newRound4SlackRig(t)
		r.start("eyes")
		human := slackTimestamp(r.now)
		r.human(human)
		before := r.ackFiles()
		if len(before) == 0 {
			t.Fatal("the acknowledgement left no file on disk to test with")
		}
		r.start("")
		if r.cfg.arrivalChoice.Enabled() {
			t.Fatal("ACK_REACTION is still on")
		}
		if receipt := r.react(human, "eyes"); !receipt.Delivered {
			t.Fatalf("receipt=%+v, want delivered (as main)", receipt)
		}
		if r.acts() != 0 {
			t.Errorf("never-silent acts=%d, want 0: with the switch unset already_reacted is main's no-op", r.acts())
		}
		if got := r.ackFiles(); strings.Join(got, ",") != strings.Join(before, ",") {
			t.Errorf("acknowledgement files %q became %q with the switch unset", before, got)
		}
		r.start("")
		if posts := r.due(); posts != 1 {
			t.Errorf("owner lines=%d, want 1 (main's rule with the switch unset)", posts)
		}
	})

	// P1, the repeats: each second reaction is the session's own repeat and
	// counts no more than it would with the acknowledgement off.
	for _, tc := range []struct {
		name, first string
		refuseAck   bool
	}{
		{"repeat/ack-eyes-session-eyes-twice", "eyes", false},
		{"repeat/ack-eyes-session-thumbsup-twice", "thumbsup", false},
		{"repeat/ack-refused-session-eyes-twice", "eyes", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRound4SlackRig(t)
			r.start("eyes")
			r.slack.mu.Lock()
			r.slack.refuse = tc.refuseAck
			r.slack.mu.Unlock()
			human := slackTimestamp(r.now)
			r.human(human)
			if receipt := r.react(human, tc.first); !receipt.Delivered {
				t.Fatalf("first receipt=%+v", receipt)
			}
			if r.acts() != 1 {
				t.Errorf("after the session's first %s: acts=%d, want 1 (it counts)", tc.first, r.acts())
			}
			r.start("eyes")
			if receipt := r.react(human, tc.first); !receipt.Delivered {
				t.Fatalf("second receipt=%+v", receipt)
			}
			if r.acts() != 1 {
				t.Errorf("after the session's second %s: acts=%d, want 1 (a repeat is no new answer)", tc.first, r.acts())
			}
			if got, want := strings.Join(r.journaled(), ","), tc.first+":counted,"+tc.first+":not counted"; got != want {
				t.Errorf("journal session_react rows=%q, want %q", got, want)
			}
		})
	}
}
