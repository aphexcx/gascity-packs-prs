import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { withState, readState } from '../src/state.js';
import { consent, hours, addItem, setReminder } from '../src/model.js';
import { tick, doctor, status } from '../src/engine.js';
import { statusText } from '../src/cli.js';

const start = Date.parse('2026-11-13T17:01:15Z');
const minute = 60_000;
const day = 86_400_000;
const owner = 'slack/invented-owner';
function fixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-recovery-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return { dir, edit: fn => withState(dir, async (s, save) => { fn(s); save(); }) };
}

test('archived delivery is reconciled after a crash or an ambiguous failure', async t => {
  for (const kind of ['follow-up', 'reminder']) for (const failure of ['crash', 'nonzero']) {
    await t.test(`${kind}: ${failure}`, async t => {
      const h = fixture(t);
      await h.edit(s => {
        consent(s, owner, true, start - day);
        hours(s, owner, { clear: true, tz: 'UTC' });
        if (kind === 'follow-up') addItem(s, owner, 'Invented pending task', start - day);
        else setReminder(s, owner, 'Invented reminder', start, start - day);
      });
      const history = new Map();
      let sends = 0;
      const send = async r => { sends++; history.set(r.key, { id: 'invented-mail', status: 'closed' });
        return { ok: failure === 'crash', unknown: true, error: 'mail exit 1' }; };
      const first = tick({ dir: h.dir, now: start, send,
        afterPersist(phase) { if (failure === 'crash' && phase === 'sent') throw new Error('crash'); } });
      if (failure === 'crash') await assert.rejects(first, /crash/); else await first;
      // The inbox is empty: only the archived history proves delivery.
      for (let d = 0; d < 8; d++) await tick({ dir: h.dir,
        now: Date.parse('2026-11-16T09:00:00Z') + d * day, send,
        lookup: async r => ({ ok: true, found: history.has(r.key), id: history.get(r.key)?.id }) });
      assert.equal(sends, 1);
      assert.equal(readState(h.dir).raises[0].state, 'sent');
    });
  }
});

test('night-only hours are informational and notify the mayor once across restarts', async t => {
  const h = fixture(t);
  await h.edit(s => {
    for (const [suffix, on] of [['on', true], ['off', false], ['unset', null]]) {
      const conversation = `${owner}-${suffix}`;
      if (on !== null) consent(s, conversation, on, start);
      hours(s, conversation, { hours: 'daily 20:00-08:00', ...(on ? { tz: 'UTC' } : {}) });
    }
  });
  const mails = [];
  for (let d = 0; d < 8; d++) {
    await tick({ dir: h.dir, now: start + d * day,
      send: async r => { mails.push(structuredClone(r)); return { ok: true }; } });
    assert.deepEqual(doctor(readState(h.dir), start + d * day), []);
  }
  assert.equal(mails.length, 3);
  assert.ok(mails.every(r => r.kind === 'hours-notice' && /no follow-up/.test(r.body)));
  assert.match(statusText(status(readState(h.dir), start), readState(h.dir).owners), /no recorded hours/);
});

test('undeliverable reminders back off, stop, and remain visible to an operator', async t => {
  const h = fixture(t);
  await h.edit(s => setReminder(s, owner, 'PRIVATE INVENTED TEXT', start, start - day));
  const attempts = [];
  for (let m = 0; m <= 90; m++) await tick({ dir: h.dir, now: start + m * minute,
    send: async () => { attempts.push(m); return { ok: false, error: 'mail spawn ENOENT' }; } });
  assert.deepEqual(attempts, [0, 1, 3, 7, 15]);
  let state = readState(h.dir);
  assert.equal(state.raises[0].state, 'stopped');
  assert.match(doctor(state, start + 90 * minute).join('\n'), /delivery stopped.*operator action required/);
  const text = statusText(status(state, start + 90 * minute));
  assert.match(text, /Mail delivery stopped: 1; operator action required/);
  assert.doesNotMatch(text, /PRIVATE INVENTED TEXT/);
  await tick({ dir: h.dir, now: start + 30 * day, send: async () => { throw new Error('must not retry'); } });
  state = readState(h.dir);
  assert.equal(state.raises[0].attempts, 5);
});
