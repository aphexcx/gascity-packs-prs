package ledger_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gchat-adapter/internal/catchup"
	"gchat-adapter/internal/config"
	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
)

func TestLedgerOfEitherParentLoads(t *testing.T) {
	for _, parent := range []string{"main", "pr-head", "merged"} {
		t.Run(parent, func(t *testing.T) {
			now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
			l := ledger.New()
			l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", MessageCursor: now.Add(-time.Hour)}
			l.Spaces["spaces/room"] = &ledger.SpaceState{Kind: "room"}
			l.Changes["spaces/dm/messages/one"] = &ledger.ChangeState{ID: "spaces/dm/messages/one", Message: "spaces/dm/messages/one", Space: "spaces/dm", Kind: "message.created", Status: "committed", Reason: "local", Cause: "before_history_floor", Created: now.Add(-time.Hour)}
			l.Sends["example-send"] = &ledger.SendState{Key: "example-send", Space: "spaces/dm", Parts: []ledger.SendPart{{RequestID: "example-request", Text: "reply", MessageName: "spaces/dm/messages/reply"}}}
			if parent != "main" {
				l.HistoryFloor, l.SpacesListedAt = now.Add(-24*time.Hour), now.Add(-time.Hour)
				l.Spaces["spaces/dm"].HistoryStart = l.SpacesListedAt
			}
			if parent != "pr-head" {
				c := l.Changes["spaces/dm/messages/one"]
				c.NoticeResolved, c.NoticeDelivered, c.NoticeAt = true, true, now.Add(-time.Minute)
				l.Sends["example-send"].Parts[0].SentAt = now.Add(-time.Minute)
			}
			path := filepath.Join(t.TempDir(), "ledger.json")
			if err := l.Save(path); err != nil {
				t.Fatal(err)
			}
			loaded, err := ledger.Load(path, true)
			if err != nil {
				t.Fatal(err)
			}
			before := loaded.Clone()
			if err := catchup.InitializeHistory(loaded, &config.Config{}, now, nil); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Changes, loaded.Changes) || !reflect.DeepEqual(before.Sends, loaded.Sends) {
				t.Fatal("migration lost parent notice fields")
			}
			if parent == "main" && (!loaded.HistoryFloor.IsZero() || !loaded.Spaces["spaces/room"].HistoryStart.Equal(now)) {
				t.Fatal("main ledger not migrated at load")
			}
			noticePath := filepath.Join(t.TempDir(), "notices.json")
			n, err := neversilent.New(neversilent.Options{Enabled: true, Path: noticePath, Now: func() time.Time { return now }, Post: func(context.Context, neversilent.Message, string) error { t.Error("unexpected owner post"); return nil }})
			if err != nil {
				t.Fatal(err)
			}
			if err := n.Track(neversilent.Message{ID: "spaces/dm/messages/unrelated", Conversation: "spaces/dm", Created: now}); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(noticePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := inbound.ReconcileNeverSilent(loaded, n); err != nil {
				t.Fatal(err)
			}
			if parent == "pr-head" {
				after, err := os.ReadFile(noticePath)
				if err != nil {
					t.Fatal(err)
				}
				if string(after) != string(original) {
					t.Fatal("PR-head ledger changed notice state")
				}
			}
			if err := loaded.Save(path); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var old parent050Ledger
			if err := json.Unmarshal(raw, &old); err != nil {
				t.Fatal(err)
			}
			rolled, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			var expected, actual map[string]any
			if err := json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(rolled, &actual); err != nil {
				t.Fatal(err)
			}
			delete(expected, "history_floor")
			delete(expected, "spaces_listed_at")
			for _, s := range expected["spaces"].(map[string]any) {
				delete(s.(map[string]any), "history_start")
			}
			for _, c := range expected["changes"].(map[string]any) {
				fields := c.(map[string]any)
				delete(fields, "notice_resolved")
				delete(fields, "notice_at")
				delete(fields, "notice_delivered")
			}
			for _, s := range expected["sends"].(map[string]any) {
				for _, p := range s.(map[string]any)["parts"].([]any) {
					delete(p.(map[string]any), "sent_at")
				}
			}
			if !reflect.DeepEqual(expected, actual) {
				t.Fatalf("frozen reader changed more than new fields: expected=%v actual=%v", expected, actual)
			}
		})
	}
}
