#!/usr/bin/env node
'use strict';

// No dependencies, network clients, message bodies, or actor names.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const { performance } = require('node:perf_hooks');
const { execFileSync } = require('node:child_process');

const DAY = 86400000;
const LIMITS = Object.freeze({ wait: 30000, refusals: 3, age: DAY, history: 14 * DAY, deadline: 2000 });
const CHUNK = 8 * 1024 * 1024;
const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const key = row => `${row.provider}/${row.conversation_id}`;
function initialState() {
  return { version: 1, cursor: null, waiting: [], exemptions: [], known: [], sessions: [], calls: {}, receipts: {}, log: [], now: 0, turn: '' };
}
function checkDeadline(start) {
  if (performance.now() - start >= LIMITS.deadline) throw new Error('measurement_timeout');
}
function identifier(s) { return typeof s === 'string' && s.length > 0 && s.length <= 4096 && !/[\x00-\x20\x7f]/.test(s); }
function parseRows(bytes, minimum, start) {
  const rows = [];
  let seq = minimum;
  for (const line of bytes.toString('utf8').split('\n')) {
    if (!line) continue;
    checkDeadline(start);
    let row;
    try { row = JSON.parse(line); } catch { throw new Error('invalid_event_json'); }
    if (!row || !Number.isSafeInteger(row.seq) || row.seq <= 0 || typeof row.type !== 'string' || !Number.isFinite(Date.parse(row.ts))) throw new Error('invalid_event_row');
    if (row.seq <= minimum) continue;
    if (seq && row.seq !== seq + 1) throw new Error('event_sequence_gap');
    seq = row.seq;
    rows.push(row);
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
  input.on('error', () => process.exit(1));
  stream.on('error', () => process.exit(1));
  stream.on('data', chunk => {
    const n = Math.min(skip, chunk.length); skip -= n; chunk = chunk.subarray(n);
    const part = chunk.subarray(0, left); left -= part.length;
    for (let written = 0; written < part.length;) written += fs.writeSync(1, part.subarray(written));
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
        const bytes = Buffer.alloc(Math.min(size, CHUNK));
        fs.readSync(fd, bytes, 0, bytes.length, size - bytes.length);
        const end = bytes.lastIndexOf(10);
        if (end < 0) continue;
        const begin = bytes.lastIndexOf(10, end - 1) + 1;
        const row = JSON.parse(bytes.subarray(begin, end).toString());
        if (!Number.isSafeInteger(row.seq)) throw new Error('invalid_event_row');
        seq = Math.max(seq, row.seq);
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
    let seq = cursor?.seq || 0;
    let same = cursor && !cursor.archive && cursor.dev === stat.dev && cursor.ino === stat.ino && cursor.offset <= stat.size;
    if (same && cursor.offset) {
      const boundary = Buffer.alloc(Math.min(128, cursor.offset));
      fs.readSync(fd, boundary, 0, boundary.length, cursor.offset - boundary.length);
      same = hash(boundary) === cursor.boundary;
    }
    if (same) offset = cursor.offset;
    else {
      const names = fs.readdirSync(dir);
      // GC publishes the new active log before the compressed archive. Even
      // a fresh cursor must wait for those captured sequences to be visible.
      const pending = names.some(name => {
        const m = /^events\.jsonl\.rotating-(\d{8}T\d{6}Z)-seq-(\d+)-(\d+)$/.exec(name);
        return m && Number(m[3]) > seq && !names.includes(`events.jsonl.archive-${m[1]}-seq-${m[2]}-${m[3]}.gz`);
      });
      if (pending) return { rows: [], caughtUp: false, cursor: cursor || { dev: 0, ino: 0, offset: 0, seq: 0, boundary: '' } };
      const archives = names.flatMap(name => {
        const m = /^events\.jsonl\.archive-(\d{8}T\d{6}Z)-seq-(\d+)-(\d+)\.gz$/.exec(name);
        if (!m) return [];
        // Message expiry belongs in reduction. Older routing and wake rows
        // still identify the authors of replies delivered within 24 hours.
        return Number(m[3]) > seq ? [{ name, first: Number(m[2]) }] : [];
      }).sort((a, b) => a.first - b.first);
      for (const [index, archive] of archives.entries()) {
        checkDeadline(start);
        const decodedOffset = cursor?.archive?.name === archive.name ? cursor.archive.offset : 0;
        const contents = archiveSlice(path.join(dir, archive.name), decodedOffset, start);
        const end = contents.lastIndexOf(10) + 1;
        if (contents.length < CHUNK && end !== contents.length) throw new Error('incomplete_archive');
        if (!end && contents.length === CHUNK) throw new Error('event_row_too_large');
        const recovered = parseRows(contents.subarray(0, end), seq, start);
        for (const row of recovered) rows.push(row);
        if (recovered.length) seq = recovered.at(-1).seq;
        if (contents.length === CHUNK) return { rows, caughtUp: false, cursor: { dev: cursor?.dev || 0, ino: cursor?.ino || 0, offset: cursor?.offset || 0, boundary: cursor?.boundary || '', seq, archive: { name: archive.name, offset: decodedOffset + end } } };
        // Persist progress between small archives too; their aggregate can
        // exceed the deadline even when each one fits in a single slice.
        if (index + 1 < archives.length) return { rows, caughtUp: false, cursor: { dev: cursor?.dev || 0, ino: cursor?.ino || 0, offset: cursor?.offset || 0, boundary: cursor?.boundary || '', seq } };
      }
    }
    checkDeadline(start);
    // Initial catch-up is bounded and continues on later calls. No refusal is
    // possible until caughtUp, because a later reply may still be unread.
    const bytes = Buffer.alloc(Math.min(stat.size - offset, CHUNK));
    const count = fs.readSync(fd, bytes, 0, bytes.length, offset);
    const end = bytes.subarray(0, count).lastIndexOf(10) + 1;
    if (!end && count === bytes.length && count === 8 * 1024 * 1024) throw new Error('event_row_too_large');
    const appended = parseRows(bytes.subarray(0, end), seq, start);
    for (const row of appended) rows.push(row);
    if (appended.length) seq = appended.at(-1).seq;
    // A newly created empty active file can precede archive publication.
    // Keep the old identity until an active row proves sequence continuity.
    if (!same && cursor && !appended.length) return { rows, caughtUp: false, cursor: { ...cursor, seq, archive: undefined } };
    offset += end;
    const boundary = Buffer.alloc(Math.min(128, offset));
    fs.readSync(fd, boundary, 0, boundary.length, offset - boundary.length);
    checkDeadline(start);
    return { rows, caughtUp: offset === stat.size, cursor: { dev: stat.dev, ino: stat.ino, offset, seq, boundary: hash(boundary) } };
  } finally { fs.closeSync(fd); }
}

function reduceEvents(state, rows, { now, agent, session, sessionName }) {
  if (state.now > now) throw new Error('clock_moved_backwards');
  const own = new Set(state.sessions);
  if (session) own.add(hash(session));
  if (sessionName) own.add(hash(sessionName));
  // Learn named-session replacements before examining delivered words.
  for (const row of rows) {
    const p = row.payload;
    if (row.type === 'session.woke' && (row.subject === agent || row.subject === sessionName) && identifier(row.session_id)) {
      own.add(hash(row.session_id));
      if (identifier(row.subject)) own.add(hash(row.subject));
    }
    if (row.type === 'extmsg.inbound' && p?.target_agent === agent && identifier(p.target_session)) own.add(hash(p.target_session));
  }
  for (const row of rows) {
    if (!['extmsg.inbound', 'extmsg.outbound'].includes(row.type)) continue;
    const p = row.payload;
    if (!p || !identifier(p.provider) || !identifier(p.conversation_id)) throw new Error('invalid_message_event');
    const at = Date.parse(row.ts);
    if (at > now) throw new Error('event_in_future');
    if (row.type === 'extmsg.inbound') {
      if (typeof p.target_agent !== 'string' && p.target_agent !== undefined) throw new Error('invalid_target_agent');
      if (!p.target_agent && identifier(p.target_session) && !own.size && now - at < LIMITS.age) throw new Error('mayor_session_unknown');
      const ours = p.target_agent ? p.target_agent === agent : identifier(p.target_session) && own.has(hash(p.target_session));
      if (!ours) continue;
      const conv = key(p);
      if (!state.known.includes(conv)) state.known.push(conv);
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
      const publishingSession = identifier(row.subject) ? row.subject : p.session;
      if (!identifier(publishingSession)) throw new Error('invalid_outbound_session');
      if (own.has(hash(publishingSession))) {
        state.waiting = state.waiting.filter(w => key(w) !== key(p) || w.at > at);
        for (const call of Object.values(state.calls)) if (call.priorWaiting && row.seq <= call.startThrough) call.priorWaiting = call.priorWaiting.filter(c => c !== key(p));
      }
    }
  }
  state.sessions = [...own];
  state.waiting = state.waiting.filter(w => now - w.at < LIMITS.age && !state.exemptions.includes(key(w)));
  state.waiting.sort((a, b) => a.at - b.at || a.seq - b.seq);
  state.log = state.log.filter(e => now - e.at <= LIMITS.history);
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
function literalArgv(command) {
  if (typeof command !== 'string' || /[\r\n\x00]/.test(command)) return null;
  const args = [];
  let word = '', quote = '', started = false;
  for (let i = 0; i < command.length; i++) {
    const c = command[i];
    if (quote === "'") { if (c === "'") quote = ''; else word += c; continue; }
    if (c === '$' || c === '`') return null;
    if (c === '\\') {
      if (++i === command.length) return null;
      word += command[i]; started = true; continue;
    }
    if (quote === '"') { if (c === '"') quote = ''; else word += c; continue; }
    if (c === "'" || c === '"') { quote = c; started = true; continue; }
    if (/[;&|<>()#*?{}\[\]~]/.test(c)) return null;
    if (/\s/.test(c)) { if (started) args.push(word); word = ''; started = false; }
    else { word += c; started = true; }
  }
  if (quote) return null;
  if (started) args.push(word);
  return args;
}
function allowed(input, aliases) {
  if (POLICY.readOnly.includes(input.tool_name)) return true;
  if (input.tool_name !== 'Bash') return false;
  const args = literalArgv(input.tool_input?.command);
  return Boolean(args && args[0] === 'gc' && (POLICY.control.includes(args[1]) || (aliases.includes(args[1]) && args.length >= 3 && !args[2].startsWith('-'))));
}
function refusal(waiting, now) {
  return waiting.map(w => `A person has waited ${Math.floor((now - w.at) / 1000)} seconds for an answer in ${key(w)}. Answer there now in one line: say what you are doing and when they will hear more. Then go on. If no words are needed, run: gc reachable pass ${key(w)}. A job that will run past about 30 seconds does not belong in this session: start it as a background subagent, or hand it to a worker as a bead.`).join('\n');
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
  if (state.receipts[id]) return state.receipts[id].output;
  let output = {};
  if (event === 'Stop') {
    state.seenThrough = Math.max(state.seenThrough || 0, options.fence ?? state.cursor?.seq ?? Math.max(0, ...state.waiting.map(w => w.seq)));
    state.waiting = state.waiting.filter(w => w.seq > state.seenThrough);
  }
  else if (event === 'PreToolUse') {
    if (!identifier(input.tool_use_id) || !identifier(input.tool_name)) throw new Error('invalid_tool_input');
    state.turn = hash(input.tool_use_id);
    const overdue = caughtUp ? state.waiting.filter(w => now - w.at > waitMs && !w.released) : [];
    if (overdue.length && !allowed(input, aliases)) {
      const byConversation = [...new Map(overdue.slice().reverse().map(w => [key(w), w])).values()].sort((a, b) => a.at - b.at);
      for (const w of overdue) {
        w.refusals++;
        state.log.push({ type: mode === 'enforce' ? 'refusal' : 'would_refuse', at: now, provider: w.provider, conversation_id: w.conversation_id, seconds: Math.floor((now - w.at) / 1000) });
        if (w.refusals >= LIMITS.refusals) {
          w.released = true;
          state.log.push({ type: 'let_go', at: now, mode, provider: w.provider, conversation_id: w.conversation_id });
        }
      }
      if (mode === 'enforce') output = { hookSpecificOutput: { hookEventName: 'PreToolUse', permissionDecision: 'deny', permissionDecisionReason: refusal(byConversation, now) } };
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
  for (const [k, v] of Object.entries(state.receipts)) if (now - v.at > LIMITS.age) delete state.receipts[k];
  for (const [k, v] of Object.entries(state.calls)) if (now - v.at > LIMITS.age) delete state.calls[k];
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
function atomic(file, bytes) {
  regular(file, true);
  const tmp = `${file}.tmp-${process.pid}-${crypto.randomBytes(6).toString('hex')}`;
  try {
    fs.writeFileSync(tmp, bytes, { mode: 0o600, flag: 'wx' });
    fs.renameSync(tmp, file);
  } finally { try { fs.unlinkSync(tmp); } catch (e) { if (e.code !== 'ENOENT') throw e; } }
}
function json(file, fallback) {
  regular(file, true);
  try { return JSON.parse(fs.readFileSync(file, 'utf8')); }
  catch (e) { if (e.code === 'ENOENT') return fallback; throw new Error('invalid_state_json'); }
}
function writeJSON(file, value) { atomic(file, JSON.stringify(value, null, 2) + '\n'); }
function withLock(city, action) {
  prepare(city);
  const dir = stateDir(city);
  const name = `lock-${process.pid}-${crypto.randomBytes(12).toString('hex')}.json`;
  const file = path.join(dir, name);
  // Nonblocking bakery tickets: each invocation owns a unique filename.
  // Recovery only unlinks dead owners' unique names, never a shared guard.
  const peers = () => fs.readdirSync(dir).filter(n => /^lock-\d+-[\w-]+\.json$/.test(n) && n !== name).flatMap(n => {
    const p = path.join(dir, n), owner = json(p, null);
    if (!owner) return [];
    if (!Number.isInteger(owner.pid) || owner.pid <= 0 || !Number.isSafeInteger(owner.ticket)) throw new Error('invalid_lock');
    try { process.kill(owner.pid, 0); }
    catch (e) { if (e.code === 'ESRCH') { try { fs.unlinkSync(p); } catch (e) { if (e.code !== 'ENOENT') throw e; } return []; } }
    return [{ ...owner, name: n }];
  });
  writeJSON(file, { pid: process.pid, ticket: 0 });
  try {
    const ticket = Math.max(0, ...peers().map(p => p.ticket)) + 1;
    writeJSON(file, { pid: process.pid, ticket });
    if (peers().some(p => !p.ticket || p.ticket < ticket || (p.ticket === ticket && p.name < name))) throw new Error('state_busy');
    return action();
  } finally { fs.unlinkSync(file); }
}
function saveState(city, state) {
  writeJSON(path.join(stateDir(city), 'state.json'), state);
  // state.json is the atomic transaction; this human-readable mirror can
  // always be regenerated from it after a crash between the two renames.
  atomic(path.join(stateDir(city), 'log.jsonl'), state.log.map(r => JSON.stringify(r)).join('\n') + (state.log.length ? '\n' : ''));
}
function loadState(city) {
  const state = json(path.join(stateDir(city), 'state.json'), initialState());
  if (state?.version !== 1 || !Number.isFinite(state.now) || !['waiting', 'exemptions', 'known', 'sessions', 'log'].every(k => Array.isArray(state[k])) || !state.calls || !state.receipts) throw new Error('invalid_state_shape');
  if (!state.waiting.every(w => identifier(w.provider) && identifier(w.conversation_id) && Number.isFinite(w.at) && Number.isSafeInteger(w.seq) && Number.isInteger(w.refusals) && typeof w.released === 'boolean')) throw new Error('invalid_waiting_state');
  if (state.cursor && !['dev', 'ino', 'offset', 'seq'].every(k => Number.isSafeInteger(state.cursor[k]) && state.cursor[k] >= 0)) throw new Error('invalid_cursor');
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
  return { ...options, env, city, now: options.now ?? Date.now(), agent, session: env.GC_AGENT === agent ? env.GC_SESSION_ID || '' : '', sessionName: env.GC_AGENT === agent ? env.GC_SESSION_NAME || '' : '', start: options.start ?? performance.now() };
}
function syncEvents(state, options) {
  const result = readEvents(options.city, state.cursor, options);
  reduceEvents(state, result.rows, options);
  state.cursor = result.cursor;
  finishCalls(state);
  return result.caughtUp;
}
function errorLog(city, now, error) {
  // Error codes only: filesystem errors can contain paths, and parser errors
  // can contain message text. Never write their raw messages.
  const known = ['measurement_timeout', 'clock_moved_backwards', 'state_busy', 'invalid_state_json', 'invalid_event_json', 'invalid_event_row', 'event_sequence_gap', 'event_in_future', 'invalid_mode'];
  const code = known.includes(error.message) ? error.message : 'measurement_failed';
  try {
    prepare(city);
    // Unique atomic records also work when the error is lock contention.
    const file = path.join(stateDir(city), `error-${now}-${crypto.randomBytes(12).toString('hex')}.jsonl`);
    atomic(file, JSON.stringify({ type: 'error', at: now, code }) + '\n');
  } catch { /* A broken state directory must never prevent a tool call. */ }
}
function runHook(input, options = {}) {
  const env = options.env || process.env;
  // This check precedes *every* filesystem operation, including mode reads.
  if (env.GC_AGENT !== (env.REACHABLE_AGENT || 'mayor') || input?.agent_id) return {};
  let o;
  try {
    o = normalized(options);
    const mode = readMode(o.city);
    if (mode === 'off') return {};
    return withLock(o.city, () => {
      const state = loadState(o.city);
      const turnId = input.hook_event_name === 'Stop' ? stopIdentity(input) : undefined;
      let fence = ['Stop', 'PostToolUse'].includes(input.hook_event_name) ? Math.max(state.cursor?.seq || 0, eventFence(o.city)) : undefined;
      const caughtUp = syncEvents(state, o);
      if (!caughtUp && input.hook_event_name === 'PreToolUse') fence = eventFence(o.city);
      const manifest = json(path.join(stateDir(o.city), 'install.json'), { aliases: [] });
      const beforeDecision = structuredClone(state);
      const result = decide(state, input, { ...o, mode, aliases: manifest.aliases, caughtUp, fence, turnId });
      checkDeadline(o.start);
      try { saveState(o.city, state); checkDeadline(o.start); }
      catch (error) {
        // A save can itself cross the deadline. Persist the actual fail-open
        // outcome before releasing the lock, including duplicate accounting.
        if (result.hookSpecificOutput) {
          decide(beforeDecision, input, { ...o, mode: 'observe', aliases: manifest.aliases, caughtUp: false, fence });
          saveState(o.city, beforeDecision);
        }
        throw error;
      }
      return result;
    });
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
function aliasesFromImports(text, city, env = process.env) {
  const aliases = [];
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
  }
  return [...new Set(aliases)];
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
function saveSettings(o, file, before, after) {
  if (JSON.stringify(before) === JSON.stringify(after)) return;
  const backup = `settings-backup-${new Date(o.now).toISOString().replace(/[:.]/g, '-')}-${crypto.randomBytes(4).toString('hex')}.json`;
  writeJSON(path.join(stateDir(o.city), backup), before);
  directory(path.dirname(file));
  writeJSON(file, after);
  if (JSON.stringify(json(file)) !== JSON.stringify(after)) throw new Error('settings_readback_failed');
}
function install(options = {}) {
  const o = normalized(options);
  return withLock(o.city, () => {
    const { file, value } = settings(o.city);
    const original = structuredClone(value);
    const manifestFile = path.join(stateDir(o.city), 'install.json');
    const old = json(manifestFile, null);
    const source = fs.realpathSync(o.source || __filename);
    const copy = path.join(fs.realpathSync(stateDir(o.city)), 'hook.cjs');
    const code = fs.readFileSync(source);
    const command = `${shellQuote(process.execPath)} ${shellQuote(copy)} --hook --city ${shellQuote(fs.realpathSync(o.city))}`;
    const aliases = aliasesFromImports(o.imports ?? localGC(o, ['import', 'list']), o.city, o.env);
    const entries = {};
    const existed = old?.existed || Object.fromEntries(EVENTS.map(e => [e, Object.hasOwn(value.hooks || {}, e)]));
    value.hooks ||= {};
    for (const event of EVENTS) {
      value.hooks[event] ||= [];
      const owned = old?.entries?.[event];
      const existing = owned && value.hooks[event].find(e => e.matcher === owned.matcher && e.hooks?.some(h => h.command === owned.command));
      let matcher = existing?.matcher || '^(?:.*)$';
      if (!existing) {
        while (value.hooks[event].some(e => e.matcher === matcher)) matcher = `(?:${matcher})`;
        value.hooks[event].push({ matcher, hooks: [{ type: 'command', command, timeout: 5 }] });
      } else {
        existing.hooks = existing.hooks.map(h => h.command === owned.command ? { ...h, command, timeout: 5 } : h);
      }
      entries[event] = { matcher, command };
    }
    if (!fs.existsSync(copy) || !fs.readFileSync(copy).equals(code)) atomic(copy, code);
    saveSettings(o, file, original, value);
    const manifest = { source, sha: hash(code), aliases, entries, existed, hooksExisted: old?.hooksExisted ?? Object.hasOwn(original, 'hooks') };
    if (JSON.stringify(old) !== JSON.stringify(manifest)) writeJSON(manifestFile, manifest);
    if (!old || !fs.existsSync(path.join(stateDir(o.city), 'mode.json'))) writeJSON(path.join(stateDir(o.city), 'mode.json'), { mode: 'observe' });
    return { installed: true, mode: readMode(o.city) };
  });
}
function uninstall(options = {}) {
  const o = normalized(options);
  return withLock(o.city, () => {
    const manifestFile = path.join(stateDir(o.city), 'install.json');
    const old = json(manifestFile, null);
    if (!old) return { installed: false };
    const { file, value } = settings(o.city);
    const before = structuredClone(value);
    for (const event of EVENTS) {
      if (!value.hooks?.[event]) continue;
      const owned = old.entries[event];
      value.hooks[event] = value.hooks[event].flatMap(entry => {
        if (entry.matcher !== owned.matcher) return [entry];
        const hooks = entry.hooks.filter(h => h.command !== owned.command);
        return hooks.length ? [{ ...entry, hooks }] : [];
      });
      if (!value.hooks[event].length && !old.existed[event]) delete value.hooks[event];
    }
    if (value.hooks && !Object.keys(value.hooks).length && !old.hooksExisted) delete value.hooks;
    saveSettings(o, file, before, value);
    writeJSON(path.join(stateDir(o.city), 'mode.json'), { mode: 'off' });
    fs.unlinkSync(manifestFile);
    fs.unlinkSync(path.join(stateDir(o.city), 'hook.cjs'));
    return { installed: false };
  });
}
function registration(o) {
  try {
    const manifest = json(path.join(stateDir(o.city), 'install.json'), null);
    if (!manifest) return { registered: false, current: false };
    const { value } = settings(o.city);
    const registered = EVENTS.every(event => value.hooks?.[event]?.some(e => e.matcher === manifest.entries[event].matcher && e.hooks?.some(h => h.type === 'command' && h.command === manifest.entries[event].command)));
    const current = hash(fs.readFileSync(o.source || __filename)) === hash(fs.readFileSync(path.join(stateDir(o.city), 'hook.cjs')));
    return { registered, current };
  } catch { return { registered: false, current: false }; }
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
  return errors;
}
function command(verb, args, options = {}) {
  if (verb === 'install' || verb === 'uninstall') {
    if (args.length) throw new Error('unexpected_arguments');
    return verb === 'install' ? install(options) : uninstall(options);
  }
  if (verb === 'doctor') return doctor(options);
  const o = normalized(options);
  return withLock(o.city, () => {
    const dir = stateDir(o.city);
    if (verb === 'mode') {
      if (args.length !== 1 || !['off', 'observe', 'enforce'].includes(args[0])) throw new Error('usage: mode off|observe|enforce');
      writeJSON(path.join(dir, 'mode.json'), { mode: args[0] });
      return { mode: args[0] };
    }
    const state = loadState(o.city);
    const caughtUp = syncEvents(state, o);
    let result;
    if (verb === 'status') {
      if (args.length && (args.length !== 1 || args[0] !== '--json')) throw new Error('usage: status [--json]');
      const waiting = [...new Map(state.waiting.slice().reverse().map(w => [key(w), w])).values()].sort((a, b) => a.at - b.at).map(w => ({ provider: w.provider, conversation_id: w.conversation_id, seconds: Math.floor((o.now - w.at) / 1000) }));
      result = { mode: readMode(o.city), ...registration(o), caught_up: caughtUp, waiting, history: state.log };
    } else if (verb === 'exempt' && args.length === 1 && args[0] === '--list') result = state.exemptions;
    else if (verb === 'pass' || verb === 'exempt') {
      if (verb === 'pass' ? args.length !== 1 : args.length !== 2 || !['--on', '--off'].includes(args[1])) throw new Error(`usage: ${verb} PROVIDER/CONVERSATION${verb === 'exempt' ? ' --on|--off' : ''}`);
      if (!caughtUp) throw new Error('event_catchup_incomplete');
      if (!state.known.includes(args[0])) throw new Error('unknown_conversation');
      if (verb === 'pass' || args[1] === '--on') state.waiting = state.waiting.filter(w => key(w) !== args[0]);
      if (verb === 'exempt') state.exemptions = args[1] === '--on' ? [...new Set([...state.exemptions, args[0]])] : state.exemptions.filter(c => c !== args[0]);
      result = { conversation: args[0], [verb === 'pass' ? 'seen' : 'exempt']: verb === 'pass' || args[1] === '--on' };
    } else throw new Error('unknown_verb');
    saveState(o.city, state);
    return result;
  });
}

module.exports = { LIMITS, POLICY, hash, key, initialState, readEvents, reduceEvents, literalArgv, allowed, refusal, decide, runHook, command, install, uninstall, doctor, aliasesFromImports };

if (require.main === module) {
  const start = performance.now();
  const args = process.argv.slice(2);
  const hook = args[0] === '--hook';
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
      const result = command(verb, args);
      if (verb === 'doctor') {
        process.stdout.write(result.length ? result.join('\n') + '\n' : 'reachable: healthy\n');
        process.exitCode = result.length ? 1 : 0;
      } else if (verb === 'status' && !args.includes('--json')) {
        process.stdout.write(`mode: ${result.mode}\nhook registered: ${result.registered}\nhook current: ${result.current}\ncaught up: ${result.caught_up}\n`);
        for (const row of result.waiting) process.stdout.write(`${key(row)}: ${row.seconds} seconds\n`);
        for (const row of result.history) process.stdout.write(JSON.stringify(row) + '\n');
      } else process.stdout.write(JSON.stringify(result, null, 2) + '\n');
    }
  } catch (error) {
    if (hook) {
      const city = (args[1] === '--city' ? args[2] : '') || process.env.GC_CITY_PATH || process.env.GC_CITY;
      if (city && path.isAbsolute(city)) errorLog(city, Date.now(), error);
    } else { process.stderr.write(`reachable: ${error.message}\n`); process.exitCode = 1; }
  }
}
