# WhatsApp env-file resolution: round 2

The [round 2c delivery results](#round-2c-delivery) below supersede the earlier
round 2b stop. Earlier measurements are retained as history.

This round restores the city fallback in the shared resolver. A pack command
receives `GC_CITY_PATH`, while the supervised adapter also receives
`GC_SERVICE_SECRETS_DIR`; both now select the same file for this pack.
The regression class is assuming that commands and supervised services have
identical environments. Its affected callers are enrollment, adapter startup,
and both the env and gateway doctor checks.

## Red before green

Production files were unchanged at round 1's head,
`2c22dbc5f70b66a8add375e12b86486dae6c273d`, when the regression tests ran.
From `whatsapp/`, with the Node toolchain first on PATH:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s -k 'env_file_agrees_across_enroll_run_and_doctor or enroll_and_doctor_match_supervised_startup or doctor_finds_existing_city_file_without_service_env or env_missing_file_fails_closed'
6 failed, 14 passed, 56 deselected in 14.96s
```

The failures were:

- The city-only matrix row, for both absolute and relative paths, selected
  home instead of city.
- `test_enroll_and_doctor_match_supervised_startup`: enrollment succeeded,
  but the real startup shell with the supervisor's environment loaded no
  credential (`missing` instead of the invented fixture credential).
- `test_doctor_finds_existing_city_file_without_service_env[env]`: a complete
  per-city file and no home file produced a missing-file failure.
- The same test's `gateway` case could not find the gateway configuration.
- Missing-file guidance omitted the city-path fallback.

After the resolver and guidance change, the full pack suite passed:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s
76 passed in 69.46s
```

The class-level test pairs the command environment with the supervisor's
environment for one temporary city. It runs the real enrollment shell against
a loopback fake gateway, observes the real startup shell through a fake Node
executable, then runs both real doctor checks under the command environment.
It verifies the credential, file mode 0600, absence of a home file, and gateway
requests. Separate doctor cases cover a city already enrolled before this run.

## Eight-row comparison

C, E, and S mean non-empty `GC_CITY_PATH`, `GC_WHATSAPP_ADAPTER_ENV`, and
`GC_SERVICE_SECRETS_DIR`, respectively. Each triple is enroll / run / doctor;
the labels identify temporary fixture files. The original base is
`c546802da5d42fb5d073b7d0bab6716b2e48b6f5`; round 1 is the SHA above.
The base observations are retained in the round 1 evidence. This round's red
and green matrix runs measured all eight rows again, including relative paths
and paths with spaces.

| C | E | S | Original base | Round 1 | Round 2 |
| --- | --- | --- | --- | --- | --- |
| 0 | 0 | 0 | home / home / home | home / home / home | home / home / home |
| 0 | 0 | 1 | service / service / service | service / service / service | service / service / service |
| 0 | 1 | 0 | override / override / override | override / override / override | override / override / override |
| 0 | 1 | 1 | override / override / override | override / override / override | override / override / override |
| **1** | **0** | **0** | **city / home / city** | **home / home / home** | **city / city / city** |
| 1 | 0 | 1 | service / service / service | service / service / service | service / service / service |
| 1 | 1 | 0 | override / override / override | override / override / override | override / override / override |
| 1 | 1 | 1 | override / override / override | override / override / override | override / override / override |

The marked row changes startup for a person starting the adapter by hand with
the city path exported and a home file: startup now selects the per-city file.
A supervised adapter has its secrets directory supplied explicitly, so its
selection does not change. Explicit paths remain authoritative even when the
selected file is missing; relative paths remain anchored at the adapter directory.

## Supervisor directory

The pack declares service name `whatsapp` and no custom `state_root`.
gc's `Service.StateRootOrDefault` therefore returns `.gc/services/whatsapp`;
`newProxyProcessInstance` resolves that against the city root, and
`proxyProcessInstance.start` supplies its `secrets` child as
`GC_SERVICE_SECRETS_DIR`. The test derives that directory from the same city
path. The general service API supports custom state roots, but this pack does
not declare one, and gc's patch configuration has no service patch surface.

## Adapter suite and runtime

Runtime: Node **v24.21.0**, measured with `pnpm exec node --version`;
Python **3.13**, with pytest supplied by `uv run --with pytest`.
No unsupported-engine warning occurred. All new tests use temporary directories
and fake services; no deployed adapter or credential file was read or changed.

From `whatsapp/adapter/test/`, the default `pnpm exec node --test` run
reported 252 tests: 247 passed, 4 failed, and 1 cancelled. The failures and cancellation were
timeouts in dead-reason retry, media crash recovery, retry after HTTP 404,
and receipt recovery. The media timeout left a test-owned adapter child
running; terminating that specific child allowed the runner to finish.

The first complete `pnpm exec node --test --test-concurrency=1` retry
reported 251 passed, 1 failed, and 0 cancelled out of 252 tests
(245879.743708 ms). The remaining failure was the failed-receipt retry
case timing out while waiting for an acknowledgement. These tests launch
the adapter JavaScript directly; both that source and the adapter tests
are unchanged from the original base and do not execute the changed shells
or doctor. The raw logs remain outside the public branch.

A second full serial run used the same command and reported 251 passed,
1 failed, and 0 cancelled out of 252 tests (285521.276667 ms). This time the
failed-receipt retry case passed; the receipt-recovery case (6b), which passed
in the first serial run, timed out awaiting an acknowledgement. Each case
passed in one full serial run, but neither serial run was entirely green.

## Round 2b validation

The saved round 2 changes were restored at
`2c22dbc5f70b66a8add375e12b86486dae6c273d`. The diff over `whatsapp/`
matches the saved patch exactly; this continuation changes only the evidence
beyond that patch. The full pack suite was rerun from `whatsapp/`:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s
76 passed in 56.11s
```

Node was again **v24.21.0**, measured with `pnpm exec node --version`.
There was no unsupported-engine warning. All 16 observed matrix rows agree
with the table above, including the absolute and relative path variants.

The adapter suite is not a gate for this round because its source and tests
are unchanged. This command produces empty output (zero bytes):

```text
git diff c546802da5d42fb5d073b7d0bab6716b2e48b6f5 HEAD -- whatsapp/adapter/src whatsapp/adapter/test
```

The complete serial run from `whatsapp/adapter/test/` was:

```text
pnpm exec node --test --test-concurrency=1
252 tests, 249 passed, 3 failed, 0 cancelled, 0 skipped (567261.143917 ms)
```

The failing cases were:

- The keeper's verbs match a user-id conversation whole: consent for one never
  changes another with the same digits; an id with no digits is found; a typed
  number never matches a user id (`conversation.test.js`).
- (7a) The lookup boundary: accepted pending at N with no later message, gc
  restarted: found from the posted boundary (< N), one unmarked and one marked
  POST (`recovery.test.js`).
- M2: An inbox page computed before the ack never brings an acknowledged
  delivery back to the no-entry branch (`review.test.js`).

Isolated reruns also included the earlier round 2 failures. They ran from the
same test directory with `pnpm exec node --test --test-concurrency=1` and an
anchored `--test-name-pattern` containing each exact test name. The nested
dead-reason test additionally skipped all sibling scenarios so that only its
`repost_bound` case ran. Each successful rerun verified that the requested
case actually executed.

| Case | Isolated result |
| --- | --- |
| (12b) `repost_bound` in `deadack.test.js` | 3 of 3 passed |
| Video SIGKILL at `after-media-journal` in `media.test.js` | 3 of 3 passed |
| HTTP 404 retry in `pipeline.test.js` | 3 of 3 passed |
| (6b) Receipt read back as unknown in `recovery.test.js` | First run failed |
| Failed receipt repost in `pipeline.test.js` | Not rerun: stopped at (6b) |
| Keeper conversation case, (7a), and M2 above | Not rerun: stopped at (6b) |

The isolated (6b) failure was `timed out waiting for the ack of delivery 1`
after 21687.256958 ms. The filtered runner reported 15 other empty-file/harness
successes and this one failed case; those successes are not additional test
coverage. Under the round's explicit rule, a failure in isolation requires a
stop. Delivery, commit, push, and the new self-review were therefore stopped.

The retry cases share a fake clock advanced by fixed budgets with short real
sleeps: `pump` defaults to 1000 ms of fake time per 25 ms real sleep. Receipt
recovery (6b) and (7a) pump 6000 ms, then wait for an acknowledgement without
advancing that clock. The keeper and M2 cases also await acknowledgement on
the real-time deadline before any clock pumping. The media case has a separate
30-second crash-recovery timeout. These are follow-up observations, not a
proved root cause. The measured 1/5/15-minute host load averages were
10.36/8.38/9.03 before the serial run and 8.23/7.57/8.15 after it on 32 logical
CPUs; CPU saturation was not measured. No adapter JavaScript or test was changed.

## Round 2c delivery

The saved fix was committed as
`a7e3d16aa6339d68c1e1716aa99e65e931be74ec`. Its diff from
`2c22dbc5f70b66a8add375e12b86486dae6c273d` over `whatsapp/` exactly matches
the saved round 2 patch. The only later change to that fix is documentation:
the pack README now includes the city fallback and the shared path semantics,
addressing the STANDARD review's minor finding. No resolver production code
or pack test was changed after the saved patch.

Main `dce538c80df3fa7731bec3d65c3d12b685530ce8` was merged once, as
`deff8424ab9ddfea51caba10bcb0f9d57b0c55ff`, with no conflicts or manual
resolutions. The merge imported main's changelog, gateway fixture, reactions,
and skill changes unchanged. The resolver fix and its tests remained identical.
There was no merge resolution delta requiring another review round.

From `whatsapp/`, with the Node toolchain first on PATH:

```text
env -u GC_TEMPLATE uv run --with pytest python3 -m pytest tests -q -s
Fix commit: 76 passed in 45.85s
Merged commit: 76 passed in 35.50s
```

`pnpm exec node --version` reported **v24.21.0**. Python was **3.13**;
pytest was supplied by `uv run --with pytest`. No unsupported-engine warning
occurred. All 16 matrix observations at the merged commit match the saved
round 2 results: eight combinations, each with absolute and relative paths.
The triples below are enroll / run / doctor; the marked row is the behavior
change from round 1.

| C | E | S | Round 2 | Merged commit |
| --- | --- | --- | --- | --- |
| 0 | 0 | 0 | home / home / home | home / home / home |
| 0 | 0 | 1 | service / service / service | service / service / service |
| 0 | 1 | 0 | override / override / override | override / override / override |
| 0 | 1 | 1 | override / override / override | override / override / override |
| **1** | **0** | **0** | **city / city / city** | **city / city / city** |
| 1 | 0 | 1 | service / service / service | service / service / service |
| 1 | 1 | 0 | override / override / override | override / override / override |
| 1 | 1 | 1 | override / override / override | override / override / override |

The adapter identity check at the merged commit produced empty output
(zero bytes, exit 0):

```text
git diff dce538c80df3fa7731bec3d65c3d12b685530ce8 HEAD -- whatsapp/adapter/src whatsapp/adapter/test
```

Only case (6b) was selected in three runs on a scratch `git archive` extract
of the original PR base, `c546802da5d42fb5d073b7d0bab6716b2e48b6f5`.
From that extract's `whatsapp/adapter/test/`, each invocation was:

```sh
pnpm exec node --test --test-concurrency=1 \
  --test-name-pattern='^\(6b\)\ the\ receipt\ read\ back\ as\ unknown\ \(gc\ restarted,\ transcript\ kept\):\ the\ record\ is\ found,\ one\ marked\ copy,\ acked\ on\ its\ delivered\ receipt$'
```

| Attempt | Selected case | Exit | Case duration |
| --- | --- | --- | --- |
| 1 | PASS | 0 | 372.491708 ms |
| 2 | PASS | 0 | 379.855916 ms |
| 3 | PASS | 0 | 337.219959 ms |

Each filtered directory run also reports 15 empty-file/harness successes;
these are not additional case coverage. The selected case executed in every
run. These adapter observations are non-gating; no full adapter suite ran in
this round, and no adapter JavaScript or test changed.

The one STANDARD self-review of the fix commit returned **CLEAN**:
**0 BLOCKER / 0 MAJOR / 1 MINOR / 0 NIT**. Its minor documentation finding
was confirmed by the merged matrix selecting the city file while the README
still listed service then home. **FIXED:** the README now lists the city
fallback. The earlier round 1 gate and finding check's major issue (different
command and supervisor environments) is **FIXED** by the saved fix and its
class-level test. The earlier round 1 self-review's guidance minor was already
**FIXED** in `2c22dbc5f70b66a8add375e12b86486dae6c273d`; the guidance now
also includes the restored city fallback.

Public-repository check: done, 0 sensitive-content changes needed. Fixture
values are invented; raw test logs and reviewer transcripts remain private.
