# Held inbound retry verification

The starting commit was `baa1898f01608e6ad7d312475ace5bb35782f562`. The red test commit was `d917b44aa1da0475e2530fb91897811f63b0d237`. The final implementation was `e7dd47c586766aff05ad3b0f0c6b6b370b28c302`. Each transcript preserves every test outcome and assertion failure from the Go JSON stream. Counts include subtests and their parent tests.

## Verification

| Check | Command | Result |
| --- | --- | --- |
| A1 and A3 | `verify -C slack-full/adapter go test -count=1 -json -run 'HeldClock' .` | Red on assertions, 33 passed and 23 failed, exit 1. Green after review, 61 passed, exit 0. Zero skips. |
| C-2 | `verify -C slack-full/adapter go vet ./...` | Exit 0. |
| C-3 | `verify -C slack-full/adapter go test -count=1 -json -run 'InboundRecovery\|Round2\|Notice\|NeverSilentHold\|IngestionMail\|HeldAnswer\|HeldClock' .` | 319 passed at the start, 380 passed after review, zero failures and skips, exit 0. |
| C-4 | `verify -C slack-full/adapter go test -race -count=1 -json -run 'HeldClock' .` | 61 passed, zero failures and skips, exit 0. |
| C-5 | `verify -C slack-full/adapter gofmt -l .` | Empty output, exit 0. |
| C-6 Reachable | `verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'` | 218 passed, one skipped by the repository's name filter, zero failures, exit 0. Node v24.21.0. |
| C-6 Python | `verify python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q -p no:cacheprovider` | Python 3.12.13, 3,122 passed, 132 skipped, 10,932 subtests passed, two warnings, zero failures, exit 0. |

The red run and original recovery baseline used `go version go1.27.3 darwin/arm64`. The final C-1 through C-5 and all ten final mutation runs used `go version go1.26.10 darwin/arm64`. Go 1.27.3 reformats an unchanged multi-value return in `company_thread_ambient.go`. Go 1.26.10 accepts the module's existing formatting.

## Test cases

| Test | Rows and observations |
| --- | --- |
| `TestHeldClockAnswer` | 22 combinations of transport status, parsed summary and members, counts, error and gate. |
| `TestHeldClockPOSTStatus` | Two paths, channel and alias. Both accept identical bodies at HTTP 200 and 300. Only the retained status changes after accounting for the alias dispatch timestamp. |
| `TestHeldClockGap` | Nine readiness times from 30 seconds through 30 hours. Seven timing rows failed on the baseline. |
| `TestHeldClockOneTurn` | 13 scripted outcomes, including a pending poll, restart before and after a POST, an acknowledgement during the POST, and simultaneous triggers. |
| `TestHeldClockOrder` | One sequence with three messages in two sessions, including a younger line that reaches its own bound. |
| `TestHeldClockRetirementOrder` | Two replay-age settings. After a failed history write recovers, the younger line gets its final POST when replay is disabled and waits when the older line can replay. |
| `TestHeldClockRow` | One sequence through retirement, continued offers and the replay completion record. |
| `TestHeldClockState` | Four rows covering the durable mark, failed saves before and at the bound, backward decoding and history keys. |
| `checkNoLoss` | A shared cleanup advances the clock 25 hours and checks every admitted line and its history for each queue sequence. |
| `TestHeldClockCadence` | One three-hour held sequence. Six offers at the start and 181 after the change. |

The state field is `LastHeldAnswerAt` with JSON key `last_held_answer_at`. The constant `heldOfferInterval` is 60 seconds. `deliveryReceipt.heldAnswer` checks the receipt and the existing `postStatus` field carries the final HTTP code. The fake held receipt and fake HTTP responses carry the UNPROVEN P3/P4 and P3 annotations at `held_clock_test.go` lines 21 and 84. The cadence fixture marks UNPROVEN P5/P7 at line 270.

## A2 mutation results

Each run applied one disposable Go source overlay to the tested implementation and used C-1 with `-overlay`. Each exited 1 on assertions. The overlay never changed the tracked source. The corresponding `red-*.txt` files contain the outcomes.

| Changed behavior | Test that detected it |
| --- | --- |
| Offer an unmarked line one minute after its last attempt. | All five ordinary-schedule rows, covering no answer, no route, failed, a partial member and short delivery. |
| Discard pending receipt ownership before the clock offers again. | Both pending-conclusion rows. |
| Offer a settled or acknowledged entry when a minute has elapsed. | Held-then-delivered and acknowledgement-during-offer rows. |
| Persist the cleared mark only after the POST. | Restart-before-POST and saved-state rows. |
| Ignore the failed pre-POST save. | Both unwritable-state rows. |
| Let a gate-off receipt set the mark. | Gate-off classifier and queue rows. |
| Send twice when a vouch and the clock agree. | Simultaneous-trigger row. |
| Ignore the automatic replay age at selection and send. | 30-hour gap row. |
| Give different sessions the same ordering key. | The independent session in the ordering sequence. |
| Remove a replayed entry before appending completion. | The 25-hour inventory found the missing entry. |

## Scope and compatibility

A0 passed. The specified base is an ancestor of the start. The sole `r.send(e)` and `r.attempt(` sites were `inbound_recovery.go` lines 1837 and 1292. The original eligibility expression occurred once. No test began with `TestHeldClock`.

A4 and A6 were checked with `git diff --name-only`, `git diff --numstat` and a zero-context diff against the starting commit. The product paths were the two pack documents and four adapter files, including two new files. The diff has zero hunks touching the original ladder at lines 27–38 and zero edits to existing tests.

A5 used `git grep -cF` on the five required format strings in `inbound_recovery.go`. Every string occurred once at the start and once at the implementation commit, including the attempt, settled, LOSS and replay-trigger formats.

The recorded command outputs appear in [scope.txt](scope.txt). The exact README and changelog additions appear in [documentation.txt](documentation.txt).

The held clock declines an entry carrying any pending receipt ID, even with a missing pending flag. It treats an unknown nonempty member status as ineligible. It gives a failed pre-POST save at the bound the existing retirement reason and writes its history row. The existing startup quarantine for an unreadable room history remains at `inbound_recovery.go` lines 69, 824–825 and 952. It belongs to the separate history alarm work.

## Review

The first DEEP review at `e689813bfe3e21f0e71fa4a4343019ebfb49afe5` returned CLEAN and identified one minor ordering error. A retried history append could succeed while the FIFO decision still used its pre-retirement state. The replay-off regression failed before the repair. Commit `e7dd47c586766aff05ad3b0f0c6b6b370b28c302` reads the stored entry at every retirement that can retain a mark. The second DEEP review returned CLEAN with zero findings. It confirmed both pending indicators exclude clock eligibility and found no new path out of the queue without a paste or a history record.
