# Alternating reconnect comparison

Measured 2026-11-13 on Node v24.21.0 (reported by `pnpm exec node --version`).
The merge is `920210cd6b3d2c38934d7149fcc60240be97ea04`; main is
`c26e664ec079f6a5e5cc9cab31a091500e4d6fd7`. Both tracked WhatsApp trees
were extracted with `git archive` into temporary directories; the branch's
source and tests were unchanged.

The load ruling changed the start threshold from 15 to 40 before the first
counted run. Each pair ran merge, then main, sequentially. A starting-load
difference over 8 would repeat both runs. No pair needed repetition.
One-minute load came from `uptime` immediately before every run. No other
visible test suite ran alongside this comparison.

Exact command, from each archived `whatsapp/adapter` directory:

```sh
env -u GC_BIN pnpm exec node --test --test-concurrency=1 test/never-silent-held-link.test.js
```

Result: **row b**. The merge failed 3/20 runs (15%); main failed 2/20
(10%). Every failure was the same reconnect wait in test C at
`whatsapp/adapter/test/never-silent-held-link.test.js:128`. These similar
rates reproduce the problem on main; they do not establish a merge-only
interaction. The required next check is the complete adapter suite.
No source, assertion, timeout or skip was changed for this comparison.

Merge load range: **12.28–31.31**. Main load range: **12.86–30.65**.
Every completed run executed all seven tests, including the two B subtests.
Each result below was read back from the completion manifest and the log.

| Pair | Merge load | Merge pass/fail | Exit | Main load | Main pass/fail | Exit | Load difference |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 1 | 31.31 | 7/0 | 0 | 24.87 | 7/0 | 0 | 6.44 |
| 2 | 23.03 | 6/1 | 1 | 19.12 | 6/1 | 1 | 3.91 |
| 3 | 29.13 | 7/0 | 0 | 29.93 | 7/0 | 0 | 0.80 |
| 4 | 30.71 | 7/0 | 0 | 26.02 | 7/0 | 0 | 4.69 |
| 5 | 24.55 | 7/0 | 0 | 30.65 | 7/0 | 0 | 6.10 |
| 6 | 28.46 | 7/0 | 0 | 23.81 | 7/0 | 0 | 4.65 |
| 7 | 19.66 | 7/0 | 0 | 22.50 | 7/0 | 0 | 2.84 |
| 8 | 18.86 | 7/0 | 0 | 15.34 | 7/0 | 0 | 3.52 |
| 9 | 20.22 | 7/0 | 0 | 15.24 | 7/0 | 0 | 4.98 |
| 10 | 16.12 | 7/0 | 0 | 15.86 | 7/0 | 0 | 0.26 |
| 11 | 13.71 | 7/0 | 0 | 13.75 | 7/0 | 0 | 0.04 |
| 12 | 14.23 | 7/0 | 0 | 12.86 | 7/0 | 0 | 1.37 |
| 13 | 12.28 | 7/0 | 0 | 14.69 | 7/0 | 0 | 2.41 |
| 14 | 17.93 | 6/1 | 1 | 15.95 | 7/0 | 0 | 1.98 |
| 15 | 16.52 | 7/0 | 0 | 18.48 | 7/0 | 0 | 1.96 |
| 16 | 26.38 | 7/0 | 0 | 29.25 | 7/0 | 0 | 2.87 |
| 17 | 30.57 | 7/0 | 0 | 28.55 | 7/0 | 0 | 2.02 |
| 18 | 25.02 | 7/0 | 0 | 23.06 | 6/1 | 1 | 1.96 |
| 19 | 22.12 | 6/1 | 1 | 19.04 | 7/0 | 0 | 3.08 |
| 20 | 15.46 | 7/0 | 0 | 13.03 | 7/0 | 0 | 2.43 |

## Failure durations

Test C is “dropping and reconnecting a held link preserves the outage and
notice marks”. Its failed assertion awaited link state `up` at line 128:3;
its declaration is at line 115. `waitFor` in `test/fakes.js:27–35` sets a
20,000 ms wall-clock deadline and throws only after that deadline passes.
The original test does not report the wait's precise start time. The table
therefore gives the measured whole-case duration and an honest bound on the
failed wait, rather than calling the whole case duration the wait duration.

| Revision / pair | Load | Failure line | Wait wall time bound (ms) | Whole case (ms) |
| --- | ---: | --- | ---: | ---: |
| merge / 2 | 23.03 | 128:3 | >20,000 and ≤22781.851 | 22781.851 |
| main / 2 | 19.12 | 128:3 | >20,000 and ≤23525.058 | 23525.058 |
| merge / 14 | 17.93 | 128:3 | >20,000 and ≤22737.064 | 22737.064 |
| main / 18 | 23.06 | 128:3 | >20,000 and ≤23289.781 | 23289.781 |
| merge / 19 | 22.12 | 128:3 | >20,000 and ≤23184.611 | 23184.611 |

Every failure reported `Error: timed out waiting for condition`. No other
case failed or was cancelled. The test-synchronization sensitivity was
reported separately; row b does not authorize changing it in this work.

Before these counted runs, an incomplete temporary archive omitted the
shared `whatsapp/media-types.json` and caused launcher errors before the
cases executed. Those setup attempts were excluded. The archive was
corrected, and the runner then required a seven-test result before proceeding.
