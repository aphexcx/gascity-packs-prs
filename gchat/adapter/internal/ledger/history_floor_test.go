package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Frozen top-level schema from 0.5.0: an older reader ignores history_floor.
type ledger050 struct {
	Version          int                      `json:"version"`
	IdentityUserID   string                   `json:"identity_user_id,omitempty"`
	IdentitySeed     string                   `json:"identity_seed,omitempty"`
	IdentityDomainID string                   `json:"identity_domain_id,omitempty"`
	Subscription     SubscriptionState        `json:"subscription"`
	Spaces           map[string]*SpaceState   `json:"spaces"`
	Transport        TransportDedup           `json:"transport"`
	Messages         map[string]*MessageState `json:"messages"`
	Changes          map[string]*ChangeState  `json:"changes"`
	Sends            map[string]*SendState    `json:"sends"`
	Unresolved       map[string]*ChangeState  `json:"unresolved"`
	Quarantine       []QuarantinedEvent       `json:"quarantine"`
	Counters         map[string]int64         `json:"counters"`
}

func TestHistoryFloorLedgerCompatibility(t *testing.T) {
	l, err := Load("testdata/ledger-0.5.0.json", true)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(l)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(before, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["history_floor"] != nil {
		t.Fatal("old ledger gained a floor")
	}
	floor := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	patch, _ := json.Marshal(map[string]any{"history_floor": floor})
	if err := json.Unmarshal(patch, l); err != nil {
		t.Fatal(err)
	}
	l.Spaces["spaces/room"] = &SpaceState{Kind: "room", Wake: "all", MessageCursor: floor, EventCursor: floor}
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["history_floor"] == nil {
		t.Fatal("new ledger did not persist floor")
	}
	var old ledger050
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	if !old.Spaces["spaces/room"].MessageCursor.Equal(floor) || !old.Spaces["spaces/room"].EventCursor.Equal(floor) {
		t.Fatal("old reader lost cursors")
	}
	rolled, _ := json.Marshal(old)
	if err := os.WriteFile(path, rolled, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(restored)
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(got, &actual); err != nil {
		t.Fatal(err)
	}
	delete(fields, "history_floor")
	if !reflect.DeepEqual(fields, actual) {
		t.Fatal("rollback changed more than the floor")
	}
}
