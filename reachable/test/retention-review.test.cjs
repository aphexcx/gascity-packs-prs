'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const zlib = require('node:zlib');
const { fixture, inbound, write, NOW } = require('../test-support/fixture.cjs');
const { initialState, decide, LIMITS } = require('../reachable.cjs');

test('review: an event stamped during snapshot acquisition is measured at snapshot completion', t => {
  const f = fixture(t);
  delete f.options.now;
  let clock = NOW;
  t.mock.method(Date, 'now', () => clock);
  const open = fs.openSync;
  t.mock.method(fs, 'openSync', function(file, ...args) {
    if (file === f.events && clock === NOW) {
      clock += 10;
      f.append(inbound(1, {}, clock));
    }
    return open.call(this, file, ...args);
  });
  const status = f.cmd('status');
  assert.equal(status.waiting.length, 1);
  assert.equal(status.skipped_rows.count, 0);
});

test('review: every malformed complete line advances, including empty lines and non-string timestamps', t => {
  const f = fixture(t);
  f.append(inbound(1), '', inbound(3), { ...inbound(4), ts: { toString: false } }, inbound(5));
  let status;
  assert.doesNotThrow(() => { status = f.cmd('status'); });
  assert.equal(status.skipped_rows.count, 2);
  assert.equal(f.state().cursor.seq, 5);
});
test('review: a replacement active file with an identical byte prefix keeps consumed skips behind the cursor', t => {
  const f = fixture(t);
  f.append(inbound(1), '{', inbound(3));
  f.cmd('status');
  const replacement = f.events + '.new';
  fs.copyFileSync(f.events, replacement); fs.renameSync(replacement, f.events);
  f.append(inbound(4));
  assert.equal(f.cmd('status').skipped_rows.count, 1);
  assert.deepEqual(f.state().waiting.map(w => w.released), [true, false, false]);
});
test('rotation: a reused inode and matching trailing bytes cannot conceal a new sequence', t => {
  const f = fixture(t);
  f.append(inbound(1)); f.cmd('status');
  f.append(inbound(2));
  const archive = f.events + '.archive-20260102T120000Z-seq-1-2.gz';
  fs.writeFileSync(archive, zlib.gzipSync(fs.readFileSync(f.events)));
  // Rewriting in place deterministically presents the same dev and inode.
  fs.writeFileSync(f.events, JSON.stringify(inbound(3)) + '\n');
  f.cmd('status');
  assert.deepEqual(f.state().waiting.map(w => w.seq), [1, 2, 3]);
});
test('review: usable routing on damaged rows still excludes another agent', t => {
  const f = fixture(t);
  f.append(inbound(1), { ...inbound(2), type: 'extmsg.outbound', subject: 'worker-example', ts: 'invalid' },
    { ...inbound(3, { target_agent: 'worker-example' }), ts: 'invalid' });
  assert.equal(f.hook(write('work')).hookSpecificOutput?.permissionDecision, 'deny');
  assert.equal(f.cmd('status').skipped_rows.count, 0);
});
test('review: the shell decision uses logical quoted words for continuations and reply accounting', () => {
  const opts = { mode: 'enforce', now: NOW, session: 'session-example', aliases: ['talk'] };
  const evaluate = command => {
    const state = initialState();
    state.waiting.push({ at: NOW - 60000, provider: 'example', conversation_id: 'room-a', seq: 1, refusals: 0 });
    const output = decide(state, { ...write('reply'), tool_name: 'Bash', tool_input: { command } }, opts);
    return { output, state };
  };
  const continuation = String.fromCharCode(92, 10);
  const subscript = evaluate('gc talk reply-current "$NAME' + continuation + '[1]"');
  assert.match(subscript.output.hookSpecificOutput?.permissionDecisionReason ?? '', /Refused because the shell command can expand/);
  assert.deepEqual(evaluate('gc talk reply-current "line' + continuation + ' two"').output, {});
  for (const prefix of ['gc "talk"', "'gc' talk", 'cd dir; gc ta\\lk']) {
    const { state } = evaluate(prefix + ' reply-current ?');
    assert.equal(state.waiting[0].refusals, 0);
    assert.equal(state.log[0].reply_shaped, true);
  }
});
test('review: an outstanding wait remains actionable after the known map reaches its cap', t => {
  const f = fixture(t);
  for (let i = 0; i <= LIMITS.known; i++) f.append(inbound(i + 1, { conversation_id: 'room-' + i }));
  f.cmd('status');
  assert.equal(Object.keys(f.state().known).length, LIMITS.known);
  assert.doesNotThrow(() => f.cmd('pass', ['example/room-0']));
  assert.ok(!f.state().waiting.some(w => w.conversation_id === 'room-0'));
});
test('review: the transaction sees a mode change after its initial identity and mode checks', t => {
  const f = fixture(t);
  f.append(inbound(1));
  const read = fs.readFileSync;
  const modeFile = path.join(f.dir, 'mode.json');
  let changed = false;
  t.mock.method(fs, 'readFileSync', function(file, ...args) {
    const data = read.call(this, file, ...args);
    if (file === modeFile && !changed) { changed = true; fs.writeFileSync(file, '{"mode":"off"}'); }
    return data;
  });
  assert.deepEqual(f.hook(write('work')), {});
});
test('review: doctor reports failed mirrors on allowed calls and backwards clocks', t => {
  const f = fixture(t);
  f.cmd('mode', ['observe']); f.hook(write('good'));
  f.options.now += 100;
  const rename = fs.renameSync;
  const mock = t.mock.method(fs, 'renameSync', function(src, dest) {
    if (dest === path.join(f.dir, 'log.jsonl')) throw new Error('fixture mirror failure');
    return rename.call(this, src, dest);
  });
  assert.deepEqual(f.hook(write('failure')), {}); mock.mock.restore();
  assert.equal(f.state().hook_ok_at, NOW);
  assert.ok(f.cmd('doctor').includes('hook_failing_open measurement_failed'));
  f.options.now += 100; f.hook(write('recover'));
  f.options.now -= 500; f.hook(write('backwards'));
  assert.ok(f.cmd('doctor').includes('hook_failing_open clock_moved_backwards'));
});
test('review: status also bounds log entries emitted by deferred call completion', t => {
  const f = fixture(t);
  const state = initialState();
  state.log = Array.from({ length: LIMITS.log }, () => ({ type: 'long_call', tool: 'Read', at: NOW }));
  state.calls.deferred = { at: NOW, tool: 'Read', ended: { at: NOW, elapsed: 60000, waitMs: 30000, through: 0 } };
  fs.writeFileSync(path.join(f.dir, 'state.json'), JSON.stringify(state));
  const status = f.cmd('status');
  assert.equal(status.history.length, LIMITS.log);
  assert.equal(status.history_dropped, 1);
});
test('review: the archive row budget excludes the bytes used to identify the old cursor', t => {
  const f = fixture(t);
  f.append(inbound(1), '{'); f.cmd('status');
  const row = { ...inbound(3), type: 'fixture.padding', padding: '' };
  row.padding = 'x'.repeat(8 * 1024 * 1024 - Buffer.byteLength(JSON.stringify(row)) - 16);
  f.append(row);
  const archive = f.events + '.archive-20260102T120000Z-seq-1-3.gz';
  fs.writeFileSync(archive, zlib.gzipSync(fs.readFileSync(f.events)));
  fs.unlinkSync(f.events); fs.writeFileSync(f.events, '');
  assert.doesNotThrow(() => f.cmd('status'));
  assert.equal(f.state().cursor.seq, 3);
});
test('review: uninstall recreates a missing settings directory for the original empty file', t => {
  const f = fixture(t);
  const dir = path.join(f.city, '.claude'), file = path.join(dir, 'settings.json');
  fs.mkdirSync(dir); fs.writeFileSync(file, '{}\n');
  f.cmd('install');
  fs.unlinkSync(file); fs.rmdirSync(dir);
  assert.doesNotThrow(() => f.cmd('uninstall'));
  assert.equal(fs.readFileSync(file, 'utf8'), '{}\n');
});
