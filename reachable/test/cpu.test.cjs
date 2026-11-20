'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const { performance } = require('node:perf_hooks');
const { command } = require('../reachable.cjs');

// Loaded by both children so the control includes startup and instrumentation.
function meter() {
  const fs = require('node:fs');
  if (process.env.EXTRA_HOOK_CPU) {
    const start = process.cpuUsage();
    let used;
    do { used = process.cpuUsage(start); } while (used.user + used.system < 150_000);
  }
  process.on('exit', () => {
    const used = process.cpuUsage();
    fs.writeFileSync(process.env.CPU_SAMPLE, String((used.user + used.system) / 1000));
  });
}

function percentile(samples, p) {
  return samples.toSorted((a, b) => a - b)[Math.ceil(p * samples.length) - 1];
}

test('R5: complete hook CPU above bare Node is under 100 ms and detects 150 ms of added work', t => {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-cpu-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  fs.mkdirSync(path.join(city, '.gc'));
  fs.writeFileSync(path.join(city, '.gc/events.jsonl'), '');
  const env = { GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example', CPU_SAMPLE: path.join(city, 'sample') };
  command('mode', ['observe'], { city, env });
  const shim = path.join(city, 'cpu.cjs');
  fs.writeFileSync(shim, '(' + meter.toString() + ')();');
  const script = path.resolve(__dirname, '../reachable.cjs');
  const run = (args, extra, id) => {
    fs.rmSync(env.CPU_SAMPLE, { force: true });
    const start = performance.now();
    const result = spawnSync(process.execPath, ['--require', shim, ...args], {
      env: { ...env, ...extra },
      input: JSON.stringify({ hook_event_name: 'PreToolUse', tool_name: 'Read', tool_use_id: id }),
      encoding: 'utf8',
    });
    const wall = performance.now() - start;
    assert.equal(result.status, 0, result.stderr);
    const cpu = Number(fs.readFileSync(env.CPU_SAMPLE, 'utf8'));
    assert.ok(Number.isFinite(cpu) && cpu > 0);
    return { cpu, wall };
  };
  for (const regression of [false, true]) {
    const hooks = [], controls = [];
    for (let i = 0; i < 20; i++) {
      hooks.push(run([script, '--hook', '--city', city], regression ? { EXTRA_HOOK_CPU: '1' } : {}, `call-${regression}-${i}`));
      controls.push(run(['-e', '0'], {}, 'control'));
    }
    const delta = percentile(hooks.map(s => s.cpu), 0.5) - percentile(controls.map(s => s.cpu), 0.5);
    const wall = list => percentile(list.map(s => s.wall), 0.95).toFixed(2);
    t.diagnostic(`extra work=${regression}, median CPU delta=${delta.toFixed(2)} ms, wall p95 hook=${wall(hooks)} ms bare=${wall(controls)} ms`);
    const withinBudget = () => assert.ok(delta < 100, `CPU delta ${delta} ms exceeds budget`);
    if (regression) assert.throws(withinBudget, /exceeds budget/);
    else withinBudget();
  }
  const state = JSON.parse(fs.readFileSync(path.join(city, '.gc/reachable/state.json'), 'utf8'));
  assert.equal(Object.keys(state.receipts).length, 40);
});
