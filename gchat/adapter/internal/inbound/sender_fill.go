package inbound

import (
	"context"
	"time"

	"gchat-adapter/internal/puller"
	api "google.golang.org/api/chat/v1"
)

type senderSource interface {
	GetMessage(context.Context, string) (*api.Message, error)
	GetMembership(context.Context, string, string) (*api.Membership, error)
}

const senderFillTimeout = 3 * time.Second

// SenderFill resolves incomplete senders for one processor generation.
type SenderFill struct {
	source senderSource
	log    puller.Logger
	now    func() time.Time
	bound  time.Duration
}

func NewSenderFill(source senderSource, log puller.Logger) *SenderFill {
	return &SenderFill{source: source, log: log, now: time.Now, bound: senderFillTimeout}
}
