# gp-91f7 implementation evidence

Base: `28918336555abbeb4f52af3c05029e801c59cb6f`, fetched from
`aphexcx/gascity-packs-prs` origin/main. New pack: `follow-through`.

## Verification

Node runtime: `pnpm exec node --version` = `v24.21.0`.
From `follow-through/test`, `pnpm exec node --test`: **37 passed, 0 failed**
after native review fixes (`19-native-review-green.log`). Earlier red runs and
their exit codes are retained in `checks.tsv`. The initial directory-operand
run was a harness error, not accepted as red-first evidence; the next run
correctly failed on the missing state and scheduling implementations.

From the repository root, `pnpm exec make test`: **2695 passed, 130 skipped,
10689 subtests passed, 2 warnings** (`12-make-test.log`, `make-check.tsv`).
The warnings are Python's existing os.fork-in-a-thread deprecation in the
Slack tests. The Makefile now includes `follow-through/tests`, whose five
tests run the Node behavior suite and the shipped wrappers, check Make/CI
registration, and verify the README's operational facts.

Live-gc command/doctor registration and the doctor canary:

```sh
GC_TEST_BIN="$(command -v gc)" pnpm exec uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q -k 'follow-through or the_doctor_delta_can_surface_a_finding'
```

**3 passed, 2 skipped, 434 deselected** (`13-live-gc.log`, `live-check.tsv`).
The skipped checks are formulas and named agents, which this pack does not
ship. Tests use scratch cities; no test sends mail to the actual mayor.

## Contract coverage

| Item | Result | Evidence |
| --- | --- | --- |
| 1 | DONE | Python pack-surface test: schema 2, no service; local Node commands/order |
| 2 | DONE | state.test.js permissions/atomic file; Python wrapper test shares one file |
| 3 | DONE | process.test.js SIGKILL before send, during send and after delivery, for both raise kinds |
| 4 | DONE | state.test.js second-process read and reused-PID recovery; process restart tests |
| 5 | DONE | model/CLI forms; schedule grammar, midnight, split spans, invalid zone |
| 6 | DONE | engine no-zone suppression; status names owners without zones |
| 7 | DONE | model/CLI item creation, filtering, terminal records, limits and unknown IDs |
| 8 | DONE | engine.test.js: each item included once across thirty days of ticks |
| 9 | DONE | schedule/DST/empty wakes; ruling 2: one bounded mail, oldest 25, durable wake filter, overflow |
| 10 | DONE | grace/outage/cutoff tests, live clock recheck, dispatch's 120-second ruling |
| 11 | DONE | consent off blocks open items and pending follow-up retries |
| 12 | DONE | fake gc arguments: agent, notify, unique stable dedup key, both subjects |
| 13 | DONE | fake-gc failures/recovery; minute throttle, 24-hour expiry, fair retry order and tick budget |
| 14 | DONE | generated fixed mail bodies below; late reminder assertion |
| 15 | DONE | CLI/model time, duration, text-file, list, cancel, validation tests |
| 16 | DONE | reminder set/fired by different processes, outage catch-up, repeated tick |
| 17 | DONE | cooldown order, simultaneous real ticks, README timing and wait-idle fact |
| 18 | DONE | exact JSON and text status, secret-text exclusion, earliest wake ordering |
| 19 | DONE | doctor stale tick/missing zone/failed raise; shipped wrapper and live gc |
| 20 | DONE | Makefile, CI pytest line, maintained-pack registry and pack README |

Items 9 and 10 include the mayor's binding gp-91f7 comment: less than 120
seconds after a wake is on time; from 120 seconds, late-wake rules apply;
the 20:00 cutoff always wins. Tests cover +1, +59, +90, +119, +120 and +600
seconds, with default afternoon items waiting until morning when too late.

## State directory and copied code

At gc `a8217556`, command execution in `cmd/gc/cmd_commands.go` calls
`citylayout.PackRuntimeEnv`; exec orders in `cmd/gc/order_store.go` call
`citylayout.PackStateDir`. Both set `GC_PACK_STATE_DIR` to
`<city>/.gc/runtime/packs/<pack-name>`. Doctor also uses PackRuntimeEnv. The
pack requires that injected absolute path, so it cannot silently split
command and tick state. The Python wrapper test actually writes through a
command, runs the order, and reads status from the same file.

Copied `whatsapp/adapter/src/zoned.js` whole at the base SHA into this pack,
including its hours grammar and IANA wall-clock conversion. Carried the
midnight/split-span, invalid-zone, DST-gap and DST-fold cases into the new
schedule tests. There are no cross-pack imports or keeper state reads.

## Generated examples

The Node test run generated these with invented text and a fake gc binary:

- `sample-state.json`: owners, items, reminder and durable delivery records.
- `sample-status.json` and `sample-status.txt`: no summary or reminder text.
- `follow-up-mail.txt` and `reminder-mail.txt`: the exact two fixed bodies.
- `fake-gc-arguments.json`: complete argv for both raises.
- `wake-times.md`: tested local dates, hours, zones and UTC instants.

## Fresh self-review

Reviewed `65663d1f7e23f2eb98f7a8e13b697f8d771817eb` against the base. Counts:
0 BLOCKER, 1 MAJOR, 2 MINOR, 0 NIT. Each finding was reproduced red before
fixing; `15-self-review-red.log` records all three failures, and
`16-self-review-green.log` records 33 passing tests afterward.

- FIXED: process identity class. Stale lock tickets could survive PID reuse.
  Every ticket now includes a process birth identity and validates it before
  waiting. The class test uses a persisted old-generation ticket whose PID
  is currently alive.
- FIXED: cross-day schedule ordering. Next-wake status now selects the global
  earliest eligible instant across opening days. The regression overlaps
  Monday's overnight span and Tuesday's midnight span.
- FIXED: RFC 3339 precision. Valid fractions longer than three digits are
  accepted at the runtime's millisecond precision; `.123456` is covered.
- INTENDED, extra monitoring recommendation: a raised reminder with a
  zero-attempt pending mail does not itself enable stale-tick red. The bead
  explicitly limits that check to consent-on owners or reminders still set.
  The pending record is durable and the next tick sends it.

The immutable self-review record is retained outside the branch at
`/Users/tailor512/city/assets/ops/pr-evidence/gp-91f7/self-review-r1.md`.

## Native DEEP round 1

Reviewed `0c4a3de13655216142b8c01a20dce666033b7e68`. Verdict BLOCK:
0 BLOCKER, 3 MAJOR, 0 MINOR, 0 NIT. All three findings were reproduced in
`17-native-review-red.log` and `18-bounded-mail-red.log` before fixing.
`19-native-review-green.log` records 37 passing tests after the fixes.

- FIXED: lexical-versus-canonical entrypoint identity. Every command, the
  order and doctor now execute through directory symlinks. The Python
  regression exercises all entrypoints through a symlinked pack directory.
- FIXED: starvation under an external deadline. Both raise kinds now attempt
  unattempted then least recently attempted mail, within a 90-second tick
  budget that reserves the send timeout and a persistence margin. The class
  regression puts eight slow failing raises before a healthy ninth, reloads
  state across six ticks and simulates deadline kills; both kinds progress.
- FIXED: unbounded argv payloads. Dispatch ruling 2 keeps one mail per wake,
  showing up to 25 oldest items with an overflow count and exact wake-list
  command. All due items retain that wake ID, including later terminal
  records. Tests exercise 26, 100 and 500 items, UTF-8 shortening, maximum
  reminder text and conversation byte limits across mutation verbs.

The immutable native record is retained outside the branch at
`/Users/tailor512/city/assets/ops/pr-evidence/gp-91f7/codex-deep-r1.md`.
Native round 2 reviews this complete set of fixes.
