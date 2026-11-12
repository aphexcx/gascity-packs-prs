package neversilent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	t                *testing.T
	now              time.Time
	opts             Options
	e                *Engine
	posts            []string
	mails            []Mail
	postErr, mailErr error
}

func setup(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	f.opts = Options{Enabled: true, City: "example-city", Channel: "gchat", Path: filepath.Join(dir, "state.json"), HealthFile: filepath.Join(dir, "health.json"), OperatorContext: "example-operator", Now: func() time.Time { return f.now },
		Post: func(_ context.Context, m Message, line string) error {
			f.posts = append(f.posts, m.Conversation+"|"+m.Thread+"|"+line)
			return f.postErr
		},
		Send: func(_ context.Context, m Mail) error { f.mails = append(f.mails, m); return f.mailErr },
	}
	f.restart()
	return f
}

func (f *fixture) check(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *fixture) restart() { f.t.Helper(); var err error; f.e, err = New(f.opts); f.check(err) }
func (f *fixture) track(id, conv string) {
	f.t.Helper()
	f.check(f.e.Track(Message{ID: id, Conversation: conv, Thread: "example-thread", Created: f.now}))
}
func (f *fixture) poll() { f.t.Helper(); f.check(f.e.Poll(context.Background())) }
func (f *fixture) health(state string) {
	f.t.Helper()
	f.check(os.WriteFile(f.opts.HealthFile, []byte(fmt.Sprintf(`{"state":%q,"since":%q,"checked_at":%q}`, state, f.now.Format(time.RFC3339), f.now.Format(time.RFC3339))), 0600))
}

func TestEpisodesPersistAndAggregateByCause(t *testing.T) {
	f := setup(t)
	for i := 0; i < 12; i++ {
		id := fmt.Sprint("example-message-", i)
		f.track(id, fmt.Sprint("example-conversation-", i%4))
		f.check(f.e.Result(id, "failed", []string{"example-session"}))
	}
	f.now = f.now.Add(FailureWait)
	f.poll()
	if len(f.posts) != 4 || len(f.mails) != 1 {
		t.Fatalf("posts=%d mails=%d", len(f.posts), len(f.mails))
	}
	if !strings.Contains(f.mails[0].Body, "messages: 12") || !strings.Contains(f.mails[0].Body, "conversations: 4") {
		t.Fatal(f.mails[0].Body)
	}
	f.restart()
	f.poll()
	if len(f.posts) != 4 || len(f.mails) != 1 {
		t.Fatal("restart repeated notification")
	}
	for i := 0; i < 12; i++ {
		f.check(f.e.Result(fmt.Sprint("example-message-", i), "delivered", []string{"example-session"}))
	}
	f.now = f.now.Add(time.Second)
	f.poll()
	if len(f.mails) != 2 || !strings.HasSuffix(f.mails[1].Subject, " cleared") {
		t.Fatalf("mails=%+v", f.mails)
	}
	if f.e.Status().OwnerLinesSevenDays != 4 || len(f.e.Status().Outages) != 0 {
		t.Fatalf("status=%+v", f.e.Status())
	}
}

func TestPendingResetsFailureAndNeverMeansDown(t *testing.T) {
	for _, failure := range []string{"failed", "unreachable"} {
		t.Run(failure, func(t *testing.T) {
			f := setup(t)
			f.track("example-message", "example-conversation")
			f.check(f.e.Result("example-message", failure, nil))
			f.now = f.now.Add(2 * time.Minute)
			f.check(f.e.Result("example-message", "pending", nil))
			f.now = f.now.Add(10 * time.Minute)
			f.poll()
			if len(f.posts)+len(f.mails) != 0 {
				t.Fatal("pending called down")
			}
			f.check(f.e.Result("example-message", failure, nil))
			f.now = f.now.Add(2 * time.Minute)
			f.poll()
			if len(f.posts) != 0 {
				t.Fatal("streak not reset")
			}
			f.now = f.now.Add(time.Minute)
			f.poll()
			if len(f.posts) != 1 {
				t.Fatal("continuous failure not reported")
			}
		})
	}
}

func TestStoppedUnknownWaitsAndSessionActSuppressesOwnerLine(t *testing.T) {
	for _, acted := range []bool{false, true} {
		f := setup(t)
		f.track("example-message", "example-conversation")
		f.check(f.e.Result("example-message", "pending", []string{"example-session"}))
		f.check(f.e.Stop("example-message", false))
		f.poll()
		if len(f.posts) != 0 || len(f.mails) != 1 {
			t.Fatal("unknown stop must mail immediately, but wait to post")
		}
		f.now = f.now.Add(2 * time.Minute)
		if acted {
			f.check(f.e.Act("example-conversation", "example-thread", "example-session", f.now))
		}
		f.now = f.now.Add(time.Minute)
		f.poll()
		want := 1
		if acted {
			want = 0
		}
		if len(f.posts) != want {
			t.Fatalf("acted=%v posts=%d", acted, len(f.posts))
		}
	}
}

func TestHealthRequiresDeliveredAndNoSessionAct(t *testing.T) {
	for _, state := range []string{"ok", "no_seat", "seat_at_cap", "session_down"} {
		for _, acted := range []bool{false, true} {
			t.Run(fmt.Sprint(state, "/", acted), func(t *testing.T) {
				f := setup(t)
				f.track("example-message", "example-conversation")
				f.check(f.e.Result("example-message", "delivered", []string{"example-session"}))
				f.now = f.now.Add(2 * time.Minute)
				if acted {
					f.check(f.e.Act("example-conversation", "example-thread", "example-session", f.now))
				}
				f.now = f.now.Add(time.Minute)
				f.health(state)
				f.poll()
				want := 1
				if state == "ok" || acted {
					want = 0
				}
				if len(f.posts) != want {
					t.Fatalf("posts=%v", f.posts)
				}
			})
		}
	}
}

func TestOwnerMarkPrecedesPostAndRefusalDoesNotBlockMail(t *testing.T) {
	f := setup(t)
	f.postErr = errors.New("refused")
	f.opts.Post = func(_ context.Context, _ Message, _ string) error {
		// A second reader sees the reservation even before the network post.
		e, err := New(f.opts)
		f.check(err)
		if len(e.state.Owners) != 1 {
			t.Fatal("owner post before durable mark")
		}
		f.posts = append(f.posts, "refused")
		return f.postErr
	}
	f.restart()
	f.track("example-message", "example-conversation")
	f.check(f.e.Stop("example-message", true))
	f.poll()
	f.restart()
	f.poll()
	if len(f.posts) != 1 || len(f.mails) != 1 || f.e.Status().OwnerLinesSevenDays != 0 {
		t.Fatalf("posts=%v mails=%v status=%+v", f.posts, f.mails, f.e.Status())
	}
}

func TestMailRetriesPersistAndDoNotRepeatSuccessfulSend(t *testing.T) {
	f := setup(t)
	f.mailErr = errors.New("refused")
	f.track("example-message", "example-conversation")
	f.check(f.e.Stop("example-message", true))
	f.poll()
	f.restart()
	f.poll()
	if len(f.mails) != 1 {
		t.Fatal("restart bypassed retry interval")
	}
	f.now = f.now.Add(time.Minute)
	f.mailErr = nil
	f.poll()
	f.restart()
	f.poll()
	if len(f.mails) != 2 || f.mails[0].Key != f.mails[1].Key {
		t.Fatal("mail marks or stable key broken")
	}
}

func TestNoOperatorContextAndOff(t *testing.T) {
	f := setup(t)
	f.opts.OperatorContext = ""
	f.restart()
	f.track("example-message", "example-conversation")
	f.check(f.e.Stop("example-message", true))
	f.poll()
	if len(f.mails) != 0 {
		t.Fatal("operator mail without context")
	}
	f.opts.Enabled = false
	f.restart()
	f.track("example-second", "example-other")
	f.check(f.e.Stop("example-second", true))
	f.poll()
	if len(f.posts) != 1 {
		t.Fatal("disabled engine posted")
	}
}

func TestOperatorMailContainsNoCustomerCanaries(t *testing.T) {
	f := setup(t)
	f.track("CANARY_MESSAGE_ID", "CANARY_ROOM_NAME")
	f.check(f.e.Stop("CANARY_MESSAGE_ID", true))
	f.poll()
	for _, m := range f.mails {
		if strings.Contains(m.Body+m.Subject+m.Key, "CANARY") {
			t.Fatal("customer coordinates leaked")
		}
		t.Log(m.Subject + "\n" + m.Body)
	}
}

func TestOwnerLineNeverBecomesAReminder(t *testing.T) {
	f := setup(t)
	f.track("example-message", "example-conversation")
	f.check(f.e.Stop("example-message", true))
	f.poll()
	f.now = f.now.Add(7 * time.Hour)
	f.poll()
	if len(f.posts) != 1 {
		t.Fatalf("old message got a reminder: %d posts", len(f.posts))
	}
	f.track("example-new-message", "example-conversation")
	f.check(f.e.Stop("example-new-message", true))
	f.poll()
	if len(f.posts) != 2 {
		t.Fatal("new message after cooldown did not get a line")
	}
}

func TestFakeGCBinaryRecordsRemoteMailArguments(t *testing.T) {
	f := setup(t)
	argsPath := filepath.Join(t.TempDir(), "args.txt")
	bin := filepath.Join(t.TempDir(), "fake-gc")
	t.Setenv("NEVER_SILENT_TEST_ARGS", argsPath)
	f.check(os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$NEVER_SILENT_TEST_ARGS\"\n"), 0700))
	f.opts.GC = bin
	f.opts.Send = nil
	f.restart()
	f.track("CANARY_MESSAGE", "CANARY_CONVERSATION")
	f.check(f.e.Stop("CANARY_MESSAGE", true))
	f.poll()
	args, err := os.ReadFile(argsPath)
	f.check(err)
	if !strings.Contains(string(args), "mail\nsend\n--context\nexample-operator\nmayor\n--dedup\nnever-silent:") || strings.Contains(string(args), "CANARY") {
		t.Fatal(string(args))
	}
	t.Log(string(args))
}
