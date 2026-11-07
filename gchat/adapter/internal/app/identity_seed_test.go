package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/principal"
	"gchat-adapter/internal/provenance"
	api "google.golang.org/api/chat/v1"
)

func TestIdentityChangeClearsDomainAtomically(t *testing.T) {
	for _, method := range []string{"seed", "observed"} {
		for _, failSave := range []bool{false, true} {
			name := method + "/saved"
			if failSave {
				name = method + "/rollback"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t, false)
				a := &application{cfg: f.cfg, l: ledger.New(), log: slog.Default()}
				a.l.IdentityUserID, a.l.IdentitySeed, a.l.IdentityDomainID = "users/111", "users/111", "old-domain"
				if err := a.l.Save(f.cfg.Endpoints.StateFile); err != nil {
					t.Fatal(err)
				}
				if failSave {
					a.l.SetSaveGuard(func() error { return errors.New("injected persistence failure") })
				}
				var err error
				if method == "seed" {
					err = a.seedIdentity("users/222")
				} else {
					c := &chat.Client{}
					if err := c.SetIdentityUserID("users/222"); err != nil {
						t.Fatal(err)
					}
					err = a.persistIdentity(c)
				}
				if (err != nil) != failSave {
					t.Fatalf("change error = %v, failSave=%v", err, failSave)
				}
				wantID, wantDomain, wantClass := "users/222", "", provenance.External
				if failSave {
					wantID, wantDomain, wantClass = "users/111", "old-domain", provenance.Organization
				}
				saved, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range []*ledger.Ledger{a.l, saved} {
					if l.IdentityUserID != wantID || l.IdentityDomainID != wantDomain {
						t.Fatalf("identity/domain not atomic: %q/%q", l.IdentityUserID, l.IdentityDomainID)
					}
					speaker := principal.New(principal.Options{}).Speaker(context.Background(), &api.User{Name: "users/333", DomainId: "old-domain"}, l.IdentityDomainID)
					if speaker.Class != wantClass {
						t.Fatalf("previous domain classified as %q, want %q", speaker.Class, wantClass)
					}
					if failSave && l.IdentitySeed != "users/111" {
						t.Fatal("identity seed did not roll back")
					}
				}
			})
		}
	}
}

func TestSeedlessLegacyLedgerReportsReseedWithoutClaimingNewConsent(t *testing.T) {
	f := newFixture(t, false)
	var logs logBuffer
	a := &application{cfg: f.cfg, l: ledger.New(), log: slog.New(slog.NewTextHandler(&logs, nil))}
	a.l.IdentityUserID = "users/222"
	if err := a.seedIdentity("users/111"); err != nil {
		t.Fatal(err)
	}
	if err := a.seedIdentity("users/111"); err != nil {
		t.Fatal(err)
	}
	if a.l.IdentityUserID != "users/111" || a.l.IdentitySeed != "users/111" {
		t.Fatalf("legacy seed not established: %+v", a.l)
	}
	if strings.Contains(logs.String(), "credential identity changed") || strings.Count(logs.String(), "legacy ledger identity re-seeded from credential") != 1 {
		t.Fatalf("misleading legacy identity log: %s", logs.String())
	}
}

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

func TestShutdownDuringReseedDoesNotPersistPreviousClient(t *testing.T) {
	f := newFixture(t, false)
	a := &application{cfg: f.cfg, l: ledger.New(), log: slog.Default()}
	old := &chat.Client{}
	if err := old.SetIdentityUserID("users/111"); err != nil {
		t.Fatal(err)
	}
	a.chatClient.Store(old)
	a.loaded.Store(true)
	if err := a.seedIdentity("users/333"); err != nil {
		t.Fatal(err)
	}
	// SIGTERM can arrive after the new seed is saved but before the new client
	// has been installed. The previous consent must not overwrite the new one.
	if err := a.saveShutdownState(f.cfg.Endpoints.StateFile); err != nil {
		t.Fatal(err)
	}
	saved, err := ledger.Load(f.cfg.Endpoints.StateFile, true)
	if err != nil || saved.IdentityUserID != "users/333" || saved.IdentitySeed != "users/333" {
		t.Fatalf("shutdown restored prior consent: %v %+v", err, saved)
	}
}
