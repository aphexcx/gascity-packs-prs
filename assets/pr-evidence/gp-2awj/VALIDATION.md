# Validation record

All channel and vendor traffic used local fakes. Go counts below include named
subtests; Node counts are the runner's test total. No unsupported-engine warning
occurred in the final Node run. `pnpm exec node --version` returned v24.21.0.

| Scope | Invocation | Observed result |
| --- | --- | --- |
| Google Chat, module directory | `env -u GC_BIN go test ./... -count=1 -timeout=30m -json` | 1,846 passed, 2 opt-in real-gc tests skipped, exit 0 |
| Google shared stopwatch after thread normalization | `go test ./internal/stopwatch -count=1 -json` | 2 passed, exit 0 |
| Slack, module directory, after second-round fixes | `go test ./... -count=1 -json` | 13,356 passed, 3 skipped, exit 0 |
| Slack, after the final paired-event guard | `go test . -run 'TestArrival\|TestCompanyRoomMentionOnly' -count=1 -json` | 53 passed, exit 0 |
| WhatsApp, adapter/test directory | `pnpm exec node --test --test-concurrency=2` | 265 passed, zero failed/cancelled/skipped, exit 0 |
| Repository | `make test` | 2,900 passed, 1 failed, 131 skipped, 10,735 subtests passed, 2 warnings, exit 2 |
| Unchanged failing repository case, isolated | `make test PYTEST_DIRS=gascity/tests PYTEST_ARGS='-q -k test_a_signal_during_the_release_is_lost_and_nothing_is_left_standing'` | 1 passed, 529 deselected, exit 0 |
| WhatsApp pack | `make test PYTEST_DIRS=whatsapp/tests` | 76 passed, exit 0 |
| Final report and copy parity | `make test PYTEST_DIRS=tests PYTEST_ARGS='-q -k stopwatch'` | 5 passed, 540 deselected, exit 0 |

Both Go modules ran `gofmt -l .` and `go vet ./...`, exit 0. Google formatting
output was empty. Slack formatting lists `company_thread_ambient.go`, which is
unchanged and produces the same formatting result at the base.
The complete Slack run preceded the final paired-event guard; the targeted
arrival and mention-only suites cover that final change.

The repository failure was the existing toolchain signal-release test: its
fixed-delay SIGTERM returned 143 instead of 0. The isolated rerun passed; the
toolchain module is unchanged. The two repository warnings are the existing
multithreaded `os.fork()` deprecation warnings.

Earlier full WhatsApp runs had different acceptance/recovery timing failures
(261 passed, 3 failed, 1 cancelled; then 263/1/1; then 264/0/1). The final run
passed after adding explicit diagnostics when publishing returns before a
test's intended killpoint; no cause for the earlier timeouts is claimed. A
targeted acceptance/SIGKILL run passed all 37 discovered test entries.

An earlier Google full run had one lost-renewal timeout (three failed parent/
subtest outcomes). Its isolated rerun passed five outcomes, and the subsequent
full suite passed. An initial run also included an optional real-gc check via
inherited GC_BIN; final runs explicitly unset it. Initial wrapper failures for
the new command allowlist and help text were fixed; the selected rerun passed
both tests and all eleven subtests.

Live Jev latency and live WhatsApp reaction delivery remain unverified. The
gateway, deployed adapters, secrets and catalog rows were not changed.
