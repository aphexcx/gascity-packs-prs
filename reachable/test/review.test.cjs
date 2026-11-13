'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const zlib = require('node:zlib');
const { performance } = require('node:perf_hooks');
const { spawnSync } = require('node:child_process');
const { readEvents, command, runHook, install, doctor, hash, aliasesFromImports } = require('../reachable.cjs');
const NOW = Date.parse('2026-01-02T12:00:00Z');
const row = (seq, type = 'extmsg.inbound', extra = {}) => ({ seq, type, ts: new Date(NOW - 60000).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', target_agent: 'mayor', ...extra } });
const line = r => JSON.stringify(r) + '\n';
const input = id => ({ hook_event_name: 'PreToolUse', tool_name: 'Bash', tool_use_id: id, tool_input: { command: 'make test' } });
function fixture(t) {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-review-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  const dir = path.join(city, '.gc'); fs.mkdirSync(dir);
  const log = path.join(dir, 'events.jsonl'); fs.writeFileSync(log, line(row(1)));
  const stateDir = path.join(dir, 'reachable');
  const o = { city, now: NOW, imports: '', env: { GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example' } };
  return { city, dir, log, stateDir, o };
}
test('review 1: an empty replacement cannot hide a subsequently published archive', t => {
  const f = fixture(t); const first = readEvents(f.city, null);
  fs.appendFileSync(f.log, line(row(2))); const rotating = f.log + '.rotating-20260102T120000Z';
  fs.renameSync(f.log, rotating); fs.writeFileSync(f.log, '');
  const gap = readEvents(f.city, first.cursor);
  fs.writeFileSync(f.log + '.archive-20260102T120000Z-seq-1-2.gz', zlib.gzipSync(fs.readFileSync(rotating)));
  fs.appendFileSync(f.log, line(row(3)));
  assert.deepEqual(readEvents(f.city, gap.cursor).rows.map(r => r.seq), [2, 3]);
});
test('review 1: fresh cursors wait for archives still being published', t => {
  const f = fixture(t); fs.appendFileSync(f.log, line(row(2)));
  const rotating = f.log + '.rotating-20260102T120000Z-seq-1-2';
  fs.renameSync(f.log, rotating); fs.writeFileSync(f.log, line(row(3, 'example.anchor')));
  const first = readEvents(f.city, null, { now: NOW });
  fs.writeFileSync(f.log + '.archive-20260102T120000Z-seq-1-2.gz', zlib.gzipSync(fs.readFileSync(rotating)));
  fs.unlinkSync(rotating);
  const second = readEvents(f.city, first.cursor, { now: NOW });
  assert.deepEqual([...first.rows, ...second.rows].map(r => r.seq), [1, 2, 3]);
});
test('review 2: Stop sees the deployed seq-stamped in-flight rotation', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o);
  runHook(input('first'), f.o);
  fs.appendFileSync(f.log, line(row(2)));
  const rotating = f.log + '.rotating-20260102T120000Z-seq-1-2';
  fs.renameSync(f.log, rotating); fs.writeFileSync(f.log, '');
  runHook({ hook_event_name: 'Stop', prompt_id: 'rotating-turn' }, f.o);
  fs.writeFileSync(f.log + '.archive-20260102T120000Z-seq-1-2.gz', zlib.gzipSync(fs.readFileSync(rotating)));
  fs.unlinkSync(rotating); fs.writeFileSync(f.log, line(row(3, 'example.padding')));
  assert.equal(command('status', ['--json'], f.o).waiting.length, 0);
});
test('review 2: Stop fences unread messages; Post keeps arrivals while catching up', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o);
  fs.writeFileSync(f.log, '');
  runHook(input('long'), { ...f.o, now: NOW - 90000 });
  for (let n = 1; n <= 10; n++) fs.appendFileSync(f.log, line(row(n, 'example.padding', { padding: 'x'.repeat(1024 * 1024) })));
  fs.appendFileSync(f.log, line(row(11)));
  runHook({ hook_event_name: 'PostToolUse', tool_use_id: 'long' }, f.o);
  runHook({ hook_event_name: 'Stop', prompt_id: 'turn-example' }, f.o);
  for (let n = 0; n < 4; n++) runHook(input('catchup-' + n), f.o);
  const status = command('status', ['--json'], f.o);
  assert.equal(status.waiting.length, 0);
  assert.equal(status.history.find(r => r.type === 'long_call').waiting, true);
  const second = fixture(t); fs.copyFileSync(f.log, second.log);
  command('mode', ['enforce'], second.o);
  runHook({ hook_event_name: 'Stop', prompt_id: 'fresh-turn' }, second.o);
  for (let n = 0; n < 4; n++) runHook(input('fresh-' + n), second.o);
  assert.equal(command('status', ['--json'], second.o).waiting.length, 0);
});
test('review 3: every interrupted lock stage is recoverable', t => {
  const f = fixture(t); command('mode', ['observe'], f.o);
  for (const name of ['recovery.json', 'lock.json', 'lock-99999999-example.json']) {
    fs.writeFileSync(path.join(f.stateDir, name), JSON.stringify({ pid: 99999999, ticket: 0 }));
  }
  assert.equal(command('mode', ['enforce'], f.o).mode, 'enforce');
  assert.ok(runHook(input('recovered'), f.o).hookSpecificOutput);
});
test('review 2: deferred call history includes a preexisting unread wait', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o); fs.writeFileSync(f.log, '');
  for (let n = 1; n <= 10; n++) fs.appendFileSync(f.log, line(row(n, 'example.padding', { padding: 'x'.repeat(1024 * 1024) })));
  fs.appendFileSync(f.log, line(row(11)));
  runHook(input('old-wait'), f.o);
  runHook({ hook_event_name: 'PostToolUse', tool_use_id: 'old-wait' }, { ...f.o, now: NOW + 31000 });
  assert.equal(command('status', ['--json'], { ...f.o, now: NOW + 31000 }).history.find(r => r.type === 'long_call').waiting, true);
});
test('review 2: deferred call history respects the Stop fence at call start', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o); fs.writeFileSync(f.log, '');
  for (let n = 1; n <= 20; n++) fs.appendFileSync(f.log, line(row(n, 'example.padding', { padding: 'x'.repeat(1024 * 1024) })));
  fs.appendFileSync(f.log, line(row(21)));
  runHook({ hook_event_name: 'Stop', prompt_id: 'earlier-turn' }, f.o);
  runHook(input('seen-before-call'), f.o);
  runHook({ hook_event_name: 'PostToolUse', tool_use_id: 'seen-before-call' }, { ...f.o, now: NOW + 31000 });
  assert.equal(command('status', ['--json'], { ...f.o, now: NOW + 31000 }).history.find(r => r.type === 'long_call').waiting, false);
});
test('review 4: archive replay advances in bounded chunks without dropping rows', t => {
  const f = fixture(t); const first = readEvents(f.city, null);
  const fd = fs.openSync(f.log, 'a');
  for (let seq = 2; seq <= 260; seq++) fs.writeSync(fd, line(row(seq, 'example.padding', { padding: 'x'.repeat(1024 * 1024) })));
  fs.closeSync(fd);
  fs.writeFileSync(f.log + '.archive-20260102T120000Z-seq-1-260.gz', zlib.gzipSync(fs.readFileSync(f.log)));
  fs.unlinkSync(f.log); fs.writeFileSync(f.log, line(row(261)));
  let cursor = first.cursor; const seen = [];
  for (let n = 0; n < 60; n++) {
    const result = readEvents(f.city, cursor, { now: NOW });
    assert.ok(result.rows.length < 20, 'one invocation must not parse a whole archive');
    seen.push(...result.rows.map(r => r.seq)); cursor = result.cursor;
    if (result.caughtUp) break;
  }
  assert.deepEqual(seen, Array.from({ length: 260 }, (_, n) => n + 2));
});
test('review 4: archive transport handles partial pipe writes', t => {
  const f = fixture(t); const first = readEvents(f.city, null);
  fs.appendFileSync(f.log, line(row(2)));
  fs.writeFileSync(f.log + '.archive-20260102T120000Z-seq-1-2.gz', zlib.gzipSync(fs.readFileSync(f.log)));
  fs.unlinkSync(f.log); fs.writeFileSync(f.log, line(row(3)));
  const shim = path.join(f.city, 'short-write.cjs');
  fs.writeFileSync(shim, "const fs = require('node:fs'); const write = fs.writeSync; fs.writeSync = (fd, b, ...a) => fd === 1 && Buffer.isBuffer(b) ? write(fd, b.subarray(0, Math.max(1, Math.floor(b.length / 2)))) : write(fd, b, ...a);");
  const before = process.env.NODE_OPTIONS; process.env.NODE_OPTIONS = '--require ' + shim;
  try { assert.deepEqual(readEvents(f.city, first.cursor).rows.map(r => r.seq), [2, 3]); }
  finally { if (before === undefined) delete process.env.NODE_OPTIONS; else process.env.NODE_OPTIONS = before; }
});
test('review 5: retained historical pack caches do not mask a new pack roll', t => {
  const f = fixture(t); const old = path.join(f.city, 'old.cjs'); const current = path.join(f.city, 'current.cjs');
  fs.writeFileSync(old, '// old'); fs.writeFileSync(current, '// current');
  install({ ...f.o, source: old });
  assert.equal(command('status', ['--json'], { ...f.o, source: current }).current, false);
  assert.ok(doctor({ ...f.o, source: current, prompt: '' }).includes('hook_copy_outdated'));
});
test('codex 1: first install preserves valid hooks without matcher properties', t => {
  const f = fixture(t); fs.mkdirSync(path.join(f.city, '.claude'));
  const before = { hooks: Object.fromEntries(['PreToolUse', 'PostToolUse', 'Stop'].map(e => [e, [{ hooks: [{ type: 'command', command: 'example-other-hook' }] }]])) };
  const file = path.join(f.city, '.claude/settings.json'); fs.writeFileSync(file, JSON.stringify(before));
  install(f.o); command('uninstall', [], f.o);
  assert.deepEqual(JSON.parse(fs.readFileSync(file)), before);
});
test('codex 1: maintenance callers cannot teach the worker identity as a mayor', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o);
  for (const verb of ['status', 'pass', 'exempt']) {
    const args = verb === 'status' ? [] : verb === 'pass' ? ['example/room-example'] : ['example/room-example', '--off'];
    command(verb, args, { ...f.o, env: { GC_AGENT: 'worker', GC_SESSION_ID: 'worker-example' } });
  }
  fs.appendFileSync(f.log, line(row(2)));
  fs.appendFileSync(f.log, line(row(3, 'extmsg.outbound', { session: 'worker-example' })));
  assert.equal(command('status', ['--json'], f.o).waiting.length, 1);
});
test('codex 1: an empty caller session cannot match an unrouted message', t => {
  for (const env of [{}, { GC_AGENT: 'worker', GC_SESSION_ID: 'worker-example' }]) {
    const f = fixture(t);
    fs.writeFileSync(f.log, line(row(1, 'extmsg.inbound', { target_agent: '', target_session: '' })));
    assert.equal(command('status', ['--json'], { ...f.o, env }).waiting.length, 0);
  }
});
test('codex 1: worker status preserves established legacy mayor sessions', t => {
  const f = fixture(t);
  fs.writeFileSync(f.log, line({ seq: 1, type: 'session.woke', ts: new Date(NOW - 90000).toISOString(), subject: 'mayor', session_id: 'session-example' }) + line(row(2, 'extmsg.inbound', { target_agent: '', target_session: 'session-example' })));
  const worker = { ...f.o, env: { GC_AGENT: 'worker', GC_SESSION_ID: 'worker-example' } };
  assert.equal(command('status', ['--json'], worker).waiting.length, 1);
  assert.equal(command('status', ['--json'], f.o).waiting.length, 1);
});
test('codex 1: unknown legacy routing is not consumed by an unbound operator', t => {
  const f = fixture(t);
  fs.writeFileSync(f.log, line(row(1, 'extmsg.inbound', { target_agent: '', target_session: 'session-example' })));
  assert.throws(() => command('status', ['--json'], { ...f.o, env: {} }), /mayor_session_unknown/);
  assert.equal(command('status', ['--json'], f.o).waiting.length, 1);
});
test('codex 1: distinct tool-free turns and duplicate Stop use genuine transcript identities', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o);
  const transcript = path.join(f.city, 'transcript.jsonl');
  fs.writeFileSync(transcript, line({ type: 'user', uuid: 'turn-one', message: { content: 'example' } }));
  const stop = { hook_event_name: 'Stop', transcript_path: transcript, last_assistant_message: 'Acknowledged.' };
  runHook(stop, f.o);
  fs.appendFileSync(f.log, line(row(2)));
  runHook(stop, f.o);
  assert.equal(command('status', ['--json'], f.o).waiting.length, 1);
  fs.appendFileSync(transcript, line({ type: 'user', uuid: 'turn-two', message: { content: 'another example' } }));
  runHook(stop, f.o);
  assert.equal(command('status', ['--json'], f.o).waiting.length, 0);
});
test('codex 1: lifecycle snapshots retry a rotation during directory discovery', t => {
  for (const event of ['Stop', 'PostToolUse']) {
    const f = fixture(t); command('mode', ['enforce'], f.o);
    fs.writeFileSync(f.log, line({ ...row(1, 'example.padding'), ts: new Date(NOW - 90000).toISOString() }));
    runHook(input('in-flight'), { ...f.o, now: NOW - 90000 });
    fs.appendFileSync(f.log, line(row(2)));
    const readdir = fs.readdirSync; let rotated = false;
    const rotating = f.log + '.rotating-20260102T120000Z-seq-1-2';
    fs.readdirSync = (...args) => {
      const names = readdir(...args);
      if (!rotated && args[0] === f.dir) { rotated = true; fs.renameSync(f.log, rotating); fs.writeFileSync(f.log, ''); }
      return names;
    };
    try { runHook({ hook_event_name: event, prompt_id: 'turn-example', tool_use_id: 'in-flight' }, f.o); }
    finally { fs.readdirSync = readdir; }
    fs.writeFileSync(f.log + '.archive-20260102T120000Z-seq-1-2.gz', zlib.gzipSync(fs.readFileSync(rotating)));
    fs.unlinkSync(rotating); fs.writeFileSync(f.log, line(row(3, 'example.padding')));
    const status = command('status', ['--json'], f.o);
    if (event === 'Stop') assert.equal(status.waiting.length, 0);
    else assert.equal(status.history.find(r => r.type === 'long_call').waiting, true);
  }
});
test('codex 2: canonical outbound subjects handle empty sessions and caller aliases', t => {
  for (const session of ['worker-example', '', undefined]) {
    const f = fixture(t); command('mode', ['enforce'], f.o);
    fs.appendFileSync(f.log, line({ ...row(2, 'extmsg.outbound', { session }), subject: 'worker-example' }));
    fs.appendFileSync(f.log, line(row(3)));
    assert.ok(runHook(input('after-worker-send'), f.o).hookSpecificOutput);
    const state = JSON.parse(fs.readFileSync(path.join(f.stateDir, 'state.json')));
    assert.equal(state.cursor.seq, 3); assert.equal(state.waiting.length, 2);
    fs.appendFileSync(f.log, line({ ...row(4, 'extmsg.outbound', { session: 'mayor' }), subject: 'session-example' }));
    assert.equal(command('status', ['--json'], f.o).waiting.length, 0);
    fs.appendFileSync(f.log, line(row(5)));
    fs.appendFileSync(f.log, line({ ...row(6, 'extmsg.outbound', { session: session === 'worker-example' ? 'mayor' : session }), subject: 'session-example' }));
    assert.equal(command('status', ['--json'], f.o).waiting.length, 0);
  }
});
test('codex 2: mayor names match inbound and outbound without learning worker names', t => {
  const f = fixture(t); f.o.env.GC_SESSION_NAME = 'mayor-name-example';
  command('mode', ['enforce'], f.o);
  fs.writeFileSync(f.log, line(row(1, 'extmsg.inbound', { target_agent: '', target_session: 'mayor-name-example' })));
  assert.ok(runHook(input('named-mayor'), f.o).hookSpecificOutput);
  command('status', [], { ...f.o, env: { GC_AGENT: 'worker', GC_SESSION_ID: 'worker-example', GC_SESSION_NAME: 'worker-name-example' } });
  fs.appendFileSync(f.log, line(row(2, 'extmsg.outbound', { session: 'worker-name-example' })));
  assert.equal(command('status', ['--json'], f.o).waiting.length, 1);
  fs.appendFileSync(f.log, line(row(3, 'extmsg.outbound', { session: 'mayor-name-example' })));
  assert.equal(command('status', ['--json'], f.o).waiting.length, 0);
});
test('codex 2: city-relative local imports resolve channels and unrelated packs', t => {
  const f = fixture(t);
  for (const [dir, name] of [['talk', 'gchat'], ['other', 'unrelated']]) {
    const root = path.join(f.city, 'packs', dir); fs.mkdirSync(root, { recursive: true });
    fs.writeFileSync(path.join(root, 'pack.toml'), `[pack]\nname = "${name}"\n`);
  }
  const imports = 'talk\t//packs/talk\t\t(path)\nother\t//packs/other\t\t(path)';
  assert.deepEqual(aliasesFromImports(imports, f.city), ['talk']);
  assert.equal(install({ ...f.o, imports }).installed, true);
});
test('codex 3: cold replay retains old wake and inbound identities for recent replies', t => {
  for (const source of ['wake', 'inbound']) {
    const f = fixture(t); f.o.env.GC_SESSION_ID = 'session-new';
    command('mode', ['enforce'], f.o);
    const ts = new Date(NOW - 2 * 86400000).toISOString();
    const old = source === 'wake' ? { seq: 1, type: 'session.woke', ts, subject: 'mayor', session_id: 'session-old' } : { ...row(1, 'extmsg.inbound', { target_session: 'session-old' }), ts };
    fs.writeFileSync(f.log + '.archive-20251231T120000Z-seq-1-1.gz', zlib.gzipSync(line(old)));
    fs.writeFileSync(f.log, line(row(2, 'extmsg.inbound', { target_session: '' })) + line({ ...row(3, 'extmsg.outbound', { session: 'session-old' }), subject: 'session-old' }) + line({ seq: 4, type: 'session.woke', ts: new Date(NOW - 10000).toISOString(), subject: 'mayor', session_id: 'session-new' }));
    assert.deepEqual(runHook({ ...input('after-handoff'), tool_name: 'Write' }, f.o), {});
    assert.equal(command('status', ['--json'], f.o).waiting.length, 0);
  }
});
test('codex 3: multiple archives preserve progress before the per-call deadline', t => {
  const f = fixture(t);
  for (let seq = 1; seq <= 4; seq++) fs.writeFileSync(f.log + `.archive-20260102T120000Z-seq-${seq}-${seq}.gz`, zlib.gzipSync(line(row(seq, 'example.padding'))));
  fs.writeFileSync(f.log, line(row(5, 'example.padding')));
  const shim = path.join(f.city, 'slow-archive.cjs');
  fs.writeFileSync(shim, 'Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 600);');
  const before = process.env.NODE_OPTIONS; process.env.NODE_OPTIONS = '--require ' + shim;
  try {
    let result = readEvents(f.city, null, { now: NOW });
    assert.equal(result.caughtUp, false);
    const seen = result.rows.map(r => r.seq);
    for (let n = 0; !result.caughtUp && n < 5; n++) { result = readEvents(f.city, result.cursor, { now: NOW }); seen.push(...result.rows.map(r => r.seq)); }
    assert.deepEqual(seen, [1, 2, 3, 4, 5]);
  } finally { if (before === undefined) delete process.env.NODE_OPTIONS; else process.env.NODE_OPTIONS = before; }
});
test('review 6: resolved manifests determine aliases for every supported source form', t => {
  const f = fixture(t); const gcHome = path.join(f.city, 'home');
  const sources = [
    ['https://github.com/example/packs/blob/main/gchat/pack.toml', 'https://github.com/example/packs.git', 'gchat'],
    ...['git@example.invalid:packs', 'ssh://example.invalid/packs', 'file:///example/packs', 'github.com/example/packs'].map(s => [s + '//gchat#main', s.startsWith('github.com/') ? 'https://' + s : s, 'gchat']),
    ['https://example.invalid/gchat.git', 'https://example.invalid/gchat.git', ''],
    ['file:///example/renamed', 'file:///example/renamed', ''],
    ['https://example.invalid/arbitrary', 'https://example.invalid/arbitrary', ''],
  ];
  let lock = 'schema = 1\n'; const imports = [];
  for (const [i, [source, clone, sub]] of sources.entries()) {
    const commit = String(i).repeat(40); const dir = path.join(gcHome, 'cache/repos', hash(clone + commit), sub);
    fs.mkdirSync(dir, { recursive: true }); fs.writeFileSync(path.join(dir, 'pack.toml'), '[pack]\n  name = "gchat"\n');
    lock += `[packs.${JSON.stringify(source)}]\ncommit = "${commit}"\n`;
    imports.push(`talk-${i}\t${source}\t\tlocked`);
  }
  fs.writeFileSync(path.join(f.city, 'packs.lock'), lock);
  const local = path.join(f.city, 'renamed'); fs.mkdirSync(local); fs.writeFileSync(path.join(local, 'pack.toml'), '[pack]\n  name = "reachable"\n');
  const misleading = path.join(f.city, 'gchat'); fs.mkdirSync(misleading); fs.writeFileSync(path.join(misleading, 'pack.toml'), '[pack]\nname = "unrelated"\n');
  imports.push('escape\t./renamed\t\t(path)', 'wrong\t./gchat\t\t(path)');
  assert.deepEqual(aliasesFromImports(imports.join('\n'), f.city, { GC_HOME: gcHome }), [...sources.map((_, i) => 'talk-' + i), 'escape']);
});
test('review 6: bundled unrelated imports do not prevent local channel resolution', t => {
  const f = fixture(t); const local = path.join(f.city, 'talk'); fs.mkdirSync(local);
  fs.writeFileSync(path.join(local, 'pack.toml'), '[pack]\nname = "gchat"\n');
  const source = 'https://github.com/gastownhall/gascity-packs/tree/main/gastown';
  const imports = `bundled\t${source}\t\tlocked\ntalk\t./talk\t\t(path)`;
  assert.deepEqual(aliasesFromImports(imports, f.city, { GC_HOME: path.join(f.city, 'home') }), ['talk']);
  fs.writeFileSync(path.join(f.city, 'packs.lock'), `[packs.${JSON.stringify(source)}]\ncommit = "${'a'.repeat(40)}"\n`);
  assert.deepEqual(aliasesFromImports(imports, f.city, { GC_HOME: path.join(f.city, 'home') }), ['talk']);
});
test('review 7: a save exceeding the deadline cannot leave a cached refusal', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o);
  const rename = fs.renameSync; let delayed = false;
  fs.renameSync = (...args) => {
    const result = rename(...args);
    if (!delayed && args[1] === path.join(f.stateDir, 'state.json')) { delayed = true; const start = performance.now(); while (performance.now() - start < 2005) {} }
    return result;
  };
  try { assert.deepEqual(runHook(input('slow'), f.o), {}); } finally { fs.renameSync = rename; }
  assert.deepEqual(runHook(input('slow'), f.o), {});
  assert.equal(command('status', ['--json'], f.o).history.filter(r => r.type === 'refusal').length, 0);
});
test('review 8: concurrent error publication retains both records', t => {
  const f = fixture(t); command('mode', ['enforce'], f.o);
  fs.writeFileSync(path.join(f.stateDir, 'state.json'), '{broken');
  const rename = fs.renameSync; let nested = false;
  fs.renameSync = (...args) => {
    if (!nested && /errors?[-.]/.test(path.basename(args[1]))) { nested = true; runHook(input('second-error'), f.o); }
    return rename(...args);
  };
  try { runHook(input('first-error'), f.o); } finally { fs.renameSync = rename; }
  const records = fs.readdirSync(f.stateDir).filter(n => /^errors?[-.].*jsonl$/.test(n)).flatMap(n => fs.readFileSync(path.join(f.stateDir, n), 'utf8').trim().split('\n'));
  assert.equal(records.length, 2);
});
test('review 7: installed outer deadline leaves time to persist fail-open rollback', t => {
  const f = fixture(t); install(f.o); command('mode', ['enforce'], f.o);
  const settings = JSON.parse(fs.readFileSync(path.join(f.city, '.claude/settings.json')));
  const timeout = settings.hooks.PreToolUse[0].hooks[0].timeout * 1000;
  const shim = path.join(f.city, 'delay.cjs');
  fs.writeFileSync(shim, `Date.now = () => ${NOW}; const fs = require('node:fs'); const rename = fs.renameSync; let delayed = false; fs.renameSync = (...a) => { const r = rename(...a); if (!delayed && a[1].endsWith('/state.json')) { delayed = true; Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 2050); } return r; };`);
  const result = spawnSync(process.execPath, ['--require', shim, path.join(f.stateDir, 'hook.cjs'), '--hook', '--city', f.city], { env: { ...process.env, ...f.o.env }, input: JSON.stringify(input('deadline')), encoding: 'utf8', timeout });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 0); assert.equal(result.stdout, '');
  assert.deepEqual(runHook(input('deadline'), f.o), {});
});
