package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Per-channel inbound delivery policy (gp-729 item 6).
//
// delivery_policy.json selects, per channel, how the inbound coalescer
// paces deliveries to bound sessions:
//
//	{
//	  "channels": {
//	    "C0BKF28CYUE": {"mode": "digest", "interval_minutes": 10},
//	    "C0AGENDA111": {"mode": "immediate"}
//	  }
//	}
//
//   - "immediate" (and any channel absent from the file, and an absent
//     file): the coalescer's short burst-debounce window applies —
//     the day-one default, no per-channel opt-in required.
//   - "digest": accumulated messages deliver verbatim in ONE batch
//     every interval_minutes. Nothing is dropped or summarized; the
//     knob only trades latency for fewer session wake-ups on channels
//     the operator flips by name, deliberately.
//
// Operator-edited off-band; same SIGHUP-or-restart reload contract as
// peer_bots.json (nil snapshot from Stage means file absent — clear the
// registry by writing `{}`, never by `rm`).

// maxDeliveryPolicyBytes bounds the registry file read, matching the
// other operator registries' defensive cap.
const maxDeliveryPolicyBytes = 1 << 20

// deliveryPolicyModeImmediate / deliveryPolicyModeDigest are the two
// accepted per-channel modes.
const (
	deliveryPolicyModeImmediate = "immediate"
	deliveryPolicyModeDigest    = "digest"
)

// deliveryPolicyMaxIntervalMinutes caps digest intervals: beyond two
// hours a "digest" is indistinguishable from an outage to the humans in
// the channel, and the liveness watchdog would start second-guessing
// the silence.
const deliveryPolicyMaxIntervalMinutes = 120

type deliveryPolicyEntry struct {
	Mode            string `json:"mode"`
	IntervalMinutes int    `json:"interval_minutes,omitempty"`
}

type deliveryPolicyFile struct {
	Channels map[string]deliveryPolicyEntry `json:"channels"`
}

// deliveryPolicySnapshot is the staged/committed parse result.
type deliveryPolicySnapshot struct {
	channels map[string]deliveryPolicyEntry
}

// deliveryPolicyRegistry is the in-memory snapshot of
// delivery_policy.json. Nil-safe: a nil registry (or an absent file)
// reports every channel as immediate.
type deliveryPolicyRegistry struct {
	mu       sync.RWMutex
	path     string
	channels map[string]deliveryPolicyEntry
}

func newDeliveryPolicyRegistry(path string) (*deliveryPolicyRegistry, error) {
	r := &deliveryPolicyRegistry{path: path, channels: map[string]deliveryPolicyEntry{}}
	snap, err := r.Stage()
	if err != nil {
		return nil, err
	}
	r.Commit(snap)
	return r, nil
}

// Stage re-parses the backing file without touching live state. A nil
// snapshot with nil error means the file is absent — the caller's
// Commit then preserves live state (registry-clearing is `{}`, not rm).
func (r *deliveryPolicyRegistry) Stage() (*deliveryPolicySnapshot, error) {
	if r == nil {
		return nil, nil
	}
	channels, err := parseDeliveryPolicy(r.path)
	if err != nil {
		return nil, err
	}
	if channels == nil {
		return nil, nil
	}
	return &deliveryPolicySnapshot{channels: channels}, nil
}

// Commit installs a staged snapshot. Nil snapshot preserves live state.
func (r *deliveryPolicyRegistry) Commit(snap *deliveryPolicySnapshot) {
	if r == nil || snap == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels = snap.channels
}

// parseDeliveryPolicy reads and validates the policy file. Returns
// (nil, nil) when the file does not exist. Validation is fail-closed:
// a corrupt or out-of-range entry rejects the whole file so a bad
// write cannot silently change delivery pacing.
func parseDeliveryPolicy(path string) (map[string]deliveryPolicyEntry, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, maxDeliveryPolicyBytes))
	dec.DisallowUnknownFields()
	var file deliveryPolicyFile
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	channels := make(map[string]deliveryPolicyEntry, len(file.Channels))
	for channel, entry := range file.Channels {
		if channel == "" {
			return nil, fmt.Errorf("parse %s: empty channel id key", path)
		}
		switch entry.Mode {
		case deliveryPolicyModeImmediate:
			if entry.IntervalMinutes != 0 {
				return nil, fmt.Errorf("parse %s: channel %s: interval_minutes is only valid with mode %q", path, channel, deliveryPolicyModeDigest)
			}
		case deliveryPolicyModeDigest:
			if entry.IntervalMinutes < 1 || entry.IntervalMinutes > deliveryPolicyMaxIntervalMinutes {
				return nil, fmt.Errorf("parse %s: channel %s: interval_minutes must be 1..%d, got %d", path, channel, deliveryPolicyMaxIntervalMinutes, entry.IntervalMinutes)
			}
		default:
			return nil, fmt.Errorf("parse %s: channel %s: mode must be %q or %q, got %q", path, channel, deliveryPolicyModeImmediate, deliveryPolicyModeDigest, entry.Mode)
		}
		channels[channel] = entry
	}
	return channels, nil
}

// digestInterval returns the digest interval for channel and true when
// the operator flipped it to digest mode; (0, false) means immediate
// (the default for unlisted channels, absent files, and nil registries).
func (r *deliveryPolicyRegistry) digestInterval(channel string) (time.Duration, bool) {
	if r == nil {
		return 0, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.channels[channel]
	if !ok || entry.Mode != deliveryPolicyModeDigest {
		return 0, false
	}
	return time.Duration(entry.IntervalMinutes) * time.Minute, true
}

// Len reports the number of configured channels. Nil-safe.
func (r *deliveryPolicyRegistry) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.channels)
}
