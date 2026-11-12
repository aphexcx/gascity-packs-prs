# Reachable validation

Node was selected by the city toolchain wrapper. `pnpm exec node --version`
reported **v24.21.0**. No unsupported-engine warning appeared.

| Invocation | Result |
| --- | --- |
| From `reachable/test`: `node --test --test-reporter=tap` | 68 passed, 0 failed, 0 skipped. |
| From repository root: `make test` | Node phase: 54 passed. Python phase: 2695 passed, 126 skipped, 10734 subtests passed; two existing `os.fork` deprecation warnings. Exit 0. |
| From `gascity`: `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q -k PnpmShimTests` | 17 passed, 513 deselected. |

The final standalone Node run includes fourteen additional regression cases after
the repository's Node phase: deferred history after Stop, partial archive pipe
writes, and the Codex classes in CODEX-1.md through CODEX-3.md. No other pack changed.

The first full repository run had one existing scheduling-sensitive assertion
in `PnpmShimTests::test_concurrent_callers_wait_for_one_install_instead_of_racing_it`:
all callers completed and exactly one install ran, but one rather than two
callers printed a wait message. Its suite passed independently, and the complete
repository rerun passed. The wrapper was not changed.

The 256 MiB fixture's final warm measurements were **35.040 ms p95** over 100
decisions and **56.319 ms p95** over 20 complete hook processes. Both limits are
100 ms. A separate 260 MiB archive fixture verifies bounded replay and complete
sequence recovery. All event logs, cities, settings and messages used by the
new tests are fixtures. No session, channel, live event log or network is used.

`green-pack.log` contains the final pack transcript. `completion.txt` records
the preceding red and green commands and their exit codes. The repository
summary is in `make-test-summary.txt`.

Codex DEEP round 4 returned CLEAN on
`4446315d877f25c88e86398d35c46d2acc97e56e`, with no actionable findings.
The final evidence commit changes no implementation or test code.

Public-repository check: complete. Seven host-path occurrences found in the
final scan were replaced with fixture paths. Earlier transcript path
normalization is retained. The final scan has zero host, account or real
conversation identifiers; fixtures contain invented values only.
