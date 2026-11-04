// THE RESTORE RULE (the gate r6's second finding, design round 7) and THE
// ALL-WATERMARK READ (the gate r7's first finding, design round 8), against
// a real child and a fake gc restored to a backup: the records after the
// backup dropped, each conversation's next_sequence set back, the receipt
// store emptied.

import assert from 'node:assert/strict';
import test from 'node:test';

import { adapterRequest, pasteText, rid, rig, stays, waitFor } from './fakes.js';

const A = '5511900000001';
const B = '5511900000002';
const GRACE = 16000; // the 5 s test client deadline plus the 10 s margin, and a second
const MARKER = (pmid) => `[redelivery ${pmid}: a first copy may have reached you; if you already answered it, do nothing]`;

async function setup(t, extra = {}) {
  const r = await rig({ extra });
  t.after(() => r.close());
  return r;
}
const acked = (r, id) => waitFor(() => r.gw.acked.has(id), { what: `the ack of delivery ${id}` });
const lines = (r, pmid, type) => r.journal().filter((l) => l.rid === rid(pmid) && (!type || l.t === type));
// The gap as it stands: the detection's line, then the same gap again with
// the head once read (the codex gate r1's first finding on PR 74).
const gapLine = (r, conversationId) => r.journal().findLast((l) => l.t === 'restore_gap' && l.conversation_id === conversationId);
const indexOf = (r, predicate) => r.journal().findIndex(predicate);

async function deliver(r, wa, pmid, body = pmid) {
  const id = r.gw.addText(wa, pmid, body);
  await acked(r, id);
  return id;
}

test('(11a) accepted pending at N, the store restored below it, the log empty: no plain post; the gap on disk BEFORE the one marked copy', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.R1' && i === 0 ? { verdict: 'pending' } : {});
  r.start();
  await deliver(r, A, 'wamid.R0');
  const backup = r.gc.backup();
  const id = r.gw.addText(A, 'wamid.R1', 'one');
  await waitFor(() => lines(r, 'wamid.R1', 'accepted').length === 1);
  const n = lines(r, 'wamid.R1', 'accepted')[0].sequence;
  r.gc.restore(backup);
  await r.pumpUntil(() => r.gw.acked.has(id), { what: 'the read-back resolved and acked' });
  const posts = r.gc.postsFor('wamid.R1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.R1')), 'the marked path, never a plain post');
  const gap = gapLine(r, A);
  assert.equal(gap.watermark_sequence, n);
  assert.equal(gap.head_sequence, 1);
  const gapAt = indexOf(r, (l) => l.t === 'restore_gap' && l.conversation_id === A);
  const markedAt = indexOf(r, (l) => l.rid === rid('wamid.R1') && l.t === 'posted' && l.marked);
  assert.ok(gapAt >= 0 && gapAt < markedAt, `the gap line (${gapAt}) precedes the marked copy's posted line (${markedAt})`);
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.equal(status.json.inbound.gaps.length, 1);
  t.diagnostic(`watermark N=${n}, store head after the restore ${gap.head_sequence}`);
});

test('(11b) as (11a) with the paste in the surviving session log: acked with no further POST, the gap still shown', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.R1b' && i === 0 ? { verdict: 'pending', logPaste: true } : {});
  r.start();
  await deliver(r, A, 'wamid.R0b');
  const backup = r.gc.backup();
  const id = r.gw.addText(A, 'wamid.R1b', 'one b');
  await waitFor(() => lines(r, 'wamid.R1b', 'accepted').length === 1);
  r.gc.restore(backup);
  r.gc.logPaste('s-mayor', { conversationId: A, text: 'one b', pmid: 'wamid.R1b' }); // the session survived
  await r.pumpUntil(() => r.gw.acked.has(id), { what: 'the read-back resolved and acked' });
  assert.equal(r.gc.count('wamid.R1b'), 1);
  assert.ok(gapLine(r, A), 'the gap is still shown');
});

test('(11c) the store re-issued sequence N to another id: the watermark reads behind, the same outcome as (11a)', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.R1c' && i === 0 ? { verdict: 'pending' } : {});
  r.start();
  await deliver(r, A, 'wamid.R0c');
  const backup = r.gc.backup();
  const id = r.gw.addText(A, 'wamid.R1c', 'one c');
  await waitFor(() => lines(r, 'wamid.R1c', 'accepted').length === 1);
  const n = lines(r, 'wamid.R1c', 'accepted')[0].sequence;
  r.gc.restore(backup);
  await deliver(r, A, 'wamid.R2c'); // re-issued sequence N
  assert.equal(r.gc.records(A).find((x) => x.Sequence === n).ProviderMessageID, 'wamid.R2c');
  await r.pumpUntil(() => r.gw.acked.has(id), { what: 'the read-back resolved and acked' });
  assert.equal(r.gc.count('wamid.R1c'), 2);
  assert.ok(r.gc.postsFor('wamid.R1c')[1].text.startsWith(MARKER('wamid.R1c')));
  assert.ok(gapLine(r, A));
});

test('(11d) a never-accepted entry in a conversation whose watermark reads behind: the marked path, two POSTs, the second marked', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.R1d' && i === 0 ? { withhold: true } : {});
  r.start();
  const backup = r.gc.backup(); // before anything: the store empty
  await deliver(r, A, 'wamid.R0d');
  const id = r.gw.addText(A, 'wamid.R1d', 'one d');
  await waitFor(() => r.gc.count('wamid.R1d') === 1);
  r.gc.restore(backup);
  await r.pump(GRACE);
  await acked(r, id);
  const posts = r.gc.postsFor('wamid.R1d');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.R1d')));
  assert.ok(gapLine(r, A));
});

test('(11d2) a never-accepted entry of B, B\'s watermark intact, while A\'s gap stands: the marked path, two POSTs, the second marked', async (t) => {
  const r = await setup(t, { WHATSAPP_WATERMARK_CHECK_MS: '2000' });
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.B1' && i === 0 ? { withhold: true } : {});
  r.start();
  await deliver(r, A, 'wamid.A0');
  await deliver(r, B, 'wamid.B0');
  const backup = r.gc.backup();
  await deliver(r, A, 'wamid.A1');
  const id = r.gw.addText(B, 'wamid.B1', 'b one');
  await waitFor(() => r.gc.count('wamid.B1') === 1);
  r.gc.restore(backup);
  await r.pumpUntil(() => gapLine(r, A), { what: 'the scheduled read finds A\'s gap' });
  assert.equal(gapLine(r, B), undefined, 'B intact');
  await r.pump(GRACE);
  await acked(r, id);
  const posts = r.gc.postsFor('wamid.B1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.B1')));
});

test('(11e) a never-accepted entry, every watermark intact and the probe NOT FOUND: the plain post, exactly one POST', async (t) => {
  const r = await setup(t);
  r.start();
  await deliver(r, A, 'wamid.E0');
  const id = r.gw.addText(A, 'wamid.E1', 'e one');
  await r.adapter.kill();
  r.start({ WHATSAPP_TEST_KILL_AT: `after-posted@${rid('wamid.E1')}` });
  assert.equal((await r.adapter.exited).signal, 'SIGKILL');
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.E1'), 1);
  assert.equal(r.gc.postsFor('wamid.E1')[0].text, 'e one');
});

test('(11f) the watermark survives compaction: an acked rid compacted away leaves the watermark, and a later restore still reads the gap', async (t) => {
  const r = await setup(t, { WHATSAPP_COMPACT_MS: '1000' });
  r.start();
  const backup = r.gc.backup();
  await deliver(r, A, 'wamid.F0');
  await r.pumpUntil(() => lines(r, 'wamid.F0').length === 0, { what: 'the acked rid compacted away' });
  assert.equal(r.journal().filter((l) => l.t === 'watermark' && l.conversation_id === A).at(-1).sequence, 1);
  r.gc.restore(backup);
  await r.pumpUntil(() => gapLine(r, A), { stepMs: 5000, what: 'the gap read after the compaction' });
});

test('(11g) clear-gap prints the gap and the head, rebases the watermark; a SIGKILL and restart read no gap; a later message raises it as before', async (t) => {
  const r = await setup(t);
  r.start();
  await deliver(r, A, 'wamid.G0');
  const backup = r.gc.backup();
  await deliver(r, A, 'wamid.G1');
  r.gc.restore(backup);
  await r.pumpUntil(() => gapLine(r, A), { stepMs: 5000, what: 'the scheduled read finds the gap' });
  const cleared = await adapterRequest(r.socket, 'POST', '/inbound/clear-gap', { wa_id: A });
  assert.equal(cleared.status, 200);
  assert.equal(cleared.json.gap.watermark_sequence, 2);
  assert.equal(cleared.json.head.sequence, 1);
  const rebased = r.journal().filter((l) => l.t === 'watermark' && l.conversation_id === A).at(-1);
  assert.equal(rebased.sequence, 1);
  assert.equal(rebased.provider_message_id, 'wamid.G0');
  assert.ok(rebased.rebased_at);
  assert.equal((await adapterRequest(r.socket, 'GET', '/status')).json.inbound.gaps.length, 0, 'the restore row reads green');
  await r.adapter.kill();
  r.start();
  // The cleared gap's lines leave the file at the restarted adapter's
  // startup compaction; a loaded host can boot the child after any fixed pump.
  await waitFor(() => r.journal().filter((l) => l.t === 'restore_gap').length === 0, { what: 'the restart compacted the cleared gap away' });
  await r.pump(61000, { stepMs: 5000 });
  await stays(() => r.journal().filter((l) => l.t === 'restore_gap').length === 0, { what: 'no gap after the clear' });
  await deliver(r, A, 'wamid.G2');
  assert.equal(r.journal().filter((l) => l.t === 'watermark' && l.conversation_id === A).at(-1).sequence, 2);
  assert.equal(r.journal().filter((l) => l.t === 'watermark' && l.conversation_id === A).at(-1).provider_message_id, 'wamid.G2');
  const empty = await adapterRequest(r.socket, 'POST', '/inbound/clear-gap', { wa_id: A });
  assert.equal(empty.status, 404, 'no gap to clear');
});

test('(11h) THE GATE r7 RULE 1: a gc-only restore WHILE THE ADAPTER RUNS drops A\'s newer record, B\'s older watermark intact: B\'s in-doubt entry takes the marked path, A\'s gap on disk before B\'s POST', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.HB1' && i === 0 ? { withhold: true } : {});
  r.start();
  await deliver(r, A, 'wamid.HA0');
  await deliver(r, B, 'wamid.HB0');
  const backup = r.gc.backup(); // after B's watermark, before A's newer one
  await deliver(r, A, 'wamid.HA1');
  const id = r.gw.addText(B, 'wamid.HB1', 'delivered, the response lost');
  await waitFor(() => r.gc.count('wamid.HB1') === 1);
  r.gc.restore(backup); // gc alone, the adapter running
  await r.pump(GRACE);
  await acked(r, id);
  const posts = r.gc.postsFor('wamid.HB1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.HB1')), `never an unmarked duplicate: ${JSON.stringify(posts[1].text)}`);
  const gap = gapLine(r, A);
  assert.ok(gap, 'A\'s gap found by the all-watermark read');
  assert.equal(gapLine(r, B), undefined, 'B\'s own watermark reads intact');
  const gapAt = indexOf(r, (l) => l.t === 'restore_gap' && l.conversation_id === A);
  const bPostAt = indexOf(r, (l) => l.rid === rid('wamid.HB1') && l.t === 'posted' && l.marked);
  assert.ok(gapAt >= 0 && gapAt < bPostAt, `A's gap (${gapAt}) is on disk before B's POST (${bPostAt})`);
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.deepEqual(status.json.inbound.gaps.map((g) => g.conversation_id), ['...0001']);
  t.diagnostic(`A gap: watermark ${gap.watermark_sequence} vs store head ${gap.head_sequence}; B's second POST marked`);
});

test('(11i) the all-watermark read is evidenced before (11e)\'s plain post: every conversation\'s watermark read after the probe', async (t) => {
  const r = await setup(t);
  r.start();
  await deliver(r, A, 'wamid.IA0');
  await deliver(r, B, 'wamid.IB0');
  const id = r.gw.addText(B, 'wamid.IB1', 'i one');
  await r.adapter.kill();
  r.start({ WHATSAPP_TEST_KILL_AT: `after-posted@${rid('wamid.IB1')}` });
  assert.equal((await r.adapter.exited).signal, 'SIGKILL');
  r.start();
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.IB1'), 1, 'the plain post, exactly once');
  const before = r.gc.inboundRequestsBefore('wamid.IB1');
  const probeAt = before.map((q) => q.route).lastIndexOf('GET /session/{id}/transcript');
  assert.ok(probeAt >= 0, 'the probe ran');
  const reads = before.slice(probeAt + 1).filter((q) => q.route === 'GET /extmsg/transcript' && q.query.limit === '1' && q.query.order === 'asc');
  assert.deepEqual([...new Set(reads.map((q) => q.query.conversation_id))].sort(), [A, B], 'A\'s watermark is read too, after the probe and before the post');
  t.diagnostic(`watermark reads between the probe and the plain POST: ${reads.map((q) => `${q.query.conversation_id.slice(-4)}@${q.query.after_sequence}`).join(', ')}`);
});

test('(11j) the scheduled read finds a restore with no in-doubt entry anywhere and shows the gap', async (t) => {
  const r = await setup(t);
  r.start();
  const backup = r.gc.backup();
  await deliver(r, A, 'wamid.J0');
  await deliver(r, B, 'wamid.J1');
  r.gc.restore(backup);
  assert.equal(r.journal().some((l) => l.t === 'restore_gap'), false);
  await r.pumpUntil(() => gapLine(r, A) && gapLine(r, B), { stepMs: 5000, what: 'both gaps from the scheduled read' });
  assert.equal(gapLine(r, A).found_by, 'schedule');
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.equal(status.json.inbound.gaps.length, 2);
});
