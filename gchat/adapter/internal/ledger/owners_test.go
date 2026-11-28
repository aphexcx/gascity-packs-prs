package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestChatOwnerStateKeys: a state file written before this change loads and
// saves with none of the new keys; a state with every new key round-trips;
// a snapshot's requests and events are its own; the state names no owner,
// claim or try.
func TestChatOwnerStateKeys(t *testing.T) {
	l, err := Load("testdata/ledger-0.5.0.json", true)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, key := range []string{"owner_requests", "owner_events"} {
		if strings.Contains(string(data), `"`+key+`"`) {
			t.Fatalf("an old state saved again gained %s", key)
		}
	}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	l.OwnerRequests = map[string]*OwnerRequest{"users/1001": {CodeHash: strings.Repeat("a", 64), Identity: "users/123", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour)}}
	l.OwnerEvents = []OwnerEvent{{At: at, Event: "taken", Claim: strings.Repeat("c", 32), Owner: "users/1001", Message: "spaces/AAA/messages/BBB"}}
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.OwnerRequests, l.OwnerRequests) || !reflect.DeepEqual(back.OwnerEvents, l.OwnerEvents) {
		t.Fatalf("round trip: %+v %+v", back.OwnerRequests, back.OwnerEvents)
	}
	snapshot := back.Clone()
	snapshot.OwnerRequests["users/1001"].Used = true
	snapshot.OwnerRequests["users/1002"] = &OwnerRequest{}
	snapshot.OwnerEvents[0].Owner = "users/9"
	if back.OwnerRequests["users/1001"].Used || back.OwnerRequests["users/1002"] != nil || back.OwnerEvents[0].Owner != "users/1001" {
		t.Fatal("a snapshot's change showed in the ledger it was copied from")
	}
	var keys map[string]json.RawMessage
	data, _ = os.ReadFile(path)
	if err := json.Unmarshal(data, &keys); err != nil {
		t.Fatal(err)
	}
	for key := range keys {
		if key == "owners" || strings.Contains(key, "claim") || strings.Contains(key, "tries") {
			t.Fatalf("state key %q", key)
		}
	}
}

// TestChatOwnerEventsKeepTheNewest: the state keeps the newest 500 events.
func TestChatOwnerEventsKeepTheNewest(t *testing.T) {
	l := New()
	for i := range OwnerEventLimit + 7 {
		l.AddOwnerEvent(OwnerEvent{Event: "removed", Owner: "users/1", Message: "spaces/A/messages/" + string(rune('a'+i%26)), At: time.Unix(int64(i), 0).UTC()})
	}
	if len(l.OwnerEvents) != OwnerEventLimit {
		t.Fatalf("kept %d events", len(l.OwnerEvents))
	}
	if !l.OwnerEvents[0].At.Equal(time.Unix(7, 0)) || !l.OwnerEvents[OwnerEventLimit-1].At.Equal(time.Unix(OwnerEventLimit+6, 0)) {
		t.Fatalf("first %v, last %v", l.OwnerEvents[0].At, l.OwnerEvents[OwnerEventLimit-1].At)
	}
}
