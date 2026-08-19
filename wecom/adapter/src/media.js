// media.js — inbound WeCom attachment hydration (jg-c7j, phase 2 of the
// media plan in README "Phase plan").
//
// WeCom media/file messages (image, video, file — and images inside mixed
// messages) arrive as an encrypted download URL plus a per-message
// AES-256-CBC aeskey, and the URL expires ~5 minutes after receipt. So the
// adapter downloads + decrypts IN the receive path (the SDK's
// wsClient.downloadFile does both — see decryptFile's scheme: key =
// base64-decoded aeskey, IV = its first 16 bytes, PKCS#7 padded to
// 32-byte blocks), writes the plaintext to a durable per-conversation
// store, and hands the agent side a local file:// path — mirroring the
// slack-full adapter's inbound hydration (externalAttachment records +
// a "[N files attached] … saved to <path>; Read that path to view it"
// text block). No token, URL, or aeskey ever leaves the adapter.
//
// Audio FILES (m4a/amr/mp3/… sent as file messages — distinct from voice
// messages, which WeCom transcribes server-side into voice.content) are
// additionally transcribed via ElevenLabs Scribe and the transcript is
// appended inline, so the mayor's session reads the content without a
// manual transcription round-trip. A transcription failure downgrades to
// a "[transcription failed: …]" note — the saved file path still
// delivers.
//
// Everything here is failure-isolating by design: a bad attachment
// (download error, oversize, decrypt error, disk error, transcription
// error) turns into a per-item note in the returned text block; the
// message itself is always delivered and the adapter never crashes on a
// hostile or broken attachment.

import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

// --- text hygiene ------------------------------------------------------------

// neutralizeMarkupBoundaries injects a zero-width space (U+200B) after
// every '<' in provider-controlled input (filenames, error strings) so a
// WeCom sender cannot forge a `</system-reminder>` (or any XML-style tag)
// inside the text block gc later wraps in its reminder envelope. Port of
// slack-full's neutralizer, same idempotency guarantee: f(f(x)) == f(x) —
// a '<' already followed by U+200B is not double-padded.
const zwsp = '​';

export function neutralizeMarkupBoundaries(s) {
  if (!s.includes('<')) return s;
  let out = '';
  for (let i = 0; i < s.length; i++) {
    out += s[i];
    if (s[i] === '<' && !s.startsWith(zwsp, i + 1)) out += zwsp;
  }
  return out;
}

// scrubErrorMessage makes an exception message safe to embed in delivered
// text and logs: any URL is dropped (WeCom download URLs are
// capability-bearing while they live), markup boundaries are neutralized,
// and the length is capped so a pathological upstream error cannot balloon
// the message.
export function scrubErrorMessage(msg) {
  let s = String(msg ?? 'unknown error').replace(/https?:\/\/\S+/g, '[url]');
  if (s.length > 200) s = `${s.slice(0, 200)}…`;
  return neutralizeMarkupBoundaries(s);
}

// --- path sanitization -------------------------------------------------------

// capUtf8Bytes truncates s to at most max UTF-8 BYTES without severing a
// code point (same code-point iteration rationale as index.js's
// chunkText: Chinese filenames are ~3 bytes per character, so a
// char-count cap would overshoot filesystem component limits threefold).
const utf8 = new TextEncoder();

function capUtf8Bytes(s, max) {
  if (utf8.encode(s).length <= max) return s;
  let out = '';
  let bytes = 0;
  for (const ch of s) {
    const b = utf8.encode(ch).length;
    if (bytes + b > max) break;
    out += ch;
    bytes += b;
  }
  return out;
}

// safePathComponent sanitizes a WeCom-supplied identifier (chatid, userid,
// msgid) for use as a filesystem path component. Port of slack-full's
// sanitizer: strict allowlist [A-Za-z0-9_.-]; everything else becomes '_';
// a leading dot becomes '_' so the result can never be '.', '..', or a
// dotfile; capped at 64 chars (WeCom ids are ASCII and well under that);
// empty input yields '_' so callers always get a usable component.
export function safePathComponent(s) {
  const maxLen = 64;
  let cleaned = '';
  for (const ch of String(s ?? '')) {
    cleaned += /[A-Za-z0-9_.-]/.test(ch) ? ch : '_';
  }
  if (cleaned.startsWith('.')) cleaned = `_${cleaned.slice(1)}`;
  if (cleaned.length > maxLen) cleaned = cleaned.slice(0, maxLen);
  return cleaned === '' ? '_' : cleaned;
}

// safeFilename keeps the sender's original filename recognizable (Chinese
// names survive — the mayor's session shows them back to the sender) while
// stripping everything that could escape or damage the store: path
// separators, NUL, control characters, and leading dots. The byte cap
// leaves headroom under the filesystem's 255-byte component limit for the
// "<msgid>-" prefix the caller prepends; when the cap truncates, a short
// extension is re-appended so downstream type detection (agent Read, the
// audio gate below) keeps working.
export function safeFilename(name) {
  let trimmed = String(name ?? '').trim();
  if (trimmed === '') return 'file';
  let cleaned = '';
  for (const ch of trimmed) {
    const code = ch.codePointAt(0);
    cleaned += (ch === '/' || ch === '\\' || code < 0x20) ? '_' : ch;
  }
  while (cleaned.startsWith('.')) cleaned = `_${cleaned.slice(1)}`;
  const maxBytes = 160;
  if (utf8.encode(cleaned).length > maxBytes) {
    const ext = extensionOf(cleaned);
    const keepExt = ext.length > 0 && ext.length <= 10 ? ext : '';
    cleaned = capUtf8Bytes(cleaned, maxBytes - utf8.encode(keepExt).length) + keepExt;
  }
  return cleaned === '' ? 'file' : cleaned;
}

function extensionOf(name) {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) return '';
  return name.slice(dot).toLowerCase();
}

// --- media typing ------------------------------------------------------------

// sniffExtension guesses a file extension from magic bytes, for media that
// arrives without a Content-Disposition filename (images/videos usually
// do). Only formats WeCom plausibly delivers; unknown content returns ''
// and the file is saved extension-less rather than mislabeled.
export function sniffExtension(buffer) {
  if (!buffer || buffer.length < 12) return '';
  const b = buffer;
  if (b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff) return '.jpg';
  if (b[0] === 0x89 && b[1] === 0x50 && b[2] === 0x4e && b[3] === 0x47) return '.png';
  if (b[0] === 0x47 && b[1] === 0x49 && b[2] === 0x46 && b[3] === 0x38) return '.gif';
  if (b[0] === 0x42 && b[1] === 0x4d) return '.bmp';
  const ascii = (start, str) => b.subarray(start, start + str.length).toString('latin1') === str;
  if (ascii(0, 'RIFF') && ascii(8, 'WEBP')) return '.webp';
  if (ascii(0, 'RIFF') && ascii(8, 'WAVE')) return '.wav';
  if (ascii(0, '#!AMR')) return '.amr';
  if (ascii(0, 'ID3') || (b[0] === 0xff && (b[1] & 0xe0) === 0xe0)) return '.mp3';
  if (ascii(0, 'OggS')) return '.ogg';
  if (ascii(0, 'fLaC')) return '.flac';
  if (ascii(0, '%PDF')) return '.pdf';
  if (ascii(4, 'ftyp')) {
    // ISO base media: brand decides audio vs video container.
    const brand = b.subarray(8, 12).toString('latin1');
    if (brand.startsWith('M4A')) return '.m4a';
    if (brand === 'qt  ') return '.mov';
    return '.mp4';
  }
  return '';
}

const mimeByExtension = {
  '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg', '.png': 'image/png',
  '.gif': 'image/gif', '.webp': 'image/webp', '.bmp': 'image/bmp',
  '.mp4': 'video/mp4', '.mov': 'video/quicktime', '.avi': 'video/x-msvideo',
  '.mkv': 'video/x-matroska',
  '.m4a': 'audio/mp4', '.amr': 'audio/amr', '.mp3': 'audio/mpeg',
  '.wav': 'audio/wav', '.ogg': 'audio/ogg', '.aac': 'audio/aac',
  '.flac': 'audio/flac', '.opus': 'audio/opus',
  '.pdf': 'application/pdf', '.txt': 'text/plain', '.md': 'text/markdown',
  '.csv': 'text/csv', '.zip': 'application/zip',
  '.doc': 'application/msword',
  '.docx': 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  '.xls': 'application/vnd.ms-excel',
  '.xlsx': 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  '.ppt': 'application/vnd.ms-powerpoint',
  '.pptx': 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
};

export function mimeTypeForFilename(name) {
  return mimeByExtension[extensionOf(name)] ?? '';
}

const audioExtensions = new Set(['.m4a', '.amr', '.mp3', '.wav', '.ogg', '.aac', '.flac', '.opus']);

// isAudioFilename gates the auto-transcription path: only audio sent AS A
// FILE qualifies — WeCom voice messages never reach here (they arrive
// pre-transcribed in voice.content and carry no download URL).
export function isAudioFilename(name) {
  return audioExtensions.has(extensionOf(name));
}

// isValidMediaAesKey gates the download: a WeCom media aeskey must be
// base64 decoding to exactly the AES-256 key length. SDK 1.0.7's
// downloadFile treats a missing/falsy key as "return the raw bytes" — so
// an item that slipped through without a real key would get its
// CIPHERTEXT stored and advertised as decrypted plaintext. Validate
// before downloading and fail the item instead.
export function isValidMediaAesKey(aeskey) {
  if (typeof aeskey !== 'string' || aeskey.length === 0) return false;
  // Strict base64 shape first: Buffer.from(_, 'base64') is lenient and
  // would silently skip garbage characters.
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(aeskey)) return false;
  return Buffer.from(aeskey, 'base64').length === 32;
}

// --- message decomposition ---------------------------------------------------

// mediaItemsForMessage extracts every downloadable attachment from an
// inbound WeCom message: the single top-level media of image/file/video
// messages, and each image item of a mixed message (index carries the
// mixed position so multi-image filenames stay distinct). Items without a
// url are skipped — nothing to download. Quoted media (msg.quote) is
// deliberately excluded: the quoted message was its own inbound delivery
// when it originally arrived.
export function mediaItemsForMessage(msg) {
  const items = [];
  switch (msg?.msgtype) {
    case 'image':
      if (msg.image?.url) items.push({ kind: 'image', url: msg.image.url, aeskey: msg.image.aeskey, index: null });
      break;
    case 'file':
      if (msg.file?.url) items.push({ kind: 'file', url: msg.file.url, aeskey: msg.file.aeskey, index: null });
      break;
    case 'video':
      if (msg.video?.url) items.push({ kind: 'video', url: msg.video.url, aeskey: msg.video.aeskey, index: null });
      break;
    case 'mixed': {
      const parts = msg.mixed?.msg_item ?? [];
      for (let i = 0; i < parts.length; i++) {
        const item = parts[i];
        if (item?.msgtype === 'image' && item.image?.url) {
          items.push({ kind: 'image', url: item.image.url, aeskey: item.image.aeskey, index: i });
        }
      }
      break;
    }
    default:
      break;
  }
  return items;
}

// --- durable store -----------------------------------------------------------

// defaultMediaDir resolves where decrypted inbound media persists. The
// store must survive reboots (unlike slack-full's /tmp spool) because the
// mayor's sessions cite these paths in beads and replies long after
// receipt. Resolution:
//
//   1. WECOM_MEDIA_DIR — explicit override.
//   2. Supervised mode: derive the city dir from GC_SERVICE_SECRETS_DIR
//      (<city>/.gc/services/wecom/secrets → <city>/.gc/wecom-media/inbound)
//      so two cities on one host never interleave their media stores —
//      same per-city hygiene as the secrets file itself.
//   3. Standalone/dev fallback: ~/city/.gc/wecom-media/inbound (the
//      jadegate workspace layout this pack was built for).
export function defaultMediaDir(env = process.env) {
  if (env.WECOM_MEDIA_DIR) return env.WECOM_MEDIA_DIR;
  if (env.GC_SERVICE_SECRETS_DIR) {
    return path.join(path.resolve(env.GC_SERVICE_SECRETS_DIR, '..', '..', '..'), 'wecom-media', 'inbound');
  }
  return path.join(os.homedir(), 'city', '.gc', 'wecom-media', 'inbound');
}

// --- admission control -------------------------------------------------------

// withDeadline bounds a promise with an independent wall-clock timer. The
// SDK's requestTimeout (axios `timeout`) does not cover a response body
// that keeps trickling — a hostile or broken CDN could hold a download
// slot forever. index.js pairs this with a per-request AbortSignal on the
// SDK's HTTP client (which actually severs the socket); this wrapper is
// the promise-level guarantee that hydration unblocks even if that abort
// misfires.
export function withDeadline(promise, ms, label) {
  let timer;
  const timeout = new Promise((resolve, reject) => {
    timer = setTimeout(() => reject(new Error(`${label} exceeded the ${ms}ms wall-clock deadline`)), ms);
    timer.unref?.();
  });
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer));
}

// createDownloadGate bounds how many media downloads hold buffers at once
// across ALL messages — the admission limit that keeps a burst of 200MB
// videos from exhausting adapter memory (worst case = slots × the size
// cap, both configurable). Waiters carry a deadline: a WeCom download URL
// dies ~5 minutes after the message was created, so a slot that cannot be
// had before the deadline REJECTS instead of eventually running a
// download that can only 4xx. The deadline is honored on EVERY admission
// path (codex r2): an already-expired deadline rejects even when a slot
// sits idle, and pump() re-checks before resolving a waiter — the
// waiter's rejection timer can fire late under event-loop delay, and a
// slot freeing up in that window must not admit a dead URL.
//
// acquire(null) waits indefinitely with no deadline — the transcription
// gate uses this (an ElevenLabs call has no URL on a fuse).
export function createDownloadGate(slots, now = Date.now) {
  let inUse = 0;
  const waiters = [];

  const makeRelease = () => {
    let released = false;
    return () => {
      if (released) return;
      released = true;
      inUse--;
      pump();
    };
  };

  const pump = () => {
    while (inUse < slots && waiters.length > 0) {
      const w = waiters.shift();
      if (w.settled) continue; // deadline already rejected it
      w.settled = true;
      if (w.timer) clearTimeout(w.timer);
      if (w.deadlineAt !== null && w.deadlineAt <= now()) {
        // The deadline passed while queued but the timer callback has not
        // run yet — reject here, never hand a dead URL a slot.
        w.reject(new Error('all download slots busy until past the URL expiry deadline'));
        continue;
      }
      inUse++;
      w.resolve(makeRelease());
    }
  };

  return {
    acquire(deadlineAt = null) {
      if (deadlineAt !== null && deadlineAt <= now()) {
        return Promise.reject(new Error('download URL already past its expiry deadline'));
      }
      if (inUse < slots) {
        inUse++;
        return Promise.resolve(makeRelease());
      }
      return new Promise((resolve, reject) => {
        const w = { resolve, reject, deadlineAt, settled: false, timer: null };
        if (deadlineAt !== null) {
          w.timer = setTimeout(() => {
            if (w.settled) return;
            w.settled = true;
            reject(new Error('all download slots busy until past the URL expiry deadline'));
          }, Math.max(0, deadlineAt - now()));
          w.timer.unref?.();
        }
        waiters.push(w);
      });
    },
  };
}

// createStoreQuota guards the durable store: a total-usage quota plus a
// minimum-free-disk check, evaluated BEFORE each save. On breach the save
// is rejected with a failure note — never by deleting anything: the store
// is append-only by fleet policy (no unapproved deletion), so reclaiming
// space is a human/mayor decision. Usage is scanned lazily once per
// process and tracked incrementally after that; out-of-band changes to
// the store drift the cached figure until the next adapter restart —
// acceptable for a guard whose job is stopping runaway growth, not
// accounting.
export function createStoreQuota({ dir, quotaBytes, minFreeBytes, statfs = fs.statfsSync }) {
  let used = null;

  const scan = (d) => {
    let total = 0;
    let entries;
    try {
      entries = fs.readdirSync(d, { withFileTypes: true });
    } catch {
      return 0; // store not created yet
    }
    for (const e of entries) {
      const p = path.join(d, e.name);
      try {
        if (e.isDirectory()) total += scan(p);
        else if (e.isFile()) total += fs.statSync(p).size;
      } catch { /* raced with external change; skip */ }
    }
    return total;
  };

  // statfs needs an existing path; before the first save the store dir
  // may not exist yet, so walk up to the nearest existing ancestor.
  const freeBytes = () => {
    let p = dir;
    for (;;) {
      try {
        const s = statfs(p);
        return Number(s.bavail) * Number(s.bsize);
      } catch (err) {
        const parent = path.dirname(p);
        if (parent === p) throw err;
        p = parent;
      }
    }
  };

  return {
    admit(addBytes) {
      if (used === null) used = scan(dir);
      if (quotaBytes > 0 && used + addBytes > quotaBytes) {
        return {
          ok: false,
          reason: `media store quota exceeded (${used} bytes stored + ${addBytes} > ${quotaBytes} cap; the store is append-only — pruning is a manual decision)`,
        };
      }
      if (minFreeBytes > 0) {
        let free;
        try {
          free = freeBytes();
        } catch {
          return { ok: true }; // a statfs failure must not block saves
        }
        if (free - addBytes < minFreeBytes) {
          return {
            ok: false,
            reason: `insufficient free disk space (${free} bytes free, ${minFreeBytes} minimum required after save)`,
          };
        }
      }
      return { ok: true };
    },
    recordSaved(bytes) {
      if (used !== null) used += bytes;
    },
  };
}

// --- ElevenLabs Scribe transcription ----------------------------------------

// resolveElevenLabsKey returns the Scribe API key, or '' when none is
// configured (the caller downgrades to a transcription-failed note — a
// missing key must not drop the message). Resolution order matches the
// mayor's reference script: $ELEVENLABS_API_KEY, then
// ~/.config/elevenlabs/api-key. Read per call, not cached — the key file
// appearing later starts working without an adapter restart.
export function resolveElevenLabsKey(env = process.env, readFile = fs.readFileSync) {
  if (env.ELEVENLABS_API_KEY) return env.ELEVENLABS_API_KEY.trim();
  try {
    return readFile(path.join(os.homedir(), '.config', 'elevenlabs', 'api-key'), 'utf8').trim();
  } catch {
    return '';
  }
}

// formatTranscript flattens a Scribe response into inline text. With
// diarization on, a multi-speaker recording renders as speaker turns
// ("speaker_0: …") — same shape the mayor's reference pipeline produces;
// single-speaker (or word-less) responses collapse to the plain text
// field.
export function formatTranscript(result) {
  const words = Array.isArray(result?.words) ? result.words : [];
  const speakers = new Set(words.map((w) => w.speaker_id).filter((s) => s != null));
  if (speakers.size <= 1) return (result?.text ?? '').trim();
  const turns = [];
  let current = null;
  for (const w of words) {
    if (w.text == null) continue;
    if (!current || w.speaker_id !== current.speaker) {
      current = { speaker: w.speaker_id, text: '' };
      turns.push(current);
    }
    current.text += w.text;
  }
  return turns.map((t) => `${t.speaker ?? 'speaker'}: ${t.text.trim()}`).join('\n').trim();
}

// transcribeAudio runs one saved audio file through ElevenLabs Scribe
// (model scribe_v1, diarization on, audio-event tags off — the pattern
// from ~/city/assets/china-braindump-part1/transcribe-scribe.sh). The
// language is auto-detected unless the caller pins one
// (WECOM_TRANSCRIBE_LANGUAGE) — the China team mostly sends Mandarin, but
// pinning zh would mangle the occasional English recording. Throws on any
// failure; the caller renders the note.
export async function transcribeAudio(buffer, filename, opts = {}) {
  const {
    apiKey,
    fetchImpl = fetch,
    timeoutMs = 180000,
    language = '',
  } = opts;
  if (!apiKey) throw new Error('no ElevenLabs API key (set ELEVENLABS_API_KEY or ~/.config/elevenlabs/api-key)');
  const form = new FormData();
  form.append('file', new Blob([buffer]), filename);
  form.append('model_id', 'scribe_v1');
  form.append('diarize', 'true');
  form.append('tag_audio_events', 'false');
  if (language) form.append('language_code', language);
  const resp = await fetchImpl('https://api.elevenlabs.io/v1/speech-to-text', {
    method: 'POST',
    headers: { 'xi-api-key': apiKey },
    body: form,
    signal: AbortSignal.timeout(timeoutMs),
  });
  if (!resp.ok) {
    const text = (await resp.text().catch(() => '')).trim();
    throw new Error(`ElevenLabs ${resp.status}: ${text.slice(0, 200)}`);
  }
  const result = await resp.json();
  const transcript = formatTranscript(result);
  if (!transcript) throw new Error('empty transcript');
  return transcript;
}

// --- hydration orchestrator --------------------------------------------------

// hydrateMessageMedia downloads, decrypts, stores, and (for audio files)
// transcribes every attachment of one inbound message. NEVER rejects and
// never throws: each item settles into either a saved attachment or a
// per-item failure note, and the returned block always names every
// attachment the message carried (mirroring slack-full's "every file is
// named even when a download failed" contract). Items run CONCURRENTLY —
// a mixed message's third image must not wait out two stalled downloads
// while its own URL burns through the ~5-minute expiry; the shared gate
// (below) is what bounds total in-flight downloads instead.
//
// Returns { attachments, block }:
//   attachments — extmsg ExternalAttachment records ({provider_id,
//                 url: file:// URL, mime_type}) for items that saved
//                 successfully; the URL never carries a token.
//   block       — text appended to the delivered message: the
//                 "[N WeCom file(s) attached]" listing plus any audio
//                 transcript / failure notes.
//
// deps:
//   downloadFile(url, aeskey) → {buffer, filename}   (SDK download+decrypt,
//                             wall-clock bounded by the caller)
//   mediaDir                  durable store root
//   maxBytes                  size cap (also enforced upstream via axios
//                             maxContentLength; this is the belt-and-braces
//                             check on the decrypted plaintext)
//   transcribe(buffer, filename) → transcript         (audio files only)
//   gate                      shared download-admission gate
//                             (createDownloadGate); optional
//   transcribeGate            shared transcription-admission gate
//                             (createDownloadGate, deadline-less acquire);
//                             bounds how many audio files are re-read
//                             from disk at once; optional
//   quota                     durable-store guard (createStoreQuota);
//                             optional
//   urlTtlMs                  admission deadline relative to the message's
//                             create_time (default 270s ≈ the 5-minute URL
//                             lifetime minus headroom for the download)
//   now                       clock (tests)
//   log(...)                  adapter logger
export async function hydrateMessageMedia(msg, deps) {
  const {
    downloadFile, mediaDir, maxBytes, transcribe, log = () => {},
    gate = null, transcribeGate = null, quota = null,
    urlTtlMs = 270000, now = Date.now,
  } = deps;
  const items = mediaItemsForMessage(msg);
  if (items.length === 0) return { attachments: [], block: '' };

  const conversationId = msg.chattype === 'group' ? msg.chatid : msg.from?.userid;
  const convDir = path.join(mediaDir, safePathComponent(conversationId ?? 'unknown'));
  const msgidPrefix = safePathComponent(msg.msgid ?? 'nomsgid');

  // The URL lifetime anchors to when WeCom minted it — the message's
  // create_time — not to when this hydration got scheduled.
  const anchorMs = msg.create_time ? msg.create_time * 1000 : now();
  const deadlineAt = anchorMs + urlTtlMs;

  const results = await Promise.all(items.map(async (item, n) => {
    const label = items.length > 1 ? `${item.kind} ${n + 1}` : `${item.kind}`;
    const fail = (name, reason) => ({ line: failureLine(n, name, reason) });

    // A missing/malformed aeskey must fail the item BEFORE download: the
    // SDK returns raw bytes when the key is falsy, and storing ciphertext
    // as if it were the file is worse than no file.
    if (!isValidMediaAesKey(item.aeskey)) {
      log(`inbound ${msg.msgid}: ${label} has no valid aeskey; not downloaded`);
      return fail(`[${item.kind}]`, 'missing or invalid decryption key');
    }

    let release = () => {};
    if (gate) {
      try {
        release = await gate.acquire(deadlineAt);
      } catch (err) {
        log(`inbound ${msg.msgid}: ${label} download not started: ${scrubErrorMessage(err.message)}`);
        return fail(`[${item.kind}]`, `download not started (${scrubErrorMessage(err.message)})`);
      }
    }

    // The slot is held from download through the disk write — the window
    // in which the (up to maxBytes) buffer must exist — and released
    // before transcription, which only ever holds small audio files.
    let saved;
    try {
      let result;
      try {
        result = await downloadFile(item.url, item.aeskey);
      } catch (err) {
        log(`inbound ${msg.msgid}: ${label} download failed: ${scrubErrorMessage(err.message)}`);
        return fail(`[${item.kind}]`, `download failed (${scrubErrorMessage(err.message)})`);
      }
      const buffer = result?.buffer;
      if (!buffer || buffer.length === 0) {
        return fail(`[${item.kind}]`, 'download returned no data');
      }
      if (maxBytes > 0 && buffer.length > maxBytes) {
        return fail(`[${item.kind}]`, `file too large (${buffer.length} bytes > ${maxBytes} cap)`);
      }

      // Filename: sender's original (Content-Disposition) when present;
      // otherwise the media kind plus a magic-byte extension so agent-side
      // type detection still works on WeCom's nameless images/videos.
      let name = result.filename ? safeFilename(result.filename) : '';
      if (!name || name === 'file') {
        name = item.kind + sniffExtension(buffer);
      } else if (!extensionOf(name)) {
        name += sniffExtension(buffer);
      }
      const indexPart = item.index === null ? '' : `${item.index}-`;
      const dest = path.join(convDir, `${msgidPrefix}-${indexPart}${name}`);

      // Quota is charged on the DELTA against whatever already sits at
      // dest: a rare duplicate hydration of the same msgid overwrites the
      // same file, and double-counting it would drift the cached usage
      // upward into premature quota rejections (codex r2).
      let prevSize = 0;
      try {
        prevSize = fs.statSync(dest).size;
      } catch { /* new file */ }
      const deltaBytes = buffer.length - prevSize;
      if (quota) {
        const verdict = quota.admit(deltaBytes);
        if (!verdict.ok) {
          log(`inbound ${msg.msgid}: ${label} save rejected: ${verdict.reason}`);
          return fail(`[${item.kind}]`, verdict.reason);
        }
      }

      try {
        // 0o700 dirs / 0o600 files: the store holds private DM content.
        fs.mkdirSync(convDir, { recursive: true, mode: 0o700 });
        fs.writeFileSync(dest, buffer, { mode: 0o600 });
      } catch (err) {
        log(`inbound ${msg.msgid}: ${label} write failed: ${scrubErrorMessage(err.message)}`);
        return fail(name, `could not save to disk (${scrubErrorMessage(err.message)})`);
      }
      quota?.recordSaved(deltaBytes);
      log(`inbound ${msg.msgid}: ${label} saved (${buffer.length} bytes)`);

      // The download buffer is dropped HERE, at the write — for every
      // media kind, audio included. Transcription re-reads the bytes from
      // disk after its own gate admission below, so audio never extends
      // the download gate's slots × cap memory bound (codex r2). The
      // size + sha256 pin down exactly what was written: the gated reread
      // refuses anything else at that pathname (codex r3).
      const wantTranscript = item.kind === 'file' && isAudioFilename(name) && !!transcribe;
      saved = {
        name,
        dest,
        wantTranscript,
        size: wantTranscript ? buffer.length : 0,
        digest: wantTranscript ? crypto.createHash('sha256').update(buffer).digest('hex') : '',
      };
    } finally {
      release();
    }

    const { name, dest, wantTranscript } = saved;
    const mime = mimeTypeForFilename(name);
    const out = {
      line: `  ${n + 1}. ${neutralizeMarkupBoundaries(name)} (${mime || item.kind}) — saved to ${neutralizeMarkupBoundaries(dest)}; Read that path to view it`,
      attachment: {
        provider_id: item.index === null ? (msg.msgid ?? '') : `${msg.msgid ?? ''}/${item.index}`,
        url: pathToFileURL(dest).href,
        ...(mime ? { mime_type: mime } : {}),
      },
    };

    // Audio FILES get an inline Scribe transcript; images/videos are read
    // directly by the receiving agent, no transcription. A transcription
    // failure is a note, never a dropped message. The bytes are loaded
    // FROM DISK only after the (deadline-less) transcription gate admits
    // us, so concurrent audio memory is bounded at transcription slots ×
    // file size — independent of the download gate, whose slot this item
    // already released at the write.
    if (wantTranscript) {
      let releaseTranscribe = () => {};
      try {
        if (transcribeGate) releaseTranscribe = await transcribeGate.acquire();
        const audioBytes = await readVerifiedAudio(dest, saved.size, saved.digest);
        const transcript = await transcribe(audioBytes, name);
        out.extra = `[audio transcript — ${neutralizeMarkupBoundaries(name)}]\n${neutralizeMarkupBoundaries(transcript)}`;
      } catch (err) {
        log(`inbound ${msg.msgid}: transcription failed: ${scrubErrorMessage(err.message)}`);
        out.extra = `[transcription failed: ${scrubErrorMessage(err.message)} — the audio file is saved at ${neutralizeMarkupBoundaries(dest)}]`;
      } finally {
        releaseTranscribe();
      }
    }
    return out;
  }));

  const attachments = results.filter((r) => r.attachment).map((r) => r.attachment);
  const lines = results.map((r) => r.line);
  const extras = results.filter((r) => r.extra).map((r) => r.extra);

  const noun = items.length === 1 ? 'file' : 'files';
  const block = [`[${items.length} WeCom ${noun} attached]`, ...lines, ...extras].join('\n');
  return { attachments, block };
}

// readVerifiedAudio re-reads a saved audio file for transcription and
// proves it is byte-identical to what the adapter wrote (codex r3): the
// pathname sits in a window between save and gated transcription, and a
// replacement, symlink swap, or oversized file there must never reach
// Scribe or dodge the size invariant. O_NOFOLLOW makes a symlink at the
// final component fail the open outright; the stat runs on the OPEN FD
// (no path race) and must show a regular file of the exact written size;
// the sha256 of the bytes actually read must match the digest computed
// from the download buffer at write time.
async function readVerifiedAudio(dest, expectedSize, expectedDigest) {
  let fd;
  try {
    fd = await fs.promises.open(dest, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW);
  } catch (err) {
    throw new Error(`saved audio file changed after save (open failed: ${err.code ?? err.message})`);
  }
  try {
    const st = await fd.stat();
    if (!st.isFile()) {
      throw new Error('saved audio file changed after save (no longer a regular file)');
    }
    if (st.size !== expectedSize) {
      throw new Error('saved audio file changed after save (size mismatch)');
    }
    const bytes = Buffer.alloc(expectedSize);
    let offset = 0;
    while (offset < expectedSize) {
      const { bytesRead } = await fd.read(bytes, offset, expectedSize - offset, offset);
      if (bytesRead === 0) {
        throw new Error('saved audio file changed after save (short read)');
      }
      offset += bytesRead;
    }
    const digest = crypto.createHash('sha256').update(bytes).digest('hex');
    if (digest !== expectedDigest) {
      throw new Error('saved audio file changed after save (content digest mismatch)');
    }
    return bytes;
  } finally {
    await fd.close();
  }
}

// failureLine renders one undeliverable attachment. The expiry note
// matters: unlike Slack (where the bytes stay fetchable via the API), a
// WeCom URL is dead ~5 minutes after receipt — there is no later recovery
// path, so the agent should ask the sender to re-send when the content is
// needed.
function failureLine(n, name, reason) {
  return `  ${n + 1}. ${neutralizeMarkupBoundaries(name)} — ${reason}; WeCom download URLs expire ~5 minutes after receipt, so the bytes are not recoverable — ask the sender to re-send if needed`;
}
