# PR 79 Round 1 verification (gp-ecsi)

Base: `727f921460ed4673321e7ef7f5532ce236e042e6`.
Source commit: `85af4f7c5efa2557f8f8c61f5770946fc13fb6db`, directly on that base.
Go runtime: `go1.27.3 darwin/arm64` (`go-version.log`).
No Node or pnpm command is needed for this Go/Python change.

Final full Go run: **21 packages, 576 top-level tests, and 1,075 subtests passed**,
with no failures or skips. Packages and independent tests were serialized after
the earlier short-deadline failures documented below. No test assertion or
deadline was changed to obtain this result.

Commands below run in `gchat/adapter`, except the Python command, which runs in
`gchat`. Exit codes are recorded in `completion-manifest.tsv`.

| Command | Result | Evidence |
| --- | --- | --- |
| `go test ./internal/inbound ./internal/chat -run 'TestSpaceInventoryForbiddenRetriesMessage\|TestResourceDenied$' -count=1 -timeout=2m` before the classifier fix | Expected failure: all four inventory-error cases were acknowledged instead of held; five classifier cases incorrectly returned resource denial | `regression-before.log` |
| `go test ./... -count=1 -timeout=30m` | Exit 1; 20/21 packages passed, including e2e; puller quarantine acknowledgement deadlines failed | `go-test.log` |
| `go test ./internal/puller -run '^TestRunQuarantineRecoversDuringRun$' -count=5 -timeout=2m` | Exit 0; all five repetitions passed | `puller-recheck.log` |
| `go test ./... -count=1 -timeout=30m -json` | Exit 1; 20/21 packages, 575/576 top-level tests, all 1,075 subtests passed; app recovery readiness deadline failed | `go-test-run2.jsonl.gz`, `go-test-run2-summary.json` |
| `go test ./internal/app -run '^TestRevokedTokenReplacementResumes$' -count=5 -timeout=2m` | Exit 0; all five repetitions passed | `app-recheck.log` |
| `GOMAXPROCS=2 go test ./... -count=1 -timeout=30m -json` | Exit 1; 20/21 packages, 575/576 top-level tests, all 1,075 subtests passed; identity replacement publish timed out | `go-test-run3.jsonl.gz`, `go-test-run3-summary.json` |
| `go test ./internal/app -run '^TestObservedIdentityReplacesContradictedLedgerIdentity$' -count=5 -timeout=2m` | Exit 1; one repetition hit the same HTTP client timeout | `identity-recheck.log` |
| `go test -overlay ../../.gc/scratch/gp-ecsi/base-chat-overlay.json ./internal/app -run '^TestObservedIdentityReplacesContradictedLedgerIdentity$' -count=10 -timeout=3m` | Exit 1; the original classifier also reproduced the HTTP client timeout | `identity-baseline-recheck.log`, `baseline-overlay-check.log` |
| `go test ./... -count=1 -timeout=30m -json -p=1 -parallel=1` | Exit 0; 21 packages, 576 top-level tests, 1,075 subtests passed; zero failures or skips | `go-test-run4.jsonl.gz`, `go-test-run4-summary.json` |
| `go vet ./...` | Exit 0, zero output lines | `go-vet.log` |
| `gofmt -l .` | Exit 0, zero files | `gofmt.log` |
| `uv run --with pytest --with PyYAML --with jsonschema python -m pytest tests -q -p no:cacheprovider` | Exit 0; 20 passed, 17 subtests passed | `pytest.log` |
| `git diff --check -- gchat/adapter` from the lane root | Exit 0 | `diff-check.log` |

The Python environment's pytest, yaml, and jsonschema imports were verified
before running pytest (`python-environment.log`). The before-fix run added the
new regression tests to the base source and deliberately retained the faulty
classifier. The final full suite exercises the same tests with the fix.

The existing recognized membership/resource-denial tests remain in place.
The added inventory regression verifies Nack plus ErrCredentials, no recorded
change/message/transport acknowledgement or inbound delivery, and exactly one
durably delivered turn when the same event is retried after inventory recovery.
The fake-gc test separately preserves HTTP 422 for a missing attachment provider ID.

The first full run's only failure was the unchanged
`TestRunQuarantineRecoversDuringRun` in its removed, replaced, and startup-repaired
scenarios, at the one-second acknowledgement wait. All five isolated repetitions
passed. In the second run puller passed, but the unchanged
`TestRevokedTokenReplacementResumes` exceeded its five-second ready wait after
token replacement; its five isolated repetitions also passed. Run three hit
`TestObservedIdentityReplacesContradictedLedgerIdentity` at its one-second HTTP
client timeout awaiting publish headers. This also occurred once in five
isolated repetitions, and once in ten repetitions using the original classifier.
All logs are retained. Neither of the two timing failures named in the prior Opus read
(shutdown bounded request body and refused-command retry) recurred in any run.

For the baseline comparison, `git show` extracted `chat.go` from the base commit
into `.gc/scratch/gp-ecsi/base-chat.go`. Go's `-overlay` mapped the lane's absolute
`gchat/adapter/internal/chat/chat.go` path to that file. The original blob and
overlay blob match (`baseline-overlay-check.log`). This is the only modified
production file, so the app comparison compiled the base production sources
without changing the worktree or creating another checkout.

The separate installed Codex helper reviewed the source commit against the base
with class STANDARD and model gpt-6-astra: **CLEAN**, no actionable findings.
See `self-review-r2.txt`, `.rc`, and `.reason`. The review correctly notes that
the full Go verification was still incomplete at review time. The mayor's
post-READY gate and read are not claimed by this self-review.

`history-check.log` verifies the base is an ancestor, the removed snapshot is not,
all 351 Syndi commit IDs are unchanged, and the changed paths are within scope.
The evidence commit changes no pack source: the whole `gchat/` tree remains
`411470a83efecf6a8d41da6316f424bd800b60ab`, as at the reviewed source commit.
`source-identity.log` is empty and its diff command exited zero. The compressed
JSONL logs retain every Go test event; the summary JSON files count package,
top-level, and subtest terminal events separately.
