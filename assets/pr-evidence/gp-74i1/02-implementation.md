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

## Mayor gate round 4: preserve accepted coverage and bound fanout

Round 3's gate blocked `e8c9737d1ac3bc7f0e5105e96e6d084cb1de776b` because
an ambiguously accepted A+B mail could complete its retry with only B's
coverage after A gained proof. The Opus read also identified the all-proven
completion, repeated shared coverage in target rows, and a single receipt
whose target pairs could exceed the saved-plan budget indefinitely.

Recovery now completes against the original durable membership. This includes
members that gained proof, since being mailed and being seen are independent
facts. If all original members have proof, the completion preserves coverage
with `unconfirmed=true` and `sent=false`: no new send is claimed. This retains
the ambiguity of an earlier attempt whose acknowledgement was lost. Fresh
batches still complete with exactly the coverage actually sent. After closing
an all-proven recovered batch, selection waits until the next pass reconstructs
the coverage; a late target cannot re-mail those lines using stale state.

The once rule is per message across targets. Targets identify who lacked proof
when a line was named; they do not create separate mail entitlements. A single
receipt over the 64 KiB plan budget is split by message/target pair, retaining
explicit attribution. The saved plan and the actual mail use the same bounded
selection. Already named messages cover subsequent fanout; unnamed messages
remain eligible for a later pass with a new identity. Tests exercise both
300 targets of one message and 300 distinct messages in one receipt.

Individual journal rows serialize only their target's coverage. Replay still
unions the shared coverage for decisions. Before retention deletes source
records, it preserves attribution needed by surviving observations in separate
bounded `ingestion_coverage` rows. This also covers a newer target whose row
survives after the original target's own verdict expires.

The final focused tests were run against the previous head's production files
as well as the fix: 26 test/subtest failures on the base, one passing fresh-batch
control; all 27 tests/subtests pass with the fix. The scenarios include A or B
gaining proof, both gaining proof, a second timeout, restart before retry,
restart after a deduplicated retry before completion, late targets before and
after recovery, retention with the original target surviving, and retention
with only a newer target surviving. Assertions check original target/batch
attribution, no repeated channel line, and no hidden newcomer. The fresh-batch
control confirms unsent rows receive no coverage.

Self-review by the implementing Codex model traced the persisted plan, recovery
selection, completion, replay, per-target serialization, and pruning. Two
additional failure paths were reproduced and fixed during that review: a late
target present during all-proven completion, and coverage disappearing after
only the newer target survived retention. Splitting exact pairs was chosen over
eliding target identities so recovery retains the same attribution contract.
Coverage from an ambiguous attempt is kept, not proven by the timeout. Round
five marks it unconfirmed whenever the retry does not confirm every kept pair;
coverage from a definite refusal is not kept. The initial
three-minute window, two-hour horizon, and rollout gate are unchanged. The
mayor's Codex gate and Opus delta read on this head follow READY.

Validation from `slack-full/adapter`, Go 1.27.3 darwin/arm64:

- `go test -json ./...`: 1,467 top-level passes (12,964 including subtests),
  three opt-in skips, zero failures, exit 0.
- `go test -race -json -run '^(TestIngestion|TestInboundJournal)' ./...`:
  55 top-level passes (140 including subtests), one opt-in skip, zero failures,
  exit 0.
- `go vet ./...` and `git diff --check`: exit 0.
- No Python or Node implementation changed in this round; Node/pnpm were not
  used. The preceding round's 948 passing Python tests are historical evidence,
  not a new execution claimed for this delta.

The `round4-*` compressed logs, parsed summary and command manifests retain
the new checks. Raw logs and mutable routing/GitHub snapshots stay local.

## Mayor gate round 5: deletion-driven retention and attempt evidence

The gate and Opus read of `8416780c` identified repeated coverage copies while
an expired segment was protected. With 300 pairs, the regression measured
140,700 extra bytes on its first prune, even though no source was deleted.
Pruning is now a separate locked operation, retaining the same append boundary.
It computes the final protected cutoff and the actual deletions first. A pass
that deletes nothing returns without writing carry records.

Replay tracks the latest self-contained source day of each pair's original
batch attribution. An `All` completion references the plan's day, not its own
day, because it carries no coverage bytes. Pruning copies only attribution
needed by surviving observations whose last source is in the deletion set.
Already surviving pairs and prior carries are skipped. Shared attribution is
still preserved when the original target expires but a newer target survives.
The carry is synced before source removal; a restart after that sync but before
all deletions does not repeat it.

The addendum's refusal distinction is implemented with version-three plans and
durable `ingestion_batch_attempt` / `ingestion_batch_refused` records. A plan
with an outstanding ambiguous attempt reuses that marker on retry; its first
attempt is already durable and a later refusal cannot invalidate it. A plan
without an attempt, or one whose only attempt was definitely refused, gets no
inferred original coverage. Its still-unseen original members retry as fresh
delivery under the bounded saved identity, with coverage only for the lines
actually sent. A previously proven but never mailed member remains eligible
for a later target. This preserves the existing rule that an already-proven
member does not generate an alarm merely because another member needs retry.

The process boundary retains bounded stderr on `exec.ExitError` without logging
it. Completed gc error replies, executable-start failures and connection refusal
are definite failures. Timeout/cancellation after start, signal termination,
connection resets, broken pipes and lost responses remain ambiguous. Replay
remembers the state before each attempt, so a definite refusal of a retry does
not clear a prior ambiguous attempt. Older plans lacking attempt markers remain
conservative. Any completion retaining pairs beyond those confirmed this pass
sets `unconfirmed`, including `sent=true` subset retries.

The reaction CLI's `--session` help now explicitly says it claims that session's
identity for ingestion proof. This is the missing round-four help item, not an
additional authorization mechanism.

Regression cases were observed against the prior production head with
only the mechanical extraction of the existing prune body: 25 failing
tests/subtests and six passing controls. The final set adds an untouched-plan
restart control that retries the full plan exactly once. It covers 25 byte-stable protected
prunes with 2 and 300 pairs, settlement and one carry, restarts before and after
deletion, duplicate old batches, surviving standalone/batch/target sources,
an old plan with a surviving `All` completion, and a retry after partial deletion.
The mail sequence covers definite refusal, process-start failure, connection
refusal, death before notify, timeout, reset, signal termination and refusal of
a later retry, with one or all members gaining proof. It asserts no hidden or
repeated line and the `Unconfirmed` field itself. Separate boundary tests verify
the attempt is durable before notify and refusal stderr remains available.

Self-review by the implementing Codex model traced source-day attribution,
final-cutoff deletion, durable carry-before-delete, attempt replay, prior
ambiguity across retries, fresh completion coverage and the legacy version
boundary. Round-three and round-four tests remain unchanged. The journal owns
durable evidence; the observer owns proof and mail decisions. No other service,
rollout setting, deadline or late horizon changes in this round. The mayor's
round-five gate and Opus delta read remain required after READY.

Final validation used Go 1.27.3 on darwin/arm64 from `slack-full/adapter`:

- `go test -json ./...`: 1,472 top-level passed, 3 skipped, zero failed;
  12,996 passes including subtests. The new regression files contribute
  32 passing tests/subtests.
- `go test -race -json -run '^(TestIngestion|TestInboundJournal)' ./...`:
  60 top-level passed, 1 skipped, zero failed; 172 passes including subtests.
- `go vet ./...` and `git diff --check`: exit zero.

From `slack-full`, `env -u GC_TEMPLATE uv run --with pytest --with pyyaml
--with jsonschema python3 -m pytest tests -q` passed 948 tests with 2 skipped.
Its two multiprocessing/fork deprecation warnings are pre-existing. The CLI
help invocation also passed and displays the proof-identity wording. Node and
pnpm were not used. Compressed logs, exact command/exit manifests and
`round5-test-summary.json` accompany this record.

An initial full Go run exposed an extra retry-attempt write shifting an existing
completion crash test. Reusing the outstanding ambiguous marker fixed it; the
unchanged round-three/four tests and the final full/race runs are green. The
initial failure is retained in the city record, separate from the final logs.
