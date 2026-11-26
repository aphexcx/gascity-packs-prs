# Saved attachment cleanup evidence

This change deletes files on a box when the adapter learns that their message
was deleted, or accepts an edit that took their attachment away. Expiry by age
is not included. A merge changes no box.

Dispatched base: `68884bf15089e79230bc88c55b591033e3fe927f`.
Integrated base: `226a0f34049fc5148a531530a063cdc063348235`.
Final source for full verification: `6e4318dd88ed3c87a12c6dcf072c040d5d2f2710`.
Tests first: `578f0ace` (all new named tests, before implementation).
Initial implementation tested: `d36ad43181196c224e29bea7fe6c410cacc71ac5`.
Review regressions first: `550ef677`, all four red before their fixes.
Round 1 fixes: `4507ae6a36eddbe4bbe9f8c2fa3dcfd97e91c888`.
Round 2 regressions first: `b99a9ba1`, both red before their fixes; its two
test seams only extract existing code without changing behavior.
Round 2 fixes: `d052cba2ee9590492f8d816c52dafedb220b26eb`.
Round 3 regression first: `87cdee7b`, red before its fix.
Mayor-authorized final-round regressions first: `b296341e`, both red at
`59ac97283a308ccbd66ae1aaa2f1cdd6e7b3d909` before correction.
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

Scoped-reviewed cleanup source: `e52a20ed85b39d97026818b09276cad17b989f8b`.
Current main was then integrated locally at `6e4318dd`; the two textual
conflicts retained both application state fields and both changelog entries.
No pull request was merged by the worker. Named checks, rollback and pack
verification were rerun on the integrated source, and full Go verification
was restarted there. This preserves all earlier review commits in history.
All 25 named tests pass individually: 25 top-level tests, 55 subtests, zero
skips and zero failures. Final pack verification also passes: 463 Python tests
and 250 subtests, 127 skips; pack lint reports zero errors; all 16 scripts pass
`bash -n` and shellcheck. Formatting and vet pass. The final non-race Go suite
passes all 22 packages: 730 top-level tests, 1,300 subtests, two skips and zero
failures. The final race run completed with exit 1: 22 packages (21 passed,
`internal/app` failed), 730 top-level tests, 1,300 subtests and two skips.
Its only failures were the two application deadlines described below; there
were zero data-race reports. All 25 named tests and 55 subtests passed in the
full race run, and the 3,000-message catch-up scenario completed. The initial
full-suite results below are retained separately from these final results.

Current main's Makefile adds a `reachable` Node prerequisite to `make test`.
The first integrated attempt used host Node 26.12.1 and failed two of 91
upstream Node tests (a CLI latency bound and an EAGAIN write), before Python
ran. The required Node 24.21.0 run passed: `pnpm exec node --version`, then
`pnpm exec make test PYTEST_DIRS='tests gchat/tests'`, from the repository root
with the city toolchain and verified Python wrapper first on PATH. All 91
Node tests passed without skips, followed by 463 Python passes, 250 subtest
passes and 127 skips. No Node or Python package was installed or code changed
for these execution failures.

The final-source full race run reported two application deadline failures:
`TestLivePullAndQuarantine` at `app_test.go:288` exceeded the status client's
one-second deadline; `TestNeverSilentGateLockLoss/no-credentials/missing` at
`never_silent_gate_test.go:110` missed its three-second needs-sign-in deadline.
Each exact test then passed alone under `-race` at integrated head:
one top-level test/no subtests for live pull, and one
top-level test/one subtest for lock loss, without skips or failures. These
retries do not establish behavior at main. No assertion or production code
was changed for them. The full head application-package retry also passes:
66 top-level tests, 32 subtests, no skips or failures.

The first attempted main comparison was invalid: a repository-relative Git
path was run from the Go module, producing an empty overlay. Those apparent
baseline runs actually tested head and are excluded from baseline evidence.
The corrected helper pins Git's working directory to the repository root and
asserts all 15 changed Go source/test replacements before starting. Both exact
tests passed on the corrected main overlay with the same individual counts.
The corrected full main application-package race run also passed: 64 top-level
tests, 29 subtests, no skips or failures. Its test inventory excludes the two
new application tests and their three subtests, confirming the comparison.
Neither deadline failure reproduced at main. Handoff remains subject to the
requested ruling on these two unreproduced failures; clean retries are not
presented as a clean original full race run.

From `gchat/adapter`, with both `GC_BIN` and `NEVER_SILENT` unset:

```sh
go test -race ./internal/app -run '^TestLivePullAndQuarantine$' -count=1 -timeout=15m -json
go test -race ./internal/app -run '^TestNeverSilentGateLockLoss$/^no-credentials$/^missing$' -count=1 -timeout=15m -json
go test -overlay "$EVIDENCE/main-corrected-source.json" -race ./internal/app -run '^TestLivePullAndQuarantine$' -count=1 -timeout=15m -json
go test -overlay "$EVIDENCE/main-corrected-source.json" -race ./internal/app -run '^TestNeverSilentGateLockLoss$/^no-credentials$/^missing$' -count=1 -timeout=15m -json
go test -race ./internal/app -run . -count=1 -timeout=15m -json
go test -overlay "$EVIDENCE/main-corrected-source.json" -race ./internal/app -run . -count=1 -timeout=15m -json
```

The main-source overlay restores every modified Go source and test file from
the integrated base and excludes added Go files with an ignore build tag.
This comparison creates no second worktree and changes no checked-out file.

The review regressions use the same individual invocation above. Each
failed behaviorally at the reviewed commit and passes after correction:

| Package | Name | Before fix | After fix | Subtests |
| --- | --- | --- | --- | ---: |
| inbound | TestCleanupDebtDoesNotDeleteReusedFiles | red | pass | 2 |
| inbound | TestEditCleanupCheckpointRejectsOlderRevisions | red | pass | 0 |
| inbound | TestCleanupFollowsSavedMediaRoots | red | pass | 3 |
| app | TestStartupCleanupDoesNotWaitForRemoteServices | red | pass | 3 |
| media | TestRemovalKeepsDebtUntilDirectoryChangesAreDurable | red | pass | 4 |
| app | TestCleanupAllowsReloadToCancelDownloads | red | pass | 0 |
| inbound | TestPendingRemovalSurvivesVacantNameReuse | red | pass | 0 |
| server | TestCatchupSnapshotReconcilesHistoryCopiesBeforeDedup | red | pass | 3 |
| server | TestDelayedEditPreservesNewerHistoryCopies | red | pass | 2 |

The final two regressions use real fake-server history reads. Both catch-up
rows fail before correction (unseen and already-recorded creation); the newer
history-copy row fails before correction, while its legacy-link row pins the
required behavior for links without source revisions. An additional red row
covers an older history response arriving after an edited catch-up snapshot
when no saved link existed at snapshot time. The existing stale-edit pin also
caught overly broad reconciliation of duplicate update events; retaining the
original update acceptance guards fixes that regression without changing its
assertion. The final production delta is 38 added and 14 removed lines across
three Go files, below the authorized approximately 150-line limit.

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
The next intermediate run was also stopped for a review correction. Its
unchanged `TestSubscriptionRefusalRetriesWithoutSignIn/404` row exceeded the
five-second ready deadline while catching up. This result is retained, not
counted as a completed suite or silently replaced by the final run.

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

The initial, pre-integration full race run reported a test-owned slice race in
`TestManagedSourceChatRefusalRetriesWithoutSignIn`: the notification callback
increments `notices` before appending to `kinds`, while the test reads
`kinds[0]` after observing the counter. The implicated files,
`internal/app/keyholder_test.go` and `internal/app/operations.go`, were unchanged
by the cleanup implementation at that stage.
The isolated command below failed with the same two race reports both at the
implementation and with all changed production Go files overlaid from the
dispatched base. Added production files were excluded by build tags in that
overlay; no second worktree was created and no checkout file was replaced.

```text
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/app -run ^TestManagedSourceChatRefusalRetriesWithoutSignIn$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test -overlay "$EVIDENCE/baseline-overlay.json" -race ./internal/app -run ^TestManagedSourceChatRefusalRetriesWithoutSignIn$ -count=1 -timeout=2m -json
```

Each isolated run executed one top-level test, no subtests, no skips, and
exited 1. The cleanup change did not repair this test. The historical read is at
`gchat/adapter/internal/app/keyholder_test.go:180`, and the write is at line
176. The original handoff ruling permits the draft with this baseline failure
listed and leaves its repair to another change. Those runs did not show a
clean full race suite. Upstream main `226a0f34` subsequently supplied the
repair, inherited by the integration; these files have no changes in this PR's
diff against that base. The final integrated results are reported separately.
The three normalized historical diagnostic records are
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
   Each link also records its source snapshot revision. An older edit cannot
   remove a newer history copy; an old link without that revision is treated
   as older than any edit. Catch-up snapshots reconcile saved copies before
   turn deduplication, including equal revisions and already-recorded creations.
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
   cannot be lowered after an interrupted turn checkpoint. A new download
   chooses another filename when a vacant name still has pending removal
   debt, preserving both ownership and crash recovery for the earlier copy.
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
> while a filename has pending removal debt, new downloads choose another name.
> Each copy's link records the snapshot revision it came from; a delayed edit
> cannot remove a copy from a newer snapshot. Links from an older ledger without
> that source revision are treated as older than any edit. Catch-up snapshots
> reconcile saved copies before delivery deduplication.
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
> media roots. Fields understood by that adapter remain, and it does not remove
> saved attachments.
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
   The first correction retired old debt when reusing a vacant name; round 3
   strengthens this by preserving pending debt and choosing another name.
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
Both round 2 findings are FIXED by
`d052cba2ee9590492f8d816c52dafedb220b26eb`.

SAFETY self-review round 3 reviewed
`d052cba2ee9590492f8d816c52dafedb220b26eb`: BLOCK, one MAJOR finding at
`internal/inbound/media_cleanup.go:95`. Crash-consistency ordering also
applies to filename reuse: a failed unlink sync leaves a vacant name whose
debt must not be cleared by the next download. The combined regression
`TestPendingRemovalSurvivesVacantNameReuse` reproduces this before correction,
then models a lost unlink across restart and checks that the old copy is
removed while the new copy survives. The fix preserves the original debt
and owner, choosing another filename for the new reservation. Its first
green attempt exposed a fixture-payload mismatch in the existing exact-content
helper; aligning the new fixture's bytes retained that assertion unchanged.

The concrete combined failure in the newly added sync-durability handling
justifies round 4 under the review policy; it is not a speculative finding.
The round 3 finding is FIXED by
`59ac97283a308ccbd66ae1aaa2f1cdd6e7b3d909`.

SAFETY self-review round 4 reviewed
`59ac97283a308ccbd66ae1aaa2f1cdd6e7b3d909`: BLOCK, two MAJOR findings.

1. Cleanup skipped by delivery deduplication
   (`internal/inbound/inbound.go:802`): a catch-up creation snapshot could
   record the latest revision without removing a history copy absent from
   that snapshot; the later equal-revision update was ignored.
2. Lost revision provenance (`internal/inbound/media_cleanup.go:143`): a
   delayed edit could remove a copy downloaded from a newer history snapshot.

The worker stopped and escalated. The 2026-11-17 ruling authorized one scoped
correction, after reproducing each case, and one review of only that round's
delta. Both cases were REAL and SHOWN by the two final regressions above.
The correction stores optional source revisions alongside existing links,
treats missing revisions as older than any edit, and reconciles snapshots
before delivery deduplication. No expiry or past-tombstone cleanup was added.
The additive ledger field remains readable by the released rollback writer.

Both round 4 findings are FIXED by
`e52a20ed85b39d97026818b09276cad17b989f8b`.

Authorized SAFETY self-review round 5 reviewed only the delta from
`59ac97283a308ccbd66ae1aaa2f1cdd6e7b3d909` to
`e52a20ed85b39d97026818b09276cad17b989f8b`: CLEAN, zero BLOCKER, MAJOR,
MINOR or NIT findings and no additional outside-delta findings. All nine
earlier findings have fixes and regression coverage. This scoped verdict
follows the explicit ruling; the mayor's full-diff gate and second-model read
remain pending.
The separate gate and second-model read belong to the draft PR's handoff.
The worker does not merge, mark ready, release or deploy.
