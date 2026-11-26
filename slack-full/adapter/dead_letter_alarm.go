package main

import (
	"context"
	"time"
)

// Declarations for the alarm contract tests.
type deadLetterAlarm struct {
	path  string
	limit time.Duration
	now   func() time.Time
}

func newDeadLetterAlarm(cfg config) *deadLetterAlarm {
	return &deadLetterAlarm{now: time.Now, limit: 120 * time.Second}
}

func (a *deadLetterAlarm) pass(ctx context.Context) {}

func runDeadLetterAlarm(ctx context.Context, cfg config) {}
