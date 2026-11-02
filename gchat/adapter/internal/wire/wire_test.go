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
		CallbackURL:       "http://example.invalid/v0/city/examplecity/svc/gchat",
		Capabilities:      Capabilities{SupportsAttachments: true, MaxMessageLength: 32000},
		ReplyInstructions: "gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"})
	if err != nil {
		t.Fatal(err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	want := `{"provider":"gchat","account_id":"example","name":"gchat adapter","callback_url":"http://example.invalid/v0/city/examplecity/svc/gchat","capabilities":{"SupportsAttachments":true,"SupportsChildConversations":false,"MaxMessageLength":32000},"reply_instructions":"gc gchat reply-current --conversation-id {conversation_id} [--thread {thread_ts}] --body-file <path>"}`
	if string(b) != want {
		t.Fatalf("got %s\nwant %s", b, want)
	}
}

// These response fixtures follow gc's InboundResult, InboundReceiptStatus,
// and OutboundResult, whose outer and nested keys deliberately differ in case.
func TestInboundResponseJSON(t *testing.T) {
	var response InboundResponse
	if err := json.Unmarshal([]byte(`{"Message":{},"delivery":{"receipt_id":"ir-1","status":"pending","delivered_bytes":0,"expected_bytes":10}}`), &response); err != nil {
		t.Fatal(err)
	}
	if response.Delivery.Status != "pending" || response.Delivery.ReceiptID != "ir-1" {
		t.Fatalf("delivery: %+v", response.Delivery)
	}
}

func TestReceiptResponseJSON(t *testing.T) {
	for _, tc := range []struct{ body, state, status string }{
		{`{"receipt_id":"ir-1","state":"pending"}`, "pending", ""},
		{`{"receipt_id":"ir-1","state":"concluded","delivery":{"receipt_id":"ir-1","status":"delivered"}}`, "concluded", "delivered"},
		{`{"receipt_id":"ir-1","state":"concluded","delivery":{"receipt_id":"ir-1","status":"pending"}}`, "concluded", "pending"},
		{`{"receipt_id":"ir-1","state":"unknown"}`, "unknown", ""},
	} {
		t.Run(tc.state+"/"+tc.status, func(t *testing.T) {
			var response ReceiptResponse
			if err := json.Unmarshal([]byte(tc.body), &response); err != nil {
				t.Fatal(err)
			}
			if response.State != tc.state || response.Delivery.Status != tc.status {
				t.Fatalf("receipt: %+v", response)
			}
		})
	}
}

func TestOutboundResultJSON(t *testing.T) {
	t.Run("failure hints survive decoding", func(t *testing.T) {
		var result OutboundResult
		if err := json.Unmarshal([]byte(`{"Receipt":{"Delivered":false,"FailureKind":"rate_limited","RetryAfter":60000000000},"TranscriptEntry":null}`), &result); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(result.Receipt)
		if err != nil {
			t.Fatal(err)
		}
		var receipt map[string]any
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt["FailureKind"] != "rate_limited" || receipt["RetryAfter"] != float64(60000000000) {
			t.Fatalf("discarded failure hints: %s", data)
		}
	})
	for _, tc := range []struct {
		entry    string
		recorded bool
	}{
		{"null", false},
		{`{"id":"entry-1"}`, true},
	} {
		t.Run(tc.entry, func(t *testing.T) {
			body := `{"Receipt":{"Delivered":true,"MessageID":"spaces/AAA/messages/BBB"},"DeliveryContext":null,"TranscriptEntry":` + tc.entry + `}`
			var result OutboundResult
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
			if !result.Receipt.Delivered || result.Receipt.MessageID != "spaces/AAA/messages/BBB" || (result.TranscriptEntry != nil) != tc.recorded {
				t.Fatalf("result: %+v", result)
			}
		})
	}
}
