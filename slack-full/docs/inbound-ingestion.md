# Inbound session ingestion

`delivered` means gc handed the payload to the session transport. It does not
prove that the harness exposed it to the model. On 2026-10-30, three Slack
deliveries had complete tmux receipts with `submitted=true`, but no harness
enqueue or rendered attachment; three neighboring deliveries had both.

The adapter now fingerprints each journaled payload without storing its text.
A background observer checks gc's raw session transcript every five seconds,
once per pending target per pass. `ingested` requires a complete matching
payload, its channel/message/thread coordinates for extmsg envelopes, and a
timestamp within the delivery window. The evidence records the target session,
transcript entry UUID (or Codex ordinal), timestamp and record kind.

Claude user text and rendered `queued_command` attachments count. Codex user
`response_item` message text counts. Assistant text, tool output, manual Slack
reads, queue enqueue/remove records, request snapshots, partial payloads and
quoted message timestamps do not count. Alias and mention-only reminders are
matched as complete reminder blocks. A coalesced receipt retains all its
original quoted-context windows.

After two minutes without proof, the journal appends `delivered_unseen` and
the observer sends `gc mail send mayor --dedup ...`, naming the channel,
message timestamp, thread, receipt and missing target. A successful alert is
recorded durably; restart reconstructs pending work and completed alerts from
the journal. Fanout targets share a mailbox dedup key. Failed mail is retried
on the next pass and logged; it does not prevent other receipts being checked.
The mail command and each transcript request have ten-second limits. The
normal alert latency is the two-minute window plus one polling/request pass.

There is no automatic whole-message resend: a draft may still be in the
harness, and resending could duplicate a founder instruction. The mayor reads
an unseen thread manually. A missing transcript or unsupported provider
format also produces `delivered_unseen`; absence of proof is not a claim that
the message was definitely lost. The existing dead-letter file continues to
represent failed transport handoffs.

The observer starts with the adapter. It uses
`GET /v0/city/{city}/session/{id}/transcript?format=raw&tail=1`, verifies the
response identity/format, caps it at 32 MiB, and never logs its text. It reads
at most 64 MiB of retained journal segments per pass, under the journal lock.
Malformed/over-budget journals fail visibly in the service log and require
operator attention; they cannot generate trustworthy ingestion receipts.
Existing journal retention still applies. Pre-upgrade rows without a payload
fingerprint are excluded to avoid retroactive alerts. The journal is the
recovery record; deleting it discards alert completion history. A crash between
mail acceptance and the completion append relies on gc's mailbox dedup until
the earlier mail is archived.

This observes the local channel, alias and mention-only deliveries that already
write the inbound journal. It changes no peer-city deliveries or company receipt
state machine. The gc input fix is tracked separately as `gf-b78`; this observer
exposes and escalates the gap, rather than claiming to fix gc's submit logic.
The founder gap watch stays enabled. Only the mayor retires it, after a full
week with zero `delivered_unseen` rows.
