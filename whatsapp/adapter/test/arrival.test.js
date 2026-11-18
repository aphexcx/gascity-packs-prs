import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createChoice, createArrival, INSTRUCTIONS } from '../src/arrival.js';
import { rig, waitFor } from './fakes.js';

const fixture = { reactions: [
  { emoji: '👀', slack_name: 'eyes', meaning: 'Received.', adapter_may_place: true },
  { emoji: '❤️', slack_name: 'heart', meaning: 'Welcome.', adapter_may_place: true },
  { emoji: '🧪', slack_name: 'fixture_arrival', meaning: 'A fixture arrival.', adapter_may_place: true },
  { emoji: '✅', slack_name: 'fixture_claim', meaning: 'Done.', adapter_may_place: false },
] };
const catalog = JSON.stringify(fixture);
const temp = (t) => { const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'arrival-test-')); t.after(() => fs.rmSync(dir, { recursive: true, force: true })); return dir; };

test('nine live deliveries reach gc while Jev holds, with four choices and one reaction each', async (t) => {
  let active = 0; let maximum = 0; const held = new Set();
  const server = http.createServer(async (req, res) => {
    for await (const _ of req) { /* drain request */ }
    active++; maximum = Math.max(maximum, active); held.add(res);
    res.on('close', () => { active--; held.delete(res); });
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => { for (const response of held) response.destroy(); server.close(); });
  const r = await rig({ extra: { ACK_REACTION: 'jev', JEV_API_KEY: 'test-credential', JEV_API_BASE_URL: `http://127.0.0.1:${server.address().port}` } });
  t.after(() => r.close());
  for (let i = 0; i < 9; i++) r.gw.addText('owner-example', `message-example-${i}`, 'synthetic');
  r.start();
  await waitFor(() => r.gc.posts.length === 9, { what: 'all nine deliveries beside held choices' });
  // The fake has answered no choice. Delivery may itself take longer than the
  // choice budget on a loaded host, so defaults may already have been sent.
  assert.ok(maximum <= 4);
  assert.deepEqual(r.gc.posts.map((post) => post.pmid), Array.from({ length: 9 }, (_, i) => `message-example-${i}`));
  await waitFor(() => r.gw.sends.filter((s) => s.type === 'reaction').length === 9, { what: 'nine default reactions' });
  assert.equal(maximum, 4);
  assert.equal(new Set(r.gw.sends.map((s) => s.idempotency_key)).size, 9);
});

test('both tie-break pairs preserve the text, criteria and chosen arrival row', async () => {
  const actual = fs.readFileSync(new URL('../../reactions.json', import.meta.url), 'utf8');
  const rows = JSON.parse(actual).reactions;
  for (const [text, names] of [['Can you find out what the plumber charged us last year?', ['eyes', 'mag']], ['We closed! Please send the invoice.', ['tada', 'eyes']]]) {
    for (const name of names) {
      const choice = createChoice({ env: { ACK_REACTION: 'jev', JEV_API_KEY: 'test-credential' }, catalog: actual, fetchImpl: async (_url, options) => {
        const body = JSON.parse(options.body); assert.equal(body.state, text); assert.equal(body.questions.ack.instructions, INSTRUCTIONS);
        for (const candidate of names) { const i = rows.findIndex((row) => row.slack_name === candidate); assert.equal(body.questions.ack.criteria[`option_${String(i + 1).padStart(3, '0')}`], rows[i].meaning); }
        const i = rows.findIndex((row) => row.slack_name === name);
        return new Response(JSON.stringify({ answers: { ack: { choice: `option_${String(i + 1).padStart(3, '0')}` } } }));
      } });
      assert.equal((await choice.choose(text, Date.now())).row.slack_name, name);
    }
  }
});

test('Jev offers catalog rows under neutral keys, preserves Unicode and sends only text', async (t) => {
  let chosen = 'option_001'; let calls = 0;
  const server = http.createServer(async (req, res) => {
    calls++; const parts = []; for await (const part of req) parts.push(part);
    assert.equal(req.method, 'POST'); assert.equal(req.url, '/v1/systemone');
    assert.equal(req.headers.authorization, 'Bearer test-credential');
    assert.deepEqual(JSON.parse(Buffer.concat(parts)), { model: 'jev-latest', state: 'synthetic', questions: { ack: {
      type: 'choice', instructions: INSTRUCTIONS, criteria: { option_001: 'Received.', option_002: 'Welcome.', option_003: 'A fixture arrival.' },
    } } });
    res.end(JSON.stringify({ answers: { ack: { choice: chosen } } }));
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve)); t.after(() => server.close());
  const choice = createChoice({ env: { ACK_REACTION: 'jev', JEV_API_KEY: 'test-credential', JEV_API_BASE_URL: `http://127.0.0.1:${server.address().port}` }, catalog });
  for (const [i, row] of fixture.reactions.slice(0, 3).entries()) { chosen = `option_00${i + 1}`; const result = await choice.choose('synthetic', Date.now()); assert.equal(result.row.emoji, row.emoji); assert.equal(result.how, 'jev'); }
  for (chosen of ['fixture_claim', '✅', 'unknown']) assert.equal((await choice.choose('synthetic', Date.now())).row.slack_name, 'eyes');
  assert.equal(calls, 6);
});

test('every unavailable answer uses one default with no retry; catalog failure allows eyes only', async () => {
  for (const value of ['', '{', '{"reactions":[]}', '{"reactions":[{"emoji":"x","slack_name":"x","meaning":"x","adapter_may_place":"true"}]}']) {
    for (const mode of ['eyes', 'jev']) assert.equal((await createChoice({ env: { ACK_REACTION: mode }, catalog: value }).choose('synthetic', Date.now())).row.slack_name, 'eyes');
    assert.throws(() => createChoice({ env: { ACK_REACTION: 'mag' }, catalog: value }), /ACK_REACTION.*reactions.json/);
  }
  for (const status of [401, 422, 429, 529, 500, 200]) {
    let calls = 0;
    const choice = createChoice({ env: { ACK_REACTION: 'jev', JEV_API_KEY: 'test-credential' }, catalog,
      fetchImpl: async () => { calls++; return new Response('{', { status }); } });
    assert.equal((await choice.choose('synthetic', Date.now())).how, 'default'); assert.equal(calls, 1);
  }
});

test('four Jev slots and injected deadline let nine messages finish without holding delivery', async (t) => {
  const timers = []; let active = 0; let max = 0;
  const choice = createChoice({ env: { ACK_REACTION: 'jev', JEV_API_KEY: 'test-credential' }, catalog, now: () => 1000,
    setTimer: (fn, ms) => { assert.equal(ms, 1500); timers.push(fn); return fn; }, clearTimer: () => {},
    fetchImpl: async (_url, { signal }) => { active++; max = Math.max(max, active); try { await new Promise((resolve, reject) => signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true })); } finally { active--; } },
  });
  const pending = Array.from({ length: 9 }, () => choice.choose('synthetic', 1000));
  await new Promise(setImmediate); assert.equal(max, 4); timers.forEach((fn) => fn());
  for (const result of await Promise.all(pending)) assert.equal(result.row.slack_name, 'eyes');
  assert.equal(max, 4);
});

test('WhatsApp reactions are once across restart, do not change read marks and exclude nonmessages', async (t) => {
  const stateDir = temp(t); const calls = []; const gateway = { send: async (body) => { calls.push(body); return { message_id: 'reaction-example' }; } };
  const choice = createChoice({ env: { ACK_REACTION: 'heart' }, catalog });
  const message = { kind: 'message', conversation_id: 'owner-example', user_id: 'user-example', message: { id: 'message-example', type: 'text', timestamp: String(Math.floor(Date.now() / 1000)), text: { body: 'synthetic' } } };
  let arrival = createArrival({ choice, stateDir, gateway });
  arrival.observe(message); arrival.observe(message); await arrival.wait();
  arrival = createArrival({ choice, stateDir, gateway }); arrival.observe(message); await arrival.wait();
  for (const kind of ['status', 'joined']) arrival.observe({ ...message, kind });
  arrival.observe({ ...message, message: { ...message.message, id: 'reaction-in', type: 'reaction' } });
  await arrival.wait(); assert.equal(calls.length, 1); assert.equal(calls[0].reaction.emoji, '\u2764\ufe0f'); assert.equal(calls[0].to, 'user-example'); assert.equal(calls[0].reaction.message_id, 'message-example'); assert.match(calls[0].idempotency_key, /^ack:/);
});

test('canary key is absent from errors and log output; empty text and missing key never call', async () => {
  const logs = []; let calls = 0; const canary = 'canary-must-stay-private';
  const choice = createChoice({ env: { ACK_REACTION: 'jev', JEV_API_KEY: canary }, catalog, log: (s) => logs.push(s), fetchImpl: async () => { calls++; throw new Error(canary); } });
  await choice.choose('', Date.now()); await choice.choose('synthetic', Date.now());
  await createChoice({ env: { ACK_REACTION: 'jev' }, catalog, log: (s) => logs.push(s), fetchImpl: async () => { calls++; } }).choose('synthetic', Date.now());
  assert.equal(calls, 1); assert.ok(!JSON.stringify(logs).includes(canary));
  assert.throws(() => createChoice({ env: { ACK_REACTION: 'jev', JEV_API_BASE_URL: 'http://example.invalid', JEV_API_KEY: canary }, catalog }), /JEV_API_BASE_URL/);
});

// --- PR 104 round 2 -------------------------------------------------------------

const DAY_MS = 24 * 60 * 60 * 1000;
const cases = JSON.parse(fs.readFileSync(new URL('./fixtures/redaction-cases.json', import.meta.url), 'utf8'));
// A failure names the case and the lengths only: the shared fixtures assemble
// credential shapes from parts, so a printed state would hold a whole one.
const caseText = (c) => (c.text_parts ? c.text_parts.join('') : c.text);

test('ACK_SCOPE is checked only when ACK_REACTION is set', () => {
  const logs = [];
  for (const mode of [undefined, '']) {
    const env = { ACK_SCOPE: 'everyone', ...(mode === undefined ? {} : { ACK_REACTION: mode }) };
    let choice;
    assert.doesNotThrow(() => { choice = createChoice({ env, catalog, log: (s) => logs.push(s) }); });
    assert.equal(choice.enabled, false);
    assert.equal(choice.all, false, 'an unknown scope reads as addressed while off');
  }
  assert.deepEqual(logs, [], 'no line while the switch is off');
  assert.throws(() => createChoice({ env: { ACK_REACTION: 'eyes', ACK_SCOPE: 'everyone' }, catalog }), /^Error: ACK_SCOPE must be addressed or all$/);
});

test('the Jev state is redacted like Slack', async () => {
  let state; let calls = 0;
  const choice = createChoice({ env: { ACK_REACTION: 'jev', JEV_API_KEY: 'test-credential' }, catalog, fetchImpl: async (_url, options) => {
    calls++; state = JSON.parse(options.body).state;
    return new Response(JSON.stringify({ answers: { ack: { choice: 'option_001' } } }));
  } });
  let checked = 0;
  for (const c of cases) {
    state = undefined; const before = calls;
    await choice.choose(caseText(c), Date.now());
    const want = Array.from(c.want).slice(0, 1000).join('');
    assert.equal(calls, before + 1, `${c.name}: one request`);
    assert.ok(state === want, `${c.name}: state is not the redacted want (state ${String(state ?? '').length} chars, want ${want.length})`);
    checked++;
  }
  assert.equal(checked, cases.length); assert.ok(checked >= 100, `only ${checked} fixture cases`);
  // Redact first, then cut: a secret that straddles the cut leaves no tail.
  state = undefined;
  await choice.choose(`${'. '.repeat(495)}${'a'.repeat(100)}`, Date.now());
  assert.equal(Array.from(state).length, 1000);
  assert.ok(!state.includes('aaaa'), 'a partial secret survived the cut');
});

test('ack markers older than one day are pruned at start; nothing else is touched', async (t) => {
  const stateDir = temp(t); const dir = path.join(stateDir, 'ack-attempts'); fs.mkdirSync(dir, { recursive: true });
  const now = Date.now();
  const make = (where, name, ageMs, body = '') => { const file = path.join(where, name); fs.writeFileSync(file, body); const at = new Date(now - ageMs); fs.utimesSync(file, at, at); };
  make(dir, 'a'.repeat(64), 2 * DAY_MS); // old, empty, ours: removed
  make(dir, 'b'.repeat(64), DAY_MS - 60000); // newer than a day: kept
  make(dir, 'c'.repeat(64), 2 * DAY_MS, 'not empty'); // not an empty marker: kept
  make(dir, 'stray-file', 2 * DAY_MS); // another name: kept
  make(dir, 'stopwatch-2020-01.jsonl', 40 * DAY_MS, '{}\n'); // a stopwatch name: kept
  make(stateDir, 'stopwatch-2020-01.jsonl', 40 * DAY_MS, '{}\n');
  const arrival = createArrival({ choice: createChoice({ env: {}, catalog }), stateDir, gateway: { send: async () => ({}) } });
  await arrival.wait();
  assert.deepEqual(fs.readdirSync(dir).sort(), ['b'.repeat(64), 'c'.repeat(64), 'stopwatch-2020-01.jsonl', 'stray-file']);
  assert.ok(fs.existsSync(path.join(stateDir, 'stopwatch-2020-01.jsonl')));
});
