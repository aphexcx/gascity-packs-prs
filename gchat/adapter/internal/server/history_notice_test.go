package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/neversilent"
	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

func TestStatusCarriesTheFloorAndTheNotices(t *testing.T) {
	f := setup(t)
	n, err := neversilent.New(neversilent.Options{Enabled: true, Path: filepath.Join(t.TempDir(), "notice.json"), Post: func(context.Context, neversilent.Message, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetNeverSilent(n)
	for _, floor := range []time.Time{time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), {}} {
		f.mu.Lock()
		f.l.HistoryFloor = floor
		f.mu.Unlock()
		code, raw, _ := f.call(t, "/status", nil)
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		var status neversilent.Status
		if err := json.Unmarshal(fields["never_silent"], &status); err != nil {
			t.Fatal(err)
		}
		if code != 200 || !status.Enabled || len(status.Outages) != 0 || (fields["history_floor"] == nil) != floor.IsZero() {
			t.Fatalf("status=%s", raw)
		}
		if !floor.IsZero() {
			var got time.Time
			if err := json.Unmarshal(fields["history_floor"], &got); err != nil || !got.Equal(floor) {
				t.Fatalf("floor=%s %v", got, err)
			}
		}
	}
}

func TestNoticePostSpaceStartMatchesProcessor(t *testing.T) {
	for _, mode := range []string{"inventory", "floor", "neither", "cursor"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			boundary := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
			f.l.SpacesListedAt = time.Time{}
			f.l.Spaces["spaces/dm"] = &ledger.SpaceState{Kind: "dm", Wake: "all"}
			if mode == "inventory" {
				f.l.SpacesListedAt = boundary
			}
			if mode == "floor" {
				f.l.HistoryFloor = boundary
			}
			if mode == "cursor" {
				f.l.Spaces["spaces/dm"].MessageCursor = boundary
			}
			other := f.l.Clone()
			before := time.Now()
			if err := f.s.PostNotice(t.Context(), neversilent.Message{Conversation: "spaces/dm"}, neversilent.DownLine); err != nil {
				t.Fatal(err)
			}
			after := time.Now()
			seen := f.l.Spaces["spaces/dm"].HistoryStart
			later := after.Add(time.Hour)
			p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: other, LedgerMu: &sync.Mutex{}, LedgerPath: filepath.Join(t.TempDir(), "processor.json"), Chat: f.options.Chat, GC: f.options.GC, Now: func() time.Time { return later }})
			if err != nil {
				t.Fatal(err)
			}
			m := &api.Message{Name: "spaces/dm/messages/new", CreateTime: later.Format(time.RFC3339Nano), Sender: &api.User{Name: "users/222", Type: "HUMAN"}, Space: &api.Space{Name: "spaces/dm", SpaceType: "DIRECT_MESSAGE"}, Text: "new message"}
			if _, err := p.Handle(t.Context(), &puller.Event{Changes: []puller.Change{{Kind: "message.created", Space: "spaces/dm", Message: m}}}); err != nil {
				t.Fatal(err)
			}
			start := other.Spaces["spaces/dm"].HistoryStart
			if mode == "neither" {
				if seen.Before(before) || seen.After(after) || !start.Equal(later) || seen.Equal(start) {
					t.Fatalf("encounter times: notice=%s processor=%s", seen, start)
				}
			} else if !seen.Equal(start) || (mode != "cursor" && !seen.Equal(boundary)) || (mode == "cursor" && !seen.IsZero()) {
				t.Fatalf("start differs: notice=%s processor=%s", seen, start)
			}
			if len(f.l.Sends) != 0 {
				t.Fatal("notice became a session reply")
			}
		})
	}
}
