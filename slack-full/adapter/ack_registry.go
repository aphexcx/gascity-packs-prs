package main

import (
	"sync"
	"time"
)

// The receipt window and cap match the scale of the separate busy registry.
// Active picks never expire or get evicted: a twin event must not select a
// different emoji while the first pick is still running.
const ackReceiptTTL = 30 * time.Minute
const ackReceiptMaxEntries = 4096

type ackReceiptKey struct{ channel, ts string }
type ackReceiptEntry struct {
	finished time.Time // zero while picking/posting
}
type ackReceiptRegistry struct {
	mu      sync.Mutex
	changed *sync.Cond
	entries map[ackReceiptKey]ackReceiptEntry
}

func newAckReceiptRegistry() *ackReceiptRegistry {
	r := &ackReceiptRegistry{entries: make(map[ackReceiptKey]ackReceiptEntry)}
	r.changed = sync.NewCond(&r.mu)
	return r
}

func (r *ackReceiptRegistry) claim(key ackReceiptKey, now time.Time) bool {
	if r == nil {
		return true // manually assembled configs may omit runtime registries
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		var oldest ackReceiptKey
		var oldestTime time.Time
		for k, entry := range r.entries {
			if entry.finished.IsZero() {
				continue
			}
			if now.Sub(entry.finished) >= ackReceiptTTL {
				delete(r.entries, k)
				continue
			}
			if oldestTime.IsZero() || entry.finished.Before(oldestTime) {
				oldest, oldestTime = k, entry.finished
			}
		}
		if _, exists := r.entries[key]; exists {
			return false
		}
		if len(r.entries) < ackReceiptMaxEntries {
			r.entries[key] = ackReceiptEntry{}
			return true
		}
		if !oldestTime.IsZero() {
			delete(r.entries, oldest)
			continue
		}
		// All entries are in flight. Backpressure keeps memory bounded without
		// evicting a live pick or dropping this distinct message.
		r.changed.Wait()
	}
}

func (r *ackReceiptRegistry) finish(key ackReceiptKey, now time.Time) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.entries[key] = ackReceiptEntry{finished: now}
	r.changed.Broadcast()
	r.mu.Unlock()
}
