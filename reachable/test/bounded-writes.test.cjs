'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { once } = require('node:events');
const { fixture, inbound, write, NOW } = require('../test-support/fixture.cjs');
const { LIMITS, initialState, reduceEvents, decide, hash } = require('../reachable.cjs');
const errors = f => fs.readdirSync(f.dir).filter(name => /^error-.*\.jsonl$/.test(name));
const script = path.resolve(__dirname, '../reachable.cjs');

function hookProcess(f, input) {
  const child = spawn(process.execPath, [script, '--hook', '--city', f.city], { env: f.options.env });
  let output = '', error = '';
  child.stdout.on('data', chunk => { output += chunk; });
  child.stderr.on('data', chunk => { error += chunk; });
  const done = once(child, 'close');
  child.stdin.end(JSON.stringify(input));
  return done.then(([code]) => {
    assert.equal(code, 0, error); assert.equal(error, '');
    return output ? JSON.parse(output) : {};
  });
}
test('R4: paired hook processes for one call leave no error record', async t => {
  const f = fixture(t, Date.now());
  for (let i = 1; i <= 20; i++) {
    f.append(inbound(i, {}, Date.now() - 60000));
    const input = write('pair-' + i);
    const [a, b] = await Promise.all([hookProcess(f, input), hookProcess(f, input)]);
    assert.deepEqual(errors(f).map(name => JSON.parse(fs.readFileSync(path.join(f.dir, name))).code), []);
    assert.deepEqual(a, b);
    assert.equal(a.hookSpecificOutput?.permissionDecision, 'deny');
    assert.equal(f.state().waiting.at(-1).refusals, 1);
  }
});
test('R4: lasting faults leave at most 100 error records', t => {
  const f = fixture(t);
  const old = path.join(f.dir, 'error-' + (NOW - 15 * 86400000) + '-abcdef.jsonl');
  fs.writeFileSync(old, JSON.stringify({ type: 'error', code: 'state_busy', at: NOW - 15 * 86400000 }));
  fs.writeFileSync(path.join(f.dir, 'state.json'), 'invalid');
  for (let i = 0; i < 150; i++) {
    f.options.now++;
    assert.deepEqual(f.hook(write(String(i))), {});
  }
  assert.equal(errors(f).length, 100);
  assert.equal(fs.existsSync(old), false);
  assert.ok(f.cmd('doctor').includes('hook_failing_open invalid_state_json'));
  fs.writeFileSync(path.join(f.dir, 'state.json'), JSON.stringify(initialState()));
  const status = f.cmd('status');
  assert.equal(status.errors.invalid_state_json.count, 100);
  assert.equal(status.errors.invalid_state_json.newest, f.options.now);
  assert.equal(status.hook_ok_at, null);
  f.options.now++;
  f.hook(write('recovered'));
  assert.equal(f.cmd('status').hook_ok_at, f.options.now);
  assert.ok(!f.cmd('doctor').some(s => s.startsWith('hook_failing_open')));
});
test('R4: an old lock held by a live pid is removed', t => {
  const f = fixture(t);
  const file = path.join(f.dir, `lock-${process.pid}-fixture.json`);
  fs.writeFileSync(file, JSON.stringify({ pid: process.pid, ticket: 1 }));
  const old = new Date(Date.now() - 60000); fs.utimesSync(file, old, old);
  f.append(inbound(1));
  assert.equal(f.hook(write('after')).hookSpecificOutput?.permissionDecision, 'deny');
  assert.equal(fs.existsSync(file), false); assert.deepEqual(errors(f), []);
});
test('R4: a held lock is waited for', async t => {
  const f = fixture(t, Date.now());
  f.append(inbound(1, {}, Date.now() - 60000));
  const child = spawn(process.execPath, ['-e', `
    const fs = require('node:fs'), path = require('node:path');
    const file = path.join(process.argv[1], 'lock-' + process.pid + '-fixture.json');
    fs.writeFileSync(file, JSON.stringify({ pid: process.pid, ticket: 1 }));
    process.stdout.write('ready');
    setTimeout(() => fs.unlinkSync(file), 200);
  `, f.dir]);
  const done = once(child, 'close');
  await once(child.stdout, 'data');
  const answer = await hookProcess(f, write('wait'));
  await done;
  assert.equal(answer.hookSpecificOutput?.permissionDecision, 'deny');
  assert.deepEqual(errors(f), []);
});
test('R4: forty thousand hook events in a day keep state.json bounded', () => {
  const state = initialState();
  const opts = { now: NOW, agent: 'mayor', session: 'session-example', mode: 'observe' };
  let seq = 0;
  for (let i = 0; i < 20000; i++) {
    opts.now = NOW + i * 4320;
    const next = Math.floor(i * 720 / 20000) + 1;
    if (next > seq) {
      seq = next;
      reduceEvents(state, [inbound(seq, {}, opts.now - 60000)], opts);
    }
    const input = write('event-' + i);
    decide(state, input, opts);
    decide(state, { ...input, hook_event_name: 'PostToolUse' }, opts);
    if (i === 1000) assert.ok(Object.keys(state.receipts).length <= 1000);
  }
  assert.ok(Object.keys(state.receipts).length <= LIMITS.receipts);
  assert.ok(Object.keys(state.calls).length <= LIMITS.calls);
  assert.ok(state.log.length <= LIMITS.log);
  assert.ok(state.history_dropped > 0);
  assert.ok(Buffer.byteLength(JSON.stringify(state)) < 1024 * 1024);
});
test('R4: a version 1 state.json from PR 96 loads and converts', t => {
  const f = fixture(t);
  const legacy = { ...initialState(), version: 1, known: ['example/room-a'], sessions: [hash('old-session')] };
  fs.writeFileSync(path.join(f.dir, 'state.json'), JSON.stringify(legacy));
  f.cmd('status');
  assert.equal(f.state().version, 2);
  assert.deepEqual(f.state().known, { 'example/room-a': NOW });
  assert.equal(f.state().sessions[hash('old-session')], NOW);
});
test('R8: a conversation not routed for 14 days leaves known', t => {
  const f = fixture(t);
  f.append(inbound(1)); f.cmd('exempt', ['example/room-a', '--on']);
  f.options.now += 14 * 86400000;
  assert.throws(() => f.cmd('pass', ['example/room-a']), /unknown_conversation/);
  assert.deepEqual(f.cmd('exempt', ['--list']), ['example/room-a']);
});
test('R8: five settings changes leave three dated backups', t => {
  const f = fixture(t);
  fs.mkdirSync(path.join(f.city, '.claude'));
  fs.writeFileSync(path.join(f.city, '.claude/settings.json'), '{}');
  for (const verb of ['install', 'uninstall', 'install', 'uninstall', 'install']) { f.options.now++; f.cmd(verb); }
  assert.equal(fs.readdirSync(f.dir).filter(n => n.startsWith('settings-backup-')).length, 3);
});

test('R5: warm decisions at all collection caps use under 100 ms of CPU at p95', t => {
  const f = fixture(t);
  const state = initialState();
  assert.ok(LIMITS.receipts > 0 && LIMITS.sessions > 0);
  for (let i = 0; i < LIMITS.receipts; i++) state.receipts[hash('receipt-' + i)] = { at: NOW, output: {} };
  for (let i = 0; i < LIMITS.calls; i++) state.calls[hash('call-' + i)] = { at: NOW, tool: 'Read', waiting: false };
  for (let i = 0; i < LIMITS.sessions; i++) state.sessions[hash('session-' + i)] = NOW;
  for (let i = 0; i < LIMITS.known; i++) state.known['example/room-' + i] = NOW;
  for (let i = 0; i < LIMITS.log; i++) state.log.push({ at: NOW, type: 'would_refuse', provider: 'example', conversation_id: 'room-a', seconds: 60 });
  fs.writeFileSync(path.join(f.dir, 'state.json'), JSON.stringify(state));
  f.hook(write('warm'));
  const samples = [];
  for (let i = 0; i < 100; i++) {
    const start = process.cpuUsage();
    f.hook(write('capped-' + i));
    const used = process.cpuUsage(start);
    samples.push((used.user + used.system) / 1000);
  }
  const p95 = samples.sort((a, b) => a - b)[94];
  t.diagnostic('all collection caps, 100 warm decisions: p95 CPU=' + p95.toFixed(2) + ' ms');
  assert.ok(p95 < 100);
  assert.deepEqual(errors(f), []);
});
