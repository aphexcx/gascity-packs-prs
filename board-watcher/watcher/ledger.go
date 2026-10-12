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
// An entry suppresses at most ONE settled change, and only the change it
// describes. The ledger is append-only and the write path never retires an
// entry, so without that rule every historical entry matched forever: after
// an own deletion of note.md, a peer's recreate-then-delete of the same name
// settled silently against the old tombstone, and a peer's revert to content
// this city once wrote was read as an own write (gf-4c9 row 8; the gf-9x2
// codex gate's ledger.go:54 finding). The watcher therefore keeps, in its
// persisted snapshot, how many times each entry has been spent (keyed by
// the entry's full text: file, sha256 and ts), and a settle also retires the
// entries the settled state supersedes (see settle; round 2, findings 3a
// and 3b). Nothing about the file format changes: the write helpers'
// contract stays as the README and schema/own_writes.schema.json describe
// it.
type ledger struct {
	path string
}

// ledgerKey is the identity of one entry: its file, hash and timestamp, in
// the order the write path records them.
func ledgerKey(e ledgerEntry) string {
	return e.File + "\x1f" + e.SHA256 + "\x1f" + e.TS
}

// settle reports whether the ledger records (file, sha) as an own write that
// has not yet been matched to a settled change, and records in consumed (a
// count per entry key) everything this settle spends:
//
//   - the match itself: the LATEST unconsumed entry of that shape, since the
//     state that settled is the newest own state of the file;
//   - every entry for the file recorded BEFORE the match: an older own state
//     the settled one supersedes (a tombstone followed by a recreate inside
//     one debounce window, an earlier write of other bytes);
//   - with no match at all, every entry for the file on record: the state
//     that settled (a peer's, or a touch) came after all of them.
//
// A retired entry can no longer explain a later change, so a peer's
// deletion after our delete-and-recreate, or a peer's revert to bytes we
// once wrote, is notified (gf-4c9 row 8, round 2: findings 3a and 3b).
// Entries recorded after the match stay live: they describe own states that
// have not settled yet. A missing ledger file suppresses nothing. Malformed
// lines are skipped: a corrupt ledger line should degrade to a spurious
// nudge, not kill the suppression of every other entry.
//
// boundary is the ledger's size, in bytes, captured BEFORE the board was
// observed (ledger.boundary): only entries that lie before it may be
// retired. An own write that lands between the board read and the ledger
// read appends an entry past the boundary, and that entry describes a
// state newer than the one settling here — retiring it would make the own
// write's own settle, one poll later, read as a peer's (the lane's codex
// round 1). The match itself may be any entry, since the state that settled
// may be exactly that late own write.
func (l *ledger) settle(file, sha string, consumed map[string]int, boundary int64) bool {
	entries := l.entriesFor(file)
	if len(entries) == 0 {
		return false
	}
	// occurrence[i] is how many earlier entries share entry i's key, so an
	// entry is unconsumed when consumed[key] <= occurrence[i]: identical
	// lines (same file, hash and second) are one key with several
	// occurrences.
	occurrence := make([]int, len(entries))
	seen := map[string]int{}
	for i, e := range entries {
		key := ledgerKey(e.entry)
		occurrence[i] = seen[key]
		seen[key]++
	}
	match := -1
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].entry.SHA256 == sha && consumed[ledgerKey(entries[i].entry)] <= occurrence[i] {
			match = i
			break
		}
	}
	cutoff := len(entries)
	if match >= 0 {
		cutoff = match + 1
	}
	for i := 0; i < cutoff; i++ {
		if i != match && entries[i].offset >= boundary {
			continue
		}
		key := ledgerKey(entries[i].entry)
		if consumed[key] < occurrence[i]+1 {
			consumed[key] = occurrence[i] + 1
		}
	}
	return match >= 0
}

// retireAll spends every entry on record before boundary, for every file:
// the baseline's view of the board already reflects every write the ledger
// described when the board was read, so none of them may explain a change
// observed after it (finding 3a).
func (l *ledger) retireAll(consumed map[string]int, boundary int64) {
	seen := map[string]int{}
	l.scan(func(e ledgerEntry, offset int64) {
		key := ledgerKey(e)
		seen[key]++
		if offset < boundary && consumed[key] < seen[key] {
			consumed[key] = seen[key]
		}
	})
}

// boundary is the ledger's current size in bytes: every entry on record lies
// before it and every entry appended from now on lies at or past it. Taken
// before the board is read, it is the line settle and retireAll draw
// between the writes a board observation can reflect and the ones it
// cannot. A missing ledger is empty.
func (l *ledger) boundary() int64 {
	info, err := os.Stat(l.path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// positionedEntry is one ledger entry with the byte offset its line starts
// at, for the boundary rule.
type positionedEntry struct {
	entry  ledgerEntry
	offset int64
}

// entriesFor reads the ledger's entries for one file, in file order.
func (l *ledger) entriesFor(file string) []positionedEntry {
	var out []positionedEntry
	l.scan(func(e ledgerEntry, offset int64) {
		if e.File == file {
			out = append(out, positionedEntry{entry: e, offset: offset})
		}
	})
	return out
}

// scan calls fn for every well-formed entry of own-writes.jsonl, in order,
// with the byte offset its line starts at. A missing file is an empty
// ledger.
func (l *ledger) scan(fn func(ledgerEntry, int64)) {
	f, err := os.Open(l.path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var offset int64
	for sc.Scan() {
		raw := sc.Bytes()
		start := offset
		offset += int64(len(raw)) + 1
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		var e ledgerEntry
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		fn(e, start)
	}
}
