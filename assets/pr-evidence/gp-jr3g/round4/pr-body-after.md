Fresh Google Chat installs and state resets begin history at the first successful
subscription attempt, optionally including up to 168 hours through
`first_start_history`. Failed setup attempts keep moving that floor until the
subscription is recorded.

Every create, edit, deletion and reaction is judged by its own time across live
delivery, redelivery and recovery. Older acts become frame-only non-turn records;
new acts on old messages still arrive. A bounded message missing its resource
time uses a valid event time; publish time never decides. With neither time,
an edit is quarantined and a bounded create skips with a missing-time counter.
Message validation rejects malformed resource times in bounded and unbounded
spaces. Unbounded spaces keep the
released behavior: missing-time edits quarantine and timeless creates deliver.

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

Round 4 answers the four round-3 findings: messages use their resource or event
time and retain the released validation behavior in unbounded spaces; a partly
saved current admission keeps its notices; first-load retirement is counted and
reported in one mail. The new message-time, partial-admission, first-load,
running-sweep and per-start dedup tests show those changes red before the fixes
and green afterward. Before taking this version, check waiting and unresolved
deliveries in `gc gchat status`; a backup restore and a running sweep count
retirements but do not mail them.

Validation uses Go 1.27.3 and Python 3.13.13 through the Makefile's uv fallback.
Exactly two full Go runs tested 22 packages each. Run 1 recorded 21 passing
packages, 736 passing top-level tests, 1,490 passing subtests and two optional
live-gc skips; one existing credential-lookup test failed in seven rows under
its 100 ms deadline. Its isolated rerun passes all 18 rows, and it passes in
full run 2. Run 2 recorded 21 passing packages, 736 passing top-level tests,
1,496 passing subtests and two skips; its only failure is the documented
load-sensitive shutdown bounded-drain case, which passes in isolation.
Neither full run is claimed as a clean exit. Both 3,000-message recoveries
finish with zero backlog, and neither timing fixture was edited.
All 25 named regressions pass individually: 25 top-level tests and 177 subtests,
with 24 commands unchanged and the authorized timestamp test renamed.
All five new tests pass individually: five top-level tests and 44 subtests.
The three notice tests pass under the race detector: three top-level tests and
20 subtests. Formatting, vet, sh -n and shellcheck pass; staticcheck is unavailable
and was not installed. Earlier full runs and their outcomes remain in the
round-3 evidence.

The prescribed Python make invocation recorded 463 passed, one failed,
127 skipped and 250 subtests passed: its directory override leaks into the
unchanged nested Makefile-discovery check. The prescribed clean-environment
equivalent passes 464 tests and 250 subtests, with 127 skips. Both outcomes
remain in the evidence.

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
preserved. Round 3's Codex gate was BLOCK with two majors; the second-model read
was CLEAN with four minors and three notes. Round 4 answers all four findings.
Round 4's DEEP self-review is CLEAN at `73c91616`, with one read-only minor
observation outside this delta: queue migration can bypass malformed timestamp
validation, as at the starting commit. That pre-existing case was not reproduced
or changed in this round; its reproduction sequence is in the evidence.
The Mayor's round-4 gate and second-model read are pending.

All checks use fakes. No real Pub/Sub subscription holding old events was
measured, and no test here used a real notice file or real operator mail.
With never-silent enabled, the notice watch holds its lock through the owner's
post and mails, so a state restore and every health change wait behind it; no
lock cycle was found. A new conversation starts at the last hourly look that
did not show it, by the box's clock, so its first message can be skipped when
Google lists the space late or the box clock runs ahead. A room's join time also
bounds the live stream, so a live message stamped before the join is no turn,
where the released adapter dropped it only during catch-up.
The first-start shape is the mayor's interpretation, and the merge card must
ask the requester to confirm it. This PR remains draft.

Main gained PR 102's Slack-only change after round 3. The round-4 comparison at
`205d0aecedf944792f1e3b3fbc420b9e5ad910d8` finds no file shared with this PR;
no main merge was made in round 4.

[Round-2 evidence](https://github.com/aphexcx/gascity-packs-prs/blob/gp-jr3g/assets/pr-evidence/gp-jr3g/round2/README.md)
records its full runs, rollback, timing and review corrections.
[Round-3 evidence](https://github.com/aphexcx/gascity-packs-prs/blob/gp-jr3g/assets/pr-evidence/gp-jr3g/round3/README.md)
records the merge, both conflict resolutions, interaction table, red/green
checks, every full run, concurrent interleavings and the review outcome.
[Round-4 evidence](https://github.com/aphexcx/gascity-packs-prs/blob/gp-jr3g/assets/pr-evidence/gp-jr3g/round4/README.md)
records the main comparison, red/green runs, per-suite counts, scope ruling,
operator strings and self-review outcome.

Bead: gp-jr3g
Follow-up-Bead: gp-avkk
Follow-up-Bead: gp-3qva
Follow-up-Bead: gp-xuam
Codex-Reviewed-By: gpt-6-astra, DEEP; round 1 worker BLOCK at 541fa5cb and gate BLOCK at ebd7f446; round 2 worker BLOCK at ea20a6fc, three fixes in 713351ae; round 3 worker BLOCK at d6721ca0, concurrency class fixed in 0741da65, gate BLOCK with two majors at a553d22f; round 4 worker CLEAN at 73c91616 with one pre-existing out-of-scope MINOR, mayor gate pending
Fable-Reviewed-By: round 1 BLOCK, round 2 not run, round 3 CLEAN with 4 MINORs, round 4 pending
Co-Authored-By: Codex <noreply@openai.com>
