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
  - Outbound: `/publish` + `/publish-media` + `/healthz` on
    `$GC_SERVICE_SOCKET` (UDS). gc appends `/publish` to the registered
    callback URL when delivering a session's reply; the adapter forwards
    it as a WeCom markdown message — to the `chatid` for group chats, to
    the peer `userid` for DMs — chunked at 3800 UTF-8 bytes.
    `/publish-media` (jg-d0xr) sends a local **image or video** file:
    chunked upload over the long connection (`aibot_upload_media_init →
    chunk × N → finish`, ≤512KB/chunk) to a `media_id`, then an
    `aibot_send_msg` image/video message. See "Outbound media" below and
    `adapter/src/outbound.js`.
- `commands/publish.sh` — `gc wecom publish`: manual/operator sends through
  the running adapter via gc's `/svc/wecom` reverse proxy — `--text` /
  `--text-file` for markdown, `--image` / `--video` for media (optional
  `--text` caption goes out as a follow-up message). Also the verb
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
downloads globally, so download buffer memory is at most slots × the
size cap — the URL-expiry deadline (`WECOM_MEDIA_URL_TTL_MS`, default
270s from the message's create_time) is honored on every admission path:
an already-expired URL is rejected even when a slot is idle, and a
queued download whose deadline passes is rejected instead of running
against a dead URL. Downloads are wall-clock bounded by
`WECOM_MEDIA_DOWNLOAD_TIMEOUT_MS` (a trickling body is aborted, not just
an idle one). Audio transcription never extends the download bound: the
buffer is dropped at the disk write and the bytes are re-read from the
saved file only after admission to a separate transcription gate
(`WECOM_TRANSCRIBE_MAX_CONCURRENT`, default 2) — and verified before
Scribe sees them (O_NOFOLLOW open, regular-file + exact-size check on
the open fd, sha256 match against the bytes written); a file changed
after save fails the transcription with a note instead. The store has a total
quota (`WECOM_MEDIA_QUOTA_BYTES`, default 10GiB, charged by delta so an
overwrite never double-counts) and a minimum-free-disk floor
(`WECOM_MEDIA_MIN_FREE_BYTES`, default 5GiB): on breach the save is
rejected with an in-message note. Nothing prunes the store automatically
— it is append-only (fleet no-deletion policy); reclaiming space is a
manual decision.

Backpressure: hydration results are held (for replay dedup) until gc
accepts the message — protected for the message's whole pending
lifetime, from enqueue to bridge settlement, so an entry queued behind
an earlier delivery in the same conversation is never reaped early. If
512 media messages are simultaneously awaiting delivery (a long gc
outage under heavy media traffic), NEW media messages deliver with an
explicit "media not ingested: hydration backlog full" note instead of
evicting a live entry — evicting one would let an SDK replay re-download
bytes already ingested. The refusal itself sticks per msgid while that
message is pending: replays get the same refusal, never a download the
queued note won't deliver.

Failure behavior is deliberate: a failed download/decrypt/save still
delivers the message with a placeholder plus an error note (the URL is
dead ~5 minutes after receipt, so the note says to ask the sender to
re-send); a failed transcription still delivers the saved file path with
a `[transcription failed: …]` note.

## Outbound media (images & videos)

`gc wecom publish --chat <id> --image /abs/path.png [--text caption]` (or
`--video /abs/path.mp4`) posts the adapter's `/publish-media`: the adapter
validates the local file, uploads it over the long connection, and pushes
the media message. Limits are WeCom's own smart-robot caps
(developer.work.weixin.qq.com/document/path/101463, verified 2026-08-20):
**images ≤10MB, jpg/jpeg/png/gif; videos ≤10MB, mp4** — checked by magic
bytes, not filename. Oversized or wrong-format files are rejected with an
actionable 400 (the adapter never transcodes — downscale/re-encode and
retry; `WECOM_IMAGE_MAX_BYTES` / `WECOM_VIDEO_MAX_BYTES` override the caps
only for tenants allowed more). WeCom image/video messages have no caption
field, so `--text` is delivered as a follow-up markdown message on the
same per-chat send chain (nothing can interleave between media and
caption). Uploads count against WeCom's robot quota (30/min, 1000/hour);
`media_id`s stay valid 3 days but are not reused across sends.

**Transcript recording.** gc's outbound wire (`PublishRequest`) is
text-only, so gc cannot deliver media itself; the adapter therefore sends
first and then POSTs gc `/extmsg/outbound` with the SAME idempotency key
it just settled. gc authorizes against the conversation's (agent-)binding
and calls back `/publish` — which answers from the settled receipt
without re-sending — then appends the outbound transcript entry
(`[image sent] name (mime, bytes, sha256 …) — source: path` + caption)
and fans out peer notifications. Recording is best-effort by design: no
`session_id`, a caller that doesn't own the binding, or a gc outage
downgrade to `transcript_recorded: false` with a note — the media was
already delivered. The dm/room kind gc keys conversations by is learned
from inbound traffic (persisted at
`<store>/../conversation-kinds.json`), with `--kind` and a `wr`
chatid-prefix heuristic as fallbacks; a wrong guess only makes gc reject
the recording (no binding under the mismatched ref) — it can never
record under a mislabeled conversation. Plain `--text` publishes through
this command still bypass gc and are not transcript-recorded (unchanged
behavior; the standard reply flow's own text recording is a separate,
gc-side concern).

## Robustness & efficiency (jg-p1mk hardening batch)

**Inbound-liveness watchdog** (`src/liveness.js`). The 8/19 incident: the
WS sat ready-but-dead ~40 minutes — socket up, pongs flowing, `/healthz`
green, zero pushes. The SDK's missed-pong detection only catches
transport death, so the adapter now tracks a last-inbound watermark
(every message frame and event callback) and, past
`WECOM_LIVENESS_STALL_AFTER_MS` (default 10min; 0 disables) of silence,
logs a loud `INBOUND LIVENESS ALARM` (repeated every 30min while the
stall persists) and appends `inbound_liveness=stalled` to `/healthz`
(first line stays `ok`, HTTP status stays 200 — the supervisor must not
restart-loop on a suspicion). WeCom has no bot-readable history API, so
unlike slack-full there is no probe to distinguish quiet-chat from
dead-push and no backfill — a real stall's messages are unrecoverable,
which is why surfacing matters. `WECOM_LIVENESS_RECONNECT=true`
additionally force-cycles the connection while stalled (rate-limited to
once per 30min) — the only remediation available for a ready-but-dead WS.

**Empty-payload surfacing** (`src/inbound.js`). The 8/22 22:53 case: a
voice frame arrived with no server transcript and no media, and the
session got a bare `[voice message]` with zero log evidence. Now a voice
frame without a transcript, a media frame without a download URL, and a
mixed frame with URL-less images all deliver with an explicit
`语音转写失败/内容缺失`-style marker and log an `EMPTY PAYLOAD` line; a
text frame rendering to nothing is dropped WITH a log line. Nothing is
ever silently thin.

**Inbound burst coalescing** (`src/inbound.js`, port of slack-full's
gp-729 coalescer). Same-chat messages arriving within
`WECOM_COALESCE_WINDOW_MS` (default 8s; 0 disables) deliver as ONE gc
inbound — header plus every message verbatim in arrival order, media
attachments concatenated, batch dedup key `wecom-batch-<first>-<last>-<n>`.
Per-chat only; the window is fixed from the first buffered message; a
buffer hitting 50 messages flushes early (nothing is ever dropped);
shutdown drains buffers first. Hydration still starts at frame arrival —
the 5-minute URL fuse never waits out the window. A single-message
window delivers byte-identical to the immediate path.

**Reply feedback (👍/👎, jg-mlfs).** Outbound markdown sends carry an
adapter-minted `feedback.id` (deterministic per idempotency key + chunk;
`WECOM_FEEDBACK_IDS=false` disables), which makes the WeCom client offer
feedback controls on bot replies. A user's rating arrives as an
`event.feedback_event` and is forwarded to the bound session as a
lightweight `[user feedback]` signal — 👍, or 👎 with WeCom's reason
codes and the user's free-text criticism, or a withdrawal — deduped on
the event msgid and riding the same per-conversation ordering and
coalescing as messages. Correlation: the publish log line's
`feedback_base=fb-…` matches the signal's `feedback_id=fb-….<chunk>`.

**Reply how-to once per conversation.** The reply_instructions template
registered with gc is one line (rendered into every inbound reminder);
the full how-to — file-based reply hygiene, media publish flags, the
`WECOM_OUTBOUND_MEDIA_ROOT` confinement — is appended to each
conversation's first delivery per adapter lifetime and re-armed if that
delivery is rejected.

**Voice ASR-repeat dedup.** WeCom's server-side voice transcription has
been observed (8/22, every long voice message) delivering the same text
repeated 2–3× verbatim. A transcript that is one ≥8-character block
repeated 2–4 times exactly (no separator, or a single space/newline)
collapses to one block with an `(ASR重复×N已折叠)` marker; the log
records counts only, never transcript content. Short doublings like
好的好的 are deliberately left alone.

**Peer-bot context buffering.** Group posts authored by userids in
`WECOM_PEER_BOT_USERIDS` (comma-separated; empty disables) never wake
the bound session. They buffer per conversation (cap 20, oldest dropped
with a count, msgid-deduped) and ride ahead of the next human delivery
as a `[peer-bot context]` read-only block, restored if that delivery is
rejected. Peer media is not hydrated — context is text-only by
contract. Liveness watchdog probes generate no gc traffic and no agent
turns by construction.

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
delivery/rejection, and text/voice regressions. `test/outbound.test.js`
drives the publish pipeline (`src/outbound.js`) with a fake WS client and
fake gc: upload→send→receipt shape, size/format rejections (magic-byte
checks, symlink/relative-path refusal), stage-latched resume under one
idempotency key (never a second upload or a twice-shown image), the
recording callback answering from the settled receipt, dm/room kind
resolution and the persisted kind store, per-chat ordering across both
endpoints, and the relocated text-publish behavior (chunking, keyed-retry
dedup).

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
3. **Done (jg-d0xr)**: outbound media — `gc wecom publish --image/--video`
   via chunked upload + `aibot_send_msg`, with extmsg transcript recording
   through gc `/extmsg/outbound` (idempotency-key dedup against re-sends).
4. Paperwork systems inventory (Duqin/Dongyun) + mandatory
   draft-then-confirm gates (template cards are the natural confirm UI).
