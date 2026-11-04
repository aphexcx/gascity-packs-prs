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
| `migrations/0001_init.sql` | The schema: cities, credentials, enroll_codes, invite_codes, phones, deliveries, media, sends, unknown_senders, stats. |
| `src/index.ts` | The Worker entry: env to gateway, the cron to `maintain()`. |
| `src/gateway.ts` | The routes, the webhook plan and its one D1 transaction, enrollment, the inbox and ack, send, read, window, media, the operator routes, the cron. |
| `src/meta.ts` | The Cloud API wire: the signature, the payload parse, the send bodies, the Graph calls. |
| `src/window.ts` | The 24-hour window: its arithmetic and which inbound types move it. |
| `test/` | vitest on `@cloudflare/vitest-plugin` (a real D1 in workerd) with a fake Graph API. `fixtures/` holds Meta's own example payloads; `fixtures/SOURCES.md` says where each came from. |

## The contract (v1)

This section is the contract **verbatim** from the bead spec of record,
`/Users/tailor512/city/assets/ops/beads/whatsapp-gateway-20261103.md` (SCOPE item 3). It is frozen.
A change to it is a bead comment on BOTH the gateway bead (gp-nn90) and the adapter bead (gp-9hf1),
never a silent edit.

- Meta side: GET /meta/webhook answers hub.challenge only when hub.verify_token equals META_VERIFY_TOKEN (403 otherwise); POST /meta/webhook verifies X-Hub-Signature-256 over the raw body with META_APP_SECRET (401 and nothing stored on a mismatch or a missing header), accepts only payloads whose metadata.phone_number_id equals META_PHONE_NUMBER_ID (others counted and dropped), and for each messages[] entry: a sender in phones is appended to that city's deliveries (dedup by wamid), a sender not in phones whose text is a live invite code binds that phone to the code's city, marks the code used and appends a synthetic "owner joined" delivery to the city (carrying the invite text's wamid, so a redelivery is one row), the three writes one D1 batch committed before anything else, then sends the owner one text "You're connected to <city name>'s mayor. Say hi." (a failed welcome send is counted, never a 5xx to Meta, so a redelivery never re-binds or lands the code as a message); any other unknown sender gets ONE text "This number serves Dynasty cities. Ask your city for an invite code." at most once per 7 days per phone and is otherwise ignored; statuses[] for the city's sends are appended as kind status; then 200, answered only after every D1 append for the payload has committed: a failed write answers 500 with nothing acknowledged and Meta redelivers, and the wamid UNIQUE constraint makes the redelivery one row (the Worker keeps nothing between requests; the D1 row is Meta's acknowledgement). Over 5,000 pending for one city: 503 (Meta retries). Bodies over 1.9 MB: 413.
- City side, all under Authorization: Bearer <credential> (401 on unknown or revoked; the credential is compared by sha256): POST /v1/enroll {code, name} with NO bearer mints a city and ONE credential for a live enrollment code and returns {city_id, credential} once (the code is then used); POST /v1/invites returns {code, expires_at} (6 digits, 24 hours, one live code per city at a time); GET /v1/inbox?after=<id>&limit=<1..200> returns the city's unacknowledged deliveries with id greater than after, oldest first, as {deliveries:[{id, received_at, kind: message|status|joined, wa_id, profile_name, message}], cursor} with cursor the last id in the page (after omitted returns the oldest unacknowledged, so a restarted box re-reads everything it never acknowledged; within one process the box skips ids it already holds in flight); POST /v1/inbox/ack {ids:[...]} deletes exactly the listed deliveries' bodies and no other, never a cumulative position, because the box delivers on independent per-sender chains and one sender's later delivery can be confirmed before another sender's earlier one (the city acks an id only on terminal success, gc's delivered receipt on the first response or read back later, or on the box's dead-letter transition, a repost bound reached, no_route (the marked copy's included), a second doubt or bead B's 24-hour pending deadline, the envelope then on the box's disk, or on the operator's resolution of a terminal hold (gc's pending with terminal true, which bead B never replays and never acks on its own: that delivery stays held here, returned by every poll, until the resolution's ack or this retention); never on a pending receipt before that deadline, bead B's rule; until then the gateway holds the delivery and every poll returns it again; bead B's deadline runs from its first post and this retention from receipt, so on an ordinary day the body is still here when that ack arrives, and an ack for an id the cron already deleted is a no-op, never an error); POST /v1/send {to, idempotency_key, type: text|reaction|template, text?, reaction?{message_id, emoji}, template?{name, language, params[]}} sends through Meta's Graph API only when `to` is bound to THIS city (403 otherwise), answers {message_id} and the SAME answer again for a repeated idempotency_key without a second Meta call, refuses a text or reaction outside the phone's window with 422 {error: window_closed}, and never refuses a template on the window (a template is the only send allowed outside it and does not open or extend it); POST /v1/read {message_id} marks read with the typing indicator; GET /v1/window returns {windows:[{conversation_id, last_inbound_at, window_closes_at}]} for every phone bound to THIS city (conversation_id the wa_id; a group id when Groups land), window_closes_at = last_inbound_at + 24 hours - the 120-second margin, the same arithmetic window.ts applies to a send so the box and the gateway never disagree on whether a send will be refused, both null for a phone that has never written, no profile name and no body (the window-keeper of the design's section of that name reads it once a minute; only an inbound customer message moves it, never a send of any type); GET /v1/media/<media_id> streams the bytes with Meta's mime type for a media id that was delivered to THIS city (404 otherwise), minting the URL from Meta and fetching with the token; GET /healthz {ok, pending_total, last_webhook_at} with no body text and no numbers of phones.
- Operator side under Authorization: Bearer <ADMIN_TOKEN>: POST /v1/admin/enroll-codes {city_name} returns {code, expires_at} (the mayor's hand mints one per city); GET /v1/admin/cities lists cities with counts, never phone numbers in clear (last four digits only).

### Readings of what the contract leaves unstated

These readings were posted as a bead comment on gp-nn90 and gp-9hf1 on 2026-11-04. None of them
changes a route, a field or a rule.

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
   | 403 | `not_bound` | `to` is not bound to this city. |
   | 422 | `window_closed` | A text or a reaction outside the window: the gateway's own check, or Meta's 131047. |
   | 409 | `in_flight` | Another request with the same `idempotency_key` is still at Meta. Retry it; a claim older than 60 s is taken over. |
   | 502 | `meta_error` (with `meta_status`, `meta_code`) | Meta refused, or never answered. Nothing was recorded, so a retry under the same key sends. |

   The idempotency key is per city. A repeat answers the stored `{message_id}` with no Meta call,
   and this is checked **before** the window, so a retry after the close still gets the first
   answer.
5. **Read.** `POST /v1/read {message_id}` answers `200 {ok: true}`. It answers `404 not_found`
   when that wamid was never delivered to this city, and `502 meta_error` when Meta refuses it.
6. **Media.** `GET /v1/media/<id>` answers `404 not_found` for a media id not delivered to this
   city, or older than Meta's 7-day media lifetime.
7. **Window.** `window_closes_at = last_inbound_at + 86400 - 120`, and a free-form send is
   allowed while `now < window_closes_at`. The inbound types that move the window are text, image,
   audio, video, document, sticker, location, contacts, interactive, button and order. A reaction,
   a system notice and an unsupported type are delivered but do not move it.
8. **Errors.** `401 {error: unauthorized}` answers a missing, unknown or revoked credential (or a
   wrong admin token). `404 {error: not_found}` answers an unknown route, and
   `405 {error: method_not_allowed}` a known route called with the wrong method.

### The window

The clock is each phone's `last_inbound_at`, and only an owner's message moves it. A send of any
type never moves it, a template included. The 120-second margin means the last second a
free-form send is allowed is **23 h 57 m 59 s** after the owner's message.

The bead's test line "a text at t0 plus 23h59 sends" cannot hold beside the frozen margin. The
tests therefore send at the close minus one second, and refuse at the close and at 24 h 01 m.

Meta's pages name only "messages or calls" as what opens the window. Whether an owner's
**reaction** opens it is not stated anywhere I could find, so the gateway reads it
conservatively: a reaction is delivered to the city but does not move the clock. The cost of
this reading is a refusal the mayor answers with a template. The opposite reading risks a
free-form send that Meta refuses with 131047. **This is a judgment call for the founders.** One
line in `src/window.ts` flips it.

## Behaviours the box adapter relies on

- **The hold.** A delivery is held, body and all, until `POST /v1/inbox/ack` names its id, and
  every poll returns it again. The hold has two bounds: the 7-day cron, and the 5,000-held cap per
  city (beyond which Meta gets 503 and retries).
- **The no-op acks.** An ack of an id the cron already deleted, or an id an earlier ack already
  acknowledged, is a 200 that changes nothing.
- **One row per wamid.** Meta's redeliveries, the invite text and its redelivery, and an
  acknowledged wamid redelivered within 8 days each leave exactly one row. A status is one row per
  (wamid, status).
- **Meta is answered after the commit.** The gateway answers Meta 200 only after every D1 write
  for the payload has committed. A failed write answers 500 with nothing acknowledged, and Meta
  redelivers.

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

  Each city carries its phone count, the last four digits of each phone, its held deliveries and
  its live credentials, plus the gateway's counters: dropped_wrong_number, unrouted_statuses,
  stale_messages, welcome_failed and canned_failed. No phone number appears in clear.
- **Revoke a city's credential** (the box gets 401 on its next call):

  ```sh
  $PNPM exec wrangler d1 execute whatsapp-gateway --remote \
    --command "UPDATE credentials SET revoked_at = unixepoch() WHERE city_id = '<city id>'"
  ```

- **Re-key a rebuilt box.** Mint an enrollment code and let the box enroll, which mints a new
  city. Then move the old city's phones to it:

  ```sh
  UPDATE phones SET city_id = '<new>' WHERE city_id = '<old>'
  ```

  Then revoke the old credential as above.
- **Unbind a phone** (so it can join another city by a new invite code):

  ```sh
  DELETE FROM phones WHERE wa_id = '<digits>'
  ```

## Data, retention, logs

- **Bodies.** A delivery's body is deleted on the city's ack, or by the daily cron 7 days after
  receipt. The body-less row keeps only (city, wamid, status) for one more day, to dedup a late
  Meta redelivery.
- **Media.** Media ids are kept 7 days (Meta's lifetime for a webhook media id). No media bytes
  are ever stored: `GET /v1/media/<id>` streams Meta's bytes through.
- **Strangers.** An unknown sender's row (last canned reply, code attempts) is pruned after 7 days.
- **Codes and keys.** Spent and expired invite codes are pruned 7 days after expiry, enrollment
  codes 30 days after, and send idempotency keys after 30 days.
- **Phones** are kept until an operator unbinds them.
- **Logs** carry fixed event names and counts only. No log line ever carries a body, a phone
  number, a profile name, a code, a token, a secret or an error's message. The test "no log line
  carries a phone number, a message body, a token or a secret" drives each logging path (a bind,
  a message, a stranger, a refused and an accepted send, a bad signature, a failed D1 write, the
  cron) and checks every line against those values.
- **Invite-code brute force.** A stranger who texts a sixth 6-digit code in one day is not bound
  even by a live code.
- **Statuses** reach a city only for the city's own sends. The welcome and the canned line are
  the gateway's, and their statuses are counted as `unrouted_statuses`.
