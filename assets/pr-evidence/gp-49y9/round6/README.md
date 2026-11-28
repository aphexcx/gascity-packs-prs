# Round 6 — hold a merged retry behind an edit checkpoint

Start commit `db4be95184fb4dfc4ad8dbe6e7dcac1563f8f111`. Tested code and documentation commit `c1fb89de0c9b7ff12c0d4dae2519837b3f3e69fc`.

After gc refused a merged turn, a member's edit checkpoint could advance past its recorded message state. Retrying then rebuilt the turn from the older recorded edit. The rebuild left the checkpoint ahead, so the pass kept writing and other rooms waited.

The retry now leaves the frozen turn intact and proceeds to the next intent. Once message state reaches the checkpoint, the next pass rebuilds the turn from the current edit and posts it once. If there is no recorded edit, the existing behavior ends the turn as `stored turn is malformed`, sends one operator end notice, and makes no further post.

### Acceptance tests

All three tests live in `gchat/adapter/internal/inbound/coalesce_checkpoint_test.go`. The baseline run used a scratch export of the start commit with just that test file added. The fake store rejects the first attempted rewrite of held records.

| Test | Start | Tested commit |
| --- | --- | --- |
| `TestMergedCheckpointHoldLetsAnotherRoomPost` | Failed on an attempted rewrite | Passed. The other room posted in the same pass, within two saves for its attempt and outcome. |
| `TestMergedCheckpointWithoutRecordedEditRaisesEndNotice` | Passed | Passed. Two polls produced one end notice and zero additional posts. |
| `TestMergedCheckpointReleasesCurrentTurnOnce` | Failed on an attempted rewrite | Passed. The held pass needed zero saves. Two passes after catch-up produced one post with the current text. |

The focused command below returned 1 at the start and 0 at the tested commit. [Baseline output](red.txt) · [Fixed output](green.txt)

```sh
env -u GC_BIN -u NEVER_SILENT go test ./internal/inbound -run '^TestMergedCheckpoint(WithoutRecordedEditRaisesEndNotice|HoldLetsAnotherRoomPost|ReleasesCurrentTurnOnce)$' -p=1 -count=1 -timeout=2m -v
```

### Verification

Go 1.27.3, Python 3.15.0 and Node 24.21.0. Suites ran serially against fakes. Go commands ran in `gchat/adapter`. Make ran at the repository root using the same Python environment isolation as round 5.

```sh
gofmt -l .
env -u GC_BIN -u NEVER_SILENT go vet ./...
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/inbound ./internal/catchup ./internal/config ./internal/ledger ./internal/server -p=1 -count=1 -timeout=30m
env -u GC_BIN -u NEVER_SILENT go test ./internal/app ./internal/inbound -run '^(TestSupersededHoldsARefreshedMemberBehindAnEditCheckpoint|TestUnansweredMergedTurnIsClassifiedBeforeAWithdrawal|TestRefusedMergedTurnStillRefreshesOnAWithdrawal|TestUnansweredMergedTurnKeepsItsNoticeAcrossHistoryRetirement|TestUnsentMergedTurnIsRetiredWholeByTheHistoryBound|TestRetiredMergedDeletionPutsTheDeletedMarkBack|TestNewKeepsTheUnansweredMemoryAndRecoversHeldActs|TestApplicationPostsAHeldTurnAtItsDeadline)$' -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT make test PYTEST_DIRS='tests gchat/tests' 'PYTHON=env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTHON python3'
```

| Check | Result | Exit |
| --- | --- | --- |
| gofmt and vet | Both produced empty output | 0 each |
| Full Go | 25 packages passed, one skipped. 983 top-level passes, 49,482 subtest passes. `TestRealGCContract` and `TestRealGCReplyReminder` skipped as expected. Round 5 had 980 top-level passes and the same subtest and skip counts. | 0 |
| Round 5 checkpoint order and seven named regressions | Eight top-level passes and 11 subtest passes in two packages | 0 |
| First race run | Four packages passed. Two credential-lookup rows in inbound missed their 100 ms deadline. | 1 |
| Full race rerun | All five packages passed. | 0 |
| First Make run | Python passed 600 tests and 263 subtests, with 128 skips, matching round 5. Reachable passed 218, failed one deadline case and skipped one. | 2 |
| Second and third Make runs | Python had the same counts. Reachable passed 218, skipped one and timed out in the archive replay case on both runs. | 2 each |

### Isolated deadline checks

Each pair ran first at the tested commit, then at the start commit. Cells give the exit code and the one-, five- and fifteen-minute load averages from `uptime` immediately before that run. The Go rows ran from the adapter with the race detector. The Node rows ran from `reachable/test`. Both Reachable deadlines also failed on the start commit. The archive child timed out once on each revision.

| Case | Pair | Tested commit | Start commit |
| --- | --- | --- | --- |
| message_space/302 | 1 | 0 · 31.70 / 18.09 / 22.12 | 0 · 31.70 / 18.09 / 22.12 |
| message_space/302 | 2 | 0 · 33.24 / 18.86 / 22.34 | 0 · 33.24 / 18.86 / 22.34 |
| message_space/302 | 3 | 0 · 32.58 / 18.96 / 22.36 | 0 · 32.54 / 19.18 / 22.41 |
| reaction_parent/401 | 1 | 0 · 32.54 / 19.18 / 22.41 | 0 · 30.89 / 19.06 / 22.35 |
| reaction_parent/401 | 2 | 0 · 30.89 / 19.06 / 22.35 | 0 · 31.30 / 19.34 / 22.43 |
| reaction_parent/401 | 3 | 0 · 31.30 / 19.34 / 22.43 | 0 · 33.76 / 20.05 / 22.66 |
| Reachable outer deadline | 1 | 0 · 13.74 / 17.03 / 21.00 | 1 · 13.92 / 17.01 / 20.97 |
| Reachable outer deadline | 2 | 0 · 13.92 / 17.01 / 20.97 | 0 · 13.92 / 17.01 / 20.97 |
| Reachable outer deadline | 3 | 0 · 13.53 / 16.88 / 20.90 | 0 · 13.53 / 16.88 / 20.90 |
| Reachable archive replay | 1 | 1 · 34.63 / 28.76 / 24.23 | 0 · 40.03 / 30.09 / 24.75 |
| Reachable archive replay | 2 | 0 · 39.82 / 30.40 / 24.92 | 1 · 35.47 / 29.77 / 24.76 |
| Reachable archive replay | 3 | 0 · 37.52 / 30.42 / 25.05 | 0 · 39.64 / 31.26 / 25.44 |

```sh
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/inbound -run '^TestLookupHoldCarriesCredentialKind$/^message_space$/^302$' -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/inbound -run '^TestLookupHoldCarriesCredentialKind$/^reaction_parent$/^401$' -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT node --test --test-reporter=tap --test-name-pattern='^review 7: installed outer deadline leaves time to persist fail-open rollback$' review.test.cjs
env -u GC_BIN -u NEVER_SILENT node --test --test-reporter=tap --test-name-pattern='^review 4: archive replay advances in bounded chunks without dropping rows$' review.test.cjs
```

The DEEP self-review covered the diff between the two commits above. Verdict CLEAN, with zero BLOCKER, MAJOR, MINOR and NIT findings.
