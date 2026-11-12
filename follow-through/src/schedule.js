import { addDays, daySpans, localDay, parseHours, spanIntervals, wallClock, weekday, zonedToInstant } from './zoned.js';

export const MINUTE = 60_000;
export const HOUR = 60 * MINUTE;
export const DAY = 24 * HOUR;

export function wakesForDay(owner, date) {
  if (!owner.tz) return [];
  if (!owner.hours) {
    if ([0, 6].includes(weekday(date))) return [];
    return [9, 17].map(h => zonedToInstant(date, h * 60, owner.tz));
  }
  const spans = daySpans(parseHours(owner.hours), owner.tz, date);
  if (!spans.length) return [];
  return [...new Set([spans[0].start, spans.at(-1).end - HOUR])].sort((a, b) => a - b);
}

export function wakeAllowed(owner, scheduled, now) {
  if (!owner?.consent || !owner.tz || now < scheduled || now - scheduled > 2 * HOUR) return false;
  const wall = wallClock(now, owner.tz);
  if (wall.hour >= 20) return false;
  if (!owner.hours) {
    // The exact 17:00 wake is allowed; a late retry must be before 17:00.
    return ![0, 6].includes(weekday(localDay(now, owner.tz))) && wall.hour >= 9 &&
      (wall.hour < 17 || now === scheduled && wall.hour === 17 && wall.minute === 0 && wall.second === 0);
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
  for (let n = -1; n <= 8; n++) {
    for (const at of wakesForDay(owner, addDays(date, n))) {
      if (at >= now && wakeAllowed(owner, at, at)) return at;
    }
  }
  return null;
}
