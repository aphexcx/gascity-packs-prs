'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { command, initialState, LIMITS } = require('../reachable.cjs');
const { fixture, inbound, NOW, write } = require('../test-support/fixture.cjs');

const room = 'example/room-a';
for (const [verb, args, readOnly] of [
  ['status', [], true], ['exempt', ['--list'], true],
  ['pass', [room], false], ['exempt', [room, '--on'], false],
]) {
  test(`R2-1: outdated copy preserves v1 state on ${[verb, ...args].join(' ')}`, t => {
    const f = fixture(t);
    f.cmd('install');
    fs.appendFileSync(path.join(f.dir, 'hook.cjs'), '\n');
    const file = path.join(f.dir, 'state.json');
    const bytes = JSON.stringify({ ...initialState(), version: 1, sessions: [], known: [room] });
    fs.writeFileSync(file, bytes);
    if (readOnly) {
      const result = f.cmd(verb, args);
      if (verb === 'status') assert.equal(result.current, false);
      else assert.deepEqual(result, []);
    } else assert.throws(() => f.cmd(verb, args), /^Error: hook_copy_outdated_run_install$/);
    assert.equal(fs.readFileSync(file, 'utf8'), bytes);
  });
}

for (const [holder, skew] of [['status', -11000], ['hook', 11000]]) {
  test(`R2-2: displaced ${holder} cannot restore a passed wait`, t => {
    const f = fixture(t);
    f.append(inbound(1));
    f.cmd('status');
    const rename = fs.renameSync;
    let displaced = false;
    t.mock.method(fs, 'renameSync', (src, dest) => {
      if (!displaced && dest === path.join(f.dir, 'state.json')) {
        displaced = true;
        const tickets = fs.readdirSync(f.dir).filter(name => /^lock-/.test(name));
        assert.equal(tickets.length, 1);
        const time = new Date(Date.now() + skew);
        fs.utimesSync(path.join(f.dir, tickets[0]), time, time);
        assert.equal(f.cmd('pass', [room]).seen, true);
      }
      return rename(src, dest);
    });
    if (holder === 'status') assert.throws(() => f.cmd('status'), /^Error: state_busy$/);
    else assert.deepEqual(f.hook(write('after')), {});
    assert.equal(displaced, true);
    assert.deepEqual(f.state().waiting, []);
  });
}

function failRename(t, file) {
  const rename = fs.renameSync;
  const mock = t.mock.method(fs, 'renameSync', (src, dest) => {
    if (dest === file) {
      mock.mock.restore();
      throw new Error('fixture_rename_failed');
    }
    return rename(src, dest);
  });
}

for (const failure of ['settings.json', 'install.json']) {
  for (const retry of [true, false]) {
    test(`R2-5: moved install recovers a failed ${failure} via ${retry ? 'retry and uninstall' : 'uninstall'}`, t => {
      const f = fixture(t);
      f.cmd('install');
      const moved = fs.mkdtempSync(f.city + '-moved-');
      t.after(() => fs.rmSync(moved, { recursive: true, force: true }));
      fs.renameSync(f.city, moved);
      const options = { ...f.options, city: moved };
      const settings = path.join(moved, '.claude/settings.json');
      failRename(t, failure === 'settings.json' ? settings : path.join(moved, '.gc/reachable/install.json'));
      assert.throws(() => command('install', [], options), /fixture_rename_failed/);
      if (retry) command('install', [], options);
      command('uninstall', [], options);
      assert.equal(fs.existsSync(settings), false, 'the initially absent settings must be restored');
    });
  }
}

test('R2-5: failed first install still selects observe on retry', t => {
  const f = fixture(t);
  failRename(t, path.join(f.city, '.claude/settings.json'));
  assert.throws(() => f.cmd('install'), /fixture_rename_failed/);
  assert.equal(f.cmd('install').mode, 'observe');
});

for (const expiry of ['age', 'cap']) {
  test(`R2-6: a listed exemption can be removed after ${expiry} eviction`, t => {
    const f = fixture(t);
    f.append(inbound(1));
    f.cmd('exempt', [room, '--on']);
    let seq = 2;
    if (expiry === 'age') f.options.now += LIMITS.history;
    else {
      const rows = Array.from({ length: LIMITS.known + 1 }, (_, i) =>
        inbound(seq++, { conversation_id: 'other-' + i }, NOW - 59000));
      f.append(...rows);
    }
    assert.deepEqual(f.cmd('exempt', ['--list']), [room]);
    assert.equal(Object.hasOwn(f.state().known, room), false);
    assert.throws(() => f.cmd('pass', [room]), /unknown_conversation/);
    assert.throws(() => f.cmd('exempt', [room, '--on']), /unknown_conversation/);
    assert.equal(f.cmd('exempt', [room, '--off']).exempt, false);
    // Other conversations' overdue waits must not account for this denial.
    f.hook({ hook_event_name: 'Stop', prompt_id: 'example-turn' });
    f.append(inbound(seq, {}, f.options.now - 60000));
    assert.equal(f.hook(write('after')).hookSpecificOutput?.permissionDecision, 'deny');
    assert.equal(f.state().waiting.find(w => w.conversation_id === 'room-a')?.refusals, 1);
  });
}

for (const damage of ['missing', 'changed']) {
  test(`M9: uninstall preserves user settings with a ${damage} snapshot`, t => {
    const f = fixture(t);
    const settings = path.join(f.city, '.claude/settings.json');
    fs.mkdirSync(path.dirname(settings));
    fs.writeFileSync(settings, '{"example":true}');
    f.cmd('install');
    const snapshot = path.join(f.dir, 'settings-before-install');
    if (damage === 'missing') fs.unlinkSync(snapshot);
    else fs.appendFileSync(snapshot, ' ');
    assert.equal(f.cmd('uninstall').layout_restored, false);
    assert.deepEqual(JSON.parse(fs.readFileSync(settings)), { example: true });
    assert.equal(f.cmd('status').mode, 'off');
  });
}

test('M10: status and doctor retain diagnostics beside an unreadable record', t => {
  const f = fixture(t);
  fs.writeFileSync(path.join(f.dir, `error-${NOW - 1}-a.jsonl`), '{"code":"state_busy"}');
  fs.writeFileSync(path.join(f.dir, `error-${NOW}-b.jsonl`), '{');
  const errors = f.cmd('status').errors;
  assert.equal(errors.unreadable_error_record.count, 1);
  assert.equal(errors.state_busy.count, 1);
  assert.ok(f.cmd('doctor').includes('hook_failing_open unreadable_error_record'));
});
