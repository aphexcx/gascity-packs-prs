# pnpm concurrency tests: controlled release

The missing wait message came from a caller waiting on the reclaim gate,
not from a caller bypassing the install. A fixed two-second fake install
could finish before that caller acquired the reclaim gate and attempted
the lane lock. The test then saw only one lane-install wait message even
though all three commands succeeded and only one install ran.

## Measurement

Base: `e84b685a177b109877c4d47d36953e4ad85539ed`.
Host: macOS, 32 logical CPUs; Python 3.15.0. Each experiment used eight
concurrent test instances beside 32 Python CPU-busy processes. No caller
was deliberately delayed. Each instance ran the actual test method with
its own disposable fixture. Times below are monotonic seconds relative to
fixture creation; CSVs contain no host paths or process identifiers.

| Experiment | Runs | Failures | Elapsed |
| --- | ---: | ---: | ---: |
| Base, initial light tracing | 50 | 1 | 31.993 s |
| Base, detailed gate tracing | 50 | 10 | 31.304 s |
| Changed tests, same detailed tracing/load | 200 | 0 | 120.911 s |

The light trace recorded process start, entry to `take_lane_lock`, and
install start/end. The detailed trace additionally records reclaim-gate
waits/acquisition, lane-lock claims and command start. Instrumentation is
inserted only in disposable fixture copies, never in repository shims.
Launching a clock recorder adds scheduling overhead, particularly while
holding the reclaim gate; these rates describe this recipe, not a predicted
failure rate in CI. The ordinary shim test suite was also running during
the changed-test experiment, adding background load.

The initial failure is in `base-load-failure.json` (run 25). All three
callers entered lock acquisition at 0.174–0.187 s, before the install ran
at 0.366–2.939 s. Thus the proposed late-process-start explanation was not
supported by this failure.

`base-gate-trace-failure.json` (run 7) establishes the alternative:

| Event | Time |
| --- | ---: |
| `vitest` enters lock acquisition | 0.269338 |
| `tsc` starts its install | 0.593246 |
| `vitest` completes first reclaim-gate sleep | 1.435620 |
| `vitest` completes second reclaim-gate sleep | 2.631939 |
| `tsc` ends its install | 2.722927 |
| `vitest` completes third reclaim-gate sleep | 3.695550 |
| `vitest` acquires reclaim gate | 3.730254 |
| `vitest` acquires lane lock | 3.769600 |
| `vitest` runs its command | 3.928230 |

That caller waited, but the reclaim-gate loop does not emit the asserted
lane-install wait message. Only `eslint` emitted it. This is not evidence
of a shim serialization failure: the caller entered the lane and ran only
after the install ended. The stop condition for a caller bypassing the
in-flight install was not observed.

The CSVs preserve every trial's result, install count, command sequence,
waiter identities, return codes, and per-caller event times. A blank event
means that caller did not perform it (or it was absent from the light
trace). Detailed JSON preserves the first failure of each baseline run.

## Reproduce

Keep `measure.py` outside the checkout when comparing revisions, or use
its absolute path from a checkout of the base. From the revision's
`gascity/` directory:

```sh
python3 /path/to/measure.py --runs 50 --jobs 8 --burners 32 --output /tmp/base.jsonl
```

At the changed revision, use the identical recipe with 200 runs:

```sh
python3 ../assets/pr-evidence/pnpm-wait-gates/measure.py --runs 200 --jobs 8 --burners 32 --output /tmp/head.jsonl
```

The runner exits 1 when any test fails. It reports each result and the
aggregate on stdout and writes detailed events to JSONL. It terminates
and reaps its CPU-busy processes in `finally`. The current script uses
the detailed trace; the light trace was the initial measurement before
the reclaim-gate instrumentation was added.

## Test audit

All locations below are in `gascity/tests/test_toolchain_shims.py` at the
changed revision. `test_` is omitted from test names in this table.

| Test (line) | Treatment |
| --- | --- |
| `concurrent_callers_wait_for_one_install_instead_of_racing_it` (622) | Hold the first install until both peers emit the lane wait. Preserve three successful calls, one install, all three execs, exactly two waiters. |
| `explicit_install_waits_behind_a_live_lock_and_project_commands_wait_behind_an_explicit_install` (846) | Replace the 0.5/2-second launch/install delays with install entry and observed waiter; explicitly assert the wait message. |
| `a_project_command_waits_behind_a_live_lock_even_when_pnpm_says_in_sync` (1384) | Replace the 1/3-second delays with install entry and observed waiter. |
| `a_project_command_runs_holding_no_lock_and_no_token` (1533) | Inspect absence of locks while the fake project command is held at its gate, replacing the 1.5/3-second timing window. |
| `two_mutating_calls_serialize_on_the_lane_lock` (1551) | Hold the first mutation until the second waits; prove the second fake has not entered, then release each in order. Remove the elapsed-time proxy; retain ordering and wait/no-wait assertions. |
| `a_mutate_does_not_wait_for_a_running_project_command` (1573) | Hold the project command until the mutation finishes; assert the command remains live. Replace the 1/4-second window and completion-time comparison. |
| `a_caller_whose_lane_is_in_sync_holds_the_lock_for_the_question_only_and_never_waits` (699) | Unchanged: a single caller with a synchronous lock-owner probe; no timed concurrent caller. |
| `lifecycle_script_that_reenters_the_wrapper_during_the_install_does_not_deadlock` (897) | Unchanged: synchronous parent/child lifecycle hook, no timed sleep arranging the reentry. |
| `a_pid_that_cannot_be_written_leaves_no_lock_and_fails_closed` (1031) | Unchanged: permission refusal before waiting, no timed peer. |
| `dir_wins_over_prefix_wherever_it_stands_and_a_refused_lock_fails_at_once` (1324) | Unchanged: the no-wait assertion concerns a read-only directory after the earlier holder is terminated; no timed peer at that assertion. |
| `a_command_started_by_a_running_command_is_any_other_caller` (1593) | Unchanged: the no-wait assertion is for a synchronous nested rebuild. The hook's background one-second sleep does not acquire a lane lock or schedule that rebuild; it cannot supply a competing holder. |

Other reclaim/signal interleaving tests do not assert the lane wait line
or its absence using the converted install/project-command timing windows.
They retain their separate test hooks and timing assertions.

## Validation

- `make test` from the repository root: 2,695 passed, 126 skipped,
  10,734 subtests passed in 451.71 s. Two existing multi-threaded `os.fork`
  deprecation warnings came from the Slack outbound flock-race test.
- `python3 -m unittest discover -s tests -p 'test_toolchain_shims.py'`
  from `gascity/`: 68 tests passed in 168.280 s.
- Loaded comparison: 0 failures / 200 runs; every run had three successful
  calls, one install and exactly two waiters.
- `python3 ../assets/pr-evidence/pnpm-wait-gates/check_cleanup.py` from
  `gascity/`: a simulated expired deadline produced the plain 60-second
  wait-message failure, and the entire blocked caller group was gone.
- `pnpm exec node --version`: `v24.21.0`. These are Python tests using fake
  pnpm; no pnpm install was needed and no unsupported-engine warning occurred.
- The production asset diff from the base is empty:
  `git diff --stat e84b685a177b109877c4d47d36953e4ad85539ed -- gascity/assets`.
- A fresh STANDARD self-review with `gpt-6-astra` at
  `2a7c7d6cbc64d951d76efc7593133b49b8bb831f` returned `CLEAN`, with no
  actionable correctness or regression findings. The later evidence-only
  commit adds the completed whole-suite result; test code is unchanged.
