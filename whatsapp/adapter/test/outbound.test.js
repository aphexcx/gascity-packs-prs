// outbound.test.js — the publish path (POST /publish, POST /publish-update),
// the chunker and the window ledger, against in-test fakes: a node:http
// gateway on 127.0.0.1:0 (POST /v1/send with idempotency memory, a window per
// phone, 422 window_closed, a fail-on-chunk-N switch) and a node:http gc
// (POST /v0/city/<city>/extmsg/outbound recording each call and, as gc does,
// calling the adapter's /publish back with the same idempotency key).
//
// Run from whatsapp/adapter: node --test test/outbound.test.js

import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { createClock } from '../src/clock.js';
import { createGatewayClient } from '../src/gateway.js';
import { conversationRef, createGcClient } from '../src/gc.js';
import { createLedger } from '../src/ledger.js';
import {
  chunkText, createPublisher, outboundChunkChars, templateKey, textChunkKey,
} from '../src/outbound.js';

const CITY = 'testcity';
const PHONE = '15551234567';
const OTHER = '15559876543';
const CREDENTIAL = 'test-credential';
const DAY = 24 * 60 * 60 * 1000;
const T0 = Date.parse('2026-11-04T09:00:00Z');

// --- fakes -------------------------------------------------------------------

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
}

function readBody(req) {
  return new Promise((resolve) => {
    const parts = [];
    req.on('data', (c) => parts.push(c));
    req.on('end', () => resolve(Buffer.concat(parts)));
  });
}

function sendJSON(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

// The fake gateway: the contract v1's POST /v1/send. Every request is kept in
// `requests`; a send that reaches "Meta" is kept in `metaCalls`. A repeated
// idempotency key answers the remembered answer without a Meta call. A text or
// reaction to a phone whose window is closed answers 422 window_closed; a
// template is never refused on the window and never opens it.
async function startFakeGateway(t, { bound = [PHONE, OTHER] } = {}) {
  const gw = {
    bound: new Set(bound),
    window: new Map(),
    answers: new Map(),
    requests: [],
    metaCalls: [],
    failChunk: null, // { index, status, times }
    delayMs: 0,
    seq: 0,
  };
  const server = http.createServer(async (req, res) => {
    const raw = await readBody(req);
    if (req.headers.authorization !== `Bearer ${CREDENTIAL}`) return sendJSON(res, 401, { error: 'unauthorized' });
    if (req.method !== 'POST' || req.url !== '/v1/send') return sendJSON(res, 404, { error: 'not_found' });
    const body = JSON.parse(raw.toString('utf8'));
    gw.requests.push(body);
    if (gw.delayMs) await new Promise((r) => setTimeout(r, gw.delayMs));
    const fc = gw.failChunk;
    if (fc && fc.times > 0 && String(body.idempotency_key).endsWith(`:${fc.index}`)) {
      fc.times -= 1;
      return sendJSON(res, fc.status, { error: 'meta_unavailable' });
    }
    if (!gw.bound.has(body.to)) return sendJSON(res, 403, { error: 'not_bound' });
    if (gw.answers.has(body.idempotency_key)) return sendJSON(res, 200, gw.answers.get(body.idempotency_key));
    if ((body.type === 'text' || body.type === 'reaction') && !gw.window.get(body.to)) {
      return sendJSON(res, 422, { error: 'window_closed' });
    }
    gw.seq += 1;
    const answer = { message_id: `wamid.fake${gw.seq}` };
    gw.answers.set(body.idempotency_key, answer);
    gw.metaCalls.push({ ...body, message_id: answer.message_id });
    return sendJSON(res, 200, answer);
  });
  const port = await listen(server);
  t.after(() => new Promise((r) => server.close(r)));
  gw.url = `http://127.0.0.1:${port}`;
  return gw;
}

// The fake gc: POST /v0/city/<city>/extmsg/outbound. As gc's HandleOutbound
// does, it calls the adapter's /publish back with the same session, ref,
// text and idempotency key, then appends the transcript entry only on a
// delivered receipt.
async function startFakeGc(t) {
  const gcFake = { outbound: [], callbacks: [], publisher: null, failStatus: 0, seq: 0 };
  const server = http.createServer(async (req, res) => {
    const raw = await readBody(req);
    if (req.method !== 'POST' || req.url !== `/v0/city/${CITY}/extmsg/outbound`) return sendJSON(res, 404, { detail: 'not found' });
    const body = JSON.parse(raw.toString('utf8'));
    gcFake.outbound.push(body);
    if (gcFake.failStatus) return sendJSON(res, gcFake.failStatus, { detail: 'gc is down' });
    const answer = await gcFake.publisher.handlePublish(JSON.stringify({
      session_id: body.session_id,
      conversation: body.conversation,
      text: body.text,
      idempotency_key: body.idempotency_key,
    }));
    gcFake.callbacks.push(answer);
    const receipt = answer.json;
    if (answer.status !== 200 || !receipt.delivered) return sendJSON(res, 200, { Receipt: receipt });
    gcFake.seq += 1;
    return sendJSON(res, 200, {
      Receipt: receipt,
      TranscriptEntry: { Sequence: gcFake.seq, ProviderMessageID: receipt.message_id, Text: body.text, Kind: 'outbound' },
    });
  });
  const port = await listen(server);
  t.after(() => new Promise((r) => server.close(r)));
  gcFake.url = `http://127.0.0.1:${port}`;
  return gcFake;
}

async function setup(t, { bound, log } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wa-outbound-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const stateDir = path.join(dir, 'state');
  const clockFile = path.join(dir, 'clock');
  const setNow = (ms) => fs.writeFileSync(clockFile, String(ms));
  setNow(T0);
  const clock = createClock({ WHATSAPP_TEST_CLOCK: clockFile });
  const gw = await startFakeGateway(t, { bound });
  const gcFake = await startFakeGc(t);
  const gateway = createGatewayClient({ url: gw.url, credential: CREDENTIAL });
  const gc = createGcClient({ base: gcFake.url, city: CITY });
  const closed = [];
  const make = () => {
    const ledger = createLedger({ stateDir, clock, log });
    const pub = createPublisher({
      stateDir, gateway, gc, city: CITY, clock, ledger, log,
      onWindowClosed: (waId) => closed.push(waId),
    });
    gcFake.publisher = pub;
    return { ledger, pub };
  };
  const { ledger, pub } = make();
  return { dir, stateDir, clock, setNow, gw, gcFake, ledger, pub, closed, make };
}

function publishBody(text, { to = PHONE, key, session = 'mayor-session', metadata } = {}) {
  return JSON.stringify({
    ...(session ? { session_id: session } : {}),
    conversation: conversationRef(CITY, to),
    text,
    ...(key ? { idempotency_key: key } : {}),
    ...(metadata ? { metadata } : {}),
  });
}

const sendsFor = (gw, key) => gw.requests.filter((r) => r.idempotency_key === key);

// --- chunking --------------------------------------------------------------

test('chunking at 4096 keeps a surrogate pair whole', () => {
  assert.equal(outboundChunkChars, 4096);
  const text = `${'a'.repeat(4095)}😀${'b'.repeat(10)}`;
  const chunks = chunkText(text);
  assert.equal(chunks.length, 2);
  assert.equal(chunks[0], 'a'.repeat(4095));
  assert.equal(chunks[1], `😀${'b'.repeat(10)}`);

  const emoji = '😀'.repeat(3000); // 6000 UTF-16 units
  const parts = chunkText(emoji);
  assert.equal(parts.join(''), emoji);
  for (const p of parts) {
    assert.ok(p.length <= 4096, `chunk of ${p.length} units`);
    assert.ok(!/[\uD800-\uDBFF]$/.test(p), 'a chunk never ends on a high surrogate');
    assert.ok(!/^[\uDC00-\uDFFF]/.test(p), 'a chunk never starts on a low surrogate');
  }
  assert.equal(parts[0].length, 4096);
});

test('chunking at 4096 prefers a newline past the midpoint', () => {
  const late = `${'x'.repeat(3000)}\n${'y'.repeat(2000)}`;
  assert.deepEqual(chunkText(late), [`${'x'.repeat(3000)}\n`, 'y'.repeat(2000)]);

  // A newline before the midpoint is not worth a short chunk: hard split.
  const early = `${'x'.repeat(1000)}\n${'y'.repeat(4000)}`;
  const chunks = chunkText(early);
  assert.equal(chunks.length, 2);
  assert.equal(chunks[0].length, 4096);
  assert.equal(chunks.join(''), early);

  assert.deepEqual(chunkText('short'), ['short']);
  assert.deepEqual(chunkText('z'.repeat(4096)), ['z'.repeat(4096)]);
});

// --- /publish --------------------------------------------------------------

test('the same idempotency key yields one gateway send and the same receipt', async (t) => {
  const s = await setup(t);
  s.gw.window.set(PHONE, true);
  const first = await s.pub.handlePublish(publishBody('hello owner', { key: 'gc-k1' }));
  assert.equal(first.status, 200);
  assert.equal(first.json.delivered, true);
  assert.equal(first.json.message_id, 'wamid.fake1');
  assert.deepEqual(first.json.conversation, conversationRef(CITY, PHONE));
  assert.equal(sendsFor(s.gw, textChunkKey('gc-k1', 0, 'hello owner')).length, 1, 'chunk 0 goes out under text:<sha256 of [key, chunk]>:0');
  assert.match(textChunkKey('gc-k1', 0, 'hello owner'), /^text:[0-9a-f]{64}:0$/);

  const again = await s.pub.handlePublish(publishBody('hello owner', { key: 'gc-k1' }));
  assert.deepEqual(again, first);

  // Two concurrent retries share the one in-flight send.
  const both = await Promise.all([
    s.pub.handlePublish(publishBody('second message', { key: 'gc-k2' })),
    s.pub.handlePublish(publishBody('second message', { key: 'gc-k2' })),
  ]);
  assert.deepEqual(both[0], both[1]);

  // A restart on the same state dir answers from the persisted receipt.
  const { pub } = s.make();
  const afterRestart = await pub.handlePublish(publishBody('hello owner', { key: 'gc-k1' }));
  assert.deepEqual(afterRestart.json, first.json);

  assert.equal(s.gw.requests.length, 2, 'one gateway request per key, however many retries');
  assert.equal(s.gw.metaCalls.length, 2);

  // The same key for a different message is a conflict, never a silent receipt.
  const conflict = await pub.handlePublish(publishBody('something else', { key: 'gc-k1' }));
  assert.equal(conflict.status, 409);
  assert.equal(conflict.json.delivered, false);
  assert.equal(conflict.json.failure_kind, 'permanent');
  assert.equal(s.gw.requests.length, 2);
});

test('a mid-chunk failure resumes at the failed chunk', async (t) => {
  const s = await setup(t);
  s.gw.window.set(PHONE, true);
  const text = `${'a'.repeat(4096)}${'b'.repeat(4096)}${'c'.repeat(100)}`;
  s.gw.failChunk = { index: 1, status: 503, times: 1 };

  const failed = await s.pub.handlePublish(publishBody(text, { key: 'gc-mid' }));
  assert.equal(failed.status, 200);
  assert.equal(failed.json.delivered, false);
  assert.equal(failed.json.failure_kind, 'transient');
  assert.equal(failed.json.metadata.chunks_delivered, '1');
  assert.equal(failed.json.metadata.chunks, '3');
  assert.equal(s.gw.metaCalls.length, 1, 'chunk 0 reached Meta, chunk 1 did not');

  // gc retries with the same key after a restart: chunk 0 is never asked for
  // again; chunks 1 and 2 go out.
  const { pub } = s.make();
  const resumed = await pub.handlePublish(publishBody(text, { key: 'gc-mid' }));
  assert.equal(resumed.json.delivered, true);
  const parts = chunkText(text);
  assert.equal(sendsFor(s.gw, textChunkKey('gc-mid', 0, parts[0])).length, 1, 'the delivered chunk is never repeated');
  assert.equal(sendsFor(s.gw, textChunkKey('gc-mid', 1, parts[1])).length, 2, 'the failed chunk is retried once under the same key');
  assert.equal(sendsFor(s.gw, textChunkKey('gc-mid', 2, parts[2])).length, 1);
  assert.deepEqual(s.gw.metaCalls.map((c) => c.text[0]), ['a', 'b', 'c']);
  assert.equal(resumed.json.message_id, s.gw.metaCalls[2].message_id);
  assert.equal(resumed.json.metadata.message_ids, s.gw.metaCalls.map((c) => c.message_id).join(','));
});

test('window_closed maps to the permanent error with the hint naming the update verb', async (t) => {
  const s = await setup(t);
  const answer = await s.pub.handlePublish(publishBody('are you there?', { key: 'gc-closed' }));
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, false);
  assert.equal(answer.json.failure_kind, 'permanent');
  assert.equal(answer.json.metadata.reason, 'window_closed');
  assert.match(answer.json.metadata.hint, new RegExp(`gc whatsapp publish --update --to ${PHONE} --text`));
  assert.deepEqual(answer.json.conversation, conversationRef(CITY, PHONE));
  assert.deepEqual(s.closed, [PHONE], 'onWindowClosed is told the phone');
  assert.equal(s.gw.metaCalls.length, 0);
  for (const v of Object.values(answer.json.metadata)) assert.equal(typeof v, 'string', 'gc reads metadata as map[string]string');
});

test('403 not bound maps to permanent with reason not_bound; a 401 maps to auth', async (t) => {
  const s = await setup(t, { bound: [PHONE] });
  const answer = await s.pub.handlePublish(publishBody('hi', { to: OTHER, key: 'gc-nb' }));
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, false);
  assert.equal(answer.json.failure_kind, 'permanent');
  assert.equal(answer.json.metadata.reason, 'not_bound');

  const revoked = createPublisher({
    stateDir: s.stateDir,
    gateway: createGatewayClient({ url: s.gw.url, credential: 'revoked' }),
    gc: createGcClient({ base: s.gcFake.url, city: CITY }),
    city: CITY,
    clock: s.clock,
    ledger: s.ledger,
  });
  const auth = await revoked.handlePublish(publishBody('hi', { key: 'gc-401' }));
  assert.equal(auth.json.delivered, false);
  assert.equal(auth.json.failure_kind, 'auth');
});

test('publish without session attribution is refused', async (t) => {
  const s = await setup(t);
  s.gw.window.set(PHONE, true);
  const refused = await s.pub.handlePublish(publishBody('hi', { session: '', key: 'gc-anon' }));
  assert.equal(refused.status, 400);
  assert.deepEqual(refused.json, { error: 'publish requires session attribution: provide session_id or metadata.source_session_id' });
  assert.equal(s.gw.requests.length, 0);

  const legacy = await s.pub.handlePublish(publishBody('hi', {
    session: '', key: 'gc-legacy', metadata: { source_session_id: 'mayor-session' },
  }));
  assert.equal(legacy.status, 200);
  assert.equal(legacy.json.delivered, true);
});

test('sends to one wa_id are serialized: a second publish waits for every chunk of the first', async (t) => {
  const s = await setup(t);
  s.gw.window.set(PHONE, true);
  s.gw.window.set(OTHER, true);
  s.gw.delayMs = 25;
  const long = `${'a'.repeat(4096)}${'a'.repeat(4096)}aa`;
  await Promise.all([
    s.pub.handlePublish(publishBody(long, { key: 'gc-A' })),
    s.pub.handlePublish(publishBody('B', { key: 'gc-B' })),
    s.pub.handlePublish(publishBody('C', { to: OTHER, key: 'gc-C' })),
  ]);
  const forPhone = s.gw.requests.filter((r) => r.to === PHONE).map((r) => r.idempotency_key);
  const longParts = chunkText(long);
  assert.deepEqual(forPhone, [...longParts.map((part, i) => textChunkKey('gc-A', i, part)), textChunkKey('gc-B', 0, 'B')]);
  const cAt = s.gw.requests.findIndex((r) => r.idempotency_key === textChunkKey('gc-C', 0, 'C'));
  assert.ok(cAt < 3, 'another phone does not wait behind the first');
});

test('a CLI publish records the outbound transcript through gc with the same idempotency key; gc\'s call-back is answered without a re-send', async (t) => {
  const s = await setup(t);
  s.gw.window.set(PHONE, true);
  const answer = await s.pub.handlePublish(publishBody('from the CLI', {
    key: 'cli-k1', session: 'mayor-session', metadata: { origin: 'cli' },
  }));
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.transcript_recorded, true);
  assert.equal(s.gcFake.outbound.length, 1);
  const rec = s.gcFake.outbound[0];
  assert.equal(rec.idempotency_key, 'cli-k1');
  assert.equal(rec.session_id, 'mayor-session');
  assert.equal(rec.text, 'from the CLI');
  assert.deepEqual(rec.conversation, conversationRef(CITY, PHONE));
  assert.equal(s.gcFake.callbacks.length, 1);
  assert.equal(s.gcFake.callbacks[0].json.delivered, true);
  assert.equal(s.gcFake.callbacks[0].json.message_id, answer.json.message_id);
  assert.equal(s.gw.metaCalls.length, 1, 'the call-back never re-sends');
  assert.equal(s.gw.requests.length, 1);

  // gc's own publish (no CLI origin) is gc's to record: no second recording.
  await s.pub.handlePublish(publishBody('a bound reply', { key: 'gc-k9' }));
  assert.equal(s.gcFake.outbound.length, 1);

  // A failure to record is logged and never fails the send.
  s.gcFake.failStatus = 500;
  const unrecorded = await s.pub.handlePublish(publishBody('again from the CLI', {
    key: 'cli-k2', metadata: { origin: 'cli' },
  }));
  assert.equal(unrecorded.json.delivered, true);
  assert.equal(unrecorded.json.transcript_recorded, false);
});

// --- /publish-update -----------------------------------------------------

test('/publish-update refused when the window is open (409)', async (t) => {
  const s = await setup(t);
  s.ledger.noteInbound(PHONE, T0 - 60_000);
  const answer = await s.pub.handlePublishUpdate(JSON.stringify({ to: PHONE, text: 'daily update' }));
  assert.equal(answer.status, 409);
  assert.equal(answer.json.delivered, false);
  assert.match(answer.json.error, /window/);
  assert.equal(s.gw.requests.length, 0);
});

test('/publish-update refused when the text is empty (400)', async (t) => {
  const s = await setup(t);
  for (const text of ['', '   ', undefined]) {
    const answer = await s.pub.handlePublishUpdate(JSON.stringify({ to: PHONE, text }));
    assert.equal(answer.status, 400);
  }
  assert.equal(s.gw.requests.length, 0);
});

test('/publish-update journaled to updates.jsonl when sent', async (t) => {
  const s = await setup(t);
  const answer = await s.pub.handlePublishUpdate(JSON.stringify({
    to: PHONE, text: 'Sales up 4% today. Reply OK to confirm.', idempotency_key: 'upd-1',
  }));
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, true);
  assert.equal(s.gw.metaCalls.length, 1);
  const call = s.gw.metaCalls[0];
  assert.equal(call.type, 'template');
  assert.equal(call.to, PHONE);
  assert.equal(call.idempotency_key, templateKey('upd-1', 'Sales up 4% today. Reply OK to confirm.'), 'the update goes out under template:<sha256 of [key, text]>');
  assert.match(call.idempotency_key, /^template:[0-9a-f]{64}$/);
  assert.deepEqual(call.template, { name: 'dynasty_update', language: 'en_US', params: ['Sales up 4% today. Reply OK to confirm.'] });

  const file = path.join(s.stateDir, 'updates.jsonl');
  assert.equal(fs.statSync(file).mode & 0o777, 0o600);
  const lines = fs.readFileSync(file, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
  assert.equal(lines.length, 1);
  assert.equal(lines[0].to, PHONE);
  assert.equal(lines[0].text, 'Sales up 4% today. Reply OK to confirm.');
  assert.equal(lines[0].idempotency_key, 'upd-1');
  assert.equal(lines[0].message_id, call.message_id);
  assert.equal(lines[0].at, new Date(T0).toISOString());

  // The same key again: the same receipt, no second send, no second line.
  const again = await s.pub.handlePublishUpdate(JSON.stringify({
    to: PHONE, text: 'Sales up 4% today. Reply OK to confirm.', idempotency_key: 'upd-1',
  }));
  assert.deepEqual(again, answer);
  assert.equal(s.gw.requests.length, 1);
  assert.equal(fs.readFileSync(file, 'utf8').trim().split('\n').length, 1);
});

test('an update with a session records its outbound transcript through gc under the same key; the call-back never re-sends', async (t) => {
  const s = await setup(t);
  const answer = await s.pub.handlePublishUpdate(JSON.stringify({
    to: PHONE, text: 'Two deliveries due tomorrow.', idempotency_key: 'upd-rec', session_id: 'mayor-session',
  }));
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.transcript_recorded, true);
  assert.equal(s.gcFake.outbound.length, 1);
  const rec = s.gcFake.outbound[0];
  assert.equal(rec.idempotency_key, 'upd-rec');
  assert.deepEqual(rec.conversation, conversationRef(CITY, PHONE));
  assert.match(rec.text, /dynasty_update template\] Two deliveries due tomorrow\.$/);
  assert.equal(s.gcFake.callbacks[0].json.message_id, answer.json.message_id);
  assert.equal(s.gw.requests.length, 1, 'gc\'s call-back is answered from the settled update');
});

test('a template send leaves the window closed: a /publish right after it still maps to window_closed with the hint, and the owner\'s next inbound reopens it', async (t) => {
  const s = await setup(t);
  // The owner last wrote two days ago: closed in the ledger and at the gateway.
  s.ledger.noteInbound(PHONE, T0 - 2 * DAY);
  assert.equal(s.ledger.windowOpen(PHONE), false);

  const update = await s.pub.handlePublishUpdate(JSON.stringify({ to: PHONE, text: 'Weekly numbers are in.' }));
  assert.equal(update.json.delivered, true);
  assert.equal(s.ledger.windowOpen(PHONE), false, 'a template never opens the window');
  assert.equal(s.ledger.windowClosesAt(PHONE), T0 - 2 * DAY + DAY - 120_000, 'nor moves it');

  const after = await s.pub.handlePublish(publishBody('Following up on the update', { key: 'gc-after-upd' }));
  assert.equal(after.json.delivered, false);
  assert.equal(after.json.metadata.reason, 'window_closed');
  assert.match(after.json.metadata.hint, /gc whatsapp publish --update/);

  // The owner writes back: the inbound engine notes it and the gateway's
  // window reopens with it.
  s.setNow(T0 + 5 * 60_000);
  s.ledger.noteInbound(PHONE, T0 + 5 * 60_000);
  s.gw.window.set(PHONE, true);
  assert.equal(s.ledger.windowOpen(PHONE), true);
  assert.equal(s.ledger.windowClosesAt(PHONE), T0 + 5 * 60_000 + DAY - 120_000);

  const reply = await s.pub.handlePublish(publishBody('Following up on the update', { key: 'gc-after-upd' }));
  assert.equal(reply.json.delivered, true, 'the same key, retried once the window reopened, goes out');
  const refused = await s.pub.handlePublishUpdate(JSON.stringify({ to: PHONE, text: 'another update' }));
  assert.equal(refused.status, 409, 'a free reply is cheaper now');
});

test('nothing logged carries message text, a phone number in clear or the credential', async (t) => {
  const lines = [];
  const s = await setup(t, { bound: [PHONE], log: (line) => lines.push(String(line)) });
  s.gw.window.set(PHONE, true);
  await s.pub.handlePublish(publishBody('SECRET-TEXT-ONE', { key: 'gc-log1' }));
  s.gw.window.set(PHONE, false);
  await s.pub.handlePublish(publishBody('SECRET-TEXT-TWO', { key: 'gc-log2' }));
  await s.pub.handlePublish(publishBody('SECRET-TEXT-THREE', { to: OTHER, key: 'gc-log3' }));
  await s.pub.handlePublishUpdate(JSON.stringify({ to: PHONE, text: 'SECRET-TEXT-FOUR' }));
  s.gcFake.failStatus = 500;
  s.gw.window.set(PHONE, true);
  await s.pub.handlePublish(publishBody('SECRET-TEXT-FIVE', { key: 'cli-log', metadata: { origin: 'cli' } }));
  assert.ok(lines.length >= 4, 'the paths above do log');
  for (const line of lines) {
    assert.ok(!line.includes('SECRET-TEXT'), `message text logged: ${line}`);
    assert.ok(!line.includes(PHONE) && !line.includes(OTHER), `a phone in clear: ${line}`);
    assert.ok(!line.includes(CREDENTIAL), `the credential: ${line}`);
  }
  assert.ok(lines.some((l) => l.includes('…4567')), 'the last four digits name the phone');
});

// --- the ledger ---------------------------------------------------------------

test('the ledger opens the window only on an inbound (24 hours minus the 120-second margin), never on a send of any type', async (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wa-ledger-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const clockFile = path.join(dir, 'clock');
  fs.writeFileSync(clockFile, String(T0));
  const clock = createClock({ WHATSAPP_TEST_CLOCK: clockFile });
  const stateDir = path.join(dir, 'state');
  const ledger = createLedger({ stateDir, clock });

  assert.equal(ledger.windowClosesAt(PHONE), null);
  assert.equal(ledger.windowOpen(PHONE), false);
  ledger.noteSend(PHONE, { type: 'template', message_id: 'wamid.t1', at: T0 });
  ledger.noteSend(PHONE, { type: 'text', message_id: 'wamid.x1', at: T0 });
  assert.equal(ledger.windowClosesAt(PHONE), null, 'no send of any type opens it');
  assert.equal(ledger.windowOpen(PHONE), false);

  ledger.noteInbound(PHONE, T0);
  const closes = T0 + DAY - 120_000;
  assert.equal(ledger.windowClosesAt(PHONE), closes);
  assert.equal(ledger.windowOpen(PHONE, closes - 1), true);
  assert.equal(ledger.windowOpen(PHONE, closes), false);

  ledger.noteInbound(PHONE, T0 - 60 * 60_000);
  assert.equal(ledger.windowClosesAt(PHONE), closes, 'an older inbound never moves it back');
  ledger.noteSend(PHONE, { type: 'text', message_id: 'wamid.x2', at: T0 + 1000 });
  assert.equal(ledger.windowClosesAt(PHONE), closes);

  const file = path.join(stateDir, 'ledger.json');
  assert.equal(fs.statSync(file).mode & 0o777, 0o600);
  const reloaded = createLedger({ stateDir, clock });
  assert.equal(reloaded.windowClosesAt(PHONE), closes);
  const snap = reloaded.snapshot();
  assert.equal(snap.conversations[PHONE].last_inbound_at, T0);
  assert.deepEqual(snap.conversations[PHONE].sends.map((x) => x.message_id), ['wamid.t1', 'wamid.x1', 'wamid.x2']);
  assert.deepEqual(snap.conversations[PHONE].sends.map((x) => x.type), ['template', 'text', 'text']);

  const custom = createLedger({ stateDir: path.join(dir, 'other'), clock, marginMs: 0 });
  custom.noteInbound(PHONE, T0);
  assert.equal(custom.windowClosesAt(PHONE), T0 + DAY);
});

test('the gateway\'s window_closed closes the ledger\'s window until the next inbound', async (t) => {
  const s = await setup(t);
  // The ledger believes the window open (a skewed or stale inbound time) but
  // the gateway, the authority, refuses: /publish-update must not then 409.
  s.ledger.noteInbound(PHONE, T0 - 60_000);
  assert.equal(s.ledger.windowOpen(PHONE), true);
  const refused = await s.pub.handlePublish(publishBody('hello?', { key: 'gc-stale' }));
  assert.equal(refused.json.metadata.reason, 'window_closed');
  assert.equal(s.ledger.windowOpen(PHONE), false);
  const update = await s.pub.handlePublishUpdate(JSON.stringify({ to: PHONE, text: 'Today: 3 orders shipped.' }));
  assert.equal(update.json.delivered, true);

  s.setNow(T0 + 60_000);
  s.ledger.noteInbound(PHONE, T0 + 60_000);
  assert.equal(s.ledger.windowOpen(PHONE), true);
});
