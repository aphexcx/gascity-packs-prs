'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const zlib = require('node:zlib');
const { command, runHook, readEvents } = require('../reachable.cjs');
const NOW = Date.parse('2026-01-02T12:00:00Z');
const row = (seq, type = 'example.padding', extra = {}) => ({
  seq, type, ts: new Date(NOW - 60000).toISOString(),
  payload: { provider: 'example', conversation_id: 'room-example', target_agent: 'mayor', ...extra },
});
const line = r => JSON.stringify(r) + '\n';
const pre = (id, tool_name = 'Write') => ({ hook_event_name: 'PreToolUse', tool_name, tool_use_id: id, tool_input: {} });
function fixture(t) {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-empty-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  const dir = path.join(city, '.gc'); fs.mkdirSync(dir);
  const log = path.join(dir, 'events.jsonl'); fs.writeFileSync(log, '');
  const o = { city, now: NOW, imports: '', env: { GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example' } };
  const state = () => JSON.parse(fs.readFileSync(path.join(dir, 'reachable/state.json')));
  command('mode', ['enforce'], o);
  return { city, dir, log, o, state };
}
function archive(f, first, last, contents) {
  fs.writeFileSync(f.log + `.archive-20260102T120000Z-seq-${first}-${last}.gz`, zlib.gzipSync(contents));
}
function rotate(f, legacy = false) {
  fs.writeFileSync(f.log, line(row(1)));
  runHook(pre('cursor', 'Read'), f.o);
  fs.appendFileSync(f.log, line(row(2, 'extmsg.inbound')));
  const rotating = f.log + '.rotating-20260102T120000Z' + (legacy ? '' : '-seq-1-2');
  fs.renameSync(f.log, rotating); fs.writeFileSync(f.log, '');
  return () => { archive(f, 1, 2, fs.readFileSync(rotating)); fs.unlinkSync(rotating); };
}
function assertEnforced(f) {
  assert.equal(runHook(pre('enforce'), f.o).hookSpecificOutput?.permissionDecision, 'deny');
  const status = command('status', ['--json'], f.o);
  assert.equal(status.caught_up, true);
  assert.deepEqual(status.waiting, [{ provider: 'example', conversation_id: 'room-example', seconds: 60 }]);
  const stat = fs.statSync(f.log), cursor = f.state().cursor;
  assert.equal(stat.size, 0, 'no sentinel row is appended');
  assert.equal(cursor.dev, stat.dev); assert.equal(cursor.ino, stat.ino); assert.equal(cursor.offset, 0);
  assert.equal(cursor.archive, undefined);
}

test('empty active log: every published rotation shape finishes replay without a sentinel', async t => {
  for (const shape of ['warm', 'cold multiple archives', 'chunked archive', 'pending publication', 'legacy pending publication']) {
    await t.test(shape, t => {
      const f = fixture(t);
      if (shape === 'cold multiple archives') {
        archive(f, 1, 1, line(row(1)));
        archive(f, 2, 2, line(row(2)));
        archive(f, 3, 3, line(row(3, 'extmsg.inbound')));
        for (let n = 0; n < 2; n++) {
          assert.deepEqual(runHook(pre('checkpoint-' + n), f.o), {});
          assert.equal(f.state().cursor.seq, n + 1, 'each archive checkpoints before the next one');
        }
      } else if (shape === 'chunked archive') {
        const padding = 'x'.repeat(1024 * 1024);
        archive(f, 1, 11, Array.from({ length: 10 }, (_, i) => line(row(i + 1, 'example.padding', { padding }))).join('') + line(row(11, 'extmsg.inbound')));
        assert.deepEqual(runHook(pre('chunk'), f.o), {});
        assert.ok(f.state().cursor.archive?.offset > 0, 'decoded archive progress is persisted');
        assert.ok(f.state().cursor.seq < 11);
      } else {
        const publish = rotate(f, shape === 'legacy pending publication');
        if (shape !== 'warm') {
          assert.deepEqual(runHook(pre('pending'), f.o), {});
          assert.equal(command('status', ['--json'], f.o).caught_up, false);
          assert.equal(f.state().cursor.seq, 1);
        }
        publish();
      }
      assertEnforced(f);
    });
  }
});

test('empty active log: Stop still marks the waiting message seen after replay', t => {
  const f = fixture(t), publish = rotate(f); publish();
  assertEnforced(f);
  assert.deepEqual(runHook({ hook_event_name: 'Stop', prompt_id: 'completed-turn' }, f.o), {});
  assert.equal(f.state().seenThrough, 2);
  assert.deepEqual(runHook(pre('after-stop'), f.o), {});
  assert.deepEqual(command('status', ['--json'], f.o).waiting, []);
});

test('empty active log: pass succeeds after publication but still rejects pending publication', t => {
  const f = fixture(t), publish = rotate(f);
  assert.throws(() => command('pass', ['example/room-example'], f.o), /event_catchup_incomplete/);
  publish();
  assert.equal(runHook(pre('before-pass'), f.o).hookSpecificOutput?.permissionDecision, 'deny');
  assert.deepEqual(command('pass', ['example/room-example'], f.o), { conversation: 'example/room-example', seen: true });
  assert.deepEqual(runHook(pre('after-pass'), f.o), {});
  assert.equal(command('status', ['--json'], f.o).caught_up, true);
});

test('empty active log: missing archive sequences fail open without adopting the active file', async t => {
  for (const shape of ['missing middle', 'missing tail', 'missing archive']) {
    await t.test(shape, t => {
      const f = fixture(t);
      rotate(f); // seq 1 has already been read; publication is deliberately damaged below.
      const rotating = f.log + '.rotating-20260102T120000Z-seq-1-2';
      fs.unlinkSync(rotating);
      if (shape === 'missing middle') archive(f, 3, 3, line(row(3, 'extmsg.inbound')));
      if (shape === 'missing tail') archive(f, 1, 2, line(row(1)));
      const cursor = f.state().cursor;
      assert.throws(() => readEvents(f.city, cursor), /event_sequence_gap/);
      assert.deepEqual(runHook(pre('gap'), f.o), {});
      assert.deepEqual(f.state().cursor, cursor);
    });
  }
});

test('empty active log: warm decisions do not reopen the published archive', t => {
  const f = fixture(t), publish = rotate(f); publish();
  runHook(pre('adopt', 'Read'), f.o);
  const open = fs.openSync, readdir = fs.readdirSync;
  fs.openSync = (file, ...args) => {
    assert.ok(!String(file).includes('.archive-'), 'caught-up calls never read archives');
    return open(file, ...args);
  };
  fs.readdirSync = (dir, ...args) => {
    assert.notEqual(dir, f.dir, 'caught-up calls do not rediscover archives');
    return readdir(dir, ...args);
  };
  try { assert.equal(runHook(pre('warm'), f.o).hookSpecificOutput?.permissionDecision, 'deny'); }
  finally { fs.openSync = open; fs.readdirSync = readdir; }
});
