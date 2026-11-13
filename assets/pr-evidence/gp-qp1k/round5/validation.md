# Final validation

Go: `go1.27.3 darwin/arm64`. Node: `pnpm exec node --version` returned
`v24.21.0`. No unsupported-engine warning appeared in the Node suite logs.
Every runner removed `GC_BIN` and `NEVER_SILENT` from its environment.
Fixtures opted in locally and used invented messages and fake services.

| Directory | Command | Result |
| --- | --- | --- |
| `slack-full/adapter` | `gofmt -l .` | Only the known, unchanged `company_thread_ambient.go` difference |
| `slack-full/adapter` | `env -u GC_BIN -u NEVER_SILENT go vet ./...` | Pass |
| `slack-full/adapter` | `env -u GC_BIN -u NEVER_SILENT go test -p=1 -json ./... -count=1` | 13,443 passing rows, 3 skipped, 0 failures; 1,633 passing top-level tests; 3 packages pass |
| `slack-full/adapter` | `env -u GC_BIN -u NEVER_SILENT go test -race -count=1 ./...` | All 3 packages pass |
| `gchat/adapter` | `env -u GC_BIN -u NEVER_SILENT go vet ./...` | Pass |
| `gchat/adapter` | `env -u GC_BIN -u NEVER_SILENT go test -p=1 -json ./... -count=1 -timeout=30m` | 1,903 passing rows, 2 skipped, 0 failures; 684 passing top-level tests; 22 packages pass |
| `whatsapp/adapter` | `env -u GC_BIN -u NEVER_SILENT pnpm test` | 338 pass, 0 failures at the final test revision |
| `whatsapp/adapter` | `pnpm exec node --test --test-concurrency=1 test/never-silent.test.js` | 78 pass, 0 failures |
| `whatsapp/gateway` | `pnpm test` | 270 pass, 4 known `bsuid-orders` timeouts; 274 total |
| `whatsapp/gateway` | `pnpm typecheck` | Pass |
| Repository root | `make test PYTEST_DIRS='slack-full/tests whatsapp/tests gchat/tests'` | 1,109 pass, 3 skipped, 99 subtests pass, 2 warnings |

The WhatsApp package script is
`node --test --test-concurrency=2 'test/*.test.js'`. Gateway dependencies
already existed; no install was needed. Google Chat's 3,000-message catch-up
completed in the full suite without an isolated rerun.

The gateway's four timeouts were the `bsuid-orders` T1 cases crossing
with/without a user ID and Meta-shaped/number-only inputs. An isolated default
five-second run passed 59 and timed out on 3 of 62. The same unchanged file
passed all 62 with
`pnpm exec vitest run test/bsuid-orders.test.ts --testTimeout=30000`.
No gateway source, test or timeout configuration was changed in this round.

## O5 load check

O5 is DONE: 20 consecutive integration-file runs passed under two Node CPU load workers (7 tests per run, 140 passes, 0 failures), using `pnpm exec node --test --test-concurrency=1 test/never-silent-integration.test.js`. The test waits for both initial pastes with the fake clock progressing and for both durable delivery observations before advancing three minutes.

The unchanged `primary delivered resolution clears a stale sidecar stop
after restart` test failed in the fifth file of the first attempted loaded
sequence, while the edited target passed. It then passed alone with
`pnpm exec node --test --test-concurrency=1 --test-name-pattern='primary delivered resolution clears a stale sidecar stop after restart' test/never-silent-integration.test.js`.
The failure, isolated pass and all completed load sequences are retained.

## Scope and review

The production implementation is `225c956f5cdf9f795d7fec5823018cc3500dcae8`.
Only one WhatsApp test wait changed afterward, in
`fffdc14d1684f79c01480e7631a13b5553722b66`; the final full WhatsApp suite and
load acceptance use that revision. Other suite inputs are unchanged.
The subsequent evidence commit changes documentation and logs only.

`scope-verification.log` records unchanged PR 85 recovery implementation and
five recovery test files against pinned main, and unchanged Google Chat and
WhatsApp production source against the original PR head.
The self-review and narrow addendum are CLEAN with zero findings.
The mayor's whole-delta gate and second-model read remain pending.
