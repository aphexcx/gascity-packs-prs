# PR 107 round 5

PR 106 was squash-merged. PR 107 now targets `main`. The reviewed tree was replayed once, with the original 46 non-merge commits still reachable at the reviewed head.

| Revision | SHA |
| --- | --- |
| Reviewed child | `24d9cb3d6d5b0a6423a43afdaff6e3c770264c17` |
| Old parent | `48c4097b1e48660b3c1373572e71bd7bce44d065` |
| Parent at squash | `471aebb850bbe14963ad085e68e0140dee62079c` |
| PR 106 squash | `4d6911d880b9d11851843ab5ca7ec1face56bcfc` |
| Fetched main (PR 134 squash) | `ebc58be18c1665509d7fac4c5a5b2149ff336ee2` |
| Same-tree commit ONE | `1b91ef8fe73ecbe0eec93cb633e8ecd44179b78a` |
| Replay R | `c03724ad68fd98d38c2e7e8bdaf89011fb7e5b2a` |
| Build and parity fixture F | `6028518176656eabb7c66d42f5e3203ee8970604` |
| Checkpoint-order test | `6332c57ced84f6e6deea949e7aa8a434d1757d51` |
| Recovery and retirement companion edits | `92960a03d1326338e2f8397e4a856f7e8455cb6e` |
| Final code and tests | `5c848b89e6e3631598d9cd0e0a748957b658dbcd` |

The final commit adds only this evidence directory. Its full SHA can be read with `git log -1 --format=%H -- assets/pr-evidence/gp-49y9/round5/README.md`. The handoff records the literal SHA after committing this file.

## Preconditions and replay

PR 107 was draft on main at the reviewed child. Its prior stack declaration was absent. Main contains PR 106's squash, PR 113's merged checkpoint change (`aeb63e574e68198a69a2b28d3abfe0a704130819`), and PR 134's delivery fix. The latter puts `--notify` on the delivery-notice mail command. The old parent is an ancestor of the parent at squash. Both counts to the child were 46.

ONE and the reviewed child each have tree `cd8d99dcf61bea04d67d1f657afdf0e1dcdb4b34` and stable patch ID `d8b2b4f732f238afa2b3330b2e886bba9240eec0` against the old parent. The diff3 trial and real replay both reported **12 hunks in seven files**. [Replay transcript and original hunks](rebase.txt).

| Hunk (line in the conflicted file) | Resolution and coverage |
| --- | --- |
| CHANGELOG.md:5 | Child entries precede main's entries, preserving chronological grouping. Documentation, inspected against both originals. |
| config/config.go:108, :457 | Both fields and both validation blocks, main first. `TestJoinReachbackValidation`, `TestCoalesceWindowSettingBounds`. |
| server/server.go:1775 | Main finishes its key-presence read and releases the lock before the child's configuration accessor. `TestStatusJevKeyIsOnlyPresence`, `TestCoalescingStatus`. |
| README.md:315 | Main's attachment paragraphs first, then the window paragraph directly above the first-start paragraph it references. Documentation, inspected against both originals. |
| inbound/inbound.go:1576 | **CHECKPOINT FIRST.** An accepted edit ahead of message state holds the member even when its saved member revision matches the message. `TestEditCleanupCheckpointRejectsOlderRevisions`, `TestRetryNeverSendsWithdrawnText`, and the new three-row order test. |
| app/workers.go:144 (R14) | The independent delivery notice ticker starts before the window worker. Both run. |
| inbound/history.go:108 (R15) | Match the original kind and judge the owner's notice first. Kept owners retain their members. Restore members only when removing their owner. |
| inbound/history.go:210 (R16) | Select all retiring rows before mutation. In the second pass recover the original kind, reverse deletion, then test notice preservation. Main's shared redaction helper reads the original event. |
| inbound/history.go:261 (R17) | Main's empty side. R16 already reverses deletion before the early exit, once. |
| inbound/inbound.go:196 (R18) | Initialize unanswered-attempt memory and the window in one constructor. Recover held acts under the ledger lock before returning. |
| inbound/inbound.go:1311 (R19) | Held acts return. Refresh yields to an unanswered attempt, which ends unknown before supersession. The notice keeps its original delivery ID. A definite refusal can still refresh. |

Hunk 4, resolved:

```go
	status["jev_api_key"] = "not set"
	s.mu.RLock()
	if s.cfg.Acknowledgement.Key != "" {
		status["jev_api_key"] = "set"
	}
	s.mu.RUnlock()
	cfg := s.configuration()
	status["coalesce_window"], status["held_acts"] = cfg.CoalesceWindow().String(), held
```

Hunk 6, resolved:

```go
	if checkpoint := l.Media[c.Message]; checkpoint != nil && checkpoint.Revision.After(state.Revision) {
		return true
	}
	if !c.MemberRevision.IsZero() && !state.Revision.After(c.MemberRevision) {
		return false
	}
```

The companion edits use the member's original event when redacting, discard the redundant member payload after redaction, and prevent refresh from rewriting an unanswered attempt. Only the two unsaved rows of `TestOwnerChangeKeepsDeliveryState` changed their expectations. The seven new tests are in `internal/inbound/coalesce_unanswered_test.go` and `internal/app/window_worker_test.go`. Named cases from each side and their results are in [verification](verification.md).

## Build boundary and patch identity

At R, vet diagnosed missing arguments at `internal/inbound/coalesce.go:24`, `coalesce_review_test.go:26`, and `coalesce_history_test.go:66`. After those were corrected, vet exposed the comparison at `internal/app/sender_credential_test.go:219`. F passes nil for both catch-up arrival maps, passes the fixture configuration to history retirement, uses deep equality for a struct containing a slice, and sets the parity fixture's coalescing window to zero. The two off-parity tests failed before the fixture change and passed afterward. [Red](red.txt) and [green](green.txt) records also cover the checkpoint-order overlay.

[All 108 per-file stable patch IDs](patch-ids.tsv) compare the old-parent-to-child and main-to-F diffs with `-U0`. The same 103 files give `7b9ebf78c27f07d3ef738ce0f027abf08dfada2d` on both sides. The five differences are `inbound/coalesce.go`, `inbound/coalesce_history_test.go`, `inbound/coalesce_review_test.go`, `inbound/history.go`, and `inbound/inbound.go`. The two main-only fixture files in F are outside the original 108.

Git's three-way merge tree `8c26d214ee5878df5632940823ff4bc96aaddc45` differs from R only in the seven conflicted files above. R to F changes exactly the three inbound call sites, `app/sender_credential_test.go`, and `inbound/offparity_test.go`. The child-to-F and old-parent-to-main diffstat lists have 837 and 834 entries. The first contains three additional files, the three inbound call sites. All remaining paths match. Reproduce the full lists with `git diff --stat 24d9cb3 60285181` and `git diff --stat 48c4097b ebc58be1`.

## Review and follow-up

Main's arrival observation is passed to the live handler before windowing. The catch-up entry supplies nil. Main's sender fill still precedes speaker selection for messages and reactions. First-read history still applies the space bound before the catch-up drain coalesces eligible rows. The Q6 owner-only re-post result calls remain at `inbound/inbound.go:1410` and `:1513`. Q8's retirement between a window flush's snapshot and save can panic. Its probe log is `q8-probe-flush-meets-live-retirement.txt`, and its fix is assigned to follow-up work. With a kept owner, a member at or after the bound may remain attached after a definite refusal.

The round-5 DEEP self-review covered the one-commit range-diff, the resolved source, F, the new tests, the patch table, and the PR text. In-scope findings at the reviewed code head: 0 BLOCKER, 0 MAJOR, 0 MINOR, 0 NIT. CLEAN. The six additional resolutions, three companion edits, two revised expectations, and seven ratified tests match the ruling text. R17 is empty because R16 contains the deletion reversal. The status accessor follows unlock, and the swapped checkpoint overlay produces the expected single failure. The first draft of that test exposed an uninitialized media map in its own fixture; the committed fixture initializes it and passes all three rows.

Two items were explicitly deferred. Q6's owner-only result calls are visible in source; a lost member notification is UNSHOWN. Q8's flush/retirement panic is SHOWN in the separate probe record and is assigned to follow-up work. Neither was changed in this replay. The public transcript replaces three historical ticket references with `<ticket>`; the code and the original diff3 text in private evidence retain their original bytes.

NEW HEAD: fresh gate
