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

import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

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
// named even when a download failed" contract).
//
// Returns { attachments, block }:
//   attachments — extmsg ExternalAttachment records ({provider_id,
//                 url: 'file://<abs path>', mime_type}) for items that
//                 saved successfully; the URL never carries a token.
//   block       — text appended to the delivered message: the
//                 "[N WeCom file(s) attached]" listing plus any audio
//                 transcript / failure notes.
//
// deps:
//   downloadFile(url, aeskey) → {buffer, filename}   (SDK download+decrypt)
//   mediaDir                  durable store root
//   maxBytes                  size cap (also enforced upstream via axios
//                             maxContentLength; this is the belt-and-braces
//                             check on the decrypted plaintext)
//   transcribe(buffer, filename) → transcript         (audio files only)
//   log(...)                  adapter logger
export async function hydrateMessageMedia(msg, deps) {
  const { downloadFile, mediaDir, maxBytes, transcribe, log = () => {} } = deps;
  const items = mediaItemsForMessage(msg);
  if (items.length === 0) return { attachments: [], block: '' };

  const conversationId = msg.chattype === 'group' ? msg.chatid : msg.from?.userid;
  const convDir = path.join(mediaDir, safePathComponent(conversationId ?? 'unknown'));
  const msgidPrefix = safePathComponent(msg.msgid ?? 'nomsgid');

  const attachments = [];
  const lines = [];
  const extras = [];

  for (let n = 0; n < items.length; n++) {
    const item = items[n];
    const label = items.length > 1 ? `${item.kind} ${n + 1}` : `${item.kind}`;
    let result;
    try {
      result = await downloadFile(item.url, item.aeskey);
    } catch (err) {
      log(`inbound ${msg.msgid}: ${label} download failed: ${scrubErrorMessage(err.message)}`);
      lines.push(failureLine(n, `[${item.kind}]`, `download failed (${scrubErrorMessage(err.message)})`));
      continue;
    }
    const buffer = result?.buffer;
    if (!buffer || buffer.length === 0) {
      lines.push(failureLine(n, `[${item.kind}]`, 'download returned no data'));
      continue;
    }
    if (maxBytes > 0 && buffer.length > maxBytes) {
      lines.push(failureLine(n, `[${item.kind}]`, `file too large (${buffer.length} bytes > ${maxBytes} cap)`));
      continue;
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

    try {
      // 0o700 dirs / 0o600 files: the store holds private DM content.
      fs.mkdirSync(convDir, { recursive: true, mode: 0o700 });
      fs.writeFileSync(dest, buffer, { mode: 0o600 });
    } catch (err) {
      log(`inbound ${msg.msgid}: ${label} write failed: ${scrubErrorMessage(err.message)}`);
      lines.push(failureLine(n, name, `could not save to disk (${scrubErrorMessage(err.message)})`));
      continue;
    }

    const mime = mimeTypeForFilename(name);
    attachments.push({
      provider_id: item.index === null ? (msg.msgid ?? '') : `${msg.msgid ?? ''}/${item.index}`,
      url: `file://${dest}`,
      ...(mime ? { mime_type: mime } : {}),
    });
    lines.push(`  ${n + 1}. ${neutralizeMarkupBoundaries(name)} (${mime || item.kind}) — saved to ${neutralizeMarkupBoundaries(dest)}; Read that path to view it`);
    log(`inbound ${msg.msgid}: ${label} saved (${buffer.length} bytes)`);

    // Audio FILES get an inline Scribe transcript; images/videos are read
    // directly by the receiving agent, no transcription. A transcription
    // failure is a note, never a dropped message.
    if (item.kind === 'file' && isAudioFilename(name) && transcribe) {
      try {
        const transcript = await transcribe(buffer, name);
        extras.push(`[audio transcript — ${neutralizeMarkupBoundaries(name)}]\n${neutralizeMarkupBoundaries(transcript)}`);
      } catch (err) {
        log(`inbound ${msg.msgid}: transcription failed: ${scrubErrorMessage(err.message)}`);
        extras.push(`[transcription failed: ${scrubErrorMessage(err.message)} — the audio file is saved at ${neutralizeMarkupBoundaries(dest)}]`);
      }
    }
  }

  const noun = items.length === 1 ? 'file' : 'files';
  const block = [`[${items.length} WeCom ${noun} attached]`, ...lines, ...extras].join('\n');
  return { attachments, block };
}

// failureLine renders one undeliverable attachment. The expiry note
// matters: unlike Slack (where the bytes stay fetchable via the API), a
// WeCom URL is dead ~5 minutes after receipt — there is no later recovery
// path, so the agent should ask the sender to re-send when the content is
// needed.
function failureLine(n, name, reason) {
  return `  ${n + 1}. ${neutralizeMarkupBoundaries(name)} — ${reason}; WeCom download URLs expire ~5 minutes after receipt, so the bytes are not recoverable — ask the sender to re-send if needed`;
}
