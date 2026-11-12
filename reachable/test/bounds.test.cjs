'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const os = require('node:os');
const { spawnSync } = require('node:child_process');
const { performance } = require('node:perf_hooks');
const { readEvents, runHook, command, install, initialState, reduceEvents, decide, aliasesFromImports } = require('../reachable.cjs');
const NOW = Date.parse('2026-01-02T12:00:00Z');
function fixture(t) {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-bounds-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  fs.mkdirSync(path.join(city, '.gc'));
  const log = path.join(city, '.gc/events.jsonl'); fs.writeFileSync(log, '');
  const options = { city, now: NOW, env: { GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example' }, imports: '' };
  return { city, log, options };
}
const row = (seq = 1) => ({ seq, type: 'extmsg.inbound', ts: new Date(NOW - 60000).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', target_agent: 'mayor' } });
const input = id => ({ hook_event_name: 'PreToolUse', tool_name: 'Bash', tool_use_id: id, tool_input: { command: 'make test' } });

test('5: an exemption ignores future inbounds until explicitly removed', t => {
  const f = fixture(t); fs.writeFileSync(f.log, JSON.stringify(row()) + '\n');
  command('exempt', ['example/room-example', '--on'], f.options);
  fs.appendFileSync(f.log, JSON.stringify(row(2)) + '\n');
  assert.equal(command('status', [], f.options).waiting.length, 0);
  command('exempt', ['example/room-example', '--off'], f.options);
  fs.appendFileSync(f.log, JSON.stringify(row(3)) + '\n');
  assert.equal(command('status', [], f.options).waiting.length, 1);
  assert.throws(() => command('exempt', ['example/unknown', '--on'], f.options));
});

test('11: an expired measurement never refuses and state-lock contention fails open', t => {
  const f = fixture(t); fs.writeFileSync(f.log, JSON.stringify(row()) + '\n');
  command('mode', ['enforce'], f.options);
  assert.deepEqual(runHook(input('timeout'), { ...f.options, start: performance.now() - 2100 }), {});
  const lock = path.join(f.city, `.gc/reachable/lock-${process.pid}-example.json`);
  fs.writeFileSync(lock, JSON.stringify({ pid: process.pid, ticket: 1 }));
  assert.deepEqual(runHook(input('busy'), f.options), {});
  fs.unlinkSync(lock);
  assert.ok(runHook(input('after'), f.options).hookSpecificOutput);
});

test('11: malformed hook stdin fails open and records one error using the installed city argument', t => {
  const f = fixture(t);
  const result = spawnSync(process.execPath, [path.resolve(__dirname, '../reachable.cjs'), '--hook', '--city', f.city], {
    env: { ...process.env, GC_AGENT: 'mayor', GC_CITY: '', GC_CITY_PATH: '' }, input: '{broken', encoding: 'utf8',
  });
  assert.equal(result.status, 0); assert.equal(result.stdout, ''); assert.equal(result.stderr, '');
  const dir = path.join(f.city, '.gc/reachable');
  const log = fs.readdirSync(dir).filter(n => n.startsWith('error-')).flatMap(n => fs.readFileSync(path.join(dir, n), 'utf8').trim().split('\n'));
  assert.equal(log.length, 1);
  assert.doesNotMatch(log[0], /broken/);
});

test('3,4: a historical mayor wake identifies an old session reply after handoff', () => {
  const state = initialState();
  const rows = [{ seq: 1, type: 'session.woke', subject: 'mayor', session_id: 'old-session', ts: new Date(NOW - 90000).toISOString() }, row(2),
    { seq: 3, type: 'extmsg.outbound', ts: new Date(NOW - 1000).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', session: 'old-session' } }];
  reduceEvents(state, rows, { now: NOW, agent: 'mayor', session: 'new-session' });
  assert.equal(state.waiting.length, 0);
});

test('4: age expiry and an unrelated reaction do not stand in for words', () => {
  const state = initialState(); const o = { now: NOW, agent: 'mayor', session: 'session-example' };
  reduceEvents(state, [row(), { seq: 2, type: 'example.reaction', ts: new Date(NOW).toISOString() }], o);
  assert.equal(state.waiting.length, 1);
  reduceEvents(state, [], { ...o, now: NOW + 86400000 });
  assert.equal(state.waiting.length, 0);
});

test('13: duplicate Stop without a prompt ID cannot clear an intervening inbound', () => {
  const state = initialState(); const o = { now: NOW, agent: 'mayor', session: 'session-example', mode: 'enforce', turnId: 'transcript-user-example' };
  decide(state, input('call'), o);
  const stop = { hook_event_name: 'Stop', session_id: 'harness-example' };
  decide(state, stop, o);
  reduceEvents(state, [row()], o); state.cursor = { seq: 1 };
  decide(state, stop, o);
  assert.equal(state.waiting.length, 1);
});

test('15: first install always starts observe; reinstall preserves an explicit mode', t => {
  const f = fixture(t); command('mode', ['enforce'], f.options);
  install(f.options);
  assert.equal(command('status', [], f.options).mode, 'observe');
  command('mode', ['enforce'], f.options); install(f.options);
  assert.equal(command('status', [], f.options).mode, 'enforce');
});

test('16: uninstall preserves a later hook added to the same matcher and rejects symlinked settings', t => {
  const f = fixture(t); install(f.options);
  const settings = path.join(f.city, '.claude/settings.json');
  const data = JSON.parse(fs.readFileSync(settings));
  data.hooks.PreToolUse[0].hooks.push({ type: 'command', command: 'example-later-hook' });
  fs.writeFileSync(settings, JSON.stringify(data));
  command('uninstall', [], f.options);
  assert.equal(JSON.parse(fs.readFileSync(settings)).hooks.PreToolUse[0].hooks[0].command, 'example-later-hook');
  fs.unlinkSync(settings); fs.symlinkSync(f.log, settings);
  assert.throws(() => install(f.options));
});

test('9: aliases come from all four pack rows, including a locally renamed directory', t => {
  const f = fixture(t); const renamed = path.join(f.city, 'renamed'); fs.mkdirSync(renamed);
  fs.writeFileSync(path.join(renamed, 'pack.toml'), '[pack]\nname = "gchat"\nschema = 2\n');
  for (const name of ['slack-full', 'whatsapp', 'reachable', 'unrelated']) { const dir = path.join(f.city, name); fs.mkdirSync(dir); fs.writeFileSync(path.join(dir, 'pack.toml'), `[pack]\nname = "${name}"\n`); }
  const text = ['s\t./slack-full\t\t(path)', 'g\t./renamed\t\t(path)', 'w\t./whatsapp\t\t(path)', 'r\t./reachable\t\t(path)', '(transitive)\t./renamed\t\t(path)', 'no\t./unrelated\t\t(path)'].join('\n');
  assert.deepEqual(aliasesFromImports(text, f.city), ['s', 'g', 'w', 'r']);
});

test('13: 256 MiB fixture decision p95 stays below 100 ms after cursor catch-up', t => {
  const f = fixture(t);
  const fd = fs.openSync(f.log, 'w');
  const total = 256 * 1024 * 1024; let written = 0, seq = 0;
  while (written < total) {
    const prefix = JSON.stringify({ seq: ++seq, type: 'fixture.tick', ts: new Date(NOW).toISOString(), padding: '' });
    const line = prefix.slice(0, -2) + 'x'.repeat(4096 - prefix.length - 1) + '"}\n';
    fs.writeSync(fd, line); written += Buffer.byteLength(line);
  }
  fs.closeSync(fd);
  assert.equal(fs.statSync(f.log).size, total);
  command('mode', ['observe'], f.options);
  let status;
  do { status = command('status', [], f.options); } while (!status.caught_up);
  const samples = [];
  for (let i = 0; i < 100; i++) {
    const start = performance.now(); runHook(input(`perf-${i}`), f.options); samples.push(performance.now() - start);
  }
  samples.sort((a, b) => a - b);
  t.diagnostic(`256 MiB, 100 warm decisions: p95=${samples[94].toFixed(3)}ms`);
  assert.ok(samples[94] < 100, `p95 ${samples[94]}ms`);
  const script = path.resolve(__dirname, '../reachable.cjs');
  const cli = [];
  for (let i = 0; i < 20; i++) {
    const start = performance.now();
    const result = spawnSync(process.execPath, [script, '--hook', '--city', f.city], { env: { ...process.env, GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example' }, input: JSON.stringify(input(`cli-${i}`)), encoding: 'utf8' });
    assert.equal(result.status, 0); assert.equal(result.stderr, ''); cli.push(performance.now() - start);
  }
  cli.sort((a, b) => a - b);
  t.diagnostic(`256 MiB, 20 complete hook processes: p95=${cli[18].toFixed(3)}ms`);
  assert.ok(cli[18] < 100, `CLI p95 ${cli[18]}ms`);
});
