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
  add(r, 'dedup');
  r.gw.link.wake();
  await waitFor(() => r.gc.count('wamid.test-dedup') === 1);
  r.gw.link.drop();
  await waitFor(async () => (await status(r)).link.mode === 'poll');
  await sleep(100);
  await r.pump(1000);
  await up(r);
  await r.pump(10000);
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
