// media.js — inbound WhatsApp media: download through the gateway's
// GET /v1/media/<id>, verify the webhook's sha256, persist, and transcribe
// audio, on the WeCom pack's media.js shape (its store quota, free-disk
// floor, ElevenLabs Scribe call and key fallback, and its "[N files
// attached] ... saved to <path>; Read that path to view it" block).
//
// The box never holds a Meta token: the gateway mints Meta's URL and
// streams the bytes for a media id that was delivered to this city. The
// webhook's media object (image, document, audio, video, sticker) carries
// {id, mime_type, sha256, filename?, caption?, voice?}; Meta writes the
// sha256 base64-encoded, and hex is accepted too. A download whose digest
// does not match is refused: nothing is saved and the message carries the
// note.
//
// THE STORE. <mediaDir>/<conversation_id>/<wamid>-<safe filename> (the
// owner's key, render.js conversationOf; for an owner of the contract v1 its
// number, the directory it always had), directories 0700
// and files 0600, written atomically (temp, fsync, rename). The name comes
// from the webhook alone (the document's filename, else the kind and an
// extension from the mime type), so a re-hydrate of the same message (a
// restart, the in-doubt rule's marked copy) finds the file and, when its
// hash matches, reuses it without a second download. An audio transcript
// is cached beside it as <file>.transcript.txt (0600), so Scribe is asked
// once per file.
//
// THE LIMITS (WHATSAPP_MEDIA_* in index.js, mirroring wecom's): maxBytes
// per file (default 100MB), quotaBytes over the whole store (default
// 10GiB, scanned once and tracked incrementally), and minFreeBytes left on
// the disk after a save (default 5GiB, fs.statfs). A breach rejects the
// save with a note; nothing is ever deleted to make room (the store is
// append-only; pruning is a human's decision).
//
// TRANSCRIPTS. Audio, a voice note included, goes through ElevenLabs
// Scribe (model scribe_v1, multipart, header xi-api-key) with the key from
// apiKey() when given, else $ELEVENLABS_API_KEY, else
// ~/.config/elevenlabs/api-key, read per call. Without a key, or on any
// Scribe failure, the file is still saved and the block says "[voice note
// saved to <path>; not transcribed: no key]" (or the failure's reason).
//
// hydrate NEVER rejects: every failure returns ok:false and a one-line
// note for the message text. Never logged: a phone number in clear (last
// four digits only), a wamid (it encodes the phone), a path (it holds the
// conversation id), message text, a caption, a transcript, or the key.

import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

import { lastFour, writeFileAtomic } from './ledger.js';

const KINDS = new Set(['image', 'document', 'audio', 'video', 'sticker']);
const TRANSCRIPT_SUFFIX = '.transcript.txt';
const utf8 = new TextEncoder();

// --- text hygiene ------------------------------------------------------------

// A zero-width space after every '<' in provider-controlled text, so a
// sender cannot forge a closing tag inside the reminder envelope gc wraps
// the message in (the WeCom and slack-full neutralizer; idempotent).
const zwsp = '​';
export function neutralizeMarkupBoundaries(s) {
  s = String(s ?? '');
  if (!s.includes('<')) return s;
  let out = '';
  for (let i = 0; i < s.length; i++) {
    out += s[i];
    if (s[i] === '<' && !s.startsWith(zwsp, i + 1)) out += zwsp;
  }
  return out;
}

function oneLine(s, max = 200) {
  let t = String(s ?? '').replace(/https?:\/\/\S+/g, '[url]').replace(/\s+/g, ' ').trim();
  if (t.length > max) t = `${t.slice(0, max)}…`;
  return neutralizeMarkupBoundaries(t);
}

// --- names -------------------------------------------------------------------

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

// safePathComponent: [A-Za-z0-9_.-] only, never a leading dot, capped; an
// id longer than the cap keeps a digest of the whole so two ids never meet.
export function safePathComponent(s, maxLen = 80) {
  const raw = String(s ?? '');
  let cleaned = '';
  for (const ch of raw) cleaned += /[A-Za-z0-9_.-]/.test(ch) ? ch : '_';
  if (cleaned.startsWith('.')) cleaned = `_${cleaned.slice(1)}`;
  if (cleaned.length > maxLen) {
    const digest = crypto.createHash('sha256').update(raw).digest('hex').slice(0, 12);
    cleaned = `${cleaned.slice(0, maxLen - 13)}~${digest}`;
  }
  return cleaned === '' ? '_' : cleaned;
}

function extensionOf(name) {
  const dot = name.lastIndexOf('.');
  if (dot <= 0 || dot === name.length - 1) return '';
  return name.slice(dot).toLowerCase();
}

// safeFilename keeps the sender's filename recognizable while defusing
// what could escape or damage the store: separators, NUL and control
// characters, angle brackets (so a path quoted in the message text never
// needs neutralizing), leading dots. Capped at 140 UTF-8 bytes so
// "<wamid>-<name>.transcript.txt" stays under the 255-byte component limit;
// a short extension survives the cap.
export function safeFilename(name) {
  const trimmed = String(name ?? '').trim();
  if (trimmed === '') return 'file';
  let cleaned = '';
  for (const ch of trimmed) {
    const code = ch.codePointAt(0);
    cleaned += (ch === '/' || ch === '\\' || ch === '<' || ch === '>' || code < 0x20 || code === 0x7f) ? '_' : ch;
  }
  while (cleaned.startsWith('.')) cleaned = `_${cleaned.slice(1)}`;
  const maxBytes = 140;
  if (utf8.encode(cleaned).length > maxBytes) {
    const ext = extensionOf(cleaned);
    const keepExt = ext.length > 0 && ext.length <= 10 ? ext : '';
    cleaned = capUtf8Bytes(cleaned, maxBytes - utf8.encode(keepExt).length) + keepExt;
  }
  return cleaned === '' ? 'file' : cleaned;
}

const extensionByMime = {
  'image/jpeg': '.jpg', 'image/png': '.png', 'image/webp': '.webp', 'image/gif': '.gif',
  'video/mp4': '.mp4', 'video/3gpp': '.3gp', 'video/quicktime': '.mov',
  'audio/ogg': '.ogg', 'audio/opus': '.opus', 'audio/mpeg': '.mp3', 'audio/mp4': '.m4a',
  'audio/aac': '.aac', 'audio/amr': '.amr', 'audio/wav': '.wav', 'audio/x-wav': '.wav',
  'application/pdf': '.pdf', 'text/plain': '.txt', 'text/csv': '.csv', 'application/zip': '.zip',
  'application/msword': '.doc', 'application/vnd.ms-excel': '.xls', 'application/vnd.ms-powerpoint': '.ppt',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document': '.docx',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet': '.xlsx',
  'application/vnd.openxmlformats-officedocument.presentationml.presentation': '.pptx',
};

const mimeEssence = (mime) => String(mime ?? '').split(';')[0].trim().toLowerCase();

// The stored name, from the webhook alone (deterministic, so a re-hydrate
// finds the same path before any download).
function storedName(kind, media) {
  const ext = extensionByMime[mimeEssence(media.mime_type)] ?? '';
  if (typeof media.filename === 'string' && media.filename.trim()) {
    const name = safeFilename(media.filename);
    return extensionOf(name) || !ext ? name : name + ext;
  }
  const base = kind === 'audio' && media.voice ? 'voice' : kind;
  return `${base}${ext}`;
}

// --- digests -----------------------------------------------------------------

// The webhook's sha256 as 32 bytes: hex, or base64 (standard or url-safe,
// padded or not). null when absent; false when present but unreadable.
export function parseSha256(value) {
  if (value === undefined || value === null || value === '') return null;
  if (typeof value !== 'string') return false;
  const s = value.trim();
  if (/^[0-9a-fA-F]{64}$/.test(s)) return Buffer.from(s, 'hex');
  if (/^[A-Za-z0-9+/_-]+={0,2}$/.test(s)) {
    const bytes = Buffer.from(s.replace(/-/g, '+').replace(/_/g, '/'), 'base64');
    if (bytes.length === 32) return bytes;
  }
  return false;
}

const digestOf = (bytes) => crypto.createHash('sha256').update(bytes).digest();

// --- the store's limits ----------------------------------------------------------

// The WeCom store guard: a whole-store quota (scanned once, then tracked)
// and a free-disk floor, both checked before a save, never by deleting.
export function createStoreQuota({ dir, quotaBytes, minFreeBytes, statfs = fs.statfsSync }) {
  let used = null;
  const scan = (d) => {
    let total = 0;
    let entries;
    try { entries = fs.readdirSync(d, { withFileTypes: true }); } catch { return 0; }
    for (const e of entries) {
      const p = path.join(d, e.name);
      try {
        if (e.isDirectory()) total += scan(p);
        else if (e.isFile()) total += fs.statSync(p).size;
      } catch { /* raced with a change; skip */ }
    }
    return total;
  };
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
          reason: `the media store quota would be exceeded (${used} bytes stored + ${addBytes} > the ${quotaBytes}-byte quota; nothing is pruned automatically)`,
        };
      }
      if (minFreeBytes > 0) {
        let free;
        try { free = freeBytes(); } catch { return { ok: true }; } // a statfs failure never blocks a save
        if (free - addBytes < minFreeBytes) {
          return {
            ok: false,
            reason: `not enough free disk (${free} bytes free; ${minFreeBytes} must remain after the save)`,
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

// --- ElevenLabs Scribe -----------------------------------------------------------

// The key: $ELEVENLABS_API_KEY, else ~/.config/elevenlabs/api-key; '' when
// neither. Read per call, so a key file added later works without a restart.
export function resolveElevenLabsKey(env = process.env, readFile = fs.readFileSync) {
  if (env.ELEVENLABS_API_KEY) return String(env.ELEVENLABS_API_KEY).trim();
  try {
    return String(readFile(path.join(os.homedir(), '.config', 'elevenlabs', 'api-key'), 'utf8')).trim();
  } catch {
    return '';
  }
}

// A Scribe response as inline text: speaker turns when diarization found
// more than one speaker, else the plain text.
export function formatTranscript(result) {
  const words = Array.isArray(result?.words) ? result.words : [];
  const speakers = new Set(words.map((w) => w.speaker_id).filter((s) => s != null));
  if (speakers.size <= 1) return String(result?.text ?? '').trim();
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

export async function transcribeWithScribe(buffer, filename, { apiKey, url, timeoutMs = 180000, mime = '', fetchImpl = fetch }) {
  const form = new FormData();
  form.append('file', new Blob([buffer], mime ? { type: mime } : {}), filename);
  form.append('model_id', 'scribe_v1');
  form.append('diarize', 'true');
  form.append('tag_audio_events', 'false');
  const resp = await fetchImpl(url, {
    method: 'POST',
    headers: { 'xi-api-key': apiKey },
    body: form,
    signal: AbortSignal.timeout(timeoutMs),
  });
  if (!resp.ok) {
    await resp.text().catch(() => '');
    throw new Error(`Scribe answered ${resp.status}`);
  }
  const transcript = formatTranscript(await resp.json());
  if (!transcript) throw new Error('Scribe returned an empty transcript');
  return transcript;
}

// --- hydration -----------------------------------------------------------------

function describeGatewayError(err) {
  if (err?.config) return 'WHATSAPP_GATEWAY_URL is not set';
  if (err?.status === 401 || err?.auth) return 'the gateway refused this city\'s credential (not enrolled: run gc whatsapp enroll)';
  if (err?.status === 404) return 'the gateway does not hold this media (404); ask the sender to send it again if it is needed';
  if (Number.isInteger(err?.status)) return `the gateway answered ${err.status}`;
  if (err?.noAnswer) return 'the gateway did not answer';
  return 'the download failed';
}

// readStored returns the bytes of a regular file at p, or null.
function readStored(p) {
  try {
    const st = fs.lstatSync(p);
    if (!st.isFile() || st.size === 0) return null;
    return fs.readFileSync(p);
  } catch {
    return null;
  }
}

export function createMedia({
  mediaDir,
  gateway,
  log = () => {},
  maxBytes = 104857600,
  quotaBytes = 10 * 2 ** 30,
  minFreeBytes = 5 * 2 ** 30,
  transcribe,
  apiKey,
  scribeURL = 'https://api.elevenlabs.io/v1/speech-to-text',
  transcribeTimeoutMs = 180000,
  statfs = fs.statfsSync,
  fetchImpl = fetch,
}) {
  const quota = createStoreQuota({ dir: mediaDir, quotaBytes, minFreeBytes, statfs });
  const keyOf = typeof apiKey === 'function' ? apiKey : () => resolveElevenLabsKey();

  // The transcript for saved audio: the cache beside the file, else Scribe
  // (or the injected transcribe), cached on success. Returns {transcript}
  // or {transcriptNote}.
  async function transcriptFor(bytes, dest, name, mime, who) {
    const cacheFile = `${dest}${TRANSCRIPT_SUFFIX}`;
    const cached = readStored(cacheFile);
    if (cached) return { transcript: cached.toString('utf8') };
    let transcript;
    try {
      if (typeof transcribe === 'function') {
        transcript = String(await transcribe(bytes, name) ?? '').trim();
      } else {
        const key = String(keyOf() ?? '').trim();
        if (!key) return { transcriptNote: 'no key' };
        transcript = await transcribeWithScribe(bytes, name, {
          apiKey: key, url: scribeURL, timeoutMs: transcribeTimeoutMs, mime: mimeEssence(mime), fetchImpl,
        });
      }
    } catch (err) {
      const reason = err?.noKey ? 'no key' : oneLine(err?.message ?? err, 120);
      log(`media ${who}: not transcribed (${reason})`);
      return { transcriptNote: reason };
    }
    if (!transcript) return { transcriptNote: 'empty transcript' };
    try {
      writeFileAtomic(cacheFile, transcript);
    } catch (err) {
      log(`media ${who}: transcript not cached (${err.code ?? err.message})`);
    }
    return { transcript };
  }

  async function hydrateOne(input) {
    const kind = KINDS.has(input?.kind) ? input.kind : 'document';
    const m = input?.media;
    const voice = kind === 'audio' && Boolean(m?.voice);
    const label = voice ? 'voice note' : kind;
    const conversationId = String(input?.conversationId ?? '').trim();
    const who = `…${lastFour(conversationId)} ${label}`;
    const fail = (reason) => {
      log(`media ${who}: not saved (${reason})`);
      return { ok: false, kind, label, note: `${label} not saved: ${reason}` };
    };
    if (m === null || typeof m !== 'object' || !String(m.id ?? '').trim()) return fail('the webhook carried no media id');
    const mediaId = String(m.id).trim();
    const expected = parseSha256(m.sha256);
    if (expected === false) return fail('the webhook\'s sha256 is unreadable, so the download cannot be verified (refused)');

    const wamid = String(input?.wamid ?? '').trim() || mediaId;
    const convDir = path.join(mediaDir, safePathComponent(conversationId || 'unknown'));
    const name = storedName(kind, m);
    const dest = path.join(convDir, `${safePathComponent(wamid)}-${name}`);

    // A re-hydrate: the file is already there with the webhook's hash.
    let bytes = readStored(dest);
    let contentType = '';
    const reused = Boolean(bytes) && (!expected || digestOf(bytes).equals(expected));
    if (!reused) {
      const previousSize = bytes ? bytes.length : 0;
      let got;
      try {
        got = await gateway.media(mediaId);
      } catch (err) {
        // A transient failure (no answer, 5xx, 408, 429) is not final: the
        // caller retries before it posts, the gateway still holding the delivery.
        const transient = Boolean(err?.noAnswer) || err?.status >= 500 || err?.status === 408 || err?.status === 429;
        return { ...fail(describeGatewayError(err)), transient };
      }
      bytes = got?.bytes;
      contentType = String(got?.contentType ?? '');
      if (!Buffer.isBuffer(bytes) || bytes.length === 0) return fail('the gateway returned no bytes');
      if (maxBytes > 0 && bytes.length > maxBytes) return fail(`file too large (${bytes.length} bytes > the ${maxBytes}-byte cap)`);
      if (expected && !digestOf(bytes).equals(expected)) {
        return fail('the download\'s sha256 does not match the webhook\'s (refused)');
      }
      if (!expected) log(`media ${who}: the webhook carried no sha256; saved unverified`);
      const verdict = quota.admit(bytes.length - previousSize);
      if (!verdict.ok) return fail(verdict.reason);
      try {
        fs.mkdirSync(convDir, { recursive: true, mode: 0o700 });
        try { fs.chmodSync(convDir, 0o700); } catch { /* not ours to change */ }
        if (previousSize) fs.rmSync(`${dest}${TRANSCRIPT_SUFFIX}`, { force: true }); // stale for new bytes
        writeFileAtomic(dest, bytes, { mode: 0o600, dirMode: 0o700 });
      } catch (err) {
        return fail(`could not save to disk (${err.code ?? 'write error'})`);
      }
      quota.recordSaved(bytes.length - previousSize);
      log(`media ${who}: saved (${bytes.length} bytes)`);
    }

    const mime = String(m.mime_type ?? '').trim() || contentType.trim();
    const result = {
      ok: true,
      kind,
      label,
      voice,
      path: dest,
      filename: path.basename(dest),
      mime_type: mime,
      size: bytes.length,
      reused,
      attachment: { provider_id: mediaId, url: pathToFileURL(dest).href, ...(mime ? { mime_type: mime } : {}) },
      note: `${path.basename(dest)} (${mime || label}) saved to ${dest}; Read that path to view it`,
    };
    if (kind === 'audio') Object.assign(result, await transcriptFor(bytes, dest, name, mime, who));
    return result;
  }

  return {
    async hydrate(input) {
      try {
        return await hydrateOne(input);
      } catch (err) {
        const kind = KINDS.has(input?.kind) ? input.kind : 'document';
        log(`media …${lastFour(input?.conversationId)} ${kind}: hydration failed inside the adapter (${err?.code ?? 'error'})`);
        return { ok: false, kind, label: kind, note: `${kind} not saved: an internal error in the adapter` };
      }
    },
    describe: describeAttachments,
  };
}

// describeAttachments renders hydrate results as the block appended to the
// message text (the caption is the renderer's, never repeated here):
//
//   [N WhatsApp file(s) attached]
//     1. <name> (<mime>) saved to <path>; Read that path to view it
//     2. <kind> not saved: <reason>
//   [voice note transcript: <name>]
//   <transcript>
//   [voice note saved to <path>; not transcribed: no key]
export function describeAttachments(results) {
  const list = (Array.isArray(results) ? results : [results]).filter((r) => r && typeof r === 'object');
  if (list.length === 0) return '';
  const lines = [`[${list.length} WhatsApp ${list.length === 1 ? 'file' : 'files'} attached]`];
  // Notes are built above from sanitized parts (a stored path never holds
  // '<' or a control character), so a path is quoted exactly as on disk.
  list.forEach((r, i) => lines.push(`  ${i + 1}. ${String(r.note ?? '').replace(/[\r\n]+/g, ' ')}`));
  for (const r of list) {
    if (!r.ok || r.kind !== 'audio') continue;
    if (r.transcript) {
      lines.push(`[${r.label} transcript: ${r.filename}]`, neutralizeMarkupBoundaries(r.transcript));
    } else {
      lines.push(`[${r.label} saved to ${r.path}; not transcribed: ${oneLine(r.transcriptNote || 'no transcript', 200)}]`);
    }
  }
  return lines.join('\n');
}
