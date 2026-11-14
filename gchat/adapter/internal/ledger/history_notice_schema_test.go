// Frozen record shapes from 4d14c7c5d52a468a2af7e387175ff4fb20366ec5.
package ledger_test

import (
	"gchat-adapter/internal/ledger"
	"gchat-adapter/internal/wire"
	"time"
)

type parent050Ledger struct {
	saveGuard func() error

	Version        int    `json:"version"`
	IdentityUserID string `json:"identity_user_id,omitempty"`
	IdentitySeed   string `json:"identity_seed,omitempty"`
	// IdentityDomainID is the Workspace domain the identity belongs to, as Chat
	// reports it on the identity's own messages; senders in it are the organization.
	IdentityDomainID string                            `json:"identity_domain_id,omitempty"`
	Subscription     parent050SubscriptionState        `json:"subscription"`
	Spaces           map[string]*parent050SpaceState   `json:"spaces"`
	Transport        ledger.TransportDedup             `json:"transport"`
	Messages         map[string]*parent050MessageState `json:"messages"`
	Changes          map[string]*parent050ChangeState  `json:"changes"`
	Sends            map[string]*parent050SendState    `json:"sends"`
	Unresolved       map[string]*parent050ChangeState  `json:"unresolved"`
	Quarantine       []parent050QuarantinedEvent       `json:"quarantine"`
	Counters         map[string]int64                  `json:"counters"`
}

type parent050SubscriptionState struct {
	Name   string    `json:"name"`
	State  string    `json:"state"`
	Expiry time.Time `json:"expiry"`
	Target string    `json:"target"`
}

type parent050SpaceState struct {
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name,omitempty"`
	// JoinedAt is the earliest history allowed by an observed membership. Old
	// ledgers have no known join bound; never infer one from a moving cursor.
	JoinedAt time.Time `json:"joined_at,omitzero"`
	// ChatType distinguishes named SPACE from GROUP_CHAT; both have gc kind room.
	ChatType      string    `json:"chat_type,omitempty"`
	Wake          string    `json:"wake"`
	MessageCursor time.Time `json:"message_cursor"`
	EventCursor   time.Time `json:"event_cursor"`
	Backlog       []string  `json:"backlog"`
	PageToken     string    `json:"page_token"`
	FrozenUpper   time.Time `json:"frozen_upper"`
}

type parent050MessageState struct {
	Revision time.Time `json:"revision"`
	Deleted  bool      `json:"deleted"`
	Thread   string    `json:"thread"`
	Mine     bool      `json:"mine"`
	SendKey  string    `json:"send_key"`
}

type parent050ChangeState struct {
	ID        string    `json:"id"`
	Space     string    `json:"space"`
	Message   string    `json:"message"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"` // intent | pending | committed | unresolved
	ReceiptID string    `json:"receipt_id"`
	Attempts  int       `json:"attempts"`
	Created   time.Time `json:"created"`
	Event     string    `json:"event"`
	Reason    string    `json:"reason"`
	CatchUp   bool      `json:"catch_up,omitempty"` // adapter added the delivery header
	// ProvenanceFramed marks adapter-authored frames; legacy records default
	// to false so user text resembling a frame is never trusted or stripped.
	ProvenanceFramed bool `json:"provenance_framed,omitempty"`
	// NextAttempt is the earliest time of the next POST of a change that gc
	// answered "not delivered" four or more times.
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// Posted is when the latest POST was made; a pending receipt's deadline
	// counts from it. Records written before it existed count from Created.
	Posted time.Time `json:"posted,omitzero"`
	// Cause says, in one word of a closed set, why the change is waiting or
	// unresolved. It is for status and the raise mail; no retry reads it.
	// Reason "retry" stays the mark of a clean retry.
	Cause string `json:"cause,omitempty"`
	// Fragment marks a change gc answered, on any POST of its current life,
	// that a fragment of the message is in a terminal ("partial", or "held"
	// with a "partial" member). Such a change is not POSTed after its fourth
	// POST, or after that answer when it came later, whatever gc answers to
	// its other POSTs. A replay clears it.
	Fragment bool `json:"fragment,omitempty"`
	// Raised is when the city's agent was first told about this change. It
	// is set only after the mail was sent.
	Raised time.Time `json:"raised,omitzero"`
	// RaisedEnd is when the city's agent was told that this change ended
	// unresolved. A change is mailed at most twice in one life (a replay
	// starts a new one): once while it waits and once when it ends, the second only when the end is not the retry bound
	// the first mail named.
	RaisedEnd time.Time `json:"raised_end,omitzero"`
}

type parent050SendState struct {
	Key       string               `json:"key"`
	InReplyTo string               `json:"in_reply_to,omitempty"`
	SpaceName string               `json:"space_name,omitempty"`
	Space     string               `json:"space"`
	Thread    string               `json:"thread"`
	Parts     []parent050SendPart  `json:"parts"`
	Receipt   *wire.PublishReceipt `json:"receipt"`
	Recorded  bool                 `json:"recorded"`
	Created   time.Time            `json:"created"`
	// Optional command recovery data; old callback-only intents remain readable.
	Request         *wire.PublishRequest `json:"request,omitempty"`
	Upload          *parent050SendUpload `json:"upload,omitempty"`
	RecordPending   bool                 `json:"record_pending,omitempty"`
	NotRecordable   bool                 `json:"not_recordable,omitempty"`
	RecordingReason string               `json:"recording_reason,omitempty"`
	// Separate from Receipt: a recording refusal must not downgrade delivery.
	RecordingFailureKind string `json:"recording_failure_kind,omitempty"`
}

type parent050SendUpload struct {
	Path         string `json:"path"`
	ResourceName string `json:"resource_name,omitempty"`
	UploadToken  string `json:"upload_token,omitempty"`
}

type parent050SendPart struct {
	RequestID   string `json:"request_id"`
	Text        string `json:"text"`
	MessageName string `json:"message_name"`
}

type parent050QuarantinedEvent struct {
	PubSubID string    `json:"pubsub_id"`
	Reason   string    `json:"reason"`
	Path     string    `json:"path"`
	At       time.Time `json:"at"`
}
