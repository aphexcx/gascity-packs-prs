package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
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
func (l *ledger) settle(file, sha string, consumed map[string]int, boundary ledgerBoundary) bool {
	entries := l.entriesFor(file)
	if len(entries) == 0 {
		return false
	}
	cut := l.validBoundary(boundary)
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
		if i != match && entries[i].offset >= cut {
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
func (l *ledger) retireAll(consumed map[string]int, boundary ledgerBoundary) {
	cut := l.validBoundary(boundary)
	seen := map[string]int{}
	l.scan(func(e ledgerEntry, offset int64) {
		key := ledgerKey(e)
		seen[key]++
		if offset < cut && consumed[key] < seen[key] {
			consumed[key] = seen[key]
		}
	})
}

// retireSettled spends every entry a settled snapshot already accounts for:
// the seed for a snapshot persisted by a binary that kept no counts. The
// evidence is per file and comes from the ledger's own order, never from a
// clock (the write helper's ts is this host's clock, a remote board's mtime
// is the board host's; the lane's codex rounds 3 to 5): for each file, the
// LAST entry that is the settled state — the content whose hash the
// snapshot settled on, or a tombstone when the snapshot has no such file —
// is spent, and so is every entry for the file before it, which a later own
// write superseded. Entries after it, and every entry of a file whose
// settled state the ledger never recorded, are writes the catch-up cycle
// has yet to settle (an own update still debouncing at the last persist, a
// write that landed while the watcher was stopped) and stay live.
func (l *ledger) retireSettled(consumed map[string]int, files map[string]snapshotEntry) {
	var entries []ledgerEntry
	l.scan(func(e ledgerEntry, _ int64) { entries = append(entries, e) })
	last := map[string]int{}
	for i, e := range entries {
		settled, present := files[e.File]
		if (present && settled.SHA256 != "" && e.SHA256 == settled.SHA256) || (!present && e.SHA256 == ledgerTombstone) {
			last[e.File] = i
		}
	}
	seen := map[string]int{}
	for i, e := range entries {
		key := ledgerKey(e)
		seen[key]++
		if cut, ok := last[e.File]; !ok || i > cut {
			continue
		}
		if consumed[key] < seen[key] {
			consumed[key] = seen[key]
		}
	}
}

// ledgerBoundary is the retirement line a cycle draws before it reads the
// board: the ledger's size then, and the hash of the bytes before it. Only
// an entry that lies before the line may be retired by a settle of that
// cycle; an entry appended after the board read lies past it, and that
// entry describes a write the board read could not have reflected.
type ledgerBoundary struct {
	size   int64
	prefix [sha256.Size]byte
}

// validBoundary returns the boundary's offset, or 0 when the ledger no
// longer starts with the bytes the boundary was drawn after: the file was
// pruned or rewritten since (the README permits pruning), so its offsets
// no longer say which entries predate the board read, and retiring nothing
// is the side that costs a spurious nudge rather than a lost one. Size
// alone does not prove the prefix: a ledger pruned and regrown can be as
// large as before with a new entry at offset zero (the lane's codex rounds
// 5 and 7).
func (l *ledger) validBoundary(boundary ledgerBoundary) int64 {
	if boundary.size == 0 {
		return 0
	}
	prefix, ok := l.prefixHash(boundary.size)
	if !ok || prefix != boundary.prefix {
		return 0
	}
	return boundary.size
}

// boundary is the ledger's current retirement line: its size in bytes
// (every entry on record lies before it and every entry appended from now
// on lies at or past it) and the hash of those bytes. Taken before the
// board is read, it is the line settle and retireAll draw between the
// writes a board observation can reflect and the ones it cannot. A missing
// ledger is empty.
func (l *ledger) boundary() ledgerBoundary {
	info, err := os.Stat(l.path)
	if err != nil {
		return ledgerBoundary{}
	}
	prefix, ok := l.prefixHash(info.Size())
	if !ok {
		return ledgerBoundary{}
	}
	return ledgerBoundary{size: info.Size(), prefix: prefix}
}

// prefixHash hashes the ledger's first size bytes; ok is false when the
// ledger cannot be read or is shorter than size.
func (l *ledger) prefixHash(size int64) (prefix [sha256.Size]byte, ok bool) {
	f, err := os.Open(l.path)
	if err != nil {
		return prefix, false
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.CopyN(h, f, size)
	if err != nil || n != size {
		return prefix, false
	}
	copy(prefix[:], h.Sum(nil))
	return prefix, true
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
	// Lines are read with their delimiter bytes so each offset is the byte
	// the line starts at in the file, whatever the line ending (a scanner
	// would drop a carriage return and undercount; the lane's codex round
	// 5).
	rd := bufio.NewReaderSize(f, 64*1024)
	var offset int64
	for {
		raw, err := rd.ReadBytes('\n')
		start := offset
		offset += int64(len(raw))
		line := bytes.TrimSpace(raw)
		if len(line) > 0 {
			var e ledgerEntry
			if json.Unmarshal(line, &e) == nil {
				fn(e, start)
			}
		}
		if err != nil {
			return
		}
	}
}
