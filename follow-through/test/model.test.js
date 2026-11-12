import test from 'node:test';
import assert from 'node:assert/strict';
import { readState } from '../src/state.js';
import { consent, hours, addItem, closeItem, setReminder, cancelReminder, parseAt, parseDuration } from '../src/model.js';

const fresh = () => readState('/nonexistent-follow-through-test');
const conversation = 'slack/example';
const now = Date.parse('2026-11-10T09:00:00Z');

test('5,6,11: consent defaults off; hours set, clear, retain/set zone; invalid changes are atomic', () => {
  const s = fresh();
  addItem(s, conversation, 'Invented question', now);
  assert.equal(s.owners[conversation].consent, null);
  assert.equal(s.owners[conversation].tz, null);
  consent(s, conversation, true, now);
  hours(s, conversation, { hours: 'Mon-Fri 09:00-18:00', tz: 'America/Chicago' });
  hours(s, conversation, { clear: true });
  assert.equal(s.owners[conversation].hours, null);
  assert.equal(s.owners[conversation].tz, 'America/Chicago');
  hours(s, conversation, { clear: true, tz: 'Asia/Tokyo' });
  assert.equal(s.owners[conversation].tz, 'Asia/Tokyo');
  const before = structuredClone(s);
  assert.throws(() => hours(s, conversation, { hours: 'daily 09:00-18:00', tz: 'Mars/Crater' }), /Mars\/Crater/);
  assert.deepEqual(s, before);
  consent(s, conversation, false, now + 1);
  assert.equal(s.owners[conversation].consent, false);
});

test('7: item IDs, length, single lines, conversation form and retained terminal records', () => {
  const s = fresh();
  const one = addItem(s, conversation, 'A'.repeat(200), now);
  const two = addItem(s, conversation, 'Invented second', now);
  assert.notEqual(one.id, two.id);
  assert.equal(one.state, 'open');
  assert.throws(() => addItem(s, conversation, 'a'.repeat(201), now), /200/);
  assert.throws(() => addItem(s, conversation, 'two\nlines', now), /line/);
  assert.throws(() => addItem(s, 'invalid', 'text', now), /provider\/conversation/);
  closeItem(s, one.id, 'done', now + 1);
  closeItem(s, two.id, 'dropped', now + 2);
  assert.equal(s.items.length, 2);
  assert.equal(one.closed_at, now + 1);
  assert.equal(two.state, 'dropped');
  assert.throws(() => closeItem(s, 'missing', 'done', now), /unknown item/);
});

test('15: RFC3339 offset and minutes/hours/days; cancellation retains the record', () => {
  const s = fresh();
  assert.equal(parseAt('2026-11-10T12:00:00+02:00'), now + 3600000);
  assert.equal(parseAt('2026-11-10T12:00:00.123456+02:00'), now + 3600000 + 123);
  for (const [text, ms] of [['30m', 1800000], ['2 hours', 7200000], ['1 day', 86400000]]) {
    assert.equal(parseDuration(text), ms);
  }
  for (const bad of ['2026-11-10T12:00:00', 'tomorrow', '2026-02-30T12:00:00Z']) assert.throws(() => parseAt(bad), /RFC 3339/);
  for (const bad of ['0m', '-1h', 'tomorrow', 'Infinity days']) assert.throws(() => parseDuration(bad), /duration/);
  assert.throws(() => setReminder(s, conversation, 'old', now, now), /future/);
  assert.throws(() => setReminder(s, conversation, 'x'.repeat(501), now + 1, now), /500/);
  const reminder = setReminder(s, conversation, 'Invented reminder', now + 60000, now);
  cancelReminder(s, reminder.id, now + 1);
  assert.equal(reminder.state, 'cancelled');
  assert.equal(reminder.cancelled_at, now + 1);
  assert.throws(() => cancelReminder(s, 'missing', now), /unknown reminder/);
});
