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

During final review, main advanced to `ab8d7471` (PRs #81 and #83). The lane
integrated it, preserving both independent changelog entries at the only
conflict. The complete adapter tree is unchanged from the tested round-4
commit, verified with `git diff --cached --exit-code HEAD -- slack-full/adapter`
before integration; no Go tests needed to be repeated for this merge. The
integration diff check and command manifest are recorded beside the test logs.

## Mayor gate round 2: constituent coverage and delayed confirmation

The mayor's independent gate round 1 reviewed `7caf22383ee42f599cd8f05723a28482e860564e`
and returned BLOCK on two major findings. These are distinct from the earlier
lane-local Codex rounds above. The binding round-2 ruling keeps this same PR
and branch, with a new commit on top of that reviewed head.

Session-act proof now requires coverage for every message in the receipt's
`Windows`, each using its own message/thread coordinates and the shared saved
send interval. Reactions and replies can combine to cover the receipt. The
completing act supplies the compatibility proof kind and time; nested member
evidence records each constituent's act without message text. An incomplete
receipt remains unresolved. The mail names only uncovered messages, taking
the union across unresolved fanout targets. That subset is transient; durable
receipt membership and saved batch dedup identities remain intact.

The lower bound now matches transcript proof: `StartedAt` minus 30 seconds,
falling back to `DeliveredAt` for legacy records without a saved start. The
upper bound remains delivery plus two hours. The three-minute initial alert
deadline is unchanged. Documentation describes the same rule.

The regressions first failed at the reviewed head: reacting only to the last
coalesced member incorrectly suppressed its receipt; a mail for an entirely
unseen receipt omitted the first constituent; and valid acts before delayed
confirmation still alarmed. New cases cover two different threads, each
partial direction, both reactions, mixed reply/reaction and both replies,
with and without restart. Delayed-confirmation tests exercise reactions and
replies at send+30s, send-30s and send-31s. Additional checks cover partial
cache hits with durable overflow, fanout coverage union, failed-mail recovery
with the original batch key, and late completion without another mail.

Self-review by the implementing Codex model traced both proof checks, per-member
matching, canonical session isolation, durable recovery, mail budgeting and
fanout union. No remaining correctness finding was identified. This does not
substitute for the mayor's pending gate round 2 or first Fable read. The module
boundary is unchanged: the journal owns session-act facts and the observer
owns receipt verdicts and mail; no new handler-side policy was introduced.

Validation on Go 1.27.3 darwin/arm64, from `slack-full/adapter`:

- `go test -json ./...`: 1,460 top-level passes (12,927 including subtests),
  three opt-in skips, exit 0.
- `go test -race -json -run '^(TestIngestion|TestInboundJournal)' ./...`:
  48 top-level passes (103 including subtests), one opt-in skip, exit 0.
- `git diff --check -- slack-full`: exit 0.

The `gate-r2-*` manifests, compressed logs and parsed test summary retain this
round's results. The mayor gates only `7caf2238..NEW` and reads the whole PR
with Fable. The lane sends READY and drains without closing the bead; the
mayor owns the roll and observation week.

## Mayor gate round 3: durable message coverage and reaction attribution

The mayor's gate round 2 blocked `42ec11578d80d2b0134ba0b79c0268071a96ff28`
on partial alerts hiding unread messages from late fanout targets. The Fable
read of that whole head also blocked reactions without caller attribution.
Both findings are addressed in this round. The mayor clarified the alert rule:
mail each message once across fanout, retaining which targets lacked proof
when that message was included. A previously mailed A is not repeated for s2;
a previously unmailed B becomes eligible when s2 receives it without proof.

Versioned batch plans and completion records now retain exact message/target
pairs. Each durable coverage entry identifies its original batch, and receipt
replay unions only those named messages. Versioned `alerted` flags settle the
individual target's alert decision; they do not propagate to late targets.
Legacy unversioned flags retain their old meaning. Retention carries the
coverage with verdicts, and restart repairs incomplete member rows from the
atomic batch completion. A retry remains restricted to its saved membership
and dedup key; newly eligible members receive a separate batch on a later pass.
The existing 64 KiB limits and visible error for an oversized single receipt
remain in force, now accounting for coverage metadata as well.

The reaction CLI forwards the calling session ID in both current-message and
explicit-message modes. The adapter persists it and matches it against the
target's pinned canonical identity, like reply proof. Wrong-session and
unattributed reactions, including legacy rows, cannot prove ingestion.
Explicit operator reactions still work without a session but provide no
session proof. Automatic company admission eyes remain excluded; an observer
regression now drives the actual admission-ack path before checking the alarm.
The measurement script uses `GC_CITY_PATH`, and the baseline document uses
generic city-relative references in place of the three home paths.

The late-fanout regression first failed in all four variants: live, restart,
crash after atomic completion, and retention across midnight. The reaction
handler regressions first failed for wrong-session, unattributed and legacy
restarted acts. Four CLI request-boundary cases first failed for missing
session IDs. All now pass. Additional coverage checks exact target/batch
attribution and keeps newly unread B out of an ambiguously accepted A retry.

Self-review by the implementing Codex model traced the version boundary,
coverage union, partial retry selection, exact target attribution through
mail grouping, atomic completion repair, retention carry, canonical reaction
matching, legacy missing identities, and explicit operator compatibility.
No remaining correctness finding was identified. The journal still owns
durable facts and the observer owns verdicts and mail; the new coverage helper
stays inside that boundary. The CLI only transports caller identity. The
three-minute window and two-hour horizon are unchanged. This self-review does
not substitute for the mayor's pending gate round 3 and Fable delta read.

Validation from the respective module directories:

- Go 1.27.3 darwin/arm64, `go test -json ./...`: 1,463 top-level passes
  (12,937 including subtests), three opt-in skips, exit 0.
- `go test -race -json -run '^(TestIngestion|TestInboundJournal)' ./...`:
  51 top-level passes (113 including subtests), one opt-in skip, exit 0.
- From `slack-full`, the Makefile's uv-managed interpreter:
  `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -q`:
  948 passed, two skipped, exit 0. Two existing Python fork deprecation warnings
  came from the outbound multiprocess test. Node/pnpm were not used.

The `gate-r3-*` artifacts retain the command manifests, compressed test logs
and parsed counts. Public logs normalize local lane/home paths; original
unmodified logs remain in the lane's external staging evidence. The PR stays
draft. READY hands this head to the mayor for gates, the authorized pin roll,
and the observation week; the lane drains without closing the bead.
