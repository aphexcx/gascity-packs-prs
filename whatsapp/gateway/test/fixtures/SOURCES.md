# Where each fixture came from

## Quoted replies and reactions (read 2026-11-13)

- https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/contextual-replies/
  documents `context: {message_id}` beside the message type. Reactions cannot
  be contextual replies; templates have no contextual bubble. Deleted or
  long-term-stored targets may arrive without a bubble (storage is typically
  after 30 days). The page does not specify a made-up id's HTTP result.
- https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/reaction-messages
  documents the reaction body and a response with a message id. A target
  older than 30 days, absent from the chat, deleted, or itself a reaction is
  not delivered and produces a messages webhook with code 131009. Successful
  reactions have only a sent-status webhook, not delivered/read webhooks.
  This page does not state whether a second reaction replaces the first or
  document empty-emoji removal. Removal is tested against the existing
  gateway contract; real-phone behavior remains unverified.

These pages were read in a browser. The tests use invented ids and a fake
Graph endpoint; no real send, gateway, phone number or model was exercised.

## Existing fixtures

Every fixture is Meta's own example payload, transcribed field for field from the reference
page named here. The pages were read 2026-11-04 (the gp-nn90 build). Only HTML entities were
decoded. The tests copy a fixture and replace `from`, `wa_id`, `id`, `timestamp` and
`phone_number_id` per case. The BSUID fixtures also have `from_user_id`, `user_id`,
`profile.username`, `recipient_id`, `recipient_user_id` and the `system` ids replaced, or deleted
where a case drops one. The shape (which keys exist, their nesting and their types) is Meta's.

| Fixture | Page |
|---|---|
| text.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/text (example "Text message") |
| reaction.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/reaction (example "Receiving a reaction"; a removal omits `emoji`) |
| image.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/image (the page truncates `image.url` with "...") |
| audio.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/audio (`voice` is a boolean; `url` truncated on the page) |
| status-sent.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/status (example 1, "sent" with conversation and pricing) |
| status-failed.json | the same page (the "failed" example; its table names 131050 while the example carries 131049) |

## Business-scoped user ids (gp-ac0u)

From https://developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids
(its changelog's newest entry is October 23, 2026), read 2026-11-04 20:4xZ. The page answers a
plain HTTP client with a 400, so it was read through a fetch tool that lists each example's
leaves as key paths. Every key and value below is the page's own, except where a row says
otherwise.

| Fixture | Example on that page |
|---|---|
| text-bsuid.json | "Incoming messages webhook": a text from a user with a username whose phone number is not shared. There is no `from` and no `wa_id`, only `from_user_id`, `user_id` and `profile.username`. The parent-id fields are Meta's too; the gateway ignores them. |
| text-phone-bsuid.json | A **composite**: text.json with the BSUID fields that page adds to every messages webhook (`contacts[].user_id`, `messages[].from_user_id`). The page shows both ids together only in placeholder syntax blocks (call permission, revoke, edit). |
| status-delivered-phone-bsuid.json | "Status messages webhook: delivered, sent to a phone number" (both `recipient_id` and `recipient_user_id`; the user has a username). |
| status-delivered-bsuid.json | "delivered, sent to a BSUID, the phone number not shareable": no `wa_id` and no `recipient_id`. |
| system-user-changed-number.json | "System messages webhooks", `user_changed_number`. Meta shows a **syntax block** with placeholders. This lane filled them: the old number is text.json's, the new number `16505559876` and the new BSUID `US.70412958836127405533` are invented, and the previous BSUID is the page's own example. `system.previous_user_id` is in the page's field list and its August 11, 2026 changelog entry, though not in that block. |
| system-user-changed-user-id.json | The same section, `user_changed_user_id`: the number change when no phone number can be shared. There is no `from` and no `system.wa_id`, and the placeholders are filled as in the row above. |

That page's "failed, sent to a phone number" status example has status-failed.json's shape: no
`contacts`, a `recipient_id` and no `recipient_user_id`. So it is not repeated here.

The send to a BSUID puts the id in `recipient` with `to` omitted. When both are given, `to` wins
(that page, "Send message requests"). The response's `contacts[0]` then carries `user_id`
instead of `wa_id`.

The send-side shapes in `test/meta.test.ts` (text, reaction, template, mark-as-read with the
typing indicator, the media URL mint and download) were read 2026-11-04 from the pages below.

- https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/text-messages
- https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/reaction-messages
- https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/overview
- https://developers.facebook.com/docs/whatsapp/cloud-api/typing-indicators
- https://developers.facebook.com/docs/whatsapp/cloud-api/reference/media

The Graph version came from https://developers.facebook.com/docs/graph-api/changelog/versions:
v26.0 is the newest, released July 29, 2026, with its expiry TBD.
