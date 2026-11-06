// keeper.js — the WINDOW-KEEPER: the design's section "The window-keeper",
// items (a)-(g), and the pack-adapter spec's SCOPE item 3 ("keeper.js");
// Taylor's rule of 01:43:46Z Wed 11/4.
//
// Meta lets the business's number send free-form only inside the 24-hour
// customer-service window that a message FROM the owner's side opens. Per
// conversation, the keeper watches that clock and raises keeper events into
// the mayor's session, so the mayor writes every word that reaches a phone:
//
//   - the DAILY UPDATE, once per local day whatever the window's state (b);
//   - the LADDER, nudges at the close minus 60, 30, 15 and 5 minutes while
//     nobody from the business has written (c), with the three hours cases;
//   - nothing after the close: the lapse is counted (d).
//
// A keeper event is a POST /extmsg/inbound on the conversation itself with
// actor {id: keeper, is_bot: true} and a deterministic provider_message_id.
// It is no gateway delivery: no journal line, no ack, no in-doubt rule. It
// takes the retry ladder on an HTTP answer gc gives before any fan-out (5xx,
// 404, 408, 429), and is otherwise dropped and counted.
//
// The authority for the clock is the gateway's GET /v1/window (a). What
// stops a repeat is the keeper's own state, keeper.json (0600, atomic
// rewrite): every mark and every day's update is written and fsynced BEFORE
// its event is posted, so a kill between the two loses that one event and
// never raises it twice. Retries live in memory only, for the same reason.
//
// tick() is called often; every schedule is a function of clock() and the
// state file, never of a timer's memory.
//
// The setters (recordInvite, bindInvite, setConsent, setHours) change a
// COPY, write it, and only then commit it in place: a failed write throws
// and leaves memory as the file on disk has it. The time of the last good
// window read is in the file too (window_read_ok_at), for the doctor row.

import fs from 'node:fs';
import path from 'node:path';

import { killPoint } from './clock.js';
import { conversationRef } from './gc.js';
import { isRetryableAnswer, ladderDelay, ladderMs } from './http.js';
import { verdictOf } from './inbound.js';
import {
  addDays, assertZone, daySpans, formatHHMM, isoDate, localDate, localDay,
  parseHours, spanIntervals, zonedToInstant,
} from './zoned.js';

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const STATUS_SPAN_MS = 7 * DAY; // the doctor row counts the last 7 days
const RECORD_KEEP_MS = 30 * DAY; // older records are pruned from the file
const MARKS = [60, 30, 15, 5]; // (c) Taylor's ladder, minutes before the close
const UPDATE_LEAD_MS = 2 * HOUR; // (b) the update two hours before close of business
const STATE_FILE = 'keeper.json';
const ACTOR = Object.freeze({ id: 'keeper', display_name: 'Dynasty window-keeper', is_bot: true });
const NUDGE_TAIL = 'One short line asking anyone in the group to reply.';
const CLOSED_TAIL = 'an actual update goes by gc whatsapp publish --update, else nothing.';

// keeper.json holds every instant as ISO-8601 (readable by gc whatsapp
// status and by a person); in memory every instant is epoch ms. A key named
// "at" or ending in "_at" is an instant.
const isInstantKey = (key) => key === 'at' || key.endsWith('_at');
const toDisk = (key, value) => (isInstantKey(key) && typeof value === 'number' ? new Date(value).toISOString() : value);
function fromDisk(key, value) {
  if (!isInstantKey(key) || typeof value !== 'string') return value;
  const ms = Date.parse(value);
  return Number.isFinite(ms) ? ms : null;
}

// The gateway's instants: integer epoch SECONDS on the wire (the gateway
// lane's contract reading 1, posted on gp-9hf1 18:20Z 11/4); ISO-8601
// strings and epoch milliseconds are read too. Null for a phone that has
// never written.
export function parseInstant(value) {
  let ms;
  if (typeof value === 'number') ms = value;
  else if (typeof value === 'string' && value.trim()) {
    const text = value.trim();
    ms = /^\d+$/.test(text) ? Number(text) : Date.parse(text);
  } else return null;
  if (!Number.isFinite(ms)) return null;
  return ms > 0 && ms < 1e11 ? ms * 1000 : ms;
}

const iso = (ms) => (ms == null ? null : new Date(ms).toISOString());
// Logs name a conversation by its last four digits only.
const tag = (conversationId) => `…${String(conversationId).slice(-4)}`;
const blank = (v) => v === undefined || v === null || (typeof v === 'string' && !v.trim());
const digitsOf = (conversationId) => String(conversationId ?? '').replace(/\D/g, '');
// A phone number as an operator types it: digits and phone punctuation only.
const PHONE_TYPED = /^[+\d\s().-]+$/;
// A conversation id as the gateway gives one: a number, or a user id.
const ID_SHAPED = /^[A-Za-z0-9._-]{1,200}$/;
// A refusal the internal listener answers with its own status (index.js).
const refusal = (status, message) => Object.assign(new Error(message), { status });

function freshRow() {
  return {
    consent: null, // null: never recorded; the keeper is OFF unless true (e)
    consent_at: null, // when it turned on: marks and updates due before it are skipped
    hours: null,
    tz: null,
    last_inbound_at: null,
    window_closes_at: null,
    plan: null, // the ladder for the current close: {close_at, decided_at, case, business_close_at, marks}
    marks: { close_at: null, fired: [] }, // marks handled for close_at: {mark, at, event, why?}
    update_day: null, // the business day of the last daily update raised (YYYY-MM-DD)
    update_at: null, // when it was raised
    update_window_closes_at: null, // the open window it was raised in
    lapse_close_at: null, // the last close counted as a lapse
    lapses: [],
    undelivered: [],
    refused_nudges: [],
  };
}

function normalizeRow(raw) {
  const r = { ...freshRow(), ...(raw && typeof raw === 'object' ? raw : {}) };
  if (!r.marks || typeof r.marks !== 'object' || !Array.isArray(r.marks.fired)) r.marks = { close_at: r.window_closes_at, fired: [] };
  for (const key of ['lapses', 'undelivered', 'refused_nudges']) if (!Array.isArray(r[key])) r[key] = [];
  return r;
}

// window_read_ok_at: the last good GET /v1/window (the doctor row reads it);
// window_read_error(_at): the last failure since, cleared by a good read;
// window_ids: the conversation ids that read listed (null until one is
// recorded), so a restart knows them before its first read (gp-g02t).
// An invite stays after its bind, as {..., bound_to, bound_at}, so a second
// sight of the same join is a no-op; prune drops it after 30 days.
const emptyState = () => ({
  version: 1, conversations: {}, invites: {}, window_read_ok_at: null, window_read_error: null, window_read_error_at: null, window_ids: null,
});
// A good read that changes nothing else is written at most once a minute
// (windowStampMs; the doctor row turns red after 5 minutes without one).
const WINDOW_STAMP_MS = MINUTE;

export function createKeeper({
  stateDir,
  clock,
  gateway,
  gc,
  city,
  log = () => {},
  updateHour = 10,
  cityTz = Intl.DateTimeFormat().resolvedOptions().timeZone,
  windowReadMs = 60000,
  // How often a good read alone rewrites keeper.json (its
  // window_read_ok_at stamp); each rewrite is fsynced. The unit tests that
  // run whole days at one-minute ticks raise it.
  windowStampMs = WINDOW_STAMP_MS,
} = {}) {
  if (!stateDir) throw new Error('keeper: stateDir is required');
  const now = typeof clock === 'function' ? clock : typeof clock?.now === 'function' ? () => clock.now() : () => Date.now();
  const file = path.join(stateDir, STATE_FILE);
  const hour = Number.isInteger(Number(updateHour)) && Number(updateHour) >= 0 && Number(updateHour) <= 23 ? Number(updateHour) : 10;
  if (hour !== Number(updateHour)) log(`keeper: update hour ${updateHour} is not 0-23; using 10`);
  let homeZone;
  try {
    homeZone = assertZone(cityTz);
  } catch (err) {
    homeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    log(`keeper: city ${err.message}; using the host's ${homeZone}`);
  }
  const readEvery = Number(windowReadMs) > 0 ? Number(windowReadMs) : 60000;
  const stampEvery = Number(windowStampMs) > 0 ? Number(windowStampMs) : WINDOW_STAMP_MS;

  const state = load();
  // A read is owed while the generation a good read satisfied is behind the
  // one noteDelivery() raised: a delivery noted while a read is in flight
  // owes the next read too (that read may have been computed before it).
  let owedGen = 1; // the first tick reads
  let readGen = 0;
  const readOwed = () => readGen !== owedGen;
  let nextReadAt = -Infinity;
  let authHoldUntil = -Infinity;
  // A failed read, owed or not, is retried on the ladder (1, 5, 15, 60 s) in
  // clock time, never at every tick.
  let readFailures = 0;
  let readRetryAt = -Infinity;
  let savedOkAt = state.window_read_ok_at;
  // The last good read's conversation ids (null: no read recorded yet, as in
  // a keeper.json written before gp-g02t). A row the last good read did not
  // list raises nothing (the Opus read r1's CRITICAL 1, the mayor's (A)): its
  // owner is unbound at the gateway, and its id may be a phone number a later
  // owner shows, so an event there would prompt the mayor to send to it.
  let lastWindowIds = state.window_ids === null ? null : new Set(state.window_ids);
  const listed = (id) => lastWindowIds === null || lastWindowIds.has(id);
  let raiseFailLogAt = -Infinity;
  const loggedOnce = new Set();
  const retries = []; // events waiting on the ladder (memory only: a kill loses them, never repeats them)
  let running = null;
  let tmpSeq = 0;

  function logOnce(key, line) {
    if (loggedOnce.has(key)) return;
    loggedOnce.add(key);
    log(line);
  }

  // --- state file ------------------------------------------------------------

  function load() {
    fs.mkdirSync(stateDir, { recursive: true, mode: 0o700 });
    let text;
    try {
      text = fs.readFileSync(file, 'utf8');
    } catch (err) {
      if (err.code === 'ENOENT') return emptyState();
      throw err;
    }
    try {
      const raw = JSON.parse(text, fromDisk);
      if (!raw || typeof raw !== 'object') throw new Error('not a JSON object');
      const s = emptyState();
      for (const [id, r] of Object.entries(raw.conversations ?? {})) s.conversations[id] = normalizeRow(r);
      for (const [code, inv] of Object.entries(raw.invites ?? {})) if (inv && typeof inv === 'object') s.invites[code] = inv;
      if (typeof raw.window_read_ok_at === 'number') s.window_read_ok_at = raw.window_read_ok_at;
      if (typeof raw.window_read_error === 'string') s.window_read_error = raw.window_read_error;
      if (typeof raw.window_read_error_at === 'number') s.window_read_error_at = raw.window_read_error_at;
      if (Array.isArray(raw.window_ids)) s.window_ids = raw.window_ids.filter((id) => typeof id === 'string');
      return s;
    } catch (err) {
      // Never guess at a damaged record: set it aside and start with every
      // conversation OFF (no consent recorded), which can raise nothing twice.
      const aside = `${file}.corrupt-${Date.now()}`;
      try { fs.renameSync(file, aside); } catch { /* keep going */ }
      log(`keeper: ${file} unreadable (${err.message}); moved to ${aside}; every conversation is OFF until consent is recorded again`);
      return emptyState();
    }
  }

  // The atomic rewrite: a temp file 0600, fsynced, renamed over, the
  // directory fsynced. Throws on failure, the file on disk then unchanged.
  // `s` is the state as it is to be (commit() passes a copy).
  function save(s = state) {
    prune(s, now());
    const text = `${JSON.stringify(s, toDisk, 2)}\n`;
    const tmp = `${file}.tmp-${process.pid}-${++tmpSeq}`;
    let fd;
    try {
      fd = fs.openSync(tmp, 'w', 0o600);
      fs.fchmodSync(fd, 0o600);
      fs.writeFileSync(fd, text);
      fs.fsyncSync(fd);
      fs.closeSync(fd);
      fd = undefined;
      fs.renameSync(tmp, file);
    } catch (err) {
      if (fd !== undefined) try { fs.closeSync(fd); } catch { /* already failing */ }
      try { fs.unlinkSync(tmp); } catch { /* already failing */ }
      throw err;
    }
    try {
      const dfd = fs.openSync(stateDir, 'r');
      try { fs.fsyncSync(dfd); } finally { fs.closeSync(dfd); }
    } catch { /* not every platform fsyncs a directory */ }
    savedOkAt = s.window_read_ok_at;
  }

  // A write that is not guarding an event: a failure is logged, not fatal.
  function persist() {
    try {
      save();
    } catch (err) {
      log(`keeper: state write failed: ${err.message}`);
    }
  }

  // The setters' write: `rows` (id -> the row as it is to be, a copy) and
  // `invites` (code -> the record) are written as part of a copy of the
  // state; only once that write succeeded are they committed, a row in place
  // (Object.assign) so a tick holding it keeps the live row. Throws the
  // write's error with memory unchanged.
  function commit({ rows = {}, invites = {} } = {}) {
    const next = {
      ...state,
      conversations: { ...state.conversations, ...rows },
      invites: { ...state.invites, ...invites },
    };
    save(next);
    for (const [id, r] of Object.entries(rows)) {
      if (Object.hasOwn(state.conversations, id)) Object.assign(state.conversations[id], r);
      else state.conversations[id] = r;
    }
    state.invites = next.invites;
  }

  // A copy of a row to change and commit (a fresh row for a new id).
  const draftRow = (id) => (Object.hasOwn(state.conversations, id) ? structuredClone(state.conversations[id]) : freshRow());

  function restoreRow(r, before) {
    for (const key of Object.keys(r)) if (!Object.hasOwn(before, key)) delete r[key];
    Object.assign(r, before);
  }

  function prune(s, t) {
    const keepAfter = t - RECORD_KEEP_MS;
    for (const r of Object.values(s.conversations)) {
      for (const key of ['lapses', 'undelivered', 'refused_nudges']) r[key] = r[key].filter((x) => !(x.at < keepAfter));
    }
    for (const [code, inv] of Object.entries(s.invites)) if (inv.recorded_at < keepAfter) delete s.invites[code];
  }

  function row(conversationId) {
    const id = String(conversationId);
    if (!Object.hasOwn(state.conversations, id)) state.conversations[id] = freshRow();
    return state.conversations[id];
  }

  // The operator's verbs name a conversation by the id gc shows
  // (whatsapp/<conversation_id>): a row is keyed by the gateway's
  // conversation_id (its GET /v1/window entry). A user id matches only whole
  // and exact: one may hold letters (the gateway's BSUID pattern: two
  // capitals, a period, up to 128 alphanumerics), so two user ids can share
  // every digit and one can have none (gp-g02t, codex self-review r1 MAJOR 1).
  // A number may be typed in any form ("+55 11 99999-0001"): text of digits
  // and phone punctuation alone matches the conversation whose id is exactly
  // its digits, which no user id is. The candidates are the keeper's rows and
  // the conversations of the last good window read; a verb never creates a
  // row for a conversation the keeper does not know (consent --off on a
  // mistyped number must not answer OK while the real one stays ON). Returns
  // the row's id.
  function knownConversation(typed) {
    const text = String(typed ?? '').trim();
    const phoneTyped = PHONE_TYPED.test(text) && /\d/.test(text);
    if (!phoneTyped && !ID_SHAPED.test(text)) {
      throw refusal(400, 'not a conversation id: give the id gc shows for the conversation (whatsapp/<conversation_id>), or the owner\'s number in any form');
    }
    const known = new Set([...Object.keys(state.conversations), ...(lastWindowIds ?? [])]);
    if (known.has(text)) return text;
    const digits = phoneTyped ? digitsOf(text) : '';
    if (digits && known.has(digits)) return digits;
    throw refusal(404, `no conversation ending ${(digits || text).slice(-4)} is known to the window-keeper (it has not joined, or the gateway's window read does not list it yet): nothing recorded`);
  }

  const zoneOf = (r) => r.tz || homeZone;

  const spanCache = new Map();
  const badHoursLogged = new Set();
  function spansOf(r) {
    if (!r.hours) return null;
    if (!spanCache.has(r.hours)) {
      try {
        spanCache.set(r.hours, parseHours(r.hours));
      } catch (err) {
        // Only a hand-edited file gets here (every setter validates).
        if (!badHoursLogged.has(r.hours)) log(`keeper: ${err.message}; treated as no hours`);
        badHoursLogged.add(r.hours);
        return null;
      }
    }
    return spanCache.get(r.hours);
  }

  // Validates onboarding fields; returns them normalized (null when absent).
  // A bad field throws with status 400.
  function checkFields(hours, tz) {
    const out = { hours: null, tz: null };
    try {
      if (!blank(tz)) out.tz = assertZone(tz);
      if (!blank(hours)) {
        parseHours(String(hours));
        out.hours = String(hours).trim().replace(/\s+/g, ' ');
      }
    } catch (err) {
      err.status ??= 400;
      throw err;
    }
    return out;
  }

  // --- the window clock (a) --------------------------------------------------

  async function readWindow(t) {
    if (t < authHoldUntil || t < readRetryAt) return;
    if (!readOwed() && t < nextReadAt) return;
    const gen = owedGen;
    let answer;
    try {
      answer = await gateway.window();
    } catch (err) {
      const error = err?.message ?? String(err);
      const delay = ladderDelay(readFailures);
      readFailures += 1;
      readRetryAt = t + delay; // clock time, not a sleep
      // A 401 is never retried in a loop (gateway.js): wait the interval.
      if (err?.auth) authHoldUntil = t + readEvery;
      // Recorded on disk: the doctor row names it while the clock is stale.
      state.window_read_error = error;
      state.window_read_error_at = t;
      persist();
      log(`keeper: window read failed (${error}); retry ${readFailures} in ${Math.max(delay, err?.auth ? readEvery : 0) / 1000}s`);
      return;
    }
    readGen = gen;
    readFailures = 0;
    readRetryAt = -Infinity;
    nextReadAt = t + readEvery;
    const hadError = state.window_read_error != null;
    state.window_read_ok_at = t;
    state.window_read_error = null;
    state.window_read_error_at = null;
    let dirty = false;
    const ids = new Set();
    for (const w of Array.isArray(answer?.windows) ? answer.windows : []) {
      if (blank(w?.conversation_id)) continue;
      const id = String(w.conversation_id);
      ids.add(id);
      if (applyWindow(id, parseInstant(w.last_inbound_at), parseInstant(w.window_closes_at), t)) dirty = true;
    }
    const listChanged = state.window_ids === null || state.window_ids.length !== ids.size || state.window_ids.some((id) => !ids.has(id));
    lastWindowIds = ids;
    state.window_ids = [...ids];
    if (dirty || hadError || listChanged || !(t - savedOkAt < stampEvery)) persist();
  }

  function applyWindow(conversationId, last, close, t) {
    const r = row(conversationId);
    if (r.window_closes_at === close && r.last_inbound_at === last) return false;
    // Only an inbound moves last_inbound_at, and only forward. A read that
    // carries an older one (a gateway restored from a backup, a replica
    // behind) is ignored: taking it would empty the fired set and raise a
    // mark's id a second time when the newer one comes back.
    if (r.last_inbound_at != null && (last == null || last < r.last_inbound_at)) {
      logOnce(`backward:${conversationId}:${last}`, `keeper: ${tag(conversationId)}: the gateway's last_inbound_at went back (${iso(last) ?? 'none'} after ${iso(r.last_inbound_at)}); read ignored`);
      return false;
    }
    const oldClose = r.window_closes_at;
    // (d) a close that passed before the message that moved it was a lapse,
    // counted here too in case no tick saw it pass (the keeper was down).
    if (oldClose != null && last != null && last >= oldClose) countLapse(conversationId, r, oldClose, t);
    r.last_inbound_at = last;
    r.window_closes_at = close;
    // (a) a new close time empties the fired set; (c) the case is decided
    // again whenever the close moves.
    if (r.marks.close_at !== close) r.marks = { close_at: close, fired: [] };
    r.plan = close == null ? null : planLadder(r, t);
    // A nudge waiting on the ladder for the old close is moot: the business wrote.
    dropRetries((job) => job.conversationId === conversationId && job.kind === 'nudge' && job.close !== close);
    return true;
  }

  // (c) The ladder's plan for the current close, decided when the close is
  // first read, again when it moves, and when the hours change.
  function planLadder(r, t) {
    const close = r.window_closes_at;
    const last = r.last_inbound_at ?? -Infinity;
    const ladder = (end) => MARKS.map((mark) => ({ mark, at: end - mark * MINUTE }));
    // A mark at or before the business's last message would nudge a window
    // it has just opened.
    const plan = (kind, marks, businessClose = null) => ({
      close_at: close,
      decided_at: t,
      case: kind,
      business_close_at: businessClose,
      marks: marks.filter((m) => m.at > last),
    });
    if (t >= close) return plan('closed', []);
    const spans = spansOf(r);
    // Without hours the marks run on the close itself at any hour: the owner reads a phone.
    if (!spans) return plan('plain', ladder(close));
    const blocks = spanIntervals(spans, zoneOf(r), Math.min(t, close - 2 * HOUR) - DAY, close);
    const touches = (a, b) => blocks.some((iv) => iv.start <= b && iv.end > a); // a span instant in [a, b]
    const inside = (x) => blocks.some((iv) => iv.start <= x && x < iv.end);
    // Case (3): no span instant between now and the close: no mark; the
    // lapse is counted when the close passes.
    if (!touches(t, close)) return plan('lapse', []);
    // Case (1): the close inside a span, or a span instant between the
    // 60-minute mark and the close: unshifted, a mark outside every span
    // dropped, not deferred.
    //
    // OPEN DESIGN QUESTION (keeper review r2, finding 8; the spec read
    // literally, left as built): a close in the first ~5 minutes of a span
    // (09:02 under 09:00-18:00) is case (1), and every mark (08:02 to 08:57)
    // falls before the open and drops, so it gets NO nudge, while a close
    // one minute earlier (08:57, before the open) is case (2) and gets four
    // the evening before. Whether such a close should shift instead is the
    // founders' call.
    if (touches(close - 60 * MINUTE, close)) return plan('unshifted', ladder(close).filter((m) => inside(m.at)));
    // Case (2): shifted to the close of business, the end of the last span
    // ending before the close. ("A mark outside hours never fires", (f),
    // holds for shifted marks too: a span shorter than an hour drops the 60.)
    const ends = blocks.filter((iv) => iv.end <= close).map((iv) => iv.end);
    if (!ends.length) return plan('lapse', []);
    const businessClose = Math.max(...ends);
    return plan('shifted', ladder(businessClose).filter((m) => inside(m.at)), businessClose);
  }

  function countLapse(conversationId, r, close, t) {
    if (r.consent !== true) return false;
    if (r.consent_at != null && close < r.consent_at) return false;
    if (r.lapse_close_at != null && r.lapse_close_at >= close) return false; // once per close
    r.lapse_close_at = close;
    r.lapses.push({ close_at: close, at: t });
    log(`keeper: ${tag(conversationId)}: the window closed with nobody from the business writing (lapse)`);
    return true;
  }

  // --- the daily update (b) --------------------------------------------------

  function dueUpdate(r, t) {
    const zone = zoneOf(r);
    const today = localDay(t, zone);
    const spans = spansOf(r);
    const slots = [];
    if (spans) {
      // Two hours before the close of the day's last span, clamped inside it;
      // no span that day, no update. A business day is the date its span
      // opens on: a span crossing midnight (18:00-02:00) raises that day's
      // update at 00:00 the next date, so yesterday is a candidate too.
      for (const day of [today, addDays(today, -1)]) {
        const blocks = daySpans(spans, zone, day);
        const lastSpan = blocks[blocks.length - 1];
        if (lastSpan) slots.push({ day: isoDate(day), at: Math.max(lastSpan.start, lastSpan.end - UPDATE_LEAD_MS), until: lastSpan.end });
      }
    } else {
      // No hours: the fixed local hour of the business's day (its recorded
      // zone, else the city's), any time until that day ends.
      slots.push({ day: isoDate(today), at: zonedToInstant(today, hour * 60, zone), until: zonedToInstant(addDays(today, 1), 0, zone) });
    }
    for (const slot of slots) {
      if (t < slot.at || t >= slot.until) continue; // not yet, or the slot passed (a keeper that was down skips it)
      if (r.update_day && slot.day <= r.update_day) continue; // once per local day
      if (r.consent_at != null && slot.at < r.consent_at) continue; // consent came after it: start with the next
      return slot;
    }
    return null;
  }

  async function raiseUpdate(conversationId, r, slot, t) {
    const close = r.window_closes_at;
    const open = close != null && t < close;
    const before = structuredClone(r);
    r.update_day = slot.day;
    // (e) at most one update per window: a second business day inside the
    // same open window (a 23-hour DST day) is recorded without an event.
    if (open && r.update_window_closes_at === close) {
      log(`keeper: ${tag(conversationId)}: ${slot.day} update skipped, one already raised in this window`);
      persist();
      return;
    }
    r.update_at = t;
    r.update_window_closes_at = open ? close : null;
    const zone = zoneOf(r);
    const head = `[window-keeper] ${conversationId}: daily update due. Send today's short business update with one acknowledgement ask.`;
    let text;
    if (open) text = `${head} Window open, closes ${formatHHMM(close, zone)}.`;
    else if (close != null) text = `${head} Window closed since ${formatHHMM(close, zone)}: ${CLOSED_TAIL}`;
    else text = `${head} Window closed (nobody from the business has written yet): ${CLOSED_TAIL}`;
    await raise(conversationId, r, before, { kind: 'update', id: `keeper:${conversationId}:${slot.day}:update`, text, close }, t);
  }

  // --- the ladder (c), the lapse (d), the quiet rules (e) --------------------

  async function runLadder(conversationId, r, t) {
    const close = r.window_closes_at;
    if (close == null) return;
    if (t >= close) {
      // No nudge after the close; a close that passed with nobody writing is one lapse.
      if (countLapse(conversationId, r, close, t)) persist();
      return;
    }
    if (r.marks.close_at !== close) r.marks = { close_at: close, fired: [] };
    if (!r.plan || r.plan.close_at !== close) {
      r.plan = planLadder(r, t);
      persist();
    }
    const handled = new Set(r.marks.fired.map((f) => f.mark));
    const due = r.plan.marks.filter((m) => m.at <= t && !handled.has(m.mark)).sort((a, b) => a.at - b.at);
    if (!due.length) return;
    // Several marks due at once (a restart, a late first read): only the
    // latest fires; the earlier ones are recorded as fired without an event.
    const before = structuredClone(r);
    const mark = due.pop();
    for (const m of due) r.marks.fired.push({ mark: m.mark, at: t, event: false, why: 'overdue' });
    const why = silence(r, mark, t);
    if (why) {
      r.marks.fired.push({ mark: mark.mark, at: t, event: false, why });
      persist();
      return;
    }
    r.marks.fired.push({ mark: mark.mark, at: t, event: true });
    const id = `keeper:${conversationId}:${new Date(close).toISOString()}:${mark.mark}`;
    await raise(conversationId, r, before, { kind: 'nudge', id, text: nudgeText(conversationId, r, mark, t), close }, t);
  }

  // The business day an instant belongs to, as dueUpdate defines it: with
  // hours, the date its span OPENS on (under 18:00-02:00, 00:30 Wednesday
  // is Tuesday's); outside every span, or without hours, the local date.
  function businessDay(r, ms) {
    const zone = zoneOf(r);
    const today = localDay(ms, zone);
    const spans = spansOf(r);
    if (spans) {
      for (const day of [today, addDays(today, -1)]) {
        if (daySpans(spans, zone, day).some((iv) => iv.start <= ms && ms < iv.end)) return isoDate(day);
      }
    }
    return isoDate(today);
  }

  // Why a due mark raises no event, or null.
  function silence(r, mark, t) {
    // (10g) consent --on resumes from the next mark.
    if (r.consent_at != null && mark.at < r.consent_at) return 'before consent';
    const zone = zoneOf(r);
    // (e) the business wrote after the day's update, on the mark's business
    // day: the update was acknowledged, no ladder that day. Business days,
    // not calendar dates: under hours that cross midnight a calendar date
    // would silence the NEXT business day's marks.
    if (r.update_at != null && r.last_inbound_at != null && r.last_inbound_at > r.update_at
      && businessDay(r, r.update_at) === businessDay(r, mark.at)) return 'quiet';
    // A mark outside hours never fires, (f): a late one (a restart) whose
    // firing instant has left the span stays silent too.
    const spans = spansOf(r);
    if (spans && !spanIntervals(spans, zone, t, t).some((iv) => iv.start <= t && t < iv.end)) return 'outside hours';
    return null;
  }

  // The mark's own number on time; the true minutes left when it fires late
  // (after a restart), never more than the mark.
  function minutesLeft(end, t, mark) {
    const n = Math.max(1, Math.min(mark, Math.ceil((end - t) / MINUTE)));
    return `${n} minute${n === 1 ? '' : 's'}`;
  }

  function nudgeText(conversationId, r, mark, t) {
    const zone = zoneOf(r);
    if (r.plan.case === 'shifted') {
      const left = minutesLeft(r.plan.business_close_at, t, mark.mark);
      return `[window-keeper] ${conversationId}: window closes at ${formatHHMM(r.window_closes_at, zone)}, after your hours; last chance before close of business in ${left}. ${NUDGE_TAIL}`;
    }
    const left = minutesLeft(r.window_closes_at, t, mark.mark);
    return `[window-keeper] ${conversationId}: window closes in ${left}, nobody from the business has written since ${formatHHMM(r.last_inbound_at, zone)}. ${NUDGE_TAIL}`;
  }

  // --- raising and posting an event ------------------------------------------

  // `r` holds the event's record (the update day, the mark fired); `before`
  // is the row as it was before that record was made.
  async function raise(conversationId, r, before, ev, t) {
    // (a) the record reaches the disk before the event exists anywhere.
    try {
      save();
    } catch (err) {
      // The file on disk was not replaced: the record is undone in memory
      // too, so the next tick raises the event again (never "fired" in
      // memory alone, lost and shown as fired by status).
      restoreRow(r, before);
      if (t - raiseFailLogAt >= MINUTE) {
        raiseFailLogAt = t;
        log(`keeper: ${tag(conversationId)}: state write failed, ${ev.kind} not raised yet (retried every tick): ${err.message}`);
      }
      return;
    }
    killPoint('keeper-after-state', ev.id);
    await post({
      conversationId,
      kind: ev.kind,
      id: ev.id,
      close: ev.close ?? null,
      attempt: 0,
      dueAt: t,
      message: {
        provider_message_id: ev.id,
        conversation: conversationRef(city, conversationId),
        actor: { ...ACTOR },
        text: ev.text,
        received_at: new Date(t).toISOString(),
      },
    });
  }

  async function post(job) {
    let answer;
    try {
      answer = await gc.postInbound(job.message);
    } catch (err) {
      if (isRetryableAnswer(err)) {
        if (job.attempt < ladderMs.length) {
          const delay = ladderDelay(job.attempt);
          job.attempt += 1;
          job.dueAt = now() + delay; // clock time, not a sleep
          retries.push(job);
          log(`keeper: ${tag(job.conversationId)}: ${job.kind} answered HTTP ${err.status}; retry ${job.attempt} in ${delay / 1000}s`);
          return;
        }
        return undelivered(job, `HTTP ${err.status} through the retry ladder`);
      }
      // No answer (gc may have acted: never repeat) or a 4xx: dropped and counted.
      return undelivered(job, err?.noAnswer ? 'no answer' : err?.malformed ? 'malformed answer' : `HTTP ${err?.status ?? 'error'}`);
    }
    // gc's receipt, read as the inbound path reads it (inbound.js verdictOf:
    // the wire's no_route, case and separators normalized). Pending means gc
    // took the event and holds it for a session: accepted, like delivered.
    // Failed, partial, no_route and an unusable receipt are undelivered.
    const { verdict } = verdictOf(answer?.delivery);
    if (verdict !== 'delivered' && verdict !== 'pending') undelivered(job, `delivery ${verdict}`);
  }

  function undelivered(job, reason) {
    const r = state.conversations[job.conversationId];
    if (!r) return;
    r.undelivered.push({ id: job.id, kind: job.kind, at: now(), reason });
    log(`keeper: ${tag(job.conversationId)}: ${job.kind} not delivered (${reason}); counted, never raised again`);
    persist();
  }

  function dropRetries(match) {
    for (let i = retries.length - 1; i >= 0; i--) if (match(retries[i])) retries.splice(i, 1);
  }

  async function retryDue(t) {
    const due = retries.filter((job) => job.dueAt <= t);
    if (!due.length) return;
    dropRetries((job) => due.includes(job));
    for (const job of due) {
      const r = state.conversations[job.conversationId];
      if (!r || r.consent !== true) continue; // consent withdrawn: dropped
      if (!listed(job.conversationId)) continue; // unbound at the gateway: dropped
      if (job.kind === 'nudge' && (r.window_closes_at !== job.close || t >= job.close)) {
        // A nudge never goes out after its close, nor after the business wrote.
        if (r.window_closes_at === job.close) undelivered(job, 'the window closed before the retry');
        continue;
      }
      await post(job);
    }
  }

  // --- tick ------------------------------------------------------------------

  async function tickOnce() {
    const t = now();
    await readWindow(t);
    await retryDue(t);
    for (const [conversationId, r] of Object.entries(state.conversations)) {
      if (r.consent !== true) continue; // (e) OFF without recorded consent: no event at all
      if (!listed(conversationId)) continue; // the last good read does not list it: no event (gp-g02t)
      try {
        const slot = dueUpdate(r, t);
        if (slot) await raiseUpdate(conversationId, r, slot, t);
        // The update's post awaited gc: consent --off may have landed
        // meanwhile (setters commit in place, so r is the live row).
        if (r.consent !== true) continue;
        // A read still owed (the first one, or the one after a delivery, a
        // delivery noted during this pass included) has not succeeded: the
        // business may just have written, so the ladder and the lapse wait
        // for a fresh clock rather than nudge on a stale one. The update
        // does not. The doctor row turns red when this lasts.
        if (readOwed()) continue;
        await runLadder(conversationId, r, t);
      } catch (err) {
        log(`keeper: ${tag(conversationId)}: ${err?.message ?? err}`);
      }
    }
  }

  function tick() {
    if (!running) {
      running = tickOnce()
        .catch((err) => log(`keeper: tick failed: ${err?.message ?? err}`))
        .finally(() => { running = null; });
    }
    return running;
  }

  // --- the onboarding fields and the counters ---------------------------------

  function replan(r, t) {
    if (r.window_closes_at != null) r.plan = planLadder(r, t);
  }

  return {
    tick,

    noteDelivery() {
      owedGen += 1;
    },

    // Throws a clear Error (status 400) on a bad hours string or zone; blank
    // means absent.
    validateHours(hours, tz) {
      checkFields(hours, tz);
    },

    // gc whatsapp invite [--proactive-consent] [--hours ...] [--tz ...]
    recordInvite(code, { consent = false, hours, tz } = {}) {
      const key = String(code ?? '').trim();
      if (!key) throw refusal(400, 'invite code is empty');
      const fields = checkFields(hours, tz);
      commit({ invites: { [key]: { consent: consent === true, hours: fields.hours, tz: fields.tz, recorded_at: now() } } });
    },

    // The phone joined with that code: the fields move onto the conversation.
    // Idempotent, so the inbound path calls it at every sight of the join
    // and at startup: true when the code is bound to that phone (now, or
    // already), false when there is nothing to bind. Throws on a failed
    // write, nothing changed.
    bindInvite(code, conversationId) {
      const key = String(code ?? '').trim();
      const id = String(conversationId);
      const invite = key && Object.hasOwn(state.invites, key) ? state.invites[key] : null;
      if (!invite) {
        logOnce(`no-invite:${key}:${id}`, `keeper: no onboarding record for that invite code; ${tag(id)} keeps its keeper record`);
        return false;
      }
      if (invite.bound_to != null) {
        if (invite.bound_to === id) return true; // a second sight of the same join
        logOnce(`bound:${key}:${id}`, `keeper: that invite code is already bound to ${tag(invite.bound_to)}; ${tag(id)} keeps its keeper record`);
        return false;
      }
      const t = now();
      const r = draftRow(id);
      if (invite.consent === true && r.consent !== true) r.consent_at = t;
      r.consent = invite.consent === true;
      r.hours = invite.hours ?? null;
      r.tz = invite.tz ?? null;
      replan(r, t);
      commit({ rows: { [id]: r }, invites: { [key]: { ...invite, bound_to: id, bound_at: t } } });
      return true;
    },

    // gc whatsapp consent <conversation_id> --on|--off. Returns the matched row's id;
    // an unknown conversation is refused (404), never recorded.
    setConsent(conversationId, on) {
      if (typeof on !== 'boolean') throw refusal(400, 'consent must be true (--on) or false (--off)');
      const id = knownConversation(conversationId);
      const r = draftRow(id);
      if (on && r.consent !== true) r.consent_at = now();
      r.consent = on;
      commit({ rows: { [id]: r } });
      if (!on) dropRetries((job) => job.conversationId === id);
      return id;
    },

    // gc whatsapp hours <conversation_id> --hours "..." --tz <zone> | --clear. Blank
    // hours clear them; tz undefined keeps the recorded zone, blank clears
    // it. Returns the matched row's id; an unknown conversation is refused (404).
    setHours(conversationId, hoursText, tz) {
      const fields = checkFields(hoursText, tz);
      const id = knownConversation(conversationId);
      const r = draftRow(id);
      r.hours = fields.hours;
      if (tz !== undefined) r.tz = fields.tz;
      replan(r, now());
      commit({ rows: { [id]: r } });
      return id;
    },

    // The gateway refused a free-form publish window_closed: counted, never a template.
    noteRefusedNudge(conversationId) {
      row(conversationId).refused_nudges.push({ at: now() });
      persist();
    },

    status() {
      const t = now();
      const since = t - STATUS_SPAN_MS;
      const recent = (list) => list.filter((x) => x.at >= since).length;
      const conversations = {};
      for (const [conversationId, r] of Object.entries(state.conversations)) {
        const zone = zoneOf(r);
        const close = r.window_closes_at;
        const plan = r.plan?.close_at === close ? r.plan : null;
        const fired = r.marks.close_at === close ? r.marks.fired : [];
        let today = null;
        try { today = localDate(t, zone); } catch { /* a bad zone shows as not today */ }
        conversations[conversationId] = {
          keeper: r.consent === true ? 'on' : 'off',
          consent: r.consent,
          listed: listed(conversationId),
          consent_at: iso(r.consent_at),
          hours: r.hours,
          tz: r.tz,
          zone,
          last_inbound_at: iso(r.last_inbound_at),
          window_closes_at: iso(close),
          window: close == null ? 'never' : t < close ? 'open' : 'closed',
          update_day: r.update_day,
          update_at: iso(r.update_at),
          update_raised_today: today != null && r.update_day === today,
          ladder: plan?.case ?? null,
          marks_planned: plan ? plan.marks.map((m) => ({ mark: m.mark, at: iso(m.at) })) : [],
          marks_fired: fired.filter((f) => f.event).map((f) => f.mark),
          marks_silent: fired.filter((f) => !f.event).map((f) => ({ mark: f.mark, why: f.why })),
          lapses_7d: recent(r.lapses),
          undelivered_7d: recent(r.undelivered),
          refused_nudges_7d: recent(r.refused_nudges),
        };
      }
      return {
        conversations,
        invites_pending: Object.values(state.invites).filter((inv) => inv.bound_to == null).length,
        // ok_at: the last good GET /v1/window; error: the last failure since.
        window_read: {
          ok_at: iso(state.window_read_ok_at),
          error: state.window_read_error,
          error_at: iso(state.window_read_error_at),
          failures: readFailures,
          retry_at: readFailures ? iso(readRetryAt) : null,
          owed: readOwed(),
        },
        retries_pending: retries.length,
      };
    },
  };
}
