# gp-bnuq evidence: gchat deferred-delivery retry and raise

Base `d2f453b8` (main). Commits: `65e502f4` (the change), `b9d33951` (the self-review folds); the head adds this folder. Paths in the transcripts are scrubbed to `<lane>`, `<tmp>` and `~`. Trailing spaces are trimmed (git diff --check); nothing else is edited.

## Red first

| File | What it shows |
| --- | --- |
| `red-0-compile.txt` | The new tests against the unchanged code: they do not build (no retry fields, no member types, no `Raise`, no `Script`). |
| `red-1-behavior.txt` | With types and fields only (no behavior): every new behavior test fails, each on a behavior gap. For example, at base "held" ends unresolved after one POST, and a later message overtakes a waiting one. The ledger and wire tests pass on the fields alone; their red is `red-0`. Two of the lane's own test bugs show here and were fixed before the code (a missing comma in a JSON splice; a receipt compared with `!=`). |
| `red-2-python.txt` | The new `gc gchat status` text test against the base `status.sh`: it fails. The old-shape test passes there, because it pins today's output. |
| `red-3-rewind-pending-mail.txt` | Two cases added during the lane's own read: a clock set back must not park a retry; the mail for a pending change names the receipt deadline. |
| `red-4-self-review-r1.txt` | The self-review folds (m1 to m6, n7) seen red before the fix. |

## Green, with the commands of the spec's (10)

| Command (from `gchat/adapter`, under `env -i`) | Commit | Result |
| --- | --- | --- |
| `gofmt -l .` | b9d33951 | empty |
| `go vet ./...` | b9d33951 | rc 0 |
| `go test ./... -count=1 -timeout=30m` | b9d33951 | 20 of 21 packages ok. `internal/app` failed two tests whose 5 s health waits ran out under the parallel load (the full run includes the 9-minute e2e). |
| the same two tests alone, `-count=5` | b9d33951 | 15 of 15 PASS (quoted below) |
| `go test ./internal/app -count=2` (the package alone) | b9d33951 | ok, rc 0 (`final-app-alone-x2-b9d33951.txt`) |
| `go test ./... -count=1 -timeout=30m` | 65e502f4 | 21 of 21 ok, rc 0 (`green-1-gotest-65e502f4.txt`) |
| `go test -race ./... -count=1 -timeout=45m` | b9d33951 | 20 of 21 packages ok; `internal/app` fails. A DATA RACE inside `TestManagedSourceChatRefusalRetriesWithoutSignIn` (keyholder_test.go:176 against :180): the test's Notify hook appends to a slice after its counter flips, and the test reads it. The same flake as above also shows. |
| the same race, `-race -run TestManagedSourceChatRefusalRetriesWithoutSignIn -count=5` | **d2f453b8 (base)** | the same two DATA RACE reports at the same lines; it predates this change. Filed as papercut `pc_f1c0acd4d7e5`. |
| `staticcheck ./...` | not run | `command -v staticcheck` finds nothing; nothing installed. |
| `GC_BIN=<a real gc> go test ./test/contract -count=1 -timeout=2m` | 65e502f4 | PASS (`contract-real-gc.txt`). The gc was built at commit a8217556e, the commit the boxes run. |
| `make test` (repo root, standalone clone) | b9d33951 | 2680 passed, 116 skipped, 10685 subtests passed, rc 0 |
| base, `go test ./... -count=1` | d2f453b8 | 21 of 22 packages ok. `internal/puller` failed under load and passed 3 of 3 alone (`baseline-gotest-d2f453b8.txt`). |

The two `internal/app` tests run alone at b9d33951 (`go test ./internal/app -count=5 -run 'TestObservedIdentityReplacesContradictedLedgerIdentity|TestSubscriptionRefusalRetriesWithoutSignIn|TestManagedSourceChatRefusalRetriesWithoutSignIn' -v`; the third is the racing test, run here without `-race`):

piped through `grep -E '^(--- |ok|FAIL)' | sort | uniq -c`, as printed:

```
   1 --- PASS: TestManagedSourceChatRefusalRetriesWithoutSignIn (2.92s)
   1 --- PASS: TestManagedSourceChatRefusalRetriesWithoutSignIn (3.01s)
   1 --- PASS: TestManagedSourceChatRefusalRetriesWithoutSignIn (3.35s)
   1 --- PASS: TestManagedSourceChatRefusalRetriesWithoutSignIn (3.63s)
   1 --- PASS: TestManagedSourceChatRefusalRetriesWithoutSignIn (3.71s)
   1 --- PASS: TestObservedIdentityReplacesContradictedLedgerIdentity (2.12s)
   2 --- PASS: TestObservedIdentityReplacesContradictedLedgerIdentity (2.13s)
   2 --- PASS: TestObservedIdentityReplacesContradictedLedgerIdentity (2.14s)
   1 --- PASS: TestSubscriptionRefusalRetriesWithoutSignIn (8.23s)
   2 --- PASS: TestSubscriptionRefusalRetriesWithoutSignIn (8.24s)
   1 --- PASS: TestSubscriptionRefusalRetriesWithoutSignIn (8.25s)
   1 --- PASS: TestSubscriptionRefusalRetriesWithoutSignIn (8.26s)
   1 ok  	gchat-adapter/internal/app	68.738s
```

Go tests at the head: 618 top-level tests, 30 of them new.
- inbound: TestFailedDeliveryIsRetriedUntilItLands (a), TestRetryWaitsDoubleToTheCapAndEndAtTheBound (b), TestSpaceOrderHoldsBehindAnUnsettledMessage (c), TestAmbiguousRetryPOSTIsNeverReposted (d), TestStuckChangeIsRaisedOnce, TestFailedRaiseIsRetriedAfterAMinute, TestUnknownDeliveryStatusIsUnresolvedAndRaisedOnce, TestWithdrawnAndOldUnresolvedChangesAreNotRaised, TestAtMostFourRaisesPerPollPass (e), TestRetryAndRaiseStateSurviveRestart (f), TestConcludedPendingReceiptIsJudged (g), TestPendingReceiptDeadlineCountsFromThePOST (h), TestHeldIsRetriedLikeFailed (m), TestPendingChangeHoldsItsSpaceForABoundedTime (n), TestRetryOptionsMustBePositive, TestClockRewindDoesNotParkARetry, TestPendingRaiseNamesTheReceiptDeadline, TestNextAttemptIsClearedOnceUsed, TestPassDoesNotRaise, TestRaiseBodyWording, TestBoundWithoutAnAnswerIsUnknown.
- ledger: TestLedger050LoadsAndSavesUnchanged, TestChangeRetryFieldsRoundTripAndAreAbsentWhenZero (j).
- server: TestStatusDeliveryRowsShowAgeCauseAndStuck, TestReplayClearsRetryStateAndKeepsRaised (k, 7).
- app: TestDeliveryRaiseMailsTheIdentityAgentWithPerChangeDedup, TestDeliveryRaiseUsesTheNotifyHookAndReportsItsFailure (l).
- wire, gcclient, fakegc: TestDeliveryMembersJSON, TestInboundAndReceiptCarryMembers, TestScriptAnswersInOrderBeforeNextDelivery.
- Python (`gchat/tests/test_commands.py`): test_status_text_without_retry_keys_prints_as_before, test_status_text_shows_age_cause_and_next_attempt (k).

(i): the "partial" arm of TestRetriesExhaustedAndPendingFailureRetries is unchanged; its "failed" arm became (a). The existing tests that changed are listed, with the reason for each, in 65e502f4's message.

The 0.5.0 ledger fixture `gchat/adapter/internal/ledger/testdata/ledger-0.5.0.json` was written by 0.5.0's own `Save` at d2f453b8 (generator beside it). Its sha256 is in `fixture-0.5.0.sha256`.

## Samples (the real processor, fake gc and fake Google, the real status handler and mail sender)

`evidence_sample_test.go.txt` is the generator, run from a scratch clone at b9d33951 and not part of the PR. It records one message that gc refuses four times as draft-deferred, then once more, and one message that gc answers with an unknown status.
- `sample-status.json`: the `/status` answer, with one waiting row and one unresolved row.
- `sample-status.txt`: `status.sh`'s own jq program applied to that JSON.
- `sample-mail-argv.txt`: the exact argv of both `gc mail send` calls (subject `gchat: delivery`, per-change dedup key, agent `mayor`). Neither the message text nor the sender appears in any sample.

## Review

- Self-review (a fresh Opus subagent on the builder's model, `self-review-r1.md`): BLOCKER 0, MAJOR 2, MINOR 7, NIT 11. Folded in b9d33951: m1 to m6, n1, n2, n3, n7. Not built, as rulings for the mayor: M1 and M2 (both follow the spec as written; a fix changes the design or touches catch-up), m7, n5, n6, n8, n9, n10, n11.
- Codex: no lane round. The seats are at their weekly limit until 20:51Z Wed 2026-11-11.
