# whatsapp

## Arrival reactions and stopwatch

`ACK_REACTION` is empty (off) by default. Set it to `jev` to send only the
message's text to TypeSafe, after the same secret redaction as Slack's
(`adapter/src/redact.js`, a port of Slack's redaction with a copy of its
patterns) and then cut to 1,000 Unicode characters,
using `JEV_API_KEY`; no sender, room, IDs or attachment bytes are sent. Jev
chooses from the arrival rows of `reactions.json`; absent credentials, unusable
catalogs, timeouts and unusable answers use the fixed eyes reaction.

A fixed arrival row's `slack_name` also works in every adapter. Claim rows,
`auto`, `model`, and unknown names are rejected at startup. With an unusable
catalog, only `jev` and `eyes` are available. `ACK_SCOPE=addressed` is the
default; `all` includes delivered room messages. `ACK_SCOPE` is checked only
when `ACK_REACTION` is set. Own messages, bots, mutations,
reactions, keeper events, replays, duplicates, and messages over 60 seconds old
are excluded. A durable attempt marker prevents a second acknowledgement after
a restart, even if a channel response was lost. Acknowledgements are never removed.
Markers older than one day are removed at start and once a day.

The adapter env file overrides inherited values; otherwise `JEV_API_KEY` comes
from the process environment. Key changes require an adapter restart. Status
reveals only whether a key is set. `JEV_API_BASE_URL` defaults to
`https://api.typesafe.ai` and requires HTTPS (local HTTP is allowed for tests);
`JEV_MODEL` defaults to `jev-latest`. There are at most four concurrent choices,
each with a budget of 1,500 milliseconds from first sight. Delivery to gc runs
independently of choices and channel requests. The reaction request target is
two seconds from first sight; channel confirmation can take longer.

WhatsApp uses the existing gateway reaction send with an idempotency key derived from the inbound message. Read marks remain at gc's first acceptance. There is no local reaction pace and a refused acknowledgement is not retried.

The WhatsApp words time is the adapter's clock at confirmed acceptance, so the round trip can make it later than the true acceptance time. A line with `acceptance_uncertain: true` is an upper bound because confirmation followed a retry, recovery, or restart.

Words are recorded once per returned message ID; another confirmation never moves the time. State defaults to `WHATSAPP_STATE_DIR`, or the city's `.gc/whatsapp` directory.

The stopwatch runs even with acknowledgement off. It writes monthly UTC
`stopwatch-YYYY-MM.jsonl` files with mode 0600, containing coordinates and
timestamps only. Inbound lines include the first 12 SHA-256 hex characters of
the adapter's source files in a fixed sort; when they cannot be read, the
release is `unknown`, one warning is logged, and the adapter starts.
Reaction lines record the first confirmation, actor and adapter choice method;
words lines record posts. An unwritable journal warns once and delivery continues.
The lines are appended and flushed off the delivery's thread, on one chain
shared with the markers' flushes (so they hold at most one thread of Node's
pool), and no delivery or send waits on the disk; a stop drains them. Each send creates one empty
marker under `words-attempts` first (a send waits for the create, never for
its flush); markers older than 31 days are removed at start and once a day.
The adapter never deletes a stopwatch file. `GC_SERVICE_STATE_ROOT` overrides the
timing directory for every adapter.

Run `gc whatsapp stopwatch` with the pack's installed alias. It requires Python 3.11+
and needs no running adapter.
With city context it discovers the service's state directory when present.
`--state-dir DIRECTORY` selects a custom directory; `--since TIME` and
`--until TIME` filter inbound channel timestamps inclusively; `--json` prints
the same numbers as JSON. Each release reports count, nearest-rank median and
p90 seconds to the first reaction and first words, plus messages older than an
hour without words. A reply answers all waiting messages in its conversation
and thread; torn journal lines are skipped.

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

The words around the code (gp-6mr7): the mayor reads the skill
[`skills/whatsapp/SKILL.md`](skills/whatsapp/SKILL.md) before any WhatsApp
reply; a WhatsApp-bound session composes the prompt fragment
[`template-fragments/whatsapp-v0.template.md`](template-fragments/whatsapp-v0.template.md);
an owner gets [`docs/owner-onboarding.md`](docs/owner-onboarding.md) (English
and Portuguese) at the invite; Afik opens the Meta account with
[`docs/meta-setup.md`](docs/meta-setup.md); [`docs/README.md`](docs/README.md)
points at all of them.

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
`$GC_CITY_PATH/.gc/services/whatsapp/secrets/env`, else
`~/.config/gc-whatsapp-adapter/env`. Each variable is used only when non-empty.
Enrollment and diagnostics use the same selection. Relative paths are based
at the adapter directory; a missing selected file never falls back to another.

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
| `WHATSAPP_OUTBOUND_ROOTS` | allowed absolute directories for outbound files and caption files, separated by the platform path delimiter (`:` on the city boxes); default the city directory and inbound media store. An explicit empty/invalid list fails closed. Denied secret/key paths always remain denied. |
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

**The owner's key (gp-g02t).** Each delivery belongs to the owner the
gateway names by its `conversation_id` (PR 76's inbox): fixed when the owner
binds (the owner's user id; for an owner PR 73 bound, its number), the same
after a number change, and never shared by two owners, a recycled number
included. The `received` line records it, and every later step reads
it from there: the gc conversation (`whatsapp/<conversation_id>`) and the
actor, the per-owner chain, the watermark, the lookup and the paste probe, the
dead letter, the window ledger, the keeper's bind and the media directory.
The `wa_id` is only the number the delivery shows (null for an owner known by
WhatsApp user id alone): the actor's display name when there is no profile
name, never a key. A delivery whose `conversation_id` is null or empty is left
unacknowledged. A delivery from the gateway contract v1 (PR 73), which has no
`conversation_id` field, is keyed by its `wa_id`, that contract's owner key:
the boxes run it until the gateway's deploy (this adapter rolls first). A
`received` line the PR 74 adapter wrote names its conversation by `wa_id` and
is read that way (tolerated, never rewritten).

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
  `gc whatsapp inbound clear-gap <conversation_id>` rebases the watermark to the store.
- **THE DEAD ENTRY IS AN ACK OWED (design round 8, the gate r7's second
  finding)**: every dead letter writes `inbound_dead_letter/<conversation_id>.jsonl`
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
required). Text is chunked at 4096 characters, serialized per conversation, and sent
per chunk under `text:<sha256 of the idempotency key, the recipient and the
whole text>:<n>`, so a retry resumes at the failed chunk; an update goes under
`template:<sha256 of the key, the recipient, the template name, language and
parameters>`, a namespace no text chunk shares. A key reused for another
owner or another message is therefore another gateway key, even once the
adapter's own record of the key (7 days or 1,024 keys) is gone and the
gateway's (30 days) is not. **The recipient** (gp-g02t): each publish and
each update reads the gateway's `GET /v1/window` once and sends to the user id
of the entry whose `conversation_id` is the conversation, never to a number (a
number may name another owner once its owner is unbound or it is recycled). A
conversation the window no longer lists (its owner unbound, or not a
conversation id at all), or lists with no user id (an owner PR 73 bound by
number alone), is gc's permanent publish error `metadata.reason not_bound`
with `metadata.recipient` saying why, and nothing is sent. PR 73's window has
no user ids; there the send names the conversation id itself. A 409
`ambiguous_recipient` (nothing sent) is `metadata.reason gateway_refused` with
`metadata.gateway_error ambiguous_recipient` and a hint, never resent. WhatsApp's 24-hour window (24 h after the owner's last message, minus a
120-second margin) is the gateway's to enforce: a free-form send outside it
answers gc's permanent publish error with `metadata.reason window_closed` and
a hint naming the update verb. `gc whatsapp publish --update --to <conversation_id>
--text ...` sends the one approved utility template (`dynasty_update`, "Your
city has an update: {{1}}"), refused while the window is open (409) or with
empty text (400), journaled with its text to `<city>/.gc/whatsapp/updates.jsonl`.
A template never opens or extends the window; only the owner's next message
does. Whether a text is an actual update is the mayor's skill rule (Afik:
"only if there is an actual update").

## Files out

`gc whatsapp publish --to <conversation_id> --file <path>` sends one image,
document, audio file or video. Add `--caption` or `--caption-file` if needed
(up to 1,024 characters; no audio captions). `--text-file` cannot accompany a
file: use `--caption-file`. A file cannot use `--update`.

JPEG, PNG and static WebP images cap at 5 MB, AAC/AMR/MP3/M4A/mono Opus Ogg
audio and MP4/3GP video at 16 MB, and PDF, UTF-8 text and Office documents at
100 MB (1 MB = 1,000,000 bytes). The adapter checks the actual size and a
signature against the extension before upload; it never transcodes. The
gateway's [Files out contract](gateway/README.md#files-out-gp-755k) cites the
current Meta list and its additional image/codec constraints.

The adapter resolves symlinks and permits only regular files within
`WHATSAPP_OUTBOUND_ROOTS`. Both supplied and resolved paths must avoid
`.gc/services`, `.gc/keys`, components `secrets`, `.ssh`, `.config`, and
filenames ending `.env` or `.pem` or beginning `id_` (case-insensitive, a
tighter rule). Caption files have the same restriction. Never send a secret,
credential or key, an unexpected file, or another person's file without
their request.

The adapter also checks the kernel pathname of the opened descriptor before
and after reading, so replacing a parent directory cannot authorize a secret
file. Hard-linked files are refused, including otherwise allowed aliases.
Linux uses `/proc/self/fd`; macOS uses the OS-shipped `/usr/sbin/lsof` for
this process and descriptor. If that facility is unavailable, or on another
OS, file publishing fails closed with `kernel descriptor path inspection
unavailable`. No additional package is installed. This per-file check covers
the capability, so no new doctor row is added.

The key printed by the publish command covers the whole upload and send.
`outbound_progress.json` holds the name, bytes, SHA256, caption and upload
handle; the gateway also durably deduplicates uploads, covering a crash
before the box records that handle. A retry with the same file, caption and
key resumes without repeating either successful operation. A changed file
conflicts. A failed upload sends nothing; an uncertain Meta outcome stays
spent under that key and requires an operator check before any new key.
The transcript names the file and its byte count/SHA256, never its bytes.
File recovery records remain in the journal for 30 days after their last
update, covering the gateway's idempotency horizon; text's seven-day/1,024
entry pruning never evicts them. File records are bounded by age, not count.
An expired/missing handle is a permanent `media_expired` receipt with guidance
to check the chat before starting a new key. The adapter still asks for a
completed send's stored answer first, so a delivered file is recovered even
after its upload handle expires.

The window rule is unchanged. A closed window is `window_closed`; wait for
the owner to write again. A file never goes out by the update template and
never opens or extends the window. Uploads use the media client's existing
120-second deadline (`WHATSAPP_MEDIA_DOWNLOAD_TIMEOUT_MS`); text calls keep
their existing deadline. No new doctor row: gateway reachability and
credentials are already covered, and file/path validation is per publish.

## The window-keeper

For a conversation whose business consented (`gc whatsapp invite
--proactive-consent`, or `gc whatsapp consent <conversation_id> --on`), the adapter
reads the gateway's `GET /v1/window` once a minute and raises keeper events
into the mayor's session (actor "Dynasty window-keeper", `is_bot: true`): the
daily update (two hours before close of business, or at
`WHATSAPP_KEEPER_UPDATE_HOUR`) and the close ladder at 60, 30, 15 and 5
minutes before the window closes, kept inside the business's hours. State in
`<city>/.gc/whatsapp/keeper.json`, written before each event; a failed window
read is retried on the ladder (1, 5, 15, 60 s) and the last good read is
stamped in the file for the doctor row, with the conversations it listed: a
conversation the last good read did not list (its owner unbound) raises
nothing. The `consent` and `hours` verbs take a joined conversation's id (a
user id whole, a number in any form) and print the last four digits the
adapter matched. The mayor never replies to a keeper event; it publishes a real
update or nothing.

## The verbs

| Verb | What it does |
| --- | --- |
| `gc whatsapp publish --to <conversation_id> --text-file <path>` | free-form text inside the window |
| `gc whatsapp publish --to <conversation_id> --file <path> [--caption-file <path>]` | one allowed file inside the window; upload/send resume under the printed key |
| `gc whatsapp publish --update --to <conversation_id> --text "..."` | the update template outside the window |
| `gc whatsapp invite [--proactive-consent] [--hours "Mon-Fri 09:00-18:00"] [--tz <zone>]` | a 6-digit, 24-hour invite code and the two lines the owner is told |
| `gc whatsapp consent <conversation_id> --off\|--on` | the keeper's consent; `--off` on the owner's word, at once |
| `gc whatsapp hours <conversation_id> --hours "..." --tz <zone>` \| `--clear` | the keeper's business hours; `--clear` removes them |
| `gc whatsapp enroll --code <code> --name <city>` | enroll once; writes the credential 0600, prints nothing secret |
| `gc whatsapp status` | enrollment, polls, entries, windows, keeper, holds, gaps, dead letters |
| `gc whatsapp inbound resolve <rid> --delivered\|--dead` | the operator's way out of a terminal hold |
| `gc whatsapp inbound clear-gap <conversation_id>` | clear a restore gap and rebase the watermark |

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
