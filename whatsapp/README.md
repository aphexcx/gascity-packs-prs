# whatsapp

The box side of the shared Dynasty WhatsApp number: WhatsApp as another door
into a city's mayor, the way `slack-full`, the Google Chat adapter and `wecom`
are. The owner and anyone they name talk to their own city's mayor on
WhatsApp; nothing here is a sales or customer channel.

Design of record: [`docs/design/whatsapp-adapter-20261103.md`](../docs/design/whatsapp-adapter-20261103.md)
(gp-76sw, approved by both founders 11/3; round 8 amended in this pack's PR,
gp-9hf1). Built on `aphexcx/gascity-packs-prs` `origin/main`
`8cb01a83bdaa6b4b42aeb63bae1ee4a6d369262f` (read at claim, 2026-11-04), stacked
on the design PR's head `bc2fd1430c3c6093555a8a52262dc69cda819810`. gc wire
shapes are read at gc's fork integration head
`f22b3ed4a5b090e6960373ffc9e7a414aa085309` (and `92a7f383f` where they differ;
the source comments cite file:line at both).

## Components

| Path | What it is |
| --- | --- |
| `pack.toml` | one `proxy_process` service, `whatsapp`, private, `./adapter/run.sh`, health `/healthz` |
| `city-fragment.toml` | `[[extmsg.default_route]] provider "whatsapp" agent "mayor"` (pack.toml cannot carry extmsg config) |
| `adapter/` | Node 20+, plain `fetch`, no package dependency; `run.sh` sources the env file and execs `node src/index.js` |
| `commands/` | `gc whatsapp publish`, `invite`, `consent`, `hours`, `enroll`, `status`, `inbound resolve`, `inbound clear-gap` |
| `doctor/` | nine rows: env, dependencies, runtime, gateway, deadletters, held, restore, probes, keeper |
| `tests/test_pack.py` | discovery, every doctor row failing closed, the verbs against a fake adapter |
| `gateway/` | the Cloudflare Worker that holds the Meta keys (bead A's; not part of this pack's box side) |

The box talks ONLY to the gateway at `whatsapp.dynasty.computer`, over HTTPS
with the per-city credential `gc whatsapp enroll` writes. It never holds a
Meta secret and has no public listener. The gateway contract v1 (the gateway
spec's SCOPE item 3) is pinned route by route by the adapter's fake gateway
(`adapter/test/fakes.js`, `FAKE_GATEWAY_ROUTES`).

## Env (names only; values live in the env file)

`adapter/run.sh` sources `$GC_WHATSAPP_ADAPTER_ENV`, else
`$GC_SERVICE_SECRETS_DIR/env` (supervised; required there), else
`~/.config/gc-whatsapp-adapter/env`.

| Name | Meaning |
| --- | --- |
| `WHATSAPP_GATEWAY_URL` | `https://whatsapp.dynasty.computer` |
| `WHATSAPP_GATEWAY_CREDENTIAL` | the per-city credential (written by `gc whatsapp enroll`, 0600) |
| `GC_CITY_NAME`, `GC_API_BASE_URL` | the city and its API (not controller-injected; check `gc status`) |
| `WHATSAPP_POLL_MS` | inbox poll, default 2000 |
| `WHATSAPP_WATERMARK_CHECK_MS` | the all-watermark restore read, default 60000 |
| `WHATSAPP_ROUTE_AGENT` | the default route's agent for the paste probe, default `mayor` |
| `WHATSAPP_KEEPER_UPDATE_HOUR`, `WHATSAPP_CITY_TZ` | the keeper's daily update hour (default 10) and the city's zone |
| `WHATSAPP_UPDATE_LANGUAGE` | the update template's language, default `en_US` |
| `WHATSAPP_MEDIA_DIR`, `WHATSAPP_MEDIA_MAX_BYTES`, `WHATSAPP_MEDIA_QUOTA_BYTES`, `WHATSAPP_MEDIA_MIN_FREE_BYTES`, `WHATSAPP_MEDIA_DOWNLOAD_TIMEOUT_MS`, `WHATSAPP_TRANSCRIBE_TIMEOUT_MS` | the inbound media store (default `<city>/.gc/whatsapp-media/inbound`) and its limits |
| `ELEVENLABS_API_KEY` | optional Scribe key for voice notes (falls back to `~/.config/elevenlabs/api-key`; without one a voice note is saved and marked not transcribed) |

`WHATSAPP_TEST_CLOCK` and `WHATSAPP_TEST_KILL_AT` are test-only (the crash
tests' clock file and SIGKILL failpoints) and unset in production.

## Inbound: the write-ahead journal

Every gateway delivery gets the request id `whatsapp:dynasty:<provider_message_id>`
(the wamid; `<wamid>:status:failed` for a failed-status delivery, so it never
collides with the outbound record of the message it reports on). The adapter
journals it in `<city>/.gc/whatsapp/inbound_journal.jsonl` (0600 in a 0700
directory; one fsynced JSON line per transition; replayed at start by folding
per rid) BEFORE anything leaves the box:

`received` (the full envelope) → `posted` (with the lookup boundary) →
`POST /extmsg/inbound` → `accepted` (receipt id, verdict, terminal, sequence) →
on gc's **delivered** word the ack to the gateway by delivery id →
`acknowledged`.

- **pending** is read back (`GET /extmsg/inbound/receipts/{id}`) every 5 s
  for 10 min, every 60 s to the hour, then every 5 min, with no cutoff but the
  **24-hour pending deadline** (dead letter `pending_deadline`, then the ack).
  The gateway holds the body until the ack.
- **pending with terminal true** is a TERMINAL HOLD: never posted again, never
  read back; `gc whatsapp inbound resolve <rid> --delivered|--dead` is the only
  way out.
- **failed / partial** reposts plain under the same rid, at most three times,
  then the dead letter `repost_bound`. **no_route** dead-letters at once.
- **No HTTP answer** (the 30 s client deadline, a reset, a refusal) is never
  re-posted on the ladder: the entry stays `posted` and takes THE IN-DOUBT RULE
  after the grace (deadline + 10 s). The ladder (1, 5, 15, 60 s) re-posts only
  on an HTTP answer gc gives before any fan-out (5xx, 404, 408, 429). A
  deterministic 4xx dead-letters (`rejected`).
- **The in-doubt rule**: first the ALL-WATERMARK READ; then the transcript
  lookup from the first posted line's boundary and a second pass from zero;
  then THE EVIDENCE RULE probe of the target session's transcript (only a user
  turn of the provider's own log holding the paste's header and its last
  `Message id:` line counts; a terminal snapshot, `format: text`, never does);
  then one MARKED copy (`[redelivery <wamid>: a first copy may have reached
  you; if you already answered it, do nothing]`) or, in doubt again, the dead
  letter `in_doubt`. A plain repost happens only for an entry the journal
  never saw accepted, absent from the store, the probe NOT FOUND, and every
  watermark read intact immediately before the post with no restore gap
  standing anywhere. A `no_route` to the marked copy probes once more, then
  dead-letters `redelivery_no_route` (gc's suppression answer and its
  empty-membership answer are the same bytes).
- **THE RESTORE RULE and THE ALL-WATERMARK READ (design round 8, the gate
  r7's first finding)**: the journal keeps, per conversation, a WATERMARK (the
  highest accepted sequence and its provider message id), kept through every
  compaction. The adapter reads the gc store at EVERY watermark, never one
  conversation's alone: at start, every `WHATSAPP_WATERMARK_CHECK_MS`, after
  every poll that brought a new delivery, as the in-doubt rule's first step,
  and again before any plain post. A watermark the store no longer holds is a
  RESTORE GAP (the `restore` doctor row), and while any gap stands every
  in-doubt message of every conversation takes the marked path.
  `gc whatsapp inbound clear-gap <wa_id>` rebases the watermark to the store.
- **THE DEAD ENTRY IS AN ACK OWED (design round 8, the gate r7's second
  finding)**: every dead letter writes `inbound_dead_letter/<wa_id>.jsonl`
  (0600, with the envelope) first, then the journal's `dead` line, then the
  ack; compaction drops only `acknowledged` entries, so a dead entry whose ack
  failed survives every compaction and its ack is re-driven at every start and
  every poll until the gateway confirms it. A kill between the file line and
  the journal line is repeated on restart from the file line, with no second
  file line.

Each POST carries `provider_message_id`, `dedup_key` = the rid, the actor
(`is_bot: false`, the profile name), `received_at` = the message time, and
media as `file://` attachments with a "[N WhatsApp file(s) attached] ... saved
to <path>; Read that path to view it" block (voice notes transcribed inline).

## Outbound, the window, and the update template

gc publishes the mayor's replies through `/publish` (session attribution
required). Text is chunked at 4096 characters, serialized per phone, and sent
per chunk under `text:<sha256 of the idempotency key, the recipient and the
whole text>:<n>`, so a retry resumes at the failed chunk; an update goes under
`template:<sha256 of the key, the recipient, the template name, language and
parameters>`, a namespace no text chunk shares. A key reused for another
owner or another message is therefore another gateway key, even once the
adapter's own record of the key (7 days or 1,024 keys) is gone and the
gateway's (30 days) is not. WhatsApp's 24-hour window (24 h after the owner's last message, minus a
120-second margin) is the gateway's to enforce: a free-form send outside it
answers gc's permanent publish error with `metadata.reason window_closed` and
a hint naming the update verb. `gc whatsapp publish --update --to <wa_id>
--text ...` sends the one approved utility template (`dynasty_update`, "Your
city has an update: {{1}}"), refused while the window is open (409) or with
empty text (400), journaled with its text to `<city>/.gc/whatsapp/updates.jsonl`.
A template never opens or extends the window; only the owner's next message
does. Whether a text is an actual update is the mayor's skill rule (Afik:
"only if there is an actual update").

## The window-keeper

For a conversation whose business consented (`gc whatsapp invite
--proactive-consent`, or `gc whatsapp consent <wa_id> --on`), the adapter
reads the gateway's `GET /v1/window` once a minute and raises keeper events
into the mayor's session (actor "Dynasty window-keeper", `is_bot: true`): the
daily update (two hours before close of business, or at
`WHATSAPP_KEEPER_UPDATE_HOUR`) and the close ladder at 60, 30, 15 and 5
minutes before the window closes, kept inside the business's hours. State in
`<city>/.gc/whatsapp/keeper.json`, written before each event; a failed window
read is retried on the ladder (1, 5, 15, 60 s) and the last good read is
stamped in the file for the doctor row. The `consent` and `hours` verbs take
a joined phone in any form and print the last four digits the adapter
matched. The mayor never replies to a keeper event; it publishes a real
update or nothing.

## The verbs

| Verb | What it does |
| --- | --- |
| `gc whatsapp publish --to <wa_id> --text-file <path>` | free-form text inside the window |
| `gc whatsapp publish --update --to <wa_id> --text "..."` | the update template outside the window |
| `gc whatsapp invite [--proactive-consent] [--hours "Mon-Fri 09:00-18:00"] [--tz <zone>]` | a 6-digit, 24-hour invite code and the two lines the owner is told |
| `gc whatsapp consent <wa_id> --off\|--on` | the keeper's consent; `--off` on the owner's word, at once |
| `gc whatsapp hours <wa_id> --hours "..." --tz <zone>` \| `--clear` | the keeper's business hours; `--clear` removes them |
| `gc whatsapp enroll --code <code> --name <city>` | enroll once; writes the credential 0600, prints nothing secret |
| `gc whatsapp status` | enrollment, polls, entries, windows, keeper, holds, gaps, dead letters |
| `gc whatsapp inbound resolve <rid> --delivered\|--dead` | the operator's way out of a terminal hold |
| `gc whatsapp inbound clear-gap <wa_id>` | clear a restore gap and rebase the watermark |

## Doctor rows

| Row | Green | Yellow / red |
| --- | --- | --- |
| env | the four names present (values withheld) | red: file or name missing (`gc whatsapp enroll`); yellow: a shell expression it will not execute |
| dependencies | Node 20+ | red: no node or older |
| runtime | the service answers `/healthz` on `GC_SERVICE_SOCKET` | yellow: socket unset; red: no answer, or the gateway refused the credential |
| gateway | `/healthz` 200 and the credential answers `/v1/inbox?limit=1` | red: unreachable, or 401 "not enrolled: run gc whatsapp enroll" |
| deadletters | none | yellow: the count by reason (`redelivery_no_route` explained) |
| held | none | yellow: each terminal hold, the oldest's age, the resolve verb |
| restore | no gap | yellow: the conversation, the journal's accepted time ahead of the store's head, the clear-gap verb |
| probes | none | yellow: paste probes answered by a terminal snapshot in the last 7 days |
| keeper | none | red: no good `GET /v1/window` read in 5 minutes (`window_read_ok_at` in keeper.json) while a conversation has the keeper ON, the last error named; yellow: lapses, undelivered keeper events, refused publishes in the last 7 days |

## Setup for a city

1. The mayor mints an enrollment code at the gateway (operator side) and gives
   it to the city.
2. `gc import add` the pack from this repo; add `whatsapp/city-fragment.toml`
   to `city.toml`'s `include` list; create the per-city env file with
   `WHATSAPP_GATEWAY_URL`, `GC_CITY_NAME` and `GC_API_BASE_URL`.
3. `gc whatsapp enroll --code <code> --name "<city name>"`.
4. Pre-warm: `(cd whatsapp/adapter && ./run.sh --deps-only)` (checks Node; the
   adapter has nothing to install). Start or restart the `whatsapp` service.
5. `gc whatsapp invite --proactive-consent --hours "..." --tz <zone>` and tell
   the owner the two printed lines. `gc whatsapp status` and the doctor rows
   show the door.

## Tests

```
cd whatsapp/adapter && pnpm exec node --test 'test/*.test.js'
env -u GC_TEMPLATE python3 -m pytest whatsapp/tests -q
```

The node suite runs every crash case against a real child process on a fake
gateway and a fake gc (one transcript record per provider message id, the
fork head's fan-out dedup on demand, a session log or a terminal pane, a
backup the test restores), under a clock file the tests advance and SIGKILL
failpoints between journal lines. The case numbers (1)–(12c) follow the
pack-adapter spec's SCOPE item 6.

## Linux

The boxes are Linux arm64. The proof (a disposable Debian bookworm arm64
container, the shape of `docs/plans/linux-proof-slack-wecom-20261027.md`; gc
need not run there, the adapter's tests and `run.sh --deps-only` are the
proof) is recorded in `assets/pr-evidence/gp-9hf1/linux-proof.txt` with its
commands and counts. A live round trip waits on the Hou Dynasty proof (bead D).

## gc gaps (fork beads at the mayor's hand, never patched around here)

- `suppressed_for` on gc's fan-out suppression answer: today a `no_route` to
  the marked copy cannot tell "already delivered" from "nobody routed", so the
  adapter dead-letters it (`redelivery_no_route`), the safer error.
- A store identity (an epoch) on the transcript list: without it the
  all-watermark read is the rule, and one residual remains (a restore taken
  after every conversation's watermark and before an acceptance the journal
  never recorded, with the paste absent from the provider's log, posts plain).
- At gc `92a7f383f` the inbound fan-out is not idempotent on
  `provider_message_id` (the fork head `f22b3ed4a` dedups it, hq-703om); the
  pack is correct on either.
