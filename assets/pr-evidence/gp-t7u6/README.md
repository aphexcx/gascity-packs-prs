# gp-t7u6: fork CI baseline

Base: ad841a9a65a108a65769cb5bc5b668937611dead (fork main after gp-obim).

## Causes and changes

- CI run 38670780842 failed because ripgrep was absent. The shutdown probe assertion used rg and therefore also failed; no shutdown production defect was found. CI now installs and prints the ripgrep version.
- The canonical-collision and redirected-root stubs rejected the legitimate handoff_stage clear before reaching artifact validation. Their inner `fail` exited nonzero, which the outer test accepted as the expected safety refusal. The full baseline with GNU tools exited 0 while printing both FAIL lines. The fixed fixtures allow only the two preparatory metadata writes, reject every other update, and require the exact intended guard diagnostic. Two mutation probes restoring the old stubs both fail under the strengthened assertions. No baseline list, waived assertions, or skipped shell tests.
- run.sh removes one trailing slash before joining private GOCACHE/GOPATH paths. The two existing GOPATH tests failed on the base with a trailing slash, then passed unchanged in both full Slack Python runs.
- README documents the five-line upstream-twin recipe and Linux-equivalent shell prerequisites on macOS. The cached uv interpreter is invoked directly and its bin directory put on PATH for subprocesses: the current no-install wrapper rejects `uv run --offline --with`.

## Local verification

- `shell-tests.txt`: exact CI loop, all 11 scripts passed, zero FAIL lines. On macOS: Bash 5.3.20, GNU sed 4.10/coreutils, physical TMPDIR=/private/tmp, ripgrep 15.2.0. `shell-baseline-gnu.txt` preserves the two misleading baseline messages. `shell-baseline.txt` records the initial unsupported BSD/Bash 3.2 environment, not a product regression.
- `go-tests.txt`: Go 1.26.5, prescribed full adapter command with GOFLAGS=-json for counting, 12,574 cases passed (1,349 top-level), 0 failures/skips. No Go source or Go test changed.
- `python-no-slash.txt` and `python-with-slash.txt`: each 655 passed, 2 skipped, 2 existing os.fork warnings, Python 3.13.13. The cached environment supplies pytest/PyYAML/jsonschema offline. `python-baseline.txt`: the same two unchanged GOPATH tests failed before the run.sh fix.
- `stub-mutation-check.txt`: 2/2 deliberately broken stubs rejected, proving unrelated setup errors no longer satisfy the safety assertions.
- `change.diff`: zero-context source delta from the base. `ci-run.txt` and `ci-run.json` record Ubuntu CI run 38732073337 at e650c342: ripgrep installation, Python, role composition, maintained pack checks, lint baseline, and all Gastown shell tests passed. READY supplies final-head check status.
- `python-ci-suites.txt`: wider CI-directory run completed with 2,204 passed, 43 skipped, 10,594 subtests passed, and 10 failures because shell subprocesses selected system Python without PyYAML. With the cached environment bin on PATH, `python-subprocess-recheck.txt` passed all 14 build-artifact checks (including all ten failures); no product changes. The corrected PATH is documented in the twin recipe.
- `codex-review-r1.txt`: validated QUICK CLEAN at e650c342. `codex-review-r2.txt`: validated QUICK CLEAN at 77336a4f, including the subprocess PATH documentation refinement. Subsequent changes add evidence only.
- No Node or pnpm test was used; no unsupported-engine warning.

## Upstream scope

Upstream main 520e95cb22992a8a1017f7a60da267f203497c7e has byte-identical run.sh. `upstream-run-sh.patch` is the isolated transplant, validated against a temporary index at that upstream commit (no conflicts). The bead explicitly excludes upstream publication; no upstream branch or PR was pushed. The mayor can route this twin patch separately. See `upstream-check.txt`.
