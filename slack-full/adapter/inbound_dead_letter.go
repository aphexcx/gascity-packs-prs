package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// --- the rejection ladder (gp-sgu7) -------------------------------------------
//
// ONE decision point for a message gc refused (permanentDeliveryFailure)
// or accepted without vouching for (errDeliveryUnvouched, gp-32q):
// nextRejectionStep. Every consumer — charge() is the only one — acts on
// its verdict; nothing else may decide whether an entry is retried.
//
// A 4xx payload refusal is deterministic: the same bytes get the same
// answer, so the entry is NEVER re-posted as-is (the 2026-09-08 incident
// re-posted one refused batch 34,000 times). The one retry allowed is a
// DIFFERENT payload: the attachments stripped and their local paths
// named in the text, because an attachment gc will not validate (a
// missing mime_type, a URL shape, a size) must not cost the founder's
// words; a second refusal, or a refusal with nothing left to strip,
// dead-letters at once. The unvouched class keeps gp-32q's bounded
// same-payload ladder (maxCoalesceDeliveryAttempts): the payload was
// accepted, only the vouch was missing, so re-posting it can succeed.

type rejectionStep int

const (
	// stepRetryWithoutAttachments: 4xx refusal, attachments present —
	// withholdAttachments, then restore for the next window.
	stepRetryWithoutAttachments rejectionStep = iota
	// stepDeadLetter: hand the entry to the dead-letter hook now.
	stepDeadLetter
	// stepRetrySame: unvouched under the cap — restore unchanged.
	stepRetrySame
)

func (s rejectionStep) String() string {
	switch s {
	case stepRetryWithoutAttachments:
		return "retry-without-attachments"
	case stepDeadLetter:
		return "dead-letter"
	case stepRetrySame:
		return "retry-same"
	}
	return fmt.Sprintf("rejectionStep(%d)", int(s))
}

// nextRejectionStep is the ladder. p.attempts counts the charges the
// entry has ALREADY taken (the caller increments after asking).
func nextRejectionStep(p pendingChannelInbound, cause error) rejectionStep {
	if permanentDeliveryFailure(cause) {
		if len(p.inbound.Attachments) > 0 {
			return stepRetryWithoutAttachments
		}
		return stepDeadLetter
	}
	if p.attempts+1 < maxCoalesceDeliveryAttempts {
		return stepRetrySame
	}
	return stepDeadLetter
}

// rejectionStatusLine is the short, operator-readable cause carried into
// the text: the HTTP status line for a gc refusal (its JSON body can be
// hundreds of bytes and quotes the file path itself), a bounded prefix
// of any other error.
func rejectionStatusLine(cause error) string {
	var pe *inboundPostError
	if errors.As(cause, &pe) {
		if pe.StatusText != "" {
			return pe.StatusText
		}
		return fmt.Sprintf("HTTP %d", pe.Status)
	}
	if cause == nil {
		return "refused"
	}
	s := cause.Error()
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

// withholdAttachments returns p with its attachments removed and a
// notice naming their local paths and the refusal appended to the
// message unit — into the files part (so the head-protected composer
// keeps it: the files block is never shed) with Text re-folded, or
// straight onto Text for a partless legacy/spool-replayed entry. The
// files themselves stay where downloadSlackFiles put them. A p with
// no attachments is returned unchanged, so a repeat call adds nothing.
func withholdAttachments(p pendingChannelInbound, cause error) pendingChannelInbound {
	if len(p.inbound.Attachments) == 0 {
		return p
	}
	paths := make([]string, 0, len(p.inbound.Attachments))
	for _, a := range p.inbound.Attachments {
		paths = append(paths, neutralizeMarkupBoundaries(strings.TrimPrefix(a.URL, "file://")))
	}
	noun := "attachments"
	if len(paths) == 1 {
		noun = "attachment"
	}
	notice := fmt.Sprintf("[%d %s withheld — gc refused this message with them attached (%s); the files remain at %s]",
		len(paths), noun, neutralizeMarkupBoundaries(rejectionStatusLine(cause)), strings.Join(paths, ", "))
	p.inbound.Attachments = nil
	if p.hasReminderParts() {
		if p.files == "" {
			p.files = notice
		} else {
			p.files += "\n" + notice
		}
		p.inbound.Text = p.foldedText()
		return p
	}
	if strings.TrimSpace(p.inbound.Text) == "" {
		p.inbound.Text = notice
	} else {
		p.inbound.Text = strings.TrimRight(p.inbound.Text, "\n") + "\n\n" + notice
	}
	return p
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
	reason := ""
	if cause != nil {
		reason = truncateReason(cause.Error())
	}
	now := time.Now().UTC()
	enc := json.NewEncoder(f)
	for _, p := range batch {
		if err := enc.Encode(inboundDeadLetterRecord{
			DeadLetteredAt: now,
			Channel:        channel,
			Attempts:       p.attempts,
			Reason:         reason,
			Inbound:        p.inbound,
		}); err != nil {
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
