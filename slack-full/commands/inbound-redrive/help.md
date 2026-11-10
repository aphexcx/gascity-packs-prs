# inbound-redrive

`gc slack inbound-redrive --conversation-id C0123456789`

Queue the channel's dead letters for ordered replay through the adapter's
internal listener. Delivered message IDs are skipped. History is retained and
successful replays append a completion record. Pending gc receipts are polled
before any repost. A 202 response means queued, not delivered. The configured
`SLACK_INBOUND_REPLAY_MAX_AGE` limit (24h by default) applies to this command too;
older records, refused payloads and records requiring a hand check are retained
and never reposted. `0` or `off` disables dead-letter replay.

After verifying a line by hand:

`gc slack inbound-redrive --conversation-id C0123456789 --acknowledge 100.123456`

This appends acknowledgement history for every outstanding copy of that message
and removes it from the counts and FIFO. It neither sends the line nor claims
delivery, and never deletes history. A 200 response reports the acknowledged count.
