#!/usr/bin/env node
'use strict';

// No dependencies, network clients, message bodies, or actor names.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const os = require('node:os');
const { isDeepStrictEqual } = require('node:util');
const { performance } = require('node:perf_hooks');
const { execFileSync } = require('node:child_process');

const DAY = 86400000;
const LIMITS = Object.freeze({
  wait: 30000, refusals: 3, age: DAY, history: 14 * DAY, deadline: 2000,
  receiptAge: 600000, receipts: 1000, calls: 1000, log: 2000, sessions: 1024,
  known: 1024, errors: 100, lockAge: 10000, writeBytes: 64 * 1024, backups: 3,
});
const CHUNK = 8 * 1024 * 1024;
const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const key = row => `${row.provider}/${row.conversation_id}`;
function initialState() {
  return { version: 2, generation: 0, cursor: null, waiting: [], exemptions: [], known: {}, sessions: {}, calls: {}, receipts: {}, log: [], now: 0, turn: '',
    skipped: { count: 0, last_seq: null, last_reason: null, last_at: null }, history_dropped: 0, hook_ok_at: null };
}
function checkDeadline(start) {
  if (performance.now() - start >= LIMITS.deadline) throw new Error('measurement_timeout');
}
function identifier(s) { return typeof s === 'string' && s.length > 0 && s.length <= 4096 && !/[\x00-\x20\x7f]/.test(s); }
const SKIP = Symbol('unusable event');
const INFERRED_SEQ = Symbol('inferred sequence');
function eventTime(ts) {
  return typeof ts === 'string' || typeof ts === 'number' ? Date.parse(ts) : NaN;
}
function parseRows(bytes, progress, start) {
  const rows = [];
  const minimum = progress.seq;
  const lines = bytes.toString('utf8').split('\n');
  lines.pop(); // The caller supplies only complete newline-terminated bytes.
  for (const line of lines) {
    checkDeadline(start);
    let row, reason;
    try { row = JSON.parse(line); } catch { reason = 'invalid_event_json'; }
    if (!Number.isSafeInteger(row?.seq) || row.seq <= 0) {
      progress.hadUnsequenced = true;
      progress.unsequenced++;
      rows.push({ ...row, seq: progress.seq, [INFERRED_SEQ]: true, [SKIP]: reason || 'invalid_event_row' });
      continue;
    }
    if (row.seq <= minimum) continue;
    if (progress.unsequenced) {
      const extra = row.seq - progress.seq - 1 - progress.unsequenced;
      if (extra > 0) rows.push({ [SKIP]: 'gap', extra });
      if (row.seq <= progress.seq) throw new Error('event_sequence_gap');
      progress.unsequenced = 0;
    } else if (progress.seq && row.seq !== progress.seq + 1) throw new Error('event_sequence_gap');
    progress.seq = row.seq;
    if (typeof row.type !== 'string' || !Number.isFinite(eventTime(row.ts))) {
      rows.push({ ...row, [SKIP]: 'invalid_event_row' });
    } else rows.push(row);
  }
  return rows;
}

// A short-lived Node child streams one decoded archive slice. It never
// materializes the whole archive; the cursor persists the decoded offset.
// Replaying the compressed prefix is necessary because gzip has no seek index.
function archiveWorker() {
  const fs = require('node:fs');
  const zlib = require('node:zlib');
  const input = fs.createReadStream(process.argv[1]);
  const stream = input.pipe(zlib.createGunzip());
  let skip = Number(process.argv[2]), left = Number(process.argv[3]);
  const retry = new Int32Array(new SharedArrayBuffer(4));
  input.on('error', () => process.exit(1));
  stream.on('error', () => process.exit(1));
  stream.on('data', chunk => {
    const n = Math.min(skip, chunk.length); skip -= n; chunk = chunk.subarray(n);
    const part = chunk.subarray(0, left); left -= part.length;
    for (let written = 0; written < part.length;) {
      try { written += fs.writeSync(1, part.subarray(written)); }
      catch (error) {
        if (error.code !== 'EAGAIN') throw error;
        // archiveSlice's parent timeout bounds retries to the remaining deadline.
        Atomics.wait(retry, 0, 0, 1);
      }
    }
    if (!left) { input.destroy(); stream.destroy(); }
  });
}
function archiveSlice(file, offset, start) {
  checkDeadline(start);
  return execFileSync(process.execPath, ['-e', `(${archiveWorker.toString()})()`, file, String(offset), String(CHUNK)], {
    timeout: Math.max(1, Math.floor(LIMITS.deadline - (performance.now() - start))),
    maxBuffer: CHUNK + 65536, stdio: ['ignore', 'pipe', 'pipe'],
  });
}
function lastSequence(bytes, truncated) {
  for (let stop = bytes.lastIndexOf(10); stop > 0;) {
    const begin = bytes.lastIndexOf(10, stop - 1) + 1;
    if (!begin && truncated) break;
    let row;
    try { row = JSON.parse(bytes.subarray(begin, stop).toString()); } catch { /* skip an unreadable line */ }
    if (Number.isSafeInteger(row?.seq) && row.seq > 0) return row.seq;
    stop = begin - 1;
  }
  return 0;
}
function sequenceAtOffset(fd, offset) {
  const limit = Math.min(CHUNK + 1, offset);
  for (let size = Math.min(512, limit);;) {
    const bytes = Buffer.alloc(size);
    fs.readSync(fd, bytes, 0, size, offset - size);
    const seq = lastSequence(bytes, offset > size);
    if (seq || size === limit) return seq;
    size = Math.min(size * 4, limit);
  }
}
function eventFence(city) {
  const dir = path.join(city, '.gc');
  for (let attempt = 0; attempt < 3; attempt++) {
    const names = fs.readdirSync(dir).sort();
    let seq = 0;
    for (const name of names) {
      const archive = /^events\.jsonl\.archive-\d{8}T\d{6}Z-seq-\d+-(\d+)\.gz$/.exec(name);
      if (archive) { seq = Math.max(seq, Number(archive[1])); continue; }
      const rotating = /^events\.jsonl\.rotating-\d{8}T\d{6}Z(?:-seq-\d+-(\d+))?$/.exec(name);
      if (name !== 'events.jsonl' && !rotating) continue;
      // The seq suffix survives a concurrent rename/removal of the file.
      if (rotating?.[1]) seq = Math.max(seq, Number(rotating[1]));
      let fd;
      try {
        fd = fs.openSync(path.join(dir, name), 'r');
        const size = fs.fstatSync(fd).size;
        const bytes = Buffer.alloc(Math.min(size, CHUNK + 1));
        fs.readSync(fd, bytes, 0, bytes.length, size - bytes.length);
        // A damaged tail has no usable fence of its own. The reader counts
        // it and releases ambiguous waits when it consumes those bytes.
        const end = bytes.lastIndexOf(10) + 1;
        const completeOffset = size - bytes.length + end;
        const last = end && end < bytes.length ? sequenceAtOffset(fd, completeOffset) : lastSequence(bytes, size > bytes.length);
        seq = Math.max(seq, last);
      } catch (e) { if (e.code !== 'ENOENT') throw e; }
      finally { if (fd !== undefined) fs.closeSync(fd); }
    }
    if (JSON.stringify(names) === JSON.stringify(fs.readdirSync(dir).sort())) return seq;
  }
  throw new Error('rotation_during_snapshot');
}

function stopIdentity(input) {
  if (identifier(input.prompt_id)) return input.prompt_id;
  if (!input.transcript_path || !path.isAbsolute(input.transcript_path)) throw new Error('missing_turn_identity');
  const fd = fs.openSync(input.transcript_path, 'r');
  try {
    const size = fs.fstatSync(fd).size;
    const bytes = Buffer.alloc(Math.min(size, CHUNK));
    fs.readSync(fd, bytes, 0, bytes.length, size - bytes.length);
    const lines = bytes.subarray(0, bytes.lastIndexOf(10) + 1).toString('utf8').split('\n');
    // A user row is stable across duplicate Stop callbacks, including when
    // the assistant row is flushed between them. Persist only its hash.
    for (let i = lines.length - 1; i >= (size > CHUNK ? 1 : 0); i--) {
      if (!lines[i]) continue;
      const row = JSON.parse(lines[i]);
      if (row.type === 'user' && !row.isSidechain && identifier(row.uuid)) return row.uuid;
    }
    throw new Error('missing_turn_identity');
  } finally { fs.closeSync(fd); }
}

// The active file is read through one descriptor. A retained boundary digest
// also catches truncate-and-rewrite on a filesystem that reuses the inode.
function readEvents(city, cursor, { start = performance.now() } = {}) {
  const dir = path.join(city, '.gc');
  const file = path.join(dir, 'events.jsonl');
  const fd = fs.openSync(file, 'r');
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile()) throw new Error('event_log_not_regular');
    let offset = 0;
    let rows = [];
    const progress = { seq: cursor?.seq || 0, unsequenced: cursor?.unsequenced || 0, hadUnsequenced: Boolean(cursor?.hadUnsequenced || cursor?.unsequenced) };
    let archivedThrough = 0;
    let matchingPrefix = false;
    if (cursor && !cursor.archive && cursor.offset && cursor.offset <= stat.size) {
      const boundary = Buffer.alloc(Math.min(128, cursor.offset));
      fs.readSync(fd, boundary, 0, boundary.length, cursor.offset - boundary.length);
      matchingPrefix = hash(boundary) === cursor.boundary;
      if (matchingPrefix) {
        // A recycled inode or an in-place rewrite can preserve the trailing
        // payload while changing the sequence at the beginning of that row.
        const seq = sequenceAtOffset(fd, cursor.offset);
        matchingPrefix = seq ? seq === cursor.seq : !cursor.seq || cursor.unsequenced > 0;
      }
    }
    const sameEmpty = cursor?.offset === 0 && !stat.size && cursor.emptyChanged === stat.ctimeMs;
    const same = cursor && !cursor.archive && cursor.dev === stat.dev && cursor.ino === stat.ino && (matchingPrefix || sameEmpty);
    if (same) offset = cursor.offset;
    else {
      const names = fs.readdirSync(dir);
      // GC publishes the new active log before the compressed archive. Even
      // a fresh cursor must wait for those captured sequences to be visible.
      const pending = names.some(name => {
        const m = /^events\.jsonl\.rotating-(\d{8}T\d{6}Z)(?:-seq-(\d+)-(\d+))?$/.exec(name);
        if (!m) return false;
        if (m[3]) return Number(m[3]) > progress.seq && !names.includes(`events.jsonl.archive-${m[1]}-seq-${m[2]}-${m[3]}.gz`);
        // Legacy rotating names have no sequence suffix. Their matching
        // published archive is the evidence that replay can proceed.
        return !names.some(n => new RegExp(`^events\\.jsonl\\.archive-${m[1]}-seq-\\d+-\\d+\\.gz$`).test(n));
      });
      if (pending) return { rows: [], caughtUp: false, cursor: cursor || { dev: 0, ino: 0, offset: 0, seq: 0, boundary: '' } };
      const archives = names.flatMap(name => {
        const m = /^events\.jsonl\.archive-(\d{8}T\d{6}Z)-seq-(\d+)-(\d+)\.gz$/.exec(name);
        if (!m) return [];
        archivedThrough = Math.max(archivedThrough, Number(m[3]));
        // Message expiry belongs in reduction. Older routing and wake rows
        // still identify the authors of replies delivered within 24 hours.
        return Number(m[3]) > progress.seq ? [{ name, first: Number(m[2]), last: Number(m[3]) }] : [];
      }).sort((a, b) => a.first - b.first);
      for (const [index, archive] of archives.entries()) {
        checkDeadline(start);
        const file = path.join(dir, archive.name);
        let decodedOffset = cursor?.archive?.name === archive.name ? cursor.archive.offset : 0;
        // A rotated archive contains the old active file verbatim. Its byte
        // fence also identifies damaged lines that have no sequence number.
        if (progress.hadUnsequenced && !cursor?.archive && cursor?.offset && archive.first <= cursor.seq + (cursor.unsequenced || 0)) {
          const boundarySize = Math.min(128, cursor.offset);
          const slice = archiveSlice(file, cursor.offset - boundarySize, start);
          if (slice.length >= boundarySize && hash(slice.subarray(0, boundarySize)) === cursor.boundary) {
            decodedOffset = cursor.offset;
          }
        }
        const contents = archiveSlice(file, decodedOffset, start);
        const fullSlice = contents.length === CHUNK;
        const end = contents.lastIndexOf(10) + 1;
        if (!fullSlice && end !== contents.length) throw new Error('incomplete_archive');
        if (!end && fullSlice) throw new Error('event_row_too_large');
        const recovered = parseRows(contents.subarray(0, end), progress, start);
        for (const row of recovered) rows.push(row);
        if (fullSlice) return { rows, caughtUp: false, cursor: { dev: cursor?.dev || 0, ino: cursor?.ino || 0, offset: cursor?.offset || 0, boundary: cursor?.boundary || '', ...progress, archive: { name: archive.name, offset: decodedOffset + end } } };
        if (progress.unsequenced && archive.last > progress.seq) {
          const extra = archive.last - progress.seq - progress.unsequenced;
          if (extra > 0) rows.push({ [SKIP]: 'gap', extra });
          progress.seq = archive.last;
          progress.unsequenced = 0;
        }
        // Persist progress between small archives too; their aggregate can
        // exceed the deadline even when each one fits in a single slice.
        if (index + 1 < archives.length) return { rows, caughtUp: false, cursor: { dev: cursor?.dev || 0, ino: cursor?.ino || 0, offset: cursor?.offset || 0, boundary: cursor?.boundary || '', ...progress } };
      }
      // Still discover missing archives when an inode changes. Bytes known
      // to match the old active prefix need no second reduction.
      if (matchingPrefix) offset = cursor.offset;
    }
    checkDeadline(start);
    // Initial catch-up is bounded and continues on later calls. No refusal is
    // possible until caughtUp, because a later reply may still be unread.
    const bytes = Buffer.alloc(Math.min(stat.size - offset, CHUNK));
    const count = fs.readSync(fd, bytes, 0, bytes.length, offset);
    const end = bytes.subarray(0, count).lastIndexOf(10) + 1;
    if (!end && count === bytes.length && count === 8 * 1024 * 1024) throw new Error('event_row_too_large');
    const appended = parseRows(bytes.subarray(0, end), progress, start);
    for (const row of appended) rows.push(row);
    // Pending publication was checked before replay; parseRows checks each
    // recovered sequence. An empty replacement needs no sentinel row once
    // replay reaches the published tail (including the saved cursor).
    if (!same && !appended.length) {
      if (stat.size === 0) {
        // A legacy rotating name has only second precision. An older archive
        // can share its timestamp, so confirm the actual rotating tail before
        // adopting an identity that would bypass discovery on the next call.
        if (eventFence(city) > archivedThrough) return { rows, caughtUp: false, cursor: { ...(cursor || { dev: 0, ino: 0, offset: 0, boundary: '' }), ...progress, archive: undefined } };
        if (archivedThrough !== progress.seq) throw new Error('event_sequence_gap');
      } else if (cursor && !matchingPrefix) return { rows, caughtUp: false, cursor: { ...cursor, ...progress, archive: undefined } };
    }
    offset += end;
    const boundary = Buffer.alloc(Math.min(128, offset));
    fs.readSync(fd, boundary, 0, boundary.length, offset - boundary.length);
    checkDeadline(start);
    return { rows, caughtUp: offset === stat.size, cursor: { dev: stat.dev, ino: stat.ino, offset, ...progress, boundary: hash(boundary),
      ...(stat.size ? {} : { emptyChanged: stat.ctimeMs }) } };
  } finally { fs.closeSync(fd); }
}

function trimMap(map, cutoff, cap, timeOf) {
  const entries = [];
  for (const [id, value] of Object.entries(map)) {
    const at = timeOf(value);
    if (at <= cutoff) delete map[id];
    else entries.push([id, at]);
  }
  if (entries.length > cap) {
    entries.sort((a, b) => a[1] - b[1]);
    for (let i = 0; i < entries.length - cap; i++) delete map[entries[i][0]];
  }
}
function trimLog(state, now) {
  state.log = state.log.filter(e => now - e.at <= LIMITS.history);
  if (state.log.length > LIMITS.log) {
    state.history_dropped += state.log.length - LIMITS.log;
    state.log = state.log.slice(-LIMITS.log);
  }
}
// Rows arrive in sequence order. Reinsert equal timestamps so the stable
// time sort in trimMap also preserves sequence order across saves.
function remember(map, id, at) {
  if (at < (map[id] ?? -Infinity)) return;
  if (at === map[id]) delete map[id];
  map[id] = at;
}
function skipEvent(state, row, reason, now, lastSeq = row.seq) {
  state.skipped = { count: state.skipped.count + (reason === 'gap' ? row.extra : 1), last_seq: lastSeq, last_reason: reason, last_at: now };
  if (typeof row.type === 'string' && row.type !== 'extmsg.outbound') return;
  const p = row.payload;
  const conv = row.type === 'extmsg.outbound' && identifier(p?.provider) && identifier(p?.conversation_id) ? key(p) : null;
  const at = eventTime(row.ts);
  for (const w of state.waiting) {
    if ((!conv || conv === key(w)) && (!Number.isFinite(at) || w.at <= at)) w.released = true;
  }
}
function reduceEvents(state, rows, { now, agent, session, sessionName }) {
  if (state.now > now) throw new Error('clock_moved_backwards');
  const own = state.sessions;
  trimMap(own, now - LIMITS.history, LIMITS.sessions, Number);
  if (session) own[hash(session)] = now;
  if (sessionName) own[hash(sessionName)] = now;
  // A later wake can identify an earlier reply in this batch. Discover
  // identities first, then apply their timestamps in event sequence order.
  const recognized = new Set(Object.keys(own));
  const learned = new Map();
  for (const row of rows) {
    if (row[SKIP]) continue;
    const p = row.payload;
    const at = eventTime(row.ts);
    if (at > now || now - at >= LIMITS.history) continue;
    if (row.type === 'session.woke' && (row.subject === agent || row.subject === sessionName) && identifier(row.session_id)) {
      const ids = [hash(row.session_id)];
      if (identifier(row.subject)) ids.push(hash(row.subject));
      learned.set(row, ids);
    }
    if (row.type === 'extmsg.inbound' && p?.target_agent === agent && identifier(p.target_session) && identifier(p.provider) && identifier(p.conversation_id)) learned.set(row, [hash(p.target_session)]);
    for (const id of learned.get(row) || []) recognized.add(id);
  }
  let lastSeq = state.last_readable_seq ?? state.cursor?.seq ?? 0;
  for (const row of rows) {
    if (!row[INFERRED_SEQ] && Number.isSafeInteger(row.seq)) lastSeq = row.seq;
    if (row[SKIP]) {
      const target = row.payload?.target_agent;
      if (row.type === 'extmsg.inbound' && identifier(target) && target !== agent) continue;
      const publisher = identifier(row.subject) ? row.subject : row.payload?.session;
      if (row.type === 'extmsg.outbound' && identifier(publisher) && !recognized.has(hash(publisher))) continue;
      skipEvent(state, row, row[SKIP], now, lastSeq);
      continue;
    }
    const at = eventTime(row.ts);
    for (const id of learned.get(row) || []) remember(own, id, at);
    if (!['extmsg.inbound', 'extmsg.outbound'].includes(row.type)) continue;
    const p = row.payload || {};
    const publishingSession = identifier(row.subject) ? row.subject : p.session;
    if (row.type === 'extmsg.inbound') {
      if (typeof p.target_agent !== 'string' && p.target_agent !== undefined) { skipEvent(state, row, 'invalid_target_agent', now); continue; }
      if (p.target_agent && p.target_agent !== agent) continue;
      if (!p.target_agent && identifier(p.target_session)) {
        if (!recognized.size && now - at < LIMITS.age) throw new Error('mayor_session_unknown');
        if (!recognized.has(hash(p.target_session))) continue;
        if (at <= now) remember(own, hash(p.target_session), at);
      } else if (!p.target_agent && row.payload) continue;
    } else if (identifier(publishingSession) && !recognized.has(hash(publishingSession))) continue;
    if (!identifier(p.provider) || !identifier(p.conversation_id)) { skipEvent(state, row, 'invalid_message_event', now); continue; }
    if (at > now) { skipEvent(state, row, 'event_in_future', now); continue; }
    if (row.type === 'extmsg.inbound') {
      const conv = key(p);
      remember(state.known, conv, at);
      if (now - at >= LIMITS.age || state.exemptions.includes(conv)) continue;
      for (const call of Object.values(state.calls)) {
        if (call.priorWaiting && row.seq > (call.seenThrough || 0) && row.seq <= call.startThrough && !call.priorWaiting.includes(conv)) call.priorWaiting.push(conv);
        if (at >= call.at && (!call.ended || at <= call.ended.at)) call.arrived = true;
      }
      if (row.seq <= (state.seenThrough || 0)) continue;
      state.waiting.push({ provider: p.provider, conversation_id: p.conversation_id, at, seq: row.seq, refusals: 0, released: false });
    } else {
      // GC credits the resolved publishing session in subject. The payload
      // retains the caller's selector, which can be an alias or empty.
      if (!identifier(publishingSession)) { skipEvent(state, row, 'invalid_outbound_session', now); continue; }
      if (recognized.has(hash(publishingSession))) {
        remember(own, hash(publishingSession), at);
        state.waiting = state.waiting.filter(w => key(w) !== key(p) || w.at > at);
        for (const call of Object.values(state.calls)) if (call.priorWaiting && row.seq <= call.startThrough) call.priorWaiting = call.priorWaiting.filter(c => c !== key(p));
      }
    }
  }
  state.last_readable_seq = lastSeq;
  trimMap(own, now - LIMITS.history, LIMITS.sessions, Number);
  trimMap(state.known, now - LIMITS.history, LIMITS.known, Number);
  trimMap(state.receipts, now - LIMITS.receiptAge, LIMITS.receipts, v => v.at);
  trimMap(state.calls, now - LIMITS.age, LIMITS.calls, v => v.at);
  state.waiting = state.waiting.filter(w => now - w.at < LIMITS.age && !state.exemptions.includes(key(w)));
  state.waiting.sort((a, b) => a.at - b.at || a.seq - b.seq);
  trimLog(state, now);
  state.now = now;
  return state;
}

// Measured in Claude Code 2.1.309 (Linux) and 2.1.322 (macOS). Imported
// names are resolved locally by install; no channel name is hard-coded as
// an executable alias. Only the harness's own tools belong in this table.
const POLICY = Object.freeze({
  readOnly: ['Read', 'Glob', 'Grep', 'WebFetch', 'WebSearch', 'ListMcpResources', 'ReadMcpResource', 'ToolSearch', 'TaskGet', 'TaskList', 'TaskOutput'],
  packs: ['slack-full', 'gchat', 'whatsapp', 'reachable'],
  control: ['handoff'],
});

// A deliberately small shell grammar: one literal argv, no expansion,
// redirect, pipeline, assignment prefix, or second command. Quotes may
// contain punctuation as data; expansions inside double quotes may not.
const AND = Symbol('&&');
const clauses = {
  work_tool: 'this tool does work',
  not_talk: 'the shell command is not one gc talk command',
  chained: 'the shell command chains, pipes, redirects, groups or backgrounds commands outside quotes',
  expansion: 'the shell command can expand $ or a backquote into a command',
  glob: 'the shell command has an unquoted glob, tilde, brace or comment character',
  line_break: 'the shell command breaks a line outside quotes',
  unclosed: 'the shell command leaves a quote open or ends in a backslash',
  cd_form: 'only one cd DIR && may stand before the talk command',
  write_place: 'a Write runs here only for a reply body of at most 64 KiB under the temp directory',
};
function shellWords(command, prefixOnly = false) {
  if (typeof command !== 'string' || command.includes('\0')) return { reason: 'not_talk' };
  const args = [];
  let word = '', quote = '', started = false;
  const finish = () => { if (started) args.push(word); word = ''; started = false; };
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    if (prefixOnly && !quote && /[ \t\r\n;&|<>()]/.test(c)) {
      finish();
      const start = args[0] === 'cd' ? 3 : 0;
      if (args.length >= start + 2) return { args };
      if (c === ';' && start && args.length === 2) { args.push(AND); continue; }
    }
    if (quote === "'") { if (c === "'") quote = ''; else word += c; continue; }
    if (c === '`') return { reason: 'expansion' };
    if (c === '$' && (!quote || /^[$](?:[({\[]|[A-Za-z_]\w*\[)/.test(command.slice(i).replace(/\\\n/g, '')))) return { reason: 'expansion' };
    if (c === '\\') {
      if (++i === command.length) return { reason: 'unclosed' };
      const next = command[i];
      if (!quote && /[\r\n]/.test(next)) return { reason: 'line_break' };
      if (!quote && /[\x00-\x08\x0b-\x1f\x7f]/.test(next)) return { reason: 'not_talk' };
      if (quote && !/["\\$`\n]/.test(next)) word += '\\';
      if (!quote || next !== '\n') word += next;
      started = true; continue;
    }
    if (quote === '"') { if (c === '"') quote = ''; else word += c; continue; }
    if (c === "'" || c === '"') { quote = c; started = true; continue; }
    if (c === '&' && command[i + 1] === '&') { finish(); args.push(AND); i++; continue; }
    if (/[;&|<>()]/.test(c)) return { reason: 'chained' };
    if (/[#*?{}\[\]~]/.test(c)) return { reason: 'glob' };
    if (/[\r\n]/.test(c)) return { reason: 'line_break' };
    if (/[\x00-\x08\x0b-\x1f\x7f]/.test(c)) return { reason: 'not_talk' };
    if (/[ \t]/.test(c)) finish();
    else { word += c; started = true; }
  }
  if (quote) return { reason: 'unclosed' };
  finish();
  return { args };
}
function literalArgv(command) {
  const { args } = shellWords(command);
  return args && !args.includes(AND) ? args : null;
}
function tempBody(input) {
  const file = input?.file_path;
  if (typeof file !== 'string' || !path.isAbsolute(file) || typeof input.content !== 'string' || Buffer.byteLength(input.content) > LIMITS.writeBytes) return false;
  try {
    const stat = fs.lstatSync(file, { throwIfNoEntry: false });
    if (stat && !stat.isFile()) return false;
    // Resolve path components in filesystem order. Lexically collapsing '..'
    // before following a symlink could check a different directory.
    let resolved = path.parse(file).root;
    for (const part of file.split(path.sep).filter(Boolean)) {
      resolved = path.join(resolved, part);
      const st = fs.lstatSync(resolved, { throwIfNoEntry: false });
      if (st) resolved = fs.realpathSync(resolved);
    }
    return [os.tmpdir(), '/tmp'].some(dir => {
      const relative = path.relative(fs.realpathSync(dir), resolved);
      return relative !== '' && relative !== '..' && !relative.startsWith('..' + path.sep) && !path.isAbsolute(relative);
    });
  } catch { return false; }
}
function refusalCode(input, aliases) {
  if (POLICY.readOnly.includes(input.tool_name)) return null;
  if (input.tool_name === 'Write') return tempBody(input.tool_input) ? null : 'write_place';
  if (input.tool_name !== 'Bash') return 'work_tool';
  const parsed = shellWords(input.tool_input?.command);
  if (parsed.reason) return parsed.reason;
  let args = parsed.args;
  const prefixed = args[0] === 'cd';
  if (prefixed) {
    if (typeof args[1] !== 'string' || args[2] !== AND || args[3] !== 'gc') return 'cd_form';
    args = args.slice(3);
  }
  if (args.includes(AND)) return 'chained';
  const talk = args[0] === 'gc' && (POLICY.control.includes(args[1]) || (aliases.includes(args[1]) && args.length >= 3 && !args[2].startsWith('-')));
  return talk ? null : 'not_talk';
}
function allowed(input, aliases) {
  return !refusalCode(input, aliases);
}
function replyShaped(input, aliases) {
  if (input.tool_name !== 'Bash') return false;
  let { args } = shellWords(input.tool_input?.command, true);
  if (!args) return false;
  if (args[0] === 'cd' && typeof args[1] === 'string' && args[2] === AND) args = args.slice(3);
  return args[0] === 'gc' && (aliases.includes(args[1]) || POLICY.control.includes(args[1]));
}
function refusal(waiting, now, ownAlias = 'reachable', code) {
  const lines = waiting.map(w => `A person has waited ${Math.floor((now - w.at) / 1000)} seconds for an answer in ${key(w)}. Answer there now in one line: say what you are doing and when they will hear more. Then go on. If no words are needed, run: gc ${ownAlias} pass ${key(w)}. A job that will run past about 30 seconds does not belong in this session: start it as a background subagent, or hand it to a worker as a bead.`);
  if (code) lines.push('Refused because ' + clauses[code] + '.');
  return lines.join('\n');
}
function decide(state, input, options) {
  const { now, mode, session, aliases = [], caughtUp = true, waitMs = LIMITS.wait } = options;
  if (mode === 'off' || input.agent_id) return {};
  const event = input.hook_event_name;
  const token = event === 'Stop' ? input.prompt_id || options.turnId : input.tool_use_id;
  if (event === 'Stop' && !identifier(token)) throw new Error('missing_turn_identity');
  // Use the harness prompt ID or its persisted user-row UUID, never response
  // content or a previous tool call shared by distinct tool-free turns.
  const id = hash(`${session}:${event}:${token}`);
  if (state.receipts[id] && now - state.receipts[id].at < LIMITS.receiptAge) return state.receipts[id].output;
  let output = {};
  if (event === 'Stop') {
    state.seenThrough = Math.max(state.seenThrough || 0, options.fence ?? state.cursor?.seq ?? Math.max(0, ...state.waiting.map(w => w.seq)));
    state.waiting = state.waiting.filter(w => w.seq > state.seenThrough);
  }
  else if (event === 'PreToolUse') {
    if (!identifier(input.tool_use_id) || !identifier(input.tool_name)) throw new Error('invalid_tool_input');
    state.turn = hash(input.tool_use_id);
    const overdue = caughtUp ? state.waiting.filter(w => now - w.at > waitMs && !w.released) : [];
    const code = overdue.length && refusalCode(input, aliases);
    if (code) {
      const shaped = replyShaped(input, aliases);
      const byConversation = [...new Map(overdue.slice().reverse().map(w => [key(w), w])).values()].sort((a, b) => a.at - b.at);
      for (const w of overdue) {
        if (!shaped) w.refusals++;
        state.log.push({ type: mode === 'enforce' ? 'refusal' : 'would_refuse', at: now, provider: w.provider, conversation_id: w.conversation_id, seconds: Math.floor((now - w.at) / 1000), ...(shaped ? { reply_shaped: true } : {}) });
        if (w.refusals >= LIMITS.refusals) {
          w.released = true;
          state.log.push({ type: 'let_go', at: now, mode, provider: w.provider, conversation_id: w.conversation_id });
        }
      }
      if (mode === 'enforce') output = { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: refusal(byConversation, now, options.ownAlias, code) } };
    }
    if (!output.hookSpecificOutput) state.calls[hash(`${session}:${input.tool_use_id}`)] = { at: now, tool: input.tool_name, waiting: state.waiting.length > 0, arrived: false, startThrough: options.fence ?? state.cursor?.seq ?? 0, seenThrough: state.seenThrough || 0, priorWaiting: caughtUp ? null : [...new Set(state.waiting.map(key))] };
  } else if (event === 'PostToolUse') {
    if (!identifier(input.tool_use_id)) throw new Error('invalid_tool_input');
    const callKey = hash(`${session}:${input.tool_use_id}`);
    const call = state.calls[callKey];
    if (call) {
      const elapsed = Number.isFinite(input.duration_ms) ? input.duration_ms : now - call.at;
      if (elapsed < 0) throw new Error('clock_moved_backwards');
      call.ended = { at: now, through: options.fence || state.cursor?.seq || 0, elapsed, waitMs };
      finishCalls(state);
    }
  } else throw new Error('unknown_hook_event');
  state.receipts[id] = { at: now, output };
  trimMap(state.receipts, now - LIMITS.receiptAge, LIMITS.receipts, v => v.at);
  trimMap(state.calls, now - LIMITS.age, LIMITS.calls, v => v.at);
  trimLog(state, now);
  return output;
}
function finishCalls(state) {
  for (const [id, call] of Object.entries(state.calls)) {
    if (!call.ended || (state.cursor?.seq || 0) < call.ended.through) continue;
    if (call.ended.elapsed > call.ended.waitMs) state.log.push({ type: 'long_call', at: call.ended.at, tool: call.tool, seconds: call.ended.elapsed / 1000, waiting: (call.priorWaiting ? call.priorWaiting.length > 0 : call.waiting) || call.arrived });
    delete state.calls[id];
  }
}

function stateDir(city) { return path.join(city, '.gc', 'reachable'); }
function regular(file, missing = false) {
  try { const s = fs.lstatSync(file); if (!s.isFile() || s.isSymbolicLink()) throw new Error('unsafe_state_file'); }
  catch (e) { if (!(missing && e.code === 'ENOENT')) throw e; }
}
function directory(dir) {
  if (!fs.existsSync(dir)) fs.mkdirSync(dir, { mode: 0o700 });
  const stat = fs.lstatSync(dir);
  if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error('unsafe_state_directory');
}
function prepare(city) {
  directory(path.join(city, '.gc'));
  directory(stateDir(city));
  fs.chmodSync(stateDir(city), 0o700);
}
function atomic(file, bytes, mode = 0o600, lock) {
  regular(file, true);
  const tmp = lock ? path.join(path.dirname(file), '.publish-' + lock.name)
    : `${file}.tmp-${process.pid}-${crypto.randomBytes(6).toString('hex')}`;
  try {
    fs.writeFileSync(tmp, bytes, { mode: 0o600, flag: 'wx' });
    if (mode !== 0o600) fs.chmodSync(tmp, mode);
    lock?.check();
    fs.renameSync(tmp, file);
  } catch (error) {
    // A successor can withdraw this staged file even after our last check.
    if (lock && error.code === 'ENOENT') lock.check();
    throw error;
  } finally { try { fs.unlinkSync(tmp); } catch (e) { if (e.code !== 'ENOENT') throw e; } }
}
function json(file, fallback) {
  regular(file, true);
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); }
  catch (e) { if (e.code === 'ENOENT') return fallback; throw new Error('invalid_state_json'); }
}
function writeJSON(file, value, lock) { atomic(file, JSON.stringify(value, null, 2) + '\n', 0o600, lock); }
function unlinkIfPresent(file) {
  try { fs.unlinkSync(file); } catch (error) { if (error.code !== 'ENOENT') throw error; }
}
function clearRemoval(dir) {
  const stat = fs.lstatSync(dir, { throwIfNoEntry: false });
  if (!stat) return;
  if (!stat.isDirectory()) throw new Error('unsafe_state_directory');
  unlinkIfPresent(path.join(dir, 'entry'));
  try { fs.rmdirSync(dir); } catch (error) { if (error.code !== 'ENOENT') throw error; }
}
function removeOwned(file, lock) {
  lock.check();
  if (!fs.lstatSync(file, { throwIfNoEntry: false })) return;
  // A successor can remove this destination directory before it publishes.
  // A paused rename then cannot take the successor's file.
  const dir = path.join(path.dirname(file), '.remove-' + lock.name);
  directory(dir);
  try {
    lock.check();
    fs.renameSync(file, path.join(dir, 'entry'));
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
    lock.check();
  } finally { clearRemoval(dir); }
}
function readTicket(file) {
  regular(file, true);
  let text;
  try { text = fs.readFileSync(file, 'utf8'); }
  catch (error) { if (error.code === 'ENOENT') return null; throw error; }
  // Older recovery code left a zero-length ticket while withdrawing a writer.
  if (!text) return { revoked: true };
  try { return JSON.parse(text); } catch { throw new Error('invalid_lock'); }
}
function withLock(city, action, options = {}) {
  prepare(city);
  const dir = stateDir(city);
  const waitUntil = (options.start ?? performance.now()) + LIMITS.deadline - 250;
  const name = `lock-${process.pid}-${crypto.randomBytes(12).toString('hex')}.json`;
  const file = path.join(dir, name);
  // Nonblocking bakery tickets: each invocation owns a unique filename.
  // Withdraw a displaced writer's staged rename in both possible destination
  // directories. Names are ticket-specific and cannot belong to a successor.
  const revoke = (n, stat) => {
    const peerFile = path.join(dir, n);
    // Replace the directory entry. Never write through the displaced inode.
    if (stat.isFile()) writeJSON(peerFile, { revoked: true });
    unlinkIfPresent(path.join(dir, '.publish-' + n));
    clearRemoval(path.join(dir, '.remove-' + n));
    const settingsDir = path.join(city, '.claude');
    if (fs.lstatSync(settingsDir, { throwIfNoEntry: false })?.isDirectory()) {
      unlinkIfPresent(path.join(settingsDir, '.publish-' + n));
      clearRemoval(path.join(settingsDir, '.remove-' + n));
    }
    unlinkIfPresent(peerFile);
  };
  const peers = names => names.filter(n => /^lock-\d+-[\w-]+\.json$/.test(n) && n !== name).flatMap(n => {
    const p = path.join(dir, n);
    const stat = fs.lstatSync(p, { throwIfNoEntry: false });
    if (!stat) return [];
    if (Math.abs(Date.now() - stat.mtimeMs) > LIMITS.lockAge) { revoke(n, stat); return []; }
    const owner = readTicket(p);
    if (!owner) return [];
    if (owner.revoked) { revoke(n, stat); return []; }
    if (!Number.isInteger(owner.pid) || owner.pid <= 0 || !Number.isSafeInteger(owner.ticket)) throw new Error('invalid_lock');
    try { process.kill(owner.pid, 0); }
    catch (e) { if (e.code === 'ESRCH') { revoke(n, stat); return []; } }
    return [{ ...owner, name: n }];
  });
  let expectedTicket = 0;
  const lock = { name, check() {
    const owner = readTicket(file);
    if (owner?.pid !== process.pid || owner.ticket !== expectedTicket) throw new Error('state_busy');
    if (lock.generation !== undefined) {
      const current = json(path.join(dir, 'state.json'), null);
      if ((current?.generation ?? 0) !== lock.generation) throw new Error('state_busy');
    }
  } };
  writeJSON(file, { pid: process.pid, ticket: 0 });
  try {
    const names = fs.readdirSync(dir);
    if (options.pruneErrors) pruneErrors(dir, names, options.now);
    const ticket = Math.max(0, ...peers(names).map(p => p.ticket)) + 1;
    writeJSON(file, { pid: process.pid, ticket }, lock);
    expectedTicket = ticket;
    const timer = new Int32Array(new SharedArrayBuffer(4));
    while (peers(fs.readdirSync(dir)).some(p => !p.ticket || p.ticket < ticket || (p.ticket === ticket && p.name < name))) {
      const remaining = waitUntil - performance.now();
      if (remaining <= 0) throw new Error('state_busy');
      Atomics.wait(timer, 0, 0, Math.min(10, remaining));
    }
    return action(lock);
  } finally { unlinkIfPresent(file); }
}
function saveState(city, state, lock) {
  state.generation = lock.generation + 1;
  writeJSON(path.join(stateDir(city), 'state.json'), state, lock);
  lock.generation = state.generation;
  // state.json is the atomic transaction; this human-readable mirror can
  // always be regenerated from it after a crash between the two renames.
  atomic(path.join(stateDir(city), 'log.jsonl'), state.log.map(r => JSON.stringify(r)).join('\n') + (state.log.length ? '\n' : ''), 0o600, lock);
}
function loadState(city, now, lock) {
  const state = json(path.join(stateDir(city), 'state.json'), initialState());
  if (state?.version === 1 && Array.isArray(state.known) && Array.isArray(state.sessions)) {
    for (const field of ['known', 'sessions']) state[field] = Object.fromEntries(state[field].map(k => [k, now]));
    Object.assign(state, { version: 2, skipped: initialState().skipped, history_dropped: 0, hook_ok_at: null });
  }
  if (state?.version !== 2 || !Number.isFinite(state.now) || !['waiting', 'exemptions', 'log'].every(k => Array.isArray(state[k])) || !['known', 'sessions', 'calls', 'receipts', 'skipped'].every(k => state[k] && typeof state[k] === 'object' && !Array.isArray(state[k]))) throw new Error('invalid_state_shape');
  if (!state.waiting.every(w => identifier(w.provider) && identifier(w.conversation_id) && Number.isFinite(w.at) && Number.isSafeInteger(w.seq) && Number.isInteger(w.refusals) && typeof w.released === 'boolean')) throw new Error('invalid_waiting_state');
  if (state.cursor && !['dev', 'ino', 'offset', 'seq'].every(k => Number.isSafeInteger(state.cursor[k]) && state.cursor[k] >= 0)) throw new Error('invalid_cursor');
  if (state.generation !== undefined && (!Number.isSafeInteger(state.generation) || state.generation < 0)) throw new Error('invalid_state_shape');
  if (lock) lock.generation = state.generation ?? 0;
  return state;
}
function readMode(city) {
  const mode = json(path.join(stateDir(city), 'mode.json'), { mode: 'off' }).mode;
  if (!['off', 'observe', 'enforce'].includes(mode)) throw new Error('invalid_mode');
  return mode;
}
function normalized(options) {
  const env = options.env || process.env;
  const city = options.city || env.GC_CITY_PATH || env.GC_CITY;
  if (!city || !path.isAbsolute(city)) throw new Error('absolute_city_required');
  const agent = env.REACHABLE_AGENT || 'mayor';
  return { ...options, env, city, now: options.now ?? Date.now(), liveClock: options.now === undefined, agent, session: env.GC_AGENT === agent ? env.GC_SESSION_ID || '' : '', sessionName: env.GC_AGENT === agent ? env.GC_SESSION_NAME || '' : '', start: options.start ?? performance.now() };
}
function syncEvents(state, options) {
  const result = readEvents(options.city, state.cursor, options);
  if (options.liveClock) options.now = Date.now();
  reduceEvents(state, result.rows, options);
  state.cursor = result.cursor;
  finishCalls(state);
  trimLog(state, options.now);
  return result.caughtUp;
}
const errorCodes = new Set(['measurement_timeout', 'clock_moved_backwards', 'state_busy', 'invalid_state_json', 'invalid_event_json', 'invalid_event_row', 'event_sequence_gap', 'event_in_future', 'invalid_mode', 'mayor_session_unknown', 'incomplete_archive', 'measurement_failed']);
function errorFiles(names) {
  return names.flatMap(name => {
    const match = /^error-(\d+)-[\da-f]+\.jsonl$/.exec(name);
    return match ? [{ name, at: Number(match[1]) }] : [];
  }).sort((a, b) => b.at - a.at || b.name.localeCompare(a.name));
}
function pruneErrors(dir, names, now, limit = LIMITS.errors) {
  const files = errorFiles(names);
  for (const [i, file] of files.entries()) {
    if (i >= limit || now - file.at > LIMITS.history) unlinkIfPresent(path.join(dir, file.name));
  }
}
function errorSummary(city, now) {
  const dir = stateDir(city);
  const summary = {};
  let names;
  try { names = fs.readdirSync(dir); } catch (e) { if (e.code === 'ENOENT') return summary; throw e; }
  for (const { name, at } of errorFiles(names).slice(0, LIMITS.errors)) {
    if (now - at > LIMITS.history) continue;
    let code;
    try {
      const record = json(path.join(dir, name), null);
      if (!record) continue;
      code = errorCodes.has(record.code) ? record.code : 'measurement_failed';
    } catch { code = 'unreadable_error_record'; }
    const group = summary[code] ||= { count: 0, newest: at };
    group.count++;
  }
  return summary;
}
function errorLog(city, now, error) {
  // Error codes only: filesystem errors can contain paths, and parser errors
  // can contain message text. Never write their raw messages.
  const code = errorCodes.has(error.message) ? error.message : 'measurement_failed';
  try {
    prepare(city);
    // Unique atomic records also work when the error is lock contention.
    const dir = stateDir(city);
    pruneErrors(dir, fs.readdirSync(dir), now, LIMITS.errors - 1);
    const file = path.join(dir, `error-${now}-${crypto.randomBytes(12).toString('hex')}.jsonl`);
    atomic(file, JSON.stringify({ type: 'error', at: now, code }) + '\n');
    pruneErrors(dir, fs.readdirSync(dir), now);
  } catch { /* A broken state directory must never prevent a tool call. */ }
}
function runHook(input, options = {}) {
  const env = options.env || process.env;
  // This check precedes *every* filesystem operation, including mode reads.
  if (env.GC_AGENT !== (env.REACHABLE_AGENT || 'mayor') || input?.agent_id) return {};
  let o;
  try {
    o = normalized(options);
    if (readMode(o.city) === 'off') return {};
    return withLock(o.city, lock => {
      const mode = readMode(o.city);
      if (mode === 'off') return {};
      // Compare transaction times in lock order, including duplicate hook
      // processes that reached the queue in the opposite order to startup.
      if (options.now === undefined) o.now = Date.now();
      const state = loadState(o.city, o.now, lock);
      const turnId = input.hook_event_name === 'Stop' ? stopIdentity(input) : undefined;
      let fence = ['Stop', 'PostToolUse'].includes(input.hook_event_name) ? Math.max(state.cursor?.seq || 0, eventFence(o.city)) : undefined;
      const caughtUp = syncEvents(state, o);
      if (!caughtUp && input.hook_event_name === 'PreToolUse') fence = eventFence(o.city);
      const manifest = json(path.join(stateDir(o.city), 'install.json'), { aliases: [] });
      const beforeDecision = structuredClone(state);
      const result = decide(state, input, { ...o, mode, aliases: manifest.aliases, ownAlias: manifest.own_alias, caughtUp, fence, turnId });
      checkDeadline(o.start);
      const loadedGeneration = lock.generation;
      try { state.hook_ok_at = o.now; saveState(o.city, state, lock); checkDeadline(o.start); }
      catch (error) {
        const committed = lock.generation !== loadedGeneration;
        if (error.message === 'state_busy') {
          if (committed) return result;
          throw error;
        }
        // A save can itself cross the deadline. Persist the actual fail-open
        // outcome before releasing the lock, including duplicate accounting.
        if (result.hookSpecificOutput) decide(beforeDecision, input, { ...o, mode: 'observe', aliases: manifest.aliases, caughtUp: false, fence });
        const rollback = result.hookSpecificOutput ? beforeDecision : state;
        rollback.hook_ok_at = beforeDecision.hook_ok_at;
        const beforeRollback = lock.generation;
        try { saveState(o.city, rollback, lock); }
        catch (retryError) {
          if (retryError.message === 'state_busy' && committed && lock.generation === beforeRollback) return result;
          throw retryError;
        }
        throw error;
      }
      return result;
    }, { ...o, pruneErrors: true });
  } catch (error) { if (o) errorLog(o.city, o.now, error); return {}; }
}
function localGC(o, args) {
  // Both commands used here inspect local configuration; neither calls a
  // city's API. They run only from explicit commands, never the hot hook.
  return execFileSync('gc', ['--city', o.city, ...args], { cwd: o.city, env: o.env, encoding: 'utf8', timeout: 1500, maxBuffer: 8 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'] });
}
function remoteParts(source) {
  const clean = source.trim().replace(/#[^#]*$/, '');
  const github = /^(?:(https?:\/\/))?github\.com\/([^/]+)\/([^/]+)\/(tree|blob)\/[^/]+(?:\/(.*))?$/.exec(clean);
  if (github) return { clone: `${github[1] || ''}github.com/${github[2]}/${github[3]}.git`, sub: github[4] === 'blob' ? (github[5] || '').replace(/(?:^|\/)pack\.toml$/, '') : github[5] || '' };
  const start = clean.includes('://') ? clean.indexOf('://') + 3 : 0;
  const split = clean.indexOf('//', start);
  let clone = (split < 0 ? clean : clean.slice(0, split)).replace(/\/+$/, '');
  if (clone.startsWith('github.com/')) clone = 'https://' + clone;
  return { clone, sub: split < 0 ? '' : clean.slice(split + 2).replace(/^\/+|\/+$/g, '') };
}
function lockedCommits(city) {
  const file = path.join(city, 'packs.lock');
  if (!fs.existsSync(file)) return {};
  const commits = {}; let source = '';
  // packs.lock is gc-generated TOML, with Go-quoted source keys and commits.
  for (const line of fs.readFileSync(file, 'utf8').split('\n')) {
    const section = /^\s*\[packs\.("(?:[^"\\]|\\.)*")\]\s*$/.exec(line);
    if (section) { source = JSON.parse(section[1]); continue; }
    if (/^\s*\[/.test(line)) source = '';
    const commit = /^\s*commit\s*=\s*"([a-zA-Z0-9_-]+)"\s*$/.exec(line);
    if (source && commit) commits[source] = commit[1];
  }
  return commits;
}
function resolveImports(text, city, env) {
  const aliases = [];
  let ownAlias;
  const commits = lockedCommits(city);
  for (const line of text.trim().split('\n')) {
    const [alias, source] = line.split('\t');
    if (!alias || alias === '(transitive)' || !source) continue;
    let dir = source.startsWith('//') ? path.join(city, source.slice(2)) : path.resolve(city, source);
    if (/^(git@|ssh:\/\/|https?:\/\/|file:\/\/|github\.com\/)/.test(source)) {
      const commit = commits[source];
      // Bundled non-channel imports may have no lock or use a synthetic
      // cache. They grant no exemption; inspect only installed manifests.
      if (!commit) continue;
      const { clone, sub } = remoteParts(source);
      const gcHome = env.GC_HOME || (env.HOME && path.join(env.HOME, '.gc'));
      if (!gcHome) throw new Error('import_resolution_unavailable');
      dir = path.join(gcHome, 'cache/repos', hash(clone + commit), sub);
      if (!fs.existsSync(path.join(dir, 'pack.toml'))) continue;
    }
    const manifest = fs.readFileSync(path.join(dir, 'pack.toml'), 'utf8');
    const pack = /^\s*\[pack\]\s*(?:#[^\n]*)?\n([\s\S]*?)(?=^\s*\[|$(?![\s\S]))/m.exec(manifest)?.[1].match(/^\s*name\s*=\s*['"]([^'"]+)['"]\s*(?:#.*)?$/m)?.[1];
    if (POLICY.packs.includes(pack)) aliases.push(alias);
    if (pack === 'reachable' && !ownAlias) ownAlias = alias;
  }
  return { aliases: [...new Set(aliases)], own_alias: ownAlias || 'reachable' };
}
function aliasesFromImports(text, city, env = process.env) {
  return resolveImports(text, city, env).aliases;
}
const shellQuote = s => "'" + s.replace(/'/g, "'\\''") + "'";
const EVENTS = ['PreToolUse', 'PostToolUse', 'Stop'];
function settings(city) {
  const dir = path.join(city, '.claude');
  if (fs.existsSync(dir) && fs.lstatSync(dir).isSymbolicLink()) throw new Error('unsafe_settings_directory');
  const file = path.join(dir, 'settings.json');
  const value = json(file, {});
  if (!value || typeof value !== 'object' || Array.isArray(value) || (value.hooks && (typeof value.hooks !== 'object' || Array.isArray(value.hooks)))) throw new Error('invalid_settings');
  for (const e of EVENTS) if (value.hooks?.[e] && !Array.isArray(value.hooks[e])) throw new Error('invalid_settings_hooks');
  return { file, value };
}
function backupSettings(o, file) {
  const dir = stateDir(o.city);
  const backup = `settings-backup-${new Date(o.now).toISOString().replace(/[:.]/g, '-')}-${crypto.randomBytes(4).toString('hex')}.json`;
  if (fs.existsSync(file)) {
    atomic(path.join(dir, backup), fs.readFileSync(file), 0o600, o.lock);
    const older = fs.readdirSync(dir).filter(n => /^settings-backup-.*\.json$/.test(n) && n !== backup)
      .map(name => ({ name, time: fs.statSync(path.join(dir, name), { bigint: true }).mtimeNs }))
      .sort((a, b) => a.time === b.time ? b.name.localeCompare(a.name) : a.time < b.time ? 1 : -1);
    for (const entry of older.slice(LIMITS.backups - 1)) {
      removeOwned(path.join(dir, entry.name), o.lock);
    }
  }
}
function saveSettings(o, file, before, after, mode = 0o600) {
  if (isDeepStrictEqual(before, after)) return;
  backupSettings(o, file);
  directory(path.dirname(file));
  atomic(file, JSON.stringify(after, null, 2) + '\n', mode, o.lock);
  if (JSON.stringify(json(file)) !== JSON.stringify(after)) throw new Error('settings_readback_failed');
}
function ownedRegistration(manifest, event, entry) {
  return [manifest?.entries[event], manifest?.pending?.previous[event]].find(owned =>
    owned && entry.matcher === owned.matcher && entry.hooks?.some(h => h.command === owned.command));
}
function install(options = {}) {
  const o = normalized(options);
  return withLock(o.city, lock => {
    o.lock = lock;
    const { file, value } = settings(o.city);
    const original = structuredClone(value);
    const manifestFile = path.join(stateDir(o.city), 'install.json');
    const old = json(manifestFile, null);
    const source = fs.realpathSync(o.source || __filename);
    const copy = path.join(fs.realpathSync(stateDir(o.city)), 'hook.cjs');
    const code = fs.readFileSync(source);
    const command = `${shellQuote(process.execPath)} ${shellQuote(copy)} --hook --city ${shellQuote(fs.realpathSync(o.city))}`;
    const imports = resolveImports(o.imports ?? localGC(o, ['import', 'list']), o.city, o.env);
    let snapshot = old?.settings_before;
    if (!old) {
      const stat = fs.lstatSync(file, { throwIfNoEntry: false });
      const bytes = stat ? fs.readFileSync(file) : null;
      snapshot = { existed: Boolean(stat), dir_existed: fs.existsSync(path.dirname(file)), mode: stat ? stat.mode & 0o7777 : null, sha256: bytes ? hash(bytes) : null };
      if (bytes) atomic(path.join(stateDir(o.city), 'settings-before-install'), bytes, 0o600, lock);
    }
    const entries = {};
    const previous = {};
    const existed = old?.existed || Object.fromEntries(EVENTS.map(e => [e, Object.hasOwn(value.hooks || {}, e)]));
    value.hooks ||= {};
    for (const event of EVENTS) {
      value.hooks[event] ||= [];
      const existing = value.hooks[event].find(entry => ownedRegistration(old, event, entry));
      let matcher = existing?.matcher || '^(?:.*)$';
      if (!existing) {
        while (value.hooks[event].some(e => e.matcher === matcher)) matcher = `(?:${matcher})`;
        value.hooks[event].push({ matcher, hooks: [{ type: 'command', command, timeout: 5 }] });
      } else {
        const owned = ownedRegistration(old, event, existing);
        previous[event] = owned;
        existing.hooks = existing.hooks.map(h => h.command === owned.command ? { ...h, command, timeout: 5 } : h);
      }
      entries[event] = { matcher, command };
    }
    const manifest = { source, sha: hash(code), ...imports, entries, existed, hooksExisted: old?.hooksExisted ?? Object.hasOwn(original, 'hooks'), ...(snapshot ? { settings_before: snapshot } : {}) };
    const firstInstall = !old || old.pending?.first_install === true;
    const changing = !isDeepStrictEqual(old, manifest) || !isDeepStrictEqual(original, value);
    if (changing) writeJSON(manifestFile, { ...manifest, pending: { previous, first_install: firstInstall } }, lock);
    if (!fs.existsSync(copy) || !fs.readFileSync(copy).equals(code)) atomic(copy, code, 0o600, lock);
    saveSettings(o, file, original, value);
    if (firstInstall || !fs.existsSync(path.join(stateDir(o.city), 'mode.json'))) writeJSON(path.join(stateDir(o.city), 'mode.json'), { mode: 'observe' }, lock);
    if (changing) writeJSON(manifestFile, manifest, lock);
    return { installed: true, mode: readMode(o.city) };
  });
}
function uninstall(options = {}) {
  const o = normalized(options);
  return withLock(o.city, lock => {
    o.lock = lock;
    const manifestFile = path.join(stateDir(o.city), 'install.json');
    const old = json(manifestFile, null);
    if (!old) return { installed: false };
    const { file, value } = settings(o.city);
    const before = structuredClone(value);
    for (const event of EVENTS) {
      if (!value.hooks?.[event]) continue;
      value.hooks[event] = value.hooks[event].flatMap(entry => {
        const owned = ownedRegistration(old, event, entry);
        if (!owned) return [entry];
        const hooks = entry.hooks.filter(h => h.command !== owned.command);
        return hooks.length ? [{ ...entry, hooks }] : [];
      });
      if (!value.hooks[event].length && !old.existed[event]) delete value.hooks[event];
    }
    if (value.hooks && !Object.keys(value.hooks).length && !old.hooksExisted) delete value.hooks;
    const snapshot = old.settings_before;
    const beforeFile = path.join(stateDir(o.city), 'settings-before-install');
    let restored = false;
    if (snapshot) {
      let bytes = null;
      if (snapshot.existed) {
        try {
          regular(beforeFile);
          const saved = fs.readFileSync(beforeFile);
          if (hash(saved) === snapshot.sha256) bytes = saved;
        } catch (error) { if (error.code !== 'ENOENT') throw error; }
      }
      restored = (!snapshot.existed || bytes !== null) && isDeepStrictEqual(value, bytes ? JSON.parse(bytes) : {});
      if (restored) {
        backupSettings(o, file);
        if (bytes) {
          directory(path.dirname(file));
          atomic(file, bytes, snapshot.mode, lock);
          if (!fs.readFileSync(file).equals(bytes)) throw new Error('settings_readback_failed');
        }
        else {
          removeOwned(file, lock);
          const dir = path.dirname(file);
          if (!snapshot.dir_existed && fs.existsSync(dir) && !fs.readdirSync(dir).length) fs.rmdirSync(dir);
        }
      }
    }
    if (!restored) saveSettings(o, file, before, value, fs.existsSync(file) ? fs.statSync(file).mode & 0o7777 : 0o600);
    writeJSON(path.join(stateDir(o.city), 'mode.json'), { mode: 'off' }, lock);
    removeOwned(path.join(stateDir(o.city), 'hook.cjs'), lock);
    removeOwned(beforeFile, lock);
    removeOwned(manifestFile, lock);
    return { installed: false, ...(snapshot ? { layout_restored: restored } : {}) };
  });
}
function registration(o) {
  let current = false;
  try {
    const manifest = json(path.join(stateDir(o.city), 'install.json'), null);
    if (!manifest) return { registered: false, current: false };
    try { current = fs.readFileSync(o.source || __filename).equals(fs.readFileSync(path.join(stateDir(o.city), 'hook.cjs'))); }
    catch { /* A missing copy can coexist with a registered command. */ }
    const { value } = settings(o.city);
    const registered = EVENTS.every(event => value.hooks?.[event]?.some(e => e.matcher === manifest.entries[event].matcher && e.hooks?.some(h => h.type === 'command' && h.command === manifest.entries[event].command)));
    return { registered, current };
  } catch { return { registered: false, current }; }
}
function doctor(options = {}) {
  const o = normalized(options);
  const errors = [];
  let mode;
  try { mode = readMode(o.city); } catch { errors.push('mode_unreadable'); }
  const r = registration(o);
  if (mode === 'enforce' && !r.registered) errors.push('enforce_without_hook');
  if (r.registered && !r.current) errors.push('hook_copy_outdated');
  try { const fd = fs.openSync(path.join(o.city, '.gc/events.jsonl'), 'r'); fs.closeSync(fd); } catch { errors.push('event_log_unreadable'); }
  try {
    const prompt = o.prompt ?? localGC(o, ['prime', o.agent, '--strict']);
    const source = o.source || __filename;
    const fragment = fs.readFileSync(path.join(path.dirname(source), 'template-fragments/reachable.template.md'), 'utf8').split('\n').slice(1, -2).join('\n');
    if (!fragment || !prompt.includes(fragment)) errors.push('prompt_fragment_missing');
  } catch { errors.push('prompt_unreadable'); }
  const summary = options.errorSummary ?? errorSummary(o.city, o.now);
  let state;
  try { state = loadState(o.city, o.now); } catch { /* The error trail survives a damaged state. */ }
  const latest = Object.entries(summary).sort((a, b) => b[1].newest - a[1].newest)[0];
  if (state && state.now > o.now && summary.clock_moved_backwards) errors.push('hook_failing_open clock_moved_backwards');
  else if (latest && (state?.hook_ok_at == null || latest[1].newest > state.hook_ok_at)) errors.push('hook_failing_open ' + latest[0]);
  if (state?.skipped.count && o.now - state.skipped.last_at <= LIMITS.history) errors.push(`rows_skipped ${state.skipped.count} last_seq ${state.skipped.last_seq}`);
  return errors;
}
function command(verb, args, options = {}) {
  if (verb === 'install' || verb === 'uninstall') {
    if (args.length) throw new Error('unexpected_arguments');
    return verb === 'install' ? install(options) : uninstall(options);
  }
  if (verb === 'doctor') return doctor(options);
  const o = normalized(options);
  const errors = verb === 'status' ? options.errorSummary ?? errorSummary(o.city, o.now) : undefined;
  return withLock(o.city, lock => {
    if (options.now === undefined) o.now = Date.now();
    const dir = stateDir(o.city);
    if (verb === 'mode') {
      if (args.length !== 1 || !['off', 'observe', 'enforce'].includes(args[0])) throw new Error('usage: mode off|observe|enforce');
      writeJSON(path.join(dir, 'mode.json'), { mode: args[0] }, lock);
      return { mode: args[0] };
    }
    const registered = registration(o);
    const outdated = !registered.current && fs.existsSync(path.join(dir, 'install.json'));
    const listing = verb === 'exempt' && args.length === 1 && args[0] === '--list';
    if (outdated && (verb === 'pass' || (verb === 'exempt' && !listing))) throw new Error('hook_copy_outdated_run_install');
    const state = loadState(o.city, o.now, lock);
    const caughtUp = syncEvents(state, o);
    let result;
    if (verb === 'status') {
      if (args.length && (args.length !== 1 || args[0] !== '--json')) throw new Error('usage: status [--json]');
      const waiting = [...new Map(state.waiting.slice().reverse().map(w => [key(w), w])).values()].sort((a, b) => a.at - b.at).map(w => ({ provider: w.provider, conversation_id: w.conversation_id, seconds: Math.floor((o.now - w.at) / 1000) }));
      result = { mode: readMode(o.city), ...registered, caught_up: caughtUp, waiting, history: state.log,
        skipped_rows: state.skipped, errors, hook_ok_at: state.hook_ok_at, history_dropped: state.history_dropped };
    } else if (listing) result = state.exemptions;
    else if (verb === 'pass' || verb === 'exempt') {
      if (verb === 'pass' ? args.length !== 1 : args.length !== 2 || !['--on', '--off'].includes(args[1])) throw new Error(`usage: ${verb} PROVIDER/CONVERSATION${verb === 'exempt' ? ' --on|--off' : ''}`);
      if (!caughtUp) throw new Error('event_catchup_incomplete');
      const removingListed = verb === 'exempt' && args[1] === '--off' && state.exemptions.includes(args[0]);
      if (!removingListed && !Object.hasOwn(state.known, args[0]) && !state.waiting.some(w => key(w) === args[0])) throw new Error('unknown_conversation');
      if (verb === 'pass' || args[1] === '--on') state.waiting = state.waiting.filter(w => key(w) !== args[0]);
      if (verb === 'exempt') state.exemptions = args[1] === '--on' ? [...new Set([...state.exemptions, args[0]])] : state.exemptions.filter(c => c !== args[0]);
      result = { conversation: args[0], [verb === 'pass' ? 'seen' : 'exempt']: verb === 'pass' || args[1] === '--on' };
    } else throw new Error('unknown_verb');
    if (!outdated) saveState(o.city, state, lock);
    return result;
  });
}

module.exports = { LIMITS, POLICY, hash, key, initialState, readEvents, reduceEvents, literalArgv, allowed, refusal, decide, runHook, command, install, uninstall, doctor, aliasesFromImports };

function errorDetail(summary) {
  return 'error records (14 days): ' + (Object.entries(summary).map(([code, row]) => code + ' ' + row.count).join(', ') || 'none');
}
if (require.main === module) {
  const start = performance.now();
  const args = process.argv.slice(2);
  const hook = args[0] === '--hook';
  let summary;
  if (hook && process.env.GC_AGENT !== (process.env.REACHABLE_AGENT || 'mayor')) process.exit(0);
  try {
    if (Number(process.versions.node.split('.')[0]) < 20) throw new Error('Node.js 20 or newer is required');
    if (hook) {
      const input = JSON.parse(fs.readFileSync(0, 'utf8'));
      const city = args[1] === '--city' ? args[2] : undefined;
      const result = runHook(input, { city, start });
      if (Object.keys(result).length) process.stdout.write(JSON.stringify(result) + '\n');
    } else {
      const verb = args.shift();
      if (verb === 'status' || verb === 'doctor') {
        const o = normalized({});
        summary = errorSummary(o.city, o.now);
      }
      const result = command(verb, args, { errorSummary: summary });
      if (verb === 'doctor') {
        process.stdout.write(result.length ? result.join('\n') + '\n' : 'reachable: healthy\n');
        process.stdout.write(errorDetail(summary) + '\n');
        process.exitCode = result.length ? 1 : 0;
      } else if (verb === 'uninstall' && result.layout_restored === false) {
        process.stdout.write('Settings changed after install. The layout was not restored.\n');
      } else if (verb === 'status' && !args.includes('--json')) {
        process.stdout.write(`mode: ${result.mode}\nhook registered: ${result.registered}\nhook current: ${result.current}\ncaught up: ${result.caught_up}\n`);
        for (const field of ['skipped_rows', 'errors', 'hook_ok_at', 'history_dropped']) process.stdout.write(field + ': ' + JSON.stringify(result[field]) + '\n');
        for (const row of result.waiting) process.stdout.write(`${key(row)}: ${row.seconds} seconds\n`);
        for (const row of result.history) process.stdout.write(JSON.stringify(row) + '\n');
      } else process.stdout.write(JSON.stringify(result, null, 2) + '\n');
    }
  } catch (error) {
    if (hook) {
      const city = (args[1] === '--city' ? args[2] : '') || process.env.GC_CITY_PATH || process.env.GC_CITY;
      if (city && path.isAbsolute(city)) errorLog(city, Date.now(), error);
    } else {
      if (summary !== undefined) process.stderr.write(errorDetail(summary) + '\n');
      process.stderr.write(`reachable: ${error.message}\n`); process.exitCode = 1;
    }
  }
}
