# Empty-log replay: round 2

Review base: `59f7131451e555f7d7e5de7b1333d119513623b2`.
Code commit: `137ca7d6c599357f979548b58ec94484211e3f65`.

The fault class was unfinished replay after a log rotation whose replacement
active file remained empty. Archives were consumed, but the cursor retained
the old file identity, so enforcement waited indefinitely for an active row.

The reader now adopts the empty replacement's device, inode and zero offset
after checking the actual rotating tail and confirming that recovered sequences
reach the published archive tail. Sequence gaps fail measurement and hooks
continue to fail open. Already-caught-up calls take the existing fast path;
they neither discover nor read archives.

## Class regression

`reachable/test/empty-replay.test.cjs` exercises the same end-to-end assertion
for each shape: the archive is published, the active file remains empty, the
overdue message refuses Write, and status reports `caught_up: true`.

| Rotation shape | Round start | Code head |
| --- | --- | --- |
| Warm saved cursor | RED | GREEN |
| Cold replay checkpointing across multiple archives | RED | GREEN |
| Chunked archive with a persisted decoded offset | RED | GREEN |
| Pending publication, then published | RED | GREEN |
| Legacy pending publication, then published | RED | GREEN |
| Legacy publication with an older same-second archive | RED | GREEN |

No sentinel row is appended. Existing rotation tests remain unchanged.
Additional cases verify missing middle sequences, a missing published tail,
and a missing archive: all fail open without advancing the saved cursor.
The middle-gap case already passed at the base; the tail and missing-archive
exception assertions failed there. A warm-call check rejects archive discovery
or reopening after adoption.

The review found another case in this class: a legacy rotating file and an older
archive can share a timestamp second. The added parameterized test covers warm,
warm-ahead-of-archive, multiple-archive, and chunked cursors, each with inbound
and outbound rows. All eight cases fail on the original source and pass at the
code head. Six cases also reproduced the first implementation's premature
adoption; the advanced-cursor cases caught premature gap errors before the
publication check was completed. See `CODEX-1.md` and the red transcripts.

## Stop and pass

Scope item 4 was **STRUCK by amendment 1**. Stop semantics and every existing
Stop test remain unchanged. The diff of `reachable/test/review.test.cjs`
against the review base is empty. A new check on the completed gate sequence
confirms that Stop marks the waiting message seen and the next Write is allowed.
The discarded pending-Stop test is absent from the committed delta.

After the gate sequence, pass returns
`{"conversation":"example/room-example","seen":true}` and the next Write
is allowed. During a genuinely pending publication, pass still throws
`event_catchup_incomplete`; the unchanged CLI prints
`reachable: event_catchup_incomplete` and exits 1.
`cli-pass-head.log` records an actual CLI probe, including the successful next Write
and zero active-log bytes; its fixture source is `pass-probe.cjs`.

## Validation

The toolchain shim was first on PATH for every test command.
`pnpm exec node --version` reported `v24.21.0`, as in round 1.
No unsupported-engine warning appeared.

| Working directory and command | Result | Transcript |
| --- | --- | --- |
| `reachable/test`: `node --test --test-reporter=tap --test-name-pattern='empty active log:'` at the base, new tests only | Exit 1; 19 tests, 7 pass, 12 fail | `red-empty-replay.log` |
| `reachable/test`: same focused command at the first implementation | Exit 0; 19 pass, 0 fail | `green-empty-replay.log` |
| Frozen base source plus the complete new test file: `node --test --test-reporter=tap` | Exit 1; 23 tests, 1 pass, 22 fail | `red-complete-base.log` |
| `reachable/test`: same focused command at the final code head | Exit 0; 29 pass, 0 fail | `green-publication-class.log` |
| `reachable/test`: `node --test --test-reporter=tap` at the final code head | Exit 0; 91 pass, 0 fail, 0 skipped | `pack-suite-head.log` |
| Repository root: `make test` before the review correction | Exit 0; 81 Node tests; 2695 Python tests, 126 skipped, 10734 subtests; 2 existing fork warnings | `make-test.log` |
| Repository root: `make test` after the review correction | Exit 2; 91 Node tests passed; Python: 2694 passed, 1 failed, 126 skipped, 10734 subtests | `make-test-final.log` |
| Repository root: `make test` retry at the code head | Exit 2; 91 Node tests passed; Python: 2694 passed, 1 failed, 126 skipped, 10734 subtests | `make-test-retry.log` |
| `gascity`: `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q -k PnpmShimTests` | Exit 0; 17 passed, 513 deselected | `pnpm-shim-rerun.log` |
| Repository root: `node assets/pr-evidence/gp-7w04/round2/pass-probe.cjs` | Exit 0; pending rejected, published pass succeeded, next Write allowed | `cli-pass-head.log` |

The focused totals include six passing file containers with no matching tests.
The final full suite adds 23 tests/subtests to round 1's 68 tests. The frozen
base check used an isolated fixture directory inside the existing clone, with
the original source obtained by `git show` and the final regression file copied
beside it. No source checkout or live event log was used.

The repository rerun hit the scheduling-sensitive pnpm assertion already
documented in round 1: all three commands completed and exactly one install
ran, but one rather than two callers logged a wait. The isolated class passed
all 17 tests. No toolchain source changed; the failure was filed for maintenance.
Scoped verification was accepted by ruling 1, conditional on that wait-message
assertion being the only whole-suite failure. The diff of `gascity/` against
the review base is empty. The retry confirmed exactly the same sole failure:
`gascity/tests/test_toolchain_shims.py:566`, `AssertionError: 1 != 2` for the
number of callers logging a wait. Both failed runs had two existing fork
deprecation warnings. No test was weakened or skipped, and no further
whole-suite rerun was performed after ruling 1.

The full suite repeats round 1's exact 256 MiB fixture, 100 warm decisions and
20 complete hook processes; both p95 limits are 100 ms.

| Measurement | Round 1 | Round 2 |
| --- | --- | --- |
| Warm decision p95 | 35.040 ms | 1.678 ms |
| Whole hook process p95 | 56.319 ms | 35.160 ms |

These are local observations, not a claimed speedup; scheduling and filesystem
cache state vary. The regression also directly checks that already-caught-up
calls do not discover or reread archives.

## Review

Both same-model self-review reads were CLEAN with zero findings at every
severity. Codex DEEP check 1 returned one MAJOR; it was reproduced and fixed
in the code head (`CODEX-1.md`). Codex DEEP check 2 returned CLEAN with zero
findings (`CODEX-2.md`). Both checks reviewed the full round delta from the
stated base. The final evidence commit changes no runtime source or tests.

## Public repository check

The full round delta and commit messages were checked. All event data is
invented. Public transcripts normalize 148 clone-path occurrences to `<clone>`
and 2 temporary fixture paths to `<temporary-fixture>`; 150 path occurrences
changed in total. No customer names, real account/conversation identifiers,
local user-home paths or credentials were added. No force push or history
rewrite is part of this delivery. `completion.txt` records commands and exits.
Trailing whitespace in transcript lines is normalized without changing results.
