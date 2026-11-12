import { randomUUID } from 'node:crypto';
import { withState } from './state.js';
import { HOUR, MINUTE, hasDaytimeHours, nextWake, recentWakes } from './schedule.js';
import { wallClock } from './zoned.js';
import { assertMailBody, lookupMail, MAIL_TIMEOUT_MS, MAX_MAIL_BODY_BYTES, sendMail } from './mail.js';

export const iso = ms => ms == null ? null : new Date(ms).toISOString();
export const MAX_WAKE_ITEMS = 25;
export const TICK_BUDGET_MS = 90_000;
export const MAX_DELIVERY_ATTEMPTS = 5;

function mailTime(ms, tz) {
  const utc = `${iso(ms)} (UTC)`;
  if (!tz) return utc;
  const wall = wallClock(ms, tz);
  const pad = n => String(n).padStart(2, '0');
  return `${wall.year}-${pad(wall.month)}-${pad(wall.day)} ${pad(wall.hour)}:${pad(wall.minute)}:${pad(wall.second)} (${tz}); ${utc}`;
}

const mailTail = "The mayor decides whether and how to write, by the channel's rules.";

export function followUpBody(conversation, wakeId, items, maxBytes = MAX_MAIL_BODY_BYTES, tz = null) {
  const prefix = [`Follow-up due in ${conversation}.`, `Wake ${wakeId}.`];
  const tail = `Read the follow-through skill. Each item gets one follow-up, or none. ${mailTail}`;
  const lines = items.slice(0, MAX_WAKE_ITEMS).map(i => `${i.id}: ${i.summary} (opened ${mailTime(i.opened_at, tz)})`);
  const assemble = shown => [...prefix, ...shown,
    ...(items.length > shown.length ? [`${items.length - shown.length} more are due. See them all: gc follow-through list --wake ${wakeId}`] : []), tail].join('\n');
  while (lines.length > 1 && Buffer.byteLength(assemble(lines)) > maxBytes) lines.pop();
  if (Buffer.byteLength(assemble(lines)) > maxBytes) {
    const available = maxBytes - Buffer.byteLength(assemble([''])) - Buffer.byteLength('…');
    if (available < Buffer.byteLength(items[0].id)) throw new Error('mail body cannot fit the item ID');
    let shortened = '';
    let size = 0;
    for (const char of lines[0]) {
      const bytes = Buffer.byteLength(char);
      if (size + bytes > available) break;
      shortened += char;
      size += bytes;
    }
    lines[0] = shortened + '…';
  }
  return assertMailBody(assemble(lines));
}

function makeRaise(state, kind, conversation, now, fields) {
  const id = randomUUID();
  const raise = { id, key: `follow-through:${id}`, kind, conversation, created_at: now,
    state: 'pending', attempts: 0, last_attempt_at: null, error: null, ...fields };
  state.raises.push(raise);
  return raise;
}

function plan(state, now) {
  const reservedItems = new Set(state.raises.filter(r => ['pending', 'failed', 'stopped'].includes(r.state)).flatMap(r => r.item_ids || []));
  for (const [conversation, owner] of Object.entries(state.owners)) {
    if (!hasDaytimeHours(owner) && !state.raises.some(r => r.kind === 'hours-notice' && r.conversation === conversation)) {
      makeRaise(state, 'hours-notice', conversation, now, {
        subject: 'follow-through: no daytime hours',
        body: `Owner ${conversation} has no recorded hours between 08:00 and 20:00; these hours allow no follow-up. ` +
          `Requested reminders are unaffected. ${mailTail}`,
      });
    }
    for (const scheduled of recentWakes(owner, now)) {
      const key = `${conversation}:${scheduled}`;
      if (state.wakes[key]) continue;
      // Mark even an empty wake so an item added later cannot retroactively
      // enter that wake. It remains open for the next scheduled wake.
      state.wakes[key] = now;
      const items = state.items.filter(i => i.conversation === conversation && i.state === 'open' && !reservedItems.has(i.id) && i.opened_at + 12 * HOUR <= scheduled)
        .sort((a, b) => a.opened_at - b.opened_at);
      if (!items.length) continue;
      const wakeId = `wake-${randomUUID()}`;
      const body = followUpBody(conversation, wakeId, items, MAX_MAIL_BODY_BYTES, owner.tz);
      for (const item of items) { item.wake_id = wakeId; reservedItems.add(item.id); }
      makeRaise(state, 'follow-up', conversation, now, {
        scheduled_at: scheduled, wake_id: wakeId, item_ids: items.map(i => i.id), subject: 'follow-through: follow-up due', body,
      });
    }
  }
  for (const reminder of state.reminders) {
    if (reminder.state !== 'set' || reminder.due_at > now || state.raises.some(r => r.reminder_id === reminder.id)) continue;
    makeRaise(state, 'reminder', reminder.conversation, now, {
      reminder_id: reminder.id, subject: 'follow-through: reminder due',
      body: reminderBody(reminder, state.owners[reminder.conversation]?.tz, now),
    });
  }
}

function reminderBody(reminder, tz, now) {
  return assertMailBody([`The owner asked for this reminder in ${reminder.conversation}.`, `${reminder.id}: ${reminder.text}`,
    `Set ${mailTime(reminder.set_at, tz)} for ${mailTime(reminder.due_at, tz)}.`,
    ...(now - reminder.due_at > 5 * MINUTE ? [`This reminder is ${Math.floor((now - reminder.due_at) / MINUTE)} minutes late.`] : []),
    `Read the follow-through skill. ${mailTail}`].join('\n'));
}

function outstanding(state, raise) {
  if (!['pending', 'failed', 'stopped'].includes(raise.state)) return false;
  if (raise.kind === 'hours-notice') return !!state.owners[raise.conversation] && !hasDaytimeHours(state.owners[raise.conversation]);
  return raise.kind === 'follow-up'
    ? state.items.some(i => raise.item_ids.includes(i.id) && !['done', 'dropped'].includes(i.state))
    : state.reminders.some(r => r.id === raise.reminder_id && r.state !== 'cancelled');
}

// A saved plan reserves its records and key, but delivery alone spends them.
// Normalize records written by earlier versions without changing their keys.
function recover(state) {
  for (const raise of state.raises) {
    if (!outstanding(state, raise) || raise.state === 'stopped') continue;
    // Older records did not distinguish a failed command from a crash after
    // delivery. Reconcile their attempted sends before allowing another one.
    if (raise.delivery_unknown === undefined) raise.delivery_unknown = raise.attempts > 0;
    raise.state = 'pending';
    for (const item of state.items.filter(i => raise.item_ids?.includes(i.id) && i.state === 'raised')) {
      item.state = 'open'; delete item.raised_at;
    }
    const reminder = state.reminders.find(r => r.id === raise.reminder_id && r.state === 'raised');
    if (reminder) { reminder.state = 'set'; delete reminder.raised_at; }
  }
}

function delivered(state, raise, now, messageId) {
  raise.state = 'sent'; raise.sent_at = now; raise.error = null; raise.delivery_unknown = false;
  if (messageId) raise.message_id = messageId;
  for (const item of state.items.filter(i => raise.item_ids?.includes(i.id) && i.state === 'open')) {
    item.state = 'raised'; item.raised_at = now;
  }
  const reminder = state.reminders.find(r => r.id === raise.reminder_id);
  if (reminder) { reminder.state = 'raised'; reminder.raised_at = now; }
}

function failed(raise, error) {
  raise.error = error || 'mail command failed';
  if (raise.attempts >= MAX_DELIVERY_ATTEMPTS) raise.state = 'stopped';
}

// afterPersist is a test-only dependency injection point: tests kill a real
// child here. All production clocks and senders default to the real runtime.
export async function tick({ dir, now, clock = now === undefined ? Date.now : () => now, send = sendMail, lookup = lookupMail, afterPersist = () => {},
  monotonic = () => performance.now(), budgetMs = TICK_BUDGET_MS }) {
  const deadline = monotonic() + budgetMs;
  return withState(dir, async (state, save) => {
    const started = clock();
    recover(state);
    plan(state, started);
    state.last_tick_at = started;
    save();
    await afterPersist('planned');
    // New raises go first; otherwise oldest attempted first. Persisting the
    // attempt before spawning gc also rotates the queue after a killed tick.
    const pending = state.raises.filter(r => r.state !== 'stopped' && outstanding(state, r))
      .sort((a, b) => (a.last_attempt_at ?? -Infinity) - (b.last_attempt_at ?? -Infinity));
    for (const raise of pending) {
      const attemptAt = clock();
      const wait = MINUTE * 2 ** Math.min(Math.max(raise.attempts - 1, 0), MAX_DELIVERY_ATTEMPTS - 1);
      if (raise.last_attempt_at !== null && attemptAt - raise.last_attempt_at < wait) continue;
      // Re-evaluate this owner's current wake, consent, zone and hours. An
      // expired original window does not consume an undelivered follow-up.
      if (raise.kind === 'follow-up' && !recentWakes(state.owners[raise.conversation], attemptAt).length) continue;
      // Leave a full send timeout plus time to persist and release the lock.
      if (deadline - monotonic() < MAIL_TIMEOUT_MS * (raise.delivery_unknown ? 2 : 1) + 1000) break;
      if (raise.attempts >= MAX_DELIVERY_ATTEMPTS && !raise.delivery_unknown) {
        failed(raise, raise.error); save(); continue;
      }
      const previousAttempt = { attempts: raise.attempts, last_attempt_at: raise.last_attempt_at };
      raise.last_attempt_at = attemptAt;
      const exhausted = raise.attempts >= MAX_DELIVERY_ATTEMPTS;
      // One last history lookup may confirm a crash during the final send;
      // it must never grant a sixth send.
      if (raise.attempts < MAX_DELIVERY_ATTEMPTS) raise.attempts++;
      save();
      if (raise.delivery_unknown) {
        let receipt;
        try { receipt = await lookup(raise); }
        catch { receipt = { ok: false, error: 'mail history unavailable' }; }
        if (receipt.ok && receipt.found) {
          delivered(state, raise, clock(), receipt.id); save(); continue;
        }
        if (!receipt.ok || exhausted) {
          failed(raise, receipt.error || 'mail delivery unconfirmed'); save(); continue;
        }
        raise.delivery_unknown = false;
      }
      // A lookup may take time or cross the end of an allowed wake.
      if (raise.kind === 'follow-up' && !recentWakes(state.owners[raise.conversation], clock()).length) {
        // Preserve the retry budget and backoff when no send can follow.
        // Keep a completed empty lookup reconciled so it is not repeated.
        Object.assign(raise, previousAttempt);
        save(); continue;
      }
      if (raise.kind === 'follow-up') {
        const items = state.items.filter(i => raise.item_ids.includes(i.id) && !['done', 'dropped'].includes(i.state))
          .sort((a, b) => a.opened_at - b.opened_at);
        raise.body = followUpBody(raise.conversation, raise.wake_id, items, MAX_MAIL_BODY_BYTES, state.owners[raise.conversation]?.tz);
      } else if (raise.kind === 'reminder') {
        raise.body = reminderBody(state.reminders.find(r => r.id === raise.reminder_id), state.owners[raise.conversation]?.tz, attemptAt);
      }
      raise.delivery_unknown = true;
      save();
      await afterPersist('attempt', raise);
      let result;
      try { result = await send(raise); }
      catch { result = { ok: false, unknown: true, error: 'mail command failed' }; }
      await afterPersist('sent', raise);
      if (result.ok) {
        delivered(state, raise, clock());
      }
      else {
        raise.delivery_unknown = result.unknown === true;
        failed(raise, result.error);
      }
      save();
    }
  }, { lockTimeoutMs: budgetMs });
}

export function status(state, now = Date.now()) {
  const owners = Object.entries(state.owners);
  return {
    owners: owners.length,
    consent_on: owners.filter(([, o]) => o.consent === true).length,
    consent_on_without_zone: owners.filter(([, o]) => o.consent === true && !o.tz).length,
    owners_without_zone: owners.filter(([, o]) => !o.tz).map(([c]) => c).sort(),
    items: { open: state.items.filter(i => i.state === 'open').length,
      raised: state.items.filter(i => i.state === 'raised').length,
      closed: state.items.filter(i => ['done', 'dropped'].includes(i.state)).length },
    reminders_set: state.reminders.filter(r => r.state === 'set').length,
    next_wakes: owners.map(([conversation, o]) => ({ conversation, at: iso(nextWake(o, now)) })),
    last_tick_at: iso(state.last_tick_at),
    failing_raises: state.raises.filter(r => outstanding(state, r))
      .map(r => ({ id: r.id, kind: r.kind, conversation: r.conversation, state: r.state, attempts: r.attempts,
        last_attempt_at: iso(r.last_attempt_at), expires_at: null, error: r.error })),
  };
}

export function doctor(state, now = Date.now()) {
  const problems = [];
  const active = Object.values(state.owners).some(o => o.consent === true && hasDaytimeHours(o)) || state.reminders.some(r => r.state === 'set');
  if (active && (state.last_tick_at === null || now - state.last_tick_at > 5 * MINUTE)) problems.push('tick has not run in the last five minutes');
  for (const [conversation, owner] of Object.entries(state.owners)) {
    if (owner.consent === true && !owner.tz && hasDaytimeHours(owner)) problems.push(`consenting owner has no zone: ${conversation}`);
  }
  for (const raise of state.raises) if (outstanding(state, raise)) problems.push(raise.state === 'stopped'
    ? `mail delivery stopped: ${raise.id}; operator action required (inspect local mayor mail and delivery history)`
    : `raise undelivered: ${raise.id}`);
  return problems;
}

export function hoursNotices(state) {
  return Object.entries(state.owners).filter(([, owner]) => !hasDaytimeHours(owner))
    .map(([conversation]) => `owner has no recorded hours between 08:00 and 20:00; no follow-up: ${conversation}`);
}
