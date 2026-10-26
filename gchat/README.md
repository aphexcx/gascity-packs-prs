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
```

All keys except `GCHAT_TOKEN_FILE` are required. The token defaults to
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
```

Configure one identity. Its name is gc's `account_id`; omit `token_file` to use
`GCHAT_TOKEN_FILE`. If the identity section is absent, the account ID defaults
to `default` and the agent to `mayor`. Settings reload on change after validation.
Media defaults to the city paths shown above; uploads must be inside
`upload_root`, without symlinks, and at most 200 MB. Inbound attachment text
includes its local path and MIME type, or an unavailable reason; Drive
attachments remain links. The root-level `catchup_budget` setting defaults to
50 records per space per pass; it is a budget, not a truncation limit.

Optional endpoint/path overrides for tests and private deployments:
`GCHAT_CHAT_API_BASE`, `GCHAT_EVENTS_API_BASE`, `GCHAT_PUBSUB_ENDPOINT`,
`GCHAT_OAUTH_AUTH_URL`, `GCHAT_OAUTH_TOKEN_URL`, `GCHAT_MEDIA_DIR`,
`GCHAT_UPLOAD_ROOT`, `GCHAT_STATE_FILE`. Export overrides in the service process;
OAuth URLs can also be read from the env file by standalone auth. The Pub/Sub
endpoint override uses an insecure connection intended for an in-memory fake.
A relative `GCHAT_STATE_FILE` resolves beside the env file, consistently for
the service and offline recovery commands.

## Commands

| Command | Purpose |
| --- | --- |
| `gc gchat auth [--check]` | Sign in, or force a token refresh and print readiness and `identity_user_id`. |
| `gc gchat publish --conversation-id spaces/AAAA --body-file reply.txt` | Send text, with optional `--thread`, `--session`, and `--idempotency-key`. |
| `gc gchat reply-current --body-file reply.txt [--thread spaces/AAAA/threads/ID]` | Resolve the session's latest inbound conversation, then its latest active binding; `--conversation-id` overrides lookup. |
| `gc gchat upload --conversation-id spaces/AAAA --file /absolute/path/file.txt` | Upload within the configured root; optional text, thread, session and idempotency key. |
| `gc gchat react --message spaces/AAAA/messages/ID --emoji '👍' [--remove]` | Add or remove the identity's reaction. |
| `gc gchat read --space spaces/AAAA [--thread NAME] [--since RFC3339] [--limit 100]` | Read normalized history, newest last; limit 1–1000. |
| `gc gchat spaces [--space spaces/AAAA --wake all\|mentions\|default]` | List spaces, or persist a wake override; `default` clears it. |
| `gc gchat status [--json]` | Show health, subscription, last event, backlog, counters and delivery IDs. |
| `gc gchat resolve ID --accept\|--replay` | Settle an unresolved delivery. |
| `gc gchat state restore FILE\|reset` | Request recovery through the running service's `/state` endpoint. |

Text commands accept `--body TEXT` or `--body-file PATH`. File contents retain
trailing newlines and stream through the wrapper without becoming process
arguments. The service accepts at most 4 MiB of JSON per command; escaping adds
to the body size. `reply-current` uses `--session`, then `GC_SESSION_ID`, then
a `GC_SESSION_NAME` lookup. Its inbound search uses the last two hours, at most
500 events, and only the gchat provider; pass the target explicitly if older
or outside that window. Pass `--thread` from the inbound turn to preserve its
thread; conversation lookup alone does not infer the thread.

Publish, reply and upload print an idempotency key to stderr **before** sending.
Keep that key and pass `--idempotency-key KEY` on every retry of the same send,
including after a timeout. A new invocation without a key creates a new send.
JSON output reports `delivered` and `recorded` separately. Delivery failure
exits nonzero; delivery without transcript recording exits zero with a warning.
Inspect `reason`, `failure_kind` and `retry_after` (nanoseconds) before retrying.

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
still in flight; each delivery's status is shown separately.

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

### Delivery uncertainty

An unresolved delivery stays in the ledger and is never automatically reposted.
Read its ID and reason in `status`; verify whether the turn arrived, then use
`resolve ID --accept` to treat it as delivered or `--replay` to post it again.
Replay can duplicate a turn whose first outcome is unknown. Status is
authoritative: `receipt lookup failed beyond deadline; retry pending` is still
pending and cannot be resolved. The terminal reason omits `; retry pending`.

`receipt pending beyond deadline` after a long outage follows one receipt
lookup, with the deadline still measured from the original intent's creation.
It is usually the `--accept` case after verifying the existing turn, rather
than a reason to assume the message was never delivered.

Space-unavailable, parent-unavailable and permanent lookup retentions are
committed records, counted under `unavailable:<space>` and logged at WARN.
Permanent lookup reasons end in `: permanent (HTTP n)`. These records are not
in Unresolved and `resolve --replay` cannot target them today. Own echoes,
local-only records and wake-policy exclusions do not add to that unavailable
counter. `no_route:<space>` means gc accepted the message without routing a
turn; check the default route or binding. Quarantine holds malformed events.

### Catch-up and restart recovery

Catch-up runs on startup and subscription recovery, retries unfinished work
with backoff, and stops when caught up. Live delivery uses Pub/Sub streaming
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
through the service, or explicitly choose `gc gchat state reset`, which warns
that messages from the gap may be lost or duplicated. These proxy commands are
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
| `no inbound event and no binding found` | Pass `--conversation-id` and the turn's `--thread`, or use the correct session. |
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
