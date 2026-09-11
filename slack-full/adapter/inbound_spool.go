package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// inbound_spool.go — durable shutdown spool for admitted-but-undelivered
// inbounds (gp-9e7 fix round 2a'/2b', durability hardened in round 3).
//
// WHY: the coalescer buffers messages AFTER they were acked to Slack,
// and the inbound-liveness watermark advances at ADMISSION time
// (noteInboundEnvelope, before the ack) — never at delivery-to-gc time.
// So an admitted item that never reaches gc has NO recovery path: Slack
// will not redeliver (it got its 200), and the startup watermark
// backfill will not re-fetch it (it sits at or below the persisted
// watermark). The shutdown drain (flushAll) retries to a bound; this
// spool is where the residue goes instead of being lost — plus any
// straggler admission that lands after the drain's final snapshot
// (an event goroutine that outlived main's bounded eventWG wait).
//
// Shape: JSONL (one spooledInbound per line), append-only during
// shutdown so the drain's leftovers and later stragglers land in the
// same file without read-modify-write races; a process death mid-append
// truncates at most the final line, which the replay tolerates but logs
// LOUDLY as loss — a truncated line can only mean crash-mid-spill of
// that batch (round 3, 1c). Every spill reports durability truthfully:
// spillBatch returns true only after the write AND fsync succeeded, so
// callers log "spooled" only when the bytes are actually on disk and
// the loud LOSS log otherwise (round 3, 1a).
//
// Startup replay is crash-safe (round 3, 1b): consume() RENAMES the
// spool to a .replaying file instead of deleting it, and the file is
// removed only AFTER every entry has been re-admitted through the
// coalescer's normal buffers (once re-admitted, the entries are covered
// by the normal drain/spill cycle on the next shutdown). A crash inside
// the replay window therefore finds the .replaying file on the next
// startup and retries it; the worst case is a duplicate replay, which
// the per-entry gc dedup keys bound.

// maxInboundSpoolLineBytes bounds ONE spool line (the JSON, without its
// newline) on BOTH sides of the file: the producer refuses to write a
// longer entry (appendLinesLocked — the spill then reports it as loss
// at the moment durability was promised, never as "spooled"), and the
// replay drops a longer line as loss on its own while everything
// around it still replays (readSpoolLines streams the file line by
// line — codex r13 finding 3: a whole-file cap let a day of verdict
// records strand the acknowledged messages spooled beside them). One
// constant on both sides is what makes spillBatch's true mean
// "replays": with a producer that wrote anything and a reader that
// capped at 4 MiB, an acknowledged entry could confirm durable and then
// be dropped at startup (codex r14 finding 4: twenty thread priors of
// 2,000 mentions each, expanded to long display names, is a legal
// 4.8 MB entry). The cap is far above any legal entry — a Slack
// message is 40,000 characters, the thread context twenty of them,
// the files block and attachment metadata a few KiB — and bounds only
// what a corrupt file can make the reader hold in memory at once.
const maxInboundSpoolLineBytes = 64 << 20

// syncFile is the fsync the compaction's temp file goes through; a
// variable so a test can make it fail (an fsync failure means the bytes
// are not on disk, whatever the write returned).
var syncFile = func(f *os.File) error { return f.Sync() }

// spooledInbound is one spooled item: the channel it was buffered for,
// whether it was a no-wake reaction entry, and the ready-to-post
// envelope (final per-message text, attachments, dedup key).
type spooledInbound struct {
	Channel  string                 `json:"channel"`
	Reaction bool                   `json:"reaction,omitempty"`
	Inbound  externalInboundMessage `json:"inbound"`
	// Message-unit parts for head-protected re-composition on replay
	// (gp-0qw); absent on lines written before they existed, which
	// then replay with the folded Inbound.Text as the body.
	ThreadAnchor string `json:"thread_anchor,omitempty"`
	Preamble     string `json:"preamble,omitempty"`
	// PreambleLean / BotQuotes: the composer's shed-before-omit fallback
	// (jg-ure5r8). Written only when the preamble carries bot quotes —
	// for a human-only preamble the lean form would be a byte-for-byte
	// duplicate of Preamble and could double a shutdown spool past its
	// read cap (codex r5 P2).
	PreambleLean string `json:"preamble_lean,omitempty"`
	BotQuotes    bool   `json:"bot_quotes,omitempty"`
	Body         string `json:"body,omitempty"`
	Files        string `json:"files,omitempty"`
	// Isolate carries pendingChannelInbound.isolate across a restart
	// (gp-sgu7): a member of a batch gc refused resumes as a single
	// probe on replay instead of re-posting the refused batch.
	Isolate bool `json:"isolate,omitempty"`
	// Refused (with Attempts) marks an entry the rejection ladder had
	// RETIRED whose dead-letter write never confirmed before shutdown
	// (gp-sgu7, codex r2 finding 1): the replay parks it straight into
	// the write retry — it is never enqueued for delivery again.
	Refused  string `json:"refused,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	// Stripped (with Attempts) marks the ladder's stripped retry — the
	// entry's attachments withheld after a refusal, the retry owed or
	// in flight at shutdown: the replay seeds the ledger's outstanding
	// verdict for its ts before admitting it, so a later copy of the
	// message inherits the decision (codex r12 finding 1).
	Stripped string `json:"stripped,omitempty"`
	// VerdictTS marks a VERDICT record rather than a message: the
	// rejection ladder RETIRED (Channel, VerdictTS) — dead-lettered, or
	// delivered without its attachments (VerdictDelivered) — for the
	// reason Verdict, at VerdictAt (unix seconds). Persisted by
	// recordVerdict so a delayed Slack redelivery of the refused
	// message after a restart is still a duplicate, not fresh bytes
	// (codex r12 finding 2). Replay seeds every record still inside
	// ladderVerdictRetention, and the seeding writes it again for the
	// next restart.
	VerdictTS        string `json:"verdict_ts,omitempty"`
	Verdict          string `json:"verdict,omitempty"`
	VerdictDelivered bool   `json:"verdict_delivered,omitempty"`
	VerdictAt        int64  `json:"verdict_at,omitempty"`
	// VerdictOutstanding marks the record of an OUTSTANDING verdict — the
	// ladder owns the message and owes it a copy: the stripped retry
	// (VerdictStripped) or the ladder's plain copy, charged or flagged
	// isolate after a batch refusal (codex r22/r23: every progression of
	// the ledger is journaled the moment it is made, so a crash with a
	// plain copy of the message staged cannot re-post the refused bytes
	// or the refused batch). Absent on a terminal record.
	VerdictOutstanding bool `json:"verdict_outstanding,omitempty"`
	VerdictStripped    bool `json:"verdict_stripped,omitempty"`
	// VerdictAttempts is the ledger's charged count at this progression
	// (codex r33 finding 2): every increment is its own record, so a
	// crash restores the bounded ladder where it stood. Records of one
	// message fold by rank, then by this count, then by time.
	VerdictAttempts int `json:"verdict_attempts,omitempty"`
	// dispositionOnly marks a salvaged DISPOSITION of an entry whose
	// message stays in a spool that could not be merged (codex r18
	// finding 2): its Refused/Stripped/Isolate seed the ledger in the
	// replay's pre-pass so a staged plain copy of the same message
	// adopts the ladder's decision, and the entry itself is never
	// admitted — its message replays when the spool merges next
	// startup. Never written to disk.
	dispositionOnly bool `json:"-"`
	// DeletedTS marks a DELETION record rather than a message: the
	// sender deleted (Channel, DeletedTS). Persisted by recordDeletion
	// so a deletion processed after a message was spooled — or in the
	// window between a tombstone check and the durable write — still
	// applies on replay (gp-0qw, codex round-4 finding 1). Replay
	// applies every deletion record to every message entry in the file
	// regardless of line order, and seeds the in-memory tombstones.
	DeletedTS string `json:"deleted_ts,omitempty"`
}

// inboundSpool is the file-backed spool. All methods are nil-safe (a
// nil spool means the operator disabled spooling with an empty path —
// the coalescer then logs residue as lost).
type inboundSpool struct {
	mu   sync.Mutex
	path string
	// sealed flips on once, at the very end of main's shutdown sequence
	// (gp-9e7 round 3, 2c): every spool write runs entirely under mu, so
	// seal() — which takes mu — JOINS any write still in flight, and a
	// write attempted after the seal is refused before it touches the
	// file. No spool write can therefore race process exit and be torn
	// by it; a post-seal straggler degrades to the loud LOSS log.
	sealed bool
	// lastWriteFailure remembers, per record key, the last failure text
	// logged, so a retried write logs once per DISTINCT failure and once
	// on recovery (codex r19 minor: one line per state change, not per
	// attempt). Guarded by mu.
	lastWriteFailure map[string]string
}

// noteWriteFailureLocked records a write failure for key and reports
// whether it is a new state (a different failure, or the first).
func (s *inboundSpool) noteWriteFailureLocked(key string, err error) bool {
	if s.lastWriteFailure == nil {
		s.lastWriteFailure = make(map[string]string)
	}
	text := err.Error()
	if s.lastWriteFailure[key] == text {
		return false
	}
	s.lastWriteFailure[key] = text
	return true
}

// noteWriteOKLocked clears key's failure state and reports whether it
// was failing (a recovery worth one line).
func (s *inboundSpool) noteWriteOKLocked(key string) bool {
	if _, failing := s.lastWriteFailure[key]; !failing {
		return false
	}
	delete(s.lastWriteFailure, key)
	return true
}

// newInboundSpool returns the spool for path, or nil when path is empty
// (spooling disabled).
func newInboundSpool(path string) *inboundSpool {
	if path == "" {
		return nil
	}
	return &inboundSpool{path: path}
}

// replayingPath is the crash-safe replay staging file: the spool is
// renamed here before re-admission and removed only after it.
func (s *inboundSpool) replayingPath() string {
	return s.path + ".replaying"
}

// spillBatch appends one channel's undeliverable batch to the spool.
// This is the coalescer's spill hook. Returns true ONLY when the batch
// is durably on disk (write + fsync confirmed) — the caller owns the
// loud per-channel LOSS log on false, the same last-resort contract as
// a disabled spool (gp-9e7 round 3, 1a). A sealed spool (process past
// its final shutdown join, 2c) refuses the write outright.
func (s *inboundSpool) spillBatch(channel string, batch []pendingChannelInbound) bool {
	if s == nil || len(batch) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		log.Printf("inbound spool: SEALED — refusing late write of chan=%s %d item(s); process exit is imminent and a torn write would corrupt the spool", channel, len(batch))
		return false
	}
	if err := s.appendLocked(channel, batch); err != nil {
		log.Printf("inbound spool: write FAILED chan=%s %d item(s): %v", channel, len(batch), err)
		return false
	}
	return true
}

// recordDeletion appends a deletion record for (channel, ts). Same
// seal and durability contract as spillBatch; the file is created on
// demand, so a deletion with nothing spooled costs one tiny line that
// the next replay consumes. Nil-safe.
func (s *inboundSpool) recordDeletion(channel, ts string) bool {
	if s == nil || channel == "" || ts == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		log.Printf("inbound spool: SEALED — refusing late deletion record chan=%s ts=%s", channel, ts)
		return false
	}
	if err := s.appendLinesLocked([]spooledInbound{{Channel: channel, DeletedTS: ts}}); err != nil {
		log.Printf("inbound spool: deletion record write FAILED chan=%s ts=%s: %v", channel, ts, err)
		return false
	}
	return true
}

// recordVerdict appends a verdict record for (channel, ts) — terminal
// or outstanding. Same seal and durability contract as recordDeletion.
// Nil-safe; the coalescer's recordVerdict hook.
func (s *inboundSpool) recordVerdict(channel, ts string, v ladderVerdict) bool {
	if s == nil || channel == "" || ts == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		log.Printf("inbound spool: SEALED — refusing late verdict record chan=%s ts=%s", channel, ts)
		return false
	}
	at := v.at
	if at.IsZero() {
		at = time.Now()
	}
	entry := spooledInbound{Channel: channel, VerdictTS: ts, Verdict: truncateReason(rejectionReasonText(v.cause)), VerdictDelivered: v.delivered, VerdictAt: at.Unix(),
		VerdictOutstanding: !v.retired, VerdictStripped: v.stripped, VerdictAttempts: v.attempts}
	if err := s.appendLinesLocked([]spooledInbound{entry}); err != nil {
		if s.noteWriteFailureLocked("verdict:"+channel, err) {
			log.Printf("inbound spool: verdict record write FAILED chan=%s ts=%s (logged once per distinct failure; the coalescer retries it): %v", channel, ts, err)
		}
		return false
	}
	if s.noteWriteOKLocked("verdict:" + channel) {
		log.Printf("inbound spool: verdict record writes recovered chan=%s ts=%s", channel, ts)
	}
	return true
}

// seal joins any in-flight spool write (every write holds mu end to
// end) and refuses all subsequent ones. Main calls it as the last step
// of shutdown, after flushAll: from seal's return onward no spool write
// can race process exit (gp-9e7 round 3, 2c). Nil-safe.
func (s *inboundSpool) seal() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
}

func (s *inboundSpool) appendLocked(channel string, batch []pendingChannelInbound) error {
	entries := make([]spooledInbound, 0, len(batch))
	for _, p := range batch {
		entry := spooledInbound{
			Channel: channel, Reaction: p.reaction, Inbound: p.inbound,
			ThreadAnchor: p.threadAnchor, Preamble: p.preamble, Body: p.body, Files: p.files,
			Isolate: p.isolate, Stripped: p.stripped,
		}
		if p.botQuotes {
			entry.PreambleLean, entry.BotQuotes = p.preambleLean, true
		}
		if p.refused != "" || p.stripped != "" || (p.attempts > 0 && !p.reaction) {
			// A refused, stripped or charged copy carries its ladder
			// standing (codex r32 finding 2: a charged unvouched retry
			// spooled as a plain line read back as nobody's, and the
			// in-place compaction dropped it beside its record). A
			// charged REACTION does not: the ladder never owns a
			// reaction (codex r33 finding 3: the count seeded an
			// ownership nothing ever settled, and the staged file was
			// retained forever), so its retries start over as before.
			entry.Refused, entry.Attempts = p.refused, p.attempts
		}
		if p.hasReminderParts() {
			// The folded Text is derivable from the parts; storing both
			// would double the line (gp-0qw, codex round-3 finding 4).
			entry.Inbound.Text = ""
		}
		entries = append(entries, entry)
	}
	return s.appendLinesLocked(entries)
}

func (s *inboundSpool) appendLinesLocked(entries []spooledInbound) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(s.path), err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %q: %w", s.path, err)
	}
	defer f.Close()
	// Record boundary (codex r17 finding 2): a write that died mid-line
	// (ENOSPC, a crash) leaves an unterminated prefix, and appending the
	// next record straight after it would fuse the two into one corrupt
	// line — the retry of a verdict record would then be confirmed
	// durable and lost at startup. A file that does not end on a newline
	// gets one first: the partial tail becomes its own corrupt line
	// (dropped loudly at replay, as it already was), and this write
	// starts on a boundary. The check reads one byte (appendFileTo's
	// seal, applied to every append).
	if st, serr := f.Stat(); serr != nil {
		return fmt.Errorf("stat %q: %w", s.path, serr)
	} else if size := st.Size(); size > 0 {
		tail := make([]byte, 1)
		if _, rerr := f.ReadAt(tail, size-1); rerr != nil {
			return fmt.Errorf("read tail of %q: %w", s.path, rerr)
		}
		if tail[0] != '\n' {
			log.Printf("inbound spool: %s ends mid-line — an earlier write died inside a record; sealing the partial line before appending so this record cannot fuse with it (the partial line is dropped LOUDLY as loss at replay)", s.path)
			if _, werr := f.Write([]byte{'\n'}); werr != nil {
				return fmt.Errorf("seal partial tail of %q: %w", s.path, werr)
			}
		}
	}
	// Every entry is encoded and measured BEFORE anything is written:
	// an entry over the line cap would be dropped by the replay, so it
	// is refused here, where the caller still owns it and logs the loss
	// truthfully; the legal entries of the same batch are written and
	// fsynced regardless (the refusal costs the oversized entry, not its
	// batch-mates), and the returned error names what was refused so
	// the spill reports false — nothing in the batch is claimed durable
	// that will not replay.
	var refused []string
	lines := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode spool entry chan=%s ts=%s: %w", entry.Channel, entry.Inbound.ProviderMessageID, err)
		}
		if len(line) > maxInboundSpoolLineBytes {
			refused = append(refused, fmt.Sprintf("chan=%s ts=%s is %d bytes", entry.Channel, entry.Inbound.ProviderMessageID, len(line)))
			continue
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		w := bufio.NewWriter(f)
		for _, line := range lines {
			w.Write(line)
			w.WriteByte('\n')
		}
		if err := w.Flush(); err != nil {
			return fmt.Errorf("write %q: %w", s.path, err)
		}
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if len(refused) > 0 {
		return fmt.Errorf("%d of %d entr%s over the %d-byte spool line cap REFUSED — LOSS, the replay could not read %s (%s); the other %d %s durable",
			len(refused), len(entries), plural(len(refused), "y", "ies"), maxInboundSpoolLineBytes, plural(len(refused), "it", "them"), strings.Join(refused, "; "), len(lines), plural(len(lines), "is", "are"))
	}
	return nil
}

// consume stages the spool for replay and returns its entries WITHOUT
// deleting anything (gp-9e7 round 3, 1b): the spool file is RENAMED to
// the .replaying staging file (an atomic same-directory rename), read
// from there, and left in place. The returned cleanup func removes the
// staging file; the caller invokes it only AFTER every entry has been
// re-admitted, so a crash inside the replay window finds the .replaying
// file on the next startup and retries it. cleanup is nil when there is
// nothing to remove (or when the file must be left for manual
// inspection).
//
// A leftover .replaying file from a crashed earlier replay is merged
// with any newer spool content, older entries first. Corrupt lines —
// a process death mid-append truncates at most the final one — are
// tolerated but logged LOUDLY as loss: a truncated line can only mean
// crash-mid-spill of that batch (round 3, 1c).
func (s *inboundSpool) consume() ([]spooledInbound, func(), error) {
	if s == nil {
		return nil, nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rp := s.replayingPath()
	readPath := rp
	// Deletion records salvaged read-only from a newer spool whose merge
	// failed: its messages wait for the next startup, but a deletion of
	// a message that IS replaying now must still apply (codex round-5
	// finding 1).
	var salvagedDeletions []spooledInbound
	if _, err := os.Stat(s.path); err == nil {
		if _, rerr := os.Stat(rp); rerr == nil {
			// Crash-mid-replay leftover AND a newer spool: append the
			// spool's bytes behind the older .replaying entries so replay
			// order stays oldest-first, then drop the spool file.
			if err := appendFileTo(rp, s.path); err != nil {
				// The spool file stays for the next startup's retry; only
				// the already-staged entries replay this time.
				log.Printf("inbound spool: merging %s into %s FAILED: %v — replaying the staged file only; the spool retries next startup", s.path, rp, err)
				// Deletions, verdicts AND entry-carried dispositions. An
				// unreadable spool defers the whole replay (codex r19
				// finding 1): the staged entries may depend on dispositions
				// in it (a plain copy staged, its stripped copy here), so
				// nothing is admitted until both can be read.
				salvaged, serr := readSpoolLines(s.path, true)
				if serr != nil {
					log.Printf("inbound spool: %s cannot be read after the failed merge (%v) — the staged file's replay is DEFERRED to the next startup: its entries may depend on dispositions in the unread spool; both files are left in place", s.path, serr)
					return nil, nil, fmt.Errorf("spool %q unreadable after a failed merge into %q: %w", s.path, rp, serr)
				}
				salvagedDeletions = salvaged
			} else if err := os.Remove(s.path); err != nil {
				log.Printf("inbound spool: remove %s after merge: %v (next startup may replay duplicates; gc dedup keys bound the damage)", s.path, err)
			}
		} else if err := os.Rename(s.path, rp); err != nil {
			// Rename failed (permissions; a basename whose .replaying
			// suffix exceeds NAME_MAX): fall back to reading the spool in
			// place. The replay then appends its re-written verdict
			// records to this SAME file, so cleanup must not remove it
			// (codex r15 finding 3): it compacts the file to its live
			// verdict records instead — still only after re-admission,
			// which is the invariant that matters.
			log.Printf("inbound spool: rename %s -> %s failed: %v — replaying in place", s.path, rp, err)
			readPath = s.path
		}
	}
	entries, err := readSpoolLines(readPath, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, nil
		}
		log.Printf("inbound spool: %s unreadable (%v) — nothing replayed; its dispositions cannot be rebuilt", readPath, err)
		return nil, nil, fmt.Errorf("spool %q unreadable: %w", readPath, err)
	}
	entries = append(entries, salvagedDeletions...)
	cleanup := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if readPath == s.path {
			s.compactToRecordsLocked()
			return
		}
		if err := os.Remove(readPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbound spool: remove %s after replay: %v (the next restart may replay duplicates; gc dedup keys bound the damage)", readPath, err)
		}
	}
	return entries, cleanup, nil
}

// compactToRecordsLocked rewrites the spool in place to its verdict
// records only — one per (channel, ts), the newest decision — after an
// in-place replay re-admitted its message entries (codex r15 finding
// 3). The rewrite goes through a short-named temp file in the same
// directory (the staging rename failed, possibly on the name's length)
// and an atomic rename over the spool; on any failure the file is left
// as it is — its messages replay again next startup, gc dedup keys
// bound the damage, and its records are still there. Caller holds s.mu.
func (s *inboundSpool) compactToRecordsLocked() {
	// A read error aborts the compaction with the journal untouched
	// (codex r17 finding 3: the error-swallowing salvage reader returned
	// a partial or empty set, and an empty replacement was renamed over
	// the journal). A corrupt or oversized LINE is still dropped as the
	// loss it already was.
	records, rerr := readSpoolLines(s.path, false)
	if rerr != nil {
		log.Printf("inbound spool: compacting %s in place FAILED (read: %v) — left as it is; its messages replay again next startup", s.path, rerr)
		return
	}
	type key struct{ channel, ts string }
	// Records first, folded by progression — one per (channel, ts), the
	// furthest decision, then the newest — because the payload pass
	// below asks them which messages the ledger owns.
	newest := make(map[key]spooledInbound)
	var order []key
	for _, e := range records {
		if e.VerdictTS == "" {
			continue
		}
		k := key{e.Channel, e.VerdictTS}
		if cur, ok := newest[k]; ok {
			if !supersedes(e, cur) {
				continue
			}
		} else {
			order = append(order, k)
		}
		newest[k] = e
	}
	// A Refused entry is a parked payload's durable home until its
	// dead-letter write confirms (codex r20 finding 1): kept, once per
	// (channel, id) — the LAST copy, which is the newest re-spool —
	// beside the verdict records, with every deletion record that names
	// it applied (the notice replaces the text) and kept (codex r21
	// finding 3: the first copy was kept and the deletion dropped, so a
	// restart with the sink still failing restored the deleted text).
	deleted := make(map[key]bool)
	for _, e := range records {
		if e.DeletedTS != "" {
			deleted[key{e.Channel, e.DeletedTS}] = true
		}
	}
	// An OWNED copy is the payload its outstanding verdict owns (codex
	// r26 finding 2: dropped here, ownership survived without a
	// deliverable message): kept, once per (channel, id) — the copy
	// furthest along the ladder, the later line at equal standing
	// (codex r33 finding 4: the newest line was kept, and a later, less
	// charged duplicate reset the message's budget after a crash) —
	// deletions applied, unless the message's folded record is terminal
	// (a retired message's copy is dead). The ledger owns every
	// message entry that carries a ladder standing of its own — refused,
	// stripped, isolated or charged — and every entry whose message has
	// an OUTSTANDING record in this file (codex r32 finding 2: a charged
	// unvouched retry, restored as the ladder's copy and re-spooled as a
	// plain line, was dropped here beside its record, and a crash before
	// the retry lost the acknowledged message).
	ledgerOwns := func(e spooledInbound) bool {
		if e.Reaction {
			return false
		}
		if e.Stripped != "" || e.Isolate || e.Attempts > 0 {
			return true
		}
		rec, ok := newest[key{e.Channel, e.Inbound.ProviderMessageID}]
		return ok && rec.VerdictOutstanding
	}
	refusedAt, ownedAt := make(map[key]int), make(map[key]int)
	var refused, owned []spooledInbound
	for _, e := range records {
		if e.VerdictTS != "" || e.DeletedTS != "" || (e.Refused == "" && !ledgerOwns(e)) {
			continue
		}
		k := key{e.Channel, e.Inbound.ProviderMessageID}
		if deleted[k] {
			p := pendingChannelInbound{inbound: e.Inbound, threadAnchor: e.ThreadAnchor, preamble: e.Preamble, body: e.Body, files: e.Files}
			applyDeletion(&p)
			e.Inbound = p.inbound
			e.ThreadAnchor, e.Preamble, e.Body, e.Files = "", "", "", ""
		}
		at, list := refusedAt, &refused
		if e.Refused == "" {
			at, list = ownedAt, &owned
		}
		if i, seen := at[k]; seen {
			if e.Refused == "" && moreProgressed(standingOf((*list)[i]), standingOf(e)) {
				continue // a later, less charged copy never replaces the ladder's furthest copy
			}
			(*list)[i] = e
		} else {
			at[k] = len(*list)
			*list = append(*list, e)
		}
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".spool-compact-*")
	if err != nil {
		log.Printf("inbound spool: compacting %s in place FAILED (temp file: %v) — left as it is; its messages replay again next startup", s.path, err)
		return
	}
	tmpPath := tmp.Name()
	w := bufio.NewWriter(tmp)
	// The replay's own filters apply here too (codex r30 minor 2), or
	// repeated in-place restarts never reclaim anything: a terminal
	// record past retention with no copy of its message present is
	// gone; an outstanding record with no copy present is ownership of
	// nothing and gone; a Refused payload whose message has a terminal
	// record (its dead-letter write confirmed) is redundant and gone —
	// exactly what the replay drops at admission.
	present := make(map[key]bool)
	for _, e := range records {
		if e.VerdictTS == "" && e.DeletedTS == "" {
			present[key{e.Channel, e.Inbound.ProviderMessageID}] = true
		}
	}
	now := time.Now()
	kept := make([]spooledInbound, 0, len(order)+2*len(refused)+2*len(owned))
	liveRecords := 0
	for _, k := range order {
		v := verdictOf(newest[k])
		if !present[k] && (!v.retired || now.Sub(v.at) > ladderVerdictRetention) {
			continue
		}
		kept = append(kept, newest[k])
		liveRecords++
	}
	liveRefused := 0
	for _, e := range refused {
		k := key{e.Channel, e.Inbound.ProviderMessageID}
		if rec, ok := newest[k]; ok && !rec.VerdictOutstanding {
			continue // its dead-letter write confirmed: the record says so, the payload is redundant
		}
		kept = append(kept, e)
		liveRefused++
	}
	liveOwned := 0
	for _, e := range owned {
		k := key{e.Channel, e.Inbound.ProviderMessageID}
		if rec, ok := newest[k]; ok && !rec.VerdictOutstanding {
			continue // the ledger retired this message: its copy is dead
		}
		kept = append(kept, e)
		liveOwned++
	}
	for _, e := range kept {
		if e.VerdictTS != "" || e.DeletedTS != "" {
			continue
		}
		if k := (key{e.Channel, e.Inbound.ProviderMessageID}); deleted[k] {
			kept = append(kept, spooledInbound{Channel: e.Channel, DeletedTS: e.Inbound.ProviderMessageID})
		}
	}
	for _, e := range kept {
		if e.VerdictTS == "" && e.DeletedTS == "" && (e.Preamble != "" || e.Body != "" || e.Files != "") {
			// The producer's rule (appendLocked): the folded Text is
			// derived from the parts and never stored beside them. The
			// reader rebuilt it; serialized again it doubled a legal
			// 34 MiB refusal past the cap the reader enforces, and the
			// next restart dropped the only payload (codex r28 finding 2).
			e.Inbound.Text = ""
		}
		line, merr := json.Marshal(e)
		if merr != nil {
			log.Printf("inbound spool: compacting %s: a record could not be encoded and is dropped: %v", s.path, merr)
			continue
		}
		if len(line) > maxInboundSpoolLineBytes {
			// Never rename a line the reader would drop over the journal.
			err = fmt.Errorf("chan=%s ts=%s would compact to %d bytes, over the %d cap", e.Channel, e.Inbound.ProviderMessageID+e.VerdictTS+e.DeletedTS, len(line), maxInboundSpoolLineBytes)
			break
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	// Every step feeds ONE error: a failed flush or fsync must reach the
	// rename guard, or an incomplete temp file replaces the journal and
	// its verdicts are lost (codex r16 finding 1: a shadowed err let the
	// rename proceed).
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = syncFile(tmp)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmpPath, s.path)
	}
	if err != nil {
		os.Remove(tmpPath)
		log.Printf("inbound spool: compacting %s in place FAILED (%v) — left as it is; its messages replay again next startup", s.path, err)
		return
	}
	log.Printf("inbound spool: %s replayed in place and compacted to %d verdict record(s), %d parked refusal(s) and %d owned cop%s", s.path, liveRecords, liveRefused, liveOwned, plural(liveOwned, "y", "ies"))
}

// appendFileTo appends src's bytes to the end of dst, fsyncing dst
// before returning. Used to merge a fresh spool behind a leftover
// .replaying file, preserving oldest-first order.
//
// Boundary seal (gp-9e7 round 5, finding 2): a process death mid-append
// can leave dst's final line PARTIAL (no trailing newline). Appending
// src directly would fuse that partial prefix with src's first entry
// into one corrupt line — and since the merge then removes src, the
// already-lost partial tail would silently CONSUME an intact entry. A
// dst that does not end on a record boundary therefore gets a newline
// written first: the partial tail becomes its own complete corrupt line,
// which replay drops with the loud CORRUPT LINE / LOSS log it already
// earns, and every intact src entry survives. A dst that ends cleanly is
// untouched (the seal check reads one byte).
// readDeletionRecords returns only the deletion records in the spool
// file at path (read-only, best-effort; a missing or oversized file
// yields none).
// readSpoolLines streams a spool file line by line. A line over
// maxInboundSpoolLineBytes, or one that does not parse, is dropped
// LOUDLY as loss and the read continues — the file as a whole is never
// refused (codex r13 finding 3). recordsOnly keeps just the deletion
// and verdict records (the salvage of a file whose message entries
// could not be merged: both kinds apply to the staged entries and to
// messages admitted after the restart, so they replay even when the
// file itself is left for the next startup).
func readSpoolLines(path string, recordsOnly bool) ([]spooledInbound, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var out []spooledInbound
	for {
		line, err := r.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// Longer than the reader's buffer: accumulate while the line
			// can still be legal (the cap plus its newline), then discard
			// the rest of the line. The cap is measured on the line
			// WITHOUT its newline — the producer's measure — so a line
			// exactly at the cap replays (codex r14 finding 4).
			buf := append([]byte(nil), line...)
			for errors.Is(err, bufio.ErrBufferFull) {
				line, err = r.ReadSlice('\n')
				if len(buf) <= maxInboundSpoolLineBytes {
					buf = append(buf, line...)
				}
			}
			line = buf
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > maxInboundSpoolLineBytes {
			log.Printf("inbound spool: OVERSIZED LINE (>%d bytes) DROPPED — LOSS: the producer refuses a line this long, so this is corruption; the rest of the file still replays", maxInboundSpoolLineBytes)
			if err != nil {
				if !errors.Is(err, io.EOF) {
					return out, err
				}
				break
			}
			continue
		}
		if len(line) > 0 {
			var e spooledInbound
			switch perr := json.Unmarshal(line, &e); {
			case perr != nil || e.Channel == "":
				if !recordsOnly {
					log.Printf("inbound spool: CORRUPT LINE (%d bytes) DROPPED — LOSS: this can only be a crash mid-spill of that batch (parse err: %v)", len(line), perr)
				}
			case recordsOnly && e.DeletedTS != "":
				out = append(out, spooledInbound{Channel: e.Channel, DeletedTS: e.DeletedTS})
			case recordsOnly && e.VerdictTS != "":
				out = append(out, spooledInbound{Channel: e.Channel, VerdictTS: e.VerdictTS, Verdict: e.Verdict, VerdictDelivered: e.VerdictDelivered, VerdictAt: e.VerdictAt,
					VerdictOutstanding: e.VerdictOutstanding, VerdictStripped: e.VerdictStripped, VerdictAttempts: e.VerdictAttempts})
			case recordsOnly && (e.Refused != "" || e.Stripped != "" || e.Isolate || (e.Attempts > 0 && !e.Reaction)):
				// The salvage keeps what an ENTRY decided too (codex r18
				// finding 2), without its message.
				out = append(out, spooledInbound{
					Channel: e.Channel, Reaction: e.Reaction,
					Inbound: externalInboundMessage{ProviderMessageID: e.Inbound.ProviderMessageID},
					Refused: e.Refused, Stripped: e.Stripped, Isolate: e.Isolate, Attempts: e.Attempts,
					dispositionOnly: true,
				})
			case recordsOnly:
			default:
				if e.Inbound.Text == "" {
					if p := (pendingChannelInbound{preamble: e.Preamble, body: e.Body, files: e.Files}); p.hasReminderParts() {
						e.Inbound.Text = p.foldedText()
					}
				}
				out = append(out, e)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return out, err
			}
			break
		}
	}
	return out, nil
}

// verdictOf decodes a verdict record line into the ledger's verdict.
func verdictOf(e spooledInbound) ladderVerdict {
	return ladderVerdict{retired: !e.VerdictOutstanding, stripped: e.VerdictStripped, delivered: e.VerdictDelivered, cause: errors.New(e.Verdict), at: time.Unix(e.VerdictAt, 0), attempts: e.VerdictAttempts}
}

// supersedes reports whether record a is the later PROGRESSION of the
// same message than b: a higher rank wins, then the higher charged
// count (codex r33 finding 2), then the newer decision.
func supersedes(a, b spooledInbound) bool {
	return verdictOf(a).progresses(verdictOf(b))
}

// standingOf reads a message entry's ladder standing — the charged
// count and the isolate flag — as the progression comparator sees it.
func standingOf(e spooledInbound) pendingChannelInbound {
	return pendingChannelInbound{attempts: e.Attempts, isolate: e.Isolate}
}

// readRecords salvages the deletion and verdict records of a spool file.
func readRecords(path string) []spooledInbound {
	out, _ := readSpoolLines(path, true)
	return out
}

func appendFileTo(dst, src string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %q: %w", src, err)
	}
	// O_APPEND affects writes only; ReadAt below is unaffected.
	f, err := os.OpenFile(dst, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %q: %w", dst, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %q: %w", dst, err)
	}
	if size := st.Size(); size > 0 {
		tail := make([]byte, 1)
		if _, err := f.ReadAt(tail, size-1); err != nil {
			return fmt.Errorf("read tail of %q: %w", dst, err)
		}
		if tail[0] != '\n' {
			log.Printf("inbound spool: %s ends mid-line — a crash mid-append truncated its final record; sealing the partial line before merging %s so it cannot fuse with (and consume) an intact entry. Replay will drop the partial line LOUDLY as loss.", dst, src)
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return fmt.Errorf("seal partial tail of %q: %w", dst, err)
			}
		}
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("append to %q: %w", dst, err)
	}
	return f.Sync()
}

// replayInto consumes the spool and re-buffers every entry through the
// coalescer's normal admission, returning the count re-buffered. The
// staging file is removed only AFTER the loop completes (gp-9e7 round
// 3, 1b): a crash mid-replay leaves the .replaying file for the next
// startup to retry — once re-admitted, the entries are covered by the
// normal drain/spill cycle on the next shutdown. Messages re-enter the
// coalesce window (the timer delivers them within one window of
// startup); reactions re-enter the no-wake side lane and piggyback on
// the channel's next real delivery, so replay can never produce the
// solo reaction wake the side lane exists to prevent.
func (s *inboundSpool) replayInto(c *inboundCoalescer) int {
	n, _ := s.replay(c)
	return n
}

// replay is replayInto with its verdict: a recovery FAILURE (a spool
// file the adapter wrote and now cannot read — the ledger of refused
// messages cannot be rebuilt) is returned to the caller, and the
// coalescer is CLOSED to admissions first (codex r20 finding 2): a
// delayed Slack copy of a message whose stripped disposition sits in
// the unreadable file would otherwise enter delivery with its original
// attachments. main() refuses to start on it; the files stay for the
// operator.
func (s *inboundSpool) replay(c *inboundCoalescer) (int, error) {
	entries, done, cerr := s.consume()
	if cerr != nil {
		if c != nil {
			c.mu.Lock()
			c.closed = true
			c.mu.Unlock()
		}
		log.Printf("inbound spool: RECOVERY FAILED (%v) — admissions are closed: nothing delivers until the spool files can be read; fix them and restart", cerr)
		return 0, cerr
	}
	// Deletion records apply to every message entry in the file
	// regardless of line order, and seed the in-memory tombstones so a
	// Slack redelivery of the same message after replay is caught too.
	deleted := make(map[string]map[string]bool)
	for _, e := range entries {
		if e.DeletedTS == "" {
			continue
		}
		if deleted[e.Channel] == nil {
			deleted[e.Channel] = make(map[string]bool)
		}
		deleted[e.Channel][e.DeletedTS] = true
		if c != nil {
			c.mu.Lock()
			c.tombstoneLocked(e.Channel, e.DeletedTS, time.Now())
			c.mu.Unlock()
		}
	}
	// Verdict records seed the ledger BEFORE any message entry is
	// admitted: a copy of a retired message in the same file (a twin
	// spooled beside its dead-lettered original, a stale stripped retry
	// staged before its own dead-letter — codex r13 finding 1) is then
	// dropped at admission like any late duplicate. Records are folded
	// to ONE verdict per (channel, ts) first (the newest decision wins)
	// and expired ones skipped, so every replay COMPACTS the journal:
	// each live verdict is written exactly once for the next restart
	// (codex r13 finding 3). The staged file is kept until every one of
	// those writes is confirmed durable (codex r13 finding 2).
	now := time.Now()
	type verdictKey struct{ channel, ts string }
	// The messages this replay carries, by identity: an EXPIRED record
	// still applies to a copy staged beside it (codex r19 finding 2 — a
	// stripped retry dead-lettered during a replay that crashed before
	// its cleanup, restarted after the retention window, was seeded as
	// outstanding and posted again); such a record is renewed (its
	// decision time now) and re-written, since its message is still in
	// the spool. An expired record with no copy present is compacted
	// away — retention is for Slack's external redeliveries only.
	present := make(map[verdictKey]bool)
	for _, e := range entries {
		if e.VerdictTS == "" && e.DeletedTS == "" {
			present[verdictKey{e.Channel, e.Inbound.ProviderMessageID}] = true
		}
	}
	folded := make(map[verdictKey]ladderVerdict)
	var order []verdictKey
	for _, e := range entries {
		if e.VerdictTS == "" {
			continue
		}
		v := verdictOf(e)
		k := verdictKey{e.Channel, e.VerdictTS}
		// A record older than the retention window: a terminal one has
		// aged out of the ledger; an outstanding one never expires by
		// time in memory (the copy it owes is the delivery), but with NO
		// copy of its message anywhere — not in this spool, and the
		// buffer died with the process — only a Slack redelivery could
		// bring one, and Slack's window is over: compacted away. With a
		// copy present, either kind is renewed and applies to it (codex
		// r19 finding 2).
		// An OUTSTANDING record with no copy of its message in the
		// replay is ownership of nothing (codex r26 finding 1): the copy
		// died with the process before its line was written, and the
		// only copy Slack may still send must be delivered, not skipped
		// on a standing verdict. Dropped, whatever its age.
		if !v.retired && !present[k] {
			log.Printf("inbound spool: chan=%s ts=%s an outstanding verdict record has no copy of its message in the spool — dropped; a redelivery is fresh bytes", e.Channel, e.VerdictTS)
			continue
		}
		if stale := now.Sub(v.at) > ladderVerdictRetention; stale {
			if !present[k] {
				continue
			}
			log.Printf("inbound spool: chan=%s ts=%s a verdict record past retention still has a copy of its message in the spool — renewed, the copy takes it", e.Channel, e.VerdictTS)
			v.at = now
		}
		// Folded by PROGRESSION first (a terminal record is never
		// overridden by a stale outstanding one that happens to be newer
		// on disk), the charged count second (codex r34 finding 2: a
		// newer record with a lower count won, and the copy admitted
		// first adopted a budget one step behind), decision time third —
		// the same order as compaction (supersedes).
		if cur, ok := folded[k]; ok {
			if !v.progresses(cur) {
				continue
			}
		} else {
			order = append(order, k)
		}
		folded[k] = v
	}
	durable := true
	for _, k := range order {
		if c == nil {
			durable = false
			break
		}
		v := folded[k]
		log.Printf("inbound spool: chan=%s ts=%s verdict restored — the rejection ladder %s before the restart", k.channel, k.ts, v.disposition())
		if !c.seedVerdict(k.channel, k.ts, v) {
			durable = false
		}
	}
	// Every disposition an ENTRY carries is seeded before any entry is
	// admitted too (codex r17 finding 1): a retained staged file can hold
	// a plain copy of A ahead of the newer spool's stripped (or refused,
	// or isolated) copy, and admitting the plain copy first let a cap
	// flush post the refused bytes before the disposition was reached.
	// Seeded, the plain copy adopts the ladder's copy (or is dropped) at
	// its own admission; the disposition lines below then find their
	// verdict standing.
	for _, e := range entries {
		if e.DeletedTS != "" || e.VerdictTS != "" || c == nil {
			continue
		}
		// A reaction entry carries at most a refusal (its own event ts is
		// its id): seeded like a message's, or an earlier plain copy in
		// the side lane rides an overflow POST before the park retires
		// it (codex r18 finding 1).
		ts := e.Inbound.ProviderMessageID
		switch {
		case e.Refused != "":
			// In memory only: the record follows the dead-letter write
			// (codex r20 finding 1); this entry's own line is the payload.
			c.seedRefused(e.Channel, ts, errors.New(e.Refused))
		case e.Stripped != "":
			// The entry's line may be the decision's only durable home:
			// the staged file stays until the replacement record is
			// confirmed (codex r24 finding 2).
			// The count travels with the disposition (codex r34 finding
			// 2): seeded without it, an earlier plain copy adopted the
			// records' lower count and a cap flush posted it before the
			// replay reached this line — one retry past row 4's bound.
			if !c.seedVerdict(e.Channel, ts, ladderVerdict{stripped: true, cause: errors.New(e.Stripped), attempts: e.Attempts}) {
				durable = false
			}
		case !e.Reaction && (e.Isolate || e.Attempts > 0):
			// A charged REACTION line (written before codex r33 finding 3)
			// seeds nothing: the ladder never owns a reaction.
			if !c.seedVerdict(e.Channel, ts, ownershipOf(pendingChannelInbound{inbound: e.Inbound, isolate: e.Isolate, attempts: e.Attempts})) {
				durable = false
			}
		}
	}
	n := 0
	for _, e := range entries {
		if e.DeletedTS != "" || e.VerdictTS != "" || e.dispositionOnly {
			continue
		}
		p := pendingChannelInbound{
			inbound: e.Inbound, reaction: e.Reaction,
			threadAnchor: e.ThreadAnchor, preamble: e.Preamble, preambleLean: e.PreambleLean, botQuotes: e.BotQuotes, body: e.Body, files: e.Files,
			isolate: e.Isolate, stripped: e.Stripped,
		}
		if !e.Reaction && deleted[e.Channel][e.Inbound.ProviderMessageID] {
			applyDeletion(&p)
			log.Printf("inbound spool: chan=%s ts=%s deleted before restart — replayed as a deletion notice", e.Channel, e.Inbound.ProviderMessageID)
		}
		if e.Refused != "" {
			if v, written := folded[verdictKey{e.Channel, e.Inbound.ProviderMessageID}]; written && v.retired {
				// A TERMINAL record stands only once the dead-letter write
				// confirmed (codex r20 finding 1): this line is the
				// payload's spool copy from before that, now redundant. An
				// OUTSTANDING record — the stripped retry this refusal
				// followed — confirms nothing about the write (codex r24
				// finding 1): the payload is parked below like any owed
				// refusal.
				log.Printf("inbound spool: chan=%s ts=%s was refused before restart and its dead-letter write confirmed (record present) — spool copy dropped", e.Channel, e.Inbound.ProviderMessageID)
				continue
			}
			// Retired before the restart; only its dead-letter write is
			// owed. Parking keeps the refused bytes out of delivery, and
			// re-spools the payload so the staged file may go.
			p.attempts = e.Attempts
			log.Printf("inbound spool: chan=%s ts=%s was refused before restart (%s) — parked for its dead-letter write, not re-posted", e.Channel, e.Inbound.ProviderMessageID, e.Refused)
			if !c.parkDeadLetter(e.Channel, p, errors.New(e.Refused), false) {
				durable = false
			}
			n++
			continue
		}
		if e.Reaction {
			if c.admitReaction(e.Channel, p, false) {
				n++
			}
			continue
		}
		p.attempts = e.Attempts // a charged copy keeps its ladder standing (codex r32 finding 2)
		// The ledger's standing verdict is applied here, as enqueue will
		// apply it: a retired message's copy is dropped WITHOUT being
		// re-spooled (codex r26 minor: re-spooled first, it outlived every
		// restart and kept renewing its expired record); an owned
		// message's copy — the stripped retry, a probe of a refused
		// batch, or a plain copy adopting the ledger's decision — is
		// re-written to the live spool AS the ladder's copy, and only
		// then is its outstanding record written (codex r25 finding 2,
		// r26 finding 1): the ledger must never own a message it has no
		// copy of, and no record may precede its payload.
		p, live := c.adoptForReplay(e.Channel, p)
		if !live {
			log.Printf("inbound spool: chan=%s ts=%s is a copy of a message the rejection ladder retired — dropped, not re-spooled", e.Channel, e.Inbound.ProviderMessageID)
			continue
		}
		if !e.Reaction && onLadderEntry(p) {
			if !s.spillBatch(e.Channel, []pendingChannelInbound{p}) {
				durable = false
			} else if !c.persistVerdict(e.Channel, e.Inbound.ProviderMessageID) {
				durable = false
			}
		}
		if e.Stripped != "" {
			log.Printf("inbound spool: chan=%s ts=%s was refused before restart (%s) — replayed as the stripped retry, its verdict restored", e.Channel, e.Inbound.ProviderMessageID, e.Stripped)
		}
		c.enqueue(e.Channel, p)
		n++
	}
	if done != nil {
		// The cleanup decision includes what ADMISSION did (codex r25
		// finding 3): a cap flush during the replay can refuse entries
		// whose refusal spill and dead-letter write both fail, leaving a
		// parked payload whose only durable copy is the staged file.
		if !durable || c.owesDurability() {
			log.Printf("inbound spool: a verdict record, a re-parked refusal or an owned copy could not be written to the new spool (%d restored) — the staged file is RETAINED for the next startup (its messages replay again then; gc dedup keys bound the damage, a re-parked refusal is parked once)", len(order))
			return n, nil
		}
		done()
	}
	return n, nil
}
