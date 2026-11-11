# gp-dl26 evidence: PR 88 round 2 (gchat deferred delivery)

Round 2 of PR 88 (bead gp-bnuq), answering the mayor's Fable read r1 at `d4a95db9`. Base of the round: `d4a95db9` (the PR head before it); PR base: main `d2f453b8`, which did not move. Commits: `766cdc40` (the seven items), `b3e248c0` (the self-review folds); the head adds this folder. Paths in the transcripts are scrubbed to `<lane>` and `<tmp>`. Nothing else is edited.

## Red first

Each item's tests were run before its code, in the lane, unless the line says otherwise.

| File | Item | What it shows |
| --- | --- | --- |
| `red-1-catchup-and-health.txt` | F1, ruling A | Run on a clone at `d4a95db9` with only the new tests copied in. Catch-up: `Pass = (0, 2, nil)`: two retrying or held-back messages keep `remaining` at 2. The pending and gc-down rows pass, as they should (they still hold). App: health stays `catching-up` for 10 s with a retrying message. |
| `red-2-pending-block.txt` | F2, ruling B | The read's burst: POSTs at +0s, +35s, +1m10s, +1m45s and +2m20s, and 2 mails. A message behind a pending one waited. |
| `red-3-5-crash-window-and-held-partial.txt` | F4 ruling C, F5 ruling F | The 0.5.0 crash-window record from the 0.5.0 fixture was POSTed. "held" with a partial member: 25 POSTs in 20 minutes, still retrying. The walk test (`TestNoSaveWritesTheCrashWindowShape`) passes before and after: it guards that this code never writes that shape. |
| `red-4-6-7-mails.txt` | F3 ruling D, F6 ruling E, F7 | One head and three behind: 4 mails. A waiting message that ended early: no second mail (two rows; the row "after the second mail" cannot fail before the feature exists). The waiting mail lacks the "still delivered" words. |
| `red-6-dedup-key.txt` | F6 | Run on a clone at `766cdc40` with `operations.go` put back to `d4a95db9`: the second mail's dedup key equals the first's. |
| `red-7-self-review-folds.txt` | self-review r1 | The fold tests, before the fold code: the replayed head silences the message behind it (0 mails), the end mail is dropped as a duplicate of a lost waiting mail, a withdrawn waiting message gets no end mail, a released head's mail says it waits behind an earlier message, the end mail claims "will not arrive", and a first mail as ended is not flagged as an end mail. |

## Green

| Command (from `gchat/adapter`, under `env -i`) | Commit | Result |
| --- | --- | --- |
| `gofmt -l .` | b3e248c0 | empty (`final-fmt-vet-b3e248c0.txt`) |
| `go vet ./...` | b3e248c0 | rc 0 |
| `go test ./... -count=1 -timeout=30m` | b3e248c0 | 21 of 21 packages ok, rc 0 (`final-gotest-b3e248c0.txt`); e2e 576 s |
| `go test -race ./internal/inbound ./internal/catchup ./internal/ledger -count=1` | b3e248c0 | ok, rc 0 (`final-race-round-packages-b3e248c0.txt`). The whole-tree race run was not repeated: its known failure is the base's keyholder race (papercut `pc_f1c0acd4d7e5`), in a package this round does not touch for races. |
| the round's 19 changed and new tests by name, `-v` | b3e248c0 | 19 of 19 PASS (`final-new-tests-b3e248c0.txt`) |
| `make test` (repo root, standalone clone) | b3e248c0 | 2680 passed, 116 skipped, 10685 subtests passed, rc 0 (`final-make-test-b3e248c0.txt`) |
| `go test ./... -count=1 -timeout=30m` | 766cdc40 (the working tree just before that commit) | 20 of 21 ok; `internal/app` `TestMissingTokenReplacementResumes` timed out its 5 s health wait (see the app flake below); e2e ok in 767 s (`green-1-gotest-766cdc40-working-tree.txt`) |
| `staticcheck ./...` | not run | not installed |
| the real-gc contract suite | not run | not in this round's step list; round 1 ran it at `65e502f4` |

Go tests: 638 top-level at the head, 623 at `d4a95db9`: 16 new, 1 replaced (`TestPendingChangeHoldsItsSpaceForABoundedTime`).

New tests, by item:
- F1: `TestHandledItemSettlesWhenThePollOwnsItsChange` (catchup, a table: failed, held, pending, gc down), `TestRetryingMessageLeavesHealthReadyAndOutboundRecoveryRunning` (app).
- F2: `TestBusyAgentBurstFlowsInArrivalOrder`, `TestPendingChangeDoesNotHoldItsSpace` (replaces `TestPendingChangeHoldsItsSpaceForABoundedTime`; records the accepted A, B, A). `TestSpaceOrderHoldsBehindAnUnsettledMessage` loses its pending arm.
- F4: `TestLedger050CrashWindowRecordIsNeverPosted`, `TestNoSaveWritesTheCrashWindowShape`; `TestLedger050LoadsAndSavesUnchanged` checks the new fixture record.
- F5: `TestHeldWithAPartialMemberKeepsThePartialBound`.
- F3: `TestStuckHeadIsMailedOnceNamingTheMessagesBehindIt`.
- F6: `TestWaitingMessageThatEndsEarlyGetsOneSecondMail` (no restart, a restart before the second mail, a restart after it), `TestEndedDeliveryMailHasItsOwnDedupKey` (app), `TestMessageFirstMailedAsEndedIsNotMailedAgain`; `TestChangeRetryFieldsRoundTripAndAreAbsentWhenZero` covers `raised_end`.
- F7: `TestWaitingMailSaysTheMessageStillArrives` (intent and pending).
- Self-review folds: `TestReplayedHeadDoesNotSilenceTheMessagesBehindIt`, `TestEndMailIsNotShadowedByALostWaitingMail`, `TestWithdrawnAfterAWaitingMailGetsItsEndMail`, `TestHeadIsTheLiveOrderTestNotTheCauseWord`.

The 0.5.0 ledger fixture gains the crash-window record. It was regenerated with the updated generator on a checkout of `d2f453b8`, so with 0.5.0's own `Save`, and again with this branch's `Save`: both give the same sha256 (`fixture-0.5.0.sha256`). The self-review regenerated it a third time and got the same.

### The app flake

`internal/app` `TestMissingTokenReplacementResumes` waits 5 s for health. It failed once in the full parallel run at `766cdc40` (`green-1-gotest-766cdc40-working-tree.txt`) and once in five runs alone while the self-review was running the suite beside it (`flake-app-missing-token-x5.txt`). It has no messages, so catch-up has no items and nothing in this round reaches it. Interleaved runs at `d4a95db9` and the head, one process each: 12 of 12 pass on both sides (`flake-app-missing-token-ab.txt`). Two `-count=5` processes per side: 10 of 10 on both (`flake-app-missing-token-count5.txt`). It is the same class as round 1's two 5-second health-wait flakes.

## Samples (the real processor, fake gc and fake Google, the app's real mail sender)

`evidence_sample_test.go.txt` is the generator, run from a scratch clone at `b3e248c0` and not part of the PR. `sample-mail-argv.txt` holds the exact argv of every `gc mail send` call:
- `spaces/AAAA`: a head that gc keeps refusing, three messages behind it: one mail, for the head, "Waiting behind it: 3 later messages of this space".
- `spaces/CCCC`: the 0.5.0 crash-window record: never POSTed, one mail, cause `unknown` "(an older version of the adapter stopped after four POSTs and did not record gc's last answer)", dedup key ending `:ended`.
- `spaces/BBBB`: a message mailed while waiting, then its receipt concluded pending without members: a second mail with its own key (`…:ended`) that says the adapter will not deliver it again on its own.
Neither the message text nor the sender appears in any sample.

## Review

- Self-review (a fresh Opus subagent on the builder's model, `self-review-r1.md`, at `766cdc40`): BLOCKER 0, MAJOR 1, MINOR 4, NIT 4. It found no duplicate-POST path and no customer text. Eight of nine mutants of the round's rules were killed by the lane's tests; the ninth (head by the stored cause word) is now killed by `TestHeadIsTheLiveOrderTestNotTheCauseWord`. Its probes: `self-review-r1-probes_test.go.txt`, their output at `766cdc40` in `self-review-r1-probes-766cdc40.txt`.
- Folded in `b3e248c0`: MAJOR-1, MINOR-1, MINOR-3, MINOR-4, NIT-1, NIT-2, NIT-3.
- Not built: MINOR-2 (probe P1: when a stuck head lands into a busy agent, each of the three messages behind it that gc answers "pending" is mailed at once, 4 mails in all). The fix counts a pending change's age from its POST, which changes round 1's rule "StuckAfter since it was recorded"; it is a ruling for the mayor. NIT-4 (status does not show `raised_end`) is not in the spec.
- Codex: no lane round. The seats are at their weekly limit until 20:51Z Wed 2026-11-11.
