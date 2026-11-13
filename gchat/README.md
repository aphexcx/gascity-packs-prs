# gchat

## What it is

A reusable Google Chat provider pack for Gas City. One Workspace user identity
speaks for the city's agents. It receives DMs, messages in joined spaces,
edits, deletions, reactions and attachments through Workspace Events API and
Pub/Sub streaming pull. It sends text, files and reactions through the Chat API.
There is no public listener or webhook URL. gc supervises the Go adapter and
proxies its private Unix socket.

DMs always wake the bound agent. Spaces default to every message, with a
per-space `mentions` option. Unbound conversations route to the configured
default agent; `gc extmsg bind` can route individual conversations elsewhere.
Every agent uses the same visible identity, without a name prefix. Cards,
dialogs, slash commands and multiple identities are outside this version.

## Google-side setup

- [ ] A Google Cloud project in the company's Workspace, with the Google Chat API, Google Workspace Events API and Cloud Pub/Sub API enabled.
- [ ] OAuth consent screen: user type Internal; an app name chosen with care.
- [ ] Chat API configuration (Google Chat API, Configuration tab): application name (25 characters at most), an avatar URL (HTTPS, square PNG or JPEG, 256 pixels or more) and a description (40 characters at most); interactive features left off. Google requires this before any create, update or delete call, even as a user, and attributes those writes to this name.
- [ ] An OAuth client of type Desktop app; its JSON placed on the city machine at the path in `GCHAT_OAUTH_CLIENT_FILE` (mode 600), never sent through chat.
- [ ] A Pub/Sub topic; `chat-api-push@system.gserviceaccount.com` granted Pub/Sub Publisher on it.
- [ ] A pull subscription on the topic, acknowledgement deadline 60 seconds.
- [ ] The identity's user account granted Pub/Sub Subscriber on the subscription (or the project).
- [ ] Project id, topic and subscription names into the secrets file; `gc gchat auth` at the keyboard; the route line into `city.toml`; `gc reload`; the live checklist.

To confirm in the first live test: that the `spaces/-` subscription covers
spaces joined after it was created; end-to-end event latency; whether the
sender's email is present under user authentication.

Use an account belonging to the agent identity. An External consent screen in
Testing issues refresh tokens that expire after seven days; use Internal for
this Workspace deployment. Consent requests `chat.messages`,
`chat.spaces.readonly`, `chat.memberships.readonly`, `pubsub`, and `openid`.
The last scope supplies the identity's numeric user ID at sign-in, allowing
inbound delivery before the first outbound message.

## Managed credentials (a key holder)

A headless machine can use desktop consent with a browser elsewhere and a
secure loopback callback tunnel or operator-assisted relay; see boxboxbox's
[Google onboarding skill](../../skills/google-onboarding/SKILL.md). A browser's
localhost is not the remote box. When another process on the machine already
holds a credential for the identity, such as boxboxbox's key holder (a service
account with domain-wide delegation, serving one Workspace user over a Unix
socket), the adapter can take its tokens from there instead:

```sh
GCHAT_TOKEN_SOURCE=keyholder
GCHAT_KEYHOLDER_SOCKET=/run/boxboxbox-keyholder/keyholder.sock
```

replaces `GCHAT_OAUTH_CLIENT_FILE` and the token file. The key holder must
answer `POST /v1/token {"scopes": [...]}` with `{"access_token", "expires_at"}`
(milliseconds since the epoch) or `{"error": "<code>", "detail": ...}`, and
`GET /v1/info` with `{"subject": "<address>"}`. The adapter asks it for
`KeyholderScopes` (`adapter/internal/token/scopes.go`): the Chat and Pub/Sub
scopes without `openid`, plus `directory.readonly`, which finds the identity's
own Chat id in the Workspace directory (without it, the id is learned from the
first outbound message). `gc gchat auth` then
belongs to the key holder's enrollment (on boxboxbox, `boxboxbox-google
enroll`), and `auth --check` asks the key holder for a token. A permanent
refusal (`delegation_not_authorized`, `subject_not_found`) is `needs-sign-in`
with the usual notice, retried every 30 seconds without a restart; a Chat
refusal of a token the key holder still mints (Chat app not configured, Chat
off for the user, a new user with no conversation yet) stays `initializing`
with one `chat-access` notice and keeps retrying. The Pub/Sub topic,
subscription and grants in the checklist above are still needed; boxboxbox's
key holder creates them.

## Install

Requirements: gc with schema-2 packs, `proxy_process` services and the extmsg
API; a Unix platform with advisory file locks; Go 1.27.2 or newer for builds;
POSIX sh, curl, jq, and the standard `od`
and `tr` utilities. Python 3 is needed only for the wrapper tests. The source
module is independent; no global Go install step is required.

1. Place the pack at `packs/gchat`. Import it under the binding `gchat` in the
   city's `pack.toml` (adjust the source path if stored elsewhere):

   ```toml
   [imports.gchat]
   source = "./packs/gchat"
   ```

2. Create `<city>/.gc/services/gchat/secrets/env` and the adjacent
   `<city>/.gc/services/gchat/config.toml` using the examples below. Keep the
   secrets directory private and the env and OAuth client JSON files mode 600.
   Use absolute paths to files and the API base URL for this city.
3. Paste the block from [city-fragment.toml](city-fragment.toml) into
   `city.toml`. The pack parser does not accept extmsg configuration inside
   `pack.toml`. The route is required to wake an agent.
4. Build once with `(cd packs/gchat/adapter && ./build.sh)`, then run
   `gc reload`. A missing token leaves the service in `needs-sign-in`, with
   health and status still available. Launchers rebuild a missing or stale
   binary from the local sources.
5. On the city machine, run `gc gchat auth` and complete consent in the browser.
   This command works while the service is stopped. The running service watches
   the token file and accepts a validated replacement without a manual restart.
6. Run `gc gchat auth --check`, `gc gchat status`, and `gc doctor`.
   The pack's doctor stops at the first failure: secrets, token refresh,
   subscription `ACTIVE`, then a gchat default route in `gc config show --json`.

gc supplies `GC_API_BASE_URL` to pack commands. Ordinary commands never source
secrets; the service reads its own configured API base from the env file.

Live checklist after installation: list spaces; DM round trip; a message in an
`all` space; a mention in a `mentions` space; a reply in the correct thread;
a file and a reaction each way; join a new space and receive its first message;
observe subscription renewal; restart and observe catch-up; measure latency.
Verify a paginated catch-up clamped to the 28-day event window, including an
event near the moving retention boundary between pages. Compare the
`identity_user_id` from `auth --check` with the first created message's sender.
Explicitly verify DM threading: this version sends `thread.name` without the
named-space reply option, and the live test must confirm Google's behavior.

## Secrets and settings

Secrets at `<city>/.gc/services/gchat/secrets/env`:

```sh
GCHAT_PROJECT_ID=example-project
GCHAT_PUBSUB_TOPIC=projects/example-project/topics/chat
GCHAT_PUBSUB_SUBSCRIPTION=projects/example-project/subscriptions/chat
GCHAT_OAUTH_CLIENT_FILE=/absolute/path/client.json
GCHAT_TOKEN_FILE=token.json
GC_CITY_NAME=examplecity
GC_API_BASE_URL=http://127.0.0.1:8372
GCHAT_OWNERS=dana@example.com,cfo@example.com
GCHAT_ORGANIZATION_DOMAIN=example.com
```

The last two are optional and drive the provenance frame, the first line of
every turn: `[from: Alice Example <alice@example.com> | organization | item
gchat:spaces/AAA/messages/BBB]`. The item is the message's own resource
name: what the agent passes to `boxboxbox-google mail --for` so the key
holder can verify who asked at the source.
`GCHAT_OWNERS` lists owners by address or Chat id (`users/123`);
`GCHAT_ORGANIZATION_DOMAIN` makes senders with that mail domain the
organization. Addresses come from the Workspace directory (People API,
`directory.readonly`, which the key holder source requests); a desktop
sign-in has no directory scope, so the frame shows the Chat id and the
organization is the Workspace domain the identity's own messages belong to,
learned from the first one it sends. Bots and deleted users are always
external. Invisible characters are removed from the text and counted in a
trailing `[N hidden characters removed]` line; `gc gchat read` history
returns the text without the frame.

All keys except `GCHAT_TOKEN_FILE` are required, with `GCHAT_OAUTH_CLIENT_FILE`
replaced by `GCHAT_TOKEN_SOURCE=keyholder` and `GCHAT_KEYHOLDER_SOCKET` for
managed credentials. The token defaults to
`token.json` relative to the secrets directory; `auth` writes it atomically.
Keep values literal `KEY=VALUE`, with optional single or double quotes. Use
full paths rather than shell expansion, and put comments on separate lines.
The file contains credentials and must never be committed or sent through chat.

The service resolves its env file from `GC_GCHAT_ADAPTER_ENV`, then
`$GC_SERVICE_SECRETS_DIR/env`, then `~/.config/gc-gchat-adapter/env` for standalone
use. Auth and doctor use the same order; when gc supplies only `GC_CITY_PATH`,
they derive its `.gc/services/gchat/secrets` directory. Explicit overrides win.
Settings are at `<secrets dir>/../config.toml`; the standalone home fallback
instead uses `~/.config/gc-gchat-adapter/config.toml`. A missing settings file
uses defaults, but create it before changing wake policies with `spaces`.

Settings example:

```toml
[defaults]
wake = "all"

[[identity]]
name = "example"
agent = "mayor"
token_file = "token.json"

[[space]]
id = "spaces/AAAA"
wake = "mentions"

[media]
inbound_dir = "/absolute/path/examplecity/.gc/gchat-media/inbound"
upload_root = "/absolute/path/examplecity/.gc/gchat-out"
max_bytes = 209715200
max_attachments = 20
max_total_bytes = 2147483648
```

Configure one identity. Its name is gc's `account_id`; omit `token_file` to use
`GCHAT_TOKEN_FILE`. If the identity section is absent, the account ID defaults
to `default` and the agent to `mayor`. Settings reload on change after validation.
Media defaults to the city paths shown above; uploads must be inside
`upload_root`, without symlinks, and at most 200 MB. Inbound attachment text
includes its local path and MIME type, or an unavailable reason; Drive
attachments remain links. One message carries at most `max_attachments` (20)
downloaded files, and the store under `inbound_dir` is capped at
`max_total_bytes` (2 GiB); past either bound the attachment is reported
unavailable instead of stored. Nothing is deleted to make room, and nothing
expires: a path the agent was handed keeps working until someone removes it,
so a full store needs a decision, not a background sweep. The root-level `catchup_budget` setting defaults to
50 records per space per pass; it is a budget, not a truncation limit.

The root-level `coalesce_window` setting is a Go duration string from `"0s"`
to `"60s"`, default `"8s"`. Room chatter waits this long from the first act,
then arrives as one turn. `coalesce_window = "0s"` restores individual turns
with their original text and ids. A validated settings replacement reloads it
with the other settings. Remove this key before rolling back to 0.5.0,
whose settings loader refuses unknown keys, as with `first_start_history` below.

The root-level `first_start_history` setting optionally includes a short history
window on first start or reset. It is a Go duration string, from `"0s"` to
`"168h"` inclusive; write `"24h"` for a day, not `"1d"`. Omit it to start at
the first successful subscription attempt. Set it before connecting; changing
it after a subscription has been recorded does not change the saved history bound.
Remove this key before rolling back to 0.5.0, whose settings loader refuses
unknown keys. The older ledger reader drops the floor, per-space starts and
inventory boundary, keeps saved cursors, and recognizes skipped acts as non-turns.

Optional endpoint/path overrides for tests and private deployments:
`GCHAT_CHAT_API_BASE`, `GCHAT_EVENTS_API_BASE`, `GCHAT_PUBSUB_ENDPOINT`,
`GCHAT_OAUTH_AUTH_URL`, `GCHAT_OAUTH_TOKEN_URL`, `GCHAT_PEOPLE_API_BASE`,
`GCHAT_MEDIA_DIR`, `GCHAT_UPLOAD_ROOT`, `GCHAT_STATE_FILE`. Export overrides in the service process;
OAuth URLs can also be read from the env file by standalone auth.

The five URL overrides must be `https`, or plain `http` on this machine
(`127.0.0.1`, `::1`, `localhost`), with no credentials and no query. The Chat
base carries the identity's bearer token and the token URL the OAuth client
secret, so a plaintext host leaks a credential rather than reaching a mirror:
the adapter refuses to start and names the key. `GCHAT_PUBSUB_ENDPOINT` is an
unauthenticated `host:port` and is refused off this machine for the same
reason — events would arrive from anyone. Both exist for the in-memory fakes.
A relative `GCHAT_STATE_FILE` resolves beside the env file, consistently for
the service and offline recovery commands.

## Never silent (opt-in)

Set `NEVER_SILENT=on` to enable notices for newly admitted human messages addressed to the mayor. It is off by default. Edits, deletions, reactions, bots and recovery traffic do not start notices. A busy mayor is never reported just for taking time to reply.

An act older than its history bound starts no notice, and a message whose attempt the first load retired earns none.

The adapter knows `not_delivered` when it stops trying, or observes three continuous minutes of definite delivery failures or connection/dial errors, city-not-found responses or server errors. A successful HTTP response saying pending resets the failure wait. An uncertain stopped delivery raises operator mail immediately but waits three minutes without a session reply or reaction before telling the owner.

The other causes are `no_seat`, `seat_at_cap` and `session_down`. They require delivery (including a concluded pending receipt with every member's whole message pasted), three minutes without words or a reaction from the addressed session, and fresh health evidence. Set `MAYOR_HEALTH_FILE` to override `<city>/.gc/mayor-health.json`. The JSON contains `state` (ok, no_seat, seat_at_cap or session_down), RFC 3339 `since` and `checked_at`, and optional RFC 3339 `resets_at`. Missing, malformed, unrecognized or more than five-minute-old health reads unknown.

Nothing in these packs writes that file yet. Its writer must run independently of gc's supervisor so evidence remains fresh when the supervisor fails. It must never call an asleep, drained or suspended session down: gc wakes those sessions for incoming messages. Until a writer exists, only not_delivered can fire.

The owner receives one of these exact lines in the same conversation and thread, with the usual reply identity:

- not_delivered or session_down: "I can't read your message right now. The team that runs me has been told, and I'll answer as soon as I'm back."
- no_seat: "I can't answer yet because no Claude account is signed in for me. The team that runs me has been told."
- seat_at_cap: "I've reached my usage limit for now and can't answer until it resets. The team that runs me has been told."

A durable reservation permits at most one attempt per conversation in six hours. A refused post is logged once, and restart never repeats that attempt. There are no reminders or recovery messages to owners. Notices do not count as session replies. The local mayor also learns which fixed line was posted.

Set `NEVER_SILENT_OPERATOR_CONTEXT` to the operator city's gc context. `NEVER_SILENT_OPERATOR_AGENT` defaults to mayor; `GC_BIN` defaults to gc. Without a context the adapter logs once when an outage starts and sends no remote mail. Each adapter/cause outage gets a start mail and a cleared mail, with durable sent marks and failed-send retries at most once a minute for 24 hours. Remote gc accepts `--dedup` but the pinned implementation does not forward it; the adapter's durable sent mark provides the guard.

Operator mail contains only city, adapter, cause, times, aggregate counts and age, owner-notice counts, relevant health state/check time and a status command. It contains no customer text, name, address or conversation coordinates. Local state retains the coordinates needed to reply; those never leave the city in operator mail.

`gc gchat status` and its `--json` output include enabled state, health and its age or unknown reason, open outages with aggregate counts and sent marks, and the number of owner lines posted in seven days.

## Commands

| Command | Purpose |
| --- | --- |
| `gc gchat auth [--check]` | Sign in, or force a token refresh and print readiness and `identity_user_id`. |
| `gc gchat publish --conversation-id spaces/AAAA --body-file post.txt` | Start a new topic that answers nothing; use `--to MESSAGE` to answer a message. |
| `gc gchat reply-current --to spaces/AAAA/messages/ID --body-file reply.txt` | Answer with the command printed by the turn. The adapter takes the space and thread from that message. |
| `gc gchat upload --to spaces/AAAA/messages/ID --file /absolute/path/file.txt` | Send a file in reply; use `--conversation-id spaces/AAAA` for a new topic. |
| `gc gchat react --message spaces/AAAA/messages/ID --emoji '👍' [--remove]` | Add or remove the identity's reaction. |
| `gc gchat read --space spaces/AAAA [--thread NAME] [--since RFC3339] [--limit 100]` | Read normalized history, newest last; limit 1–1000. |
| `gc gchat spaces [--space spaces/AAAA --wake all\|mentions\|default]` | List spaces, or persist a wake override; `default` clears it. |
| `gc gchat status [--json]` | Show health, subscription, last event, backlog, counters, and deliveries with their age and cause. |
| `gc gchat resolve ID --accept\|--replay` | Settle an unresolved delivery. |
| `gc gchat state restore FILE\|reset` | Request recovery through the running service's `/state` endpoint. |

Text commands accept `--body TEXT` or `--body-file PATH`. File contents retain
trailing newlines and stream through the wrapper without becoming process
arguments. The service accepts at most 4 MiB of JSON per command; escaping adds
to the body size. For an answer, run the reply command printed by that turn,
with `--to MESSAGE`. The value is on its Message id line or in the `gchat:`
item in its first line. A bare message name, its `gchat:` item, or the adapter's
edit, deletion or reaction id all identify the same message. The adapter uses
its recorded thread, or reads the message from Google when it has no record.
An unknown or unreadable message is refused. `--top-level` sends in the same
space without a thread. An additional `--conversation-id` or `--thread` must
agree with the reference; a conflict is refused.

`reply-current` requires `--to`, including for turns printed before this
change. `publish` and `upload` require `--to` or `--conversation-id`; both
may be supplied when they agree. Publishing with a space starts a new topic
and answers nothing. Sends use `--session`, then `GC_SESSION_ID`, for transcript
recording. If only a session name is available, pass its id with `--session`.
The wrappers make no event, binding or session lookup.

Publish, reply and upload print an idempotency key to stderr **before** sending.
Keep that key and pass `--idempotency-key KEY` on every retry of the same send,
including after a timeout. A new invocation without a key creates a new send.
JSON output reports `delivered` and `recorded` separately. Delivery failure
exits nonzero; delivery without transcript recording exits zero with a warning.
Inspect `reason`, `failure_kind` and `retry_after` (nanoseconds) before retrying.
Every result also includes `conversation_id`, `space_name` (when known),
`thread`, and `in_reply_to` (empty for a new topic). A delivered send writes
one INFO line with its operation, destination ids, answered message, key,
posted message id and session; the text is excluded. The ledger keeps the
answered message and known space name. Rolling back drops those optional
fields on the next save; existing delivery receipts and destinations survive.

All network commands use `/v0/city/$GC_CITY_NAME/svc/gchat/<route>` at
`GC_API_BASE_URL`; mutations include `X-GC-Request: gc-gchat`. Publish and reply
use `/commands/publish`, upload uses `/commands/upload`; `/publish` is gc's
callback. Wrappers never connect directly to the service socket. Auth is local.

## Operating

`status` distinguishes `initializing`, `needs-sign-in`, `recovery-required`,
`catching-up`, and `ready`. `/healthz` answers immediately during startup,
with HTTP 200 and `X-GC-Health: degraded` when work cannot yet proceed, so gc
keeps the proxy available. Check the health body and subscription state rather
than treating HTTP 200 alone as ready. Subscription expiry and retry details,
last event time, backlog, quarantine, recording backlog and all counters are
visible. `pending` includes intents waiting to be sent or retried and receipts
still in flight; each delivery's status is shown separately, and `stuck`
counts those waiting two minutes or more (see Delivery uncertainty). Health
can read `ready` while messages are waiting or stuck: a message that gc keeps
refusing is retried by the adapter's poll and does not hold catch-up or
readiness. `status` and the `gchat: delivery` mail are where a stuck message
shows.

A rejected access token gets one forced refresh and client restart, counted
under `credential_refresh_restarts`. Permanent refresh failures such as
`invalid_grant`, or two credential failures within five minutes despite
refreshes, enter `needs-sign-in`. The configured agent receives one mail notice
per cause. Run `auth` again; accepted token replacements resume work. Old token
files without `identity_user_id` warn once to re-run auth and can otherwise
learn the identity from the first outbound response. An observed sender that
disagrees with the consent seed wins, with one log entry.
On the first upgrade from a ledger without `identity_seed`, the token seeds
the identity once, replacing any old observed identity. Later starts retain
an observed identity until the consent subject changes.

Subscription 403/404 failures immediately notify `[[identity]].agent` (default
`mayor`) with setup guidance and a sanitized cause. Other subscription failures,
including quota errors, retry with backoff and notify after three consecutive
failed reconciliation attempts; isolated transient failures stay quiet. Status
polls do not count as attempts. Notices deduplicate by cause until subscription
recovery, which clears the failure count and notice deduplication.

### Room message windows

After a hosted box updates this pack and restarts its adapter, room chatter
arrives up to eight seconds later, with a burst in one turn. Direct messages,
mentions of the mayor and replies in a thread she wrote in arrive as fast as
before. A mention or thread reply takes that room's held messages along in
one turn. Reactions wait out the window, including reactions in DMs. After
downtime, missed messages arrive in merged turns. Nothing changes before the
pack update and adapter restart; `coalesce_window = "0s"` turns it off.

Each space has a fixed window, ending early at 50 acts or before adding a
message would exceed 32,000 bytes. One oversized message goes alone. Each
message keeps its own sender, class and reply id. Edits inside a window replace
unseen text, and messages created then deleted are omitted. Status shows the
window and held act count; held acts are not stuck deliveries.

The ledger saves held messages before acknowledging them and saves a merged
turn before posting it. A stop leaves them there; the next start delivers them
before that space's catch-up. A merged turn has one receipt, retry record and
unresolved id, with its member count in status. Resolve that id once to replay
the exact merged text or accept every member. History reads still use each
message's own text. A rollback delivers held messages individually and keeps
delivered groups completed. A frozen group in flight remains one delivery on
the older reader: pending receipts are polled, and an interrupted POST becomes
one unresolved record, requiring the usual explicit replay or acceptance.

### Delivery uncertainty

gc answers every inbound POST with a delivery status. `delivered` and
`no_route` settle the message. `failed` and `held` mean nothing reached the
agent's terminal, so a retry is clean: the adapter retries the message until
gc takes it. The first four POSTs follow each other within seven seconds.
After that the adapter waits 8, 16 and 32 seconds, then one minute between
POSTs, for up to 24 hours from the time it recorded the message. The usual
cause is text in the agent's input box (`draft_deferred`): gc will not paste
over it, and the message lands once the box clears. `partial` means a fragment
already reached the terminal, and each retry can add another. So once gc has
answered `partial` to any POST of a message, the message is not posted after
its fourth POST, whatever gc answers to its other POSTs; if that answer comes
after the fourth POST, the adapter stops there. The same goes for a message
that gc answers `held` with a `partial` member: when a message goes to several
sessions, gc ranks `held` above `partial`. The ledger keeps that mark
(`fragment`) across a restart. A replay clears it: the replay's POSTs are
counted from one again.

The messages of one space reach gc in the order the adapter recorded them. A
message waits (`behind_earlier`) while an earlier message of its space is
still retrying or not yet posted. A message pending at gc holds nothing back:
the next message is posted at once, so a burst to a busy agent flows. The
cost: if the earlier receipt then concludes that it was not delivered, the
earlier message is retried after the later one reached the agent, as adapter
0.5.0 allows. A message of another space never waits.

When gc's receipt concludes with the status `pending`, the adapter reads its
members. If every member took the whole paste and none is a terminal hold,
the message commits as `landed_unconfirmed`, counted under `turns:<space>`
and `unconfirmed:<space>`. gc vouches that the whole message is in the
session's terminal. It does not prove that the agent read it. A terminal hold
becomes unresolved with the cause `held`; any other concluded pending receipt
stays unresolved as `receipt concluded with pending delivery`.

`status` shows each delivery's age and cause, and for a waiting one its next
attempt and the number of POSTs, plus the waiting and end mail times and any
fragment mark. `stuck` counts deliveries waiting two minutes or more since
they were recorded, or, while pending, since their latest POST if later.
The adapter mails the identity's agent (`[[identity]].agent`,
default `mayor`) about such a message, with the subject `gchat: delivery`.
When several messages of a space wait, only the first of them is mailed: the
others wait behind it, and its mail says how many did when it was sent. The
mail is not sent again when that count changes. A message that waited behind
it is mailed on its own when it becomes the first waiting message itself, or
when it ends unresolved. A message pending at gc holds nothing back and is
mailed on its own. The mail names the space, thread, message, cause and age,
the time the adapter stops retrying, and the commands to read the message and
the status. It says that the message will still be delivered when the
agent's session clears, with the same delivery ID, so an agent that acts on
it from Chat history must not act on it again when it arrives.

A message that becomes unresolved is mailed too, unless its sender withdrew
it. A message is mailed at most twice in each life (its first, and each
replay): once while it waits, and once more if it then ends unresolved for a reason other than the 24-hour bound its first
mail named, a withdrawal by its sender included. That second mail says the
adapter will not deliver it again on its own; the cause and reason say
whether it reached the agent. The ledger keeps both marks (`raised`,
`raised_end`), so a restart sends neither again. A replay clears both marks:
it gets one new waiting mail if still waiting two minutes after the replay
(or its latest pending POST, if later), and one new end mail under the same
rule. A replay that lands sends no mail. Its waiting mail covers the messages
behind it. A mail still in flight when a replay starts marks nothing in the
new life. Each life has its own mail keys at gc, so an earlier mail never
suppresses the replay's mail. A burst released by a stuck head, or after a gc
outage, gets two minutes from each pending POST before a waiting mail is due.
An upgrade does not clear marks on a replay already in flight under an older
adapter: those marks keep their earlier suppression rules. Replays requested
on this version start the new mail allowance.
The mail never carries the message's text, its sender or gc's error text. A
mail that fails is tried again a minute later. The retry state lives in the
ledger: after a restart the next POST still waits for its time, and a POST
that was in flight when the adapter stopped is unresolved as `interrupted
POST; delivery unknown`.

Adapter 0.5.0 could stop between its two saves after a fourth "not
delivered" answer, leaving a message in intent with four POSTs and no cause.
This version never posts such a record again: it ends unresolved as
`delivery retries exhausted` with the cause `unknown`, as 0.5.0 would have
ended it, and is mailed. Going back to adapter 0.5.0 over a ledger this
version wrote turns a waiting message with four or more POSTs unresolved
without a mail, and posts a message that waited behind another at once:
resolve the waiting rows before a roll back. This follow-up adds no ledger
fields. Adapter 0.5.0 still reads it, but drops the retry and mail fields on
save and does not implement the replay mail or pending-age rules.

An unresolved delivery stays in the ledger and is never automatically
reposted. Read its ID, reason and cause in `status`; verify whether the turn
arrived, then use `resolve ID --accept` to treat it as delivered or `--replay`
to post it again. Replay can duplicate a turn whose first outcome is unknown.
A replay is posted at once and takes its place in its space's order by the
time of the replay. Its answer reports `landed`, or `waiting` and why, using
the result of that pass; a message with no gc route reports `not_routed`.
Once the replay is saved, the command succeeds even if its delivery pass
cannot finish: the answer includes `pass_error` alongside the message's
state. Previously that case returned HTTP 502. Scripts should inspect the
answer's `status`, `reason` and optional `pass_error`; command success alone
does not mean delivery. `waiting for an operator` means the adapter has
stopped retrying, including when a withdrawn message only needs accepting.
If a fragment remains in the input box, clear the fragment first, then replay.
Status is authoritative: `receipt lookup failed beyond
deadline; retry pending` is still pending and cannot be resolved. The
terminal reason omits `; retry pending`.

| Cause of an unresolved delivery | What happened | What to do |
| --- | --- | --- |
| `draft_deferred`, `pending_interaction`, `not_delivered` with `not delivered within the retry bound` | gc did not take the message for 24 hours. | Clear what blocks the agent's session, then `--replay`. |
| `not_delivered` (or another waiting cause) with `delivery retries exhausted` | gc answered one of the message's POSTs `partial`, or `held` with a `partial` member, so the adapter stopped at the fourth POST, or at that answer when it came later: a fragment may be in the terminal. | Look at the session; `--accept` if the agent has the message. Otherwise clear any fragment left in the agent's input box, then `--replay`. A replay into a box that still holds the fragment meets `draft_deferred` and waits up to 24 hours. It gets its own waiting mail after two minutes; `gc gchat status` shows it. |
| `behind_earlier` | It waited 24 hours behind an earlier message of its space and was never posted. | Settle the earlier message, then `--replay`. |
| `held` | gc holds a paste it cannot finish without an operator. | Look at the session before anything else; `--accept` if the message is there, else `--replay`. |
| `unknown` | The outcome is not known: a lost POST response, an interrupted POST, a rejected POST, a receipt past its deadline or concluded pending without a whole paste, a status the adapter does not know, no answer from gc at all for 24 hours (`not delivered within the retry bound` while gc was down), or a record adapter 0.5.0 left between two saves (`delivery retries exhausted`; gc's last answer was not recorded). The reason says which. | Check whether the turn arrived; `--accept` if it did, `--replay` if not. |
| `withdrawn` | The sender deleted or edited the message before it was delivered. | Nothing; an edit arrives as its own message. It is mailed only when a waiting mail went out for it, to say it will not be delivered. |
| none | Unresolved before causes were recorded. | Check, then accept or replay; it is not mailed. |

`receipt pending beyond deadline` comes one hour after the latest POST (after
the recorded time, for a record written before the POST time was kept). It is
usually the `--accept` case after verifying the existing turn, rather than a
reason to assume the message was never delivered.

Space-unavailable, parent-unavailable and permanent lookup retentions are
committed records, counted under `unavailable:<space>` and logged at WARN.
Permanent lookup reasons end in `: permanent (HTTP n)`. These records are not
in Unresolved and `resolve --replay` cannot target them today. Own echoes,
local-only records and wake-policy exclusions do not add to that unavailable
counter. `no_route:<space>` means gc accepted the message without routing a
turn; check the default route or binding. Quarantine holds malformed events.

### Catch-up and restart recovery

A first start with no ledger and no established marker begins at the start of
the first successful subscription attempt. Failed setup attempts keep moving
that bound until a subscription is recorded. `first_start_history` can include
up to a week before it. Status names the saved bound.

Each create, edit, deletion or reaction is judged by when that act happened.
An act before its space's bound is never a turn, queued delivery or delivery
mail, on the live stream or during recovery. A new edit or reaction on an old
message still arrives. `gc gchat read` keeps reading the old message's body.
An act with no usable resource or event time uses its Pub/Sub publish time;
with no usable time at all it is skipped, counted as
`history_time_missing:<space>` and logged without message text.

A space first encountered without a cursor or observed join starts at the
previous completed spaces inventory. On a running adapter this includes at
most the preceding hourly sweep. A direct message opened during an outage
keeps messages since the inventory before that outage. On a fresh install's
first inventory the start is the first-start bound, including its configured
window. A known membership still supplies its join bound.

At its first load under this version, a released 0.5.0 ledger establishes an
inventory boundary at load time, with no operator step and no global floor.
Its cursorless spaces start there. A space with a saved cursor stays unchanged
and recovers its whole outage, with the existing overlap and event-retention
limits. An unfinished cursorless interval restarts from its new bound; queued
older acts leave the queue as committed non-turn records. Their count appears
in status as `history_backlog_skipped:<space>`. They remain readable from Chat.
A space never read reports no lost history merely because its start is old.

A rollback keeps the saved cursors and skipped acts as non-turn records. It
drops the new history bounds and inventory boundary, so spaces first found
after rollback return to the released adapter's history behavior. Remove
`first_start_history` from settings before rolling back.

Catch-up runs on startup and subscription recovery, retries unfinished work
with backoff, and stops when caught up. A recovered message that gc keeps
refusing, or that waits behind an earlier one of its space, does not keep
catch-up open: once catch-up handed it over, the ledger holds it and the
poll retries it. A receipt still pending at gc keeps its item open, for at
most an hour from its POST. Live delivery uses Pub/Sub streaming
pull; there is no minute-by-minute Chat polling. Message and event cursors are
separate and advance only when a frozen interval has been handled; live
deliveries never advance them. Backlog drains over repeated budgeted passes.

When Google refuses a space's history, status shows `History held` and its
reason; `unavailable:<space>` and a WARN appear for each new hold, not every
retry. The first hourly inventory refresh at least one hour after the last
denial retries it, as do an observed listing rejoin, service restart, and
subscription recovery. A successful page clears
the hold. Only records after the held page then post; records already enumerated
before the hold remain committed `catchup.unavailable` records and cannot be
replayed by `resolve`.

Google retains space events for 28 days. `unrecoverable:<space>` is reported
once per interval when a known event cursor is older than the local 28-day
boundary; retained records still deliver. A space first discovered during the
outage has no old cursor and reports no such loss. A clamped request omits its
start time and requests Google's retained window. The same omission applies
within five minutes of the boundary to tolerate clock skew, without changing
the persisted bound or the loss-reporting rule. Older unavailable history
cannot be reconstructed by this adapter.

Outbound parts and receipts are persisted before gc recording. Restart recovery
and retries use the saved key to avoid sending the same parts again. Recording
can succeed only for a bound session; no session, another session's binding,
gc downtime, or a transcript write failure is reported separately from delivery.
A later successful record-only retry repeats gc's notification to the
conversation's other sessions: gc deduplicates the transcript, not that
notification.

At startup the adapter registers with gc and re-registers after a refused
connection, gc's no-adapter 422, or its restart-time city-not-running 404.
An already-running registration retry keeps its backoff, up to 60 seconds.
Its registration key is
`gchat-register-<sha256(body)>`. gc caches it per city for 30 minutes. An
identical registration within 30 minutes of a manual `DELETE /extmsg/adapters`
replays cached success without registering again: restart the city or wait
30 minutes after that deletion.

### Ledger recovery

Back up `<city>/.gc/services/gchat/state.json` with its `secrets/.gchat-established`
marker. The ledger owns delivery outcomes, multipart progress and acknowledged
backlog. A corrupt ledger, or a missing ledger after the marker was created,
enters `recovery-required` and preserves the bad bytes. Health, status and state
remain reachable; publishing returns a typed transient failure and new events
are not acknowledged.

The state file is a complete compact JSON document, atomically replaced and
fsynced on every save. Whitespace is insignificant: older indented JSON ledgers
load unchanged, including pending sends, unresolved turns, and backlog. Typed
in-memory clones isolate mutations before saving. Every save still rewrites the
whole ledger; large catch-up runs incur disk and serialization cost. Keep enough
free space for the current file and its replacement, and back up the JSON rather
than depending on its indentation.

Use `gc gchat state restore /absolute/path/backup.json` to request recovery
through the service to keep its saved history bound and recovery cursors, or
explicitly choose `gc gchat state reset`. Reset starts at the current time,
minus the optional `first_start_history` window, and moves that bound to its
first successful subscription attempt. Acts before the bound will not be delivered;
`gc gchat read` still reads them. Use restore to retain recovery of the gap.
These proxy commands are
accepted only in `recovery-required` and refuse while delivery is in flight.
If reconciliation fails, retry the same restore/reset request.

For offline recovery, **stop the service first** (gc users stop the city or
service), set `GC_GCHAT_ADAPTER_ENV` to its env file, then run the adapter binary's
`state restore FILE` or `state reset`. The service holds `<ledger>.lock` and
records its socket there; offline commands take the same lock even when gc uses
a private socket elsewhere. `--force` routes recovery to that running service.
Never delete the lock file. If it was removed or replaced, restart the service:
the old process permanently refuses further saves and requires recovery. An
unheld lock file is stale and does not block recovery.

### Text formatting

This release converts its supported Markdown subset to Chat markup before
splitting messages at 32,000 bytes, preserving code and source whitespace.
The pinned Chat client exposes `Message.MarkupSyntax`, including
`MARKUP_SYNTAX_MARKDOWN`. Switching modes still needs live user-auth checks for
code spans, fences, links, lists and block quotes, and split/rejoin tests for
each. A per-message setting is safer than changing a shared app default across
cities. Keep the Chat markup default for this release; the Markdown mode is not
enabled by the adapter.

The converter is a subset, not a full CommonMark parser. Quoted fences are
ordinary text: markers inside them can be converted, and splitting does not
add quoted fence wrappers. In rare nested emphasis, an inner opener of the
same marker inside a different-marker pair can leave the outer opener literal.
Same-marker nesting also differs from CommonMark: `*(*foo*)*` renders the outer
markers as Chat bold. Backslash escapes pass through unchanged; a backslash
does not protect a following formatting marker from conversion.
Backticks in link destinations can also affect later code/link lookahead.
Use unquoted fenced blocks and simple emphasis when exact presentation matters.

## Troubleshooting

| Output | Action |
| --- | --- |
| `Missing secrets file: ...; required keys: ...` or `secrets ...: missing required keys: ...` | Create the named env file and set every listed key; verify env path precedence. |
| `settings ...: unknown key ...` or `defaults.wake must be all or mentions` | Correct the named setting; invalid reloads keep the previous configuration. |
| `installed (Desktop) client is required` | Download a Desktop OAuth client JSON rather than a Web or service-account credential. |
| `OAuth consent failed: access_denied` / `context canceled` | Repeat auth at the keyboard; failed or canceled consent keeps the old token. |
| `OAuth consent missing scopes: ...` | Grant every named scope on a new consent attempt. |
| `OAuth consent returned no valid id_token sub` | Repeat auth with the requested `openid` scope. |
| `OAuth loopback listener ...` | Check loopback access, free ephemeral ports and local firewall rules; retry. |
| `write token ...` / `replace token ...` | Check directory ownership, permissions and disk space; inspect the named path. |
| `OAuth credential needs sign-in` | Run `gc gchat auth`; inspect the refresh failure reason. |
| `subscription is SUSPENDED; expected ACTIVE` | Inspect status retry cause, API enablement, membership and Pub/Sub IAM; doctor stops until ACTIVE. |
| `state restore/reset requires recovery-required health` | The live service is not in recovery mode; do not overwrite its ledger offline while it runs. |
| `reply-current requires --to MESSAGE` | Use the Message id line or the `gchat:` item in the first line of the turn being answered. |
| `delivery failed` | Inspect the JSON reason and retry hint; retry the same send with the printed key. |
| `Delivered, but not recorded in gc` | Inspect binding/session and gc health; retry recording with the same key. |

Validate source changes with `gc lint packs/gchat`, shellcheck and `bash -n`
on every shell script, and the Python wrapper tests in `tests/`. From `adapter/`,
run `gofmt -l .`, `go vet ./...`, and `staticcheck ./...`.
Use `go test ./... -count=1 -timeout=30m` for the full suite and
`go test -race ./... -count=1 -timeout=45m` for a full race run. The 3,000-message
recovery test performs real durable writes and needs that explicit budget.
The integration fixtures use in-process fakes. `GC_BIN=/absolute/path/gc
go test ./test/contract -count=1 -timeout=2m` runs the real-gc contract in an
isolated temporary city on an ephemeral port, including a failed delivery's
retry hint. The real `gc init` requires `lsof` on `PATH`. It never needs a
running city's API.
