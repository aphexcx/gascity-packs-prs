The Slack adapter can start before gc publishes the city API. Previously it
exited on the boot-time 404, leaving the measured box without Slack until a
manual restart. Startup now serves advisory health while registration retries
within one ten-minute budget, with waits doubling from one to thirty seconds.
Normal requests receive 503 until registration and recovery finish.

Connection refusal, city-not-found 404s, all 5xx responses, timeouts, resets,
EOF and unexpected EOF are retryable. Authentication and other permanent
4xx failures still exit. The template compatibility retry fires once for a
400 naming `reply_instructions` or the Huma 422 identifying it as an unexpected
property. Deadline failures retain the last status or transport error.

## Round 2 — gp-xgjf

Round base: `eae795ac4f904d65bebd75a7778731a8dec32995`. Main at start:
`8cb01a83bdaa6b4b42aeb63bae1ee4a6d369262f`. This round fast-forwards the
existing PR; it does not rewrite round 1.

Measure first: the complete Fable r1 record was read, including Findings and
NOTEs 2, 4, 5 and 6. Its Findings at line 93 describe the 5xx/transport gap;
the mayor's `gp-ktdn-boot-measure/README.md:12` records seven minutes six
seconds without a restart, and line 17 quotes the measured 404 body. At the
round base, `slack-full/adapter/registration_startup.go:85` retries only
refusal/city-404 and `registration_startup_test.go:257` pins 500 as fatal.
`slack-full/adapter/main.go:6836` gives the forward client a 20-second timeout;
lines 2460 and 2465 preserve transport and response-read error causes.

1. **MINOR 1:** `slack-full/adapter/registration_startup.go:87` now accepts
   500–599 responses, `errors.As` to `net.Error` with `Timeout()`, and
   `errors.Is` for `syscall.ECONNRESET`, `io.EOF` and `io.ErrUnexpectedEOF`.
   The existing policy at line 119, single context at line 126, and back-off
   at lines 163 and 176 remain unchanged. The table at
   `slack-full/adapter/registration_startup_test.go:328` pins 26 cases,
   including 401/403, unrelated 404/422, 500/502/503, wrapped timeout,
   reset and EOF errors. Process tests at lines 188 and 227 prove two 503s
   then success, and a client timeout then success, with the same PID,
   attempt logs and retained template. `SLACK_TEST_REGISTRATION_CLIENT_TIMEOUT=1`
   at line 31 affects only the child test process (100 ms); production stays
   at 20 seconds. Deadline tests at lines 369 and 393 retain the last 503
   or EOF inside the same total budget.
2. **MINOR 2:** the fixture at
   `slack-full/adapter/registration_startup_test.go:326` contains the measured
   body whole, including type, title, status, hou-dynasty detail and code.
   The classifier asserts retry, and an independent comparison verifies all
   170 bytes against the mayor's README.
3. **MINOR 3:** round-2 commits carry contiguous `Bead: gp-ktdn`,
   `Bead: gp-xgjf` and `Co-Authored-By: OpenAI Codex <noreply@openai.com>`
   trailers. The prior commits remain intact.

NOTE 2 remains open: the revision of the binary actually running on the
boxes is not established. The fix does not depend on that revision or on
supervisor restart timing. Box rollout and restart-policy investigation
remain with the mayor. NOTEs 5/6 are unchanged.

## Validation

Evidence: [`assets/pr-evidence/gp-ktdn/round-2/`](assets/pr-evidence/gp-ktdn/round-2/).
Go 1.27.3 on darwin/arm64; Python 3.15 in the verified existing pytest venv.
No pnpm/Node tests apply. Commands run from the named module directories:

- RED against unchanged round-1 production code: both new process tests
  compile and fail at runtime on `not retryable` exits. The same tests pass
  after the classifier change; see `01-red.log` and `03-focused.log`.
- Adapter `go build -o /tmp/gp-xgjf-slack-adapter .`: PASS.
- Adapter `go test -race -count=1 -timeout=5m -json ./...`: **12,876** passing
  tests/subtests (1,443 top-level), two packages, three opt-in skips.
- Adapter `SLACK_TEST_BOOT_DELAY=1 go test -race -count=1 -timeout=90s
  -run '^TestRegistration' -v .`: **42** passing tests/subtests, no skips,
  including the 20-second closed-port case and both new recovery cases.
- CLI `go test -race -count=1 -timeout=5m -json ./...`: **237** passing
  tests/subtests (199 top-level), eight packages, no skips.
- From `slack-full`, `/tmp/gp-ktdn-pytest/bin/python -m pytest tests/ -q`:
  **907 passed**, two skipped, two existing multiprocessing/fork warnings.

The lane's Codex self-review is recorded in the round-2 evidence before READY.
The mayor's round-2 gate and cross-model read follow READY. This PR stays draft;
the merge and any box rollout require the mayor's separate handoff.
