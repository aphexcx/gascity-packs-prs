import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { withState, readState } from '../src/state.js';
import { consent, hours, addItem, setReminder } from '../src/model.js';
import { tick, doctor, status } from '../src/engine.js';
import { statusText } from '../src/cli.js';

const start = Date.parse('2026-11-13T17:01:15Z');
const minute = 60_000;
const day = 86_400_000;
const owner = 'slack/invented-owner';
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-recovery-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return { dir, edit: fn => withState(dir, async (s, save) => { fn(s); save(); }) };
}

test('archived delivery is reconciled after a crash or an ambiguous failure', async t => {
  for (const kind of ['follow-up', 'reminder']) for (const failure of ['crash', 'nonzero']) {
    await t.test(`${kind}: ${failure}`, async t => {
      const h = fixture(t);
      await h.edit(s => {
        consent(s, owner, true, start - day);
        hours(s, owner, { clear: true, tz: 'UTC' });
        if (kind === 'follow-up') addItem(s, owner, 'Invented pending task', start - day);
        else setReminder(s, owner, 'Invented reminder', start, start - day);
      });
      const history = new Map();
      let sends = 0;
      const send = async r => { sends++; history.set(r.key, { id: 'invented-mail', status: 'closed' });
        return { ok: failure === 'crash', unknown: true, error: 'mail exit 1' }; };
      const first = tick({ dir: h.dir, now: start, send,
        afterPersist(phase) { if (failure === 'crash' && phase === 'sent') throw new Error('crash'); } });
      if (failure === 'crash') await assert.rejects(first, /crash/); else await first;
      // The inbox is empty: only the archived history proves delivery.
      for (let d = 0; d < 8; d++) await tick({ dir: h.dir,
        now: Date.parse('2026-11-16T09:00:00Z') + d * day, send,
        lookup: async r => ({ ok: true, found: history.has(r.key), id: history.get(r.key)?.id }) });
      assert.equal(sends, 1);
      assert.equal(readState(h.dir).raises[0].state, 'sent');
    });
  }
});

test('night-only hours are informational and notify the mayor once across restarts', async t => {
  const h = fixture(t);
  await h.edit(s => {
    for (const [suffix, on] of [['on', true], ['off', false], ['unset', null]]) {
      const conversation = `${owner}-${suffix}`;
      if (on !== null) consent(s, conversation, on, start);
      hours(s, conversation, { hours: 'daily 20:00-08:00', ...(on ? { tz: 'UTC' } : {}) });
    }
  });
  const mails = [];
  for (let d = 0; d < 8; d++) {
    await tick({ dir: h.dir, now: start + d * day,
      send: async r => { mails.push(structuredClone(r)); return { ok: true }; } });
    assert.deepEqual(doctor(readState(h.dir), start + d * day), []);
  }
  assert.equal(mails.length, 3);
  assert.ok(mails.every(r => r.kind === 'hours-notice' && /no follow-up/.test(r.body)));
  assert.match(statusText(status(readState(h.dir), start), readState(h.dir).owners), /no recorded hours/);
});

test('undeliverable reminders back off, stop, and remain visible to an operator', async t => {
  const h = fixture(t);
  await h.edit(s => setReminder(s, owner, 'PRIVATE INVENTED TEXT', start, start - day));
  const attempts = [];
  for (let m = 0; m <= 90; m++) await tick({ dir: h.dir, now: start + m * minute,
    send: async () => { attempts.push(m); return { ok: false, error: 'mail spawn ENOENT' }; } });
  assert.deepEqual(attempts, [0, 1, 3, 7, 15]);
  let state = readState(h.dir);
  assert.equal(state.raises[0].state, 'stopped');
  assert.match(doctor(state, start + 90 * minute).join('\n'), /delivery stopped.*operator action required/);
  const text = statusText(status(state, start + 90 * minute));
  assert.match(text, /Mail delivery stopped: 1; operator action required/);
  assert.doesNotMatch(text, /PRIVATE INVENTED TEXT/);
  await tick({ dir: h.dir, now: start + 30 * day, send: async () => { throw new Error('must not retry'); } });
  state = readState(h.dir);
  assert.equal(state.raises[0].attempts, 5);
});

test('a deferred follow-up preserves its final attempt for a healthy sender', async t => {
  const h = fixture(t);
  await h.edit(s => {
    consent(s, owner, true, Date.parse('2026-11-09T00:00Z'));
    hours(s, owner, { hours: 'daily 09:00-09:01', tz: 'UTC' });
    addItem(s, owner, 'Invented pending task', Date.parse('2026-11-09T00:00Z'));
  });
  let sends = 0;
  let lookups = 0;
  const send = async () => { sends++; return { ok: sends === 5, unknown: false, error: 'invented spawn failure' }; };
  const lookup = async () => { lookups++; return { ok: true, found: false }; };
  for (const date of ['10', '11', '12', '13']) {
    await tick({ dir: h.dir, now: Date.parse(`2026-11-${date}T09:00Z`), send, lookup });
  }
  const before = readState(h.dir).raises[0];
  const boundary = Date.parse('2026-11-14T09:01Z');
  let reads = 0;
  await tick({ dir: h.dir, clock: () => ++reads <= 2 ? boundary - 100 : boundary, send, lookup });
  const deferred = readState(h.dir);
  const deferredText = statusText(status(deferred, boundary));
  assert.equal(sends, 4);
  assert.equal(lookups, 0);
  await tick({ dir: h.dir, now: Date.parse('2026-11-15T09:00Z'), send, lookup });
  const delivered = readState(h.dir);
  t.diagnostic(`deferred attempts ${deferred.raises[0].attempts}; final state ${delivered.raises[0].state}; total sends ${sends}`);
  assert.equal(deferred.raises[0].attempts, before.attempts);
  assert.equal(deferred.raises[0].last_attempt_at, before.last_attempt_at);
  assert.match(deferredText, /attempts 4;/);
  assert.doesNotMatch(deferredText, /Mail delivery stopped/);
  assert.equal(sends, 5);
  assert.equal(delivered.raises[0].attempts, 5);
  assert.equal(delivered.raises[0].state, 'sent');
  assert.equal(delivered.items[0].state, 'raised');
});

test('wake-boundary deferrals preserve attempts and backoff with or without history lookup', async t => {
  const cases = [
    { name: 'hours close', hours: 'daily 09:00-09:01', boundary: '2026-11-10T09:01:00Z' },
    { name: 'two-hour grace', hours: 'daily 09:00-23:00', boundary: '2026-11-10T11:00:00.001Z' },
    { name: 'default afternoon cutoff', hours: null, boundary: '2026-11-10T17:02:00Z' },
    { name: 'night cap', hours: 'daily 18:30-23:00', boundary: '2026-11-10T20:00:00Z' },
  ];
  for (const c of cases) for (const history of [false, true]) {
    await t.test(`${c.name}: ${history ? 'not-found lookup' : 'no lookup'}`, async t => {
      const h = fixture(t);
      const boundary = Date.parse(c.boundary);
      await h.edit(s => {
        consent(s, owner, true, boundary - 2 * day);
        hours(s, owner, c.hours ? { hours: c.hours, tz: 'UTC' } : { clear: true, tz: 'UTC' });
        addItem(s, owner, 'Invented pending task', boundary - 2 * day);
      });
      let sends = 0;
      let lookups = 0;
      const send = async () => { sends++; return { ok: false, unknown: true, error: 'invented uncertain send' }; };
      const lookup = async () => { lookups++; return { ok: true, found: false }; };
      if (history) await tick({ dir: h.dir, now: boundary - 100 - day, send, lookup });
      const before = readState(h.dir).raises[0] ?? { attempts: 0, last_attempt_at: null };
      const sendsBefore = sends;
      let reads = 0;
      await tick({ dir: h.dir, clock: () => ++reads <= 2 ? boundary - 100 : boundary, send, lookup });
      const deferred = readState(h.dir).raises[0];
      assert.equal(sends, sendsBefore, 'the boundary must prevent sending');
      assert.equal(lookups, history ? 1 : 0);
      assert.equal(deferred.state, 'pending');
      assert.equal(deferred.attempts, before.attempts, 'deferral must not spend an attempt');
      assert.equal(deferred.last_attempt_at, before.last_attempt_at, 'deferral must not restart backoff');
      if (history) assert.equal(deferred.delivery_unknown, false, 'a completed lookup must stay reconciled');
      await tick({ dir: h.dir, now: boundary - 100 + day,
        send: async () => { sends++; return { ok: true }; }, lookup });
      const delivered = readState(h.dir).raises[0];
      assert.equal(sends, sendsBefore + 1);
      assert.equal(lookups, history ? 1 : 0, 'a refunded lookup must not repeat');
      assert.equal(delivered.attempts, before.attempts + 1);
      assert.equal(delivered.state, 'sent');
    });
  }
});

test('five real failed follow-up sends still back off and stop visibly', async t => {
  for (const unknown of [false, true]) await t.test(`delivery unknown: ${unknown}`, async t => {
    const h = fixture(t);
    const morning = Date.parse('2026-11-10T09:00Z');
    await h.edit(s => {
      consent(s, owner, true, morning - day);
      hours(s, owner, { hours: 'daily 09:00-20:00', tz: 'UTC' });
      addItem(s, owner, 'Invented pending task', morning - day);
    });
    const attempts = [];
    let lookups = 0;
    for (let m = 0; m <= 30; m++) await tick({ dir: h.dir, now: morning + m * minute,
      send: async () => { attempts.push(m); return { ok: false, unknown, error: 'invented send failure' }; },
      lookup: async () => { lookups++; return { ok: true, found: false }; } });
    const state = readState(h.dir);
    assert.deepEqual(attempts, [0, 1, 3, 7, 15]);
    assert.equal(lookups, unknown ? 4 : 0);
    assert.equal(state.raises[0].attempts, 5);
    assert.equal(state.raises[0].state, 'stopped');
    assert.match(doctor(state, morning + 30 * minute).join('\n'), /delivery stopped.*operator action required/);
    const text = statusText(status(state, morning + 30 * minute));
    assert.match(text, /Mail delivery stopped: 1; operator action required/);
    assert.match(text, /stopped; attempts 5;/);
    let laterSends = 0;
    await tick({ dir: h.dir, now: morning + day, send: async () => { laterSends++; return { ok: true }; } });
    assert.equal(laterSends, 0);
  });
});
