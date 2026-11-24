# Round 2: old acts never become new turns

PR 106 remains a draft. This round starts at
`ebd7f446b875c85d84f0bef9194de2cc2c5cfcce`; its tests-first commit is
`509d8f56`. Implementation and fixture changes are in `ea20a6fc` and `95312270`;
the self-review fixes are in `713351ae`.
The implementation uses **Way B, Reading 1**, as dispatched: a per-space start
from the preceding completed inventory. A released ledger without that saved
inventory establishes it at first load. No global floor is added to that ledger.

The first successful subscription attempt supplies a fresh install's floor.
Every attempt updates it until the subscription is recorded. Each act uses its
own time; a recent edit, deletion or reaction on an old message still arrives.
The shared bound covers live delivery, redelivery, history, saved queues and
subsequent polling or replay. Unknown times use Pub/Sub publish time when
available, otherwise skip and increment `history_time_missing:<space>`.

A new room on a running adapter includes at most the preceding hourly sweep.
A DM opened during an outage includes messages since the inventory before the
outage. Existing cursors retain their outage. The released-ledger upgrade test
removes **three old queued rows**, delivers three saved-cursor outage messages
and one new DM, and delivers zero of 120 messages from 281–400 days ago.
The new `history_backlog_skipped:<space>` counter records the queue removal.

Skipped records contain only the existing envelope and attribution frame, no
old body or downloaded attachments. `Reason=local` is understood by released
readers; diagnostic `Cause=before_history_floor` records the bound decision.
`Cause=history_time_missing` records the unknown-time case. Pending, unresolved
and quarantine lists do not grow from these skips. Load-time migration also
retires saved attempts corresponding to removed queue rows, so polling cannot
post them after removal. Already committed deliveries are preserved.

## Red and green checks

All checks use fakes. No Google account, live adapter, customer state or real
Pub/Sub backlog was exercised. Each named test uses this command from
`gchat/adapter`, with the package and test name in the table:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./internal/PACKAGE \
  -run '^TEST$' -count=1 -p=1 -timeout=10m -json
```

The extra tests use `-timeout=5m`. Exact invocations and exit codes are in the
manifest JSON files; per-run results and assertion output are in `runs/`.
`results.json` counts terminal Go JSON events. Parent subtests count as subtests;
package results are separate. Red runs target the original PR production tree,
with later tests overlaid when necessary. The two pins below already passed.

`overlay.py BASE OUTPUT_DIRECTORY` reproduces those source overlays from the
repository root; the output directory must be outside the repository. Use
`--reader` for the released-reader exchange. Pass the resulting `overlay.json`
through `GOFLAGS=-overlay=...` when running the selected tests.

| Package | Test | Red passing/failing rows | Final green rows |
|---|---|---:|---:|
| app | TestNoOldActIsEverATurn | 30/30 | 60 |
| app | TestOldActOrdersAndNewActs | 5/4 | 9 |
| app | TestFloorFollowsTheFirstSuccessfulConnection | 0/5 | 5 |
| app | TestReleasedLedgerCursorlessSpacesStartAtLoad | 0/2 | 2 |
| app | TestReleasedCursorlessHistoryCannotReplayAfterHold | 0/1 | 1 |
| inbound | TestLiveActsBeforeTheFloorAreNotTurns | 2/6 | 8 |
| inbound | TestSkippedActReadsAsNotDeliveredAfterRollback | 0/5 | 5 |
| inbound | TestNewActsOnOldMessagesKeepTheirFrame | 1/3 | 4 |
| inbound | TestBoundedActWithoutTimeUsesPublishTime | 1/2 | 3 |
| inbound | TestHistoryBatchUsesEachResourceTime | 1/1 | 2 |
| inbound | TestMessageWithoutActTimeUsesPublishTime | 1/5 | 6 |
| inbound | TestNewReactionRemovalKeepsSkippedAttribution | 0/1 | 1 |
| inbound | TestOldActNeverDownloadsAttachments | 1/1 | 2 |
| inbound | TestHistoryRollbackExchange | 0/1 | 1 |
| inbound | TestRollbackSkippedCreateDoesNotWake (pin) | 1/0 | 1 |
| catchup | TestCursorlessSpaceStartsWhereItWasFirstSeen | 0/6 | 6 |
| catchup | TestNeverReadSpaceReportsNoLoss | 0/2 | 2 |
| catchup | TestSavedCursorStillDeliversItsOutage (pin) | 1/0 | 1 |
| catchup | TestCatchupMissingEnvelopeTime | 2/2 | 4 |
| catchup | TestHistoryFloorSurvivesRetentionFallback | 16/8 | 24 |
| catchup | TestBoundedQueueRetiresSavedDelivery | 0/15 | 15 |
| ledger | TestHistoryFloorLedgerCompatibility | 0/1 | 1 |

The 60-row class matrix below is the executed `TestNoOldActIsEverATurn` run.
Each cell is **single/batch**, and each letter identifies a path: L live,
R redelivered, C catch-up, Q saved queue, O operator replay, P later poll.

| Act | L | R | C | Q | O | P |
|---|---|---|---|---|---|---|
| message.created | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass |
| message.updated | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass |
| message.deleted | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass |
| reaction.created | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass |
| reaction.deleted | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass | pass/pass |

The four old-act order cases previously produced six unwanted turns. The five
post-bound act cases preserve new activity on old messages. Retention-fallback
rows cover single and batch edits, deletions and reactions with floor, join-only
and combined bounds. The join-only rows answer the earlier read's finding 13.

The retry-floor cases cover 503, 403, a completed failed create operation,
reload and a 24-hour window. The first red create fixture incorrectly used an
ambiguous 503 and timed out; `red2-TestFloorFollowsTheFirstSuccessfulConnection`
is the corrected red run. `queue-intent-base-red` is a compile failure from
referencing new fields directly; `queue-intent-base-red2` uses JSON to remain
compatible with the base and fails all 15 behavioral rows.

## Released-reader exchange

The current writer saved five skipped-act records. The exact production tree at
`4d14c7c5d52a468a2af7e387175ff4fb20366ec5` then loaded that file, in a
mentions-only room, and processed an unmentioned edit and deletion for every
retained message. It produced **zero turns after every operation**.
Both isolated commands passed; see `rollback-manifest.json` and
`runs/rollback-write.txt`, `runs/rollback-read.txt`.

The reader overlay keeps only `TestHistoryRollbackExchange`, restores all
existing Go source and tests from the released commit, and replaces newly added
Go files with an empty package declaration. Set `GCHAT_TEST_EXCHANGE_MODE` to
`write` then `read`, use one `GCHAT_TEST_EXCHANGE_FILE`, and set `GOFLAGS` to
`-overlay=<reader-overlay.json>` only for the reader. The default test mode
performs the exchange in-process and adds no optional suite skip.

The compatibility test uses the frozen released space schema, verifies omission
of zero-valued fields, and confirms rollback drops only the new floor, inventory
and per-space-start fields while preserving cursors and skip records.

## Recovery deadline

The baseline 30 isolated runs all passed. **No natural failure was captured.**
Its longest final ready wait was **9.051166458 seconds**, near the original
10-second limit. A test-only overlay then delayed every ledger save by 200 ms,
enabled only before the final restart. The original test failed after
**10.008125334 seconds**, with **15/24 turns** and 42 delayed saves observed.
With the revised wait, the same injection passed after **20.582409458 seconds**,
with all 24 turns. The dispatch ruling accepted this controlled reproduction as
evidence for the disk-save deadline cause. No slow-save hook is in production.

The new test waits for the exact 24 turns and empty backlog, then checks health,
uniqueness and restart behavior. The saved 18-row queue at a five-row pass budget
has a bounded **86-second ceiling**: four saves per row at one second per save,
four one-second pass delays, and ten seconds for startup and health. This is a
test deadline derived from queued work, with no retry or weakened assertion.

The first post-fix isolated run passed **30/30**, longest final recovery wait
**8.184807041 seconds**. Per-run timing is retained in `runs/e2e-before.txt`,
`runs/e2e-after.txt`, `runs/slow-before.txt` and `runs/slow-after.txt`.

A later 30-run check passed 30/30 with a longest wait of **9.100470500 seconds**.
After all review fixes, the final `713351ae` scenario passed **30/30**, longest
wait **13.134079208 seconds**, while the full suite was running other packages.
That final natural recovery exceeded the former ten-second ceiling and still
met all delivery, backlog and health assertions. This does not change the
baseline statement: no original-test natural failure was captured. Every
individual timing is in `timings.json`, with the corresponding run record.

```sh
env -u GC_BIN -u NEVER_SILENT go test ./test/e2e \
  -run '^TestEndToEnd$/^a_live_event_arriving' -count=30 -p=1 -timeout=30m -json
```

## Validation and review

Go: `go1.27.3 darwin/arm64`. Python: `3.13.13`, using Makefile's uv fallback
because system Python has no pytest. `make test PYTEST_DIRS=gchat/tests` passed
**49 tests and 89 subtests**. Formatting produced no output and `go vet ./...`
passed. Staticcheck was not installed, so it was not run; no shell scripts changed.

The first full Go run exited 1: **19 packages passed, two failed; 683 top-level
tests passed, six failed, two skipped; 1,307 subtests passed, 16 failed**.
The failures were `TestCatchupCadence`, `TestSubscriptionRefusalRetriesWithoutSignIn`,
`TestLookupHoldCarriesCredentialKind`, `TestLookupHoldPreservesTokenError`,
`TestLiveReactionThenCatchUpDoesNotRecordOrPostAgain` and
`TestHeadIsTheLiveOrderTestNotTheCauseWord`. The five fixture cases were corrected
and passed in focused checks. The subscription case hit its five-second ready
deadline in the full run and an isolated repeat; three original-head repetitions
passed. Its timeout and production behavior have not been changed. The full
run's end-to-end package passed, including the 3,000-message scenario.
The two skips are the optional `TestRealGCContract` and `TestRealGCReplyReminder`;
`GC_BIN` was unset as required.

The second full run exited 0: **21 packages, 690 top-level tests and 1,338
subtests passed; two optional live-gc tests skipped**. This intermediate run
overlapped assembly of the review corrections and is not cited as evidence of
the final commit.

The third full run, against frozen source
`713351ae9ea6e768788eab7e2e66a984b80a0dd5`, exited 0: **21 packages, 693
top-level tests and 1,358 subtests passed; two optional live-gc tests skipped**.
There were no failures. See `runs/full-go-3.txt` and `results.json`.

All **25 named regression tests** were rerun individually on the final
`713351ae` source: **25 top-level tests and 177 subtests passed**. Exact commands
and exit codes are in `final-named-manifest.json`; each `final-named-` run file
contains its results. This includes the full 60-row act matrix and every
self-review regression.

Full-suite invocation, from `gchat/adapter`:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
```

Existing history fixtures now state their preceding inventory. Direct reaction
fixtures supply occurrence times, preserving lookup-failure and dedup assertions.
The cadence fixture also supplies its prior inventory so it still exercises a
held historical interval. These fixtures no longer accidentally test an empty
first-start interval or an unknown-time skip.

One early upgrade run reported one notice and one unresolved record. The next
five and thirty isolated runs passed. The final test waits for the room's saved
recovery cursor before shutdown, not merely the fake endpoint's receipt of three
posts. The original failure remains in `green-extra-` evidence; the final named
`green-TestReleasedCursorlessHistoryCannotReplayAfterHold` run passed.

Round 1's Codex gate blocked two major classes: old live acts bypassing the
floor, and subscription retries leaving the floor behind. The second model
reproduced both, plus two minors: false history-loss reporting before a space
was read and skipped records misread as delivered after rollback. This round
answers all four and the cursorless-space case. The mayor's round-2 gate,
second-model read and requester confirmation remain pending. The first-start
shape remains the mayor's interpretation and requires requester confirmation.

The one required DEEP self-review examined only the delta from `ebd7f446` to
`ea20a6fc05fc3af89b5971b0cf243a564396321d`. Its recorded verdict is **BLOCK**,
with **three MAJOR findings**, all reproduced and fixed in `713351ae`:

| Finding class | Reproduction at reviewed head | Fix and green result |
|---|---|---|
| Incomplete migration of durable attempts | `TestCursorlessMigrationRetiresDetachedAttempts`: all 15 rows fail; old intent rows post a turn | Reconcile pre-bound intent, pending and unresolved records even when their queue row is gone; retain frame and actor only. All 15 rows pass. |
| Matching update IDs through mutable message state | `TestCursorlessMigrationRetiresEveryUpdateRevision`: all three rows fail; the second update survives | Match the exact stored revision ID first, then a pre-bound base record. All three rows pass, including a later new update producing one turn. |
| Changing a persisted retention identity | `TestRetainedHistoryKeepsReleasedEventIdentity`: single and batch rows fail | Keep envelope time in the durable key independently of act-time ordering. Both rows pass; both also passed at the original PR head, making this a compatibility pin. |

See `self-review.txt`, `review-final-manifest.json` and the `review-red2-`,
`review-green2-` and `review-base-` run records. The first retention-key batch
fixture omitted its per-message wrapper and reached quarantine; the corrected
fixture also asserts zero unresolved records. Only the corrected red run is
used above. The original-head runs fail the first two new regressions and pass
the retention identity pin. No second formal self-review was run and the
recorded BLOCK verdict was not relabeled. The review ran read-only without
tests; this round's evidence was assembled after its snapshot.

`operator-strings.diff` contains the complete old/new documentation and
production changes, including every added or changed operator string. The new
warning is `inbound act has no usable history time`; both counters are listed
above. `timing-overlays.diff` records the test-only timing instrumentation and
slow-save injection used for the controlled recovery experiment.

Public-repo check: done, 5 things changed. Five local path occurrences were
removed from the public review record. Added source, commit messages, evidence
and PR body were checked; the evidence contains only fake resources.
