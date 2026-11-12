import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createStopwatch, releaseId } from '../src/stopwatch.js';
import { createPublisher } from '../src/outbound.js';

function temp(t) { const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'stopwatch-test-')); t.after(() => fs.rmSync(dir, { recursive: true, force: true })); return dir; }
function events(dir) { return fs.readdirSync(dir).filter((f) => /^stopwatch-.*\.jsonl$/.test(f)).flatMap((f) => fs.readFileSync(path.join(dir, f), 'utf8').trim().split('\n').filter(Boolean).map((s) => JSON.parse(s))); }

test('monthly journal is private, deduplicates after restart and keeps first reaction', (t) => {
  const stateDir = temp(t); let watch = createStopwatch({ stateDir, release: 'release-example' });
  const inbound = { event: 'inbound', conversation: 'owner-example', message: 'message-example', at: '2026-01-31T23:59:59Z', message_at: '2026-01-31T23:59:58Z' };
  watch.record(inbound); watch.record(inbound);
  const reaction = { event: 'reaction', conversation: inbound.conversation, message: inbound.message, actor: 'adapter', choice: 'fixed', at: '2026-02-01T00:00:00Z' };
  watch.record(reaction); watch = createStopwatch({ stateDir, release: 'release-example' }); watch.record(inbound); watch.record(reaction);
  assert.equal(events(stateDir).length, 2);
  for (const f of fs.readdirSync(stateDir)) assert.equal(fs.statSync(path.join(stateDir, f)).mode & 0o777, 0o600);
});

test('release follows source bytes in fixed order and a failed journal warns once', (t) => {
  const stateDir = temp(t); fs.writeFileSync(path.join(stateDir, 'a.js'), 'one'); fs.writeFileSync(path.join(stateDir, 'b.js'), 'two');
  const first = releaseId(stateDir); assert.equal(first, releaseId(stateDir)); assert.equal(first.length, 12);
  fs.writeFileSync(path.join(stateDir, 'a.js'), 'changed'); assert.notEqual(first, releaseId(stateDir));
  const logs = []; const watch = createStopwatch({ stateDir: path.join(stateDir, 'a.js'), release: 'example', log: (s) => logs.push(s) });
  watch.record({ event: 'words', at: new Date().toISOString() }); watch.record({ event: 'words', at: new Date().toISOString() }); assert.equal(logs.length, 1);
});

for (const lost of [false, true]) for (const restart of [false, true]) test(`confirmed words once: lost=${lost} restart=${restart}`, async (t) => {
  const stateDir = temp(t); let now = Date.parse('2026-01-01T00:00:00Z'); let attempts = 0;
  let watch = createStopwatch({ stateDir, release: 'release-example', clock: () => now });
  const gateway = { window: async () => ({ windows: [{ conversation_id: 'owner-example', user_id: 'user-example' }] }), send: async () => {
    attempts++; if (lost && attempts === 1) throw Object.assign(new Error('lost response'), { noAnswer: true }); return { message_id: 'returned-message-example' };
  } };
  const make = () => createPublisher({ stateDir, gateway, city: 'example', clock: () => now, stopwatch: watch });
  let publisher = make(); const request = JSON.stringify({ session_id: 'session-example', conversation: { conversation_id: 'owner-example' }, text: 'synthetic', idempotency_key: 'send-example' });
  await publisher.handlePublish(request);
  if (restart) { watch = createStopwatch({ stateDir, release: 'release-example', clock: () => now }); publisher = make(); }
  now += 1000; await publisher.handlePublish(request); now += 1000; await publisher.handlePublish(request);
  const words = events(stateDir).filter((e) => e.event === 'words'); assert.equal(words.length, 1); assert.equal(words[0].acceptance_uncertain, lost); assert.equal(words[0].at, new Date(Date.parse('2026-01-01T00:00:00Z') + (lost ? 1000 : 0)).toISOString());
  watch.words('owner-example', 'returned-message-example', true); assert.equal(events(stateDir).filter((e) => e.event === 'words').length, 1);
});
