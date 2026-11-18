# Reachable test plumbing evidence

Bead: gp-xopr. Builder self-review: STANDARD. Coordinator gate: DEEP, because the authorized fix changes hook source.

- Base: `226a0f34049fc5148a531530a063cdc063348235`.
- Tested implementation head: `1a67ac96ac767429ad1af03b9b19695a17016d70`.
- The final PR adds this evidence in a separate commit; its full tip is recorded in the PR and handoff.
- `pnpm exec node --version`: `v24.21.0`; `node --version`: `v26.12.1`.
- No unsupported-engine warning appeared. One suite ran at a time. All cities created by reachable tests were temporary.

## Change and scope

The performance test remains in `reachable/test/bounds.test.cjs:110` with its original body, 256 MiB fixture, 100 warm samples, 20 complete-process samples, both p95 < 100 ms assertions, and both diagnostics unchanged. Only its declaration changes: unless `REACHABLE_PERF` is exactly `1`, TAP reports `SKIP performance check: run make test-reachable-perf (REACHABLE_PERF=1)`.

`make test-reachable-perf` selects that test by name and enables it. The default `make test` runs reachable and the complete existing Python directory list sequentially, records both statuses, prints their summary, and fails if either failed. It invokes the same reachable command directly so GNU make dry runs cannot execute the entire combined recipe. `test-reachable`, `PYTEST_RUNNER`, `PYTEST_DIRS`, `PYTEST_ARGS`, and the Python command's `env -u GC_TEMPLATE` remain intact.

The authorized hook fix changes only archiveWorker: nine added lines and one removed line in `reachable/reachable.cjs`. It retries EAGAIN after a 1 ms wait without advancing the partial-write offset. The existing parent timeout continues to enforce the remaining deadline. Two regression tests are added to `reachable/test/review.test.cjs`. Rule text, modes, reachable/README.md, pack manifest, commands, doctor, template fragments, and workflows remain byte-identical to the base.

## EAGAIN: outcome A, diagnosed and fixed

The baseline failure is `reachable/test/review.test.cjs:97`, **review 4: archive replay advances in bounded chunks without dropping rows**. The failing call is line 106. It is a real fault in the hook's archive decoder, not a fixture write. Coordinates in the diagnosis below refer to the base; the existing test moves to line 98 in the implementation.

The stack from [Node 24 suite run 4](./initial-node24-suite-4.log) contains:

```text
Error: EAGAIN: resource temporarily unavailable, write
    at Object.writeSync (node:fs:977:3)
    at Gunzip.<anonymous> ([eval]:12:65)
    at archiveSlice (<clone>/reachable/reachable.cjs:60:10)
    at readEvents (<clone>/reachable/reachable.cjs:161:26)
    at TestContext.<anonymous> (<clone>/reachable/test/review.test.cjs:106:20)
```

The generated child program's `[eval]:12:65` maps to `reachable/reachable.cjs:54`: the loop handles partial writes but lets EAGAIN escape. The parent uses `execFileSync`, `stdio: ['ignore', 'pipe', 'pipe']`, a deadline-bound timeout, and an 8 MiB + 65536 output limit. Its thrown child error propagates through archiveSlice and readEvents.

A startup-only probe would be misleading here. The native [fd probe](./pipe-flags.c) inspects fd 1 without spawning or modifying it. The [archive diagnostic](./archive-pipe-probe.cjs) extracts the unchanged worker and uses the same spawn options and 8 MiB slice. In [Node 24's 40 diagnostic slices](./native-probe-node24.log), one failed: flags were 2 (blocking) at startup and **65542, with O_NONBLOCK set, at EAGAIN**. SO_SNDTIMEO was zero at both observations. [Node 26's 40 slices](./native-probe-node26.log) had no failures. These diagnostic counts are separate from the required suite repetitions.

The native diagnostic was run before the hook edit. Reproduce it on macOS against the base source, with the Node header directory substituted:

```sh
git show 226a0f34049fc5148a531530a063cdc063348235:reachable/reachable.cjs > <tmp>/reachable-base.cjs
clang -shared -fPIC -undefined dynamic_lookup -I <node-headers> assets/pr-evidence/gp-xopr/pipe-flags.c -o <tmp>/pipe-flags.node
pnpm exec node assets/pr-evidence/gp-xopr/archive-pipe-probe.cjs <tmp>/reachable-base.cjs <tmp>/pipe-flags.node
node assets/pr-evidence/gp-xopr/archive-pipe-probe.cjs <tmp>/reachable-base.cjs <tmp>/pipe-flags.node
```

At baseline `runHook:483` the exception yields `{}`; `errorLog:444` classifies it as `measurement_failed`. Because readEvents throws before `syncEvents` assigns the cursor and before state is saved, the next call retries the same slice. Persistent backpressure could therefore stall replay; the failure does not advance past unread rows. The retry fix leaves this call/save ordering unchanged.

The coordinator was notified before any Makefile or test source edit and subsequently authorized the bounded retry in this bead. The child retries only EAGAIN; every other write error still escapes. Each retry waits 1 ms, while archiveSlice's existing execFileSync timeout bounds the entire child to the remaining deadline. No deadline value or fail-open behavior changes.

The new filled-pipe regression pauses the reader, forces real short writes on non-blocking stdout, waits for a real kernel EAGAIN, then drains the reader and checks every byte. A second regression keeps writes blocked, proves the child retried, and verifies that the unchanged parent timeout produces `{}`, `measurement_failed`, and the unchanged cursor. Both failed against the base hook before the fix. Their final results and the 20-run replay comparison appear below.

### Required initial repetitions

Every invocation ran in `<clone>/reachable/test`, at the unchanged base. For each Node version, five full suites ran first, followed by five repetitions of the named test.

```sh
pnpm exec node --test --test-reporter=tap
pnpm exec node --test --test-reporter=tap --test-name-pattern='review 4: archive replay advances in bounded chunks without dropping rows' review.test.cjs
node --test --test-reporter=tap
node --test --test-reporter=tap --test-name-pattern='review 4: archive replay advances in bounded chunks without dropping rows' review.test.cjs
```

Table identifiers beginning with 24 use Node 24.21.0; 26 uses Node 26.12.1. Each uptime was captured immediately before that run. E = archive EAGAIN above; T = timing bound at `bounds.test.cjs:110`; D = the unchanged `review 7: installed outer deadline leaves time to persist fail-open rollback` timing test. This change addresses E and moves T to the opt-in check. The D observation is retained; its test is unchanged.

| Run/log | Uptime | TAP counts | Exit | Failures |
| --- | --- | --- | --- | --- |
| [24-suite-1](./initial-node24-suite-1.log) | 23:31  up 7 days, 10:54, 5 users, load averages: 11.99 16.27 13.34 | tests 91; pass 91; fail 0; skipped 0 | 0 | — |
| [24-suite-2](./initial-node24-suite-2.log) | 23:32  up 7 days, 10:55, 5 users, load averages: 10.90 15.90 13.24 | tests 91; pass 91; fail 0; skipped 0 | 0 | — |
| [24-suite-3](./initial-node24-suite-3.log) | 23:32  up 7 days, 10:55, 5 users, load averages: 10.11 15.48 13.13 | tests 91; pass 91; fail 0; skipped 0 | 0 | — |
| [24-suite-4](./initial-node24-suite-4.log) | 23:32  up 7 days, 10:55, 5 users, load averages: 8.92 14.95 12.98 | tests 91; pass 89; fail 2; skipped 0 | 1 | E, D |
| [24-suite-5](./initial-node24-suite-5.log) | 23:32  up 7 days, 10:55, 5 users, load averages: 8.44 14.75 12.93 | tests 91; pass 91; fail 0; skipped 0 | 0 | — |
| [24-named-1](./initial-node24-named-1.log) | 23:32  up 7 days, 10:55, 5 users, load averages: 8.89 14.54 12.88 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [24-named-2](./initial-node24-named-2.log) | 23:32  up 7 days, 10:56, 5 users, load averages: 8.50 14.36 12.83 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [24-named-3](./initial-node24-named-3.log) | 23:33  up 7 days, 10:56, 5 users, load averages: 8.38 14.24 12.79 | tests 1; pass 0; fail 1; skipped 0 | 1 | E |
| [24-named-4](./initial-node24-named-4.log) | 23:33  up 7 days, 10:56, 5 users, load averages: 8.35 14.14 12.77 | tests 1; pass 0; fail 1; skipped 0 | 1 | E |
| [24-named-5](./initial-node24-named-5.log) | 23:33  up 7 days, 10:56, 5 users, load averages: 8.35 14.14 12.77 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [26-suite-1](./initial-node26-suite-1.log) | 23:33  up 7 days, 10:56, 5 users, load averages: 8.16 14.00 12.73 | tests 91; pass 90; fail 1; skipped 0 | 1 | T |
| [26-suite-2](./initial-node26-suite-2.log) | 23:33  up 7 days, 10:56, 5 users, load averages: 7.91 13.59 12.61 | tests 91; pass 90; fail 1; skipped 0 | 1 | T |
| [26-suite-3](./initial-node26-suite-3.log) | 23:33  up 7 days, 10:56, 5 users, load averages: 7.40 13.20 12.49 | tests 91; pass 89; fail 2; skipped 0 | 1 | T, E |
| [26-suite-4](./initial-node26-suite-4.log) | 23:33  up 7 days, 10:57, 5 users, load averages: 7.18 12.96 12.41 | tests 91; pass 90; fail 1; skipped 0 | 1 | T |
| [26-suite-5](./initial-node26-suite-5.log) | 23:34  up 7 days, 10:57, 5 users, load averages: 6.66 12.47 12.25 | tests 91; pass 90; fail 1; skipped 0 | 1 | T |
| [26-named-1](./initial-node26-named-1.log) | 23:34  up 7 days, 10:57, 5 users, load averages: 7.36 12.35 12.21 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [26-named-2](./initial-node26-named-2.log) | 23:34  up 7 days, 10:57, 5 users, load averages: 7.00 12.11 12.12 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [26-named-3](./initial-node26-named-3.log) | 23:34  up 7 days, 10:57, 5 users, load averages: 6.53 11.84 12.03 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [26-named-4](./initial-node26-named-4.log) | 23:34  up 7 days, 10:58, 5 users, load averages: 8.41 12.09 12.11 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |
| [26-named-5](./initial-node26-named-5.log) | 23:35  up 7 days, 10:58, 5 users, load averages: 8.06 11.89 12.04 | tests 1; pass 1; fail 0; skipped 0 | 0 | — |

EAGAIN frequency: Node 24 full suite 1/5, named test 2/5; Node 26 full suite 1/5, named test 0/5. Failing runs had one-minute load averages 7.40–8.92. Natural load reproduced the fault, so synthetic load was unnecessary.

## Backpressure regression and repeated replay

All commands below ran in the selected tree's `reachable/test` directory. The base regression tree pairs the exact base hook with the newly added tests. The baseline replay tree is the unmodified `git archive` of the base.

```sh
pnpm exec node --test --test-reporter=tap --test-name-pattern='^archive backpressure:' review.test.cjs
node --test --test-reporter=tap --test-name-pattern='^archive backpressure:' review.test.cjs
```

The Node 24 regression command ran against both base and head; the Node 26 command ran against head.

| Run/log | Uptime | TAP counts | Exit |
| --- | --- | --- | --- |
| [backpressure-base](./backpressure-base.log) | 0:17  up 7 days, 11:40, 5 users, load averages: 7.03 10.77 10.27 | tests 2; pass 0; fail 2; skipped 0 | 1 |
| [backpressure-head24](./backpressure-head24.log) | 0:17  up 7 days, 11:40, 5 users, load averages: 7.03 10.77 10.27 | tests 2; pass 2; fail 0; skipped 0 | 0 |
| [backpressure-head26](./backpressure-head26.log) | 0:17  up 7 days, 11:40, 5 users, load averages: 7.03 10.77 10.27 | tests 2; pass 2; fail 0; skipped 0 | 0 |

The filled-pipe case requires actual short writes and a kernel EAGAIN before it allows the reader to drain. The permanent-backpressure case verifies retries, unchanged fail-open output and error classification, and the unchanged cursor after the parent's existing deadline.

The following exact command ran 20 times against the base, then 20 times against head, under Node 24.21.0:

```sh
pnpm exec node --test --test-reporter=tap --test-name-pattern='review 4: archive replay advances in bounded chunks without dropping rows' review.test.cjs
```

Each run selects one test, with zero skips. Base: **8 passed, 12 failed with EAGAIN**. Head: **20 passed, zero failed**. No synthetic busy loops were started.

| Run | Base result/log | Base uptime | Head result/log | Head uptime |
| --- | --- | --- | --- | --- |
| 1 | [PASS, exit 0](./replay-base-01.log) | 0:17  up 7 days, 11:41, 5 users, load averages: 7.11 10.72 10.25 | [PASS, exit 0](./replay-head-01.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 8.27 10.39 10.17 |
| 2 | [PASS, exit 0](./replay-base-02.log) | 0:17  up 7 days, 11:41, 5 users, load averages: 6.86 10.61 10.22 | [PASS, exit 0](./replay-head-02.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 8.01 10.30 10.14 |
| 3 | [FAIL, exit 1](./replay-base-03.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 6.71 10.52 10.19 | [PASS, exit 0](./replay-head-03.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 7.92 10.24 10.12 |
| 4 | [PASS, exit 0](./replay-base-04.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 6.71 10.52 10.19 | [PASS, exit 0](./replay-head-04.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 7.54 10.08 10.06 |
| 5 | [FAIL, exit 1](./replay-base-05.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 7.53 10.57 10.21 | [PASS, exit 0](./replay-head-05.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 7.00 9.88 9.99 |
| 6 | [PASS, exit 0](./replay-base-06.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.73 10.97 10.35 | [PASS, exit 0](./replay-head-06.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 7.40 9.92 10.00 |
| 7 | [PASS, exit 0](./replay-base-07.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.59 10.92 10.34 | [PASS, exit 0](./replay-head-07.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 7.92 9.94 10.01 |
| 8 | [FAIL, exit 1](./replay-base-08.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.47 10.87 10.32 | [PASS, exit 0](./replay-head-08.log) | 0:19  up 7 days, 11:43, 5 users, load averages: 7.52 9.82 9.97 |
| 9 | [PASS, exit 0](./replay-base-09.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.47 10.87 10.32 | [PASS, exit 0](./replay-head-09.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.32 9.74 9.94 |
| 10 | [PASS, exit 0](./replay-base-10.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.49 10.83 10.31 | [PASS, exit 0](./replay-head-10.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.45 9.73 9.93 |
| 11 | [FAIL, exit 1](./replay-base-11.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.21 10.75 10.29 | [PASS, exit 0](./replay-head-11.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.46 9.66 9.90 |
| 12 | [FAIL, exit 1](./replay-base-12.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 9.21 10.75 10.29 | [PASS, exit 0](./replay-head-12.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.59 9.65 9.90 |
| 13 | [PASS, exit 0](./replay-base-13.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 8.80 10.64 10.25 | [PASS, exit 0](./replay-head-13.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.62 9.62 9.89 |
| 14 | [FAIL, exit 1](./replay-base-14.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 8.65 10.57 10.23 | [PASS, exit 0](./replay-head-14.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.49 9.56 9.87 |
| 15 | [FAIL, exit 1](./replay-base-15.log) | 0:18  up 7 days, 11:41, 5 users, load averages: 8.65 10.57 10.23 | [PASS, exit 0](./replay-head-15.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.49 9.49 9.84 |
| 16 | [FAIL, exit 1](./replay-base-16.log) | 0:18  up 7 days, 11:42, 5 users, load averages: 8.65 10.57 10.23 | [PASS, exit 0](./replay-head-16.log) | 0:20  up 7 days, 11:43, 5 users, load averages: 7.13 9.38 9.80 |
| 17 | [FAIL, exit 1](./replay-base-17.log) | 0:18  up 7 days, 11:42, 5 users, load averages: 8.65 10.57 10.23 | [PASS, exit 0](./replay-head-17.log) | 0:20  up 7 days, 11:44, 5 users, load averages: 7.36 9.39 9.80 |
| 18 | [FAIL, exit 1](./replay-base-18.log) | 0:18  up 7 days, 11:42, 5 users, load averages: 8.65 10.57 10.23 | [PASS, exit 0](./replay-head-18.log) | 0:21  up 7 days, 11:44, 5 users, load averages: 6.77 9.20 9.72 |
| 19 | [FAIL, exit 1](./replay-base-19.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 8.03 10.38 10.16 | [PASS, exit 0](./replay-head-19.log) | 0:21  up 7 days, 11:44, 5 users, load averages: 6.71 9.14 9.70 |
| 20 | [FAIL, exit 1](./replay-base-20.log) | 0:19  up 7 days, 11:42, 5 users, load averages: 8.03 10.38 10.16 | [PASS, exit 0](./replay-head-20.log) | 0:21  up 7 days, 11:44, 5 users, load averages: 6.41 9.04 9.66 |

## Verification results

All three alternating pairs used the shell's Node 26.12.1. Base was extracted with `git archive 226a0f34049fc5148a531530a063cdc063348235` into `<tmp>/base`; it was never checked out in the lane. The head was the tested implementation above.

Each invocation was exactly `env -u GC_BIN -u GC_TEMPLATE make test`, from the corresponding tree. Each uptime was captured immediately before the command. No synthetic load was needed.

| Run/log | Uptime | Reachable counts | Python result | CLI p95 | Make exit |
| --- | --- | --- | --- | --- | --- |
| [pair-1-base](./pair-1-base.log) | 0:21  up 7 days, 11:44, 5 users, load averages: 5.81 8.83 9.58 | tests 91; pass 90; fail 1; skipped 0 | not run | 112.772ms | 2 |
| [pair-1-head](./pair-1-head.log) | 0:21  up 7 days, 11:44, 5 users, load averages: 5.80 8.67 9.51 | tests 93; pass 92; fail 0; skipped 1 | 2978 passed, 131 skipped, 2 warnings, 10734 subtests passed in 620.84s (0:10:20) | skipped | 0 |
| [pair-2-base](./pair-2-base.log) | 0:32  up 7 days, 11:55, 5 users, load averages: 5.32 6.34 7.84 | tests 91; pass 90; fail 1; skipped 0 | not run | 132.466ms | 2 |
| [pair-2-head](./pair-2-head.log) | 0:33  up 7 days, 11:56, 5 users, load averages: 6.17 6.44 7.80 | tests 93; pass 92; fail 0; skipped 1 | 2978 passed, 131 skipped, 2 warnings, 10734 subtests passed in 631.35s (0:10:31) | skipped | 0 |
| [pair-3-base](./pair-3-base.log) | 0:44  up 7 days, 12:07, 5 users, load averages: 8.03 7.37 7.61 | tests 91; pass 90; fail 1; skipped 0 | not run | 117.269ms | 2 |
| [pair-3-head](./pair-3-head.log) | 0:44  up 7 days, 12:07, 5 users, load averages: 7.48 7.28 7.58 | tests 93; pass 92; fail 0; skipped 1 | 2978 passed, 131 skipped, 2 warnings, 10734 subtests passed in 559.40s (0:09:19) | skipped | 0 |

All base runs failed the timing assertion and produced no Python output. All head runs passed both groups and printed `Test summary: reachable=0; Python=0 (0=pass, nonzero=fail)`. The performance test was visibly skipped with its required reason.

### Failure status, opt-in target, and exact switch

The negative checks ran in a temporary copy of the implementation. A temporary Python file defined one deliberately failing test and one passing marker. One extra temporary Node test failed only when REACHABLE_FORCE_FAILURE=1. Neither fixture is part of the shipped suites.

```sh
# negative-python (cwd: <tmp>/negative)
env -u GC_BIN -u GC_TEMPLATE make test PYTEST_DIRS=negative-tests 'PYTEST_ARGS=-q -k intentional_failure'
# negative-reachable (cwd: <tmp>/negative)
env -u GC_BIN -u GC_TEMPLATE REACHABLE_FORCE_FAILURE=1 make test PYTEST_DIRS=negative-tests 'PYTEST_ARGS=-q -k success_marker'
# opt-in-loaded (cwd: <clone>)
make test-reachable-perf
# switch-zero (cwd: <clone>/reachable/test)
env REACHABLE_PERF=0 node --test --test-reporter=tap bounds.test.cjs
# switch-one (cwd: <clone>/reachable/test)
env REACHABLE_PERF=1 node --test --test-reporter=tap bounds.test.cjs
```

| Run/log | Uptime | Counts | Exit |
| --- | --- | --- | --- |
| [negative-python](./negative-python.log) | 0:54  up 7 days, 12:17, 5 users, load averages: 6.21 6.57 7.13 | tests 94; pass 93; fail 0; skipped 1; 1 failed, 1 deselected in 0.02s | 2 |
| [negative-reachable](./negative-reachable.log) | 0:54  up 7 days, 12:17, 5 users, load averages: 7.58 6.89 7.23 | tests 94; pass 92; fail 1; skipped 1; 1 passed, 1 deselected in 0.00s | 2 |
| [opt-in-loaded](./opt-in-loaded.log) | 0:55  up 7 days, 12:18, 5 users, load averages: 8.62 7.17 7.32 | tests 1; pass 0; fail 1; skipped 0 | 2 |
| [switch-zero](./switch-zero.log) | 0:55  up 7 days, 12:18, 5 users, load averages: 8.89 7.25 7.35 | tests 10; pass 9; fail 0; skipped 1 | 0 |
| [switch-one](./switch-one.log) | 0:55  up 7 days, 12:18, 5 users, load averages: 8.89 7.25 7.35 | tests 10; pass 9; fail 1; skipped 0 | 1 |

The deliberate Python failure reported `reachable=0; Python=1`. The deliberate reachable failure still ran Python and reported `reachable=1; Python=0`. Both make invocations exited 2.

- opt-in-loaded: 256 MiB, 100 warm decisions: p95=1.726ms; 256 MiB, 20 complete hook processes: p95=127.631ms.
- switch-one: 256 MiB, 100 warm decisions: p95=1.458ms; 256 MiB, 20 complete hook processes: p95=120.245ms.

The opt-in performance failure under load is expected: it proves the unchanged 100 ms assertion still runs. REACHABLE_PERF=0 visibly skips exactly one test; REACHABLE_PERF=1 runs it. No load-average-under-2 window was observed for a quiet opt-in run, so that optional run was not performed.

Machine-readable results and every exact command/exit pair are retained in `verification-runs.json` and `final-completion-manifest.txt`. Logs redact private paths, remove trailing whitespace, and end in exactly one newline.

## Final change stat

```text
 Makefile                                                         |   14 +-
 assets/pr-evidence/gp-xopr/README.md                             |  350 ++++++++++++++++
 assets/pr-evidence/gp-xopr/archive-pipe-probe.cjs                |   41 ++
 assets/pr-evidence/gp-xopr/backpressure-base.log                 |   85 ++++
 assets/pr-evidence/gp-xopr/backpressure-head24.log               |   22 +
 assets/pr-evidence/gp-xopr/backpressure-head26.log               |   22 +
 assets/pr-evidence/gp-xopr/before-hook-fix-pair-1-base.log       |  635 ++++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/before-hook-fix-pair-1-head.log       |  670 +++++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/before-hook-fix-pair-2-base.log       |  635 ++++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/before-hook-fix-pair-2-head.log       |  670 +++++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/before-hook-fix-runs.json             |   92 ++++
 assets/pr-evidence/gp-xopr/check-make-dry-run.py                 |   27 ++
 assets/pr-evidence/gp-xopr/dry-run-after.log                     |    1 +
 assets/pr-evidence/gp-xopr/dry-run-before.log                    |    6 +
 assets/pr-evidence/gp-xopr/dry-run.log                           |    5 +
 assets/pr-evidence/gp-xopr/final-completion-manifest.txt         |   54 +++
 assets/pr-evidence/gp-xopr/initial-completion-manifest.txt       |   20 +
 assets/pr-evidence/gp-xopr/initial-node24-named-1.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node24-named-2.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node24-named-3.log            |   72 ++++
 assets/pr-evidence/gp-xopr/initial-node24-named-4.log            |   72 ++++
 assets/pr-evidence/gp-xopr/initial-node24-named-5.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node24-suite-1.log            |  561 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node24-suite-2.log            |  561 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node24-suite-3.log            |  561 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node24-suite-4.log            |  637 ++++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node24-suite-5.log            |  561 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node26-named-1.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node26-named-2.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node26-named-3.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node26-named-4.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node26-named-5.log            |   16 +
 assets/pr-evidence/gp-xopr/initial-node26-suite-1.log            |  577 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node26-suite-2.log            |  577 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node26-suite-3.log            |  633 ++++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node26-suite-4.log            |  577 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-node26-suite-5.log            |  577 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/initial-runs.json                     |  340 +++++++++++++++
 assets/pr-evidence/gp-xopr/native-probe-node24.log               |   40 ++
 assets/pr-evidence/gp-xopr/native-probe-node26.log               |   40 ++
 assets/pr-evidence/gp-xopr/negative-python.log                   |  592 ++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/negative-reachable.log                |  595 ++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/opt-in-loaded.log                     |   34 ++
 assets/pr-evidence/gp-xopr/pair-1-base.log                       |  579 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pair-1-head.log                       |  625 +++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pair-2-base.log                       |  579 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pair-2-head.log                       |  625 +++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pair-3-base.log                       |  579 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pair-3-head.log                       |  625 +++++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pipe-flags.c                          |   30 ++
 assets/pr-evidence/gp-xopr/pre-fix-base.log                      |  579 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/pre-fix-recursive-head.log            |  565 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/privacy-check.json                    |    6 +
 assets/pr-evidence/gp-xopr/replay-base-01.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-02.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-03.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-04.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-05.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-06.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-07.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-08.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-09.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-10.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-11.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-12.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-13.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-base-14.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-15.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-16.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-17.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-18.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-19.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-base-20.log                    |   72 ++++
 assets/pr-evidence/gp-xopr/replay-head-01.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-02.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-03.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-04.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-05.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-06.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-07.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-08.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-09.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-10.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-11.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-12.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-13.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-14.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-15.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-16.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-17.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-18.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-19.log                    |   16 +
 assets/pr-evidence/gp-xopr/replay-head-20.log                    |   16 +
 assets/pr-evidence/gp-xopr/scope-check.json                      |   16 +
 assets/pr-evidence/gp-xopr/switch-one.log                        |   88 ++++
 assets/pr-evidence/gp-xopr/switch-zero.log                       |   70 ++++
 assets/pr-evidence/gp-xopr/timeout-extension-base.log            |  579 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/timeout-extension-head-incomplete.log |  568 +++++++++++++++++++++++++
 assets/pr-evidence/gp-xopr/verification-runs.json                | 1043 ++++++++++++++++++++++++++++++++++++++++++++++
 reachable/reachable.cjs                                          |   10 +-
 reachable/test/bounds.test.cjs                                   |    2 +-
 reachable/test/review.test.cjs                                   |   69 ++-
 102 files changed, 19627 insertions(+), 6 deletions(-)
```

## Scope and dry-run regression

The scope check verifies that only bounds.test.cjs line 110 changes in that file; all fixture, sample, diagnostic, and assertion bytes remain intact. In reachable.cjs, everything before archiveWorker and everything from archiveSlice onward is byte-identical to the base. PYTEST_RUNNER, PYTEST_DIRS, PYTEST_ARGS, and the standalone test-reachable command are also byte-identical.

An implementation regression was found and fixed before the final verification: placing `$(MAKE)` inside the combined shell recipe makes GNU make execute the entire recipe even under `make -n`. The existing Python suite exercises dry runs, exposing recursive test execution. The final recipe invokes the unchanged reachable command directly.

[check-make-dry-run.py](./check-make-dry-run.py) checks the entire class of dry-run interpreter execution using inert executables in a temporary directory. Against implementation `0a129bafccab85b9f1daed6e7715038b0768428b`, it observed two interpreter executions and exited 1. Against the corrected Makefile, it observed zero executions and exited 0. Fix: `92aa83311ae8d31ef85a1797c8bdd1bb2a50a0f4`; the Makefile is unchanged in the final implementation.

```sh
python3 -I assets/pr-evidence/gp-xopr/check-make-dry-run.py <tmp>/pre-fix-Makefile
python3 -I assets/pr-evidence/gp-xopr/check-make-dry-run.py Makefile
```

## Additional captured attempts

These receipts are retained for completeness and are not counted as the final three comparisons:

- `pre-fix-base.log`: base timing failure, 90 passed / 1 failed, CLI p95 114.242 ms, make exit 2.
- `pre-fix-recursive-head.log`: the first combined recipe was stopped after the dry-run recursion was identified. Node completed with 90 passed / 1 skipped; Python did not complete. No full-run success is claimed.
- `timeout-extension-base.log` and `timeout-extension-head-incomplete.log`: a base timing failure and an interrupted head attempt while the verification watchdog was increased after a full run took nearly ten minutes. The incomplete run is not counted as a result.
- `before-hook-fix-pair-*.log` and `before-hook-fix-runs.json`: two complete comparisons at implementation `92aa83311ae8d31ef85a1797c8bdd1bb2a50a0f4`, before the authorized hook fix was applied. Both head runs reported the real EAGAIN failure and still completed Python: 2978 passed, 131 skipped, 10734 passing subtests, make exit 2.

The initial Node 24 suite run 4 also observed the unchanged outer-deadline test at baseline review.test.cjs:321 return a null child status instead of 0. Its cause was not investigated as part of this change; the complete TAP receipt is retained. No change was made to that test.

## CI scope

`.github/workflows/ci.yml:50` runs:

```sh
python3 -m pytest tests contributing-to-gascity/tests gascity/tests discord/tests github/tests oversight-rig/tests slack-full/tests slack-channel/tests pr-pipeline/tests profiler/tests whatsapp/tests gchat/tests scripts/tests follow-through/tests -q
```

Lines 52–54 are:

```yaml
- name: Run follow-through Node tests on Linux
  working-directory: follow-through/test
  run: node --test
```

No workflow runs anything under reachable. CI remains unchanged; adding reachable functional coverage is a separate follow-up.

## Self-review

Round 1, self-review on the builder's model, covers the complete source diff, evidence, and proposed PR body. Final verdict: CLEAN. Findings: 0 BLOCKER, 1 MAJOR fixed, 0 MINOR, 0 NIT; zero outstanding findings.

| Severity | Location | Finding and outcome |
| --- | --- | --- |
| MAJOR | Makefile:30 at implementation 0a129bafccab85b9f1daed6e7715038b0768428b | A recursive make reference caused the combined recipe to execute under make -n. FIXED in 92aa83311ae8d31ef85a1797c8bdd1bb2a50a0f4. The class-level dry-run check is red before (two interpreter executions) and green after (zero). |

The review checked both failure-status paths with real failing selections, the visible skip reason and exact switch, the unchanged performance-test body, real pipe backpressure and partial writes, the unchanged parent deadline and fail-open behavior, all permitted file boundaries, and the public-repository restrictions. The EAGAIN diagnosis was resolved by the authorized fix; it is not left as a deferred finding. The coordinator's DEEP gate and second-model read remain separate from this self-review.
