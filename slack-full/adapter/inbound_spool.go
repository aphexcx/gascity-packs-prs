package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// maxInboundSpoolBytes bounds a spool read; a healthy spool holds at
// most a few windows' worth of chatter, so anything near this is a bug.
const maxInboundSpoolBytes = 16 << 20

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
	Body         string `json:"body,omitempty"`
	Files        string `json:"files,omitempty"`
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
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %q: %w", s.path, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("encode spool entry chan=%s ts=%s: %w", entry.Channel, entry.Inbound.ProviderMessageID, err)
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write %q: %w", s.path, err)
	}
	return f.Sync()
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
				salvagedDeletions = readDeletionRecords(s.path)
			} else if err := os.Remove(s.path); err != nil {
				log.Printf("inbound spool: remove %s after merge: %v (next startup may replay duplicates; gc dedup keys bound the damage)", s.path, err)
			}
		} else if err := os.Rename(s.path, rp); err != nil {
			// Rename failed (permissions?): fall back to reading the spool
			// in place. cleanup then removes the spool itself — still only
			// after re-admission, which is the invariant that matters.
			log.Printf("inbound spool: rename %s -> %s failed: %v — replaying in place", s.path, rp, err)
			readPath = s.path
		}
	}
	data, err := os.ReadFile(readPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbound spool: %s unreadable (%v) — nothing replayed", readPath, err)
		}
		return nil, nil
	}
	if len(data) > maxInboundSpoolBytes {
		log.Printf("inbound spool: %s oversized (%d bytes) — refusing to replay; inspect and remove it manually", readPath, len(data))
		return nil, nil
	}
	var entries []spooledInbound
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxInboundSpoolBytes)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e spooledInbound
		if err := json.Unmarshal(line, &e); err != nil || e.Channel == "" {
			log.Printf("inbound spool: CORRUPT LINE (%d bytes) DROPPED — LOSS: this can only be a crash mid-spill of that batch (parse err: %v)", len(line), err)
			continue
		}
		// Lines written with parts omit the redundant folded Text;
		// rebuild it here so every consumer sees a complete entry.
		if e.Inbound.Text == "" {
			if p := (pendingChannelInbound{preamble: e.Preamble, body: e.Body, files: e.Files}); p.hasReminderParts() {
				e.Inbound.Text = p.foldedText()
			}
		}
		entries = append(entries, e)
	}
	entries = append(entries, salvagedDeletions...)
	cleanup := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := os.Remove(readPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbound spool: remove %s after replay: %v (the next restart may replay duplicates; gc dedup keys bound the damage)", readPath, err)
		}
	}
	return entries, cleanup
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
func readDeletionRecords(path string) []spooledInbound {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > maxInboundSpoolBytes {
		return nil
	}
	var out []spooledInbound
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), maxInboundSpoolBytes)
	for sc.Scan() {
		var e spooledInbound
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Channel == "" || e.DeletedTS == "" {
			continue
		}
		out = append(out, spooledInbound{Channel: e.Channel, DeletedTS: e.DeletedTS})
	}
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
	n := 0
	for _, e := range entries {
		if e.DeletedTS != "" {
			continue
		}
		p := pendingChannelInbound{
			inbound: e.Inbound, reaction: e.Reaction,
			threadAnchor: e.ThreadAnchor, preamble: e.Preamble, body: e.Body, files: e.Files,
		}
		if !e.Reaction && deleted[e.Channel][e.Inbound.ProviderMessageID] {
			applyDeletion(&p)
			log.Printf("inbound spool: chan=%s ts=%s deleted before restart — replayed as a deletion notice", e.Channel, e.Inbound.ProviderMessageID)
		}
		if e.Reaction {
			if c.admitReaction(e.Channel, p, false) {
				n++
			}
			continue
		}
		c.enqueue(e.Channel, p)
		n++
	}
	if done != nil {
		done()
	}
	return n
}
