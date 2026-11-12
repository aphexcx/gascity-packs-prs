import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { withState, readState } from '../src/state.js';
import { consent, hours, addItem, setReminder, closeItem } from '../src/model.js';
import { tick, status, doctor } from '../src/engine.js';
import { statusText } from '../src/cli.js';
import { HOUR, MINUTE, DAY, wakesForDay, wakeAllowed } from '../src/schedule.js';
import { parseHours, spanIntervals, wallClock } from '../src/zoned.js';

const at = Date.parse;
const opened = at('2026-11-09T00:00Z');
const afternoon = at('2026-11-10T17:01:30Z');
const morning = at('2026-11-11T09:00Z');
function harness(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-round2-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const mails = new Map();
  return { dir, mails,
    edit: fn => withState(dir, async (s, save) => { fn(s); save(); }),
    send: async r => { mails.set(r.key, structuredClone(r)); return { ok: true }; },
  };
}

test('round2 delivery class: recovery preserves every due item and reminder with one key', async t => {
  // Deterministic generated outages and send durations include the reader's
  // A/A2/A3/E/F, zero-attempt plans, and migration from an old failed raise.
  for (const c of [
    { name: 'one failed afternoon send', fail: true },
    { name: 'plan then outage beyond grace', kill: true },
    { name: 'plan then 25 hour outage', kill: true, outage: 25 * HOUR },
    { name: 'fourteen slow owners', owners: 14, delay: 3000 },
    { name: 'consent off then on', fail: true, revoke: true },
    { name: 'three day sender failure', fail: true, outage: 3 * DAY },
    { name: 'clock jumps across hours', owners: 4, delay: 3 * HOUR },
    { name: 'old failed persisted raises', kill: true, legacy: true },
  ]) await t.test(c.name, async t => {
    const h = harness(t);
    const count = c.owners || 1;
    await h.edit(s => {
      for (let i = 0; i < count; i++) {
        const owner = `slack/invented-owner-${i}`;
        consent(s, owner, true, opened);
        hours(s, owner, { clear: true, tz: 'UTC' });
        addItem(s, owner, 'Invented pending task', opened);
        setReminder(s, owner, 'Invented requested reminder', afternoon, opened);
      }
    });
    let clock = afternoon;
    const first = tick({ dir: h.dir, clock: () => clock,
      send: async r => {
        const result = c.fail ? { ok: false, error: 'invented failure' } : await h.send(r);
        clock += c.delay || 0;
        return result;
      }, afterPersist(phase) { if (c.kill && phase === 'planned') throw new Error('simulated outage'); },
    });
    if (c.kill) await assert.rejects(first, /simulated outage/); else await first;
    const keys = readState(h.dir).raises.map(r => r.key);
    if (c.legacy) await h.edit(s => {
      for (const r of s.raises) r.state = 'failed';
      for (const i of s.items) i.state = 'raised';
      for (const r of s.reminders) r.state = 'raised';
    });
    if (c.revoke) {
      await h.edit(s => { for (const owner of Object.keys(s.owners)) consent(s, owner, false, clock); });
      await tick({ dir: h.dir, now: morning, send: h.send });
      assert.ok(readState(h.dir).items.every(i => i.state === 'open'), 'undelivered items stay open');
      await h.edit(s => { for (const owner of Object.keys(s.owners)) consent(s, owner, true, morning + MINUTE); });
    }
    const recovery = Math.max(clock, afternoon + (c.outage || 0), morning + (c.revoke ? DAY : 0));
    // Recovery is attempted outside hours too: reminders must not wait for a wake.
    await tick({ dir: h.dir, now: recovery, send: h.send });
    assert.equal([...h.mails.values()].filter(r => r.kind === 'reminder').length, count);
    for (let day = 0; day < 8; day++) {
      const now = morning + day * DAY;
      if (now < recovery) continue;
      await tick({ dir: h.dir, now, send: h.send });
      await tick({ dir: h.dir, now: now + MINUTE, send: h.send });
    }
    const state = readState(h.dir);
    const delivered = [...h.mails.values()];
    assert.equal(delivered.filter(r => r.kind === 'follow-up').length, count);
    assert.equal(delivered.flatMap(r => r.item_ids || []).length, count);
    assert.equal(state.raises.length, keys.length, 'recovery must reuse the durable dedup keys');
    assert.deepEqual(state.raises.map(r => r.key), keys);
    assert.ok(state.raises.every(r => r.state === 'sent' && r.attempts > 0));
    assert.equal(status(state, state.last_tick_at).failing_raises.length, 0);
    assert.deepEqual(doctor(state, state.last_tick_at), []);
    if (c.outage) assert.ok(delivered.some(r => r.kind === 'reminder' && /minutes late/.test(r.body)));
  });
});

test('round2 delivery health: closing an undelivered item clears its failed raise', async t => {
  const h = harness(t);
  await h.edit(s => {
    consent(s, 'slack/invented', true, opened);
    hours(s, 'slack/invented', { clear: true, tz: 'UTC' });
    addItem(s, 'slack/invented', 'Invented task', opened);
  });
  await tick({ dir: h.dir, now: afternoon, send: async () => ({ ok: false, error: 'invented failure' }) });
  assert.ok(doctor(readState(h.dir), afternoon).some(p => /raise/.test(p)));
  await h.edit(s => { s.raises[0].state = 'failed'; closeItem(s, s.items[0].id, 'done', afternoon); });
  assert.deepEqual(doctor(readState(h.dir), afternoon), []);
  assert.equal(status(readState(h.dir), afternoon).failing_raises.length, 0);
  await tick({ dir: h.dir, now: morning, send: h.send });
  assert.equal(h.mails.size, 0);
});

test('round2 hours class: generated short, split and midnight spans contain every wake and send', async t => {
  const patterns = ['Mon-Fri 09:00-09:30', 'Mon-Fri 09:00-12:00, 13:00-13:30',
    'Mon-Fri 09:00-17:00, 17:30-18:00', 'daily 18:00-02:00',
    ...[1, 15, 30, 59].map(n => `daily 11:00-11:${String(n).padStart(2, '0')}`)];
  for (const tz of ['UTC', 'America/Chicago', 'Asia/Tokyo']) {
    for (const recorded of patterns) await t.test(`${tz} ${recorded}`, async t => {
      const owner = { tz, hours: recorded, consent: true };
      const date = { year: 2026, month: 11, day: 10 };
      const inside = now => spanIntervals(parseHours(recorded), tz, now, now).some(s => s.start <= now && now < s.end);
      const wakes = wakesForDay(owner, date);
      assert.ok(wakes.length > 0);
      for (const wake of wakes) assert.ok(inside(wake), `wake outside hours: ${new Date(wake).toISOString()}`);
      for (const wake of wakes) for (let minute = 0; minute <= 120; minute++) {
        const now = wake + minute * MINUTE;
        if (!inside(now)) assert.equal(wakeAllowed(owner, wake, now), false, 'gate outside hours');
      }
      const h = harness(t);
      await h.edit(s => { s.owners['slack/invented'] = owner; addItem(s, 'slack/invented', 'Invented task', opened); });
      const sentAt = [];
      for (const now of [...new Set(wakes.flatMap(w => [w, w + MINUTE, w + 30 * MINUTE, w + HOUR]))].sort((a, b) => a - b)) {
        await tick({ dir: h.dir, now, send: async r => { sentAt.push(now); return h.send(r); } });
      }
      assert.ok(sentAt.every(inside), 'send outside hours');
    });
  }
});

test('round2 hours class: a due item cannot be sent at a nominal wake outside its spans', async t => {
  for (const [recorded, instant] of [
    ['Mon-Fri 09:00-09:30', '2026-11-10T08:30Z'],
    ['Mon-Fri 09:00-12:00, 13:00-13:30', '2026-11-10T12:30Z'],
    ['Mon-Fri 09:00-17:00, 17:30-18:00', '2026-11-10T17:00Z'],
  ]) {
    const h = harness(t);
    await h.edit(s => {
      consent(s, 'slack/invented', true, opened);
      hours(s, 'slack/invented', { tz: 'UTC', hours: recorded });
      addItem(s, 'slack/invented', 'Invented due task', opened);
    });
    await tick({ dir: h.dir, now: at(instant), send: h.send });
    assert.equal(h.mails.size, 0, recorded);
    assert.equal(readState(h.dir).items[0].state, 'open');
  }
});

test('round2 night cap: wakes remain on the local day between 08:00 and 20:00', () => {
  for (const tz of ['UTC', 'America/Chicago', 'Asia/Tokyo']) {
    for (const hours of ['daily 00:00-07:59', 'daily 20:00-06:00', 'daily 18:00-02:00', 'daily 01:00-18:00', 'daily 07:00-08:01']) {
      for (const date of [{ year: 2026, month: 3, day: 8 }, { year: 2026, month: 11, day: 1 }]) {
        const wakes = wakesForDay({ tz, hours }, date);
        if (hours.includes('07:59') || hours.includes('20:00-06:00')) assert.deepEqual(wakes, []);
        for (const wake of wakes) {
          const wall = wallClock(wake, tz);
          assert.ok(wall.hour >= 8 && wall.hour < 20);
          assert.equal(wall.day, date.day);
        }
      }
    }
  }
});

test('round2 night-only spans still determine nominal wakes before projection', () => {
  const date = { year: 2026, month: 11, day: 10 };
  for (const [hours, expected] of [
    ['Tue 09:00-09:30, 21:00-23:00', ['09:00', '09:29']],
    ['Tue 09:00-12:00, 21:00-23:00', ['09:00', '11:59']],
    ['Tue 01:00-03:00, 18:00-18:30', ['18:00']],
  ]) assert.deepEqual(wakesForDay({ tz: 'UTC', hours }, date).map(n => new Date(n).toISOString().slice(11, 16)), expected);
});

test('round2 night cap: status and doctor name owners with no daytime hours', async t => {
  const h = harness(t);
  await h.edit(s => {
    consent(s, 'slack/invented-night-owner', true, opened);
    hours(s, 'slack/invented-night-owner', { tz: 'UTC', hours: 'daily 20:00-08:00' });
    addItem(s, 'slack/invented-night-owner', 'Invented task', opened);
    setReminder(s, 'slack/invented-night-owner', 'Invented night reminder', opened + HOUR, opened);
  });
  await tick({ dir: h.dir, now: opened + HOUR, send: h.send });
  assert.equal(h.mails.size, 1, 'requested reminders still arrive at night');
  const state = readState(h.dir);
  assert.match(statusText(status(state, opened + HOUR), state.owners), /Next wake slack\/invented-night-owner: none \(no recorded hours between 08:00 and 20:00\)/);
  assert.deepEqual(doctor(state, opened + HOUR), ['owner has no recorded hours between 08:00 and 20:00: slack/invented-night-owner']);
});

test('round2 mail bodies: owner request, local and UTC times, mayor decides', async t => {
  for (const tz of ['Asia/Tokyo', null]) {
    const h = harness(t);
    await h.edit(s => {
      if (tz) { consent(s, 'slack/invented', true, opened); hours(s, 'slack/invented', { tz, clear: true }); }
      addItem(s, 'slack/invented', 'Invented task', opened);
      setReminder(s, 'slack/invented', 'Invented reminder', opened + MINUTE, opened);
    });
    await tick({ dir: h.dir, now: at('2026-11-10T00:00Z'), send: h.send });
    for (const mail of h.mails.values()) {
      assert.match(mail.body, /UTC/);
      if (tz) assert.match(mail.body, /Asia\/Tokyo/);
      if (mail.kind === 'reminder') assert.match(mail.body.split('\n')[0], /owner asked for this reminder/i);
      assert.match(mail.body.split('\n').at(-1), /mayor decides whether and how to write.*channel.*rules/i);
    }
  }
});
