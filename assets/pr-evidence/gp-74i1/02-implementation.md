# gp-74i1 implementation and self-review

The observer accepts explicit `/react` additions and confirmed session replies
as durable ingestion facts. Reaction removal, Slack failure, automatic
admission/busy acknowledgements, another channel/thread/session, and acts
outside the hand-off tolerance or two-hour horizon cannot establish proof.
The three-minute initial deadline remains unchanged by mayor ruling
`ci-wisp-gozwdyu`.

The adapter records facts only after delivery success. Reply timestamps come
from Slack's original message ID, so a delayed idempotent replay cannot make
an old reply cover a new inbound. Replaying the inbound journal reconstructs
the bounded act cache; overflow uses a bounded disk lookup instead of losing
proof. Retention preserves the 30-second tolerance across midnight.

Each pass groups newly eligible receipts by channel and folds fanout targets
into one line per receipt. The sorted receipt set determines the mail dedup
key. Successful mail marks every member individually. Failed mail leaves
members retryable and does not prevent another channel's mail. A second act
check before sending catches proof recorded during other transcript fetches.

Self-review traced success, benign `already_reacted`, failure and removal
through the reaction handler; confirmed and replayed publishes through
`recordDeliveredPublish`; journal restoration, capacity overflow and pruning;
and batch failure/restart. `/react` currently carries no caller session ID,
so its proof is explicitly a bot action through that endpoint. Publish proof
matches the target's pinned canonical session. Automatic company reactions
use a different path and never write the session-act marker.

Validation on Go 1.27.3 darwin/arm64, from `slack-full/adapter`:

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
