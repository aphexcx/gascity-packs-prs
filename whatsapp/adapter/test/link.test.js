import assert from 'node:assert/strict';
import test from 'node:test';
import { adapterRequest, CREDENTIAL, rig, sleep, stays, waitFor } from './fakes.js';

async function setup(t, extra = {}) {
  const r = await rig({ extra: { WHATSAPP_HELD_CONNECTION: 'on', WHATSAPP_POLL_MS: '2000', ...extra } });
  t.after(() => r.close());
  return r;
}
const status = async (r) => (await adapterRequest(r.socket, 'GET', '/status')).json.inbound;
const polls = (r) => r.gw.requests.filter((entry) => entry.route === 'GET /v1/inbox').length;
const up = (r) => waitFor(async () => (await status(r).catch(() => ({}))).link?.state === 'up', { timeoutMs: 2000, what: 'held link' });
const add = (r, suffix) => r.gw.addText('invented-owner', `wamid.test-${suffix}`, `invented ${suffix}`);
const acked = (r, id) => waitFor(() => r.gw.acked.has(id), { timeoutMs: 2000, what: 'wake-driven ack' })
  .catch(async (error) => { throw new Error(`${error.message}; ${JSON.stringify(await status(r))}; ${r.adapter.log()}`); });

test('held: credential travels only in the upgrade header', async (t) => {
  const r = await setup(t);
  r.start();
  await up(r);
  assert.deepEqual(r.gw.link.attempts.map(({ url, authorization, protocol }) => ({ url, authorization, protocol })),
    [{ url: '/v1/wake', authorization: `Bearer ${CREDENTIAL}`, protocol: undefined }]);
  const link = (await status(r)).link;
  assert.equal(link.mode, 'held');
  assert.deepEqual(Object.keys(link).sort(), ['last_wake_at', 'mode', 'reason', 'reconnects', 'since', 'state', 'wakes']);
  assert.ok(!JSON.stringify(link).includes(CREDENTIAL));
  assert.ok(!JSON.stringify(link).includes(r.gw.url));
});

test('held: a wake delivers and acknowledges within two real seconds despite a ten-minute poll', async (t) => {
  const r = await setup(t, { WHATSAPP_POLL_MS: '600000' });
  r.start();
  await up(r);
  await waitFor(() => polls(r) >= 1);
  const id = add(r, 'immediate');
  r.gw.link.wake();
  await acked(r, id);
  assert.equal(r.gc.count('wamid.test-immediate'), 1);
  assert.equal((await status(r)).link.wakes, 1);
});

test('held: a wake during an in-flight read queues exactly one further read', async (t) => {
  const r = await setup(t, { WHATSAPP_POLL_MS: '600000' });
  r.start();
  await up(r);
  await waitFor(async () => (await status(r)).polls >= 1);
  const before = polls(r);
  r.gw.inboxDelayMs = 150;
  const first = add(r, 'first');
  r.gw.link.wake();
  await waitFor(() => polls(r) === before + 1);
  const second = add(r, 'second');
  r.gw.link.wake();
  r.gw.link.wake();
  await acked(r, first);
  await acked(r, second);
  await stays(() => polls(r) === before + 2);
  assert.equal(r.gc.count('wamid.test-first'), 1);
  assert.equal(r.gc.count('wamid.test-second'), 1);
});

test('held: a quiet connection reads only once each safety interval', async (t) => {
  const r = await setup(t, { WHATSAPP_HELD_POLL_MS: '60000' });
  r.start();
  await up(r);
  await sleep(100);
  await waitFor(async () => (await status(r)).polls >= 1);
  const before = polls(r);
  await r.pump(59000, { stepMs: 1000, realMs: 15 });
  assert.equal(polls(r), before);
  await r.pump(1000);
  await waitFor(() => polls(r) === before + 1);
  await r.pump(60000, { stepMs: 1000, realMs: 15 });
  await waitFor(() => polls(r) === before + 2);
});

test('held: every reconnect reads a delivery whose wake was lost', async (t) => {
  const r = await setup(t, { WHATSAPP_POLL_MS: '600000' });
  r.start();
  await up(r);
  r.gw.link.drop();
  await waitFor(async () => (await status(r)).link.mode === 'poll');
  await sleep(100); // The immediate fallback read finishes before this delivery exists.
  const id = add(r, 'reconnect');
  await r.pump(1000);
  await up(r);
  await acked(r, id);
});

test('held: route missing and explicit off retain the ordinary polling cadence', async (t) => {
  for (const cause of ['missing', 'off']) {
    const r = await setup(t, cause === 'off' ? { WHATSAPP_HELD_CONNECTION: 'off' } : {});
    if (cause === 'missing') r.gw.link.enabled = false;
    r.start();
    await waitFor(async () => (await status(r).catch(() => ({}))).polls >= 1);
    const before = polls(r);
    const id = add(r, cause);
    await r.pump(2000);
    await acked(r, id);
    assert.equal(polls(r), before + 1);
    assert.equal((await status(r)).link.mode, 'poll');
    if (cause === 'off') assert.equal(r.gw.link.attempts.length, 0);
    await r.close();
  }
});

test('held: a missed pong closes the socket within forty clock seconds and resumes polling', async (t) => {
  const r = await setup(t);
  r.start();
  await up(r);
  const id = add(r, 'missed-pong');
  r.gw.link.answerPings = false;
  await r.pump(30000, { stepMs: 1000, realMs: 15 });
  assert.equal(r.gw.link.pings.length, 1);
  assert.equal((await status(r)).link.state, 'up');
  await r.pump(10000, { stepMs: 1000, realMs: 15 });
  await waitFor(async () => (await status(r)).link.mode === 'poll');
  await waitFor(() => r.gw.link.sockets.size === 0);
  await acked(r, id);
});

test('held: reconnect attempts follow 1, 5, 15, 60, then 300 seconds without a loop', async (t) => {
  const r = await setup(t);
  r.gw.link.enabled = false;
  r.start();
  await waitFor(() => r.gw.link.attempts.length === 1);
  await sleep(50);
  const start = r.clock.now();
  let elapsed = 0;
  for (const delay of [1000, 5000, 15000, 60000, 300000, 300000]) {
    const count = r.gw.link.attempts.length;
    r.clock.advance(delay - 1);
    await stays(() => r.gw.link.attempts.length === count, { forMs: 40 });
    r.clock.advance(1);
    await waitFor(() => r.gw.link.attempts.length === count + 1);
    await sleep(50);
    elapsed += delay;
    assert.equal(r.gw.link.attempts.at(-1).at, start + elapsed);
  }
});

test('held: the inbox 401 stops all further polls and reconnects', async (t) => {
  const r = await setup(t);
  r.gw.revoked = true;
  r.start();
  await waitFor(async () => (await status(r).catch(() => ({}))).auth_failed);
  const attempts = r.gw.link.attempts.length;
  assert.equal(polls(r), 1);
  await r.pump(600000, { stepMs: 30000 });
  assert.equal(polls(r), 1);
  assert.equal(r.gw.link.attempts.length, attempts);
});

test('held: offline for an hour loses no delivery and posts each once after restart', async (t) => {
  const r = await setup(t);
  const ids = ['offline-a', 'offline-b', 'offline-c'].map((suffix) => add(r, suffix));
  r.gw.link.wake();
  r.clock.advance(3600000);
  r.start();
  await up(r);
  for (const id of ids) await acked(r, id);
  assert.equal(r.gc.posts.length, 3);
});

test('held: wake, reconnect and safety reads of a held row cause one post', async (t) => {
  const r = await setup(t, { WHATSAPP_HELD_POLL_MS: '10000' });
  r.gw.ackBehaviour = [500];
  r.start();
  await up(r);
  const id = add(r, 'dedup');
  r.gw.link.wake();
  await waitFor(() => r.gc.count('wamid.test-dedup') === 1);
  assert.equal(r.gw.held(id), true);
  r.gw.link.drop();
  await waitFor(async () => (await status(r)).link.mode === 'poll');
  await sleep(100);
  await r.pump(1000);
  await up(r);
  assert.equal(r.gw.held(id), true);
  const beforeSafetyRead = polls(r);
  await r.pump(10000);
  assert.ok(polls(r) > beforeSafetyRead);
  assert.equal(r.gw.held(id), true);
  assert.ok(r.gw.ackCalls.filter((call) => call.ids.includes(id)).every((call) => call.behaviour === 500));
  r.gw.ackBehaviour = ['ok'];
  await r.pump(10000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.test-dedup'), 1);
});

test('held: SIGKILL after received during a wake read recovers exactly once', async (t) => {
  const r = await setup(t, { WHATSAPP_TEST_KILL_AT: 'after-received' });
  r.start();
  await up(r);
  const id = add(r, 'crash');
  r.gw.link.wake();
  const exit = await r.adapter.exited;
  assert.equal(exit.signal, 'SIGKILL');
  await r.restart({ WHATSAPP_TEST_KILL_AT: '' });
  await up(r);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.test-crash'), 1);
});

test('held: a closed socket resumes the ordinary poll without waiting for the held interval', async (t) => {
  const r = await setup(t);
  r.start();
  await up(r);
  await sleep(100);
  const id = add(r, 'closed');
  r.gw.link.enabled = false;
  r.gw.link.drop();
  await waitFor(async () => (await status(r)).link.mode === 'poll');
  await acked(r, id);
});

test('held: 4503 rollback close immediately resumes polling and posts the held delivery once', async (t) => {
  const r = await setup(t);
  r.start();
  await up(r);
  await sleep(100);
  const id = add(r, 'rollback');
  r.gw.link.enabled = false;
  r.gw.link.close(4503);
  await waitFor(async () => (await status(r)).link.mode === 'poll');
  await acked(r, id);
  await r.pump(2000);
  assert.equal(r.gc.count('wamid.test-rollback'), 1);
  assert.equal((await status(r)).link.mode, 'poll');
});

// --- gp-urx1: the follow-ups to PR 103 ------------------------------------------
// In these tests the fake gateway holds the socket and answers ping but never
// sends a wake unless the test says so: what a box sees when the gateway was
// deployed without its wake binding, or when every wake fails at the gateway.

const SAFETY_MS = 300000; // WHATSAPP_HELD_POLL_MS at its default
const linkView = ({ mode, state, reason }) => ({ mode, state, reason });
// No read in flight: every inbox read the fake answered has completed in the box.
const settled = (r) => waitFor(async () => (await status(r)).polls === polls(r), { timeoutMs: 2000, what: 'reads settled' });

// Started, up, and its connect's read done; returns the inbox reads so far.
async function heldAndQuiet(r) {
  r.start();
  await up(r);
  await waitFor(async () => (await status(r)).polls >= 1);
  await sleep(100);
  await settled(r);
  return polls(r);
}

// The clock to the safety read, in steps that let every ping be answered.
async function untilSafetyRead(r, reads) {
  await r.pump(SAFETY_MS - 5000, { stepMs: 5000, realMs: 40 });
  assert.equal(polls(r), reads, 'no read before the safety interval');
  r.clock.advance(5000);
  await waitFor(() => polls(r) === reads + 1, { what: 'the safety read' });
}

// The next read comes exactly ms after the last one, not a millisecond before.
// When it does not come, the failure says where it came instead.
async function nextReadAfter(r, ms) {
  const reads = polls(r);
  const from = r.clock.now();
  r.clock.advance(ms - 1);
  await stays(() => polls(r) === reads, { forMs: 80, what: `no read before ${ms} ms` });
  r.clock.advance(1);
  try {
    await waitFor(() => polls(r) === reads + 1, { timeoutMs: 2000, what: `a read at ${ms} ms` });
  } catch {
    await r.pumpUntil(() => polls(r) > reads, { stepMs: 1000, realMs: 25, timeoutMs: 30000, what: 'any later read' });
    assert.fail(`the next read came ${r.clock.now() - from} ms after the last one, not ${ms} ms`);
  }
  await sleep(60);
}

for (const recovery of ['wake', 'reconnect']) {
  test(`held: a safety read that finds an unannounced delivery reads at WHATSAPP_POLL_MS until a ${recovery} (gp-urx1)`, async (t) => {
    const r = await setup(t);
    const reads = await heldAndQuiet(r);
    const first = add(r, `unannounced-${recovery}`);
    await untilSafetyRead(r, reads);
    await acked(r, first);
    const trippedAt = r.clock.now();
    const fallback = (await status(r)).link;
    // Red at the base: the next read is the safety interval away.
    await nextReadAfter(r, 2000);
    assert.deepEqual(linkView(fallback), { mode: 'poll', state: 'up', reason: 'unannounced_delivery' });
    assert.equal(fallback.since, trippedAt);
    assert.deepEqual(Object.keys(fallback).sort(), ['last_wake_at', 'mode', 'reason', 'reconnects', 'since', 'state', 'wakes']);
    const second = add(r, `still-unannounced-${recovery}`);
    await nextReadAfter(r, 2000);
    await acked(r, second);
    assert.equal((await status(r)).link.mode, 'poll');

    if (recovery === 'wake') {
      const before = polls(r);
      r.gw.link.wake();
      await waitFor(() => polls(r) === before + 1, { what: "the wake's read" });
    } else {
      r.gw.link.drop();
      await waitFor(async () => (await status(r)).link.state === 'down');
      await r.pump(1000); // the first rung of the reconnect ladder
      await up(r);
    }
    await sleep(100);
    assert.deepEqual(linkView((await status(r)).link), { mode: 'held', state: 'up', reason: '' });
    // Back on the safety interval: nothing for 295 s, then one read at 300 s.
    await untilSafetyRead(r, polls(r));
    assert.equal(r.gc.count(`wamid.test-unannounced-${recovery}`), 1);
    assert.equal(r.gc.count(`wamid.test-still-unannounced-${recovery}`), 1);
  });
}

test('held: a delivery the gateway returns again at a safety read is not new and keeps the safety interval (gp-urx1)', async (t) => {
  const r = await setup(t);
  r.gw.ackBehaviour = [500]; // every ack fails, so every read returns the row again
  await heldAndQuiet(r);
  const id = add(r, 'returned-again');
  r.gw.link.wake();
  await waitFor(() => r.gc.count('wamid.test-returned-again') === 1);
  await sleep(100);
  await untilSafetyRead(r, polls(r));
  await sleep(100);
  assert.equal(r.gw.held(id), true);
  assert.deepEqual(linkView((await status(r)).link), { mode: 'held', state: 'up', reason: '' });
  const reads = polls(r);
  await r.pump(10000, { stepMs: 1000, realMs: 15 });
  assert.equal(polls(r), reads);
  assert.equal(r.gc.count('wamid.test-returned-again'), 1);
});

test('held: a wake that lands during the safety read that finds its delivery keeps the safety interval (gp-urx1)', async (t) => {
  const r = await setup(t);
  const reads = await heldAndQuiet(r);
  const id = add(r, 'wake-during-read');
  r.gw.inboxDelayMs = 300; // the page holds the delivery; the answer comes later
  await untilSafetyRead(r, reads);
  r.gw.link.wake(); // the wake lands while the safety read is in flight
  await acked(r, id);
  await waitFor(() => polls(r) === reads + 2, { what: 'the one coalesced read the wake asked for' });
  r.gw.inboxDelayMs = 0;
  await sleep(400);
  assert.deepEqual(linkView((await status(r)).link), { mode: 'held', state: 'up', reason: '' });
  await r.pump(10000, { stepMs: 1000, realMs: 15 });
  assert.equal(polls(r), reads + 2);
  assert.equal(r.gc.count('wamid.test-wake-during-read'), 1);
});

// Known point (iii): a wake that lands after the safety read that already took
// its delivery trips the rule once. The cost is the short reads before the
// wake lands: floor(lateness / WHATSAPP_POLL_MS).
for (const lateMs of [1000, 3000]) {
  test(`held: a wake ${lateMs / 1000} s after the safety read that took its delivery costs ${Math.floor(lateMs / 2000)} extra short read(s) (gp-urx1)`, async (t) => {
    const r = await setup(t);
    const reads = await heldAndQuiet(r);
    const id = add(r, `late-wake-${lateMs}`);
    await untilSafetyRead(r, reads);
    await acked(r, id);
    assert.equal((await status(r)).link.reason, 'unannounced_delivery');
    const tripped = polls(r);
    const expected = Math.floor(lateMs / 2000);
    await r.pump(lateMs, { stepMs: 500, realMs: 40 });
    // A loaded host may send the read due at 2 s after the clock passed it; wait for it in real time.
    if (expected) await waitFor(() => polls(r) >= tripped + expected, { timeoutMs: 2000, what: 'the short reads' });
    await settled(r);
    await sleep(100);
    const extra = polls(r) - tripped;
    t.diagnostic(`late by ${lateMs} ms: ${extra} extra short read(s) before the wake`);
    assert.equal(extra, expected);
    r.gw.link.wake();
    await waitFor(() => polls(r) === tripped + extra + 1, { what: "the late wake's read" });
    await sleep(100);
    assert.deepEqual(linkView((await status(r)).link), { mode: 'held', state: 'up', reason: '' });
    await r.pump(10000, { stepMs: 1000, realMs: 15 });
    assert.equal(polls(r), tripped + extra + 1);
    assert.equal(r.gc.count(`wamid.test-late-wake-${lateMs}`), 1);
  });
}

// The self-review's MINOR: a read a wake asked for that fails is still owed as
// announced; its retry, a timed read, must not trip the rule.
test('held: the retry of a failed read that a wake asked for is still announced (gp-urx1)', async (t) => {
  const r = await setup(t);
  const reads = await heldAndQuiet(r);
  const id = add(r, 'failed-wake-read');
  r.gw.inboxFailures = 1;
  r.gw.link.wake();
  await waitFor(() => polls(r) === reads + 1, { what: "the wake's read, answered 503" });
  await sleep(100);
  await r.pumpUntil(() => r.gw.acked.has(id), { stepMs: 1000, realMs: 40, what: 'the retry that takes the delivery' });
  await sleep(150); // not settled(): the 503 is a request the box never counts as a read
  assert.deepEqual(linkView((await status(r)).link), { mode: 'held', state: 'up', reason: '' });
  const after = polls(r);
  await r.pump(10000, { stepMs: 1000, realMs: 15 });
  assert.equal(polls(r), after);
  assert.equal(r.gc.count('wamid.test-failed-wake-read'), 1);
});

// Item 2: the box's switch reads off in any case and with spaces; an unknown
// value leaves the link on and is named once in the log at start (ruling B).
test('held: WHATSAPP_HELD_CONNECTION reads off in any case and names an unknown value once at start (gp-urx1)', async (t) => {
  for (const [value, on] of [[' Off ', false], ['OFF', false], ['false', true]]) {
    const r = await setup(t, { WHATSAPP_HELD_CONNECTION: value });
    r.start();
    await waitFor(async () => (await status(r).catch(() => ({}))).polls >= 1);
    if (on) await up(r);
    assert.equal((await status(r)).link.state, on ? 'up' : 'disabled', JSON.stringify(value));
    assert.equal(r.gw.link.attempts.length, on ? 1 : 0, JSON.stringify(value));
    const named = r.adapter.log().split('\n').filter((line) => line.includes('WHATSAPP_HELD_CONNECTION'));
    assert.equal(named.length, on ? 1 : 0, r.adapter.log());
    if (on) assert.ok(named[0].includes(JSON.stringify(value)), named[0]);
    await r.close();
  }
});

// Item 4: a gateway that accepts the upgrade and closes the socket at once.
test('held: a socket closed at once reconnects on the ladder over one clock-minute, not every second (gp-urx1)', async (t) => {
  const r = await setup(t);
  r.gw.link.closeOnOpen = true;
  const start = r.clock.now();
  r.start();
  await waitFor(() => r.gw.link.attempts.length === 1);
  await waitFor(async () => (await status(r).catch(() => ({}))).link?.state === 'down');
  await sleep(100);
  await r.pump(59000, { stepMs: 1000, realMs: 60 }); // clock-seconds 0 to 59
  await sleep(200);
  const at = r.gw.link.attempts.map((attempt) => attempt.at - start);
  const reads = polls(r);
  t.diagnostic(`one clock-minute: upgrades at ${at.join(', ')} ms; inbox reads ${reads}; a polling box reads 30`);
  assert.equal(at.length, 4, `upgrades at ${at.join(', ')}`);
  [1000, 5000, 15000].forEach((rung, i) => assert.ok(at[i + 1] - at[i] >= rung, `upgrades at ${at.join(', ')}`));
  assert.ok(reads <= 38, `${reads} inbox reads`);
});
