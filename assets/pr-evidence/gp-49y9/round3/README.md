# Coalescing round 3

Round 3 preserves delivery safety when a frozen turn changes owner, invalidates withdrawn payloads in every stopped copy, and quotes body sender/header lines according to their display. A box that sets nothing changes behavior when its adapter updates: coalescing remains on at eight seconds by default.

Product commit: `53b5b8bc5dff5dd19b0650fa7e8674b1c1922a3a`. All endpoints are fakes. Toolchains are Go 1.27.3 (darwin/arm64) and Python 3.15.0. Every Go invocation unsets `GC_BIN` and `NEVER_SILENT`; [checks.json](checks.json) records each command, exit, count and failure. All 40 scheduled app runs are reported below, including every failure. The final 20 used documentation-fix commit `32228ffd052970c40472cfb080a64399b7dc63d7`; runtime code was unchanged from the product commit.

## Merge and baseline

Merge M is `be5a7add42870f7795f2197ab96f6f3dbfcafaf7`, joining round 2's `d33b2e6269ba9d5362200ebe0ab85b0f08bb5f9e` to approved parent `48c4097b1e48660b3c1373572e71bd7bce44d065`. Exactly three conflict hunks were resolved:

- `ValidateChange` applies `historyTime` only in bounded spaces, then calls the parent's one-argument `validate`.
- `RetireSpaceHistory` retains two-phase selection through `Delivery` and `OriginalKind`, and returns the parent's retired list.
- Each retirement retains the parent's counter increment and append, and tests `OriginalKind` for deletion. A merged turn of N acts counts N retired records.

The clean merges in the README and `never_silent.go` were inspected. Before any further checkout edit, the parent's recorded named-01 through named-25 and final-notice-race commands, its eight explicitly required tests, and round 2's 15 named regressions passed: 42 invocations, 51 top-level tests, 317 subtests, no skips or failures.

Then the app package ran alone 20 separate times at M with `env -u GC_BIN -u NEVER_SILENT go test ./internal/app -count=1 -p=1 -timeout=30m -json`. All 20 passed: 1,620 top-level tests, 2,840 subtests, no skips or failures. Total elapsed command time was 10,841.146 seconds. Each run is retained in [app-runs.json](app-runs.json). No wait, timeout or existing fixture was edited.

## Reproductions before fixes

All new named tests were committed separately before the product fix. These commits retain M's product sources, so their red runs measure the merged baseline rather than a different implementation.

| Test | Test commit | Red result |
| --- | --- | --- |
| `TestOwnerChangeKeepsDeliveryState` | `4424653f43848c20af9468a5cc7c46f93648ed17` | All 4 rows fail. Unsaved/live and unsaved/catch-up accept the survivor twice in 9 POSTs. Partial/live and partial/catch-up put it in 9 POSTs. |
| `TestOwnerChangeDeliveryClass` | `42f96a1e848560259a3249355f795e710cd5dffd` | 13 failing rows, 24 passing pins. |
| `TestWithdrawalReachesEveryStoredCopy` | `21dd72c29c9426663c76bcc0ef5ad3e8747aafec` | 33 merged/folded rows fail; the ordinary single-record pin passes. |
| `TestFromLineIsJudgedAsItDisplays` | `22e15211abb9feefd2339e622eda217816d020d6` | 756 failing leaf cases and 1,704 passing leaf pins, split equally across merged and retried-singleton forms. |

The logs named `red-*-calibration` above were valid reproductions on their first run; the fixtures reached their intended states. There was one separate reader-harness calibration failure: released 0.5.0 has no named `wire.Delivery` type. Commit `17627cf0599db42b9e460c7d4a91546fe794777d` uses the shared receipt shape. That build failure remains recorded separately from the subsequent actual reader failures.

## Delivery state follows the turn

Promotion snapshots the old owner's delivery state before retirement mutates it, and transfers `Attempts`, `Fragment`, `Reason`, `Cause`, `NextAttempt`, `Created` and `Posted` to the new owner. Both record-time and resume-time refresh use this mechanism. The existing interrupted-POST guard stops survivors of interrupted intent under their new owner; the operator can inspect and replay those sanitized survivors or accept them. A turn already unresolved when its text is withdrawn loses its payload and can only be accepted. A deleted old owner remains retired with an empty replay payload. Known refusal retries retain their schedule, and fragments retain the four-POST bound across every key.

[owner-rows.json](owner-rows.json) records each red/pin verdict. The four basic rows and 36 matrix rows log every fake POST and its acceptance, every record's final status/effective delivery status/reason/cause/attempt count/fragment/payload/membership, and mail count. The extra durable-backoff pin asserts the promoted fields but does not log every final record or mail count. The 36-row matrix covers six first outcomes, three withdrawals and both refresh paths. Its 12 failing matrix rows were owner deletion after unsaved delivery, partial delivery or a pre-POST crash (both paths), plus all six lost-answer withdrawal/path rows. The extra durable known-refusal backoff row also failed. The other 24 rows were pins.

Acceptance means a POST answered delivered, or received with its answer lost. A partial answer is a fragment, not an acceptance. The exact basic partial-then-failed sequence therefore expects zero accepted survivor POSTs; the unsaved-delivered sequence expects exactly one. Both require at most four total survivor POSTs.

By reading `ReconcileNeverSilent` and `unresolved`: a withdrawn retirement forgets its notice; ambiguous and fragmented unresolved delivery stops the notice without asserting definite non-delivery; a rejected POST or a clean known-refusal retry can stop it as definite. Committed delivered records reconcile as delivered; committed no-route records stop definitely. Cleanup preserves the prior unresolved reason and cause. The new row logs measure delivery-raise mails; these notice-engine branch descriptions are code readings, not new enabled-engine experiments.

## Every stored copy and every reader

The withdrawing save clears `Event` in both `Changes` and `Unresolved` for pending/unresolved merged or folded records carrying superseded members. It preserves `MemberEvent`, `MemberKind`, status, reason, cause and receipt. Cleanup precedes member refresh, and a recovery pass also cleans older frozen stopped copies before processing. Record shape controls this protection even with the setting now off. Ordinary singles and committed records are unchanged.

The writer exports 34 cases: 14 prior reasons in merged and folded forms, one pending receipt per form, merged-owner deletion, merged-member edit, folded edit, and the single pin. [withdrawal-rows.json](withdrawal-rows.json) records each case. [readers.json](readers.json) retains every harness run, including calibration; [red-readers-valid-rows.json](red-readers-valid-rows.json) and [green-readers-rows.json](green-readers-rows.json) retain per-reader observations.

At the baseline, the current reader failed 32 cases and passed merged-pending plus single; released 0.5.0 and main each failed all 33 merged/folded cases and passed single. After the fix, the writer and all three readers each pass all 34 cases. Every cleared unresolved record rejects replay with HTTP 400 `stored turn is malformed; cannot replay`, makes zero replay POSTs and can be accepted. Pending receipts are actually issued by each fake, then concluded failed. The current reader can deliver sanitized survivors; older readers stop without posting withdrawn text.

The unchanged round 2 export and reader harness also pass: 7 writer cases and 7 cases under each of current, released `d2f453b83a428546dfe7fa01dc2bd831c0b47ae0`, and main `205d0aecedf944792f1e3b3fbc420b9e5ad910d8`. No round 2 test or harness was edited.

## Display comparison

Only `QuoteMergedHeaders` and its Unicode import change: its comparison removes Unicode Cf, maps Zs and tab to ASCII space, then applies the existing trim/header/from-prefix checks. The delivered line retains its original bytes with `> ` prepended when required. `Render` and `IsFrame` are unchanged; the existing `Strip` behavior still applies.

The class test covers every Go Cf code point at marker positions 0 through 6, every Zs code point and tab before the marker and in place of its space, and both live/catch-up header forms with a joiner or nonbreaking space. Each runs in merged and refreshed-singleton forms: 2,460 leaf cases. [display-rows.json](display-rows.json) retains each red/pin and green case.

## Verification and review

The first fixed run passes all four new tests and 2,577 subtests with no skips. Each new test alone at `-count=20` also passes: 80 top-level tests and 51,540 subtests (80 basic-owner, 740 owner-class, 680 withdrawal and 50,040 display subtest passes). The focused race run passes all three owner/withdrawal tests and 75 subtests. The root Python suite passes 465 tests and 250 subtests, with 127 skips, using the environment-clearing Make invocation recorded in [checks.json](checks.json).

`gofmt -l .` emits nothing and `go vet ./...` passes. The full Go suite was run once this round: `go test ./... -p=1 -count=1 -timeout=30m -json` passed all 22 packages, 784 top-level tests and 4,179 subtests in 1,672.874 seconds. The two expected skips are `TestRealGCContract` and `TestRealGCReplyReminder`. Every run, including the reader-harness calibration failure, is retained in [checks.json](checks.json). Runtime code was tested at the product commit; subsequent pre-review edits only clarify the operator wording and record evidence.

The unchanged 14-act window-off script produces identical combined POST/ledger bytes at the product commit and approved parent. Both SHA-256 values are `b66f9b224a75fb8ebdaf9e7605c544b4e243e515af0fdbf448710ce7732853af`; [window-off.json](window-off.json) records both revisions and invocations.

The one fresh DEEP self-review returned **CLEAN** at `fa7dce6449fef75823d12e1cbaaaa8b998cd0d07`, with no major or critical finding. Its one MINOR wording finding was **FIXED** in `32228ffd052970c40472cfb080a64399b7dc63d7`: interrupted intent can leave replayable sanitized survivors; already-unresolved withdrawn payloads are accept-only. [The original review](self-review.md) and [finding outcome](self-review-results.json) preserve that distinction. The review's statement that final app runs were pending records its historical timing; the completed measurement follows. The mayor's round 3 DEEP gate and second-model read happen after handoff and remain pending. Round 2's gate and read were BLOCKED; the reproductions above cover their three blocking classes.

## All 40 isolated app runs

At M, all 20 runs passed. At `32228ffd052970c40472cfb080a64399b7dc63d7`, 10 of 20 passed and 10 failed: 1,610 top-level passes, 2,835 subtest passes, 0 skips, 11,529.899 seconds of command time. Failed tests are additional to the pass counts. Every run used `env -u GC_BIN -u NEVER_SILENT go test ./internal/app -count=1 -p=1 -timeout=30m -json`, sequentially with no concurrent build or review in this lane. No waits, timeouts or existing fixtures changed; there were no replacement reruns.

| Failing test (most specific) | Failed final runs |
| --- | ---: |
| `TestSubscriptionRefusalRetriesWithoutSignIn/404` | 1 |
| `TestFreshTokenIdentityDeliversFirstDM` | 1 |
| `TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` | 4 |
| `TestMissingTokenReplacementResumes` | 2 |
| `TestRevokedTokenReplacementResumes` | 1 |
| `TestManagedSourceLateIdentityReachesRunningClient` | 1 |

[Failure diagnostics and source-path analysis](app-failures.md) distinguish each assertion from its broader startup/shutdown path. The fixtures set `0s`, but running generations still launch `RunWindows`, which enters the inbound gate. These tests therefore include the broader PR path; their snapshots do not establish coalescing as the cause or isolate the delayed phase. [app-summary.json](app-summary.json) includes parent and leaf failure totals.

| Revision | Run | Exit | Top-level passes | Subtest passes | Skips | Seconds | Failed tests (leaf) |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| M | 01 | 0 | 81 | 142 | 0 | 498.697 | none |
| M | 02 | 0 | 81 | 142 | 0 | 585.182 | none |
| M | 03 | 0 | 81 | 142 | 0 | 560.352 | none |
| M | 04 | 0 | 81 | 142 | 0 | 550.457 | none |
| M | 05 | 0 | 81 | 142 | 0 | 520.411 | none |
| M | 06 | 0 | 81 | 142 | 0 | 534.050 | none |
| M | 07 | 0 | 81 | 142 | 0 | 566.386 | none |
| M | 08 | 0 | 81 | 142 | 0 | 570.065 | none |
| M | 09 | 0 | 81 | 142 | 0 | 593.291 | none |
| M | 10 | 0 | 81 | 142 | 0 | 578.062 | none |
| M | 11 | 0 | 81 | 142 | 0 | 603.500 | none |
| M | 12 | 0 | 81 | 142 | 0 | 529.259 | none |
| M | 13 | 0 | 81 | 142 | 0 | 527.533 | none |
| M | 14 | 0 | 81 | 142 | 0 | 513.530 | none |
| M | 15 | 0 | 81 | 142 | 0 | 495.580 | none |
| M | 16 | 0 | 81 | 142 | 0 | 492.834 | none |
| M | 17 | 0 | 81 | 142 | 0 | 492.229 | none |
| M | 18 | 0 | 81 | 142 | 0 | 582.526 | none |
| M | 19 | 0 | 81 | 142 | 0 | 540.264 | none |
| M | 20 | 0 | 81 | 142 | 0 | 506.938 | none |
| final | 01 | 1 | 80 | 141 | 0 | 532.968 | `TestSubscriptionRefusalRetriesWithoutSignIn/404` |
| final | 02 | 0 | 81 | 142 | 0 | 552.738 | none |
| final | 03 | 1 | 80 | 142 | 0 | 551.436 | `TestFreshTokenIdentityDeliversFirstDM` |
| final | 04 | 1 | 80 | 141 | 0 | 533.204 | `TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` |
| final | 05 | 1 | 80 | 141 | 0 | 546.392 | `TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` |
| final | 06 | 1 | 80 | 142 | 0 | 555.550 | `TestMissingTokenReplacementResumes` |
| final | 07 | 0 | 81 | 142 | 0 | 511.700 | none |
| final | 08 | 0 | 81 | 142 | 0 | 495.926 | none |
| final | 09 | 0 | 81 | 142 | 0 | 491.057 | none |
| final | 10 | 1 | 80 | 142 | 0 | 576.474 | `TestMissingTokenReplacementResumes` |
| final | 11 | 0 | 81 | 142 | 0 | 537.766 | none |
| final | 12 | 1 | 80 | 141 | 0 | 497.285 | `TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` |
| final | 13 | 1 | 80 | 141 | 0 | 577.719 | `TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded` |
| final | 14 | 1 | 80 | 142 | 0 | 627.084 | `TestRevokedTokenReplacementResumes` |
| final | 15 | 1 | 80 | 142 | 0 | 660.294 | `TestManagedSourceLateIdentityReachesRunningClient` |
| final | 16 | 0 | 81 | 142 | 0 | 653.987 | none |
| final | 17 | 0 | 81 | 142 | 0 | 676.236 | none |
| final | 18 | 0 | 81 | 142 | 0 | 621.649 | none |
| final | 19 | 0 | 81 | 142 | 0 | 610.309 | none |
| final | 20 | 0 | 81 | 142 | 0 | 720.125 | none |

## Operator strings

Every changed operator string, with exact old and new text, is retained in [operator-text.json](operator-text.json). The Google Chat skill was inspected and has no newly false sentence. The default, caps, window lengths, single-turn bytes, reply template, envelope and pack version are unchanged.

gchat/README.md, change 1:

Old:

```text
the key; when the owner was deleted, the newest survivor owns the replacement.
```

New:

```text
the key; when the owner was deleted, the newest survivor owns the replacement,
including the turn's attempt count, fragment marker and retry schedule.
Changing the owner never resets the fragment retry bound. An interrupted intent
with an unknown POST outcome becomes an unresolved surviving turn; inspect the
delivery before accepting it or replaying its sanitized payload. If a turn was
already unresolved when the withdrawal arrived, its frozen payload is cleared
and only acceptance is available. An owner change never makes an unknown
outcome automatically sendable.
The deleted owner's retired record has no replayable payload and can be accepted.
```

gchat/README.md, change 2:

Old:

```text
row itself. History reads still use each message's own text. With coalescing
enabled, a withdrawn record has no replayable payload: replay returns
`stored turn is malformed; cannot replay` (HTTP 400), leaves the original
withdrawal reason in status, and makes no POST, including after a rollback to
0.5.0. Use `--accept` to settle that record. A rollback delivers held messages individually and keeps
```

New:

```text
row itself. History reads still use each message's own text. Any pending or
already unresolved merged or folded copy containing withdrawn text loses its
replayable payload, even if it stopped for another reason or coalescing is now
off. Its prior reason and cause remain intact. Pending receipts are still
polled; a failed receipt may rebuild a replacement with current survivors.
Replaying an unresolved record whose payload was cleared returns
`stored turn is malformed; cannot replay` (HTTP 400), preserves its reason,
and makes no POST, including after a rollback to 0.5.0. Use `--accept` to settle
that record. A rollback delivers held messages individually and keeps
```

gchat/CHANGELOG.md, change 3:

Old:

```text
(no text)
```

New:

```text

- Owner changes preserve delivery uncertainty, fragment limits and retry state; withdrawals clear every pending or unresolved merged/folded payload, and merged body headers are quoted as they display through Unicode format characters and spaces.
```

## Ruling interpretations and limits

Source references moved with the merge and fixes: `inbound.go`'s `ValidateChange` is now line 626, record refresh line 1115, resume refresh line 1170, interrupted-POST guard line 1188, unavailable-answer branch line 1282, and `unresolved` line 1435. `coalesce_retry.go`'s refresh remains line 17 and its new stored-copy cleanup starts at line 140. In `provenance.go`, the import shifts `IsFrame` to line 125, `Render` to line 133 and `QuoteMergedHeaders` to line 156; the first two bodies are unchanged. `RetireSpaceHistory` now starts at `history.go:136`. The app launch at `workers.go:133`, `RunWindows` at `coalesce.go:362`, and notice reconciliation at `never_silent.go:13` retain their references.

1. Red at M means M's product sources with separately committed tests, before any product edit.
2. Inheritance was chosen: interrupted intent leaves a sanitized surviving owner unresolved with an explicit replay/accept choice. A previously unresolved turn whose text is withdrawn instead loses its payload and is accept-only. The deleted owner is non-replayable.
3. Pending and unresolved copies are protected; committed records are left alone. Pending receipts still conclude through the reader's processor.
4. Partial is not an acceptance; unknown delivery is never automatically repeated. The four-POST bound applies to fragments, not every known refusal.
5. `NextAttempt` and `Cause` follow promotion; the extra durable-backoff row demonstrates this, beyond a code reading.
6. The quoted retried singleton is the edited/refreshed survivor. An unchanged single turn retains its original bytes.
7. Ordinary singles retain the existing withdrawal check, pinned under all readers.
8. Header means the two existing adapter header forms, not gc's separate envelope renderer.
9. Zs/tab are tested before the marker and as its space; Cf is tested at every specified position.

No real gc or Google endpoint was exercised. Real Google unthreaded group-chat identity, a live held create meeting a catch-up edit, deployed gc image pins and inbound attachment limits remain unproven. Case changes, fullwidth brackets, lone-CR/U+2028/U+0085 line splitting and gc envelope changes are outside this round. App workers still start `RunWindows` in every generation and enter the inbound gate once even at `0s`; this is a code reading, not a claim that app timing failures are unrelated to the PR.
