# inbound-redrive

`gc slack inbound-redrive --conversation-id C0123456789`

Queue the channel's dead letters for ordered replay through the adapter's
internal listener. Delivered message IDs are skipped. History is retained and
successful replays append a completion record. Pending gc receipts are polled
before any repost. A 202 response means queued, not delivered. The configured
`SLACK_INBOUND_REPLAY_MAX_AGE` limit (24h by default) applies to the bare command;
its preview prints the skipped count and oldest timestamp before queuing work.
`0` or `off` disables automatic and bare-command replay.

To deliberately replay older history:

`gc slack inbound-redrive --conversation-id C0123456789 --include-older`

The preview prints and flushes the eligible count before the command queues
replay. Refused payloads, unresolved receipts and records requiring a hand check
remain excluded. The service log records the verb, flag and counts. The explicit
permission survives a queued attempt's receipt checks, including restart; a failed
concluded attempt requires another explicit command to replay old history again.

After verifying a line by hand:

`gc slack inbound-redrive --conversation-id C0123456789 --acknowledge 100.123456`

This appends acknowledgement history for every outstanding copy of that message
and removes it from the counts and FIFO. It neither sends the line nor claims
delivery, and never deletes history. A 200 response reports the acknowledged count.
