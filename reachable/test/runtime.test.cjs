'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { runHook, command, install, uninstall, doctor, initialState } = require('../reachable.cjs');
const NOW = Date.parse('2026-01-02T12:00:00Z');
function fixture(t) {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-runtime-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  fs.mkdirSync(path.join(city, '.gc'));
  fs.writeFileSync(path.join(city, '.gc/events.jsonl'), JSON.stringify({ seq: 1, type: 'extmsg.inbound', ts: new Date(NOW - 60000).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', target_agent: 'mayor', actor: 'PRIVATE NAME', text: 'PRIVATE TEXT' } }) + '\n');
  const prompt = fs.readFileSync(path.join(__dirname, '../template-fragments/reachable.template.md'), 'utf8').split('\n').slice(1, -2).join('\n');
  for (const name of ['slack-full', 'reachable']) { const dir = path.join(city, name); fs.mkdirSync(dir); fs.writeFileSync(path.join(dir, 'pack.toml'), `[pack]\nname = "${name}"\n`); }
  const options = { city, now: NOW, env: { GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example' }, imports: 'talk-example\t./slack-full\t\t(path)\nreachable\t./reachable\t\t(path)', prompt, source: path.resolve(__dirname, '../reachable.cjs') };
  return { city, options, stateFile: path.join(city, '.gc/reachable/state.json') };
}
const input = id => ({ hook_event_name: 'PreToolUse', tool_name: 'Bash', tool_use_id: id, tool_input: { command: 'make test' } });

test('6,15: workers and subagents do no I/O; an uninstalled city stays off', t => {
  const f = fixture(t);
  assert.deepEqual(runHook(input('off'), f.options), {});
  assert.equal(fs.existsSync(path.join(f.city, '.gc/reachable')), false);
  assert.deepEqual(runHook(input('worker'), { ...f.options, city: '/does-not-exist', env: { GC_AGENT: 'worker' } }), {});
  assert.deepEqual(runHook({ ...input('sub'), agent_id: 'subagent-example' }, { ...f.options, city: '/does-not-exist' }), {});
});

test('2,14,15: modes, pass, exemptions and private atomic state', t => {
  const f = fixture(t);
  command('mode', ['enforce'], f.options);
  assert.ok(runHook(input('deny'), f.options).hookSpecificOutput);
  let status = command('status', ['--json'], f.options);
  assert.equal(status.mode, 'enforce'); assert.equal(status.waiting.length, 1);
  assert.throws(() => command('pass', ['example/unknown-room'], f.options));
  command('pass', ['example/room-example'], f.options);
  assert.equal(command('status', ['--json'], f.options).waiting.length, 0);
  command('exempt', ['example/room-example', '--on'], f.options);
  assert.deepEqual(command('exempt', ['--list'], f.options), ['example/room-example']);
  command('exempt', ['example/room-example', '--off'], f.options);
  assert.deepEqual(command('exempt', ['--list'], f.options), []);
  assert.throws(() => command('mode', ['maybe'], f.options));
  assert.equal(fs.statSync(path.dirname(f.stateFile)).mode & 0o777, 0o700);
  for (const file of fs.readdirSync(path.dirname(f.stateFile))) {
    const p = path.join(path.dirname(f.stateFile), file);
    assert.equal(fs.statSync(p).mode & 0o777, 0o600);
    assert.doesNotMatch(fs.readFileSync(p, 'utf8'), /PRIVATE NAME|PRIVATE TEXT|make test/);
  }
});

test('11: unreadable log, invalid state and backwards clock fail open and log one error', t => {
  const f = fixture(t); command('mode', ['enforce'], f.options);
  fs.writeFileSync(f.stateFile, 'invalid');
  assert.deepEqual(runHook(input('broken'), f.options), {});
  assert.equal(fs.readdirSync(path.dirname(f.stateFile)).filter(n => n.startsWith('error-')).length, 1);
  fs.writeFileSync(f.stateFile, JSON.stringify(initialState()));
  runHook(input('first'), f.options);
  assert.deepEqual(runHook(input('backwards'), { ...f.options, now: NOW - 1000 }), {});
  fs.unlinkSync(path.join(f.city, '.gc/events.jsonl'));
  assert.deepEqual(runHook(input('missing'), f.options), {});
});

test('16: install is idempotent, preserves other matchers, backs up, and uninstall removes only owned entries', t => {
  const f = fixture(t);
  fs.mkdirSync(path.join(f.city, '.claude'));
  const settingsFile = path.join(f.city, '.claude/settings.json');
  const before = { permissions: { allow: ['Read'] }, hooks: { PreToolUse: [{ matcher: '^(?:.*)$', hooks: [{ type: 'command', command: 'example-other-hook' }] }] } };
  fs.writeFileSync(settingsFile, JSON.stringify(before));
  install(f.options);
  assert.equal(command('status', ['--json'], f.options).mode, 'observe');
  const first = fs.readFileSync(settingsFile, 'utf8');
  const installed = JSON.parse(first);
  assert.equal(installed.hooks.PreToolUse.length, 2);
  assert.notEqual(installed.hooks.PreToolUse[0].matcher, installed.hooks.PreToolUse[1].matcher);
  install(f.options);
  assert.equal(fs.readFileSync(settingsFile, 'utf8'), first);
  uninstall(f.options);
  assert.deepEqual(JSON.parse(fs.readFileSync(settingsFile, 'utf8')), before);
  assert.equal(fs.readdirSync(path.join(f.city, '.gc/reachable')).filter(n => n.startsWith('settings-backup-')).length, 2);
});

test('17: doctor detects absent registration, stale copy, missing log and missing prompt fragment', t => {
  const f = fixture(t); install(f.options);
  assert.deepEqual(doctor(f.options), []);
  command('mode', ['enforce'], f.options);
  fs.writeFileSync(path.join(f.city, '.gc/reachable/hook.cjs'), '// stale');
  assert.ok(doctor(f.options).includes('hook_copy_outdated'));
  assert.ok(doctor({ ...f.options, prompt: '' }).includes('prompt_fragment_missing'));
  assert.ok(doctor({ ...f.options, prompt: '## Stay reachable\n' }).includes('prompt_fragment_missing'));
  fs.unlinkSync(path.join(f.city, '.gc/events.jsonl'));
  assert.ok(doctor(f.options).includes('event_log_unreadable'));
  fs.writeFileSync(path.join(f.city, '.claude/settings.json'), '{}');
  assert.ok(doctor(f.options).includes('enforce_without_hook'));
});
