package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"
)

// --- inbound delivery failure classes + dead letters (gp-xnc) ---------------

// inboundPostError is a non-2xx response from gc's extmsg inbound
// endpoint. It carries the status so the coalescer can tell a
// deterministic payload rejection from everything else; its text keeps
// the historical "<status line>: <body>" form operators grep for.
type inboundPostError struct {
	Status     int
	StatusText string
	Body       string
}

func (e *inboundPostError) Error() string {
	return fmt.Sprintf("%s: %s", e.StatusText, e.Body)
}

// permanentDeliveryFailure reports whether an inbound delivery error is
// a PAYLOAD rejection — gc looked at the message and will never accept
// this exact payload: 400 (malformed), 413 (too large), 415 (media
// type), 422 (validation, the gp-xnc incident). Only those dead-letter.
// Every other outcome keeps the coalescer's retry-forever durability:
// network errors and 5xx (gc restarting), 408/429 (transient by
// definition), AND operational 4xx such as 401/403/404/405 (wrong city
// name, a route missing mid-rollout, auth) — those are deployment
// problems an operator fixes in place, and dead-lettering every
// buffered message after three windows would turn a config mistake
// into a recovery chore (codex r1 finding 4).
func permanentDeliveryFailure(err error) bool {
	var pe *inboundPostError
	if !errors.As(err, &pe) {
		return false
	}
	switch pe.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// inboundDeadLetterRecord is one JSONL line in a channel's dead-letter
// file: the inbound envelope exactly as the adapter tried to POST it
// (the per-message envelope, without the coalesced wrapper text), so an
// operator can inspect the message and re-post it by hand once the
// rejection cause is fixed.
type inboundDeadLetterRecord struct {
	DeadLetteredAt time.Time              `json:"dead_lettered_at"`
	Channel        string                 `json:"channel"`
	Attempts       int                    `json:"attempts"`
	Reason         string                 `json:"reason"`
	Inbound        externalInboundMessage `json:"inbound"`
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

// maxDeadLetterReasonBytes bounds the stored error text INCLUDING the
// truncation marker — a gc validation body is a few hundred bytes;
// anything larger is cut on a rune boundary.
const maxDeadLetterReasonBytes = 4096

const deadLetterTruncMarker = "…(truncated)"

// truncateReason bounds s to maxDeadLetterReasonBytes without splitting
// a multi-byte rune, reserving room for the marker (codex r1 finding 6).
func truncateReason(s string) string {
	if len(s) <= maxDeadLetterReasonBytes {
		return s
	}
	cut := maxDeadLetterReasonBytes - len(deadLetterTruncMarker)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + deadLetterTruncMarker
}

// writeInboundDeadLetter appends one record per message to
// <dir>/<channel>.jsonl (channel sanitized exactly like the inbound file
// store's path components) and returns the file path. It reports
// success ONLY once the bytes are fsynced and the file is closed —
// the coalescer keeps the entry buffered on any error (codex r1
// finding 3). Store discipline matches inboundFileStore (gc-ywe.6)
// and is ENFORCED, not just requested at create time: the directory is
// chmod'd 0700 even when pre-existing, the file is opened O_NOFOLLOW
// (a planted symlink at <channel>.jsonl fails with ELOOP instead of
// redirecting the append), must be a regular file, and is chmod'd 0600
// (codex r1 finding 5).
func writeInboundDeadLetter(dir, channel string, batch []pendingChannelInbound, cause error) (string, error) {
	reason := ""
	if cause != nil {
		reason = truncateReason(cause.Error())
	}
	now := time.Now().UTC()
	records := make([]inboundDeadLetterRecord, 0, len(batch))
	for _, p := range batch {
		records = append(records, inboundDeadLetterRecord{
			DeadLetteredAt: now,
			Channel:        channel,
			Attempts:       p.attempts,
			Reason:         reason,
			Inbound:        p.inbound,
		})
	}
	return writeInboundDeadLetterRecords(dir, channel, records)
}

// writeAliasDeadLetter appends one record for a lost address-by-handle
// injection (gp-3yg): the channel envelope plus the aliased session, the
// handle and the exact rendered reminder, so a hand recovery re-injects
// the right bytes into the right session. Same file, same durability
// contract as writeInboundDeadLetter.
func writeAliasDeadLetter(dir, channel string, inbound externalInboundMessage, sessionID, handle, body string, cause error) (string, error) {
	reason := ""
	if cause != nil {
		reason = truncateReason(cause.Error())
	}
	return writeInboundDeadLetterRecords(dir, channel, []inboundDeadLetterRecord{{
		DeadLetteredAt: time.Now().UTC(),
		Channel:        channel,
		Reason:         reason,
		Inbound:        inbound,
		AliasSessionID: sessionID,
		AliasHandle:    handle,
		AliasBody:      body,
	}})
}

// writeInboundDeadLetterRecords is the durable append behind both writers.
func writeInboundDeadLetterRecords(dir, channel string, records []inboundDeadLetterRecord) (string, error) {
	created, err := missingAncestors(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("enforce 0700 on %q: %w", dir, err)
	}
	path := filepath.Join(dir, safePathComponent(channel)+".jsonl")
	// O_NONBLOCK: a planted FIFO would otherwise block the open until a
	// reader appears — under the channel flush mutex (codex r2 finding
	// 1). Regular files ignore it; it is cleared again before Go's
	// runtime sees the fd so the *os.File behaves like any other.
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return "", fmt.Errorf("open %q (no-follow, non-blocking): %w", path, err)
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		return "", fmt.Errorf("clear O_NONBLOCK on %q: %w", path, err)
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return "", err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return "", fmt.Errorf("dead-letter file %q is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		if err := f.Chmod(0o600); err != nil {
			_ = f.Close()
			return "", fmt.Errorf("enforce 0600 on %q: %w", path, err)
		}
	}
	enc := json.NewEncoder(f)
	for _, rec := range records {
		if err := enc.Encode(rec); err != nil {
			_ = f.Close()
			return path, err
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return path, fmt.Errorf("fsync %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return path, err
	}
	// Durability of the directory ENTRIES (codex r2 finding 2, r3
	// finding 1): fsync of the file alone does not persist a newly
	// created name; a power loss after the hook returned true could
	// otherwise erase the record the coalescer just retired. dir and
	// its parent are synced UNCONDITIONALLY on every successful write —
	// not only when this call created dir — so a retry after an attempt
	// that created the directory but failed before its sync still
	// confirms the entry. Every deeper ancestor this call created is
	// synced too. main() pre-creates dir at startup, so at write time
	// the chain is normally already there; the residual gap (a ≥2-level
	// chain created at write time by an attempt that failed mid-sync,
	// then retried) is accepted.
	for _, d := range created {
		if d == dir || d == filepath.Dir(dir) {
			continue
		}
		if err := syncDir(d); err != nil {
			return path, err
		}
	}
	if err := syncDir(dir); err != nil {
		return path, err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return path, err
	}
	return path, nil
}

// missingAncestors lists dir and every ancestor that does not exist
// yet, deepest first, followed by the nearest EXISTING ancestor (whose
// entry for the first created directory must be synced too). A Stat
// error other than not-exist is returned as-is.
func missingAncestors(dir string) ([]string, error) {
	var out []string
	d := dir
	for {
		_, err := os.Stat(d)
		if err == nil {
			if len(out) > 0 {
				out = append(out, d)
			}
			return out, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("stat %q: %w", d, err)
		}
		out = append(out, d)
		parent := filepath.Dir(d)
		if parent == d {
			return nil, fmt.Errorf("no existing ancestor for %q", dir)
		}
		d = parent
	}
}

// syncDir fsyncs a directory so entries created in it are durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir %q for fsync: %w", dir, err)
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return fmt.Errorf("fsync dir %q: %w", dir, err)
	}
	return d.Close()
}
