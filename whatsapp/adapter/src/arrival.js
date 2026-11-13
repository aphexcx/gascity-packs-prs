// Arrival reactions are independent of delivery and are never taken back.
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { conversationOf } from './render.js';
import { redactText } from './redact.js';

export const MAX_AGE_MS = 60000;
export const CHOICE_BUDGET_MS = 1500;
export const REACTION_BUDGET_MS = 2000;
export const CONCURRENCY = 4;
// An acknowledgement marker matters only within MAX_AGE_MS of its message.
export const ACK_MARKER_MAX_AGE_MS = 24 * 60 * 60 * 1000;
export const INSTRUCTIONS = 'Pick the reaction that best acknowledges this chat message to an assistant. If two reactions fit, a request outranks a feeling; otherwise the narrower one wins.';
const DEFAULT = Object.freeze({ emoji: '👀', slack_name: 'eyes' });
const fallback = () => ({ row: DEFAULT, how: 'default' });
const digest = (s) => createHash('sha256').update(s).digest('hex');

export function createChoice({ env = process.env, catalog, fetchImpl = fetch, now = Date.now, setTimer = setTimeout, clearTimer = clearTimeout, log = () => {} } = {}) {
  const mode = env.ACK_REACTION || ''; let scope = env.ACK_SCOPE || 'addressed';
  const key = env.JEV_API_KEY || ''; const model = env.JEV_MODEL || 'jev-latest';
  const base = env.JEV_API_BASE_URL || 'https://api.typesafe.ai';
  // The scope is checked only when the switch is set; unset, any value reads
  // as addressed, with no line, as before the switch existed.
  if (!['addressed', 'all'].includes(scope)) {
    if (mode) throw new Error('ACK_SCOPE must be addressed or all');
    scope = 'addressed';
  }
  let url;
  if (mode) {
    try { url = new URL(base); } catch { /* diagnosed without echoing a credential-bearing URL */ }
    if (!url || url.username || url.password || url.search || url.hash || !(url.protocol === 'https:' || url.protocol === 'http:' && ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname))) throw new Error('JEV_API_BASE_URL must use HTTPS or local HTTP, without credentials, query or fragment');
  }
  const rows = new Map(); const names = new Set();
  if (mode) {
    if (catalog === undefined) { try { catalog = fs.readFileSync(new URL('../../reactions.json', import.meta.url), 'utf8'); } catch { catalog = ''; } }
    let source; try { source = JSON.parse(catalog)?.reactions; } catch { /* fixed fallback */ }
    if (Array.isArray(source)) for (const [i, row] of source.entries()) {
      if (!row || typeof row !== 'object' || typeof row.adapter_may_place !== 'boolean' || !row.adapter_may_place || !['emoji', 'slack_name', 'meaning'].every((k) => typeof row[k] === 'string' && row[k])) continue;
      if (names.has(row.slack_name)) throw new Error('ACK_REACTION: duplicate arrival name in reactions.json');
      names.add(row.slack_name); rows.set(`option_${String(i + 1).padStart(3, '0')}`, row);
    }
    if (rows.size > 255) throw new Error('ACK_REACTION: reactions.json exceeds 255 arrival choices');
    if (!rows.size) log('ACK_REACTION: reactions.json unusable; using fixed eyes default');
  }
  const fixed = [...rows.values()].find((row) => row.slack_name === mode) || (!rows.size && mode === DEFAULT.slack_name ? DEFAULT : null);
  if (mode && mode !== 'jev' && !fixed) throw new Error('ACK_REACTION must be jev or an arrival name in reactions.json');
  if (mode === 'jev' && !key) log('JEV_API_KEY not set; using fixed eyes default');
  let active = 0; const waiters = new Set();
  async function acquire(signal) {
    while (active >= CONCURRENCY) {
      if (signal.aborted) return false;
      await new Promise((resolve) => {
        const wake = () => { waiters.delete(wake); signal.removeEventListener('abort', wake); resolve(); };
        waiters.add(wake); signal.addEventListener('abort', wake, { once: true });
      });
    }
    if (signal.aborted) return false;
    active++; return true;
  }
  async function request(state, signal) {
    if (!await acquire(signal)) return fallback();
    try {
      const response = await fetchImpl(`${base.replace(/\/+$/, '')}/v1/systemone`, { method: 'POST', signal, redirect: 'error',
        headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
        body: JSON.stringify({ model, state, questions: { ack: { type: 'choice', instructions: INSTRUCTIONS, criteria: Object.fromEntries([...rows].map(([name, row]) => [name, row.meaning])) } } }),
      });
      if (response.status !== 200) return fallback();
      const reader = response.body.getReader(); const chunks = []; let size = 0;
      try { for (;;) { const { done, value } = await reader.read(); if (done) break; size += value.length; if (size > 65536) return fallback(); chunks.push(value); } } finally { await reader.cancel().catch(() => {}); }
      const answer = JSON.parse(Buffer.concat(chunks).toString('utf8'));
      const row = rows.get(answer?.answers?.ack?.choice); return row ? { row, how: 'jev' } : fallback();
    } catch { return fallback(); } finally { active--; for (const wake of [...waiters]) wake(); }
  }
  return {
    enabled: Boolean(mode), all: scope === 'all', keyStatus: key ? 'set' : 'not set',
    async choose(text, seen = now()) {
      if (!mode) return null;
      if (mode !== 'jev') return { row: fixed, how: 'fixed' };
      // Slack's secret redaction first, then the cut (redact.js).
      const state = Array.from(redactText(String(text || ''))).slice(0, 1000).join('');
      const remaining = CHOICE_BUDGET_MS - (now() - seen);
      if (!key || !rows.size || !state.trim() || remaining <= 0) return fallback();
      const controller = new AbortController(); let timer;
      const deadline = new Promise((resolve) => { timer = setTimer(() => { controller.abort(); resolve(fallback()); }, remaining); });
      try { return await Promise.race([request(state, controller.signal), deadline]); }
      finally { clearTimer(timer); controller.abort(); }
    },
  };
}

// An attempt marker, created exclusively: null when it already exists (the
// attempt was spent before). Asynchronous, so no caller's thread waits on the
// disk; flushMarker makes it durable, with its directory.
export async function createMarker(stateDir, namespace, id) {
  const dir = path.join(stateDir, namespace);
  await fsp.mkdir(dir, { recursive: true, mode: 0o700 });
  try { return { dir, handle: await fsp.open(path.join(dir, digest(id)), 'wx', 0o600) }; }
  catch (err) { if (err.code === 'EEXIST') return null; throw err; }
}

export async function flushMarker({ dir, handle }) {
  try { await handle.sync(); } finally { await handle.close(); }
  const parent = await fsp.open(dir, 'r');
  try { await parent.sync(); } finally { await parent.close(); }
}

// Atomically reserve before any network effect. A crash spends the attempt;
// an uncertain response must not produce a second reaction after restart.
export async function reserve(stateDir, namespace, id) {
  const marker = await createMarker(stateDir, namespace, id);
  if (!marker) return false;
  await flushMarker(marker);
  return true;
}

// Removes this adapter's own markers older than maxAgeMs, by modification
// time: only empty files named by a sha256 in hex. Nothing else is touched.
export async function pruneMarkers(dir, maxAgeMs, now = Date.now()) {
  let names;
  try { names = await fsp.readdir(dir); } catch { return; }
  for (const name of names) {
    if (!/^[0-9a-f]{64}$/.test(name)) continue;
    const file = path.join(dir, name);
    try {
      const stat = await fsp.lstat(file);
      if (stat.isFile() && stat.size === 0 && stat.mtimeMs < now - maxAgeMs) await fsp.unlink(file);
    } catch { /* gone or unreadable: left as it is */ }
  }
}

export function createArrival({ choice, stateDir, gateway, stopwatch, clock = Date.now, log = () => {} }) {
  const tasks = new Set(); let warned = false;
  const warn = () => { if (!warned) { warned = true; log('ACK_REACTION: acknowledgement failed; no retry'); } };
  const track = (task) => { tasks.add(task); task.finally(() => tasks.delete(task)); return task; };
  // At start, and once a day from index.js while running.
  const prune = () => (stateDir ? track(pruneMarkers(path.join(stateDir, 'ack-attempts'), ACK_MARKER_MAX_AGE_MS).catch(() => {})) : Promise.resolve());
  prune();
  return {
    prune,
    observe(delivery, { replay = false, seen = clock() } = {}) {
      const msg = delivery?.message; const conversation = conversationOf(delivery);
      const at = Number(msg?.timestamp) * 1000;
      if (replay || delivery?.kind !== 'message' || !conversation || !msg?.id || ['reaction', 'system', 'unsupported'].includes(msg.type) || !Number.isFinite(at) || at <= 0 || seen - at > MAX_AGE_MS) return;
      const task = new Promise((resolve) => setImmediate(resolve)).then(async () => {
        stopwatch?.record({ event: 'inbound', conversation, message: msg.id, message_at: new Date(at).toISOString(), at: new Date(seen).toISOString() });
        if (!choice.enabled) return;
        // Claimed and flushed before the Jev call: at most one, across a crash.
        if (!await reserve(stateDir, 'ack-attempts', `${conversation}\0${msg.id}`)) return;
        // Only the sender's words, never generated attachment descriptions,
        // interactive IDs, locations, sender profiles or transport metadata.
        const text = msg.type === 'text' ? msg.text?.body : msg[msg.type]?.caption ?? msg.button?.text ?? msg.interactive?.button_reply?.title ?? msg.interactive?.list_reply?.title ?? '';
        const picked = await choice.choose(text, seen);
        const to = Object.hasOwn(delivery, 'user_id') ? delivery.user_id : conversation;
        if (typeof to !== 'string' || !to) { warn(); return; }
        const response = await gateway.send({ to, idempotency_key: `ack:${digest(`${conversation}\0${msg.id}`)}`, type: 'reaction', reaction: { message_id: msg.id, emoji: picked.row.emoji } });
        if (!response?.message_id) { warn(); return; }
        stopwatch?.record({ event: 'reaction', conversation, message: msg.id, at: new Date(clock()).toISOString(), actor: 'adapter', choice: picked.how });
      }).catch(warn);
      track(task);
    },
    async wait() { while (tasks.size) await Promise.all([...tasks]); },
  };
}
