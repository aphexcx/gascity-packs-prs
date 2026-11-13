# Coalescing review round 2

Parent: `a553d22f3756240be77e9e0f8bc12025265c2ae2`. The initial head was
`8101a65e53176f8413e33127b9422e7c7370097d`; merge `2e8bd289` brought in the parent.
Four conflict hunks were resolved by hand: inbound Pass keeps window flushing
and notice reconciliation; server status keeps both fields; status.sh keeps
both lines; test_commands.py keeps both tests.

## ChangeState.Kind readers before the retry fix

Inventory: `rg -n '\.Kind' gchat/adapter/internal -g '*.go' -g '!**/*_test.go'`.
Lines below refer to the merged baseline. Event kinds, space kinds, reflection
kinds and the released-fixture generator are not ChangeState readers.

| Reader | Coalesced record handling before this fix |
| --- | --- |
| inbound/coalesce.go:50,62,69,83 | Only the newly recorded input act; it has not been folded or merged yet. Correct. |
| inbound/coalesce.go:95 | Copies the create kind before replacing it with coalesced. Correct. |
| ledger/ledger.go:167 | OriginalKind fallback when MemberKind is empty. Correct. |
| inbound/inbound.go:1271 | superseded rejects coalesced; incorrect for owners and folded creates. |
| inbound/inbound.go:1286 | superseded ignores an update which owns a merged turn; incorrect. |
| inbound/history.go:98,108 | Queued-history migration compares raw kinds; coalesced acts need original kinds. |
| inbound/history.go:130 | Space-history migration ignores coalesced attempts; needs original kinds. |
| inbound/history.go:145,146,158,164 | Migration ID, diagnostic kind and deletion handling need original kinds once admitted above. |
| inbound/never_silent.go:36 | History retirement's create alias must use original kind. |

## Retry contract and gc transcript limit

The dispatch ruling permits current bytes under a surviving owner's key. A
deleted owner is retired and the newest surviving member becomes the owner.
The edit or deletion follows the rewritten delivery. A folded singleton ends
withdrawn as a single delivery does.

Read-only gc inspection at `1eb9b9062d71c8e69cbc075685fc2b075dd0a293`:
`internal/extmsg/transcript_service.go:74-82` returns the existing row without
comparing or replacing text. `internal/api/handler_extmsg.go:451-499` permits
fan-out after a failed attempt and suppresses pending or delivered keys.
When gc had recorded a turn before it refused it, gc's saved transcript keeps
that first text; the adapter sends the current text and then the withdrawal;
a gc change is needed for the saved row itself.

An unset coalesce_window means 8 seconds, on. A box that sets nothing changes
behavior when its adapter updates. Setting `coalesce_window = "0s"` disables it.

## What was demonstrated

The retry class test has ten rows: merged non-owner, merged owner, folded
create, edit owning a merged turn, and catch-up; each receives a deletion or
an edit after gc refuses the frozen POST with 503. Every row fails against
the original head and passes 20 repetitions after the fix. The accepted
[POST frames](retry-frames.json) show the surviving/current turn followed by
the deletion or independent edit. A replacement edit can therefore appear
in the refreshed turn and again as the explicit edit act, as dispatch ordered.
An unchanged survivor appears exactly once. Folded singletons end withdrawn;
their independent edit/deletion is the only delivered frame.

Deleted members end unresolved with the parent's withdrawal reason. An edit
owning a turn also withdraws its earlier unsent standalone create. These short
outages produce zero operator mails, exactly like the parent: raise.go sends
a withdrawal mail only to end an earlier stuck mail. The tests originally
expected one mail per unresolved record; reading that parent rule corrected
the expectation, without changing the mail implementation.

The refreshed delivery is saved in the **same ledger save** as the withdrawing
act. `TestWithdrawalSaveSurvivesRestart` inspects that save and reloads the
actual ledger for both a deleted owner and a deleted non-owner. A surviving
owner keeps its key; a deleted owner ends withdrawn and the newest surviving
member takes ownership. `member_revision` records the effective text revision
after a refresh; released readers ignore it and use the already-saved Event.
The original proposal to re-hold every survivor under a fresh key was replaced
by the dispatch ruling before implementation.

`TestRetryTranscriptKeepsFirstText` models gc recording the first POST before
refusing fan-out. It proves that the fake transcript keeps the first text while
the retry fan-out receives the survivor followed by the withdrawal. This is a
fake of the source contract, not a claim about a real gc deployment.

The remaining fixes are shown by these tests:

| Item | Test | Original-head result | Final repeated result |
| --- | --- | --- | --- |
| 2 | TestMergedTurnCannotBeForged | Two new lookalike rows fail | 20 tests, 40 subtests pass |
| 3 | TestHistoryPreservesFoldedLiteralEditPrefix | 8s fails; 0s pins | 20 tests, 40 subtests pass |
| 4 | TestMalformedHeldRecordIsOneUnresolved | Both member positions fail | 20 tests, 40 subtests pass |
| 4 | TestDueWindowNeverRefusesAnotherSpace | Fails before recording the DM | 20 tests pass |
| 5 | TestCatchUpRetryCountsEveryAct | 70 delivered, 20 reported, budget 50 | 20 tests pass |
| 1 class | TestCoalescedKindReadersRetireOldHistory | Folded, merged and queued-update rows fail | 20 tests, 60 subtests pass |
| 1 durability | TestWithdrawalSaveSurvivesRestart | Both rows fail | 20 tests, 40 subtests pass |
| 8 pin | TestWindowOffIsToday | Pass | 20 tests pass |
| Parent pin | TestSupersededIntentNeverRepostsOldText | Pass, unchanged | 20 tests, 40 subtests pass |

The literal-prefix fix uses the edit's existing MemberOf link to a create of
the same message. That identifies a fold even after the create becomes a
merged member; an extra prefix flag would duplicate this durable fact.

The malformed-record tests assert one unresolved record and one operator mail,
delivery of the healthy room member and the DM, and continued receipt polling.
The outage test proves the DM is recorded on its first attempt and both spaces
recover once. The initial focused race run passes six top-level tests and 17
subtests; the final run passes eight top-level tests and 33 subtests, including
refreshed singleton provenance and retired history attribution.

The first catch-up fix still failed all 20 repetitions because the queue's
`settled` predicate also includes retryable intents. The final fix distinguishes
completed delivery owners from retryable ones; it passed all 20 repetitions.

## Default-window application coverage

Both existing end-to-end scenarios now run with `off` and `default` rows.
`default` leaves the setting unset and checks the 8-second default. No subtest
was removed or skipped.

- `TestEndToEnd/reaction_on_own_message_is_a_turn,_on_another's_is_not/default`
  passes 20 repetitions. The committed/turn waits include the configured
  window. Its original red-stage run passed; the review's intermittent timeout
  is addressed by allowing the window in the wait.
- `TestEndToEnd/subscription_lapsed,_catch-up_delivers_only_what_was_missed,_once,_with_the_cursor_advancing_only_past_handled_records/default`
  fails at the old four-single-turn assertion, then passes 20 repetitions with
  one prior turn plus one merged catch-up turn. Every missed act appears once.

The combined repeated invocation is
`env -u GC_BIN -u NEVER_SILENT go test ./test/e2e -run '^TestEndToEnd$/(reaction_on_own_message_is_a_turn|subscription_lapsed)' -count=20 -timeout=30m -json`:
20 top-level tests and 120 subtests pass. The off rows remain single-turn pins.

## Compatibility and window-off equality

The regular `TestCoalescedLedgerCompatibility` includes a withdrawn owner:
20 tests and 80 subtests pass after stripping fields unknown to the released
schema. The [actual-reader runner](rollback-reader.py), derived from round 1's
runner, exports current writer snapshots and overlays the actual released
source `d2f453b83a428546dfe7fa01dc2bd831c0b47ae0` and main
`205d0aecedf944792f1e3b3fbc420b9e5ad910d8`, without a checkout. The same test
runs against the current reader. Seven states cover owner deleted, non-owner
deleted, non-owner edited, no survivor, folded singleton deleted, folded
singleton edited, and a standalone create withdrawn by a merged edit owner.

Each reader uses its real loader, `/resolve` endpoint, `/status` endpoint and
inbound processor. Explicit replay of every withdrawn ID makes zero POSTs and
leaves exactly one unresolved row per ID with its original withdrawal reason.
The mixed ledger's healthy survivor and later deletion/edit still arrive;
current recovery groups the two deletion acts in the no-survivor row, while
older readers send them separately. Both deliver exactly two deletion acts.
No withdrawn text or duplicate unchanged survivor is sent. See
[results](rollback-results.json), including the six failing withdrawal states
per reader before retirement was fixed.

Writer, current reader, released reader and main reader each pass 20 repetitions:
20 top-level tests and 140 subtests per stage. The red writer included the
uncommitted raw-Kind-only prototype; the results file records that delta rather
than representing it as a clean committed source snapshot.

The durable retirement keeps the original per-message Event in MemberEvent and
clears the replayable Event. A replay operator sees
`stored turn is malformed; cannot replay` (HTTP 400); status retains the original
withdrawal reason and the operator can use `--accept`. This also holds after
rollback to 0.5.0. Window-off legacy single records retain their old bytes.

Status is exercised before and after polling in all seven reader rows. History
uses OriginalEvent, which reads MemberEvent before Event;
`TestWithdrawnRecordKeepsHistorySpeaker` proves the retired owner's original
sender survives a later history-floor migration, without retaining the old
body in that migrated history row. The notice reconciliation branch matches
the withdrawal reason and calls Forget before inspecting any delivery outcome;
it does not read Event. This last point is a source read, not a new notice test.

The first endpoint green run passed released/main but failed one current-reader
assertion because it expected two POSTs rather than two deletion acts. Correcting
that test expectation required no product change. The earlier three-state
rollback run and the intermediate raw-Kind-only fix were insufficient: a
merged edit owner still allowed the released reader to resend an old create.

The review's same 14-act script was run through this implementation with
`0s` and through parent `a553d22f3756240be77e9e0f8bc12025265c2ae2` using a Go
source overlay. Both produce 13 POSTs and identical combined POST/ledger dumps:

- Head SHA-256: `b66f9b224a75fb8ebdaf9e7605c544b4e243e515af0fdbf448710ce7732853af`
- Parent SHA-256: `b66f9b224a75fb8ebdaf9e7605c544b4e243e515af0fdbf448710ce7732853af`

The first overlay invocation had an empty source inventory because git's path
spec was relative to the module directory. It failed to build, established no
behavior, and was corrected by pinning git to the repository root and rejecting
an empty inventory. The subsequent [comparison](window-off.json) succeeded.

## Text and limits

[Operator text changes](operator-text.diff) contain every changed skill,
README and changelog string with its old and new form. No merged header text,
single-turn Render or IsFrame rule changed. The skill's rule remains true:
"Each class applies only to the text below its own from line, up to the next one."
The three clarifications are:

- "gc's line in front names only the anchor's sender; each message's own from
  line says who wrote it."
- "The message reference is on the turn's Message id line or in the `gchat:`
  item on the message's own from line; in a merged turn, use that message's
  from line, not the turn's first line."
- "The header counts acts (an edit, a deletion and a reaction each count),
  and says \"in this room\" in a DM too."

Read, not separately tested: a withdrawn tracked mention is forgotten by
ReconcileNeverSilent via the withdrawal reason. The save/reconcile runs under
the admission history lock; a replacement survivor is not newly enrolled by
that reconciliation. A surviving owner retains its tracking.

The deployed box image's gc pin and therefore its per-inbound attachment bound
were not established. No gc code was built or changed. The two Google-only
questions remain unproven: real unthreaded group-chat thread identity, and a
live held create meeting a catch-up edit. Nothing here ran against real Google.

## Fresh DEEP self-review

The review covered this round from merge `2e8bd289` through
`cd8b03a652e67161d2c444c6b5934e463d430d77`, including the four manual merge
resolutions. Verdict: BLOCK, two MAJOR findings and one MINOR. No merge-resolution
finding was added. The result remains BLOCK as recorded; fixes are not a new
review verdict. The mayor's final stacked-delta gate and second-model read are
pending.

| Finding class | Reproduction and resolution |
| --- | --- |
| MAJOR: provenance boundary after edit-prefix removal | Sixteen live/catch-up, retained/replacement-owner, withdrawal-order and restart rows fail. `TestRetriedSingletonKeepsProvenance` passes 20 repetitions (320 leaf subtests) after `18a65afa7d2faa3028818fe4e9c44abd44614c44` quotes newly exposed body headers before persisting even singleton text. FIXED. |
| MAJOR: rollback replay can bypass supersession | The actual endpoint harness fails six withdrawal states under each current/released/main reader. `2e267bc10ff09f75ba86730c4c76af737b86f596` stores original member history separately and clears replayable bytes. Deleted merged owner, folded singleton, merged-edit-owner withdrawal and the remaining class rows all refuse replay without changing the reason. FIXED. |
| MINOR: successful catch-up prefix lost from error count | `TestCatchUpReportsPartialSuccess` fails when the first oversized turn lands and the next returns 503. `18a65afa7d2faa3028818fe4e9c44abd44614c44` counts the successful transition before returning the later error; 20 repetitions pass. FIXED. |

## Validation records

[Original-head red runs](red-tests.json) and [final named checks](green-tests.json)
record exact invocations, counts and exits. Each named test ran alone with
`-count=20`; the final set is 15 names, 300 top-level passes and 860 subtest
passes. All Go invocations unset GC_BIN and NEVER_SILENT. Go: 1.27.3 darwin/arm64.
Python: 3.15.0. gofmt produced no names;
go vet, sh -n and shellcheck -x passed.

Python's first required invocation failed in the pre-existing nested-make
runner test because MAKEFLAGS propagated the requested PYTEST_DIRS override:
464 passed, 1 failed, 127 skipped, 250 subtests passed. The documented command
scrubs make variables only from the Python process and passes:
`make test PYTEST_DIRS='tests gchat/tests' PYTHON='env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTHON python3'`.
Final result: 465 passed, 127 skipped, 250 subtests passed.

Three full Go runs used
`env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json`
from `gchat/adapter`. Every run is retained in [validation.json](validation.json):

| Run | Result | Packages | Top-level tests | Subtests | Top-level skips |
| --- | --- | --- | --- | --- | --- |
| 1, before the fresh self-review fixes | PASS | 22 pass | 772 pass | 1,542 pass | 2 |
| 2, final product code | FAIL | 21 pass, 1 fail | 774 pass, 1 fail | 1,558 pass | 2 |
| 3, final product code, no other verification jobs | PASS | 22 pass | 775 pass | 1,558 pass | 2 |

Run 2's sole failure was `TestRestartWithALedgerStillDeliversTheOutage`: its
five-second readiness wait ended in `catching-up / recovering retained history`.
The unchanged test then passed 20 isolated repetitions at both the parent and
current head, and passed in full run 3. These isolated results do not establish
that the timeout occurs on the parent. No product or test change was made for
the timeout. `TestRevokedTokenReplacementResumes` passed in all three full runs
and was not changed.

The final product tree tested by runs 2 and 3 is
`48b1675eaf7bc773d3316b0d3036abd1dd95de59`. Later commits add evidence only.
The two skipped tests require a real gc binary; no real gc or Google endpoint
was used. Final gate/read verdicts remain pending.
