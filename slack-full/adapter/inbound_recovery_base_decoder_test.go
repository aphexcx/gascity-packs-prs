package main

// Decoder and wire structs copied from be225cc1 for rollback compatibility tests.
import (
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"path/filepath"
	"time"
)

type round2BaseInboundRecoveryEntry struct {
	ManualRecovery    bool                   `json:"manual_recovery,omitempty"`
	TrustedFilePaths  []string               `json:"trusted_file_paths,omitempty"`
	Landed            bool                   `json:"landed,omitempty"`
	Parts             *recoveryParts         `json:"parts,omitempty"`
	LegacyUncertain   bool                   `json:"legacy_uncertain,omitempty"`
	Submission        *recoverySubmission    `json:"submission,omitempty"`
	ComposerHold      *deliveryComposerHold  `json:"composer_hold,omitempty"`
	AliasSessionID    string                 `json:"alias_session_id,omitempty"`
	AliasHandle       string                 `json:"alias_handle,omitempty"`
	AliasBody         string                 `json:"alias_body,omitempty"`
	Inbound           externalInboundMessage `json:"inbound"`
	Reaction          bool                   `json:"reaction,omitempty"`
	Rendered          bool                   `json:"rendered,omitempty"`
	OriginalReceiptID string                 `json:"original_receipt_id,omitempty"`
	PendingReceiptID  string                 `json:"pending_receipt_id,omitempty"`
	PendingReceipt    bool                   `json:"pending_receipt,omitempty"`
	TargetSessions    []string               `json:"target_sessions,omitempty"`
	HeldAt            time.Time              `json:"held_at"`
	NextAttemptAt     time.Time              `json:"next_attempt_at"`
	Attempts          int                    `json:"attempts"`
	DeadLetteredAt    time.Time              `json:"dead_lettered_at,omitempty"`
	DeliveredAt       time.Time              `json:"delivered_at,omitempty"`
	CompletionPending bool                   `json:"completion_pending,omitempty"`
	Retiring          bool                   `json:"retiring,omitempty"`
	ReplayNotBefore   time.Time              `json:"replay_not_before,omitempty"`
	Reason            string                 `json:"reason,omitempty"`
}

type round2BaseInboundHoldNotice struct {
	TS       string    `json:"ts"`
	ThreadTS string    `json:"thread_ts"`
	Since    time.Time `json:"since"`
	Resolved bool      `json:"resolved,omitempty"`
}

type round2BaseInboundRecoveryState struct {
	Entries map[string]round2BaseInboundRecoveryEntry `json:"entries"`
	Notices map[string]round2BaseInboundHoldNotice    `json:"notices"`
}

type round2BaseInboundDeadLetterRecord struct {
	ManualRecovery    bool                   `json:"manual_recovery,omitempty"`
	TrustedFilePaths  []string               `json:"trusted_file_paths,omitempty"`
	ReceiptStateKnown bool                   `json:"receipt_state_known,omitempty"`
	Parts             *recoveryParts         `json:"parts,omitempty"`
	Submission        *recoverySubmission    `json:"submission,omitempty"`
	OriginalReceiptID string                 `json:"original_receipt_id,omitempty"`
	TargetSessions    []string               `json:"target_sessions,omitempty"`
	PendingReceiptID  string                 `json:"pending_receipt_id,omitempty"`
	PendingReceipt    bool                   `json:"pending_receipt,omitempty"`
	Rendered          bool                   `json:"rendered,omitempty"`
	Reaction          bool                   `json:"reaction,omitempty"`
	ReplayedAt        time.Time              `json:"replayed_at,omitempty"`
	DeadLetteredAt    time.Time              `json:"dead_lettered_at"`
	Channel           string                 `json:"channel"`
	Attempts          int                    `json:"attempts"`
	Reason            string                 `json:"reason"`
	Inbound           externalInboundMessage `json:"inbound"`
	// AliasSessionID/AliasHandle/AliasBody are set only for a lost
	// address-by-handle injection (gp-3yg): the aliased session it was
	// meant for, the handle, and the exact rendered reminder. A hand
	// recovery re-injects AliasBody into AliasSessionID; re-posting the
	// channel envelope instead would route to the channel-bound session
	// (codex r2 P2 #4).
	AliasSessionID string `json:"alias_session_id,omitempty"`
	AliasHandle    string `json:"alias_handle,omitempty"`
	AliasBody      string `json:"alias_body,omitempty"`
}

func round2BaseReadRecoveryState(path string) (*round2BaseInboundRecoveryState, error) {
	f, err := openBeneathFlags(filepath.Dir(path), filepath.Base(path), unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("inbound recovery state is not a regular file")
	}
	var state round2BaseInboundRecoveryState
	decoder := json.NewDecoder(f)
	if err := decoder.Decode(&state); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing data in inbound recovery state")
	}
	return &state, nil
}
