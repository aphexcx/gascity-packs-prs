// Package neversilent reports explicit delivery and health failures. Silence
// alone is never evidence that an agent is unavailable.
package neversilent

import (
	"encoding/json"
	"io"
	"os"
	"time"
)

const (
	FailureWait   = 3 * time.Minute
	HealthMaxAge  = 5 * time.Minute
	OwnerCooldown = 6 * time.Hour
	MailRetry     = time.Minute
	MailLifetime  = 24 * time.Hour
)

type Health struct {
	State      string    `json:"state"`
	Since      time.Time `json:"since,omitempty"`
	CheckedAt  time.Time `json:"checked_at,omitempty"`
	ResetsAt   time.Time `json:"resets_at,omitempty"`
	AgeSeconds float64   `json:"age_seconds"`
	Reason     string    `json:"reason,omitempty"`
}

// ReadHealth never includes file contents or filesystem errors in diagnostics.
// A partial write, unrecognized state or stale writer is unknown, not down.
func ReadHealth(path string, now time.Time, maxAge time.Duration) Health {
	unknown := func(reason string) Health { return Health{State: "unknown", Reason: reason} }
	f, err := os.Open(path)
	if err != nil {
		return unknown("unavailable")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 16*1024+1))
	if err != nil || len(b) > 16*1024 {
		return unknown("unreadable or oversized")
	}
	var h Health
	if json.Unmarshal(b, &h) != nil {
		return unknown("invalid JSON or timestamp")
	}
	switch h.State {
	case "ok", "no_seat", "seat_at_cap", "session_down":
	default:
		return unknown("unrecognized state")
	}
	if h.Since.IsZero() || h.CheckedAt.IsZero() {
		return unknown("missing timestamp")
	}
	if h.CheckedAt.After(now) || h.Since.After(h.CheckedAt) {
		return unknown("inconsistent timestamp")
	}
	h.AgeSeconds = now.Sub(h.CheckedAt).Seconds()
	if now.Sub(h.CheckedAt) > maxAge {
		return unknown("stale")
	}
	h.Reason = ""
	return h
}
