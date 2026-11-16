# Verification

Base: `68884bf15089e79230bc88c55b591033e3fe927f`.
Production source: `5640396ad41fd76f67314f82d6d0ada96e97dfb7`.
Final test source: `5b579d614aadc5392385300cf93de5194a546fca` (test-only fix).
Toolchain: `go version go1.27.3 darwin/arm64`.
All checks unset `GC_BIN` and use local fakes. No live Chat service was used.

## Base and regression proof

- `gofmt -l .`: clean; `env -u GC_BIN go vet ./...`: passed.
- `env -u GC_BIN go test -count=1 ./...`: 21 packages passed. The existing
  end-to-end backlog case exhausted its default package-derived test budget
  after delivering 2,743 of 3,000 messages, leaving 257 in the ledger.
- `env -u GC_BIN make test PYTEST_DIRS=gchat/tests` from the repository root:
  49 tests and 89 subtests passed.
- `base-regressions-red.log` records the tests-first failures against the base
  implementation: adding-message delivery, bounded history, persisted first
  bounds, bare-row joins, membership fallback/failure and the setting.
- A queued baseline race attempt was interrupted after it started on a changing
  tree. It is not valid baseline race evidence and is not represented as such.

The first regression commit is `09a7e1a1`. Review-follow-up regression tests
precede their fixes in `cee4790f` and `44c3c0cb`; the latter corrects the fake
city callback route. The upload fixture also uses its canonical media path.
The corrected interrupted-command test fails on `171bb546` and passes with
the fix. The landed-receipt count failure is in the first review-red transcript.
The final focused run passed 7 top-level tests and 6 subtests in 2 packages.

## Head

Commands run from `gchat/adapter` unless a different directory is stated:

- `env -u GC_BIN gofmt -l .`: clean.
- `env -u GC_BIN go vet ./...`: passed.
- `env -u GC_BIN go test -p 1 -count=1 -timeout=30m -json ./...`: 21 packages
  passed; 702 top-level tests and 1,244 subtests passed, 2 tests skipped.
  One top-level test and one subtest failed in the unchanged server package:
  `TestExpiredDirectCommandFailureSurvivesRestart/no-session` returned HTTP 500
  at its 200 ms deadline. The end-to-end package passed, including all 3,000
  missed messages with zero retained backlog.
- `env -u GC_BIN go test -p 1 -count=10 -timeout=30m -json ./internal/server -run '^TestExpiredDirectCommandFailureSurvivesRestart$'`:
  all 10 repetitions and 30 subtests passed on the same source.
- `env -u GC_BIN go test -p 1 -count=1 -timeout=30m -json ./internal/server`:
  94 tests and 126 subtests passed on the same source.
- The first race run found an existing unsynchronized notice slice in
  `TestManagedSourceChatRefusalRetriesWithoutSignIn`. It was stopped to fix
  that fixture, and is preserved as an interrupted run. Commit `5b579d61`
  adds a mutex around the slice and publishes the notice count after the
  append. All notification assertions remain; production source is unchanged.
- `env -u GC_BIN go test -p 1 -race -count=10 -timeout=30m -json ./internal/app -run '^TestManagedSourceChatRefusalRetriesWithoutSignIn$'`:
  all 10 repetitions passed after the fixture correction.
- `env -u GC_BIN go test -p 1 -race -count=1 -timeout=30m -json ./...`:
  all 22 packages passed, with 703 top-level tests and 1,245 subtests passing,
  2 tests skipped, and no failures or data races. The end-to-end drain delivered
  all 3,000 messages with zero retained backlog.
- Final root `env -u GC_BIN make test PYTEST_DIRS=gchat/tests`:
  49 tests and 89 subtests passed.

Packages run serially with an explicit 30-minute package timeout because the
baseline large-drain test exhausted its default budget, and concurrent package
runs exposed short existing HTTP fixture deadlines. No test assertion, HTTP
deadline or end-to-end deadline calculation was weakened. The first full-head
failure remains recorded; the server rechecks passed without a source or test
change. The non-race and server-recheck commands used `5640396a`; the corrected
fixture and repeated race commands used `5b579d61`.

The JSON-run transcripts retain test/subtest/package result events and relevant
failure/backlog diagnostics, omitting application chatter. Counts come from the
Go test JSON events. Plain base transcripts are preserved with temporary paths
redacted. Manifests record commands, exit codes and the exact tested source.

The manifests keep the original local log names; their public counterparts are:

| Local run log | Public transcript |
| --- | --- |
| `red-test.log` | `base-regressions-red.log` |
| `final-test.jsonl` | `head-test.log` |
| `final-race.jsonl` (interrupted) | `head-race-first.log` |
| `race-fixed.jsonl` | `head-race.log` |
| `final-pack.log` | `head-pack.log` |
| Server/fixture recheck `.jsonl` files | Matching `.log` files |

The zero-byte formatting and vet logs are represented by their manifest exits.
