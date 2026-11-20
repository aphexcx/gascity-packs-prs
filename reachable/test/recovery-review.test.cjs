'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const zlib = require('node:zlib');
const { fixture, inbound, NOW, write } = require('../test-support/fixture.cjs');

function expire(file) {
  const past = new Date(Date.now() - 11000);
  fs.utimesSync(file, past, past);
}
function expireHolder(f) {
  const tickets = fs.readdirSync(f.dir).filter(n => /^lock-/.test(n));
  assert.equal(tickets.length, 1);
  expire(path.join(f.dir, tickets[0]));
}
function intercept(t, method, match, callback) {
  const original = fs[method];
  const mock = t.mock.method(fs, method, (...args) => {
    if (match(...args)) {
      mock.mock.restore();
      callback(...args);
    }
    return original(...args);
  });
}

test('R2-R1: a displaced mirror writer returns its committed receipt', t => {
  const f = fixture(t);
  f.append(inbound(1));
  intercept(t, 'renameSync', (_, dest) => dest === path.join(f.dir, 'log.jsonl'), () => {
    expireHolder(f);
    f.cmd('status');
  });
  const call = write('same');
  const original = f.hook(call);
  const duplicate = f.hook(call);
  assert.equal(duplicate.hookSpecificOutput?.permissionDecision, 'deny');
  assert.deepEqual(original, duplicate);
});

test('R2-R2: interrupted manifest removal preserves a successor install', t => {
  const f = fixture(t);
  f.cmd('install');
  const file = path.join(f.dir, 'install.json');
  let resumed = false;
  const reinstall = () => {
    if (resumed) return;
    resumed = true;
    expireHolder(f);
    f.cmd('install');
  };
  // Either physical removal or a move to private trash is a deletion.
  intercept(t, 'unlinkSync', src => src === file, reinstall);
  intercept(t, 'renameSync', src => src === file, reinstall);
  try { f.cmd('uninstall'); } catch (error) { assert.equal(error.message, 'state_busy'); }
  assert.equal(resumed, true);
  assert.equal(f.cmd('status').registered, true);
});

test('R2-R3: a revoked zero ticket cannot promote itself', t => {
  const f = fixture(t);
  let published = 0, displaced = false;
  intercept(t, 'renameSync', (_, dest) => /^lock-/.test(path.basename(dest)) && ++published === 2, (_, ticket) => {
    displaced = true;
    expire(ticket);
    f.cmd('mode', ['observe']);
  });
  assert.throws(() => f.cmd('mode', ['off']), /^Error: state_busy$/);
  assert.equal(displaced, true);
  assert.equal(f.cmd('status').mode, 'observe');
});

test('R2-R4: unfinished bytes after the longest row do not lower the Stop fence', t => {
  const f = fixture(t);
  f.append({ ...inbound(1), type: 'example.tick' });
  f.cmd('status');
  const row = { ...inbound(2), pad: '' };
  row.pad = 'x'.repeat(8 * 1024 * 1024 - Buffer.byteLength(JSON.stringify(row)) - 1);
  f.append(row);
  fs.appendFileSync(f.events, '{');
  f.hook({ hook_event_name: 'Stop', prompt_id: 'example-turn' });
  assert.equal(f.state().seenThrough, 2);
  assert.deepEqual(f.state().waiting, []);
});

test('R2-R5: recovery replaces a stale ticket without opening it for writes', t => {
  const f = fixture(t);
  const stale = path.join(f.dir, 'lock-123-example.json');
  fs.writeFileSync(stale, JSON.stringify({ pid: process.pid, ticket: 1 }));
  expire(stale);
  const open = fs.openSync;
  const writes = [];
  t.mock.method(fs, 'openSync', (file, flags, ...rest) => {
    if (file === stale && typeof flags === 'number' && (flags & (fs.constants.O_WRONLY | fs.constants.O_RDWR))) writes.push(file);
    return open(file, flags, ...rest);
  });
  f.cmd('mode', ['observe']);
  assert.deepEqual(writes, []);
  assert.equal(fs.existsSync(stale), false);
});

test('R2-R6: unreadable settings allow pass with a matching hook copy', t => {
  const f = fixture(t);
  f.cmd('install');
  f.append(inbound(1));
  fs.writeFileSync(path.join(f.city, '.claude/settings.json'), '{');
  assert.equal(f.cmd('pass', ['example/room-a']).seen, true);
  const status = f.cmd('status');
  assert.equal(status.current, true);
  assert.equal(status.registered, false);
});

test('R2-R7: revocation observed after stat is recoverable', t => {
  const f = fixture(t);
  const peer = path.join(f.dir, 'lock-123-example.json');
  fs.writeFileSync(peer, JSON.stringify({ pid: process.pid, ticket: 1 }));
  // Model the empty legacy marker appearing between the stat and read.
  intercept(t, 'readFileSync', file => file === peer, () => fs.writeFileSync(peer, ''));
  assert.equal(f.cmd('mode', ['observe']).mode, 'observe');
  assert.equal(fs.existsSync(peer), false);
});

test('R2-R8: consecutive archive and active gaps retain the last readable sequence', t => {
  const f = fixture(t);
  f.append(inbound(1), inbound(undefined, { target_agent: 'other' }));
  const archive = path.join(path.dirname(f.events), 'events.jsonl.archive-20260102T120000Z-seq-1-3.gz');
  fs.writeFileSync(archive, zlib.gzipSync(fs.readFileSync(f.events)));
  fs.writeFileSync(f.events, '');
  assert.equal(f.cmd('status').skipped_rows.last_seq, 1);
  f.append(inbound(undefined, { target_agent: 'other' }), { ...inbound(6), type: 'example.tick' });
  assert.deepEqual(f.cmd('status').skipped_rows, { count: 2, last_seq: 1, last_reason: 'gap', last_at: NOW });
});
