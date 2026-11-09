# gp-74i1 implementation and self-review

The observer accepts explicit `/react` additions and confirmed session replies
as durable ingestion facts. Reaction removal, Slack failure, automatic
admission/busy acknowledgements, another channel/thread/session, and acts
outside the hand-off tolerance or two-hour horizon cannot establish proof.
The three-minute initial deadline remains unchanged by mayor ruling
`ci-wisp-gozwdyu`.

The adapter records facts only after delivery success. Reply timestamps come
from Slack's original message ID and must follow the inbound's Slack timestamp,
so a delayed idempotent replay cannot make an old reply cover a new inbound.
Replaying the inbound journal reconstructs
the bounded act cache; overflow uses a bounded disk lookup instead of losing
proof. Retention preserves the 30-second tolerance across midnight.

Each pass groups newly eligible receipts by channel and folds fanout targets
into one line per receipt. The sorted receipt set determines the mail dedup
key, which is saved with its membership before sending. A successful send
writes one atomic batch completion before marking members individually;
restart repairs any missing individual rows. A retry recovers its original
batch before considering new arrivals on the next pass. Failed mail leaves
members retryable and does not prevent another channel's mail. A second act
check before sending catches proof recorded during other transcript fetches.

Self-review traced success, benign `already_reacted`, failure and removal
through the reaction handler; confirmed and replayed publishes through
`recordDeliveredPublish`; journal restoration, capacity overflow and pruning;
and batch failure/restart. `/react` currently carries no caller session ID,
so its proof is explicitly a bot action through that endpoint. Publish proof
matches the target's pinned canonical session. Automatic company reactions
use a different path and never write the session-act marker.

Initial revision validation on Go 1.27.3 darwin/arm64, from `slack-full/adapter`:

- Baseline ingestion suite: 31 top-level passes, one opt-in live test skipped.
- New regression tests failed before implementation: the positive reaction,
  reply and restart cases still alarmed; three receipts produced three mails.
- `go test -json ./...`: 1,450 top-level passes (12,894 including subtests),
  three skipped, exit 0.
- `go test -race -json -run '^(TestIngestion|TestInboundJournal)' ./...`:
  38 top-level passes (70 including subtests), one skipped, exit 0.
- `git diff --check -- slack-full`: exit 0.

Each command has an exit-status manifest. `test-summary.json` parses test and
package status fields; compressed JSONL logs retain the original output.
No Python product code changed; no pnpm/Node tests were run.

The live Slack pack remains pinned to its existing revision until Afik
authorizes the citadel pin bump. The mayor owns the Fable/Opus review stages,
rollout and seven-day observation. The three source papercuts remain open.

## Codex round 1 corrections

Round 1 reviewed `f82365a45d2ba81b99987f106e273965de638b22` and returned BLOCK
with two major findings: an older reply could cover a newer input via the
30-second tolerance, and partial batch completion could change the recovery
dedup key. The changes above address both. New regressions cover ordinary and
delayed old replies, a crash after mail acceptance with one completed member
and a new arrival, and a crash after atomic completion before zero or one
individual completion rows. Recovery repairs the individual rows even after
the observation horizon. The old-reply test failed before the correction.

The expanded focused race suite passes 41 top-level tests (77 with subtests),
with one opt-in live test skipped. The first correction run exposed completion
phase preservation in the existing journal reducer; preserving completed rows
resolved those failures and kept the existing late-proof cases green.

Final round-2 validation: full adapter suite 1,453 top-level passes (12,902
including subtests), three skips; focused race suite 41 top-level passes (78
including subtests), one skip. Both exit 0. This includes the existing
own-message-ID reply form that Slack normalizes to the root thread.

## Codex round 2 corrections

Round 2 reviewed `3be099e2e9e655820a46a18d7cba73d980d27931` and returned BLOCK
on an oversized batch exceeding the journal scanner's record limit and on
retention pruning source rows between atomic batch completion and individual
completion. Both failures were reproduced: 8,500 ordinary receipt keys made
the scanner report `token too long`; a one-day-retention retry after the
two-hour horizon lost both member rows when interrupted after batch completion.

New batches now budget their serialized journal record and mail body to 64 KiB,
leaving excess receipts eligible for later passes. Oversized single receipts
fail visibly for their channel; other channels still proceed. Completed mail
whose individual rows are pending remains unsettled for retention, preserving
the source until each individual row is durable. Both regressions pass with
the existing ingestion tests under the race detector: 43 top-level passes
(80 with subtests), one opt-in live test skipped.

Final round-3 full adapter suite: 1,455 top-level passes (12,904 including
subtests), three opt-in skips, exit 0. The focused race suite and diff check
also exit 0. The three-minute deadline and two-hour late-proof horizon remain
unchanged; only incomplete durable completion writes extend retention.

## Codex round 3 corrections

Round 3 reviewed `a0f0536bbd413c16ade3a4fd8556e273fb4ba812` and returned BLOCK:
delayed fanout could enlarge a saved batch's retry beyond the mail budget,
and an older oversized batch could never recover. Both regressions failed
before the fix. Mail now contains immutable receipt coordinates, leaving
target sessions in the journal. Legacy oversized retries preserve their
original dedup identity and full membership while the mail shows a bounded
prefix, omitted count and saved journal batch ID. Full atomic completion
references the prepared membership rather than repeating a near-limit record.

The eight batch tests (including subtests) pass after the fix. The new retry
test fails its first mail, adds 1,000 fanout targets, reconstructs the journal,
and verifies successful recovery with the original key. The legacy test
preserves all 1,500 receipts and checks bounded summary and completion writes.

Round-4 validation on Go 1.27.3 darwin/arm64: `go test -json ./...` passes
1,457 top-level tests (12,906 including subtests), with three opt-in skips.
`go test -race -json -run '^(TestIngestion|TestInboundJournal)' ./...` passes
45 top-level tests (82 including subtests), with one opt-in skip. Both commands
ran from `slack-full/adapter` and exited 0. The scoped diff check also passed.
