# Sender attribution after the merge from main

Git joined the reviewed head `3d32e2d8d98f85b20859fde5d12b3511cca87b12` and main `e0b06b76d1d87493ae632651f5b5c24898c99ff2` without conflicts. The merge retains the reviewed change across all 19 files. The full Go suite passed. The nine-package race run exited 1. The three affected tests passed in isolation as detailed below.

## Merge and patch identity

D1 passed. `git rev-list --parents -n 1` at the merge printed the merge, reviewed head and main in that order.

```text
8eb392d134e6ea9a811803b769c1f189be54bcc9 3d32e2d8d98f85b20859fde5d12b3511cca87b12 e0b06b76d1d87493ae632651f5b5c24898c99ff2
```

D2 passed. `git merge-tree --write-tree --name-only` with main and the reviewed head exited 0. Its complete output follows. `git show -s --format=%T` on the merge printed the same tree. `git show --cc` showed no combined diff.

```text
47609fd5676bb53a0ad8d170f16b9fd9067a7555
```

D3 passed. The stable patch ID comparison excludes evidence. The first pair runs from `5f1ce902fce977f6b6d95da231652b0f753cce37` to the reviewed head. The second runs from main to the merge.

```text
before files: 19 after files: 19
only before: []
only after: []
WHOLE before: 2e8cd79948b62f85e6cd8d0a1c44e6febcd5cdb5
WHOLE after : 2e8cd79948b62f85e6cd8d0a1c44e6febcd5cdb5
SAME gchat/CHANGELOG.md 313c43fc1419aa58e1b6bb3956e33541486d4541 313c43fc1419aa58e1b6bb3956e33541486d4541
SAME gchat/README.md 19170df4fdd7bafcc61df20bfd677f71f08b9699 19170df4fdd7bafcc61df20bfd677f71f08b9699
SAME gchat/adapter/internal/app/app.go 686bb37a15aaff78fc470978e19d2430b4229716 686bb37a15aaff78fc470978e19d2430b4229716
SAME gchat/adapter/internal/app/sender_credential_test.go 627b16db46cf58d31c74eb2309fd41364fe2731e 627b16db46cf58d31c74eb2309fd41364fe2731e
SAME gchat/adapter/internal/app/sender_fill_test.go 29f30eb69b83d2f4186d09158ca660696d473135 29f30eb69b83d2f4186d09158ca660696d473135
SAME gchat/adapter/internal/app/workers.go 63769deca8a709f9e26f64463a33a27a65e0754e 63769deca8a709f9e26f64463a33a27a65e0754e
SAME gchat/adapter/internal/chat/chat.go 99eca27528e02575cd403c504f999d13fb007ab6 99eca27528e02575cd403c504f999d13fb007ab6
SAME gchat/adapter/internal/inbound/history.go 8cea10bc4ec577417dea646d71a2e172fc654a90 8cea10bc4ec577417dea646d71a2e172fc654a90
SAME gchat/adapter/internal/inbound/inbound.go 3f649f78359c03cc519e473b26df381d172cfead 3f649f78359c03cc519e473b26df381d172cfead
SAME gchat/adapter/internal/inbound/sender_fill.go 4b1ca9accaae6503976c438c471f69c0cfc31054 4b1ca9accaae6503976c438c471f69c0cfc31054
SAME gchat/adapter/internal/inbound/sender_fill_auth_test.go 894077cfc4491f67f71175ffead11b372275d6a9 894077cfc4491f67f71175ffead11b372275d6a9
SAME gchat/adapter/internal/inbound/sender_fill_cache_test.go 742b2e52719ef5fbdf92296f54a07495da7793ec 742b2e52719ef5fbdf92296f54a07495da7793ec
SAME gchat/adapter/internal/inbound/sender_fill_test.go 877a8c6138662eb558c623b3f7873af4e46099ec 877a8c6138662eb558c623b3f7873af4e46099ec
SAME gchat/adapter/internal/principal/directory_cause_test.go 8be5cfbf09c0c6cafd17a0eccbc3910ca2b37a5b 8be5cfbf09c0c6cafd17a0eccbc3910ca2b37a5b
SAME gchat/adapter/internal/principal/principal.go b2f8660c0fc32342b820abaefe618d84d3c31223 b2f8660c0fc32342b820abaefe618d84d3c31223
SAME gchat/adapter/internal/principal/warnings.go af87a7288f126777789c5e43d8897ef77e7358c4 af87a7288f126777789c5e43d8897ef77e7358c4
SAME gchat/adapter/internal/token/lookup_client_test.go 379d3d10397b591cc6a53aa767bc5ab5672962eb 379d3d10397b591cc6a53aa767bc5ab5672962eb
SAME gchat/adapter/internal/token/token.go 8ecccde4d080ba82cee80f194a63aca10ce0a35c 8ecccde4d080ba82cee80f194a63aca10ce0a35c
SAME gchat/adapter/test/e2e/e2e_test.go aadaf8ecfc4fe633025b9a875d8036fd84167dcb aadaf8ecfc4fe633025b9a875d8036fd84167dcb
```

## Adapter verification

D4 passed under the load-only ruling. Go 1.27.3 on darwin/arm64 ran the suites sequentially using the repository verification environment and local fakes. Python 3.12.13 used pytest 9.2.0.

| Check | Reviewed head | Merged tree | Exit |
| --- | --- | --- | ---: |
| Go vet | Clean | Empty output | 0 |
| gofmt | Empty output | Empty output | 0 |
| Full Go suite | 2,689 passed, 2 skipped | 894 top-level tests and 1,879 subtests passed (2,773 total), 2 skipped. 25 packages passed, 1 had no tests. | 0 |
| Race suite | 1,043 passed across 6 packages | 1,522 passed, 4 failure records (3 top-level tests and 1 child), 0 skipped. 7 packages passed, 2 failed across the expanded 9-package run. | 1 |
| Pack Python suite | 54 tests and 102 subtests passed | 54 tests and 102 subtests passed | 0 |

```sh
verify -C gchat/adapter go vet ./...
verify -C gchat/adapter gofmt -l .
verify -C gchat/adapter go test -count=1 -json -timeout=30m ./...
verify -C gchat/adapter go test -race -json -count=1 -timeout=45m gchat-adapter/internal/principal gchat-adapter/internal/inbound gchat-adapter/internal/app gchat-adapter/internal/chat gchat-adapter/internal/token gchat-adapter/test/e2e gchat-adapter/internal/ledger gchat-adapter/internal/media gchat-adapter/internal/server
verify python3 -m pytest gchat/tests -q -p no:cacheprovider
```

The source inventory counted 848 `func Test…` declarations at the old base, 875 at main, 874 at the reviewed head and 901 at the merge. Every occurrence on each parent is present in the merge. The nearest recorded run of main’s adapter changes passed 870 top-level tests and 1,811 subtests. The full and race runs on the merge each passed `TestDeletedMessageRemovesItsSavedFiles` and `TestSenderFillDeletion`. The race recovery fixture delivered 3,000 of 3,000 messages with zero pending.

## LOAD-ONLY REDS

In the nine-package race run, `TestExpiredDirectCommandFailureSurvivesRestart/no-session` returned HTTP 500 at `gchat/adapter/internal/server/server_test.go:1925` after the request deadline at line 1917. It passed 30/30 on the merge with race detection (M3), 30/30 on the merge without it (M4), and 30/30 on archived main with race detection (M5). The server failure is main's own, load-only, by the mayor's second ruling. In the same run, `TestMissingTokenReplacementResumes` at `gchat/adapter/internal/app/app_test.go:235` and `TestRestartWithALedgerStillDeliversTheOutage` at `gchat/adapter/internal/app/history_floor_test.go:340` missed five-second readiness waits. Each passed its single isolated race rerun, in 3.17 s and 2.47 s respectively. All three cases closed under L3.

```text
app_test.go:235: health did not become ready in 5s: {State:initializing Reason:connecting and reconciling subscriptions}
--- FAIL: TestMissingTokenReplacementResumes (5.96s)
server_test.go:1925: deadline result: 500 {ConversationID: SpaceName: Thread: InReplyTo: Delivered:false Recorded:false MessageID: Key: Reason: FailureKind: RetryAfter:0}
--- FAIL: TestExpiredDirectCommandFailureSurvivesRestart/no-session (1.97s)
history_floor_test.go:340: health did not become ready in 5s: {State:catching-up Reason:recovering retained history}
--- FAIL: TestRestartWithALedgerStillDeliversTheOutage (6.33s)
```

M1 measured 19 changed `gchat/` files, with 1,940 insertions and 24 deletions. M2 found the same 12 changed files in the server’s dependency package directories with `go list -deps` and `go list -deps -test`. The overlap comprises seven production files and five test files in `internal/chat`, `internal/inbound`, `internal/principal` and `internal/token`. The server directory matched main byte for byte. At `internal/server/server.go:887`, `commandHTTP` takes its second ledger snapshot. When the 200 ms request expires, `lockLedger` returns the context error and the handler at line 889 writes HTTP 500 with an `error` field. The test decodes the response into `CommandResult`, which has no such field.

These isolated checks each exited 0. The archived-main check used the same Go race command and verification environment as M3 in main’s extracted adapter module.

```sh
verify -C gchat/adapter go test -race -count=30 -run 'TestExpiredDirectCommandFailureSurvivesRestart/no-session' -json gchat-adapter/internal/server
verify -C gchat/adapter go test -count=30 -run 'TestExpiredDirectCommandFailureSurvivesRestart/no-session' -json gchat-adapter/internal/server
verify -C gchat/adapter go test -race -json -count=1 -timeout=45m -run '^TestMissingTokenReplacementResumes$' gchat-adapter/internal/app
verify -C gchat/adapter go test -race -json -count=1 -timeout=45m -run '^TestRestartWithALedgerStillDeliversTheOutage$' gchat-adapter/internal/app
```

## QUICK review

The reviewer read the complete merged `Processor.message`, its fill and media paths, the empty combined diff, and D1–D4. In `internal/inbound/inbound.go`, updates and deletions still schedule cleanup on the original message name at line 879. Catch-up reconciliation and edit handling run before the fill at lines 916–917 and 931. The fill runs once at line 958 before classification at line 966. A deletion without a usable fill retains the original actor and frame at lines 1006–1012.

At line 1019 the attachment path receives a message whose sender can have a newly filled display name and Workspace domain ID. Its Chat user ID stays the same, and the fill adds no email address. The media store persists file ownership and revisions without sender fields (`internal/inbound/media_cleanup.go:54–111`). Separately, the inbound event ledger retains the display name and rendered attribution, which can include an independently resolved address. The adapter can also persist its own learned Workspace domain (`internal/inbound/inbound.go:1138`, `:1171`, `:1174`).

The merge review returned CLEAN with zero new findings. The earlier gate’s seven blocking findings remain open, including G4 on cancellation and sender classification during retry. Redirect handling remains outside this round. The push receipt and hosted check states are recorded against the final evidence commit in the handoff.
