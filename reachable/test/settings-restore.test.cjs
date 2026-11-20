'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createHash } = require('node:crypto');
const { fixture } = require('../test-support/fixture.cjs');
const value = { permissions: { allow: ['Read'] } };
const cases = [
  ['two spaces', JSON.stringify(value, null, 2) + '\n', 0o600],
  ['four spaces', JSON.stringify(value, null, 4) + '\n', 0o600],
  ['compact', JSON.stringify(value), 0o600],
  ['mode 0644', JSON.stringify(value, null, 2) + '\n', 0o644],
  ['no file', null, null],
  ['no directory', null, null],
];
for (const [name, original, mode] of cases) {
  test(`R7: uninstall restores the settings bytes and mode, ${name}`, t => {
    const f = fixture(t);
    const dir = path.join(f.city, '.claude'), file = path.join(dir, 'settings.json');
    if (name !== 'no directory') fs.mkdirSync(dir);
    if (original !== null) {
      fs.writeFileSync(file, original);
      fs.chmodSync(file, mode);
    }
    const digest = () => fs.existsSync(file) ? createHash('sha256').update(fs.readFileSync(file)).digest('hex') : 'absent';
    const stat = () => fs.existsSync(file) ? (fs.statSync(file).mode & 0o777).toString(8) : 'absent';
    const before = `${digest()} ${stat()}`;
    f.cmd('install'); f.cmd('install'); f.cmd('uninstall');
    t.diagnostic(`${name}: ${before} -> ${digest()} ${stat()}`);
    assert.equal(fs.existsSync(file), original !== null);
    if (original !== null) {
      assert.deepEqual(fs.readFileSync(file), Buffer.from(original));
      assert.equal(fs.statSync(file).mode & 0o777, mode);
    }
    assert.equal(fs.existsSync(dir), name !== 'no directory');
    assert.equal(fs.existsSync(path.join(f.dir, 'settings-before-install')), false);
  });
}
test('R7: uninstall keeps edits and the current mode when it restores the JSON value', t => {
  const f = fixture(t);
  f.cmd('install');
  const file = path.join(f.city, '.claude/settings.json');
  const changed = JSON.parse(fs.readFileSync(file)); changed.custom = true;
  fs.writeFileSync(file, JSON.stringify(changed)); fs.chmodSync(file, 0o640);
  const result = f.cmd('uninstall');
  assert.equal(result.layout_restored, false);
  assert.deepEqual(JSON.parse(fs.readFileSync(file)), { custom: true });
  assert.equal(fs.statSync(file).mode & 0o777, 0o640);
});
