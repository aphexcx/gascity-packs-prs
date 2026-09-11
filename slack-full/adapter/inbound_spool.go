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

// recordVerdict appends a terminal verdict record for (channel, ts).
// Same seal and durability contract as recordDeletion. Nil-safe; the
// coalescer's recordVerdict hook.
func (s *inboundSpool) recordVerdict(channel, ts string, v ladderVerdict) bool {
	if s == nil || channel == "" || ts == "" || !v.retired {
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
	entry := spooledInbound{Channel: channel, VerdictTS: ts, Verdict: truncateReason(rejectionReasonText(v.cause)), VerdictDelivered: v.delivered, VerdictAt: at.Unix()}
	if err := s.appendLinesLocked([]spooledInbound{entry}); err != nil {
		log.Printf("inbound spool: verdict record write FAILED chan=%s ts=%s: %v", channel, ts, err)
		return false
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
		if p.refused != "" || p.stripped != "" {
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
func (s *inboundSpool) consume() ([]spooledInbound, func()) {
	if s == nil {
		return nil, nil
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
				salvagedDeletions = readRecords(s.path)
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
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbound spool: %s unreadable (%v) — nothing replayed", readPath, err)
		}
		return nil, nil
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
	return entries, cleanup
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
	records, rerr := readSpoolLines(s.path, true)
	if rerr != nil {
		log.Printf("inbound spool: compacting %s in place FAILED (read: %v) — left as it is; its messages replay again next startup", s.path, rerr)
		return
	}
	type key struct{ channel, ts string }
	newest := make(map[key]spooledInbound)
	var order []key
	for _, e := range records {
		if e.VerdictTS == "" {
			continue
		}
		k := key{e.Channel, e.VerdictTS}
		if cur, ok := newest[k]; ok {
			if e.VerdictAt <= cur.VerdictAt {
				continue
			}
		} else {
			order = append(order, k)
		}
		newest[k] = e
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".spool-compact-*")
	if err != nil {
		log.Printf("inbound spool: compacting %s in place FAILED (temp file: %v) — left as it is; its messages replay again next startup", s.path, err)
		return
	}
	tmpPath := tmp.Name()
	w := bufio.NewWriter(tmp)
	for _, k := range order {
		line, merr := json.Marshal(newest[k])
		if merr != nil {
			continue
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	// Every step feeds ONE error: a failed flush or fsync must reach the
	// rename guard, or an incomplete temp file replaces the journal and
	// its verdicts are lost (codex r16 finding 1: a shadowed err let the
	// rename proceed).
	err = w.Flush()
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
	log.Printf("inbound spool: %s replayed in place and compacted to %d verdict record(s)", s.path, len(order))
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
				out = append(out, spooledInbound{Channel: e.Channel, VerdictTS: e.VerdictTS, Verdict: e.Verdict, VerdictDelivered: e.VerdictDelivered, VerdictAt: e.VerdictAt})
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
	entries, done := s.consume()
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
	folded := make(map[verdictKey]ladderVerdict)
	var order []verdictKey
	for _, e := range entries {
		if e.VerdictTS == "" {
			continue
		}
		v := ladderVerdict{retired: true, delivered: e.VerdictDelivered, cause: errors.New(e.Verdict), at: time.Unix(e.VerdictAt, 0)}
		if v.expired(now) {
			continue
		}
		k := verdictKey{e.Channel, e.VerdictTS}
		if cur, ok := folded[k]; ok {
			if !v.at.After(cur.at) {
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
		log.Printf("inbound spool: chan=%s ts=%s verdict restored — the rejection ladder %s before the restart; every later copy is a duplicate", k.channel, k.ts, v.disposition())
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
		if e.DeletedTS != "" || e.VerdictTS != "" || e.Reaction || c == nil {
			continue
		}
		ts := e.Inbound.ProviderMessageID
		switch {
		case e.Refused != "":
			if !c.seedVerdict(e.Channel, ts, ladderVerdict{retired: true, cause: errors.New(e.Refused)}) {
				durable = false
			}
		case e.Stripped != "":
			c.seedVerdict(e.Channel, ts, ladderVerdict{stripped: true, cause: errors.New(e.Stripped)})
		case e.Isolate:
			c.seedVerdict(e.Channel, ts, ownershipOf(pendingChannelInbound{inbound: e.Inbound, isolate: true, attempts: e.Attempts}))
		}
	}
	n := 0
	for _, e := range entries {
		if e.DeletedTS != "" || e.VerdictTS != "" {
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
			// Retired before the restart; only its dead-letter write is
			// owed. Parking keeps the refused bytes out of delivery.
			p.attempts = e.Attempts
			log.Printf("inbound spool: chan=%s ts=%s was refused before restart (%s) — parked for its dead-letter write, not re-posted", e.Channel, e.Inbound.ProviderMessageID, e.Refused)
			// The Refused line was this retirement's only durable state:
			// the park's record must confirm before the staged file may
			// go (codex r14 finding 3).
			if !c.parkDeadLetter(e.Channel, p, errors.New(e.Refused)) {
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
		if e.Stripped != "" {
			// The stripped retry, owed or in flight at shutdown: its
			// disposition is the ledger's outstanding verdict for the
			// message, seeded above before any copy was admitted so an
			// earlier or later copy adopts it and the landing settles it
			// (codex r12 finding 1, r17 finding 1).
			p.attempts = e.Attempts
			log.Printf("inbound spool: chan=%s ts=%s was refused before restart (%s) — replayed as the stripped retry, its verdict restored", e.Channel, e.Inbound.ProviderMessageID, e.Stripped)
		}
		c.enqueue(e.Channel, p)
		n++
	}
	if done != nil {
		if !durable {
			log.Printf("inbound spool: a verdict record could not be written to the new spool (%d restored, plus any re-parked refusal) — the staged file is RETAINED for the next startup (its messages replay again then; gc dedup keys bound the damage, a re-parked refusal is parked once)", len(order))
			return n
		}
		done()
	}
	return n
}
