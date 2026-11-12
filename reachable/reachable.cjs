#!/usr/bin/env node
'use strict';

// No dependencies, network clients, message bodies, or actor names.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const zlib = require('node:zlib');
const { performance } = require('node:perf_hooks');
const { execFileSync } = require('node:child_process');

const DAY = 86400000;
const LIMITS = Object.freeze({ wait: 30000, refusals: 3, age: DAY, history: 14 * DAY, deadline: 2000 });
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

// The active file is read through one descriptor. A retained boundary digest
// also catches truncate-and-rewrite on a filesystem that reuses the inode.
function readEvents(city, cursor, { now = Date.now(), start = performance.now() } = {}) {
  const dir = path.join(city, '.gc');
  const file = path.join(dir, 'events.jsonl');
  const fd = fs.openSync(file, 'r');
  try {
    const stat = fs.fstatSync(fd);
    if (!stat.isFile()) throw new Error('event_log_not_regular');
    let offset = 0;
    let rows = [];
    let seq = cursor?.seq || 0;
    let same = cursor && cursor.dev === stat.dev && cursor.ino === stat.ino && cursor.offset <= stat.size;
    if (same && cursor.offset) {
      const boundary = Buffer.alloc(Math.min(128, cursor.offset));
      fs.readSync(fd, boundary, 0, boundary.length, cursor.offset - boundary.length);
      same = hash(boundary) === cursor.boundary;
    }
    if (same) offset = cursor.offset;
    else {
      const archives = fs.readdirSync(dir).flatMap(name => {
        const m = /^events\.jsonl\.archive-(\d{8}T\d{6}Z)-seq-(\d+)-(\d+)\.gz$/.exec(name);
        if (!m) return [];
        const stamp = m[1].replace(/^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/, '$1-$2-$3T$4:$5:$6Z');
        return Number(m[3]) > seq && (cursor || Date.parse(stamp) >= now - LIMITS.age) ? [{ name, first: Number(m[2]) }] : [];
      }).sort((a, b) => a.first - b.first);
      for (const archive of archives) {
        checkDeadline(start);
        const contents = zlib.gunzipSync(fs.readFileSync(path.join(dir, archive.name)), { maxOutputLength: 512 * 1024 * 1024 });
        if (contents.length && contents.at(-1) !== 10) throw new Error('incomplete_archive');
        const recovered = parseRows(contents, seq, start);
        for (const row of recovered) rows.push(row);
        if (recovered.length) seq = recovered.at(-1).seq;
      }
    }
    checkDeadline(start);
    // Initial catch-up is bounded and continues on later calls. No refusal is
    // possible until caughtUp, because a later reply may still be unread.
    const bytes = Buffer.alloc(Math.min(stat.size - offset, 8 * 1024 * 1024));
    const count = fs.readSync(fd, bytes, 0, bytes.length, offset);
    const end = bytes.subarray(0, count).lastIndexOf(10) + 1;
    if (!end && count === bytes.length && count === 8 * 1024 * 1024) throw new Error('event_row_too_large');
    const appended = parseRows(bytes.subarray(0, end), seq, start);
    for (const row of appended) rows.push(row);
    if (appended.length) seq = appended.at(-1).seq;
    offset += end;
    const boundary = Buffer.alloc(Math.min(128, offset));
    fs.readSync(fd, boundary, 0, boundary.length, offset - boundary.length);
    checkDeadline(start);
    return { rows, caughtUp: offset === stat.size, cursor: { dev: stat.dev, ino: stat.ino, offset, seq, boundary: hash(boundary) } };
  } finally { fs.closeSync(fd); }
}

function reduceEvents(state, rows, { now, agent, session }) {
  if (state.now > now) throw new Error('clock_moved_backwards');
  const own = new Set(state.sessions);
  if (session) own.add(hash(session));
  // Learn named-session replacements before examining delivered words.
  for (const row of rows) {
    const p = row.payload;
    if (row.type === 'session.woke' && row.subject === agent && identifier(row.session_id)) own.add(hash(row.session_id));
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
      const ours = p.target_agent ? p.target_agent === agent : p.target_session === session;
      if (!ours) continue;
      const conv = key(p);
      if (!state.known.includes(conv)) state.known.push(conv);
      if (now - at >= LIMITS.age || state.exemptions.includes(conv)) continue;
      state.waiting.push({ provider: p.provider, conversation_id: p.conversation_id, at, seq: row.seq, refusals: 0, released: false });
      for (const call of Object.values(state.calls)) if (at >= call.at) call.arrived = true;
    } else {
      if (!identifier(p.session)) throw new Error('invalid_outbound_session');
      if (own.has(hash(p.session))) state.waiting = state.waiting.filter(w => key(w) !== key(p) || w.at > at);
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
  const token = event === 'Stop' ? `${input.prompt_id || ''}:${hash(input.last_assistant_message || '')}:${state.turn || ''}` : input.tool_use_id;
  // A duplicate Stop shares the last tool boundary even when the harness
  // omits prompt_id. Reading another event must not change that identity.
  const id = hash(`${session}:${event}:${token}`);
  if (state.receipts[id]) return state.receipts[id].output;
  let output = {};
  if (event === 'Stop') state.waiting = [];
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
    if (!output.hookSpecificOutput) state.calls[hash(`${session}:${input.tool_use_id}`)] = { at: now, tool: input.tool_name, waiting: state.waiting.length > 0, arrived: false };
  } else if (event === 'PostToolUse') {
    if (!identifier(input.tool_use_id)) throw new Error('invalid_tool_input');
    const callKey = hash(`${session}:${input.tool_use_id}`);
    const call = state.calls[callKey];
    if (call) {
      const elapsed = Number.isFinite(input.duration_ms) ? input.duration_ms : now - call.at;
      if (elapsed < 0) throw new Error('clock_moved_backwards');
      if (elapsed > waitMs) state.log.push({ type: 'long_call', at: now, tool: call.tool, seconds: elapsed / 1000, waiting: call.waiting || call.arrived });
      delete state.calls[callKey];
    }
  } else throw new Error('unknown_hook_event');
  state.receipts[id] = { at: now, output };
  for (const [k, v] of Object.entries(state.receipts)) if (now - v.at > LIMITS.age) delete state.receipts[k];
  for (const [k, v] of Object.entries(state.calls)) if (now - v.at > LIMITS.age) delete state.calls[k];
  return output;
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
  const file = path.join(stateDir(city), 'lock.json');
  const recovery = path.join(stateDir(city), 'recovery.json');
  const create = () => fs.writeFileSync(file, JSON.stringify({ pid: process.pid }), { flag: 'wx', mode: 0o600 });
  // Serialize stale-lock recovery too: otherwise two processes could both
  // observe a dead owner, and one could unlink the other's fresh lock.
  try {
    try {
      create();
      if (fs.existsSync(recovery)) { fs.unlinkSync(file); throw new Error('state_busy'); }
    } catch (error) {
      if (error.code !== 'EEXIST') throw error;
      fs.writeFileSync(recovery, JSON.stringify({ pid: process.pid }), { flag: 'wx', mode: 0o600 });
      try {
        const old = json(file, null);
        if (old) {
          if (!Number.isInteger(old.pid) || old.pid <= 0) throw new Error('invalid_lock');
          let alive = true;
          try { process.kill(old.pid, 0); } catch (e) { if (e.code === 'ESRCH') alive = false; }
          if (alive) throw new Error('state_busy');
          fs.unlinkSync(file);
        }
        create();
      } finally { fs.unlinkSync(recovery); }
    }
  } catch (e) { throw new Error(e.code === 'EEXIST' ? 'state_busy' : e.message); }
  try { return action(); } finally { fs.unlinkSync(file); }
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
  return { ...options, env, city, now: options.now ?? Date.now(), agent: env.REACHABLE_AGENT || 'mayor', session: env.GC_SESSION_ID || '', start: options.start ?? performance.now() };
}
function syncEvents(state, options) {
  const result = readEvents(options.city, state.cursor, options);
  reduceEvents(state, result.rows, options);
  state.cursor = result.cursor;
  return result.caughtUp;
}
function errorLog(city, now, error) {
  // Error codes only: filesystem errors can contain paths, and parser errors
  // can contain message text. Never write their raw messages.
  const known = ['measurement_timeout', 'clock_moved_backwards', 'state_busy', 'invalid_state_json', 'invalid_event_json', 'invalid_event_row', 'event_sequence_gap', 'event_in_future', 'invalid_mode'];
  const code = known.includes(error.message) ? error.message : 'measurement_failed';
  try {
    prepare(city);
    const file = path.join(stateDir(city), 'errors.jsonl');
    regular(file, true);
    let rows = [];
    if (fs.existsSync(file)) rows = fs.readFileSync(file, 'utf8').trim().split('\n').filter(Boolean).map(s => JSON.parse(s)).filter(r => now - r.at <= LIMITS.history);
    rows.push({ type: 'error', at: now, code });
    atomic(file, rows.map(r => JSON.stringify(r)).join('\n') + '\n');
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
      const caughtUp = syncEvents(state, o);
      const manifest = json(path.join(stateDir(o.city), 'install.json'), { aliases: [] });
      const result = decide(state, input, { ...o, mode, aliases: manifest.aliases, caughtUp });
      checkDeadline(o.start);
      saveState(o.city, state);
      checkDeadline(o.start);
      return result;
    });
  } catch (error) { if (o) errorLog(o.city, o.now, error); return {}; }
}
function localGC(o, args) {
  // Both commands used here inspect local configuration; neither calls a
  // city's API. They run only from explicit commands, never the hot hook.
  return execFileSync('gc', ['--city', o.city, ...args], { cwd: o.city, env: o.env, encoding: 'utf8', timeout: 1500, maxBuffer: 8 * 1024 * 1024, stdio: ['ignore', 'pipe', 'pipe'] });
}
function aliasesFromImports(text, city) {
  const aliases = [];
  for (const line of text.trim().split('\n')) {
    const [alias, source] = line.split('\t');
    if (!alias || alias === '(transitive)' || !source) continue;
    let pack;
    if (/^https?:/.test(source)) pack = source.replace(/#.*$/, '').replace(/\/$/, '').split('/').at(-1);
    else {
      // Local paths need not end with a pack's name.
      const file = path.join(path.resolve(city, source), 'pack.toml');
      if (fs.existsSync(file)) {
        const text = fs.readFileSync(file, 'utf8');
        pack = /\[pack\]([\s\S]*?)(?=\n\[|$)/.exec(text)?.[1].match(/^name\s*=\s*['"]([^'"]+)['"]/m)?.[1];
      } else pack = path.basename(source);
    }
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
    const aliases = aliasesFromImports(o.imports ?? localGC(o, ['import', 'list']), o.city);
    const entries = {};
    const existed = old?.existed || Object.fromEntries(EVENTS.map(e => [e, Object.hasOwn(value.hooks || {}, e)]));
    value.hooks ||= {};
    for (const event of EVENTS) {
      value.hooks[event] ||= [];
      const owned = old?.entries?.[event];
      const existing = value.hooks[event].find(e => e.matcher === owned?.matcher && e.hooks?.some(h => h.command === owned.command));
      let matcher = existing?.matcher || '^(?:.*)$';
      if (!existing) {
        while (value.hooks[event].some(e => e.matcher === matcher)) matcher = `(?:${matcher})`;
        value.hooks[event].push({ matcher, hooks: [{ type: 'command', command, timeout: 2 }] });
      } else {
        existing.hooks = existing.hooks.map(h => h.command === owned.command ? { ...h, command, timeout: 2 } : h);
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
    const current = hash(fs.readFileSync(manifest.source)) === hash(fs.readFileSync(path.join(stateDir(o.city), 'hook.cjs')));
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
    const source = o.source || json(path.join(stateDir(o.city), 'install.json'), {}).source || __filename;
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
