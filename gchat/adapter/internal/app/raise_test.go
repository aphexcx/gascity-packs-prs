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

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/inbound"
)

func raiseApp(opts Options) *application {
	cfg := &config.Config{Secrets: config.Secrets{CityName: "examplecity"}}
	cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "support"}}
	return &application{cfg: cfg, opts: opts, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestDeliveryRaiseMailsTheIdentityAgentWithPerChangeDedup(t *testing.T) {
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
	r := inbound.Raise{ID: "spaces/AAAA/messages/BBBB", Space: "spaces/AAAA", Message: "spaces/AAAA/messages/BBBB", Body: "line one\nline two"}
	if err := a.raiseDelivery(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	// printf splits the multi-line body over lines; the fixed arguments come first.
	args := strings.Split(string(b), "\n")
	want := []string{"mail", "send", "support", "--dedup", fmt.Sprintf("gchat:examplecity:example:delivery:%x", sha256.Sum256([]byte(r.ID))), "-s", "gchat: delivery", "-m", "line one", "line two"}
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

// gp-dl26, ruling E: the one second mail of a message (it ended after a
// waiting mail) has its own dedup key; gc would drop a repeat of the first.
func TestSecondDeliveryMailHasItsOwnDedupKey(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GCHAT_TEST_MAIL_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	a := raiseApp(Options{})
	keys := map[bool]string{}
	for _, second := range []bool{false, true} {
		argsFile := filepath.Join(dir, fmt.Sprintf("args-%v", second))
		t.Setenv("GCHAT_TEST_MAIL_ARGS", argsFile)
		if err := a.raiseDelivery(t.Context(), inbound.Raise{ID: "spaces/AAAA/messages/BBBB", Body: "body", Second: second}); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		args := strings.Split(string(b), "\n")
		keys[second] = args[slices.Index(args, "--dedup")+1]
	}
	first := fmt.Sprintf("gchat:examplecity:example:delivery:%x", sha256.Sum256([]byte("spaces/AAAA/messages/BBBB")))
	if keys[false] != first || keys[true] != first+":ended" {
		t.Fatalf("dedup keys: first %q second %q", keys[false], keys[true])
	}
}
