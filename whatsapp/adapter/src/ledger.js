// ledger.js — the box's window ledger: the last inbound owner message per
// conversation (the owner's key at the gateway: render.js conversationOf,
// the same id the mayor's reply names), the 24-hour service window it
// opens, and the send ledger.
//
// Meta opens or extends the free-form window ONLY on an inbound customer
// message; a template is the one send allowed outside it and leaves it
// closed. So exactly one call moves the window, noteInbound, and no send of
// any type (text, reaction, template) ever does: noteSend appends to the
// send ledger and touches nothing else. The window closes at
// last_inbound_at + 24 hours - the margin (120 seconds), the same arithmetic
// the gateway's window.ts applies, so the box and the gateway agree on
// whether a send will be refused.
//
// The ledger is a FALLBACK between window reads, never the authority: the
// gateway's GET /v1/window (the keeper's read) and its 422 window_closed are.
// One concession to that: when the gateway refuses a send window_closed,
// noteWindowClosed records the refusal's time, and the window reads closed
// until an inbound newer than the refusal. Without it a ledger that believed
// the window open (a stale or skewed inbound time) would answer
// /publish-update 409 while the gateway refuses the free-form reply, and the
// mayor would have no path to the owner at all.
//
// Persisted at <stateDir>/ledger.json (dir 0700, file 0600), rewritten
// atomically on every change: temp file, fsync, rename, directory fsync. A
// write failure is logged and the in-memory ledger carries on (it is a
// fallback); an unreadable file is renamed aside and the ledger starts empty.

import fs from 'node:fs';
import path from 'node:path';

export const LEDGER_FILE = 'ledger.json';
export const WINDOW_MS = 24 * 60 * 60 * 1000;
export const DEFAULT_MARGIN_MS = 120 * 1000;
const SENDS_KEPT = 50; // per conversation, newest last

// writeFileAtomic replaces file with data so a reader (or a restart after a
// kill) sees the old content or the new, never a torn one. The temp name is
// short and fixed-length (a media file's own name can sit near the 255-byte
// component limit).
export function writeFileAtomic(file, data, { mode = 0o600, dirMode = 0o700 } = {}) {
  const dir = path.dirname(file);
  fs.mkdirSync(dir, { recursive: true, mode: dirMode });
  const tmp = path.join(dir, `.tmp-${process.pid}-${Math.random().toString(16).slice(2, 10)}`);
  let fd;
  try {
    fd = fs.openSync(tmp, 'w', mode);
    fs.writeSync(fd, data);
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fd = undefined;
    fs.renameSync(tmp, file);
  } catch (err) {
    if (fd !== undefined) {
      try { fs.closeSync(fd); } catch { /* already failing */ }
    }
    try { fs.rmSync(tmp, { force: true }); } catch { /* best effort */ }
    throw err;
  }
  try {
    const dfd = fs.openSync(dir, 'r');
    try { fs.fsyncSync(dfd); } finally { fs.closeSync(dfd); }
  } catch { /* a directory fsync is not supported everywhere */ }
}

// THE ONE LABEL of an id on every log line and operator surface (gp-g02t,
// codex self-review r2): its last four letters or digits. A conversation id
// is a phone number or a WhatsApp user id, which may hold letters; the verbs'
// confirmations, status, the doctor and the logs all show the same four.
export function lastFour(id) {
  const kept = String(id ?? '').replace(/[^0-9A-Za-z]/g, '');
  return kept.slice(-4) || '????';
}

// toMs reads an instant as epoch milliseconds: a number (seconds when it is
// too small to be milliseconds, as Meta's webhook timestamps are), a digit
// string, or an ISO string. Anything else is NaN.
export function toMs(at) {
  if (typeof at === 'string' && /^\d+(\.\d+)?$/.test(at.trim())) at = Number(at);
  if (typeof at === 'number') return Number.isFinite(at) ? (at > 0 && at < 1e11 ? at * 1000 : at) : NaN;
  if (typeof at === 'string') return Date.parse(at);
  if (at instanceof Date) return at.getTime();
  return NaN;
}

export function createLedger({ stateDir, clock = () => Date.now(), marginMs = DEFAULT_MARGIN_MS, log = () => {} } = {}) {
  const file = path.join(stateDir, LEDGER_FILE);
  const rows = new Map(); // conversation id -> { last_inbound_at, closed_at, sends: [{type, message_id, at}] }

  try {
    const raw = JSON.parse(fs.readFileSync(file, 'utf8'));
    for (const [conversationId, row] of Object.entries(raw?.conversations ?? {})) {
      const last = Number(row?.last_inbound_at);
      const closed = Number(row?.closed_at);
      rows.set(conversationId, {
        last_inbound_at: Number.isFinite(last) && last > 0 ? last : null,
        closed_at: Number.isFinite(closed) && closed > 0 ? closed : null,
        sends: Array.isArray(row?.sends) ? row.sends.slice(-SENDS_KEPT) : [],
      });
    }
  } catch (err) {
    if (err.code !== 'ENOENT') {
      const aside = `${file}.corrupt-${Date.now()}`;
      try { fs.renameSync(file, aside); } catch { /* leave it */ }
      log(`window ledger unreadable (${err.code ?? 'parse error'}); set aside, starting empty`);
    }
  }

  const row = (conversationId) => {
    const key = String(conversationId);
    let r = rows.get(key);
    if (!r) {
      r = { last_inbound_at: null, closed_at: null, sends: [] };
      rows.set(key, r);
    }
    return r;
  };

  const serialize = () => {
    const conversations = {};
    for (const [conversationId, r] of rows) {
      conversations[conversationId] = {
        last_inbound_at: r.last_inbound_at,
        window_closes_at: r.last_inbound_at === null ? null : r.last_inbound_at + WINDOW_MS - marginMs,
        closed_at: r.closed_at,
        sends: r.sends.map((s) => ({ ...s })),
      };
    }
    return { version: 1, margin_ms: marginMs, conversations };
  };

  const persist = () => {
    try {
      writeFileAtomic(file, `${JSON.stringify(serialize())}\n`);
    } catch (err) {
      log(`window ledger write failed (${err.code ?? err.message}); kept in memory`);
    }
  };

  const windowClosesAt = (conversationId) => {
    const r = rows.get(String(conversationId));
    if (!r || r.last_inbound_at === null) return null;
    return r.last_inbound_at + WINDOW_MS - marginMs;
  };

  return {
    // The ONE call that opens or extends the window: an inbound owner
    // message at atMs. Only ever moves forward.
    noteInbound(conversationId, atMs) {
      const at = toMs(atMs);
      if (!Number.isFinite(at) || at <= 0) return;
      const r = row(conversationId);
      if (r.last_inbound_at !== null && at <= r.last_inbound_at) return;
      r.last_inbound_at = at;
      persist();
    },
    windowClosesAt,
    windowOpen(conversationId, now = clock()) {
      const closes = windowClosesAt(conversationId);
      if (closes === null || now >= closes) return false;
      const r = rows.get(String(conversationId));
      if (r.closed_at !== null && r.closed_at >= r.last_inbound_at) return false;
      return true;
    },
    // The gateway refused a free-form send window_closed at atMs: closed
    // until an inbound newer than that. Never opens anything.
    noteWindowClosed(conversationId, atMs = clock()) {
      const at = toMs(atMs);
      if (!Number.isFinite(at)) return;
      const r = row(conversationId);
      if (r.closed_at !== null && at <= r.closed_at) return;
      r.closed_at = at;
      persist();
    },
    // The send ledger: every send the box made, any type. Never moves the
    // window.
    noteSend(conversationId, { type, message_id, at } = {}) {
      const r = row(conversationId);
      const when = toMs(at);
      r.sends.push({
        type: String(type ?? ''),
        message_id: String(message_id ?? ''),
        at: Number.isFinite(when) ? when : clock(),
      });
      if (r.sends.length > SENDS_KEPT) r.sends.splice(0, r.sends.length - SENDS_KEPT);
      persist();
    },
    snapshot: serialize,
  };
}
