package app

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/ledger"
)

func TestPersistedIdentitySeedSurvivesRestart(t *testing.T) {
	f := newFixture(t, false)
	f.replaceIdentityToken(t, "users/111")
	// This is the on-disk state after Chat contradicted the original token sub.
	previous := &application{cfg: f.cfg, l: ledger.New(), log: slog.Default()}
	if err := previous.seedIdentity("users/111"); err != nil {
		t.Fatal(err)
	}
	previous.l.IdentityUserID = "users/222"
	if err := previous.l.Save(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	var logs logBuffer
	f.startOptions(t, Options{Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	f.health(t, "ready", 5*time.Second)
	read := func() ledger.Ledger {
		data, err := os.ReadFile(f.cfg.Endpoints.StateFile)
		if err != nil {
			t.Fatal(err)
		}
		var l ledger.Ledger
		if err := json.Unmarshal(data, &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	if got := read().IdentityUserID; got != "users/222" {
		t.Fatalf("restart overwrote observed identity: %s", got)
	}
	f.mu.Lock()
	before := len(f.requests)
	f.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	f.mu.Lock()
	requests := append([]string(nil), f.requests[before:]...)
	f.mu.Unlock()
	for _, r := range requests {
		if strings.Contains(r, "/adapters") {
			t.Fatalf("restart rebuilt identity: %v", requests)
		}
	}
	if strings.Contains(logs.String(), "credential identity changed") {
		t.Fatalf("same consent logged mismatch again: %s", logs.String())
	}
	f.replaceIdentityToken(t, "users/333")
	waitFor(t, 5*time.Second, func() bool { return read().IdentityUserID == "users/333" })
	f.health(t, "ready", 5*time.Second)
	data, _ := os.ReadFile(f.cfg.Endpoints.StateFile)
	var persisted map[string]any
	_ = json.Unmarshal(data, &persisted)
	if persisted["identity_seed"] != "users/333" {
		t.Fatalf("new consent seed not saved: %s", data)
	}
	if n := strings.Count(logs.String(), "credential identity changed"); n != 1 {
		t.Fatalf("new consent logs=%d: %s", n, logs.String())
	}
}
