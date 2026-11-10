# Inbound session ingestion

`delivered` means gc handed the payload to the session transport. It does not
prove that the harness exposed it to the model. On 2026-10-30, four Slack
deliveries had complete tmux receipts with `submitted=true`, but no harness
enqueue or rendered attachment; three neighboring deliveries had both.

The adapter now fingerprints each journaled payload without storing its text.
A background observer checks gc's raw session transcript every five seconds,
sharing one bounded fetch across each target's pending receipts. Rendered-input
proof requires a complete matching
payload, its provider/channel/message/thread coordinates for extmsg envelopes, and a
timestamp within the delivery window. The evidence records the target session,
transcript entry UUID (or SHA-256 of the compact native Codex record), timestamp
and record kind. Native Codex proof does not require a gc-added ordinal.
The journal retains the message's provider so `ADAPTER_PROVIDER` overrides
match their actual rendered headers. Older rows default to `slack`.

Claude user text and rendered `queued_command` attachments count. Codex user
`response_item` message text counts. Assistant text, tool output, manual Slack
reads, queue enqueue/remove records, request snapshots, partial payloads and
quoted message timestamps do not count. Alias and mention-only reminders are
matched as complete reminder blocks. A coalesced receipt retains all its
original quoted-context windows.

## What counts as seen

A complete rendered input, a successful explicit `/react` add on the inbound
message, or a confirmed session `/publish` reply into its thread can mark the
receipt `ingested`. Both acts must belong to the target session. A reply's Slack
timestamp must follow the inbound message's timestamp; the clock tolerance
cannot make an older reply cover a later input. A top-level inbound uses its
own message timestamp as the thread root. The reaction CLI sends its calling
session ID in current and explicit-message modes. An unattributed reaction,
including a legacy journal row without a session, is not proof. Company
admission eyes and automatic busy reactions do not count.
Failed reactions, reaction removals, and replies to another thread do not count.

For a coalesced receipt, every constituent message needs a qualifying reaction
on its own timestamp or a reply into its own thread. The acts may be mixed;
one reaction to the final message cannot cover the other messages. The journal
keeps each member's proof, with the completing act supplying the receipt's
proof kind and timestamp. A partial receipt remains unresolved, and its alert
names only the members still lacking proof across the unresolved targets.

The adapter journals successful acts with their source, channel, message/thread,
session, emoji where applicable, and timestamp, never their text. Reaction time is the adapter
clock; reply time is Slack's original publish timestamp, including on a later
idempotent replay. Acts must fall between the saved send start (`StartedAt`)
minus the existing 30-second clock tolerance and the two-hour proof horizon
after delivery. Coalesced members share that send interval. A delayed delivery
confirmation cannot exclude an earlier session act; legacy rows without a
start use delivery time. Restart reconstructs the facts.
The in-memory act cache holds at most 4,096 entries; on overflow, a bounded
journal lookup prevents eviction from turning a seen line into an alarm.

The journal preserves each member's handoff eligibility. `pending` members
with complete byte counts are monitored from their initial receipt, including mixed fanouts;
complete recovery handoffs enroll even when no further receipt poll is needed.
Unconfirmed submission neither proves nor disproves ingestion. Direct reminder
fingerprints and the original send timestamp survive delayed alias and
mention-only confirmation. Name bindings are resolved
through gc's session API and pinned durably to the canonical session ID for
that delivery. New deliveries resolve names afresh. Repeated receipt polls
cannot undo `ingested`, move the first handoff's deadline, or exclude input
that arrived between initial acceptance and a delayed conclusion.

After checking rendered input and session acts, three minutes without proof
appends `delivered_unseen`. Each observer pass sends at most one
`gc mail send mayor --dedup ...` per channel, with a count in the subject and
each message timestamp, thread and receipt in the body. Target sessions stay
in the journal so delayed fanout cannot enlarge a retry's mail payload.
Fanout targets share one line per uncovered message. Before mail, the journal saves
the exact message/target pairs and their stable batch dedup key. Completion records
which messages were named, their target attribution and their mail batch. A message
is mailed once across targets: a line already named is not mailed again for a late
target. The targets record attribution, not separate mail entitlements. A different
message previously seen only by the first target gets a new mail: if s1 has answered B
and only A was alerted, a later s2 with neither seen triggers B without repeating A.
Recovery retries only the saved message/target set under its original key, even if
a member completed or gained proof; new members wait for the next pass. If gc
accepted the original mail but its response was lost, recovery preserves the
entire original coverage, including members that gained proof before the retry.
Any completion retaining pairs beyond those confirmed by this pass is marked
`unconfirmed`, including a successful retry of only a subset. When every member
has proof, `sent` stays false. This retains ambiguous acceptance; it does not
claim that the original attempt was confirmed.
New attempts record the exact message/target coverage and attempt time immediately
before notification. Each eligible retry atomically replaces its sidecar before
sending. Recovery keeps only
the union of attempts not definitely refused. A refused A+B plan followed by
an ambiguous B-only retry keeps B, leaving never-mailed A eligible for a later
target without proof. Refusing a retry cannot erase an earlier ambiguous send.
Nonzero gc replies and failures to start or establish a connection are
definite refusals; timeouts after start, signal termination and connection loss
after sending remain ambiguous. A refusal of a later retry cannot erase an
earlier ambiguous attempt. A version-five plan without a sidecar has never been
attempted. Older v0/v2 plans without attempt state retain their conservative
ambiguous interpretation; v3/v4 journal attempt/refusal markers are read-only
migration input and are never written again. These older plans were introduced
in earlier, unrolled revisions of this change. The
`alerted` flag in versioned rows settles that target's alert decision, never other
messages or targets. Only legacy unversioned rows retain receipt-wide coverage.
Attempt/refusal state lives in one file per plan under the journal's
`attempts/` directory, named by a digest of the plan ID. It records the exact
last attempted pairs, the bounded union of prior ambiguous pairs, the attempt
time, refusal state and retry schedule. It never expands legacy membership.
A temporary file is synced, closed and renamed; the directory is synced before
notification. Attempt updates never scan or rewrite a journal segment, so a
valid final plan row without a newline is accepted consistently by replay and
retry. The journal remains append-only, with plan and completion records.
Repeated refusals add zero journal bytes. Completion is durable before its
sidecar is removed; replay and retention discard completed/unknown sidecars,
and startup replay and pruning sweep abandoned temporary files.
Definite refusals
retry after 5, 10, 20, 40, 80 and 160 seconds, then every five minutes; the
counter and next eligible time survive restart. Recovery sends on the first
eligible retry pass. A gc exit is authoritative even when a descendant holds
its output pipes: the additional wait is limited to one second.
New batches are limited to a 64 KiB journal-record/mail-body budget; larger
backlogs drain over successive passes. This stays below both the journal
reader's 1 MiB line ceiling and command argument limits. Sidecars have a separate
8 MiB cap for legacy exact attempts; their bytes count toward the existing
64 MiB observation budget. An oversized sidecar write fails before notification
and leaves the previous state intact. A receipt with too many
message/target pairs is split into bounded saved plans. Named messages stay
covered across targets; unnamed messages remain eligible on the next pass.
Each target's journal row stores only its own coverage. Retention carries shared
coverage in separate `ingestion_coverage` rows only when its last durable source
will actually be deleted. A pair already present in a surviving segment is
not copied, and a prune that deletes nothing writes nothing. Coverage referenced
by an `All` completion still depends on the saved plan's segment.
Pending plans protect their source day until completion, even when proof
settles the last observation during that pass.
Source-day tracking uses the message/target pair without its batch attribution,
so a surviving copy under another batch prevents an unnecessary carry.
A single pair that cannot fit produces a visible retryable error without blocking
other channels.
An oversized batch saved by an older adapter retries under its original dedup
key, with a bounded receipt prefix, omitted count and journal batch ID; the
saved membership remains complete. It is not split into new mail identities.
Successful mail writes one atomic batch-completion record before the individual
receipt completion rows. Full completion references the saved membership,
avoiding another large record for legacy batches. Restart repairs missing individual rows without
mailing again, including after the late-proof horizon. Retention preserves
source records until those individual completion rows are durable, even if a
previous day's failed mail succeeds after its observation horizon. Failed mail is retried
on the next eligible pass and logged; a channel's state-write failure is collected
without preventing other channels from mailing.
The mail command and each target's transcript fetch have ten-second limits.
The normal alert latency is the three-minute window plus one polling pass.
The controlled busy-session proof needed 2m16s for the first queued input to
render after the held tool call, so a two-minute window was too short.
Longer tool calls can outlast the alert window: the mayor's consolidator runs
for 60–90 minutes. An alerted row therefore remains observable for two hours
from handoff (90 minutes plus 30 minutes of margin). Matching input rendered
within that horizon appends `status=ingested`, `late=true` with its evidence;
this supersedes the earlier unseen verdict without retracting or repeating the
alert. After the horizon an alerted row stops polling. Failed alerts remain
retryable. The initial deadline remains three minutes, after checking rendered
input, explicit reactions and session replies. November 9's seven conditional
late samples had a 13m32.321s p95, but six came from one turn; that does not
establish routine handoff delay or justify increasing the deadline.

There is no automatic whole-message resend: a draft may still be in the
harness, and resending could duplicate a founder instruction. The mayor reads
an unseen thread manually. A missing transcript or unsupported provider
format without qualifying session acts also produces `delivered_unseen`;
absence of proof is not a claim that
the message was definitely lost. The existing dead-letter file continues to
represent failed transport handoffs.

Transport recovery is separate from this observer: the durable HOLD queue
retries only definite non-delivery, polls outstanding receipts, and excludes
unpollable or refused records from automatic sends. Its status posts are
adapter-authored, never session acts or own-thread evidence. A terminal handoff
notice does not prove submission or reading. Dead letters older than the
configured 24h replay limit remain recorded and never replay, even by the
redrive verb; an operator can acknowledge a hand-checked line without deleting
its history. See the README's held-delivery section for clocks, notice bounds,
counts, additive state fields and acknowledgement syntax.

The observer starts with the adapter by default. Set
`SLACK_INGESTION_OBSERVER_DISABLED=1` before starting the adapter to disable
transcript polling and observer alerts; inbound journaling remains enabled
with its normal configured retention, without observation-related retention
extensions. Re-enabling observes retained eligible deliveries; expired records
are not recovered.
It uses
`GET /v0/city/{city}/session/{id}/transcript?format=raw&tail=1`, verifies the
response identity/format, and follows `before` cursors across compaction
boundaries. All pages together are capped at 32 MiB and 64 requests; missing
or repeated cursors fail visibly. Transcript text is never logged. It reads
at most 64 MiB of retained segments per journal scan, under the journal lock
(including a fallback lookup when the act cache overflows).
Malformed JSON lines are skipped with a counted warning that contains no
journal contents. Appends isolate an unterminated final fragment so it cannot
swallow the next complete record. Read errors and over-budget journals fail
visibly and require operator attention.
Retention preserves the oldest unresolved delivery's day and all later
segments until ingestion is proved or its alerted late horizon expires. This keeps
one-day retention from erasing an observation at midnight and preserves
failed alerts across restarts. Initial pending receipts retain their original
timing and settled-member evidence independently until transport follow-up
concludes or ends; a member's ingestion never concludes the whole receipt.
Before pruning, settled member verdicts are copied durably into the current
segment if newer retained transport rows could otherwise re-enroll them.
Their timestamps, evidence and alert state are preserved; copies expire with
normal journal retention. Follow-ups ending without
a gc conclusion write `phase=follow_up_ended`, `status=unverified`; this is an
adapter disposition, not a delivery claim. After a crash, orphan timing is
protected for at most 23 minutes: the two ten-minute follow-up attempts plus
the three-minute observation margin. This bound does not expire actual handoff
observations or unsent unseen alerts.
Normal pruning resumes when observations settle;
read failures or over-budget reconstruction defer pruning with an error.
Pre-upgrade rows without a payload fingerprint are excluded to avoid retroactive
alerts. The journal is the
recovery record; deleting it discards alert completion history. A crash between
mail acceptance and the completion append relies on gc's mailbox dedup until
the earlier mail is archived.

This observes the local channel, alias and mention-only deliveries that already
write the inbound journal. It changes no peer-city deliveries or company receipt
state machine. The gc input fix is tracked separately as `gf-b78`; this observer
exposes and escalates the gap, rather than claiming to fix gc's submit logic.
The founder gap watch stays enabled. Only the mayor retires it, after a full
week with zero `delivered_unseen` rows.
