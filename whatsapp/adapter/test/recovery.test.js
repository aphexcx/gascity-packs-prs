// Crash recovery against a real child process (the pack-adapter spec's
// SCOPE item 6, kill points (1) to (9)): the adapter runs on the fakes with a
// temp state dir and WHATSAPP_TEST_CLOCK; WHATSAPP_TEST_KILL_AT SIGKILLs it
// at a named point between two journal lines; the restart runs on the same
// dir. The fake gc keeps one transcript record per provider_message_id, as
// gc does, and counts every POST.

import assert from 'node:assert/strict';
import test from 'node:test';

import { adapterRequest, pasteText, rid, rig, sleep, stays, waitFor } from './fakes.js';

const WA = '5511900000001';
const GRACE = 11000; // the client deadline (600 ms in the tests) plus the 10 s margin
const MARKER = (pmid) => `[redelivery ${pmid}: a first copy may have reached you; if you already answered it, do nothing]`;

async function setup(t, extra = {}) {
  const r = await rig({ extra });
  t.after(() => r.close());
  return r;
}

const acked = (r, id) => waitFor(() => r.gw.acked.has(id), { what: `the ack of delivery ${id}` });
const lines = (r, pmid, type) => r.journal().filter((l) => l.rid === rid(pmid) && (!type || l.t === type));

test('(1) killed after the poll and before the received line: the restart re-reads the held delivery; one POST', async (t) => {
  const r = await setup(t);
  const id = r.gw.addText(WA, 'wamid.K1', 'one');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: 'before-received' });
  assert.equal((await first.exited).signal, 'SIGKILL');
  assert.equal(lines(r, 'wamid.K1').length, 0, 'nothing journaled');
  assert.equal(r.gw.held(id), true, 'the gateway still holds it');
  r.start();
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K1'), 1);
  t.diagnostic(`POSTs for wamid.K1: ${r.gc.count('wamid.K1')}`);
});

test('(2) killed after received and before posted: one POST', async (t) => {
  const r = await setup(t);
  const id = r.gw.addText(WA, 'wamid.K2', 'two');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: 'before-posted' });
  assert.equal((await first.exited).signal, 'SIGKILL');
  assert.deepEqual(lines(r, 'wamid.K2').map((l) => l.t), ['received']);
  r.start();
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K2'), 1);
});

test('(3) killed after posted, gc recorded and pasted, before accepted, the log empty: one marked copy, one record, never a third', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true } : {});
  const id = r.gw.addText(WA, 'wamid.K3', 'three');
  r.start();
  await waitFor(() => r.gc.count('wamid.K3') === 1, { what: 'the first POST' });
  await r.adapter.kill();
  assert.deepEqual(lines(r, 'wamid.K3').map((l) => l.t), ['received', 'posted']);
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  const posts = r.gc.postsFor('wamid.K3');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(`${MARKER('wamid.K3')}\nthree`), posts[1].text);
  assert.equal(r.gc.recordsFor('wamid.K3').length, 1, 'gc keeps one transcript record');
  await r.pump(120000, { stepMs: 10000 });
  await stays(() => r.gc.count('wamid.K3') === 2, { what: 'no third POST' });
  t.diagnostic(`marked copy text: ${JSON.stringify(posts[1].text)}`);
});

test('(3b) killed after posted with the request never reaching gc: the probe NOT FOUND, every watermark intact: exactly one plain POST', async (t) => {
  const r = await setup(t);
  const id = r.gw.addText(WA, 'wamid.K3b', 'three b');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: 'after-posted' });
  assert.equal((await first.exited).signal, 'SIGKILL');
  assert.equal(r.gc.count('wamid.K3b'), 0);
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K3b'), 1);
  assert.equal(r.gc.postsFor('wamid.K3b')[0].text, 'three b', 'the plain post carries no marker');
});

test('(3c) as (3) with the paste in the session log as a user turn: acked, exactly one POST', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true, logPaste: true } : {});
  const id = r.gw.addText(WA, 'wamid.K3c', 'three c');
  r.start();
  await waitFor(() => r.gc.count('wamid.K3c') === 1);
  await r.adapter.kill();
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K3c'), 1);
  assert.equal(lines(r, 'wamid.K3c', 'accepted').at(-1)?.via, 'probe');
});

test('(3d) as (3) with NO log and the id only in the PANE (format text): NOT FOUND, one marked copy, never acked on the snapshot', async (t) => {
  const r = await setup(t);
  r.gc.sessionMode.set('s-mayor', 'pane');
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true, panePaste: true } : {});
  const id = r.gw.addText(WA, 'wamid.K3d', 'three d');
  r.start();
  await waitFor(() => r.gc.count('wamid.K3d') === 1);
  await r.adapter.kill();
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K3d'), 2);
  assert.ok(r.gc.postsFor('wamid.K3d')[1].text.startsWith(MARKER('wamid.K3d')));
  const probes = lines(r, 'wamid.K3d', 'probe');
  assert.equal(probes.filter((p) => p.shape === 'pane' && p.found === false).length, 1, 'one snapshot probe journaled');
  assert.equal(lines(r, 'wamid.K3d', 'accepted').some((l) => l.via === 'probe'), false, 'never acked on a snapshot');
});

test('(3e) a tool-result turn, an assistant turn and a later paste whose BODY quotes the id: NOT FOUND, the marked copy', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true } : {});
  const id = r.gw.addText(WA, 'wamid.K3e', 'three e');
  r.start();
  await waitFor(() => r.gc.count('wamid.K3e') === 1);
  await r.adapter.kill();
  const real = pasteText({ conversationId: WA, text: 'three e', pmid: 'wamid.K3e' });
  r.gc.addLogTurn('s-mayor', 'user', `[result] ${real}`);
  r.gc.addLogTurn('s-mayor', 'assistant', `I saw ${real}`);
  r.gc.addLogTurn('s-mayor', 'user', pasteText({ conversationId: WA, text: 'quoting:\nMessage id: wamid.K3e\nend', pmid: 'wamid.LATER' }));
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K3e'), 2);
  assert.ok(r.gc.postsFor('wamid.K3e')[1].text.startsWith(MARKER('wamid.K3e')));
  assert.deepEqual(lines(r, 'wamid.K3e', 'probe').map((p) => [p.shape, p.found]), [['log', false]]);
});

test('(3f) the snapshot probe line survives a SIGKILL, a restart and a compaction (the probes row reads it)', async (t) => {
  const r = await setup(t, { WHATSAPP_COMPACT_MS: '1000' });
  r.gc.sessionMode.set('s-mayor', 'pane');
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true, panePaste: true } : {});
  const id = r.gw.addText(WA, 'wamid.K3f', 'three f');
  r.start();
  await waitFor(() => r.gc.count('wamid.K3f') === 1);
  await r.adapter.kill();
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  await r.adapter.kill();
  r.start();
  await r.pump(3000);
  await waitFor(() => !r.journal().some((l) => l.rid === rid('wamid.K3f') && l.t === 'received'), { what: 'the acked rid compacted away' });
  const probes = r.journal().filter((l) => l.t === 'probe' && l.rid === rid('wamid.K3f'));
  assert.equal(probes.length, 1);
  assert.equal(probes[0].shape, 'pane');
});

test('(4) the response lost without a kill: the entry stays posted; after the grace one marked copy; two POSTs, one record, no third', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true } : {});
  const id = r.gw.addText(WA, 'wamid.K4', 'four');
  r.start();
  await waitFor(() => r.gc.count('wamid.K4') === 1);
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K4'), 2);
  assert.equal(r.gc.recordsFor('wamid.K4').length, 1);
  await r.pump(60000, { stepMs: 10000 });
  await stays(() => r.gc.count('wamid.K4') === 2, { what: 'no third POST' });
});

test('(5) killed after a delivered receipt and before the ack: the restart acks from the journal with no second POST', async (t) => {
  const r = await setup(t);
  const id = r.gw.addText(WA, 'wamid.K5', 'five');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: `before-ack@${rid('wamid.K5')}` });
  assert.equal((await first.exited).signal, 'SIGKILL');
  assert.equal(lines(r, 'wamid.K5', 'accepted')[0].verdict, 'delivered');
  assert.equal(r.gw.held(id), true);
  r.start();
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K5'), 1);
});

test('(6) killed while the receipt was pending; gc resolves it delivered: the restart reads it back, acks, one POST', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'pending' });
  const id = r.gw.addText(WA, 'wamid.K6', 'six');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: `after-accepted@${rid('wamid.K6')}` });
  assert.equal((await first.exited).signal, 'SIGKILL');
  const receiptId = lines(r, 'wamid.K6', 'accepted')[0].receipt_id;
  r.gc.conclude(receiptId, { status: 'delivered', delivered_bytes: 3, expected_bytes: 3, members: [{ session_id: 's-mayor', status: 'delivered', delivered_bytes: 3, expected_bytes: 3 }] });
  assert.equal(r.gw.held(id), true, 'held through the pending');
  r.start();
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K6'), 1);
});

test('(6b) the receipt read back as unknown (gc restarted, transcript kept): the record is found, one marked copy, acked on its delivered receipt', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { verdict: 'pending' } : {});
  const id = r.gw.addText(WA, 'wamid.K6b', 'six b');
  r.start();
  await waitFor(() => lines(r, 'wamid.K6b', 'accepted').length === 1);
  r.gc.restart();
  await r.pump(6000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K6b'), 2);
  assert.ok(r.gc.postsFor('wamid.K6b')[1].text.startsWith(MARKER('wamid.K6b')));
});

test('(6c) the fork head\'s fan-out dedup answers the marked copy no_route (suppression), the log empty: dead-lettered redelivery_no_route, acked, never as delivered', async (t) => {
  const r = await setup(t);
  r.gc.dedupFanout = true;
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true } : {});
  const id = r.gw.addText(WA, 'wamid.K6c', 'six c');
  r.start();
  await waitFor(() => r.gc.count('wamid.K6c') === 1);
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K6c'), 2);
  assert.equal(r.gc.pastes.filter((p) => p.pmid === 'wamid.K6c').length, 1, 'one paste');
  const dead = r.deadLetters(WA).filter((l) => l.rid === rid('wamid.K6c'));
  assert.equal(dead.length, 1);
  assert.equal(dead[0].reason, 'redelivery_no_route');
  assert.match(dead[0].detail, /already delivered.*nobody is routed/);
  assert.equal(dead[0].envelope.message.id, 'wamid.K6c');
  assert.equal(lines(r, 'wamid.K6c', 'accepted').some((l) => l.verdict === 'delivered'), false, 'never acked as delivered');
  assert.equal(lines(r, 'wamid.K6c', 'probe').length, 2, 'probed before the copy and once more on the no_route');
});

test('(6d) a concluded receipt whose delivery is pending stays on the schedule; when gc forgets it the in-doubt rule sends one marked copy', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { verdict: 'pending', concludedPending: true } : {});
  const id = r.gw.addText(WA, 'wamid.K6d', 'six d');
  r.start();
  await waitFor(() => lines(r, 'wamid.K6d', 'accepted').length === 1);
  await r.pump(30000, { stepMs: 5000 });
  assert.equal(r.gw.held(id), true, 'not acked on a concluded pending');
  assert.equal(r.gc.count('wamid.K6d'), 1);
  r.gc.restart();
  await r.pump(6000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K6d'), 2);
  assert.ok(r.gc.postsFor('wamid.K6d')[1].text.startsWith(MARKER('wamid.K6d')));
});

test('(6e) the lost response without a kill never re-posts on the ladder: one POST until the grace has passed', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { withhold: true } : {});
  const id = r.gw.addText(WA, 'wamid.K6e', 'six e');
  r.start();
  await waitFor(() => r.gc.count('wamid.K6e') === 1);
  await r.pump(9000);
  await stays(() => r.gc.count('wamid.K6e') === 1, { forMs: 500, what: 'one POST inside the grace' });
  await r.pump(2000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K6e'), 2);
});

async function acceptedBefore(r, pmid, body) {
  const id = r.gw.addText(WA, pmid, body);
  await acked(r, id);
  return id;
}

test('(7a) the lookup boundary: accepted pending at N with no later message, gc restarted: found from the posted boundary (< N), one unmarked and one marked POST', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.B1' && i === 0 ? { verdict: 'pending' } : {});
  r.start();
  await acceptedBefore(r, 'wamid.B0', 'zero');
  const id = r.gw.addText(WA, 'wamid.B1', 'one');
  await waitFor(() => lines(r, 'wamid.B1', 'accepted').length === 1);
  const n = lines(r, 'wamid.B1', 'accepted')[0].sequence;
  const boundary = lines(r, 'wamid.B1', 'posted')[0].boundary;
  r.gc.restart();
  await r.pump(6000);
  await acked(r, id);
  assert.ok(boundary < n, `boundary ${boundary} < N ${n}`);
  const posts = r.gc.postsFor('wamid.B1');
  assert.equal(posts.length, 2);
  assert.equal(posts[0].text, 'one');
  assert.ok(posts[1].text.startsWith(MARKER('wamid.B1')), 'never an unmarked duplicate');
  t.diagnostic(`posted boundary ${boundary}, accepted sequence N=${n}`);
});

test('(7b) the same with two later messages accepted at N+1 and N+2 before the receipt was lost', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.C1' && i === 0 ? { verdict: 'pending' } : {});
  r.start();
  await acceptedBefore(r, 'wamid.C0', 'zero');
  const id = r.gw.addText(WA, 'wamid.C1', 'one');
  await waitFor(() => lines(r, 'wamid.C1', 'accepted').length === 1);
  await acceptedBefore(r, 'wamid.C2', 'two');
  await acceptedBefore(r, 'wamid.C3', 'three');
  const n = lines(r, 'wamid.C1', 'accepted')[0].sequence;
  assert.deepEqual([lines(r, 'wamid.C2', 'accepted')[0].sequence, lines(r, 'wamid.C3', 'accepted')[0].sequence], [n + 1, n + 2]);
  const boundary = lines(r, 'wamid.C1', 'posted')[0].boundary;
  r.gc.restart();
  await r.pump(6000);
  await acked(r, id);
  assert.ok(boundary < n);
  assert.equal(r.gc.count('wamid.C1'), 2);
  assert.ok(r.gc.postsFor('wamid.C1')[1].text.startsWith(MARKER('wamid.C1')));
  t.diagnostic(`posted boundary ${boundary}, N=${n}, later messages at ${n + 1} and ${n + 2}`);
});

async function noRouteOnMarked(t, { meaning, evidence }) {
  const r = await setup(t);
  const pmid = `wamid.NR-${meaning}-${evidence}`;
  r.gc.dedupFanout = meaning === 'suppression';
  r.gc.policy = (p, m, i) => {
    if (i === 0) return { withhold: true };
    // The first paste lands late, by the time the marked copy goes.
    if (evidence === 'log') r.gc.logPaste('s-mayor', { conversationId: WA, text: 'late', pmid });
    if (evidence === 'pane') r.gc.panes.set('s-mayor', pasteText({ conversationId: WA, text: 'late', pmid }));
    return meaning === 'empty' ? { noMembers: true } : {};
  };
  if (evidence === 'pane') r.gc.sessionMode.set('s-mayor', 'pane');
  const id = r.gw.addText(WA, pmid, 'late');
  r.start();
  await waitFor(() => r.gc.count(pmid) === 1);
  if (meaning === 'empty') r.gc.restart(); // gc forgot the first fan-out
  await r.pump(GRACE);
  await acked(r, id);
  return { r, pmid, id };
}

test('(7c) no_route on the marked copy, the suppression meaning, the log CARRYING the paste: acked as delivered, no dead letter, two POSTs, one paste', async (t) => {
  const { r, pmid } = await noRouteOnMarked(t, { meaning: 'suppression', evidence: 'log' });
  assert.equal(r.gc.count(pmid), 2);
  assert.equal(r.gc.pastes.filter((p) => p.pmid === pmid).length, 1);
  assert.equal(r.deadLetters(WA).length, 0);
  assert.equal(lines(r, pmid, 'accepted').at(-1).via, 'probe');
});

test('(7d) the empty-membership meaning: the same outcome, and the two no_route answers byte-identical but for the receipt id', async (t) => {
  const a = await noRouteOnMarked(t, { meaning: 'suppression', evidence: 'log' });
  const b = await noRouteOnMarked(t, { meaning: 'empty', evidence: 'log' });
  assert.equal(b.r.gc.count(b.pmid), 2);
  assert.equal(b.r.deadLetters(WA).length, 0);
  assert.equal(lines(b.r, b.pmid, 'accepted').at(-1).via, 'probe');
  const wire = (x) => {
    const answer = x.r.gc.postsFor(x.pmid)[1].answer;
    const d = { ...answer.delivery };
    delete d.receipt_id;
    return JSON.stringify({ delivery: d, Duplicate: answer.Duplicate });
  };
  assert.equal(wire(a), wire(b), 'nothing on the wire tells the two meanings apart');
  t.diagnostic(`both no_route answers: ${wire(a)}`);
});

test('(7e) as (7c) with the id only in the PANE: NOT FOUND, dead-lettered redelivery_no_route, acked, never as delivered', async (t) => {
  const { r, pmid } = await noRouteOnMarked(t, { meaning: 'suppression', evidence: 'pane' });
  const dead = r.deadLetters(WA);
  assert.equal(dead.length, 1);
  assert.equal(dead[0].reason, 'redelivery_no_route');
  assert.equal(lines(r, pmid, 'accepted').some((l) => l.verdict === 'delivered'), false);
  assert.ok(lines(r, pmid, 'probe').some((p) => p.shape === 'pane'), 'the probes row counts the snapshot');
});

test('(8) a failure concluding after one hour: read inside the hour after the conclusion, reposted plain, two POSTs, acked', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { verdict: 'pending' } : {});
  const id = r.gw.addText(WA, 'wamid.K8', 'eight');
  r.start();
  await waitFor(() => lines(r, 'wamid.K8', 'accepted').length === 1);
  const receiptId = lines(r, 'wamid.K8', 'accepted')[0].receipt_id;
  await r.pump(90 * 60000, { stepMs: 60000, realMs: 15 });
  assert.equal(r.gw.held(id), true);
  r.gc.conclude(receiptId, { status: 'failed', delivered_bytes: 0, expected_bytes: 5, members: [{ session_id: 's-mayor', status: 'failed', delivered_bytes: 0, expected_bytes: 5 }] });
  await r.pump(10 * 60000, { stepMs: 30000 });
  await acked(r, id);
  const posts = r.gc.postsFor('wamid.K8');
  assert.equal(posts.length, 2);
  assert.equal(posts[1].text, 'eight', 'a plain repost on gc\'s explicit failed word');
  t.diagnostic('pending 90 min of advanced clock, concluded failed, reposted inside the hour after');
});

test('(8b) the same with the adapter killed through the conclusion and restarted 70 min after it: unknown, the record from the posted boundary, one marked copy', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { verdict: 'pending' } : {});
  const id = r.gw.addText(WA, 'wamid.K8b', 'eight b');
  r.start();
  await waitFor(() => lines(r, 'wamid.K8b', 'accepted').length === 1);
  const receiptId = lines(r, 'wamid.K8b', 'accepted')[0].receipt_id;
  await r.pump(30 * 60000, { stepMs: 60000, realMs: 15 });
  await r.adapter.kill();
  r.clock.advance(60 * 60000);
  r.gc.conclude(receiptId, { status: 'failed', delivered_bytes: 0, expected_bytes: 7 });
  r.clock.advance(70 * 60000);
  r.gc.receipts.delete(receiptId); // gc keeps a conclusion one hour
  r.start();
  await r.pump(2000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.K8b'), 2);
  assert.ok(r.gc.postsFor('wamid.K8b')[1].text.startsWith(MARKER('wamid.K8b')));
});

test('(8c) the terminal hold, then unknown, then 24 h: no further POST, no read-back, the gateway holds it; resolve --dead and --delivered', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid) => (pmid.startsWith('wamid.T') ? { verdict: 'pending', terminal: true } : {});
  const id1 = r.gw.addText(WA, 'wamid.T1', 'held one');
  const id2 = r.gw.addText(WA, 'wamid.T2', 'held two');
  r.start();
  await waitFor(() => lines(r, 'wamid.T1', 'held_terminal').length === 1 && lines(r, 'wamid.T2', 'held_terminal').length === 1);
  r.gc.restart();
  await r.pump(25 * 60 * 60000, { stepMs: 30 * 60000, realMs: 20 });
  assert.equal(r.gc.count('wamid.T1') + r.gc.count('wamid.T2'), 2, 'no additional POST');
  assert.equal(r.gc.requests.filter((q) => q.route.includes('receipts')).length, 0, 'no read-back of a terminal hold');
  assert.equal(r.gw.held(id1) && r.gw.held(id2), true);
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.deepEqual(status.json.inbound.held.map((h) => h.rid).sort(), [rid('wamid.T1'), rid('wamid.T2')]);
  const dead = await adapterRequest(r.socket, 'POST', '/inbound/resolve', { rid: rid('wamid.T1'), as: 'dead' });
  assert.equal(dead.status, 200);
  assert.equal(dead.json.wa_id_last4, '0001');
  await acked(r, id1);
  assert.equal(r.deadLetters(WA).find((l) => l.rid === rid('wamid.T1')).reason, 'held_terminal');
  const delivered = await adapterRequest(r.socket, 'POST', '/inbound/resolve', { rid: rid('wamid.T2'), as: 'delivered' });
  assert.equal(delivered.status, 200);
  await acked(r, id2);
  assert.equal(r.deadLetters(WA).some((l) => l.rid === rid('wamid.T2')), false, '--delivered writes no dead letter');
  assert.equal(r.gc.count('wamid.T1') + r.gc.count('wamid.T2'), 2);
});

test('(8d) the hold across a SIGKILL, and a kill between the accepted (terminal) line and the held_terminal line, restarted 70 min later', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'pending', terminal: true });
  const id = r.gw.addText(WA, 'wamid.T3', 'held three');
  const first = r.start({ WHATSAPP_TEST_KILL_AT: `after-accepted@${rid('wamid.T3')}` });
  assert.equal((await first.exited).signal, 'SIGKILL');
  assert.deepEqual(lines(r, 'wamid.T3').map((l) => l.t), ['received', 'posted', 'accepted']);
  r.gc.restart();
  r.clock.advance(70 * 60000);
  r.start();
  await r.pump(GRACE * 3);
  await stays(() => r.gc.count('wamid.T3') === 1, { what: 'no POST' });
  assert.equal(r.gc.requests.filter((q) => q.route.includes('receipts') || q.query.limit === '500').length, 0, 'no read-back, no lookup');
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.deepEqual(status.json.inbound.held.map((h) => h.rid), [rid('wamid.T3')]);
  await r.adapter.kill();
  r.start();
  await r.pump(25 * 60 * 60000, { stepMs: 60 * 60000, realMs: 20 });
  assert.equal(r.gc.count('wamid.T3'), 1);
  assert.equal(r.gw.held(id), true);
});

test('(9) the pending deadline: pending 24 h across a kill and restart; dead-lettered pending_deadline with the read count; held until that ack; one POST', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'pending' });
  const id = r.gw.addText(WA, 'wamid.K9', 'nine');
  r.start();
  await waitFor(() => lines(r, 'wamid.K9', 'accepted').length === 1);
  const receiptId = lines(r, 'wamid.K9', 'accepted')[0].receipt_id;
  await r.pump(12 * 60 * 60000, { stepMs: 20 * 60000, realMs: 20 });
  assert.equal(r.gw.held(id), true);
  await r.adapter.kill();
  r.start();
  await sleep(150);
  const readsBefore = r.gc.requests.filter((q) => q.route.includes('receipts')).length;
  await r.pump(13 * 60 * 60000, { stepMs: 20 * 60000, realMs: 20 });
  await acked(r, id);
  assert.ok(r.gc.requests.filter((q) => q.route.includes('receipts')).length > readsBefore, 'the restart kept reading it back');
  const dead = r.deadLetters(WA).find((l) => l.rid === rid('wamid.K9'));
  assert.equal(dead.reason, 'pending_deadline');
  assert.equal(dead.receipt_id, receiptId);
  assert.ok(dead.read_count > 0 && dead.last_read_at, 'read count and last read time recorded');
  assert.equal(dead.envelope.message.text.body, 'nine');
  r.gc.conclude(receiptId, { status: 'delivered', delivered_bytes: 4, expected_bytes: 4 });
  const after = r.gc.requests.length;
  await r.pump(30 * 60000, { stepMs: 5 * 60000 });
  assert.equal(r.gc.requests.slice(after).filter((q) => q.route.includes('receipts')).length, 0, 'a later conclusion is never read');
  assert.equal(r.gc.count('wamid.K9'), 1);
  t.diagnostic(`dead letter: reason ${dead.reason}, read_count ${dead.read_count}`);
});
