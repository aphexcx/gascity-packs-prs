package app

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/config"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/server"
)

func startDefaultNotifier(t *testing.T, f *fixture) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- RunWithOptions(ctx, f.cfg, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	f.health(t, "needs-sign-in", time.Second)
}

func TestStartupRegistersOnce(t *testing.T) {
	f := newFixture(t, true)
	f.start(t)
	f.health(t, "ready", 5*time.Second)
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, r := range f.requests {
		if strings.Contains(r, "/extmsg/adapters") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("startup registrations=%d: %v", count, f.requests)
	}
}

func TestDefaultNoticeUsesConfiguredAgentAndDedup(t *testing.T) {
	f := newFixture(t, false)
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "mail-args")
	// This fake executable receives arguments only; no real mail is sent.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$GCHAT_TEST_MAIL_ARGS.tmp\"\nmv -f \"$GCHAT_TEST_MAIL_ARGS.tmp\" \"$GCHAT_TEST_MAIL_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gc"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BIN", filepath.Join(dir, "gc"))
	t.Setenv("GCHAT_TEST_MAIL_ARGS", argsFile)
	f.cfg.Settings.Identities = []config.Identity{{Name: "example", Agent: "support", TokenFile: f.cfg.Secrets.TokenFile}}
	// f.start substitutes Notify; call the runtime directly to test its default.
	startDefaultNotifier(t, f)
	waitFor(t, time.Second, func() bool { b, _ := os.ReadFile(argsFile); return len(b) > 0 })
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(args) < 3 || !slices.Equal(args[:3], []string{"mail", "send", "support"}) {
		t.Fatalf("recipient: %v", args)
	}
	i := slices.Index(args, "--dedup")
	if i < 0 || i+1 == len(args) || args[i+1] != "gchat:examplecity:example:credentials" {
		t.Fatalf("dedup: %v", args)
	}
}

func TestHealthyGenerationResetsRestartBackoff(t *testing.T) {
	a := &application{}
	for range 8 {
		a.nextRestartDelay()
	}
	if d := a.nextRestartDelay(); d < 30*time.Second {
		t.Fatalf("backoff did not grow: %s", d)
	}
	a.healthyGeneration.Store(true)
	if d := a.nextRestartDelay(); d < 500*time.Millisecond || d >= time.Second {
		t.Fatalf("healthy run retained old backoff: %s", d)
	}
}

func TestHealthTransitionsCoalesce(t *testing.T) {
	var logs logBuffer
	cfg := &config.Config{}
	s, err := server.New(server.Options{Config: cfg, Ledger: ledger.New(), LedgerMu: new(sync.Mutex), LedgerPath: filepath.Join(t.TempDir(), "state.json")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := &application{s: s, log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	a.health("needs-sign-in", "credential rejected")
	a.health("needs-sign-in", "credential rejected")
	if n := strings.Count(logs.String(), "adapter health changed"); n != 1 {
		t.Fatalf("health transitions=%d: %s", n, logs.String())
	}
	a.health("ready", "")
	if !a.healthyGeneration.Load() {
		t.Fatal("healthy generation not observed")
	}
}
