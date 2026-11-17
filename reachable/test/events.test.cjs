'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const zlib = require('node:zlib');
const { readEvents, reduceEvents, initialState } = require('../reachable.cjs');

const NOW = Date.parse('2026-01-02T12:00:00Z');
const inbound = (seq, extra = {}) => ({ seq, type: 'extmsg.inbound', ts: new Date(NOW - 60000).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', target_agent: 'mayor', ...extra } });
function fixture(t) {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-test-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  fs.mkdirSync(path.join(city, '.gc'));
  const log = path.join(city, '.gc/events.jsonl');
  const write = rows => fs.writeFileSync(log, rows.map(r => JSON.stringify(r) + '\n').join(''));
  return { city, log, write };
}

test('13: cursor reads appended complete rows once and preserves partial rows', t => {
  const f = fixture(t); f.write([inbound(1)]);
  const first = readEvents(f.city, null);
  assert.deepEqual(first.rows.map(r => r.seq), [1]);
  fs.appendFileSync(f.log, JSON.stringify(inbound(2)));
  const partial = readEvents(f.city, first.cursor);
  assert.deepEqual(partial.rows, []);
  fs.appendFileSync(f.log, '\n');
  const second = readEvents(f.city, partial.cursor);
  assert.deepEqual(second.rows.map(r => r.seq), [2]);
  assert.deepEqual(readEvents(f.city, second.cursor).rows, []);
});

test('13: rotation recovers an unread archived row without repeating the cursor row', t => {
  const f = fixture(t); f.write([inbound(1)]);
  const first = readEvents(f.city, null);
  fs.appendFileSync(f.log, JSON.stringify(inbound(2)) + '\n');
  fs.writeFileSync(path.join(f.city, '.gc/events.jsonl.archive-20260102T120000Z-seq-1-2.gz'), zlib.gzipSync(fs.readFileSync(f.log)));
  fs.unlinkSync(f.log); f.write([inbound(3)]);
  assert.deepEqual(readEvents(f.city, first.cursor).rows.map(r => r.seq), [2, 3]);
});

test('11: malformed rows and sequence gaps fail measurement', t => {
  const f = fixture(t); fs.writeFileSync(f.log, 'invalid\n');
  assert.throws(() => readEvents(f.city, null));
  f.write([inbound(1)]); const first = readEvents(f.city, null);
  fs.appendFileSync(f.log, JSON.stringify(inbound(3)) + '\n');
  assert.throws(() => readEvents(f.city, first.cursor));
});

test('3: agent identity survives a handoff; other agents, duplicates and expired rows do not wait', () => {
  const state = initialState();
  const rows = [inbound(1, { target_session: 'session-old' }),
    inbound(2, { conversation_id: 'other-room', target_agent: 'worker' }),
    { ...inbound(3), type: 'extmsg.inbound_duplicate' },
    { ...inbound(4, { conversation_id: 'old-room' }), ts: new Date(NOW - 86400001).toISOString() },
    inbound(5, { target_agent: '', target_session: 'session-new', conversation_id: 'legacy-room' })];
  reduceEvents(state, rows, { now: NOW, agent: 'mayor', session: 'session-new' });
  assert.deepEqual(state.waiting.map(w => w.conversation_id), ['room-example', 'legacy-room']);
});

test('4: only the mayor outbound in the same conversation clears a wait', () => {
  const state = initialState();
  reduceEvents(state, [inbound(1)], { now: NOW, agent: 'mayor', session: 'session-new' });
  const send = session => ({ seq: 2, type: 'extmsg.outbound', ts: new Date(NOW).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', session } });
  reduceEvents(state, [send('worker-session')], { now: NOW, agent: 'mayor', session: 'session-new' });
  assert.equal(state.waiting.length, 1);
  reduceEvents(state, [send('session-new')], { now: NOW, agent: 'mayor', session: 'session-new' });
  assert.equal(state.waiting.length, 0);
});
