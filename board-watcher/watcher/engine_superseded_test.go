package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The gf-4c9 round-2 gate findings on row 8 (packs vs upstream at
// engine.go:284, packs vs main at ledger.go:89; gf-jr0 findings 3a and 3b):
// an own-write ledger entry stayed eligible after the state it recorded had
// been superseded — by an identical-content rewrite that settled as
// "content unchanged" without consuming it, by a baseline that recorded the
// board without consuming anything, or by a newer own state (a recreate
// inside the deletion's debounce window) that settled instead of it. Each
// stale entry then matched a later PEER change of the same shape and
// silenced its notification. A settle now retires every entry the settled
// state supersedes, and the baseline retires everything on record.

// TestIdenticalRewriteThenPeerRevertNotifies is finding 3a's first
// sequence: our own write of A settles silently; our own rewrite of the
// SAME content settles as unchanged; a peer then changes A to B (notified)
// and back to A. That revert is the peer's change: the second own entry,
// superseded by the unchanged-content settle, may not read it as ours.
func TestIdenticalRewriteThenPeerRevertNotifies(t *testing.T) {
	r := newTestRig(t)
	r.tick() // empty baseline

	ours := "# our own post\n"
	sha := sha256hex([]byte(ours))
	r.recordOwnWrite(t, "ours.md", sha)
	r.be.set("ours.md", 4000, ours)
	r.pastDebounce()
	if len(r.not.calls) != 0 {
		t.Fatalf("own write notified: %v", r.not.calls)
	}

	// The same bytes written again by this city: a second entry, an mtime
	// bump, and a settle that sees the content unchanged.
	r.recordOwnWrite(t, "ours.md", sha)
	r.be.set("ours.md", 5000, ours)
	r.pastDebounce()
	if len(r.not.calls) != 0 {
		t.Fatalf("identical rewrite notified: %v", r.not.calls)
	}

	r.be.set("ours.md", 6000, ours+"peer amendment\n")
	r.pastDebounce()
	if len(r.not.calls) != 1 || r.not.calls[0].Verb != "updated" {
		t.Fatalf("peer edit: %v", r.not.calls)
	}
	r.be.set("ours.md", 7000, ours)
	r.pastDebounce()
	if len(r.not.calls) != 2 || r.not.calls[1].Verb != "updated" {
		t.Fatalf("the peer's revert after our identical rewrite was not notified (the stale own entry suppressed it): %v", r.not.calls)
	}
}

// TestBaselineRetiresHistoricalLedgerEntries is finding 3a's second
// sequence: the ledger already holds entries when the watcher baselines
// (our content of doc.md, our tombstone of gone.md). Everything in the
// ledger at baseline describes a write that is already part of the
// baseline state, so a later peer change that happens to match one of those
// entries — a revert to our old content, a deletion of a doc we once
// deleted — is the peer's and must be notified.
func TestBaselineRetiresHistoricalLedgerEntries(t *testing.T) {
	r := newTestRig(t)
	ours := "# ours\n"
	r.recordOwnWrite(t, "doc.md", sha256hex([]byte(ours)))
	r.recordOwnWrite(t, "gone.md", ledgerTombstone)
	r.be.set("doc.md", 1000, ours)
	r.tick() // baseline: doc.md present with our content, gone.md absent
	if len(r.not.calls) != 0 {
		t.Fatalf("baseline notified: %v", r.not.calls)
	}

	r.be.set("doc.md", 2000, ours+"peer amendment\n")
	r.pastDebounce()
	if len(r.not.calls) != 1 || r.not.calls[0].Verb != "updated" {
		t.Fatalf("peer edit: %v", r.not.calls)
	}
	r.be.set("doc.md", 3000, ours)
	r.pastDebounce()
	if len(r.not.calls) != 2 || r.not.calls[1].Verb != "updated" || r.not.calls[1].Name != "doc.md" {
		t.Fatalf("the peer's revert to our pre-baseline content was not notified: %v", r.not.calls)
	}

	r.be.set("gone.md", 4000, "the peer's new doc")
	r.pastDebounce()
	if len(r.not.calls) != 3 || r.not.calls[2].Verb != "added" {
		t.Fatalf("peer add: %v", r.not.calls)
	}
	r.be.remove("gone.md")
	r.pastDebounce()
	if len(r.not.calls) != 4 || r.not.calls[3].Verb != "removed" || r.not.calls[3].Name != "gone.md" {
		t.Fatalf("the peer's deletion was not notified (our pre-baseline tombstone suppressed it): %v", r.not.calls)
	}
}

// TestOwnDeleteAndRecreateInOneWindowThenPeerDeleteNotifies is finding 3b:
// this city deletes ours.md and recreates it before the deletion settles,
// so only the recreated content settles (silently, against its entry) and
// the deletion's tombstone was left unconsumed. A peer's later deletion
// matched that historical tombstone and lost its notification. The
// recreate's settle now retires the superseded tombstone, persisted with
// the snapshot, so a restart in between changes nothing.
func TestOwnDeleteAndRecreateInOneWindowThenPeerDeleteNotifies(t *testing.T) {
	r := newTestRig(t)
	r.be.set("ours.md", 1000, "ours v1")
	r.tick() // baseline

	// Delete, observed once; then recreate inside the quiet window.
	r.recordOwnWrite(t, "ours.md", ledgerTombstone)
	r.be.remove("ours.md")
	r.tick()
	r.advance(time.Second)
	recreated := "ours v2"
	r.recordOwnWrite(t, "ours.md", sha256hex([]byte(recreated)))
	r.be.set("ours.md", 2000, recreated)
	r.pastDebounce()
	if len(r.not.calls) != 0 {
		t.Fatalf("own delete-and-recreate notified: %v", r.not.calls)
	}
	snap, err := loadSnapshot(r.eng.cfg.snapshotPath())
	if err != nil || snap == nil {
		t.Fatalf("snapshot not persisted: %v", err)
	}
	if got := snap.Files["ours.md"].SHA256; got != sha256hex([]byte(recreated)) {
		t.Fatalf("snapshot content = %s, want the recreated doc", got)
	}

	// A restart in between must not re-arm the superseded tombstone.
	r2 := &testRig{be: r.be, not: &fakeNotifier{}, clock: r.clock.Add(time.Minute)}
	r2.eng = newEngine(r.eng.cfg, r.be, r2.not)
	r2.eng.now = func() time.Time { return r2.clock }
	r2.tick()

	r2.be.remove("ours.md")
	r2.pastDebounce()
	if len(r2.not.calls) != 1 || r2.not.calls[0].Verb != "removed" || r2.not.calls[0].Name != "ours.md" {
		t.Fatalf("the peer's deletion after our delete-and-recreate was not notified (the superseded tombstone suppressed it): %v", r2.not.calls)
	}
}

// TestOwnWriteSupersededByAPeerBeforeSettleDoesNotSilenceTheRevert is the
// content twin of finding 3b: our own write of A is overtaken by a peer's
// B before anything settles, so B settles (notified) and A's entry was
// never consumed; the peer's later revert to A is theirs.
func TestOwnWriteSupersededByAPeerBeforeSettleDoesNotSilenceTheRevert(t *testing.T) {
	r := newTestRig(t)
	r.be.set("doc.md", 1000, "v0")
	r.tick() // baseline

	ours := "ours A"
	r.recordOwnWrite(t, "doc.md", sha256hex([]byte(ours)))
	r.be.set("doc.md", 2000, ours)
	r.tick()
	r.advance(time.Second)
	r.be.set("doc.md", 3000, "peer B")
	r.pastDebounce()
	if len(r.not.calls) != 1 || r.not.calls[0].Verb != "updated" {
		t.Fatalf("the peer's B that overtook our write: %v", r.not.calls)
	}
	r.be.set("doc.md", 4000, ours)
	r.pastDebounce()
	if len(r.not.calls) != 2 || r.not.calls[1].Verb != "updated" {
		t.Fatalf("the peer's revert to our never-settled content was not notified: %v", r.not.calls)
	}
}

// TestLoadOfASnapshotWithoutLedgerCountsSeedsThem is the pre-upgrade half
// of finding 3a: a watcher upgraded in place loads a snapshot persisted by a
// binary that kept no counts, so every historical ledger entry would get one
// suppression. The load seeds the counts for everything on record and
// persists them at once; an entry appended after that load is live, and a
// second restart does not seed again.
func TestLoadOfASnapshotWithoutLedgerCountsSeedsThem(t *testing.T) {
	r := newTestRig(t)
	ours := "# ours\n"
	superseded := "# ours, an older version\n"
	// The pre-upgrade snapshot: doc.md settled at our content at 00:30, no
	// counts. The historical entries (00:00) predate that settle: the
	// superseded content, the settled content, a tombstone of an absent doc.
	settledAt := time.Date(2026, 7, 30, 0, 30, 0, 0, time.UTC).Unix()
	r.be.set("doc.md", settledAt, ours)
	pre := fmt.Sprintf(`{"schema":1,"files":{"doc.md":{"mtime_unix":%d,"size":%d,"sha256":%q}}}`+"\n",
		settledAt, len(ours), sha256hex([]byte(ours)))
	if err := os.MkdirAll(filepath.Dir(r.eng.cfg.snapshotPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.eng.cfg.snapshotPath(), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	r.recordOwnWrite(t, "doc.md", sha256hex([]byte(superseded)))
	r.recordOwnWrite(t, "doc.md", sha256hex([]byte(ours)))
	r.recordOwnWrite(t, "gone.md", ledgerTombstone)
	// One own write of a new doc landed while the watcher was stopped: its
	// file is on the board and its entry is dated after doc.md's settle; the
	// snapshot does not account for it.
	pending := "# ours, written while the watcher was down\n"
	r.recordOwnWriteAt(t, "pending.md", sha256hex([]byte(pending)), "2026-07-30T00:45:00Z")
	r.be.set("pending.md", settledAt+900, pending)

	r.tick() // loads the snapshot, seeds the counts
	snap, err := loadSnapshot(r.eng.cfg.snapshotPath())
	if err != nil || snap == nil || snap.LedgerConsumed == nil {
		t.Fatalf("the seed was not persisted with the snapshot: %+v (%v)", snap, err)
	}
	if len(snap.LedgerConsumed) != 3 {
		t.Fatalf("seeded counts = %v, want the three historical entries and not the pending one", snap.LedgerConsumed)
	}
	// The catch-up cycle settles the pending own write silently.
	r.advance(r.eng.cfg.debounce + time.Second)
	r.tick()
	if len(r.not.calls) != 0 {
		t.Fatalf("the own write that landed while the watcher was stopped was notified (its entry was seeded away): %v", r.not.calls)
	}

	// A peer's revert to the superseded bytes, and a peer's deletion of a
	// doc we once deleted, are theirs.
	r.be.set("doc.md", 2000, superseded)
	r.pastDebounce()
	if len(r.not.calls) != 1 || r.not.calls[0].Verb != "updated" {
		t.Fatalf("the peer's change to our superseded pre-upgrade bytes was not notified: %v", r.not.calls)
	}
	r.be.set("gone.md", 3000, "peer")
	r.pastDebounce()
	r.be.remove("gone.md")
	r.pastDebounce()
	if len(r.not.calls) != 3 || r.not.calls[2].Verb != "removed" {
		t.Fatalf("the peer's deletion was not notified (the pre-upgrade tombstone suppressed it): %v", r.not.calls)
	}

	// An own write after the upgrade is live, across a restart in between:
	// the seeded snapshot must not be seeded again.
	fresh := "# ours, after the upgrade\n"
	r.recordOwnWrite(t, "doc.md", sha256hex([]byte(fresh)))
	r2 := &testRig{be: r.be, not: &fakeNotifier{}, clock: r.clock.Add(time.Minute)}
	r2.eng = newEngine(r.eng.cfg, r.be, r2.not)
	r2.eng.now = func() time.Time { return r2.clock }
	r2.tick()
	r2.be.set("doc.md", 4000, fresh)
	r2.pastDebounce()
	if len(r2.not.calls) != 0 {
		t.Fatalf("an own write recorded after the seed was notified after a restart (the counts were seeded again): %v", r2.not.calls)
	}
}

// TestOwnWriteLandingDuringAPeerSettleStaysLive is the lane's codex round 1
// on the retirement rule: a peer's A is being settled (its content already
// fetched) when this city's own write of B lands — the file and its ledger
// entry — before the settle reads the ledger. A's settle matches nothing and
// retires what it supersedes, which must not include B's entry: it lies
// past the boundary drawn before the board was read, and B is the next
// state to settle, silently, as our own.
func TestOwnWriteLandingDuringAPeerSettleStaysLive(t *testing.T) {
	r := newTestRig(t)
	r.be.set("doc.md", 1000, "v0")
	r.tick() // baseline

	ours := "ours B"
	landed := false
	r.be.onFetch = func(name string) {
		if name != "doc.md" || landed {
			return
		}
		landed = true
		r.be.set("doc.md", 3000, ours)
		r.recordOwnWrite(t, "doc.md", sha256hex([]byte(ours)))
	}
	r.be.set("doc.md", 2000, "peer A")
	r.pastDebounce()
	if !landed {
		t.Fatal("the own write did not land during the peer's settle")
	}
	if len(r.not.calls) != 1 || r.not.calls[0].Verb != "updated" {
		t.Fatalf("the peer's A: %v", r.not.calls)
	}
	r.be.onFetch = nil

	r.pastDebounce()
	if len(r.not.calls) != 1 {
		t.Fatalf("our own write that landed during the peer's settle was notified (its entry was retired by that settle): %v", r.not.calls)
	}
	snap, _ := loadSnapshot(r.eng.cfg.snapshotPath())
	if got := snap.Files["doc.md"].SHA256; got != sha256hex([]byte(ours)) {
		t.Fatalf("snapshot content = %s, want our B", got)
	}
}

// TestSeedKeepsAnOwnUpdateThatWasDebouncingAtTheLastPersist is the lane's
// codex round 3 on the seed: an own update to a.md is still debouncing when
// an unrelated peer update to b.md settles and persists the (count-less)
// snapshot, so the snapshot is NEWER than a.md's entry although a.md's
// settled state is older. The seed must judge a.md's entry against a.md's
// own settled state and keep it: the catch-up settles our update silently.
func TestSeedKeepsAnOwnUpdateThatWasDebouncingAtTheLastPersist(t *testing.T) {
	r := newTestRig(t)
	base := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	ours := "# ours, updated\n"
	r.be.set("a.md", base.Unix(), "a v0")
	r.be.set("b.md", base.Unix(), "b v0")
	// The pre-upgrade snapshot: both docs settled at base, b.md later settled
	// a peer's update at +20m (the persist that made the snapshot newer than
	// our a.md update at +10m, which was still debouncing).
	peerB := "b, the peer's version"
	pre := fmt.Sprintf(`{"schema":1,"files":{"a.md":{"mtime_unix":%d,"size":4,"sha256":%q},"b.md":{"mtime_unix":%d,"size":%d,"sha256":%q}}}`+"\n",
		base.Unix(), sha256hex([]byte("a v0")), base.Add(20*time.Minute).Unix(), len(peerB), sha256hex([]byte(peerB)))
	if err := os.MkdirAll(filepath.Dir(r.eng.cfg.snapshotPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.eng.cfg.snapshotPath(), []byte(pre), 0o644); err != nil {
		t.Fatal(err)
	}
	r.be.set("b.md", base.Add(20*time.Minute).Unix(), peerB)
	r.recordOwnWriteAt(t, "a.md", sha256hex([]byte(ours)), base.Add(10*time.Minute).UTC().Format(time.RFC3339))
	r.be.set("a.md", base.Add(10*time.Minute).Unix(), ours)
	// And an own update to b.md whose entry is dated BEFORE b.md settled the
	// peer's version — a remote board's clock ahead of this host's (the
	// lane's codex rounds 4 and 5): no clock is consulted, the ledger never
	// recorded b.md's settled content, so the entry stays live.
	oursB := "b, ours, the board's clock ahead"
	r.recordOwnWriteAt(t, "b.md", sha256hex([]byte(oursB)), base.Add(15*time.Minute).UTC().Format(time.RFC3339))
	r.be.set("b.md", base.Add(20*time.Minute).Unix()+1, oursB)

	r.tick() // loads, seeds
	snap, err := loadSnapshot(r.eng.cfg.snapshotPath())
	if err != nil || snap == nil || snap.LedgerConsumed == nil {
		t.Fatalf("the seed was not persisted: %+v (%v)", snap, err)
	}
	if len(snap.LedgerConsumed) != 0 {
		t.Fatalf("a still-pending own update was seeded away: %v", snap.LedgerConsumed)
	}
	r.advance(r.eng.cfg.debounce + time.Second)
	r.tick()
	if len(r.not.calls) != 0 {
		t.Fatalf("our own updates, debouncing when the snapshot was last persisted (one dated before it by a clock ahead), were notified after the upgrade: %v", r.not.calls)
	}
}
