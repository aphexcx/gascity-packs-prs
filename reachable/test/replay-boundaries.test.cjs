'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const zlib = require('node:zlib');
const { LIMITS } = require('../reachable.cjs');
const { fixture, inbound, NOW, write } = require('../test-support/fixture.cjs');

function appendLongestRow(f) {
  const row = { ...inbound(2), padding: '' };
  row.padding = 'x'.repeat(8 * 1024 * 1024 - Buffer.byteLength(JSON.stringify(row)) - 1);
  f.append(row);
}
const tick = seq => ({ ...inbound(seq), type: 'example.tick' });

test('R2-3: cursor advances past a maximum-size complete row', t => {
  const f = fixture(t);
  f.append(tick(1));
  f.cmd('status');
  appendLongestRow(f);
  assert.equal(f.cmd('status').caught_up, true);
  f.append(tick(3));
  f.cmd('status');
  assert.equal(f.cmd('status').caught_up, true);
  assert.equal(f.state().cursor.seq, 3);
});

test('R2-3: Stop includes a maximum-size complete row in its fence', t => {
  const f = fixture(t);
  f.append(tick(1));
  f.cmd('status');
  appendLongestRow(f);
  f.hook({ hook_event_name: 'Stop', prompt_id: 'example-turn' });
  assert.equal(f.state().seenThrough, 2);
  assert.deepEqual(f.state().waiting, []);
});

for (const source of ['other inbound', 'mayor inbound', 'archive tail']) {
  test(`R2-4: an inferred gap releases older waits after ${source}`, t => {
    const f = fixture(t);
    const target_agent = source === 'mayor inbound' ? 'mayor' : 'other';
    f.append(inbound(1), inbound(undefined, { target_agent }));
    if (source === 'archive tail') {
      const archive = path.join(path.dirname(f.events), 'events.jsonl.archive-20260102T120000Z-seq-1-3.gz');
      fs.writeFileSync(archive, zlib.gzipSync(fs.readFileSync(f.events)));
      fs.writeFileSync(f.events, '');
    } else f.append(tick(4));
    assert.deepEqual(f.hook(write('after')), {});
    const { waiting, skipped } = f.state();
    assert.equal(waiting[0].released, true);
    assert.deepEqual(skipped, { count: source === 'mayor inbound' ? 2 : 1, last_seq: 1, last_reason: 'gap', last_at: NOW });
  });
}

for (const equalTimes of [false, true]) {
  test(`R2-7: recent wake survives replay eviction with ${equalTimes ? 'equal' : 'different'} timestamps`, t => {
    const f = fixture(t);
    const wake = (seq, session_id, at) => ({
      seq, ts: new Date(at).toISOString(), type: 'session.woke', subject: 'mayor', session_id,
    });
    let seq = 1;
    const old = NOW - 240000;
    const rows = [wake(seq++, 'session-a', old)];
    for (let i = 0; i < LIMITS.sessions; i++) rows.push(wake(seq++, 'session-other-' + i, old));
    rows.push(wake(seq++, 'session-a', equalTimes ? old : NOW - 120000), inbound(seq++));
    f.append(...rows);
    assert.equal(f.cmd('status').waiting.length, 1);
    f.append({ ...inbound(seq), type: 'extmsg.outbound', subject: 'session-a' });
    assert.deepEqual(f.hook(write('after')), {});
    assert.deepEqual(f.state().waiting, []);
  });
}
