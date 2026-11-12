package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gchat-adapter/internal/wire"
)

// Frozen SendState schema from adapter 0.5.0, before reply provenance. Its
// encoding/json decoder ignores the optional fields added by this change.
type send050 struct {
	Key                  string               `json:"key"`
	Space                string               `json:"space"`
	Thread               string               `json:"thread"`
	Parts                []SendPart           `json:"parts"`
	Receipt              *wire.PublishReceipt `json:"receipt"`
	Recorded             bool                 `json:"recorded"`
	Created              time.Time            `json:"created"`
	Request              *wire.PublishRequest `json:"request,omitempty"`
	Upload               *SendUpload          `json:"upload,omitempty"`
	RecordPending        bool                 `json:"record_pending,omitempty"`
	NotRecordable        bool                 `json:"not_recordable,omitempty"`
	RecordingReason      string               `json:"recording_reason,omitempty"`
	RecordingFailureKind string               `json:"recording_failure_kind,omitempty"`
}

func TestReplyLedgerCompatibilityBothDirections(t *testing.T) {
	l, err := Load("testdata/ledger-0.5.0.json", true)
	if err != nil {
		t.Fatal(err)
	}
	message := "spaces/example/messages/question"
	p := &wire.PublishRequest{Conversation: wire.Conversation{ConversationID: "spaces/example"}, ReplyToMessageID: "spaces/example/threads/question", IdempotencyKey: "reply"}
	l.Sends["reply"] = &SendState{Key: "reply", Space: p.Conversation.ConversationID, Thread: p.ReplyToMessageID,
		InReplyTo: message, SpaceName: "Example", Request: p, RecordPending: true,
		Receipt: &wire.PublishReceipt{Delivered: true, MessageID: "spaces/example/messages/answer"},
		Parts:   []SendPart{{RequestID: "reply-1", MessageName: "spaces/example/messages/answer"}}}
	path := filepath.Join(t.TempDir(), "ledger.json")
	if err := l.Save(path); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Load(path, true)
	if err != nil || reloaded.Sends["reply"].InReplyTo != message {
		t.Fatalf("new reader: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	var old map[string]*send050
	if err := json.Unmarshal(document["sends"], &old); err != nil {
		t.Fatal(err)
	}
	if v := old["reply"]; !v.Receipt.Delivered || v.Space != p.Conversation.ConversationID || v.Thread != p.ReplyToMessageID || !v.RecordPending || v.Request.IdempotencyKey != "reply" {
		t.Fatalf("0.5.0 reader lost delivery: %+v", v)
	}
	document["sends"], err = json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	rolled, err := Load(path, true)
	if err != nil {
		t.Fatal(err)
	}
	v := rolled.Sends["reply"]
	if v.InReplyTo != "" || v.SpaceName != "" || !v.Receipt.Delivered || !v.RecordPending || v.Thread != p.ReplyToMessageID {
		t.Fatalf("rollback should drop only optional provenance: %+v", v)
	}
	plain, err := json.Marshal(&SendState{Key: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(plain, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["in_reply_to"]; present {
		t.Fatal("empty reference must be omitted")
	}
}
