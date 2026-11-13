package server

import (
	"encoding/json"
	"testing"
	"time"

	"gchat-adapter/internal/inbound"
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/puller"
	"gchat-adapter/internal/wire"
	api "google.golang.org/api/chat/v1"
)

func TestStatusNamesTheHistoryFloor(t *testing.T) {
	f := setup(t)
	code, raw, _ := f.call(t, "/status", nil)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if code != 200 || fields["history_floor"] != nil {
		t.Fatalf("legacy floor: %s", raw)
	}
	floor := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	data, _ := json.Marshal(map[string]any{"history_floor": floor})
	f.mu.Lock()
	err := json.Unmarshal(data, f.l)
	f.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	code, raw, _ = f.call(t, "/status", nil)
	var status struct {
		Floor time.Time `json:"history_floor"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	if code != 200 || !status.Floor.Equal(floor) {
		t.Fatalf("status floor=%s", status.Floor)
	}
}

// Also run with the exact base server.go overlaid: a rollback must normalize
// content from Chat, never reuse the attribution-only record as message text.
func TestHistoryFloorRollbackReadKeepsContent(t *testing.T) {
	f := setup(t)
	f.l.HistoryFloor = time.Now().UTC().Add(-time.Minute)
	m := &api.Message{Name: "spaces/room/messages/before", Text: "readable old body", CreateTime: f.l.HistoryFloor.Add(-time.Hour).Format(time.RFC3339Nano), Sender: &api.User{Name: "users/456"}, Thread: &api.Thread{Name: "spaces/room/threads/topic"}}
	f.g.Messages[m.Name] = m
	p, err := inbound.New(inbound.Options{Config: f.cfg, Ledger: f.l, LedgerMu: f.mu, LedgerPath: f.options.LedgerPath, Chat: f.options.Chat, GC: f.options.GC})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.HandleChange(t.Context(), puller.Change{Kind: "message.created", Space: "spaces/room", Message: m}); err != nil {
		t.Fatal(err)
	}
	saved, err := ledger.Load(f.options.LedgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Changes[m.Name].Reason != "local" || saved.Changes[m.Name].Cause != "before_history_floor" || len(f.gc.Inbound()) != 0 {
		t.Fatal("old create did not stay a committed non-turn")
	}
	f.l.Replace(saved)
	if content, ok := historyContent(f.l, m); ok {
		t.Errorf("attribution-only record accepted as cached content: %q", content.Text)
	}
	code, raw, _ := f.call(t, "/read?space=spaces/room", nil)
	got := decode[[]wire.InboundMessage](t, raw)
	if code != 200 || len(got) != 1 || got[0].Text != m.Text {
		t.Fatalf("history content changed: status=%d messages=%+v", code, got)
	}
}
