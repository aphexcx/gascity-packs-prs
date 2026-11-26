package main

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const heldOfferInterval = time.Minute

func (receipt deliveryReceipt) heldAnswer(gated bool, err error) bool {
	// The coalescer reports a withheld vouch as an error even when gc
	// answered the POST. A transport failure carries no such assurance.
	if !gated || (err != nil && !errors.Is(err, errDeliveryUnvouched)) ||
		receipt.postStatus < 200 || receipt.postStatus >= 300 || !receipt.present || receipt.followUpLanded ||
		normalizeReceiptStatus(receipt.status) != "held" || (receipt.deliveredOK && receipt.delivered > 0) {
		return false
	}
	for _, member := range receipt.members {
		if member.DeliveredOK && member.Delivered > 0 {
			return false
		}
		if strings.TrimSpace(member.Status) == "" {
			continue
		}
		switch normalizeReceiptStatus(member.Status) {
		case "held", "failed":
		default:
			return false
		}
	}
	return true
}

func (r *inboundRecovery) heldClockActive(e inboundRecoveryEntry, now time.Time) bool {
	if !r.gated || e.LastHeldAnswerAt.IsZero() || e.inactive() || e.Landed || e.Reaction ||
		e.PendingReceipt || e.PendingReceiptID != "" {
		return false
	}
	// Explicit permission to replay older history is for that operator
	// request. It never extends the automatic clock.
	return e.DeadLetteredAt.IsZero() || (r.replayMaxAge > 0 && now.Sub(e.DeadLetteredAt) <= r.replayMaxAge)
}

var errOfferState = errors.New("could not persist state before recovery offer")

func (r *inboundRecovery) saveBeforeOffer(key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.state.Entries[key]
	if !ok || current.inactive() {
		return errRecoveryInactive
	}
	cleared := current
	cleared.LastHeldAnswerAt = time.Time{}
	r.state.Entries[key] = cleared
	if err := r.saveLocked(); err != nil {
		r.state.Entries[key] = current
		return fmt.Errorf("%w: %w", errOfferState, err)
	}
	return nil
}
