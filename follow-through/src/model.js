import { randomUUID } from 'node:crypto';
import { assertZone, parseHours } from './zoned.js';
import { DAY, HOUR, MINUTE } from './schedule.js';

function ownerFor(state, conversation) {
  if (typeof conversation !== 'string' || !/^[A-Za-z0-9._-]+\/[^\s\x00-\x1f\x7f]+$/.test(conversation)) {
    throw new Error('conversation must be provider/conversation-id');
  }
  return state.owners[conversation] ??= { consent: null, consent_at: null, hours: null, tz: null };
}

function textValue(value, max, singleLine) {
  const text = typeof value === 'string' ? value.trim() : '';
  if (!text || [...text].length > max) throw new Error(`text must contain 1 to ${max} characters`);
  if (singleLine && /[\r\n\u2028\u2029]/.test(text)) throw new Error('summary must be one line');
  if (/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(text)) throw new Error('text contains control characters');
  return text;
}

export function consent(state, conversation, on, now) {
  if (typeof on !== 'boolean') throw new Error('consent needs --on or --off');
  Object.assign(ownerFor(state, conversation), { consent: on, consent_at: now });
}

export function hours(state, conversation, options) {
  if (options.clear && options.hours !== undefined) throw new Error('--clear and --hours are exclusive');
  if (!options.clear && options.hours === undefined && options.tz === undefined) throw new Error('give --hours, --clear or --tz');
  if (options.hours !== undefined) parseHours(options.hours);
  const zone = options.tz === undefined ? undefined : assertZone(options.tz);
  const owner = ownerFor(state, conversation);
  if (options.clear) owner.hours = null;
  else if (options.hours !== undefined) owner.hours = options.hours;
  if (zone !== undefined) owner.tz = zone;
}

export function addItem(state, conversation, summary, now) {
  const value = textValue(summary, 200, true);
  ownerFor(state, conversation);
  const item = { id: `item-${randomUUID()}`, conversation, summary: value, opened_at: now, state: 'open' };
  state.items.push(item);
  return item;
}

export function closeItem(state, id, outcome, now) {
  const item = state.items.find(i => i.id === id);
  if (!item) throw new Error(`unknown item: ${id}`);
  if (!['done', 'dropped'].includes(outcome)) throw new Error('invalid item outcome');
  if (item.state === outcome) return;
  if (['done', 'dropped'].includes(item.state)) throw new Error(`item is already ${item.state}: ${id}`);
  item.state = outcome;
  item.closed_at = now;
}

export function parseAt(text) {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.\d{1,3})?(Z|[+-]\d{2}:\d{2})$/i.exec(text);
  const ms = Date.parse(text);
  const fail = () => { throw new Error('time must be valid RFC 3339 with an offset (for example 2026-11-12T09:00:00-06:00)'); };
  if (!match || !Number.isFinite(ms)) return fail();
  const [, year, month, day, hour, minute, second, offset] = match;
  const days = new Date(Date.UTC(Number(year), Number(month), 0)).getUTCDate();
  if (+month < 1 || +month > 12 || +day < 1 || +day > days || +hour > 23 || +minute > 59 || +second > 59 ||
      offset !== 'Z' && offset !== 'z' && (+offset.slice(1, 3) > 23 || +offset.slice(4) > 59)) return fail();
  return ms;
}

export function parseDuration(text) {
  const match = /^(\d+(?:\.\d+)?)\s*(m|min|minute|minutes|h|hour|hours|d|day|days)$/i.exec(text);
  const units = { m: MINUTE, h: HOUR, d: DAY };
  const ms = match ? +match[1] * units[match[2][0].toLowerCase()] : NaN;
  if (!Number.isSafeInteger(ms) || ms <= 0) throw new Error('duration must be positive minutes, hours or days (30m, 2h, 1d)');
  return ms;
}

export function setReminder(state, conversation, text, due, now) {
  const value = textValue(text, 500, false);
  if (!Number.isFinite(due) || !Number.isFinite(new Date(due).getTime()) || due <= now) throw new Error('reminder time must be in the future');
  ownerFor(state, conversation);
  const reminder = { id: `reminder-${randomUUID()}`, conversation, text: value, due_at: due, set_at: now, state: 'set' };
  state.reminders.push(reminder);
  return reminder;
}

export function cancelReminder(state, id, now) {
  const reminder = state.reminders.find(r => r.id === id);
  if (!reminder) throw new Error(`unknown reminder: ${id}`);
  if (reminder.state === 'cancelled') return;
  if (reminder.state !== 'set') throw new Error(`reminder has already been raised: ${id}`);
  reminder.state = 'cancelled';
  reminder.cancelled_at = now;
}
