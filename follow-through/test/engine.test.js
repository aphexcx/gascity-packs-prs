import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { withState, readState } from '../src/state.js';
import { consent, hours, addItem, setReminder, cancelReminder } from '../src/model.js';
import { tick, status, doctor } from '../src/engine.js';
import { statusText } from '../src/cli.js';

const at = text => Date.parse(text);
const conversation = 'slack/example';
const start = at('2026-11-09T20:00:00Z');

function harness(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const mails = [];
  return { dir, mails, send: async raise => { mails.push(structuredClone(raise)); return { ok: true }; },
    edit: fn => withState(dir, async (s, save) => { fn(s); save(); }) };
}

test('8,9: every due item in one wake; never raised twice across thirty days', async t => {
  const h = harness(t);
  await h.edit(s => {
    consent(s, conversation, true, start);
    hours(s, conversation, { clear: true, tz: 'UTC' });
    addItem(s, conversation, 'Invented first', start);
    addItem(s, conversation, 'Invented second', start);
    addItem(s, conversation, 'Too young for morning', start + 2 * 3600000);
  });
  for (let day = 0; day < 30; day++) {
    for (const hour of ['09', '17']) await tick({ dir: h.dir, now: at(`2026-11-10T${hour}:00:00Z`) + day * 86400000, send: h.send });
  }
  assert.equal(h.mails.length, 2);
  assert.equal(h.mails[0].item_ids.length, 2);
  assert.equal(h.mails[1].item_ids.length, 1);
  assert.equal(new Set(h.mails.flatMap(r => r.item_ids)).size, 3);
  assert.equal(readState(h.dir).items.filter(i => i.state === 'raised').length, 3);
});

test('9,10,11: empty wakes, no zone, off between wakes, outage grace and 20:00 cutoff', async t => {
  const h = harness(t);
  await h.edit(s => { consent(s, conversation, true, start); hours(s, conversation, { hours: 'daily 09:00-23:00', tz: 'UTC' }); });
  await tick({ dir: h.dir, now: at('2026-11-10T09:00Z'), send: h.send });
  assert.equal(h.mails.length, 0);
  await h.edit(s => { addItem(s, conversation, 'Invented pending', start); });
  await tick({ dir: h.dir, now: at('2026-11-10T11:01Z'), send: h.send });
  await tick({ dir: h.dir, now: at('2026-11-10T22:00Z'), send: h.send });
  assert.equal(h.mails.length, 0);
  await h.edit(s => consent(s, conversation, false, start));
  await tick({ dir: h.dir, now: at('2026-11-11T09:00Z'), send: h.send });
  assert.equal(readState(h.dir).items[0].state, 'open');
  await h.edit(s => consent(s, conversation, true, start));
  await tick({ dir: h.dir, now: at('2026-11-12T10:59Z'), send: h.send });
  assert.equal(h.mails.length, 1);
  await h.edit(s => { s.owners[conversation].tz = null; addItem(s, conversation, 'No zone', start); });
  await tick({ dir: h.dir, now: at('2026-11-13T09:00Z'), send: h.send });
  assert.equal(h.mails.length, 1);
});

test('9,10 ruling: default afternoon wake arrives within 120s or waits for morning', async t => {
  for (const offset of [1, 59, 90, 119, 120, 600]) {
    const h = harness(t);
    await h.edit(s => {
      consent(s, conversation, true, start);
      hours(s, conversation, { clear: true, tz: 'UTC' });
      addItem(s, conversation, 'Invented afternoon question', start);
    });
    await tick({ dir: h.dir, now: at('2026-11-10T17:00Z') + offset * 1000, send: h.send });
    assert.equal(h.mails.length, offset < 120 ? 1 : 0);
    if (offset >= 120) assert.equal(readState(h.dir).items[0].state, 'open');
    await tick({ dir: h.dir, now: at('2026-11-11T09:00Z'), send: h.send });
    assert.equal(h.mails.length, 1);
  }
});

test('3,14,15,16: reminders are durable, independent of hours/consent, cancelled ones never fire', async t => {
  const h = harness(t);
  await h.edit(s => {
    setReminder(s, conversation, 'Invented late reminder', start + 60000, start);
    const cancel = setReminder(s, conversation, 'Cancelled text', start + 60000, start);
    cancelReminder(s, cancel.id, start);
  });
  await tick({ dir: h.dir, now: start + 10 * 60000, send: async r => {
    assert.equal(readState(h.dir).reminders[0].state, 'raised');
    assert.equal(readState(h.dir).raises[0].attempts, 1);
    return h.send(r);
  } });
  await tick({ dir: h.dir, now: start + 10 * 60000 + 1, send: h.send });
  assert.equal(h.mails.length, 1);
  assert.equal(h.mails[0].subject, 'follow-through: reminder due');
  assert.match(h.mails[0].body, /This reminder is 9 minutes late\./);
  assert.match(h.mails[0].body, /^Reminder due in slack\/example\./);
  assert.match(h.mails[0].body, /Read the follow-through skill before you write to the owner\.$/);
});

test('3,13: failed mail retries once a minute with the same key; expires at 24h', async t => {
  const h = harness(t);
  await h.edit(s => setReminder(s, conversation, 'Invented retry', start + 1, start));
  const attempts = [];
  const send = async r => { attempts.push(r.key); return { ok: attempts.length === 4, error: 'exit 1' }; };
  for (let minute = 0; minute < 4; minute++) {
    await tick({ dir: h.dir, now: start + 1 + minute * 60000, send });
    await tick({ dir: h.dir, now: start + 2 + minute * 60000, send });
  }
  assert.equal(attempts.length, 4);
  assert.equal(new Set(attempts).size, 1);
  assert.equal(readState(h.dir).raises[0].state, 'sent');
  await h.edit(s => setReminder(s, conversation, 'Invented permanent failure', start + 10 * 60000, start));
  await tick({ dir: h.dir, now: start + 10 * 60000, send: async () => ({ ok: false, error: 'exit 1' }) });
  await tick({ dir: h.dir, now: start + 10 * 60000 + 86400000, send: h.send });
  assert.equal(readState(h.dir).raises[1].state, 'failed');
  assert.equal(h.mails.length, 0);
});

test('11,13: revoking consent also blocks a pending follow-up retry', async t => {
  const h = harness(t);
  await h.edit(s => {
    consent(s, conversation, true, start);
    hours(s, conversation, { clear: true, tz: 'UTC' });
    addItem(s, conversation, 'Invented retry', start);
  });
  await tick({ dir: h.dir, now: at('2026-11-10T09:00Z'), send: async () => ({ ok: false, error: 'exit 1' }) });
  await h.edit(s => consent(s, conversation, false, start));
  await tick({ dir: h.dir, now: at('2026-11-10T09:01Z'), send: h.send });
  assert.equal(h.mails.length, 0);
});

test('10: recheck the wall clock before each mail when earlier delivery takes time', async t => {
  const h = harness(t);
  await h.edit(s => {
    for (const c of ['slack/first', 'slack/second']) {
      consent(s, c, true, start);
      hours(s, c, { hours: 'daily 09:00-20:59', tz: 'UTC' });
      addItem(s, c, 'Invented evening follow-up', start);
    }
  });
  let clock = at('2026-11-10T19:59:00Z');
  await tick({ dir: h.dir, clock: () => clock, send: async r => {
    await h.send(r);
    clock = at('2026-11-10T20:00:00Z');
    return { ok: true };
  } });
  assert.equal(h.mails.length, 1);
});

test('18,19: status has exact safe fields, doctor reports stale tick, missing zone and failed raises', async t => {
  const h = harness(t);
  await h.edit(s => {
    consent(s, conversation, true, start);
    addItem(s, conversation, 'SECRET SUMMARY', start);
    setReminder(s, conversation, 'SECRET REMINDER', start + 1000, start);
  });
  const snapshot = status(readState(h.dir), start);
  assert.deepEqual(snapshot, {
    owners: 1, consent_on: 1, consent_on_without_zone: 1, owners_without_zone: [conversation],
    items: { open: 1, raised: 0, closed: 0 }, reminders_set: 1,
    next_wakes: [{ conversation, at: null }], last_tick_at: null, failing_raises: [],
  });
  assert.doesNotMatch(JSON.stringify(snapshot), /SECRET/);
  assert.equal(statusText(snapshot), [
    'Owners: 1; consent on: 1; consenting without zone: 1',
    'Owners without zone: slack/example', 'Items: 1 open, 0 raised, 0 closed',
    'Reminders set: 1', 'Next wake slack/example: none', 'Last tick: never', 'Failing raises: 0',
  ].join('\n'));
  assert.deepEqual(doctor(readState(h.dir), start), ['tick has not run in the last five minutes', `consenting owner has no zone: ${conversation}`]);
  await tick({ dir: h.dir, now: start + 1000, send: async () => ({ ok: false, error: 'exit 1' }) });
  await tick({ dir: h.dir, now: start + 1000 + 86400000, send: h.send });
  assert.match(doctor(readState(h.dir), start + 1000 + 86400000).join('\n'), /raise failed/);
  assert.doesNotMatch(JSON.stringify(status(readState(h.dir), start)), /SECRET/);
});
