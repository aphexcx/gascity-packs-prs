import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const cli = fileURLToPath(new URL('../src/cli.js', import.meta.url));

test('1,5,7,15,18,19: command lifecycle, file input, private status and doctor exit', t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'follow-through-cli-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const run = (...args) => spawnSync(process.execPath, [cli, ...args], {
    env: { ...process.env, GC_PACK_STATE_DIR: path.join(dir, 'state') }, encoding: 'utf8',
  });
  const ok = (...args) => { const r = run(...args); assert.equal(r.status, 0, r.stderr); return r.stdout.trim(); };
  assert.match(ok('--help'), /follow-through/);
  ok('consent', 'slack/one', '--on');
  assert.equal(run('doctor').status, 1);
  ok('hours', 'slack/one', '--hours', 'Mon-Fri 09:00-18:00', '--tz', 'UTC');
  ok('hours', 'slack/one', '--clear', '--tz', 'Asia/Tokyo');
  ok('hours', 'slack/one', '--clear');
  const badZone = run('hours', 'slack/one', '--tz', 'Mars/Crater');
  assert.equal(badZone.status, 1);
  assert.equal(badZone.stderr.trim().split('\n').length, 1);
  assert.match(badZone.stderr, /Mars\/Crater/);
  const summaryFile = path.join(dir, 'summary.txt');
  fs.writeFileSync(summaryFile, 'SECRET FILE SUMMARY\n');
  const first = ok('add', '--conversation', 'slack/one', '--summary-file', summaryFile);
  const second = ok('add', '--conversation', 'slack/two', '--summary', 'SECRET INLINE SUMMARY');
  const all = JSON.parse(ok('list', '--json'));
  assert.equal(all.length, 2);
  assert.equal(JSON.parse(ok('list', '--conversation', 'slack/one', '--json')).length, 1);
  assert.match(ok('list'), /SECRET FILE SUMMARY/);
  ok('done', first);
  ok('drop', second);
  assert.equal(JSON.parse(ok('list', '--json')).length, 0);
  assert.equal(JSON.parse(ok('list', '--all', '--json')).length, 2);
  assert.equal(run('done', 'missing').status, 1);
  assert.equal(run('add', '--conversation', 'slack/one', '--summary', 'x'.repeat(201)).status, 1);
  const textFile = path.join(dir, 'text.txt');
  fs.writeFileSync(textFile, 'SECRET REMINDER');
  const one = ok('remind', '--conversation', 'slack/one', '--in', '2h', '--text-file', textFile);
  assert.match(one, /Asia\/Tokyo/);
  const future = new Date(Date.now() + 86400000).toISOString();
  ok('remind', '--conversation', 'slack/two', '--at', future, '--text', 'SECRET INLINE REMINDER');
  assert.equal(JSON.parse(ok('remind', '--list', '--json')).length, 2);
  ok('remind', '--cancel', one.split(' ')[0]);
  assert.match(ok('remind', '--list'), /cancelled/);
  for (const format of [[], ['--json']]) assert.doesNotMatch(ok('status', ...format), /SECRET/);
  const snapshot = JSON.parse(ok('status', '--json'));
  assert.equal(snapshot.items.closed, 2);
  assert.equal(snapshot.reminders_set, 1);
  ok('consent', 'slack/one', '--off');
  for (const args of [
    ['consent', 'slack/one', '--on', '--off'], ['hours', 'slack/one', '--clear', '--hours', 'daily 09:00-18:00'],
    ['remind', '--list', '--cancel', 'missing'], ['remind', '--in', '1h', '--at', future],
    ['add', '--conversation', 'slack/one', '--summary', 'a', '--summary-file', summaryFile],
    ['status', '--surprise'], ['status', 'unwanted'], ['drop'],
  ]) assert.equal(run(...args).status, 1, args.join(' '));
});
