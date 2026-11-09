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
receipt `ingested`. Reply proof must belong to the target session; a top-level
inbound uses its own message timestamp as the thread root. Explicit reactions
are bot actions through `/react` (that endpoint does not carry caller session
identity). Company admission eyes and automatic busy reactions do not count.
Failed reactions, reaction removals, and replies to another thread do not count.

The adapter journals successful acts with their source, channel, message/thread,
emoji or session, and timestamp, never their text. Reaction time is the adapter
clock; reply time is Slack's original publish timestamp, including on a later
idempotent replay. Acts must fall between hand-off minus the existing 30-second
clock tolerance and the two-hour proof horizon. Restart reconstructs the facts.
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
each message timestamp, thread, receipt and missing target in the body.
Fanout members share one receipt line; the dedup key covers the sorted receipt
set. Each receipt's successful alert is
recorded durably; restart reconstructs pending work and completed alerts from
the journal. Failed mail is retried
on the next pass and logged; it does not prevent other receipts being checked.
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
