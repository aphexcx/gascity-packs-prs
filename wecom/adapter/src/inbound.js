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
    // Compaction: reclaim the dead prefix once it dominates a
    // beyond-double-cap array. Loop-free copy (no variadic spread), rare
    // by construction, indices rebuilt for the survivors.
    if (seenHead > seenMsgIdCap && seenHead * 2 > seenMsgIdOrder.length) {
      const offset = seenHead;
      seenMsgIdOrder = seenMsgIdOrder.slice(seenHead);
      seenHead = 0;
      for (let i = 0; i < seenMsgIdOrder.length; i++) {
        const id = seenMsgIdOrder[i];
        // Remap only the LIVE slot for this id — a stale duplicate slot
        // (same id re-marked later) must not steal the live index.
        if (seenIndex.get(id) === i + offset) seenIndex.set(id, i);
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
      for (const { frame, handle } of entries) {
        const msg = frame?.body;
        if (!msg) continue;
        const hydration = handle?.promise ?? null;
        if (msg.msgid && handle?.owner) {
          cleanups.push(() => {
            const current = hydrations.get(msg.msgid);
            if (current && current.promise === hydration) hydrations.delete(msg.msgid);
          });
        }
        if (msg.msgid && (seenMsgIds.has(msg.msgid) || inflightMsgIds.has(msg.msgid) || seenInBatch.has(msg.msgid))) {
          continue;
        }
        const conversation = conversationForMessage(cfg, msg);
        if (!conversation.conversation_id) {
          log(`inbound ${msg.msgid}: no conversation id (chattype=${msg.chattype}); dropped`);
          continue;
        }
        let text = renderText(msg);
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
        members.push({ msg, conversation, text, attachments });
      }
      if (members.length === 0) return;

      const newest = members[members.length - 1];
      let text = newest.text;
      let attachments = newest.attachments;
      let dedupKey = newest.msg.msgid;
      let label = `inbound ${newest.msg.msgid}`;
      if (members.length > 1) {
        // Header and sender interpolations are neutralized; the member
        // bodies pass through raw, exactly like the single path — gc's
        // reminder formatter sanitizes the whole text field.
        const lines = [`[${members.length} WeCom messages coalesced, in arrival order]`];
        for (const m of members) {
          lines.push(`${neutralizeMarkupBoundaries(m.msg.from?.userid ?? 'unknown')}: ${m.text}`);
        }
        text = lines.join('\n');
        attachments = members.flatMap((m) => m.attachments);
        dedupKey = `wecom-batch-${members[0].msg.msgid ?? 'nomsgid'}-${newest.msg.msgid ?? 'nomsgid'}-${members.length}`;
        label = `inbound batch ${members[0].msg.msgid}..${newest.msg.msgid}`;
      }

      const message = {
        provider_message_id: newest.msg.msgid,
        conversation: newest.conversation,
        actor: {
          id: newest.msg.from?.userid ?? '',
          display_name: newest.msg.from?.userid ?? '',
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
        received_at: newest.msg.create_time
          ? new Date(newest.msg.create_time * 1000).toISOString()
          : new Date().toISOString(),
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

  // flushAll synchronously drains every buffered conversation — the
  // normal-shutdown path, so already-received frames are not lost to a
  // SIGTERM landing inside a window. Returns a promise that settles when
  // every flushed delivery settles.
  function flushAll() {
    const flushed = [];
    for (const [key, buf] of [...coalesceBuffers]) {
      flushed.push(buf.promise);
      flushConversation(key, buf);
    }
    return Promise.allSettled(flushed);
  }

  const enqueueInbound = (frame) => {
    const msg = frame?.body ?? {};
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
      const key = msg.chattype === 'group' ? msg.chatid : msg.from?.userid;
      if (coalesceWindowMs > 0 && key) {
        return enqueueCoalesced(key, { frame, handle });
      }
      return chainBatch(key, [{ frame, handle }]);
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
    }),
  };
}
