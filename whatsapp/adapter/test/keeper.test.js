// keeper.test.js — the window-keeper (src/keeper.js, src/zoned.js) under the
// test clock, the pack-adapter spec's SCOPE item 6 cases (10a)–(10h).
//
// In-test fakes: a gateway serving GET /v1/window from the inbound times a
// test sets (and counting POST /v1/send, which the keeper must never call),
// and a gc serving POST /v0/city/{city}/extmsg/inbound that records every
// keeper event attempt by provider_message_id at the test clock's time and
// answers 500, 400, no_route or no answer at all on demand. The clock is a
// file (WHATSAPP_TEST_CLOCK); a test advances it and calls keeper.tick()
// directly, no timers. A restart is a new createKeeper on the same state
// dir; a SIGKILL is a real child process (WHATSAPP_TEST_KILL_AT, or a
// self-kill between two marks). Every asserted local time is logged with
// t.diagnostic so the transcript shows it.

import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import { createClock } from '../src/clock.js';
import { createGatewayClient } from '../src/gateway.js';
import { createGcClient } from '../src/gc.js';
import { createKeeper } from '../src/keeper.js';
import { assertZone, parseHours, zonedToInstant } from '../src/zoned.js';

const SEC = 1000;
const MIN = 60 * SEC;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const MARGIN = 120 * SEC; // the gateway's window margin: close = last inbound + 24 h - 120 s
const CITY = 'hou-dynasty';
const CHI = 'America/Chicago'; // CST (-06:00) all through Nov 2026 after Nov 1
const SAO = 'America/Sao_Paulo'; // -03:00, no DST
const WA = '15550001111';
const BR = '5511999990001';
const SRC = new URL('../src/', import.meta.url);

const at = (wall) => {
  const ms = Date.parse(wall);
  assert.ok(Number.isFinite(ms), `bad test instant ${wall}`);
  return ms;
};
const iso = (ms) => new Date(ms).toISOString();
const local = (ms, tz) => new Intl.DateTimeFormat('sv-SE', {
  timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit',
  hour: '2-digit', minute: '2-digit', second: '2-digit', hourCycle: 'h23',
}).format(ms);

const nudgeId = (wa, close, mark) => `keeper:${wa}:${iso(close)}:${mark}`;
const updateId = (wa, day) => `keeper:${wa}:${day}:update`;
const TAIL = 'One short line asking anyone in the group to reply.';
const plainText = (wa, n, since) => `[window-keeper] ${wa}: window closes in ${n} minutes, nobody from the business has written since ${since}. ${TAIL}`;
const shiftedText = (wa, closes, n) => `[window-keeper] ${wa}: window closes at ${closes}, after your hours; last chance before close of business in ${n} minutes. ${TAIL}`;
const updateHead = (wa) => `[window-keeper] ${wa}: daily update due. Send today's short business update with one acknowledgement ask.`;
const openText = (wa, closes) => `${updateHead(wa)} Window open, closes ${closes}.`;
const closedText = (wa, since) => `${updateHead(wa)} Window closed since ${since}: an actual update goes by gc whatsapp publish --update, else nothing.`;

function listen(handler) {
  return new Promise((resolve) => {
    const server = http.createServer(handler);
    server.listen(0, '127.0.0.1', () => resolve({ server, url: `http://127.0.0.1:${server.address().port}` }));
  });
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let text = '';
    req.setEncoding('utf8');
    req.on('data', (chunk) => { text += chunk; });
    req.on('end', () => resolve(text));
    req.on('error', reject);
  });
}

function sendJSON(res, status, body) {
  res.writeHead(status, { 'Content-Type': 'application/json' });
  res.end(JSON.stringify(body));
}

async function harness(t, { tz = CHI, windowReadMs, format = 'iso' } = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'whatsapp-keeper-test-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const clockFile = path.join(dir, 'clock');
  const stateDir = path.join(dir, 'state');
  const clock = createClock({ WHATSAPP_TEST_CLOCK: clockFile });
  const setClock = (ms) => fs.writeFileSync(clockFile, String(ms));

  // The fake gateway: the window per phone from the last inbound the test
  // sets, as ISO strings or epoch ms (the contract accepts both).
  const lastInbound = new Map();
  const gw = { reads: 0, sends: 0, failReads: false, format };
  const gateway = await listen(async (req, res) => {
    if (req.headers.authorization !== 'Bearer city-token') return sendJSON(res, 401, { error: 'unauthorized' });
    if (req.method === 'GET' && req.url === '/v1/window') {
      gw.reads += 1;
      if (gw.failReads) return sendJSON(res, 503, { error: 'down' });
      const fmt = (ms) => (ms == null ? null : gw.format === 'ms' ? ms : iso(ms));
      const windows = [...lastInbound].map(([id, last]) => ({
        conversation_id: id,
        last_inbound_at: fmt(last),
        window_closes_at: fmt(last == null ? null : last + DAY - MARGIN),
      }));
      return sendJSON(res, 200, { windows });
    }
    if (req.method === 'POST' && req.url === '/v1/send') {
      gw.sends += 1;
      await readBody(req);
      return sendJSON(res, 200, { message_id: 'wamid.x' });
    }
    return sendJSON(res, 404, { error: 'no route' });
  });

  // The fake gc: every keeper event attempt recorded at the clock's time;
  // the answers per provider_message_id queued on demand.
  const attempts = [];
  const answers = new Map();
  let receipts = 0;
  const gcFake = await listen(async (req, res) => {
    const body = await readBody(req);
    if (req.method !== 'POST' || req.url !== `/v0/city/${CITY}/extmsg/inbound`) return sendJSON(res, 404, { error: 'not found' });
    const { message } = JSON.parse(body);
    const id = message.provider_message_id;
    attempts.push({ id, message, at: clock() });
    const answer = answers.get(id)?.shift() ?? 'delivered';
    if (answer === 'drop') return req.socket.destroy();
    if (typeof answer === 'number') return sendJSON(res, answer, { error: `status ${answer}` });
    receipts += 1;
    return sendJSON(res, 200, {
      transcript_entry: { Sequence: receipts, ProviderMessageID: id },
      delivery: { receipt_id: `r${receipts}`, status: answer },
    });
  });

  t.after(() => {
    for (const { server } of [gateway, gcFake]) {
      server.closeAllConnections();
      server.close();
    }
  });

  const logs = [];
  const make = (opts = {}) => createKeeper({
    stateDir,
    clock,
    city: CITY,
    cityTz: tz,
    gateway: createGatewayClient({ url: gateway.url, credential: 'city-token' }),
    gc: createGcClient({ base: gcFake.url, city: CITY }),
    log: (line) => logs.push(String(line)),
    ...(windowReadMs ? { windowReadMs } : {}),
    ...opts,
  });

  // Advance the clock in steps, ticking at each, until `until`.
  async function run(keeper, until, step = MIN) {
    let now = clock();
    while (now < until) {
      now = Math.min(now + step, until);
      setClock(now);
      await keeper.tick();
    }
  }

  // A real child process running the keeper on the same state dir, clock
  // file and fakes: it ticks at each step and, with selfKill, SIGKILLs
  // itself after the last; WHATSAPP_TEST_KILL_AT places a kill at the
  // keeper's failpoint.
  async function child({ steps, killAt = '', selfKill = false }) {
    const url = (name) => JSON.stringify(new URL(name, SRC).href);
    const code = `
      import { writeFileSync } from 'node:fs';
      import { createClock } from ${url('clock.js')};
      import { createGatewayClient } from ${url('gateway.js')};
      import { createGcClient } from ${url('gc.js')};
      import { createKeeper } from ${url('keeper.js')};
      const env = process.env;
      const keeper = createKeeper({
        stateDir: env.KEEPER_STATE,
        clock: createClock(env),
        city: env.KEEPER_CITY,
        cityTz: env.KEEPER_TZ,
        gateway: createGatewayClient({ url: env.KEEPER_GW, credential: 'city-token' }),
        gc: createGcClient({ base: env.KEEPER_GC, city: env.KEEPER_CITY }),
      });
      for (const step of env.KEEPER_STEPS.split(',')) {
        writeFileSync(env.WHATSAPP_TEST_CLOCK, step);
        await keeper.tick();
      }
      if (env.KEEPER_SELF_KILL) process.kill(process.pid, 'SIGKILL');
    `;
    const env = {
      ...process.env,
      WHATSAPP_TEST_CLOCK: clockFile,
      WHATSAPP_TEST_KILL_AT: killAt,
      KEEPER_STATE: stateDir,
      KEEPER_CITY: CITY,
      KEEPER_TZ: tz,
      KEEPER_GW: gateway.url,
      KEEPER_GC: gcFake.url,
      KEEPER_STEPS: steps.join(','),
      KEEPER_SELF_KILL: selfKill ? '1' : '',
    };
    delete env.NODE_TEST_CONTEXT;
    const proc = spawn(process.execPath, ['--input-type=module', '-e', code], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let stderr = '';
    proc.stderr.on('data', (chunk) => { stderr += chunk; });
    proc.stdout.resume();
    const [exitCode, signal] = await once(proc, 'exit');
    return { exitCode, signal, stderr };
  }

  return {
    dir, stateDir, clock, setClock, gw, attempts, answers, logs, make, run, child,
    inbound: (id, last) => lastInbound.set(id, last),
    count: (id) => attempts.filter((a) => a.id === id).length,
    // The marks raised for one close, in order of first attempt.
    marks: (wa, close) => [...new Set(attempts.filter((a) => a.id.startsWith(`keeper:${wa}:${iso(close)}:`)).map((a) => a.id))]
      .map((id) => Number(id.split(':').pop())),
    updates: (wa) => attempts.filter((a) => a.id.startsWith(`keeper:${wa}:`) && a.id.endsWith(':update')),
  };
}

// Each expected event raised exactly once, at that wall time, with that text.
function assertFired(t, h, tz, expected) {
  for (const { id, wall, text } of expected) {
    const got = h.attempts.filter((a) => a.id === id);
    assert.equal(got.length, 1, `${id} raised exactly once (got ${got.length})`);
    t.diagnostic(`${id} fired ${local(got[0].at, tz)} ${tz}`);
    assert.equal(local(got[0].at, tz), local(at(wall), tz), `${id} fired at ${wall}`);
    if (text) assert.equal(got[0].message.text, text);
  }
}

function steps(from, to, step = MIN) {
  const out = [];
  for (let ms = from; ms <= to; ms += step) out.push(ms);
  return out;
}

// --- (10a) ---------------------------------------------------------------

test('(10a) no hours: the four nudges fire at the close minus 60, 30, 15 and 5 (06:58 07:28 07:43 07:53 for a close at 07:58), each once, is_bot true', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00'); // Tue 08:00 CST
  const close = last + DAY - MARGIN; // Wed 07:58 CST
  h.inbound(WA, last);
  h.setClock(last);
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T08:30:00-06:00'));

  t.diagnostic(`window_closes_at ${local(close, CHI)} ${CHI}`);
  assert.deepEqual(h.marks(WA, close), [60, 30, 15, 5]);
  assertFired(t, h, CHI, [
    { id: nudgeId(WA, close, 60), wall: '2026-11-11T06:58:00-06:00', text: plainText(WA, 60, '08:00') },
    { id: nudgeId(WA, close, 30), wall: '2026-11-11T07:28:00-06:00', text: plainText(WA, 30, '08:00') },
    { id: nudgeId(WA, close, 15), wall: '2026-11-11T07:43:00-06:00', text: plainText(WA, 15, '08:00') },
    { id: nudgeId(WA, close, 5), wall: '2026-11-11T07:53:00-06:00', text: plainText(WA, 5, '08:00') },
    { id: updateId(WA, '2026-11-10'), wall: '2026-11-10T10:00:00-06:00', text: openText(WA, '07:58') },
  ]);
  for (const a of h.attempts) {
    assert.deepEqual(a.message.actor, { id: 'keeper', display_name: 'Dynasty window-keeper', is_bot: true });
    assert.deepEqual(a.message.conversation, { scope_id: CITY, provider: 'whatsapp', account_id: 'dynasty', conversation_id: WA, kind: 'dm' });
    assert.equal(a.message.received_at, iso(a.at));
  }
  assert.equal(h.attempts.length, 5, 'four nudges and one update, nothing else');

  const row = keeper.status().conversations[WA];
  assert.equal(row.keeper, 'on');
  assert.equal(row.window_closes_at, iso(close));
  assert.deepEqual(row.marks_fired, [60, 30, 15, 5]);
  assert.equal(row.lapses_7d, 1, 'the close passed with nobody writing');
  const mode = fs.statSync(path.join(h.stateDir, 'keeper.json')).mode & 0o777;
  t.diagnostic(`keeper.json mode ${mode.toString(8)}`);
  assert.equal(mode, 0o600);
});

// --- (10b) ---------------------------------------------------------------

test('(10b) an inbound between the 60 and the 30 cancels the old close, empties the fired set, and the new close fires 06:08 06:38 06:53 07:03', async (t) => {
  // The minute read is throttled to 10 minutes here, so only the read owed
  // after the delivery sees the new close; the gateway answers epoch ms.
  const h = await harness(t, { tz: CHI, windowReadMs: 10 * MIN, format: 'ms' });
  const last0 = at('2026-11-10T08:00:00-06:00');
  const close0 = last0 + DAY - MARGIN; // Wed 07:58
  h.inbound(WA, last0);
  h.setClock(last0);
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T07:10:00-06:00'));
  assert.deepEqual(h.marks(WA, close0), [60]);
  assert.deepEqual(keeper.status().conversations[WA].marks_fired, [60]);

  // The owner writes at 07:10 Wed: the gateway's last_inbound_at moves.
  const last1 = at('2026-11-11T07:10:00-06:00');
  const close1 = last1 + DAY - MARGIN; // Thu 07:08
  h.inbound(WA, last1);
  const reads = h.gw.reads;
  await keeper.tick();
  assert.equal(h.gw.reads, reads, 'no read inside the interval without a delivery');
  keeper.noteDelivery();
  await keeper.tick();
  assert.equal(h.gw.reads, reads + 1, 'the delivery owes one read at the next tick');
  let row = keeper.status().conversations[WA];
  assert.equal(row.window_closes_at, iso(close1));
  assert.deepEqual(row.marks_fired, [], 'a new close empties the fired set');

  await h.run(keeper, at('2026-11-12T07:05:00-06:00'));
  assert.deepEqual(h.marks(WA, close0), [60], 'no 30, 15 or 5 for the old close');
  t.diagnostic(`new window_closes_at ${local(close1, CHI)} ${CHI}`);
  assertFired(t, h, CHI, [
    { id: nudgeId(WA, close1, 60), wall: '2026-11-12T06:08:00-06:00', text: plainText(WA, 60, '07:10') },
    { id: nudgeId(WA, close1, 30), wall: '2026-11-12T06:38:00-06:00', text: plainText(WA, 30, '07:10') },
    { id: nudgeId(WA, close1, 15), wall: '2026-11-12T06:53:00-06:00', text: plainText(WA, 15, '07:10') },
    { id: nudgeId(WA, close1, 5), wall: '2026-11-12T07:03:00-06:00', text: plainText(WA, 5, '07:10') },
    { id: updateId(WA, '2026-11-11'), wall: '2026-11-11T10:00:00-06:00', text: openText(WA, '07:08') },
  ]);
  row = keeper.status().conversations[WA];
  assert.deepEqual(row.marks_fired, [60, 30, 15, 5]);
  assert.equal(row.lapses_7d, 0, 'the old close was no lapse: the business wrote before it');
});

// --- (10c) ---------------------------------------------------------------

test('(10c) hours shift: close at 02:00 under Mon-Fri 09:00-18:00 fires 17:00 17:30 17:45 17:55 the day before, shifted text', async (t) => {
  const h = await harness(t, { tz: CHI }); // the city's zone differs: the recorded zone must win
  const last = at('2026-11-10T02:02:00-03:00'); // Tue 02:02 São Paulo
  const close = last + DAY - MARGIN; // Wed 02:00
  h.inbound(BR, last);
  h.setClock(last);
  const keeper = h.make();
  keeper.recordInvite('482913', { consent: true, hours: 'Mon-Fri 09:00-18:00', tz: SAO });
  keeper.bindInvite('482913', BR);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T03:00:00-03:00'));

  t.diagnostic(`window_closes_at ${local(close, SAO)} ${SAO}`);
  assert.deepEqual(h.marks(BR, close), [60, 30, 15, 5]);
  assertFired(t, h, SAO, [
    { id: nudgeId(BR, close, 60), wall: '2026-11-10T17:00:00-03:00', text: shiftedText(BR, '02:00', 60) },
    { id: nudgeId(BR, close, 30), wall: '2026-11-10T17:30:00-03:00', text: shiftedText(BR, '02:00', 30) },
    { id: nudgeId(BR, close, 15), wall: '2026-11-10T17:45:00-03:00', text: shiftedText(BR, '02:00', 15) },
    { id: nudgeId(BR, close, 5), wall: '2026-11-10T17:55:00-03:00', text: shiftedText(BR, '02:00', 5) },
    { id: updateId(BR, '2026-11-10'), wall: '2026-11-10T16:00:00-03:00', text: openText(BR, '02:00') },
  ]);
  assert.equal(h.attempts.length, 5);
  const row = keeper.status().conversations[BR];
  assert.equal(row.ladder, 'shifted');
  assert.equal(row.tz, SAO);
  assert.equal(row.hours, 'Mon-Fri 09:00-18:00');
  assert.equal(row.lapses_7d, 1);
});

test('(10c) hours unshifted: the owner wrote 09:30 Tue, close 09:28 Wed: the 60 and 30 drop, the 15 and 5 fire at 09:13 and 09:23', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T09:30:00-03:00');
  const close = last + DAY - MARGIN; // Wed 09:28
  h.inbound(BR, last);
  h.setClock(last);
  const keeper = h.make();
  keeper.recordInvite('100200', { consent: true, hours: 'Mon-Fri 09:00-18:00', tz: SAO });
  keeper.bindInvite('100200', BR);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T10:00:00-03:00'));

  t.diagnostic(`window_closes_at ${local(close, SAO)} ${SAO}; 08:28 and 08:58 fall before the 09:00 open`);
  assert.deepEqual(h.marks(BR, close), [15, 5]);
  assert.equal(h.count(nudgeId(BR, close, 60)), 0);
  assert.equal(h.count(nudgeId(BR, close, 30)), 0);
  assertFired(t, h, SAO, [
    { id: nudgeId(BR, close, 15), wall: '2026-11-11T09:13:00-03:00', text: plainText(BR, 15, '09:30') },
    { id: nudgeId(BR, close, 5), wall: '2026-11-11T09:23:00-03:00', text: plainText(BR, 5, '09:30') },
    { id: updateId(BR, '2026-11-10'), wall: '2026-11-10T16:00:00-03:00', text: openText(BR, '09:28') },
  ]);
  const row = keeper.status().conversations[BR];
  assert.equal(row.ladder, 'unshifted');
  assert.deepEqual(row.marks_planned.map((m) => m.mark), [15, 5], 'the out-of-span marks are dropped from the plan, not deferred');
  assert.deepEqual(row.marks_silent, []);
});

test('(10c) hours crossing midnight: 18:00-02:00 and a close at 03:00 fire 01:00 01:30 01:45 01:55', async (t) => {
  const h = await harness(t, { tz: CHI });
  const close = at('2026-11-11T03:00:00-03:00'); // Wed 03:00
  const last = close - DAY + MARGIN; // Tue 03:02
  h.inbound(BR, last);
  h.setClock(last);
  const keeper = h.make();
  keeper.setHours(BR, '18:00-02:00', SAO);
  keeper.setConsent(BR, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T04:00:00-03:00'));

  t.diagnostic(`window_closes_at ${local(close, SAO)} ${SAO}; close of business 02:00`);
  assert.deepEqual(h.marks(BR, close), [60, 30, 15, 5]);
  assertFired(t, h, SAO, [
    { id: nudgeId(BR, close, 60), wall: '2026-11-11T01:00:00-03:00', text: shiftedText(BR, '03:00', 60) },
    { id: nudgeId(BR, close, 30), wall: '2026-11-11T01:30:00-03:00', text: shiftedText(BR, '03:00', 30) },
    { id: nudgeId(BR, close, 15), wall: '2026-11-11T01:45:00-03:00', text: shiftedText(BR, '03:00', 15) },
    { id: nudgeId(BR, close, 5), wall: '2026-11-11T01:55:00-03:00', text: shiftedText(BR, '03:00', 5) },
    // The business day is the date its span opens: Tuesday's 18:00-02:00
    // span closes 02:00 Wed, so Tuesday's update is raised 00:00 Wed.
    { id: updateId(BR, '2026-11-10'), wall: '2026-11-11T00:00:00-03:00', text: openText(BR, '03:00') },
  ]);
  assert.equal(h.attempts.length, 5);
});

test('(10c) hours, no span before the close: a close on Saturday under Mon-Fri 09:00-18:00 raises no mark and counts one lapse', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: 10 * MIN });
  const last = at('2026-11-13T20:00:00-03:00'); // Fri 20:00, after hours
  const close = last + DAY - MARGIN; // Sat 19:58
  h.inbound(BR, last);
  h.setClock(last);
  const keeper = h.make();
  keeper.recordInvite('777000', { consent: true, hours: 'Mon-Fri 09:00-18:00', tz: SAO });
  keeper.bindInvite('777000', BR);
  await keeper.tick();
  await h.run(keeper, close - MIN);
  let row = keeper.status().conversations[BR];
  assert.equal(row.ladder, 'lapse');
  assert.deepEqual(row.marks_planned, []);
  assert.equal(row.lapses_7d, 0, 'not a lapse before the close passes');
  await h.run(keeper, at('2026-11-16T00:00:00-03:00'), 5 * MIN); // through the weekend
  row = keeper.status().conversations[BR];
  t.diagnostic(`window_closes_at ${local(close, SAO)} ${SAO}; events ${h.attempts.length}; lapses ${row.lapses_7d}`);
  assert.equal(h.attempts.length, 0, 'no mark, and no update on a day with no span');
  assert.equal(row.lapses_7d, 1);
});

test('(10c) quiet rule: the business wrote 16:20 Fri after the 16:00 update, so no ladder mark that day', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last0 = at('2026-11-12T17:00:00-03:00'); // Thu 17:00
  const close0 = last0 + DAY - MARGIN; // Fri 16:58, inside the span
  h.inbound(BR, last0);
  h.setClock(at('2026-11-13T15:00:00-03:00'));
  const keeper = h.make();
  keeper.setHours(BR, 'Mon-Fri 09:00-18:00', SAO);
  keeper.setConsent(BR, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-13T16:20:00-03:00'));
  assertFired(t, h, SAO, [
    { id: nudgeId(BR, close0, 60), wall: '2026-11-13T15:58:00-03:00', text: plainText(BR, 60, '17:00') },
    { id: updateId(BR, '2026-11-13'), wall: '2026-11-13T16:00:00-03:00', text: openText(BR, '16:58') },
  ]);

  const last1 = at('2026-11-13T16:20:00-03:00');
  const close1 = last1 + DAY - MARGIN; // Sat 16:18: after hours, shifted to Fri 18:00
  h.inbound(BR, last1);
  keeper.noteDelivery();
  await keeper.tick();
  await h.run(keeper, at('2026-11-14T17:00:00-03:00'));
  const row = keeper.status().conversations[BR];
  t.diagnostic(`new close ${local(close1, SAO)}; ladder ${row.ladder} at ${row.marks_planned.map((m) => local(Date.parse(m.at), SAO)).join(', ')}; silent ${JSON.stringify(row.marks_silent)}`);
  assert.equal(row.ladder, 'shifted');
  assert.deepEqual(h.marks(BR, close1), [], 'no ladder on the day the business wrote after the update');
  assert.deepEqual(row.marks_silent.map((m) => m.why), ['quiet', 'quiet', 'quiet', 'quiet']);
  assert.equal(h.attempts.length, 2);
  assert.equal(row.lapses_7d, 1, 'the Saturday close lapsed, by design');
});

// --- (10d) ---------------------------------------------------------------

test('(10d) no nudge after the close: one lapse, the next update says "Window closed since 07:58", a refused nudge is counted, no template sent', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN; // Wed 07:58
  h.inbound(WA, last);
  h.setClock(last);
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, close);
  assert.deepEqual(h.marks(WA, close), [60, 30, 15, 5]);
  const before = h.attempts.length;

  // The mayor's 5-minute nudge reaches the gateway after the close: refused window_closed.
  keeper.noteRefusedNudge(WA);
  await h.run(keeper, at('2026-11-11T09:59:00-06:00'));
  assert.equal(h.attempts.length, before, 'nothing raised between the close and the next update');
  let row = keeper.status().conversations[WA];
  assert.equal(row.window, 'closed');
  assert.equal(row.lapses_7d, 1);
  assert.equal(row.refused_nudges_7d, 1);

  await h.run(keeper, at('2026-11-11T23:00:00-06:00'));
  assertFired(t, h, CHI, [
    { id: updateId(WA, '2026-11-11'), wall: '2026-11-11T10:00:00-06:00', text: closedText(WA, '07:58') },
  ]);
  row = keeper.status().conversations[WA];
  assert.equal(row.lapses_7d, 1, 'a lapse counted once per close');
  assert.equal(h.gw.sends, 0, 'the keeper never sends: no template');
  assert.equal(h.attempts.length, before + 1);
});

test('(10d) a keeper down across the close raises no overdue nudge after it and counts one lapse', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN;
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:00:00-06:00'));
  const first = h.make();
  first.setConsent(WA, true);
  await first.tick();
  assert.deepEqual(first.status().conversations[WA].marks_planned.map((m) => m.mark), [60, 30, 15, 5]);
  // Down from 06:00 to 08:30, across every mark and the close.
  h.setClock(at('2026-11-11T08:30:00-06:00'));
  const second = h.make();
  await second.tick();
  await h.run(second, at('2026-11-11T09:30:00-06:00'));
  t.diagnostic(`back at 08:30 ${CHI} after the 07:58 close: events ${h.attempts.length}`);
  assert.equal(h.attempts.length, 0);
  assert.equal(second.status().conversations[WA].lapses_7d, 1);
});

// --- (10e) ---------------------------------------------------------------

test('(10e) no hours: the daily update fires once per local day at 10:00 in the recorded zone, else the city\'s', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: HOUR });
  h.inbound(WA, at('2026-11-09T12:00:00-06:00')); // closes Tue 11:58 CST
  h.inbound(BR, at('2026-11-09T15:00:00-03:00')); // closes Tue 14:58 São Paulo
  h.setClock(at('2026-11-10T00:00:00-06:00'));
  const keeper = h.make();
  keeper.setConsent(WA, true); // no zone recorded: the city's (Chicago)
  keeper.setHours(BR, '', SAO); // a zone alone, no hours
  keeper.setConsent(BR, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-12T00:00:00-06:00'));

  assert.equal(h.updates(WA).length, 2);
  assert.equal(h.updates(BR).length, 2);
  assertFired(t, h, CHI, [
    { id: updateId(WA, '2026-11-10'), wall: '2026-11-10T10:00:00-06:00', text: openText(WA, '11:58') },
    { id: updateId(WA, '2026-11-11'), wall: '2026-11-11T10:00:00-06:00', text: closedText(WA, '11:58') },
  ]);
  assertFired(t, h, SAO, [
    { id: updateId(BR, '2026-11-10'), wall: '2026-11-10T10:00:00-03:00', text: openText(BR, '14:58') },
    { id: updateId(BR, '2026-11-11'), wall: '2026-11-11T10:00:00-03:00', text: closedText(BR, '14:58') },
  ]);
});

test('(10e) hours: the daily update fires two hours before the close of business (16:00 under 09:00-18:00), clamped inside a short span, none on Sunday', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: HOUR });
  h.inbound(BR, at('2026-11-12T12:00:00-03:00')); // closes Fri 11:58
  h.setClock(at('2026-11-13T00:00:00-03:00'));
  const keeper = h.make();
  keeper.setHours(BR, 'Mon-Fri 09:00-18:00, Sat 10:00-11:00', SAO);
  keeper.setConsent(BR, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-16T00:00:00-03:00'));
  assert.equal(h.updates(BR).length, 2, 'Friday and Saturday, nothing Sunday');
  assertFired(t, h, SAO, [
    { id: updateId(BR, '2026-11-13'), wall: '2026-11-13T16:00:00-03:00', text: closedText(BR, '11:58') },
    { id: updateId(BR, '2026-11-14'), wall: '2026-11-14T10:00:00-03:00', text: closedText(BR, '11:58') },
  ]);
});

test('(10e) a SIGKILL between the update\'s state write and its post, then a restart the same day: the update is never raised twice', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: HOUR });
  h.inbound(WA, at('2026-11-09T12:00:00-06:00'));
  h.setClock(at('2026-11-10T09:00:00-06:00'));
  h.make().setConsent(WA, true);
  const id = updateId(WA, '2026-11-10');
  const killed = await h.child({
    steps: [at('2026-11-10T09:59:00-06:00'), at('2026-11-10T10:00:00-06:00')],
    killAt: `keeper-after-state@${id}`,
  });
  t.diagnostic(`child exit ${killed.exitCode} signal ${killed.signal} ${killed.stderr.trim()}`);
  assert.equal(killed.signal, 'SIGKILL');
  assert.equal(h.count(id), 0, 'killed after the write, before the post');
  const onDisk = JSON.parse(fs.readFileSync(path.join(h.stateDir, 'keeper.json'), 'utf8'));
  assert.equal(onDisk.conversations[WA].update_day, '2026-11-10');

  const keeper = h.make();
  await keeper.tick();
  await h.run(keeper, at('2026-11-10T23:59:00-06:00'));
  assert.equal(h.count(id), 0, 'the lost update is not raised again after the restart');
  await h.run(keeper, at('2026-11-11T12:00:00-06:00'));
  assertFired(t, h, CHI, [
    { id: updateId(WA, '2026-11-11'), wall: '2026-11-11T10:00:00-06:00', text: closedText(WA, '11:58') },
  ]);
});

test('(10e) the update posted, then a SIGKILL and a restart the same day: not raised again', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: HOUR });
  h.inbound(WA, at('2026-11-09T12:00:00-06:00'));
  h.setClock(at('2026-11-10T09:00:00-06:00'));
  h.make().setConsent(WA, true);
  const id = updateId(WA, '2026-11-10');
  const killed = await h.child({ steps: steps(at('2026-11-10T09:58:00-06:00'), at('2026-11-10T10:02:00-06:00')), selfKill: true });
  assert.equal(killed.signal, 'SIGKILL');
  assertFired(t, h, CHI, [{ id, wall: '2026-11-10T10:00:00-06:00', text: openText(WA, '11:58') }]);
  const keeper = h.make();
  await keeper.tick();
  await h.run(keeper, at('2026-11-10T23:59:00-06:00'));
  assert.equal(h.count(id), 1);
});

// --- (10f) ---------------------------------------------------------------

test('(10f) a SIGKILL between the 30 and the 15 marks and a restart: the 15 and the 5 fire, the 60 and the 30 do not fire again', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN; // Wed 07:58
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:50:00-06:00'));
  h.make().setConsent(WA, true);
  const killed = await h.child({ steps: steps(at('2026-11-11T06:50:00-06:00'), at('2026-11-11T07:35:00-06:00')), selfKill: true });
  t.diagnostic(`child killed at ${local(h.clock(), CHI)} ${CHI}: signal ${killed.signal}`);
  assert.equal(killed.signal, 'SIGKILL');
  assert.deepEqual(h.marks(WA, close), [60, 30]);

  const keeper = h.make();
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T08:10:00-06:00'));
  assertFired(t, h, CHI, [
    { id: nudgeId(WA, close, 60), wall: '2026-11-11T06:58:00-06:00' },
    { id: nudgeId(WA, close, 30), wall: '2026-11-11T07:28:00-06:00' },
    { id: nudgeId(WA, close, 15), wall: '2026-11-11T07:43:00-06:00', text: plainText(WA, 15, '08:00') },
    { id: nudgeId(WA, close, 5), wall: '2026-11-11T07:53:00-06:00', text: plainText(WA, 5, '08:00') },
  ]);
  assert.equal(h.attempts.length, 4);
});

test('(10f) a SIGKILL between the 30\'s state write and its post: the 30 is lost and never raised twice; the 15 and the 5 fire after the restart', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN;
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:50:00-06:00'));
  h.make().setConsent(WA, true);
  const killed = await h.child({
    steps: steps(at('2026-11-11T06:50:00-06:00'), at('2026-11-11T07:35:00-06:00')),
    killAt: `keeper-after-state@${nudgeId(WA, close, 30)}`,
  });
  t.diagnostic(`child killed at ${local(h.clock(), CHI)} ${CHI}: signal ${killed.signal}`);
  assert.equal(killed.signal, 'SIGKILL');
  assert.deepEqual(h.marks(WA, close), [60]);

  const keeper = h.make();
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T08:10:00-06:00'));
  assert.equal(h.count(nudgeId(WA, close, 30)), 0, 'lost once, never raised twice');
  assert.deepEqual(h.marks(WA, close), [60, 15, 5]);
  assertFired(t, h, CHI, [
    { id: nudgeId(WA, close, 15), wall: '2026-11-11T07:43:00-06:00' },
    { id: nudgeId(WA, close, 5), wall: '2026-11-11T07:53:00-06:00' },
  ]);
});

test('(10f) a keeper down across the 60, 30 and 15 marks fires only the latest overdue one on restart (with the true minutes left), then the 5', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN; // Wed 07:58
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:50:00-06:00'));
  const first = h.make();
  first.setConsent(WA, true);
  await first.tick();
  // Down from 06:50 to 07:50: the 60, 30 and 15 are all overdue.
  h.setClock(at('2026-11-11T07:50:00-06:00'));
  const keeper = h.make();
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T08:00:00-06:00'));
  assert.deepEqual(h.marks(WA, close), [15, 5]);
  assertFired(t, h, CHI, [
    { id: nudgeId(WA, close, 15), wall: '2026-11-11T07:50:00-06:00', text: plainText(WA, 8, '08:00') },
    { id: nudgeId(WA, close, 5), wall: '2026-11-11T07:53:00-06:00', text: plainText(WA, 5, '08:00') },
  ]);
  const row = keeper.status().conversations[WA];
  assert.deepEqual(row.marks_fired, [15, 5]);
  assert.deepEqual(row.marks_silent, [{ mark: 60, why: 'overdue' }, { mark: 30, why: 'overdue' }]);
});

test('(10b) a failed window read after a delivery is logged, never throws, is retried at the next tick, and holds the ladder until the clock is fresh', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last0 = at('2026-11-10T08:00:00-06:00');
  const close0 = last0 + DAY - MARGIN; // Wed 07:58
  h.inbound(WA, last0);
  h.setClock(at('2026-11-11T06:50:00-06:00'));
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T06:57:30-06:00'), 30 * SEC);

  // The owner writes at 06:57:30; the gateway then fails every read.
  const last1 = at('2026-11-11T06:57:30-06:00');
  h.inbound(WA, last1);
  h.gw.failReads = true;
  keeper.noteDelivery();
  const reads = h.gw.reads;
  await h.run(keeper, at('2026-11-11T06:58:30-06:00'), 250); // across the old 60 mark
  t.diagnostic(`reads while failing: ${h.gw.reads - reads}; log: ${h.logs.find((l) => l.includes('window read failed'))}`);
  assert.equal(h.gw.reads - reads, 240, 'one read per tick (60 s of 250 ms ticks) while the owed read fails');
  assert.ok(h.logs.some((l) => l.includes('window read failed')));
  assert.equal(h.count(nudgeId(WA, close0, 60)), 0, 'no nudge on a stale clock');
  assert.equal(keeper.status().window_read.error !== null, true);

  h.gw.failReads = false;
  await h.run(keeper, at('2026-11-11T08:30:00-06:00'));
  const row = keeper.status().conversations[WA];
  assert.equal(row.window_closes_at, iso(last1 + DAY - MARGIN));
  assert.deepEqual(h.marks(WA, close0), [], 'the old close raised nothing');
  assert.equal(row.lapses_7d, 0);
});

// --- (10g) ---------------------------------------------------------------

test('(10g) without recorded consent: no event at all through a whole window and the next day\'s update hour', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: 10 * MIN });
  const last = at('2026-11-10T08:00:00-06:00');
  h.inbound(WA, last); // bound, nothing recorded
  h.inbound(BR, last); // invited without --proactive-consent
  h.setClock(last);
  const keeper = h.make();
  keeper.recordInvite('313131', { consent: false, hours: '', tz: '' });
  keeper.bindInvite('313131', BR);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T11:00:00-06:00'));
  const status = keeper.status().conversations;
  t.diagnostic(`events ${h.attempts.length}; keeper ${status[WA].keeper}/${status[BR].keeper}`);
  assert.equal(h.attempts.length, 0);
  assert.equal(status[WA].keeper, 'off');
  assert.equal(status[BR].keeper, 'off');
  assert.equal(status[BR].consent, false);
  assert.equal(status[WA].consent, null);
});

test('(10g) consent --off mid-window stops the remaining marks; --on resumes from the next mark', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN;
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:00:00-06:00'));
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T07:00:00-06:00'));
  assert.deepEqual(h.marks(WA, close), [60]);
  keeper.setConsent(WA, false);
  await h.run(keeper, at('2026-11-11T07:35:00-06:00'));
  assert.deepEqual(h.marks(WA, close), [60], 'the 30 at 07:28 does not fire while off');
  keeper.setConsent(WA, true);
  await h.run(keeper, at('2026-11-11T08:10:00-06:00'));
  assert.equal(h.count(nudgeId(WA, close, 30)), 0, 'the 30 passed while off: --on resumes from the next');
  assertFired(t, h, CHI, [
    { id: nudgeId(WA, close, 60), wall: '2026-11-11T06:58:00-06:00' },
    { id: nudgeId(WA, close, 15), wall: '2026-11-11T07:43:00-06:00' },
    { id: nudgeId(WA, close, 5), wall: '2026-11-11T07:53:00-06:00' },
  ]);
  assert.deepEqual(keeper.status().conversations[WA].marks_silent, [{ mark: 30, why: 'before consent' }]);
});

// --- (10h) ---------------------------------------------------------------

test('(10h) a keeper event answered 500 is retried on the ladder in clock time (+1 s, +5 s) until delivered', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN;
  const id = nudgeId(WA, close, 60);
  h.answers.set(id, [500, 500, 'delivered']);
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:50:00-06:00'));
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T06:58:00-06:00'));
  await h.run(keeper, at('2026-11-11T06:58:30-06:00'), 250);
  const times = h.attempts.filter((a) => a.id === id).map((a) => a.at);
  t.diagnostic(`attempts at ${times.map((ms) => local(ms, CHI)).join(', ')} ${CHI}`);
  const t0 = at('2026-11-11T06:58:00-06:00');
  assert.deepEqual(times, [t0, t0 + 1 * SEC, t0 + 6 * SEC]);
  const row = keeper.status().conversations[WA];
  assert.equal(row.undelivered_7d, 0);
  assert.deepEqual(row.marks_fired, [60]);
});

test('(10h) an event answered no_route, answered 400 or dropped with no answer is counted on the keeper row and never re-raised for that mark', async (t) => {
  const h = await harness(t, { tz: CHI });
  const last = at('2026-11-10T08:00:00-06:00');
  const close = last + DAY - MARGIN;
  h.answers.set(nudgeId(WA, close, 60), ['no_route']);
  h.answers.set(nudgeId(WA, close, 30), ['drop']);
  h.answers.set(nudgeId(WA, close, 15), [400]);
  h.inbound(WA, last);
  h.setClock(at('2026-11-11T06:50:00-06:00'));
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-11T08:10:00-06:00'), 15 * SEC);
  for (const mark of [60, 30, 15, 5]) assert.equal(h.count(nudgeId(WA, close, mark)), 1, `mark ${mark} raised once`);
  const row = keeper.status().conversations[WA];
  t.diagnostic(`undelivered (7 d) ${row.undelivered_7d}`);
  assert.equal(row.undelivered_7d, 3);
  assert.deepEqual(row.marks_fired, [60, 30, 15, 5]);
});

test('(10h) a keeper event answered 500 every time is dropped after the ladder (1, 5, 15, 60 s) and counted', async (t) => {
  const h = await harness(t, { tz: CHI, windowReadMs: HOUR });
  const id = updateId(WA, '2026-11-10');
  h.answers.set(id, [500, 500, 500, 500, 500, 500, 500]);
  h.inbound(WA, at('2026-11-09T12:00:00-06:00'));
  h.setClock(at('2026-11-10T09:59:00-06:00'));
  const keeper = h.make();
  keeper.setConsent(WA, true);
  await keeper.tick();
  await h.run(keeper, at('2026-11-10T10:05:00-06:00'), SEC);
  const times = h.attempts.filter((a) => a.id === id).map((a) => a.at);
  t.diagnostic(`attempts at ${times.map((ms) => local(ms, CHI)).join(', ')} ${CHI}`);
  const t0 = at('2026-11-10T10:00:00-06:00');
  assert.deepEqual(times, [t0, t0 + 1 * SEC, t0 + 6 * SEC, t0 + 21 * SEC, t0 + 81 * SEC]);
  assert.equal(keeper.status().conversations[WA].undelivered_7d, 1);
});

// --- zoned.js and the onboarding fields -------------------------------------

test('(zoned) business hours parse: day ranges, lists, midnight crossing, inherited days', () => {
  assert.deepEqual(parseHours('Mon-Fri 09:00-18:00, Sat 10:00-14:00'), [
    { days: [1, 2, 3, 4, 5], open: 540, close: 1080 },
    { days: [6], open: 600, close: 840 },
  ]);
  assert.deepEqual(parseHours('18:00-02:00'), [{ days: [0, 1, 2, 3, 4, 5, 6], open: 1080, close: 120 }]);
  assert.deepEqual(parseHours('Mon-Fri 09:00-12:00, 13:00-18:00'), [
    { days: [1, 2, 3, 4, 5], open: 540, close: 720 },
    { days: [1, 2, 3, 4, 5], open: 780, close: 1080 },
  ]);
  assert.deepEqual(parseHours('fri-mon 20:00-24:00'), [{ days: [0, 1, 5, 6], open: 1200, close: 1440 }]);
  assert.deepEqual(parseHours('Tuesday 9:30-17:00'), [{ days: [2], open: 570, close: 1020 }]);
});

test('(zoned) bad business hours and bad zones are refused with a clear message', async (t) => {
  for (const bad of ['', 'Mon-Fri', 'Mon-Fri 9-18', 'Funday 09:00-10:00', 'Mon 09:00-09:00', 'Mon 25:00-26:00', 'Mon 09:00-18:60']) {
    assert.throws(() => parseHours(bad), /hours/, `refuses ${JSON.stringify(bad)}`);
  }
  assert.throws(() => assertZone('Mars/Olympus_Mons'), /time zone/);
  assert.equal(assertZone(SAO), SAO);
  const h = await harness(t);
  h.setClock(at('2026-11-10T08:00:00-06:00'));
  const keeper = h.make();
  assert.throws(() => keeper.setHours(WA, 'weekdays 9 to 5', SAO), /hours/);
  assert.throws(() => keeper.setHours(WA, 'Mon-Fri 09:00-18:00', 'Mars/Olympus_Mons'), /time zone/);
  assert.throws(() => keeper.recordInvite('1', { consent: true, hours: 'Mon-Fri 09:00-18:00', tz: 'Nowhere/City' }), /time zone/);
  assert.throws(() => keeper.validateHours('Mon-Fri 9-18', SAO), /hours/);
  keeper.validateHours('Mon-Fri 09:00-18:00', SAO);
  assert.equal(keeper.status().conversations[WA], undefined, 'a refused change records nothing');
});

test('(zoned) a wall time to an instant: plain, a DST gap moves forward, a DST fold takes the earlier', (t) => {
  const cases = [
    [SAO, { year: 2026, month: 11, day: 10 }, 17 * 60, '2026-11-10T20:00:00.000Z'],
    [CHI, { year: 2026, month: 11, day: 10 }, 10 * 60, '2026-11-10T16:00:00.000Z'],
    [CHI, { year: 2026, month: 3, day: 8 }, 2 * 60 + 30, '2026-03-08T08:30:00.000Z'], // 02:30 never happens: 03:30 CDT
    [CHI, { year: 2026, month: 11, day: 1 }, 60 + 30, '2026-11-01T06:30:00.000Z'], // 01:30 twice: the CDT one
    [SAO, { year: 2026, month: 11, day: 10 }, 26 * 60, '2026-11-11T05:00:00.000Z'], // 02:00 the next day
  ];
  for (const [tz, date, minutes, want] of cases) {
    const got = iso(zonedToInstant(date, minutes, tz));
    t.diagnostic(`${tz} ${JSON.stringify(date)} +${minutes} min -> ${got}`);
    assert.equal(got, want);
  }
});
