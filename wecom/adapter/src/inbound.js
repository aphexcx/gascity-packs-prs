// inbound.js — the WeCom-frame → gc-extmsg pipeline, extracted from
// index.js so the wiring (early hydration, replay dedup, per-conversation
// ordering, delivery retry, cleanup) is testable with a fake downloader
// and a fake gc — no WebSocket, no live gc (jg-c7j codex round-1).
// index.js owns process concerns (config, WS client, listener, signals)
// and instantiates one pipeline; tests instantiate their own with
// injected deps. All dedup/ordering state is per-instance.

import { hydrateMessageMedia, mediaItemsForMessage } from './media.js';

// --- gc extmsg wire helpers (wire-compatible with internal/extmsg) ----------

export async function postJSON(target, body) {
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

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// postJSONWithRetry retries transient failures indefinitely on a backoff
// ladder capped at 60s, and rethrows the first non-retryable error. The
// retry is the delivery mechanism — WeCom replay is not guaranteed, so a
// gc outage of any length must not translate into dropped messages. Memory
// is bounded by traffic during the outage: one small pending frame per
// undelivered message.
const retryDelaysMs = [1000, 5000, 15000, 60000];

export async function postJSONWithRetry(target, body, label, log = () => {}) {
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

// --- frame rendering ---------------------------------------------------------

export function conversationForMessage(cfg, msg) {
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
// text gc transports. Media placeholders stay as the base text; the
// hydration block (download+decrypt+store, src/media.js) is appended by
// bridgeInbound — so a message whose hydration failed entirely still
// reads as '[file message]' plus the failure note, never as silence.
export function renderText(msg) {
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

// --- pipeline ----------------------------------------------------------------

// createInboundPipeline wires frames to gc.
//
// deps:
//   cfg           cityName, provider, botId, gcAPIBase, mediaDir,
//                 mediaMaxBytes, mediaUrlTtlMs
//   downloadFile  (url, aeskey) → {buffer, filename} — SDK download+decrypt,
//                 wall-clock bounded by the caller
//   transcribe    (buffer, filename) → transcript (audio files)
//   gate          shared download-admission gate (createDownloadGate)
//   quota         durable-store guard (createStoreQuota)
//   postInbound   (target, body, label, log) — delivery; defaults to the
//                 retry-forever poster above (tests inject a fake gc)
//   now, log      clock and logger
//
// enqueueInbound returns the chained bridge promise (settles when this
// frame's delivery attempt settles) — production ignores it; tests await.
export function createInboundPipeline(deps) {
  const {
    cfg,
    downloadFile,
    transcribe = null,
    gate = null,
    quota = null,
    postInbound = postJSONWithRetry,
    now = Date.now,
    log = () => {},
  } = deps;

  // Dedup by msgid: reconnects can replay frames the bot already handled.
  // An id is marked seen only AFTER gc accepts the POST — marking earlier
  // would turn a transient gc failure into permanent message loss when the
  // SDK replays the frame. inflightMsgIds suppresses concurrent duplicates
  // of a not-yet-accepted id.
  const seenMsgIds = new Set();
  const seenMsgIdOrder = [];
  const seenMsgIdCap = 2048;
  const inflightMsgIds = new Set();

  // In-flight media hydrations keyed by msgid: an SDK frame replay arriving
  // while the first copy's download is still running reuses the same
  // promise instead of downloading (and storing) the bytes a second time.
  // Entries normally leave when the message's bridge settles; the cap and
  // TTL bound the map through pathological cases (a gc outage holding
  // hundreds of bridges in retry, a bridge path that never consumed its
  // entry) — evicting early only costs dedup for a replay whose URL is
  // dead by then anyway.
  const hydrations = new Map(); // msgid → { promise, at }
  const hydrationsCap = 512;
  const hydrationsTtlMs = 30 * 60 * 1000;

  function sweepHydrations() {
    // Map iterates in insertion order, so the front is always the oldest:
    // evict stale entries, and beyond that the oldest entries whenever the
    // map is full (leaving room for the entry about to be inserted).
    const cutoff = now() - hydrationsTtlMs;
    for (const [k, v] of hydrations) {
      if (v.at > cutoff && hydrations.size < hydrationsCap) break;
      hydrations.delete(k);
    }
  }

  function markSeen(msgid) {
    if (!msgid || seenMsgIds.has(msgid)) return;
    seenMsgIds.add(msgid);
    seenMsgIdOrder.push(msgid);
    if (seenMsgIdOrder.length > seenMsgIdCap) {
      seenMsgIds.delete(seenMsgIdOrder.shift());
    }
  }

  // startHydration kicks off download+decrypt+store for a media frame the
  // moment it arrives — BEFORE the per-conversation bridge chain, which can
  // sit arbitrarily long behind a gc-outage retry loop while the WeCom
  // download URL burns through its ~5-minute lifetime. Returns null for
  // non-media frames and for msgids already delivered (the bridge drops
  // those anyway).
  function startHydration(msg) {
    if (!msg || mediaItemsForMessage(msg).length === 0) return null;
    if (msg.msgid) {
      if (seenMsgIds.has(msg.msgid)) return null;
      const existing = hydrations.get(msg.msgid);
      if (existing) return existing.promise;
    }
    const promise = hydrateMessageMedia(msg, {
      downloadFile,
      mediaDir: cfg.mediaDir,
      maxBytes: cfg.mediaMaxBytes,
      urlTtlMs: cfg.mediaUrlTtlMs,
      transcribe,
      gate,
      quota,
      now,
      log,
    });
    if (msg.msgid) {
      sweepHydrations();
      hydrations.set(msg.msgid, { promise, at: now() });
    }
    return promise;
  }

  async function bridgeInbound(frame, hydration) {
    const msg = frame.body;
    if (!msg) return;
    if (msg.msgid && (seenMsgIds.has(msg.msgid) || inflightMsgIds.has(msg.msgid))) return;

    const conversation = conversationForMessage(cfg, msg);
    if (!conversation.conversation_id) {
      log(`inbound ${msg.msgid}: no conversation id (chattype=${msg.chattype}); dropped`);
      if (msg.msgid) hydrations.delete(msg.msgid);
      return;
    }
    let text = renderText(msg);
    if (!text) {
      if (msg.msgid) hydrations.delete(msg.msgid);
      return;
    }

    // Media hydration started the moment the frame arrived (the download
    // URL is on a 5-minute fuse — it cannot wait behind this conversation's
    // gc retry queue); by the time this chained bridge runs, the promise is
    // usually already settled. hydrateMessageMedia never rejects, but a
    // defensive catch keeps an unforeseen bug from dropping the message:
    // worst case the agent sees the bare placeholder text.
    let attachments = [];
    if (hydration) {
      const hydrated = await hydration.catch((err) => {
        log(`inbound ${msg.msgid}: media hydration error: ${err.message}`);
        return { attachments: [], block: '' };
      });
      attachments = hydrated.attachments;
      if (hydrated.block) text = `${text}\n${hydrated.block}`;
    }

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
      ...(attachments.length > 0 ? { attachments } : {}),
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
      await postInbound(target, { message }, `inbound ${msg.msgid}`, log);
      markSeen(msg.msgid);
      log(`inbound ${msg.msgid} → gc (${conversation.kind} ${conversation.conversation_id}, ${msg.msgtype})`);
    } catch (err) {
      // Only deterministic 4xx rejections land here (transient failures
      // retry forever): a replay would fail identically, so mark seen to
      // stop pointless re-posts.
      markSeen(msg.msgid);
      log(`inbound ${msg.msgid} rejected by gc (${err.message}); dropped`);
    } finally {
      if (msg.msgid) {
        inflightMsgIds.delete(msg.msgid);
        hydrations.delete(msg.msgid);
      }
    }
  }

  // Per-conversation serial delivery: with independent async bridges, a
  // transient failure on message A would let a later message B land in gc
  // first, reversing conversation context. Chain frames per conversation;
  // separate conversations still proceed concurrently. Entries are removed
  // once their chain drains, so memory tracks active conversations only.
  const convoChains = new Map();
  const enqueueInbound = (frame) => {
    const msg = frame?.body ?? {};
    // Hydration starts NOW, outside the chain — the media download URL
    // expires ~5 minutes after this frame, and the chain can be stuck
    // behind an earlier message's gc retry loop for longer than that.
    const hydration = startHydration(msg);
    const key = msg.chattype === 'group' ? msg.chatid : msg.from?.userid;
    const prev = convoChains.get(key) ?? Promise.resolve();
    const next = prev.then(() => bridgeInbound(frame, hydration)).catch((err) => log(`bridge error: ${err.message}`));
    convoChains.set(key, next);
    next.finally(() => {
      if (convoChains.get(key) === next) convoChains.delete(key);
    });
    return next;
  };

  return {
    enqueueInbound,
    // Introspection for tests and diagnostics only — not a public surface.
    stats: () => ({
      hydrations: hydrations.size,
      inflight: inflightMsgIds.size,
      seen: seenMsgIds.size,
      chains: convoChains.size,
    }),
  };
}
