# PR 72 round 2 — gp-xgjf / gp-ktdn

Round base: `eae795ac4f904d65bebd75a7778731a8dec32995`.
Remote main at start: `8cb01a83bdaa6b4b42aeb63bae1ee4a6d369262f`.
Remote main immediately before push: the same SHA (`remote-push.txt`).
The assigned lane starts at that round base on `gp-xgjf`; its commit will
fast-forward the existing PR branch `gp-ktdn` without a rebase or forced update.

## Measurement and scope

Read the complete Fable r1 record at
`/Users/tailor512/city/assets/ops/codex-gates/gp-ktdn-pr72-fable-read-r1-eae795ac.md`,
including Findings and NOTEs 2, 4, 5 and 6, and the mayor's boot measurement at
`/Users/tailor512/city/assets/ops/pr-evidence/gp-ktdn-boot-measure/README.md`.

- At the round base, `registration_startup.go:85` accepted only connection
  refusal and the city-not-found 404; `registration_startup_test.go:257`
  explicitly expected a 500 to be fatal. The Fable read identifies the fork's
  503 `adapter registry not available` response at
  `internal/api/huma_handlers_extmsg.go:31` on `f22b3ed4a`.
- `main.go:6836` sets `gcForwardClient` to 20 seconds. Its `Do` errors return
  unchanged at `main.go:2460`; response-read errors wrap their cause at
  `main.go:2465`. Thus `errors.As` can reach `net.Error.Timeout()` through
  `url.Error`, and `errors.Is` can reach reset, EOF and unexpected EOF causes.
- The round base's policy at `registration_startup.go:106` is 1 second,
  doubling to 30 seconds, with one ten-minute context at line 113. The policy
  and loop are unchanged; their new locations are lines 119 and 126. There is
  no second retry budget.
- The boot README section (a), line 12, records seven minutes six seconds
  without a restart after the 20:25:17Z fatal exit. Section (b), line 17,
  quotes the exact city-not-found response. Section (c) estimates city
  availability at 45–65 seconds after boot.
- NOTE 2 remains open: the revision of the binary actually running on the
  boxes is not established. This fix does not depend on that revision or on
  supervisor restart timing. The restart policy and box rollout remain the
  mayor's work. NOTEs 5 and 6 (signal exit reporting and health during spool
  recovery) are unchanged.

## Changes

- MINOR 1: `registrationCanRetry` at `registration_startup.go:87` additionally
  accepts every 500–599 status, `errors.As(err, &transportErr)` with
  `transportErr.Timeout()`, and `errors.Is` for `syscall.ECONNRESET`, `io.EOF`
  and `io.ErrUnexpectedEOF`. Existing refusal and city-404 handling remains.
  Permanent 4xx failures and the once-only 400/422 template fallback remain.
- The classification table at `registration_startup_test.go:328` pins 26
  cases, including all requested retry/fatal cases, both status-range edges,
  a non-timeout DNS error, cancellation, and a 429.
- Subprocess tests at lines 188 and 227 prove two 503s followed by success,
  and a hung first request followed by success after a client timeout. Both
  preserve the template, assert attempt logs and back-off, and register in
  the same process. The test-only `SLACK_TEST_REGISTRATION_CLIENT_TIMEOUT=1`
  knob at line 31 changes the child client's timeout to 100 ms; production
  retains 20 seconds. It does not shorten the overall registration budget.
- Deadline tests at lines 369 and 393 prove that a repeated 503 or EOF
  exhausts the same total budget and retains the last status or error text.
- MINOR 2: `measuredCityNotFoundBody` at line 326 is the full measured 404
  body, byte for byte, including the type, title, status, hou-dynasty detail
  and code. The classifier and deadline tests use it. `07-fixture.log`
  records the independent 170-byte equality check against the mayor's README.
- MINOR 3: every round-2 commit includes contiguous `Bead: gp-ktdn`,
  `Bead: gp-xgjf` and `Co-Authored-By: OpenAI Codex <noreply@openai.com>`
  trailers. Existing round-1 commits are not rewritten.
- The changelog and service comments name the expanded retry set; no
  service configuration key, dependency or rollout setting changes.

## RED and verification

`01-red.log` records the two final subprocess regression tests against the
unchanged round-1 production code, before changing the classifier. Both
compiled and failed at runtime: a 503 and the client timeout each logged
`next wait=0s (not retryable)` and exited. This is the same test code used
for GREEN; later edits added classification and deadline coverage only.
Trailing whitespace in the portable RED log is trimmed; its raw local copy
is `/tmp/gp-xgjf-red-raw.log`.

Toolchain: `go version go1.27.3 darwin/arm64`. Python uses the existing
`/tmp/gp-ktdn-pytest/bin/python` venv; `pytest.__file__` was verified at
`/private/tmp/gp-ktdn-pytest/lib/python3.15/site-packages/pytest/__init__.py`.
No pnpm/Node command applies to this Go/Python change.

Each command runs from its module directory and has a `.status` completion
record. Full Go JSONL transcripts remain local and ignored; parsed summaries
are committed. The completion manifest is read before reporting results.

- Adapter build: `go build -o /tmp/gp-xgjf-slack-adapter .` — PASS.
- Focused acceptance: `SLACK_TEST_BOOT_DELAY=1 go test -race -count=1
  -timeout=90s -run '^TestRegistration' -v .` — 42 passing tests/subtests,
  no failures or skips, including delayed port, both new recovery cases,
  classification, compatibility and deadline coverage.
- CLI: `go test -race -count=1 -timeout=5m -json ./...` — 237 passing
  tests/subtests (199 top-level), eight packages, no failures or skips.
- Pack: `/tmp/gp-ktdn-pytest/bin/python -m pytest tests/ -q` from
  `slack-full` — 907 passed, two skipped, two existing multiprocessing/fork
  deprecation warnings.
- Adapter: `go test -race -count=1 -timeout=5m -json ./...` — 12,876
  passing tests/subtests (1,443 top-level), two packages, no failures and
  three opt-in skips. The delayed-port skip is exercised by the focused run.

## Review and handoff

The installed Codex helper reviewed implementation commit
`8e56df8863dce5f65cd28a3831cddae82d0a9cae` against
`eae795ac4f904d65bebd75a7778731a8dec32995` with `ROUND_BASE=1`,
`--class STANDARD` and `--model gpt-6-astra`. Result: **VERDICT: CLEAN**,
exit 0, zero actionable findings. See `08-codex.out`, `.rc`, `.reason`
and `.status`. The reviewer used committed test evidence; it did not rerun
tests in the read-only sandbox.

The following commit adds only this verdict, handoff documentation and remote
measurements. Production code and tests are identical to the reviewed commit.
The mayor owns the subsequent round-2 gate, cross-model read, founder card
and merge decision. PR 72 remains draft.
