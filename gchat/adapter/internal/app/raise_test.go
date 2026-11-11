package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/inbound"
)

func raiseApp(opts Options) *application {
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity"}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "support"}}
	return &application{cfg: cfg, opts: opts, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestDeliveryRaiseMailsTheIdentityAgentWithPerLifeDedup(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "mail-args")
	// The fake gc records its arguments, one per line; no real mail is sent.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GCHAT_TEST_MAIL_ARGS\"\nexit \"${GCHAT_TEST_MAIL_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	t.Setenv("GCHAT_TEST_MAIL_ARGS", argsFile)
	a := raiseApp(Options{})
	recorded := time.Date(2026, 11, 10, 20, 22, 58, 123456789, time.UTC)
	r := inbound.Raise{ID: "spaces/AAAA/messages/BBBB", Space: "spaces/AAAA", Message: "spaces/AAAA/messages/BBBB", Body: "line one\nline two", Recorded: recorded}
	if err := a.raiseDelivery(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	// printf splits the multi-line body over lines; the fixed arguments come first.
	args := strings.Split(string(b), "\n")
	want := []string{"mail", "send", "support", "--dedup", fmt.Sprintf("gchat:examplecity:example:delivery:%x", sha256.Sum256([]byte(r.ID+"\x002026-11-10T20:22:58.123456789Z"))), "-s", "gchat: delivery", "-m", "line one", "line two"}
	if len(args) < len(want) || !slices.Equal(args[:len(want)], want) {
		t.Fatalf("mail arguments:\n got %q\nwant %q", args, want)
	}
	t.Setenv("GCHAT_TEST_MAIL_EXIT", "1")
	if err := a.raiseDelivery(t.Context(), r); err == nil {
		t.Fatal("a failed mail send reached the processor as success")
	}
}

func TestDeliveryRaiseUsesTheNotifyHookAndReportsItsFailure(t *testing.T) {
	var subject, body string
	fail := errors.New("mail refused")
	result := error(nil)
	a := raiseApp(Options{Notify: func(_ context.Context, s, b string) error { subject, body = s, b; return result }})
	r := inbound.Raise{ID: "spaces/AAAA/messages/BBBB", Body: "the body"}
	if err := a.raiseDelivery(t.Context(), r); err != nil || subject != "gchat: delivery" || body != "the body" {
		t.Fatalf("hook: %v %q %q", err, subject, body)
	}
	result = fail
	if err := a.raiseDelivery(t.Context(), r); !errors.Is(err, fail) {
		t.Fatalf("hook failure not reported: %v", err)
	}
}

// gp-dl26, ruling E and self-review r1 MINOR-1: a mail that says the message
// ended has its own dedup key, never the waiting mail's; gc drops a mail
// whose key is still live. gp-t542: a replay's life has keys of its own, so a
// live key of the life before it never drops the replay's mails.
func TestEndedDeliveryMailHasItsOwnDedupKey(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GCHAT_TEST_MAIL_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	a := raiseApp(Options{})
	life := time.Date(2026, 11, 10, 20, 22, 58, 0, time.UTC)
	replay := life.Add(25 * time.Hour)
	key := func(recorded time.Time, ended bool) string {
		argsFile := filepath.Join(dir, fmt.Sprintf("args-%d-%v", recorded.Unix(), ended))
		t.Setenv("GCHAT_TEST_MAIL_ARGS", argsFile)
		if err := a.raiseDelivery(t.Context(), inbound.Raise{ID: "spaces/AAAA/messages/BBBB", Body: "body", Ended: ended, Recorded: recorded}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(string(b), "\n")
		return args[slices.Index(args, "--dedup")+1]
	}
	first := fmt.Sprintf("gchat:examplecity:example:delivery:%x", sha256.Sum256([]byte("spaces/AAAA/messages/BBBB\x002026-11-10T20:22:58Z")))
	if waiting, ended := key(life, false), key(life, true); waiting != first || ended != first+":ended" {
		t.Fatalf("dedup keys: waiting %q ended %q", waiting, ended)
	}
	if again := key(life, false); again != first {
		t.Fatalf("a retry of one mail changed its key: %q", again)
	}
	second := fmt.Sprintf("gchat:examplecity:example:delivery:%x", sha256.Sum256([]byte("spaces/AAAA/messages/BBBB\x002026-11-11T21:22:58Z")))
	if waiting, ended := key(replay, false), key(replay, true); waiting != second || ended != second+":ended" || second == first {
		t.Fatalf("the replay's dedup keys: waiting %q ended %q", waiting, ended)
	}
}
