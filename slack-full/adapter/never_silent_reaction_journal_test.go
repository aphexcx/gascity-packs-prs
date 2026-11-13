package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjarmak/gc-slack-adapter/internal/neversilent"
)

func TestNeverSilentReactionJournal(t *testing.T) {
	for _, tc := range []struct {
		name           string
		enabled, fresh bool
	}{
		{"off_already_reacted", false, false},
		{"on_already_reacted", true, false},
		{"on_fresh", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			dir := t.TempDir()
			path := filepath.Join(dir, "notices.json")
			e, err := neversilent.New(neversilent.Options{Enabled: tc.enabled, Path: path, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.fresh {
					_, _ = w.Write([]byte(`{"ok":true}`))
				} else {
					_, _ = w.Write([]byte(`{"ok":false,"error":"already_reacted"}`))
				}
			}))
			defer fake.Close()
			base := slackAPIBase
			slackAPIBase = fake.URL
			defer func() { slackAPIBase = base }()
			j := &inboundJournal{dir: filepath.Join(dir, "journal"), now: func() time.Time { return now }}
			cfg := config{cityPath: dir, neverSilent: e, inboundJournal: j}
			w := httptest.NewRecorder()
			handleReact(cfg)(w, httptest.NewRequest(http.MethodPost, "/react", strings.NewReader(`{"conversation":{"conversation_id":"example-channel"},"message_id":"example-message","emoji":"eyes","session_id":"example-session"}`)))
			var receipt reactReceipt
			if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || !receipt.Delivered {
				t.Fatalf("receipt: %s", w.Body.String())
			}
			rows := ingestionJournalRows(t, j)
			if len(rows) != 1 || rows[0].SessionAct == nil {
				t.Fatalf("journal reaction acts=%d, want 1", len(rows))
			}
			if !tc.enabled {
				data, err := json.Marshal(rows[0].SessionAct)
				if err != nil {
					t.Fatal(err)
				}
				var actual map[string]any
				if err := json.Unmarshal(data, &actual); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{"kind": "reaction", "source": "session_react", "channel": "example-channel", "message_ts": "example-message", "session_id": "example-session", "emoji": "eyes", "timestamp": now.Format(time.RFC3339Nano)}
				if !reflect.DeepEqual(actual, want) {
					t.Fatalf("switch-off row=%v, want main's row=%v", actual, want)
				}
				return
			}
			check := func(stage string) {
				t.Helper()
				data, err := os.ReadFile(path)
				if os.IsNotExist(err) && !tc.fresh {
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var state struct {
					Acts []json.RawMessage `json:"acts"`
				}
				if err := json.Unmarshal(data, &state); err != nil {
					t.Fatal(err)
				}
				want := 0
				if tc.fresh {
					want = 1
				}
				if len(state.Acts) != want {
					t.Fatalf("%s: engine acts=%d want=%d", stage, len(state.Acts), want)
				}
			}
			check("direct")
			if err := cfg.reconcileNeverSilent(); err != nil {
				t.Fatal(err)
			}
			check("reconciled")
		})
	}
}
