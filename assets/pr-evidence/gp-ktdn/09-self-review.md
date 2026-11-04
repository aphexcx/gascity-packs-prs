# gp-ktdn self-review and verification

Self-review by the implementation worker on its own model, before READY.

## Review

- Traced the synchronous gc service constructor to city-route publication. Starting the health listeners early and returning advisory HTTP 200 breaks that startup dependency. Both TCP/public and UDS/internal servers use the same startup gate; ordinary requests remain retryable 503 until registration and spool recovery finish.
- Checked that normal health behavior resumes after startup, preserving existing company, socket, liveness and dispatch-drop diagnostics.
- Checked compatibility once-only state across transient failures. Legacy 400 must name `reply_instructions`; measured 422 must identify that field as an unexpected property. An unrelated field, invalid value, authentication failure, generic missing route, DNS error or server failure does not trigger compatibility or boot retries.
- Checked deadline propagation into HTTP requests and waits, one attempt log with the next delay, bounded/single-line error details, listener failure handling, startup signal cancellation and fatal exit retaining the last failure.
- The only production retry budget is ten minutes with 1/2/4/8/16/30-second waits. Shortened deadlines exist only in subprocess tests. No production timing/configuration switch was added.
- Process tests use explicit fake credentials and isolated temporary state, and exercise real startup plus the UDS health probe. The delayed-port run succeeded in the same process at attempt 6 (~31 seconds), after the fake API port opened at +20 seconds.
- No remaining correctness finding from self-review. The mayor owns the cross-model Fable review and merge gate.

## Verification

Toolchain: city toolchain directory first on PATH; `go version go1.27.3 darwin/arm64`. Python tests used `/tmp/gp-ktdn-pytest/bin/python` (Python 3.15), with `pytest.__file__` verified in that environment. No pnpm/Node tests apply to this Go/Python change.

- RED: unchanged `origin/main` production source plus the new process regression test. `go test -count=1 -run '^TestRegistrationStartup(RetriesCityNotRunning|ReplyInstructionsCompatibility)$' -v .` from `slack-full/adapter`: city-not-running case failed because startup exited after two 404s without serving health; legacy compatibility passed. See `02-red.log` and `.status`.
- Initial focused GREEN: `go test -race -count=1 -timeout=60s -run '^TestRegistration' -v .` from `slack-full/adapter`. See `03-green.log`; the separate delayed-port opt-in was skipped here.
- Final focused acceptance: `SLACK_TEST_BOOT_DELAY=1 go test -race -count=1 -timeout=90s -run '^TestRegistration' -v .` from `slack-full/adapter`. All cases passed, including both 400/422 compatibility shapes, deadline/fatal behavior and the delayed port. See `08-boot-and-registration.log`.
- Build: `go build -o /tmp/gp-ktdn-slack-adapter .` from `slack-full/adapter`, successful.
- Full race suites: `go test -race -count=1 -timeout=5m -json ./...` from each of `slack-full/adapter` and `slack-full/cli`. See `05-adapter-summary.json`, `06-cli-summary.json` and their completion status files. Summaries are parsed from full local JSONL logs; counts distinguish top-level tests from subtests.
- Pack tests: `/tmp/gp-ktdn-pytest/bin/python -m pytest tests/ -q` from `slack-full`: 907 passed, 2 skipped. Two multiprocessing/fork deprecation warnings; no test failures. See `07-pytest.log`.

The first full adapter run passed before the measured 422 classification addition. A final full adapter run is being collected for the final handoff; its summary replaces the initial summary once complete. CLI and Python sources were unchanged by that addition.
