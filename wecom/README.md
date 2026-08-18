# wecom — WeCom (企业微信) AI Bot pack for Gas City

Bridges a WeCom Smart Robot (智能机器人, API mode / **Long Connection**) to a
gc city, so the China team can message the mayor from WeCom/WeChat and get
replies in place. Built for the jadegate (Shanghai) city per gp-86d; mirrors
the slack-full pack architecture: adapter + `pack.toml` service registration +
env contract + default route to the mayor.

The defining property vs. the Slack packs: **no public endpoint anywhere**.
The WeCom AI Bot long-connection protocol runs over an *outbound* WebSocket
to `wss://openws.work.weixin.qq.com`, so the adapter runs happily on a
laptop behind NAT inside the mainland — no domain, no trusted-IP callback,
no funnel.

## Components

- `adapter/` — Node.js bridge on Tencent's official
  [`@wecom/aibot-node-sdk`](https://www.npmjs.com/package/@wecom/aibot-node-sdk)
  (auth, heartbeat, reconnect, reply queues stay upstream-maintained).
  - Inbound: text / voice / mixed frames → `POST /v0/city/{city}/extmsg/inbound`
    addressed to the mayor. WeCom transcribes voice server-side, so voice
    arrives as `[voice] <transcript>`. Image/file/video are placeholders in
    phase 1 (their download URLs are AES-encrypted and expire in 5 minutes;
    attachment surfacing is a follow-up).
  - Outbound: `/publish` + `/healthz` on `$GC_SERVICE_SOCKET` (UDS). gc
    appends `/publish` to the registered callback URL when delivering a
    session's reply; the adapter forwards it as a WeCom markdown message —
    to the `chatid` for group chats, to the peer `userid` for DMs — chunked
    at 3800 chars.
- `commands/publish.sh` — `gc wecom publish`: manual/operator sends through
  the running adapter via gc's `/svc/wecom` reverse proxy. Also the verb
  gc's inbound nudges cite (registered as the adapter's
  `reply_instructions`), so the mayor's reply flow works without a
  `reply-current` verb.
- `city-fragment.toml` — the `[[extmsg.default_route]]` routing unbound
  wecom conversations to the `mayor` agent; added to city.toml's `include`
  at setup (pack.toml has no extmsg surface). Without it gc acks inbound
  POSTs and then drops them as unrouted.

## Secrets

`~/.config/gc-wecom-adapter/env` (never in the repo):

```
WECOM_BOT_ID=...
WECOM_BOT_SECRET=...
GC_CITY_NAME=...
GC_API_BASE_URL=http://127.0.0.1:8372
```

`GC_CITY_NAME` and `GC_API_BASE_URL` are both required — the controller
injects the service socket and URL prefix but neither of these (check the
city's actual API port with `gc status`).

## Setup

1. Write the env file above.
2. Add the default route to the CITY config — not optional: without it gc
   acks inbound messages and then drops them as unrouted (pack.toml cannot
   carry it; the pack parser rejects extmsg keys). Either paste this block
   into city.toml directly, or copy `city-fragment.toml` from this pack
   NEXT TO city.toml and list it in `include` (include paths resolve
   relative to city.toml's directory — for remote pack imports the cached
   pack checkout is not at a resolvable relative path):

   ```toml
   [[extmsg.default_route]]
   provider = "wecom"
   agent = "mayor"
   ```
3. Pre-warm dependencies so the first supervised start never pays
   npm-install latency inside the supervisor's readiness window:

   ```
   (cd wecom/adapter && ./run.sh --deps-only)
   ```

Both come from the WeCom console when creating the robot: **Workspace →
Smart Robot → Create Robot → Manual → API Mode → connection method "Use
Long Connection"** — the page then displays Bot ID and Secret. The admin
must have long-connection permission enabled (Admin Console → Security &
Management → Management Tools → Smart Robot → API Mode Management), and the
robot's visible scope decides who can talk to it.

## Known protocol constraints (as of 2026-08)

- Long-connection robots do **not** work in external/customer groups —
  internal company chats and DMs only.
- A second adapter instance authenticating with the same bot causes the
  server to drop the first connection (`event.disconnected_event`): run
  exactly one adapter per bot.
- Welcome replies (`enter_chat`) must be sent within 5 seconds of the event.

## Phase plan (gp-86d / jg-1yx)

1. **This pack**: skeleton + long-connection client, DM + group round-trip
   with the mayor session.
2. Voice: already text via WeCom server-side ASR for the basic path;
   evaluate local transcription only if quality/coverage demands it.
3. Paperwork systems inventory (Duqin/Dongyun) + mandatory
   draft-then-confirm gates (template cards are the natural confirm UI).
