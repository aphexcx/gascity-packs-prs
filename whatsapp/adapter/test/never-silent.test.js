import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { readHealth, createNeverSilent } from '../src/never-silent.js';
import { createPublisher } from '../src/outbound.js';

test('never-silent rechecks eligibility inside the publisher queue after session words', async (t) => {
  const f = fixture(t);
  let release;
  let started;
  const sending = new Promise((resolve) => { started = resolve; });
  const sends = [];
  const publisher = createPublisher({ stateDir: f.opts.stateDir, notices: f.e, clock: () => f.now,
    gateway: { window: async () => ({ windows: [{ conversation_id: 'example-owner' }] }),
      send: async (body) => { sends.push(body); if (sends.length === 1) { started(); await new Promise((resolve) => { release = resolve; }); } return { message_id: 'example-post' }; } },
  });
  f.opts.post = (m, line, canPost) => publisher.notice(m.conversation, line, 'example-notice', canPost);
  // Keep the engine used by publisher and inject the callback through a closure.
  const original = f.e;
  f.restart();
  original.act = (...args) => f.e.act(...args);
  f.e.track({ id: 'example-message', conversation: 'example-owner', targets: ['example-session'] });
  f.e.stop('example-message', false);
  f.now += 180000;
  const reply = publisher.handlePublish(JSON.stringify({ session_id: 'example-session', conversation: { conversation_id: 'example-owner' }, text: 'example reply', idempotency_key: 'example-reply' }));
  await sending;
  const polling = f.e.poll();
  await new Promise((resolve) => setImmediate(resolve));
  release();
  assert.equal((await reply).json.delivered, true);
  await polling;
  assert.equal(sends.length, 1);
  assert.equal(f.e.status().owner_lines_seven_days, 0);
});

test('never-silent health: the four states and every unknown case', (t) => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'never-silent-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'health.json');
  const now = Date.parse('2026-11-12T08:00:00Z');
  assert.equal(readHealth(file, now).state, 'unknown');
  for (const state of ['ok', 'no_seat', 'seat_at_cap', 'session_down', 'busy']) {
    fs.writeFileSync(file, JSON.stringify({ state, since: '2026-11-12T07:00:00Z', checked_at: '2026-11-12T08:00:00Z' }));
    assert.equal(readHealth(file, now).state, state === 'busy' ? 'unknown' : state);
  }
  for (const body of ['{', '{}', '{"state":"no_seat","since":"yesterday","checked_at":"now"}',
    '{"state":"session_down","since":"2026-11-12T07:00:00Z","checked_at":"2026-11-12T07:54:59Z"}']) {
    fs.writeFileSync(file, body);
    const health = readHealth(file, now);
    assert.equal(health.state, 'unknown');
    assert.ok(health.reason);
  }
});

function fixture(t, overrides = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'never-silent-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const f = { now: Date.parse('2026-11-12T08:00:00Z'), posts: [], mails: [], fail: false };
  f.opts = { enabled: true, stateDir: dir, healthFile: path.join(dir, 'health.json'), city: 'example-city', operatorContext: 'example-operator', clock: () => f.now,
    post: async (m, text) => { f.posts.push({ m, text }); },
    sendLocal: async () => {},
    send: async (mail) => { f.mails.push(mail); if (f.fail) throw new Error('refused'); }, ...overrides };
  f.restart = () => { f.e = createNeverSilent(f.opts); };
  f.restart();
  return f;
}

test('never-silent durable episodes aggregate twelve messages in four conversations', async (t) => {
  const f = fixture(t);
  for (let i = 0; i < 12; i++) { f.e.track({ id: `example-${i}`, conversation: `example-${i % 4}` }); f.e.result(`example-${i}`, 'failed', ['example-session']); }
  f.now += 180000; await f.e.poll();
  assert.equal(f.posts.length, 4); assert.equal(f.mails.length, 1);
  assert.match(f.mails[0].body, /messages: 12/); assert.match(f.mails[0].body, /conversations: 4/);
  f.restart(); await f.e.poll(); assert.equal(f.posts.length, 4); assert.equal(f.mails.length, 1);
  for (let i = 0; i < 12; i++) f.e.result(`example-${i}`, 'delivered', ['example-session']);
  f.now += 1000; await f.e.poll(); assert.equal(f.mails.length, 2); assert.match(f.mails[1].subject, / cleared$/);
  t.diagnostic(JSON.stringify(f.mails));
});

test('never-silent pending resets every failure streak and never means down', async (t) => {
  for (const kind of ['failed', 'unreachable']) {
    const f = fixture(t); f.e.track({ id: 'example-message', conversation: 'example-conversation' });
    f.e.result('example-message', kind); f.now += 120000; f.e.result('example-message', 'pending');
    f.now += 600000; await f.e.poll(); assert.equal(f.posts.length, 0);
    f.e.result('example-message', kind); f.now += 120000; await f.e.poll(); assert.equal(f.posts.length, 0);
    f.now += 60000; await f.e.poll(); assert.equal(f.posts.length, 1);
  }
});

test('never-silent unknown stop mails at once but a session reply suppresses the owner line', async (t) => {
  const f = fixture(t); f.e.track({ id: 'example-message', conversation: 'example-conversation' });
  f.e.result('example-message', 'pending', ['example-session']); f.e.stop('example-message', false);
  await f.e.poll(); assert.equal(f.mails.length, 1); assert.equal(f.posts.length, 0);
  f.now += 120000; f.e.act('example-conversation', 'example-session', f.now);
  f.now += 60000; await f.e.poll(); assert.equal(f.posts.length, 0);
});

test('never-silent an attributed reply suppresses unknown delivery even when no receipt named the target', async (t) => {
  const f = fixture(t); f.e.track({ id: 'example-message', conversation: 'example-conversation' });
  f.e.stop('example-message', false); f.e.act('example-conversation', 'example-session');
  f.now += 180000; await f.e.poll(); assert.equal(f.posts.length, 0);
});

test('never-silent health states post the exact owner lines only after delivery', async (t) => {
  const expected = {
    no_seat: "I can't answer yet because no Claude account is signed in for me. The team that runs me has been told.",
    seat_at_cap: "I've reached my usage limit for now and can't answer until it resets. The team that runs me has been told.",
    session_down: "I can't read your message right now. The team that runs me has been told, and I'll answer as soon as I'm back.",
  };
  for (const state of ['ok', ...Object.keys(expected)]) {
    const f = fixture(t); f.e.track({ id: 'example-message', conversation: 'example-conversation' });
    f.e.result('example-message', 'delivered', ['example-session']); f.now += 180000;
    fs.writeFileSync(f.opts.healthFile, JSON.stringify({ state, since: new Date(f.now).toISOString(), checked_at: new Date(f.now).toISOString() }));
    await f.e.poll(); assert.equal(f.posts.length, state === 'ok' ? 0 : 1);
    if (state !== 'ok') { assert.equal(f.posts[0].text, expected[state]); t.diagnostic(f.posts[0].text); }
  }
});

test('never-silent marks precede owner posts, refusal still mails, no reminders', async (t) => {
  const f = fixture(t);
  f.opts.post = async () => { f.posts.push('refused'); const saved = JSON.parse(fs.readFileSync(path.join(f.opts.stateDir, 'never-silent.json'))); assert.equal(Object.keys(saved.owners).length, 1); throw new Error('window_closed'); };
  f.restart(); f.e.track({ id: 'example-message', conversation: 'example-conversation' }); f.e.stop('example-message', true);
  await f.e.poll(); f.restart(); f.now += 7 * 3600000; await f.e.poll();
  assert.equal(f.posts.length, 1); assert.equal(f.mails.length, 1); assert.equal(f.e.status().owner_lines_seven_days, 0);
});

test('never-silent mail retry marks survive restart and remote mail has no customer canaries', async (t) => {
  const f = fixture(t); f.fail = true; f.e.track({ id: 'CANARY_MESSAGE', conversation: 'CANARY_CUSTOMER_ROOM' }); f.e.stop('CANARY_MESSAGE', true);
  await f.e.poll(); f.restart(); await f.e.poll(); assert.equal(f.mails.length, 1);
  f.now += 60000; f.fail = false; await f.e.poll(); f.restart(); await f.e.poll(); assert.equal(f.mails.length, 2);
  assert.equal(f.mails[0].key, f.mails[1].key); assert.doesNotMatch(JSON.stringify(f.mails), /CANARY/);
});

test('never-silent is off by default and missing context sends no operator mail', async (t) => {
  const f = fixture(t, { enabled: false }); f.e.track({ id: 'example-message', conversation: 'example-conversation' }); f.e.stop('example-message', true); await f.e.poll(); assert.equal(f.posts.length, 0);
  const logs = [];
  const g = fixture(t, { operatorContext: '', log: (line) => logs.push(line) });
  assert.equal(logs.length, 0, 'no outage at startup');
  g.e.track({ id: 'example-message', conversation: 'example-conversation' }); g.e.stop('example-message', true); await g.e.poll(); assert.equal(g.mails.length, 0);
  g.restart(); await g.e.poll(); assert.equal(logs.length, 1, 'one log per durable outage');
});

test('never-silent clear mail survives an expired start mail', async (t) => {
  const f = fixture(t); f.fail = true;
  f.e.track({ id: 'example-message', conversation: 'example-conversation' }); f.e.stop('example-message', true);
  await f.e.poll(); f.now += 24 * 3600000 + 60000;
  f.e.forget('example-message'); f.fail = false;
  await f.e.poll();
  assert.equal(f.mails.length, 2); assert.match(f.mails[1].subject, / cleared$/);
});

test('never-silent local cause-one mail survives owner refusal and unknown grace', async (t) => {
  for (const definite of [false, true]) {
    const local = [];
    const f = fixture(t, { post: async () => { throw new Error('refused'); }, sendLocal: async (m, line) => { local.push({ m, line }); } });
    f.e.track({ id: 'example-message', conversation: 'example-conversation' }); f.e.stop('example-message', definite);
    await f.e.poll();
    assert.equal(local.length, 1); assert.equal(local[0].line, '');
    f.restart(); await f.e.poll(); assert.equal(local.length, 1);
  }
});
