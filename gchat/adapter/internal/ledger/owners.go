package ledger

import "time"

// Declarations only: the tests come first.
type OwnerRequest struct {
	CodeHash  string    `json:"code_hash"`
	Identity  string    `json:"identity"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
}

type OwnerEvent struct {
	At      time.Time `json:"at"`
	Event   string    `json:"event"`
	Claim   string    `json:"claim,omitempty"`
	Owner   string    `json:"owner"`
	Message string    `json:"message"`
}

const OwnerEventLimit = 500

func (l *Ledger) AddOwnerEvent(e OwnerEvent) {}
