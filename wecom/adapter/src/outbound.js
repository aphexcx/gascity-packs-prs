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
import { promisify } from 'node:util';

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

// Thrown when a CRITICAL journal write cannot be made durable, or when
// the journal booted degraded. Answered as HTTP 503 failure_kind
// "journal_unavailable": the send is refused rather than run without the
// durable state a safe retry depends on (codex jg-d0xr round-2 finding 3).
export class JournalUnavailableError extends Error {}

// createAttemptJournal persists per-key media delivery progress ACROSS
// restarts (codex jg-d0xr finding 3): stage latches lived only in memory,
// so a restart forgot which uploads and sends had already happened and a
// retried key re-uploaded and RE-SENT media users had already seen. Each
// stage mutation is journaled before the next provider action depends on
// it — in particular sendAttempted is written BEFORE aibot_send_msg goes
// out, so a crash in that window hydrates as delivery-unknown rather than
// silently retryable.
//
// Round-2 hardening: the journal FAILS CLOSED instead of open.
//   - Critical writes (the pre-provider-write latches) succeed or THROW
//     JournalUnavailableError; the caller must stop the send. Non-critical
//     writes (post-delivery latches whose loss only makes a restart
//     over-refuse via delivery-unknown) log and continue, but roll their
//     in-memory mutation back so a later successful persist cannot
//     resurrect a patch whose send was aborted.
//   - Durability: the temp file is fsync'd before the atomic rename and
//     the directory is fsync'd after it; the previous generation rotates
//     to `<file>.bak` on every persist (one write behind, at most).
//   - Startup corruption QUARANTINES the corrupt file (renamed aside,
//     never silently discarded) and recovers from the newest valid
//     generation — main, then the fsync'd tmp (a crash between the two
//     renames leaves tmp as the newest survivor), then the backup. If no
//     valid generation exists while corrupt ones do, the journal boots
//     DEGRADED and every media publish is refused until an operator
//     inspects the quarantined files — starting empty would re-send media
//     users already saw.
//
// Retention policy (round-2 finding 7): entries are pruned only past
// `cap` (default 512), oldest SETTLED first — a durable receipt is
// dropped only when the journal is full of newer entries, and dropping a
// non-settled entry (which loses its delivery-unknown protection) is the
// documented last resort, logged when it happens.
//
// filePath null keeps the journal in memory only (tests).
export function createAttemptJournal({ filePath = null, log = () => {}, cap = 512 } = {}) {
  const entries = new Map(); // key → { fingerprint, mediaId, sendAttempted, mediaSent, … }
  let degraded = false;

  // tryLoad → Map (valid), undefined (absent), or null (corrupt/unreadable).
  const tryLoad = (p) => {
    let raw;
    try {
      raw = fs.readFileSync(p, 'utf8');
    } catch (err) {
      if (err.code === 'ENOENT') return undefined;
      log(`outbound attempt journal ${p} unreadable (${scrubErrorMessage(err.message)})`);
      return null;
    }
    try {
      const parsed = JSON.parse(raw);
      if (!parsed || typeof parsed !== 'object' || typeof parsed.entries !== 'object' || parsed.entries === null) {
        throw new Error('unexpected shape');
      }
      const m = new Map();
      for (const [key, entry] of Object.entries(parsed.entries)) {
        if (entry && typeof entry === 'object') m.set(key, entry);
      }
      return m;
    } catch (err) {
      log(`outbound attempt journal ${p} corrupt (${scrubErrorMessage(err.message)})`);
      return null;
    }
  };

  const quarantine = (p) => {
    const dest = `${p}.corrupt-${Date.now()}`;
    try {
      fs.renameSync(p, dest);
      log(`outbound attempt journal: quarantined ${p} → ${dest}`);
    } catch (err) {
      if (err.code !== 'ENOENT') log(`outbound attempt journal: could not quarantine ${p} (${scrubErrorMessage(err.message)})`);
    }
  };

  if (filePath) {
    let loaded;
    let sawCorrupt = false;
    for (const p of [filePath, `${filePath}.tmp`, `${filePath}.bak`]) {
      const m = tryLoad(p);
      if (m instanceof Map) {
        loaded = m;
        if (p !== filePath) log(`outbound attempt journal: recovered ${m.size} entries from ${p}`);
        break;
      }
      if (m === null) {
        sawCorrupt = true;
        quarantine(p);
      }
    }
    if (loaded) {
      for (const [k, v] of loaded) entries.set(k, v);
    } else if (sawCorrupt) {
      degraded = true;
      log('outbound attempt journal: no valid generation survives its corruption — DEGRADED; '
        + 'media publishing fails closed until the quarantined journal files are inspected and removed');
    }
  }

  function persist() {
    if (!filePath) return;
    fs.mkdirSync(path.dirname(filePath), { recursive: true, mode: 0o700 });
    const tmp = `${filePath}.tmp`;
    const payload = JSON.stringify({ version: 2, entries: Object.fromEntries(entries) });
    // fsync the temp file BEFORE the rename: an atomic rename of
    // un-flushed data is atomically nothing after a power loss.
    const fd = fs.openSync(tmp, 'w', 0o600);
    try {
      fs.writeFileSync(fd, payload);
      fs.fsyncSync(fd);
    } finally {
      fs.closeSync(fd);
    }
    // Rotate the previous good generation to .bak, land the new one, and
    // fsync the directory so both renames are durable.
    try {
      fs.renameSync(filePath, `${filePath}.bak`);
    } catch (err) {
      if (err.code !== 'ENOENT') throw err;
    }
    fs.renameSync(tmp, filePath);
    const dirFd = fs.openSync(path.dirname(filePath), 'r');
    try {
      fs.fsyncSync(dirFd);
    } finally {
      fs.closeSync(dirFd);
    }
  }

  return {
    // record merges `patch` into the key's entry and persists. critical
    // (the default) throws JournalUnavailableError when durability cannot
    // be established — callers must stop the send; critical:false logs,
    // rolls the in-memory mutation back, and returns false.
    record(key, patch, { critical = true } = {}) {
      const fail = (err) => {
        const wrapped = new JournalUnavailableError(
          `the outbound attempt journal cannot be written (${err.code ?? scrubErrorMessage(err.message)}) — `
          + 'refusing to send without durable delivery state; free disk space or fix permissions and retry with the same idempotency key',
        );
        if (critical) throw wrapped;
        log(wrapped.message);
        return false;
      };
      if (degraded) return fail(new Error('journal is degraded (quarantined corrupt state on startup)'));
      const hadKey = entries.has(key);
      const prev = entries.get(key);
      const merged = { ...(prev ?? {}), ...patch, updatedAt: Date.now() };
      entries.delete(key); // re-insert so Map order tracks recency
      entries.set(key, merged);
      const pruned = [];
      while (entries.size > cap) {
        // Documented retention policy: drop the oldest SETTLED entry
        // first; only when nothing is settled drop the oldest of all —
        // that entry loses its delivery-unknown protection, so say so.
        let dropped = null;
        for (const [k, e] of entries) {
          if (e.receipt) { dropped = k; break; }
        }
        if (dropped === null) {
          dropped = entries.keys().next().value;
          log(`outbound attempt journal at cap ${cap} with nothing settled: dropping unresolved entry for a key (its restart-resume protection is lost)`);
        }
        pruned.push([dropped, entries.get(dropped)]);
        entries.delete(dropped);
      }
      try {
        persist();
      } catch (err) {
        // Roll back: the caller aborts on a critical failure, and a later
        // successful persist must not resurrect this patch as if the
        // aborted action had happened.
        entries.delete(key);
        if (hadKey) entries.set(key, prev);
        for (const [k, v] of pruned) {
          if (!entries.has(k)) entries.set(k, v);
        }
        return fail(err);
      }
      return true;
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
    isDegraded() {
      return degraded;
    },
  };
}

// isDefiniteSendFailure decides whether a provider SEND failure provably
// happened BEFORE the frame was written, or is an explicit negative
// acknowledgement — the only two classes safe to retry blindly (codex
// jg-d0xr round-2 finding 4). Everything else is delivery-UNKNOWN by
// DEFAULT: round 1 enumerated only timeout-shaped errors as ambiguous,
// so the pinned SDK's socket-loss rejection of already-written frames —
// "WebSocket connection closed (…), reply for reqId … cancelled"
// (clearPendingMessages in @wecom/aibot-node-sdk 1.0.7) — cleared
// sendAttempted and a retry could display the message twice.
//
// Verified against every rejection path in SDK 1.0.7's sendReply/
// processReplyQueue/handleReplyAck/clearPendingMessages:
//   pre-write, definite  → 'WebSocket not connected, unable to send data'
//                          (send() threw before ws.send)
//   pre-write, definite  → 'Reply queue for reqId … exceeds max size (…)'
//                          (refused before enqueue)
//   negative ack, definite → the raw ACK FRAME object with errcode ≠ 0
//                          (the provider saw and rejected the message)
//   post-write, UNKNOWN  → 'Reply ack timeout (…) for reqId: …'
//   post-write, UNKNOWN  → '<reason>, reply for reqId: … cancelled'
//   anything else        → UNKNOWN (fail toward refusing, never re-sending)
//
// Upload failures never come through here — an upload is invisible to the
// chat, so re-running it is always safe.
function isDefiniteSendFailure(err) {
  if (err && typeof err.errcode === 'number' && err.errcode !== 0) return true;
  const msg = String(err?.message ?? '');
  if (/not connected/i.test(msg)) return true;
  if (/^Reply queue for reqId .* exceeds max size/i.test(msg)) return true;
  return false;
}

// sendErrorText renders any SDK rejection — Error or raw errcode ack
// frame — as a message string for scrubbing/logging.
function sendErrorText(err) {
  if (err && typeof err.errcode === 'number') {
    return `provider rejected the message: errcode ${err.errcode}${err.errmsg ? ` (${err.errmsg})` : ''}`;
  }
  return String(err?.message ?? err ?? 'unknown error');
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

// openConfinedMediaFile enforces the outbound-media root (codex jg-d0xr
// finding 1, hardened in round 2): /publish-media may only read files
// under WECOM_OUTBOUND_MEDIA_ROOT, refuses symlinks ANYWHERE in the path,
// and — the round-2 point — leaves NO gap between the check and the open
// that a rename/symlink swap could race. The round-1 walk lstat'ed each
// component and then opened the ORIGINAL pathname: a process able to
// write beneath the root could rename a checked directory and replace it
// with a symlink before the open (O_NOFOLLOW protects only the final
// component), escaping the root.
//
// This adapter runs on macOS, where openat2(RESOLVE_BENEATH |
// RESOLVE_NO_SYMLINKS) does not exist and Node exposes no openat/fstatat.
// The equivalent here is a SYNCHRONOUS chdir-anchored walk — the process
// cwd plays the role of the directory descriptor:
//
//   1. chdir(realpath(root)) anchors the walk (the root path itself is
//      operator-configured and trusted; everything BELOW it is not).
//   2. Each component is looked up RELATIVELY — a single-component name,
//      so the kernel never re-traverses the checked prefix — lstat'ed
//      (any symlink refused), chdir'ed into, and then verified by
//      stat('.') dev/ino against the lstat: if the entry was swapped
//      between the lstat and the chdir (even dir-for-dir), the identity
//      check fails and the request is refused. A symlink swapped in after
//      its lstat lands the chdir at a different inode — refused too.
//   3. The FINAL component is opened relatively with O_NOFOLLOW from the
//      verified directory and fstat-verified against its lstat; the
//      returned fd is what the caller reads — never the pathname again.
//
// The walk is fully synchronous, so no other JavaScript can observe the
// temporarily-changed cwd (single thread, no awaits between the chdir and
// the restore in `finally`). The one theoretical hazard — an ASYNC fs call
// elsewhere in the process using a RELATIVE path resolving on the libuv
// threadpool mid-walk — does not exist in this adapter: every fs path it
// touches is absolute (file_path is validated absolute; store/journal
// paths derive from absolute config). Residual vectors are documented in
// the jg-d0xr round-2 fix report: a hard link to an out-of-root file
// planted INSIDE the root defeats any path-based confinement (openat2
// included) and requires in-root write access — the trust boundary the
// root defines.
//
// The root is canonicalized per request (a root created after boot starts
// working without a restart); the target is compared LEXICALLY against it
// (no symlink resolution — a path that only reaches the root through a
// link is refused, not normalized).
export function openConfinedMediaFile(filePath, outboundMediaRoot) {
  if (!outboundMediaRoot) {
    throw new ForbiddenError(
      'outbound media publishing is disabled: WECOM_OUTBOUND_MEDIA_ROOT is not set — '
      + 'configure the directory outbound media files may be read from (the adapter fails closed without it)',
    );
  }
  let rootReal;
  try {
    rootReal = fs.realpathSync(outboundMediaRoot);
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
  const parts = rel.split(path.sep);
  const cannotOpen = (err) => new ClientError(
    `cannot open ${filePath} (${err.code ?? err.message}); pass an absolute path to a readable file under the outbound media root`,
  );
  const prevCwd = process.cwd();
  let fd = null;
  try {
    try {
      process.chdir(rootReal);
    } catch {
      throw new ForbiddenError(
        `outbound media publishing is disabled: WECOM_OUTBOUND_MEDIA_ROOT (${outboundMediaRoot}) does not resolve to an existing directory`,
      );
    }
    for (let i = 0; i < parts.length; i++) {
      const part = parts[i];
      let st;
      try {
        st = fs.lstatSync(part);
      } catch (err) {
        throw cannotOpen(err);
      }
      if (st.isSymbolicLink()) {
        throw new ForbiddenError(
          `${path.join(rootReal, ...parts.slice(0, i + 1))} is a symlink; symlinks are not allowed `
          + 'anywhere in an outbound media path — pass the real path under the outbound media root',
        );
      }
      if (i < parts.length - 1) {
        if (!st.isDirectory()) throw cannotOpen({ code: 'ENOTDIR' });
        try {
          process.chdir(part);
        } catch (err) {
          throw cannotOpen(err);
        }
        // Identity check: the directory we LANDED in must be the inode the
        // lstat saw. chdir itself would happily follow a symlink swapped
        // in after the lstat; the dev/ino comparison catches exactly that
        // (and any dir-for-dir swap), because the impostor is a different
        // inode. This is the per-component RESOLVE_NO_SYMLINKS equivalent.
        const here = fs.statSync('.');
        if (here.dev !== st.dev || here.ino !== st.ino) {
          throw new ForbiddenError(
            `${filePath}: a path component changed while it was being verified — refusing to follow it; retry`,
          );
        }
      } else {
        try {
          fd = fs.openSync(part, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
        } catch (err) {
          if (err.code === 'ELOOP' || err.code === 'EMLINK') {
            throw new ForbiddenError(
              `${resolved} is a symlink; symlinks are not allowed anywhere in an outbound media path — `
              + 'pass the real path under the outbound media root',
            );
          }
          throw cannotOpen(err);
        }
        // The open was relative to the VERIFIED directory, so even a raced
        // regular-file swap stays confined; the fstat identity check just
        // pins the bytes read to the inode the walk admitted.
        const fst = fs.fstatSync(fd);
        if (fst.dev !== st.dev || fst.ino !== st.ino) {
          throw new ClientError(`${filePath} changed while it was being opened; retry`);
        }
      }
    }
    return { fd, resolvedPath: resolved };
  } catch (err) {
    if (fd !== null) {
      try { fs.closeSync(fd); } catch { /* already closed */ }
    }
    throw err;
  } finally {
    try {
      process.chdir(prevCwd);
    } catch {
      // The original cwd vanished while we walked; land somewhere sane
      // rather than staying parked inside the media root.
      try { process.chdir(path.dirname(rootReal)); } catch { process.chdir('/'); }
    }
  }
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

// readMediaFile validates and reads one local media file FROM AN ALREADY
// OPEN fd — the one openConfinedMediaFile's verified walk produced (codex
// jg-d0xr round-2 finding 1: the pathname is never re-opened after the
// check, so there is no window for a swap between them). Client mistakes
// throw ClientError (→ HTTP 400 with the message verbatim); anything else
// propagates. Asynchronous I/O for the bytes (codex jg-d0xr finding 9): a
// 10MB synchronous read stalled the event loop — and with it every
// concurrent send and the WS heartbeat — per request. Always closes the fd.
const fdRead = promisify(fs.read);
const fdClose = promisify(fs.close);
const fdFstat = promisify(fs.fstat);

async function readMediaFile({ fd, filePath, mediaKind, maxBytes }) {
  const spec = mediaKindSpecs[mediaKind];
  try {
    const st = await fdFstat(fd);
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
      const { bytesRead } = await fdRead(fd, buffer, offset, st.size - offset, offset);
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
    await fdClose(fd).catch(() => {});
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
      mediaSize: e.mediaSize,
      uploadFilename: e.uploadFilename,
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
  //
  // Round-2 finding 6: each wake REFETCHES the entry from the map. The
  // owner may have been refused at admission (gate/path/journal) and
  // retired the untouched entry via releaseUntouchedState — a waiter that
  // kept using its captured reference would install ownership on an
  // ORPHAN no other caller can see, while a parallel fresh claim creates
  // a second owner for the same key in the map: two concurrent sends.
  // Refetching re-admits atomically within the iteration (the map get,
  // the admission, and the owner installation have no await between
  // them). A captured state that settled WITH a receipt is still safe to
  // answer from directly even if it was evicted meanwhile — it is the
  // same dedup answer the map would have given.
  async function claimPublishState(key) {
    for (;;) {
      const state = publishStateFor(key);
      if (!state) return { refused: true };
      if (state.receipt) return { receipt: state.receipt, state };
      if (!state.promise) return { state, token: installOwner(state) };
      await state.promise.catch(() => {});
      if (state.receipt) return { receipt: state.receipt, state };
      // Otherwise: refetch — never resume a possibly-retired reference.
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
    const fail503Journal = (message) => {
      res.writeHead(503, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({
        conversation: convo,
        delivered: false,
        failure_kind: 'journal_unavailable',
        error: message,
        idempotency_key: key,
      }));
    };

    // The attempt journal is what makes a retried key safe; without it the
    // adapter must not send at all (round-2 finding 3 — fail closed).
    if (journal.isDegraded?.()) {
      fail503Journal(
        'the outbound attempt journal booted degraded (corrupt state was quarantined on startup) — '
        + 'media publishing is disabled until an operator inspects the quarantined journal files',
      );
      return;
    }

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

    // Pre-delivery capability check (round-2 finding 1): gc's binding
    // check runs only AFTER delivery, inside transcript recording, and
    // only when a session_id was supplied — it never gates the send. The
    // strongest pre-delivery target check the adapter can enforce without
    // a gc authorization endpoint is inbound evidence: media may only be
    // published to a conversation the adapter has SEEN talk to the bot
    // (the persistent kind store learns every inbound conversation).
    // WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS=true opts back into
    // proactive media pushes to never-seen ids.
    if (cfg.mediaRequireKnownConversation && !kindStore?.lookup(chatid)) {
      token.finish(new Error(`unknown conversation ${chatid}`));
      releaseUntouchedState(key, state);
      fail403(
        `media may only be published to conversations with inbound history (${chatid} has none) — `
        + 'have the peer message the bot first, or set WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS=true on the adapter to allow proactive media pushes',
      );
      return;
    }

    // A retry attaching to a still-running retained upload must NOT
    // acquire another admission slot or reread the file (round-2 finding
    // 5): the retained uploadRun already owns both the buffer and the
    // slot. Only two shapes touch the gate and the filesystem — a fresh
    // upload (slot held from before the read until the SDK settles the
    // upload), and a latched-media_id resume (slot held just long enough
    // to read + hash for the digest check, released before the sends).
    const attachToRetainedUpload = !state.mediaId && !!state.uploadPromise;

    let releaseUpload = null;
    let media = null;
    if (!attachToRetainedUpload) {
      // Global admission BEFORE any file I/O (finding 9): a slot must be
      // held to allocate a media buffer at all; the queue cap turns a
      // burst into fast 429s instead of unbounded 10MB allocations.
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

      // Only the admitted owner touches the filesystem (finding 7):
      // settled and conflicting retries were answered above without any
      // file access. The confined walk and the open are ONE operation
      // (round-2 finding 1): the bytes are read from the fd the verified
      // walk produced, so a rename/symlink swap after the check has
      // nothing left to race.
      try {
        const opened = openConfinedMediaFile(filePath, cfg.outboundMediaRoot);
        media = await readMediaFile({ fd: opened.fd, filePath, mediaKind, maxBytes });
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
      // Filename shown in the WeCom bubble: caller override, else the
      // file's basename — sanitized, and always carrying the DETECTED
      // extension so WeCom's own server-side type checks see a name that
      // matches the bytes (a .png named photo.jpg uploads as
      // photo.jpg.png, not a lie).
      let filename = safeFilename(pub.filename || path.basename(filePath));
      if (!filename.toLowerCase().endsWith(media.detectedExtension)) {
        const jpegAlias = media.detectedExtension === '.jpg' && filename.toLowerCase().endsWith('.jpeg');
        if (!jpegAlias) filename += media.detectedExtension;
      }
      const digest = crypto.createHash('sha256').update(media.buffer).digest('hex');
      if (state.fingerprint) {
        // A partial state carries the digest of the bytes already uploaded
        // under this key; the same path re-read with DIFFERENT content
        // must not ride that media_id.
        if (state.fingerprint.digest !== digest) {
          releaseUpload();
          token.finish(new Error(`idempotency conflict on ${key}`));
          fail409(['media digest']);
          return;
        }
        if (state.mediaSize === undefined) state.mediaSize = media.size;
        if (!state.uploadFilename) state.uploadFilename = filename;
      } else {
        // Journal the fingerprint BEFORE any provider write (finding 3):
        // from here on, a restart can validate and resume this key. This
        // write is CRITICAL — if it cannot be made durable, the send stops
        // here (round-2 finding 3: swallowed journal failures let provider
        // writes proceed as though the latches were durable). mediaSize
        // and uploadFilename latch alongside so attach-resumes (which
        // never reread the file) can build the transcript entry.
        const fingerprint = { ...probe, digest };
        try {
          journal.record(key, { fingerprint, mediaSize: media.size, uploadFilename: filename });
        } catch (err) {
          releaseUpload();
          token.finish(err);
          if (err instanceof JournalUnavailableError) {
            releaseUntouchedState(key, state);
            fail503Journal(err.message);
            return;
          }
          throw err;
        }
        state.endpoint = 'publish-media';
        state.fingerprint = fingerprint;
        state.mediaSize = media.size;
        state.uploadFilename = filename;
      }
      if (state.mediaId) {
        // The upload is already latched — the buffer was only needed for
        // the digest verification above. Free the slot before the
        // (buffer-less) send stages rather than across them.
        releaseUpload();
        releaseUpload = null;
      }
    }

    // Everything the remaining stages need survives in the state latches,
    // so attach-resumes work without the buffer.
    const effectiveFilename = state.uploadFilename;
    const effectiveDigest = state.fingerprint.digest;
    const effectiveSize = state.mediaSize;

    const spec = mediaKindSpecs[mediaKind];
    const send = async () => {
      // Three latched stages so a gc-style retry of the same key resumes
      // where the failure happened instead of repeating delivered steps:
      // a re-upload wastes quota (30/min per robot), and a re-send shows
      // the user the media twice. Each latch is journaled the moment it
      // matters (finding 3) so the resume survives an adapter restart.
      if (!state.mediaId) {
        if (!state.uploadPromise) {
          // The upload promise is RETAINED until the SDK settles it
          // (finding 3): withUploadDeadline only stops the wait — the
          // chunked upload keeps running inside the SDK, and starting a
          // second one on retry would double quota use and race two
          // uploads of the same bytes over one connection. A retry
          // re-awaits the same promise; if the abandoned upload
          // eventually resolves, its media_id is latched out-of-band.
          //
          // The admission SLOT transfers to the uploadRun (round-2
          // finding 5): it frees when the UPLOAD settles, not when a
          // deadline-limited waiter gives up — the retained upload still
          // owns the buffer, so releasing on the deadline made the
          // global bound false exactly under the timeout it was meant to
          // handle. A permanently hung upload therefore consumes its
          // slot; that is the documented cost of having no SDK-level
          // cancellation.
          const slotRelease = releaseUpload;
          releaseUpload = null;
          const buffer = media.buffer;
          const uploadRun = (async () => uploadMedia(buffer, { type: spec.wecomType, filename: effectiveFilename }))()
            .then((uploaded) => {
              if (!uploaded?.media_id) throw new Error('media upload returned no media_id');
              if (!state.mediaId) {
                state.mediaId = uploaded.media_id;
                journal.record(key, { mediaId: uploaded.media_id }, { critical: false });
              }
              return uploaded;
            });
          uploadRun.catch(() => {}); // retained without a live waiter
          const settleRetention = () => {
            if (state.uploadPromise === uploadRun) state.uploadPromise = undefined;
            slotRelease();
          };
          uploadRun.then(settleRetention, settleRetention);
          state.uploadPromise = uploadRun;
        }
        await withUploadDeadline(state.uploadPromise);
      }
      if (!state.mediaSent) {
        // sendAttempted persists BEFORE the frame goes out: a crash in
        // this window must hydrate as delivery-unknown, not retryable.
        journal.record(key, { sendAttempted: true });
        let frame;
        try {
          frame = await sendMediaMessage(chatid, spec.wecomType, state.mediaId);
        } catch (err) {
          if (isDefiniteSendFailure(err)) {
            // Provably pre-write, or an explicit provider rejection:
            // clear the attempt so the key stays retryable. Non-critical:
            // if this reset is lost, a restart over-refuses (delivery-
            // unknown) — the safe direction.
            journal.record(key, { sendAttempted: false }, { critical: false });
          } else {
            // Post-write or unrecognized: the frame may already be
            // visible in the chat (round-2 finding 4 — default ambiguous).
            state.deliveryUnknown = true;
            journal.record(key, { deliveryUnknown: true }, { critical: false });
          }
          throw err;
        }
        state.messageID = frame?.headers?.req_id ?? state.messageID;
        state.mediaSent = true;
        journal.record(key, { mediaSent: true, messageID: state.messageID ?? '' }, { critical: false });
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
            if (isDefiniteSendFailure(err)) {
              journal.record(key, { chunksAttempted: state.chunksDelivered }, { critical: false });
            } else {
              state.deliveryUnknown = true;
              journal.record(key, { deliveryUnknown: true }, { critical: false });
            }
            throw err;
          }
          state.captionMessageID = frame?.headers?.req_id ?? state.captionMessageID;
          state.chunksDelivered = i + 1;
          journal.record(key, {
            chunksDelivered: state.chunksDelivered,
            captionMessageID: state.captionMessageID ?? '',
          }, { critical: false });
        }
      }
    };
    try {
      await chainSend(chatid, send);
    } catch (err) {
      token.finish(err);
      if (err instanceof JournalUnavailableError) {
        // A CRITICAL pre-provider-write journal latch could not be made
        // durable, so the send stopped BEFORE the frame went out (round-2
        // finding 3). Nothing was delivered; the same key retries cleanly
        // once the journal is writable again.
        fail503Journal(err.message);
        return;
      }
      const stage = !state.mediaId ? 'upload' : (!state.mediaSent ? 'send' : `caption chunk ${state.chunksDelivered + 1}`);
      log(`publish-media → ${chatid} ${mediaKind} failed at ${stage}: ${sendErrorText(err)}`);
      if (state.deliveryUnknown) {
        // Post-write failures without a negative acknowledgement are NOT
        // retryable (findings 3/4): the frame may have been displayed.
        failDeliveryUnknown(`${stage} stage: ${scrubErrorMessage(sendErrorText(err))}`);
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
        error: scrubErrorMessage(sendErrorText(err)),
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
    journal.record(key, { receipt: state.receipt }, { critical: false });
    token.finish();
    log(`publish-media → ${chatid} ${mediaKind} delivered (${effectiveSize} bytes, session=${pub.session_id ?? ''})`);

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
          text: transcriptTextFor({
            mediaKind,
            filename: effectiveFilename,
            size: effectiveSize,
            digest: effectiveDigest,
            filePath,
            caption,
          }),
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

    // Cache the COMPLETE media response as the receipt (finding 8, the
    // adapter-side half): a settled-key retry used to get back only the
    // bare {conversation, message_id, delivered} and lose media_id and
    // the transcript outcome. Recording itself stays single-shot — the
    // safe retry-repair of an AMBIGUOUS recording failure needs gc-side
    // dedup of transcript appends first (an ambiguous /extmsg/outbound
    // may have appended; re-posting could double the transcript entry),
    // so a recording miss remains a truthful transcript_recorded:false.
    const response = {
      ...state.receipt,
      media_id: state.mediaId,
      idempotency_key: key,
      ...(state.captionMessageID ? { caption_message_id: state.captionMessageID } : {}),
      transcript_recorded: transcriptRecorded,
      ...(transcriptNote ? { transcript_note: transcriptNote } : {}),
    };
    state.receipt = response;
    journal.record(key, { receipt: response }, { critical: false });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(response));
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
