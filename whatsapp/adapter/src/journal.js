// journal.js — the write-ahead inbound journal (design line 114).
//
// <city>/.gc/whatsapp/inbound_journal.jsonl, 0600 in a 0700 directory: one
// JSON line per state transition, each fsynced before the caller takes its
// next step, replayed at start by folding the lines per request id. The fold
// is the adapter's whole memory of what it owes gc and the gateway; nothing
// in process memory is the record.
//
// Line types (field t):
//   received      {rid, delivery_id, kind, wa_id, profile_name, envelope, received_at}
//   posted        {rid, at, reposts, boundary, marked}       before each POST /extmsg/inbound
//   accepted      {rid, receipt_id, verdict, terminal, sequence, members, via, at}
//   redelivered   {rid, at}                                  before the one marked copy
//   held_terminal {rid, receipt_id, members, at}
//   dead          {rid, reason, at}                          after the dead-letter file line
//   acknowledged  {rid, at}                                  after the gateway CONFIRMED the ack
//   watermark     {conversation_id, sequence, provider_message_id, accepted_at, rebased_at?}
//   watermark_cleared {conversation_id, at}                  clear-gap on an empty store
//   restore_gap   {conversation_id, watermark_sequence, watermark_accepted_at, head_sequence, head_created_at, found_at}
//   gap_cleared   {conversation_id, at}
//   probe         {rid, session_id, shape: log|pane|none|error, found, at}
//
// THE GATE r7's SECOND RULE: a dead line is an acknowledgement OWED, never a
// licence to compact. Compaction drops only an rid whose fold holds an
// acknowledged line; a dead rid without one survives every compaction, so
// the gateway's redelivery of it meets the dead entry, never the no-entry
// branch.

import fs from 'node:fs';
import path from 'node:path';

export const JOURNAL_FILE = 'inbound_journal.jsonl';
export const PROBE_RETENTION_MS = 7 * 24 * 60 * 60 * 1000;

const ridTypes = new Set(['received', 'posted', 'accepted', 'redelivered', 'held_terminal', 'dead', 'acknowledged']);

export function emptyFold() {
  return { entries: new Map(), watermarks: new Map(), gaps: new Map(), probes: [] };
}

function entryFor(fold, rid) {
  let e = fold.entries.get(rid);
  if (!e) {
    e = {
      rid,
      received: null,
      waId: '',
      deliveryId: undefined,
      kind: '',
      posts: 0,
      firstPosted: null,
      lastPosted: null,
      accepted: null,
      everAccepted: false,
      redelivered: false,
      markedPosted: false,
      held: null,
      dead: null,
      acknowledged: null,
      last: 'received',
      members: [],
    };
    fold.entries.set(rid, e);
  }
  return e;
}

// apply folds one line into the state. It is the only place a line's meaning
// is decided, so replay and live operation agree by construction.
export function applyLine(fold, line) {
  switch (line.t) {
    case 'received': {
      const e = entryFor(fold, line.rid);
      if (!e.received) {
        e.received = line;
        e.waId = String(line.wa_id ?? '');
        e.deliveryId = line.delivery_id;
        e.kind = line.kind;
      }
      return;
    }
    case 'posted': {
      const e = entryFor(fold, line.rid);
      e.posts += 1;
      if (!e.firstPosted) e.firstPosted = line;
      e.lastPosted = line;
      if (line.marked) e.markedPosted = true;
      e.last = 'posted';
      return;
    }
    case 'accepted': {
      const e = entryFor(fold, line.rid);
      e.accepted = line;
      e.everAccepted = true;
      if (Array.isArray(line.members) && line.members.length) e.members = line.members;
      e.last = 'accepted';
      // An accepted-pending line with terminal true IS held_terminal to the
      // fold, so a kill before the held_terminal line changes nothing.
      if (line.verdict === 'pending' && line.terminal === true) e.held = e.held || line;
      // The operator's --delivered (or any later delivered word) releases a hold.
      if (line.verdict === 'delivered') e.held = null;
      if (Number.isSafeInteger(line.sequence) && line.sequence > 0 && e.waId) {
        raiseWatermark(fold, e.waId, {
          sequence: line.sequence,
          provider_message_id: line.provider_message_id,
          accepted_at: line.at,
        });
      }
      return;
    }
    case 'redelivered': {
      const e = entryFor(fold, line.rid);
      e.redelivered = true;
      e.last = 'redelivered';
      return;
    }
    case 'held_terminal': {
      const e = entryFor(fold, line.rid);
      e.held = line;
      if (Array.isArray(line.members) && line.members.length) e.members = line.members;
      return;
    }
    case 'dead': {
      const e = entryFor(fold, line.rid);
      if (!e.dead) e.dead = line;
      return;
    }
    case 'acknowledged': {
      const e = entryFor(fold, line.rid);
      e.acknowledged = line;
      return;
    }
    case 'watermark':
      // An explicit watermark line sets the value exactly (the clear-gap
      // rebase is the one hand that may lower it); accepted lines only raise.
      fold.watermarks.set(String(line.conversation_id), {
        sequence: line.sequence,
        provider_message_id: line.provider_message_id,
        accepted_at: line.accepted_at,
        rebased_at: line.rebased_at,
      });
      return;
    case 'watermark_cleared':
      fold.watermarks.delete(String(line.conversation_id));
      return;
    case 'restore_gap':
      fold.gaps.set(String(line.conversation_id), line);
      return;
    case 'gap_cleared':
      fold.gaps.delete(String(line.conversation_id));
      return;
    case 'probe':
      fold.probes.push(line);
      return;
    default:
      return; // An unknown line from a newer adapter is kept and ignored.
  }
}

function raiseWatermark(fold, conversationId, wm) {
  const current = fold.watermarks.get(conversationId);
  if (current && current.sequence >= wm.sequence) return false;
  fold.watermarks.set(conversationId, { ...wm });
  return true;
}

// The entry's state, derived from the fold alone. Precedence: a confirmed
// ack retires the entry; a dead entry owes its ack; an operator's or a
// probe's delivered word owes its ack; a terminal hold waits for a hand;
// otherwise the last progress line decides.
export function entryState(e) {
  if (!e) return 'none';
  if (e.acknowledged) return 'acknowledged';
  if (e.dead) return 'dead';
  if (e.held) return 'held';
  switch (e.last) {
    case 'posted': return 'posted';
    case 'redelivered': return 'redeliver';
    case 'accepted': return `accepted:${e.accepted?.verdict ?? 'unsupported'}`;
    default: return 'received';
  }
}

// An ack is owed for a dead entry and for one gc delivered (or the probe or
// the operator vouched for), until the gateway confirms it.
export function ackOwed(e) {
  const state = entryState(e);
  return state === 'dead' || state === 'accepted:delivered' || state === 'accepted:unsupported';
}

export function readLines(file) {
  let text;
  try {
    text = fs.readFileSync(file, 'utf8');
  } catch (err) {
    if (err.code === 'ENOENT') return { lines: [], tornAt: -1 };
    throw err;
  }
  // Each line is written with its newline in one write and fsynced before
  // the caller moves on, so only a tail with no newline (a kill mid-write)
  // is torn; that transition was never confirmed and is dropped. A
  // non-JSON line anywhere else is corruption we refuse to guess about.
  const end = text.lastIndexOf('\n') + 1;
  const body = text.slice(0, end);
  const tornAt = end < text.length ? Buffer.byteLength(body) : -1;
  const lines = [];
  body.split('\n').forEach((raw, i) => {
    if (raw === '') return;
    try {
      lines.push(JSON.parse(raw));
    } catch {
      throw new Error(`${file} line ${i + 1} is not JSON; refusing to start on a corrupt journal`);
    }
  });
  return { lines, tornAt };
}

export function foldLines(lines) {
  const fold = emptyFold();
  for (const line of lines) applyLine(fold, line);
  return fold;
}

function fsyncDir(dir) {
  try {
    const fd = fs.openSync(dir, 'r');
    try { fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
  } catch { /* some filesystems refuse a directory fsync; the rename stands */ }
}

export function openJournal({ stateDir, clock = () => Date.now() }) {
  fs.mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  try { fs.chmodSync(stateDir, 0o700); } catch { /* best effort */ }
  const file = path.join(stateDir, JOURNAL_FILE);
  const { lines, tornAt } = readLines(file);
  if (tornAt >= 0) fs.truncateSync(file, tornAt);
  let fold = foldLines(lines);
  let fd = fs.openSync(file, 'a', 0o600);
  fs.chmodSync(file, 0o600);

  function append(line) {
    const stamped = { ...line };
    if (stamped.at === undefined && !('found_at' in stamped)) stamped.at = clock();
    fs.writeSync(fd, `${JSON.stringify(stamped)}\n`);
    fs.fsyncSync(fd);
    applyLine(fold, stamped);
    return stamped;
  }

  // Compaction: an atomic rewrite (temp file, fsync, rename, directory
  // fsync). Keeps every line of an rid with no acknowledged line (a dead rid
  // whose ack is unconfirmed included: the gate r7's second rule), each
  // conversation's current watermark, every standing gap, and the probe lines
  // of the last seven days (the probes doctor row counts them).
  function compact() {
    const { lines: current } = readLines(file);
    const now = clock();
    const kept = [];
    for (const line of current) {
      if (ridTypes.has(line.t)) {
        const e = fold.entries.get(line.rid);
        if (e && !e.acknowledged) kept.push(line);
      } else if (line.t === 'probe') {
        const e = fold.entries.get(line.rid);
        if ((e && !e.acknowledged) || now - (line.at ?? 0) < PROBE_RETENTION_MS) kept.push(line);
      }
    }
    // The gap and watermark lines go last, from the fold, so their final
    // value after replay is exactly the current one.
    for (const gap of fold.gaps.values()) kept.push(gap);
    for (const [conversationId, wm] of fold.watermarks) {
      kept.push({ t: 'watermark', conversation_id: conversationId, ...wm });
    }
    const tmp = `${file}.tmp-${process.pid}`;
    const out = fs.openSync(tmp, 'w', 0o600);
    try {
      fs.writeSync(out, kept.map((l) => `${JSON.stringify(l)}\n`).join(''));
      fs.fsyncSync(out);
    } finally {
      fs.closeSync(out);
    }
    fs.closeSync(fd);
    fs.renameSync(tmp, file);
    fsyncDir(stateDir);
    fd = fs.openSync(file, 'a', 0o600);
    // Prune the live fold to what the rewritten file folds to, keeping the
    // surviving entry objects themselves (in-flight steps hold them).
    let dropped = 0;
    for (const [rid, e] of fold.entries) {
      if (e.acknowledged) { fold.entries.delete(rid); dropped += 1; }
    }
    fold.probes = fold.probes.filter((p) => {
      const e = fold.entries.get(p.rid);
      return (e && !e.acknowledged) || now - (p.at ?? 0) < PROBE_RETENTION_MS;
    });
    return { lines: kept.length, droppedEntries: dropped };
  }

  return {
    file,
    get fold() { return fold; },
    get entries() { return fold.entries; },
    get watermarks() { return fold.watermarks; },
    get gaps() { return fold.gaps; },
    append,
    compact,
    close() { try { fs.closeSync(fd); } catch { /* already closed */ } },
  };
}
