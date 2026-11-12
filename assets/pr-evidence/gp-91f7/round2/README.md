# Follow-through: PR 93, round 2

Round base: `3a1caa3ce5e17b9ac438bc68363861406648b5a6`.
All examples and fixtures are invented. No owner messages or production state
were used. Raw local transcripts and review records stay outside this public
repository; the counts and reproducible commands are recorded here.

## Regression classes

1. **Python collection and suite drift.** The repository regression reads
   CI's Python step and Makefile's directory list, compares the lists, and
   checks module-name uniqueness across their union. At the base it failed
   with both the duplicate `test_pack` module and three missing Makefile
   directories. The follow-through module is now named
   `test_follow_through_pack.py`; CI and Makefile list the same 14 directories.
   No other pack's module was renamed. The Check job also runs this pack's
   Node suite directly on Linux.
2. **Undelivered work consumed by planning.** Eight deterministic recovery
   cases cover an afternoon send failure, an outage after planning, a
   25-hour outage, fourteen slow owners, consent off/on, a three-day sender
   outage, clock jumps during sends, and old persisted failed raises. All
   eight failed at the base and pass after the fix. Keys survive recovery;
   every due record is delivered once through the deduplicating sender.
   A separate test checks that doctor reports undelivered work and clears
   the finding after delivery or closure. A late reminder's body is refreshed
   at the delivery attempt, including when no attempt preceded the outage.
3. **Wakes outside recorded hours.** Generated short, split and overnight
   spans across UTC, America/Chicago and Asia/Tokyo produced 21 failing and
   three passing cases at the base; all 24 now pass. Independent minute-grid
   gate assertions and ticks at the three former out-of-span wake instants
   check that no due item is sent outside its spans.

The base run, from `follow-through/test`, was
`pnpm exec node --test --test-name-pattern=round2`: 34 failed, nine passed
(the count includes parent tests and Node's empty filtered-file results).
The Python red command, from the repository root, was
`env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q -k python_suite_directories_match`:
one failed, 439 deselected. The same Python command is now green: one passed,
439 deselected.

## Night cap and review regressions

For a final span under an hour, use its start as the second nominal wake, then move each nominal wake to the nearest minute inside both the recorded hours and 08:00–20:00 on that wake's actual local date, prefer the earlier minute on a tie, and coalesce duplicates; no overlap means no wake that day.

The tests cover both DST transitions, night-only hours, the 08:00 and 20:00
boundaries, status/doctor's explanation for an owner with no daytime overlap,
and requested reminders that still arrive at night.

The self-review panel found three additional cases, each reproduced before its
fix. A night-only final span must still determine the nominal second wake:
`Tue 09:00-09:30, 21:00-23:00` produces 09:00 and 09:29. An overnight span
must preserve a valid wake on its following date: `Tue 21:00-09:00` produces
Wednesday 08:00. Finally, a short span just after midnight cannot create a
wake on the previous day; its second nominal wake is clamped to the span's
start. Each focused regression was red with one failure. The nominal-wake
correction is applied before projection; projection uses the wake's actual
local date. Generated inverse assertions also ensure every returned wake
can be reached by the tick's recent-wake lookup.

The native review also found that a reminder could be reported cancelled
after delivery but before its result was saved. A real-process test kills
at plan, attempt, send and in-send boundaries, then invokes the cancellation
CLI. Three uncertain-delivery cases failed before the fix; only a never-
attempted plan may be cancelled. After any persisted attempt the existing
already-raised refusal is retained, and recovery uses the original key.

## Verification

`pnpm exec node --version`: **v24.21.0**.

From `follow-through/test`, `pnpm exec node --test` at
`0fd7fa175e768e2da24d19e786405e57dc6c32cf`: **84 passed, zero failed**.
This includes all eight SIGKILL boundary cases, concurrent ticks, durable
dedup keys, retry fairness, maximum UTF-8 bodies, CLI behavior, and the new
recovery/scheduling tests. No unsupported-engine warning appeared.

`actionlint .github/workflows/ci.yml`: **exit 0**.

The initial repository-wide run passed 2,767 tests and 10,689 subtests, with
131 skips and one failure: a repository file-policy scanner read the old
indexed name while the Python-module rename was unstaged. The rename is now
committed. The final `PATH=city-toolchain:$PATH make test`, run from the
repository root, passed **2,768 tests and 10,689 subtests**, with **131 skips**
and two existing Python fork deprecation warnings. No other pack needed a
repair. The run began at `534530d5` and completed after the cancellation fix
was committed; its follow-through Python suite exercised the final code.
The separate 84-test Node run names the final code SHA above.

Native DEEP rounds against the round's base:

- `fe2caab8`: BLOCK, two MAJOR findings: lost valid overnight wakes and
  cancellation after uncertain delivery. Both were reproduced and fixed.
- `534530d5`: BLOCK, one MAJOR finding: the same cancellation class, fixed
  in the following commit.
- `0fd7fa175e768e2da24d19e786405e57dc6c32cf`: CLEAN, no actionable findings.

The final self-review panel is also clean. Its scheduling probe checked
2,898 configurations and 2,910 wakes across six zones and DST dates, with
zero hours/night-cap violations and zero unreachable wakes. Reviewers did
not write to the lane; native review did not rerun tests in its sandbox.

## Delivery verification

The pushed code head `0fd7fa175e768e2da24d19e786405e57dc6c32cf` passed
every PR check. The exact line from
`gh pr checks 93 -R aphexcx/gascity-packs-prs` was:

```text
Check  pass  17m55s  https://github.com/aphexcx/gascity-packs-prs/actions/runs/43162229363/job/130995869485
```

That Check ran both the Python pack suites and the explicit
`Run follow-through Node tests on Linux` step successfully. Subsequent
delivery changes contain only this evidence document. The final handoff
also checks the final documentation commit's own Check result.

Public-repository review covered the whole round diff, prior pushed evidence,
commit messages and PR title/body. No prohibited customer data or secret was
found; zero privacy corrections were needed.

## Mail bodies

These complete examples use deliberately invented identifiers and text.

```text
Follow-up due in slack/invented-owner.
Wake wake-invented.
item-invented: Invented task (opened 2026-11-09 09:00:00 (Asia/Tokyo); 2026-11-09T00:00:00.000Z (UTC))
Read the follow-through skill. Each item gets one follow-up, or none. The mayor decides whether and how to write, by the channel's rules.
```

```text
The owner asked for this reminder in slack/invented-owner.
reminder-invented: Invented reminder
Set 2026-11-09 09:00:00 (Asia/Tokyo); 2026-11-09T00:00:00.000Z (UTC) for 2026-11-09 09:01:00 (Asia/Tokyo); 2026-11-09T00:01:00.000Z (UTC).
This reminder is 1439 minutes late.
Read the follow-through skill. The mayor decides whether and how to write, by the channel's rules.
```

With no recorded zone, the same time fields use labelled UTC alone. The
commands, arguments and status JSON keys stay unchanged. Undelivered raises
have no expiry, so their existing `expires_at` field is null.
