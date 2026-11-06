// Package wire holds the exact JSON shapes gc exchanges with an extmsg adapter.
// Field names and casing come from gc's internal/extmsg/types.go and
// internal/api/huma_types_extmsg.go; do not "fix" them.
package wire

import "time"

// Conversation is gc's ConversationRef. ScopeID is required by gc (the city name).
type Conversation struct {
	ScopeID              string `json:"scope_id"`
	Provider             string `json:"provider"`
	AccountID            string `json:"account_id"`
	ConversationID       string `json:"conversation_id"`
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	Kind                 string `json:"kind"` // "dm" | "room" | "thread"
}

type Actor struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	IsBot       bool   `json:"is_bot"`
}

type Attachment struct {
	ProviderID string `json:"provider_id"`
	URL        string `json:"url"`
	MIMEType   string `json:"mime_type,omitempty"`
}

// InboundMessage is gc's ExternalInboundMessage.
type InboundMessage struct {
	ProviderMessageID string       `json:"provider_message_id"`
	Conversation      Conversation `json:"conversation"`
	Actor             Actor        `json:"actor"`
	Text              string       `json:"text"`
	ExplicitTarget    string       `json:"explicit_target,omitempty"`
	ReplyToMessageID  string       `json:"reply_to_message_id,omitempty"`
	Attachments       []Attachment `json:"attachments,omitempty"`
	DedupKey          string       `json:"dedup_key,omitempty"`
	ReceivedAt        time.Time    `json:"received_at"`
}

// InboundEnvelope is the body of POST /v0/city/{city}/extmsg/inbound (pre-normalized path).
type InboundEnvelope struct {
	Message InboundMessage `json:"message"`
}

// InboundResponse is what gc answers. Delivery.Status is one of
// delivered | pending | partial | failed | no_route.
// ReceiptID is set whenever Delivery is present; poll .../receipts/{id} while
// Status is pending.
type InboundResponse struct {
	Delivery struct {
		Status    string `json:"status"`
		ReceiptID string `json:"receipt_id,omitempty"`
	} `json:"delivery"`
}

// ReceiptResponse is gc's answer to GET .../extmsg/inbound/receipts/{id}:
// State is pending | concluded | unknown; Delivery is set when concluded.
type ReceiptResponse struct {
	State    string `json:"state"`
	Delivery struct {
		Status string `json:"status"`
	} `json:"delivery"`
}

// PublishRequest is what gc POSTs to <callback_url>/publish.
type PublishRequest struct {
	SessionID        string            `json:"session_id,omitempty"`
	Conversation     Conversation      `json:"conversation"`
	Text             string            `json:"text"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// PublishReceipt is the adapter's answer, always with HTTP 200. Metadata values
// must be strings; RetryAfter is a Go duration in nanoseconds.
type PublishReceipt struct {
	MessageID    string            `json:"message_id,omitempty"`
	Conversation Conversation      `json:"conversation"`
	Delivered    bool              `json:"delivered"`
	FailureKind  string            `json:"failure_kind,omitempty"` // transient | rate_limited | auth | not_found | permanent
	RetryAfter   int64             `json:"retry_after,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// Capabilities is serialized by gc with PascalCase keys (untagged struct there).
type Capabilities struct {
	SupportsAttachments        bool `json:"SupportsAttachments"`
	SupportsChildConversations bool `json:"SupportsChildConversations"`
	MaxMessageLength           int  `json:"MaxMessageLength"`
}

// Registration is the body of POST /v0/city/{city}/extmsg/adapters.
type Registration struct {
	Provider          string       `json:"provider"`
	AccountID         string       `json:"account_id"`
	Name              string       `json:"name,omitempty"`
	CallbackURL       string       `json:"callback_url,omitempty"`
	Capabilities      Capabilities `json:"capabilities"`
	ReplyInstructions string       `json:"reply_instructions,omitempty"`
}

// OutboundRequest is the body of POST /v0/city/{city}/extmsg/outbound.
type OutboundRequest struct {
	SessionID        string       `json:"session_id"`
	Conversation     Conversation `json:"conversation"`
	Text             string       `json:"text,omitempty"`
	ReplyToMessageID string       `json:"reply_to_message_id,omitempty"`
	IdempotencyKey   string       `json:"idempotency_key,omitempty"`
}

// OutboundResult is gc's PascalCase answer; TranscriptEntry nil means gc
// delivered but did not record.
type OutboundResult struct {
	Receipt struct {
		Conversation Conversation      `json:"Conversation"`
		Metadata     map[string]string `json:"Metadata"`
		Delivered    bool              `json:"Delivered"`
		MessageID    string            `json:"MessageID"`
		FailureKind  string            `json:"FailureKind"`
		RetryAfter   int64             `json:"RetryAfter"`
	} `json:"Receipt"`
	TranscriptEntry *map[string]any `json:"TranscriptEntry"`
}

// RequestHeader is required by gc on every mutation. Any non-empty value.
const RequestHeader = "X-GC-Request"
