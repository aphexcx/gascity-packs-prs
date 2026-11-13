# First-start history bound

Base: `4d14c7c5d52a468a2af7e387175ff4fb20366ec5`.
Tests-first commit: `9061f5be`.
Implementation and review fixes: `16951dbb`, `ec506890`.

A new ledger records its history floor before the established marker. The floor
moves until the first subscription is recorded, then stays fixed. Every room and
direct message uses it, including spaces discovered later. A restart keeps its
saved cursors. Existing ledgers gain no floor. Reset uses the same rule, with an
optional window of at most 168 hours; restore retains the backup's state.

This implements the mayor's reading of the request that a new box should not
replay old messages. The merge card asks the requester to confirm that shape.

## Red first, then green

The tests-first commit changes no production code. From `gchat/adapter`:

```sh
go test ./internal/app ./internal/catchup ./internal/config ./internal/server ./internal/ledger ./cmd/gc-gchat-adapter -run 'Test(FirstStart|Floor|LiveMessageBefore|RestartWithALedger|JoinTimeAndFirstStart|ResetTakes|StatusNamesTheHistoryFloor|HistoryFloorLedger)' -count=1 -timeout=5m -json
```

At the base this produced 17 failed and 9 passed test events, including subtests,
across six packages. The class test, `TestFirstStartReplaysNothing`, delivered
10 turns: seven old creates, an old edit, an old reaction, and the one new direct
message. All eight old messages, including the identity's own message, were
touched by automatic recovery. After the change the same fixture delivers
exactly one turn, keeps every old message readable, records none of the old
history, leaves every backlog empty, and sends zero notifications.

The first green regression run added `./internal/inbound` and used this filter:

```sh
go test ./internal/app ./internal/catchup ./internal/config ./internal/inbound ./internal/server ./internal/ledger ./cmd/gc-gchat-adapter -run 'Test(FirstStart|Floor|LiveMessageBefore|RestartWithALedger|JoinTimeAndFirstStart|ResetTakes|StatusNamesTheHistoryFloor|HistoryFloor)' -count=1 -timeout=5m -json
```

It passed 27 test events across seven packages. The later review fixes have
additional focused red and green checks described below.

| Contract | Regression | Base behavior | Changed behavior |
| --- | --- | --- | --- |
| 1 | `TestFirstStartWritesTheFloorBeforeTheMarker` | Fresh floor missing; saved old ledger unchanged | Fresh floor persisted; old ledger unchanged |
| 2 | `TestFloorMovesUntilTheFirstSubscription` | Floor missing | Advances at first connection; fixed after recorded subscription |
| 3 | `TestFloorBoundsEveryNewSpace` | Six turns instead of two | One new turn per room and DM; later space also bounded; no history-loss warning |
| 4 | `TestLiveMessageBeforeTheFloorIsNotATurn` | Adopted subscription posts the old message | Committed `before_history_floor`, zero old-message posts or notifications |
| 5 | `TestFirstStartHistoryIsReadableAndNeverATurn` | Old message is readable but becomes a turn | Readable without a change record or turn |
| 6 | `TestRestartWithALedgerStillDeliversTheOutage` | Pass | Pass: three outage messages, once each, marked catch-up |
| 7 | `TestJoinTimeAndFirstStartAreOneRule` | Pass for the join bound | Pass with both bounds; only the post-join message arrives |
| 8 | `TestFirstStartHistorySettingBounds` | Valid windows rejected as unknown; day guidance missing | Absent, 0s, 24h, 168h accepted; negative, over-week, year, integer, 1d and empty string refused |
| 8 | `TestFirstStartWindowReachesBack` | Two old messages arrive | Only the 23-hour message arrives; the 25-hour message stays out |
| 9 | `TestResetTakesTheFirstStartBound` in CLI and app | Old and new messages arrive | Only the new message arrives in both reset paths |
| 10 | `TestStatusNamesTheHistoryFloor` | Field missing | Exact floor shown; absent on a legacy ledger |
| 10 | `test_status_text_names_the_history_floor` | History line missing | Exact line follows Last event; absent without a floor |
| 13 | `TestHistoryFloorLedgerCompatibility` | New floor cannot persist | Both readers work; rollback loses only the floor and retains cursors |
| 14 | `TestFirstStartReplaysNothing` | 10 turns | One turn; no old history records, backlog or notifications |

The Python status regression was red with one failed and 48 deselected tests:

```sh
make test PYTEST_DIRS=gchat/tests PYTEST_ARGS='-q -k status_text_names_the_history_floor'
```

`TestFirstSavedFloorSurvivesMarkerFailure` additionally makes the marker directory
read-only and checks that the first saved file already has its floor when marker
creation fails. It passes without a persistence hook or production test seam.

## Review findings

The fresh self-review read all 22 files at `16951dbb` and returned zero blockers,
one major and one minor. Both findings were reproduced before fixing them in
`ec506890`:

- MAJOR, consuming a later act while suppressing a create: a prior reaction
  creates message state, then an old create payload advances its revision to an
  edit that has not arrived yet. `TestHistoryFloorKeepsNewActs/reaction-first`
  failed with zero edit turns. The fix preserves the prior revision and consumes
  only the create. Both unseen and reaction-first cases pass.
- MINOR, losing attribution in a retained non-turn record: old create, new edit,
  then deletion by message name lost the sender and frame. The unseen case
  failed its actor/frame assertion. The suppressed create now retains the
  existing envelope and frame, with no old body or attachments. The test checks
  no POST, backlog or raise for the suppressed create and correct attribution
  on the later deletion. The scope ruling permits this preservation; the
  renderer and first-line format are unchanged.

The class-level review test also checks a new reaction on an old own message.
Before the formal self-review, its unseen-edit case found and fixed the same
revision-consumption class for a message with no prior state. A test assertion
that incorrectly treated normal turn counters as history loss was corrected to
check `unrecoverable:` counters and warnings specifically.

The origin result is returned by `LoadWithOrigin`; existing `Load` callers keep
the same value and error contract. This preserves the unchanged legacy
`TestLoadMissingFile` equality assertion.

## Verification

Go checks ran on `go1.27.3 darwin/arm64`. Python checks ran through the Makefile's
uv environment: Python 3.13.13 and pytest 9.2.0. No JavaScript checks apply.

The fake-only base suite passed 652 top-level tests and 1,167 subtests across
all 21 packages, with `TestRealGCContract` and `TestRealGCReplyReminder` skipped.
It used a read-only Go overlay of the exact base blobs for changed Go files,
omitting newly added tests, without switching the lane or creating a worktree.
`BASE_OVERLAY` names that private overlay file; `GOFLAGS` propagates it to the
nested CLI builds as well as the main test command:

```sh
env -u GC_BIN GOFLAGS="-overlay=$BASE_OVERLAY" go test ./... -p=1 -count=1 -timeout=30m -json
```

From `gchat/adapter`, the final production-code commit `ec506890` ran:

```sh
env -u GC_BIN go test ./... -p=1 -count=1 -timeout=30m -json
```

It passed 666 top-level tests and 1,180 subtests, with two optional real-gc tests
skipped. Two top-level tests failed: `TestManagedSourceDeliversDM` timed out
waiting five seconds for health, and `TestEndToEnd` had one failing subtest,
`revoked_token_puts_the_adapter_in_\`needs-sign-in\`_and_health_says_so`, which
timed out waiting ten seconds for a turn. Nineteen packages passed and two
failed. Another lane was running the same full suite concurrently on this host.
The managed-source case passed in the subsequent focused run without a change.
The revoked-token case failed again in isolation: it started with invalid
credentials and expected a message written before the first successful
connection to arrive. That expectation conflicts with the new first-connection
bound. Its fixture now establishes the adapter before revocation, preserving
its outage-recovery assertions and deadlines. The focused case then passed.

The final focused review-fix run passed seven top-level tests and two subtests
across four packages:

```sh
env -u GC_BIN go test ./internal/inbound ./internal/app ./internal/ledger ./internal/server -run 'Test(HistoryFloorKeepsNewActs|FirstSavedFloorSurvivesMarkerFailure|FloorMovesUntilTheFirstSubscription|LoadMissingFile|ReadReactStatusAndState|RevokedTokenReplacementResumes|ManagedSourceDeliversDM)$' -count=1 -p=1 -timeout=5m -json
```

The revoked-token fixture's red and green runs used the same command. Red had
one failing top-level test and one failing subtest; green passed both:

```sh
env -u GC_BIN go test ./test/e2e -run '^TestEndToEnd$/^revoked_token' -count=1 -p=1 -timeout=5m -json
```

The draft PR validation and READY record the subsequent complete e2e rerun
after that fixture adjustment, using:

```sh
env -u GC_BIN go test ./test/e2e -p=1 -count=1 -timeout=30m -json
```

From the repository root, `make test PYTEST_DIRS=gchat/tests` passed at both
revisions: 48 tests and 89 subtests at the base, 49 tests and 89 subtests at the
head. `gofmt -l .` was empty at base and head; `go vet ./...` passed at both,
with the final run using `env -u GC_BIN go vet ./...`. `shellcheck -x` and
`sh -n` passed for the base and head status command. The `-x` flag follows its
existing common helper. `gc lint gchat --json` passed with zero errors.

Earlier exploratory full runs inherited `GC_BIN`, enabling two optional
contract tests. They started temporary example cities and loopback supervisors
with fake Google and agent runtimes; both passed. No deployed city, customer
box, Google API or live adapter endpoint was contacted. A subsequent read found
zero temporary contract city directories and zero contract supervisor
processes. Those runs are disclosed here, not used as the fake-only suite
evidence. Final validation explicitly unsets `GC_BIN` and confirms both tests
skip.

## Existing tests changed

- The 3,000-message e2e outage now starts, stops, seeds history, then restarts.
  Its backlog, budget, delivery and ordering assertions are unchanged.
- The e2e live-event/frozen-cursor/restart scenario uses the same real outage
  setup. Its equal-time pagination and restart assertions are unchanged.
- The revoked-token e2e scenario also establishes the adapter before revocation,
  so retained messages belong to an actual credential outage. Its health,
  notification and delivery assertions and deadlines are unchanged.
- The CLI reset branch of `TestStateOfflineRestoreAndReset`, the server reset
  branch of `TestReadReactStatusAndState`, and the e2e corrupt-ledger reset
  scenario assert the new definite-loss warning.

The server warning assertion and revoked-token fixture were additional
occurrences beyond those listed in the spec. The existing tests otherwise
remain unchanged. The added
first-subscription test has a 15-second startup budget and waits for a new
subscription read before checking that a later generation preserves the floor.
The existing non-turn reason switch in `historyContent` was another occurrence
beyond the two named switches; it now recognizes `before_history_floor` so an
on-demand read still fetches the message content. Other cited base locations
matched the checked-out base.

## Compatibility and retention

A rollback to 0.5.0 requires removing `first_start_history` from settings if it
was configured: the baseline settings tests demonstrate that unknown-key refusal.
The ledger rollback drops `history_floor`. Spaces already saved keep their
cursors; a space first discovered after rollback resumes the old epoch-based
history behavior. The ledger version and pack version do not change.

Skipped history is not enumerated or recorded. If an old create arrives on the
live stream, the ledger keeps `MessageState` revision/thread/mine/send-key state
and a committed `ChangeState` with `id`, `space`, `message`, `kind`, `created`,
`reason=before_history_floor`, `event`, and `provenance_framed`; `event` contains
the existing sender envelope and provenance frame, without the old message body
or attachments. This lets later edits, deletions and reactions keep their
existing behavior.

Operator-visible wording is recorded in [strings.txt](strings.txt). No pack
version, token path, outbound path, renderer, skill, deployed box or image is
changed. All identifiers in new fixtures are invented.
