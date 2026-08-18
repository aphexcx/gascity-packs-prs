// gc-wecom-adapter — the WeCom (企业微信) AI Bot ↔ gc bridge.
//
// A WeCom Smart Robot in API mode ("Long Connection") speaks over an
// OUTBOUND WebSocket to wss://openws.work.weixin.qq.com — no public
// endpoint, no funnel, no inbound TLS. This adapter keeps that long
// connection via Tencent's official @wecom/aibot-node-sdk and bridges both
// directions to gc:
//
//   - Inbound: text / voice / mixed messages (and file/image/video
//     placeholders) → POST /v0/city/{city}/extmsg/inbound. Routing to the
//     mayor comes from the pack's city-fragment default route, never from
//     a per-message addressee. WeCom transcribes voice server-side, so
//     voice frames carry text.
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

import http from 'node:http';
import fs from 'node:fs';
import process from 'node:process';
import AiBot from '@wecom/aibot-node-sdk';

const { WSClient } = AiBot;

// --- config ----------------------------------------------------------------

function getenv(name, fallback = '') {
  const v = process.env[name];
  return v === undefined || v === '' ? fallback : v;
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

// --- gc extmsg wire helpers (wire-compatible with internal/extmsg) ----------

async function postJSON(target, body) {
  const resp = await fetch(target, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-GC-Request': 'gc-wecom-adapter',
    },
    body: JSON.stringify(body),
    // Deadline so a stalled gc handler (connection accepted, no response)
    // aborts into the retry loop instead of pinning the per-conversation
    // queue forever behind a request that never settles.
    signal: AbortSignal.timeout(15000),
  });
  if (!resp.ok) {
    const text = (await resp.text().catch(() => '')).trim();
    const err = new Error(`${resp.status} ${resp.statusText}: ${text}`);
    err.status = resp.status;
    throw err;
  }
  // Drain the success body: undici only reuses a connection once its
  // response body is consumed, and sustained inbound traffic on
  // never-drained responses accumulates connections until delivery stalls.
  await resp.arrayBuffer().catch(() => {});
}

// A 4xx is normally a deterministic rejection — retrying sends the same
// bytes to the same validator. Two exceptions are transient: network
// failures (no status), and 404, which gc's city-scoped endpoints return
// during normal city startup/restart until reconciliation completes
// ("city not found or not running"). A genuinely wrong city name also
// 404s — that misconfiguration shows up as an endless, logged retry loop
// rather than silent message loss, which is the right failure mode here.
function isRetryable(err) {
  return err.status === undefined || err.status >= 500 || err.status === 404;
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// postJSONWithRetry retries transient failures indefinitely on a backoff
// ladder capped at 60s, and rethrows the first non-retryable error. The
// retry is the delivery mechanism — WeCom replay is not guaranteed, so a
// gc outage of any length must not translate into dropped messages. Memory
// is bounded by traffic during the outage: one small pending frame per
// undelivered message.
const retryDelaysMs = [1000, 5000, 15000, 60000];

async function postJSONWithRetry(target, body, label) {
  for (let attempt = 0; ; attempt++) {
    try {
      await postJSON(target, body);
      return;
    } catch (err) {
      if (!isRetryable(err)) throw err;
      const delay = retryDelaysMs[Math.min(attempt, retryDelaysMs.length - 1)];
      if (attempt === 0 || attempt % 10 === 0) {
        log(`${label}: attempt ${attempt + 1} failed (${err.message}); retrying in ${delay / 1000}s`);
      }
      await sleep(delay);
    }
  }
}

function conversationForMessage(cfg, msg) {
  // Group chats carry a chatid; DMs ("single") identify the conversation
  // by the peer's userid — the same value sendMessage() expects for each.
  const isGroup = msg.chattype === 'group';
  return {
    scope_id: cfg.cityName,
    provider: cfg.provider,
    account_id: cfg.botId,
    conversation_id: isGroup ? msg.chatid : msg.from?.userid,
    kind: isGroup ? 'room' : 'dm',
  };
}

// renderText flattens each supported WeCom message type into the plain
// text gc transports. Media stays a placeholder in phase 1 — the encrypted
// download URLs expire in 5 minutes, so proper attachment surfacing needs
// the download+decrypt path (SDK downloadFile) and a file:// hand-off.
function renderText(msg) {
  switch (msg.msgtype) {
    case 'text':
      return msg.text?.content ?? '';
    case 'voice':
      // WeCom transcribes voice server-side; content IS the transcript.
      return msg.voice?.content ? `[voice] ${msg.voice.content}` : '[voice message]';
    case 'mixed': {
      const parts = (msg.mixed?.msg_item ?? []).map((item) =>
        item.msgtype === 'text' ? (item.text?.content ?? '') : '[image]'
      );
      return parts.join('').trim() || '[mixed message]';
    }
    case 'image':
      return '[image message]';
    case 'file':
      return '[file message]';
    case 'video':
      return '[video message]';
    default:
      return `[${msg.msgtype} message]`;
  }
}

// --- inbound: WeCom frame → gc extmsg -------------------------------------

// Dedup by msgid: reconnects can replay frames the bot already handled.
// An id is marked seen only AFTER gc accepts the POST — marking earlier
// would turn a transient gc failure into permanent message loss when the
// SDK replays the frame. inflightMsgIds suppresses concurrent duplicates
// of a not-yet-accepted id.
const seenMsgIds = new Set();
const seenMsgIdOrder = [];
const seenMsgIdCap = 2048;
const inflightMsgIds = new Set();

function markSeen(msgid) {
  if (!msgid || seenMsgIds.has(msgid)) return;
  seenMsgIds.add(msgid);
  seenMsgIdOrder.push(msgid);
  if (seenMsgIdOrder.length > seenMsgIdCap) {
    seenMsgIds.delete(seenMsgIdOrder.shift());
  }
}

async function bridgeInbound(cfg, frame) {
  const msg = frame.body;
  if (!msg) return;
  if (msg.msgid && (seenMsgIds.has(msg.msgid) || inflightMsgIds.has(msg.msgid))) return;

  const conversation = conversationForMessage(cfg, msg);
  if (!conversation.conversation_id) {
    log(`inbound ${msg.msgid}: no conversation id (chattype=${msg.chattype}); dropped`);
    return;
  }
  const text = renderText(msg);
  if (!text) return;

  const message = {
    provider_message_id: msg.msgid,
    conversation,
    actor: {
      id: msg.from?.userid ?? '',
      display_name: msg.from?.userid ?? '',
      is_bot: false,
    },
    // No explicit_target: routing is the default_route fragment's job
    // (and per-conversation bindings). Stamping an addressee here would
    // mislabel messages on rebound conversations — gc carries it into the
    // reminder and tells the receiving agent the message was addressed to
    // someone else.
    text,
    dedup_key: msg.msgid,
    received_at: msg.create_time
      ? new Date(msg.create_time * 1000).toISOString()
      : new Date().toISOString(),
  };

  const target = `${cfg.gcAPIBase}/v0/city/${encodeURIComponent(cfg.cityName)}/extmsg/inbound`;
  if (msg.msgid) inflightMsgIds.add(msg.msgid);
  try {
    // Transient gc failures retry indefinitely — WeCom replay after a
    // reconnect is not guaranteed, so the retry loop is the delivery
    // mechanism, not a bonus. The in-flight marker holds for the whole
    // retry so a replay arriving mid-retry can't double-post.
    await postJSONWithRetry(target, { message }, `inbound ${msg.msgid}`);
    markSeen(msg.msgid);
    log(`inbound ${msg.msgid} → gc (${conversation.kind} ${conversation.conversation_id}, ${msg.msgtype})`);
  } catch (err) {
    // Only deterministic 4xx rejections land here (transient failures
    // retry forever): a replay would fail identically, so mark seen to
    // stop pointless re-posts.
    markSeen(msg.msgid);
    log(`inbound ${msg.msgid} rejected by gc (${err.message}); dropped`);
  } finally {
    if (msg.msgid) inflightMsgIds.delete(msg.msgid);
  }
}

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
        // Never the entry just inserted for `key` — it has no promise
        // yet, so an unguarded scan would evict it immediately and a
        // concurrent same-key retry would double-send.
        if (k !== key && !s.promise) {
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
    // ship. Point the reply flow at the verb that exists.
    reply_instructions: `gc wecom publish --chat {conversation_id} --text '<your reply>'`,
    capabilities: {
      SupportsChildConversations: false,
      SupportsAttachments: false,
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
  const wsOptions = { botId: cfg.botId, secret: cfg.botSecret, maxReconnectAttempts: -1, logger: sdkLogger };
  if (cfg.wsURL) wsOptions.wsUrl = cfg.wsURL;
  const wsClient = new WSClient(wsOptions);

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

  // Per-conversation serial delivery: with independent async bridges, a
  // transient failure on message A would let a later message B land in gc
  // first, reversing conversation context. Chain frames per conversation;
  // separate conversations still proceed concurrently. Entries are removed
  // once their chain drains, so memory tracks active conversations only.
  const convoChains = new Map();
  const enqueueInbound = (frame) => {
    const msg = frame?.body ?? {};
    const key = msg.chattype === 'group' ? msg.chatid : msg.from?.userid;
    const prev = convoChains.get(key) ?? Promise.resolve();
    const next = prev.then(() => bridgeInbound(cfg, frame)).catch((err) => log(`bridge error: ${err.message}`));
    convoChains.set(key, next);
    next.finally(() => {
      if (convoChains.get(key) === next) convoChains.delete(key);
    });
  };
  for (const evt of ['message.text', 'message.voice', 'message.mixed', 'message.image', 'message.file', 'message.video']) {
    wsClient.on(evt, enqueueInbound);
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
