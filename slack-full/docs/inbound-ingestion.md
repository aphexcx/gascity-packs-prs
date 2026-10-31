# Inbound session ingestion

`delivered` means gc handed the payload to the session transport. It does not
prove that the harness exposed it to the model. On 2026-10-30, four Slack
deliveries had complete tmux receipts with `submitted=true`, but no harness
enqueue or rendered attachment; three neighboring deliveries had both.

The adapter now fingerprints each journaled payload without storing its text.
A background observer checks gc's raw session transcript every five seconds,
sharing one bounded fetch across each target's pending receipts. `ingested`
requires a complete matching
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

After three minutes without proof, the journal appends `delivered_unseen` and
the observer sends `gc mail send mayor --dedup ...`, naming the channel,
message timestamp, thread, receipt and missing target. A successful alert is
recorded durably; restart reconstructs pending work and completed alerts from
the journal. Fanout targets share a mailbox dedup key. Failed mail is retried
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
retryable. The three-minute initial alert deadline is unchanged.

There is no automatic whole-message resend: a draft may still be in the
harness, and resending could duplicate a founder instruction. The mayor reads
an unseen thread manually. A missing transcript or unsupported provider
format also produces `delivered_unseen`; absence of proof is not a claim that
the message was definitely lost. The existing dead-letter file continues to
represent failed transport handoffs.

The observer starts with the adapter by default. Set
`SLACK_INGESTION_OBSERVER_DISABLED=1` before starting the adapter to disable
transcript polling and observer alerts; inbound journaling remains enabled.
It uses
`GET /v0/city/{city}/session/{id}/transcript?format=raw&tail=1`, verifies the
response identity/format, and follows `before` cursors across compaction
boundaries. All pages together are capped at 32 MiB and 64 requests; missing
or repeated cursors fail visibly. Transcript text is never logged. It reads
at most 64 MiB of retained journal segments per pass, under the journal lock.
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
