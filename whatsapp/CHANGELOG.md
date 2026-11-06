# Changelog

## Unreleased (gp-g02t, PR 76 round 2)

- The adapter keys every owner by the gateway's `conversation_id` (PR 76's
  inbox), never the number: the gc conversation and the actor, the chain, the
  watermark, the lookup and the probe, the dead letter, the window ledger, the
  keeper's bind and the media directory. A delivery with no number is
  acknowledged and filed; a number change keeps one conversation; a recycled
  number never mixes two owners. The number (`wa_id`) is display only. A
  delivery from PR 73's gateway (no `conversation_id` field) keys by its
  `wa_id`, so the boxes take this adapter before the gateway's deploy; the
  journal's and the outbound progress store's PR 74 records are read as
  written.
- The verbs name a conversation as `<conversation_id>` (a phone number or a
  WhatsApp user id); the adapter's local `/consent`, `/hours` and
  `/inbound/clear-gap` take `conversation_id` and answer `conversation_last4`.

## 0.0.1 (2026-11-04, gp-9hf1)

- The whatsapp pack: a `proxy_process` adapter that polls the Dynasty WhatsApp
  gateway, delivers each owner's messages to gc extmsg through a write-ahead
  journal, publishes the mayor's replies inside the 24-hour window and the one
  update template outside it, saves media and transcribes voice notes, and
  runs the window-keeper; nine doctor rows; the `gc whatsapp` verbs.
- The design gate r7's two recovery rules, built: every retained watermark is
  read before any plain recovery and on a schedule while the adapter runs (a
  gc-only restore while running is found before the next plain post); a dead
  journal entry is kept until the gateway confirms its ack.
