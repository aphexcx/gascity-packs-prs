package inbound

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

// Optional exchange mode lets the exact released reader consume the file the
// current writer saved. With no mode this exercises the exchange in-process.
func TestHistoryRollbackExchange(t *testing.T) {
	f := setup(t)
	mode, path := os.Getenv("GCHAT_TEST_EXCHANGE_MODE"), os.Getenv("GCHAT_TEST_EXCHANGE_FILE")
	if path == "" {
		path = filepath.Join(t.TempDir(), "ledger.json")
	}
	if mode != "read" {
		if err := json.Unmarshal([]byte(`{"history_floor":"2026-10-22T12:00:00Z"}`), f.l); err != nil {
			t.Fatal(err)
		}
		for i, kind := range []string{"message.created", "message.updated", "message.deleted", "reaction.created", "reaction.deleted"} {
			parts := strings.Split(kind, ".")
			m := message()
			m.Name += "-" + string(rune('a'+i))
			m.LastUpdateTime = "2026-10-22T11:00:00Z"
			var resource any = m
			if parts[0] == "reaction" {
				resource = &api.Reaction{Name: m.Name + "/reactions/one", User: &api.User{Name: "users/222"}}
			}
			raw, _ := json.Marshal(map[string]any{parts[0]: resource})
			ev, err := puller.Decode(&pubsub.Message{ID: kind, Data: raw, Attributes: map[string]string{"ce-type": "google.workspace.chat." + parts[0] + ".v1." + parts[1], "ce-time": "2026-10-22T11:00:00Z"}})
			if err != nil {
				t.Fatal(err)
			}
			handle(t, f, ev)
		}
		if len(f.gc.Inbound()) != 0 {
			t.Fatal("writer delivered skipped history")
		}
		if err := f.l.Save(path); err != nil {
			t.Fatal(err)
		}
		if mode == "write" {
			t.Log("wrote five skipped-act records for the released reader")
			return
		}
	}
	l, err := ledger.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	*f.l = *l
	// JSON works at both versions; the released shape ignores the field.
	_ = json.Unmarshal([]byte(`{"history_floor":"0001-01-01T00:00:00Z"}`), f.l)
	f.opts.Config.Settings.Defaults.Wake = config.WakeMentions
	for _, s := range f.l.Spaces {
		s.Wake = "mentions"
	}
	names := map[string]bool{}
	for _, c := range f.l.Changes {
		names[c.Message] = true
	}
	for name := range names {
		m := message()
		m.Name = name
		m.LastUpdateTime = "2026-10-22T14:00:00Z"
		for _, kind := range []string{"message.updated", "message.deleted"} {
			payload := m
			if kind == "message.deleted" {
				payload = &api.Message{Name: name}
			}
			raw, _ := json.Marshal(map[string]any{"message": payload})
			action := strings.TrimPrefix(kind, "message.")
			ev, err := puller.Decode(&pubsub.Message{ID: name + action, Data: raw, PublishTime: time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC), Attributes: map[string]string{"ce-type": "google.workspace.chat.message.v1." + action, "ce-time": "2026-10-22T15:00:00Z"}})
			if err != nil {
				t.Fatal(err)
			}
			handle(t, f, ev)
			if len(f.gc.Inbound()) != 0 {
				t.Fatalf("released reader after %s: turns=%d", kind, len(f.gc.Inbound()))
			}
		}
	}
	t.Logf("read %d skipped-act records: zero turns after every unmentioned edit and deletion", len(names))
}
