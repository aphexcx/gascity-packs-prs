# Bring-current follow-up validation

Status: **accepted with one documented cancellation**. The code and test assertions remain
exactly those of merge `920210cd6b3d2c38934d7149fcc60240be97ea04`.
Main is `c26e664ec079f6a5e5cc9cab31a091500e4d6fd7`; the original PR head is
`45af2199a03db93a3a758b19c2a01245d36ceba8`. The merge was preserved.
These changes add evidence only. Nothing was deployed or called live.

## Reconnect comparison and decision

The [20 alternating pairs](comparison.md) produced 3/20 failed merge runs
and 2/20 failed main runs. All failures were test C's wait for `up` at line
128. Every run executed all seven tests; every pair satisfied the revised
start-load rule (under 40, difference at most 8). This is **row b**: similar
rates on both revisions, with no merge-only failure established. The test's
timing sensitivity was reported separately. No behavior changed; no test
limit, assertion or skip changed.

## Verification

Node: `v24.21.0`, measured with `pnpm exec node --version`. Commands ran
sequentially through the city toolchain. `pnpm test` ran the adapter's own
script successfully; its nonzero result below is the test cancellation,
not a wrapper refusal. No unsupported-engine warning was emitted.

| Directory | Exact command | Result | Load | Exit |
| --- | --- | --- | ---: | ---: |
| `whatsapp/adapter` | `env -u GC_BIN pnpm test` | 431 total; 430 passed, 0 failed, 1 cancelled, 0 skipped | 13.80 | 1 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm test` | 351 passed, 6 files | 15.18 | 0 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm typecheck` | passed | 14.84 | 0 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm build` | dry run completed; no deployment | 14.46 | 0 |
| `whatsapp/gateway` | `env -u GC_BIN pnpm exec vitest run test/wake.test.ts -t gp-urx1` | 18 passed, 34 excluded by the requested filter | 16.98 | 0 |
| `whatsapp/adapter` | `env -u GC_BIN pnpm exec node --test --test-concurrency=1 --test-name-pattern=gp-urx1 test/link.test.js test/link-engine.test.js` | 11 passed | 17.70 | 0 |
| `whatsapp/adapter` | `env -u GC_BIN pnpm exec node --test --test-concurrency=1 test/never-silent-held-link.test.js` | 7 passed, including C and D | 21.31 | 0 |
| `<clone>` | `env -u GC_BIN -u GC_TEMPLATE uv run --no-project --with pytest python -m pytest whatsapp/tests -q` | 107 passed | 22.25 | 0 |

Each result was read from its log and completion manifest. The full adapter
run began at one-minute load **13.80** and took **341,709.893 ms**. Test C
passed in **3,696.102 ms**. Its sole cancellation was:

`audio survives SIGKILL at after-media-journal with one upload and one send`
at `whatsapp/adapter/test/files-out.test.js:432`, after **30,044.578 ms**
against that test's **30,000 ms** limit. The run is not represented as a
431/431 pass. The acceptance ruling on 2026-11-13 waived the full-pass
condition for exactly this cancellation after the six passing isolated runs
below. The overrun was **44.6 ms**; the timeout was not changed.

## Audio cancellation: required isolated comparison

The entire file ran three times at the merge and three times at main,
alternating, with one-minute load recorded before every run. The file and
`src/outbound-file.js` and `src/outbound.js` are identical between those
revisions. These supplementary runs use the original recorded-load rule;
the revised under-40/pair-gap rule applies to the 20 held-link pairs above.

Exact command, from each archived `whatsapp/adapter`:

```sh
env -u GC_BIN pnpm exec node --test --test-concurrency=1 test/files-out.test.js
```

| Order | Revision | Load | Pass / total | Cancelled | Seconds | Exit |
| --- | --- | ---: | ---: | ---: | ---: | ---: |
| 1 | Merge | 17.71 | 57 / 57 | 0 | 44.902 | 0 |
| 2 | Main | 13.29 | 57 / 57 | 0 | 66.334 | 0 |
| 3 | Merge | 27.25 | 57 / 57 | 0 | 76.142 | 0 |
| 4 | Main | 37.87 | 57 / 57 | 0 | 82.219 | 0 |
| 5 | Merge | 48.50 | 57 / 57 | 0 | 60.202 | 0 |
| 6 | Main | 35.31 | 57 / 57 | 0 | 38.708 | 0 |

The cancellation did not reproduce in any of these six whole-file runs.
It was reported separately; neither the file nor its timeout was changed.

## Lost-wake state

The unchanged original file passed 7/7 in the lane. A scratch copy of that
file added only a diagnostic read immediately after test D's safety-read
assertion; every original assertion remained. It ran with:

```sh
env -u GC_BIN pnpm exec node --test --test-concurrency=1 --test-name-pattern='^D:' test/observe-held-link.test.js
```

The selected case passed (1/1, exit 0). Observed fields were `mode: poll`,
`state: up`, `reason: unannounced_delivery`, `wakes: 0`, `reconnects: 0`.
The displayed status is **`poll (up; unannounced_delivery)`**. The diagnostic
copy exists only in the temporary archive; no branch test was edited.

## Scope and publication

The original merge and its complete changelog sections remain unchanged.
All six automatically merged files were read again against both parents'
changes, with no merge-resolution defect found. The native STANDARD review
is [recorded separately](follow-up-review.md).

The acceptance ruling authorizes the evidence commit and fast-forward update
of the existing draft PR. Every executable VERIFY line is complete; the PR
head, draft state and mergeability are read back after publication. The PR
remains draft; no new PR, merge, ready transition, deployment or live message
is included. **No behaviour changed.**
Public-repository check: done, 0 content redactions required in these new
artifacts; only the public bead of record and placeholder local paths appear.
