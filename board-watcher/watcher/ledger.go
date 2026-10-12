package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
)

// ledgerTombstone is the sha256 value that records an own DELETION of a
// board doc, so a city's own removals don't nudge its mayor.
const ledgerTombstone = "-"

// ledgerEntry is one line of own-writes.jsonl — the interop contract with
// each city's write path (citadel's board-record-write command, boomtown's
// board-post helper). See the pack README "Own-write ledger" section; the
// canonical shape is schema/own_writes.schema.json.
type ledgerEntry struct {
	Schema int    `json:"schema"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	TS     string `json:"ts"`
}

// ledger answers "did we write this ourselves?" against own-writes.jsonl.
// It re-reads the file on every check: checks happen only when a change
// settles (rare), the file is tiny, and re-reading picks up hashes the
// write helper appended moments ago without any coordination.
//
// An entry suppresses exactly ONE settled change. The ledger is append-only
// and the write path never retires an entry, so without that rule every
// historical entry matched forever: after an own deletion of note.md, a
// peer's recreate-then-delete of the same name settled silently against the
// old tombstone, and a peer's revert to content this city once wrote was
// read as an own write (gf-4c9 row 8; the gf-9x2 codex gate's ledger.go:54
// finding). The watcher therefore keeps, in its persisted snapshot, how many
// times each entry has already been consumed (keyed by the entry's full
// text: file, sha256 and ts, so two genuine own writes of identical content
// at different times are two entries and suppress two changes). Nothing
// about the file format changes: the write helpers' contract stays as the
// README and schema/own_writes.schema.json describe it.
type ledger struct {
	path string
}

// ledgerKey is the identity of one entry: its file, hash and timestamp, in
// the order the write path records them.
func ledgerKey(e ledgerEntry) string {
	return e.File + "\x1f" + e.SHA256 + "\x1f" + e.TS
}

// consume reports whether the ledger records (file, sha) as an own write
// that has not yet been matched to a settled change, and if so records the
// match in consumed (a count per entry key) so the same entry cannot
// suppress a second change. A missing ledger file suppresses nothing.
// Malformed lines are skipped: a corrupt ledger line should degrade to a
// spurious nudge, not kill the suppression of every other entry.
func (l *ledger) consume(file, sha string, consumed map[string]int) bool {
	f, err := os.Open(l.path)
	if err != nil {
		return false
	}
	defer f.Close()
	// Count the matching entries by key first: identical lines (same file,
	// hash and second) are one key with several occurrences.
	occurrences := map[string]int{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e ledgerEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.File != file || e.SHA256 != sha {
			continue
		}
		key := ledgerKey(e)
		if occurrences[key] == 0 {
			order = append(order, key)
		}
		occurrences[key]++
	}
	for _, key := range order {
		if consumed[key] < occurrences[key] {
			consumed[key]++
			return true
		}
	}
	return false
}
