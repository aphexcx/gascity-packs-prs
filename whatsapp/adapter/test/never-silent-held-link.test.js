import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { rig, adapterRequest, waitFor, stays, rid } from './fakes.js';
import { FAILURE_WAIT_MS, OWNER_LINES } from '../src/never-silent.js';

const TICK_MS = 250;
const HELD_POLL_MS = 300000;

async function setup(t, held = true) {
  const r = await rig();
  const mail = path.join(r.dir, 'mail');
  fs.mkdirSync(mail);
  const bin = path.join(r.dir, 'fake-gc');
  fs.writeFileSync(bin, `#!${process.execPath}\nconst fs = require('fs');
const file = ${JSON.stringify(mail)} + '/mail-' + process.pid;
fs.writeFileSync(file + '.tmp', JSON.stringify(process.argv.slice(2)));
fs.renameSync(file + '.tmp', file + '.json');\n`, { mode: 0o755 });
  r.remote = () => fs.readdirSync(mail).filter((file) => file.endsWith('.json'))
    .map((file) => JSON.parse(fs.readFileSync(path.join(mail, file), 'utf8'))).filter((args) => args.includes('--context'));
  r.state = () => {
    const file = path.join(r.dir, '.gc', 'whatsapp', 'never-silent.json');
    return fs.existsSync(file) ? JSON.parse(fs.readFileSync(file, 'utf8')) : { messages: {}, outages: [] };
  };
  t.after(async () => {
    // Do not SIGKILL the adapter while a fake mail child still owns a fixture file.
    try {
      await waitFor(() => r.state().outages.every((o) => o.localMail?.sent)
        && Object.values(r.state().owners ?? {}).every((o) => !o.posted || o.localMail?.sent),
      { what: 'local fake mail completion before cleanup' });
    } finally { await r.close(); }
  });
  r.status = async () => (await adapterRequest(r.socket, 'GET', '/status')).json;
  r.inboxReads = () => r.gw.requests.filter((req) => req.route === 'GET /v1/inbox').length;
  r.gc.policy = () => ({ verdict: 'pending', terminal: true, deliveredBytes: 0 });
  r.start({ NEVER_SILENT: 'on', NEVER_SILENT_OPERATOR_CONTEXT: 'example-operator', GC_BIN: bin,
    WHATSAPP_HELD_CONNECTION: held ? 'on' : 'off', WHATSAPP_POLL_MS: '2000',
    WHATSAPP_TICK_MS: String(TICK_MS), WHATSAPP_GATEWAY_TIMEOUT_MS: '5000' });
  await waitFor(async () => {
    try { const s = await r.status(); return s.inbound.link.state === (held ? 'up' : 'disabled') && s.inbound.polls > 0; }
    catch { return false; }
  }, { what: 'initial inbox read and link state' });
  // Drain the connect-triggered read before placing a message in the fake inbox.
  await stays(() => r.gc.pastes.length === 0, { forMs: 2 * TICK_MS });
  return r;
}

async function admit(r, name, held = true) {
  r.gw.addText('example-owner', name, 'invented request');
  if (held) r.gw.link.wake();
  else r.clock.advance(2000);
  await waitFor(() => r.state().messages[rid(name)]?.stopped !== undefined, { what: 'receipt result and terminal stop' });
  await waitFor(() => r.remote().length === 1, { what: 'immediate outage mail' });
  return r.state().messages[rid(name)];
}

async function lineAtBound(r, message) {
  r.clock.set(message.stopped + FAILURE_WAIT_MS - TICK_MS);
  await stays(() => r.gw.sends.length === 0, { forMs: 2 * TICK_MS, what: 'owner grace before bound' });
  r.clock.advance(TICK_MS);
  await waitFor(() => r.gw.sends.length === 1, { what: 'owner line at bound' });
  await waitFor(() => r.remote().length === 1, { what: 'outage mail at bound' });
  assert.equal(r.gw.sends[0].text, OWNER_LINES.not_delivered);
  assert.equal(r.state().owners['example-owner'].at, message.stopped + FAILURE_WAIT_MS);
  assert.equal(r.remote().length, 1);
}

test('A: notice clock and immediate mail do not wait for the held inbox poll', async (t) => {
  const r = await setup(t);
  const m = await admit(r, 'example-clock');
  assert.equal(r.state().outages[0].start, m.stopped);
  const reads = r.inboxReads();
  assert.ok(reads > 0);
  await lineAtBound(r, m);
  assert.equal(r.inboxReads(), reads, 'three-minute notice must precede the five-minute inbox read');
  assert.equal((await r.status()).inbound.link.state, 'up');
  t.diagnostic('pin: mail at stop; line at +180000 ms; no additional inbox read; tick 250 ms');
});

test('A: continuous failed receipts notify at three minutes without an inbox read', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'failed', deliveredBytes: 0 });
  r.gw.addText('example-owner', 'example-failed', 'invented request');
  r.gw.link.wake();
  await waitFor(() => r.state().messages[rid('example-failed')]?.failureSince !== undefined);
  const m = r.state().messages[rid('example-failed')];
  assert.equal(m.failureKind, 'failed');
  const reads = r.inboxReads();
  assert.equal(r.remote().length, 0);
  await lineAtBound(r, { stopped: m.failureSince });
  await waitFor(() => r.remote().length === 1);
  assert.equal(r.inboxReads(), reads);
  assert.equal(r.state().messages[rid('example-failed')].stopped, undefined);
});

test('B: wake and poll carry the same admission and receipt result into notices', async (t) => {
  for (const held of [true, false]) await t.test(held ? 'wake' : 'poll', async (t) => {
    const r = await setup(t, held);
    const m = await admit(r, 'example-receipt', held);
    assert.equal(m.failureKind, undefined, 'pending is not a definite failure');
    assert.deepEqual(m.targets, ['s-mayor']);
    assert.equal(m.created, r.clock.now());
    assert.equal(m.observed, m.created);
    assert.equal(m.stopped, m.created);
    assert.equal(r.gc.pastes.length, 1);
    assert.equal((await r.status()).inbound.link.wakes, held ? 1 : 0);
    await lineAtBound(r, m);
    await stays(() => r.gw.sends.length === 1 && r.remote().length === 1);
    assert.equal(r.gc.outbound.length, 0, 'notice is not session words');
  });
});

test('C: dropping and reconnecting a held link preserves the outage and notice marks', async (t) => {
  const r = await setup(t);
  const m = await admit(r, 'example-reconnect');
  await lineAtBound(r, m);
  const outage = r.state().outages[0].id;
  r.gw.link.enabled = false;
  r.gw.link.drop();
  await waitFor(async () => (await r.status()).inbound.link.state === 'down');
  const reads = r.inboxReads();
  r.clock.advance(2000);
  await waitFor(() => r.inboxReads() > reads, { what: 'ordinary two-second fallback read' });
  r.gw.link.enabled = true;
  r.clock.advance(5000);
  await waitFor(async () => (await r.status()).inbound.link.state === 'up');
  r.gw.link.wake();
  await stays(() => r.gw.sends.length === 1 && r.remote().length === 1, { forMs: 2 * TICK_MS });
  assert.equal(r.state().outages.length, 1);
  assert.equal(r.state().outages[0].id, outage);
  assert.equal(r.state().outages[0].end, undefined, 'a reconnect is not a delivered message');
  assert.equal(r.remote().filter((args) => args.some((a) => a.endsWith(' cleared'))).length, 0);
});

test('D: a lost wake starts no outage before arrival and uses the adapter arrival clock', async (t) => {
  const r = await setup(t);
  const sent = r.clock.now();
  r.gw.addText('example-owner', 'example-late', 'invented late request');
  const reads = r.inboxReads();
  r.clock.advance(HELD_POLL_MS - TICK_MS);
  await stays(() => Object.keys(r.state().messages).length === 0 && r.remote().length === 0 && r.gw.sends.length === 0,
    { forMs: 2 * TICK_MS, what: 'no notices before the safety read' });
  assert.equal(r.inboxReads(), reads);
  assert.equal((await r.status()).inbound.link.state, 'up');
  r.clock.advance(TICK_MS);
  await waitFor(() => r.state().messages[rid('example-late')]?.stopped !== undefined);
  await waitFor(() => r.remote().length === 1);
  const m = r.state().messages[rid('example-late')];
  assert.equal(m.created, sent + HELD_POLL_MS);
  assert.equal(m.observed, m.created);
  assert.equal(m.stopped, m.created);
  assert.equal(r.inboxReads(), reads + 1);
  await lineAtBound(r, m);
  t.diagnostic('pin: provider sent +0; adapter admitted +300000; first owner line +480000 ms');
});
