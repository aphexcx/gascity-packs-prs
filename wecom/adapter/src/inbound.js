// inbound.js — the WeCom-frame → gc-extmsg pipeline, extracted from
// index.js so the wiring (early hydration, replay dedup, per-conversation
// ordering, delivery retry, cleanup) is testable with a fake downloader
// and a fake gc — no WebSocket, no live gc (jg-c7j codex round-1).
// index.js owns process concerns (config, WS client, listener, signals)
// and instantiates one pipeline; tests instantiate their own with
// injected deps. All dedup/ordering state is per-instance.

import { hydrateMessageMedia, mediaItemsForMessage, neutralizeMarkupBoundaries } from './media.js';

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
  // Provider-controlled ids are coerced to strings when present (codex
  // jg-p1mk r3 finding 1): gc's wire types require strings, and a
  // JSON-valid numeric id would otherwise make gc reject the whole
  // envelope — in a batch, losing the healthy siblings with it.
  const isGroup = msg.chattype === 'group';
  const id = isGroup ? msg.chatid : msg.from?.userid;
  return {
    scope_id: cfg.cityName,
    provider: cfg.provider,
    account_id: cfg.botId,
    conversation_id: id == null ? id : String(id),
    kind: isGroup ? 'room' : 'dm',
  };
}

// dedupeAsrRepeats collapses a WeCom voice transcript that is one block
// repeated verbatim 2–4 times (jg-p1mk add C — observed nightly on 8/22:
// every long voice message's server-side ASR arrived with the same text
// duplicated 2-3x, with no separator or a single space/newline between
// copies). Detection is EXACT repetition of the whole content only, and
// only for blocks of ≥ 10 characters in content of ≥ 24 — a polite
// doubling like 好的好的 (or an emphatic 重要的事情说三遍 triple) must
// never be "deduped". Runs to a fixed point so a 4x repeat expressed as
// 2x-of-2x still collapses fully. Returns { text, repeats };
// repeats === 1 means untouched.
//
// False-positive stance (codex r2 finding 6): a human deliberately
// repeating a phrase (重要的事情说三遍-style) IS collapsible when it
// exceeds the guards, and no provider signal distinguishes that from
// the ASR bug. Accepted trade-off per the mayor's order — the observed
// bug hits EVERY long voice message, the collapse is disclosed in-text
// by the (ASR重复×N已折叠) marker (never silent), and the guards below
// (blocks ≥ 10 chars, total ≥ 24 chars) keep short emphatic repeats
// untouched. Deliberate ≥24-char triple repeats collapsing is the cost
// of the ~60% payload cut on genuinely broken transcripts.
const asrRepeatSeparators = ['', ' ', '\n'];
const asrMinBlockChars = 10;
const asrMinContentChars = 24;

export function dedupeAsrRepeats(content) {
  let text = String(content ?? '');
  if (text.length < asrMinContentChars) return { text, repeats: 1 };
  let repeats = 1;
  for (;;) {
    let hit = null;
    for (const sep of asrRepeatSeparators) {
      for (let k = 2; k <= 4 && !hit; k++) {
        const blockLen = (text.length - sep.length * (k - 1)) / k;
        if (!Number.isInteger(blockLen) || blockLen < asrMinBlockChars) continue;
        const block = text.slice(0, blockLen);
        if (Array(k).fill(block).join(sep) === text) hit = { block, k };
      }
      if (hit) break;
    }
    if (!hit) break;
    text = hit.block;
    repeats *= hit.k;
  }
  return { text, repeats };
}

// renderText flattens each supported WeCom message type into the plain
// text gc transports. Media placeholders stay as the base text; the
// hydration block (download+decrypt+store, src/media.js) is appended by
// bridgeInbound — so a message whose hydration failed entirely still
// reads as '[file message]' plus the failure note, never as silence.
// onAsrDedup (optional) is called with {repeats, fromChars, toChars}
// when a voice transcript's ASR repeats were collapsed — the bridge logs
// counts there; transcript CONTENT never reaches a log.
export function renderText(msg, onAsrDedup = null) {
  switch (msg.msgtype) {
    case 'text':
      return msg.text?.content ?? '';
    case 'voice': {
      // WeCom transcribes voice server-side; content IS the transcript.
      if (!msg.voice?.content) return '[voice message]';
      const { text, repeats } = dedupeAsrRepeats(msg.voice.content);
      if (repeats === 1) return `[voice] ${msg.voice.content}`;
      onAsrDedup?.({ repeats, fromChars: msg.voice.content.length, toChars: text.length });
      // The marker keeps the collapse auditable in the delivered text —
      // the no-content-logging policy forbids keeping the original in
      // the service log.
      return `[voice] ${text} (ASR重复×${repeats}已折叠)`;
    }
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
    case 'event':
      // Only feedback events are enqueued (index.js wires
      // event.feedback_event into the pipeline); other event kinds are
      // handled at the WS layer or ignored, but render defensively.
      return msg.event?.eventtype === 'feedback_event'
        ? renderFeedbackText(msg)
        : `[${msg.event?.eventtype ?? 'event'} event]`;
    default:
      return `[${msg.msgtype} message]`;
  }
}

// renderFeedbackText flattens a WeCom feedback_event callback (jg-mlfs)
// into the lightweight signal delivered to the bound session. Wire shape
// (aibot 接收事件 doc): event.feedback_event = { id, type, content,
// inaccurate_reason_list } — type 1 = 👍, 2 = 👎 (with optional free-text
// content and reason codes), 3 = the user withdrew earlier feedback. The
// id is the adapter-minted feedback id attached to the outbound markdown
// send (outbound.js), so the session/operator can correlate the rating
// with the exact reply via the publish log's feedback_base. The whole
// line is neutralized: content is sender-controlled free text.
const feedbackTypeLabels = {
  1: '👍 praise',
  2: '👎 negative',
  3: 'feedback withdrawn',
};

const inaccurateReasonLabels = {
  1: 'unrelated to the question',
  2: 'incomplete information',
  3: 'factual errors',
  4: 'data/analysis problems',
};

export function renderFeedbackText(msg) {
  const fb = msg?.event?.feedback_event ?? {};
  const kind = feedbackTypeLabels[fb.type] ?? `feedback type ${fb.type ?? 'unknown'}`;
  let text = `[user feedback] ${kind} from ${msg?.from?.userid ?? 'unknown'} on bot reply`;
  if (fb.id) text += ` feedback_id=${fb.id}`;
  if (fb.type === 2) {
    const reasons = (Array.isArray(fb.inaccurate_reason_list) ? fb.inaccurate_reason_list : [])
      .map((code) => inaccurateReasonLabels[code] ?? `reason ${code}`);
    if (reasons.length > 0) text += ` (${reasons.join(', ')})`;
    if (fb.content) text += ` — “${fb.content}”`;
  }
  return neutralizeMarkupBoundaries(text);
}

// replyHelpBlock is the full reply how-to, delivered once per
// conversation per adapter lifetime, appended to that conversation's
// first forwarded inbound (jg-p1mk add D — the gp-729 item-3 port). The
// registered reply_instructions template (index.js) is a single line
// rendered into every reminder; this block carries the mechanics that
// used to ride every message: file-based reply hygiene and the media
// publish flags with their confinement constraint.
export function replyHelpBlock(conversationId) {
  const id = neutralizeMarkupBoundaries(conversationId ?? '');
  return `[conversation ${id} — full reply how-to, sent once per chat per adapter session]\n`
    + `To reply: write your reply to a file, then run: gc wecom publish --chat ${id} --text-file <path>. `
    + 'File-based so arbitrary reply text (apostrophes, code, Chinese quotes) never needs shell escaping.\n'
    + `To send an image or video: gc wecom publish --chat ${id} --image <path> (or --video <path>), `
    + "optional --text caption; media files must live under the adapter's WECOM_OUTBOUND_MEDIA_ROOT directory.";
}

// emptyPayloadNote detects a media-bearing message whose payload is
// MISSING — the 8/22 22:53 real case was a voice frame with neither a
// server-side transcript nor downloadable media, which delivered as a
// bare '[voice message]' with zero log lines. Anything the sender meant
// to convey but WeCom did not deliver must surface as an explicit marker
// in the delivered text plus a loud log line (bridge below), never as
// silence.
export function emptyPayloadNote(msg) {
  switch (msg?.msgtype) {
    case 'voice':
      return msg.voice?.content
        ? ''
        : '[voice payload empty — 语音转写失败/内容缺失: WeCom delivered neither a transcript nor '
          + 'downloadable media for this voice message; ask the sender to re-send or type it]';
    case 'image':
    case 'file':
    case 'video':
      return mediaItemsForMessage(msg).length > 0
        ? ''
        : `[${msg.msgtype} payload empty — 内容缺失: WeCom delivered no downloadable media URL for this `
          + `${msg.msgtype} message; ask the sender to re-send]`;
    case 'mixed': {
      const parts = msg.mixed?.msg_item ?? [];
      if (parts.length === 0) {
        return '[mixed payload empty — 内容缺失: WeCom delivered a mixed message with no items]';
      }
      const missing = parts.filter((item) => item?.msgtype === 'image' && !item.image?.url).length;
      return missing > 0
        ? `[${missing} image(s) in this mixed message carried no download URL — 内容缺失: `
          + 'ask the sender to re-send them]'
        : '';
    }
    default:
      return '';
  }
}

// conversationKeyFor derives the pipeline's INTERNAL per-conversation
// key. Namespaced by chat type (codex jg-p1mk r1 finding 1): a DM
// peer's userid and a group's chatid live in different WeCom id spaces,
// and a collision between them must never share a coalescing buffer,
// chain, peer-context buffer, or reply-help mark — a merged buffer
// would deliver a private DM into the group-bound gc conversation. gc's
// own conversation identity already distinguishes the two via `kind`;
// this key mirrors that.
export function conversationKeyFor(chattypeOrKind, id) {
  const isGroup = chattypeOrKind === 'group' || chattypeOrKind === 'room';
  return id == null ? undefined : `${isGroup ? 'g' : 'd'}:${id}`;
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
//   transcribeGate shared transcription-admission gate (deadline-less)
//   quota         durable-store guard (createStoreQuota)
//   postInbound   (target, body, label, log) — delivery; defaults to the
//                 retry-forever poster above (tests inject a fake gc)
//   hydrationsCap / hydrationsTtlMs
//                 replay-cache bounds (defaults 512 / 30min; test knobs)
//   seenMsgIdCap  seen-set bound (default 2048; test knob)
//   coalesceWindowMs
//                 per-conversation burst-coalescing window (default
//                 cfg.coalesceWindowMs, then 0 = disabled); frames for
//                 one conversation arriving within the window deliver
//                 as ONE combined gc POST (jg-p1mk item 3)
//   coalesceMaxBatch
//                 early-flush threshold (default 50); a buffer this full
//                 delivers immediately instead of waiting out its window
//   now, log      clock and logger
//
// enqueueInbound returns a promise that settles when this frame's
// delivery attempt settles (through its coalesced batch when buffering
// is active) — production ignores it; tests await.
export function createInboundPipeline(deps) {
  const {
    cfg,
    downloadFile,
    transcribe = null,
    gate = null,
    transcribeGate = null,
    quota = null,
    postInbound = postJSONWithRetry,
    hydrationsCap = 512,
    hydrationsTtlMs = 30 * 60 * 1000,
    seenMsgIdCap = 2048,
    replyHelpOnce = true,
    now = Date.now,
    log = () => {},
  } = deps;

  // Dedup by msgid: reconnects can replay frames the bot already handled.
  // An id is marked seen only AFTER gc accepts the POST — marking earlier
  // would turn a transient gc failure into permanent message loss when the
  // SDK replays the frame. inflightMsgIds suppresses concurrent duplicates
  // of a not-yet-accepted id.
  const seenMsgIds = new Set();
  // Eviction bookkeeping (codex r5): seenMsgIdOrder is append-only with a
  // cursor (seenHead) marking the logical front — slots behind the cursor
  // are dead. seenNonPending is a RUNNING count of marks with no pending
  // frames: the cap governs exactly that population, so eviction decisions
  // never rescan the array, and marks flip in and out of the count at the
  // pending transitions (addPending / removePending below).
  let seenMsgIdOrder = [];
  let seenHead = 0;
  let seenNonPending = 0;
  // msgid → its live slot index in seenMsgIdOrder. A slot is live iff this
  // map still points at it — eviction deletes the entry, so a re-marked id
  // gets a fresh slot and the stale one reads as dead. Also lets
  // removePending REWIND the cursor to a settling mark's position, keeping
  // eviction oldest-non-pending-first without rescanning.
  const seenIndex = new Map();
  const inflightMsgIds = new Set();

  // In-flight media hydrations keyed by msgid: an SDK frame replay arriving
  // while the first copy's download is still running reuses the same
  // promise instead of downloading (and storing) the bytes a second time.
  // Entries normally leave when the message's bridge settles.
  //
  // Bounding (codex r2): every entry between insert and bridge-settle is
  // LIVE — its dedup is load-bearing (a 513-message outage where the
  // oldest live entry got evicted downloaded every attachment twice). So
  // the map is bounded by REFUSING NEW admissions at the cap (the refused
  // message still delivers, with an explicit not-ingested note), never by
  // evicting live entries. The TTL sweep is leak insurance only, and it
  // skips msgids whose bridge is mid-delivery (inflight) — an arbitrarily
  // long gc outage must not reopen the double-download window.
  const hydrations = new Map(); // msgid → { promise, at }

  // Pending ownership (codex r3): a msgid is "pending" from the moment a
  // frame for it is ENQUEUED until that frame's bridge settles — not just
  // while its POST is in flight (inflightMsgIds), which starts only after
  // the bridge unblocks. A media message queued behind an earlier
  // same-conversation delivery has a live, load-bearing hydration entry
  // long before inflight is set; the TTL sweep must never evict it.
  // Refcounted because replays enqueue additional frames for the same
  // msgid: the mark holds until the LAST enqueued frame settles.
  const pendingMsgIds = new Map(); // msgid → count of enqueued, unsettled frames

  function addPending(msgid) {
    if (!msgid) return;
    const count = pendingMsgIds.get(msgid) ?? 0;
    pendingMsgIds.set(msgid, count + 1);
    // A seen mark whose msgid becomes pending again (a replay of an old
    // delivered id) leaves the cap accounting until it drains — its dedup
    // is load-bearing for exactly that replay.
    if (count === 0 && seenMsgIds.has(msgid)) seenNonPending--;
  }

  function removePending(msgid) {
    if (!msgid) return;
    const count = pendingMsgIds.get(msgid);
    if (count === undefined) return;
    if (count <= 1) {
      pendingMsgIds.delete(msgid);
      // The refusal decision (below) is cached for the message's pending
      // lifetime; once no frame for this msgid is outstanding, a future
      // replay may legitimately re-evaluate admission.
      refusals.delete(msgid);
      // The mark (if this msgid has one) re-enters cap accounting; rewind
      // the cursor to its slot so the retained OLD mark rolls off first
      // (never a newer one), then re-trim any excess its exemption forced
      // trimSeen to retain.
      if (seenMsgIds.has(msgid)) {
        seenNonPending++;
        const slot = seenIndex.get(msgid);
        if (slot !== undefined && slot < seenHead) seenHead = slot;
      }
      trimSeen();
    } else {
      pendingMsgIds.set(msgid, count - 1);
    }
  }

  // Conversations that already received the full reply how-to this
  // adapter lifetime (jg-p1mk add D). Real deployments hold dozens of
  // chats, but the ids are provider-controlled — cap the set anyway
  // (codex r4 finding 3) and evict oldest-first: the cost of eviction
  // is one re-sent help block, never loss.
  const replyHelpCap = deps.replyHelpCap ?? 512;
  const replyHelpSent = new Set();

  function markReplyHelpSent(convKey) {
    replyHelpSent.add(convKey);
    while (replyHelpSent.size > replyHelpCap) {
      replyHelpSent.delete(replyHelpSent.values().next().value);
    }
  }

  // --- peer-bot context buffering (jg-p1mk add A; slack-full gp-kop port) ---
  //
  // Room posts authored by allowlisted peer bots (cfg.peerBotUserIds —
  // other mayors' bots sharing a group chat) must NOT wake the bound
  // session: they buffer here as tagged read-only context and ride with
  // the conversation's next real human delivery. In-memory, capped both
  // ways: per conversation, past peerContextCap the OLDEST entries drop
  // with a count (context is best-effort by contract — unlike human
  // messages, a dropped peer line is an acceptable loss and the block
  // says how many); and across conversations, past
  // peerConversationsCap the least-recently-touched conversation's
  // whole buffer drops, logged (codex r2 finding 7 — a peer bot
  // spanning many one-shot rooms, or hostile frames varying chatid,
  // must not grow the map without bound). msgid-deduped so an SDK
  // replay of a buffered peer frame cannot double a line — including
  // across a rejection-restore cycle (codex r2 finding 9). Every item
  // carries the pipeline arrival sequence so delivery can preserve
  // provider order relative to the human batch (codex r2 finding 8).
  const peerContextCap = deps.peerContextCap ?? 20;
  const peerConversationsCap = deps.peerConversationsCap ?? 64;
  const peerBotUserIds = new Set(cfg.peerBotUserIds ?? []);
  const peerContexts = new Map(); // convKey → { items: [{msgid, userid, text, seq}], seen: Set, dropped: n, touchedAt }

  // Monotonic arrival sequence shared by peer items and pipeline frames.
  let arrivalSeq = 0;

  function bufferPeerContext(key, msg) {
    // Render FIRST, before any map mutation (codex r2 finding 2): a
    // hostile peer frame whose render throws must neither crash through
    // the SDK's emitter nor leave an empty buffer entry behind.
    let text;
    try {
      text = renderText(msg);
    } catch (err) {
      log(`peer context: post from ${msg.from?.userid} in ${key} unrenderable (${err.message}); dropped`);
      return;
    }
    let ctx = peerContexts.get(key);
    if (!ctx) {
      ctx = { items: [], seen: new Set(), dropped: 0, touchedAt: arrivalSeq++ };
      peerContexts.set(key, ctx);
      evictPeerConversations();
    }
    ctx.touchedAt = arrivalSeq++;
    if (msg.msgid && ctx.seen.has(msg.msgid)) return;
    if (msg.msgid) ctx.seen.add(msg.msgid);
    ctx.items.push({
      msgid: msg.msgid,
      userid: String(msg.from?.userid ?? 'unknown'),
      text,
      seq: arrivalSeq++,
    });
    trimPeerItems(ctx);
    log(`peer context: buffered post from ${msg.from?.userid} in ${key} (pending=${ctx.items.length}); not waking the session`);
  }

  function trimPeerItems(ctx) {
    while (ctx.items.length > peerContextCap) {
      const evicted = ctx.items.shift();
      if (evicted.msgid) ctx.seen.delete(evicted.msgid);
      ctx.dropped++;
    }
  }

  function evictPeerConversations() {
    while (peerContexts.size > peerConversationsCap) {
      let victim = null;
      let victimAt = Infinity;
      for (const [key, ctx] of peerContexts) {
        if (ctx.touchedAt < victimAt) {
          victim = key;
          victimAt = ctx.touchedAt;
        }
      }
      if (victim === null) return;
      const dropped = peerContexts.get(victim);
      peerContexts.delete(victim);
      log(`peer context: evicted the least-recently-touched conversation ${victim} (${dropped.items.length} buffered posts lost) at the ${peerConversationsCap}-conversation cap`);
    }
  }

  // takePeerContext removes and returns the conversation's buffered peer
  // items that arrived BEFORE maxSeq (the carrying batch's newest frame
  // — codex r3 finding 2: an older queued batch, delayed behind a gc
  // retry, must not steal a peer post that arrived after a NEWER human
  // message; that post stays buffered for the delivery it belongs
  // with). restorePeerContext puts a taken set back (ahead of anything
  // buffered meanwhile, deduped by msgid against it) when the carrying
  // delivery was rejected.
  function takePeerContext(key, maxSeq = Infinity) {
    const ctx = peerContexts.get(key);
    if (!ctx) return null;
    const takenItems = ctx.items.filter((item) => item.seq === undefined || item.seq < maxSeq);
    if (takenItems.length === 0 && ctx.dropped === 0) return null;
    const remaining = ctx.items.filter((item) => !(item.seq === undefined || item.seq < maxSeq));
    const taken = {
      items: takenItems,
      seen: new Set(takenItems.filter((item) => item.msgid).map((item) => item.msgid)),
      dropped: ctx.dropped,
      touchedAt: ctx.touchedAt,
    };
    if (remaining.length === 0) {
      peerContexts.delete(key);
    } else {
      ctx.items = remaining;
      ctx.seen = new Set(remaining.filter((item) => item.msgid).map((item) => item.msgid));
      ctx.dropped = 0; // historical drops report with the taken block
    }
    return taken;
  }

  function restorePeerContext(key, taken) {
    if (!taken) return;
    const current = peerContexts.get(key);
    if (!current) {
      taken.touchedAt = arrivalSeq++;
      peerContexts.set(key, taken);
      evictPeerConversations();
      return;
    }
    // A peer frame replayed while the carrying POST was in flight sits
    // in `current` already — the merge must keep exactly one copy, and
    // the ORIGINAL (older-seq) one, so arrival order survives the
    // rejection cycle (codex r2 finding 9; r4 finding 4: keeping the
    // replayed copy instead put it after posts it actually preceded).
    const replacedByOriginal = current.items.filter((item) => !item.msgid || !taken.seen.has(item.msgid));
    current.items = [...taken.items, ...replacedByOriginal]
      .sort((a, b) => (a.seq ?? -1) - (b.seq ?? -1));
    current.seen = new Set(current.items.filter((item) => item.msgid).map((item) => item.msgid));
    current.dropped += taken.dropped;
    current.touchedAt = arrivalSeq++;
    trimPeerItems(current);
  }

  function formatPeerContextBlock(items, dropped, heading) {
    const noun = items.length === 1 ? 'post' : 'posts';
    const droppedNote = dropped > 0 ? `; ${dropped} older dropped at the ${peerContextCap}-item cap` : '';
    const lines = [`[${heading} — ${items.length} ${noun}${droppedNote}; read-only, no reply expected]`];
    for (const item of items) {
      lines.push(`${neutralizeMarkupBoundaries(item.userid)}: ${item.text}`);
    }
    return lines.join('\n');
  }

  // Backlog refusals cached by msgid (codex r3): a refused message's
  // bridge can sit queued for a long time; if capacity clears meanwhile, a
  // REPLAY must get the SAME refusal — not a fresh hydration that
  // downloads media the already-queued refusal note will never deliver.
  const refusals = new Map(); // msgid → refusal result promise

  function sweepHydrations() {
    // Map iterates in insertion order, so the front is always the oldest.
    const cutoff = now() - hydrationsTtlMs;
    for (const [k, v] of hydrations) {
      if (v.at > cutoff) break;
      if (pendingMsgIds.has(k)) continue; // a bridge still owns it: load-bearing
      hydrations.delete(k);
    }
  }

  function markSeen(msgid) {
    if (!msgid || seenMsgIds.has(msgid)) return;
    seenMsgIds.add(msgid);
    seenIndex.set(msgid, seenMsgIdOrder.length);
    seenMsgIdOrder.push(msgid);
    // A mark made while its msgid still has pending frames (the normal
    // bridge-delivery case — pending closes at the enqueue finally, after
    // this) joins cap accounting later, at removePending.
    if (!pendingMsgIds.has(msgid)) seenNonPending++;
    trimSeen();
  }

  // trimSeen keeps the seen set at its cap but EXEMPTS msgids that still
  // have pending frames (codex r4): a delivered msgid whose replay sits
  // queued behind another delivery relies on the seen check for POST
  // dedup — 2,048 churned messages must not evict it into a double POST
  // (gc does not consume dedup_key). The exemption means the set can
  // exceed the cap by exactly the retained pending marks; it re-trims
  // when a pending msgid's last frame settles (removePending above).
  // Eviction is oldest-non-pending-first — a retained old mark must never
  // push a NEWER mark out in its place (a fresh delivery's replay can be
  // seconds away).
  function trimSeen() {
    // The cap governs the NON-PENDING population only (codex r5): eviction
    // triggers on the running count, never on array length or on what
    // happens to sit at the front — [old(non-pending), pending] + new must
    // keep all three when the non-pending count is within the cap.
    while (seenNonPending > seenMsgIdCap) {
      // Advance past dead slots (evicted or re-marked ids) and past
      // pending-exempt marks; removePending rewinds the cursor to a
      // settling mark's slot, so skipped pending marks are reachable again
      // the moment they re-enter cap accounting.
      const id = seenMsgIdOrder[seenHead];
      if (id === undefined) break; // accounting says evict, nothing evictable — defensive
      if (seenIndex.get(id) !== seenHead || pendingMsgIds.has(id)) {
        seenHead++;
        continue;
      }
      seenMsgIds.delete(id);
      seenIndex.delete(id);
      seenNonPending--;
      seenHead++;
    }
    // Compaction: reclaim DEAD slots (evicted or re-marked ids) once
    // they dominate the array. Two invariants, each the fix for a shipped
    // bug:
    //
    //   - Live slots survive wherever they sit (codex jg-p1mk r5,
    //     pre-existing since jg-c7j): the eviction loop advances
    //     seenHead PAST pending-exempt marks, which are live — the old
    //     slice(seenHead) discarded them into unevictable ghosts (still
    //     in seenMsgIds, stale seenIndex) that consumed the cap and let
    //     a fresh id's replay double-POST.
    //   - The trigger is DEAD-SLOT MASS — the thing compaction actually
    //     reclaims — never cursor position (jg-p1mk post-cap
    //     verification): a cursor-based trigger with a reset-to-zero
    //     cursor re-scanned the exempt prefix and re-compacted the full
    //     array on EVERY delivery once pending marks piled up — O(n²)
    //     on the inbound hot path. Re-triggering now requires another
    //     max(cap, half-the-array) evictions/re-marks, each of which
    //     creates exactly one dead slot: amortized O(1) per delivery.
    //
    // seenIndex.size IS the live-slot count (one live slot per id, by
    // construction). The cursor is remapped, not reset: prefix
    // survivors stay behind it — "already skipped this pass" — and
    // removePending's rewind reaches them the moment they re-enter cap
    // accounting, exactly as before.
    const deadSlots = seenMsgIdOrder.length - seenIndex.size;
    if (deadSlots > seenMsgIdCap && deadSlots * 2 > seenMsgIdOrder.length) {
      const survivors = [];
      let prefixSurvivors = 0;
      for (let i = 0; i < seenMsgIdOrder.length; i++) {
        const id = seenMsgIdOrder[i];
        if (seenIndex.get(id) !== i) continue; // dead or stale duplicate
        if (i < seenHead) prefixSurvivors++;
        survivors.push(id);
      }
      seenMsgIdOrder = survivors;
      seenHead = prefixSurvivors;
      for (let i = 0; i < survivors.length; i++) {
        seenIndex.set(survivors[i], i);
      }
    }
  }

  // startHydration kicks off download+decrypt+store for a media frame the
  // moment it arrives — BEFORE the per-conversation bridge chain, which can
  // sit arbitrarily long behind a gc-outage retry loop while the WeCom
  // download URL burns through its ~5-minute lifetime. Returns null for
  // non-media frames and for msgids already delivered (the bridge drops
  // those anyway); otherwise a handle { promise, owner } — owner is true
  // only for the frame whose call INSERTED the hydrations entry, and only
  // that frame's bridge may delete it (a non-owner replay deleting the
  // entry it merely reused would strip the original's dedup).
  function startHydration(msg) {
    if (!msg) return null;
    const itemCount = mediaItemsForMessage(msg).length;
    if (itemCount === 0) return null;
    if (msg.msgid) {
      if (seenMsgIds.has(msg.msgid)) return null;
      const existing = hydrations.get(msg.msgid);
      if (existing) return { promise: existing.promise, owner: false };
      // A refusal sticks for the message's whole pending lifetime: if
      // capacity cleared while the refused bridge is still queued, a
      // replay admitted here would download media that the queued refusal
      // note never delivers (codex r3).
      const refused = refusals.get(msg.msgid);
      if (refused) return { promise: refused, owner: false };
      sweepHydrations();
      if (hydrations.size >= hydrationsCap) {
        // Backlog full (hydrationsCap media messages already awaiting gc
        // delivery): refuse NEW ingestion rather than evict a live entry
        // whose dedup is protecting an in-flight delivery. The message
        // still delivers with an honest note; by the time the backlog
        // could clear, this URL would be dead anyway.
        log(`inbound ${msg.msgid}: hydration backlog full (${hydrationsCap}); media not ingested`);
        const noun = itemCount === 1 ? 'file' : 'files';
        const refusal = Promise.resolve({
          attachments: [],
          block: `[${itemCount} WeCom ${noun} attached]\n  media not ingested: the adapter's hydration backlog is full (${hydrationsCap} media messages awaiting gc delivery); WeCom download URLs expire ~5 minutes after receipt, so the bytes are not recoverable — ask the sender to re-send once delivery recovers`,
        });
        refusals.set(msg.msgid, refusal);
        return { promise: refusal, owner: false };
      }
    }
    const promise = hydrateMessageMedia(msg, {
      downloadFile,
      mediaDir: cfg.mediaDir,
      maxBytes: cfg.mediaMaxBytes,
      urlTtlMs: cfg.mediaUrlTtlMs,
      transcribe,
      gate,
      transcribeGate,
      quota,
      now,
      log,
    });
    if (msg.msgid) {
      hydrations.set(msg.msgid, { promise, at: now() });
      return { promise, owner: true };
    }
    return { promise, owner: false };
  }

  // bridgeBatch delivers one ordered batch of same-conversation frames as
  // a SINGLE gc POST. The single-frame batch (the only shape when
  // coalescing is disabled) produces an envelope byte-identical to the
  // pre-coalescer bridge; a multi-frame batch combines the members into
  // one text block (header + one "sender: text" segment per member, in
  // arrival order — media and text stay interleaved exactly as sent),
  // concatenates attachments in member order, and carries a
  // batch-specific dedup key ("wecom-batch-<first>-<last>-<n>") so gc's
  // dedup can never collapse it with a member's own msgid.
  async function bridgeBatch(entries) {
    // Cleanup is owner- AND promise-conditional (codex r2+r3): only the
    // frame whose startHydration call inserted the entry may delete it,
    // and only while it still holds THE promise that frame consumed. A
    // non-owner replay deleting the entry it merely reused would strip
    // the original delivery's dedup; an unconditional delete races
    // replacements (ABA). The function-wide try/finally below (codex r4)
    // guarantees every member's cleanup runs on EVERY exit — early
    // returns, the POST paths, and exceptions anywhere in the body (e.g.
    // an out-of-range but JSON-valid create_time making toISOString()
    // throw), which would otherwise strand owner entries until the TTL
    // sweep.
    const cleanups = [];
    try {
      // Per-member admission + rendering. seenInBatch collapses duplicate
      // msgids WITHIN the batch (an SDK replay landing inside the window
      // buffers a second entry for the same id — the in-batch variant of
      // the replay the seen set catches across batches; first copy wins).
      const members = [];
      const seenInBatch = new Set();
      for (const { frame, handle, seq } of entries) {
        const msg = frame?.body;
        if (!msg) continue;
        const hydration = handle?.promise ?? null;
        if (msg.msgid && handle?.owner) {
          cleanups.push(() => {
            const current = hydrations.get(msg.msgid);
            if (current && current.promise === hydration) hydrations.delete(msg.msgid);
          });
        }
        // Per-member containment: a hostile frame that throws anywhere in
        // its own processing (the known case is an out-of-range but
        // JSON-valid create_time making toISOString() throw — which is
        // why received_at is computed HERE, per member, not in the shared
        // envelope build) is dropped ALONE. Pre-coalescer, each frame
        // bridged independently, so one malformed frame could never take
        // its burst neighbors down with it; that containment must survive
        // batching.
        try {
          if (msg.msgid && (seenMsgIds.has(msg.msgid) || inflightMsgIds.has(msg.msgid) || seenInBatch.has(msg.msgid))) {
            continue;
          }
          const conversation = conversationForMessage(cfg, msg);
          if (!conversation.conversation_id) {
            log(`inbound ${msg.msgid}: no conversation id (chattype=${msg.chattype}); dropped`);
            continue;
          }
          let text = renderText(msg, ({ repeats, fromChars, toChars }) => {
            log(`inbound ${msg.msgid}: voice ASR repeat collapsed ×${repeats} (${fromChars} → ${toChars} chars)`);
          });
          if (!text) {
            // A frame that renders to nothing (a text message with empty
            // content) cannot deliver — but it must not vanish silently
            // (jg-p1mk item 2 discipline).
            log(`inbound ${msg.msgid}: EMPTY PAYLOAD — ${msg.msgtype} message rendered no text; dropped`);
            continue;
          }
          // Empty-payload surfacing (jg-p1mk item 2, 8/22 22:53 real case):
          // a voice frame with no transcript, or a media frame with no
          // download URL, delivers with an explicit marker plus a loud log
          // line — never as a bare placeholder with zero evidence.
          const note = emptyPayloadNote(msg);
          if (note) {
            log(`inbound ${msg.msgid}: EMPTY PAYLOAD — ${msg.msgtype} message carried no usable payload; delivering with explicit marker`);
            text = `${text}\n${note}`;
          }
          const receivedAt = msg.create_time
            ? new Date(msg.create_time * 1000).toISOString()
            : new Date().toISOString();

          // Media hydration started the moment the frame arrived (the
          // download URL is on a 5-minute fuse — it cannot wait behind this
          // conversation's gc retry queue, nor behind the coalescing
          // window); by the time this chained bridge runs, the promise is
          // usually already settled. hydrateMessageMedia never rejects, but
          // a defensive catch keeps an unforeseen bug from dropping the
          // message: worst case the agent sees the bare placeholder text.
          let attachments = [];
          if (hydration) {
            const hydrated = await hydration.catch((err) => {
              log(`inbound ${msg.msgid}: media hydration error: ${err.message}`);
              return { attachments: [], block: '' };
            });
            attachments = hydrated.attachments;
            if (hydrated.block) text = `${text}\n${hydrated.block}`;
          }
          if (msg.msgid) seenInBatch.add(msg.msgid);
          // sender is finalized HERE, inside the containment, so the
          // shared combine below is throw-free (codex r2 finding 1).
          members.push({
            msg,
            conversation,
            text,
            attachments,
            receivedAt,
            seq,
            sender: neutralizeMarkupBoundaries(msg.from?.userid ?? 'unknown'),
          });
        } catch (err) {
          log(`inbound ${msg.msgid}: member processing failed (${err.message}); dropped from batch`);
        }
      }
      if (members.length === 0) return;

      const newest = members[members.length - 1];
      let text = newest.text;
      let attachments = newest.attachments;
      let dedupKey = newest.msg.msgid == null ? newest.msg.msgid : String(newest.msg.msgid);
      let label = `inbound ${newest.msg.msgid}`;
      if (members.length > 1) {
        // Header and sender interpolations are neutralized; the member
        // bodies pass through raw, exactly like the single path — gc's
        // reminder formatter sanitizes the whole text field.
        const lines = [`[${members.length} WeCom messages coalesced, in arrival order]`];
        for (const m of members) {
          lines.push(`${m.sender}: ${m.text}`);
        }
        text = lines.join('\n');
        attachments = members.flatMap((m) => m.attachments);
        dedupKey = `wecom-batch-${members[0].msg.msgid ?? 'nomsgid'}-${newest.msg.msgid ?? 'nomsgid'}-${members.length}`;
        label = `inbound batch ${members[0].msg.msgid}..${newest.msg.msgid}`;
      }

      // Once-per-conversation reply how-to (jg-p1mk add D): appended to
      // the conversation's FIRST delivery this adapter lifetime. A
      // deterministic rejection unmarks (the session never saw it);
      // transient failures retry the same built text, help included.
      const convId = newest.conversation.conversation_id;
      const convKey = conversationKeyFor(newest.conversation.kind, convId);

      // Buffered peer-bot posts ride with the human delivery that wakes
      // the session (jg-p1mk add A) — restored on rejection below.
      // Provider order is preserved relative to the batch (codex r2
      // finding 8): items that arrived BEFORE the batch's first frame
      // render as prior context above it; items that arrived after (a
      // peer reacting to the buffered human message inside the window)
      // render below it, so a peer response never reads as context that
      // predates the request that caused it.
      // Take is bounded by the batch's NEWEST member (posts after it stay
      // buffered for a later delivery); the before/after split anchors on
      // the first VALID member — a dropped malformed first entry must not
      // misplace the boundary (codex r3 finding 2).
      const newestSeq = newest.seq ?? Infinity;
      const firstSeq = members[0].seq ?? -Infinity;
      const peerCtx = takePeerContext(convKey, newestSeq);
      if (peerCtx) {
        const before = peerCtx.items.filter((item) => item.seq === undefined || item.seq < firstSeq);
        const after = peerCtx.items.filter((item) => item.seq !== undefined && item.seq >= firstSeq);
        if (before.length > 0 || peerCtx.dropped > 0) {
          text = `${formatPeerContextBlock(before, peerCtx.dropped, 'peer-bot context since the last delivery')}\n\n${text}`;
        }
        if (after.length > 0) {
          text = `${text}\n\n${formatPeerContextBlock(after, 0, 'peer-bot posts arriving after the first message above')}`;
        }
      }

      // Once-per-conversation reply how-to (jg-p1mk add D), always the
      // final block. A deterministic rejection unmarks (the session
      // never saw it); transient failures retry the same built text.
      const firstHelp = replyHelpOnce && !replyHelpSent.has(convKey);
      if (firstHelp) {
        markReplyHelpSent(convKey);
        text = `${text}\n\n${replyHelpBlock(convId)}`;
      }

      const message = {
        // Every provider-controlled field gc types as a string is
        // coerced (codex r3 finding 1): a numeric msgid/userid must
        // degrade to its string form, never to a rejected batch.
        provider_message_id: newest.msg.msgid == null ? newest.msg.msgid : String(newest.msg.msgid),
        conversation: newest.conversation,
        actor: {
          id: String(newest.msg.from?.userid ?? ''),
          display_name: String(newest.msg.from?.userid ?? ''),
          is_bot: false,
        },
        // No explicit_target: routing is the default_route fragment's job
        // (and per-conversation bindings). Stamping an addressee here would
        // mislabel messages on rebound conversations — gc carries it into
        // the reminder and tells the receiving agent the message was
        // addressed to someone else.
        text,
        ...(attachments.length > 0 ? { attachments } : {}),
        dedup_key: dedupKey,
        received_at: newest.receivedAt,
      };

      const target = `${cfg.gcAPIBase}/v0/city/${encodeURIComponent(cfg.cityName)}/extmsg/inbound`;
      const msgids = members.map((m) => m.msg.msgid).filter(Boolean);
      for (const id of msgids) inflightMsgIds.add(id);
      try {
        // Transient gc failures retry indefinitely — WeCom replay after a
        // reconnect is not guaranteed, so the retry loop is the delivery
        // mechanism, not a bonus. The in-flight markers hold for the whole
        // retry so a replay arriving mid-retry can't double-post.
        await postInbound(target, { message }, label, log);
        for (const id of msgids) markSeen(id);
        log(`${label} → gc (${newest.conversation.kind} ${newest.conversation.conversation_id}, ${members.length === 1 ? newest.msg.msgtype : `${members.length} messages`})`);
      } catch (err) {
        // Only deterministic 4xx rejections land here (transient failures
        // retry forever): a replay would fail identically, so mark seen to
        // stop pointless re-posts.
        for (const id of msgids) markSeen(id);
        if (firstHelp) replyHelpSent.delete(convKey);
        restorePeerContext(convKey, peerCtx);
        log(`${label} rejected by gc (${err.message}); dropped`);
      } finally {
        for (const id of msgids) inflightMsgIds.delete(id);
      }
    } finally {
      for (const cleanup of cleanups) cleanup();
    }
  }

  // Per-conversation serial delivery: with independent async bridges, a
  // transient failure on message A would let a later message B land in gc
  // first, reversing conversation context. Chain batches per conversation;
  // separate conversations still proceed concurrently. Entries are removed
  // once their chain drains, so memory tracks active conversations only.
  const convoChains = new Map();

  function chainBatch(key, entries) {
    const prev = convoChains.get(key) ?? Promise.resolve();
    const next = prev.then(() => bridgeBatch(entries)).catch((err) => log(`bridge error: ${err.message}`));
    convoChains.set(key, next);
    return next.finally(() => {
      // Every enqueued frame settles exactly once (early-returns
      // included), so the refcount balances even under replays.
      for (const e of entries) removePending(e.frame?.body?.msgid);
      if (convoChains.get(key) === next) convoChains.delete(key);
    });
  }

  // --- inbound burst coalescing (jg-p1mk item 3; port of slack-full's ---
  // inbound_coalescer.go, simplified for WeCom).
  //
  // Rapid multi-message bursts (voice series, 3×1-line answers) each
  // historically produced their own gc delivery = their own agent turn.
  // With a window configured (WECOM_COALESCE_WINDOW_MS; 0 disables), a
  // conversation's frames buffer for the window and deliver as ONE
  // inbound carrying every message verbatim, in arrival order. Per-chat
  // only — different conversations never share a batch (they already
  // never shared a chain). The window is FIXED from the first buffered
  // frame (not a sliding debounce): steady chatter flushes every window
  // instead of deferring indefinitely. A buffer reaching
  // coalesceMaxBatch flushes early — nothing is ever evicted or dropped;
  // the cap bounds latency, not content. WeCom has no analog of
  // slack-full's urgent (bot-mentioned/targeted) class, so there is no
  // flush-ahead path; ordering versus in-flight deliveries is inherited
  // from the per-conversation chain the flush enqueues onto.
  //
  // Crash acceptance, same as slack-full's: buffered frames were already
  // received from the provider, so an adapter crash inside the window
  // loses that window's chatter (WeCom replay is not guaranteed). A
  // normal shutdown drains every buffer first (flushAll, wired to
  // SIGTERM/SIGINT in index.js). Delivery failures need no restore path
  // here: the batch POST rides postJSONWithRetry (transient failures
  // retry forever inside the chain; deterministic rejections drop with
  // the same semantics as the single path).
  const coalesceWindowMs = deps.coalesceWindowMs ?? cfg.coalesceWindowMs ?? 0;
  const coalesceMaxBatch = deps.coalesceMaxBatch ?? 50;
  const coalesceBuffers = new Map(); // convKey → { entries, timer, settle, promise }

  function flushConversation(key, buf) {
    // Identity check makes a stale timer callback (buffer already
    // early-flushed and replaced) a strict no-op.
    if (coalesceBuffers.get(key) !== buf) return;
    coalesceBuffers.delete(key);
    clearTimeout(buf.timer);
    buf.settle(chainBatch(key, buf.entries));
  }

  function enqueueCoalesced(key, entry) {
    let buf = coalesceBuffers.get(key);
    if (!buf) {
      buf = { entries: [], timer: null, settle: null, promise: null };
      buf.promise = new Promise((resolve) => { buf.settle = resolve; });
      buf.timer = setTimeout(() => flushConversation(key, buf), coalesceWindowMs);
      buf.timer.unref?.();
      coalesceBuffers.set(key, buf);
    }
    buf.entries.push(entry);
    if (buf.entries.length >= coalesceMaxBatch) {
      log(`coalesce: ${key} buffer full (${buf.entries.length}); early flush`);
      flushConversation(key, buf);
    }
    return buf.promise;
  }

  // flushAll drains every buffered conversation AND awaits the delivery
  // chains — the normal-shutdown path, so already-received frames are
  // not lost to a SIGTERM landing inside a window. It also flips the
  // pipeline into draining mode: frames arriving mid-drain bypass the
  // window into chains this loop then observes, instead of opening an
  // unobserved buffer that outlives the snapshot (codex r2 finding 4).
  // A chain wedged in gc-outage retries never settles — the CALLER
  // bounds the wait (index.js shutdown caps it at 5s).
  let draining = false;
  async function flushAll() {
    draining = true;
    for (const [key, buf] of [...coalesceBuffers]) {
      flushConversation(key, buf);
    }
    // Chains can grow while being awaited (late frames, chained
    // batches); loop until the map empties, bounded defensively.
    for (let round = 0; round < 32 && convoChains.size > 0; round++) {
      await Promise.allSettled([...convoChains.values()]);
    }
  }

  const enqueueInbound = (frame) => {
    const msg = frame?.body ?? {};
    // Normalize provider identity fields ONCE at intake (codex r4
    // finding 2): every internal map, truthiness gate, and allowlist
    // below assumes strings. A JSON-valid numeric id must behave as its
    // string form EVERYWHERE — msgid 0 was emitted to gc as "0" but
    // skipped by truthiness-based dedup (double POST on replay), and a
    // numeric userid could never match the string peer-bot allowlist
    // (bot post waking the session). The wire-boundary String() calls
    // stay as defense in depth for direct callers.
    if (msg.msgid != null) msg.msgid = String(msg.msgid);
    if (msg.chatid != null) msg.chatid = String(msg.chatid);
    if (msg.from && msg.from.userid != null) msg.from.userid = String(msg.from.userid);
    // Peer-bot room posts divert to the context buffer BEFORE any
    // pipeline state opens (jg-p1mk add A): no pending refcount, no
    // hydration (a peer's media URL burns unfetched — context is
    // text-only by contract), no chain, no wake. They surface as a
    // tagged read-only block ahead of the next human delivery.
    if (peerBotUserIds.size > 0 && msg.chattype === 'group'
      && msg.from?.userid && peerBotUserIds.has(msg.from.userid)) {
      bufferPeerContext(conversationKeyFor('group', msg.chatid), msg);
      return Promise.resolve();
    }
    // Pending ownership opens HERE — before the frame even buffers — so
    // the TTL sweep can tell a live-but-queued hydration from a leak.
    // The refcount now also spans the coalescing window: a buffered
    // media frame's hydration entry is load-bearing for the whole wait.
    addPending(msg.msgid);
    // Everything between the addPending above and the promise handoff
    // below runs synchronously; a throw in that window (hydration setup,
    // buffer/chain wiring) would otherwise leak the refcount forever,
    // since no settled bridge ever decrements it (codex r4).
    try {
      // Hydration starts NOW, outside the buffer and the chain — the
      // media download URL expires ~5 minutes after this frame, and the
      // coalescing window plus an earlier message's gc retry loop can
      // outlast that.
      const handle = startHydration(msg);
      // Namespaced key (codex jg-p1mk r1 finding 1): a DM userid that
      // collides with a group chatid must not share a buffer or chain.
      const key = conversationKeyFor(msg.chattype, msg.chattype === 'group' ? msg.chatid : msg.from?.userid);
      const entry = { frame, handle, seq: arrivalSeq++ };
      // Draining (shutdown): bypass the window so late frames join a
      // chain the drain awaits instead of opening an unobserved buffer.
      if (coalesceWindowMs > 0 && key && !draining) {
        return enqueueCoalesced(key, entry);
      }
      return chainBatch(key, [entry]);
    } catch (err) {
      removePending(msg.msgid);
      throw err;
    }
  };

  return {
    enqueueInbound,
    flushAll,
    // Introspection for tests and diagnostics only — not a public surface.
    stats: () => ({
      hydrations: hydrations.size,
      inflight: inflightMsgIds.size,
      seen: seenMsgIds.size,
      chains: convoChains.size,
      pending: pendingMsgIds.size,
      refusals: refusals.size,
      buffered: coalesceBuffers.size,
      peerContexts: peerContexts.size,
    }),
  };
}
