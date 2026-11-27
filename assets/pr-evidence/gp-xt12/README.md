# Unanswered inbound POSTs and delivery notices

Start commit `7ad24d107a8117265fd123c66eabbd666bab1863`. The tests-first commit is `888581d91663a4e5b92d3306a6db6f172e33d413`. The final implementation commit is `d0e3d60f134b67f34c2c09752ab35895d50d5bcf`. Go reported `go1.27.3 darwin/arm64`. All endpoints were local fakes, each test used temporary state, and the mail and wake commands ran against stubs.

## Reproduced behavior

An interrupted first POST, followed by deletion before recovery, used to end withdrawn with zero notices. The fake had started one fan-out in one case and zero in the other. The same failure occurred with an edit. With the interrupted-attempt guard first, each change ends unknown and sends one end notice. An hour later none is due. Four definite negative answers followed by withdrawal retain their existing behavior.

A live space inventory could also replace an unresolved unknown change with a committed history row before the first notice tick. After reload and an hour, the sweep case had zero notices and the no-sweep control had one. `TestUnansweredPostNoticeAcrossLiveFirstRead` verifies that both send one, redact the historical payload when applicable, and make one POST in all. It also exercises retirement while the first POST is still open. A late successful reply cannot overwrite the retained notice. The queued-history path uses the same preservation rule. [First retirement reproduction](retirement.log)

The two older replay exceptions remain covered by `TestUnansweredPostLegacyExceptionInterruptedReplay` and `TestUnansweredPostSecondLegacyExceptionSavedIntent`. The former needs a preceding life's end mark on a replay whose POST was interrupted. The latter needs an older handler to have saved an inherited mark before the replay's first POST. Both current-replay controls receive their notice. An already sent waiting notice naming the 24-hour bound covers an end at that bound. The control without a waiting notice receives an end notice.

## Verification

Commands use the Bash `verify` function from the base branch. Go counts include named subtests. The no-test package is counted separately.

| Check | Command or observation | Result |
| --- | --- | --- |
| A0 | `git merge-base --is-ancestor b774e4a045a3d102f06294ed7b27ba57a9209fb5 HEAD` at start | Exit 0. The four Git searches returned one inbound POST at inbound.go:1290, six resume calls in that file, one mail send at operations.go:240, and one unavailable-response reason at inbound.go:1326. |
| A1 | C-1 on the tests-first commit | Exit 1. 16 passing and 29 failing test events. 22 packages passed, three failed and one had no tests. All failures were assertions. |
| A2 | C-1 with each isolated Go overlay | Four expected failures. Removing the uptime requirement produced 14 failures. Removing the receipt sign produced seven. Retaining a sign across rebuild and using it to bypass the interrupted guard produced four. Recording the mark after a failed wake produced 20. Each overlay was outside the repository. |
| A3 / C-1 | `verify -C gchat/adapter go test -count=1 -json -timeout=30m -run 'UnansweredPost\|DeliveryWake' ./...` | Exit 0. 79 test events passed, 25 packages passed, one had no tests. |
| A4 | `git diff --quiet START HEAD --` followed by the protected paths | Original list exited 1 only for the authorized catch-up retirement caller, queued-history caller and regression test. The remaining protected paths exited 0. |
| A5 | Extract `Processor.report` through its final brace with awk from START and HEAD, then diff | Exit 0. Empty output. |
| A6 | `git diff --numstat START HEAD -- gchat/adapter`, followed by a zero-context diff of existing tests | Four old lines changed. In app/raise_test.go:31 and :80 the stub emits the successful notification result, and :49 expects the two send flags. At test/e2e/e2e_test.go:76 the stub emits that result. |
| C-2 | `verify -C gchat/adapter go vet ./...` | Exit 0. |
| C-3 | `verify -C gchat/adapter go test -p=1 -count=1 -json -timeout=30m ./...` | Exit 0. 2,887 test events passed, two skipped, zero failed. 25 packages passed and one had no tests. |
| C-4 | `verify -C gchat/adapter go test -p=1 -race -count=1 -json -timeout=45m gchat-adapter/internal/gcclient gchat-adapter/internal/inbound gchat-adapter/internal/app gchat-adapter/internal/fakes/fakegc gchat-adapter/internal/catchup` | Exit 0. 1,264 test events and all five packages passed. Zero failures or skips. |
| C-4 end-to-end fixture | `verify -C gchat/adapter go test -p=1 -race -count=1 -json -timeout=45m gchat-adapter/test/e2e` | Exit 0. All 70 test events and the package passed. Zero failures or skips. |
| C-5 | `verify -C gchat/adapter gofmt -l .` | Exit 0. Empty output. |
| C-6 | `verify python3 -m pytest gchat/tests -q -p no:cacheprovider` | Exit 0. Python 3.12.13, 54 tests and 102 subtests passed. |
| Reachable handoff | `verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'` | Exit 0. 218 passed, one skipped by the repository's command. |
| Python handoff | `verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider` | Exit 0. 3,136 tests and 11,033 subtests passed, 136 skipped, two warnings. The real gc binary was absent from PATH. |
| A8 | Read the delivery mail, replay, unknown-cause and Unreleased sections against the patch | Costs, the receipt prerequisite, live-system assumptions and the restart exception are documented. |
| A9 | Public-repository check of the diff, fixtures, commit messages, evidence, PR title and body | 27 files changed, including eight evidence files. The scan found zero flagged private paths, unrelated tickets, real identifiers or credentials. |

The [focused transcripts](focused.txt) and [four negative-control transcripts](negative-controls.txt) record package and test outcomes. The [Git checks](acceptance.txt) include the protected paths and original test edits. The [documentation additions](wording.txt) reproduce the README and changelog wording.

Earlier concurrent runs failed short waits in the application and server tests. Each affected test passed in isolation, including `TestCommandPreparationFailureSettlesExistingIntent` and the race run of `TestShutdownCheckpointsBeforeDrainingStalledRequestBody`. The first Python handoff had one shell signal test failure, which also passed in isolation. [Go failure output](suite-failures.txt) · [Command exit codes](checks.txt)

## Implementation review

The DEEP Codex review at `6ebca3666b45573bc9fe1cf27833ec664ce8dd1f` returned BLOCK with one MAJOR finding of unknown deployment likelihood. It found no additional forbidden re-post, uncovered unknown end or premature sent mark under the documented premises and exceptions. The finding concerned a child inheriting the mail command's stdout or stderr. A direct process exit or timeout could leave the notice worker indefinitely waiting for pipe EOF.

`TestDeliveryWakeInheritedOutput` reproduced the stall for both a timed-out command and a successful command that had already confirmed the wake. The fix at `d0e3d60f134b67f34c2c09752ab35895d50d5bcf` bounds output draining to one second and still checks a successful command's JSON result. Both cases passed. The nudge uses `Run` with no captured descriptors. Outcome of the sole finding: fixed. The downstream independent gate and read are separate.

## Tests and fixtures

| Fault | Named coverage |
| --- | --- |
| A second fan-out | `TestUnansweredPostOneTurn` has 13 named cases. `TestUnansweredPostHealthRequiresReceiptSign` compares the signed and unsigned paths. |
| A late delivery classified as unknown | `TestUnansweredPostRequestBounds` covers four calls. `TestUnansweredPostReadsSlowAnswer` and `TestUnansweredPostHealthDeadline` cover a late answer and the probe deadline. |
| A missing end notice | `TestUnansweredPostEnds` covers seven re-post outcomes. The deletion, edit, first-read, legacy replay and retry-bound tests cover interrupted or superseded ends. |
| Reordered delivery | `TestUnansweredPostOrder` exercises delivered, failed and pending outcomes. |
| Lost durable state | `TestUnansweredPostDurableAttempts` captures five save sequences. `TestUnansweredPostExistingLedger` loads four prior states. |
| Mail or wake failure | `TestDeliveryWakeSlow` exercises nine command outcomes. `TestDeliveryWakeFailureLog` covers the five failure classes. `TestDeliveryWakeInheritedOutput` covers a descendant after both a timeout and a successful exit. |
| Notice blocked by a POST or a receipt blocked by mail | `TestDeliveryWakeApart` and `TestDeliveryWakeReceiptWhileMailIsHeld` each hold one operation open. |
| Production wiring | `TestDeliveryWakeWire` delivers through the application with the stub executable and checks the durable end mark. |

The fake claim, receipt, health and client-departure behaviors carry an UNPROVEN comment in internal/fakes/fakegc/fakegc.go:34, the proof fixture in internal/inbound/unanswered_test.go:24, and the mail and nudge fixture in internal/app/delivery_wake_test.go:27. Whether a particular HTTP 500 came before the claim and whether 120 seconds covers a deployed slow call also require live evidence.

## Constants and emitted words

The inbound request's `inboundPostTimeout` is 120 seconds. The other client methods keep 35 seconds. `healthCheckTimeout` and `processMargin` are five seconds each. The health reply limit `maxHealthBytes` is 65,536 bytes. `mailTimeout` is 120 seconds and `wakeTimeout` is 60 seconds. The delivery workers tick every five seconds. The mock's `lostAnswer` is the existing reason `POST response unavailable; delivery unknown`.

The new reason is `re-post not confirmed (WORD); delivery unknown`. WORD is `no_route`, `failed`, `held`, `partial`, `http_N` with the numeric HTTP status, `unreachable`, or `no_answer`. The nudge is `gchat: a delivery notice is waiting in your mail`. The failed-notice log adds `cause` with `mail_timeout`, `mail_exit`, `mail_no_result`, `wake_timeout`, or `wake_exit`. The existing first-seen log also reports the count `retired_deliveries`; an owed notice stays in the ledger.

Retirement reuses the reason `interrupted POST; delivery unknown` for an interrupted intent and `receipt unavailable or unknown` for a pending receipt. An already unresolved row keeps its recorded reason when its payload is redacted.
