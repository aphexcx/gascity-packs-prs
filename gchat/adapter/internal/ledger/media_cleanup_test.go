package ledger_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gchat-adapter/internal/ledger"
)

func TestMediaCleanupLedgerCompatibility(t *testing.T) {
	const fixture = "testdata/ledger-0.5.0.json"
	before, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(before, &raw); err != nil {
		t.Fatal(err)
	}
	// Only resource identifiers, opaque digests and recovery state are added.
	raw["media"] = json.RawMessage(`{"spaces/example/messages/one":{"delete_pending":true,"files":{"0123456789abcdef":"spaces/example/messages/one/attachments/file"}}}`)
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	l, err := ledger.Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Clone().Save(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var loaded map[string]json.RawMessage
	if err = json.Unmarshal(after, &loaded); err != nil {
		t.Fatal(err)
	}
	var got, want any
	json.Unmarshal(loaded["media"], &got)
	json.Unmarshal(raw["media"], &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("cleanup checkpoint lost on load, clone or save")
	}
	delete(loaded, "media")
	clean, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(clean, &got)
	json.Unmarshal(before, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("released ledger state changed")
	}
}
