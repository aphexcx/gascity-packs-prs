# PR 109 round 4: retarget onto main

Validation disclosure: the legacy Slack test-stub race reproduced in **1/20
head runs and 0/20 main runs**. There is no runtime production writer of the
stub URL. The existing legacy dispatch/read path is unchanged; a PR 109
change causing the scheduling difference is not shown. The ruling permits
delivery with this disclosed result. Two earlier flaky checks passed their
authorized 20-repetition and full-suite reruns. A fourth finding exposed an
early sample in PR 109's own test helper; the only authorized edit waits for
that callback to finish, with all assertions and expected values unchanged.
The complete measurements and limits are below. No production behavior was
changed by this round, and PR 109 remains a draft for the Mayor's gates.

## Commits and tree proof

- Reviewed PR head: `a5f8c61e352ff4dc01bdd5d1d90820500ef298f7`.
- Parent merge: `0e1dee7fd2f45f33fb22a08077eff30562a204b1`.
  Its first parent is the reviewed PR head; its second parent is
  `f2d2f92d1aa5762df35f98d94adccd503af75fa1`.
- Main ancestry merge:
  `de95f930f9ce74bce8e4539b57db1a2fafca7ec9`.
  Its first parent is the parent merge and its second parent is
  `920830934bf7d60e3ac15e5691e5f52d17db06c6`.
- Before the second merge, both main and the parent's final head resolved
  to tree `f7190c00a51f82eb44cf0b00edf1b2d9b45ca099`.
- The combined tree before and after the second merge was
  `ababeeb189e86b3bf55d55e30c71b6614c924add`.
  The tree diff was empty, and the main-ancestor check passed.
- Final code/test head: `300f28b3dbfe571c3aa315edfa1d2f95cdb8e148`,
  adding only the authorized nine-line test-helper wait.
- The commit containing this README adds only evidence to that final head.

The remote tips were freshly fetched and matched the specified commits
before either merge. No rebase or force push was used.

## Merge resolution and pair coverage

Conflicted files: **0**. Automatically merged files: **2**.
`git show --remerge-diff` on the parent merge has no resolution diff.

| Conflicted hunk | Parent intent | PR 109 intent | Resolution |
| --- | --- | --- | --- |
| None | No conflicting hunk | No conflicting hunk | No manual resolution |

These are the complete intersection of files changed by PR 109 and by the
parent's round 5. The named existing tests all passed in the merged-tree
Google Chat suite; they were not edited.

| Shared file | PR 109 behavior and tests | Main behavior and tests |
| --- | --- | --- |
| `gchat/adapter/internal/chat/chat.go` | Reaction hooks, successful removal, and repeated-provider-answer handling: `TestSessionFirstReactionCountsAsWithAckOff`, including removal and later-message notice rows; `TestHeldAnswerParityWithAckOff` | Retained HTTP client and membership reads: `TestJoinRechecksIdentityBeforeConsumingAndWriting`, `TestRoomFoundWithoutAnEventNeverReadsFromTheEpoch`, `TestMembershipReadThatMayPassFailsTheFreeze` |
| `gchat/adapter/internal/server/server.go` | The react verb preserves repeated-reaction accounting and waiting-call order: `TestSessionFirstReactionCountsAsWithAckOff`, `TestHeldAnswerParityWithAckOff` | Join route, readiness and identity boundaries: `TestJoinBeforeLedgerLoadPreservesExistingState`, `TestJoinRechecksIdentityBeforeConsumingAndWriting` |

The cross-feature guards also passed:
`TestFirstReadAcknowledgementBoundary` (catch-up-only and live-before-first-read),
`TestLegacyIdentityHoldResumesInterruptedOutbound`, and
`TestHoldReactionContextEndsWithItsCaller`.
They cover no acknowledgement for first-read/catch-up delivery, startup
identity hold recovery, and cancellation of a waiting reaction respectively.
No new tests were needed, so no new-test red/green mutation run was performed.

## Validation and the stop

Toolchain: Go `go1.27.3 darwin/arm64`. Commands ran from their Go module.
Counts below are top-level tests unless stated otherwise.

| Tree | Command | Result |
| --- | --- | --- |
| Combined tree, `gchat/adapter` | `env -u GC_BIN go test -json ./... -count=1 -timeout=30m` | Exit 1: 783 passed, 1 failed, 2 skipped. 24 packages passed, 1 failed, 1 had no tests. The 3,000-message end-to-end test passed. |
| Exact main snapshot, `gchat/adapter` | `env -u GC_BIN go test -json ./internal/app -run '^TestRevokedTokenReplacementResumes$' -count=10 -timeout=2m` | Exit 0: all 10 repetitions passed. |
| Exact main snapshot, `gchat/adapter` | `env -u GC_BIN go test -json ./... -count=1 -timeout=30m` | The same test passed in 3.52 seconds. The comparison was then deliberately stopped with SIGTERM, exit 143. This is not a complete passing baseline suite. |

The failure on the combined tree was:

```text
app_test.go:346: health did not become ready in 5s: {State:catching-up Reason:recovering retained history}
--- FAIL: TestRevokedTokenReplacementResumes (5.37s)
```

The exact-main snapshot was extracted from
`920830934bf7d60e3ac15e5691e5f52d17db06c6` within the assigned lane.
No additional Git worktree was created. The isolated comparison and the
matching test in the full-suite comparison both passed, so neither result
justifies treating the merged-tree failure as a known main failure.
See `validation.txt` for the trimmed outputs and command completion record.

The subsequent ruling allowed one measured retry protocol without source or
test changes. Its completed results were:

| Run at the combined tree | Exit | Result | Load before (1/5/15 minutes) | Load after |
| --- | ---: | --- | --- | --- |
| Initial full suite | 1 | 783 passed, 1 failed, 2 skipped | Not recorded | Not recorded |
| Same test alone, `-count=20 -timeout=30m` | 0 | 20/20 passed | 9.74 / 9.27 / 10.75 | 11.85 / 9.77 / 10.82 |
| Full suite, same original flags | 0 | 784 passed, 2 skipped; 25 packages passed, 1 had no tests | 11.43 / 10.02 / 10.86 | 7.46 / 7.80 / 9.20 |

Loads were measured with `uptime` immediately before and after each rerun.
The initial run's load is unavailable and was not reconstructed. The
deadline/load classification follows the ruling's explicit 20/20 plus green
full-suite condition. The PR body and handoff retain this disclosure.

### Slack log capture and measured rerun

The first full Slack run had 1,684 passed, 1 failed, and 3 skipped (exit 1).
`TestOffParityWithTheBase`, `slack-full/adapter/offparity_test.go:475`,
captured three extra coalescer retry/isolation lines and missed none of its
expected lines. Its other compared outputs did not fail. The initial load
average was not recorded.

Before the later helper edit,
`git diff --stat a5f8c61e352ff4dc01bdd5d1d90820500ef298f7 de95f930f9ce74bce8e4539b57db1a2fafca7ec9 -- slack-full/adapter`
was empty: **zero changed lines**. The retarget merge's adapter code and
tests were exactly PR 109's reviewed head. Neither PR 109 nor this round
edited the off-parity test. The final difference is only the nine-line
helper wait in another test file; production code remains identical.

The retry streams trace to the first coalescer in
`TestOwnedPayloadSurvivesACrashDuringUptime` (one restored entry) and the
`the members of a refused batch` fixture of
`TestOwnershipRecordFollowsItsPayload` (two restored entries and the resume
line), both in `inbound_rejection_ladder_test.go`. Their always-failing
deliverers and unclosed 20 ms coalescers continue retrying after their tests
return. The first-failure records and subsequent numbered retry streams line
up with the thirteenth failures inside the parity capture. Another two-entry
stream from `TestPausedIsolationIsDurable` fired just before the capture and
was not one of the three extra lines.

`captureLog` in `main_test.go:4600` redirects the process-wide standard
logger. These producer tests and the parity test do not call `t.Parallel`;
the overlap is their surviving asynchronous timer callbacks writing into a
later serial test's capture. No fixture, capture helper, or source was fixed
in this round. This isolation issue is for separate follow-up.

The complete-pack main baseline passed: 1,674 passed, 3 skipped, exit 0;
load before 6.27 / 7.72 / 8.64, after 4.70 / 5.99 / 7.65.
An earlier baseline snapshot omitted sibling fixtures; that invalid
comparison was stopped (143), discarded as a source comparison, and replaced
by the complete-pack run. It is not counted as a main source failure.

The merged tree passed 20/20 isolated off-parity repetitions, exit 0;
load before 6.58 / 6.23 / 7.55, after 4.60 / 5.78 / 7.32.
The full Slack rerun passed: 1,685 passed, 3 skipped, 5 packages passed,
exit 0; load before 12.38 / 7.63 / 7.91, after 5.75 / 6.30 / 7.23.
The 20/20 repetitions and green full suite satisfy the second ruling's
condition to continue with this disclosure. The later race check is reported
separately below.

The final full Slack run at `300f28b3` after the helper edit saw this same
capture failure again: 1,684 passed, 1 failed, 3 skipped, exit 1; exactly
the same three extra coalescer lines and no missing expected line. It is
the same previously classified test failure, not a fifth distinct failure.
It is recorded without another retry. The final full suite is not claimed
green.

Seventeen of PR 109's 18 test files remain byte-for-byte identical. The one
exception is `slack-full/adapter/session_first_reaction_counts_test.go`,
`firstEyes` only, as authorized after the fourth finding. All 21 test files
changed by the parent's round 5 remain byte-for-byte identical. No existing
main test was modified by the merged PR delta. `test-preservation.json`
records the exact file comparison.

PR 109's nine Google Chat test files all passed, with these counts:

| File relative to `gchat/adapter/internal/` | Passed top-level tests |
| --- | ---: |
| `ack/hold_context_test.go` | 1 |
| `ack/hold_test.go` | 1 |
| `ack/one_taker_test.go` | 1 |
| `ack/session_mark_test.go` | 1 |
| `app/held_answer_parity_test.go` | 2 |
| `app/session_first_reaction_counts_test.go` | 1 |
| `app/stamp_message_before_waiting_call_test.go` | 1 |
| `app/stopwatch_waited_reaction_test.go` | 1 |
| `app/waiter_gives_up_test.go` | 2 |
| Total | 11 |

Slack's corresponding nine files also passed all 11 top-level tests in its
normal full rerun: `internal/ack/hold_context_test.go` (1),
`internal/ack/hold_test.go` (1), `internal/ack/one_taker_test.go` (1),
`internal/ack/session_mark_test.go` (1), `held_answer_parity_test.go` (2),
`session_first_reaction_counts_test.go` (1),
`stamp_message_before_waiting_call_test.go` (1),
`stopwatch_waited_reaction_test.go` (1), and `waiter_gives_up_test.go` (2).

### Race checks and the authorized helper edit

Google Chat's race run passed 247 tests, exit 0, across `internal/ack`,
`internal/app`, `internal/arrival`, `internal/chat`, and `internal/server`.
Four packages passed and arrival had no tests. No race was reported.

The full Slack race run of `.` and `./internal/ack` exited 1: 1,659 passed,
2 failed, 3 skipped. It is **not** reported as a green race suite.

1. `TestArrivalCoversLegacyThreadRoutes/sticky`: cleanup in
   `withSlackAPIStub`, `thread_context_test.go:40`, writes `slackAPIBase`
   while the legacy reaction goroutine reads it in
   `postReactionMethodContext`, `main.go:3512`, spawned by
   `processSlackEvent` at `main.go:5164`.
   The same isolated `-race -run '^TestArrivalCoversLegacyThreadRoutes$'
   -count=20 -timeout=30m` command reported the race in 0/20 main runs
   (exit 0) and 1/20 head runs (19 passed, 1 failed; exit 1).
   Main loads: 9.50 / 6.85 / 6.81 before, 9.28 / 6.89 / 6.82 after.
   Head loads: 12.15 / 8.16 / 7.30 before, 12.77 / 8.35 / 7.37 after.
   All non-test references to `slackAPIBase` were checked: the only write
   is package initialization at `main.go:338`; all remaining references
   are reads, with no address-taking or runtime reassignment.
   The production diff changes `handleReact`, not the racing legacy
   dispatch/read path. The test already returns without joining that
   goroutine on main. The observed head-only sample therefore does not
   establish which PR 109 change caused the scheduling difference.
   The explicit ruling permits continuation in every measured outcome.
2. `TestSessionFirstReactionCountsAsWithAckOff/session-first/ack-answer-lost/ack-jev`:
   reviewed `session_first_reaction_counts_test.go:306` printed
   `after the session's first eyes: acts=-1, want 1`.
   The expected first success, lost already-reacted answer, and repeat
   remained present. `-1` is the helper's initial sentinel: its callback
   had started but had not stored the sample when `firstEyes` returned.
   The isolated `-race -run '^TestSessionFirstReactionCountsAsWithAckOff$'
   -count=20 -timeout=30m` run passed 20/20 before the fix, with no
   failing real count or journal assertion. Loads: 3.95 / 5.64 / 6.60
   before, 3.78 / 5.48 / 6.51 after.
   The ruling authorized the fix even when the isolated runs were clean.
   Commit `300f28b3dbfe571c3aa315edfa1d2f95cdb8e148` adds a channel closed
   at the end of `first`, then waits before reading the stored sample.
   It uses the rig's existing 10-second bound and fails explicitly if the
   callback never finishes. The sample is still taken at the same point
   inside the callback; no assertion, expected count, or journal check
   changed. After the fix, the same command passed 20/20, exit 0, with
   loads 6.28 / 5.71 / 6.47 before and 6.57 / 5.88 / 6.51 after.

Both `go vet ./...` runs exited 0 with no diagnostics; Slack vet was repeated
after the helper edit and remained clean. Google Chat's full `gofmt -l .`
output was empty. Slack's output was `company_thread_ambient.go` on both
exact main and the merged tree; that unchanged baseline deviation was left
alone. The edited helper file is formatted. The repository `make test`
target was not used; Python is run directly from each pack. No Node test
command ran. Final command counts are in `validation.txt`.

Direct Python commands were run from each pack:
`env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q`.
Google Chat: 52 passed and 101 subtests passed, exit 0.
Slack: 986 passed, 3 skipped, 10 subtests passed, exit 0. Its two
`DeprecationWarning` messages concern forking from a multithreaded test
process. The structured command records are in `suite-results.json`.

## Scope outcomes and review

1. **DONE:** parent merge completed without conflict; both full Go suites
   passed after the disclosed rerun protocols. One later, explicitly
   authorized PR 109 test-helper edit is identified above.
2. **DONE for existing pair coverage:** both overlapping files and their
   passing regression tests are named above. No new test or behavior.
3. **DONE:** equal-tree proof, ancestry-only merge, empty tree diff, and
   main ancestor check. `main-stat.txt` gives the stat at the ancestry merge:
   32 files changed, 6,803 insertions, 99 deletions, all PR 109's existing
   delta. This evidence commit is additional documentation only.
4. **Delivery contract:** the same draft PR 109 is retargeted to `main`;
   the final readback, including its exact evidence-commit head and
   mergeability, is recorded in the handoff. The prior body is preserved
   exactly, followed by this round's section and validation disclosures.
5. **Validation:** full suites, vet, formatting, race runs, isolated
   measurements, and direct Python results are recorded with exit codes.
   The non-green Slack race result is disclosed under the explicit ruling;
   it was not silently retried into a green claim. The final full Slack
   capture-flake recurrence is also disclosed, with no further retry.
6. **DONE:** new production lines from merge resolution or later changes:
   **0**. The full pre-existing PR production delta against main is 673
   added/deleted lines. The sole test edit adds 9 lines in `firstEyes`.

One STANDARD self-review on the worker's own model examined the parent
merge's remerge diff, the ancestry merge, this evidence, the existing pair
tests, the helper completion wait, and the PR-body addition. Findings:
**BLOCKER 0, MAJOR 0,
MINOR 0, NIT 0**. This is not a Codex gate or second-model review.
The deadline, capture, formatting, and stub-race observations are outside
the resolution; a regression introduced by the resolution is **NOT SHOWN**.
The early-sample helper is **FIXED** by the separately authorized test-only
commit. No other test or production fix was attempted.

Public-repo check: done, **0** things changed for redaction. The new commit
messages and evidence contain no local home paths or private identifiers.
The Mayor's whole-delta Codex gate and second-model read remain separate
handoff requirements. This worker neither marks the PR ready nor merges it.
