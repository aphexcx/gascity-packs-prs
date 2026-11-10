# inbound-redrive

`gc slack inbound-redrive --conversation-id C0123456789`

Queue the channel's dead letters for ordered replay through the adapter's
internal listener. Delivered message IDs are skipped. History is retained and
successful replays append a completion record. Pending gc receipts are polled
before any repost. A 202 response means queued, not delivered.
