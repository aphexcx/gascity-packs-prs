# Follow-through retry accounting, round 2

Round base: `2dde43e3f693bc42d9515a2cc2615ab83f33d673`.
All tests use temporary state, invented owners and text, and fake mail.

## Red reproduction

With the regression tests added to the unchanged round base,
`pnpm exec node --test` from `follow-through/test`, on Node `v24.21.0`,
reported **108 tests: 98 passed, 10 failed, 0 skipped**, exit 1.
The failures were the gate sequence and the eight boundary cases plus their
parent test. The gate sequence recorded `deferred attempts 5; final state
stopped; total sends 4`, even though the fifth send would have succeeded.

The class test covers the owner's closing time, two-hour grace deadline,
default afternoon cutoff, and night cap. Each is crossed once without a
pending history lookup and once after a lookup returns not found. It checks
that the deferred tick sends nothing and preserves both `attempts` and
`last_attempt_at`. The next eligible tick must send successfully without
repeating a completed empty lookup.

## Fix and state-write audit

The fault class is persisting a counter or clock before the act it records,
then leaving that charge behind when the act is deferred. The fix snapshots
the two retry fields and restores them at the second wake-check deferral.
This preserves the existing write-ahead crash ordering. An empty lookup's
reconciliation remains saved; failed or exhausted lookups retain their charge.

| State write in the PR delta or its delivery path | Outcome and reason |
| --- | --- |
| `tick`: attempt count before a send, no lookup pending | FIXED: a wake deferral restores the previous count. |
| `tick`: attempt count before an empty lookup followed by deferral | FIXED: the count is restored while the completed reconciliation remains recorded. |
| `tick`: `last_attempt_at` on both deferred paths | FIXED: restore the exact prior value, including null, rather than restarting backoff. |
| `tick`: failed, found, or exhausted history lookup | NOT A CASE: the lookup occurred; failure stays bounded and a found receipt confirms delivery. |
| `tick`: `delivery_unknown = true` before sending | NOT A CASE: intentional write-ahead uncertainty, not a successful delivery or attempt total. |
| `recover`: migration default for `delivery_unknown`, pending state and restored items | NOT A CASE: reconstructs an unresolved delivery, without asserting a new send. |
| `plan` / `makeRaise`: notice plans, wake marks and item reservations | NOT A CASE: durable planning records; items remain open until confirmed delivery. |
| `tick`: `last_tick_at` | NOT A CASE: heartbeat for the tick that has started. |
| `delivered`: sent state, receipt, sent/raised timestamps | NOT A CASE: written only after a successful send or matching history receipt. |
| `failed`: error and stopped state, including the pre-send exhaustion check | NOT A CASE: records an observed failure or an already exhausted budget. |
| `mail.js` and changed status/doctor output | NOT A CASE: these changes write no persisted delivery state. |

The earlier minor finding about status overstating four sends as five is
FIXED by the same accounting correction. The gate regression asserts that
the deferred status says `attempts 4` and does not report stopped delivery.

## Validation

The first post-fix run passed 103 tests and failed five: four new no-lookup
cases and their parent asserted that a fresh, unattempted raise already had
an explicit false uncertainty marker. The assertion now applies only after
an actual lookup; all attempt-count and timestamp checks were already green.

The final `pnpm exec node --test` run from `follow-through/test`, on Node
`v24.21.0`, passed **108 tests, 0 failed, 0 skipped**, exit 0. The gate sequence
now records `deferred attempts 4; final state sent; total sends 5`.
`pnpm exec node --version` confirmed the test runtime; no unsupported-engine
warning occurred.

Both known-failed and uncertain follow-up sends stop after exactly five real
sends at minute offsets `[0, 1, 3, 7, 15]`. Doctor and status report the stop.
The existing reminder bound, unavailable-history bound, and crash recovery
tests also pass.

With the same Node 24 toolchain first on PATH,
`uv run --with pytest python -m pytest tests -q` from `follow-through` passed
**5 tests**, exit 0. This includes the whole Node suite and the installed
command, order and doctor wrappers. The pytest module was resolved before
the run. `git diff --check -- follow-through assets/pr-evidence/gp-2grw`
also passed.

The round's own delta receives one STANDARD review at the committed head.
Its verdict and SHA are recorded in the draft PR's review trail; the mayor's
gate and read remain separate.
