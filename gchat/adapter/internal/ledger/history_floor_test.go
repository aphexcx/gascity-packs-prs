package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Frozen schemas from 0.5.0, including the space shape: a rollback drops
// history_floor, spaces_listed_at and history_start, and keeps its cursors.
type space050 struct {
	Kind          string    `json:"kind"`
	DisplayName   string    `json:"display_name,omitempty"`
	JoinedAt      time.Time `json:"joined_at,omitzero"`
	ChatType      string    `json:"chat_type,omitempty"`
	Wake          string    `json:"wake"`
	MessageCursor time.Time `json:"message_cursor"`
	EventCursor   time.Time `json:"event_cursor"`
	Backlog       []string  `json:"backlog"`
	PageToken     string    `json:"page_token"`
	FrozenUpper   time.Time `json:"frozen_upper"`
}

type ledger050 struct {
	Version          int                      `json:"version"`
	IdentityUserID   string                   `json:"identity_user_id,omitempty"`
	IdentitySeed     string                   `json:"identity_seed,omitempty"`
	IdentityDomainID string                   `json:"identity_domain_id,omitempty"`
	Subscription     SubscriptionState        `json:"subscription"`
	Spaces           map[string]*space050     `json:"spaces"`
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
	if fields["spaces_listed_at"] != nil {
		t.Fatal("old ledger gained inventory metadata")
	}
	var emptyFields map[string]json.RawMessage
	empty, _ := json.Marshal(&SpaceState{})
	_ = json.Unmarshal(empty, &emptyFields)
	if emptyFields["history_start"] != nil {
		t.Fatal("empty start was serialized")
	}
	floor := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	patch, _ := json.Marshal(map[string]any{"history_floor": floor, "spaces_listed_at": floor})
	if err := json.Unmarshal(patch, l); err != nil {
		t.Fatal(err)
	}
	l.Spaces["spaces/room"] = &SpaceState{Kind: "room", Wake: "all", MessageCursor: floor, EventCursor: floor}
	patch, _ = json.Marshal(map[string]any{"history_start": floor})
	if err := json.Unmarshal(patch, l.Spaces["spaces/room"]); err != nil {
		t.Fatal(err)
	}
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
	if fields["spaces_listed_at"] == nil {
		t.Fatal("inventory boundary did not persist")
	}
	var spaces map[string]map[string]json.RawMessage
	_ = json.Unmarshal(fields["spaces"], &spaces)
	if spaces["spaces/room"]["history_start"] == nil {
		t.Fatal("space start did not persist")
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
	delete(fields, "spaces_listed_at")
	delete(spaces["spaces/room"], "history_start")
	fields["spaces"], _ = json.Marshal(spaces)
	var actualSpaces map[string]map[string]json.RawMessage
	_ = json.Unmarshal(actual["spaces"], &actualSpaces)
	actual["spaces"], _ = json.Marshal(actualSpaces)
	if !reflect.DeepEqual(fields, actual) {
		t.Fatal("rollback changed more than the floor")
	}
}
