# whatsapp-gateway

The key holder and router for the one Dynasty WhatsApp number. It is a Cloudflare Worker with D1
at **https://whatsapp.dynasty.computer**, in the **Houmanoids** Cloudflare account, and runs
nowhere else.

Meta delivers inbound WhatsApp messages only by webhook to a public HTTPS address, and a city box
has none. The Meta access token must also live in exactly one place. So this Worker does five jobs:

- It holds the number's Meta keys and receives Meta's webhook.
- It verifies Meta's signature over the raw bytes.
- It maps each registered owner's phone to that owner's city.
- It holds each city's deliveries until the city acknowledges them by id.
- It sends each city's replies for it, with WhatsApp's 24-hour window enforced.

A city box never holds a Meta secret. It holds one gateway credential, minted at enrollment.

- Design of record: `docs/design/whatsapp-adapter-20261103.md` (packs PR 71 at
  bc2fd1430c3c6093555a8a52262dc69cda819810), "The path" and "The window-keeper".
- Built by bead gp-nn90 on the base aphexcx/gascity-packs-prs main
  8cb01a83bdaa6b4b42aeb63bae1ee4a6d369262f. The box side is the `whatsapp` pack's adapter
  (`whatsapp/adapter/`, bead gp-9hf1), which implements the other half of the contract below.
- Robert Welch's whatsapp-ingress Worker was read as a reference for the relay
  contract it proved. Nothing was copied from it, and nothing here talks to it.

## Layout

| Path | What |
|---|---|
| `wrangler.toml` | The Worker: name, custom domain, the D1 binding `DB`, the daily cron, the vars. It names no account id and no database id. |
| `migrations/0001_init.sql` | The schema: cities, credentials, enroll_codes, invite_codes, phones, deliveries, media, sends, orphan_statuses, unknown_senders, stats. |
| `migrations/0002_bsuid.sql` | Business-scoped user ids (gp-ac0u): phones keyed by `conversation_id` with `wa_id`, `user_id` and `username`; the ids on deliveries, sends and orphan statuses; strangers keyed by `sender_key`. |
| `migrations/0003_retries.sql` | Meta's retries (gp-0g2s; re-cut in place by gp-lv49 to the design of record): the deliveries dedup key without the city; `phones.bound_stamp`; `links` (each number change, old user id to new), `unbinds` (the user ids an unbind recorded) and `marks` (each number's newest pairing with a user id); `code_attempt_wamids` (every counted wrong-code text, seeded from `unknown_senders.last_code_wamid`, gp-w376). |
| `src/index.ts` | The Worker entry: env to gateway, the cron to `maintain()`. |
| `src/gateway.ts` | The routes, the webhook plan and its one D1 transaction, enrollment, the inbox and ack, send, read, window, media, the operator routes, the cron. |
| `src/meta.ts` | The Cloud API wire: the signature, the payload parse, the send bodies, the Graph calls. |
| `src/window.ts` | The 24-hour window: its arithmetic and which inbound types move it. |
| `test/` | vitest on `@cloudflare/vitest-plugin` (a real D1 in workerd) with a fake Graph API. `fixtures/` holds Meta's own example payloads; `fixtures/SOURCES.md` says where each came from. |

## The contract (v1)

This section is the contract **verbatim** from the bead spec of record,
`/Users/tailor512/city/assets/ops/beads/whatsapp-gateway-20261103.md` (SCOPE item 3). It is frozen.
A change to it is a bead comment on BOTH the gateway bead (gp-nn90) and the adapter bead (gp-9hf1),
never a silent edit. The text below is unchanged. The fields bead gp-ac0u added for Meta's
business-scoped user ids are additive, and are listed under "Business-scoped user ids" below. They
were posted on both beads.

- Meta side: GET /meta/webhook answers hub.challenge only when hub.verify_token equals META_VERIFY_TOKEN (403 otherwise); POST /meta/webhook verifies X-Hub-Signature-256 over the raw body with META_APP_SECRET (401 and nothing stored on a mismatch or a missing header), accepts only payloads whose metadata.phone_number_id equals META_PHONE_NUMBER_ID (others counted and dropped), and for each messages[] entry: a sender in phones is appended to that city's deliveries (dedup by wamid), a sender not in phones whose text is a live invite code binds that phone to the code's city, marks the code used and appends a synthetic "owner joined" delivery to the city (carrying the invite text's wamid, so a redelivery is one row), the three writes one D1 batch committed before anything else, then sends the owner one text "You're connected to <city name>'s mayor. Say hi." (a failed welcome send is counted, never a 5xx to Meta, so a redelivery never re-binds or lands the code as a message); any other unknown sender gets ONE text "This number serves Dynasty cities. Ask your city for an invite code." at most once per 7 days per phone and is otherwise ignored; statuses[] for the city's sends are appended as kind status; then 200, answered only after every D1 append for the payload has committed: a failed write answers 500 with nothing acknowledged and Meta redelivers, and the wamid UNIQUE constraint makes the redelivery one row (the Worker keeps nothing between requests; the D1 row is Meta's acknowledgement). Over 5,000 pending for one city: 503 (Meta retries). Bodies over 1.9 MB: 413.
- City side, all under Authorization: Bearer <credential> (401 on unknown or revoked; the credential is compared by sha256): POST /v1/enroll {code, name} with NO bearer mints a city and ONE credential for a live enrollment code and returns {city_id, credential} once (the code is then used); POST /v1/invites returns {code, expires_at} (6 digits, 24 hours, one live code per city at a time); GET /v1/inbox?after=<id>&limit=<1..200> returns the city's unacknowledged deliveries with id greater than after, oldest first, as {deliveries:[{id, received_at, kind: message|status|joined, wa_id, profile_name, message}], cursor} with cursor the last id in the page (after omitted returns the oldest unacknowledged, so a restarted box re-reads everything it never acknowledged; within one process the box skips ids it already holds in flight); POST /v1/inbox/ack {ids:[...]} deletes exactly the listed deliveries' bodies and no other, never a cumulative position, because the box delivers on independent per-sender chains and one sender's later delivery can be confirmed before another sender's earlier one (the city acks an id only on terminal success, gc's delivered receipt on the first response or read back later, or on the box's dead-letter transition, a repost bound reached, no_route (the marked copy's included), a second doubt or bead B's 24-hour pending deadline, the envelope then on the box's disk, or on the operator's resolution of a terminal hold (gc's pending with terminal true, which bead B never replays and never acks on its own: that delivery stays held here, returned by every poll, until the resolution's ack or this retention); never on a pending receipt before that deadline, bead B's rule; until then the gateway holds the delivery and every poll returns it again; bead B's deadline runs from its first post and this retention from receipt, so on an ordinary day the body is still here when that ack arrives, and an ack for an id the cron already deleted is a no-op, never an error); POST /v1/send {to, idempotency_key, type: text|reaction|template, text?, reaction?{message_id, emoji}, template?{name, language, params[]}} sends through Meta's Graph API only when `to` is bound to THIS city (403 otherwise), answers {message_id} and the SAME answer again for a repeated idempotency_key without a second Meta call, refuses a text or reaction outside the phone's window with 422 {error: window_closed}, and never refuses a template on the window (a template is the only send allowed outside it and does not open or extend it); POST /v1/read {message_id} marks read with the typing indicator; GET /v1/window returns {windows:[{conversation_id, last_inbound_at, window_closes_at}]} for every phone bound to THIS city (conversation_id the wa_id; a group id when Groups land), window_closes_at = last_inbound_at + 24 hours - the 120-second margin, the same arithmetic window.ts applies to a send so the box and the gateway never disagree on whether a send will be refused, both null for a phone that has never written, no profile name and no body (the window-keeper of the design's section of that name reads it once a minute; only an inbound customer message moves it, never a send of any type); GET /v1/media/<media_id> streams the bytes with Meta's mime type for a media id that was delivered to THIS city (404 otherwise), minting the URL from Meta and fetching with the token; GET /healthz {ok, pending_total, last_webhook_at} with no body text and no numbers of phones.
- Operator side under Authorization: Bearer <ADMIN_TOKEN>: POST /v1/admin/enroll-codes {city_name} returns {code, expires_at} (the mayor's hand mints one per city); GET /v1/admin/cities lists cities with counts, never phone numbers in clear (last four digits only).

### Readings of what the contract leaves unstated

These readings were posted as bead comments on gp-nn90 and gp-9hf1 on 2026-11-04 (the first
before the code, the second after the self-review, which changed items 4 and 7). None of them
changes a route or a field.

1. **Times.** Every time field is an integer of Unix epoch **seconds**: `received_at`,
   `expires_at`, `last_inbound_at`, `window_closes_at` and `last_webhook_at`. `last_inbound_at` is
   Meta's own timestamp of the owner's message, never the receive time, so a redelivery hours late
   does not stretch the window. A timestamp ahead of the gateway's clock is clamped to the clock.
2. **Inbox.** With no `after`, the inbox reads from `after=0`. `limit` defaults to 100 and accepts
   1..200; anything else answers 400. An empty page answers
   `{deliveries: [], cursor: <the after it was given>}`. `message` carries one thing per kind:
   - `message`: Meta's `messages[]` object exactly as received (its `id` is the wamid).
   - `status`: Meta's `statuses[]` object exactly as received (its `id` is the outbound wamid, and
     a failed status carries `errors[]`).
   - `joined`: the invite text's Meta `messages[]` object.

   `profile_name` is null on a status.
3. **Ack.** `POST /v1/inbox/ack {ids: [1..500 positive integers]}` answers `200 {ok: true}`. It
   drops the listed bodies of THIS city and no other. Three kinds of id are each a no-op 200,
   never an error:
   - an id the cron already deleted;
   - an id an earlier ack already acknowledged (the **repeated ack**: the contract's deleted-id
     clause read for the ack's own deletion);
   - another city's id.

   A malformed body answers 400. A body-less row keeps each acknowledged wamid until the cron's
   eighth day, so Meta redelivering an acknowledged message is still one row, never a second
   delivery.
4. **Send.** A send answers `200 {message_id: <wamid>}`. Errors are `{error: <code>}`:

   | Status | Code | When |
   |---|---|---|
   | 400 | `bad_request` | A malformed send. |
   | 400 | `text_too_long` | Text over 4096 characters (code points); the box chunks. |
   | 403 | `not_bound` | `to` names no owner of this city, or one with no user_id ("Business-scoped user ids", rule 7). |
   | 409 | `ambiguous_recipient` (with `hint` and `owner_user_id`) | `to` names two owners of this city: one owner's `conversation_id`, a phone number (a PR 73 binding), and another owner who shows that number now (a recycled number). Nothing is sent and the key is not spent. Resend to `owner_user_id`: the current user_id of the owner whose `conversation_id` is `to`, or null when it has none (then `GET /v1/window`). Added by gp-0g2s, `owner_user_id` by gp-w376; see "Business-scoped user ids", rule 7. |
   | 422 | `window_closed` | A text or a reaction outside the window: the gateway's own check, or Meta's 131047. |
   | 409 | `in_flight` | Another request with the same `idempotency_key` is still at Meta (claimed under 60 s ago). Retry it. |
   | 502 | `meta_error` (with `meta_status`, `meta_code`) | Meta answered and refused (a 4xx). Nothing was sent, so a retry under the same key sends. |
   | 502 | `outcome_unknown` (with `meta_status`, `meta_code`) | No answer, a 5xx, or a 2xx without a message id: Meta may have sent it. The key is spent. |
   | 409 | `outcome_unknown` | A repeat of a key whose first call ended `outcome_unknown`, or whose claim was abandoned (the Worker died between the call and the record) over 60 s ago. No second Meta call is ever made for it. |

   The idempotency key is per city. A repeat answers the stored `{message_id}` with no Meta call,
   and this is checked **before** the window, so a retry after the close still gets the first
   answer. The contract's "the SAME answer again for a repeated idempotency_key without a second
   Meta call" is read strictly: once a call may have reached Meta, that key never calls again, and
   the box decides what an `outcome_unknown` reply means (a new key resends, at the risk of the
   owner reading it twice).
5. **Read.** `POST /v1/read {message_id}` answers `200 {ok: true}`. It answers `404 not_found`
   when that wamid was never delivered to this city, and `502 meta_error` when Meta refuses it.
6. **Media.** `GET /v1/media/<id>` answers `404 not_found` for a media id not delivered to this
   city, or older than Meta's 7-day media lifetime.
7. **Window.** `window_closes_at = last_inbound_at + 86400 - 120`, and a free-form send is
   allowed while `now < window_closes_at`. The inbound types that move the window are text,
   reaction, image, audio, video, document, sticker, location, contacts, interactive, button and
   order. A system notice, an unsupported type and a type the gateway does not know are delivered
   but do not move it.
8. **Errors.** `401 {error: unauthorized}` answers a missing, unknown or revoked credential (or a
   wrong admin token). `404 {error: not_found}` answers an unknown route, and
   `405 {error: method_not_allowed}` a known route called with the wrong method.

### The window

The clock is each phone's `last_inbound_at`, and only an owner's message moves it. A send of any
type never moves it, a template included. The 120-second margin means the last second a
free-form send is allowed is **23 h 57 m 59 s** after the owner's message.

The bead's test line "a text at t0 plus 23h59 sends" cannot hold beside the frozen margin. The
tests therefore send at the close minus one second, and refuse at the close and at 24 h 01 m.

Meta's pages name only "messages or calls" as what opens the window, and say nothing of
reactions (developers.facebook.com/docs/whatsapp/pricing, read 2026-11-04). The gateway counts an
owner's **reaction** as the owner writing, so it moves the clock. Two reasons:
- A thumbs-up is how an owner most often answers the window-keeper's "reply ok so I can keep this
  line open". Refusing it would keep nudging an owner who had already answered.
- If Meta disagrees, its 131047 refusal reads `window_closed`, so the cost is one refused send,
  never a lost message.

**This is a judgment call for the founders.** One line in `src/window.ts` flips it.

## Business-scoped user ids (gp-ac0u)

Meta names a WhatsApp user by a **wa_id** (the phone number), a **user_id** (a business-scoped
user id, or BSUID), or both. A BSUID is the user's ISO 3166 alpha-2 country code, a period, then up
to 128 alphanumerics: `US.13491208655302741918`. Meta's page on them is
developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids; its
changelog's newest entry is October 23, 2026, and it was read 2026-11-04. Meta requires every
business to support BSUIDs.

What Meta sends:

- **Messages.** A message carries `from_user_id` beside `from`. A user who has a WhatsApp username
  is named by the user_id **alone**, with `from` and the contacts `wa_id` omitted. The exception is
  a business that has a reason to see the phone: it wrote to or heard from the user in the last
  30 days, or the user is in its contact book. The username is `contacts[].profile.username`.
- **Statuses.** A status carries `recipient_user_id`, except a failed status of a send to a phone
  number. `recipient_id` is omitted for a send to a user_id when the phone is not shared.
- **Number changes.** Meta regenerates the user_id when the user changes phone number. It sends a
  `system` message, `user_changed_number` or (with no phone shared) `user_changed_user_id`. That
  message carries `system.user_id` (the new id), `system.previous_user_id` and `system.wa_id` (the
  new phone, when shared), and no `from_user_id`.
- **Sends to a user_id** put it in `recipient`, with `to` omitted. When both are given, `to`
  wins.

What the gateway does:

1. **The owner key.** An owner row is keyed by its `conversation_id`, set once at the bind to the
   owner's user id and never changed after that (gp-g02t). Never the number: a number names its
   owner only while that owner is bound and no one else shows it, so a key taken from one would
   outlive the owner and pass to whoever binds on that number next, into the same box
   conversation. The row also holds the user_id Meta names the owner by now, unique (after a
   number change that is a newer id than the key). It holds no phone number: the number an owner
   shows now is its mark (rule 3). A row PR 73 bound by its number alone keeps that number as its
   key, holds it in `wa_id`, and has no user_id (rule 2), until the operator re-invites it (Deploy
   step 7).
2. **Routing: by the user id, never the number** (gp-lv49; the design of record,
   `whatsapp/docs/design-bsuid-pairing-and-spans.md`). Since early April 2026 Meta puts the
   sender's user id on every message. A message goes to the binding that answers to its user id
   at the moment Meta stamped it, or to no city:
   - A binding answers to the user id it was bound by, and to each user id a chain of
     number-change notices links to it (rule 5). It answers to an id from its own invite text, or
     from that id's last unbind if that is later, until the notice that replaced the id.
   - **A message naming no user id reaches no city** and binds nothing, whatever number it names.
     A number can change hands without the gateway seeing it, and the pairing that would say so may
     still be in flight. Such a message hears nothing, because the gateway sends only to user ids,
     and it is counted as `unrouted_messages`.
   - **No adoption.** A user id no binding answers to is a stranger's. Its live invite code binds;
     any other text, a six-digit typo included, hears the canned line and reaches no city. A row
     never gains or swaps a user id, except by a notice's link.
   - **A known user id's message that no span holds** reaches no city, hears nothing and is counted
     as `unrouted_messages`, once for each delivery Meta makes of it. That is a message stamped
     before its binding's invite text, after the notice that replaced its id, or at or before the
     id's unbind.
   - **A recycled number.** A phone number Meta pairs with another user id is someone else's now.
     The newcomer's messages are a stranger's: they hear the canned line, and the newcomer can join
     any city with an invite code. The number's mark moves to the newcomer (rule 3), so the old
     owner stops showing it, and the old owner's city still reaches the old owner at its user id.
   - An owner PR 73 bound by its number alone has no user id. Nothing routes to it, and nothing can
     be sent to it. The mayor unbinds and re-invites each such owner at the deploy (Deploy, step 7).
3. **The number an owner shows: the mark.** Each phone number has one high-water mark, its newest
   pairing with a user id (`marks`). Any message naming the number beside a user id moves the mark
   to that id, whoever sent it (an owner, a stranger, a code text), unless Meta stamped the message
   below the mark. A number-change notice moves its new number to the new id and clears its old
   number. An owner shows the number whose mark names its current user id. The mark decides only
   that: the admin listing's last four digits, the `wa_id` a delivery carries when the message omits
   it, and which owner a send's `to` names by a number (rule 7). It never routes a message. An
   owner known by its user id alone shows no number until Meta pairs one with it.
4. **The window** is the owner row's, so it moves on any message routed to the owner, under its
   current user id or a previous one, and never back.
5. **Number changes.** The notice is delivered to the owner's city as kind `message` with
   `message.type` `system`, the shape every system notice already has. It moves no window and is
   never answered. It is routed by its `previous_user_id`, as any message is by its user id. It
   also becomes a **link** from the previous user id to the new one (`links`):
   - **Links are recorded in any order of arrival.** A link whose old id a binding answers to
     re-keys that binding: its `user_id` becomes the newest id along the chain of links. A link
     whose old id no binding reaches yet waits, for 8 days, for the notice that reaches it. Two
     notices Meta delivers in reverse leave the owner on the newest id.
   - **A replay changes nothing.** There is one link per old id.
   - **One id, one binding.** A link to a user id another binding answers to is never recorded,
     and the two bindings stay as they are (see the known limit below).
   - **A notice naming no `previous_user_id`** (Meta omits it when the old id is unavailable) links
     nothing and reaches no city. It is counted as `notices_without_previous_id` and as
     `unrouted_messages`. The owner keeps its old id and needs a new invite.
   - The notice moves the marks (rule 3): its new number shows the new id, and its old number
     shows nobody.

   The `conversation_id` stays. The owner's next message, under its new id, therefore continues the
   same conversation.

   **Meta's retries** (gp-0g2s). Meta retries a webhook for up to 7 days, in any order, so a
   message can arrive after the notice that replaced the user id it names. The gateway routes by
   the binding that answered to the id at the moment Meta stamped the message (rule 2), not by
   who answers to it now:
   - **A retry under a previous user id reaches the owner** while its binding stands: the old id was
     the binding's until the notice. Once an operator has unbound the owner, it goes nowhere.
   - **A routed retry is the owner's message.** It is delivered and moves the owner's window. It
     counts no code attempt, binds nothing and changes none of the owner's ids or names.
   - **A notice that arrives late or twice** links what it links once, and changes nothing more.
   - **The horizon is 8 days** (`TOMBSTONE_SECONDS`). Meta retries a webhook for up to 7 days
     ("Webhook delivery failure", developers.facebook.com/docs/whatsapp/cloud-api/guides/set-up-webhooks,
     read 2026-11-06), and the gateway drops any message stamped more than 8 days ago, so no message a
     link, an unbind or a mark may decide can arrive later. The daily cron then deletes it. The
     horizon's safety does not rest on Meta's window: the stale filter drops whatever is stamped more
     than 8 days ago, whatever Meta does. A longer window would only drop more, and each drop is
     counted as `stale_messages`.
   - **Meta stamps every message.** One without a stamp is read as stamped on its arrival, so a
     previous user id never routes it.
   - **The 5,000-held cap.** It is read before the write, from the cities of the owners whose
     current user id the payload names. A retry under a previous user id is not counted there. That
     is a bounded overshoot: the retries are what the owner sent before the change.

   **A known limit** (the design note's question 2, answer (a)). Meta does not promise webhook
   order. Say a notice's first delivery fails, and the owner writes under the new id before Meta's
   retry. That message is a stranger's: it hears the canned line and is not delivered. Once the
   notice lands, the owner's next message reaches their city. An invite code texted in that window
   binds a second owner row, and the notice then links nothing.
6. **Strangers** are keyed by their user_id. This covers the canned line once per 7 days and the 5
   code attempts a day. A message naming no user id hears nothing and counts no attempt.
7. **Sends go to a user id** (gp-lv49; the design note's point 3). `POST /v1/send`'s `to` names one
   of the city's owners by its `conversation_id`, its current `user_id`, or a number it shows (rule
   3). Whatever `to` named, the gateway sends to the owner's current user_id, byte for byte in
   Meta's `recipient`: a number can change hands without the gateway seeing it, and a user id
   cannot. The gateway's own welcome and canned line go to the user id too. An owner with no
   user_id (PR 73's binding by number alone) has no address, and a send to it answers
   `403 not_bound`.

   **A `to` naming two owners is refused** (gp-0g2s; the mayor's rule R1, 22:39Z 11/4). Say owner
   A is keyed by its phone number P (a binding PR 73 made; since gp-g02t no new binding is keyed
   by a number, rule 1), and owner B, bound by its user id, shows P now (a recycled number). Then
   `to: P` is both A's conversation and the number B shows. The box may mean either, and a guess
   would hand one owner's message to the other. So the gateway answers `409 {error:
   "ambiguous_recipient", hint: "send to the owner's current id", owner_user_id}`. It sends nothing
   and spends no key. `owner_user_id` is the current user_id of the owner whose `conversation_id`
   is `to`, the owner the box files under it, or null when that owner has none, as a PR 73 row
   never has (gp-w376; the Fable read r1). It is the user_id the gateway knows the owner by now:
   while a notice is still on its way, that may be one Meta has already replaced, and then a
   resend answers as any send to it would. The answer names nothing else: not the number, not the
   other owner, not an id the gateway has already replaced. The same id is the `user_id` of the
   owner's entry in `GET /v1/window`. Do not take it from the newest delivery: right after a number
   change that delivery is the notice itself, which still carries the old id. The pack's adapter
   never sends to a number: it sends to the user id `GET /v1/window` gives for the conversation
   (gp-g02t), so its own sends meet no 409, and it refuses one without a resend. A repeat of a key
   Meta already took still answers its first `message_id`, because it makes no new call. In
   another city, P names only that city's owner, so it is no ambiguity.
8. **Authentication templates.** One-tap, zero-tap and copy-code authentication templates cannot be
   sent to a user_id. Meta refuses them with code 131062, and the gateway answers `502 meta_error`
   with `meta_code` 131062, freeing the key. The one template here (`dynasty_update`, utility) is
   not one of them.

Contract v1, additive only (nothing renamed, nothing removed, no kind added):

| Where | Added |
|---|---|
| `GET /v1/inbox`, each delivery | `user_id`, `username`, `conversation_id`: null when unknown. `conversation_id` is the owner's key: the field a box files a delivery under, the same for every delivery of one owner whichever ids it carries. On a message or joined delivery, `user_id` is the one the message names (a notice's previous user_id), and `wa_id` is the number it names, or, when it names none, the number the owner shows (rule 3; null when it shows none). A status carries the recipient's ids as Meta sent them, and its `username` when Meta sends one (delivered, read). |
| `GET /v1/window`, each window | `user_id` (null when unknown). `conversation_id` is the owner's key. |
| `POST /v1/send` | `to` also takes a user_id (`^[A-Z]{2}\.(ENT\.)?[A-Za-z0-9]{1,128}$`), an owner's `conversation_id`, or a number an owner shows (rule 3). Whatever it names, the send goes to the owner's current user_id, in Meta's `recipient` (rule 7). A malformed one answers 400 `bad_request`; one that is no owner of this city, or one with no user_id, answers 403 `not_bound`; one that names two owners of this city answers 409 `ambiguous_recipient` with a `hint` and `owner_user_id`, the current user_id of the owner `to` is the conversation_id of (gp-0g2s, gp-w376, rule 7). |
| `GET /v1/admin/cities` | `phones_last4` holds the last four digits of the number each owner shows (rule 3), or `"id"` for an owner that shows none. No part of a user_id ever appears. The counters add `notices_without_previous_id` (rule 5). |
| `POST /v1/admin/unbind` (operator side, gp-lv49) | `{id}` under the admin token: unbinds every owner `id` names (its `conversation_id`, its current `user_id`, or the number a PR 73 binding holds), recording every user_id it answered to, and answers `{unbound: <n>}`. It replaces the raw `DELETE` the runbook gave ("Unbind an owner"). A malformed body answers 400 `bad_request`. |

An owner may join and write with a WhatsApp username: the gateway knows them by their user_id.

## Behaviours the box adapter relies on

- **The owner's key is `conversation_id`** (gp-g02t). Every delivery the inbox returns names its
  owner by `conversation_id`: a message, a joined and a status alike, and a row the v1 Worker
  wrote reads its `wa_id` as one. Its `wa_id` is only the number it shows: null for an owner
  known by user id alone, the new number after a number change, the same number for two owners
  after a recycled number. So the box keys every conversation, ack and journal line by
  `conversation_id`, shows the number only, and sends each reply to the user id `GET /v1/window`
  gives for the conversation (nothing at all when the window no longer lists it). The pack's
  adapter does so since gp-g02t (`whatsapp/adapter/src/render.js` `conversationOf`, `outbound.js`
  THE RECIPIENT). An adapter that keys by `wa_id` leaves a
  null-number owner's deliveries unacknowledged (held 7 days, then lost, and counted against the
  5,000 cap meanwhile), splits an owner's conversation at a number change, and puts two owners
  in one conversation after a recycled number. The boxes therefore take that adapter before
  this gateway deploys ("Deploy", before step 1).
- **The hold.** A delivery is held, body and all, until `POST /v1/inbox/ack` names its id, and
  every poll returns it again. The hold has two bounds: the 7-day cron, and the 5,000-held cap per
  city (beyond which Meta gets 503 and retries).
- **The no-op acks.** An ack of an id the cron already deleted, or an id an earlier ack already
  acknowledged, is a 200 that changes nothing.
- **One row per wamid.** Meta's redeliveries, the invite text and its redelivery, and an
  acknowledged wamid redelivered within 8 days each leave exactly one row. A status is one row per
  (wamid, status).
- **A message stays with the city it was sent to** (gp-0g2s). Say an operator unbinds a phone,
  the phone joins another city, and then Meta retries one of its earlier messages. If the first
  city stored the message, the retry is still the first city's row, held or acknowledged. The
  dedup key is `(wamid, status)`, with no city in it, and a media grant follows the city that
  holds the message's wamid. If no city stored it (its first delivery failed), it goes nowhere: the
  unbind recorded the owner's user ids, and a message Meta stamped before the unbind reaches no
  city, the new one included ("Business-scoped user ids", rule 2, and "Unbind an owner"). Either
  way the new city gets no delivery and no access to its media.
- **409 `ambiguous_recipient` on a send** (gp-0g2s). `owner_user_id` is the current user_id of
  the owner `to` names by its conversation_id (gp-w376), null when it has none; `GET /v1/window`
  gives the same id. See "Business-scoped user ids", rule 7. The pack's adapter sends to that
  window's user id for the conversation, never to a number (gp-g02t). It can meet a 409 only when
  its window read came from PR 73's gateway (no user ids: it names the conversation id) and the
  send reached this one, in the minutes of the deploy; it refuses it and does not resend.
- **Meta is answered after the commit.** The gateway answers Meta 200 only after every D1 write
  for the payload has committed. A failed write answers 500 with nothing acknowledged, and Meta
  redelivers.
- **Statuses of the city's own sends.** A status reaches a city only for a wamid one of its sends
  recorded. A status that beats the send's own record is held apart, and the send adopts it into
  its city when it records its wamid.

## Beyond the contract

These rules add to the contract without changing a route or a field. Each has its own test.

- **Stale messages.** A message Meta stamped more than 8 days ago is dropped and counted
  (`stale_messages`). That is longer than any dedup row lives, so it could otherwise be a second
  delivery of something already acknowledged. Meta's own retries stop at 7 days (Meta's webhooks
  page, cited at "Business-scoped user ids", rule 5, "The horizon").
- **Invite-code attempts.** A stranger may text 6-digit codes 5 times a day. The sixth is not
  bound even by a live code, and a Meta redelivery of any attempt is not a second attempt, however
  the redeliveries interleave: A, B, A, B, A is two attempts (gp-0g2s). The gateway keeps each
  counted text's wamid for 8 days, as long as Meta may redeliver it. Codes
  are checked against the time Meta stamped the text, so a code texted while live still binds on
  a redelivery after its expiry. The residual risk is many WhatsApp accounts guessing in parallel:
  each live code is one in a million per guess, and a code lives 24 hours.
- **Sends Meta never confirmed.** See `outcome_unknown` above. Only Meta's explicit refusal frees
  a key for a retry.
- **Batches.** A payload is written in D1 batches of at most 400 statements, cut only between
  messages. One message's bind, code, joined row and delivery always commit together. On a later
  batch's failure the earlier batches stand (their welcomes and canned lines still go), and Meta's
  redelivery writes only the rest.
- **Body limits.** Request bodies are read as a stream and abandoned at the cap: 1.9 MB for
  Meta's webhook, 256 KB for a city's call. That holds whether or not the request declared a
  length.
- **Platform logs.** The Worker's per-request invocation logs are off (`wrangler.toml`): they
  would record the URL, and Meta's handshake carries the verify token in its query string.

## Develop and test

Use the city's pnpm by its absolute path (it pins Node 24). Run these from `whatsapp/gateway/`:

```sh
PNPM=/Users/tailor512/city/.gc/shims/toolchain/pnpm
$PNPM install
$PNPM test            # vitest run: workerd + D1 + a fake Graph API; nothing reaches the network
$PNPM typecheck       # tsc --noEmit
$PNPM exec wrangler deploy --dry-run --outdir dist   # builds the bundle; needs no login
```

No dependency runs an install script (`pnpm-workspace.yaml`, `allowBuilds`). esbuild and workerd
ship their binaries as optional dependencies.

## Deploy (the mayor's hand)

Everything below happens in the **Houmanoids** Cloudflare account, which holds `dynasty.computer`.
Never Robert's account or his domain.

**Before step 1: the boxes' adapter** (gp-g02t). Every box that runs the whatsapp pack must run
its adapter at this change's pack pin, or later, before step 3. That adapter keys every owner by
`conversation_id` ("Behaviours the box adapter relies on"). It still reads PR 73's inbox, whose
deliveries have no `conversation_id` field (it keys those by `wa_id`), so the boxes can take it
while PR 73's gateway serves. An older adapter refuses this gateway's deliveries that show no
number and splits or mixes conversations by the number. Check each box's pack pin names a commit
that holds `conversationOf` in `whatsapp/adapter/src/render.js`. The order is the boxes' adapter,
then the migrations (step 3), then the deploy (step 4).

1. **Log in and check the account.**

   ```sh
   cd whatsapp/gateway && $PNPM install
   $PNPM exec wrangler login
   $PNPM exec wrangler whoami
   ```

   `whoami` must name the Houmanoids account.
2. **Create the D1.**

   ```sh
   $PNPM exec wrangler d1 create whatsapp-gateway
   ```

   Copy the `database_id` it prints into the `[[d1_databases]]` block of `wrangler.toml`, in the
   deploy checkout only (`database_id = "<id>"` under `migrations_dir`). Never commit it.
3. **Apply the schema.**

   ```sh
   $PNPM exec wrangler d1 migrations apply whatsapp-gateway --remote
   ```

   On a gateway that is already deployed, the same command applies only the migrations it has not
   yet seen. 0002 (business-scoped user ids) keeps every bound owner, and that owner's wa_id
   becomes its conversation id. Run the deploy (step 4) right after it. Between the two commands,
   the Worker still running behaves like this:
   - **It serves** bound owners' messages, sends, statuses and the window.
   - **It answers 500 to** a payload that holds a stranger's text or an invite text, because that
     code knows neither the new key nor `sender_key`. The owners' messages in the same payload
     wait with it, and Meta redelivers the whole payload once the new code is live.
   - **It leaves two traces, both handled:**
     - Its rows carry no `conversation_id`, and the new inbox reads their `wa_id` as one.
     - Its acks leave the backfilled `conversation_id` on a body-less row, and the daily cron
       clears it.

   (`assets/pr-evidence/gp-ac0u/deploy-gap.txt` runs the old code on the new schema.)

   0003 (Meta's retries, gp-0g2s; re-cut in place by gp-lv49) rebuilds the deliveries dedup key
   as `(wamid, status)`. It adds `phones.bound_stamp` and four tables (`links`, `unbinds`, `marks`,
   `code_attempt_wamids`) that the old code never reads. It adds nothing to the gap above:
   `assets/pr-evidence/gp-lv49/deploy-gap.txt` runs the deployed code (PR 73) on 0001, 0002 and
   0003 as re-cut. The rebuild fails only where two cities already hold one wamid. Only the
   misroute 0003 fixes could have written such a pair, and the deployed D1 holds no deliveries.
   0003 was edited in place because no remote D1 has applied it: main had only 0001. Confirm that
   before step 3 with `$PNPM exec wrangler d1 migrations list whatsapp-gateway --remote`, which
   must list 0002 and 0003 as not yet applied. **If it lists 0003 as applied, stop.** That D1 ran
   the parked text of 0003: wrangler skips this 0003 by its name, so `links`, `unbinds` and
   `marks` never exist and this code answers 500 to every message payload, and the new text cannot
   be run by hand either (its first statement fails: the index `deliveries_wamid_once` already
   exists). That D1 needs a new migration, written for it, before this deploy.

   0003 also carries each stranger's last counted code text (`unknown_senders.last_code_wamid`)
   into `code_attempt_wamids` (gp-w376). A code text the old code counted, and Meta redelivers
   after the deploy, is then no new attempt. No attempt is counted in the gap, because the old
   code's stranger path answers 500 (above). The order is always migrations, then the deploy, in
   one sitting.

4. **Deploy.**

   ```sh
   $PNPM exec wrangler deploy
   ```

   The `routes` entry (`custom_domain = true`) creates `whatsapp.dynasty.computer` with its
   certificate. Until the secrets are set, the gateway answers 503 `not_configured` to Meta and
   401 on every admin route. That is safe to leave live.
5. **Set the five secrets**, one prompt each, values from Afik's Meta setup below:

   ```sh
   $PNPM exec wrangler secret put META_ACCESS_TOKEN
   $PNPM exec wrangler secret put META_APP_SECRET
   $PNPM exec wrangler secret put META_VERIFY_TOKEN
   $PNPM exec wrangler secret put META_PHONE_NUMBER_ID
   $PNPM exec wrangler secret put ADMIN_TOKEN
   ```

   `META_VERIFY_TOKEN` and `ADMIN_TOKEN` are ours to generate (`openssl rand -base64 32`). The
   verify token goes to Afik for the webhook form. The admin token stays with the mayor.
6. **Check.**

   ```sh
   curl -s https://whatsapp.dynasty.computer/healthz
   ```

   It must answer `{"ok":true,"pending_total":0,"last_webhook_at":null}`.
7. **Re-invite the owners PR 73 bound by number alone** (the design note's question 1, answer
   (a); gp-lv49). Such an owner holds no user id, so from this deploy on nothing reaches it and
   nothing can be sent to it. Its own messages, which carry its user id, are a stranger's: they
   hear the canned line. List them:

   ```sh
   $PNPM exec wrangler d1 execute whatsapp-gateway --remote \
     --command "SELECT conversation_id, city_id FROM phones WHERE user_id IS NULL"
   ```

   Unbind each by its `conversation_id` with `POST /v1/admin/unbind` (Operator runbook), then ask
   its city for a new invite code (`gc whatsapp invite`) and give it to the owner. The owner's
   code text binds it again, by its user id, and the new binding is keyed by that user id (rule
   1): the box files it in a new conversation (`whatsapp/<user id>`), and the old one, keyed by
   the number, keeps its history. The Fable reads of 11/5 found none; recount at the deploy.

The test-number to production-number switch is one `wrangler secret put META_PHONE_NUMBER_ID`.
Every box's gc account id is the stable alias `dynasty`, so no conversation identity changes.

## Meta setup (Afik's hand)

1. In the Houmanoids business portfolio, create a Meta app with the **WhatsApp** product.
2. Add the business number. Meta's free test number reaches five phones and is enough for the
   first week. Set the display name **Dynasty** and a plain photo.
3. Create a **system user** with a **permanent token** carrying `whatsapp_business_messaging`,
   `whatsapp_business_management` and `business_management`. This is `META_ACCESS_TOKEN`.
4. Copy the **app secret** (App settings, Basic). This is `META_APP_SECRET`.
5. Copy the **phone number id** from WhatsApp, API Setup. This is `META_PHONE_NUMBER_ID`. It is
   not the phone number.
6. Under WhatsApp, Configuration, set the webhook:
   - Callback URL: `https://whatsapp.dynasty.computer/meta/webhook`.
   - Verify token: the `META_VERIFY_TOKEN` value the mayor generated.

   Meta then calls the handshake, which must succeed. Subscribe to the **messages** field.
7. Submit the one template (next section).

## The one template

| Field | Value |
|---|---|
| Category | **utility** |
| Name | `dynasty_update` |
| Language | `en_US` |
| Body | `Your city has an update: {{1}}` |
| Language | `pt_BR` |
| Body | `Sua cidade tem uma novidade: {{1}}` |
| Sample for `{{1}}` | `The shipment cleared customs this morning.` |

A city sends it with:

```json
{
  "type": "template",
  "template": {
    "name": "dynasty_update",
    "language": "en_US",
    "params": ["..."]
  }
}
```

It is the only send the gateway allows outside the window, and it never opens or extends the
window. Meta reviews templates within 24 hours and can re-categorize one. If Meta reads
`dynasty_update` as marketing, its price changes; the route does not.

## Operator runbook

Admin calls carry `Authorization: Bearer $ADMIN_TOKEN`.

- **Mint an enrollment code** for a city (one per city, valid 72 hours):

  ```sh
  curl -s -X POST https://whatsapp.dynasty.computer/v1/admin/enroll-codes \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'content-type: application/json' \
    -d '{"city_name":"Hou Dynasty"}'
  ```

  It answers `{"code":"XXXX-XXXX-XXXX-XXXX","expires_at":...}`. The box runs
  `gc whatsapp enroll <code>`. `city_name` is the name owners see in the welcome line.
- **Read the cities.**

  ```sh
  curl -s https://whatsapp.dynasty.computer/v1/admin/cities -H "Authorization: Bearer $ADMIN_TOKEN"
  ```

  Each city carries its owner count (`phones`), the last four digits of the number each owner
  shows ("Business-scoped user ids", rule 3; `id` for an owner that shows none), its held
  deliveries and its live credentials. The gateway's counters follow: dropped_wrong_number,
  orphaned_statuses, stale_messages, welcome_failed, canned_failed, unrouted_messages (a message
  that reached no city and is no stranger's: one naming no user id, a known id's message no span
  held, a notice no binding answers to) and notices_without_previous_id (a number change Meta sent
  without the old id: that owner needs a new invite). The webhook log line counts unrouted
  messages per payload (`unrouted`). Both count deliveries: a message Meta delivers twice counts
  twice, and a payload that answered 500 writes no log line. No phone number and no user_id
  appears in clear.
- **Revoke a city's credential** (the box gets 401 on its next call):

  ```sh
  $PNPM exec wrangler d1 execute whatsapp-gateway --remote \
    --command "UPDATE credentials SET revoked_at = unixepoch() WHERE city_id = '<city id>'"
  ```

- **Re-key a rebuilt box.** Mint an enrollment code and let the box enroll, which mints a new
  city. Then move everything the old city owns to it in one `wrangler d1 execute --remote
  --command`. That includes its held deliveries, which are what a rebuilt box needs most:

  ```sql
  UPDATE phones SET city_id = '<new>' WHERE city_id = '<old>';
  UPDATE deliveries SET city_id = '<new>' WHERE city_id = '<old>';
  UPDATE media SET city_id = '<new>' WHERE city_id = '<old>';
  UPDATE sends SET city_id = '<new>' WHERE city_id = '<old>';
  DELETE FROM invite_codes WHERE city_id = '<old>' AND used_at IS NULL;
  UPDATE credentials SET revoked_at = unixepoch() WHERE city_id = '<old>' AND revoked_at IS NULL
  ```

  The new city is fresh, so no row collides. The box re-reads every held delivery from
  `after=0`.
- **Unbind an owner** (so it can join another city by a new invite code), by any id it is known
  by: its `conversation_id`, its current `user_id`, or the number a PR 73 binding holds:

  ```sh
  curl -s -X POST https://whatsapp.dynasty.computer/v1/admin/unbind \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H 'content-type: application/json' \
    -d '{"id":"<id>"}'
  ```

  It answers `{"unbound":<n>}`, 0 when no owner has that id. Never unbind with a raw
  `DELETE FROM phones`. The route deletes the binding and, in the same write, records for 8 days
  every user id the owner answered to: its current one, and each one a number change linked to it
  (`unbinds`). That record is what keeps the owner's earlier messages out of its next city (the
  design note's T8). It covers every id a number change links to a recorded one, in either
  direction, whenever that notice lands, after the unbind included.

  No unbinding is needed for a recycled number. When Meta names that phone beside a different
  user_id, the new person is a stranger who joins by an invite code under their user_id, and the
  old owner's city still reaches the old owner at its user id.

  After an unbind, nothing the owner sent before it reaches any city, the next one included. A
  message the first city stored stays that city's row. One it never stored goes nowhere: Meta
  stamped it at or before the unbind, under an id the unbind recorded, so it reaches no city and
  binds nothing, even when its text is another city's live invite code. It is counted as
  `unrouted_messages`. What the owner writes after the unbind is a stranger's until it joins a city
  by a new code, which it can do at once. The cron deletes the record after 8 days, when no such
  message can arrive. The record compares Meta's stamp of a message with the gateway's clock at
  the unbind, so an owner's code text sent in the same second as its unbind binds nothing; the
  owner sends it again.

## Data, retention, logs

- **Bodies.** A delivery's body is deleted on the city's ack, or by the daily cron 7 days after
  receipt, and its wa_id, user_id, username, conversation_id and profile name go with it. The
  body-less row keeps only (city, wamid, status) for one more day, to dedup a late Meta
  redelivery. The daily cron also clears any id left on a body-less row.
- **Sends** keep the owner's `conversation_id` with their idempotency key for 30 days (statuses
  reach the city under it), even after the owner is unbound.
- **Media.** Media ids are kept 7 days (Meta's lifetime for a webhook media id). No media bytes
  are ever stored: `GET /v1/media/<id>` streams Meta's bytes through.
- **Strangers.** An unknown sender's row (last canned reply, code attempts) is pruned after 7 days.
  The wamid of each counted code text is kept for 8 days, with no id beside it.
- **Number changes and unbinds.** A number change's link (the old user id, the new one, Meta's
  stamp) is kept 8 days from that stamp (`links`). An unbind's record (each user id the owner
  answered to, and the time) is kept 8 days from the unbind (`unbinds`). The cron deletes both at
  the end of that time. Before a link goes, the cron writes the unbind floor of its ids (the last
  unbind of any id linked to them) on both, so a late notice's reach outlives its link for as
  long as the unbind's own record lives. An owner row keeps Meta's stamp of its invite text (`bound_stamp`) until an
  operator unbinds it.
- **Marks.** Each phone number's newest pairing with a user id, and Meta's stamp of it (`marks`).
  A mark naming an owner's current user id is the number that owner shows, kept while it does. Any
  other mark (a stranger's number, an unbound owner's, one a notice cleared) is deleted 8 days
  after its stamp. A stranger's number beside its user id is kept nowhere else.
- **Orphan statuses** (a status no send has recorded) are pruned after one day. The gateway's own
  welcome and canned lines leave theirs there.
- **Codes and keys.** Spent and expired invite codes are pruned 7 days after expiry, enrollment
  codes 30 days after, and send idempotency keys after 30 days.
- **Owners** (the phones table: the `conversation_id`, which is the owner's user id at the bind or,
  for a PR 73 binding, its number; the user id; the last username and profile name Meta showed;
  and for a PR 73 binding its number in `wa_id`) are kept until an operator unbinds them. A
  username the owner later drops stays until then.
- **Logs** carry fixed event names and counts only. No log line ever carries a body, a phone
  number, a user_id, a username, a profile name, a code, a token, a secret or an error's message.
  Three tests check every line against those values:
  - "no log line carries a phone number, a message body, a token or a secret" drives the
    gateway's logging paths: a bind, a message, a stranger, sends refused by Meta (with a phone in
    Meta's error text), by `not_bound` and by `window_closed`, an `outcome_unknown` send, a bad
    signature and a failed D1 write.
  - "the Worker entry's own console lines" drives `src/index.ts` itself: a webhook, a refused
    handshake and the cron.
  - "no log line carries a user_id, a username or a phone on the BSUID paths"
    (`test/bsuid.test.ts`) covers the user_id paths. It drives a bind and a message by user_id with
    a username, a stranger by user_id, sends to a user_id (Meta's 131062 refusal quoting the id, an
    `outcome_unknown`, `not_bound`, a malformed id and `window_closed`), a status and a number
    change.
