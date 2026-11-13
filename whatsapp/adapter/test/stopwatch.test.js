import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { createStopwatch, releaseId } from '../src/stopwatch.js';
import { createPublisher } from '../src/outbound.js';

function temp(t) { const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'stopwatch-test-')); t.after(() => fs.rmSync(dir, { recursive: true, force: true })); return dir; }
function events(dir) { return fs.readdirSync(dir).filter((f) => /^stopwatch-.*\.jsonl$/.test(f)).flatMap((f) => fs.readFileSync(path.join(dir, f), 'utf8').trim().split('\n').filter(Boolean).map((s) => JSON.parse(s))); }

test('monthly journal is private, deduplicates after restart and keeps first reaction', async (t) => {
  const stateDir = temp(t); let watch = createStopwatch({ stateDir, release: 'release-example' });
  const inbound = { event: 'inbound', conversation: 'owner-example', message: 'message-example', at: '2026-01-31T23:59:59Z', message_at: '2026-01-31T23:59:58Z' };
  watch.record(inbound); watch.record(inbound);
  const reaction = { event: 'reaction', conversation: inbound.conversation, message: inbound.message, actor: 'adapter', choice: 'fixed', at: '2026-02-01T00:00:00Z' };
  watch.record(reaction); await watch.flush(); watch = createStopwatch({ stateDir, release: 'release-example' }); watch.record(inbound); watch.record(reaction);
  await watch.flush();
  assert.equal(events(stateDir).length, 2);
  for (const f of fs.readdirSync(stateDir)) assert.equal(fs.statSync(path.join(stateDir, f)).mode & 0o777, 0o600);
});

test('release follows source bytes in fixed order and a failed journal warns once', async (t) => {
  const stateDir = temp(t); fs.writeFileSync(path.join(stateDir, 'a.js'), 'one'); fs.writeFileSync(path.join(stateDir, 'b.js'), 'two');
  const first = releaseId(stateDir); assert.equal(first, releaseId(stateDir)); assert.equal(first.length, 12);
  fs.writeFileSync(path.join(stateDir, 'a.js'), 'changed'); assert.notEqual(first, releaseId(stateDir));
  const logs = []; const watch = createStopwatch({ stateDir: path.join(stateDir, 'a.js'), release: 'example', log: (s) => logs.push(s) });
  watch.record({ event: 'words', at: new Date().toISOString() }); watch.record({ event: 'words', at: new Date().toISOString() }); await watch.flush(); assert.equal(logs.length, 1);
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
  if (restart) { await watch.flush(); watch = createStopwatch({ stateDir, release: 'release-example', clock: () => now }); publisher = make(); }
  now += 1000; await publisher.handlePublish(request); now += 1000; await publisher.handlePublish(request);
  await watch.flush();
  const words = events(stateDir).filter((e) => e.event === 'words'); assert.equal(words.length, 1); assert.equal(words[0].acceptance_uncertain, lost); assert.equal(words[0].at, new Date(Date.parse('2026-01-01T00:00:00Z') + (lost ? 1000 : 0)).toISOString());
  watch.words('owner-example', 'returned-message-example', true); await watch.flush(); assert.equal(events(stateDir).filter((e) => e.event === 'words').length, 1);
});

// --- PR 104 round 2 -------------------------------------------------------------

const DAY_MS = 24 * 60 * 60 * 1000;
const WARNING = 'stopwatch: release unavailable; recorded as unknown';

test('release unknown when the source cannot be read; one warning; the adapter starts', async (t) => {
  // The real adapter process, from a copy of src/ holding one source file it
  // cannot read: the release cannot be hashed.
  const fakes = await import('./fakes.js');
  const { spawn } = await import('node:child_process');
  // The pack's layout: src/ reads ../../media-types.json at load.
  const pack = temp(t); const copy = path.join(pack, 'adapter');
  for (const name of ['media-types.json', 'reactions.json']) fs.copyFileSync(path.join(fakes.ADAPTER_DIR, '..', name), path.join(pack, name));
  fs.cpSync(path.join(fakes.ADAPTER_DIR, 'src'), path.join(copy, 'src'), { recursive: true });
  fs.copyFileSync(path.join(fakes.ADAPTER_DIR, 'package.json'), path.join(copy, 'package.json'));
  const unreadable = path.join(copy, 'src', 'zz-unreadable.js');
  fs.writeFileSync(unreadable, '// not imported\n'); fs.chmodSync(unreadable, 0o000);
  const dir = fakes.tempDir(); t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const clock = fakes.createTestClock(dir);
  const gw = await fakes.startFakeGateway({ clock }); t.after(() => gw.close());
  const gc = await fakes.startFakeGc({ clock }); t.after(() => gc.close());
  const child = spawn(process.execPath, [path.join(copy, 'src', 'index.js')], { env: fakes.adapterEnv({ dir, clock, gw, gc }), stdio: ['ignore', 'pipe', 'pipe'] });
  const out = []; child.stdout.on('data', (d) => out.push(d.toString())); child.stderr.on('data', (d) => out.push(d.toString()));
  let exited = null; child.on('close', (code, signal) => { exited = { code, signal }; });
  t.after(() => { if (child.exitCode === null && child.signalCode === null) child.kill('SIGKILL'); });
  const id = gw.addText('owner-example', 'message-example-release', 'synthetic');
  await fakes.waitFor(() => exited || gw.acked.has(id), { what: 'the delivery or an exit' });
  assert.equal(exited, null, `the adapter exited at start (exit code ${exited?.code})`);
  const stateDir = path.join(dir, '.gc', 'whatsapp');
  const inbound = await fakes.waitFor(() => events(stateDir).find((e) => e.event === 'inbound'), { what: 'the inbound line' });
  assert.equal(inbound.release, 'unknown');
  assert.equal(out.join('').split('\n').filter((l) => l.endsWith(WARNING)).length, 1, 'one warning line');

  // The same through the constructor, with a release root that cannot be listed.
  const logs = []; const unit = temp(t);
  let watch;
  assert.doesNotThrow(() => { watch = createStopwatch({ stateDir: unit, releaseRoot: path.join(unit, 'missing'), log: (s) => logs.push(s) }); });
  watch.record({ event: 'inbound', conversation: 'owner-example', message: 'message-example', at: '2026-01-01T00:00:01Z', message_at: '2026-01-01T00:00:00Z' });
  await watch.flush?.();
  assert.deepEqual(logs, [WARNING]);
  assert.equal(events(unit)[0].release, 'unknown');
});

test('words markers older than 31 days are pruned at start; nothing else is touched', async (t) => {
  const stateDir = temp(t); const dir = path.join(stateDir, 'words-attempts'); fs.mkdirSync(dir, { recursive: true });
  const now = Date.now();
  const make = (where, name, ageMs, body = '') => { const file = path.join(where, name); fs.writeFileSync(file, body); const at = new Date(now - ageMs); fs.utimesSync(file, at, at); };
  make(dir, 'a'.repeat(64), 32 * DAY_MS); // old, empty, ours: removed
  make(dir, 'b'.repeat(64), 30 * DAY_MS); // inside a file send's retention: kept
  make(dir, 'c'.repeat(64), 40 * DAY_MS, 'not empty'); // not an empty marker: kept
  make(dir, 'stray-file', 40 * DAY_MS); // another name: kept
  make(stateDir, 'stopwatch-2020-01.jsonl', 400 * DAY_MS, '{}\n'); // a stopwatch file is never removed
  const watch = createStopwatch({ stateDir, release: 'release-example' });
  await watch.flush?.();
  assert.deepEqual(fs.readdirSync(dir).sort(), ['b'.repeat(64), 'c'.repeat(64), 'stray-file']);
  assert.ok(fs.existsSync(path.join(stateDir, 'stopwatch-2020-01.jsonl')));
});
