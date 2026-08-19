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
  - Inbound: text / voice / mixed / image / file / video frames →
    `POST /v0/city/{city}/extmsg/inbound` addressed to the mayor. WeCom
    transcribes voice server-side, so voice arrives as `[voice]
    <transcript>`. Media/file attachments are downloaded + AES-256-CBC
    decrypted **in the receive path** (their URLs expire in ~5 minutes),
    persisted to the durable store below, and surfaced to the bound
    session as `file://` attachment records plus a
    `[N WeCom file(s) attached] … saved to <path>; Read that path to view
    it` text block (mirroring slack-full's inbound hydration). Audio
    **files** (m4a/amr/mp3/… — distinct from voice messages) additionally
    get an inline ElevenLabs Scribe transcript
    (`model_id=scribe_v1, diarize=true, tag_audio_events=false`; key from
    `$ELEVENLABS_API_KEY` or `~/.config/elevenlabs/api-key`); a
    transcription or download failure downgrades to a note — the message
    always delivers. See `adapter/src/media.js`.
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

## Inbound media store

Decrypted attachments persist (reboot-safe, unlike slack-full's /tmp
spool — the mayor cites these paths in beads and replies long after
receipt) at:

```
<store>/<conversation-id>/<message-id>[-<mixed-index>]-<original-filename>
```

`<store>` resolves to `$WECOM_MEDIA_DIR`, else
`<city>/.gc/wecom-media/inbound` derived from `GC_SERVICE_SECRETS_DIR` in
supervised mode (per-city, so two cities on one host never interleave
stores), else `~/city/.gc/wecom-media/inbound`. Dirs are 0700 and files
0600 (DM content). Path components are sanitized; original filenames
(Chinese included) survive minus separators/control chars.

Limits (all env-tunable, see `adapter/run.sh`): per-file size cap
`WECOM_MEDIA_MAX_BYTES` (default 200MB, enforced while the download
streams — the wire allows +32 bytes for WeCom's PKCS#7 padding);
`WECOM_MEDIA_MAX_CONCURRENT_DOWNLOADS` (default 3) bounds in-flight
downloads globally, so worst-case buffer memory is slots × the size cap;
a queued download that cannot start within `WECOM_MEDIA_URL_TTL_MS`
(default 270s from the message's create_time) is rejected instead of
running against a dead URL; the download itself is wall-clock bounded by
`WECOM_MEDIA_DOWNLOAD_TIMEOUT_MS` (a trickling body is aborted, not just
an idle one). The store has a total quota (`WECOM_MEDIA_QUOTA_BYTES`,
default 10GiB) and a minimum-free-disk floor
(`WECOM_MEDIA_MIN_FREE_BYTES`, default 5GiB): on breach the save is
rejected with an in-message note. Nothing prunes the store automatically
— it is append-only (fleet no-deletion policy); reclaiming space is a
manual decision.

Failure behavior is deliberate: a failed download/decrypt/save still
delivers the message with a placeholder plus an error note (the URL is
dead ~5 minutes after receipt, so the note says to ask the sender to
re-send); a failed transcription still delivers the saved file path with
a `[transcription failed: …]` note.

## Adapter tests

```
cd wecom/adapter && pnpm install && pnpm test   # node --test, no test deps
```

`test/media.test.js` covers the WeCom media crypto scheme against
generated fixtures (encrypt per spec → SDK `decryptFile`), the real SDK
download path over a local HTTP server, filename/path sanitization,
admission gate/quota behavior, Scribe request shape, and the
failure-isolation contract. `test/inbound.test.js` drives the extracted
frame→gc pipeline (`src/inbound.js`) with a fake downloader and fake gc:
extmsg POST shape (attachments included), hydration starting while the
conversation chain is blocked, replay dedup mid-download, cleanup after
delivery/rejection, and text/voice regressions.

## Secrets

Supervised (proxy_process) runs — the normal case — REQUIRE the per-city
secrets file: `<city>/.gc/services/wecom/secrets/env` (the directory gc
scaffolds and passes as `GC_SERVICE_SECRETS_DIR`; the adapter fails fast
if the file is missing there). Per-city scoping is what stops two cities
on one host from sourcing each other's bot credentials and cross-wiring
connections. `~/.config/gc-wecom-adapter/env` works only for standalone
dev runs of `adapter/run.sh` outside gc. Contents (never in the repo):

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
- **No offline replay**: messages sent while no connection is live are NOT
  queued and replayed by Tencent on reconnect (observed live 2026-08-18:
  a DM sent before the supervised service first connected never produced
  a callback frame). Keeping the service supervised is what bounds this
  loss window to restarts.
- A second adapter instance authenticating with the same bot causes the
  server to drop the first connection (`event.disconnected_event`): run
  exactly one adapter per bot.
- Welcome replies (`enter_chat`) must be sent within 5 seconds of the event.

## Phase plan (gp-86d / jg-1yx / jg-c7j)

1. **Done (jg-1yx)**: skeleton + long-connection client, DM + group
   round-trip with the mayor session.
2. **Done (jg-c7j)**: media/file ingestion — download+decrypt in the
   receive path, durable `file://` hand-off, ElevenLabs Scribe transcripts
   for audio files. Voice messages stay on WeCom's server-side ASR.
3. Paperwork systems inventory (Duqin/Dongyun) + mandatory
   draft-then-confirm gates (template cards are the natural confirm UI).
