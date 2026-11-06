// check.mjs — the whatsapp pack's doctor rows. Exit 0 green, 1 yellow, 2 red.
// Diagnostic only: never sources the env file, never prints a value, a
// message text or a full phone number.
//
// The journal-backed rows (held, restore, probes) fold the journal with the
// adapter's own code (../adapter/src/journal.js), so the row and the adapter
// can never disagree about what a line means.

import { createHash } from 'node:crypto';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import http from 'node:http';
import path from 'node:path';

import { entryState, foldLines, readLines, JOURNAL_FILE, PROBE_RETENTION_MS } from '../adapter/src/journal.js';

const WEEK_MS = 7 * 24 * 60 * 60 * 1000;

function finish(code, message) {
  console.log(message);
  process.exitCode = code;
}

const last4 = (id) => String(id ?? '').replace(/\D/g, '').slice(-4) || '????';
// The adapter's ref for an entry (inbound.js ref): a wamid encodes the phone, so never the rid.
const ref = (rid) => `m-${createHash('sha256').update(String(rid)).digest('hex').slice(0, 10)}`;

function stateDir() {
  const e = process.env;
  if (e.WHATSAPP_STATE_DIR) return e.WHATSAPP_STATE_DIR;
  if (e.GC_CITY_PATH) return path.join(e.GC_CITY_PATH, '.gc', 'whatsapp');
  return '';
}

function envFile() {
  const e = process.env;
  if (e.GC_WHATSAPP_ADAPTER_ENV) return e.GC_WHATSAPP_ADAPTER_ENV;
  const secrets = e.GC_SERVICE_SECRETS_DIR || (e.GC_CITY_PATH && path.join(e.GC_CITY_PATH, '.gc/services/whatsapp/secrets'));
  return secrets ? path.join(secrets, 'env') : path.join(e.HOME || '', '.config/gc-whatsapp-adapter/env');
}

// Literal assignments only: an expansion or a command is reported, never run.
function readEnvFile(names) {
  const file = envFile();
  let text;
  try { text = readFileSync(file, 'utf8'); } catch { return { missingFile: file }; }
  const values = Object.fromEntries(names.map((name) => [name, process.env[name] || '']));
  const dynamic = new Set();
  for (const line of text.split('\n')) {
    const match = line.match(/^\s*(?:export\s+)?([A-Z_]+)=(.*)$/);
    if (!match || !names.includes(match[1])) continue;
    const [, name, raw] = match;
    const literal = raw.trim().match(/^(?:'([^']*)'|"([^"$`\\]*)"|([^\s'"$`\\;|&<>#()]*))\s*(?:#.*)?$/);
    if (!literal) { dynamic.add(name); continue; }
    dynamic.delete(name);
    values[name] = literal[1] ?? literal[2] ?? literal[3];
  }
  return { file, values, dynamic };
}

function env() {
  const names = ['WHATSAPP_GATEWAY_URL', 'WHATSAPP_GATEWAY_CREDENTIAL', 'GC_CITY_NAME', 'GC_API_BASE_URL'];
  const read = readEnvFile(names);
  if (read.missingFile) return finish(2, 'adapter env file missing or unreadable; set GC_WHATSAPP_ADAPTER_ENV or create the per-city whatsapp secrets/env file (gc whatsapp enroll writes the credential into it)');
  const missing = names.filter((name) => !read.values[name] && !read.dynamic.has(name));
  if (missing.length) return finish(2, `missing adapter env vars: ${missing.join(' ')}${missing.includes('WHATSAPP_GATEWAY_CREDENTIAL') ? ' (not enrolled: run gc whatsapp enroll)' : ''}`);
  if (read.dynamic.size) return finish(1, `cannot verify shell expressions without executing them: ${[...read.dynamic].join(' ')}`);
  finish(0, `adapter env vars present: ${names.join(' ')} (values withheld)`);
}

function dependencies() {
  const major = Number(process.versions.node.split('.')[0]);
  if (major < 20) return finish(2, `Node.js 20 or newer is required (found ${process.versions.node})`);
  finish(0, `node ${process.versions.node}; the adapter has no package dependency (plain fetch)`);
}

function runtime() {
  const socketPath = process.env.GC_SERVICE_SOCKET;
  if (!socketPath) return finish(1, 'GC_SERVICE_SOCKET is unset; pass the running service socket path from its startup log to check /healthz');
  if (!path.isAbsolute(socketPath) || Buffer.byteLength(socketPath) >= 104) {
    return finish(2, 'GC_SERVICE_SOCKET must be an absolute Unix socket path shorter than 104 bytes');
  }
  try {
    if (!statSync(socketPath).isSocket()) throw new Error('not a socket');
  } catch { return finish(2, 'GC_SERVICE_SOCKET does not name an existing Unix socket'); }
  let done = false;
  const end = (code, message) => {
    if (done) return;
    done = true;
    clearTimeout(timer);
    finish(code, message);
    request.destroy();
  };
  const request = http.get({ socketPath, path: '/healthz' }, (response) => {
    const chunks = [];
    response.on('data', (c) => chunks.push(c));
    response.on('end', () => {
      if (response.statusCode !== 200) return end(2, 'Unix socket /healthz did not return 200');
      let body = {};
      try { body = JSON.parse(Buffer.concat(chunks).toString('utf8')); } catch { /* keep {} */ }
      if (body.gateway_auth && body.gateway_auth !== 'ok') return end(2, `the adapter answers /healthz but the gateway refused its credential (${body.gateway_auth}): run gc whatsapp enroll`);
      end(0, 'Unix socket /healthz returns 200');
    });
  });
  const timer = setTimeout(() => end(2, 'Unix socket /healthz timed out'), 2000);
  request.on('error', () => end(2, 'Unix socket /healthz connection failed'));
}

async function gateway() {
  const read = readEnvFile(['WHATSAPP_GATEWAY_URL', 'WHATSAPP_GATEWAY_CREDENTIAL']);
  const url = (read.values?.WHATSAPP_GATEWAY_URL || '').replace(/\/+$/, '');
  if (!url) return finish(2, 'WHATSAPP_GATEWAY_URL is not set (the env file or the environment)');
  const fetchOnce = async (pathname, headers = {}) => {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 5000);
    try {
      return await fetch(`${url}${pathname}`, { headers, signal: controller.signal });
    } finally {
      clearTimeout(timer);
    }
  };
  let health;
  try { health = await fetchOnce('/healthz'); } catch { return finish(2, `the gateway at ${url} is unreachable`); }
  if (health.status !== 200) return finish(2, `the gateway at ${url} answered /healthz ${health.status}`);
  const credential = read.values?.WHATSAPP_GATEWAY_CREDENTIAL;
  if (!credential) return finish(2, 'not enrolled: run gc whatsapp enroll');
  let inbox;
  try { inbox = await fetchOnce('/v1/inbox?limit=1', { Authorization: `Bearer ${credential}` }); } catch { return finish(2, `the gateway at ${url} is unreachable`); }
  if (inbox.status === 401) return finish(2, 'not enrolled: run gc whatsapp enroll (the gateway refused the credential)');
  if (inbox.status !== 200) return finish(2, `the gateway answered the credential check with ${inbox.status}`);
  finish(0, `the gateway at ${url} answers /healthz and accepts the credential`);
}

function loadFold() {
  const dir = stateDir();
  if (!dir) return { error: 'GC_CITY_PATH is unset; cannot locate <city>/.gc/whatsapp' };
  try {
    const { lines } = readLines(path.join(dir, JOURNAL_FILE));
    return { fold: foldLines(lines) };
  } catch (err) {
    return { error: `the inbound journal is unreadable: ${err.message}` };
  }
}

function deadletters() {
  const dir = stateDir();
  if (!dir) return finish(1, 'GC_CITY_PATH is unset; cannot locate <city>/.gc/whatsapp');
  const deadDir = path.join(dir, 'inbound_dead_letter');
  let files = [];
  try { files = readdirSync(deadDir).filter((f) => f.endsWith('.jsonl')); } catch { return finish(0, 'no dead letters'); }
  const reasons = {};
  let count = 0;
  for (const file of files) {
    const { lines } = readLines(path.join(deadDir, file));
    for (const line of lines) {
      count += 1;
      reasons[line.reason] = (reasons[line.reason] ?? 0) + 1;
    }
  }
  if (!count) return finish(0, 'no dead letters');
  const parts = Object.entries(reasons).map(([reason, n]) => `${reason} ${n}`).join(', ');
  const twoMeanings = reasons.redelivery_no_route
    ? '; redelivery_no_route means gc took the message and either already delivered it or nobody is routed to the conversation: do not re-send it from here'
    : '';
  finish(1, `${count} dead letter(s) in ${deadDir} (${parts})${twoMeanings}`);
}

function held() {
  const { fold, error } = loadFold();
  if (error) return finish(1, error);
  const holds = [...fold.entries.values()].filter((e) => entryState(e) === 'held');
  if (!holds.length) return finish(0, 'no terminal holds');
  const now = Date.now();
  const oldest = Math.max(...holds.map((e) => now - (e.held?.at ?? now)));
  const list = holds.map((e) => `${ref(e.rid)} (...${last4(e.waId)}, receipt ${e.held?.receipt_id || e.accepted?.receipt_id || '?'}, held ${e.held?.at ? new Date(e.held.at).toISOString() : '?'})`).join(', ');
  finish(1, `${holds.length} terminal hold(s), the oldest ${Math.round(oldest / 60000)} min: ${list}; resolve each with gc whatsapp inbound resolve <ref> --delivered|--dead`);
}

function restore() {
  const { fold, error } = loadFold();
  if (error) return finish(1, error);
  if (!fold.gaps.size) return finish(0, `no restore gap (${fold.watermarks.size} watermark(s))`);
  const lines = [...fold.gaps.values()].map((g) => `...${last4(g.conversation_id)}: the journal's accepted ${g.watermark_accepted_at ? new Date(g.watermark_accepted_at).toISOString() : '?'} (sequence ${g.watermark_sequence}) is not in the gc store (${g.head_sequence == null ? 'its head unknown' : `its head ${g.head_created_at ?? 'none'}, sequence ${g.head_sequence}`})`);
  finish(1, `restore gap: ${lines.join('; ')}; every in-doubt message takes the marked path until gc whatsapp inbound clear-gap <wa_id>`);
}

function probes() {
  const { fold, error } = loadFold();
  if (error) return finish(1, error);
  const since = Date.now() - PROBE_RETENTION_MS;
  const snapshots = fold.probes.filter((p) => p.shape === 'pane' && (p.at ?? 0) >= since);
  if (!snapshots.length) return finish(0, 'no terminal-snapshot probe in the last 7 days');
  const sessions = [...new Set(snapshots.map((p) => p.session_id))].join(', ');
  finish(1, `${snapshots.length} paste probe(s) answered by a terminal snapshot (format text) in the last 7 days (session ${sessions}): that session yields no provider log, so every in-doubt message there takes the marked path`);
}

// The keeper reads GET /v1/window once a minute and stamps the good reads
// in keeper.json (window_read_ok_at, rewritten once a minute). Without a
// fresh clock the ladder and the lapse count wait, so a stamp older than 5
// minutes (or none) while any conversation has the keeper ON is red.
const WINDOW_READ_STALE_MS = 5 * 60 * 1000;

function keeper() {
  const dir = stateDir();
  if (!dir) return finish(1, 'GC_CITY_PATH is unset; cannot locate <city>/.gc/whatsapp');
  let state;
  try { state = JSON.parse(readFileSync(path.join(dir, 'keeper.json'), 'utf8')); } catch { return finish(0, 'the window-keeper has no state yet'); }
  const now = Date.now();
  const since = now - WEEK_MS;
  // keeper.json stores instants as ISO strings (keeper.js toDisk).
  const atMs = (x) => (typeof x === 'number' ? x : typeof x?.at === 'number' ? x.at : Date.parse(x?.at ?? '') || 0);
  const instant = (x) => (typeof x === 'number' ? x : typeof x === 'string' ? Date.parse(x) : NaN);
  const recent = (list) => (Array.isArray(list) ? list.filter((x) => atMs(x) >= since).length : 0);
  const conversations = state.conversations ?? {};
  const rows = [];
  for (const [waId, c] of Object.entries(conversations)) {
    const lapses = recent(c.lapses);
    const undelivered = recent(c.undelivered);
    const refused = recent(c.refused_nudges);
    if (lapses || undelivered || refused) rows.push(`...${last4(waId)}: ${lapses} lapse(s), ${undelivered} undelivered keeper event(s), ${refused} refused publish(es)`);
  }
  const on = Object.values(conversations).filter((c) => c?.consent === true).length;
  const okAt = instant(state.window_read_ok_at);
  if (on && !(now - okAt <= WINDOW_READ_STALE_MS)) {
    const when = Number.isFinite(okAt)
      ? `last read OK ${new Date(okAt).toISOString()} (${Math.round((now - okAt) / 60000)} min ago)`
      : 'never read OK';
    const errAt = instant(state.window_read_error_at);
    const why = state.window_read_error
      ? `; the last error${Number.isFinite(errAt) ? ` (${new Date(errAt).toISOString()})` : ''}: ${state.window_read_error}`
      : '; no error recorded (is the adapter running? gc service list)';
    const also = rows.length ? `; last 7 days: ${rows.join('; ')}` : '';
    return finish(2, `window-keeper: the gateway's window clock (GET /v1/window) is ${when} while ${on} conversation(s) have the keeper ON: after a restart or a delivery the ladder and the lapse count wait for a fresh read${why}${also}`);
  }
  if (!rows.length) return finish(0, `window-keeper: no lapse and no undelivered event in the last 7 days (${Object.keys(conversations).length} conversation(s))`);
  finish(1, `window-keeper, last 7 days: ${rows.join('; ')}`);
}

const checks = { env, dependencies, runtime, gateway, deadletters, held, restore, probes, keeper };
const name = process.argv[2];
if (!checks[name]) finish(2, 'unknown doctor check');
else await checks[name]();
