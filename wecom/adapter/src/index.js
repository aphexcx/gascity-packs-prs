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
//   - Outbound: a UDS (or loopback TCP) listener for /publish + /healthz.
//     gc's extmsg subsystem POSTs {callback_url}/publish to deliver a
//     bound session's reply; the adapter forwards it with sendMessage —
//     chatid for group chats, the peer userid for DMs.
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

import http from 'node:http';
import fs from 'node:fs';
import process from 'node:process';
import AiBot from '@wecom/aibot-node-sdk';

import {
  createDownloadGate,
  createStoreQuota,
  defaultMediaDir,
  resolveElevenLabsKey,
  transcribeAudio,
  withDeadline,
} from './media.js';
import { createInboundPipeline, postJSON, sleep } from './inbound.js';

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

// --- outbound: gc /publish → WeCom sendMessage -----------------------------

// WeCom markdown messages cap out around 4096 BYTES; chunk conservatively
// in UTF-8 bytes — Chinese text is ~3 bytes per character, so counting
// UTF-16 code units would overshoot the cap threefold. Splitting iterates
// code points (for...of), which can never sever a surrogate pair.
const outboundChunkBytes = 3800;
const utf8 = new TextEncoder();

function chunkText(text) {
  if (utf8.encode(text).length <= outboundChunkBytes) return [text];
  const chunks = [];
  let current = '';
  let currentBytes = 0;
  let lastNewlineOffset = -1; // index into `current` just past the last \n
  for (const ch of text) {
    const chBytes = utf8.encode(ch).length;
    if (currentBytes + chBytes > outboundChunkBytes) {
      // Prefer breaking at the last newline in the window so markdown
      // constructs aren't severed mid-line.
      if (lastNewlineOffset > current.length / 2) {
        chunks.push(current.slice(0, lastNewlineOffset));
        current = current.slice(lastNewlineOffset);
      } else {
        chunks.push(current);
        current = '';
      }
      currentBytes = utf8.encode(current).length;
      lastNewlineOffset = -1;
    }
    current += ch;
    currentBytes += chBytes;
    if (ch === '\n') lastNewlineOffset = current.length;
  }
  if (current.length > 0) chunks.push(current);
  return chunks;
}

// Idempotency: gc retries a publish (callback timeout, transient error)
// with the same idempotency_key. Track per-key chunk progress and the
// final receipt so a retry never re-sends chunks WeCom users already saw.
const publishStates = new Map(); // key → { chunksDelivered, messageID, receipt, promise }
const publishStatesCap = 512;

// Per-chat outbound serialization: the SDK only serializes sends sharing a
// req_id, so two publishes to the same chat (different idempotency keys)
// could otherwise interleave one reply between another's chunks. Chain the
// whole chunk loop per chatid; different chats stay concurrent.
const sendChains = new Map();

function chainSend(chatid, fn) {
  const prev = sendChains.get(chatid) ?? Promise.resolve();
  const next = prev.catch(() => {}).then(fn);
  sendChains.set(chatid, next);
  // The cleanup must hang off a rejection-proofed derivative: `next` can
  // reject (the caller handles that), and `.finally()` on a rejecting
  // promise yields ANOTHER rejecting promise — left bare, that derived
  // rejection is unhandled and kills the process on the first failed send.
  next.catch(() => {}).then(() => {
    if (sendChains.get(chatid) === next) sendChains.delete(chatid);
  });
  return next;
}

function publishStateFor(key) {
  if (!key) return { chunksDelivered: 0 }; // untracked, per-call state
  let state = publishStates.get(key);
  if (!state) {
    state = { chunksDelivered: 0 };
    publishStates.set(key, state);
    if (publishStates.size > publishStatesCap) {
      // Evict the oldest SETTLED state only: evicting one whose send is
      // still in flight would let a gc retry of that key re-queue the
      // whole message from scratch — duplicate chunks users already saw.
      // If everything is in flight (pathological load), grow temporarily.
      for (const [k, s] of publishStates) {
        // Only fully completed entries (receipt present) are evictable.
        // Never the entry just inserted for `key` (promise-less until
        // after insert), and never a failed partial (chunksDelivered>0,
        // no receipt) — evicting one lets a later gc retry restart at
        // chunk zero and duplicate chunks users already saw.
        if (k !== key && s.receipt && !s.promise) {
          publishStates.delete(k);
          break;
        }
      }
    }
  }
  return state;
}

function handlePublish(cfg, wsClient) {
  return async (req, res, body) => {
    let pub;
    try {
      pub = JSON.parse(body);
    } catch {
      res.writeHead(400).end('invalid JSON');
      return;
    }
    const convo = pub.conversation ?? {};
    const chatid = convo.conversation_id;
    if (!chatid || !pub.text) {
      res.writeHead(400).end('conversation.conversation_id and text are required');
      return;
    }

    const state = publishStateFor(pub.idempotency_key);
    // Atomically claim the send: only the waiter that observes neither a
    // receipt nor an in-flight promise proceeds; everyone else awaits and
    // RE-CHECKS. The loop matters — after a failed send, resumed waiters
    // that simply fell through would each start their own send and resend
    // the same chunk concurrently. No await sits between the final check
    // and the promise assignment below, so the claim is race-free on the
    // event loop.
    for (;;) {
      if (state.receipt) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(state.receipt));
        return;
      }
      if (!state.promise) break;
      await state.promise.catch(() => {});
    }

    const send = async () => {
      const chunks = chunkText(pub.text);
      for (let i = state.chunksDelivered; i < chunks.length; i++) {
        const receipt = await wsClient.sendMessage(chatid, {
          msgtype: 'markdown',
          markdown: { content: chunks[i] },
        });
        state.messageID = receipt?.headers?.req_id ?? state.messageID;
        state.chunksDelivered = i + 1;
      }
    };
    state.promise = chainSend(chatid, send);
    try {
      await state.promise;
    } catch (err) {
      log(`publish → ${chatid} failed at chunk ${state.chunksDelivered + 1}: ${err.message}`);
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'provider_error',
      }));
      return;
    } finally {
      state.promise = undefined;
    }
    state.receipt = {
      conversation: convo,
      message_id: state.messageID ?? '',
      delivered: true,
    };
    log(`publish → ${chatid} delivered (session=${pub.session_id ?? ''})`);
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(state.receipt));
  };
}

function startInternalListener(cfg, wsClient) {
  const publish = handlePublish(cfg, wsClient);
  const server = http.createServer((req, res) => {
    if (req.method === 'GET' && req.url.startsWith('/healthz')) {
      res.writeHead(200).end('ok');
      return;
    }
    if (req.method === 'POST' && req.url.startsWith('/publish')) {
      // Stream-decode as UTF-8: coercing each Buffer chunk to a string
      // independently corrupts a multibyte sequence (Chinese, emoji) that
      // a chunk boundary happens to split.
      req.setEncoding('utf8');
      let body = '';
      req.on('data', (d) => { body += d; });
      req.on('end', () => { publish(req, res, body).catch((err) => {
        log(`publish handler error: ${err.message}`);
        if (!res.headersSent) res.writeHead(500).end();
      }); });
      return;
    }
    res.writeHead(404).end();
  });

  if (cfg.serviceSocket) {
    // The controller owns the UDS lifecycle but a crashed predecessor can
    // leave a stale socket file behind; listen() would EADDRINUSE on it.
    try { fs.unlinkSync(cfg.serviceSocket); } catch { /* ENOENT is fine */ }
    server.listen(cfg.serviceSocket, () => {
      log(`internal listener on UDS ${cfg.serviceSocket} (/publish, /healthz)`);
    });
  } else {
    const [host, port] = cfg.listenInternal.split(':');
    server.listen(Number(port), host, () => {
      log(`internal listener on ${cfg.listenInternal} (/publish, /healthz)`);
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
    reply_instructions: 'Write your reply to a file, then run: gc wecom publish --chat {conversation_id} --text-file <path>',
    capabilities: {
      SupportsChildConversations: false,
      // Inbound only: media/file messages hydrate into file:// attachment
      // records (src/media.js). Outbound stays text/markdown.
      SupportsAttachments: true,
      MaxMessageLength: outboundChunkBytes,
    },
  });
  log(`registered with gc as provider=${cfg.provider} account=${cfg.botId} callback=${cfg.internalCallbackURL}`);
}

// --- main -------------------------------------------------------------------

async function main() {
  const cfg = loadConfig();

  // Debug-suppressing logger: the SDK's default logger writes every
  // callback frame at DEBUG via JSON.stringify — full message bodies,
  // user ids, media URLs/AES keys, and response_url tokens — and
  // proxy_process output persists to the service log. Conversation
  // content must never be retained there; info/warn/error pass through.
  // warn/error can embed serialized frame data on malformed or novel
  // frames — scrub anything from the first brace onward and drop varargs
  // (which carry raw objects) so payloads never persist at any level.
  const scrub = (m) => String(m).replace(/\{[\s\S]*$/, '{…redacted}');
  const sdkLogger = {
    debug: () => {},
    info: (m) => log('[sdk]', scrub(m)),
    warn: (m) => log('[sdk][warn]', scrub(m)),
    error: (m) => log('[sdk][error]', scrub(m)),
  };

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

  const server = startInternalListener(cfg, wsClient);

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
  }

  if (cfg.welcomeText) {
    wsClient.on('event.enter_chat', (frame) => {
      // replyWelcome must run within 5s of the event frame.
      wsClient.replyWelcome(frame, { msgtype: 'text', text: { content: cfg.welcomeText } })
        .catch((err) => log(`welcome reply failed: ${err.message}`));
    });
  }

  wsClient.connect();

  if (cfg.registerOnStart) {
    // Runs concurrently with the WS connection; retries until gc accepts.
    registerAdapterUntilDone(cfg);
  }

  const shutdown = (signal) => {
    log(`${signal} received; shutting down`);
    wsClient.disconnect();
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 2000).unref();
  };
  process.on('SIGINT', () => shutdown('SIGINT'));
  process.on('SIGTERM', () => shutdown('SIGTERM'));
}

main().catch((err) => {
  console.error(`gc-wecom-adapter: ${err.message}`);
  process.exit(1);
});
