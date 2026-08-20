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
// anything else propagates. O_NOFOLLOW + fstat-on-the-open-fd is the same
// no-path-race hygiene as media.js's readVerifiedAudio: the size check and
// the bytes read cannot be swapped out from under each other.
function readMediaFile(filePath, mediaKind, maxBytes) {
  const spec = mediaKindSpecs[mediaKind];
  let fd;
  try {
    fd = fs.openSync(filePath, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  } catch (err) {
    if (err.code === 'ELOOP' || err.code === 'EMLINK') {
      throw new ClientError(`${filePath} is a symlink; pass the real file path`);
    }
    throw new ClientError(`cannot open ${filePath} (${err.code ?? err.message}); pass an absolute path to a readable file on the adapter host`);
  }
  try {
    const st = fs.fstatSync(fd);
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
      const bytesRead = fs.readSync(fd, buffer, offset, st.size - offset, offset);
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
    fs.closeSync(fd);
  }
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
    for (;;) {
      if (state.receipt) return { receipt: state.receipt, state };
      if (!state.promise) return { state, token: installOwner(state) };
      await state.promise.catch(() => {});
    }
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

    const claim = await claimPublishState(pub.idempotency_key);
    if (claim.receipt) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(claim.receipt));
      return;
    }
    const { state, token } = claim;

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

    // Validate + read the file BEFORE claiming the idempotency state: a
    // rejected file must not leave a partial entry a later retry of the
    // same key would resume from.
    let media;
    try {
      media = readMediaFile(filePath, mediaKind, maxBytes);
    } catch (err) {
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
    const claim = await claimPublishState(key);
    if (claim.receipt) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(claim.receipt));
      return;
    }
    const { state, token } = claim;

    const spec = mediaKindSpecs[mediaKind];
    const send = async () => {
      // Three latched stages so a gc-style retry of the same key resumes
      // where the failure happened instead of repeating delivered steps:
      // a re-upload wastes quota (30/min per robot), and a re-send shows
      // the user the media twice.
      if (!state.mediaId) {
        const uploaded = await withUploadDeadline(
          uploadMedia(media.buffer, { type: spec.wecomType, filename }),
        );
        if (!uploaded?.media_id) throw new Error('media upload returned no media_id');
        state.mediaId = uploaded.media_id;
      }
      if (!state.mediaSent) {
        const frame = await sendMediaMessage(chatid, spec.wecomType, state.mediaId);
        state.messageID = frame?.headers?.req_id ?? state.messageID;
        state.mediaSent = true;
      }
      if (caption) {
        const chunks = chunkText(caption);
        for (let i = state.chunksDelivered; i < chunks.length; i++) {
          const frame = await sendMessage(chatid, {
            msgtype: 'markdown',
            markdown: { content: chunks[i] },
          });
          state.captionMessageID = frame?.headers?.req_id ?? state.captionMessageID;
          state.chunksDelivered = i + 1;
        }
      }
    };
    try {
      await chainSend(chatid, send);
    } catch (err) {
      token.finish(err);
      const stage = !state.mediaId ? 'upload' : (!state.mediaSent ? 'send' : `caption chunk ${state.chunksDelivered + 1}`);
      log(`publish-media → ${chatid} ${mediaKind} failed at ${stage}: ${err.message}`);
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
    token.finish();
    log(`publish-media → ${chatid} ${mediaKind} delivered (${media.size} bytes, session=${pub.session_id ?? ''})`);

    let transcriptRecorded = false;
    let transcriptNote = '';
    if (pub.session_id) {
      const kind = resolveKind(convo.kind, chatid);
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
    stats: () => ({ publishStates: publishStates.size, sendChains: sendChains.size }),
  };
}
