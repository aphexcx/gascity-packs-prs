# Outbound delivery evidence

For text publishes, Slack's successful `chat.postMessage` response with a
message timestamp means the message was accepted: `delivered=true`, with no
`failure_kind`. A subsequent readback cannot turn that accepted write into a
failed publish. This prevents callers from duplicating posts after a read
failure. Actual write rejections and responses without a timestamp still fail.

The adapter separately checks the exact timestamp and canonicalized text via
`conversations.history`, or `conversations.replies` for a threaded reply. The
thread query selects `oldest=latest=message_id`, `inclusive=true`, and `limit=2`
to leave room for the parent plus the selected reply. Readback retries remain
bounded to three attempts, 200 ms apart, with a four-second request timeout.

Successful confirmation omits `metadata.readback` on the publish response.
Unconfirmed reads retain `delivered=true` and describe the result in metadata:

| `metadata.readback` | Observation |
| --- | --- |
| `readback_unconfirmed` | The selected timestamp was absent, or its text did not match. |
| `readback_unavailable` | The read request, HTTP response, or response decoding failed. |
| `readback_auth` | Slack rejected the read token or its history scope. |

These are read diagnostics, not new members of gc's closed publish-failure
vocabulary. A content mismatch can indicate truncation, a later edit, or an
unrecognized Slack rewrite. `metadata.truncated=true`, when supplied by Slack,
survives alongside the readback result. Inspect the existing message before
deciding to send a correction; do not automatically repost it.

`gc slack publish`, `publish-to-channel`, and `reply-current` preserve receipt
metadata in their default JSON output. They exit zero when the receipt carries
a message timestamp, including legacy receipts with `delivered=false`. The
terse output reports that acceptance as `delivered=true` without a failure kind;
`--verbose` preserves the original envelope. An unconfirmed readback is reported
on stderr with the message timestamp, including legacy failures whose metadata
was absent. Unknown responses without acceptance evidence still exit nonzero.

Within the existing in-memory idempotency window, a retry of an accepted but
unconfirmed post rechecks the original message. It never releases that key
because of an empty read or content mismatch. Confirmation still claims the
busy-reaction and own-thread bookkeeping exactly once, independently of the
acceptance flag. The cache TTL, restart behavior, and unkeyed publishes' lack
of deduplication are unchanged.

Keyed publishes also persist evidence under
`<GC_CITY_PATH>/.gc/slack/publish-receipts/` (fallback:
`/tmp/gc-slack-adapter/publish-receipts/`). Files are named by the full SHA-256
digest of the idempotency key; neither the key nor the body is stored.
`GET /publish-receipt?idempotency_key=<URL-encoded key>` on the internal adapter
server returns the timestamp, conversation/thread identifiers, posted-text
SHA-256, posting time, and a top-level `readback` state. Newly recorded
acceptance has `delivered=true, readback=pending`; completed reads record
`confirmed` or one of the diagnostic states above. Confirmed evidence cannot
be downgraded by a slower read. Existing receipt files keep their historical
fields until a fresh read updates them; a stored timestamp is still acceptance
evidence even if an older adapter wrote `delivered=false`.

The durable store retains evidence for 30 days and does not extend the dedup
window or prevent reposts after restart. A 404 means unknown, never proof of
non-delivery. Storage errors remain distinct from delivery and are exposed as
`metadata.receipt_store` on publish responses. This contract applies to
slack-full text publishes; file uploads and company-turn reports retain their
own protocols.
