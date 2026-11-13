# Changelog

## Unreleased (gp-urx1)

- A box whose safety read finds a delivery that no wake announced reads at
  its ordinary poll interval until the next wake or reconnect. Its status
  then reads `poll (up; unannounced_delivery)`, not held.
- Both off switches, the gateway's `WAKE` and the box's
  `WHATSAPP_HELD_CONNECTION`, read `off` in any case and with spaces around
  it. `false`, `0` and `no` still leave the link on; the box names such a
  value once in its log at start.
- The gateway README gives the rollback as two deploys in order: `WAKE` off
  first, the `CITY_WAKE` binding out only after every socket has closed.
- A socket closed as soon as it opens climbs the reconnect ladder; the
  ladder starts over only after the link's first pong.

No deployment is included in this change.

## Unreleased (gp-bod9)

- Add `publish --reply-to` and quote the inbound message in the default reply
  line. Only the first chunk carries context; retries keep the first quote.
  Missing quotes are reported on stderr without failing a delivered reply.
  Adapter and gateway can ship in either order; older gateways send plain.
  Quotes reach phones after both update. Gateway deployment is separate and
  requires migration 0005 first; this change adds no migration. Quotes and
  reactions remain free-form sends inside the 24-hour window and do not reopen it.
- The updated gateway accepts only known message targets belonging to the
  calling city. Unknown quotes go plain; unknown reactions are refused.
  An inbound target older than the gateway's eight-day retention is unknown.

- Add `gc whatsapp react --to --message --emoji` for hand reactions and
  removal with an empty emoji. It uses the existing gateway route, records
  a transcript and preserves the 24-hour window. After a pack update and
  adapter restart the mayor can react; nothing automatic is switched on.

## Unreleased (gp-60dl)

- A hibernating WebSocket per city wakes the adapter to read its existing
  inbox. Quiet connections use a five-minute safety read; disconnected or
  unsupported boxes keep polling. The journal, inbox and acknowledgement
  contract stay unchanged. Status shows whether the link is held or polling.
- The gateway switch closes existing sockets on the next message or inbox
  read; a per-box switch restores polling immediately on restart. No D1
  migration, and no deployment is included in this change.
- Add opt-in never-silent owner notices, durable outage mail and health/status reporting for newly addressed human messages.

## Unreleased (gp-tu0b)

- Test fixtures now use invented names.

## Unreleased (gp-xt33)

- Docs only: the gateway README names the relay it read as a reference by its
  owner's role, a hosting partner, instead of by name. No code change.

## Unreleased (gp-g02t, PR 76 round 2)

- The skill carries the reactions with fixed meanings, with the same
  tables and rules as the Slack fragment and the Google Chat skill, and says
  the mayor cannot place a reaction on WhatsApp yet (gp-adh3). Until it can,
  a message that only thanks, confirms, agrees or closes a topic gets an
  answer in words (gp-1v3u). The rows are a catalog, `reactions.json` at the
  pack's root and the same file in all three packs (gp-bis1).
- The adapter keys every owner by the gateway's `conversation_id` (PR 76's
  inbox), never the number: the gc conversation and the actor, the chain, the
  watermark, the lookup and the probe, the dead letter, the window ledger, the
  keeper's bind and the media directory. A delivery with no number is
  acknowledged and filed; a number change keeps one conversation; a recycled
  number never shares one. The number (`wa_id`) is display only. A delivery from PR 73's gateway (no `conversation_id` field)
  keys by its `wa_id`, so the boxes take this adapter before the gateway's
  deploy; the journal's and the outbound progress store's PR 74 records are
  read as written.
- The verbs name a conversation as `<conversation_id>` (a phone number or a
  WhatsApp user id); the adapter's local `/consent`, `/hours` and
  `/inbound/clear-gap` take `conversation_id`, and `/consent` and `/hours`
  answer `conversation_last4`. The keeper's verbs match a user id whole (one
  may hold letters); a number may still be typed in any form.
- Every publish and update goes to the user id the gateway's `GET /v1/window`
  gives for the conversation, never to a number: a conversation the window no
  longer lists, or lists with no user id, is refused `not_bound` and nothing is
  sent. The window-keeper raises nothing for a conversation the last good
  window read did not list.
- The gateway (PR 76): a new binding's `conversation_id` is the owner's user
  id, never the number; PR 73's number-keyed bindings keep theirs until the
  re-invite.

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
