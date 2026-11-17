package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestJoinLedgerCompatibility(t *testing.T) {
	raw, err := os.ReadFile("testdata/ledger-0.5.0.json")
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]json.RawMessage
	if err = json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	doc["join_requests"] = json.RawMessage(`{"spaces/room":{"code_hash":"example-hash","created_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-02T00:00:00Z","used":false}}`)
	doc["join_attempts"] = json.RawMessage(`[{"at":"2026-01-01T00:00:00Z","space":"spaces/room","owner_message":"spaces/dm/messages/approval","result":"joined"}]`)
	path := filepath.Join(t.TempDir(), "ledger.json")
	b, _ := json.Marshal(doc)
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	l, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Save(path); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]json.RawMessage
	_ = json.Unmarshal(b, &updated)
	if updated["join_requests"] == nil || updated["join_attempts"] == nil {
		t.Fatal("new adapter dropped join records")
	}
	// The released decoder only keeps its frozen top-level fields. Simulate its
	// unknown-field discard, and compare every retained field with the fixture.
	for key := range updated {
		if _, ok := old[key]; !ok {
			delete(updated, key)
		}
	}
	for key, want := range old {
		var x, y any
		_ = json.Unmarshal(want, &x)
		_ = json.Unmarshal(updated[key], &y)
		if !reflect.DeepEqual(x, y) {
			t.Fatalf("rollback changed existing field %s", key)
		}
	}
	b, _ = json.Marshal(updated)
	_ = os.WriteFile(path, b, 0600)
	if _, err = Load(path, true); err != nil {
		t.Fatal(err)
	}
}

func TestJoinAndFirstReadLedgerRoundTripClone(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New()
	l.LastSpaceList = at
	l.Spaces["spaces/room"] = &SpaceState{}
	l.Spaces["spaces/room"].StartFirstRead(at, at.Add(time.Minute), 5*time.Minute, "membership", "event")
	l.Spaces["spaces/room"].FirstRead.PreJoinMessages = map[string]bool{"spaces/room/messages/adding": true}
	l.JoinRequests = map[string]*JoinRequest{
		"spaces/room": {CodeHash: "example-hash", Identity: "users/123456789", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour)},
	}
	l.JoinAttempts = []JoinAttempt{{At: at, Space: "spaces/room", OwnerMessage: "spaces/dm/messages/approval", Result: "joined"}}
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, l) {
		t.Fatal("save/load changed first-read or join state")
	}
	clone := loaded.Clone()
	if !reflect.DeepEqual(clone, loaded) {
		t.Fatal("clone changed first-read or join state")
	}
	clone.LastSpaceList = at.Add(time.Hour)
	clone.Spaces["spaces/room"].FirstRead.Complete = true
	clone.Spaces["spaces/room"].FirstRead.PreJoinMessages["spaces/room/messages/adding"] = false
	clone.JoinRequests["spaces/room"].Used = true
	clone.JoinAttempts[0].Result = "refused_invited"
	if !reflect.DeepEqual(loaded, l) {
		t.Fatal("clone mutation reached the original")
	}
}
