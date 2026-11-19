package ledger

import "time"

// JoinRequest retains only a digest of the one-time code. It is scoped to a
// room by its map key and to the authenticated identity, including on restart.
type JoinRequest struct {
	CodeHash  string    `json:"code_hash"`
	Identity  string    `json:"identity"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
}

// JoinAttempt is an audit record, never a copy of Google's or the owner's text.
type JoinAttempt struct {
	At           time.Time `json:"at"`
	Space        string    `json:"space"`
	OwnerMessage string    `json:"owner_message"`
	Result       string    `json:"result"`
}
