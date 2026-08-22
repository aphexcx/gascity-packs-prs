// liveness.test.js — unit tests for the inbound-liveness watchdog
// (src/liveness.js, jg-p1mk item 1), with a fake clock and tick() driven
// directly. Pins the 8/19 ready-but-dead detection contract: stall
// alarms fire past the threshold (once, then on the repeat cadence),
// recovery logs and resets on the next inbound frame, silence predating
// process start never alarms early, the optional reconnect remediation
// is rate-limited and failure-isolated, and /healthz detail reports the
// degraded state.

import assert from 'node:assert/strict';
import { test } from 'node:test';

import { createInboundLiveness } from '../src/liveness.js';

// makeTracker builds a watchdog on a fake clock; logs[] captures lines.
function makeTracker(opts = {}) {
  let t = opts.startAt ?? 1_000_000;
  const clock = {
    now: () => t,
    advance: (ms) => { t += ms; },
  };
  const logs = [];
  const tracker = createInboundLiveness({
    stallAfterMs: 10_000,
    alarmRepeatMs: 60_000,
    now: clock.now,
    log: (...args) => logs.push(args.join(' ')),
    ...opts.opts,
  });
  return { tracker, clock, logs };
}

const alarms = (logs) => logs.filter((l) => l.includes('INBOUND LIVENESS ALARM'));

test('a zero stall threshold disables the watchdog entirely', () => {
  const { tracker, clock, logs } = makeTracker({ opts: { stallAfterMs: 0 } });
  clock.advance(1_000_000_000);
  tracker.tick();
  assert.equal(logs.length, 0);
  assert.equal(tracker.state().state, 'watchdog_off');
  assert.match(tracker.healthzDetail(), /inbound_liveness=watchdog_off/);
});

test('no alarm inside the threshold; one alarm past it; state flips to stalled', () => {
  const { tracker, clock, logs } = makeTracker();
  tracker.noteInbound();
  clock.advance(9_999);
  tracker.tick();
  assert.equal(alarms(logs).length, 0);
  assert.equal(tracker.state().state, 'ok');

  clock.advance(2);
  tracker.tick();
  assert.equal(alarms(logs).length, 1);
  assert.equal(tracker.state().state, 'stalled');
  assert.match(tracker.healthzDetail(), /inbound_liveness=stalled/);
  assert.match(tracker.healthzDetail(), /stalled_since=/);
});

test('a persisting stall re-alarms only on the repeat cadence', () => {
  const { tracker, clock, logs } = makeTracker();
  tracker.noteInbound();
  clock.advance(10_001);
  tracker.tick();
  assert.equal(alarms(logs).length, 1);

  // Ticks inside the repeat interval stay quiet (the stall persists).
  clock.advance(30_000);
  tracker.tick();
  assert.equal(alarms(logs).length, 1);

  clock.advance(30_001);
  tracker.tick();
  assert.equal(alarms(logs).length, 2);
});

test('inbound resumes: RECOVERED is logged, state clears, the next stall alarms afresh', () => {
  const { tracker, clock, logs } = makeTracker();
  tracker.noteInbound();
  clock.advance(10_001);
  tracker.tick();
  assert.equal(tracker.state().state, 'stalled');

  tracker.noteInbound();
  assert.equal(tracker.state().state, 'ok');
  assert.equal(logs.filter((l) => l.includes('RECOVERED')).length, 1);

  // A fresh stall alarms immediately again — the repeat clamp must not
  // survive a recovery.
  clock.advance(10_001);
  tracker.tick();
  assert.equal(alarms(logs).length, 2);
});

test('silence before process start never alarms early (idle anchors to startedAt)', () => {
  const { tracker, clock, logs } = makeTracker();
  // No inbound ever. Idle counts from construction, not from epoch 0.
  clock.advance(9_999);
  tracker.tick();
  assert.equal(alarms(logs).length, 0);
  clock.advance(2);
  tracker.tick();
  assert.equal(alarms(logs).length, 1);
});

test('the reconnect remediation fires on stall, rate-limited, and re-fires after the floor', () => {
  let cycles = 0;
  const { tracker, clock } = makeTracker({
    opts: { reconnect: () => { cycles++; }, reconnectMinIntervalMs: 40_000 },
  });
  tracker.noteInbound();
  clock.advance(10_001);
  tracker.tick();
  assert.equal(cycles, 1);

  clock.advance(39_000);
  tracker.tick();
  assert.equal(cycles, 1); // inside the floor

  clock.advance(1_001);
  tracker.tick();
  assert.equal(cycles, 2);
  assert.equal(tracker.state().reconnectsTotal, 2);
});

test('a throwing reconnect hook is caught and logged, never fatal', () => {
  const { tracker, clock, logs } = makeTracker({
    opts: { reconnect: () => { throw new Error('boom'); } },
  });
  tracker.noteInbound();
  clock.advance(10_001);
  tracker.tick();
  assert.equal(logs.filter((l) => l.includes('reconnect hook failed: boom')).length, 1);
  assert.equal(tracker.state().state, 'stalled');
});

test('healthzDetail reports ok state with counters and last_inbound', () => {
  const { tracker, clock } = makeTracker();
  tracker.noteInbound();
  clock.advance(5_000);
  tracker.tick();
  const detail = tracker.healthzDetail();
  assert.match(detail, /inbound_liveness=ok/);
  assert.match(detail, /frames=1/);
  assert.match(detail, /alarms=0/);
  assert.match(detail, /last_inbound=\d{4}-\d{2}-\d{2}T/);
  assert.ok(!detail.includes('stalled_since'));
});

test('start()/stop() arm and clear the interval without leaking a live timer', async () => {
  const { tracker } = makeTracker();
  tracker.start(5);
  tracker.start(5); // idempotent — a second start must not orphan a timer
  await new Promise((r) => setTimeout(r, 20));
  tracker.stop();
  tracker.stop(); // idempotent
});
