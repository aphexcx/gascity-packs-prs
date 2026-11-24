# Round 4: message time, partial admission, and retirement reporting

This round starts at `a553d22f3756240be77e9e0f8bc12025265c2ae2` and updates
the same draft PR. No main merge was made. The checked main tip is
`205d0aecedf944792f1e3b3fbc420b9e5ad910d8`, whose only commit since
`863e9aa88795101b45734f3fc0fbdcbbba2d6229` is PR 102's compact mention-only
Slack reminders. Its 11 changed files and this PR's 294 changed files have an
empty intersection. `main-files.txt`, `pr-files.txt`, `shared-files.txt` and
`main-tip.txt` retain the comparison; the unrelated evidence directory's bead
name is replaced with `<unrelated-change>` in the public main list.

## Changes and red-first checks

The timestamp class is a message's time taken from somewhere other than its
resource time or event time. `TestMessageTimeIsItsOwnOrItsEventTime`, in
`internal/inbound/history_time_test.go`, tests 32 cases at the decoded-event
boundary. The round-3 production source fails 18 and passes 14. The fix in
`ded745d1` passes all 32: bounded messages use resource time, then event time;
publish time never decides. Message validation rejects malformed resource
times in bounded and unbounded spaces.
Missing-time edits quarantine. A timeless create skips and increments the
missing-time counter in a bounded space; in an unbounded space it remains a
turn. Unbounded message fields are not filled from event time. Resource-time
precedence and batch forms are included. Reactions retain their old fallback.
The tests were committed first in `093e44f4`.

The admission class is a partly persisted admission read as a retired attempt.
`TestHalfSavedAdmissionKeepsItsNotices`, in
`internal/app/history_admission_test.go`, reproduces the two persisted stores
after Track succeeds and the primary ledger save fails. Both the same-process
reconciliation and disk-reload cases fail at the baseline: no owner line, no
operator mail, and the enrollment removed. After `25c6596f`, both give exactly
one owner line and one operator mail; two further four-minute polls repeat
neither. An alias only retires an enrollment older than the skip record.
The tests were committed first in `f726446c`.

`TestFirstStartCountsAndToldRetiredDeliveries`, in
`internal/app/history_retirement_test.go`, covers one waiting delivery, a saved
cursor, two deliveries in different spaces, backup restore, and restore with a
saved cursor. At the baseline, one/two/restore fail their absent counters;
one/two also lack the required mail. Both saved-cursor controls pass and each
delivers one turn. After `73c91616`, all five pass. Each record retired by
RetireSpaceHistory increments `history_delivery_retired:<space>` and contributes
its message reference to the load's report. The existing retirement predicate
and missing-time counter are unchanged. The tests precede the fix in
`12b930a2` and `2405014c`.

The final scope ruling mails at load only: a backup restore and a running
sweep count retirements but do not mail them. The initial dispatch requested
restore mail, and the first red run included that expectation. The scope stop
identified the required server callback; the subsequent ruling withdrew that
expectation. The server package remains untouched. A later restore-mail change
would retain references across pending restore retries and call an optional
callback after successful save and reconciliation.

`TestHistoryRetirementNoticeDedup` fails at the baseline, where two different
starts with the same body collapse into one notification. It passes at the
fix: retries of the same start deduplicate and distinct starts have different
keys. `TestRunningSweepCountsRetiredDeliveries`, in
`internal/catchup/history_retirement_test.go`, fails all five baseline rows and
passes after the fix. It covers create/update/delete and both reaction kinds,
three unfinished statuses per row, missing creation time, a second sweep that
does not recount, and zero turns afterward. Its tests precede the fix in
`454ef564`.

Baseline runs use Go's source overlay to substitute every production Go file
changed by this round with its exact blob from the starting commit. Test files
remain present. No second checkout or worktree is used. The first retirement
red run retained the unrelated admission fix; the later baseline and final
ruled red runs restore it too. `manifest.json` records every command and exit
code; `completion-manifest.jsonl` gives one command/result per line, and `runs/`
contains the logs. No failure is omitted.

## The renamed existing test

`TestMessageWithoutActTimeUsesPublishTime` becomes
`TestMessageWithoutActTimeNeverUsesPublishTime`. Its six expectations change
only as required by the new message-time contract:

| Row | Before | After |
| --- | --- | --- |
| created/old | Ack, no turn, before-history skip | Ack, no turn, missing-time count 1 |
| created/new | Ack, one turn | Ack, no turn, missing-time count 1 |
| created/none | Ack, no turn, missing-time count 1 | unchanged |
| updated/old | Ack, no turn, before-history skip | missing-update-time quarantine, no turn |
| updated/new | Ack, one turn | missing-update-time quarantine, no turn |
| updated/none | Ack, no turn, missing-time count 1 | missing-update-time quarantine, no turn |

No other existing test is edited. The new retirement test's restore row was
changed within this round to follow the scope ruling described above.

## Verification

Go is `go1.27.3 darwin/arm64`. The Makefile's uv fallback runs Python `3.13.13`.
All 25 named commands pass: 25 top-level tests and 177 subtests. Twenty-four
commands are unchanged; one uses the authorized renamed timestamp test.
All five new tests pass individually at `73c91616`: five top-level tests and
44 subtests. TestOldActStartsNoNotice passes its 12 rows unchanged, and
TestCursorlessMigrationRetiresDetachedAttempts passes its 15 rows unchanged.
The three notice tests pass with `-race`: three tests and 20 subtests.
Formatting emits nothing; vet, sh -n and shellcheck pass. Staticcheck is not
installed and was not run. No toolchain dependency was installed.

The required `make test PYTEST_DIRS='tests gchat/tests'` exits 2 with 463 passed,
one failed, 127 skipped and 250 subtests passed. As in round 3, the directory
override leaks into the nested Makefile-discovery check. The clean-environment
equivalent passes 464 tests and 250 subtests, with 127 skips:

```sh
env -u GC_BIN -u NEVER_SILENT -u GC_TEMPLATE -u MAKEFLAGS -u MFLAGS \
  -u MAKEOVERRIDES -u PYTEST_DIRS \
  uv run --with pytest --with pyyaml --with jsonschema \
  python3 -m pytest tests gchat/tests -q
```

Full Go invocation, from gchat/adapter:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
```

The first full Go run exits 1 after 1,571.311 seconds. It tests 22 packages:
21 pass and inbound fails. There are 736 passing top-level tests, one failing
top-level test, 1,490 passing subtests, seven failing subtests and two optional
live-gc skips. The 3,000-message recovery completes with zero backlog.
The sole failed test is the unchanged TestLookupHoldCarriesCredentialKind:
reaction_parent with 401/403/408/409/503/429 and reaction_space with 401.
Its 100 ms request deadline produces transient errors without the expected
HTTP metadata; the failed rows take 0.21–0.72 seconds. The inbound package had
already passed on its own (119 tests, 258 subtests), and the immediate isolated
rerun passes all 18 rows. This is consistent with load-sensitive fixture timing;
neither the fixture nor its timeout is changed. The first failure remains in
the manifest and log.

The second full run exits 1 after 1,388.924 seconds: 22 packages tested,
21 passing packages and one failing app package; 736 passing top-level tests,
one failing top-level test, 1,496 passing subtests, one failing subtest and
two optional live-gc skips. Its only failure is the bead-listed load-sensitive
TestShutdownCheckpointsBeforeDrainingStalledRequestBody/bounded, with
"HTTP body prevented bounded drain" at 3.36 seconds. The isolated bounded
case passes (one test, one subtest). Its timeout and assertions are unchanged.
The credential-lookup test passes in this full run, and recovery again reaches
3,000/3,000 with zero backlog. Exactly two full Go runs were made; neither
is represented as a clean full-run exit. All requested new and named regression
commands pass, and the final full run has only the specified timing exception.

## Self-review

The single fresh DEEP self-review at
`73c916165cb641195404d986dc8df016a1f9a27b`, over this round's commits only,
returned **CLEAN**, with no blocking finding in the delta. The immutable answer
is in `self-review.txt`. Production code and tests are unchanged after this
review. Documentation avoids claiming validation on the pre-existing queue
migration path described below.

One MINOR was READ, pre-existing and outside the delta. Its class is queue
migration bypassing timestamp validation. Reproduction requires a cursorless
space containing a validly shaped queued create whose CreateTime is
`not-a-time`, whose event/publish/queue times are zero, and whose Malformed
field is empty. Migration commits it as history_time_missing instead of
quarantining it; the reviewer identified the same sequence at the base.
Outcome: **NOT REPRODUCED in this round**, and not changed, per the scope rule.
The reviewer ran no tests in its sandbox and did not infer a full-suite pass.

The Mayor's round-4 gate and second-model read remain pending. Round 3's gate
was BLOCK on two MAJORs; its second-model read was CLEAN with four MINORs.
This round answers those four findings. All baseline source anchors in the
spec matched; the edits naturally move subsequent line numbers.

## Operator strings and limits

`operator-strings.diff` preserves the old and new README passages and the
notification branch. The new body recorded by the fake for one retirement is:

```text
The first load ended 1 waiting deliveries as history:
spaces/dm/messages/one
```

The two-space case uses count 2 and lists both resource references. It contains
no message text or sender name. No existing turn or notice text changes.
The first-load README now tells operators to inspect waiting and unresolved
deliveries before taking the version, and explains the new counter and mail.

All checks use fakes, with GC_BIN and NEVER_SILENT removed before each Go
command; fixtures install their own fake implementations when needed. Main's
behavior for timeless creates and edits was read, not executed on main's tree.
The new bounded and unbounded behavior is shown by this round's tests.

`pr-body-before.md`, `pr-body-after.md` and `pr-body.diff` preserve every PR-body
sentence changed by this round, including the three requested notes about the
notice lock, late space listing or clock skew, and the live-stream join bound.
The public-repository check changed four artifacts: the unrelated bead label
in the main file list, private paths in both full-suite logs, and private paths
in the review record. The final scan found no private paths, private names or
unrelated bead IDs. Neither the code nor the tests contain a real provider ID.
