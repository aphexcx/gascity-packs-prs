import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import test from 'node:test';
import { createPublisher } from '../src/outbound.js';
import { createGatewayClient } from '../src/gateway.js';
import { createLedger } from '../src/ledger.js';
import { createListener } from '../src/index.js';
import { createNeverSilent } from '../src/never-silent.js';
import { startFakeGateway, tempDir, createTestClock, CREDENTIAL } from './fakes.js';

const OWNER = 'example-owner';
const MESSAGE = 'wamid.reaction-canary';
const request = (extra = {}) => ({ session_id: 's-mayor', conversation: { conversation_id: OWNER },
  message_id: MESSAGE, emoji: '👀', idempotency_key: 'react-key', ...extra });
const keyFor = (body) => `reaction:${crypto.createHash('sha256').update(JSON.stringify([
  body.idempotency_key, body.conversation.conversation_id, body.message_id, body.emoji,
])).digest('hex')}`;

async function setup(t, { enabled = false } = {}) {
  const dir = tempDir('wa-react-');
  const clock = createTestClock(dir);
  const gw = await startFakeGateway({ clock });
  t.after(async () => { await gw.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  gw.addText(OWNER, MESSAGE, 'fixture request');
  gw.userIds.set(OWNER, 'US.fixture');
  const stateDir = path.join(dir, 'state');
  const logs = [], transcripts = [], callbacks = [];
  const ledger = createLedger({ stateDir, clock: clock.now });
  ledger.noteInbound(OWNER, clock.now());
  const notices = createNeverSilent({ enabled, stateDir, clock: clock.now, post: async () => {} });
  const gateway = createGatewayClient({ url: gw.url, credential: CREDENTIAL });
  const r = { dir, clock, gw, gateway, ledger, notices, logs, transcripts, callbacks, stateDir };
  r.restart = () => {
    r.publisher = createPublisher({ stateDir, city: 'test-city', cityDir: dir, gateway, ledger, notices, clock: clock.now,
      log: (line) => logs.push(line), gc: { recordOutbound: async (body) => {
        transcripts.push(body);
        const answer = await r.publisher.handlePublish(JSON.stringify(body));
        callbacks.push(answer);
        return answer.json.delivered ? { TranscriptEntry: { Sequence: transcripts.length } } : {};
      } } });
  };
  r.restart();
  r.react = (body = request()) => r.publisher.handleReact(JSON.stringify(body));
  return r;
}

for (const emoji of ['👀', '']) test(`reaction ${emoji ? 'placement' : 'removal'}: wire, receipt, ledger and redacted logs`, async (t) => {
  const r = await setup(t);
  const before = r.ledger.windowClosesAt(OWNER);
  const body = request({ emoji });
  const answer = await r.react(body);
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.message_id, 'wamid.out.1');
  assert.equal(answer.json.idempotency_key, body.idempotency_key);
  assert.deepEqual(answer.json.metadata, { reaction: emoji ? 'placed' : 'removed', reacted_to: MESSAGE });
  assert.deepEqual(r.gw.sends.map(({ owner, at, ...wire }) => wire), [{ to: 'US.fixture', type: 'reaction',
    reaction: { message_id: MESSAGE, emoji }, idempotency_key: keyFor(body) }]);
  assert.equal(r.ledger.snapshot().conversations[OWNER].sends[0].type, 'reaction');
  assert.equal(r.ledger.windowClosesAt(OWNER), before);
  assert.ok(r.logs.length);
  assert.ok(r.logs.every((line) => !line.includes(MESSAGE) && !line.includes(OWNER)));
  assert.equal(fs.existsSync(path.join(r.stateDir, 'never-silent.json')), false);
});

for (const [label, extra] of [
  ['session', { session_id: '' }], ['conversation', { conversation: {} }],
  ...['', 'x'.repeat(257), 'white space', 'a\nb', 'a\u0000b', '{message_ts}', 'a\u007fb'].map((message_id, i) => [`id-${i}`, { message_id }]),
  ...[null, 4, 'eyes', '1', ' ', '👀'.repeat(17)].map((emoji, i) => [`emoji-${i}`, { emoji }]),
]) test(`reaction rejects ${label} before sending`, async (t) => {
  const r = await setup(t);
  const result = await r.react(request(extra));
  assert.equal(result.status, 400);
  assert.equal(r.gw.windowCalls, 0);
  assert.equal(r.gw.sends.length, 0);
  if (extra.emoji === 'eyes') assert.match(result.json.error, /emoji character/);
});

test('reaction accepts metadata session attribution and the listener route', async (t) => {
  const r = await setup(t);
  const server = http.createServer(createListener({ publisher: r.publisher }));
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => server.close(resolve)));
  const response = await fetch(`http://127.0.0.1:${server.address().port}/react`, { method: 'POST',
    body: JSON.stringify(request({ session_id: '', metadata: { source_session_id: 's-mayor' } })) });
  assert.equal(response.status, 200);
  assert.equal((await response.json()).delivered, true);
});

test('reaction window refusal records closure without suggesting a template', async (t) => {
  const r = await setup(t);
  r.gw.lastInbound.clear();
  const answer = await r.react();
  assert.equal(answer.json.delivered, false);
  assert.equal(answer.json.metadata.reason, 'window_closed');
  assert.doesNotMatch(answer.json.metadata.hint, /update/i);
  assert.equal(r.ledger.snapshot().conversations[OWNER].closed_at, r.clock.now());
  assert.equal(r.gw.sends.length, 0);
});

for (const [error, expected] of [
  [{ status: 502, json: { error: 'meta_error', meta_status: 400, meta_code: 131009 } }, 'gateway_refused'],
  [{ status: 422, json: { error: 'unknown_message' } }, 'unknown_message'],
]) test(`reaction refusal ${expected} is permanent`, async (t) => {
  const r = await setup(t);
  r.gateway.send = async () => { throw Object.assign(new Error(MESSAGE), error); };
  const result = await r.react();
  assert.equal(result.status, 200);
  assert.equal(result.json.failure_kind, 'permanent');
  assert.equal(result.json.metadata.reason, expected);
  if (expected === 'gateway_refused') {
    assert.equal(result.json.metadata.gateway_error, 'meta_error');
    assert.equal(result.json.metadata.meta_code, '131009');
  }
  assert.ok(r.logs.every((line) => !line.includes(MESSAGE)));
});

test('reaction refuses an unbound conversation without a send', async (t) => {
  const r = await setup(t);
  r.gw.bound.clear();
  assert.equal((await r.react()).json.metadata.reason, 'not_bound');
  assert.equal(r.gw.sends.length, 0);
});

test('reaction settled receipt survives restart; changed target or emoji conflicts', async (t) => {
  const r = await setup(t);
  const first = await r.react();
  r.restart();
  assert.deepEqual(await r.react(), first);
  assert.equal((await r.react(request({ message_id: 'wamid.other' }))).status, 409);
  assert.equal((await r.react(request({ emoji: '👍' }))).status, 409);
  assert.equal(r.gw.sends.length, 1);
});

test('reaction retries a 500 and a lost answer under the same gateway key', async (t) => {
  const r = await setup(t);
  r.gw.sendFailOn.add(keyFor(request()));
  assert.equal((await r.react()).json.failure_kind, 'transient');
  const send = r.gateway.send;
  r.gateway.send = async (body) => { await send(body); throw Object.assign(new Error('lost answer'), { noAnswer: true }); };
  assert.equal((await r.react()).json.delivered, false);
  r.gateway.send = send;
  assert.equal((await r.react()).json.delivered, true);
  assert.equal(r.gw.sends.length, 1);
});

test('reaction shares the publish chain for its owner', async (t) => {
  const r = await setup(t);
  const send = r.gateway.send;
  let release, entered;
  const held = new Promise((resolve) => { release = resolve; });
  const started = new Promise((resolve) => { entered = resolve; });
  const types = [];
  r.gateway.send = async (body) => { types.push(body.type); if (body.type === 'text') { entered(); await held; } return send(body); };
  const pub = r.publisher.handlePublish(JSON.stringify({ ...request(), text: 'fixture reply', idempotency_key: 'publish-key' }));
  await started;
  const reaction = r.react();
  await new Promise((resolve) => setImmediate(resolve));
  assert.deepEqual(types, ['text']);
  release();
  assert.equal((await pub).json.delivered, true);
  assert.equal((await reaction).json.delivered, true);
  assert.deepEqual(types, ['text', 'reaction']);
});

for (const emoji of ['👀', '']) test(`CLI reaction ${emoji ? 'placement' : 'removal'} records one transcript and counts as an answer`, async (t) => {
  const r = await setup(t, { enabled: true });
  r.notices.track({ id: MESSAGE, conversation: OWNER, targets: ['s-mayor'] });
  r.notices.result(MESSAGE, 'failed');
  const body = request({ emoji, metadata: { origin: 'cli' } });
  const answer = await r.react(body);
  assert.equal(answer.json.transcript_recorded, true);
  assert.equal(r.callbacks[0].json.delivered, true);
  assert.equal(r.transcripts[0].text, emoji ? `[reaction ${emoji} to ${MESSAGE}]` : `[reaction removed from ${MESSAGE}]`);
  await r.react(body);
  assert.equal(r.transcripts.length, 1);
  assert.equal(r.gw.sends.length, 1);
  r.clock.advance(240000);
  await r.notices.poll();
  assert.deepEqual(r.notices.status().outages, []);
});

for (const verb of ['text', 'file', 'update']) test(`in-flight key ownership: rejected ${verb} never poisons a reaction receipt`, async (t) => {
  const r = await setup(t);
  const file = path.join(r.dir, 'fixture.txt');
  fs.writeFileSync(file, 'fixture file');
  const window = r.gateway.window;
  let release, entered;
  const held = new Promise((resolve) => { release = resolve; });
  const started = new Promise((resolve) => { entered = resolve; });
  r.gateway.window = async () => { entered(); await held; return window(); };
  const reaction = r.react();
  await started;
  const publish = () => verb === 'update'
    ? r.publisher.handlePublishUpdate(JSON.stringify({ to: OWNER, text: 'fixture update', session_id: 's-mayor', idempotency_key: 'react-key' }))
    : r.publisher.handlePublish(JSON.stringify({ ...request(), text: verb === 'file' ? undefined : 'fixture reply', ...(verb === 'file' ? { file } : {}) }));
  try { assert.equal((await publish()).status, 409); } finally { release(); }
  assert.equal((await reaction).json.delivered, true);
  assert.equal((await publish()).status, 409, 'a rejected verb never inherits the reaction receipt');
  assert.deepEqual(r.gw.sends.map((s) => s.type), ['reaction']);
});

for (const verb of ['text', 'reaction', 'file']) test(`unsent attempt: ${verb} retry attributes the actual sending session`, async (t) => {
  const r = await setup(t, { enabled: true });
  const file = path.join(r.dir, 'fixture.txt');
  fs.writeFileSync(file, 'fixture file');
  const publish = (session_id) => verb === 'reaction' ? r.react(request({ session_id }))
    : r.publisher.handlePublish(JSON.stringify({ ...request({ session_id }), text: verb === 'file' ? undefined : 'fixture reply',
      ...(verb === 'file' ? { file, caption: 'fixture reply' } : {}) }));
  const method = verb === 'file' ? 'window' : 'send';
  const original = r.gateway[method];
  r.gateway[method] = async () => { throw Object.assign(new Error('not dispatched'), { noAnswer: true, notSent: true }); };
  assert.equal((await publish('s-first')).json.delivered, false);
  r.gateway[method] = original;
  r.clock.advance(1000);
  r.notices.track({ id: MESSAGE, conversation: OWNER, targets: ['s-retry'] });
  r.notices.result(MESSAGE, 'failed');
  assert.equal((await publish('s-retry')).json.delivered, true);
  const state = JSON.parse(fs.readFileSync(path.join(r.stateDir, 'never-silent.json')));
  assert.equal(state.acts.at(-1).session, 's-retry');
  r.clock.advance(240000);
  await r.notices.poll();
  assert.deepEqual(r.notices.status().outages, []);
});
