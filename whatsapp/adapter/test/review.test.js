// Regressions for the self-review r1 findings (assets/pr-evidence/gp-9hf1/
// self-review-r1.md), each against a real child on the fakes.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

import { adapterRequest, rid, rig, stays, waitFor } from './fakes.js';

const A = '5511900000001';
const B = '5511900000002';
const GRACE = 11000;
const MARKER = (pmid) => `[redelivery ${pmid}: a first copy may have reached you; if you already answered it, do nothing]`;

async function setup(t, extra = {}) {
  const r = await rig({ extra });
  t.after(() => r.close());
  return r;
}
const acked = (r, id) => waitFor(() => r.gw.acked.has(id), { what: `the ack of delivery ${id}` });
const lines = (r, pmid, type) => r.journal().filter((l) => l.rid === rid(pmid) && (!type || l.t === type));

function loseJournal(r) {
  const file = path.join(r.dir, '.gc', 'whatsapp', 'inbound_journal.jsonl');
  fs.renameSync(file, `${file}.lost`);
}

test('M1: a journal lost with the box resolves a held delivery by the lookup from zero, never a plain first post', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { verdict: 'pending' } : {});
  const id = r.gw.addText(A, 'wamid.LJ1', 'lost journal');
  r.start();
  await waitFor(() => lines(r, 'wamid.LJ1', 'accepted').length === 1);
  await r.adapter.kill();
  loseJournal(r);
  r.gc.restart();
  r.clock.advance(2 * 60000);
  r.start();
  await r.pump(3000);
  await acked(r, id);
  const posts = r.gc.postsFor('wamid.LJ1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.LJ1')), `never an unmarked repost: ${JSON.stringify(posts[1].text)}`);
  assert.ok(r.gc.requests.some((q) => q.route === 'GET /extmsg/transcript' && q.query.after_sequence === '0' && (q.returned ?? []).includes('wamid.LJ1')),
    'the lookup from zero found the record');
  assert.equal(lines(r, 'wamid.LJ1', 'posted')[0].synthetic, 'before-genesis');
});

test('M1: a dead entry whose ack failed, its journal lost: the dead-letter file line keeps it dead, acked, never posted', async (t) => {
  const r = await setup(t);
  r.gw.ackBehaviour = [500];
  r.gc.policy = () => ({ status: 400 });
  const id = r.gw.addText(A, 'wamid.LJ2', 'rejected once');
  r.start();
  await waitFor(() => lines(r, 'wamid.LJ2', 'dead').length === 1);
  await waitFor(() => r.gw.ackCalls.length >= 1);
  await r.adapter.kill();
  loseJournal(r);
  r.gw.ackBehaviour = ['ok'];
  r.clock.advance(2 * 60000);
  r.start();
  await acked(r, id);
  await r.pump(GRACE * 2, { stepMs: 2000 });
  await stays(() => r.gc.count('wamid.LJ2') === 1, { what: 'never posted again' });
  assert.equal(r.deadLetters(A).length, 1, 'one dead-letter line');
});

test('M1: a delivery that arrives after the journal began is posted plain at once (no lookup)', async (t) => {
  const r = await setup(t);
  r.start();
  await waitFor(() => r.gc.registrations.length === 1);
  const id = r.gw.addText(A, 'wamid.LJ3', 'fresh');
  await acked(r, id);
  assert.equal(r.gc.requests.filter((q) => q.route === 'GET /extmsg/transcript' && q.query.limit === '500').length, 0);
});

test('M2: an inbox page computed before the ack never brings an acknowledged delivery back to the no-entry branch', async (t) => {
  const r = await setup(t, { WHATSAPP_COMPACT_MS: '0' });
  r.gw.inboxDelayMs = 300;
  const id = r.gw.addText(A, 'wamid.RACE', 'race');
  r.start();
  await acked(r, id);
  await stays(() => r.gc.count('wamid.RACE') === 1, { forMs: 1500, what: 'one POST' });
  await waitFor(() => lines(r, 'wamid.RACE').length === 0, { what: 'compacted once a later poll completed' });
  await stays(() => r.gc.count('wamid.RACE') === 1, { forMs: 800, what: 'still one POST after the compaction' });
});

test('Minor 1: the pending deadline reads the receipt first; a conclusion just before it is acted on', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'pending' });
  const id = r.gw.addText(A, 'wamid.DL', 'late conclusion');
  r.start();
  await waitFor(() => lines(r, 'wamid.DL', 'accepted').length === 1);
  const receiptId = lines(r, 'wamid.DL', 'accepted')[0].receipt_id;
  await r.pump(24 * 60 * 60000 - 2 * 60000, { stepMs: 60 * 60000, realMs: 20 });
  r.gc.conclude(receiptId, { status: 'delivered', delivered_bytes: 15, expected_bytes: 15 });
  await r.pump(10 * 60000, { stepMs: 60000 });
  await acked(r, id);
  assert.equal(r.deadLetters(A).length, 0, 'no pending_deadline dead letter');
  assert.equal(lines(r, 'wamid.DL', 'accepted').at(-1).verdict, 'delivered');
});

test('Minor 2: a 2xx without a usable receipt is never acked on; the in-doubt rule decides', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { emptyBody: true } : {});
  const id = r.gw.addText(A, 'wamid.EB', 'empty body');
  r.start();
  await waitFor(() => r.gc.count('wamid.EB') === 1);
  await stays(() => !r.gw.acked.has(id), { forMs: 400, what: 'no ack on an empty 2xx' });
  await r.pump(GRACE);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.EB'), 2);
  assert.ok(r.gc.postsFor('wamid.EB')[1].text.startsWith(MARKER('wamid.EB')));
});

test('Minor 4: gc refusing connections is "not sent": retried on the ladder, posted once when gc is back, never in doubt', async (t) => {
  const r = await setup(t);
  r.start();
  await waitFor(() => r.gc.registrations.length === 1);
  await r.gc.down();
  const id = r.gw.addText(A, 'wamid.CR', 'gc down');
  await waitFor(() => lines(r, 'wamid.CR', 'posted').length >= 1);
  await r.pump(6000);
  await r.gc.up();
  await r.pump(GRACE * 2, { stepMs: 5000 });
  await acked(r, id);
  assert.equal(r.gc.count('wamid.CR'), 1);
  assert.equal(r.gc.postsFor('wamid.CR')[0].text, 'gc down', 'a plain post: it never left before');
  assert.equal(lines(r, 'wamid.CR', 'probe').length, 0, 'no in-doubt probe');
});

test('Minor 6: an entry first posted before a clear-gap is never posted plain after it', async (t) => {
  const r = await setup(t, { WHATSAPP_WATERMARK_CHECK_MS: '2000' });
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.CGX' && i === 0 ? { withhold: true } : {});
  r.start();
  const a0 = r.gw.addText(A, 'wamid.CG0', 'a0');
  await acked(r, a0);
  const backup = r.gc.backup();
  const a1 = r.gw.addText(A, 'wamid.CG1', 'a1');
  await acked(r, a1);
  const x = r.gw.addText(B, 'wamid.CGX', 'x');
  await waitFor(() => r.gc.count('wamid.CGX') === 1);
  r.gc.restore(backup);
  await r.pump(3000);
  await waitFor(() => r.journal().some((l) => l.t === 'restore_gap' && l.conversation_id === A));
  const cleared = await adapterRequest(r.socket, 'POST', '/inbound/clear-gap', { wa_id: A });
  assert.equal(cleared.status, 200);
  await r.pump(GRACE);
  await acked(r, x);
  const posts = r.gc.postsFor('wamid.CGX');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.CGX')), 'the marked path, never plain');
});

test('Minor 7: a transient media failure retries before the post; the message is posted once, with the file', async (t) => {
  const r = await setup(t);
  r.gw.media.set('img-1', { bytes: Buffer.from('jpegbytes'), mime: 'image/jpeg' });
  r.gw.mediaFailures = 2;
  const id = r.gw.addDelivery({
    wa_id: A,
    message: { from: A, id: 'wamid.MT', timestamp: String(Math.floor(r.clock.now() / 1000)), type: 'image', image: { id: 'img-1', mime_type: 'image/jpeg' } },
  });
  r.start();
  await waitFor(() => r.gw.requests.filter((q) => q.route.startsWith('GET /v1/media/')).length >= 1);
  await r.pumpUntil(() => r.gw.acked.has(id), { what: 'the media retried and the message acked' });
  assert.equal(r.gc.count('wamid.MT'), 1);
  const [post] = r.gc.postsFor('wamid.MT');
  assert.match(post.text, /attached/);
  assert.doesNotMatch(post.text, /not saved/);
});

test('Minor 9: acks go in batches of at most 100 ids', async (t) => {
  // A 120-message burst fsyncs 240 journal lines on the child's event loop;
  // a 5 s gc deadline keeps every post answered, so 120 acks pile up owed.
  const r = await setup(t, { WHATSAPP_GC_POST_TIMEOUT_MS: '5000' });
  r.gw.ackMaxIds = 100;
  r.gw.ackBehaviour = [500];
  const ids = [];
  for (let i = 0; i < 120; i++) ids.push(r.gw.addText(`55119000${String(1000 + i)}`, `wamid.AB${i}`, `m${i}`));
  r.start();
  await waitFor(() => r.gc.posts.length === 120, { timeoutMs: 20000, what: '120 posts' });
  await waitFor(() => r.journal().filter((l) => l.t === 'accepted' && l.verdict === 'delivered').length === 120, { timeoutMs: 20000, what: '120 delivered' });
  r.gw.ackBehaviour = ['ok'];
  await r.pump(70000, { stepMs: 5000 });
  await waitFor(() => ids.every((id) => r.gw.acked.has(id)), { timeoutMs: 10000, what: 'every id acked' });
  assert.ok(r.gw.ackCalls.every((c) => c.ids.length <= 100), 'no batch over 100');
});
