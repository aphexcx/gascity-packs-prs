import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { parseHours, assertZone, zonedToInstant } from '../src/zoned.js';
import { wakesForDay, wakeAllowed, nextWake } from '../src/schedule.js';

const day = { year: 2026, month: 11, day: 10 };
const iso = n => new Date(n).toISOString();
export const wakeCases = [
  ['Asia/Tokyo', null, day, ['2026-11-10T00:00:00.000Z', '2026-11-10T08:00:00.000Z']],
  ['America/Chicago', null, day, ['2026-11-10T15:00:00.000Z', '2026-11-10T23:00:00.000Z']],
  ['UTC', 'Mon-Fri 09:00-12:00, 13:00-18:00', day, ['2026-11-10T09:00:00.000Z', '2026-11-10T17:00:00.000Z']],
  ['UTC', 'Mon-Fri 18:00-02:00', day, ['2026-11-10T18:00:00.000Z', '2026-11-11T01:00:00.000Z']],
  ['America/Chicago', 'daily 01:00-18:00', { year: 2026, month: 3, day: 8 }, ['2026-03-08T07:00:00.000Z', '2026-03-08T22:00:00.000Z']],
  ['America/Chicago', 'daily 01:00-18:00', { year: 2026, month: 11, day: 1 }, ['2026-11-01T06:00:00.000Z', '2026-11-01T23:00:00.000Z']],
];

test('5,9: two wake instants across split hours, midnight, east/west zones and DST', () => {
  for (const [tz, hours, date, expected] of wakeCases) {
    assert.deepEqual(wakesForDay({ tz, hours }, date).map(iso), expected);
  }
  assert.deepEqual(wakesForDay({ tz: 'UTC', hours: null }, { year: 2026, month: 11, day: 14 }), []);
  if (process.env.FOLLOW_THROUGH_TEST_EVIDENCE_DIR) {
    const evidence = process.env.FOLLOW_THROUGH_TEST_EVIDENCE_DIR;
    fs.mkdirSync(evidence, { recursive: true });
    const rows = wakeCases.map(([tz, hours, date, expected]) => `| ${tz} | ${hours || 'default'} | ${date.year}-${date.month}-${date.day} | ${expected.join(' | ')} |`);
    fs.writeFileSync(path.join(evidence, 'wake-times.md'), '# Wake times proved by schedule.test.js\n\n| Zone | Hours | Local opening day | First UTC instant | Second UTC instant |\n| --- | --- | --- | --- | --- |\n' + rows.join('\n') + '\n');
  }
});

test('5: keeper hours grammar and zone validation copied with their edge cases', () => {
  assert.deepEqual(parseHours('18:00-02:00'), [{ days: [0,1,2,3,4,5,6], open: 1080, close: 120 }]);
  assert.equal(parseHours('Mon-Fri 09:00-12:00, 13:00-18:00').length, 2);
  assert.equal(parseHours('fri-mon 20:00-24:00')[0].close, 1440);
  for (const bad of ['', 'Funday 09:00-10:00', 'Mon 09:00-09:00', 'Mon 25:00-26:00', 'Mon 09:00-18:60']) {
    assert.throws(() => parseHours(bad), /hours/);
  }
  assert.throws(() => assertZone('Mars/Olympus_Mons'), /Mars\/Olympus_Mons/);
  assert.equal(iso(zonedToInstant({ year: 2026, month: 3, day: 8 }, 150, 'America/Chicago')), '2026-03-08T08:30:00.000Z');
  assert.equal(iso(zonedToInstant({ year: 2026, month: 11, day: 1 }, 90, 'America/Chicago')), '2026-11-01T06:30:00.000Z');
});

test('6,10: no zone, no consent, grace expiry, closed hours and 20:00 stop wakes', () => {
  const owner = { consent: true, tz: 'UTC', hours: 'Mon-Fri 09:00-23:00' };
  const at = Date.parse('2026-11-10T09:00:00Z');
  assert.equal(wakeAllowed(owner, at, at + 2 * 3600000), true);
  assert.equal(wakeAllowed(owner, at, at + 2 * 3600000 + 1), false);
  assert.equal(wakeAllowed(owner, at, at - 1), false);
  assert.equal(wakeAllowed({ ...owner, consent: false }, at, at), false);
  assert.equal(wakeAllowed({ ...owner, tz: null }, at, at), false);
  assert.equal(wakeAllowed(owner, Date.parse('2026-11-10T22:00Z'), Date.parse('2026-11-10T22:00Z')), false);
  assert.equal(wakeAllowed({ ...owner, hours: null }, Date.parse('2026-11-10T17:00Z'), Date.parse('2026-11-10T17:00Z')), true);
  assert.equal(wakeAllowed({ ...owner, hours: null }, Date.parse('2026-11-10T17:00Z'), Date.parse('2026-11-10T17:00:01Z')), true);
  assert.equal(nextWake({ ...owner, tz: null }, at), null);
});

test('9,10 ruling: on time for <120s, then late policy; 20:00 always wins', () => {
  const scheduled = Date.parse('2026-11-10T17:00:00Z');
  for (const hours of [null, 'Mon-Fri 09:00-18:00']) {
    const owner = { consent: true, tz: 'UTC', hours };
    for (const seconds of [1, 59, 90, 119]) assert.equal(wakeAllowed(owner, scheduled, scheduled + seconds * 1000), true);
    for (const seconds of [120, 600]) assert.equal(wakeAllowed(owner, scheduled, scheduled + seconds * 1000), hours !== null);
  }
  const owner = { consent: true, tz: 'UTC', hours: 'daily 09:00-23:00' };
  const evening = Date.parse('2026-11-10T20:00:00Z');
  assert.equal(wakeAllowed(owner, evening, evening), false);
  assert.equal(wakeAllowed(owner, evening, evening + 90000), false);
});

test('18: next wake is the earliest instant across overlapping opening days', () => {
  const owner = { consent: true, tz: 'UTC', hours: 'Mon 18:00-03:00, Tue 00:00-02:00' };
  assert.equal(iso(nextWake(owner, Date.parse('2026-11-09T23:30Z'))), '2026-11-10T00:00:00.000Z');
});
