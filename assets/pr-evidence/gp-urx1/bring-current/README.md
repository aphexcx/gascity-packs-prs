# PR 105: bring-current validation

Status: **BLOCKED by the reconnect acceptance test; not published.**
No source behavior or test assertion was changed to resolve the failure.
The existing draft PR remains at its original head. No deployment occurred.

## Revisions

| Role | Commit |
| --- | --- |
| Common base | `4d14c7c5d52a468a2af7e387175ff4fb20366ec5` |
| Original PR head / first merge parent | `45af2199a03db93a3a758b19c2a01245d36ceba8` |
| Fetched main / second merge parent | `c26e664ec079f6a5e5cc9cab31a091500e4d6fd7` |
| Verified code head / merge commit | `920210cd6b3d2c38934d7149fcc60240be97ea04` |

This file is in a separate evidence-only commit after the verified code head.
The handoff records that evidence commit's full SHA. Main had not advanced
beyond the specified target when fetched.

## Resolution

The independent pre-merge measurement found **one conflicted file** and
**six automatically merged files**. Seven paths changed on both sides.

| File / hunk | Main | PR | Kept / reason |
| --- | --- | --- | --- |
| `whatsapp/CHANGELOG.md`, top insertion | Reaction and quoted-reply entries | Four held-connection follow-ups and the no-deployment line | Both complete sections, PR first; main's section order and every word retained |

An exact-content comparison proved that the resolved changelog equals the
whole main changelog with only the complete PR section inserted at the top.
No line from either side was dropped or rewritten. The remerge diff contains
only marker removal and the separating blank line. Outside that file, the
merge equals the automatic merge tree `406c32fdf599ee051e1be4c19aa02dc514d5e8ff`.
The diff whitespace check and conflict-marker scan both passed.

| Automatically merged file | Changes from each side and result |
| --- | --- |
| `whatsapp/README.md` | PR: held switch and polling rows. Main: quotes, reactions and notice settings. Both retained; no clash. |
| `whatsapp/adapter/src/inbound.js` | PR: announced-read tracking and unannounced fallback. Main: notice admission and delivery observations. Both retained; no clash. |
| `whatsapp/adapter/src/index.js` | PR: one held-switch parse and startup warning. Main: notice wiring, reactions and quoted replies. Both retained; no clash. |
| `whatsapp/adapter/test/fakes.js` | PR: inbox failure injection. Main: quote headers, registration controls, reply text and delivered-byte overrides. Both retained; no collision. |
| `whatsapp/gateway/README.md` | PR: switch semantics, polling, pong-reset ladder and staged rollback. Main: replay, quotes and migration 0005. Both retained; no clash. |
| `whatsapp/gateway/test/wake.test.ts` | PR: all 18 switch cases. Main: adopted-status/lost-answer replay case. Both retained; no duplicate test names or conflicting assertions. |

The reviewer read all 4,011 lines of these six merged files, plus both
parent diffs. The startup warning remains before the first tick. Held rollback
closes sockets with WAKE off, waits, then removes the binding while retaining
the class. Migration 0005 precedes deployment of the current gateway and can
remain after rollback; the procedures do not contradict each other.
WAKE is parsed once in `wake-client.ts`; the entry point forwards it and
gateway routes consume the resulting `enabled` flag.

## Verification

Node from the city wrapper: `pnpm exec node --version` = **v24.21.0**.
All suites ran sequentially with `GC_BIN` unset, using local fakes.
No unsupported-engine warning occurred. The failed full adapter run printed
pnpm's missing-node_modules warning after the test failure; no dependency
installation was needed to run its dependency-free Node tests.

Every command and exit code is retained in [validation.log](validation.log).
It contains selected status lines, not the full diagnostic transcripts.

| Directory | Exact command | Result | Exit |
| --- | --- | --- | --- |
| `whatsapp/gateway` | `env -u GC_BIN pnpm test` | 351 passed, 6 files | 0 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm typecheck` | passed | 0 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm build` | dry run completed | 0 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm exec vitest run test/wake.test.ts -t gp-urx1` | 18 passed, 34 skipped by filter | 0 |
| `whatsapp/adapter` | `env -u GC_BIN pnpm test` | 430 passed, 1 failed, 431 total | 1 |
| `whatsapp/adapter` | `env -u GC_BIN pnpm exec node --test --test-concurrency=1 --test-name-pattern=gp-urx1 test/link.test.js test/link-engine.test.js` | 11 passed | 0 |
| `whatsapp/adapter` | `env -u GC_BIN pnpm exec node --test --test-concurrency=1 test/never-silent-held-link.test.js` | three runs: 6/7, 7/7, 7/7 | 1, 0, 0 |
| `<clone>` | `env -u GC_BIN -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest whatsapp/tests -q` | 107 passed | 0 |

### Lost-wake test D

Original test D passed in the full suite and all three isolated merge runs.
It checks the notice clock and the read count before advancing the clock
again. The new ordinary-poll cadence therefore does not contradict its
read-count assertion.

An observation-only copy outside the repository retained all assertions,
used the same imported fixtures, and added one diagnostic immediately after
the safety-read count assertion. The original test file was unchanged.
The filtered observation passed 1/1, exit 0, using:

```text
env -u GC_BIN pnpm exec node --test --test-concurrency=1 --test-name-pattern='^D:' <tmp>/held-link-observation.test.mjs
```

It measured the status fields:

```json
{"mode":"poll","state":"up","since":1793804700000,"last_wake_at":null,"wakes":0,"reconnects":0,"reason":"unannounced_delivery"}
```

These fields correspond to `poll (up; unannounced_delivery)`.
The original diagnostic remained: provider sent +0, adapter admitted
+300000 ms, first owner line +480000 ms.

### Reconnect test C and alternating comparison

The full adapter suite timed out at
`whatsapp/adapter/test/never-silent-held-link.test.js:128`, waiting for
the connection to become up after the gateway was enabled and the fake
clock advanced. The first isolated merge run reproduced that same failure.

The entire main tree was obtained with `git archive` in a temporary
directory. Each row below ran the same isolated-file command from the
verification table. The three load values are the 1/5/15-minute averages
from `uptime` immediately before that run.

| Order | Revision | Pass / fail | Exit | Load averages |
| --- | --- | --- | --- | --- |
| 1 | Merge | 6 / 1 | 1 | 5.02 / 14.71 / 24.26 |
| 2 | Main | 7 / 0 | 0 | 7.65 / 13.22 / 22.93 |
| 3 | Merge | 7 / 0 | 0 | 9.03 / 12.97 / 22.50 |
| 4 | Main | 7 / 0 | 0 | 6.31 / 11.44 / 21.22 |
| 5 | Merge | 7 / 0 | 0 | 5.39 / 10.65 / 20.54 |
| 6 | Main | 7 / 0 | 0 | 5.11 / 10.25 / 20.17 |

An initial partial-main archive failed before test execution because
`whatsapp/media-types.json` was absent. It is retained in the log as a setup
failure and excluded from the six valid comparisons above. The full archive
corrected the setup before main's first valid run.

The intermittent failure was observed only at the merge, so the requested
R4 stop applies. These small samples do not prove a production defect.
The source suggests a possible missing synchronization barrier between the
parallel inbox request and the refused upgrade; no speculative fix was made.

## Review and handoff

One STANDARD review on the worker's own model examined the merge resolution
and the six shared files. Counts: **0 BLOCKER, 1 MAJOR, 0 MINOR, 0 NIT**.
The single MAJOR is the reproduced reconnect acceptance failure, with outcome
**NOT SHOWN as a production defect; validation BLOCKED**. No independent
source defect was established. See [self-review.md](self-review.md).

Two minimal follow-up scopes were identified:

1. Synchronize the test with completion of the refused upgrade before the
   next clock jump, preserving production behavior and notice assertions.
2. Capture connection transitions and retry deadlines, then change production
   scheduling only if a violated rule is demonstrated. Preserve pong-only
   reset of the reconnect ladder.

Scope items: 1 merge DONE locally; 2 changelog DONE; 3 six-file reading DONE;
4 verification DONE with the recorded blocker; 5 PR body/publication NOT DONE
because R4 stops publication. The existing draft PR was neither pushed nor
edited. No new PR, ready transition, merge or deployment occurred.

Public-repository check: done, 0 content redactions required. New prose uses
only the PR's public bead of record and placeholder local paths. The validation
excerpt has no private paths or real chat, phone or Slack identifiers.
