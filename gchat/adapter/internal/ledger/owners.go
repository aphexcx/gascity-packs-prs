package ledger

import "time"

// OwnerRequest is one removal's request, kept as a JoinRequest is: only a
// digest of its one-time code, the identity it belongs to and its times. It
// is scoped to the Chat user id to remove by its map key.
type OwnerRequest struct {
	CodeHash  string    `json:"code_hash"`
	Identity  string    `json:"identity"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
}

// OwnerEvent is one fact the adapter saw: a code the door took or ended, or
// an owner the door removed. It carries Chat ids and names of messages only,
// never a code, a message's text, a person's name or an address.
type OwnerEvent struct {
	At time.Time `json:"at"`
	// Event is taken, tries_ended or removed.
	Event string `json:"event"`
	// Claim is the claim id the door gave, when it gave one.
	Claim string `json:"claim,omitempty"`
	// Owner is the sender's Chat user id, or the removed owner's.
	Owner string `json:"owner"`
	// Message is the name of the message that carried the code or the yes.
	Message string `json:"message"`
}

// OwnerEventLimit is how many owner events the state keeps, the newest.
const OwnerEventLimit = 500

// AddOwnerEvent appends one event and keeps the newest OwnerEventLimit.
func (l *Ledger) AddOwnerEvent(e OwnerEvent) {
	l.OwnerEvents = append(l.OwnerEvents, e)
	if n := len(l.OwnerEvents) - OwnerEventLimit; n > 0 {
		l.OwnerEvents = append([]OwnerEvent(nil), l.OwnerEvents[n:]...)
	}
}
