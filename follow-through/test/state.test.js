import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { withState, readState, resolveStateDir } from '../src/state.js';

export function directory(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

test('2: tick and command use the same explicit gc pack state directory', () => {
  const common = { GC_PACK_STATE_DIR: '/tmp/city/.gc/runtime/packs/follow-through' };
  assert.equal(resolveStateDir({ ...common, ORDER_DIR: '/cache/follow-through/orders' }),
    resolveStateDir({ ...common, GC_CITY_PATH: '/tmp/city' }));
  assert.throws(() => resolveStateDir({}), /GC_PACK_STATE_DIR/);
  assert.throws(() => resolveStateDir({ GC_PACK_STATE_DIR: 'relative' }), /absolute/);
});

test('2,4: private atomic file survives another process with every record intact', async (t) => {
  const dir = directory(t);
  await withState(dir, async (s, save) => {
    s.owners['slack/example'] = { consent: true, tz: 'UTC', hours: null };
    s.items.push({ id: 'item-example', summary: 'Invented item' });
    s.reminders.push({ id: 'reminder-example', text: 'Invented reminder' });
    save();
  });
  assert.equal(fs.statSync(dir).mode & 0o777, 0o700);
  assert.equal(fs.statSync(path.join(dir, 'state.json')).mode & 0o777, 0o600);
  const child = spawnSync(process.execPath, ['--input-type=module', '-e',
    `import {readState} from ${JSON.stringify(new URL('../src/state.js', import.meta.url).href)}; console.log(JSON.stringify(readState(process.argv[1])));`, dir], { encoding: 'utf8' });
  assert.equal(child.status, 0, child.stderr);
  assert.deepEqual(JSON.parse(child.stdout), readState(dir));
  assert.deepEqual(fs.readdirSync(dir), ['state.json']);
});

test('2: interrupted replacement leaves the previous valid state', async (t) => {
  const dir = directory(t);
  await withState(dir, async (s, save) => { s.last_tick_at = 123; save(); });
  fs.writeFileSync(path.join(dir, '.abandoned.tmp'), '{');
  assert.equal(readState(dir).last_tick_at, 123);
  fs.writeFileSync(path.join(dir, 'state.json'), '{');
  await assert.rejects(withState(dir, async () => {}), /state/);
});
