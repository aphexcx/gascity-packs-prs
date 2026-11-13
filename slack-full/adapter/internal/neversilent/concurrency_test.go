package neversilent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClearMailSurvivesExpiredStartMail(t *testing.T) {
	f := setup(t)
	f.track("example-message", "example-conversation")
	f.check(f.e.Stop("example-message", true))
	f.mailErr = errors.New("operator unavailable")
	f.poll()
	f.now = f.now.Add(MailLifetime + time.Minute)
	f.check(f.e.Forget("example-message"))
	f.mailErr = nil
	f.poll()
	if len(f.mails) != 2 || !strings.HasSuffix(f.mails[1].Subject, " cleared") {
		t.Fatalf("clear mail lost after start expired: %d attempts", len(f.mails))
	}
}

func TestAttributedReplyWithoutReceiptSuppressesUnknownStop(t *testing.T) {
	f := setup(t)
	f.track("example-message", "example-conversation")
	f.check(f.e.Stop("example-message", false))
	f.check(f.e.Act("example-conversation", "", "example-session", f.now))
	f.now = f.now.Add(FailureWait)
	f.poll()
	if len(f.posts) != 0 {
		t.Fatal("reply without a receipt was ignored")
	}
}

func TestSessionActDuringOwnerSendSuppressesNextConversation(t *testing.T) {
	f := setup(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	f.opts.Post = func(_ context.Context, m Message, _ string) error {
		f.posts = append(f.posts, m.ID)
		if m.ID == "a" {
			close(entered)
			<-release
		}
		return nil
	}
	f.restart()
	for _, id := range []string{"a", "b"} {
		f.track(id, id)
		f.check(f.e.Result(id, "pending", []string{"example-session"}))
		f.check(f.e.Stop(id, false))
	}
	f.now = f.now.Add(FailureWait)
	go func() { done <- f.e.Poll(context.Background()) }()
	<-entered
	acted := make(chan error, 1)
	go func() { acted <- f.e.Act("b", "example-thread", "example-session", f.now) }()
	select {
	case err := <-acted:
		f.check(err)
	case <-time.After(time.Second):
		close(release)
		<-done
		<-acted
		t.Fatal("network effect blocked session observation")
	}
	close(release)
	f.check(<-done)
	if len(f.posts) != 1 {
		t.Fatal("posted after session answered")
	}
}

func TestNewPendingSupersedesStoppedAndDeliveredEvidence(t *testing.T) {
	f := setup(t)
	f.track("example", "conversation")
	f.check(f.e.Result("example", "delivered", []string{"mayor"}))
	f.check(f.e.Stop("example", false))
	f.now = f.now.Add(time.Minute)
	f.check(f.e.ResultAt("example", "pending", []string{"mayor"}, f.now))
	f.restart()
	f.now = f.now.Add(FailureWait)
	f.health("session_down")
	f.poll()
	if len(f.posts) != 0 || len(f.mails) != 0 {
		t.Fatal("new pending attempt retained obsolete failure/delivery evidence")
	}
}

func TestWithdrawalBeforeEnrollmentSurvivesRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		f := setup(t)
		f.check(f.e.Forget("withdrawn"))
		if restart {
			f.restart()
		}
		f.track("withdrawn", "conversation")
		f.check(f.e.Result("withdrawn", "failed", []string{"mayor"}))
		f.check(f.e.Stop("withdrawn", true))
		f.poll()
		if len(f.posts) != 0 || len(f.mails) != 0 {
			t.Fatalf("withdrawal lost before enrollment (restart=%v)", restart)
		}
	}
}

func TestHealthChangeDuringOwnerSendDoesNotPostStaleLine(t *testing.T) {
	f := setup(t)
	f.opts.Post = func(_ context.Context, m Message, text string) error {
		f.posts = append(f.posts, text)
		if m.ID == "a" {
			f.health("seat_at_cap")
		}
		return nil
	}
	f.restart()
	for _, id := range []string{"a", "b"} {
		f.track(id, id)
		f.check(f.e.Result(id, "delivered", []string{"mayor"}))
	}
	f.now = f.now.Add(FailureWait)
	f.health("no_seat")
	f.poll()
	if len(f.posts) != 1 {
		t.Fatalf("posted stale no-seat line after health changed: %v", f.posts)
	}
}
