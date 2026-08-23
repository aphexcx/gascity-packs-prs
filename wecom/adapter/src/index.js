// gc-wecom-adapter — the WeCom (企业微信) AI Bot ↔ gc bridge.
//
// A WeCom Smart Robot in API mode ("Long Connection") speaks over an
// OUTBOUND WebSocket to wss://openws.work.weixin.qq.com — no public
// endpoint, no funnel, no inbound TLS. This adapter keeps that long
// connection via Tencent's official @wecom/aibot-node-sdk and bridges both
// directions to gc:
//
//   - Inbound: text / voice / mixed / image / file / video messages →
//     POST /v0/city/{city}/extmsg/inbound. Routing to the mayor comes from
//     the pack's city-fragment default route, never from a per-message
//     addressee. WeCom transcribes voice server-side, so voice frames
//     carry text. Media attachments are downloaded + decrypted in the
//     receive path (their URLs expire in ~5 minutes), persisted under the
//     durable media store, and surfaced as file:// attachments plus a
//     text block; audio FILES additionally get an inline ElevenLabs
//     Scribe transcript. See src/media.js.
//
//   - Outbound: a UDS (or loopback TCP) listener for /publish +
//     /publish-media + /healthz. gc's extmsg subsystem POSTs
//     {callback_url}/publish to deliver a bound session's reply; the
//     adapter forwards it with sendMessage — chatid for group chats, the
//     peer userid for DMs. /publish-media (gc wecom publish
//     --image|--video|--file) uploads a local image/video/file over the
//     long connection and pushes it as a media message, then records the
//     outbound transcript through gc /extmsg/outbound. See src/outbound.js.
//
// Required env:
//
//	WECOM_BOT_ID       Bot ID from the WeCom console (API mode creation).
//	WECOM_BOT_SECRET   Bot Secret from the WeCom console.
//	GC_CITY_NAME       gc city the adapter bridges into.
//
// Controller-injected env (proxy_process mode):
//
//	GC_SERVICE_SOCKET      UDS path the internal listener binds.
//	GC_SERVICE_URL_PREFIX  Reverse-proxy prefix gc routes to this service;
//	                       used to compute the self-registration callback URL.
//	GC_API_BASE_URL        gc API base — REQUIRED in proxy_process mode
//	                       (the controller does not inject it); standalone
//	                       dev falls back to http://127.0.0.1:9443.
//
// Optional env:
//
//	LISTEN_INTERNAL        TCP bind when GC_SERVICE_SOCKET is unset
//	                       (default 127.0.0.1:8790).
//	REGISTER_ON_START      "true" (default) self-registers as an extmsg adapter.
//	WECOM_WELCOME_TEXT     Welcome message sent on enter_chat (default: none).
//	WECOM_WS_URL           Override the long-connection endpoint (private
//	                       deployments publish their own; default is the
//	                       SDK's built-in wss://openws.work.weixin.qq.com).
//	WECOM_MEDIA_DIR        Durable store for decrypted inbound media.
//	                       Default: <city>/.gc/wecom-media/inbound derived
//	                       from GC_SERVICE_SECRETS_DIR in supervised mode;
//	                       ~/city/.gc/wecom-media/inbound standalone.
//	WECOM_MEDIA_MAX_BYTES  Attachment size cap (default 209715200 = 200MB).
//	WECOM_MEDIA_DOWNLOAD_TIMEOUT_MS
//	                       Media download wall-clock deadline (default
//	                       120000) — covers the whole response body.
//	WECOM_MEDIA_MAX_CONCURRENT_DOWNLOADS
//	                       Global download-admission bound (default 3);
//	                       download buffer memory ≤ this × the size cap.
//	WECOM_TRANSCRIBE_MAX_CONCURRENT
//	                       Concurrent Scribe transcriptions (default 2);
//	                       audio bytes are re-read from disk only after
//	                       admission, adding ≤ this × file size.
//	WECOM_MEDIA_URL_TTL_MS Download-URL lifetime from message create_time
//	                       (default 270000); queued downloads that cannot
//	                       start inside it are rejected with a note.
//	WECOM_MEDIA_QUOTA_BYTES
//	                       Durable-store quota (default 10GiB). On breach
//	                       saves are rejected with a note — the store is
//	                       append-only; nothing is ever auto-deleted.
//	WECOM_MEDIA_MIN_FREE_BYTES
//	                       Minimum free disk space to leave after a save
//	                       (default 5GiB); saves breaching it are rejected.
//	WECOM_TRANSCRIBE_TIMEOUT_MS
//	                       ElevenLabs Scribe timeout (default 180000).
//	WECOM_TRANSCRIBE_LANGUAGE
//	                       Pin a Scribe language_code (e.g. "zh"); default
//	                       empty = auto-detect.
//	ELEVENLABS_API_KEY     Scribe API key; falls back to
//	                       ~/.config/elevenlabs/api-key. Unset = audio
//	                       files deliver with a transcription-failed note.
//	WECOM_OUTBOUND_MEDIA_ROOT
//	                       Directory outbound media files may be read
//	                       from. REQUIRED for /publish-media — without it
//	                       every media publish is refused (fail closed):
//	                       the endpoint would otherwise let any local
//	                       caller send arbitrary adapter-readable files to
//	                       a WeCom chat. Symlinks anywhere in a media path
//	                       are rejected, and the confinement walk is
//	                       race-free (descriptor-anchored per-component
//	                       traversal; the checked fd is what gets read).
//	WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS
//	                       "true" allows /publish-media to conversations
//	                       the adapter has never seen inbound traffic
//	                       from. Default false: media (the exfiltration-
//	                       capable surface) only goes to chats with
//	                       inbound evidence — the strongest pre-delivery
//	                       target check available without a gc
//	                       authorization endpoint.
//	WECOM_IMAGE_MAX_BYTES  Outbound image cap (default 10485760 = 10MB —
//	                       WeCom's own smart-robot limit; raise only if
//	                       your tenant allows more).
//	WECOM_VIDEO_MAX_BYTES  Outbound video cap (default 10485760 = 10MB,
//	                       the WeCom smart-robot limit; mp4 only).
//	WECOM_FILE_MAX_BYTES   Outbound file cap (default 20971520 = 20MB,
//	                       the WeCom smart-robot limit; any file type).
//	WECOM_UPLOAD_TIMEOUT_MS
//	                       Wall-clock deadline for one whole chunked media
//	                       upload (default 300000 — 10MB in ≤512KB chunks
//	                       over a mainland uplink needs headroom).
//	WECOM_UPLOAD_MAX_CONCURRENT
//	                       Global outbound-upload admission bound (default
//	                       2); media buffer memory ≤ this × the media cap.
//	WECOM_UPLOAD_MAX_QUEUE Requests allowed to WAIT for an upload slot
//	                       (default 8); beyond it /publish-media answers
//	                       429 before reading the file.
//	WECOM_PUBLISH_MAX_BODY_BYTES
//	                       Internal-listener request body cap (default
//	                       1048576 = 1MiB); oversized bodies answer 413
//	                       and the connection is severed.
//	WECOM_LIVENESS_STALL_AFTER_MS
//	                       Inbound-liveness watchdog threshold (default
//	                       600000 = 10min; 0 disables): with no inbound
//	                       frame for this long, the adapter logs a loud
//	                       ALARM and /healthz reports
//	                       inbound_liveness=stalled (HTTP status stays
//	                       200) until inbound resumes. See src/liveness.js.
//	WECOM_LIVENESS_RECONNECT
//	                       "true" force-cycles the long connection while
//	                       stalled (rate-limited; default false) — the
//	                       only remediation available for a
//	                       ready-but-dead WS, since WeCom has no history
//	                       API to probe or backfill from.
//	WECOM_COALESCE_WINDOW_MS
//	                       Per-conversation inbound burst-coalescing
//	                       window (default 8000; 0 disables): same-chat
//	                       messages arriving within the window deliver as
//	                       ONE combined gc inbound, in arrival order.
//	WECOM_FEEDBACK_IDS     "false" stops attaching feedback ids to
//	                       outbound markdown sends. Default on: every
//	                       text publish carries a deterministic
//	                       feedback id, WeCom then shows 👍/👎 on the
//	                       reply, and a user's rating arrives as an
//	                       event.feedback_event the adapter forwards to
//	                       the bound session (jg-mlfs).
//	WECOM_PEER_BOT_USERIDS Comma-separated userids of peer bots sharing
//	                       group chats. Their posts never wake the bound
//	                       session — they buffer (capped, oldest dropped
//	                       with a count) and ride ahead of the next human
//	                       delivery as tagged read-only context. Empty
//	                       (default) disables.

import http from 'node:http';
import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import AiBot from '@wecom/aibot-node-sdk';

import {
  createDownloadGate,
  createSdkLogger,
  createStoreQuota,
  defaultMediaDir,
  resolveElevenLabsKey,
  transcribeAudio,
  withDeadline,
} from './media.js';
import { createInboundPipeline, postJSON, sleep } from './inbound.js';
import { createInboundLiveness } from './liveness.js';
import { createRequestListener, hardenServer } from './listener.js';
import {
  createAttemptJournal,
  createConversationKindStore,
  createOutboundPublisher,
  outboundChunkBytes,
} from './outbound.js';

const { WSClient } = AiBot;

// --- config ----------------------------------------------------------------

function getenv(name, fallback = '') {
  const v = process.env[name];
  return v === undefined || v === '' ? fallback : v;
}

// intEnv parses a positive-integer env override; a missing, malformed, or
// non-positive value falls back rather than silently disabling a limit.
function intEnv(name, fallback) {
  const n = Number.parseInt(getenv(name), 10);
  return Number.isFinite(n) && n > 0 ? n : fallback;
}

// intEnv0 additionally accepts 0 — for knobs where zero is a meaningful
// "off" setting (watchdog threshold, coalescing window), not a mistake.
function intEnv0(name, fallback) {
  const n = Number.parseInt(getenv(name), 10);
  return Number.isFinite(n) && n >= 0 ? n : fallback;
}

function loadConfig() {
  const cfg = {
    botId: getenv('WECOM_BOT_ID'),
    botSecret: getenv('WECOM_BOT_SECRET'),
    cityName: getenv('GC_CITY_NAME'),
    gcAPIBase: getenv('GC_API_BASE_URL').replace(/\/+$/, ''),
    // Fixed provider key: the pack's required city-fragment default route
    // and command surface are keyed to "wecom" — an override here would
    // silently detach registration from the route and drop every unbound
    // conversation.
    provider: 'wecom',
    welcomeText: getenv('WECOM_WELCOME_TEXT'),
    wsURL: getenv('WECOM_WS_URL'),
    serviceSocket: getenv('GC_SERVICE_SOCKET'),
    serviceURLPrefix: getenv('GC_SERVICE_URL_PREFIX').replace(/\/+$/, ''),
    listenInternal: getenv('LISTEN_INTERNAL', '127.0.0.1:8790'),
    registerOnStart: getenv('REGISTER_ON_START', 'true') !== 'false',
    internalCallbackURL: '',
    mediaDir: defaultMediaDir(process.env),
    mediaMaxBytes: intEnv('WECOM_MEDIA_MAX_BYTES', 200 * 1024 * 1024),
    mediaDownloadTimeoutMs: intEnv('WECOM_MEDIA_DOWNLOAD_TIMEOUT_MS', 120000),
    // Admission bound on concurrent downloads: worst-case buffer memory is
    // maxConcurrentDownloads × mediaMaxBytes (600MB at the defaults).
    mediaMaxConcurrentDownloads: intEnv('WECOM_MEDIA_MAX_CONCURRENT_DOWNLOADS', 3),
    // How long after a message's create_time its download URL is treated
    // as alive; a queued download that cannot start inside this window is
    // rejected instead of running only to 4xx on a dead URL.
    mediaUrlTtlMs: intEnv('WECOM_MEDIA_URL_TTL_MS', 270000),
    mediaQuotaBytes: intEnv('WECOM_MEDIA_QUOTA_BYTES', 10 * 1024 * 1024 * 1024),
    mediaMinFreeBytes: intEnv('WECOM_MEDIA_MIN_FREE_BYTES', 5 * 1024 * 1024 * 1024),
    transcribeTimeoutMs: intEnv('WECOM_TRANSCRIBE_TIMEOUT_MS', 180000),
    transcribeLanguage: getenv('WECOM_TRANSCRIBE_LANGUAGE'),
    // Bounds how many audio files are re-read from disk and held during
    // Scribe calls at once; audio memory ≤ this × the size cap, on top of
    // (and independent from) the download gate's bound.
    transcribeMaxConcurrent: intEnv('WECOM_TRANSCRIBE_MAX_CONCURRENT', 2),
    // Outbound media caps: 10MB is WeCom's own smart-robot limit for both
    // images (png/jpg/jpeg/gif) and videos (mp4) — see src/outbound.js.
    // Oversized files are rejected with a downscale/re-encode message,
    // never transcoded here.
    // Outbound-media root: /publish-media only reads files under this
    // directory, and refuses everything (fail closed) when it is unset —
    // see assertConfinedMediaPath in src/outbound.js.
    outboundMediaRoot: getenv('WECOM_OUTBOUND_MEDIA_ROOT'),
    // Pre-delivery capability gate (jg-d0xr round-2 finding 1): media
    // sends require inbound evidence for the target conversation unless
    // explicitly opened up.
    mediaRequireKnownConversation: getenv('WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS') !== 'true',
    imageMaxBytes: intEnv('WECOM_IMAGE_MAX_BYTES', 10 * 1024 * 1024),
    videoMaxBytes: intEnv('WECOM_VIDEO_MAX_BYTES', 10 * 1024 * 1024),
    fileMaxBytes: intEnv('WECOM_FILE_MAX_BYTES', 20 * 1024 * 1024),
    uploadTimeoutMs: intEnv('WECOM_UPLOAD_TIMEOUT_MS', 300000),
    // Outbound upload admission (src/outbound.js createUploadGate):
    // buffer memory ≤ uploadMaxConcurrent × the media cap; waiters beyond
    // uploadMaxQueue are answered 429 before any file I/O.
    uploadMaxConcurrent: intEnv('WECOM_UPLOAD_MAX_CONCURRENT', 2),
    uploadMaxQueue: intEnv('WECOM_UPLOAD_MAX_QUEUE', 8),
    // Internal-listener body cap: publish bodies are small JSON (media
    // travels by path, not in the body); over-cap requests answer 413.
    publishMaxBodyBytes: intEnv('WECOM_PUBLISH_MAX_BODY_BYTES', 1024 * 1024),
    // Inbound-liveness watchdog (jg-p1mk item 1; src/liveness.js): the
    // 8/19 ready-but-dead WS sat silent 40 minutes with /healthz green.
    livenessStallAfterMs: intEnv0('WECOM_LIVENESS_STALL_AFTER_MS', 10 * 60 * 1000),
    livenessReconnect: getenv('WECOM_LIVENESS_RECONNECT') === 'true',
    // Inbound burst coalescing (jg-p1mk item 3; src/inbound.js): rapid
    // same-chat bursts deliver as one combined inbound.
    coalesceWindowMs: intEnv0('WECOM_COALESCE_WINDOW_MS', 8000),
    // Outbound feedback ids (jg-mlfs): attach a feedback id to markdown
    // sends so WeCom offers 👍/👎 on replies and reports ratings back.
    feedbackIds: getenv('WECOM_FEEDBACK_IDS', 'true') !== 'false',
    // Peer-bot context buffering (jg-p1mk add A): group posts from these
    // userids never wake the bound session — they buffer and ride ahead
    // of the next human delivery. Empty (default) disables.
    peerBotUserIds: getenv('WECOM_PEER_BOT_USERIDS')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean),
  };

  const missing = [];
  if (!cfg.botId) missing.push('WECOM_BOT_ID');
  if (!cfg.botSecret) missing.push('WECOM_BOT_SECRET');
  // GC_CITY_NAME is interpolated into every /v0/city/{city}/... URL; a
  // wrong default would silently route traffic to the wrong city.
  if (!cfg.cityName) missing.push('GC_CITY_NAME');
  if (missing.length > 0) {
    throw new Error(`missing required env vars: ${missing.join(', ')}`);
  }

  if (cfg.serviceSocket) {
    // proxy_process mode: gc reaches us via GC_API_BASE_URL +
    // GC_SERVICE_URL_PREFIX. gc's extmsg HTTP adapter appends "/publish"
    // itself when calling, so the registered base must NOT include it.
    if (!cfg.serviceURLPrefix) {
      throw new Error('GC_SERVICE_SOCKET is set but GC_SERVICE_URL_PREFIX is empty — controller-injected env is incomplete');
    }
    // The controller injects the socket and URL prefix but NOT the API
    // base; a silent localhost default here would point registration and
    // every inbound POST at the wrong port while /healthz stays green.
    if (!cfg.gcAPIBase) {
      throw new Error('GC_SERVICE_SOCKET is set but GC_API_BASE_URL is empty — set it in the adapter env file (the controller does not inject it)');
    }
    cfg.internalCallbackURL = cfg.gcAPIBase + cfg.serviceURLPrefix;
  } else {
    if (!cfg.gcAPIBase) cfg.gcAPIBase = 'http://127.0.0.1:9443';
    cfg.internalCallbackURL = `http://${cfg.listenInternal}`;
  }
  return cfg;
}

function log(...args) {
  console.log(new Date().toISOString(), ...args);
}

// The inbound direction (WeCom frame → gc extmsg POST, including media
// hydration, replay dedup, and per-conversation ordering) lives in
// src/inbound.js as a testable pipeline; main() below instantiates it
// with the real SDK downloader, Scribe transcriber, download gate, and
// store quota.

// The outbound direction (text /publish with idempotent chunked sends,
// image/video/file /publish-media with upload → media_id → aibot_send_msg and
// gc transcript recording, per-chat send chains) lives in src/outbound.js
// as a testable publisher; main() below instantiates it with the real SDK
// client, and startInternalListener routes both endpoints to it.

function startInternalListener(cfg, publisher, healthDetail = null) {
  // Route matching, the body byte cap, and multibyte-safe decoding live
  // in src/listener.js (testable); socket-level header/request deadlines
  // come from hardenServer (codex jg-d0xr finding 10).
  const server = hardenServer(http.createServer(createRequestListener({
    publisher,
    log,
    maxBodyBytes: cfg.publishMaxBodyBytes,
    healthDetail,
  })));

  if (cfg.serviceSocket) {
    // The controller owns the UDS lifecycle but a crashed predecessor can
    // leave a stale socket file behind; listen() would EADDRINUSE on it.
    try { fs.unlinkSync(cfg.serviceSocket); } catch { /* ENOENT is fine */ }
    server.listen(cfg.serviceSocket, () => {
      log(`internal listener on UDS ${cfg.serviceSocket} (/publish, /publish-media, /healthz)`);
    });
  } else {
    const [host, port] = cfg.listenInternal.split(':');
    server.listen(Number(port), host, () => {
      log(`internal listener on ${cfg.listenInternal} (/publish, /publish-media, /healthz)`);
    });
  }
  return server;
}

// --- adapter self-registration --------------------------------------------

// The adapter registry is in-memory on the gc side: until registration
// lands, gc has no callback URL and no reply instructions, so a
// registration that failed once and was merely logged would leave
// messaging dead until a manual restart. Retry forever on a capped
// backoff — the endpoint is idempotent.
async function registerAdapterUntilDone(cfg) {
  let delay = 5000;
  for (;;) {
    try {
      await registerAdapter(cfg);
      return;
    } catch (err) {
      log(`adapter registration failed: ${err.message}; retrying in ${delay / 1000}s`);
      await sleep(delay);
      delay = Math.min(delay * 2, 60000);
    }
  }
}

async function registerAdapter(cfg) {
  const target = `${cfg.gcAPIBase}/v0/city/${encodeURIComponent(cfg.cityName)}/extmsg/adapters`;
  await postJSON(target, {
    provider: cfg.provider,
    account_id: cfg.botId,
    name: 'wecom-adapter',
    callback_url: cfg.internalCallbackURL,
    // Without this, gc's inbound nudge tells the session to run the
    // generic "gc wecom reply-current ..." — a verb this pack doesn't
    // ship. Point the reply flow at the verb that exists. File-based so
    // arbitrary reply text (apostrophes, code, Chinese quotes) never has
    // to survive shell interpolation.
    //
    // ONE LINE only (jg-p1mk add D, the gp-729 item-3 port): gc renders
    // this into EVERY inbound reminder, and the previous ~120-word block
    // (media flags, root-confinement note) cost that many tokens per
    // message. The full how-to now arrives once per conversation per
    // adapter lifetime, appended by the pipeline to that conversation's
    // first delivery (src/inbound.js replyHelpBlock).
    reply_instructions: 'Reply: write the reply to a file, then run: gc wecom publish --chat {conversation_id} --text-file <path>',
    capabilities: {
      SupportsChildConversations: false,
      // Inbound: media/file messages hydrate into file:// attachment
      // records (src/media.js). Outbound: text/markdown via /publish;
      // image/video via /publish-media (src/outbound.js).
      SupportsAttachments: true,
      MaxMessageLength: outboundChunkBytes,
    },
  });
  log(`registered with gc as provider=${cfg.provider} account=${cfg.botId} callback=${cfg.internalCallbackURL}`);
}

// --- main -------------------------------------------------------------------

async function main() {
  const cfg = loadConfig();

  // SDK logger (src/media.js createSdkLogger — codex jg-d0xr finding 11):
  // DEBUG dropped (full callback frames), INFO allowlisted to connection
  // lifecycle because SDK 1.0.7 logs upload filenames and
  // upload_id/media_id at INFO, warn/error scrubbed — braces truncated,
  // URLs dropped, plain-string identifiers redacted, varargs (raw
  // objects) never forwarded. proxy_process output persists to the
  // service log; conversation content must never be retained there.
  const sdkLogger = createSdkLogger(log);

  // maxReconnectAttempts -1 = unlimited: a long network outage must never
  // strand a "healthy" process with a permanently dead connection (the
  // supervisor only watches /healthz, which the HTTP listener answers).
  // Auth-failure retries stay at the SDK default — bad credentials don't
  // heal by retrying; the terminal-error handler below exits instead so
  // the supervisor restarts us (picking up a rotated env file).
  // requestTimeout governs the SDK's HTTP client — media downloads are its
  // only use. The SDK default (10s) is far too short for a large file over
  // a mainland uplink.
  const wsOptions = {
    botId: cfg.botId,
    secret: cfg.botSecret,
    maxReconnectAttempts: -1,
    requestTimeout: cfg.mediaDownloadTimeoutMs,
    logger: sdkLogger,
  };
  if (cfg.wsURL) wsOptions.wsUrl = cfg.wsURL;
  const wsClient = new WSClient(wsOptions);

  // Size cap, enforced WHILE streaming (axios aborts the response once the
  // byte count crosses maxContentLength) — the belt-and-braces post-hoc
  // check in media.js can't protect memory on a multi-GB body by itself.
  // The wire carries CIPHERTEXT: WeCom's PKCS#7 padding (32-byte blocks)
  // adds up to 32 bytes, so allow that overhead here and keep the exact
  // plaintext cap on the decrypted buffer in media.js. wsClient.api is the
  // SDK's documented advanced-use accessor for its download HTTP client.
  wsClient.api.httpClient.defaults.maxContentLength = cfg.mediaMaxBytes + 32;

  // Per-request wall-clock abort: axios's `timeout` (the SDK's
  // requestTimeout) is an inactivity-style timeout — a response body that
  // keeps trickling never trips it, pinning a download slot forever. The
  // interceptor arms a fresh AbortSignal per request so the socket is
  // actually severed at the deadline; the withDeadline wrapper on the
  // downloader below is the promise-level backstop.
  wsClient.api.httpClient.interceptors.request.use((requestConfig) => {
    requestConfig.signal = AbortSignal.timeout(cfg.mediaDownloadTimeoutMs);
    return requestConfig;
  });

  // Conversation-kind memory for outbound transcript refs (dm vs room —
  // part of gc's conversation identity), learned from inbound traffic and
  // persisted next to the media store so it survives restarts.
  const kindStore = createConversationKindStore({
    filePath: path.join(path.dirname(cfg.mediaDir), 'conversation-kinds.json'),
    log,
  });

  // Outbound attempt journal: media stage latches (upload → send →
  // caption) persisted across restarts so a retried idempotency key
  // resumes instead of re-sending, and interrupted sends come back as
  // delivery-unknown. Lives next to the kind store.
  const journal = createAttemptJournal({
    filePath: path.join(path.dirname(cfg.mediaDir), 'outbound-attempts.json'),
    log,
  });

  const publisher = createOutboundPublisher({
    cfg,
    journal,
    sendMessage: (chatid, body) => wsClient.sendMessage(chatid, body),
    uploadMedia: (buffer, options) => wsClient.uploadMedia(buffer, options),
    sendMediaMessage: (chatid, type, mediaId) => wsClient.sendMediaMessage(chatid, type, mediaId),
    // One wall-clock bound on the whole chunked upload: each WS chunk has
    // the SDK's reply-ack timeout, but 20 chunks × retries can still
    // stretch; the /publish-media HTTP response must unblock eventually.
    withUploadDeadline: (p) => withDeadline(p, cfg.uploadTimeoutMs, 'media upload'),
    kindStore,
    log,
  });

  // Inbound-liveness watchdog (src/liveness.js): the SDK's missed-pong
  // detection only catches transport death; this catches the 8/19
  // ready-but-dead shape — socket healthy, zero pushes. The optional
  // remediation cycles the connection: disconnect() suppresses the SDK's
  // auto-reconnect (manual close), connect() re-arms it and re-auths.
  // connect() must not run until the old socket's close handler has
  // FULLY completed (codex jg-p1mk r1 finding 4, r2 finding 3): SDK
  // 1.0.7 emits 'disconnected' MID-handler, then nulls the shared ws
  // reference and re-checks isManualClose — so connecting synchronously
  // inside the event callback still loses the fresh socket to the
  // handler's tail and re-arms a duplicate auto-reconnect. Defer one
  // macrotask past the event; keep a timer fallback for an already-dead
  // socket that emits nothing, and remove the listener on whichever
  // path fires first so repeated cycles never accumulate listeners.
  const cycleConnection = () => {
    let cycled = false;
    const connectOnce = () => {
      if (cycled) return;
      cycled = true;
      wsClient.off('disconnected', deferredConnect);
      wsClient.connect();
    };
    const deferredConnect = () => {
      setTimeout(connectOnce, 0);
    };
    wsClient.once('disconnected', deferredConnect);
    setTimeout(connectOnce, 3000).unref?.();
    wsClient.disconnect();
  };
  const liveness = createInboundLiveness({
    stallAfterMs: cfg.livenessStallAfterMs,
    reconnect: cfg.livenessReconnect ? cycleConnection : null,
    log,
  });

  const server = startInternalListener(cfg, publisher, () => liveness.healthzDetail());

  // Every server push — messages of any type AND event callbacks — is
  // inbound-liveness evidence. Connection lifecycle (authenticated,
  // pongs) deliberately is not: it stays green while pushes are dead.
  wsClient.on('message', () => liveness.noteInbound());
  wsClient.on('event', () => liveness.noteInbound());

  wsClient.on('authenticated', () => log('wecom long connection authenticated'));
  wsClient.on('reconnecting', (attempt) => log(`wecom reconnecting (attempt ${attempt})`));
  wsClient.on('error', (err) => {
    log(`wecom ws error: ${err.message}`);
    if (err?.code === 'WS_RECONNECT_EXHAUSTED' || err?.code === 'WS_AUTH_FAILURE_EXHAUSTED') {
      // Terminal for this process: the SDK has stopped reconnecting.
      // Exit non-zero so the supervisor restarts the adapter.
      log('terminal connection error; exiting for supervisor restart');
      process.exit(1);
    }
  });
  wsClient.on('event.disconnected_event', () => {
    // The server drops this connection when a NEW connection authenticates
    // with the same bot (usually a stray second instance), and SDK 1.0.7
    // disables reconnects afterwards — staying alive would mean answering
    // /healthz green while permanently deaf. Exit so the supervisor
    // restarts us; on restart we re-authenticate and reclaim the
    // connection once the other instance is gone.
    log('wecom server disconnected this connection (newer connection took over); exiting for supervisor restart');
    process.exit(1);
  });

  // The inbound pipeline (src/inbound.js) owns hydration scheduling,
  // replay dedup, per-conversation ordering, and gc delivery.
  const pipeline = createInboundPipeline({
    cfg,
    log,
    // Both bounds on one downloader: the request interceptor above severs
    // the socket at the deadline; withDeadline guarantees the hydration
    // promise unblocks even if that abort misfires.
    downloadFile: (url, aeskey) =>
      withDeadline(wsClient.downloadFile(url, aeskey), cfg.mediaDownloadTimeoutMs, 'media download'),
    // Key + language resolve per call, not at startup: the operator
    // dropping ~/.config/elevenlabs/api-key in place starts working on
    // the next audio file without an adapter restart.
    transcribe: (buffer, filename) => transcribeAudio(buffer, filename, {
      apiKey: resolveElevenLabsKey(),
      timeoutMs: cfg.transcribeTimeoutMs,
      language: cfg.transcribeLanguage,
    }),
    gate: createDownloadGate(cfg.mediaMaxConcurrentDownloads),
    // Separate small gate for transcription: audio bytes are re-read from
    // disk only after admission here, so they never count against (or wait
    // on) the download slots.
    transcribeGate: createDownloadGate(cfg.transcribeMaxConcurrent),
    quota: createStoreQuota({
      dir: cfg.mediaDir,
      quotaBytes: cfg.mediaQuotaBytes,
      minFreeBytes: cfg.mediaMinFreeBytes,
    }),
  });
  for (const evt of ['message.text', 'message.voice', 'message.mixed', 'message.image', 'message.file', 'message.video']) {
    wsClient.on(evt, pipeline.enqueueInbound);
    // Every inbound frame also teaches the kind store its conversation's
    // dm/room kind — the outbound transcript ref needs it (see outbound.js).
    wsClient.on(evt, kindStore.observe);
  }

  // User feedback on bot replies (jg-mlfs): a 👍/👎/withdrawal on a
  // feedback-id-carrying reply arrives as event.feedback_event; the
  // pipeline renders it as a lightweight "[user feedback]" signal and it
  // rides the same dedup (event msgids share the callback msgid
  // namespace), per-conversation ordering, and coalescing as messages.
  wsClient.on('event.feedback_event', pipeline.enqueueInbound);
  wsClient.on('event.feedback_event', kindStore.observe);

  if (cfg.welcomeText) {
    wsClient.on('event.enter_chat', (frame) => {
      // replyWelcome must run within 5s of the event frame.
      wsClient.replyWelcome(frame, { msgtype: 'text', text: { content: cfg.welcomeText } })
        .catch((err) => log(`welcome reply failed: ${err.message}`));
    });
  }

  wsClient.connect();
  liveness.start();

  if (cfg.registerOnStart) {
    // Runs concurrently with the WS connection; retries until gc accepts.
    registerAdapterUntilDone(cfg);
  }

  let shuttingDown = false;
  const shutdown = (signal) => {
    if (shuttingDown) return;
    shuttingDown = true;
    log(`${signal} received; shutting down`);
    liveness.stop();
    const finish = () => {
      wsClient.disconnect();
      server.close(() => process.exit(0));
      setTimeout(() => process.exit(0), 2000).unref();
    };
    // Drain coalescing buffers first and WAIT for the flushed deliveries
    // to settle (codex jg-p1mk r1 finding 5 — an unawaited drain let the
    // process exit mid-POST, losing the buffered frames the drain
    // exists to save). A delivery wedged in gc-outage retries must not
    // wedge shutdown: the deadline below caps the wait, accepting that
    // an unreachable gc loses the window's frames either way.
    let finished = false;
    const finishOnce = () => {
      if (finished) return;
      finished = true;
      finish();
    };
    pipeline.flushAll().then(finishOnce, finishOnce);
    setTimeout(finishOnce, 5000).unref();
  };
  process.on('SIGINT', () => shutdown('SIGINT'));
  process.on('SIGTERM', () => shutdown('SIGTERM'));
}

main().catch((err) => {
  console.error(`gc-wecom-adapter: ${err.message}`);
  process.exit(1);
});
