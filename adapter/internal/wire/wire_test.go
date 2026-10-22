package wire

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestInboundMessageJSON(t *testing.T) {
	m := InboundMessage{
		ProviderMessageID: "spaces/AAA/messages/BBB",
		Conversation: Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example",
			ConversationID: "spaces/AAA", Kind: "room"},
		Actor:            Actor{ID: "users/1", DisplayName: "Pat", IsBot: false},
		Text:             "hello",
		ReplyToMessageID: "spaces/AAA/threads/CCC",
		Attachments:      []Attachment{{ProviderID: "a1", URL: "file:///tmp/x.png", MIMEType: "image/png"}},
		ReceivedAt:       time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	b, err := json.Marshal(InboundEnvelope{Message: m})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"message":{"provider_message_id":"spaces/AAA/messages/BBB","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/AAA","kind":"room"},"actor":{"id":"users/1","display_name":"Pat","is_bot":false},"text":"hello","reply_to_message_id":"spaces/AAA/threads/CCC","attachments":[{"provider_id":"a1","url":"file:///tmp/x.png","mime_type":"image/png"}],"received_at":"2026-01-02T03:04:05Z"}}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestPublishReceiptJSON(t *testing.T) {
	r := PublishReceipt{MessageID: "spaces/AAA/messages/BBB",
		Conversation: Conversation{ScopeID: "examplecity", Provider: "gchat", AccountID: "example", ConversationID: "spaces/AAA", Kind: "room"},
		Delivered:    true, Metadata: map[string]string{"thread": "spaces/AAA/threads/CCC", "parts": "2"}}
	b, _ := json.Marshal(r)
	want := `{"message_id":"spaces/AAA/messages/BBB","conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/AAA","kind":"room"},"delivered":true,"metadata":{"parts":"2","thread":"spaces/AAA/threads/CCC"}}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
	f := PublishReceipt{Conversation: r.Conversation, Delivered: false, FailureKind: "rate_limited", RetryAfter: int64(2 * time.Second)}
	b, _ = json.Marshal(f)
	want = `{"conversation":{"scope_id":"examplecity","provider":"gchat","account_id":"example","conversation_id":"spaces/AAA","kind":"room"},"delivered":false,"failure_kind":"rate_limited","retry_after":2000000000}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

func TestRegistrationCapabilitiesPascalCase(t *testing.T) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	// Keep the golden fixture's literal angle brackets; both forms are valid JSON.
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(Registration{Provider: "gchat", AccountID: "example", Name: "gchat adapter",
		CallbackURL:       "http://127.0.0.1:8372/v0/city/examplecity/svc/gchat",
		Capabilities:      Capabilities{SupportsAttachments: true, MaxMessageLength: 32000},
		ReplyInstructions: "gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"})
	if err != nil {
		t.Fatal(err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	want := `{"provider":"gchat","account_id":"example","name":"gchat adapter","callback_url":"http://127.0.0.1:8372/v0/city/examplecity/svc/gchat","capabilities":{"SupportsAttachments":true,"SupportsChildConversations":false,"MaxMessageLength":32000},"reply_instructions":"gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}
