package main

import (
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
