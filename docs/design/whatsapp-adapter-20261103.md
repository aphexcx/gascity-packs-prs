# WhatsApp adapter and gateway: the design of record

This is the technical design of the `whatsapp` pack and its gateway, stated as the code at this
commit builds it. It replaces a longer planning document first written on 2026-11-03. How the gateway
decides which owner a message is from has its own note,
[`whatsapp/docs/design-bsuid-pairing-and-spans.md`](../../whatsapp/docs/design-bsuid-pairing-and-spans.md).
Every env name, verb and doctor row is listed in [`whatsapp/README.md`](../../whatsapp/README.md),
and every gateway route and field in [`whatsapp/gateway/README.md`](../../whatsapp/gateway/README.md).

## What it is

WhatsApp is one more door into a city's mayor, beside Slack, Google Chat and WeCom. One business
number on Meta's WhatsApp Cloud API serves every city that imports the pack. A city's owners, and the
people they invite, message that number from their own phones; each reaches their own city's mayor,
and the mayor's replies come back in the same chat. It is an owner-to-mayor channel, not a sales,
marketing or support channel, and it is not open to the public: only a phone a city has invited
reaches a city. Chats are one-to-one; group chats are not built.

Two programs carry it:

| Part | Where it runs | Its job |
| --- | --- | --- |
| The gateway, `whatsapp/gateway/` | One Cloudflare Worker with a D1 database at `https://whatsapp.dynasty.computer` | Holds the number's Meta secrets, receives Meta's webhook, routes each message to its owner's city, holds each delivery until that city acknowledges it (for up to seven days), and makes every Graph API call for every city |
| The adapter, `whatsapp/adapter/` | Every city box, as the pack's `proxy_process` service `whatsapp` (Node 20 or newer, no package dependencies) | Polls the gateway, delivers each message to the mayor through gc's external-messaging API under a write-ahead journal, publishes the mayor's replies, and runs the window-keeper |

Around them sit `whatsapp/commands/` (the `gc whatsapp` verbs), `whatsapp/doctor/` (nine doctor
rows), the mayor's rules for the channel in `whatsapp/skills/whatsapp/SKILL.md` and
`whatsapp/template-fragments/`, and `whatsapp/city-fragment.toml` (the default route to the mayor).

Meta delivers inbound messages only by webhook to a public HTTPS address, and the access token must
live in one place. So a city box has no public listener, holds no Meta secret, and talks only to the
gateway, over HTTPS, with the one credential it was given at enrollment.

## Identity

- **The account.** gc sees one account id everywhere, the stable alias `dynasty`: the adapter's
  registration, every inbound envelope, every keeper event and every outbound transcript reference.
  Meta's phone number id is gateway configuration only, so replacing the number re-keys nothing in
  any city.
- **The conversation.** Each owner is one gc conversation (provider `whatsapp`, account `dynasty`,
  kind `dm`) whose id is the gateway's `conversation_id` for that owner: the owner's WhatsApp
  business-scoped user id, fixed when the owner binds. It survives a change of number and is never
  shared by two owners, a recycled number included. The number (`wa_id`) is shown, never used as a
  key. A binding made before user ids existed holds a number and no user id, so no message routes
  to it and no send reaches it (`not_bound`); inviting that owner again makes a new binding keyed by
  the user id, and an operator removes the old one.
- **Routing.** The gateway routes a message by the sender's user id, never by the number, and sends
  every reply to a user id. A message the gateway cannot place, one naming no user id included,
  reaches no city; it is counted, never held and never guessed.

## The path

### In

1. Meta POSTs its webhook. The gateway verifies `X-Hub-Signature-256` over the raw body with the
   app secret (a mismatch or a missing header is a 401, and nothing is stored) and accepts only
   payloads for its own phone number id.
2. A message from a bound owner is appended to that owner's city's deliveries, deduplicated by Meta's
   message id (the wamid). A text from an unknown sender that is a live invite code binds the sender
   (see "Enrollment and the invite binding"). Any other unknown sender gets one canned line, at most
   once per seven days, and is otherwise ignored. Statuses Meta reports for a city's own sends are
   appended to that city's deliveries too.
3. Meta gets its 200 only after every D1 write for the payload has committed; anything lost before
   the commit is redelivered by Meta. A body is held for up to seven days without an acknowledgement,
   and a city with 5,000 unacknowledged deliveries makes the gateway answer 503, so Meta retries later.
4. The adapter polls `GET /v1/inbox` (every `WHATSAPP_POLL_MS`, 2 seconds by default). The inbox
   answers with the city's unacknowledged deliveries, oldest first, and every poll reads them from the
   start, so a restarted box reads again everything it never acknowledged.
5. The adapter writes the delivery to its journal, then posts it to gc's `POST /extmsg/inbound` with
   `provider_message_id` (the wamid), `dedup_key` (the request id), the actor (the owner's profile
   name when there is one, `is_bot: false`), `received_at` (Meta's time for the message) and any
   attachments. gc gives it to the mayor by the city's default route. On gc's first acceptance of a
   message the adapter marks it read in WhatsApp (`POST /v1/read`, best effort).
6. When gc says the message was delivered, the adapter acknowledges it with `POST /v1/inbox/ack`
   and the delivery ids. The gateway deletes exactly those bodies. An acknowledgement names ids,
   never a position, because each owner's messages travel on their own chain and one owner's later
   message can land before another owner's earlier one.

A status of the city's own send is acknowledged without reaching gc unless it failed
(`[delivery failed: <code> <title>]`); a reaction reaches the mayor as `[reaction <emoji> to <id>]`.

Media and voice notes come in through the gateway, which serves the bytes at `GET /v1/media/{id}`,
because Meta serves media only to the access token. The adapter checks each file against the
webhook's sha256, stores it under the city's media directory within size, quota and free-space
limits, and attaches it to the gc message as a `file://` attachment with a line naming the saved
path. Audio, voice notes included, is transcribed on the box with the city's own optional
speech-to-text key; without one, or when transcription fails, the file is saved and marked not
transcribed.

### Out

1. The mayor publishes through gc, which calls the adapter's `/publish` with the publishing session
   named; `gc whatsapp publish` is the verb.
2. The adapter reads `GET /v1/window` once for the send and addresses it to the user id of the
   window entry whose `conversation_id` is the conversation. A conversation the window does not list,
   or lists with no user id, is refused as `not_bound`, and nothing is sent.
3. Text is cut into chunks of at most 4,096 characters, and each conversation's sends go one at a
   time. Each chunk goes under a gateway idempotency key derived from the publish key, the recipient
   and the whole text, so a retry resumes at the chunk that failed and a key reused for another
   message or another owner is a different key.
4. The gateway sends through the Graph API. Outside the 24-hour window it refuses every send but a
   template with `window_closed`, which the adapter returns to gc as a permanent publish error that
   names the update verb. A send whose outcome Meta left unknown stays spent under its key; it is
   never sent again under that key.
5. A file goes out the same way, one image, document, audio file or video per publish and only inside
   the window. The adapter checks its size and type against Meta's limits and its path against the
   allowed roots and a list of denied paths, uploads it with `POST /v1/uploads`, then sends it; the
   upload and the send resume under one key.

## The journal

The adapter's memory of what it owes gc and the gateway is one file,
`<city>/.gc/whatsapp/inbound_journal.jsonl` (0600, in a 0700 directory). Each state change is one
JSON line, fsynced before the next step, and the adapter rebuilds its state at start by folding the
lines per request id. Nothing in process memory is the record.

Every delivery gets the request id `whatsapp:dynasty:<wamid>` (a status delivery adds
`:status:<status>`, so it never collides with the send it reports on). Its lines run:

`received` (the full envelope) → `posted` (with a lookup boundary) → the gc POST → `accepted` (the
receipt id, the verdict, the terminal flag, the transcript sequence) → the acknowledgement →
`acknowledged`.

The rules for each answer:

- **Delivered.** Acknowledge the delivery to the gateway. The adapter acknowledges only what gc
  delivered (by its verdict, the probe below, or an operator's resolve) or what it dead-lettered.
- **Pending.** Read the receipt back (`GET /extmsg/inbound/receipts/{id}`) every 5 seconds for 10
  minutes, every minute to the hour, then every 5 minutes. There is no cutoff short of a deadline 24
  hours after the first post, after which the entry is dead-lettered (`pending_deadline`) and
  acknowledged.
- **Pending and terminal.** gc has stopped trying on its own. The entry is a terminal hold: never
  posted again, never read back, shown on the doctor's `held` row, and released only by an operator
  with `gc whatsapp inbound resolve <rid> --delivered|--dead`.
- **Failed or partial.** Nothing is in doubt, since gc said what happened: repost under the same
  request id (as a marked copy if the entry has already gone out as one), at most three times, then
  dead-letter (`repost_bound`).
- **No route.** Dead-letter at once (`no_route`).
- **A refused connection, or an HTTP 5xx, 404, 408 or 429.** gc took nothing: post again on the
  ladder of 1, 5, 15 and then every 60 seconds. Any other 4xx dead-letters the entry (`rejected`).
- **No usable answer** (the 30-second client deadline, a reset, a malformed answer, a success with no
  receipt). Never posted again on the ladder: after the deadline plus ten seconds the entry takes the
  in-doubt rule.

**The in-doubt rule.** A post whose outcome was lost is settled from evidence, in this order:

1. An entry already in doubt after its one marked copy is dead-lettered (`in_doubt`), never posted a
   third time.
2. The all-watermark read (below).
3. A search of gc's conversation transcript for the request's `provider_message_id`, from the
   boundary the first `posted` line recorded (the highest sequence the journal held for the
   conversation before the post), then a second pass from zero, since a rebuilt store numbers from
   one again.
4. A probe of the mayor's session transcript. Only a user turn in the provider's own session log that
   holds the paste's header and its message id proves the paste landed; then the entry is delivered
   and acknowledged. A terminal snapshot holds drafts and is never proof.
5. Otherwise, if the transcript holds the message, or the journal ever saw gc accept it, or a restore
   gap stands anywhere, or the entry was first posted before an operator last cleared a gap, the
   adapter sends one marked copy: `[redelivery <wamid>: a first copy may have reached you; if you
   already answered it, do nothing]`.
6. Only when none of those holds does it read every watermark again and, if no gap has appeared,
   post the message once, plain.

A `no_route` answer to the marked copy is probed once more, then dead-lettered
(`redelivery_no_route`), since gc answers a suppressed duplicate and an empty route with the same
bytes. So a crash loses nothing the gateway still holds, and the only copy the adapter marks is the
one of step 5, with that copy's own reposts: a repost after a `failed` or `partial` verdict, and a
repost on the ladder after a refused connection or an HTTP 5xx, 404, 408 or 429, otherwise go out
unmarked with the first post's `provider_message_id` and request id, and whether the mayor's session
then shows the text once is gc's rule for a repeated `provider_message_id`, which lives in gc,
outside this repository.

**The restore rule.** The journal keeps each conversation's watermark, the highest accepted
transcript sequence and its message id, through every compaction. The journal's record that gc took
a message outranks a gc store that no longer shows it: a watermark whose sequence the store no longer
holds with that message is a restore gap, written to the journal and shown on the doctor's `restore`
row, and while any gap stands every in-doubt entry of every conversation takes the marked path, never
a plain post. `gc whatsapp inbound clear-gap <conversation_id>` clears a gap and rebases its
watermark to the store.

**Dead letters.** A dead letter writes the entry, envelope included, to
`<city>/.gc/whatsapp/inbound_dead_letter/<conversation_id>.jsonl` first, then the journal's `dead`
line, then owes the acknowledgement. A kill between the file line and the journal line is finished on
restart from the file line, with no second file line.

**Compaction** runs at start and every ten minutes (`WHATSAPP_COMPACT_MS`). It rewrites the journal
keeping each conversation's current watermark, every standing gap and the last gap clear, the probe
lines of the last seven days, and every entry that has no `acknowledged` line. An acknowledged entry
is dropped only once a poll that began after its acknowledgement has finished.

## The 24-hour window

Meta states the rule this way: "When a WhatsApp user messages you or calls you, a 24-hour timer called
a customer service window starts", and outside that window a business may send only template
messages ([Meta, WhatsApp pricing](https://developers.facebook.com/docs/whatsapp/pricing)).

The gateway enforces it per owner. Its clock is Meta's own timestamp of the owner's last inbound
customer message, never moved backward and never ahead of the gateway's clock; a bind starts it at the
code text's time. Only an inbound message moves it (a reaction counts; a system notice or a type Meta
marks unsupported does not), and no send moves it, a template included. A free-form send is refused
from 120 seconds before the 24 hours end, so a send that leaves at the edge cannot reach Meta after
the close. `GET /v1/window` answers with the same arithmetic the send check uses, so the box and the
gateway never disagree about whether a send will be refused. If Meta itself refuses a send as outside
the window (its error 131047), the gateway answers `window_closed` too.

Outside the window the mayor may write first only with the one approved utility template,
`dynasty_update` ("Your city has an update: {{1}}"), sent by
`gc whatsapp publish --update --to <conversation_id> --text "..."`. The text is the template's one
parameter, at most 1,024 characters. The adapter refuses the update when the text is empty and when
its own record of the window says the window is open, where a free reply is the right send; the
gateway never refuses a template on the window. Every update is recorded with its text in
`<city>/.gc/whatsapp/updates.jsonl`. A template never opens or extends the window; only the owner's
next message does. Whether a text is an actual update is the mayor's skill rule, not code.

## The window-keeper

The keeper lives in the adapter; the gateway only serves `GET /v1/window`. The adapter reads the
window once a minute and after each new delivery. For an owner whose business consented
(`gc whatsapp invite --proactive-consent`, or `gc whatsapp consent <conversation_id> --on`), it
raises keeper events into the mayor's session as the actor "Dynasty window-keeper"
(`is_bot: true`): a daily update prompt once per business day (two hours before the close of
business, or, with no hours set, at `WHATSAPP_KEEPER_UPDATE_HOUR` in the owner's recorded zone, else
`WHATSAPP_CITY_TZ`), and a close ladder at 60, 30, 15 and 5 minutes before the window closes, kept
inside the business's hours (`gc whatsapp hours`). With no hours set the marks run before the window
closes, at any hour; with hours set a mark that falls outside every span is dropped, and the marks
run before the window closes when the close falls inside a span or a span reaches into the window's
last 60 minutes, else before the close of business, the end of the last span that ends before the window
closes (a close at 02:00 under Mon-Fri 09:00-18:00 fires at 17:00, 17:30, 17:45 and 17:55 the day
before), and not at all when no span is left before the close.
No nudge fires after the close; the daily update
still does, and says since when the window has been closed. Its state lives in
`<city>/.gc/whatsapp/keeper.json`, written before each event, and an owner the last good window read
did not list raises nothing. It is off for every owner until consent is given. The mayor never
replies to a keeper event; it publishes a real update, or nothing.

## Enrollment and the invite binding

**A city enrolls once.** The gateway's operator mints an enrollment code for the city
(`POST /v1/admin/enroll-codes`, which takes the city's name; 16 characters, valid 72 hours). On the
box, `gc whatsapp enroll --code <code> --name <name>` calls `POST /v1/enroll`, which creates the city
(recording the name as the box's) and returns its one credential, once. The verb writes it (0600) as
`WHATSAPP_GATEWAY_CREDENTIAL` to `$GC_WHATSAPP_ADAPTER_ENV`, else `$GC_SERVICE_SECRETS_DIR/env`, else
`$GC_CITY_PATH/.gc/services/whatsapp/secrets/env` when `GC_CITY_PATH` is set, else
`~/.config/gc-whatsapp-adapter/env`, and tells the operator to restart the service; the adapter's
own order (under "What a city configures") has no third place, so with neither of the first two set
and `GC_CITY_PATH` set the verb writes a file the adapter reads only when its own environment points
it there (a supervised adapter whose `$GC_SERVICE_SECRETS_DIR` is that directory), and an
unsupervised adapter, reading `~/.config/gc-whatsapp-adapter/env`, does not see the credential. The
adapter sends it as a bearer token on every call; the gateway stores only its sha256.
The gateway depends on no vault, and every city, a test city included, enrolls the same way.

**An owner binds by invite.** `gc whatsapp invite` asks the gateway (`POST /v1/invites`) for a
six-digit code that lives 24 hours; minting a new code deletes the city's unused one. The owner texts
the code, alone, to the number from their own phone. The gateway binds the sender's user id to the
city, spends the code and appends a synthetic "owner joined" delivery for the city in the same D1
batch, then sends the owner "You're connected to <city name>'s mayor. Say hi." A code binds only:

- a sender with a user id that no binding answers to, directly or through a number-change link;
- from a text Meta stamped after that id's last unbind and while the code was live;
- within five code attempts per sender per day.

A redelivered code text binds nothing twice. An owner who is already bound and texts another city's
code is not bound again: the text reaches their current city as an ordinary message. An operator
removes an owner with `POST /v1/admin/unbind`; there is no `gc` verb for it.

## What a city configures

1. Import the pack and add `whatsapp/city-fragment.toml` to `city.toml`'s `include` list. It routes
   WhatsApp conversations to the mayor by default; without it gc finds no route and the adapter
   dead-letters every message as `no_route`.
2. Write the adapter's env file: `$GC_WHATSAPP_ADAPTER_ENV`, else `$GC_SERVICE_SECRETS_DIR/env` when
   supervised, else `~/.config/gc-whatsapp-adapter/env`. It needs `WHATSAPP_GATEWAY_URL`,
   `GC_CITY_NAME` and `GC_API_BASE_URL`; `gc whatsapp enroll` adds `WHATSAPP_GATEWAY_CREDENTIAL`
   (to which file: see "Enrollment and the invite binding").
   Every other name has a default, the optional `ELEVENLABS_API_KEY` for audio included.
3. Enroll with the code the operator gives the city, start the `whatsapp` service, and invite the
   first owner. The doctor rows and `gc whatsapp status` show the door.

The gateway's operator sets, once, five Worker secrets (`META_ACCESS_TOKEN`, `META_APP_SECRET`,
`META_VERIFY_TOKEN`, `META_PHONE_NUMBER_ID` and `ADMIN_TOKEN`, put with `wrangler secret put` and
never written to the repository), the D1 binding `DB` with its migrations applied, and the vars in
`whatsapp/gateway/wrangler.toml` (the public URL, the display name and the Graph API version). Meta
must approve the `dynasty_update` template.

## Two review findings that changed the design

A design review before the build found two holes in the recovery rules. Each produced a rule the
adapter now carries, with tests in the adapter suite.

1. **Check every watermark before an unmarked retry.** The first rules read every conversation's
   watermark only at start and, during recovery, only the affected conversation's. A gc-only restore
   while the adapter runs could drop conversation A's newer record while B's older watermark stayed
   intact; a lost answer on B would then find no record and no gap, and B would be posted again
   unmarked. The rule: the watermark read covers every retained watermark, never one conversation's
   alone. It runs at start when the journal holds a watermark, every `WHATSAPP_WATERMARK_CHECK_MS`
   (60 seconds by default) while the adapter runs, after every poll that brought a new delivery, at
   the start of the in-doubt rule, and again immediately before the in-doubt rule's one plain post,
   which it gates.
2. **Keep a dead entry until its acknowledgement is confirmed.** The first rules let compaction drop
   dead entries, though the dead letter comes before the acknowledgement to the gateway. If that
   acknowledgement failed, compaction removed the guard while the gateway still held the delivery,
   and the next start posted it again as new. The rule: a dead entry is an acknowledgement owed.
   Compaction drops only entries with an `acknowledged` line, written after the gateway confirmed
   the acknowledgement, and the acknowledgement of a dead entry is sent again at every start and on
   its retry ladder while the adapter runs, until the gateway confirms it.

## What gc does not give yet

- A field naming the suppressed receipt on gc's `no_route` answer would let a `no_route` to the
  marked copy read as delivered; until then it is dead-lettered.
- gc's transcript list carries no store identity, so the all-watermark read is the rule, and one case
  remains: a restore taken after every conversation's watermark and before an acceptance the journal
  never recorded, with the paste absent from the provider's log, posts plain.

Adapter comments citing "design line 13", "design line 114" or the keeper's lettered items mean the
earlier text; those rules are in "Identity", "The path", "The journal" and "The window-keeper".
