// outbound.js — the mayor's replies out to WhatsApp: POST /publish (from gc,
// and from `gc whatsapp publish --to <wa_id> --text` through gc's
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
// textChunkKey(key, i, chunk) = `text:<sha256 of [key, chunk]>:<i>`, an
// update under templateKey(key, text) = `template:<sha256 of [key, text]>`
// (disjoint namespaces, one message per key), and the gateway answers a
// repeated key with the same message_id without a second Meta call. The
// adapter also persists per-key
// progress (<stateDir>/outbound_progress.json: chunks delivered, their
// message ids, the settled receipt), so a retry under the same key resumes
// at the failed chunk and never asks for a delivered one again, and a
// settled key answers its receipt without a gateway call, across restarts.
// A lost progress write is therefore safe: the retry re-asks the gateway
// under the same chunk key and gets the same answer.
//
// THE CLI'S TRANSCRIPT. gc records the outbound transcript for its own
// publishes (HandleOutbound, step 6). A CLI publish reaches this adapter
// directly, so the adapter records it the way the WeCom adapter records
// media: after delivery it POSTs gc's /extmsg/outbound on the same
// conversation as the inbound envelope (conversationRef: account_id
// dynasty, conversation_id the wa_id) with the SAME idempotency key; gc
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

// THE GATEWAY'S IDEMPOTENCY KEY (the codex gate r1's third finding on PR 74).
// The gateway remembers one answer per key per city for 30 days and answers
// a repeat with it, with no Meta call. So a gateway key names exactly one
// message: the operation in its own namespace (a text chunk and a template
// never share a key: an update keyed "K:0" and a text keyed "K" once both
// went to the gateway as "K:0"), then the sha256 of the caller's key WITH
// the content it carries (a caller key reused for another text, once the
// progress store below has forgotten it after 7 days or 1024 keys, is a new
// gateway key, never the first message's receipt), then a text chunk's
// index. Any caller key fits the gateway's 1-200 character bound
// (gateway.ts:746 at 392f9c09). One caller key, one text and one chunk
// always give one gateway key: a retry still gets the gateway's first
// answer (its REPEATED KEY rule).
export const textChunkKey = (key, index, chunk) => `text:${sha256Hex(JSON.stringify([key, chunk]))}:${index}`;
export const templateKey = (key, text) => `template:${sha256Hex(JSON.stringify([key, text]))}`;

const str = (v) => (typeof v === 'string' ? v.trim() : '');
const isObject = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);
const clone = (v) => JSON.parse(JSON.stringify(v));

// The per-key progress store: key -> {endpoint, wa_id, text_sha256,
// transcript_sha256?, chunks, delivered, message_ids, receipt?, recording?,
// updated_at}. Bounded by age and count; rewritten atomically on change.
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
      if (now - (Number(e.updated_at) || 0) > ttlMs) entries.delete(key);
    }
    if (entries.size > cap) {
      const oldest = [...entries.entries()].sort((a, b) => (a[1].updated_at || 0) - (b[1].updated_at || 0));
      for (const [key] of oldest.slice(0, entries.size - cap)) entries.delete(key);
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
      } catch (err) {
        // Safe to carry on: a retry re-asks the gateway under the same
        // chunk key and gets the same answer without a second Meta call.
        log(`outbound progress write failed (${err.code ?? err.message}); kept in memory`);
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
}) {
  const progress = createProgressStore({ file: path.join(stateDir, PROGRESS_FILE), clock, log });
  const updatesFile = path.join(stateDir, UPDATES_FILE);
  const inflight = new Map(); // key -> { promise, waId, textSha, endpoint }

  // Per-wa_id serialization: two publishes to one phone never interleave
  // their chunks; different phones stay concurrent.
  const chains = new Map();
  function chain(waId, fn) {
    const prev = chains.get(waId) ?? Promise.resolve();
    const next = prev.catch(() => {}).then(fn);
    chains.set(waId, next);
    next.catch(() => {}).then(() => {
      if (chains.get(waId) === next) chains.delete(waId);
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
        error: 'this idempotency key was already used for a different message (another phone, text or verb); use a fresh key',
        metadata: { reason: 'idempotency_conflict' },
      },
    };
  }

  // failure maps a gateway error to gc's receipt, answered 200.
  function failure(err, { waId, conversation, key, chunks, delivered }) {
    let kind = 'permanent';
    let reason = 'gateway_refused';
    let hint = '';
    if (err?.status === 422 && err.json?.error === 'window_closed') {
      reason = 'window_closed';
      hint = 'The 24-hour WhatsApp window for this conversation is closed: Meta refuses a free-form message until the owner writes again. '
        + `An actual update goes as the approved template: gc whatsapp publish --update --to ${waId} --text "<the update>". `
        + 'Otherwise wait for the owner\'s next message; this reply can then be retried with the same idempotency key.';
    } else if (err?.status === 403) {
      reason = 'not_bound';
      hint = 'This phone is not bound to this city at the gateway; the owner joins by texting a code from gc whatsapp invite to the Dynasty number.';
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
    } else if (isTransient(err) || err?.malformed || err?.noMessageId) {
      kind = 'transient';
      reason = 'gateway_unavailable';
      hint = 'Retry with the same idempotency key: delivered chunks are never sent again.';
    }
    log(`send to …${lastFour(waId)} failed at chunk ${delivered + 1}/${chunks}: ${reason} (${err?.status ?? 'no answer'})`);
    const metadata = { reason, chunks: String(chunks), chunks_delivered: String(delivered) };
    if (hint) metadata.hint = hint;
    if (Number.isInteger(err?.status)) metadata.gateway_status = String(err.status);
    return {
      status: 200,
      json: { conversation, delivered: false, failure_kind: kind, idempotency_key: key, error: hint || reason, metadata },
    };
  }

  // Runs one key's work once: a concurrent call with the same key awaits
  // the same answer; one with the same key and another message conflicts.
  async function once(key, fingerprint, conversation, work) {
    const running = inflight.get(key);
    if (running) {
      if (running.waId !== fingerprint.waId || running.textSha !== fingerprint.textSha || running.endpoint !== fingerprint.endpoint) {
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

  async function sendChunks({ key, waId, text, textSha, conversation }) {
    const chunks = chunkText(text);
    let entry = progress.get(key);
    if (entry?.receipt) return { status: 200, json: entry.receipt };
    if (!entry) {
      entry = { endpoint: 'publish', wa_id: waId, text_sha256: textSha, chunks: chunks.length, delivered: 0, message_ids: [] };
    }
    for (let i = entry.delivered; i < chunks.length; i++) {
      let answer;
      try {
        answer = await gateway.send({ to: waId, idempotency_key: textChunkKey(key, i, chunks[i]), type: 'text', text: chunks[i] });
        if (!str(answer?.message_id)) throw Object.assign(new Error('the gateway answered without a message id'), { noMessageId: true });
      } catch (err) {
        if (entry.delivered > 0) progress.save(key, entry);
        if (err?.status === 422 && err.json?.error === 'window_closed') {
          ledger?.noteWindowClosed?.(waId, clock());
          try { onWindowClosed(waId); } catch (hookErr) { log(`onWindowClosed hook failed: ${hookErr?.message ?? hookErr}`); }
        }
        return failure(err, { waId, conversation, key, chunks: chunks.length, delivered: entry.delivered });
      }
      const messageId = str(answer.message_id);
      entry.message_ids[i] = messageId;
      entry.delivered = i + 1;
      ledger?.noteSend?.(waId, { type: 'text', message_id: messageId, at: clock() });
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
    log(`publish to …${lastFour(waId)} delivered (${chunks.length} chunk${chunks.length === 1 ? '' : 's'})`);
    return { status: 200, json: entry.receipt };
  }

  // recordTranscript posts a delivered CLI publish to gc's /extmsg/outbound
  // under the same key. 'attempting' is written before the POST so a
  // concurrent or post-crash retry never posts twice; only a DEFINITE miss
  // (gc answered, nothing appended) may be re-attempted by a later retry.
  async function recordTranscript({ key, entry, sessionId, waId, text }) {
    entry.recording = 'attempting';
    progress.save(key, entry);
    try {
      const result = await gc.recordOutbound({
        session_id: sessionId,
        conversation: conversationRef(city, waId),
        text,
        idempotency_key: key,
      });
      entry.recording = (result?.TranscriptEntry ?? result?.transcript_entry) ? 'recorded' : 'not_recorded';
      if (entry.recording !== 'recorded') log(`transcript for …${lastFour(waId)}: gc recorded no entry`);
    } catch (err) {
      entry.recording = Number.isInteger(err?.status) ? 'not_recorded' : 'unknown';
      log(`transcript for …${lastFour(waId)}: recording failed (${err?.status ?? 'no answer'}); the send stands`);
    }
    progress.save(key, entry);
    return entry.recording;
  }

  async function withTranscript(answer, { key, sessionId, waId, text }) {
    const entry = progress.get(key);
    if (answer.status !== 200 || !answer.json.delivered || !entry?.receipt) return answer;
    const recordable = entry.recording === undefined || entry.recording === 'not_recorded';
    const outcome = recordable ? await recordTranscript({ key, entry, sessionId, waId, text }) : entry.recording;
    const json = { ...answer.json, transcript_recorded: outcome === 'recorded' };
    if (outcome !== 'recorded') {
      json.transcript_note = outcome === 'not_recorded'
        ? 'delivered, but gc recorded no transcript entry (retry with the same key to record it)'
        : 'delivered; whether gc recorded the transcript is unknown, so it is not posted again';
    }
    return { status: 200, json };
  }

  async function handlePublish(raw) {
    try {
      let pub;
      try { pub = JSON.parse(raw); } catch { return bad('invalid JSON'); }
      if (!isObject(pub)) return bad('the body must be a JSON object');
      const metadata = isObject(pub.metadata) ? pub.metadata : {};
      const sessionId = str(pub.session_id) || str(metadata.source_session_id);
      if (!sessionId) return { status: 400, json: { error: ATTRIBUTION_ERROR } };
      const waId = str(pub.conversation?.conversation_id);
      if (!waId) return bad('conversation.conversation_id (the wa_id) is required');
      if (typeof pub.text !== 'string' || !pub.text.trim()) return bad('text is required');
      if (pub.idempotency_key !== undefined && (typeof pub.idempotency_key !== 'string' || !pub.idempotency_key)) {
        return bad('idempotency_key must be a non-empty string when given');
      }
      const key = pub.idempotency_key || `whatsapp-${crypto.randomUUID()}`;
      const conversation = { ...conversationRef(city, waId), ...pub.conversation, conversation_id: waId };
      const textSha = sha256Hex(pub.text);
      const cli = metadata.origin === 'cli';

      // A settled key answers its receipt: gc's retry, and gc's call-back
      // for a CLI publish (or an update) under the same key, never re-send.
      const entry = progress.get(key);
      if (entry) {
        const same = entry.wa_id === waId && (
          (entry.endpoint === 'publish' && entry.text_sha256 === textSha)
          || (entry.receipt && entry.transcript_sha256 === textSha)
        );
        if (!same) return conflict(conversation, key);
        if (entry.receipt) {
          const answer = { status: 200, json: clone(entry.receipt) };
          return cli ? withTranscript(answer, { key, sessionId, waId, text: pub.text }) : answer;
        }
      }

      const answer = await once(key, { waId, textSha, endpoint: 'publish' }, conversation,
        () => chain(waId, () => sendChunks({ key, waId, text: pub.text, textSha, conversation })));
      return cli ? withTranscript(answer, { key, sessionId, waId, text: pub.text }) : answer;
    } catch (err) {
      log(`publish failed inside the adapter: ${err?.message ?? err}`);
      return { status: 500, json: { delivered: false, failure_kind: 'transient', error: 'internal adapter error; retry with the same idempotency key' } };
    }
  }

  async function sendUpdate({ key, to, text, textSha, conversation }) {
    const existing = progress.get(key);
    if (existing?.receipt) return { status: 200, json: existing.receipt };
    let answer;
    try {
      answer = await gateway.send({
        to,
        idempotency_key: templateKey(key, text),
        type: 'template',
        template: { name: UPDATE_TEMPLATE, language: updateLanguage, params: [text] },
      });
      if (!str(answer?.message_id)) throw Object.assign(new Error('the gateway answered without a message id'), { noMessageId: true });
    } catch (err) {
      return failure(err, { waId: to, conversation, key, chunks: 1, delivered: 0 });
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
      wa_id: to,
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
      const to = str(body.to);
      if (!to) return bad('to (the owner\'s wa_id) is required');
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
        if (entry.endpoint !== 'update' || entry.wa_id !== to || entry.text_sha256 !== textSha) return conflict(conversation, key);
        if (entry.receipt) {
          const answer = { status: 200, json: clone(entry.receipt) };
          return sessionId ? withTranscript(answer, { key, sessionId, waId: to, text: updateTranscriptText(text) }) : answer;
        }
      }

      if (!inflight.has(key) && ledger?.windowOpen?.(to)) {
        const closes = ledger.windowClosesAt(to);
        return {
          status: 409,
          json: {
            conversation,
            delivered: false,
            failure_kind: 'permanent',
            error: `the 24-hour window for this phone is open until ${new Date(closes).toISOString()}: a free-form reply is cheaper than the template; send it with gc whatsapp publish --to ${to} --text "<the reply>"`,
            metadata: { reason: 'window_open', window_closes_at: new Date(closes).toISOString() },
          },
        };
      }

      const answer = await once(key, { waId: to, textSha, endpoint: 'update' }, conversation,
        () => chain(to, () => sendUpdate({ key, to, text, textSha, conversation })));
      return sessionId ? withTranscript(answer, { key, sessionId, waId: to, text: updateTranscriptText(text) }) : answer;
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
