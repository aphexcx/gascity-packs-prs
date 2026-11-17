# Saved attachment cleanup evidence

This change deletes files on a box when the adapter learns that their message
was deleted, or accepts an edit that took their attachment away. Expiry by age
is not included. A merge changes no box.

Base: `68884bf15089e79230bc88c55b591033e3fe927f`.
Tests first: `578f0ace` (all new named tests, before implementation).
Initial implementation tested: `d36ad43181196c224e29bea7fe6c410cacc71ac5`.
Review regressions first: `550ef677`, all four red before their fixes.
Round 1 fixes: `4507ae6a36eddbe4bbe9f8c2fa3dcfd97e91c888`.
Round 2 regressions first: `b99a9ba1`, both red before their fixes; its two
test seams only extract existing code without changing behavior.
Tests use temporary directories and in-process fakes. No Google service or
running adapter was contacted. Existing assertions were not weakened.

## Named tests: baseline and implementation

Each row was run alone from `gchat/adapter` with:

```text
env -u GC_BIN -u NEVER_SILENT go test ./internal/PACKAGE -run ^NAME$ -count=1 -timeout=2m -json
```

`PACKAGE` and `NAME` are the exact table values. Each invocation ran one
top-level test; the subtest counts below were identical at the base and the
implementation. No named test skipped. A pin passes at the base and protects
existing behavior; red means a failing behavioral assertion, not a build error.

| Package | Name | Base | Implementation | Subtests |
| --- | --- | --- | --- | ---: |
| inbound | TestRemovalStaysInsideTheMessageFolder | red | pass | 7 |
| inbound | TestRemovalRefusesUnsafeIds | pin | pass | 14 |
| inbound | TestDeletedMessageRemovesItsSavedFiles | red | pass | 0 |
| inbound | TestDeletionWithNoSavedFilesChangesNothing | pin | pass | 4 |
| inbound | TestDeletionRemovesEveryCopy | red | pass | 0 |
| inbound | TestDeletionLearnedAfterAnOutageRemovesFiles | red | pass | 0 |
| inbound | TestRemovalLogHoldsIdsOnly | red | pass | 2 |
| inbound | TestEditKeepsFilesUnlessTheAttachmentWasRemoved | red | pass | 7 |
| inbound | TestFailedRemovalIsLoggedAndRetried | red | pass | 0 |
| inbound | TestFailedRemovalDoesNotStopOtherMessages | pin | pass | 0 |
| inbound | TestDeleteAfterRestartRemovesSavedFiles | red | pass | 0 |
| inbound | TestStopBetweenTombstoneAndRemovalIsFinished | red | pass | 0 |
| inbound | TestStartLeavesFilesOfMessagesDeletedBefore | pin | pass | 0 |
| server | TestHistoryReadSavesAreRemovedToo | red | pass | 4 |
| server | TestSaveRacingADeletionLeavesNoFile | red | pass | 0 |
| ledger | TestMediaCleanupLedgerCompatibility | red | pass | 0 |

Totals: 16 top-level tests, 38 subtests, zero skips. Twelve baseline invocations
failed; all 16 implementation invocations passed. Within mixed tests, link
refusal and text-only/stale edits also pin behavior already present. The
unsafe-ID test additionally exercises recovery with a malformed persisted
obligation, bypassing event validation.

## Verification

The initial suite results below precede the review fixes. Final source
verification will be recorded before publication. After round 1, all 20
then-current named tests passed individually: 20 top-level tests, 46 subtests,
zero skips and zero failures. The round 2 corrections add two named tests
and four subtests; both passed individually after their fixes.

The four review regressions use the same individual invocation above. Each
failed behaviorally at the reviewed commit and passes after correction:

| Package | Name | Before fix | After fix | Subtests |
| --- | --- | --- | --- | ---: |
| inbound | TestCleanupDebtDoesNotDeleteReusedFiles | red | pass | 2 |
| inbound | TestEditCleanupCheckpointRejectsOlderRevisions | red | pass | 0 |
| inbound | TestCleanupFollowsSavedMediaRoots | red | pass | 3 |
| app | TestStartupCleanupDoesNotWaitForRemoteServices | red | pass | 3 |
| media | TestRemovalKeepsDebtUntilDirectoryChangesAreDurable | red | pass | 4 |
| app | TestCleanupAllowsReloadToCancelDownloads | red | pass | 0 |

For the durability regression, directory-sync failures are injected through
the removal helper's local dependency. Delete, edit, absent-entry retry and
parent-directory retry each retained debt only after the correction. The
reload regression holds a real fake-HTTP download and proves that the health
transition can reach generation cancellation while cleanup waits for it.

An intermediate full run was stopped for the round 2 production corrections.
Before stopping, unchanged `TestRunQuarantineRecoversDuringRun` hit its deadline
in all three rows. The individual test (one top-level, three subtests) then
passed, as did the whole puller package on both current and dispatched-base
source. No puller code or assertion changed. Final verification supersedes
this interrupted run; its result is not counted as a completed suite.

Go: `go1.27.3 darwin/arm64`. Python for pytest: `3.13.13` from an existing
environment whose pytest, PyYAML and jsonschema imports were verified. No
packages were installed. `staticcheck` was absent and was not installed.

- `gofmt -l .`: exit 0, empty output, from `gchat/adapter`.
- `env -u GC_BIN -u NEVER_SILENT go vet ./...`: exit 0, from `gchat/adapter`.
- `env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json`:
  exit 0; 22 packages passed, 702 top-level tests, 1,257 subtests, two test
  skips, zero failures. Run from `gchat/adapter`.
- `env -u GC_BIN -u NEVER_SILENT go test -race ./... -count=1 -timeout=45m -json`:
  exit 1; 22 packages completed (21 passed, `internal/app` failed), 702
  top-level tests, 1,257 subtests, two test skips. The only failures were
  the baseline credential-test race and the known shutdown timing case
  described below. All 16 new tests and their 38 subtests passed in this
  run, including the held-download deletion race. The 3,000-message
  recovery scenario passed. Run from `gchat/adapter`.
- `make test PYTEST_DIRS='tests gchat/tests'`: 463 passed, 127 skipped,
  250 subtests passed. Both adapter environment flags were unset.
- `gc lint gchat --json`: passed, zero errors. The README's `packs/gchat`
  example resolves here to the enumerated pack directory `gchat`.
- `bash -n` on each of the 16 pack shell scripts, and `shellcheck` on all 16:
  every command exited 0.

The first two scoped Python runs each had 462 passes and one failure in the
existing Makefile-defaults test: command-line make overrides were inherited
by its nested `make -n`. The test passed alone. The final scoped make run used
the verified interpreter on PATH through a temporary wrapper that unsets
`MAKEFLAGS`, `MFLAGS`, `MAKELEVEL`, `MAKEOVERRIDES`, `PYTEST_DIRS` and `PYTHON`
before starting Python. That leaves nested make probes at their defaults;
the same complete suite then passed, with no test or repository change.

The full race run reported a pre-existing test-owned slice race in
`TestManagedSourceChatRefusalRetriesWithoutSignIn`: the notification callback
increments `notices` before appending to `kinds`, while the test reads
`kinds[0]` after observing the counter. The implicated files,
`internal/app/keyholder_test.go` and `internal/app/operations.go`, are unchanged.
The isolated command below failed with the same two race reports both at the
implementation and with all changed production Go files overlaid from the
dispatched base. Added production files were excluded by build tags in that
overlay; no second worktree was created and no checkout file was replaced.

```text
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/app -run ^TestManagedSourceChatRefusalRetriesWithoutSignIn$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test -overlay "$EVIDENCE/baseline-overlay.json" -race ./internal/app -run ^TestManagedSourceChatRefusalRetriesWithoutSignIn$ -count=1 -timeout=2m -json
```

Each isolated run executed one top-level test, no subtests, no skips, and
exited 1. This test was not changed. The read is at
`gchat/adapter/internal/app/keyholder_test.go:180`, and the write is at line
176. The handoff ruling permits the draft with this baseline failure listed;
repair belongs to a separate change. A clean full race run was not shown.
The three normalized diagnostic records are
[current](current-keyholder-race.txt), [base](baseline-keyholder-race.txt),
and [full run](full-race-keyholder-extract.txt).

The documented load-sensitive
`TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` also failed
in the full race run (`HTTP body prevented bounded drain`, 3.22 seconds).
Its isolated retry passed: one top-level test, one subtest, zero skips,
exit 0. No code or assertion was changed for it. From `gchat/adapter`:

```text
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/app -run '^TestShutdownCheckpointsBeforeDrainingStalledRequestBody$/^bounded$' -count=1 -timeout=2m -json
```

The released 0.5.0 ledger source at
`d2f453b83a428546dfe7fa01dc2bd831c0b47ae0` was compiled in an isolated fixture
module with its released wire types. A checkpoint loaded and saved by the new
ledger was then loaded and saved by that released ledger (`go run .`, exit 0).
The final-source comparison proved that rollback drops only `media` and
`media_roots`; every field of the
released fixture stays equal. `TestLedger050LoadsAndSavesUnchanged` also guards
loading the released fixture with no new fields.

## Design and contract

1. Deletion removes every regular file directly inside exactly that message's
   folder, then the empty folder. Parent and sibling folders stay. This includes
   history saves, duplicate copies, unseen, own and withheld messages.
2. Removal independently checks both IDs as single segments and opens every
   directory component with `O_NOFOLLOW`. `unlinkat` is relative to the opened
   message directory. Links, subdirectories and other nonregular entries stay
   untouched and keep cleanup pending. Non-Unix removal reports `unsupported`.
3. One info line reports a positive removed count. A failed attempt adds one
   warning with the resource ID, remaining count and cause. No filename, local
   path, size, sender, content or raw filesystem error enters these logs.
4. The ledger maps each message's root-scoped filename digests to attachment
   resource IDs, and separately remembers configured media store roots.
   It stores no attachment filenames or paths below a store root. Both save callers
   reserve the link before downloading, covering a stop during a history save.
   This keeps the existing flat folder layout and old turn paths intact.
   Removed attachment IDs enqueue all linked copies; old unlinked files stay
   until message deletion. Attachment-ID stability across edits is UNPROVEN.
5. Cleanup is a separate durable obligation, not a failed delivery. Each local
   recovery pass makes one attempt per pending message; a refusal does not hold
   another turn behind it. Every failed attempt is visible in the adapter log.
6. Tombstone and deletion obligation are saved together. A service-owned pass
   runs immediately and every five seconds without waiting for credentials,
   registration, subscriptions or history recovery. Past tombstones without an
   obligation are not enrolled; past deletions stay untouched at an update.
   File unlinks are synced through the message directory, and removal of that
   directory through its parent, before debt can clear. A failed sync retains
   debt, including when a retry finds the message directory already absent.
   State-reset exclusion uses a separate mutex from health transitions, so
   credential/settings reload can still cancel a stalled download.
7. Cleanup, edits and downloads share the existing normalization gate. A
   deletion waits for a running download and removes it; later history saves
   recheck the durable tombstone. Accepted edit revisions also suppress saves
   of attachments removed by an equal or newer edit. A durable edit revision
   cannot be lowered after an interrupted turn checkpoint. Reusing a vacant
   filename clears its previous removal debt before the new download.
8. The deletion turn and provenance frame are unchanged; no sentence was added
   to the turn. Existing deletion tests were left unchanged.
9. Documentation names the deletion behavior, absence of expiry and the limits.
   No setting, command, subscription, version, deployment or release changed.

## Added operator and user text

Logs (values are resource IDs and counts):

```text
INFO saved attachments removed message=<resource> count=<number>
WARN saved attachment removal deferred message=<resource> remaining=<number> cause=<word>
```

Cause words: `unsafe`, `io`, `ledger`, `unsupported`. `remaining=-1` means the
directory could not be inspected. New attachment-unavailable reasons:
`message deleted`, `attachment removed`, `media state unavailable`.
The deletion turn gained no text.

Skill sentence:

> When a message is deleted, the adapter removes the files it saved for it, and a local path from that message stops working.

README text replacing the expiry statement and adding the cleanup paragraphs:

> Nothing is deleted to make room, and nothing expires by age, so a full store
> needs a decision, not a background sweep.
>
> When the adapter learns that a message was deleted, it removes the attachment
> files it saved for that message, including copies saved by history reads and
> edits. An accepted edit removes only the saved files of attachments it no
> longer carries. Files saved before this change have no attachment link: an
> edit leaves them, and a message deletion removes them. Attachment resource
> names are assumed to stay the same across edits (UNPROVEN). If Google changes
> a kept attachment's name, its earlier paths stop working and the edit saves
> a fresh copy. Text edits and history reads still save additional copies.
>
> Failed removals remain in the ledger and are retried at startup and every
> five seconds, independently of credentials, registration, subscriptions and
> history recovery, without holding later messages. Configured media roots are
> remembered across settings reloads and restarts, so a deletion also reaches
> copies in earlier stores. Attachment links use a digest scoped to their store;
> reusing a vacant filename cannot transfer an old removal to a kept attachment.
> An edit checkpoint keeps its revision across a crash, preventing older edits
> from restoring removed attachments. Directory removals are synchronized before
> their ledger obligation is cleared; synchronization failures remain retryable.
> Cleanup does not hold the health lock while waiting for downloads, so a
> credential or settings reload can cancel a stalled download. The adapter log
> reports the message id, removed count, and, on failure, a remaining count and
> one cause word; `remaining=-1` means the directory could not be inspected.
> Links and nested folders are left in place and reported for operator attention.
> The first start after an update leaves files of past deletions alone: only
> cleanup obligations recorded by the new code are retried. A rollback to the
> released adapter drops these obligations, attachment links and remembered
> media roots; other ledger
> state remains, and that adapter does not remove saved attachments.
>
> Only the adapter's saved files are removed. Ledger text, turns already sent
> to the mayor's session, and copies made elsewhere remain. Deletions in rooms
> the mayor has left, or during outages longer than Google's event retention,
> cannot be learned; files of a room the mayor leaves stay. Commands, settings,
> Google subscriptions and deletion turns are unchanged. A merge changes no
> box; adoption needs a separate adapter release and image or pack update.

Changelog entry:

> Remove saved attachments when a message deletion is learned, and files of
> attachments an accepted edit took away. Cleanup retries survive restart;
> past deletions are left alone at an update. Nothing expires by age or is
> removed to make room. Ledger text, delivered turns and other copies stay;
> commands, settings, subscriptions and deletion turns are unchanged. Deletions
> outside membership or event retention cannot be learned. A merge changes
> no box (gp-s7oh).

The media package comment now says the adapter removes saved files when a
message deletion is learned or an accepted edit takes an attachment away;
durable obligations retry after restart, past deletions alone do not enroll
files, nothing expires by age or is removed to make room, and ledger text,
delivered turns and copies elsewhere remain. Missed deletions outside
membership or event retention cannot trigger cleanup.

## Review and publication

Public-repository check: 18 absolute source paths in the three diagnostic
excerpts were normalized to repository-relative paths. The added source,
fixtures, evidence and PR text contain no private paths, real messaging IDs,
private names or unrelated city bead IDs. Test attachments are invented.

SAFETY self-review round 1 reviewed
`2dd8954229a3c35d87468280289b4b67c49b353c`: BLOCK, four MAJOR findings.
Each was reproduced before its fix, including all cases in its fault class:

1. Stale removal debt after pathname reuse: a failed download or externally
   removed file left a reservation that could delete a later kept attachment.
   Correction retires the former debt when a vacant name is reserved again.
2. Split edit checkpoints: a crash after the media checkpoint allowed an older
   event to lower the accepted revision. Correction keeps media revisions
   monotonic and prevents older events or unsent intents overtaking them.
3. Incomplete durable store identity: changing the configured media root left
   earlier copies behind. Correction remembers canonical configured roots
   before activating them and scopes file digests to the root.
4. Recovery coupled to remote readiness: registration, credentials or
   subscriptions could block local cleanup. Correction runs local recovery
   independently of those services, with immediate and periodic attempts.

All four round 1 findings are FIXED by
`4507ae6a36eddbe4bbe9f8c2fa3dcfd97e91c888`. Their original locations were
`internal/inbound/media_cleanup.go:76`, `:108`, `:158`, and
`internal/inbound/inbound.go:283` (paths relative to `gchat/adapter`).

SAFETY self-review round 2 reviewed
`4507ae6a36eddbe4bbe9f8c2fa3dcfd97e91c888`: BLOCK, two MAJOR findings.

1. Crash-consistency ordering (`internal/media/remove_unix.go:86`): unlinks
   could be lost in a host crash after the ledger on another filesystem had
   cleared the obligation. The fix syncs message and parent directories,
   preserves debt on sync failure, and retries parent sync when already absent.
2. Cancellation blocked by lock scope (`internal/app/media_cleanup.go:57`):
   cleanup held the health mutex while waiting for a download, preventing
   reload from reaching cancellation. The fix separates reset exclusion from
   health transitions, including the HTTP state-reset lock acquisition order.

Both were reproduced by the two round 2 tests above before correction.
The next review and final verification will be recorded before publication.
The separate gate and second-model read belong to the draft PR's handoff.
The worker does not merge, mark ready, release or deploy.
