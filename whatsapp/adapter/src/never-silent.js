import fs from 'node:fs';
import path from 'node:path';
import { randomUUID, createHash } from 'node:crypto';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { writeFileAtomic } from './ledger.js';

export const FAILURE_WAIT_MS = 3 * 60 * 1000;
export const HEALTH_MAX_AGE_MS = 5 * 60 * 1000;
export const OWNER_COOLDOWN_MS = 6 * 60 * 60 * 1000;
export const MAIL_RETRY_MS = 60 * 1000;
export const MAIL_LIFETIME_MS = 24 * 60 * 60 * 1000;

const states = new Set(['ok', 'no_seat', 'seat_at_cap', 'session_down']);
const rfc3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const stamp = (value) => typeof value === 'string' && rfc3339.test(value) ? Date.parse(value) : NaN;

export function readHealth(file, now, maxAge = HEALTH_MAX_AGE_MS) {
  const unknown = (reason) => ({ state: 'unknown', reason });
  let h;
  try {
    if (fs.statSync(file).size > 16384) return unknown('oversized');
    h = JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch { return unknown('unavailable or invalid JSON'); }
  if (!h || !states.has(h.state)) return unknown('unrecognized state');
  const since = stamp(h.since); const checked = stamp(h.checked_at);
  if (!Number.isFinite(since) || !Number.isFinite(checked) || (h.resets_at !== undefined && !Number.isFinite(stamp(h.resets_at)))) return unknown('invalid or missing timestamp');
  if (since > checked || checked > now) return unknown('inconsistent timestamp');
  if (now - checked > maxAge) return unknown('stale');
  return { state: h.state, since: h.since, checked_at: h.checked_at, age_seconds: (now - checked) / 1000,
    ...(h.resets_at === undefined ? {} : { resets_at: h.resets_at }) };
}

export const OWNER_LINES = Object.freeze({
  not_delivered: "I can't read your message right now. The team that runs me has been told, and I'll answer as soon as I'm back.",
  session_down: "I can't read your message right now. The team that runs me has been told, and I'll answer as soon as I'm back.",
  no_seat: "I can't answer yet because no Claude account is signed in for me. The team that runs me has been told.",
  seat_at_cap: "I've reached my usage limit for now and can't answer until it resets. The team that runs me has been told.",
});
const meanings = { not_delivered: 'the message did not land and will not without recovery', no_seat: 'no seat is signed in', seat_at_cap: 'the seat has reached its usage cap', session_down: 'the session cannot answer' };
const exec = promisify(execFile);
const iso = (at) => new Date(at).toISOString();
const dictionary = (value) => Object.assign(Object.create(null), value);

export function createNeverSilent({ enabled = false, stateDir, healthFile = '', city = '', operatorContext = '', operatorAgent = 'mayor', localAgent = 'mayor', gcBin = 'gc',
  clock = Date.now, post, send, sendLocal, log = () => {} } = {}) {
  const file = path.join(stateDir, 'never-silent.json');
  let state = { messages: dictionary(), owners: dictionary(), acts: [], posted: [], outages: [] };
  if (enabled) {
    if (typeof post !== 'function') throw new Error('never-silent requires an owner sender');
    try {
      state = JSON.parse(fs.readFileSync(file, 'utf8'));
      if (!state.messages || !state.owners || !Array.isArray(state.outages) || !Array.isArray(state.acts) || !Array.isArray(state.posted)) throw new Error('invalid state');
      state.messages = dictionary(state.messages); state.owners = dictionary(state.owners);
    } catch (err) { if (err.code !== 'ENOENT') throw new Error('never-silent state cannot be read; preserve it for recovery'); }
  }
  const mailCommand = async (args) => { await exec(gcBin || 'gc', ['mail', 'send', ...args], { timeout: 10000, maxBuffer: 16384 }); };
  send ??= (mail) => mailCommand(['--context', operatorContext, operatorAgent || 'mayor', '--dedup', mail.key, '-s', mail.subject, '-m', mail.body]);
  // The existing local adapter report has no customer text. This additional
  // mail tells the returning mayor exactly which fixed line its owner saw.
  sendLocal ??= (message, line) => {
    const digest = createHash('sha256').update(message.id + String(message.created)).digest('hex');
    return mailCommand([localAgent, '--dedup', `whatsapp-owner-notice:${digest}:${line ? 'owner' : 'delivery'}`, '-s', 'whatsapp: delivery', '-m',
      `An addressed message remains unresolved.\n${line ? `The adapter told the owner: ${JSON.stringify(line)}\n` : ''}Conversation: ${message.conversation}\nStatus: gc whatsapp status\n`]);
  };
  function change(fn) {
    const next = structuredClone(state);
    next.messages = dictionary(next.messages); next.owners = dictionary(next.owners);
    fn(next);
    writeFileAtomic(file, JSON.stringify(next));
    state = next;
  }
  function track(message) {
    if (!enabled || Object.hasOwn(state.messages, message.id)) return;
    if (!message.id || !message.conversation) throw new Error('never-silent: missing message coordinates');
    change((s) => { s.messages[message.id] = { ...message, created: message.created ?? clock(), targets: message.targets ?? [] }; });
  }
  function result(id, kind, targets = [], at = clock()) {
    if (!enabled || !state.messages[id] || state.messages[id].removed) return;
    change((s) => {
      const m = s.messages[id]; const now = at;
      if (m.observed !== undefined && at < m.observed) return;
      m.observed = at;
      if (targets.length) m.targets = [...targets];
      if (['failed', 'unreachable'].includes(kind)) {
        if (m.failureKind !== kind || m.failureSince === undefined) m.failureSince = now;
        m.failureKind = kind;
      } else { delete m.failureSince; delete m.failureKind; }
      if (kind === 'delivered') { m.delivered ??= now; delete m.stopped; }
    });
  }
  function stop(id, definite, at = clock()) {
    if (!enabled || !state.messages[id] || state.messages[id].removed) return;
    change((s) => { const m = s.messages[id]; if (m.observed !== undefined && at < m.observed) return; m.observed = at; if (m.stopped === undefined) { m.stopped = at; m.uncertain = !definite; } });
  }
  function forget(id) { if (enabled && state.messages[id]) change((s) => { s.messages[id].removed = true; }); }
  function act(conversation, session, at = clock()) {
    if (enabled && session && !state.acts.some((a) => a.conversation === conversation && a.session === session && a.at === at)) change((s) => { s.acts.push({ conversation, session, at }); });
  }
  function answered(m) { return state.acts.some((a) => a.conversation === m.conversation && a.at >= m.created && (!m.targets.length || m.targets.includes(a.session))); }
  function cause(m, health, now) {
    if (m.removed || answered(m)) return null;
    if (m.stopped !== undefined) return { cause: 'not_delivered', post: !m.uncertain || now - m.stopped >= FAILURE_WAIT_MS };
    if (m.failureSince !== undefined && now - m.failureSince >= FAILURE_WAIT_MS) return { cause: 'not_delivered', post: true };
    if (m.delivered !== undefined && now - m.delivered >= FAILURE_WAIT_MS && ['no_seat', 'seat_at_cap', 'session_down'].includes(health.state)) return { cause: health.state, post: true };
    return null;
  }
  function mail(outage, cleared, now) {
    const lines = [`city: ${city}`, 'channel: whatsapp', `cause: ${outage.cause} — ${meanings[outage.cause]}`, `started: ${iso(outage.start)}`];
    if (cleared) lines.push(`ended: ${iso(outage.end)}`);
    else {
      lines.push(`conversations: ${outage.conversations}`, `messages: ${outage.messages}`, `oldest_age_seconds: ${Math.max(0, Math.floor((now - outage.oldest) / 1000))}`, `owners_told: ${outage.ownersTold > 0} (${outage.ownersTold} conversations)`);
      if (outage.cause !== 'not_delivered') lines.push(`health: ${outage.health.state}`, `checked_at: ${outage.health.checked_at}`);
      lines.push('status: gc whatsapp status --json');
    }
    return { subject: `never-silent: ${city}: whatsapp: ${outage.cause}${cleared ? ' cleared' : ''}`, body: lines.join('\n') + '\n', key: `never-silent:${outage.id}${cleared ? ':cleared' : ''}` };
  }
  let polling = false;
  async function poll() {
    if (!enabled || polling) return;
    polling = true;
    try {
      const now = clock(); const health = readHealth(healthFile, now); const groups = new Map(); const ready = new Set(); const posts = [];
      for (const m of Object.values(state.messages)) {
        const found = cause(m, health, now); if (!found) continue;
        if (!groups.has(found.cause)) groups.set(found.cause, []);
        groups.get(found.cause).push(m); if (found.post) ready.add(m.id);
      }
      for (const messages of groups.values()) messages.sort((a, b) => a.created - b.created || a.id.localeCompare(b.id));
      let newOutages = 0;
      change((s) => {
        for (const o of s.outages) if (o.end === undefined && !groups.has(o.cause)) o.end = now;
        for (const [kind, messages] of groups) {
          let outage = s.outages.find((o) => o.cause === kind && o.end === undefined);
          if (!outage) { outage = { id: randomUUID(), cause: kind, start: now, mail: {}, clearedMail: {}, localMail: {}, sample: messages[0] }; s.outages.push(outage); newOutages++; }
          Object.assign(outage, { messages: messages.length, conversations: new Set(messages.map((m) => m.conversation)).size, oldest: messages[0].created, health });
          for (const message of messages) {
            const m = s.messages[message.id]; const mark = s.owners[m.conversation];
            if (!ready.has(m.id) || m.notified) continue;
            m.notified = true;
            if (!mark || now - mark.at >= OWNER_COOLDOWN_MS) {
              const line = OWNER_LINES[kind];
              s.owners[m.conversation] = { at: now, line, message: m, cause: kind, localMail: {}, posted: false };
              posts.push({ message: m, line });
            }
          }
        }
      });
      if (!operatorContext) for (let i = 0; i < newOutages; i++) log('never-silent: operator context unset; no operator mail will be sent');
      for (const p of posts) {
        const canPost = () => {
          const current = cause(state.messages[p.message.id], readHealth(healthFile, clock()), clock());
          return Boolean(current?.post && OWNER_LINES[current.cause] === p.line);
        };
        if (!canPost()) continue;
        try { if (await post(p.message, p.line, canPost) === false) continue; }
        catch { log('never-silent: owner post refused or uncertain; reserved mark retained'); continue; }
        change((s) => { s.owners[p.message.conversation].posted = true; s.posted.push(clock()); });
      }
      change((s) => {
        for (const o of s.outages) {
          if (o.end !== undefined) continue;
          o.ownersTold = new Set((groups.get(o.cause) ?? []).filter((m) => s.owners[m.conversation]?.posted && now - s.owners[m.conversation].at < OWNER_COOLDOWN_MS).map((m) => m.conversation)).size;
        }
        s.posted = s.posted.filter((at) => now - at <= 7 * MAIL_LIFETIME_MS);
      });
      if (operatorContext) for (let i = 0; i < state.outages.length; i++) for (const cleared of [false, true]) {
        const o = state.outages[i]; const field = cleared ? 'clearedMail' : 'mail'; const mark = o[field]; const start = cleared ? o.end : o.start;
        if (start === undefined || mark.sent || now - start > MAIL_LIFETIME_MS || (mark.tried !== undefined && now - mark.tried < MAIL_RETRY_MS)) continue;
        change((s) => { s.outages[i][field].tried = now; });
        try { await send(mail(o, cleared, now)); }
        catch { log('never-silent: operator mail failed; retry in one minute'); continue; }
        change((s) => { s.outages[i][field].sent = true; });
      }
      for (let i = 0; i < state.outages.length; i++) {
        const o = state.outages[i]; const mark = o.localMail ?? {};
        if (o.cause !== 'not_delivered' || !o.sample || mark.sent || now - o.start > MAIL_LIFETIME_MS || (mark.tried !== undefined && now - mark.tried < MAIL_RETRY_MS)) continue;
        const owner = state.owners[o.sample.conversation];
        const line = owner?.posted ? owner.line : '';
        change((s) => { s.outages[i].localMail = { ...mark, tried: now }; });
        try { await sendLocal(o.sample, line); }
        catch { log('never-silent: local delivery mail failed; retry in one minute'); continue; }
        change((s) => {
          s.outages[i].localMail.sent = true;
          if (line) s.owners[o.sample.conversation].localMail.sent = true;
        });
      }
      for (const [conversation, mark] of Object.entries(state.owners)) {
        // WhatsApp gains its own-city delivery mail for cause one only.
        if (!mark.posted || mark.cause !== 'not_delivered' || mark.localMail.sent || now - mark.at > MAIL_LIFETIME_MS || (mark.localMail.tried !== undefined && now - mark.localMail.tried < MAIL_RETRY_MS)) continue;
        change((s) => { s.owners[conversation].localMail.tried = now; });
        try { await sendLocal(mark.message, mark.line); }
        catch { log('never-silent: local owner notice mail failed; retry in one minute'); continue; }
        change((s) => { s.owners[conversation].localMail.sent = true; });
      }
    } finally { polling = false; }
  }
  function status() {
    const now = clock();
    return { enabled, health: readHealth(healthFile, now), outages: state.outages.filter((o) => o.end === undefined).map((o) => ({ cause: o.cause, start: iso(o.start), conversations: o.conversations, messages: o.messages, owners_told: o.ownersTold, mail_sent: Boolean(o.mail.sent) })),
      owner_lines_seven_days: state.posted.filter((at) => now - at <= 7 * MAIL_LIFETIME_MS).length };
  }
  return { track, result, stop, forget, act, poll, status };
}
