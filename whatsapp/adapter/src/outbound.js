// outbound.js — the mayor's replies out to WhatsApp: POST /publish (from gc,
// and from `gc whatsapp publish --to <conversation_id> --text` through gc's
// /svc/whatsapp proxy) and POST /publish-update (from `gc whatsapp publish
// --update`), on the WeCom pack's outbound shape (chunking, per-key chunk
// progress, per-chat serialization, the CLI's transcript recording).
//
// THE WIRE. gc's extmsg HTTP adapter POSTs {session_id?, conversation,
// text, reply_to_message_id?, idempotency_key?, metadata?} and reads a 2xx
// body as {message_id?, conversation, delivered, failure_kind?, metadata?}
// (metadata is map[string]string; internal/extmsg/http_adapter.go at gc's
// fork head f22b3ed4a). Every outcome this adapter DECIDES is answered 200
// with that receipt, so gc reads the failure kind exactly; only a malformed
// request (400) and an idempotency conflict (409, which gc maps to
// permanent) are answered otherwise.
//
// THE WINDOW. The gateway is the authority: a text outside the owner's
// 24-hour window comes back 422 window_closed, answered here as the
// permanent failure with metadata.reason window_closed and a hint naming
// the one path that works outside it, `gc whatsapp publish --update`. The
// update is the dynasty_update template with the text as its one
// parameter; it is never refused on the window and never opens it.
// /publish-update refuses (409) when the box's own ledger says the window
// is open, a free reply being cheaper, and (400) on empty text. Every
// template send is appended to <stateDir>/updates.jsonl (0600, fsynced).
//
// IDEMPOTENCY. Chunk i of a publish goes to the gateway under
// textChunkKey(key, to, text, i) = `text:<sha256 of [key, to, the whole
// text]>:<i>`, an update under templateKey(key, to, template) =
// `template:<sha256 of [key, to, name, language, params]>` (disjoint
// namespaces, one message per key: the recipient and the whole message, the
// same fingerprint the progress store checks), and the gateway answers a
// repeated key with the same message_id without a second Meta call. The
// adapter also persists per-key
// progress (<stateDir>/outbound_progress.json: chunks delivered, their
// message ids, the settled receipt), so a retry under the same key resumes
// at the failed chunk and never asks for a delivered one again, and a
// settled key answers its receipt without a gateway call, across restarts.
// Text retries can reconstruct their chunk keys after a lost progress write.
// Files must persist their upload handle before sending. A missing file entry
// can still recover a completed send by its key, even after the window closes.
//
// THE RECIPIENT (gp-g02t; the mayor's (A), 12:05Z 11/6). A reply names its
// conversation, the key the inbound side files the owner under (render.js
// conversationOf). A conversation id that is a phone number names its owner
// alone only while that owner is bound and no other owner of the city shows
// the number, since PR 76's send resolves `to` by conversation_id, user_id or
// a shown number. So each publish and each update reads GET /v1/window once
// and sends to the user id of the entry whose conversation_id is the
// conversation (the design of record: every send goes to a user id):
//   - no such entry (the owner unbound, or not a conversation id at all):
//     not_bound, nothing sent;
//   - an entry whose user_id is null (an owner PR 73 bound by number alone):
//     not_bound, nothing sent;
//   - an entry with no user_id field (PR 73's gateway): the conversation id,
//     that contract's owner key.
// The gateway key stays bound to the conversation id. A number change that
// lands between the read and the send answers 403 not_bound; a retry under
// the same key reads the window again. An id the gateway still finds naming
// two owners (409 ambiguous_recipient, PR 76's rule R1) is the permanent
// gateway_refused with metadata.gateway_error ambiguous_recipient and the
// gateway's hint, never resent: see failure().
//
// THE CLI'S TRANSCRIPT. gc records the outbound transcript for its own
// publishes (HandleOutbound, step 6). A CLI publish reaches this adapter
// directly, so the adapter records it the way the WeCom adapter records
// media: after delivery it POSTs gc's /extmsg/outbound on the same
// conversation as the inbound envelope (conversationRef: account_id
// dynasty, conversation_id the owner's) with the SAME idempotency key; gc
// calls this adapter's /publish back with that key, which answers the
// settled receipt without re-sending, and then appends the transcript
// entry. A CLI publish is marked metadata.origin = "cli" (gc's own wire
// never carries it: its /extmsg/outbound input has no metadata). A
// recording failure is logged and reported (transcript_recorded false),
// never a failed send.
//
// Never logged: message text, a phone number in clear (last four digits
// only), the credential.

import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';

import { conversationRef } from './gc.js';
import { isTransient } from './http.js';
import { lastFour, writeFileAtomic } from './ledger.js';
import { killPoint } from './clock.js';
import { outboundRoots, prepareFile, readCaption, fileTranscript } from './outbound-file.js';

export const outboundChunkChars = 4096;
export const PROGRESS_FILE = 'outbound_progress.json';
export const UPDATES_FILE = 'updates.jsonl';
export const UPDATE_TEMPLATE = 'dynasty_update';
export const ATTRIBUTION_ERROR = 'publish requires session attribution: provide session_id or metadata.source_session_id';

const DAY_MS = 24 * 60 * 60 * 1000;

// chunkText splits text into WhatsApp-sized messages: at most 4096 UTF-16
// code units each (Meta's text body limit counts characters; a code unit
// count never undercounts it). It iterates code points, so a surrogate
// pair is never severed, and it breaks at the last newline in the window
// when that newline is past the window's midpoint, so a line is not cut
// mid-way for nothing.
export function chunkText(text) {
  const s = String(text ?? '');
  if (s.length <= outboundChunkChars) return [s];
  const chunks = [];
  let current = '';
  let lastNewline = -1; // index in current just past its last '\n'
  for (const ch of s) {
    if (current.length + ch.length > outboundChunkChars) {
      if (lastNewline > current.length / 2) {
        chunks.push(current.slice(0, lastNewline));
        current = current.slice(lastNewline);
      } else {
        chunks.push(current);
        current = '';
      }
      lastNewline = -1; // the carried tail holds no newline
    }
    current += ch;
    if (ch === '\n') lastNewline = current.length;
  }
  if (current.length > 0) chunks.push(current);
  return chunks;
}

const sha256Hex = (s) => crypto.createHash('sha256').update(String(s)).digest('hex');

// THE GATEWAY'S IDEMPOTENCY KEY (the codex gate r1's third finding and the
// Opus read r2's MAJOR 1 on PR 74). The gateway remembers one answer per key
// per city for 30 days and answers a repeat with it, with no Meta call and
// whoever the recipient. The progress store below refuses a caller key
// reused for another conversation, text or verb, but only while it remembers the
// key (7 days or 1024 keys). So a gateway key names exactly the message the
// store checks: the operation in its own namespace (a text chunk and a
// template never share a key: an update keyed "K:0" and a text keyed "K"
// once both went to the gateway as "K:0"), then the sha256 of a JSON array
// of the caller's key, the recipient and the WHOLE message (a text's whole
// text, never the chunk alone; a template's name, language and parameters
// in order), then a text chunk's index. A caller key the store has
// forgotten, reused for another owner or another message, is then a new
// gateway key, never the first message's receipt, and a text sharing an
// earlier text's first chunk is never answered with that chunk. A key is
// 73 characters for a template, 70 plus the index's digits for a text
// chunk, inside the gateway's 1-200 bound (gateway.ts:746 at 392f9c09). One
// caller key, recipient and message always give the same gateway keys: a
// retry still gets the gateway's first answer (its REPEATED KEY rule).
export const textChunkKey = (key, to, text, index) => `text:${sha256Hex(JSON.stringify([key, to, text]))}:${index}`;
export const templateKey = (key, to, { name, language, params }) => `template:${sha256Hex(JSON.stringify([key, to, name, language, params]))}`;

const str = (v) => (typeof v === 'string' ? v.trim() : '');
const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const clone = (v) => JSON.parse(JSON.stringify(v));

// The per-key progress store: key -> {endpoint, conversation_id, text_sha256,
// transcript_sha256?, chunks, delivered, message_ids, receipt?, recording?,
// updated_at}. Text entries are bounded by age and count. File recovery records
// stay for the gateway's 30-day idempotency horizon and are never count-evicted:
// keeping the handle lets a pending send resume without another upload.
// Rewritten atomically on change; no media bytes are stored here.
// An entry written before gp-g02t names its conversation under wa_id, that
// field's old name; the value was always the conversation id gc (or the
// operator's --to) named, and entryConversation reads either.
const entryConversation = (entry) => (Object.hasOwn(entry, 'conversation_id') ? entry.conversation_id : entry.wa_id);
function createProgressStore({ file, clock, log, cap = 1024, ttlMs = 7 * DAY_MS }) {
  const entries = new Map();
  try {
    const raw = JSON.parse(fs.readFileSync(file, 'utf8'));
    for (const [key, e] of Object.entries(raw?.entries ?? {})) {
      if (isObject(e)) entries.set(key, e);
    }
  } catch (err) {
    if (err.code !== 'ENOENT') {
      try { fs.renameSync(file, `${file}.corrupt-${Date.now()}`); } catch { /* leave it */ }
      log(`outbound progress unreadable (${err.code ?? 'parse error'}); set aside, starting empty`);
    }
  }

  const prune = () => {
    const now = clock();
    for (const [key, e] of entries) {
      const retention = e.endpoint === 'file' ? 30 * DAY_MS : ttlMs;
      if (now - (Number(e.updated_at) || 0) > retention) entries.delete(key);
    }
    const counted = [...entries.entries()].filter(([, e]) => e.endpoint !== 'file');
    if (counted.length > cap) {
      const oldest = counted.sort((a, b) => (a[1].updated_at || 0) - (b[1].updated_at || 0));
      for (const [key] of oldest.slice(0, counted.length - cap)) entries.delete(key);
    }
  };

  return {
    get: (key) => entries.get(key),
    save(key, entry) {
      entry.updated_at = clock();
      entries.set(key, entry);
      prune();
      try {
        writeFileAtomic(file, `${JSON.stringify({ version: 1, entries: Object.fromEntries(entries) })}\n`);
        return true;
      } catch (err) {
        // Text can reconstruct its chunk key; file callers must check this
        // result before uploading or sending. Receipt writes can be retried
        // through the gateway's stored answer under the same send key.
        log(`outbound progress write failed (${err.code ?? err.message}); kept in memory`);
        return false;
      }
    },
  };
}

function appendLine(file, line) {
  fs.mkdirSync(path.dirname(file), { recursive: true, mode: 0o700 });
  const fd = fs.openSync(file, 'a', 0o600);
  try {
    try { fs.fchmodSync(fd, 0o600); } catch { /* the create mode already holds */ }
    fs.writeSync(fd, `${JSON.stringify(line)}\n`);
    fs.fsyncSync(fd);
  } finally {
    fs.closeSync(fd);
  }
}

export function createPublisher({
  stateDir,
  gateway,
  gc,
  city,
  clock = () => Date.now(),
  log = () => {},
  ledger,
  onWindowClosed = () => {},
  updateLanguage = 'en_US',
  cityDir,
  mediaDir,
  outboundRootConfig,
  mediaGateway = gateway,
  stopwatch,
}) {
  const progress = createProgressStore({ file: path.join(stateDir, PROGRESS_FILE), clock, log });
  const updatesFile = path.join(stateDir, UPDATES_FILE);
  const inflight = new Map(); // key -> { promise, conversationId, textSha, endpoint }

  // Per-conversation serialization: two publishes to one owner never
  // interleave their chunks; different owners stay concurrent.
  const chains = new Map();
  function chain(conversationId, fn) {
    const prev = chains.get(conversationId) ?? Promise.resolve();
    const next = prev.catch(() => {}).then(fn);
    chains.set(conversationId, next);
    next.catch(() => {}).then(() => {
      if (chains.get(conversationId) === next) chains.delete(conversationId);
    });
    return next;
  }

  const bad = (error) => ({ status: 400, json: { error } });

  function conflict(conversation, key) {
    return {
      status: 409,
      json: {
        conversation,
        delivered: false,
        failure_kind: 'permanent',
        idempotency_key: key,
        error: 'this idempotency key was already used for a different message (another conversation, text or verb); use a fresh key',
        metadata: { reason: 'idempotency_conflict' },
      },
    };
  }

  // failure maps a gateway error to gc's receipt, answered 200.
  function failure(err, { conversationId, conversation, key, chunks, delivered }) {
    let kind = 'permanent';
    let reason = 'gateway_refused';
    let hint = '';
    let gatewayError = '';
    if (err?.status === 422 && err.json?.error === 'window_closed') {
      reason = 'window_closed';
      hint = 'The 24-hour WhatsApp window for this conversation is closed: Meta refuses a free-form message until the owner writes again. '
        + `An actual update goes as the approved template: gc whatsapp publish --update --to ${conversationId} --text "<the update>". `
        + 'Otherwise wait for the owner\'s next message; this reply can then be retried with the same idempotency key.';
    } else if (err?.status === 409 && err.json?.error === 'ambiguous_recipient') {
      // PR 76's rule R1 (gp-0g2s): `to` names two owners of this city and the
      // gateway sent nothing. Since gp-g02t a send names the owner's user id
      // from the window (THE RECIPIENT), which names one owner. A `to` is a
      // phone number only when the window read came from PR 73's gateway and
      // the send reached PR 76's, in the minutes of the deploy: this is the
      // guard for that. Refused, never resent (gateway_refused, the ambiguity
      // named).
      gatewayError = 'ambiguous_recipient';
      const gatewayHint = typeof err.json.hint === 'string' && err.json.hint ? ` (the gateway's hint: "${err.json.hint}")` : '';
      hint = `The gateway found two owners of this city answering to this conversation's id (a phone number another owner of this city now shows: a recycled number) and sent nothing${gatewayHint}. `
        + 'This adapter does not resend.';
    } else if (err?.status === 403) {
      reason = 'not_bound';
      if (err.recipient === 'not_listed') {
        hint = `The gateway lists no owner of this city under ${conversationId} (unbound, or not a conversation id): nothing was sent. `
          + 'Name the conversation id gc shows (whatsapp/<conversation_id>); an unbound owner joins again with a code from gc whatsapp invite.';
      } else if (err.recipient === 'no_user_id') {
        hint = 'This owner was bound by its phone number alone (PR 73) and has no WhatsApp user id, so nothing can be sent to it: '
          + 'unbind it and give it a new invite code (the gateway README, Deploy step 7).';
      } else {
        hint = 'This conversation is not bound to this city at the gateway; the owner joins by texting a code from gc whatsapp invite to the Dynasty number.';
      }
    } else if (err?.status === 401 || err?.auth) {
      kind = 'auth';
      reason = 'not_enrolled';
      hint = 'The gateway refused this city\'s credential: run gc whatsapp enroll.';
    } else if (err?.status === 429) {
      kind = 'rate_limited';
      reason = 'rate_limited';
      hint = 'Retry later with the same idempotency key.';
    } else if (err?.config) {
      reason = 'not_configured';
      hint = 'WHATSAPP_GATEWAY_URL is not set in the adapter\'s env.';
    } else if (err?.json?.error === 'outcome_unknown') {
      // The gateway's reading 4 (update 2): Meta may have sent this chunk and
      // the key is spent; a repeat under it never calls Meta again.
      reason = 'outcome_unknown';
      hint = `The gateway cannot tell whether WhatsApp sent part ${delivered + 1} of ${chunks}: check the chat before resending. `
        + 'A resend needs a fresh idempotency key (run gc whatsapp publish again without --idempotency-key), at the risk of the owner reading it twice.';
    } else if (err?.json?.error === 'in_flight') {
      kind = 'transient';
      reason = 'in_flight';
      hint = 'The gateway is still sending this key to WhatsApp: retry shortly with the same idempotency key.';
    } else if (err?.progressSave) {
      kind = 'transient';
      reason = 'outbound_progress_unavailable';
      hint = 'The file send is paused because its journal could not be saved. Restore state-directory writes, then retry with the same idempotency key.';
    } else if (isTransient(err) || err?.malformed || err?.noMessageId) {
      kind = 'transient';
      reason = 'gateway_unavailable';
      hint = 'Retry with the same idempotency key: delivered chunks are never sent again.';
    }
    log(`send to …${lastFour(conversationId)} failed at chunk ${delivered + 1}/${chunks}: ${reason} (${err?.status ?? 'no answer'})`);
    const metadata = { reason, chunks: String(chunks), chunks_delivered: String(delivered) };
    if (gatewayError) metadata.gateway_error = gatewayError;
    if (hint) metadata.hint = hint;
    if (err?.recipient) metadata.recipient = err.recipient; // refused here: the gateway answered nothing
    else if (Number.isInteger(err?.status)) metadata.gateway_status = String(err.status);
    return {
      status: 200,
      json: { conversation, delivered: false, failure_kind: kind, idempotency_key: key, error: hint || reason, metadata },
    };
  }

  // THE RECIPIENT AT SEND TIME (header). One GET /v1/window; the entry whose
  // conversation_id is the conversation names the owner.
  async function recipientFor(conversationId) {
    const answer = await gateway.window();
    const entries = Array.isArray(answer?.windows) ? answer.windows : [];
    const entry = entries.find((w) => isObject(w) && String(w.conversation_id) === conversationId);
    if (!entry) return { refused: 'not_listed' };
    if (!Object.hasOwn(entry, 'user_id')) return { to: conversationId, closes: entry.window_closes_at }; // PR 73's window: its owner key
    const userId = typeof entry.user_id === 'string' ? entry.user_id.trim() : '';
    return userId ? { to: userId, closes: entry.window_closes_at } : { refused: 'no_user_id' };
  }

  // Resolves the recipient, then sends to it; a failed read or a refusal is
  // gc's receipt (failure()), and nothing is sent.
  async function toOwner(context, send) {
    let resolved;
    try {
      resolved = await recipientFor(context.conversationId);
    } catch (err) {
      return failure(err, context);
    }
    if (resolved.refused) {
      return failure(Object.assign(new Error('no recipient'), { status: 403, recipient: resolved.refused }), context);
    }
    return send(resolved.to, resolved.closes);
  }

  // Runs one key's work once: a concurrent call with the same key awaits
  // the same answer; one with the same key and another message conflicts.
  async function once(key, fingerprint, conversation, work) {
    const running = inflight.get(key);
    if (running) {
      if (running.conversationId !== fingerprint.conversationId || running.textSha !== fingerprint.textSha || running.endpoint !== fingerprint.endpoint) {
        return conflict(conversation, key);
      }
      return clone(await running.promise);
    }
    const promise = work();
    inflight.set(key, { promise, ...fingerprint });
    try {
      return clone(await promise);
    } finally {
      if (inflight.get(key)?.promise === promise) inflight.delete(key);
    }
  }

  async function sendChunks({ key, conversationId, to, text, textSha, conversation }) {
    const chunks = chunkText(text);
    let entry = progress.get(key);
    if (entry?.receipt) return { status: 200, json: entry.receipt };
    if (!entry) {
      entry = { endpoint: 'publish', conversation_id: conversationId, text_sha256: textSha, chunks: chunks.length, delivered: 0, message_ids: [] };
    }
    for (let i = entry.delivered; i < chunks.length; i++) {
      let answer;
      const sendKey = textChunkKey(key, conversationId, text, i);
      const uncertain = (await stopwatch?.attempt(sendKey)) ?? false;
      try {
        answer = await gateway.send({ to, idempotency_key: sendKey, type: 'text', text: chunks[i] });
        if (!str(answer?.message_id)) throw Object.assign(new Error('the gateway answered without a message id'), { noMessageId: true });
        stopwatch?.words(conversationId, answer.message_id, uncertain);
      } catch (err) {
        if (entry.delivered > 0) progress.save(key, entry);
        if (err?.status === 422 && err.json?.error === 'window_closed') {
          ledger?.noteWindowClosed?.(conversationId, clock());
          try { onWindowClosed(conversationId); } catch (hookErr) { log(`onWindowClosed hook failed: ${hookErr?.message ?? hookErr}`); }
        }
        return failure(err, { conversationId, conversation, key, chunks: chunks.length, delivered: entry.delivered });
      }
      const messageId = str(answer.message_id);
      entry.message_ids[i] = messageId;
      entry.delivered = i + 1;
      ledger?.noteSend?.(conversationId, { type: 'text', message_id: messageId, at: clock() });
      if (entry.delivered < chunks.length) progress.save(key, entry);
    }
    entry.receipt = {
      message_id: entry.message_ids[chunks.length - 1],
      conversation,
      delivered: true,
      idempotency_key: key,
      metadata: { chunks: String(chunks.length), message_ids: entry.message_ids.join(',') },
    };
    progress.save(key, entry);
    log(`publish to …${lastFour(conversationId)} delivered (${chunks.length} chunk${chunks.length === 1 ? '' : 's'})`);
    return { status: 200, json: entry.receipt };
  }

  // recordTranscript posts a delivered CLI publish to gc's /extmsg/outbound
  // under the same key. 'attempting' is written before the POST so a
  // concurrent or post-crash retry never posts twice; only a DEFINITE miss
  // (gc answered, nothing appended) may be re-attempted by a later retry.
  async function recordTranscript({ key, entry, sessionId, conversationId, text }) {
    entry.recording = 'attempting';
    progress.save(key, entry);
    try {
      const result = await gc.recordOutbound({
        session_id: sessionId,
        conversation: conversationRef(city, conversationId),
        text,
        idempotency_key: key,
      });
      entry.recording = (result?.TranscriptEntry ?? result?.transcript_entry) ? 'recorded' : 'not_recorded';
      if (entry.recording !== 'recorded') log(`transcript for …${lastFour(conversationId)}: gc recorded no entry`);
    } catch (err) {
      entry.recording = Number.isInteger(err?.status) ? 'not_recorded' : 'unknown';
      log(`transcript for …${lastFour(conversationId)}: recording failed (${err?.status ?? 'no answer'}); the send stands`);
    }
    progress.save(key, entry);
    return entry.recording;
  }

  async function withTranscript(answer, { key, sessionId, conversationId, text }) {
    const entry = progress.get(key);
    if (answer.status !== 200 || !answer.json.delivered || !entry?.receipt) return answer;
    const recordable = entry.recording === undefined || entry.recording === 'not_recorded';
    const outcome = recordable ? await recordTranscript({ key, entry, sessionId, conversationId, text }) : entry.recording;
    const json = { ...answer.json, transcript_recorded: outcome === 'recorded' };
    if (outcome !== 'recorded') {
      json.transcript_note = outcome === 'not_recorded'
        ? 'delivered, but gc recorded no transcript entry (retry with the same key to record it)'
        : 'delivered; whether gc recorded the transcript is unknown, so it is not posted again';
    }
    return { status: 200, json };
  }

  async function handleFile(pub, { key, sessionId, conversationId, conversation, cli }) {
    let file, caption;
    try {
      if (pub.text !== undefined && pub.text !== '') return bad('use caption or caption_file with file, not text');
      if (pub.caption !== undefined && pub.caption_file !== undefined) return bad('use caption or caption_file, not both');
      if (pub.caption !== undefined && typeof pub.caption !== 'string') return bad('caption must be text');
      const roots = outboundRoots({ cityDir, mediaDir, configured: outboundRootConfig });
      file = prepareFile(pub.file, { roots });
      caption = pub.caption_file !== undefined ? readCaption(pub.caption_file, { roots }) : (pub.caption ?? '');
      if ([...caption].length > 1024) return bad('caption exceeds 1024 characters');
      if (file.kind === 'audio' && caption) return bad('WhatsApp audio does not support a caption');
    } catch (err) { return bad(err.code ? `file refused: ${err.code}` : err.message); }
    const descriptor = { name: file.name, size: file.size, sha256: file.sha256, mime: file.mime, kind: file.kind };
    const text = fileTranscript(descriptor, caption);
    const textSha = sha256Hex(JSON.stringify([conversationId, descriptor, caption]));
    const existing = progress.get(key);
    if (existing && (existing.endpoint !== 'file' || entryConversation(existing) !== conversationId || existing.text_sha256 !== textSha)) return conflict(conversation, key);
    if (existing?.receipt) {
      const answer = { status: 200, json: clone(existing.receipt) };
      return cli ? withTranscript(answer, { key, sessionId, conversationId, text }) : answer;
    }
    const context = { conversationId, conversation, key, chunks: 1, delivered: 0 };
    const answer = await once(key, { conversationId, textSha, endpoint: 'file' }, conversation,
      () => chain(conversationId, () => toOwner(context, async (to, closes) => {
        let entry = progress.get(key);
        if (!entry) {
          entry = { endpoint: 'file', conversation_id: conversationId, text_sha256: textSha,
            transcript_sha256: sha256Hex(text), file: descriptor, caption, chunks: 1, delivered: 0, message_ids: [] };
        }
        const operation = sha256Hex(JSON.stringify([key, conversationId, textSha]));
        let uploading = false;
        try {
          const recoverOnly = !entry.media_id && (!Number.isFinite(closes) || closes * 1000 <= clock());
          const saveBeforeSend = () => {
            if (!progress.save(key, entry)) throw Object.assign(new Error('file journal write failed'), { progressSave: true });
          };
          if (!entry.media_id && !recoverOnly) {
            saveBeforeSend();
            killPoint('before-media-upload', key);
            uploading = true;
            const uploaded = await mediaGateway.upload(file, `file-upload:${operation}`);
            uploading = false;
            killPoint('after-media-upload', key);
            if (!str(uploaded?.media_id) || !Number.isFinite(uploaded.expires_at)) throw Object.assign(new Error('invalid upload answer'), { malformed: true });
            entry.media_id = uploaded.media_id;
            entry.media_expires_at = uploaded.expires_at;
          }
          // A delayed resume starts the send's retention horizon now, not at
          // upload time. Save before sending so a lost reply cannot age it out.
          if (!recoverOnly) saveBeforeSend();
          killPoint('after-media-journal', key);
          // With no journal handle, probe the existing send key before refusing.
          // The gateway checks that key before the window and grant. This sentinel
          // is not a gateway UUID handle, so even a window reopening during the
          // request cannot cause a new Meta send. No upload is made for the probe.
          const uncertain = (await stopwatch?.attempt(`file-send:${operation}`)) ?? false;
          const sent = await gateway.send({ to, idempotency_key: `file-send:${operation}`, type: file.kind,
            media: { id: entry.media_id ?? `unuploaded:${operation}`, ...(caption ? { caption } : {}), ...(file.kind === 'document' ? { filename: file.name } : {}) } });
          killPoint('after-media-send', key);
          if (!str(sent?.message_id)) throw Object.assign(new Error('missing message id'), { noMessageId: true });
          stopwatch?.words(conversationId, sent.message_id, uncertain || recoverOnly);
          entry.delivered = 1;
          entry.message_ids = [sent.message_id];
          entry.receipt = { conversation, delivered: true, message_id: sent.message_id, idempotency_key: key,
            metadata: { chunks: '1', message_ids: sent.message_id, file_name: file.name, file_bytes: String(file.size), file_sha256: file.sha256 } };
          progress.save(key, entry);
          killPoint('after-media-receipt', key);
          ledger?.noteSend?.(conversationId, { type: file.kind, message_id: sent.message_id, at: clock() });
          return { status: 200, json: entry.receipt };
        } catch (err) {
          if (err?.status === 422 && err.json?.error === 'window_closed') ledger?.noteWindowClosed?.(conversationId, clock());
          const refused = failure(err, context);
          if (!entry.media_id && err?.json?.error === 'media_not_found') {
            refused.json.failure_kind = 'transient';
            refused.json.metadata.reason = 'window_changed';
            refused.json.metadata.hint = 'No completed send was found and the window changed during recovery. Retry with the same idempotency key.';
            refused.json.error = refused.json.metadata.hint;
          } else if (err?.json?.error === 'media_not_found' || err?.json?.error === 'media_expired') {
            refused.json.failure_kind = 'permanent';
            refused.json.metadata.reason = 'media_expired';
            refused.json.metadata.hint = 'The upload handle is unavailable or expired. The gateway found no completed send under this key. '
              + 'Check the chat, then start a new publish with a fresh key if the owner still needs the file; repeating this key cannot restore the handle.';
            refused.json.error = refused.json.metadata.hint;
          }
          if (uploading && err?.json?.error === 'outcome_unknown') {
            refused.json.metadata.hint = 'Meta may have kept the upload, but the gateway has no confirmed handle. Nothing was sent to the owner. '
              + 'This key will not upload again; an operator must decide whether to start a new publish with a fresh key.';
            refused.json.error = refused.json.metadata.hint;
          }
          if (refused.json.metadata.reason === 'window_closed') {
            refused.json.metadata.hint = 'Files can be sent only after the owner writes again. A file cannot use the update template. '
              + `Retry with the same --idempotency-key ${key} to recover any completed send without sending twice.`;
            refused.json.error = refused.json.metadata.hint;
          }
          return refused;
        }
      })));
    return cli ? withTranscript(answer, { key, sessionId, conversationId, text }) : answer;
  }

  async function handlePublish(raw) {
    try {
      let pub;
      try { pub = JSON.parse(raw); } catch { return bad('invalid JSON'); }
      if (!isObject(pub)) return bad('the body must be a JSON object');
      const metadata = isObject(pub.metadata) ? pub.metadata : {};
      const sessionId = str(pub.session_id) || str(metadata.source_session_id);
      if (!sessionId) return { status: 400, json: { error: ATTRIBUTION_ERROR } };
      const conversationId = str(pub.conversation?.conversation_id);
      if (!conversationId) return bad('conversation.conversation_id (the owner\'s conversation id) is required');
      if (pub.idempotency_key !== undefined && (typeof pub.idempotency_key !== 'string' || !pub.idempotency_key)) {
        return bad('idempotency_key must be a non-empty string when given');
      }
      const key = pub.idempotency_key || `whatsapp-${crypto.randomUUID()}`;
      const conversation = { ...conversationRef(city, conversationId), ...pub.conversation, conversation_id: conversationId };
      const cli = metadata.origin === 'cli';
      if (Object.hasOwn(pub, 'file')) return await handleFile(pub, { key, sessionId, conversationId, conversation, cli });
      if (typeof pub.text !== 'string' || !pub.text.trim()) return bad('text is required');
      if (pub.caption !== undefined || pub.caption_file !== undefined) return bad('a caption requires a file');
      const textSha = sha256Hex(pub.text);

      // A settled key answers its receipt: gc's retry, and gc's call-back
      // for a CLI publish (or an update) under the same key, never re-send.
      const entry = progress.get(key);
      if (entry) {
        const same = entryConversation(entry) === conversationId && (
          (entry.endpoint === 'publish' && entry.text_sha256 === textSha)
          || (entry.receipt && entry.transcript_sha256 === textSha)
        );
        if (!same) return conflict(conversation, key);
        if (entry.receipt) {
          const answer = { status: 200, json: clone(entry.receipt) };
          return cli ? withTranscript(answer, { key, sessionId, conversationId, text: pub.text }) : answer;
        }
      }

      const answer = await once(key, { conversationId, textSha, endpoint: 'publish' }, conversation,
        () => chain(conversationId, () => toOwner(
          { conversationId, conversation, key, chunks: chunkText(pub.text).length, delivered: progress.get(key)?.delivered ?? 0 },
          (to) => sendChunks({ key, conversationId, to, text: pub.text, textSha, conversation }),
        )));
      return cli ? withTranscript(answer, { key, sessionId, conversationId, text: pub.text }) : answer;
    } catch (err) {
      log(`publish failed inside the adapter: ${err?.message ?? err}`);
      return { status: 500, json: { delivered: false, failure_kind: 'transient', error: 'internal adapter error; retry with the same idempotency key' } };
    }
  }

  async function sendUpdate({ key, to, recipient, text, textSha, conversation }) {
    const existing = progress.get(key);
    if (existing?.receipt) return { status: 200, json: existing.receipt };
    const template = { name: UPDATE_TEMPLATE, language: updateLanguage, params: [text] };
    let answer;
    const sendKey = templateKey(key, to, template);
    const uncertain = (await stopwatch?.attempt(sendKey)) ?? false;
    try {
      answer = await gateway.send({ to: recipient, idempotency_key: sendKey, type: 'template', template });
      if (!str(answer?.message_id)) throw Object.assign(new Error('the gateway answered without a message id'), { noMessageId: true });
      stopwatch?.words(to, answer.message_id, uncertain);
    } catch (err) {
      return failure(err, { conversationId: to, conversation, key, chunks: 1, delivered: 0 });
    }
    const messageId = str(answer.message_id);
    const at = clock();
    let journaled = true;
    try {
      appendLine(updatesFile, {
        at: new Date(at).toISOString(),
        to,
        text,
        idempotency_key: key,
        message_id: messageId,
        template: UPDATE_TEMPLATE,
        language: updateLanguage,
      });
    } catch (err) {
      journaled = false;
      log(`update to …${lastFour(to)} sent but not journaled (${err.code ?? err.message})`);
    }
    // The send ledger only: a template never opens or extends the window.
    ledger?.noteSend?.(to, { type: 'template', message_id: messageId, at });
    const entry = {
      endpoint: 'update',
      conversation_id: to,
      text_sha256: textSha,
      transcript_sha256: sha256Hex(updateTranscriptText(text)),
      chunks: 1,
      delivered: 1,
      message_ids: [messageId],
      receipt: {
        message_id: messageId,
        conversation,
        delivered: true,
        idempotency_key: key,
        metadata: { template: UPDATE_TEMPLATE, language: updateLanguage, ...(journaled ? {} : { journaled: 'false' }) },
      },
    };
    progress.save(key, entry);
    log(`update to …${lastFour(to)} sent as the ${UPDATE_TEMPLATE} template`);
    return { status: 200, json: entry.receipt };
  }

  async function handlePublishUpdate(raw) {
    try {
      let body;
      try { body = JSON.parse(raw); } catch { return bad('invalid JSON'); }
      if (!isObject(body)) return bad('the body must be a JSON object');
      if (Object.hasOwn(body, 'file') || body.caption !== undefined || body.caption_file !== undefined) return bad('files cannot use the update template');
      const to = str(body.to);
      if (!to) return bad('to (the owner\'s conversation id) is required');
      if (typeof body.text !== 'string' || !body.text.trim()) {
        return bad('text is required: the update template carries one text parameter');
      }
      if (body.idempotency_key !== undefined && (typeof body.idempotency_key !== 'string' || !body.idempotency_key)) {
        return bad('idempotency_key must be a non-empty string when given');
      }
      const key = body.idempotency_key || `whatsapp-update-${crypto.randomUUID()}`;
      const text = body.text;
      const textSha = sha256Hex(text);
      const conversation = conversationRef(city, to);
      const sessionId = str(body.session_id);

      const entry = progress.get(key);
      if (entry) {
        if (entry.endpoint !== 'update' || entryConversation(entry) !== to || entry.text_sha256 !== textSha) return conflict(conversation, key);
        if (entry.receipt) {
          const answer = { status: 200, json: clone(entry.receipt) };
          return sessionId ? withTranscript(answer, { key, sessionId, conversationId: to, text: updateTranscriptText(text) }) : answer;
        }
      }

      // The recipient first (an unbound owner's conversation is not_bound,
      // whatever the box's ledger says of its window), then the window.
      const windowOpen = () => {
        const closes = ledger.windowClosesAt(to);
        return {
          status: 409,
          json: {
            conversation,
            delivered: false,
            failure_kind: 'permanent',
            error: `the 24-hour window for this conversation is open until ${new Date(closes).toISOString()}: a free-form reply is cheaper than the template; send it with gc whatsapp publish --to ${to} --text "<the reply>"`,
            metadata: { reason: 'window_open', window_closes_at: new Date(closes).toISOString() },
          },
        };
      };
      const answer = await once(key, { conversationId: to, textSha, endpoint: 'update' }, conversation,
        () => chain(to, () => toOwner(
          { conversationId: to, conversation, key, chunks: 1, delivered: 0 },
          (recipient) => (ledger?.windowOpen?.(to) ? windowOpen() : sendUpdate({ key, to, recipient, text, textSha, conversation })),
        )));
      return sessionId ? withTranscript(answer, { key, sessionId, conversationId: to, text: updateTranscriptText(text) }) : answer;
    } catch (err) {
      log(`publish-update failed inside the adapter: ${err?.message ?? err}`);
      return { status: 500, json: { delivered: false, failure_kind: 'transient', error: 'internal adapter error; retry with the same idempotency key' } };
    }
  }

  return { handlePublish, handlePublishUpdate };
}

// The outbound transcript line for an update: the template is Meta-side
// text around this one parameter, so the record says which it was.
export function updateTranscriptText(text) {
  return `[update sent as the ${UPDATE_TEMPLATE} template] ${text}`;
}
