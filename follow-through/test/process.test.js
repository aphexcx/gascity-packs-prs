import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawn, spawnSync } from 'node:child_process';
import { withState, readState } from '../src/state.js';
import { consent, hours, addItem, closeItem, setReminder } from '../src/model.js';
import { status } from '../src/engine.js';
import { statusText } from '../src/cli.js';

const engine = new URL('../src/engine.js', import.meta.url).href;
const now = Date.parse('2026-11-10T09:00:00Z');
const conversation = 'slack/example';
const fakeSource = `#!/usr/bin/env node
const fs = require('node:fs');
const args = process.argv.slice(2);
fs.appendFileSync(process.env.FT_TEST_CALLS, JSON.stringify(args) + '\\n');
const key = args[args.indexOf('--dedup') + 1];
let calls = fs.readFileSync(process.env.FT_TEST_CALLS, 'utf8').trim().split('\\n').length;
if (calls <= Number(process.env.FT_TEST_FAILS || 0)) process.exit(1);
if (process.env.FT_TEST_KILL_DURING === 'yes' && calls === 1) {
  process.kill(process.ppid, 'SIGKILL');
  process.exit(1);
}
const sent = fs.existsSync(process.env.FT_TEST_SENT) ? fs.readFileSync(process.env.FT_TEST_SENT, 'utf8').trim().split('\\n').map(JSON.parse) : [];
if (!sent.some(row => row.key === key)) {
  fs.appendFileSync(process.env.FT_TEST_SENT, JSON.stringify({ key, args }) + '\\n');
}
`;

function fixture(t, overrides = {}) {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-process-'));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  const bin = path.join(root, 'fake-gc.cjs');
  fs.writeFileSync(bin, fakeSource, { mode: 0o700 });
  const dir = path.join(root, 'state');
  const env = { ...process.env, GC_BIN: bin, FT_TEST_CALLS: path.join(root, 'calls.jsonl'),
    FT_TEST_SENT: path.join(root, 'sent.jsonl'), ...overrides };
  const rows = file => fs.existsSync(file) ? fs.readFileSync(file, 'utf8').trim().split('\n').filter(Boolean).map(JSON.parse) : [];
  const run = (time = now, killPhase = '') => new Promise(resolve => {
    const script = `import {tick} from ${JSON.stringify(engine)};
      await tick({dir: process.argv[1], now: Number(process.argv[2]), afterPersist(phase) {
        if (phase === process.argv[3]) process.kill(process.pid, 'SIGKILL');
      }});`;
    const child = spawn(process.execPath, ['--input-type=module', '-e', script, dir, String(time), killPhase], { env, stdio: ['ignore', 'pipe', 'pipe'] });
    let stderr = '';
    child.stderr.on('data', chunk => { stderr += chunk; });
    child.on('close', (code, signal) => resolve({ code, signal, stderr }));
  });
  return { dir, env, run, calls: () => rows(env.FT_TEST_CALLS), sent: () => rows(env.FT_TEST_SENT) };
}

async function seed(h, kind) {
  await withState(h.dir, async (s, save) => {
    if (kind === 'follow-up') {
      consent(s, conversation, true, now - 86400000);
      hours(s, conversation, { clear: true, tz: 'UTC' });
      addItem(s, conversation, 'Invented follow-up', now - 86400000);
    } else setReminder(s, conversation, 'Invented reminder', now, now - 86400000);
    save();
  });
}

test('12,14: fake gc records agent, notify, stable key, subjects and fixed fact bodies', async t => {
  const h = fixture(t, { FOLLOW_THROUGH_AGENT: 'local-mayor' });
  await seed(h, 'follow-up');
  await seed(h, 'reminder');
  assert.equal((await h.run()).code, 0);
  assert.equal(h.calls().length, 2);
  for (const args of h.calls()) {
    assert.deepEqual(args.slice(0, 5), ['mail', 'send', 'local-mayor', '--notify', '--dedup']);
    assert.match(args[5], /^follow-through:/);
    assert.equal(args[6], '-s');
    assert.equal(args[8], '-m');
    assert.match(args[9], /The mayor decides whether and how to write, by the channel's rules\.$/);
  }
  assert.equal(h.calls()[0][7], 'follow-through: follow-up due');
  assert.equal(h.calls()[1][7], 'follow-through: reminder due');
  assert.match(h.calls()[0][9], /^Follow-up due in slack\/example\.\nWake wake-/);
  if (process.env.FOLLOW_THROUGH_TEST_EVIDENCE_DIR) {
    const evidence = process.env.FOLLOW_THROUGH_TEST_EVIDENCE_DIR;
    fs.mkdirSync(evidence, { recursive: true });
    const state = readState(h.dir);
    fs.writeFileSync(path.join(evidence, 'sample-state.json'), JSON.stringify(state, null, 2) + '\n');
    fs.writeFileSync(path.join(evidence, 'sample-status.json'), JSON.stringify(status(state, now), null, 2) + '\n');
    fs.writeFileSync(path.join(evidence, 'sample-status.txt'), statusText(status(state, now)) + '\n');
    fs.writeFileSync(path.join(evidence, 'fake-gc-arguments.json'), JSON.stringify(h.calls(), null, 2) + '\n');
    fs.writeFileSync(path.join(evidence, 'follow-up-mail.txt'), h.calls()[0][9] + '\n');
    fs.writeFileSync(path.join(evidence, 'reminder-mail.txt'), h.calls()[1][9] + '\n');
  }
});

test('3,4,16: SIGKILL at every raise boundary keeps records and one deduplicated mail', async t => {
  for (const kind of ['follow-up', 'reminder']) {
    for (const phase of ['planned', 'attempt', 'sent', 'during']) {
      await t.test(`${kind}: killed ${phase}`, async t => {
        const h = fixture(t, { FOLLOW_THROUGH_AGENT: '', FT_TEST_KILL_DURING: phase === 'during' ? 'yes' : '' });
        await seed(h, kind);
        const killed = await h.run(now, phase === 'during' ? '' : phase);
        assert.equal(killed.signal, 'SIGKILL', killed.stderr);
        assert.equal(readState(h.dir).raises.length, 1);
        assert.equal((await h.run(now + 60000)).code, 0);
        assert.equal((await h.run(now + 60001)).code, 0);
        assert.equal(h.sent().length, 1);
        assert.equal(h.sent()[0].args[2], 'mayor');
        assert.equal(new Set(h.calls().map(args => args[5])).size, 1);
        assert.equal(readState(h.dir).raises[0].state, 'sent');
      });
    }
  }
});

test('13: three real gc failures then success make one delivered mail with one key', async t => {
  const h = fixture(t, { FT_TEST_FAILS: '3' });
  await seed(h, 'reminder');
  for (let minute = 0; minute < 4; minute++) assert.equal((await h.run(now + minute * 60000)).code, 0);
  assert.equal(h.calls().length, 4);
  assert.equal(h.sent().length, 1);
  assert.equal(new Set(h.calls().map(args => args[5])).size, 1);
});

test('17: two ticks started together serialize and deliver once', async t => {
  const h = fixture(t);
  await seed(h, 'reminder');
  const results = await Promise.all([h.run(), h.run()]);
  for (const r of results) assert.equal(r.code, 0, r.stderr);
  assert.equal(h.calls().length, 1);
  assert.equal(h.sent().length, 1);
});

test('9 ruling 2: large wakes send one bounded mail and list exactly every raised item', async t => {
  for (const count of [26, 100, 500]) {
    const h = fixture(t);
    await withState(h.dir, async (s, save) => {
      consent(s, conversation, true, now - 86400000);
      hours(s, conversation, { clear: true, tz: 'UTC' });
      for (let i = 0; i < count; i++) addItem(s, conversation, String(i).padStart(4, '0') + 'x'.repeat(196), now - 86400000 + i);
      // A different conversation/wake must not leak into the filter.
      addItem(s, 'slack/other', 'Invented separate item', now - 86400000);
      save();
    });
    assert.equal((await h.run()).code, 0);
    assert.equal((await h.run(now + 60000)).code, 0);
    assert.equal(h.calls().length, 1);
    const body = h.calls()[0][9];
    assert.ok(Buffer.byteLength(body) <= 65536);
    assert.equal(body.split('\n').filter(line => line.startsWith('item-')).length, 25);
    assert.match(body, new RegExp(`${count - 25} more are due\\. See them all: gc follow-through list --wake (wake-[a-f0-9-]+)`));
    const state = readState(h.dir);
    assert.equal(state.items.filter(i => i.state === 'raised').length, count);
    const wake = state.items[0].wake_id;
    assert.ok(wake);
    assert.ok(state.items.slice(0, count).every(i => i.wake_id === wake));
    assert.ok(body.indexOf('0000') < body.indexOf('0024'));
    await withState(h.dir, async (s, save) => {
      closeItem(s, s.items[0].id, 'done', now + 1000);
      closeItem(s, s.items[1].id, 'dropped', now + 1000);
      save();
    });
    const cli = new URL('../src/cli.js', import.meta.url);
    const result = spawnSync(process.execPath, [cli.pathname, 'list', '--wake', wake, '--json'], {
      env: { ...h.env, GC_PACK_STATE_DIR: h.dir }, encoding: 'utf8',
    });
    assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).length, count);
    assert.ok(JSON.parse(result.stdout).every(i => i.wake_id === wake));
  }
});

test('9 ruling 2: maximum UTF-8 reminder reaches gc below the body bound', async t => {
  const h = fixture(t);
  await withState(h.dir, async (s, save) => {
    setReminder(s, 'slack/' + '🧪'.repeat(126) + 'xx', '🧪'.repeat(500), now, now - 86400000);
    save();
  });
  assert.equal((await h.run()).code, 0);
  assert.equal(h.calls().length, 1);
  assert.ok(Buffer.byteLength(h.calls()[0][9]) <= 65536);
  assert.ok(h.calls()[0][9].includes('🧪'.repeat(500)));
});
