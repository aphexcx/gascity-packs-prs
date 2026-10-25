# gc-slack-adapter setup

End-to-end walkthrough to get a Gas City session talking to a Slack
workspace via DMs (or rooms) — outbound posts from the session land in
Slack, inbound human replies route back to the bound session.
Estimated time: ~20 min of clicking + a few one-line commands.

## Architecture: two listeners, only one Funneled

The adapter binds **two separate ports** by design:

- **`:8765` — public listener** — serves only `/slack/events` (HMAC-verified)
  and `/healthz`. This is the one Tailscale Funnel exposes to the public
  internet.
- **`127.0.0.1:8766` — internal listener** — serves only `/publish`.
  Bound to localhost so it is **physically unreachable from outside the
  machine**. gc reaches it via the loopback interface.

Why this split: gc and Slack are different trust zones. The Slack endpoint
is authenticated by signing secret. The publish endpoint is authenticated
by network locality (only local processes can reach localhost). If both
endpoints lived on the public port, anyone on the internet who guessed
the URL could POST publish requests and make your bot say arbitrary things.

> **Port-collision override.** If `:8765` or `:8766` is already in use
> on your host, point the adapter at free ports via env vars:
>
>   - `LISTEN_PUBLIC=:8775`              # Funnel forwards to this
>   - `LISTEN_INTERNAL=127.0.0.1:8776`
>   - `INTERNAL_CALLBACK_URL=http://127.0.0.1:8776`
>
> Wherever this guide references `8765`/`8766`, substitute the
> overridden values. The defaults work unchanged on hosts where the
> documented ports are free.

## Step 1 — Tailscale Funnel public URL

You need a stable public HTTPS URL Slack can POST to. Tailscale Funnel
gives you one for free.

```bash
# Confirm Funnel is enabled for your tailnet
tailscale funnel status

# Expose ONLY the public listener port (:8765) — NOT 8766
tailscale funnel --bg --https=443 8765
```

Tailscale prints the public URL — something like
`https://<machine>.<tailnet>.ts.net`. **Copy this URL — you need it for
the Slack app's Event Subscriptions Request URL.**

Verify isolation: the publish endpoint is NOT exposed:

```bash
# This should return 404 (publish is not on the public listener):
curl -s -o /dev/null -w "%{http_code}\n" \
  -X POST https://<your-tailnet>.ts.net/publish

# This should return ok (proves the public listener works):
curl -s https://<your-tailnet>.ts.net/healthz
```

## Step 2 — Create the Slack app

1. Go to https://api.slack.com/apps → **Create New App** → **From scratch**.
2. Name it `gc-oversight` (or whatever). Pick your personal workspace.
3. In the left sidebar:

   **OAuth & Permissions** →
   - Bot Token Scopes — add:
     - `chat:write` (post messages)
     - `im:history` (read DM history for inbound replies **and** to verify
       outbound ones — see below)
     - `im:read` (open DM channel)
     - `users:read` (resolve display names — optional but useful)

     A history scope is required for every conversation kind you bind, not
     just DMs: `/publish` confirms each post by reading it back, so a token
     without history for a bound channel reports every publish as
     `Delivered:false` / `readback_auth` even though the message is visible in
     Slack. Add `channels:history` (public channels), `groups:history`
     (private channels) and `mpim:history` (group DMs) when you bind rooms.
     `slack-full/schema/apps.schema.json` is the authoritative list for
     manifest-driven installs.
   - Click **Install to Workspace** at the top, approve.
   - Copy the **Bot User OAuth Token** (`xoxb-...`) — you need it.

   **Basic Information** →
   - Scroll to **App Credentials**.
   - Copy the **Signing Secret** — you need it.
   - Note the **Team ID** under App Credentials (looks like
     `T0XXXXXXXXX`) — that's your `SLACK_WORKSPACE_ID`.

   **Event Subscriptions** →
   - Toggle **Enable Events** → ON.
   - Request URL: paste
     `https://<your-tailnet>.ts.net/slack/events`
     (Slack will verify it; the adapter handles the challenge
     automatically. The adapter must be running for this to succeed —
     start it after Step 4 and come back here.)
   - **Subscribe to bot events** — add:
     - `message.im` (DMs to your bot)
     - `reaction_added` and `reaction_removed` (human-reaction
       visibility, gp-by3 — requires the `reactions:read` scope; see
       `manifest/README.md` for the full production event list)
   - Save changes.

   **App Home** →
   - Show Tabs → enable **Messages Tab**.
   - **Allow users to send Slash commands and messages from the messages
     tab** → ON.

### Optional: Socket Mode transport (no public URL)

Instead of the Funnel-fronted Request URL, the adapter can receive
events over an outbound WebSocket (Slack **Socket Mode**). This removes
the public-HTTP single point of failure entirely — recommended for
production single-workspace deployments.

1. **Basic Information** → **App-Level Tokens** → **Generate Token and
   Scopes**. Name it (e.g. `socket-mode`), add scope
   `connections:write`, generate, and copy the `xapp-…` token.
2. Set `SLACK_APP_TOKEN=xapp-…` in the adapter's env (same place as
   `SLACK_BOT_TOKEN`). With the default `SLACK_SOCKET_MODE=auto` the
   adapter dials Slack on startup; look for `socket mode: connected` in
   the log and `socket_mode=connected` on `/healthz`.
3. **Cutover**: in the app config, **Socket Mode** → toggle **Enable
   Socket Mode** ON. This atomically stops Request-URL delivery and
   routes events down the socket. Keep the Event Subscriptions URL
   configured — rollback is toggling Socket Mode back OFF (and/or
   `SLACK_SOCKET_MODE=off` on the adapter).

Order matters: start the adapter with the token BEFORE flipping the
toggle, so there is no window where neither transport is listening.
The inbound-liveness watchdog (`SLACK_LIVENESS_*` in the README env
table) alarms if inbound goes silent while channel history keeps
growing — on either transport.

## Step 3 — Find your DM channel ID with the bot

After installing the app, open Slack, click on the app's name in the
sidebar to start a DM conversation. Send any message to it (e.g.
"hello"). Then in your terminal:

```bash
curl -sS -H "Authorization: Bearer xoxb-YOUR-TOKEN" \
  "https://slack.com/api/conversations.list?types=im&limit=200" \
  | jq -r '.channels[] | [.id, .user] | @tsv'
```

This lists DM channel IDs and the user IDs they're with. Find the one
where the user is your own user ID (you can find it via:
`curl -sS -H "Authorization: Bearer xoxb-YOUR-TOKEN" https://slack.com/api/auth.test | jq`).

**Copy the DM channel ID** (looks like `D0XXXXXXXXX`) — you need it
for the bind step below.

## Step 4 — Configure and start the adapter

Create the env file (`$XDG_CONFIG_HOME` defaults to `$HOME/.config`):

```bash
mkdir -p "${XDG_CONFIG_HOME:-$HOME/.config}/gc-slack-adapter"
cat > "${XDG_CONFIG_HOME:-$HOME/.config}/gc-slack-adapter/env" <<'EOF'
SLACK_WORKSPACE_ID=T0XXXXXXXXX
SLACK_BOT_TOKEN=xoxb-YOUR-BOT-TOKEN-HERE
SLACK_SIGNING_SECRET=YOUR-SIGNING-SECRET-HERE
GC_CITY_NAME=<your-city-name>

# Defaults shown — only override if you have a port conflict.
# LISTEN_PUBLIC=:8765                      # Funnel exposes this
# LISTEN_INTERNAL=127.0.0.1:8766           # gc-only, localhost
# INTERNAL_CALLBACK_URL=http://127.0.0.1:8766

# Optional override; defaults to http://127.0.0.1:9443 (no /v0/... suffix).
# Under proxy_process supervision you don't set this — but you do need
# it sourced before `gc start` so the supervisor inherits it for the
# spawned adapter (it is NOT auto-injected by the controller).
# GC_API_BASE_URL=http://127.0.0.1:8372

# Bound goroutine fan-out on inbound dispatch paths (slash-command,
# slack-event, alias-resolved). Each in-flight dispatch holds an
# http.Client with a 10s timeout, so unbounded fan-out scales memory
# and FD pressure with traffic. Default 50; raise for high-traffic
# workspaces, lower to harden against bursts. Must be a positive
# integer; 0/negative/non-numeric values fail at startup. sec-S-04.
# SLACK_DISPATCH_CONCURRENCY=50
EOF
chmod 600 "${XDG_CONFIG_HOME:-$HOME/.config}/gc-slack-adapter/env"
```

Note: `PUBLIC_URL` is no longer needed by the adapter itself (we register
with the internal callback URL). You only need the public URL to plug
into the Slack app's Event Subscriptions config.

Run the adapter (replace `<gascity-repo>` with your local checkout
path):

```bash
cd <gascity-packs-repo>/slack-full/adapter
./run.sh
```

You should see:

```
starting gc-slack-adapter public=:8765 internal=127.0.0.1:8766 gc=http://127.0.0.1:8372 city=<your-city-name>
registered with gc as provider=slack account=T0XXXXXXXXX callback=http://127.0.0.1:8766/publish (LOCALHOST ONLY)
public listener serving on :8765 (Slack events)
internal listener serving on 127.0.0.1:8766 (gc publish only)
```

If registration fails, check that `gc supervisor` is running and the
city name matches.

Now go back to **Step 2 → Event Subscriptions** and click **Verify** on
the Request URL — Slack will POST a challenge, the adapter will respond,
and Slack should show ✓ Verified.

## Step 5 — Bind a session to your Slack DM

The session that should receive Slack DMs needs to be created and
bound to the DM channel ID from Step 3. Names are placeholders below;
substitute the names your consumer pack actually uses (the
`oversight-rig` example pack uses `oversight-rig.chief-of-staff`
under the alias `cos`).

```bash
CITY_DIR=<your-city-dir>     # e.g. ~/gas-city
CITY_NAME=<your-city-name>   # the directory's basename, by default

# Create the session
gc --city "$CITY_DIR" session new <pack>.<role> \
  --no-attach --alias <short-alias> --title "<role> (slack)"
# Output: Session gc-XXXXX created

# Capture the session ID
SID=$(gc --city "$CITY_DIR" session list --json \
  | jq -r '.[] | select(.alias == "<short-alias>") | .id')
echo "Session: $SID"

# Bind to Slack DM
curl -sS -X POST "${GC_API_BASE_URL:-http://127.0.0.1:8372}/v0/city/${CITY_NAME}/extmsg/bind" \
  -H 'Content-Type: application/json' \
  -d "$(jq -n \
    --arg sid "$SID" \
    --arg acct "$SLACK_WORKSPACE_ID" \
    --arg chan "D0XXXXXXXXX" \
    '{session_id: $sid, conversation: {provider: "slack", account_id: $acct, id: $chan}}')"
```

Replace `D0XXXXXXXXX` with your DM channel ID from Step 3. For
multi-session rooms with peer fanout, see `gc slack bind-room` in the
slack-full README instead.

## Step 6 — Wire env vars for any consumer pack scripts

If your consumer pack has scripts that need to know the bound session
id and the API endpoint, export the relevant vars wherever the gc
supervisor inherits its env from (`~/.profile`, `~/.bashrc`, or a
systemd `EnvironmentFile=`):

```bash
export GC_API_BASE_URL=http://127.0.0.1:8372
export GC_CITY_NAME=<your-city-name>
# Consumer-pack-specific session id captured in Step 5:
export GC_OVERSIGHT_SESSION_ID="$SID"
export GC_PACK_DIR=<gascity-repo>/examples/<your-consumer-pack>
```

Restart the supervisor so the new env propagates.

## Step 7 — Test end-to-end

From inside any session whose DM is bound, post and watch for the
roundtrip:

```bash
gc session attach $SID
# Inside the session:
gc slack reply-current --body 'hello from <pack>.<role>'
```

You should see:
- The adapter logs a `publish:` line
- A Slack DM appears in your channel (under the session's identity if
  one is registered via `gc slack identity`, otherwise the default bot
  identity)

Reply to the Slack message with "ack". You should see:
- Slack POSTs to `/slack/events`
- The adapter logs an `inbound:` line
- The bound session receives the message (visible via
  `gc session peek $SID`)

## Running the adapter as a service

For always-on, install as a systemd user service (replace
`<gascity-repo>` with your checkout path):

```bash
mkdir -p ~/.config/systemd/user
cat > ~/.config/systemd/user/gc-slack-adapter.service <<EOF
[Unit]
Description=gc Slack adapter
After=network-online.target

[Service]
Type=simple
EnvironmentFile=-${XDG_CONFIG_HOME:-$HOME/.config}/gc-slack-adapter/env
ExecStart=<gascity-packs-repo>/slack-full/adapter/run.sh
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
EOF
systemctl --user daemon-reload
systemctl --user enable --now gc-slack-adapter.service
journalctl --user -u gc-slack-adapter -f
```

If you'd rather have the gc supervisor manage the adapter (Phase A
`proxy_process`), follow the cutover sequence in the slack-full README
instead — that path eliminates the standalone systemd unit and lets
gc reverse-proxy `/publish` over a UDS while the public Slack
endpoint stays bound to TCP `:8765`.

## Peer posts

An allowlisted peer's post is its own inbound on arrival in every channel
and thread, waking the bound session in both full and minimal mode. Its
registry label is the sender name. It uses the human reminder shape, whole
text, thread anchor and reply coordinates, without adapter instructions about
whether to respond. Each city's agent prompt owns that decision.

`peer_bots.json` (default `<GC_CITY_PATH>/.gc/slack/peer_bots.json`, override
with `SLACK_PEER_BOTS_PATH`) supplies the allowlist and friendly labels:

```json
{
  "peers": [
    {"label": "citadel", "bot_id": "B0XXXXXXXXX", "wake": true},
    {"label": "sinan", "app_id": "A0XXXXXXXXX"},
    {"label": "boomtown", "bot_user_id": "U0XXXXXXXXX"}
  ]
}
```

- Each entry needs a `label` and at least one of `app_id`, `bot_id`, or
  `bot_user_id`. Unlisted bots do not originate inbounds. This removes the
  previous unlisted-bot ambient buffer; it does not change registry entries.
- `wake` and `immediate_channels` still parse for configuration compatibility.
  They have no effect: every channel is immediate. Startup logs that change
  once. Keeping the keys avoids breaking existing operator files on upgrade.
- Failed forwards alone enter an in-memory FIFO. Later human, batch or peer
  deliveries carry whole queued posts when they fit the reminder budget.
  `[N omitted — awaiting delivery]` reports the remaining queue in both modes.
  There is no per-item clip or 20-item eviction. An oversized head rides whole
  on the next delivery, over budget, with a log naming its channel, byte size
  and budget. Later entries retain FIFO order and ride subsequent deliveries.
  Restart loses this in-memory queue, and sustained failures can grow
  it. The count and read pointer make pending context visible in the meantime.
  Attachments and trusted local paths survive recovery. A payload refusal
  defers the riders and retries the new message alone; rejected attachments
  become a visible withholding notice with their saved paths. A held delivery
  that is ultimately lost retains all included peers in its dead letter.
- Peer text does not grant alias routing or busy reactions. Automatic
  reply-current/react/upload anchor selection includes peer inbounds by the
  same latest-message rule as human inbounds. Event and transcript selection
  agree on the author; a peer never inherits an older human's unrelated thread.
  Explicit reply coordinates still select the exact post being answered.
- Loop safety: every candidate is resolved through `bots.info` (per-
  bot TTL cache) and the adapter never delivers its own posts back to
  itself, even if misconfigured into the allowlist. An author that
  cannot be definitively resolved is dropped, not delivered.
- Same reload contract as the other registries: edit the file, then
  SIGHUP or restart the adapter.
- Unlisted or contradicted bot identities log one info line per bot id per
  hour, including channel, timestamp and bot/app/user ids. Successful peer
  deliveries log the registry label. See the [upgrade note](../CHANGELOG.md#upgrade).

Full thread context quotes humans, own-bot posts, peers and other bots under
one rule: chronological order, whole collapsed text and the same delivered
watermark/count. Context fits whole quotes newest-first, displaying the retained
suffix chronologically. One `[N omitted — thread context over budget]` counts
all older omitted lines, including already-delivered and incomplete-context
notices. Quotes take priority over those notices. An oversized newest quote
is cut at a UTF-8 boundary with `… [truncated]` and counts as fitted.

`SLACK_REMINDER_TEXT_BUDGET` (default 3,500 bytes; 0 disables) subtracts the
protected anchor, new body and files to determine available context space.
When that protected unit alone exhausts the budget, context instead receives
`SLACK_REMINDER_CONTEXT_FLOOR` (default 1,200 bytes; positive integer). Small
context therefore rides whole under an oversized new post. The new message
and attachments always stay whole. A pathologically small positive budget
can be exceeded by the mandatory truncation marker and omission count.
Help still yields first; multi-message full-mode batches retain their verbatim
contract. A partial context window is journaled as unknown because the rendered
quote text does not supply exact timestamps for the selected suffix.

`gc slack read` returns every author in channel and thread mode. Both keep
the newest N messages (`--limit`, default 50), displayed chronologically.
Thread reads follow cursors to the end while retaining only N messages in
memory; a long thread can require multiple Slack API requests. `has_more`
also reports older matching messages omitted by this newest-N window.

## Optional — inbound token efficiency (gp-729, gp-9e7)

The adapter coalesces bursts of untargeted channel chatter into one
delivered inbound per debounce window (default 8s), registers a
one-line reply-instruction template with gc (the full how-to arrives
once per channel per adapter lifetime), collapses already-delivered
thread priors in context preambles, suppresses the duplicate direct
copy for alias targets that are already channel-bound, and renders
`#name (Cid)` in pack-owned wrapper blocks (needs the `channels:read`
bot scope; falls back to the raw id without it).

On top of that (gp-9e7):

- A firing flush timer sweeps every other buffered non-digest channel,
  so one idle wake delivers ALL channels' window traffic as aligned
  per-channel batches (digest channels keep their own interval).
- Reaction notifications (`reaction_added`/`reaction_removed`,
  gp-by3) never wake a session solo: they buffer no-wake and ride the
  channel's next real delivery. A reaction on one of the adapter's own
  outbound posts may join an already-armed coalesce window.
- The send commands (`reply-current`, `publish`, `publish-to-channel`,
  `upload`) print a terse receipt by default — delivered flag,
  message/file id, conversation id, thread ts — instead of echoing the
  full result envelope (and your own outbound text) back into the
  sending session's context. `--verbose` restores the full envelope.

Knobs:

- `SLACK_INJECTION_MODE` — `full` (default) preserves the current reminder
  format; `minimal` keeps the channel name/id, sender name/id, message ts,
  thread anchor, new text, attachment descriptions, and one `Context: gc slack
  read --conversation-id <C> --thread-ts <root>` pointer per message. It
  registers `reply-to <root-or-message-ts>` automatically, overriding
  `SLACK_REPLY_INSTRUCTIONS`. Thread re-quotes and reply how-to blocks are
  omitted. Every allowlisted peer post arrives independently with its whole
  text, exactly like a human post. The first failed forward rides a later
  delivery whole even over budget; the remaining count is explicit and
  deferred recovery is retained.
  Thread context is not fetched in minimal mode. The parent author/first line
  comes from a bounded cache of observed roots; a cold cache keeps the thread
  id alone, and edits/deletions invalidate cached excerpts. A reaction with
  no known thread root points to the channel. Mention-only routing may still
  scan a historical thread whose
  ownership predates the adapter's own-thread registry; this is routing, not
  context injection.
  **Before opting in**, put both contracts in every receiving agent's prompt:
  "For Slack context, run the reminder's `gc slack read` pointer. To answer, write
  a file and run `gc slack reply-current --conversation-id <C> --reply-to
  <reply-to ts> --body-file <file>`." Mention-only recipients also need their
  existing `publish-to-channel --conversation-id <C> --thread-ts <ts>
  --body-file <file>` contract. Takes effect at the next adapter start;
  unknown values fail startup. Restore `full` and restart to roll back.
- `SLACK_INBOUND_JOURNAL_RETENTION_DAYS` — `1..365`, default `7`. The adapter
  appends text-free delivery metadata under
  `<GC_CITY_PATH>/.gc/slack/inbound-journal/YYYYMMDD.jsonl`: message/channel/
  thread ids, target handle and receipt-provided sessions, adapter Text bytes,
  mode, quoted-window endpoints, collapsed count and watermark. Files rotate
  at 16 MiB into numbered day segments; all segments expire by UTC day.
  One row records each accepted forward; a held receipt's conclusion adds a
  `concluded` row with the same receipt id. Reposts are distinct attempts.
  `accepted_unconfirmed` and missing session ids mean gc supplied no evidence
  of the last hop, not confirmed delivery. Journal errors are logged without
  retrying a message that gc already accepted. The journal contains no message
  text; Slack reads recover content subject to Slack retention/access.
- `SLACK_REPLY_INSTRUCTIONS` — the per-message reply line gc renders into
  every inbound reminder (jg-fc1csv). `full` (default) registers
  `Reply: gc slack reply-current --conversation-id <C> --reply-to <ts>
  --body-file <file>`; `compact` registers `reply-to <ts>` (~22 fewer
  tokens per message) and leaves the command shape to the once-per-channel
  how-to and the agent's prompt. The how-to arrives once per channel per
  ADAPTER lifetime, so a restarted or compacted agent session may not have
  it: before setting `compact`, add a standing note to every bound agent's
  prompt, e.g. "Slack reminders end with `reply-to <ts>`. To answer one,
  write the reply to a file and run `gc slack reply-current
  --conversation-id <the C… id in the reminder's slack/<id> header>
  --reply-to <ts> --body-file <file>`." Takes effect at the next adapter
  start (the template is registered once); unknown values fail startup.
- `SLACK_COALESCE_WINDOW` — burst debounce for untargeted inbounds
  (Go duration, default `8s`; `0` restores per-message immediate
  forwarding). Targeted / bot-mentioned messages always deliver
  immediately and flush any pending buffer ahead of themselves.
- `SLACK_INBOUND_DEAD_LETTER_DIR` — where a coalesced inbound that gc
  rejects as unacceptable payload (HTTP 400/413/415/422, e.g. a 422
  validation error) is written after 3 rejected deliveries instead of
  blocking the channel forever (gp-xnc). One JSONL file per channel
  (`<channel>.jsonl`, 0700 dir / 0600 file, fsynced); each line carries
  the full inbound envelope, the attempt count, and the rejection
  reason, for a manual re-post once the cause is fixed. Default
  `<GC_CITY_PATH>/.gc/slack/inbound_dead_letter`. A rejected
  multi-message batch is first isolated (every member re-posted alone)
  so only the poisoned message is charged; a write that cannot be
  confirmed keeps the entry buffered and retries next window. Every
  other failure — gc unreachable, 5xx, 408/429, and operational 4xx
  such as 401/403/404 (wrong city name, route missing mid-rollout) —
  keeps retrying every window as before and never dead-letters.
- `delivery_policy.json` (default
  `<GC_CITY_PATH>/.gc/slack/delivery_policy.json`, override with
  `SLACK_DELIVERY_POLICY_PATH`) — per-channel delivery pacing:

```json
{
  "channels": {
    "C0XXXXXXXXX": {"mode": "digest", "interval_minutes": 10},
    "C0YYYYYYYYY": {"mode": "immediate"}
  }
}
```

  A `digest` channel accumulates and delivers verbatim in one batch
  every `interval_minutes` (1–120) — nothing is dropped or summarized.
  Every unlisted channel (and an absent file) is `immediate`: only the
  short burst debounce applies. Same SIGHUP-or-restart reload contract
  as the other registries.

## Troubleshooting

- **Adapter starts but Slack URL verify fails**: confirm Tailscale Funnel
  is up (`tailscale funnel status`), check `curl https://<your-url>/healthz`
  returns ok, and confirm the Slack app's Request URL exactly matches
  `https://<your-url>/slack/events` (note the path).
- **"register adapter" fails on startup**: gc supervisor needs to be
  running; verify `gc cities` lists your city name.
- **Outbound publish errors**: check `gc supervisor logs` for `publish:`
  failures. If you see `channel_not_found` the bot isn't a member of the
  channel — for DMs this shouldn't happen since you DM'd it; for a
  channel, invite the bot. If you see `missing_scope` from `files.*`,
  the bot lacks `files:write` (outbound) or `files:read` (inbound).
- **Inbound replies don't reach the bound session**: check the signing
  secret is correct; check the `message.im` event subscription is active
  in the Slack app config; confirm the session is bound (look up
  `extmsg/bindings` via `gc slack status --session <SID>`).
- **`coalesced inbound POST failed: … 422 … validation-failed` in the
  service log**: gc rejected an inbound payload (historically a Slack
  voice clip with no `mimetype`, gp-xnc). The message is isolated from
  its batch-mates, retried 3 times, then written to
  `<SLACK_INBOUND_DEAD_LETTER_DIR>/<channel>.jsonl` with the full
  envelope and the rejection reason — grep the log for `dead-lettered`
  for the summary line, and re-post from the file once the cause is
  fixed. Later messages in the channel keep flowing.
- **`receipt follow-up: LOSS …` or `receipt follow-up: STUCK …` in the
  service log** (gp-3yg): gc answered `pending` for a delivery (the
  session was busy; the adapter held rather than duplicate) and the
  follow-up poll of `GET /extmsg/inbound/receipts/{id}` then learned
  the background send never landed (`LOST`, re-posted once; `LOSS` when
  the recovery copy was not vouched for either — reaching nobody or
  being accepted without a receipt counts as not vouched) or never
  concluded
  (`STUCK` — not re-posted, because a slow send is not a lost one).
  Both write the envelope to the dead-letter file with a reason that
  says which. The `LOSS` log line itself is the durable-record alarm —
  one per record written (or per failed write) — not a loss verdict:
  read the record's reason. A "lost" reason means gc said the delivery
  never landed and no recovery copy was vouched for or positively
  landed: re-post from the file. An "unknown, verify by hand" reason
  (`STUCK`) means nobody can say: check the session's transcript
  before re-posting. `RESOLVED` and `RECOVERED` lines need
  no action (one may say the payload "landed whole; only the submit
  went unconfirmed" — the gp-2io busy-probe shape: gc's per-member
  byte counts prove the message is in the pane; gc simply could not
  watch the turn start, and neither a re-post nor a dead letter would
  be right). `UNVERIFIED` means gc never offered a usable receipt status
  for the whole follow-up window (10 min) — the running gc predates the
  receipt endpoint — and the hold stands unverified, as before gp-3yg;
  a transient 404/5xx (gc restarting, a city's server being replaced)
  is retried, not taken as the answer, and a hold gc did answer for
  before going silent is recorded as `STUCK` at the deadline rather
  than failed open. A lost address-by-handle
  injection is recorded with `alias_session_id`, `alias_handle` and
  the exact `alias_body`: re-inject that body into that session by
  hand, not the channel envelope.
