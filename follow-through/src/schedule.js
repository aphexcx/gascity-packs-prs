import { addDays, daySpans, localDay, parseHours, spanIntervals, wallClock, weekday, zonedToInstant } from './zoned.js';

export const MINUTE = 60_000;
export const HOUR = 60 * MINUTE;
export const DAY = 24 * HOUR;
export const ON_TIME_MS = 120_000;

export function wakesForDay(owner, date) {
  if (!owner.tz) return [];
  if (!owner.hours) {
    if ([0, 6].includes(weekday(date))) return [];
    return [9, 17].map(h => zonedToInstant(date, h * 60, owner.tz));
  }
  const start = zonedToInstant(date, 8 * 60, owner.tz);
  const end = zonedToInstant(date, 20 * 60, owner.tz);
  const hours = parseHours(owner.hours);
  const spans = daySpans(hours, owner.tz, date);
  if (!spans.length) return [];
  const allowed = spanIntervals(hours, owner.tz, start, end - 1)
    .map(s => ({ start: Math.max(start, s.start), end: Math.min(end, s.end) }))
    .filter(s => s.start < s.end);
  if (!allowed.length) return [];
  // Hours have minute precision. Move either nominal wake to the nearest
  // allowed minute, preferring the earlier minute on a tie; coalesce twins.
  const nearest = target => allowed.map(s => Math.max(s.start, Math.min(target, s.end - MINUTE)))
    .sort((a, b) => Math.abs(a - target) - Math.abs(b - target) || a - b)[0];
  return [...new Set([nearest(spans[0].start), nearest(spans.at(-1).end - HOUR)])].sort((a, b) => a - b);
}

export function hasDaytimeHours(owner) {
  if (!owner.hours) return true;
  return parseHours(owner.hours).some(s => s.close > s.open
    ? s.open < 20 * 60 && s.close > 8 * 60
    : s.open < 20 * 60 || s.close > 8 * 60);
}

export function wakeAllowed(owner, scheduled, now, onTimeMs = ON_TIME_MS) {
  if (!owner?.consent || !owner.tz || now < scheduled || now - scheduled > 2 * HOUR) return false;
  const wall = wallClock(now, owner.tz);
  if (wall.hour < 8 || wall.hour >= 20) return false;
  if (!owner.hours) {
    if (now - scheduled < onTimeMs) return true;
    return ![0, 6].includes(weekday(localDay(now, owner.tz))) && wall.hour >= 9 && wall.hour < 17;
  }
  return spanIntervals(parseHours(owner.hours), owner.tz, now, now)
    .some(s => s.start <= now && now < s.end);
}

export function recentWakes(owner, now) {
  if (!owner.tz) return [];
  const date = localDay(now, owner.tz);
  return [-1, 0].flatMap(n => wakesForDay(owner, addDays(date, n)))
    .filter(at => wakeAllowed(owner, at, now)).sort((a, b) => a - b);
}

export function nextWake(owner, now) {
  if (!owner.consent || !owner.tz) return null;
  const date = localDay(now, owner.tz);
  const candidates = [];
  for (let n = -1; n <= 8; n++) {
    for (const at of wakesForDay(owner, addDays(date, n))) {
      if (at >= now && wakeAllowed(owner, at, at)) candidates.push(at);
    }
  }
  return candidates.length ? Math.min(...candidates) : null;
}
