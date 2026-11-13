# Round 5: Slack recovery and notice interactions

Original PR head: `0b790454653fe4d6de64c057c0ae295b8d4a0057`.
Pinned main: `c99694367081b2b3e98a58aafaf1320e091621fe`.
Merge: `7ef1e77d6f03412f74ec380fbe243563e0d9b84a`, with those two
commits as its first and second parents respectively.
Test-first commit: `401963e5d90ce0aa4a0c1d538baab96883d9383f`.
Implementation: `225c956f5cdf9f795d7fec5823018cc3500dcae8`.
Final test-only timing correction: `fffdc14d1684f79c01480e7631a13b5553722b66`.

Main advanced during verification to
`4d14c7c5d52a468a2af7e387175ff4fb20366ec5` (WhatsApp WebSocket wake with
polling fallback, PR 103). This round merges only its required pinned main.

The six conflicts match the recorded merge-tree output. Each changelog keeps
main's entries before the notice entry. The dead-letter struct retains main's
recovery fields and the notice certainty flag. Both follow-up helpers and the
coalescer sink refresh recovery before recording the stop. The inbound POST
defer retains main's complete success branch and the notice error branch.
Startup still builds the engine before recovery takes its configuration copy.

The coalescer sink's unchanged body is extracted as `inboundDeadLetterSink`
so tests can reach the installed callback. PR 85's recovery implementation and
all five recovery test files remain identical to pinned main.

## Interaction tests

All rows use invented messages, a fake HTTP gc and a controlled clock. The
initial red run used the merge commit with only tests and the unchanged sink
extraction in the worktree. The one-sided checks removed one observer at a
time, then restored both source files with `git checkout --` before any commit.

| Item | Test | Scenario rows | Before fix |
| --- | --- | ---: | --- |
| A | `TestNeverSilentHoldLegacyWriters` | 9 | Pin; all nine red with either side alone |
| B | `TestNeverSilentHoldPostBranches` | 2 | Pin; refused connections red with main's side alone |
| C | `TestNeverSilentHoldLadder` | 2 | Pin: held and failed answers for 29 minutes |
| D | `TestNeverSilentHoldBound` | 2 | Pin: uncertain bound stop and previously notified failure |
| E | `TestNeverSilentHoldLandedReplay` | 2 | Pin: rendered and coalesced replay, ten reconciles and restart |
| F | `TestNeverSilentHoldUnlandedReplay` | 7 | Six unlanded rows red; accepted-pending control green |
| G | `TestNeverSilentHoldPermanentRefusal` | 2 | Both first-refusal rows red |

The final focused run also includes three reaction-journal rows, the two
existing repeated-reaction rows and the new read-only engine-method unit test.
All pass. Full runner counts and commands are in the validation record.

The Slack engine alone gains `IsStopped`, a read-only method used to preserve
a dead letter's stop during unsuccessful recovery attempts. This is an
additional intentional difference from the Google Chat engine. Recovery
history's permanent-refusal reason makes a first refusal definite without
changing an existing stopped message's uncertainty.

## O3, O5 and O16

O3 is fixed. The switch-off `already_reacted` row has exactly main's keys and
values; the enabled repeat writes one ingestion row and no notice activity;
a fresh reaction writes one row and one activity, including after reconcile.
The original PR-head run reproduced missing repeat rows and duplicate fresh
activity timestamps. One timestamp now identifies the fresh direct activity
and its journal replay. The optional no-activity marker is omitted when off.

O5's change stays in the integration test. It waits for durable delivery
observations for both invented conversations before advancing three minutes.
The initial loaded baseline passed, so the prior whole read's story 3 remains
the original failure evidence; that read reported a 21-second timeout in this
row and three isolated passes. The first verification sequence passed four
files, then its fifth file failed the unchanged primary-delivery restart
test's wait for `held_terminal`; the edited whole-paste row passed in all five.
The final acceptance sequence and isolated result are recorded below.

A second loaded sequence passed two files and then reproduced an earlier
wait in the edited test itself: waiting for both initial pastes with fake
time standing still. That wait now uses `pumpUntil` too, so scheduled retries
can progress. It still requires exactly two pastes. The same reviewer checked
this one-line correction in a narrow addendum and found it CLEAN; no fresh
reviewer or whole-PR review was started.

O16 is fixed in place: four round-3 log lines have only the per-user temporary
prefix replaced by `$TMPDIR/`. No history is rewritten. The complete evidence
tree is scanned again after adding this round's sanitized logs.

## Review and validation

One fresh read-only DEEP review of the implementation SHA is CLEAN:
BLOCKER 0 / MAJOR 0 / MINOR 0 / NIT 0. It covers the merge combined diff,
both-parent diffs for the six conflicts, and the ordinary commits after it.
Its separate O5 validation note is handled by recording the failed unchanged
row and completing the final sequence. No new review round was started.

The mayor's whole-delta gate, second-model read and founder decision remain
pending. All runs unset `GC_BIN` and `NEVER_SILENT`; test fixtures enable their
own engines and use fake services. No live adapter state or conversation was
read, and no deployment was performed.

See [validation.md](validation.md) for exact commands, final counts, known
load-sensitive failures and their isolated results. The command manifests
record each exit status; the private-path scan records the final evidence
tree check.
