// outbound.js — the gc-/CLI- → WeCom publish pipeline, extracted from
// index.js so the send machinery (chunking, idempotency, per-chat
// ordering) is testable with a fake WS client and a fake gc — the same
// extraction rationale as src/inbound.js (jg-c7j codex round-1). index.js
// owns process concerns (config, WS client, listener, signals) and
// instantiates one publisher; tests instantiate their own with injected
// deps. All idempotency/ordering state is per-instance.
//
// Two publish surfaces share one state:
//
//   /publish        text/markdown. Called by gc's extmsg HTTP adapter to
//                   deliver a bound session's reply, and by `gc wecom
//                   publish --text` through the /svc/wecom proxy.
//   /publish-media  image/video files (jg-d0xr). Called ONLY by `gc wecom
//                   publish --image|--video` — gc's PublishRequest wire
//                   carries text alone, so gc itself never posts here.
//                   The adapter uploads the file over the long connection
//                   (aibot_upload_media_init → chunk × N → finish, ≤512KB
//                   per chunk, ≤100 chunks), then pushes the media message
//                   (aibot_send_msg with image/video.media_id).
//
// Outbound transcript recording (the piece gc cannot do for media): after
// a media send is delivered, the adapter POSTs gc's /extmsg/outbound with
// the SAME idempotency key it just completed. gc authorizes the publish
// against the conversation's (agent-)binding, calls back this adapter's
// /publish with that key — which returns the already-settled receipt
// WITHOUT re-sending (the idempotency map is exactly the dedup gc retries
// rely on) — and then appends the outbound transcript entry, records
// delivery context, and fans out peer notifications. Delivery is never
// held hostage by recording: the media message is sent first, and a
// recording failure (no session, caller doesn't own the binding, gc down)
// downgrades to transcript_recorded:false in the response.
//
// WeCom media limits (developer.work.weixin.qq.com/document/path/101463,
// verified 2026-08-20): image ≤10MB (png/jpg/jpeg/gif), video ≤10MB (mp4),
// voice ≤2MB (amr), file ≤20MB; uploaded media_ids stay valid 3 days;
// upload rate ≤30/min and ≤1000/hour per robot. Only image and video are
// implemented — they are the reply shapes the mayor needs; oversized or
// wrong-format files are REJECTED with an actionable message (downscale /
// re-encode is deliberately out of scope for the adapter).

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

import {
  mimeTypeForFilename,
  neutralizeMarkupBoundaries,
  safeFilename,
  scrubErrorMessage,
  sniffExtension,
} from './media.js';

// --- text chunking -----------------------------------------------------------

// WeCom markdown messages cap out around 4096 BYTES; chunk conservatively
// in UTF-8 bytes — Chinese text is ~3 bytes per character, so counting
// UTF-16 code units would overshoot the cap threefold. Splitting iterates
// code points (for...of), which can never sever a surrogate pair.
export const outboundChunkBytes = 3800;
const utf8 = new TextEncoder();

export function chunkText(text) {
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

// --- conversation-kind memory --------------------------------------------------

// gc keys a conversation by its FULL ref — kind included (see gc's
// bindingConversationLabel / transcriptConversationLabel) — so recording
// an outbound transcript entry needs the same dm/room kind the inbound
// pipeline stamped when the conversation first arrived. WeCom's outbound
// surface only takes a bare chatid/userid, so the adapter remembers each
// conversation's kind as inbound traffic reveals it (chattype=group →
// room, single → dm) in a small JSON file that survives restarts. Any
// chat a session can REPLY to has, by construction, sent at least one
// inbound message — so lookups hit for the entire reply flow; only
// proactive pushes to never-seen ids fall through to the caller's --kind
// flag or the 'wr' chatid-prefix heuristic. A wrong guess is self-limiting:
// the mismatched ref resolves no binding, gc rejects the recording, and
// the response says so — nothing is ever recorded under a mislabeled ref.
export function createConversationKindStore({ filePath, log = () => {}, cap = 4096 }) {
  const kinds = new Map(); // conversation_id → 'dm' | 'room'

  try {
    const raw = JSON.parse(fs.readFileSync(filePath, 'utf8'));
    for (const [id, kind] of Object.entries(raw?.kinds ?? {})) {
      if (kind === 'dm' || kind === 'room') kinds.set(id, kind);
    }
  } catch (err) {
    // Missing file is the normal first boot; anything else (corrupt JSON,
    // permissions) starts empty and logs — the store is a cache of facts
    // inbound traffic will re-teach, never the source of truth.
    if (err.code !== 'ENOENT') log(`conversation-kind store unreadable (${scrubErrorMessage(err.message)}); starting empty`);
  }

  function persist() {
    try {
      fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 });
      const tmp = `${filePath}.tmp`;
      fs.writeFileSync(tmp, JSON.stringify({ version: 1, kinds: Object.fromEntries(kinds) }), { mode: 0o600 });
      fs.renameSync(tmp, filePath);
    } catch (err) {
      log(`conversation-kind store write failed: ${scrubErrorMessage(err.message)}`);
    }
  }

  return {
    // observe learns from an inbound WeCom frame. Wired alongside the
    // inbound pipeline's message listeners; a write happens only when a
    // NEW conversation id appears (or its kind changes — never in
    // practice), so disk traffic is one small atomic replace per new chat.
    observe(frame) {
      const msg = frame?.body;
      if (!msg) return;
      const isGroup = msg.chattype === 'group';
      const id = isGroup ? msg.chatid : msg.from?.userid;
      if (!id) return;
      const kind = isGroup ? 'room' : 'dm';
      if (kinds.get(id) === kind) return;
      kinds.delete(id); // re-insert so Map order tracks recency for the cap
      kinds.set(id, kind);
      while (kinds.size > cap) {
        kinds.delete(kinds.keys().next().value);
      }
      persist();
    },
    lookup(conversationId) {
      return kinds.get(conversationId);
    },
    size() {
      return kinds.size;
    },
  };
}

// --- outbound attempt journal ---------------------------------------------------

// createAttemptJournal persists per-key media delivery progress ACROSS
// restarts (codex jg-d0xr finding 3): stage latches lived only in memory,
// so a restart forgot which uploads and sends had already happened and a
// retried key re-uploaded and RE-SENT media users had already seen. Each
// stage mutation is journaled before the next provider action depends on
// it — in particular sendAttempted is written BEFORE aibot_send_msg goes
// out, so a crash in that window hydrates as delivery-unknown rather than
// silently retryable. Same atomic tmp+rename, 0600 discipline as the
// conversation-kind store; writes are small and media sends are capped at
// 30/min by WeCom, so the synchronous write is not on a hot path.
// filePath null keeps the journal in memory only (tests).
export function createAttemptJournal({ filePath = null, log = () => {}, cap = 512 } = {}) {
  const entries = new Map(); // key → { fingerprint, mediaId, sendAttempted, mediaSent, … }

  if (filePath) {
    try {
      const raw = JSON.parse(fs.readFileSync(filePath, 'utf8'));
      for (const [key, entry] of Object.entries(raw?.entries ?? {})) {
        if (entry && typeof entry === 'object') entries.set(key, entry);
      }
    } catch (err) {
      // Missing file is the normal first boot; anything else starts empty
      // and logs — losing the journal degrades to the pre-journal world
      // (retries may duplicate after a restart), never to a crash.
      if (err.code !== 'ENOENT') log(`outbound attempt journal unreadable (${scrubErrorMessage(err.message)}); starting empty`);
    }
  }

  function persist() {
    if (!filePath) return;
    try {
      fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 });
      const tmp = `${filePath}.tmp`;
      fs.writeFileSync(tmp, JSON.stringify({ version: 1, entries: Object.fromEntries(entries) }), { mode: 0o600 });
      fs.renameSync(tmp, filePath);
    } catch (err) {
      log(`outbound attempt journal write failed: ${scrubErrorMessage(err.message)}`);
    }
  }

  return {
    record(key, patch) {
      const merged = { ...(entries.get(key) ?? {}), ...patch, updatedAt: Date.now() };
      entries.delete(key); // re-insert so Map order tracks recency
      entries.set(key, merged);
      while (entries.size > cap) {
        // Drop the oldest SETTLED entry first; failing that, the oldest of
        // all — the journal must never be the thing that grows unbounded.
        let dropped = null;
        for (const [k, e] of entries) {
          if (e.receipt) { dropped = k; break; }
        }
        entries.delete(dropped ?? entries.keys().next().value);
      }
      persist();
    },
    get(key) {
      return entries.get(key);
    },
    entries() {
      return [...entries];
    },
    size() {
      return entries.size;
    },
  };
}

// isAckAmbiguous decides whether a provider send failure leaves delivery
// UNKNOWN (finding 3): the SDK's reply-ack timeout ("Reply ack timeout
// (10000ms) for reqId: …") means the frame went out and the
// acknowledgement never came back — WeCom may well have displayed the
// message, so a blind same-key retry could show it twice. Anything else
// the SDK throws (not connected, reply queue full, errcode≠0 response) is
// a definite non-delivery and stays retryable. Upload failures never come
// through here — an upload is invisible to the chat, so re-running it is
// always safe.
function isAckAmbiguous(err) {
  return err?.code === 'ETIMEDOUT' || /\btimed?\s*out\b/i.test(err?.message ?? '');
}

// --- media file admission ------------------------------------------------------

// What each media kind accepts, per the WeCom smart-robot limits above.
// Detection is by MAGIC BYTES (sniffExtension), not the filename — WeCom
// validates content server-side, and a clear client error beats a cryptic
// provider rejection after a full chunked upload.
const mediaKindSpecs = {
  image: {
    wecomType: 'image',
    allowed: new Set(['.jpg', '.png', '.gif']),
    formatsLabel: 'jpg/jpeg, png, or gif',
    capEnv: 'WECOM_IMAGE_MAX_BYTES',
  },
  video: {
    wecomType: 'video',
    allowed: new Set(['.mp4']),
    formatsLabel: 'mp4',
    capEnv: 'WECOM_VIDEO_MAX_BYTES',
  },
};

class ClientError extends Error {}
class ForbiddenError extends Error {}

// --- outbound-media root confinement --------------------------------------------

// assertConfinedMediaPath enforces the outbound-media root (codex jg-d0xr
// finding 1): /publish-media may only read files under
// WECOM_OUTBOUND_MEDIA_ROOT, and refuses symlinks ANYWHERE in the path.
// Without it, any local caller able to reach the internal listener could
// exfiltrate every image/video readable by the adapter to a WeCom chat —
// the O_NOFOLLOW open protects only the FINAL component; symlinked parent
// directories were traversed freely. Fail closed: no configured root, no
// media publishing at all.
//
// The root is canonicalized per request (a root created after boot starts
// working without a restart); the target is compared LEXICALLY against it
// (no symlink resolution — a path that only reaches the root through a
// link is refused, not normalized), then every component below the root
// is lstat'ed and any symlink is rejected even if it would resolve back
// inside the root. The O_NOFOLLOW open that follows re-checks the final
// component at open time; racing a PARENT component into a symlink after
// the walk requires write access inside the root itself, which is exactly
// the trust boundary the root defines. Session/target authorization
// stays where it lives today: gc's /extmsg/outbound checks the session's
// binding when the transcript is recorded — the internal listener has no
// pre-delivery authorization surface, which is why the filesystem scope
// here must be airtight on its own.
export async function assertConfinedMediaPath(filePath, outboundMediaRoot) {
  if (!outboundMediaRoot) {
    throw new ForbiddenError(
      'outbound media publishing is disabled: WECOM_OUTBOUND_MEDIA_ROOT is not set — '
      + 'configure the directory outbound media files may be read from (the adapter fails closed without it)',
    );
  }
  let rootReal;
  try {
    rootReal = await fs.promises.realpath(outboundMediaRoot);
  } catch {
    throw new ForbiddenError(
      `outbound media publishing is disabled: WECOM_OUTBOUND_MEDIA_ROOT (${outboundMediaRoot}) does not resolve to an existing directory`,
    );
  }
  const resolved = path.resolve(filePath);
  const rel = path.relative(rootReal, resolved);
  if (rel === '' || rel.startsWith('..') || path.isAbsolute(rel)) {
    throw new ForbiddenError(`file_path must live under the outbound media root ${rootReal}: ${filePath}`);
  }
  let current = rootReal;
  for (const part of rel.split(path.sep)) {
    current = path.join(current, part);
    let st;
    try {
      st = await fs.promises.lstat(current);
    } catch (err) {
      throw new ClientError(`cannot open ${filePath} (${err.code ?? err.message}); pass an absolute path to a readable file under the outbound media root`);
    }
    if (st.isSymbolicLink()) {
      throw new ForbiddenError(`${current} is a symlink; symlinks are not allowed anywhere in an outbound media path — pass the real path under the outbound media root`);
    }
  }
  return resolved;
}

// postJSONParsed mirrors inbound.js's postJSON (same headers, same 15s
// deadline) but returns the PARSED response body: the transcript-recording
// caller must inspect gc's OutboundResult — a 200 with Delivered:false or
// no TranscriptEntry means nothing was recorded, and reporting
// transcript_recorded:true on it would be a lie.
async function postJSONParsed(target, body) {
  const resp = await fetch(target, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-GC-Request': 'gc-wecom-adapter',
    },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(15000),
  });
  if (!resp.ok) {
    const text = (await resp.text().catch(() => '')).trim();
    const err = new Error(`${resp.status} ${resp.statusText}: ${text}`);
    err.status = resp.status;
    throw err;
  }
  return resp.json().catch(() => ({}));
}

// readMediaFile opens, validates, and reads one local media file. Client
// mistakes throw ClientError (→ HTTP 400 with the message verbatim);
// anything else propagates. O_NOFOLLOW + stat-on-the-open-handle is the
// same no-path-race hygiene as media.js's readVerifiedAudio: the size
// check and the bytes read cannot be swapped out from under each other.
// Asynchronous I/O throughout (codex jg-d0xr finding 9): a 10MB
// synchronous read stalled the event loop — and with it every concurrent
// send and the WS heartbeat — per request.
async function readMediaFile(filePath, mediaKind, maxBytes) {
  const spec = mediaKindSpecs[mediaKind];
  let handle;
  try {
    handle = await fs.promises.open(filePath, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  } catch (err) {
    if (err.code === 'ELOOP' || err.code === 'EMLINK') {
      throw new ClientError(`${filePath} is a symlink; pass the real file path`);
    }
    throw new ClientError(`cannot open ${filePath} (${err.code ?? err.message}); pass an absolute path to a readable file on the adapter host`);
  }
  try {
    const st = await handle.stat();
    if (!st.isFile()) {
      throw new ClientError(`${filePath} is not a regular file`);
    }
    if (st.size === 0) {
      throw new ClientError(`${filePath} is empty`);
    }
    if (st.size > maxBytes) {
      throw new ClientError(
        `${mediaKind} is too large: ${st.size} bytes > the ${maxBytes}-byte WeCom ${mediaKind} cap — `
        + `downscale/re-encode it before sending (the adapter deliberately does not transcode); `
        + `${spec.capEnv} overrides the cap only if your WeCom tenant allows more`,
      );
    }
    const buffer = Buffer.alloc(st.size);
    let offset = 0;
    while (offset < st.size) {
      const { bytesRead } = await handle.read(buffer, offset, st.size - offset, offset);
      if (bytesRead === 0) throw new ClientError(`${filePath} changed while reading (short read)`);
      offset += bytesRead;
    }
    const detected = sniffExtension(buffer);
    if (!spec.allowed.has(detected)) {
      const detectedLabel = detected === '' ? 'unrecognized content' : `${detected.slice(1)} content`;
      throw new ClientError(
        `WeCom ${mediaKind} messages accept ${spec.formatsLabel} — ${filePath} has ${detectedLabel}; convert the file and retry`,
      );
    }
    return { buffer, size: st.size, detectedExtension: detected };
  } finally {
    await handle.close();
  }
}

// --- global upload admission ------------------------------------------------------

// Thrown when the upload gate is saturated; answered as HTTP 429 — the
// caller retries with the SAME idempotency key once load drains.
class BusyError extends Error {}

// createUploadGate bounds outbound media admission globally (codex
// jg-d0xr finding 9): at most `slots` requests hold a media buffer
// (read → hash → chunked upload) at once, and at most `maxQueue` more may
// wait for a slot — anything beyond that is refused up front, BEFORE the
// file is read, so a burst of /publish-media requests cannot allocate
// unbounded 10MB buffers or saturate the long connection. Waiters hold no
// buffer. Same idempotent-release/pump discipline as media.js's
// createDownloadGate; no deadline variant because a local file, unlike a
// WeCom download URL, is not on a fuse.
export function createUploadGate({ slots, maxQueue }) {
  let inUse = 0;
  const waiters = [];
  const makeRelease = () => {
    let released = false;
    return () => {
      if (released) return;
      released = true;
      inUse--;
      while (inUse < slots && waiters.length > 0) {
        inUse++;
        waiters.shift()(makeRelease());
      }
    };
  };
  return {
    acquire() {
      if (inUse < slots) {
        inUse++;
        return Promise.resolve(makeRelease());
      }
      if (waiters.length >= maxQueue) {
        return Promise.reject(new BusyError(
          `the adapter is at its outbound upload capacity (${slots} uploading, ${maxQueue} queued) — retry shortly with the same idempotency key`,
        ));
      }
      return new Promise((resolve) => { waiters.push(resolve); });
    },
  };
}

// --- publisher ----------------------------------------------------------------

// createOutboundPublisher wires the two publish handlers around one shared
// idempotency map and one per-chat send chain.
//
// deps:
//   cfg              cityName, provider, botId, gcAPIBase, imageMaxBytes,
//                    videoMaxBytes, uploadTimeoutMs
//   sendMessage      (chatid, body) → receipt frame   [wsClient.sendMessage]
//   uploadMedia      (buffer, {type, filename}) → {media_id}
//                    [wsClient.uploadMedia — init/chunk/finish over the WS]
//   sendMediaMessage (chatid, type, mediaId) → receipt frame
//                    [wsClient.sendMediaMessage — aibot_send_msg]
//   withUploadDeadline(promise) → promise — wall-clock bound on the whole
//                    chunked upload (index.js wraps media.js withDeadline);
//                    identity in tests
//   kindStore        createConversationKindStore instance (or null)
//   journal          createAttemptJournal instance — persists media stage
//                    latches across restarts; defaults to an in-memory
//                    journal (no file)
//   postOutbound     (target, body) → parsed OutboundResult — POST gc
//                    /extmsg/outbound; defaults to postJSONParsed above
//                    (tests inject a fake)
//   log              adapter logger
//   publishStatesCap test knob (default 512)
export function createOutboundPublisher(deps) {
  const {
    cfg,
    sendMessage,
    uploadMedia,
    sendMediaMessage,
    withUploadDeadline = (p) => p,
    kindStore = null,
    journal = createAttemptJournal(),
    postOutbound = postJSONParsed,
    log = () => {},
    publishStatesCap = 512,
  } = deps;

  // Idempotency: gc retries a publish (callback timeout, transient error)
  // with the same idempotency_key. Track per-key chunk progress and the
  // final receipt so a retry never re-sends chunks WeCom users already
  // saw. Media publishes share the map: their post-send /extmsg/outbound
  // recording call comes BACK through /publish with the same key, and the
  // settled receipt here is what stops that callback from re-sending.
  const publishStates = new Map(); // key → { chunksDelivered, messageID, receipt, promise, … }

  // Global outbound upload admission (finding 9): worst-case media buffer
  // memory ≤ slots × the media cap; queued waiters hold no buffer yet.
  const uploadGate = createUploadGate({
    slots: cfg.uploadMaxConcurrent ?? 2,
    maxQueue: cfg.uploadMaxQueue ?? 8,
  });

  // Seeded receipts for gc's transcript-recording callback (finding 6):
  // while a delivered media publish awaits its /extmsg/outbound
  // round-trip, its receipt lives HERE — a lookup separate from the
  // shared publishStates map, so ordinary text-dedup cap pressure can
  // never evict it. If it were evictable, gc's callback would miss the
  // settled receipt and treat the transcript text as a fresh /publish —
  // visibly sending filenames and host source paths into the chat. The
  // map is bounded by the number of in-flight recordings (each admitted
  // through the upload gate above); entries are removed the moment
  // recording settles either way.
  const transcriptSeeds = new Map(); // key → receipt

  // Rehydrate media publish states from the attempt journal (finding 3):
  // a restart used to lose every latch, so a retried key re-uploaded and
  // RE-SENT media users had already seen. A journaled entry whose
  // aibot_send_msg (or caption chunk) was attempted without a recorded
  // acknowledgement comes back as delivery-unknown — refused for blind
  // retry, because the message may already be visible in the chat.
  for (const [journaledKey, e] of journal.entries()) {
    publishStates.set(journaledKey, {
      endpoint: 'publish-media',
      fingerprint: e.fingerprint,
      mediaId: e.mediaId,
      mediaSent: !!e.mediaSent,
      messageID: e.messageID,
      captionMessageID: e.captionMessageID,
      chunksDelivered: e.chunksDelivered ?? 0,
      receipt: e.receipt,
      deliveryUnknown: !!e.deliveryUnknown
        || (!!e.sendAttempted && !e.mediaSent && !e.receipt)
        || ((e.chunksAttempted ?? 0) > (e.chunksDelivered ?? 0) && !e.receipt),
    });
  }

  // Per-chat outbound serialization: the SDK only serializes sends sharing
  // a req_id, so two publishes to the same chat (different idempotency
  // keys) could otherwise interleave one reply between another's chunks —
  // or a caption between someone else's image and text. Chain the whole
  // send per chatid; different chats stay concurrent.
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

  // publishStateFor returns the tracked state for a key, admitting a new
  // one only within the cap. Returns null when a NEW key cannot be
  // admitted: only fully settled, unpinned entries are evictable — never
  // a live (in-flight) send, never a pinned seed whose recording callback
  // is still due, and never a failed partial (its latches are exactly
  // what a retry resumes from; losing them re-sends media users already
  // saw). When nothing is evictable the key is REFUSED (HTTP 503) instead
  // of growing without bound or evicting an unsafe entry (codex jg-d0xr
  // findings 6 and 9 — the old code grew "temporarily", i.e. forever).
  function publishStateFor(key) {
    if (!key) return { chunksDelivered: 0 }; // untracked, per-call state
    let state = publishStates.get(key);
    if (!state) {
      if (publishStates.size >= publishStatesCap) {
        let evicted = false;
        for (const [k, s] of publishStates) {
          if (s.receipt && !s.promise && !s.pinned) {
            publishStates.delete(k);
            evicted = true;
            break;
          }
        }
        if (!evicted) return null;
      }
      state = { chunksDelivered: 0 };
      publishStates.set(key, state);
    }
    return state;
  }

  // installOwner marks `state` as owned by this caller and installs the
  // owner promise SYNCHRONOUSLY — the promise every other claimer of the
  // same key waits on. The owner settles it through token.finish(err?),
  // which clears the promise only while the token still owns it (a stale
  // finish can never clobber a successor's claim) and only THEN wakes the
  // waiters, so a waiter resumed by the settlement always observes either
  // the receipt or a claimable (promise-less) state — never a half-torn
  // one.
  function installOwner(state) {
    let settle;
    const promise = new Promise((resolve, reject) => { settle = { resolve, reject }; });
    // Waiters attach their own .catch in the claim loop; this guard keeps
    // an owner failure from ever surfacing as an unhandled rejection when
    // no waiter happens to be queued.
    promise.catch(() => {});
    const token = {
      finish(err) {
        if (state.owner !== token) return;
        state.owner = undefined;
        state.promise = undefined;
        if (err) settle.reject(err); else settle.resolve();
      },
    };
    state.owner = token;
    state.promise = promise;
    return token;
  }

  // claimPublishState resolves the atomic-claim loop both handlers share:
  // returns { receipt } when the key already settled (answer it straight
  // from the map — this is what dedups gc's recording callback), or
  // { state, token } once this caller owns the right to run the send. The
  // loop matters — after a failed send, resumed waiters that simply fell
  // through would each start their own send and resend the same chunk
  // concurrently. The owner promise is installed synchronously INSIDE the
  // claim (no await sits between the check and the installation — codex
  // jg-d0xr finding 5: leaving the installation to the caller let every
  // waiter resumed by one failure claim ownership at once, duplicating
  // sends and transcript recordings).
  async function claimPublishState(key) {
    const state = publishStateFor(key);
    if (!state) return { refused: true };
    for (;;) {
      if (state.receipt) return { receipt: state.receipt, state };
      if (!state.promise) return { state, token: installOwner(state) };
      await state.promise.catch(() => {});
    }
  }

  // fingerprintConflicts lists the request fields on which `probe`
  // disagrees with the state already latched under the same key —
  // endpoint first (a /publish key reused on /publish-media can otherwise
  // return an unrelated receipt), then the stored operation fingerprint.
  // The digest is compared separately, by the owner, once the file has
  // actually been read.
  function fingerprintConflicts(state, probe) {
    if (state.endpoint && state.endpoint !== probe.endpoint) return ['endpoint'];
    if (!state.fingerprint) return [];
    return Object.keys(probe).filter((f) => state.fingerprint[f] !== probe[f]);
  }

  // releaseUntouchedState drops a state entry that was claimed but never
  // progressed (admission refused the request before any provider work):
  // leaving it behind would count refused requests against the map cap
  // forever (finding 9) — there is nothing for a retry to resume.
  function releaseUntouchedState(key, state) {
    if (!key) return;
    if (state.fingerprint || state.mediaId || state.mediaSent || state.receipt || state.chunksDelivered > 0) return;
    if (publishStates.get(key) === state) publishStates.delete(key);
  }

  // --- /publish: text/markdown --------------------------------------------

  async function handlePublish(req, res, body) {
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

    // Seeded media receipts answer FIRST (finding 6): gc's recording
    // callback must always find the pinned receipt, whatever the shared
    // dedup map is doing under load.
    const seeded = pub.idempotency_key ? transcriptSeeds.get(pub.idempotency_key) : undefined;
    if (seeded) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(seeded));
      return;
    }

    const claim = await claimPublishState(pub.idempotency_key);
    if (claim.refused) {
      // The idempotency map is at cap with nothing safely evictable:
      // refusing the NEW key beats evicting a live/pinned entry whose
      // loss re-sends messages users already saw. gc retries later.
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'overloaded',
      }));
      return;
    }
    if (claim.receipt) {
      // A media key lands here too: gc's transcript-recording callback
      // posts the SAME key back through /publish, and this settled-receipt
      // answer is exactly what keeps it from re-sending. Never endpoint-
      // check receipts on this path.
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(claim.receipt));
      return;
    }
    const { state, token } = claim;
    if (state.endpoint && state.endpoint !== 'publish') {
      // An UNSETTLED media state must not be resumed as text — its
      // chunksDelivered counts caption chunks of a different send.
      token.finish(new Error(`idempotency conflict on ${pub.idempotency_key}`));
      res.writeHead(409, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'idempotency_conflict',
      }));
      return;
    }
    if (pub.idempotency_key) state.endpoint = 'publish';

    const send = async () => {
      const chunks = chunkText(pub.text);
      for (let i = state.chunksDelivered; i < chunks.length; i++) {
        const chunkReceipt = await sendMessage(chatid, {
          msgtype: 'markdown',
          markdown: { content: chunks[i] },
        });
        state.messageID = chunkReceipt?.headers?.req_id ?? state.messageID;
        state.chunksDelivered = i + 1;
      }
    };
    try {
      await chainSend(chatid, send);
    } catch (err) {
      token.finish(err);
      log(`publish → ${chatid} failed at chunk ${state.chunksDelivered + 1}: ${err.message}`);
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'provider_error',
      }));
      return;
    }
    // The receipt must be visible BEFORE the owner promise settles, so a
    // waiter woken by finish() answers from the map instead of re-claiming.
    state.receipt = {
      conversation: convo,
      message_id: state.messageID ?? '',
      delivered: true,
    };
    token.finish();
    log(`publish → ${chatid} delivered (session=${pub.session_id ?? ''})`);
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(state.receipt));
  }

  // --- /publish-media: image/video ------------------------------------------

  // resolveKind picks the dm/room kind for the outbound transcript ref:
  // an explicit caller kind wins, then the learned inbound map, then the
  // WeCom chatid convention (group chatids start with "wr"; userids
  // don't). See createConversationKindStore for why a wrong guess is
  // self-limiting rather than data-corrupting.
  function resolveKind(requested, conversationId) {
    if (requested) return requested;
    const learned = kindStore?.lookup(conversationId);
    if (learned) return learned;
    return conversationId.startsWith('wr') ? 'room' : 'dm';
  }

  // recordOutboundTranscript posts the completed media publish into gc's
  // /extmsg/outbound so it lands in the conversation transcript (kind
  // outbound, provider_message_id = the media message's receipt id) and
  // fans out to peer sessions. gc's callback to /publish with the same
  // idempotency key answers from the settled receipt — no re-send.
  // Success is judged by gc's OutboundResult actually CARRYING a
  // TranscriptEntry (the append is non-fatal on the gc side, and a
  // Delivered:false receipt records nothing); anything else throws a
  // human-readable note the caller reports — never failing the (already
  // delivered) publish on it.
  async function recordOutboundTranscript({ sessionID, conversationID, kind, key, text }) {
    const target = `${cfg.gcAPIBase}/v0/city/${encodeURIComponent(cfg.cityName)}/extmsg/outbound`;
    const result = await postOutbound(target, {
      session_id: sessionID,
      conversation: {
        scope_id: cfg.cityName,
        provider: cfg.provider,
        account_id: cfg.botId,
        conversation_id: conversationID,
        kind,
      },
      text,
      idempotency_key: key,
    });
    // gc's OutboundResult is an untagged Go struct — PascalCase keys.
    if (!result?.TranscriptEntry) {
      throw new Error('gc accepted the publish but recorded no transcript entry');
    }
  }

  // transcriptTextFor renders the outbound transcript entry: the same
  // bracketed-tag style the inbound pipeline uses, plus the attachment
  // metadata gc's text-only outbound wire can carry (filename, mime,
  // size, digest, source path) and the caption when one was sent.
  // Filenames/paths are operator-supplied but neutralized anyway — the
  // entry ends up inside gc's reminder envelope like inbound text does.
  function transcriptTextFor({ mediaKind, filename, size, digest, filePath, caption }) {
    const mime = mimeTypeForFilename(filename) || mediaKind;
    const head = `[${mediaKind} sent] ${neutralizeMarkupBoundaries(filename)} `
      + `(${mime}, ${size} bytes, sha256 ${digest.slice(0, 12)}) — source: ${neutralizeMarkupBoundaries(filePath)}`;
    return caption ? `${head}\n${caption}` : head;
  }

  async function handlePublishMedia(req, res, body) {
    let pub;
    try {
      pub = JSON.parse(body);
    } catch {
      res.writeHead(400).end('invalid JSON');
      return;
    }
    const convo = pub.conversation ?? {};
    const chatid = convo.conversation_id;
    const filePath = pub.file_path;
    const mediaKind = pub.media_kind;
    const caption = typeof pub.text === 'string' ? pub.text : '';
    const fail400 = (message) => {
      res.writeHead(400, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ conversation: convo, delivered: false, failure_kind: 'invalid_request', error: message }));
    };
    const fail403 = (message) => {
      res.writeHead(403, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ conversation: convo, delivered: false, failure_kind: 'forbidden', error: message }));
    };
    if (!chatid || !filePath) {
      fail400('conversation.conversation_id and file_path are required');
      return;
    }
    if (!(mediaKind in mediaKindSpecs)) {
      fail400(`media_kind must be one of: ${Object.keys(mediaKindSpecs).join(', ')}`);
      return;
    }
    if (!path.isAbsolute(filePath)) {
      fail400(`file_path must be absolute (the adapter runs with its own working directory): ${filePath}`);
      return;
    }
    if (convo.kind && convo.kind !== 'dm' && convo.kind !== 'room') {
      fail400(`conversation.kind must be "dm" or "room", got ${JSON.stringify(convo.kind)}`);
      return;
    }
    const maxBytes = mediaKind === 'image' ? cfg.imageMaxBytes : cfg.videoMaxBytes;

    // The idempotency key is REQUIRED here: the caller (gc wecom publish)
    // generates one per LOGICAL invocation and reuses it on every retry.
    // The old adapter-side UUID fallback meant a rerun after a lost HTTP
    // response minted a fresh key and sent/recorded the media again
    // (codex jg-d0xr finding 2) — and every failed autogenerated-key
    // state was garbage no retry could ever resume (finding 9).
    const key = pub.idempotency_key;
    if (typeof key !== 'string' || key === '') {
      fail400('idempotency_key is required: gc wecom publish generates one per invocation — pass --idempotency-key to reuse a previous one across retries');
      return;
    }

    // The request-level operation fingerprint (finding 4): idempotency
    // state keyed by the key ALONE let a retried key with a different
    // chat/file/caption inherit the previous attempt's latched media_id —
    // sending file A to conversation B and recording B's metadata for it.
    // Every reuse of a key must describe the SAME logical send; anything
    // else is answered 409, never partially resumed. The media digest
    // joins the fingerprint once the owner has read the file below.
    const probe = {
      endpoint: 'publish-media',
      conversation_id: chatid,
      media_kind: mediaKind,
      file_path: filePath,
      filename: typeof pub.filename === 'string' ? pub.filename : '',
      caption,
    };
    const fail409 = (fields) => {
      res.writeHead(409, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'idempotency_conflict',
        error: `idempotency_key ${key} was already used for a different publish (mismatched: ${fields.join(', ')}) — reuse a key only to retry the identical send; use a fresh key for a new one`,
        idempotency_key: key,
      }));
    };
    const failDeliveryUnknown = (detail = '') => {
      res.writeHead(502, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'delivery_unknown',
        error: 'the WeCom acknowledgement for a send under this idempotency key never arrived — the message may or may not be visible in the chat. '
          + 'Check the chat first; re-send with a FRESH key only if it is genuinely missing.'
          + (detail ? ` (${detail})` : ''),
        idempotency_key: key,
      }));
    };

    const claim = await claimPublishState(key);
    if (claim.refused) {
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'overloaded',
        error: 'the adapter\'s publish-state table is full of live or unresolved sends — retry shortly with the same idempotency key',
        idempotency_key: key,
      }));
      return;
    }
    if (claim.receipt) {
      // Settled: validate the fingerprint WITHOUT touching the file
      // (finding 7 — the original may have been deleted or moved since
      // delivery; the cached receipt is still the truthful answer).
      const conflicts = fingerprintConflicts(claim.state, probe);
      if (conflicts.length > 0) {
        fail409(conflicts);
        return;
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(claim.receipt));
      return;
    }
    const { state, token } = claim;
    {
      const conflicts = fingerprintConflicts(state, probe);
      if (conflicts.length > 0) {
        token.finish(new Error(`idempotency conflict on ${key}`));
        fail409(conflicts);
        return;
      }
    }
    if (state.deliveryUnknown) {
      // A previous attempt's acknowledgement never arrived (or the
      // adapter died mid-send): the message may already be visible in
      // the chat, so this key is not blindly retryable (finding 3).
      token.finish(new Error(`delivery unknown for ${key}`));
      failDeliveryUnknown();
      return;
    }

    // Global admission BEFORE any file I/O (finding 9): a slot must be
    // held to allocate a media buffer at all; the queue cap turns a burst
    // into fast 429s instead of unbounded 10MB allocations. The slot is
    // released the moment the upload stage settles, in the send below.
    let releaseUpload;
    try {
      releaseUpload = await uploadGate.acquire();
    } catch (err) {
      token.finish(err);
      releaseUntouchedState(key, state);
      res.writeHead(429, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'overloaded',
        error: err.message,
        idempotency_key: key,
      }));
      return;
    }

    // Only the admitted owner touches the filesystem (finding 7): settled
    // and conflicting retries were answered above without any file access.
    // Confinement first (finding 1) — the root check must precede the
    // open, so nothing outside the outbound media root is ever read.
    let media;
    try {
      await assertConfinedMediaPath(filePath, cfg.outboundMediaRoot);
      media = await readMediaFile(filePath, mediaKind, maxBytes);
    } catch (err) {
      releaseUpload();
      token.finish(err);
      releaseUntouchedState(key, state);
      if (err instanceof ForbiddenError) {
        fail403(err.message);
        return;
      }
      if (err instanceof ClientError) {
        fail400(err.message);
        return;
      }
      throw err;
    }
    // Filename shown in the WeCom bubble: caller override, else the file's
    // basename — sanitized, and always carrying the DETECTED extension so
    // WeCom's own server-side type checks see a name that matches the
    // bytes (a .png named photo.jpg uploads as photo.jpg.png, not a lie).
    let filename = safeFilename(pub.filename || path.basename(filePath));
    if (!filename.toLowerCase().endsWith(media.detectedExtension)) {
      const jpegAlias = media.detectedExtension === '.jpg' && filename.toLowerCase().endsWith('.jpeg');
      if (!jpegAlias) filename += media.detectedExtension;
    }
    const digest = crypto.createHash('sha256').update(media.buffer).digest('hex');
    if (state.fingerprint) {
      // A partial state carries the digest of the bytes already uploaded
      // under this key; the same path re-read with DIFFERENT content must
      // not ride that media_id.
      if (state.fingerprint.digest !== digest) {
        releaseUpload();
        token.finish(new Error(`idempotency conflict on ${key}`));
        fail409(['media digest']);
        return;
      }
    } else {
      state.endpoint = 'publish-media';
      state.fingerprint = { ...probe, digest };
      // Journal the fingerprint BEFORE any provider write (finding 3):
      // from here on, a restart can validate and resume this key.
      journal.record(key, { fingerprint: state.fingerprint });
    }

    const spec = mediaKindSpecs[mediaKind];
    const send = async () => {
      // Three latched stages so a gc-style retry of the same key resumes
      // where the failure happened instead of repeating delivered steps:
      // a re-upload wastes quota (30/min per robot), and a re-send shows
      // the user the media twice. Each latch is journaled the moment it
      // matters (finding 3) so the resume survives an adapter restart.
      try {
        if (!state.mediaId) {
          if (!state.uploadPromise) {
            // The upload promise is RETAINED until the SDK settles it
            // (finding 3): withUploadDeadline only stops the wait — the
            // chunked upload keeps running inside the SDK, and starting a
            // second one on retry would double quota use and race two
            // uploads of the same bytes over one connection. A retry
            // re-awaits the same promise; if the abandoned upload
            // eventually resolves, its media_id is latched out-of-band.
            const uploadRun = Promise.resolve(
              uploadMedia(media.buffer, { type: spec.wecomType, filename }),
            ).then((uploaded) => {
              if (!uploaded?.media_id) throw new Error('media upload returned no media_id');
              if (!state.mediaId) {
                state.mediaId = uploaded.media_id;
                journal.record(key, { mediaId: uploaded.media_id });
              }
              return uploaded;
            });
            uploadRun.catch(() => {}); // retained without a live waiter
            const clearRetention = () => {
              if (state.uploadPromise === uploadRun) state.uploadPromise = undefined;
            };
            uploadRun.then(clearRetention, clearRetention);
            state.uploadPromise = uploadRun;
          }
          await withUploadDeadline(state.uploadPromise);
        }
      } finally {
        // The buffer's provider-side use ends with the upload stage —
        // free the admission slot before the (buffer-less) send stages.
        releaseUpload();
      }
      if (!state.mediaSent) {
        // sendAttempted persists BEFORE the frame goes out: a crash in
        // this window must hydrate as delivery-unknown, not retryable.
        journal.record(key, { sendAttempted: true });
        let frame;
        try {
          frame = await sendMediaMessage(chatid, spec.wecomType, state.mediaId);
        } catch (err) {
          if (isAckAmbiguous(err)) {
            state.deliveryUnknown = true;
            journal.record(key, { deliveryUnknown: true });
          } else {
            // Definite non-delivery (never written / provider refused):
            // clear the attempt so the key stays retryable.
            journal.record(key, { sendAttempted: false });
          }
          throw err;
        }
        state.messageID = frame?.headers?.req_id ?? state.messageID;
        state.mediaSent = true;
        journal.record(key, { mediaSent: true, messageID: state.messageID ?? '' });
      }
      if (caption) {
        const chunks = chunkText(caption);
        for (let i = state.chunksDelivered; i < chunks.length; i++) {
          journal.record(key, { chunksAttempted: i + 1 });
          let frame;
          try {
            frame = await sendMessage(chatid, {
              msgtype: 'markdown',
              markdown: { content: chunks[i] },
            });
          } catch (err) {
            if (isAckAmbiguous(err)) {
              state.deliveryUnknown = true;
              journal.record(key, { deliveryUnknown: true });
            } else {
              journal.record(key, { chunksAttempted: state.chunksDelivered });
            }
            throw err;
          }
          state.captionMessageID = frame?.headers?.req_id ?? state.captionMessageID;
          state.chunksDelivered = i + 1;
          journal.record(key, {
            chunksDelivered: state.chunksDelivered,
            captionMessageID: state.captionMessageID ?? '',
          });
        }
      }
    };
    try {
      await chainSend(chatid, send);
    } catch (err) {
      token.finish(err);
      const stage = !state.mediaId ? 'upload' : (!state.mediaSent ? 'send' : `caption chunk ${state.chunksDelivered + 1}`);
      log(`publish-media → ${chatid} ${mediaKind} failed at ${stage}: ${err.message}`);
      if (state.deliveryUnknown) {
        // Ack timeouts are NOT retryable failures (finding 3): the frame
        // went out and WeCom may have displayed it.
        failDeliveryUnknown(`${stage} stage: ${scrubErrorMessage(err.message)}`);
        return;
      }
      res.writeHead(502, { 'Content-Type': 'application/json' });
      // Unlike /publish (whose caller is gc's receipt parser), this
      // endpoint answers the CLI — include the scrubbed provider error so
      // the operator sees WHY instead of a bare failure kind.
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'provider_error',
        error: scrubErrorMessage(err.message),
        // Echoed so the operator can rerun with the SAME key and resume
        // instead of duplicating whatever stages already delivered.
        idempotency_key: key,
      }));
      return;
    }
    // Settle the receipt BEFORE the recording call below: gc's callback
    // to /publish with this key must find it settled, or it would try to
    // send the transcript text as a fresh markdown message. It must also
    // be visible before token.finish() wakes same-key waiters.
    state.receipt = {
      conversation: convo,
      message_id: state.messageID ?? '',
      delivered: true,
    };
    journal.record(key, { receipt: state.receipt });
    token.finish();
    log(`publish-media → ${chatid} ${mediaKind} delivered (${media.size} bytes, session=${pub.session_id ?? ''})`);

    let transcriptRecorded = false;
    let transcriptNote = '';
    if (pub.session_id) {
      const kind = resolveKind(convo.kind, chatid);
      // Pin the receipt for the whole recording round-trip (finding 6):
      // the seed in its own lookup guarantees the callback a hit, and
      // pinned=true keeps the map entry itself out of eviction's reach
      // until recording settles.
      state.pinned = true;
      transcriptSeeds.set(key, state.receipt);
      try {
        await recordOutboundTranscript({
          sessionID: pub.session_id,
          conversationID: chatid,
          kind,
          key,
          text: transcriptTextFor({ mediaKind, filename, size: media.size, digest, filePath, caption }),
        });
        transcriptRecorded = true;
      } catch (err) {
        transcriptNote = `delivered, but not recorded in the extmsg transcript: ${scrubErrorMessage(err.message)}`;
        log(`publish-media → ${chatid}: transcript recording failed: ${scrubErrorMessage(err.message)}`);
      } finally {
        transcriptSeeds.delete(key);
        state.pinned = false;
      }
    } else {
      transcriptNote = 'delivered, but not recorded in the extmsg transcript: no session_id supplied (set GC_SESSION_ID or pass --session)';
    }

    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({
      ...state.receipt,
      media_id: state.mediaId,
      idempotency_key: key,
      ...(state.captionMessageID ? { caption_message_id: state.captionMessageID } : {}),
      transcript_recorded: transcriptRecorded,
      ...(transcriptNote ? { transcript_note: transcriptNote } : {}),
    }));
  }

  return {
    handlePublish,
    handlePublishMedia,
    // Introspection for tests and diagnostics only — not a public surface.
    stats: () => ({
      publishStates: publishStates.size,
      sendChains: sendChains.size,
      transcriptSeeds: transcriptSeeds.size,
    }),
  };
}
