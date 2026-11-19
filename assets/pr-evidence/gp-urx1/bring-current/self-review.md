# STANDARD self-review: merge resolution

Reviewed SHA: `920210cd6b3d2c38934d7149fcc60240be97ea04`.
Verdict: **BLOCKED on validation**.
Counts: **0 BLOCKER, 1 MAJOR, 0 MINOR, 0 NIT**.

One fresh reviewer on the worker's own model read all six automatically
merged files in full, both parent diffs, and the remerge diff. The review was
read-only and ran no tests. The worker supplied the executed run results.
The PR body stayed unchanged because publication was stopped; it was not
re-reviewed. No second review round was opened.

## F1: intermittent reconnect acceptance failure — MAJOR

Location: `whatsapp/adapter/test/never-silent-held-link.test.js:128`.

The full adapter run passed 430/431; C timed out awaiting an up connection.
Alternating isolated runs passed 6/7, 7/7, 7/7 at the merge and 7/7, 7/7,
7/7 at main. The same timeout was reproduced only at the merge in these runs.

Outcome: **NOT SHOWN as a production defect; validation failure reproduced**.
The contract requires stopping publication. The cause remains unproved.
The test waits for an inbox request, which is not a completion barrier for
the concurrent refused upgrade before the next clock jump.

Two minimal follow-up scopes:

1. Establish refused-upgrade completion before enabling the gateway and
   advancing through the retry interval. Preserve production behavior and
   every notice assertion.
2. Capture connection transitions, callback timestamps and retry deadlines.
   Correct production scheduling only if a violated reconnect rule is shown;
   preserve the intentional pong-only ladder reset.

No correction was made within this merge-only scope.

## Resolution findings

The changelog retains both complete sections. The remerge diff only removes
conflict markers and separates the sections. The six shared files retain
both sides without a source-level clash. All 18 gateway switch cases and
main's adopted-status replay test remain, without duplicate test names.
The index warning precedes the first tick; notice callbacks run after
publisher initialization. Notice admission still uses the adapter clock.
WAKE is parsed once in `wake-client.ts:15`, then consumed as `enabled`.
The held rollback and migration 0005 requirements do not conflict.

Original lost-wake test D passed. Its read-count assertion precedes the next
clock advance. The separate post-review status observation is recorded in
the README; it is additional executed evidence, not a second review round.
