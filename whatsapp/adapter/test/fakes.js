// fakes.js — the fake gateway (the contract v1, route by route, with the
// gateway lane's posted readings: every time an integer of epoch SECONDS, the
// ack answered {ok: true}), the fake gc
// (extmsg inbound/receipts/transcript, sessions, session transcripts, the
// adapter registration and the outbound transcript, modelled on gc's fork
// head f22b3ed4a), a test clock, and the child-process harness the
// crash-recovery tests SIGKILL.

import { spawn } from 'node:child_process';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { mediaTypes } from '../src/outbound-file.js';

export const ADAPTER_DIR = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
export const CITY = 'test-city';
export const CREDENTIAL = 'cred-test-0123456789';
export const MARGIN_MS = 120000;
export const DAY_MS = 24 * 60 * 60 * 1000;

export const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// 20 s: the suite runs four files at once and a child under that load can be
// slow; a genuine failure only takes longer to report.
export async function waitFor(predicate, { timeoutMs = 20000, stepMs = 15, what = 'condition' } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const value = await predicate();
    if (value) return value;
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await sleep(stepMs);
  }
}

// Holds the result constant for a while: "nothing more happens".
export async function stays(predicate, { forMs = 400, stepMs = 20, what = 'condition' } = {}) {
  const deadline = Date.now() + forMs;
  while (Date.now() < deadline) {
    if (!(await predicate())) throw new Error(`${what} stopped holding`);
    await sleep(stepMs);
  }
}

export function tempDir(prefix = 'wa-') {
  // Short, under /tmp: macOS caps a Unix socket path at 104 bytes.
  return fs.mkdtempSync(path.join(os.platform() === 'darwin' ? '/tmp' : os.tmpdir(), prefix));
}

export function createTestClock(dir, startMs = Date.parse('2026-11-04T15:00:00Z')) {
  const file = path.join(dir, 'clock');
  let now = startMs;
  fs.writeFileSync(file, String(now));
  return {
    file,
    now: () => now,
    set(ms) { now = ms; fs.writeFileSync(file, String(now)); },
    advance(ms) { now += ms; fs.writeFileSync(file, String(now)); return now; },
  };
}

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(`http://127.0.0.1:${server.address().port}`)));
}

function readJSON(req) {
  return new Promise((resolve) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => {
      const text = Buffer.concat(chunks).toString('utf8');
      try { resolve(text ? JSON.parse(text) : {}); } catch { resolve({ __raw: text }); }
    });
  });
}

function send(res, status, body, headers = {}) {
  if (res.writableEnded || res.destroyed) return;
  if (Buffer.isBuffer(body)) {
    res.writeHead(status, headers).end(body);
    return;
  }
  res.writeHead(status, { 'Content-Type': 'application/json', ...headers }).end(JSON.stringify(body));
}

// A behaviour queue: each call takes the next entry; the last one repeats.
function takeBehaviour(list, fallback) {
  if (!list.length) return fallback;
  return list.length > 1 ? list.shift() : list[0];
}

// --- the fake gateway ----------------------------------------------------------

export async function startFakeGateway({ clock } = {}) {
  const now = () => (clock ? clock.now() : Date.now());
  const g = {
    credential: CREDENTIAL,
    revoked: false,
    nextId: 1,
    deliveries: new Map(), // id -> delivery (held until acked)
    acked: new Set(),
    ackCalls: [],
    ackBehaviour: [], // 'ok' | 500 | 'hang'
    sends: [], // every Graph call the gateway would make
    sendAnswers: new Map(), // idempotency_key -> answer
    uploads: new Map(), // outbound handle -> fake Meta bytes and metadata
    uploadAnswers: new Map(),
    uploadCalls: [],
    uploadFailures: [],
    sendFailOn: new Set(), // idempotency keys that answer 500 once
    bound: new Set(),
    // PR 76's additions (gp-ac0u, gp-0g2s at ba82ab46): an owner's user_id
    // on its GET /v1/window entry, which a send's `to` may name; and the
    // numbers that name two owners of the city (a recycled number), a send
    // to which answers 409 ambiguous_recipient, sends nothing and spends no key.
    userIds: new Map(), // wa_id -> the owner's user_id
    ambiguous: new Set(), // `to` values naming two owners
    // PR 76's window entries carry a user_id field (null for a PR 73 row);
    // PR 73's carry none. And PR 76's marks: the number an owner shows now,
    // by which a send's `to` also names it (gateway.ts send, gp-g02t).
    v2Owners: new Set(), // owners whose window entry carries user_id
    shows: new Map(), // number -> the conversation_id of the owner who shows it
    lastInbound: new Map(), // wa_id -> ms
    invites: [],
    enrollCodes: new Set(['enroll-ok']),
    media: new Map(),
    reads: [],
    windowCalls: 0,
    requests: [],
    hung: [],
    inboxDelayMs: 0, // a page computed now, answered later (the poll/ack race)
    mediaFailures: 0, // media downloads that answer 503 before one succeeds
    ackMaxIds: Infinity, // a batch over this answers 413
  };

  // A delivery in the gateway contract v1 (PR 73's inbox: wa_id is the
  // owner's key) unless conversation_id is given: then in PR 76's inbox shape
  // (gateway.ts inbox(): wa_id the number or null, user_id, username, and the
  // owner's stable conversation_id), and the fake keys the owner (bound, its
  // window, a send's `to`) by that conversation_id, as the gateway does.
  g.addDelivery = ({ kind = 'message', wa_id, profile_name = 'Owner', message, conversation_id, user_id = null, username = null }) => {
    const id = g.nextId++;
    const v2 = conversation_id !== undefined;
    const delivery = v2
      ? {
        id, received_at: Math.floor(now() / 1000), kind, wa_id: wa_id == null ? null : String(wa_id), user_id, username,
        conversation_id, profile_name: kind === 'status' ? null : profile_name, message,
      }
      : { id, received_at: Math.floor(now() / 1000), kind, wa_id: String(wa_id), profile_name: kind === 'status' ? null : profile_name, message };
    const owner = String(v2 ? conversation_id : wa_id);
    g.deliveries.set(id, delivery);
    g.bound.add(owner);
    if (v2) {
      g.v2Owners.add(owner);
      if (user_id) g.userIds.set(owner, user_id);
    }
    if (kind === 'message' || kind === 'joined') g.lastInbound.set(owner, Number(message?.timestamp) * 1000 || now());
    return id;
  };
  g.addText = (wa_id, wamid, body, extra = {}) => g.addDelivery({
    wa_id,
    message: { from: String(wa_id), id: wamid, timestamp: String(Math.floor(now() / 1000)), type: 'text', text: { body }, ...extra },
  });
  g.held = (id) => g.deliveries.has(id) && !g.acked.has(id);
  g.windowFor = (waId) => {
    const last = g.lastInbound.get(String(waId));
    const userId = g.userIds.get(String(waId));
    const ids = { conversation_id: String(waId), ...(userId ? { user_id: userId } : g.v2Owners.has(String(waId)) ? { user_id: null } : {}) };
    if (!last) return { ...ids, last_inbound_at: null, window_closes_at: null };
    return {
      ...ids,
      last_inbound_at: Math.floor(last / 1000),
      window_closes_at: Math.floor(last / 1000) + 86400 - 120,
    };
  };
  // Every bound owner a send's `to` names, as PR 76's send resolves it: its
  // conversation_id (PR 73: its wa_id), its user_id, or a number it shows.
  g.namedBy = (to) => {
    const named = new Set();
    if (g.bound.has(to)) named.add(to);
    for (const [owner, userId] of g.userIds) if (userId === to && g.bound.has(owner)) named.add(owner);
    const shown = g.shows.get(to);
    if (shown !== undefined && g.bound.has(shown)) named.add(shown);
    return [...named];
  };
  g.ownerOf = (to) => g.namedBy(to)[0];
  g.windowOpen = (waId) => {
    const last = g.lastInbound.get(String(waId));
    return Boolean(last) && now() < last + DAY_MS - MARGIN_MS;
  };

  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://gw');
    const route = `${req.method} ${url.pathname}`;
    const rawUpload = [];
    if (route === 'POST /v1/uploads') for await (const chunk of req) rawUpload.push(chunk);
    const body = route === 'POST /v1/uploads' ? undefined : req.method === 'POST' ? await readJSON(req) : undefined;
    g.requests.push({ route, at: now(), body });
    if (route === 'GET /healthz') return send(res, 200, { ok: true, pending_total: g.deliveries.size - g.acked.size, last_webhook_at: null });
    if (route === 'POST /v1/enroll') {
      if (!g.enrollCodes.has(body.code)) return send(res, 403, { error: 'enrollment code unknown or used' });
      g.enrollCodes.delete(body.code);
      return send(res, 200, { city_id: 'city-1', credential: 'cred-minted-at-enroll' });
    }
    const auth = req.headers.authorization ?? '';
    if (g.revoked || auth !== `Bearer ${g.credential}`) return send(res, 401, { error: 'unauthorized' });

    if (route === 'POST /v1/uploads') {
      const key = req.headers['idempotency-key'];
      if (g.uploadAnswers.has(key)) return send(res, 200, g.uploadAnswers.get(key));
      const rule = mediaTypes[req.headers['content-type']];
      const bytes = Buffer.concat(rawUpload);
      if (!rule) return send(res, 415, { error: 'unsupported_media_type' });
      if (bytes.length > rule.maxBytes) return send(res, 413, { error: 'media_too_large' });
      const failure = g.uploadFailures.shift();
      if (failure) return send(res, failure, { error: 'meta_error' });
      const answer = { media_id: `media.out.${g.uploads.size + 1}`, expires_at: Math.floor(now() / 1000) + 30 * 86400 };
      const upload = { ...answer, bytes, kind: rule.kind, mime: req.headers['content-type'], name: decodeURIComponent(req.headers['x-filename']), key };
      g.uploads.set(answer.media_id, upload);
      g.uploadCalls.push(upload);
      g.uploadAnswers.set(key, answer);
      return send(res, 200, answer);
    }

    if (route === 'POST /v1/invites') {
      const invite = { code: String(100000 + g.invites.length), expires_at: Math.floor((now() + DAY_MS) / 1000) };
      g.invites.push(invite);
      return send(res, 200, invite);
    }
    if (route === 'GET /v1/inbox') {
      const after = url.searchParams.has('after') ? Number(url.searchParams.get('after')) : -Infinity;
      const limit = Math.min(Math.max(Number(url.searchParams.get('limit') ?? 200), 1), 200);
      const page = [...g.deliveries.values()]
        .filter((d) => !g.acked.has(d.id) && d.id > after)
        .sort((a, b) => a.id - b.id)
        .slice(0, limit);
      if (g.inboxDelayMs) await new Promise((r) => setTimeout(r, g.inboxDelayMs));
      return send(res, 200, { deliveries: page, cursor: page.length ? page[page.length - 1].id : (Number.isFinite(after) ? after : null) });
    }
    if (route === 'POST /v1/inbox/ack') {
      const behaviour = takeBehaviour(g.ackBehaviour, 'ok');
      g.ackCalls.push({ ids: body.ids, behaviour, at: now() });
      if ((body.ids ?? []).length > g.ackMaxIds) return send(res, 413, { error: 'too many ids' });
      if (behaviour === 'hang') { g.hung.push(res); return undefined; }
      if (behaviour !== 'ok') return send(res, behaviour, { error: 'ack failed' });
      for (const id of body.ids ?? []) g.acked.add(id); // deletes exactly the listed ids
      return send(res, 200, { ok: true });
    }
    if (route === 'POST /v1/send') {
      const { idempotency_key: key, type } = body;
      const to = String(body.to);
      const named = g.namedBy(to);
      const owner = named[0];
      if (!owner) return send(res, 403, { error: 'not_bound' });
      if (g.sendAnswers.has(key)) return send(res, 200, g.sendAnswers.get(key));
      if (named.length > 1 || g.ambiguous.has(to)) {
        return send(res, 409, { error: 'ambiguous_recipient', hint: 'send to the owner\'s current id', owner_user_id: g.userIds.get(to) ?? null });
      }
      // A PR 76 owner with no user id (a PR 73 row) has no address.
      if (g.v2Owners.has(owner) && !g.userIds.get(owner)) return send(res, 403, { error: 'not_bound' });
      if (g.sendFailOn.has(key)) { g.sendFailOn.delete(key); return send(res, 500, { error: 'graph failed' }); }
      if (type !== 'template' && !g.windowOpen(owner)) return send(res, 422, { error: 'window_closed' });
      if (['image', 'audio', 'video', 'document'].includes(type)) {
        const uploaded = g.uploads.get(body.media?.id);
        if (!uploaded || uploaded.kind !== type || uploaded.expires_at <= now() / 1000) return send(res, 404, { error: 'media_not_found' });
      }
      const answer = { message_id: `wamid.out.${g.sends.length + 1}` };
      g.sends.push({ ...body, owner, at: now() });
      g.sendAnswers.set(key, answer);
      return send(res, 200, answer);
    }
    if (route === 'POST /v1/read') { g.reads.push(body.message_id); return send(res, 200, { ok: true }); }
    if (route === 'GET /v1/window') {
      g.windowCalls += 1;
      return send(res, 200, { windows: [...g.bound].map((waId) => g.windowFor(waId)) });
    }
    const media = url.pathname.match(/^\/v1\/media\/(.+)$/);
    if (req.method === 'GET' && media) {
      const item = g.media.get(decodeURIComponent(media[1]));
      if (!item) return send(res, 404, { error: 'not found' });
      if (g.mediaFailures > 0) { g.mediaFailures -= 1; return send(res, 503, { error: 'graph unavailable' }); }
      return send(res, 200, item.bytes, { 'Content-Type': item.mime });
    }
    return send(res, 404, { error: `no route ${route}` });
  });
  g.url = await listen(server);
  g.close = () => new Promise((resolve) => {
    for (const res of g.hung) res.destroy();
    server.closeAllConnections?.();
    server.close(() => resolve());
  });
  return g;
}

// The routes the fake gateway serves, for the contract-v1 diff (ACCEPTANCE:
// "the fake gateway pins every route of the contract v1").
export const FAKE_GATEWAY_ROUTES = [
  'GET /healthz',
  'POST /v1/enroll',
  'POST /v1/invites',
  'GET /v1/inbox',
  'POST /v1/inbox/ack',
  'POST /v1/send',
  'POST /v1/uploads',
  'POST /v1/read',
  'GET /v1/window',
  'GET /v1/media/<media_id>',
];

// --- the fake gc -------------------------------------------------------------------

export function pasteText({ conversationId, actor = 'Owner', kind = 'human', text, pmid, thread }) {
  // formatExtmsgNotifyReminder, handler_extmsg.go:656-685 at f22b3ed4a (long form).
  let out = `<system-reminder>\nNew message in shared conversation whatsapp/${conversationId}:\n\n- ${actor} (${kind}): ${text}\n\n`;
  out += `Message id: ${pmid}${thread ? ` (in thread ${thread})` : ''}\n\n`;
  out += 'Reply: write the reply to a file, then run: gc whatsapp publish --to ' + conversationId + ' --text-file <path>\n</system-reminder>';
  return out;
}

export async function startFakeGc({ clock, city = CITY } = {}) {
  const now = () => (clock ? clock.now() : Date.now());
  const f = {
    city,
    conversations: new Map(), // conversation_id -> {records: [], nextSequence, earliest}
    posts: [], // every POST /extmsg/inbound {pmid, text, at, answered}
    pastes: [], // every paste into a session {pmid, text, session}
    receipts: new Map(),
    fanout: new Map(), // pmid -> {receiptId, status} (the fork head's dedup memory)
    // Off by default: the fake models gc at 92a7f383f (and the fork head once
    // its receipt store has forgotten the first fan-out: a restart or the
    // hour); on, it models the fork head's live fan-out dedup (hq-703om).
    dedupFanout: false,
    nextReceipt: 1,
    policy: () => ({}), // (pmid, message, postIndex) -> {status, verdict, terminal, withhold, noMembers, paste}
    sessions: [{ id: 's-mayor', template: 'mayor', state: 'active', running: true }],
    sessionMode: new Map([['s-mayor', 'log']]), // log | pane | none | 500
    logs: new Map([['s-mayor', []]]),
    panes: new Map(),
    transcriptErrors: [], // statuses to answer GET /extmsg/transcript with, one per call
    registrations: [],
    outbound: [],
    requests: [],
    withheld: [],
  };

  const conv = (id) => {
    if (!f.conversations.has(id)) f.conversations.set(id, { records: [], nextSequence: 1, earliest: 1 });
    return f.conversations.get(id);
  };
  f.count = (pmid) => f.posts.filter((p) => p.pmid === pmid).length;
  f.postsFor = (pmid) => f.posts.filter((p) => p.pmid === pmid);
  f.records = (conversationId) => conv(conversationId).records;
  f.recordsFor = (pmid) => [...f.conversations.values()].flatMap((c) => c.records).filter((r) => r.ProviderMessageID === pmid);
  f.addLogTurn = (sessionId, role, text) => f.logs.get(sessionId).push({ role, text, timestamp: new Date(now()).toISOString() });
  f.logPaste = (sessionId, { conversationId, text, pmid, actor }) => f.addLogTurn(sessionId, 'user', pasteText({ conversationId, text, pmid, actor }));
  // Restart: receipts and the fan-out memory are in memory; the transcript is the beads store's.
  f.restart = () => { f.receipts.clear(); f.fanout.clear(); };
  f.backup = () => JSON.parse(JSON.stringify([...f.conversations]));
  // Restore the store to a backup: records after it dropped, next_sequence set
  // back, receipts emptied (gc restarts with the restore).
  f.restore = (backup) => {
    f.conversations = new Map(JSON.parse(JSON.stringify(backup)));
    f.restart();
  };
  f.conclude = (receiptId, delivery) => {
    const r = f.receipts.get(receiptId);
    r.state = 'concluded';
    r.delivery = { receipt_id: receiptId, ...delivery };
    r.concluded_at = new Date(now()).toISOString();
    const pmid = r.pmid;
    if (pmid) f.fanout.set(pmid, { receiptId, status: delivery.status });
  };
  f.releaseWithheld = () => { for (const w of f.withheld.splice(0)) w(); };
  f.inboundRequestsBefore = (pmid, n = 1) => {
    // The request log up to the n-th POST of pmid.
    let seen = 0;
    const out = [];
    for (const r of f.requests) {
      out.push(r);
      if (r.route === 'POST /extmsg/inbound' && r.pmid === pmid && ++seen === n) return out;
    }
    return out;
  };

  function newReceipt(pmid, delivery, state) {
    const id = `rcpt-${f.nextReceipt++}`;
    const receipt = { receipt_id: id, state, pmid, delivery: { receipt_id: id, ...delivery }, begun_at: new Date(now()).toISOString() };
    f.receipts.set(id, receipt);
    return receipt;
  }

  async function inbound(req, res, body) {
    const message = body.message ?? {};
    const pmid = message.provider_message_id;
    const conversationId = message.conversation?.conversation_id;
    const postIndex = f.count(pmid);
    const plan = f.policy(pmid, message, postIndex) ?? {};
    const post = { pmid, text: message.text, at: now(), message, answered: false };
    f.posts.push(post);
    // gc answers these before any fan-out (and before the transcript append
    // for a validation error).
    if (plan.status) return send(res, plan.status, { title: 'injected', status: plan.status });

    // 1. the durable transcript, deduplicated on provider_message_id
    //    (transcript_service.go:70-77): one record per id.
    const c = conv(conversationId);
    let record = c.records.find((r) => r.ProviderMessageID === pmid);
    const duplicate = Boolean(record);
    // An unrouted message is acknowledged and appended nowhere
    // (internal/extmsg/inbound.go:380-384 at f22b3ed4a).
    if (!record && !plan.noMembers) {
      record = {
        ID: `rec-${conversationId}-${c.nextSequence}`,
        Conversation: message.conversation,
        Sequence: c.nextSequence++,
        Kind: 'inbound',
        ProviderMessageID: pmid,
        Actor: message.actor,
        Text: message.text,
        CreatedAt: new Date(now()).toISOString(),
      };
      c.records.push(record);
    }

    // 2. the fan-out, claimed per message at the fork head (extmsgClaimInboundFanout).
    let receipt;
    const remembered = f.dedupFanout ? f.fanout.get(pmid) : undefined;
    if (remembered && remembered.status === 'delivered') {
      // The suppression arm: a fresh receipt concluded no_route, no paste.
      receipt = newReceipt(pmid, { status: 'no_route', delivered_bytes: 0, expected_bytes: 0 }, 'concluded');
    } else if (remembered && remembered.status === 'pending') {
      receipt = f.receipts.get(remembered.receiptId);
    } else if (plan.noMembers) {
      // The empty-membership arm: the same bytes as the suppression arm.
      receipt = newReceipt(pmid, { status: 'no_route', delivered_bytes: 0, expected_bytes: 0 }, 'concluded');
    } else {
      const sessionId = 's-mayor';
      if (plan.paste !== false) f.pastes.push({ pmid, text: message.text, session: sessionId, at: now() });
      if (plan.logPaste) f.logPaste(sessionId, { conversationId, text: message.text, pmid, actor: message.actor?.display_name });
      if (plan.panePaste) f.panes.set(sessionId, pasteText({ conversationId, text: message.text, pmid }));
      const verdict = plan.verdict ?? 'delivered';
      const bytes = Buffer.byteLength(message.text ?? '');
      const member = { session_id: sessionId, status: verdict, delivered_bytes: verdict === 'failed' ? 0 : bytes, expected_bytes: bytes };
      if (plan.terminal) member.terminal = true;
      const delivery = { status: verdict, delivered_bytes: member.delivered_bytes, expected_bytes: bytes, members: [member] };
      if (plan.terminal) delivery.terminal = true;
      receipt = newReceipt(pmid, delivery, verdict === 'pending' && !plan.concludedPending ? 'pending' : 'concluded');
      if (f.dedupFanout) f.fanout.set(pmid, { receiptId: receipt.receipt_id, status: verdict });
    }
    const answer = {
      Message: message,
      TranscriptEntry: record,
      Duplicate: duplicate,
      TargetAgentName: 'mayor',
      delivery: receipt.delivery,
    };
    if (plan.withhold) {
      // gc recorded and pasted, and the answer never reaches the adapter.
      await new Promise((resolve) => {
        f.withheld.push(resolve);
        req.on('close', resolve);
      });
      return undefined;
    }
    post.answered = true;
    // gc took it and the 2xx carries no usable body.
    if (plan.emptyBody) { res.writeHead(200).end(); return undefined; }
    post.answer = JSON.parse(JSON.stringify(answer));
    return send(res, 200, answer);
  }

  const server = http.createServer(async (req, res) => {
    const url = new URL(req.url, 'http://gc');
    const prefix = `/v0/city/${encodeURIComponent(f.city)}`;
    if (!url.pathname.startsWith(prefix)) return send(res, 404, { detail: 'city not found or not running' });
    const p = url.pathname.slice(prefix.length);
    const body = req.method === 'POST' ? await readJSON(req) : undefined;
    const entry = { route: `${req.method} ${p.replace(/\/session\/[^/]+\//, '/session/{id}/').replace(/receipts\/.+$/, 'receipts/{id}')}`, at: now(), query: Object.fromEntries(url.searchParams) };
    if (body?.message?.provider_message_id) entry.pmid = body.message.provider_message_id;
    f.requests.push(entry);

    if (req.method === 'POST' && p === '/extmsg/inbound') return inbound(req, res, body);
    if (req.method === 'POST' && p === '/extmsg/adapters') { f.registrations.push(body); return send(res, 200, { status: 'registered' }); }
    if (req.method === 'POST' && p === '/extmsg/outbound') {
      f.outbound.push(body);
      return send(res, 200, f.recordOutbounds ? { TranscriptEntry: { id: f.outbound.length } } : { ok: true });
    }
    const receipt = p.match(/^\/extmsg\/inbound\/receipts\/(.+)$/);
    if (req.method === 'GET' && receipt) {
      const id = decodeURIComponent(receipt[1]);
      const r = f.receipts.get(id);
      if (!r) return send(res, 200, { receipt_id: id, state: 'unknown' });
      return send(res, 200, { receipt_id: id, state: r.state, delivery: r.delivery, begun_at: r.begun_at, concluded_at: r.concluded_at });
    }
    if (req.method === 'GET' && p === '/extmsg/transcript') {
      if (f.transcriptErrors.length) return send(res, f.transcriptErrors.shift(), { title: 'injected' });
      const q = url.searchParams;
      const c = conv(q.get('conversation_id'));
      const after = Number(q.get('after_sequence') ?? 0);
      const limit = Math.min(Number(q.get('limit') ?? 100) || 100, 500);
      const desc = q.get('order') === 'desc';
      // after_sequence is EXCLUSIVE; the list starts at the cursor plus one or
      // at earliest_available_sequence (transcript_service.go:862-864, 888).
      const start = Math.max(after + 1, c.earliest);
      let items = c.records.filter((r) => r.Sequence >= start).sort((a, b) => a.Sequence - b.Sequence);
      if (desc) items.reverse();
      items = items.slice(0, limit);
      entry.returned = items.map((r) => r.ProviderMessageID);
      return send(res, 200, { items, total: items.length });
    }
    if (req.method === 'GET' && p === '/sessions') return send(res, 200, { items: f.sessions, total: f.sessions.length });
    const st = p.match(/^\/session\/([^/]+)\/transcript$/);
    if (req.method === 'GET' && st) {
      const id = decodeURIComponent(st[1]);
      const mode = f.sessionMode.get(id);
      if (!mode) return send(res, 404, { title: 'session not found' });
      if (typeof mode === 'number') return send(res, mode, { title: 'injected' });
      if (mode === 'log') return send(res, 200, { id, template: 'mayor', provider: 'claude', format: 'conversation', turns: f.logs.get(id) ?? [] });
      if (mode === 'pane') {
        const pane = f.panes.get(id) ?? '';
        return send(res, 200, { id, template: 'mayor', provider: 'claude', format: 'text', turns: pane ? [{ role: 'output', text: pane }] : [] });
      }
      return send(res, 200, { id, template: 'mayor', provider: 'claude', format: 'conversation', turns: [] });
    }
    return send(res, 404, { title: `no route ${req.method} ${p}` });
  });
  f.url = await listen(server);
  const port = server.address().port;
  // gc down: the port refuses connections (a connect-phase failure), then up.
  f.down = () => new Promise((resolve) => { server.closeAllConnections?.(); server.close(() => resolve()); });
  f.up = () => new Promise((resolve) => server.listen(port, '127.0.0.1', () => resolve()));
  f.close = () => new Promise((resolve) => {
    if (!server.listening) { resolve(); return; }
    f.releaseWithheld();
    server.closeAllConnections?.();
    server.close(() => resolve());
  });
  return f;
}

// --- the adapter as a real child process -----------------------------------------

export function adapterEnv({ dir, clock, gw, gc, extra = {} }) {
  return {
    PATH: process.env.PATH,
    HOME: dir,
    GC_CITY_NAME: CITY,
    GC_API_BASE_URL: gc.url,
    GC_CITY_PATH: dir,
    WHATSAPP_GATEWAY_URL: gw.url,
    WHATSAPP_GATEWAY_CREDENTIAL: CREDENTIAL,
    WHATSAPP_TEST_CLOCK: clock.file,
    WHATSAPP_TICK_MS: '10',
    WHATSAPP_POLL_MS: '0',
    // 5 s, not production's 30: a withheld answer still times out quickly,
    // and a child on a loaded host still answers in time.
    WHATSAPP_GC_POST_TIMEOUT_MS: '5000',
    WHATSAPP_GATEWAY_TIMEOUT_MS: '600',
    WHATSAPP_WATERMARK_CHECK_MS: '60000',
    WHATSAPP_COMPACT_MS: '3600000',
    WHATSAPP_KEEPER_WINDOW_READ_MS: '60000',
    // These tests jump the clock by hours; a stamp at every jump is an
    // fsynced keeper.json rewrite on the child's event loop. The stamp's own
    // test (keeper.test.js (10b)) runs the production minute.
    WHATSAPP_KEEPER_WINDOW_STAMP_MS: '86400000',
    WHATSAPP_CITY_TZ: 'America/Chicago',
    GC_SERVICE_SOCKET: path.join(dir, 's.sock'),
    GC_SERVICE_URL_PREFIX: '/v0/city/test-city/svc/whatsapp',
    REGISTER_ON_START: 'true',
    ...extra,
  };
}

export function startAdapter(env) {
  const child = spawn(process.execPath, [path.join(ADAPTER_DIR, 'src', 'index.js')], {
    env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const out = [];
  child.stdout.on('data', (d) => out.push(d.toString()));
  child.stderr.on('data', (d) => out.push(d.toString()));
  const exited = new Promise((resolve) => child.on('exit', (code, signal) => resolve({ code, signal })));
  return {
    child,
    exited,
    log: () => out.join(''),
    async kill() {
      if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL');
      return exited;
    },
  };
}

// The adapter's own journal and dead letters, read the way a restart reads them.
export function journalLines(dir) {
  const file = path.join(dir, '.gc', 'whatsapp', 'inbound_journal.jsonl');
  if (!fs.existsSync(file)) return [];
  return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
}

export function deadLetterLines(dir, waId) {
  const d = path.join(dir, '.gc', 'whatsapp', 'inbound_dead_letter');
  if (!fs.existsSync(d)) return [];
  const files = waId ? [`${waId}.jsonl`] : fs.readdirSync(d);
  return files.flatMap((name) => {
    const file = path.join(d, name);
    if (!fs.existsSync(file)) return [];
    return fs.readFileSync(file, 'utf8').split('\n').filter(Boolean).map((l) => JSON.parse(l));
  });
}

// A request to the adapter's internal listener (the UDS gc proxies to).
export function adapterRequest(socketPath, method, pathname, body) {
  return new Promise((resolve, reject) => {
    const payload = body === undefined ? undefined : JSON.stringify(body);
    const req = http.request({ socketPath, method, path: pathname, headers: payload ? { 'Content-Type': 'application/json' } : {} }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const text = Buffer.concat(chunks).toString('utf8');
        let json;
        try { json = JSON.parse(text); } catch { json = text; }
        resolve({ status: res.statusCode, json });
      });
    });
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
}

// One whole rig: clock, fakes, a temp city dir, and an adapter starter.
export async function rig({ extra = {}, startMs } = {}) {
  const dir = tempDir();
  const clock = createTestClock(dir, startMs);
  const gw = await startFakeGateway({ clock });
  const gc = await startFakeGc({ clock });
  const env = adapterEnv({ dir, clock, gw, gc, extra });
  let current;
  const r = {
    dir,
    clock,
    gw,
    gc,
    env,
    socket: env.GC_SERVICE_SOCKET,
    start(more = {}) {
      current = startAdapter({ ...env, ...more });
      r.adapter = current;
      return current;
    },
    async restart(more = {}) {
      await current?.kill();
      return r.start(more);
    },
    journal: () => journalLines(dir),
    deadLetters: (waId) => deadLetterLines(dir, waId),
    // Advance the test clock in steps, giving the child real time to act.
    async pump(totalMs, { stepMs = 1000, realMs = 25 } = {}) {
      let left = totalMs;
      while (left > 0) {
        const step = Math.min(stepMs, left);
        clock.advance(step);
        left -= step;
        await sleep(realMs);
      }
    },
    // Advance the clock a step at a time until the condition holds.
    async pumpUntil(predicate, { stepMs = 1000, realMs = 30, timeoutMs = 15000, what = 'condition' } = {}) {
      const deadline = Date.now() + timeoutMs;
      for (;;) {
        if (await predicate()) return;
        if (Date.now() > deadline) throw new Error(`timed out pumping for ${what}`);
        clock.advance(stepMs);
        await sleep(realMs);
      }
    },
    async close() {
      await current?.kill();
      await gw.close();
      await gc.close();
      fs.rmSync(dir, { recursive: true, force: true });
    },
  };
  return r;
}

export const rid = (pmid) => `whatsapp:dynasty:${pmid}`;
