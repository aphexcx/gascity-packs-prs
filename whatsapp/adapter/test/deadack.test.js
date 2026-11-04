// THE GATE r7 RULE 2: a dead journal entry is an acknowledgement OWED and is
// kept through every compaction until the gateway confirms the ack; the
// dead-letter file line precedes the dead journal line; the transition is
// idempotent on replay. Against a real child.

import assert from 'node:assert/strict';
import test from 'node:test';

import { adapterRequest, rid, rig, stays, waitFor } from './fakes.js';

const WA = '5511900000001';
const GRACE = 11000;

async function setup(t, extra = {}) {
  const r = await rig({ extra: { WHATSAPP_COMPACT_MS: '1000', ...extra } });
  t.after(() => r.close());
  return r;
}
const lines = (r, pmid, type) => r.journal().filter((l) => l.rid === rid(pmid) && (!type || l.t === type));

// After the dead transition with the ack failing: a compaction, a SIGKILL,
// a restart on a healthy gateway. The dead line survives, the ack is re-sent
// and confirmed, gc sees no new POST, the gateway releases the delivery only
// on that ack.
async function compactKillRestart(t, r, { id, pmid, reason }) {
  await waitFor(() => lines(r, pmid, 'dead').length === 1, { what: `the dead line (${reason})` });
  assert.equal(lines(r, pmid, 'dead')[0].reason, reason);
  await waitFor(() => r.gw.ackCalls.length >= 1, { what: 'a failed ack' });
  const postsAtDead = r.gc.count(pmid);
  const compactions = () => (r.adapter.log().match(/journal compacted \(cadence\)/g) ?? []).length;
  const before = compactions();
  await r.pump(3000);
  await waitFor(() => compactions() > before, { what: 'a compaction after the dead line' });
  assert.equal(lines(r, pmid, 'acknowledged').length, 0, 'no ack confirmed yet');
  assert.equal(lines(r, pmid, 'dead').length, 1, 'the dead line survives the compaction');
  assert.equal(lines(r, pmid, 'received').length, 1, 'with its envelope');
  assert.equal(r.gw.held(id), true, 'the gateway still holds the delivery');
  await r.adapter.kill();
  r.gw.ackBehaviour = ['ok'];
  r.start();
  await waitFor(() => r.gw.acked.has(id), { what: 'the re-driven ack confirmed' });
  await waitFor(() => lines(r, pmid, 'acknowledged').length === 1 || lines(r, pmid).length === 0);
  await r.pump(GRACE * 2, { stepMs: 2000 });
  await stays(() => r.gc.count(pmid) === postsAtDead, { what: 'no new POST' });
  const failed = r.gw.ackCalls.filter((c) => c.behaviour !== 'ok' && c.ids.includes(id)).length;
  t.diagnostic(`${reason}: ${failed} failed ack(s), then the ack confirmed after the restart; POSTs ${postsAtDead}, none after`);
}

test('(12a) a dead-letter transition whose ack fails (500, then nothing), a compaction, a SIGKILL, a restart: the dead line survives, the ack is re-sent and confirmed, no new POST', async (t) => {
  const r = await setup(t);
  r.gw.ackBehaviour = [500, 'hang'];
  r.gc.policy = () => ({ status: 400 });
  const id = r.gw.addText(WA, 'wamid.D1', 'rejected');
  r.start();
  await waitFor(() => r.gw.ackCalls.length >= 1);
  await r.pump(2000);
  await waitFor(() => r.gw.ackCalls.some((c) => c.behaviour === 'hang'), { what: 'the second ack hanging' });
  await compactKillRestart(t, r, { id, pmid: 'wamid.D1', reason: 'rejected' });
  assert.deepEqual(r.gw.ackCalls.map((c) => c.behaviour).slice(0, 2), [500, 'hang']);
  assert.equal(r.gc.count('wamid.D1'), 1);
});

test('(12b) the same across every dead reason', async (t) => {
  const scenarios = {
    async pending_deadline(r) {
      r.gc.policy = () => ({ verdict: 'pending' });
      const id = r.gw.addText(WA, 'wamid.DB-pd', 'x');
      r.start();
      await waitFor(() => lines(r, 'wamid.DB-pd', 'accepted').length === 1);
      await r.pump(25 * 60 * 60000, { stepMs: 60 * 60000, realMs: 20 });
      return { id, pmid: 'wamid.DB-pd' };
    },
    async in_doubt(r) {
      r.gc.policy = () => ({ withhold: true });
      const id = r.gw.addText(WA, 'wamid.DB-id', 'x');
      r.start();
      await waitFor(() => r.gc.count('wamid.DB-id') === 1);
      await r.pump(GRACE);
      await waitFor(() => r.gc.count('wamid.DB-id') === 2);
      await r.pump(GRACE);
      return { id, pmid: 'wamid.DB-id' };
    },
    async redelivery_no_route(r) {
      r.gc.dedupFanout = true;
      r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true } : {});
      const id = r.gw.addText(WA, 'wamid.DB-nr', 'x');
      r.start();
      await waitFor(() => r.gc.count('wamid.DB-nr') === 1);
      await r.pump(GRACE);
      return { id, pmid: 'wamid.DB-nr' };
    },
    async held_terminal(r) {
      r.gc.policy = () => ({ verdict: 'pending', terminal: true });
      const id = r.gw.addText(WA, 'wamid.DB-ht', 'x');
      r.start();
      await waitFor(() => lines(r, 'wamid.DB-ht', 'held_terminal').length === 1);
      const answer = await adapterRequest(r.socket, 'POST', '/inbound/resolve', { rid: rid('wamid.DB-ht'), as: 'dead' });
      assert.equal(answer.status, 200);
      return { id, pmid: 'wamid.DB-ht' };
    },
    async repost_bound(r) {
      r.gc.policy = () => ({ verdict: 'failed' });
      const id = r.gw.addText(WA, 'wamid.DB-rb', 'x');
      r.start();
      await waitFor(() => r.gc.count('wamid.DB-rb') === 1);
      await r.pump(30000, { stepMs: 1000 });
      await waitFor(() => r.gc.count('wamid.DB-rb') === 4, { what: 'four POSTs' });
      return { id, pmid: 'wamid.DB-rb' };
    },
    async rejected(r) {
      r.gc.policy = () => ({ status: 422 });
      const id = r.gw.addText(WA, 'wamid.DB-4x', 'x');
      r.start();
      return { id, pmid: 'wamid.DB-4x' };
    },
    async no_route(r) {
      r.gc.policy = () => ({ noMembers: true });
      const id = r.gw.addText(WA, 'wamid.DB-no', 'x');
      r.start();
      return { id, pmid: 'wamid.DB-no' };
    },
  };
  for (const [reason, run] of Object.entries(scenarios)) {
    await t.test(reason, async (st) => {
      const r = await setup(st);
      r.gw.ackBehaviour = [500];
      const { id, pmid } = await run(r);
      await compactKillRestart(st, r, { id, pmid, reason });
      assert.equal(r.deadLetters(WA).filter((l) => l.rid === rid(pmid)).length, 1, 'one dead-letter line');
    });
  }
});

test('(12c) a kill between the dead-letter file line and the dead journal line: the restart repeats the transition with no second dead-letter line, then acks', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ status: 400 });
  const id = r.gw.addText(WA, 'wamid.D3', 'rejected');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: `between-deadletter-and-dead@${rid('wamid.D3')}` });
  assert.equal((await first.exited).signal, 'SIGKILL');
  assert.equal(r.deadLetters(WA).length, 1, 'the file line is on disk');
  assert.equal(lines(r, 'wamid.D3', 'dead').length, 0, 'the journal line is not');
  r.start();
  await waitFor(() => r.gw.acked.has(id), { what: 'the ack after the repeated transition' });
  assert.equal(r.deadLetters(WA).length, 1, 'no second dead-letter line');
  await r.pump(GRACE * 2, { stepMs: 2000 });
  await stays(() => r.gc.count('wamid.D3') === 1, { what: 'no second POST' });
});
