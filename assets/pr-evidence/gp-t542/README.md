# gp-t542 evidence: PR 88 round 3 (gchat deferred delivery)

Round 3 of PR 88 (bead gp-bnuq) answers the mayor's codex gate r2 at `136dbae1`. The gate's verdict was BLOCK, on two MAJORs found by static tracing with no tests run.

- **Base of the round:** `136dbae1`, the PR head before it.
- **PR base:** main `d2f453b8`. Main has since moved to `4bf236c2` (PR 87, files under `whatsapp/` only). It was not merged in; `git merge-tree` shows the branch merges into it cleanly.
- **Commits:**
  - `c1ae854f`: the two items.
  - `b217a722`: the folds from self-review r1.
  - `0af1ac61`: the folds from self-review r2.
  - The head adds this folder.

In the transcripts, paths are scrubbed to `<lane>`, `<tmp>` and `~`. Nothing else is edited.

## Red first

| File | Item | What it shows |
| --- | --- | --- |
| `red-0-first-attempt-136dbae1.txt` | M1 | The first red run, kept for the record. The two "never a fragment" guard rows failed only because the test scripted 40 later answers: after them the fake's default answer was `delivered`. The test now scripts 400. |
| `red-1-m1-m2-136dbae1.txt` | M1, M2 | Run on a clone at `136dbae1` with the new `round3_test.go` copied in, minus the one line that clears a field `136dbae1` lacks. M1: each fragment row made 124 POSTs in two hours, wanting 4, and the message behind the fragment head waited (`intent behind_earlier`). A replay could not even start from an exhausted record, because the first life never ended. The "never a fragment" rows pass, as they should. M2: the replayed record carried `raised` and `raised_end` stamped by the first life's mail. The same-life guard passes. |
| `red-2-ledger-server-compile-136dbae1.txt` | M1 | The extended ledger and replay tests do not compile at `136dbae1`: there is no `Fragment`. |
| `red-3-dedup-key-c1ae854f.txt`, `red-3-shim_test.go.txt` | r1 MAJOR | Run on a clone at `c1ae854f` with the fold's tests and a shim: `DedupKey()` returns the key the app sent at `c1ae854f`, one per change plus `:ended`. With gc's dedup modelled, the replay's end mail was dropped (2 mails at gc, wanting 3). The replay's own waiting mail was also dropped (1 waiting mail for the two lives, wanting 2). The key test itself was stripped there, because `Raise.Recorded` is new. |
| `red-4-r2-folds-b217a722.txt` | r2 MINOR-2, NIT-3, NIT-5 | Run on a clone at `b217a722` with the r2 tests. The replay's end mail does not say the message was replayed, and the exhausted cause line does not gloss `draft_deferred`. The key-within-a-life test passes there, because the code was right. Under the mutation that makes the key change on every send (`Recorded: now` in `report`), it fails. |

## Green

| Command (from `gchat/adapter`, in a clean clone at the commit, under `env -i`) | Commit | Result |
| --- | --- | --- |
| `gofmt -l .` and `go vet ./...` | 0af1ac61 | empty and rc 0 (`final-fmt-vet-0af1ac61.txt`) |
| `go test ./... -count=1 -timeout=30m` | 0af1ac61 | 20 of 21 packages ok; e2e took 621 s (`final-gotest-0af1ac61.txt`). `internal/app` `TestSubscriptionRefusalRetriesWithoutSignIn/404` timed out a 5 s health wait; see the flakes below. |
| `go test -race ./internal/inbound ./internal/ledger ./internal/server -count=1` | 0af1ac61 | ok, rc 0 (`final-race-round-packages-0af1ac61.txt`). `internal/app` under `-race` fails on the base (the keyholder race, papercut `pc_f1c0acd4d7e5`), so it is not in this run. |
| `make test` (repo root) | 0af1ac61 | 2680 passed, 116 skipped, 10685 subtests passed, rc 0 (`final-make-test-0af1ac61.txt`) |
| the round's new and changed tests by name, `-v` | 0af1ac61 | 14 of 14 PASS, 21 with subtests (`final-new-tests-0af1ac61.txt`) |
| the same suites | c1ae854f | `suites-c1ae854f-*.txt`: 20 of 21 ok. `internal/app` `TestRevokedTokenReplacementResumes` timed out a 5 s health wait while the r1 reviewer ran tests beside the suite. `make test` 2680 passed. |
| the same suites | b217a722 | `suites-b217a722-*.txt`: 19 of 21 ok. `internal/app` `TestCorruptLedgerServesStatusAndResetResumes` timed out a 1 s health wait, and `internal/puller` `TestRunQuarantineRecoversDuringRun` timed out a condition wait (a load flake recorded at the base in round 1). Both happened while the r2 reviewer ran tests beside the suite. `make test` 2680 passed. |
| `green-1-round-tests-c1ae854f.txt` | c1ae854f | The round's M1/M2 tests and their neighbours, 24 of 24 PASS. |

**The flakes, measured.** `flake-ab-0af1ac61.txt` holds an interleaved A/B, one test run at a time, of each test that failed in a suite run this round, at `0af1ac61` against a clean clone at `136dbae1`. Each test ran six rounds, head then base. `TestRevokedTokenReplacementResumes`, `TestCorruptLedgerServesStatusAndResetResumes`, `TestSubscriptionRefusalRetriesWithoutSignIn` and `TestRunQuarantineRecoversDuringRun` each passed 6 of 6 at the head and 6 of 6 at the base. Each failed only in a full parallel run, where its short health or condition wait timed out; round 1 recorded the same family at its base. The round's only change under `internal/app` is the mail key in `raiseDelivery` (and its test); `internal/puller` is untouched.

**Go tests.** There are 645 top-level tests at the head and 637 at `136dbae1`, counted with `git grep '^func Test…(t *testing.T)'`. The difference is 8 new tests and 1 renamed.

New tests:
- M1:
  - `TestFragmentOnAnyAnswerKeepsTheFourPOSTBound`, a table:
    - partial then three failures;
    - the same with a restart after the first answer;
    - failure, partial, two failures;
    - held with a partial member then three failures, with and without a restart;
    - never a fragment, with and without a restart.
  - `TestFragmentMessageDoesNotHoldItsSpace`.
  - `TestReplayClearsTheFragmentMark`.
- M2:
  - `TestRaiseCompletingAcrossAReplayStampsNothing`: an end mail in flight across a replay, through gc's dedup, up to the replay's own end mail.
  - `TestRaiseCompletingAcrossAReplayOfAWaitingMessage`: a waiting mail in flight across a replay (r1 probe A).
  - `TestRaiseCompletingAcrossAnEndInTheSameLifeIsMarked`: the same-life guard.
  - `TestDedupKeyIsPerLifeAndKind`.
  - `TestLostMailKeepsItsKeyWithinALifeAcrossARestart` (r2 MINOR-2).

Changed tests:
- `TestChangeRetryFieldsRoundTripAndAreAbsentWhenZero` covers `fragment`.
- `TestReplayClearsRetryStateAndKeepsRaised` checks that a replay clears `fragment`.
- App tests:
  - `TestDeliveryRaiseMailsTheIdentityAgentWithPerLifeDedup` was renamed from `…PerChangeDedup`.
  - `TestEndedDeliveryMailHasItsOwnDedupKey`: the keys are per life.
- `gcMail`, the gc dedup fake in `round2_test.go`, now keys by `Raise.DedupKey()`.
- `TestReplayedHeadDoesNotSilenceTheMessagesBehindIt` checks that the replay's end mail says it was replayed.

## Self-review

Each review was a fresh reviewer on the builder's model, with probes kept only in scratch clones.

- **`self-review-r1.md` at `c1ae854f`:** BLOCKER 0, MAJOR 1, MINOR 2, NIT 3. Its probes are in `self-review-r1-probes*_test.go.txt`. All folded in `b217a722` except one NIT, left: the inbound tests' replay helper mirrors `server.go`, and the server half is `TestReplayClearsRetryStateAndKeepsRaised`.
- **`self-review-r2.md` at `b217a722`:** BLOCKER 0, MAJOR 0, MINOR 2, NIT 3. Its probes are in `self-review-r2-probes_test.go.txt`. All folded in `0af1ac61`.
  - The r2 reviewer notes one slip of its own: a probe file sat briefly in `/tmp` during a mutation run. It is test code only.
