package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestMediaAndJoinRecordsShareOneLedger(t *testing.T) {
	const room = "spaces/example"
	const message = room + "/messages/one"
	const attachment = message + "/attachments/example"
	const digest = "0123456789abcdef"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	want := New()
	want.Media = map[string]*MediaState{
		message: {
			Files:         map[string]string{digest: attachment},
			Sources:       map[string]time.Time{digest: at},
			Remove:        map[string]bool{digest: true},
			DeletePending: true,
			Revision:      at,
			Retained:      []string{message + "/attachments/kept"},
		},
	}
	want.MediaRoots = []string{t.TempDir()}
	want.JoinRequests = map[string]*JoinRequest{
		room: {CodeHash: "example-hash", Identity: "users/123456789", CreatedAt: at, ExpiresAt: at.Add(24 * time.Hour)},
	}
	want.JoinAttempts = []JoinAttempt{{At: at, Space: room, OwnerMessage: "spaces/dm/messages/approval", Result: "joined"}}

	readObject := func(path string) map[string]any {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	saved := readObject(path)
	released := readObject("testdata/ledger-0.5.0.json")
	for _, key := range []string{"media", "media_roots", "join_requests", "join_attempts"} {
		value, present := saved[key]
		if !present {
			t.Fatalf("saved ledger missing key %q", key)
		}
		released[key] = value
	}

	loaded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, want) {
		t.Fatal("save/load changed the ledger containing media and join records")
	}
	clone := loaded.Clone()
	if !reflect.DeepEqual(clone, want) {
		t.Fatal("clone changed the ledger containing media and join records")
	}
	media := clone.Media[message]
	delete(media.Files, digest)
	delete(media.Sources, digest)
	delete(media.Remove, digest)
	media.DeletePending = false
	media.Revision = at.Add(time.Minute)
	media.Retained[0] = attachment
	clone.MediaRoots[0] = "changed"
	clone.JoinRequests[room].Used = true
	clone.JoinAttempts[0].Result = "refused_invited"
	if !reflect.DeepEqual(loaded, want) {
		t.Fatal("mutating the clone changed the loaded ledger")
	}

	data, err := json.Marshal(released)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Save(path); err != nil {
		t.Fatal(err)
	}
	if got := readObject(path); !reflect.DeepEqual(got, released) {
		t.Fatal("load/save changed the released fixture with media and join records")
	}
}
