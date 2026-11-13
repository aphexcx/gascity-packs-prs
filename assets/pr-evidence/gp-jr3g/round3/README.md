# Round 3: history bounds and never-silent notices

Main was merged once, at `863e9aa88795101b45734f3fc0fbdcbbba2d6229`.
The merge commit is `0b479e5757d2036c15225019b75709abfdb8ff28`, with first
parent `31f5e33e8ecc103f19f1fee0ed7045a787e72dba` and second parent main.
It contains only the automatic merge and the two ruled status resolutions.
The status route retains both keys; the CLI prints History immediately after
Last event, then Never silent, then Backlog. Both parents' README and changelog
entries remain in their automatically merged positions.

`merge-tree.txt`, `merge.txt` and `resolved-regions.txt` preserve the conflict
measurement, merge output and resolutions. Formatting produced no output and
vet passed before the merge was committed. `interactions.md` records the
pre-test read of all ten shared files and both subsystems' additional readers.

The tests are a separate commit, `beee309a`. The retirement fix is a subsequent
commit, `d6721ca01063a5c03b44aeddd42c4594c8e30db0`, confined to inbound and the
notice engine, with the requested README sentence.
The self-review correction is `0741da657a8e2011a6a2944470c6e97f0970aced`;
its production edits remain in inbound, app and the notice engine.

## Interaction checks

All commands and exit codes are in `manifest.json`; `runs/` holds their output.
The runner explicitly removed `GC_BIN` and `NEVER_SILENT` from every command's
environment. Fixtures install their own fake gc or engine callbacks. No command
used a live provider, live ledger, real notice file or operator mail.

| Row | Test and result at the merge | Result at the fix |
| --- | --- | --- |
| A | `TestOldActStartsNoNotice`: pin, 12 cases, each with the positive current-message control | 1 test, 12 subtests pass |
| B | `TestRetiredAttemptEndsItsNotice`: 12 retirement cases fail; both saved-cursor controls pass | 1 test, all 14 subtests pass |
| C | `TestLedgerOfEitherParentLoads`: pin, main/PR-head/merged ledger shapes | 1 test, 3 subtests pass; released-reader exchange also passes |
| D | `TestStatusCarriesTheFloorAndTheNotices` and `test_status_prints_history_before_never_silent`: both pass with both hunks, and each fails when either parent's hunk is removed | Go and Python checks pass with both hunks restored |
| E | `TestConfigCarriesBothSettings`: pin for on, unset and four other switch spellings; both invalid durations refused in every row | 1 test, 6 subtests pass |
| F | The exact 25 commands of round 2's final named manifest, unchanged | 25 tests and 177 subtests pass, including the 60-row act matrix |

A covers old and missing-time creates on L/R/C/Q/O/P, zero turns, no waiting
backlog or unfinished attempt, no notice records, no owner line, no local or
operator mail, and an empty status outage list. Each case also drives a current
live create to a definite delivery failure: one tracked message, one owner
line, one operator mail and one local mail. Actual Pub/Sub assigns publish time;
the no-usable-time L/R/O/P cases therefore inject its decoded event with no
publish-time fallback. Redelivery includes ledger and engine reload. The first
A run had two fixture failures (the isolated replay server was initializing,
returning 503). Marking that test server ready restored the intended 400 replay
refusal; the corrected pin passed at the unchanged merge production source.

B builds a saved main-shaped attempt and its separate notice file on fakes.
It covers unresolved, failed intent, delivered evidence plus fresh no_seat
health, removed queue rows, a queue row with no usable time, backup restore with
the engine alive, and a saved-cursor control. Every row runs with and without
an owner line and successful start mail already sent before the load. Three
post-load polls, four fake minutes apart, expose the stale notice. At the merge,
each retirement case retains one open outage; previously unnotified cases send
one owner line and one start mail. Previously notified cases never clear.

The fix recognizes committed/local history retirement before replaying delivery
observations. It forgets only an existing enrollment, including the original
message ID of a missing-time queued create when no primary record survives.
One hundred untracked skipped acts leave the notice file at its single existing
message entry. No retirement policy, notice line, mail body, timing or switch
changed. After retirement an unnotified owner sees nothing; a previously
notified owner sees no repeat, and the operator receives the normal cleared
mail. The saved-cursor control retains exactly one owner line and start mail.

C preserves main's three change notice fields and send-part sent_at through
migration on unaffected records. A PR-head ledger changes no engine state.
The frozen 0.5.0 record shapes, copied from the merge base, drop only the new
history and notice fields. Main's own reader recognizes reason local as a
non-turn and ignores the history cause. The original round-2 rollback writer
and reader were rerun against the exact base source using `round2/overlay.py
--reader`; the overlay needed no change for main's new Go files. Both commands
pass and every edit/deletion after rollback still produces zero turns.

`TestNoticePostSpaceStartMatchesProcessor` passes four cases: inventory, floor,
neither, and saved cursor. A notice post can start a cursorless space during
uncached type lookup; with saved inventory or floor it uses the same boundary
as inbound, while with neither boundary the two paths use their own encounter
times. Existing cursors remain untouched, and posting a notice creates no
session-send record. State reset's existing separate notice-file behavior is
outside this round.

## Validation

Go is `go1.27.3 darwin/arm64`. The Makefile's uv fallback actually runs Python
`3.13.13` with pytest `9.2.0`; the shell's bare python3 is `3.15.0`.
Before self-review, the six new Go tests pass individually: six top-level tests
and 39 subtests. The concurrency regression adds one test and four subtests,
and passes with the race detector as well. All seven pass individually at
`0741da65`: seven top-level tests and 43 subtests.
All 25 unchanged round-2 named tests pass individually: 25 and 177.
Final formatting produces no output; vet, sh -n and shellcheck -x pass.
Staticcheck is not installed and was not run; nothing was installed.

The required `make test PYTEST_DIRS='tests gchat/tests'` exits 2 with 463 passed,
one failed, 127 skipped and 250 subtests passed. Its Makefile-discovery test
inherits the command-line PYTEST_DIRS override in its nested make -n invocation,
so it no longer sees the default directory list it asserts. Both that test and
the Makefile are unchanged from main. The isolated test passes with inherited
make variables cleared. The equivalent full suite in that clean environment
passes twice, including at `0741da65`: 464 tests and 250 subtests, with 127 skips
in each run:

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

The first full Go run, using the `d6721ca0` binaries, exits 0: 22 packages,
731 top-level tests and 1,449 subtests pass, with two optional live-gc tests
skipped. The full run's 3,000-message recovery finishes with zero backlog.
The second full run validates `0741da65` after the concurrency correction and
exits 0: 22 packages, 732 top-level tests and 1,453 subtests pass, with the same
two optional live-gc skips. Its 3,000-message recovery also finishes at 3000/3000
with zero backlog. Exactly two full Go runs were made, both successful; the
first took 1,356.904 seconds and the second 1,583.429 seconds.

## Self-review

The one fresh DEEP self-review inspected the merge against both parents and
this round's commits through `d6721ca0`. Its verdict remains **BLOCK**, with
one MAJOR finding class: retirement based on a ledger snapshot not ordered
against admission or restore. It found no other issue in this round. The
mechanical merge, retained parent tests, interaction table and space-start
answer were confirmed. The immutable review is preserved in `self-review.txt`.

Both cases were reproduced on the reviewed production source. A missing-time
skip followed by a current live create, then reconciliation of the earlier
snapshot, gives zero owner lines and zero mails instead of one each. A restored
cursorless backup retires its stopped attempt, but reconciliation of the
pre-restore snapshot gives one owner line and one mail instead of none. Both
rows of `TestHistoryNoticeStaleSnapshotProbe` fail. The test-only appended
source is `stale-snapshot-probe.go.txt`; it runs as an overlay of the reviewed
app test file, without changing production. This probe deliberately schedules
the obsolete snapshots; it is not claimed to be a supported use of the new
ordering protocol.

Outcome: **FIXED in `0741da65`**. Enrollment plus its primary ledger record,
and snapshot plus retirement reconciliation, share a short local transaction
lock. That lock is released before gc delivery or notice effects. A separate
app mutex orders the entire notice pass with POST /state, while the ledger
mutex and ordinary health transitions remain available during external I/O.
Thus either the notice completes before the load, or the load retires the
attempt before the next notice decision; no stale decision crosses the load.

`TestHistoryNoticeInterleavings` covers snapshot-before-admission,
track-before-record, restore-before-poll and restore-during-poll. All four
pass, including under `go test -race`. A notice already in flight completes
before restore returns, then its ordinary cleared mail follows retirement;
a restore that runs first produces no owner line or start mail. The initial
implementation used the existing health-transition mutex; the final version
uses its own notice/restore mutex so it does not delay unrelated health changes.
No second formal review was run, and the original BLOCK verdict is not relabeled.

## Operator strings and limits

`operator-strings.diff` contains every production/documentation line this round
adds after the merge. The one new operator sentence is: "An act older than its
history bound starts no notice, and a message whose attempt the first load
retired earns none." It had no prior sentence. Both status lines are retained
byte for byte from their parents; no owner or mail string changes.

Round 1's gate and second-model read blocked, and round 2 answered their
findings. Round 2's gate did not run because the branch was behind main. The
whole-PR gate and second-model read after this round remain pending, as does
the requester's ruling on the first-start interpretation. The PR stays draft.

Main later advanced to `205d0aecedf944792f1e3b3fbc420b9e5ad910d8` (PR 102,
Slack delivery reminders), with no gchat changes. The contract permits one
merge of the start-time main and explicitly says not to chase later commits.
