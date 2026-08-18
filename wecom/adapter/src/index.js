// gc-wecom-adapter — the WeCom (企业微信) AI Bot ↔ gc bridge.
//
// A WeCom Smart Robot in API mode ("Long Connection") speaks over an
// OUTBOUND WebSocket to wss://openws.work.weixin.qq.com — no public
// endpoint, no funnel, no inbound TLS. This adapter keeps that long
// connection via Tencent's official @wecom/aibot-node-sdk and bridges both
// directions to gc:
//
//   - Inbound: text / voice / mixed messages (and file/image/video
//     placeholders) → POST /v0/city/{city}/extmsg/inbound, addressed to
//     the mayor session (override with WECOM_INBOUND_TARGET). WeCom
//     transcribes voice server-side, so voice frames carry text.
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
//	GC_API_BASE_URL        gc API base (default http://127.0.0.1:9443).
//
// Optional env:
//
//	LISTEN_INTERNAL        TCP bind when GC_SERVICE_SOCKET is unset
//	                       (default 127.0.0.1:8790).
//	REGISTER_ON_START      "true" (default) self-registers as an extmsg adapter.
//	ADAPTER_PROVIDER       extmsg provider name (default "wecom").
//	WECOM_INBOUND_TARGET   Session handle inbound messages address (default "mayor").
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
    gcAPIBase: getenv('GC_API_BASE_URL', 'http://127.0.0.1:9443').replace(/\/+$/, ''),
    provider: getenv('ADAPTER_PROVIDER', 'wecom'),
    inboundTarget: getenv('WECOM_INBOUND_TARGET', 'mayor'),
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
    cfg.internalCallbackURL = cfg.gcAPIBase + cfg.serviceURLPrefix;
  } else {
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
  });
  if (!resp.ok) {
    const text = (await resp.text().catch(() => '')).trim();
    throw new Error(`${resp.status} ${resp.statusText}: ${text}`);
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
    text,
    explicit_target: cfg.inboundTarget,
    dedup_key: msg.msgid,
    received_at: msg.create_time
      ? new Date(msg.create_time * 1000).toISOString()
      : new Date().toISOString(),
  };

  const target = `${cfg.gcAPIBase}/v0/city/${encodeURIComponent(cfg.cityName)}/extmsg/inbound`;
  if (msg.msgid) inflightMsgIds.add(msg.msgid);
  try {
    await postJSON(target, { message });
    markSeen(msg.msgid);
    log(`inbound ${msg.msgid} → gc (${conversation.kind} ${conversation.conversation_id}, ${msg.msgtype})`);
  } catch (err) {
    // Not marked seen: if the SDK replays this frame after reconnecting,
    // the bridge retries instead of dropping the message.
    log(`inbound ${msg.msgid} POST failed: ${err.message}`);
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
    let messageID = '';
    try {
      for (const chunk of chunkText(pub.text)) {
        const receipt = await wsClient.sendMessage(chatid, {
          msgtype: 'markdown',
          markdown: { content: chunk },
        });
        messageID = receipt?.headers?.req_id ?? messageID;
      }
    } catch (err) {
      log(`publish → ${chatid} failed: ${err.message}`);
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'provider_error',
      }));
      return;
    }
    log(`publish → ${chatid} delivered (session=${pub.session_id ?? ''})`);
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
      conversation: convo,
      message_id: messageID,
      delivered: true,
    }));
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

  const wsOptions = { botId: cfg.botId, secret: cfg.botSecret };
  if (cfg.wsURL) wsOptions.wsUrl = cfg.wsURL;
  const wsClient = new WSClient(wsOptions);

  const server = startInternalListener(cfg, wsClient);

  wsClient.on('authenticated', () => log('wecom long connection authenticated'));
  wsClient.on('reconnecting', (attempt) => log(`wecom reconnecting (attempt ${attempt})`));
  wsClient.on('error', (err) => log(`wecom ws error: ${err.message}`));
  wsClient.on('event.disconnected_event', () => {
    // The server drops the old connection when a NEW connection authenticates
    // with the same bot — usually a second adapter instance. Surface loudly.
    log('wecom server disconnected this connection: a newer connection took over');
  });

  for (const evt of ['message.text', 'message.voice', 'message.mixed', 'message.image', 'message.file', 'message.video']) {
    wsClient.on(evt, (frame) => { bridgeInbound(cfg, frame).catch((err) => log(`bridge error: ${err.message}`)); });
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
    try {
      await registerAdapter(cfg);
    } catch (err) {
      // Registration is retried on next restart; inbound POSTs fail closed
      // until gc knows the provider, so surface the reason.
      log(`adapter registration failed: ${err.message}`);
    }
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
