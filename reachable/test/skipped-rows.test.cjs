'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const zlib = require('node:zlib');
const { fixture, inbound, write, NOW } = require('../test-support/fixture.cjs');
const outbound = seq => ({ ...inbound(seq), type: 'extmsg.outbound' });
const badRows = [
  ['invalid_event_json', '{', 1],
  ['invalid_event_row', { ...inbound(2), seq: null }, 1],
  ['invalid_event_row', { ...inbound(2), ts: 'never' }, 2],
  ['invalid_message_event', inbound(2, { conversation_id: 'bad id' }), 2],
  ['invalid_target_agent', inbound(2, { target_agent: 42 }), 2],
  ['invalid_outbound_session', outbound(2), 2],
  ['event_in_future', inbound(2, {}, NOW + 600000), 2],
  [null, inbound(2, { target_agent: 'worker', conversation_id: 'bad id' }), null],
];
for (const [reason, row, lastSeq] of badRows) {
  test(`R3: after ${reason || 'an unrelated row'} (last seq ${lastSeq}) a later message is still refused`, t => {
    const f = fixture(t);
    f.append({ ...inbound(1), type: 'fixture.tick' }, row, inbound(3));
    assert.equal(f.hook(write('after')).hookSpecificOutput?.permissionDecision, 'deny');
    const status = f.cmd('status', ['--json']);
    assert.equal(status.skipped_rows?.count, reason ? 1 : 0);
    if (reason) {
      assert.equal(status.skipped_rows.last_seq, lastSeq);
      assert.equal(status.skipped_rows.last_reason, reason);
      assert.equal(status.skipped_rows.last_at, NOW);
      assert.ok(f.cmd('doctor').includes(`rows_skipped 1 last_seq ${lastSeq}`));
    }
  });
}

test('R3: a skipped outbound row releases the waits it could have ended', t => {
  const f = fixture(t);
  f.append(inbound(1), inbound(2, { conversation_id: 'room-b' }), outbound(3));
  const status = (() => { try { return f.cmd('status'); } catch { return {}; } })();
  assert.equal(status.skipped_rows?.count, 1);
  assert.deepEqual(f.state().waiting.map(w => w.released), [true, false]);
  f.append({ ...outbound(4), payload: {} });
  assert.deepEqual(f.hook(write('after')), {});
  assert.ok(f.state().waiting.every(w => w.released));
  f.append(inbound(5), '{');
  assert.deepEqual(f.hook(write('unknown')), {});
});

test('R3: a gap right after an unreadable line is accepted, and any other gap still fails open', t => {
  const f = fixture(t);
  f.append(inbound(1), '{');
  assert.doesNotThrow(() => f.cmd('status'));
  f.append(inbound(5));
  assert.equal(f.cmd('status').skipped_rows.count, 3);
  assert.equal(f.cmd('status').skipped_rows.count, 3);
  f.append(inbound(7));
  assert.deepEqual(f.hook(write('gap')), {});
  assert.throws(() => f.cmd('status'), /event_sequence_gap/);
});

test('R3: rotating a consumed malformed line cannot release a newer wait or count the line twice', t => {
  for (const tail of [false, true]) {
    const f = fixture(t);
    f.append(inbound(1), '{', inbound(3));
    if (tail) f.append('{');
    assert.equal(f.cmd('status').skipped_rows.count, tail ? 2 : 1);
    f.append(inbound(tail ? 5 : 4));
    const last = tail ? 5 : 4;
    const archive = path.join(path.dirname(f.events), 'events.jsonl.archive-20260102T120000Z-seq-1-' + last + '.gz');
    fs.writeFileSync(archive, zlib.gzipSync(fs.readFileSync(f.events)));
    fs.unlinkSync(f.events); fs.writeFileSync(f.events, '');
    assert.equal(f.cmd('status').skipped_rows.count, tail ? 2 : 1);
    assert.deepEqual(f.state().waiting.map(w => w.released), [true, tail, false]);
  }
});
