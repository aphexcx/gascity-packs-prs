import { randomUUID } from 'node:crypto';
import { withState } from './state.js';
import { DAY, HOUR, MINUTE, nextWake, recentWakes, wakeAllowed } from './schedule.js';
import { sendMail } from './mail.js';

export const iso = ms => ms == null ? null : new Date(ms).toISOString();

function makeRaise(state, kind, conversation, now, fields) {
  const id = randomUUID();
  const raise = { id, key: `follow-through:${id}`, kind, conversation, created_at: now,
    state: 'pending', attempts: 0, last_attempt_at: null, error: null, ...fields };
  state.raises.push(raise);
  return raise;
}

function plan(state, now) {
  for (const [conversation, owner] of Object.entries(state.owners)) {
    for (const scheduled of recentWakes(owner, now)) {
      const key = `${conversation}:${scheduled}`;
      if (state.wakes[key]) continue;
      // Mark even an empty wake so an item added later cannot retroactively
      // enter that wake. It remains open for the next scheduled wake.
      state.wakes[key] = now;
      const items = state.items.filter(i => i.conversation === conversation && i.state === 'open' && i.opened_at + 12 * HOUR <= scheduled);
      if (!items.length) continue;
      for (const item of items) { item.state = 'raised'; item.raised_at = now; }
      makeRaise(state, 'follow-up', conversation, now, {
        scheduled_at: scheduled, item_ids: items.map(i => i.id), subject: 'follow-through: follow-up due',
        body: [`Follow-up due in ${conversation}.`, ...items.map(i => `${i.id}: ${i.summary} (opened ${iso(i.opened_at)})`),
          'Read the follow-through skill before you write to the owner. Each item gets one follow-up, or none.'].join('\n'),
      });
    }
  }
  for (const reminder of state.reminders) {
    if (reminder.state !== 'set' || reminder.due_at > now) continue;
    reminder.state = 'raised';
    reminder.raised_at = now;
    const late = Math.floor((now - reminder.due_at) / MINUTE);
    makeRaise(state, 'reminder', reminder.conversation, now, {
      reminder_id: reminder.id, subject: 'follow-through: reminder due',
      body: [`Reminder due in ${reminder.conversation}.`, `${reminder.id}: ${reminder.text}`,
        `Set ${iso(reminder.set_at)} for ${iso(reminder.due_at)}.`,
        ...(now - reminder.due_at > 5 * MINUTE ? [`This reminder is ${late} minutes late.`] : []),
        'Read the follow-through skill before you write to the owner.'].join('\n'),
    });
  }
}

// afterPersist is a test-only dependency injection point: tests kill a real
// child here. All production clocks and senders default to the real runtime.
export async function tick({ dir, now, clock = now === undefined ? Date.now : () => now, send = sendMail, afterPersist = () => {} }) {
  return withState(dir, async (state, save) => {
    const started = clock();
    plan(state, started);
    state.last_tick_at = started;
    save();
    await afterPersist('planned');
    for (const raise of state.raises) {
      if (raise.state !== 'pending') continue;
      const attemptAt = clock();
      if (attemptAt - raise.created_at >= DAY) { raise.state = 'failed'; save(); continue; }
      if (raise.last_attempt_at !== null && attemptAt - raise.last_attempt_at < MINUTE) continue;
      // Retries remain subject to consent, hours, grace and the evening cap.
      if (raise.kind === 'follow-up' && !wakeAllowed(state.owners[raise.conversation], raise.scheduled_at, attemptAt)) continue;
      raise.last_attempt_at = attemptAt;
      raise.attempts++;
      save();
      await afterPersist('attempt', raise);
      let result;
      try { result = await send(raise); }
      catch { result = { ok: false, error: 'mail command failed' }; }
      await afterPersist('sent', raise);
      if (result.ok) { raise.state = 'sent'; raise.sent_at = clock(); raise.error = null; }
      else raise.error = result.error || 'mail command failed';
      save();
    }
  });
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
    failing_raises: state.raises.filter(r => r.state === 'failed' || r.state === 'pending' && r.attempts > 0)
      .map(r => ({ id: r.id, kind: r.kind, conversation: r.conversation, state: r.state, attempts: r.attempts,
        last_attempt_at: iso(r.last_attempt_at), expires_at: iso(r.created_at + DAY), error: r.error })),
  };
}

export function doctor(state, now = Date.now()) {
  const problems = [];
  const active = Object.values(state.owners).some(o => o.consent === true) || state.reminders.some(r => r.state === 'set');
  if (active && (state.last_tick_at === null || now - state.last_tick_at > 5 * MINUTE)) problems.push('tick has not run in the last five minutes');
  for (const [conversation, owner] of Object.entries(state.owners)) {
    if (owner.consent === true && !owner.tz) problems.push(`consenting owner has no zone: ${conversation}`);
  }
  for (const raise of state.raises) if (raise.state === 'failed') problems.push(`raise failed: ${raise.id}`);
  return problems;
}
