package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/chat"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/fakes/fakegc"
	"gchat-adapter/internal/fakes/fakegoogle"
	"gchat-adapter/internal/gcclient"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	api "google.golang.org/api/chat/v1"
)

func TestResetTakesTheFirstStartBound(t *testing.T) {
	env, state, _ := stateFixture(t)
	before := time.Now().UTC()
	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"state", "reset"}, &stdout, &stderr, envAt(env), nil); code != 0 {
		t.Fatalf("reset=%d: %s", code, &stderr)
	}
	l, err := ledger.Load(state, true)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(l)
	var saved struct {
		Floor time.Time `json:"history_floor"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Floor.Before(before) || saved.Floor.After(time.Now()) {
		t.Errorf("reset floor=%s", saved.Floor)
	}
	if !strings.Contains(stderr.String(), "gc gchat read still reads them") || !strings.Contains(stderr.String(), "state restore") {
		t.Errorf("reset warning=%s", &stderr)
	}
	g, gc := fakegoogle.New(t, nil), fakegc.New(t)
	g.Spaces["spaces/room"] = &api.Space{Name: "spaces/room", SpaceType: "SPACE"}
	for id, at := range map[string]time.Time{"old": before.Add(-time.Hour), "new": time.Now()} {
		name := "spaces/room/messages/" + id
		g.Messages[name] = &api.Message{Name: name, Text: id, CreateTime: at.Format(time.RFC3339Nano), Sender: &api.User{Name: "users/222", Type: "HUMAN"}, Thread: &api.Thread{Name: "spaces/room/threads/topic"}}
	}
	cfg := &config.Config{SecretsDir: filepath.Dir(env), Secrets: config.Secrets{CityName: "examplecity"}, Endpoints: config.Endpoints{StateFile: state, ChatAPIBase: g.URL()}}
	c, err := chat.New(t.Context(), http.DefaultClient, cfg.Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	mu := &sync.Mutex{}
	p, err := inbound.New(inbound.Options{Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: state, Chat: c, GC: gcclient.New(gc.URL(), "examplecity"), IdentityUserID: fakegoogle.UserID})
	if err != nil {
		t.Fatal(err)
	}
	r, err := catchup.New(catchup.Options{Config: cfg, Ledger: l, LedgerMu: mu, LedgerPath: state, Chat: c, Processor: p})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Freeze(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := gc.Inbound()
	if len(got) != 1 || got[0].ProviderMessageID != "spaces/room/messages/new" {
		t.Fatalf("reset delivered %d turns", len(got))
	}
}
