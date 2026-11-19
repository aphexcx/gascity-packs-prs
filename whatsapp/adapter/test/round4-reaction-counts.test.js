// PR 104 round 4, R2 (WhatsApp): with ACK_REACTION and NEVER_SILENT both on,
// a session's reaction counts for never-silent exactly as it would with the
// acknowledgement off. On WhatsApp the acknowledgement goes to the gateway
// under its own key (src/arrival.js) and the react verb under the reaction's
// key; the gateway answers each send with a new message id and keeps no
// record of which emoji is on a message, so the session's reaction of the
// acknowledged emoji is a send of its own, and main counts every delivered
// reaction (react.test.js). These tests pin that with the switch on.
//
// P2: the react verb writes the session's reaction to the stopwatch, as the
// Google Chat and Slack react verbs do; a removal writes nothing.
//
// The real adapter process against the fakes, as both-switches.test.js drives
// it; GC_BIN is a fake gc that only records its arguments. A second owner's
// unanswered message is the canary: its owner line shows that never-silent
// polled past the three minutes.

import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { rig, waitFor, adapterRequest } from './fakes.js';
import { OWNER_LINES } from '../src/never-silent.js';

const OWNER = 'example-owner';
const CANARY = 'example-unanswered';
const MESSAGE = 'example-addressed';
const EYES = '\u{1F440}';

async function setup(t, ack = 'eyes') {
  const r = await rig();
  t.after(() => r.close());
  const bin = path.join(r.dir, 'fake-gc');
  const mail = path.join(r.dir, 'mail.jsonl');
  fs.writeFileSync(bin, `#!${process.execPath}\nrequire('fs').appendFileSync(${JSON.stringify(mail)}, JSON.stringify(process.argv.slice(2))+'\\n');\n`, { mode: 0o755 });
  r.options = { NEVER_SILENT: 'on', NEVER_SILENT_OPERATOR_CONTEXT: 'example-operator', GC_BIN: bin, ACK_REACTION: ack };
  r.health = (state) => {
    fs.mkdirSync(path.join(r.dir, '.gc'), { recursive: true });
    const now = new Date(r.clock.now()).toISOString();
    fs.writeFileSync(path.join(r.dir, '.gc', 'mayor-health.json'), JSON.stringify({ state, since: now, checked_at: now }));
  };
  r.stateDir = path.join(r.dir, '.gc', 'whatsapp');
  r.stopwatch = () => fs.readdirSync(r.stateDir).filter((f) => /^stopwatch-\d{4}-\d{2}\.jsonl$/.test(f))
    .flatMap((f) => fs.readFileSync(path.join(r.stateDir, f), 'utf8').split('\n').filter(Boolean).map((line) => JSON.parse(line)));
  r.ackFiles = () => {
    const dir = path.join(r.stateDir, 'ack-attempts');
    return fs.existsSync(dir) ? fs.readdirSync(dir).sort() : [];
  };
  r.ready = () => waitFor(async () => {
    try { return (await adapterRequest(r.socket, 'GET', '/healthz')).status === 200; } catch { return false; }
  }, { what: 'the adapter after a restart' });
  // Both owners' messages delivered and held by never-silent before the clock moves.
  r.delivered = (conversations) => r.pumpUntil(() => {
    const sidecar = path.join(r.stateDir, 'never-silent.json');
    if (!fs.existsSync(sidecar)) return false;
    const { messages } = JSON.parse(fs.readFileSync(sidecar));
    return conversations.every((conversation) => Object.values(messages).some((m) => m.conversation === conversation && m.delivered !== undefined));
  }, { what: 'the deliveries recorded before advancing the notice clock' });
  return r;
}

const reactionsTo = (r, id) => r.gw.sends.filter((s) => s.type === 'reaction' && s.reaction.message_id === id);
const ownerLines = (r, to) => r.gw.sends.filter((s) => s.type === 'text' && s.to === to);
const react = (r, id, emoji, key) => adapterRequest(r.socket, 'POST', '/react',
  { session_id: 's-mayor', conversation: { conversation_id: OWNER }, message_id: id, emoji, idempotency_key: key });

async function due(r) {
  r.clock.advance(180000);
  r.health('session_down');
  await waitFor(() => ownerLines(r, CANARY).length === 1, { what: 'the canary\'s owner line' });
}

test('round 4 (R2): both switches on, the session\'s reaction of the acknowledged emoji counts, across a restart', { timeout: 90000 }, async (t) => {
  const r = await setup(t);
  r.start(r.options);
  r.gw.addText(OWNER, MESSAGE, 'fixture words');
  r.gw.addText(CANARY, 'example-canary', 'fixture words');
  await r.delivered([OWNER, CANARY]);
  await waitFor(() => reactionsTo(r, MESSAGE).length === 1, { what: 'the acknowledgement' });
  assert.equal(reactionsTo(r, MESSAGE)[0].reaction.emoji, EYES);

  const answer = await react(r, MESSAGE, EYES, 'round4-class');
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, true);
  assert.deepEqual(reactionsTo(r, MESSAGE).map((s) => s.reaction.emoji), [EYES, EYES], 'the gateway saw the acknowledgement and the session\'s reaction');

  await r.restart(r.options);
  await r.ready();
  await due(r);
  assert.deepEqual(ownerLines(r, OWNER), [], 'the session answered with a reaction; no owner line is due');
});

test('round 4 (R2): the control, the session silent across the same restart: the owner line is posted', { timeout: 90000 }, async (t) => {
  const r = await setup(t);
  r.start(r.options);
  r.gw.addText(OWNER, MESSAGE, 'fixture words');
  r.gw.addText(CANARY, 'example-canary', 'fixture words');
  await r.delivered([OWNER, CANARY]);
  await waitFor(() => reactionsTo(r, MESSAGE).length === 1, { what: 'the acknowledgement' });
  await r.restart(r.options);
  await r.ready();
  await due(r);
  await waitFor(() => ownerLines(r, OWNER).length === 1, { what: 'the owner line' });
  assert.equal(ownerLines(r, OWNER)[0].text, OWNER_LINES.session_down);
});

test('round 4 (R2): the switch unset after an acknowledgement, the session\'s reaction counts as main counts it', { timeout: 90000 }, async (t) => {
  const r = await setup(t);
  r.start(r.options);
  r.gw.addText(OWNER, MESSAGE, 'fixture words');
  r.gw.addText(CANARY, 'example-canary', 'fixture words');
  await r.delivered([OWNER, CANARY]);
  await waitFor(() => reactionsTo(r, MESSAGE).length === 1, { what: 'the acknowledgement' });
  const before = r.ackFiles();
  assert.equal(before.length, 2, 'the acknowledgement markers on disk to test with');

  await r.restart({ ...r.options, ACK_REACTION: '' });
  await r.ready();
  const answer = await react(r, MESSAGE, EYES, 'round4-unset');
  assert.equal(answer.json.delivered, true);
  assert.equal(reactionsTo(r, MESSAGE).length, 2);
  assert.deepEqual(r.ackFiles(), before, 'the markers stay as they were with the switch unset');
  await due(r);
  assert.deepEqual(ownerLines(r, OWNER), [], 'main counts a delivered reaction as the answer');
});

test('round 4 (P2): the react verb writes the session\'s reaction to the stopwatch; a removal writes nothing', { timeout: 90000 }, async (t) => {
  // The acknowledgement off, so the session's reaction is each message's
  // first (the stopwatch keeps the first reaction per message).
  const r = await setup(t, '');
  r.start(r.options);
  for (const id of ['example-first', 'example-second', 'example-third']) r.gw.addText(OWNER, id, 'fixture words');
  await waitFor(() => r.journal().filter((line) => line.t === 'accepted' && line.verdict === 'delivered').length === 3, { what: 'three deliveries' });
  assert.equal((await react(r, 'example-first', EYES, 'round4-p2-first')).json.delivered, true);
  assert.equal((await react(r, 'example-second', '', 'round4-p2-removal')).json.delivered, true);
  assert.equal((await react(r, 'example-third', EYES, 'round4-p2-third')).json.delivered, true);
  // Lines are written in order: once the third is on disk, the removal's
  // line would be too.
  await waitFor(() => r.stopwatch().some((e) => e.event === 'reaction' && e.message === 'example-third'), { timeoutMs: 10000, what: 'the third reaction line' });
  const lines = r.stopwatch().filter((e) => e.event === 'reaction');
  assert.deepEqual(lines.map((e) => [e.conversation, e.message, e.actor]), [[OWNER, 'example-first', 'agent'], [OWNER, 'example-third', 'agent']]);
});
