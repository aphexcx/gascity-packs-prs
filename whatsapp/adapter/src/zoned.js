// zoned.js — wall clocks in IANA time zones, and business hours, for the
// window-keeper (keeper.js; the design's section "The window-keeper", items
// (b) and (c)). No dependencies.
//
// An instant's wall clock in a zone comes from Intl.DateTimeFormat's
// formatToParts. The reverse, a wall time in a zone to an instant, is offset
// correction: read the wall time as if it were UTC, subtract a UTC offset
// of the zone, and keep the result only when the zone's offset AT that
// result is the one subtracted. The offsets tried are the one the first
// correction lands on and the ones in force a day either side, which
// covers every DST night:
//   - a fold (01:30 on the fall-back night happens twice): two candidates
//     agree; the earlier wins;
//   - a gap (02:30 on the spring-forward night never happens): none agrees;
//     the offset from before the gap moves the time forward by the gap's
//     length (03:30), so an update hour or a span edge is never lost.

const DAY = 86_400_000;

const formatters = new Map();

function formatterFor(tz) {
  let f = formatters.get(tz);
  if (!f) {
    f = new Intl.DateTimeFormat('en-US', {
      timeZone: tz,
      hourCycle: 'h23',
      year: 'numeric',
      month: '2-digit',
      day: '2-digit',
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
    });
    formatters.set(tz, f);
  }
  return f;
}

// assertZone returns the zone's canonical name, or throws a message an
// operator can act on.
export function assertZone(tz) {
  const name = typeof tz === 'string' ? tz.trim() : '';
  if (!name) throw new Error('time zone is empty: give an IANA zone such as America/Sao_Paulo');
  try {
    return formatterFor(name).resolvedOptions().timeZone;
  } catch {
    throw new Error(`unknown time zone "${tz}": give an IANA zone such as America/Sao_Paulo`);
  }
}

// wallClock: the instant's local year, month (1-12), day, hour, minute,
// second in the zone.
export function wallClock(ms, tz) {
  const p = {};
  for (const part of formatterFor(tz).formatToParts(new Date(ms))) {
    if (part.type !== 'literal') p[part.type] = Number(part.value);
  }
  return { year: p.year, month: p.month, day: p.day, hour: p.hour === 24 ? 0 : p.hour, minute: p.minute, second: p.second };
}

// The zone's UTC offset at an instant, in ms (São Paulo: -3 h).
function offsetAt(ms, tz) {
  const w = wallClock(ms, tz);
  const whole = Math.floor(ms / 1000) * 1000;
  return Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute, w.second) - whole;
}

// zonedToInstant: the instant at which the zone's wall clock reads `date`
// plus `minutes` (minutes may run past 24 h: 26 * 60 is 02:00 the next day).
export function zonedToInstant(date, minutes, tz) {
  const wall = Date.UTC(date.year, date.month - 1, date.day, 0, minutes);
  const first = wall - offsetAt(wall, tz);
  const before = offsetAt(wall - DAY, tz);
  const offsets = new Set([offsetAt(first, tz), before, offsetAt(wall + DAY, tz)]);
  const agreeing = [...offsets].map((o) => wall - o).filter((t) => wall - offsetAt(t, tz) === t);
  if (agreeing.length) return Math.min(...agreeing);
  return wall - before; // a DST gap: forward by the gap's length
}

// --- local dates ------------------------------------------------------------

const pad = (n, w = 2) => String(n).padStart(w, '0');

export function localDay(ms, tz) {
  const w = wallClock(ms, tz);
  return { year: w.year, month: w.month, day: w.day };
}

export function addDays(date, n) {
  const d = new Date(Date.UTC(date.year, date.month - 1, date.day + n));
  return { year: d.getUTCFullYear(), month: d.getUTCMonth() + 1, day: d.getUTCDate() };
}

// 0 = Sunday … 6 = Saturday.
export function weekday(date) {
  return new Date(Date.UTC(date.year, date.month - 1, date.day)).getUTCDay();
}

export function isoDate(date) {
  return `${pad(date.year, 4)}-${pad(date.month)}-${pad(date.day)}`;
}

// YYYY-MM-DD of the instant in the zone.
export function localDate(ms, tz) {
  return isoDate(localDay(ms, tz));
}

// HH:MM of the instant in the zone (the keeper's texts).
export function formatHHMM(ms, tz) {
  const w = wallClock(ms, tz);
  return `${pad(w.hour)}:${pad(w.minute)}`;
}

// --- business hours ---------------------------------------------------------
//
// The onboarding text (gc whatsapp invite --hours, gc whatsapp hours): a
// comma- (or semicolon-) separated list of items, each "[days] HH:MM-HH:MM"
// with one or more ranges:
//   "Mon-Fri 09:00-18:00, Sat 10:00-14:00"
//   "18:00-02:00"                       (no days: every day; crosses midnight)
//   "Mon-Fri 09:00-12:00 13:00-18:00"   (two ranges on the same days)
//   "Mon-Fri 09:00-12:00, 13:00-18:00"  (an item without days takes the
//                                        days of the item before it)
// Days: Mon..Sun (three letters or the full name, any case), a range
// ("Fri-Mon" wraps), several joined by "/" or spaces, or "daily". A range
// whose close is before its open crosses midnight: the span opens on the
// listed day and closes the next. 24:00 is allowed as a close.
//
// The result: [{days: [0..6], open: minutes, close: minutes}], a span
// belonging to the day it OPENS on.

const DAY_NAMES = ['sunday', 'monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday'];
const EVERY_DAY = [0, 1, 2, 3, 4, 5, 6];
const EXAMPLE = 'write e.g. "Mon-Fri 09:00-18:00, Sat 10:00-14:00"';

function dayIndex(token) {
  const t = token.toLowerCase().replace(/\.$/, '');
  if (t.length < 3) return -1;
  return DAY_NAMES.findIndex((name) => name.startsWith(t));
}

function parseDays(text, fail) {
  const t = text.trim().toLowerCase();
  if (['daily', 'every day', 'everyday'].includes(t)) return EVERY_DAY;
  const days = new Set();
  for (const part of t.split(/[\s/&+]+/).filter(Boolean)) {
    const ends = part.split('-');
    if (ends.length > 2) fail(`"${part}" is not a day or a day range`);
    const [a, b] = [dayIndex(ends[0]), dayIndex(ends[ends.length - 1])];
    if (a < 0 || b < 0) fail(`"${part}" is not a day (Mon, Tue, Wed, Thu, Fri, Sat, Sun)`);
    for (let i = a; ; i = (i + 1) % 7) {
      days.add(i);
      if (i === b) break;
    }
  }
  if (!days.size) fail('no days given');
  return [...days].sort((x, y) => x - y);
}

function parseClock(text, { isClose }, fail) {
  const m = /^(\d{1,2}):(\d{2})$/.exec(text);
  const h = m ? Number(m[1]) : NaN;
  const min = m ? Number(m[2]) : NaN;
  if (!m || min > 59 || h > 24 || (h === 24 && (min !== 0 || !isClose))) fail(`"${text}" is not a time HH:MM (00:00 to 23:59, 24:00 as a close)`);
  return h * 60 + min;
}

export function parseHours(text) {
  const source = typeof text === 'string' ? text : '';
  if (!source.trim()) throw new Error(`business hours are empty: ${EXAMPLE}`);
  const fail = (why) => {
    throw new Error(`bad business hours "${source}": ${why}; ${EXAMPLE}`);
  };
  const norm = source.replace(/[‒-―]/g, '-').replace(/\s*-\s*/g, '-').trim();
  const spans = [];
  let days = null;
  for (const raw of norm.split(/[,;]/)) {
    const item = raw.trim();
    if (!item) continue;
    const m = /^(.*?)\s*((?:\d{1,2}:\d{2}-\d{1,2}:\d{2}\s*)+)$/.exec(item);
    if (!m) fail(`"${item}" is not "<days> HH:MM-HH:MM"`);
    if (m[1]) days = parseDays(m[1], fail);
    else if (days === null) days = EVERY_DAY;
    for (const range of m[2].trim().split(/\s+/)) {
      const [o, c] = range.split('-');
      const open = parseClock(o, { isClose: false }, fail);
      const close = parseClock(c, { isClose: true }, fail);
      if (open === close) fail(`"${range}" is an empty span`);
      spans.push({ days: [...days], open, close });
    }
  }
  if (!spans.length) fail('no span given');
  return spans;
}

// A span's close in minutes from the start of its opening day.
const closeMinutes = (s) => (s.close > s.open ? s.close : s.close + 24 * 60);

function mergeIntervals(list) {
  const out = [];
  for (const iv of [...list].sort((a, b) => a.start - b.start)) {
    const last = out[out.length - 1];
    if (last && iv.start <= last.end) last.end = Math.max(last.end, iv.end);
    else out.push({ start: iv.start, end: iv.end });
  }
  return out;
}

function opening(spans, tz, date) {
  const wd = weekday(date);
  return spans
    .filter((s) => s.days.includes(wd))
    .map((s) => ({ start: zonedToInstant(date, s.open, tz), end: zonedToInstant(date, closeMinutes(s), tz) }));
}

// daySpans: the spans OPENING on the local date, merged, as instants
// [start, end), in order (the last is the day's last span).
export function daySpans(spans, tz, date) {
  return mergeIntervals(opening(spans, tz, date));
}

// spanIntervals: every span instant interval [start, end) touching
// [fromMs, toMs], merged where spans meet or overlap.
export function spanIntervals(spans, tz, fromMs, toMs) {
  const out = [];
  const last = isoDate(localDay(toMs, tz));
  // From the day before: a span opened yesterday may cross into fromMs.
  for (let date = addDays(localDay(fromMs, tz), -1); isoDate(date) <= last; date = addDays(date, 1)) {
    for (const iv of opening(spans, tz, date)) if (iv.end > fromMs && iv.start <= toMs) out.push(iv);
  }
  return mergeIntervals(out);
}
