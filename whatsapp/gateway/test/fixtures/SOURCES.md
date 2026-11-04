# Where each fixture came from

Every fixture is Meta's own example payload, transcribed field for field from the reference
page named here. The pages were read 2026-11-04 (the gp-nn90 build). Only HTML entities were
decoded. The tests copy a fixture and replace `from`, `wa_id`, `id`, `timestamp` and
`phone_number_id` per case. The shape (which keys exist, their nesting and their types) is
Meta's.

| Fixture | Page |
|---|---|
| text.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/text (example "Text message") |
| reaction.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/reaction (example "Receiving a reaction"; a removal omits `emoji`) |
| image.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/image (the page truncates `image.url` with "...") |
| audio.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/audio (`voice` is a boolean; `url` truncated on the page) |
| status-sent.json | https://developers.facebook.com/documentation/business-messaging/whatsapp/webhooks/reference/messages/status (example 1, "sent" with conversation and pricing) |
| status-failed.json | the same page (the "failed" example; its table names 131050 while the example carries 131049) |

The send-side shapes in `test/meta.test.ts` (text, reaction, template, mark-as-read with the
typing indicator, the media URL mint and download) were read 2026-11-04 from the pages below.

- https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/text-messages
- https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/reaction-messages
- https://developers.facebook.com/documentation/business-messaging/whatsapp/templates/overview
- https://developers.facebook.com/docs/whatsapp/cloud-api/typing-indicators
- https://developers.facebook.com/docs/whatsapp/cloud-api/reference/media

The Graph version came from https://developers.facebook.com/docs/graph-api/changelog/versions:
v26.0 is the newest, released July 29, 2026, with its expiry TBD.
