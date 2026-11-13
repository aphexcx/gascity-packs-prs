# Google Chat coalescing evidence

Room chatter now waits a fixed, durable window per space (eight seconds by
default), then arrives as one turn with every message's own sender line.
Direct messages remain immediate single turns. Mentions and replies in the
identity's threads take held chatter along immediately. Live reactions wait,
including those in direct messages. `coalesce_window = "0s"` preserves the
previous text and IDs byte for byte.

The measured implementation is `30ef56d2c558f1a284d31019c79805f43cdc568b`.
It includes the stacked base's second revision,
`31f5e33e8ecc103f19f1fee0ed7045a787e72dba`, by merge commit. Initial
acceptance tests were committed separately as `6c27fe9c`, before product
changes. The baseline measurement and red runs use
`ebd7f446b875c85d84f0bef9194de2cc2c5cfcce`.

## Contract

1. **DONE — setting and off switch:** absent means eight seconds; `0s` through
   `60s` accepted; invalid types, units and bounds refused with the key named.
   The existing settings reload rebuilds the generation; changing this key
   does not require a separate manual restart.
2. **DONE — routing:** direct messages, mentions, own-thread replies and
   catch-up go immediately; ordinary room acts and live reactions wait.
   Mentions and own-thread replies flush held chatter in the same turn.
3. **DONE — windows:** fixed from the first act, independent by space, with
   their own timer; 50 acts or 32,000 text bytes cap a group. Oversized acts
   go alone. Recovery polling cannot post a held act early.
4. **DONE — text and budget:** provider-time order, deterministic anchor,
   intact individual sender lines, quoted forged frames/headers, and all
   four byte budgets pass.
5. **DONE — changes inside a window:** unseen edits fold, unseen deletions
   disappear, delivered-message edits/deletions retain their established
   wording, reaction pairs cancel, attachments stay with their message.
6. **DONE — durability:** held records precede acknowledgment; frozen members
   and bytes precede POST. Restart, all three crash points, redelivery,
   receipt polling, uncertainty, replay and acceptance are covered.
7. **DONE — catch-up:** 120 missed messages produce three capped turns;
   budget accounting still counts acts and cursor advancement follows
   settlement. History bounds pass with coalescing on and off.
8. **DONE — ledger compatibility:** new reader loads the released fixture;
   rollback-reader cases cover held, delivered and in-flight groups. History
   and deletion attribution use the original individual message.
9. **DONE — operator guidance:** README, skill, Unreleased changelog and
   status describe timing, attribution, setting, reload and rollback. No
   pack version changed.

## Measured bytes

These are UTF-8 bytes, not model tokens. The real processor runs against
repository fakes. The envelope helper mirrors the formula in those fakes;
gc's renderer is outside this repository. No live Google endpoint or real
gc renderer was exercised.

| Sample | Turns | People's words | Adapter fixed | Whole-turn fixed | Total |
| --- | ---: | ---: | ---: | ---: | ---: |
| Before: one room message | 1 | 40 | 113 | 452 | 492 |
| After: one room message | 1 | 40 | 113 | 452 | 492 |
| Before: one direct message | 1 | 40 | 113 | 452 | 492 |
| After: one direct message | 1 | 40 | 113 | 452 | 492 |
| Before: five room messages | 5 | 97 | 573 | 2,264 | 2,361 |
| After: five room messages | 1 | 97 | 646 | 985 | 1,082 |
| After: two room messages | 1 | 48 | 296 | 635 | 683 |
| After: fifty room messages | 1 | 970 | 5,174 | 5,513 | 6,483 |

**B1:** single-message fixed bytes remain 452, and the adapter adds only its
from line plus one byte. **B2:** each merged member fits its unchanged from
line plus two bytes, with a header allowance of 80 bytes live or 140 timed;
both header forms are tested. **B3:** five-message whole-turn fixed bytes
are 985, below 1,000. **B4:** two-message fixed bytes are 635, below the
904-byte bound for two individual turns.

Full text captures: [before](baseline.txt), [after singles/off-mode
pins](final-singles.txt), and [after merged turns](merged-budget.txt).

## Tests and review

[Red named runs](red-named-tests.json) and [green named
runs](green-named-tests.json) give each invocation, exit code and count
(top-level tests and subtests combined in these two manifests). Every Go
command ran from `gchat/adapter` with `GC_BIN` and `NEVER_SILENT` unset.
The red replay uses a Go overlay of the original stacked base plus the
initial test commit, without changing the lane. The server fixture spelling
was corrected to `options` for that replay; no red is merely a build failure.

| Initial test | Baseline | Implementation |
| --- | --- | --- |
| TestCatchUpIsMerged | RED | GREEN |
| TestCoalesceWindowSettingBounds | RED | GREEN |
| TestBaselineTurnBytes | PASS: baseline pin | GREEN |
| TestTurnFixedBytesBudget | RED | GREEN |
| TestWindowOffIsToday | PASS: compatibility pin | GREEN |
| TestWindowIsFixedFromTheFirstAct | RED | GREEN |
| TestPollNeverPostsAHeldAct | RED | GREEN |
| TestSteadyChatterFlushesEveryWindow | RED | GREEN |
| TestSpacesDoNotDelayEachOther | RED | GREEN |
| TestWhatGoesAtOnce | RED | GREEN |
| TestWindowCaps | RED | GREEN |
| TestActsInsideTheWindow | RED | GREEN |
| TestHeldActsSurviveARestart | RED | GREEN |
| TestRedeliveryOfAHeldAct | RED | GREEN |
| TestMergedTurnKeepsEveryFromLine | RED | GREEN |
| TestMergedTurnCannotBeForged | RED | GREEN |
| TestCrashPointsOfAMergedTurn | RED | GREEN |
| TestMergedTurnIsOneDelivery | RED | GREEN |
| TestCoalescedLedgerCompatibility | RED | GREEN |
| TestCoalescingStatus | RED | GREEN |

Twelve further tests were added during implementation, review and base
integration. All pass individually. They are not claimed as original-base
red runs: `TestCatchUpWindowKeepsFloor`,
`TestHeldActSavePrecedesAcknowledgment`, `TestMentioningEditTakesHeldChatter`,
`TestMergedRedeliveryWhilePendingOrDelivered`,
`TestLateAndCatchUpHeaderBudget`, `TestFoldedEditsKeepProvenance`,
`TestCoalescingKeepsHistoryBoundary`, `TestWindowTimerOwnsDeadline`,
`TestMergedTurnUsesProviderOrderAndOneAnchor`,
`TestCoalescedReaderLoadsReleasedLedger`,
`TestCoalescedMembersCloneIndependently`, and
`TestMergedResolveAndHistoryReaders`. The floor, released-reader and original
clone checks also pin existing behavior. The added Python test is
`ProxyCommandsTest.test_status_text_names_window_and_held_acts`.

The DEEP self-review of `0a7e30f6d62141d90082ceeb14b8c44205b2afe4`
returned **BLOCK**, with one MAJOR finding and no other findings. Its class
was **body mutation after provenance sanitization**: stripping `edited: `
after rendering could expose a forged first body line. The class test first
failed for live single, live merged, catch-up single and catch-up merged
forms. The fix chooses the final body before `provenance.Render`; all four
forms now pass, as does the literal `edited:` preservation pin.

Outcome: **FIXED in `30ef56d2c558f1a284d31019c79805f43cdc568b`**.
See [the original review](self-review-r1.txt) and
[reproduction/fix results](review-resolution.json). The review could not run
tests inside its sandbox; the outside-sandbox reproductions and validation
are recorded here. This is not a later CLEAN review verdict. The mayor's
gate and second-model read remain pending.

## Validation

Go **1.27.3 darwin/arm64**; Python **3.15.0**. No Node or pnpm invocation
is involved. [Validation results](validation.json) record the final SHA,
exact commands, exits, package/top-level/subtest/skip counts and repeated
window/race runs. [Other checks](checks.json) record formatting, vet and
shell checks. `staticcheck` was absent, so the optional check was skipped.

The final full Go suite passes **21 packages, 725 top-level tests and 1,421
subtests**, with **two top-level skips** (`TestRealGCContract` and
`TestRealGCReplyReminder`, which require real gc). None of the known
load-sensitive cases failed on this final run; the 3,000-message catch-up
case passed in 470.39 seconds. The exact command from `gchat/adapter` was:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
```

The Python command is:

```sh
make test PYTEST_DIRS='tests gchat/tests' PYTHON='env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTHON python3'
```

It passes **463 tests, 250 subtests, with 127 skips**. The literal command
without that `PYTHON` override initially fails an unrelated Makefile test:
make's command-line overrides leak into its nested `make -n`. The invocation
above clears only that inherited make environment for the Python child; no
unrelated test or Makefile was changed.

All 32 new Go functions pass individually. The inbound coalescing tests run
20 times: **480 top-level tests and 940 subtests pass**. The focused race
run passes **3 top-level tests and 7 subtests**. The existing
`TestCloneIsolatesEveryMutableRecord` also passes separately: an early full
run exposed a nil-record clone panic, fixed before the reviewed commit.

Existing immediate-delivery fixtures explicitly set `coalesce_window` to
`0s`; their assertions are unchanged:

| Existing file | Change |
| --- | --- |
| `internal/inbound/inbound_test.go` | `setup` keeps legacy immediate turns. |
| `internal/catchup/catchup_test.go` | `setup` keeps individual recovered turns. |
| `internal/server/server_test.go` | `setup` keeps existing status/read fixtures immediate. |
| `internal/app/app_test.go` | `newFixture` keeps lifecycle fixtures immediate. |
| `test/e2e/e2e_test.go` | `newFixture` keeps end-to-end fixture expectations immediate. |
| `test/contract/contract_test.go` | `TestRealGCContract` keeps its previous transport contract. |
| `gchat/tests/test_commands.py` | Adds the new status-row test; existing tests unchanged. |

Go paths in this table are relative to `gchat/adapter`.

## Ledger and rollback

Held acts carry durable `HeldUntil` and `ActAt` fields; the anchor owns the
frozen delivery, `Members`, receipt, retries and unresolved record, while
other members point to it through `MemberOf` and settle atomically. Original
member `Event` and `Kind` are preserved in `MemberEvent` and `MemberKind`,
so reads and deletions still use the individual message's text and sender.

Before rollback, remove the unknown setting key. Held acts deliver singly
on the released reader and completed groups stay completed. An in-flight
frozen group remains one delivery: pending receipts are polled, retryable
delivery uses the frozen bytes, and an interrupted POST becomes one
unresolved record requiring explicit replay or acceptance. Compatibility
checks include the actual released reader, not only a schema simulation.

The regular `TestCoalescedLedgerCompatibility` strips fields unknown to the
released schema and exercises current recovery. The additional
[rollback runner](rollback-reader.py) exports five snapshots through the
new writer, then overlays every Go source file from released commit
`d2f453b83a428546dfe7fa01dc2bd831c0b47ae0` (pack version 0.5.0) and runs
`TestCoalescedLedgerCompatibility` with that source's actual `ledger.Load`
and `Poll`. It uses the current module dependency lock, creates no checkout,
and leaves the implementation lane unchanged. The released-reader cases
are held, delivered, pending receipt, frozen before POST and interrupted
POST; they assert turn counts, unchanged frozen bytes/IDs, member settlement
and the one unresolved record. Run it from the repository root:

```sh
python3 assets/pr-evidence/gp-49y9/rollback-reader.py
```

[Released-reader results](rollback-results.json) record both writer and
reader commands and counts: each passes one top-level test and five subtests.
The first harness attempt used the later named
receipt-delivery type and did not compile against 0.5.0; it was corrected to
assign the old anonymous field directly. This was a harness error, not a
behavioral failure or a claimed red test. The pending harness also initially
returned unknown because the fresh fake had never issued the receipt; the
final harness restores that already-issued delivery before starting recovery.

## Operator text, before and after

Before, there was no merged header or coalescing status line. The new exact
header forms are:

```text
[N messages in this room, oldest first. Each has its own from line.]
[N messages, oldest first; YYYY-MM-DD HH:MM to YYYY-MM-DD HH:MM UTC. Each has its own from line.]
[catch-up: N messages, oldest first; YYYY-MM-DD HH:MM to YYYY-MM-DD HH:MM UTC. Each has its own from line.]
```

The timed form applies when construction is over one minute after the oldest
act; catch-up always includes times. A singleton retains its existing text.
Every member's existing from line is unchanged. Existing reaction frames
have no `item` field: their unchanged body carries the target message ID.

The new status line is `Coalescing: <duration>; held acts: <count>` (for
example, `Coalescing: 8s; held acts: 3`). A delivery row adds
`; members <count>` only when a group has members.

Previously `coalesce_window` was an unknown setting. New validation messages
are `coalesce_window must be a Go duration: <parse error>` and
`coalesce_window must be between 0s and 60s`, within the existing settings
file error wrapper. A numeric TOML value is refused by the existing typed
decoder with `coalesce_window` named. The exact new parse-error format is
`coalesce_window must be a Go duration: %w`; bounds use the literal above.
The timer's retry log is `inbound window deferred; retained for retry`.

[Operator text diff](operator-text.diff) contains all old/new README, skill,
changelog and status lines, with zero context to keep unrelated changes out.
On a box, this feature begins only after a pack update and adapter restart.
Subsequent validated settings changes use the existing reload mechanism.

All people, addresses and provider IDs in the captures are invented.
Public-repo check: done, 0 things changed; no fixture redactions were needed.
