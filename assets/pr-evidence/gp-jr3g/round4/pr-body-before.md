Fresh Google Chat installs and state resets begin history at the first successful
subscription attempt, optionally including up to 168 hours through
`first_start_history`. Failed setup attempts keep moving that floor until the
subscription is recorded.

Every create, edit, deletion and reaction is judged by its own time across live
delivery, redelivery and recovery. Older acts become frame-only non-turn records;
new acts on old messages still arrive. Missing times use Pub/Sub publish time,
or skip with a counter and a content-free warning when no time is available.

This implements **Way B, Reading 1**: cursorless spaces start at the preceding
completed inventory. A released ledger establishes that inventory boundary at
first load, without gaining a global floor or requiring an operator action.
Existing cursors keep their outage. A DM opened while the adapter was down keeps
messages since the inventory before the outage. Migration removes old queue rows
and reconciles old saved delivery attempts. Rollback keeps cursors and recognizes
`Reason=local` skips; `Cause` retains the diagnostic history decision. Never-read
spaces do not report false history loss.

Round 3 merges main's PRs 98, 95 and 90, retaining both status keys and printing
History before Never silent. With opt-in notices enabled, old acts start no
notice and load-retired attempts stop contributing to outages; current messages
still receive notices. Admission, retirement reconciliation and backup restore
are ordered so a stale snapshot cannot suppress a current notice or emit one
for a restored, retired attempt.

Validation uses Go 1.27.3 and Python 3.13.13 through the Makefile's uv fallback.
The final round-3 full Go run passed 22 packages, 732 top-level tests and 1,453
subtests, with two optional live-gc skips. Its 3,000-message recovery emptied the
backlog. Exactly two full Go runs were made, both successful.
The first round-3 full run, before the concurrency correction, passed 22 packages,
731 top-level tests and 1,449 subtests, with two optional live-gc skips. All 25
unchanged round-2 regressions pass individually at the corrected source (177
subtests), as do all seven new tests (43 subtests). The four concurrency cases
also pass with the race detector. Formatting, vet, sh -n and shellcheck pass;
staticcheck is unavailable and was not installed.

The prescribed Python make invocation first recorded 463 passed, one failed,
127 skipped and 250 subtests passed: its directory override leaks into a nested
Makefile-discovery check. That unchanged test passes in isolation with inherited
make variables cleared, and the equivalent complete suite passes 464 tests and
250 subtests with 127 skips. Both outcomes remain in the evidence.

The old-act class test passes all 60 rows. The released-reader exchange produces
zero turns after unmentioned edits and deletions of all five skipped-act kinds;
it passes again after the merge using the unchanged reader overlay. Parent
ledger shapes retain their fields through migration, and the frozen 0.5.0 shape
drops only the new fields. The upgrade case from round 2 removes three old queued
rows, delivers three saved-cursor outage messages and one new DM, and delivers
zero of 120 old DM messages.

Round 2's interruption-scenario baseline passed 30/30; no natural failure was
captured. A test-only 200 ms save delay reproduced the old ten-second timeout
at 15/24 turns. The revised bounded wait passed the same injection with 24/24
turns, using an 86-second ceiling derived from saved work. Round 2's final source
passed 30/30 selected scenario runs, longest recovery wait 13.134079208 seconds,
while the full suite ran other packages. Every original assertion remains;
round 3 changes none of those tests.

Round 1's Codex gate was BLOCK with two majors, and the Fable read was BLOCK
with the same two majors and two minors; round 2 answered all four. Round 2's
worker self-review was BLOCK at `ea20a6fc` with three reproduced and fixed
classes: detached attempts, update-ID association and persisted retention-key
compatibility. The round-2 gate did not run because the branch was behind main.
Round 3's DEEP self-review was BLOCK at `d6721ca0` with one concurrency class;
both cases were reproduced and fixed in `0741da65`. The recorded verdict is
preserved. The whole-delta gate and second-model read for round 3 are pending.

All checks use fakes. No real Pub/Sub subscription holding old events was
measured, and no test here used a real notice file or real operator mail.
The first-start shape is the mayor's interpretation, and the merge card must
ask the requester to confirm it. This PR remains draft.

Main subsequently gained PR 102's Slack-only change; this round follows its
one-merge contract and does not chase commits after its start-time main.

[Round-2 evidence](https://github.com/aphexcx/gascity-packs-prs/blob/gp-jr3g/assets/pr-evidence/gp-jr3g/round2/README.md)
records its full runs, rollback, timing and review corrections.
[Round-3 evidence](https://github.com/aphexcx/gascity-packs-prs/blob/gp-jr3g/assets/pr-evidence/gp-jr3g/round3/README.md)
records the merge, both conflict resolutions, interaction table, red/green
checks, every full run, concurrent interleavings and the review outcome.

Bead: gp-jr3g
Follow-up-Bead: gp-avkk
Follow-up-Bead: gp-3qva
Codex-Reviewed-By: gpt-6-astra, DEEP; round 1 worker BLOCK at 541fa5cb and gate BLOCK at ebd7f446; round 2 worker BLOCK at ea20a6fc, three fixes in 713351ae; round 3 worker BLOCK at d6721ca0, concurrency class fixed in 0741da65; whole-delta gate pending
Fable-Reviewed-By: round 1 BLOCK, round 2 not run, round 3 pending
Co-Authored-By: Codex <noreply@openai.com>
