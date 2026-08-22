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
)

// inbound_spool.go — durable shutdown spool for admitted-but-undelivered
// inbounds (gp-9e7 fix round 2a'/2b').
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
// truncates at most the final line, which consume() skips with a log.
// The next startup consumes the file (read + remove) and re-buffers
// every entry through the coalescer's normal admission — messages
// re-enter the coalesce window (delivering on its timer), reactions
// re-enter the no-wake side lane — so replay inherits every delivery
// invariant, including "reactions never wake a session solo".

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
}

// inboundSpool is the file-backed spool. All methods are nil-safe (a
// nil spool means the operator disabled spooling with an empty path —
// the coalescer then logs residue as lost).
type inboundSpool struct {
	mu   sync.Mutex
	path string
}

// newInboundSpool returns the spool for path, or nil when path is empty
// (spooling disabled).
func newInboundSpool(path string) *inboundSpool {
	if path == "" {
		return nil
	}
	return &inboundSpool{path: path}
}

// spillBatch appends one channel's undeliverable batch to the spool.
// This is the coalescer's spill hook: errors are logged (there is no
// caller able to retry — the process is exiting), never returned.
func (s *inboundSpool) spillBatch(channel string, batch []pendingChannelInbound) {
	if s == nil || len(batch) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.appendLocked(channel, batch); err != nil {
		log.Printf("inbound spool: SPOOL WRITE FAILED chan=%s %d item(s) LOST: %v", channel, len(batch), err)
	}
}

func (s *inboundSpool) appendLocked(channel string, batch []pendingChannelInbound) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(s.path), err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open %q: %w", s.path, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, p := range batch {
		line, err := json.Marshal(spooledInbound{Channel: channel, Reaction: p.reaction, Inbound: p.inbound})
		if err != nil {
			return fmt.Errorf("encode spool entry chan=%s ts=%s: %w", channel, p.inbound.ProviderMessageID, err)
		}
		w.Write(line)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write %q: %w", s.path, err)
	}
	return f.Sync()
}

// consume reads and REMOVES the spool file, returning its entries.
// Corrupt lines (a process death mid-append truncates at most the final
// one) are logged and skipped; a missing file is simply empty.
func (s *inboundSpool) consume() []spooledInbound {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("inbound spool: %s unreadable (%v) — nothing replayed", s.path, err)
		}
		return nil
	}
	if len(data) > maxInboundSpoolBytes {
		log.Printf("inbound spool: %s oversized (%d bytes) — refusing to replay; inspect and remove it manually", s.path, len(data))
		return nil
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
			log.Printf("inbound spool: skipping corrupt line (%d bytes): %v", len(line), err)
			continue
		}
		entries = append(entries, e)
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("inbound spool: remove %s after consume: %v (replay may duplicate on the next restart; gc dedup keys bound the damage)", s.path, err)
	}
	return entries
}

// replayInto consumes the spool and re-buffers every entry through the
// coalescer's normal admission, returning the count re-buffered.
// Messages re-enter the coalesce window (the timer delivers them within
// one window of startup); reactions re-enter the no-wake side lane and
// piggyback on the channel's next real delivery, so replay can never
// produce the solo reaction wake the side lane exists to prevent.
func (s *inboundSpool) replayInto(c *inboundCoalescer) int {
	n := 0
	for _, e := range s.consume() {
		p := pendingChannelInbound{inbound: e.Inbound, reaction: e.Reaction}
		if e.Reaction {
			if c.admitReaction(e.Channel, p, false) {
				n++
			}
			continue
		}
		c.enqueue(e.Channel, p)
		n++
	}
	return n
}
