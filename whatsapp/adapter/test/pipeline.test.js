// The inbound pipeline's ordinary cases, against a real child on the fakes.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

import { adapterRequest, rid, rig, stays, waitFor } from './fakes.js';

const X = '5511900000001';
const Y = '5511900000002';

async function setup(t, extra = {}) {
  const r = await rig({ extra });
  t.after(() => r.close());
  return r;
}
const acked = (r, id) => waitFor(() => r.gw.acked.has(id), { what: `the ack of delivery ${id}` });
const lines = (r, pmid, type) => r.journal().filter((l) => l.rid === rid(pmid) && (!type || l.t === type));

test('registers with gc as provider whatsapp, account dynasty, with the reply instructions', async (t) => {
  const r = await setup(t);
  r.start();
  await waitFor(() => r.gc.registrations.length === 1);
  const reg = r.gc.registrations[0];
  assert.equal(reg.provider, 'whatsapp');
  assert.equal(reg.account_id, 'dynasty');
  assert.equal(reg.callback_url, `${r.gc.url}/v0/city/test-city/svc/whatsapp`);
  assert.match(reg.reply_instructions, /gc whatsapp publish --to \{conversation_id\} --text-file/);
  assert.equal(reg.envelope_format, undefined, 'the long envelope format by omission (the evidence rule reads it)');
});

test('on gc\'s first acceptance the owner\'s message is marked read (POST /v1/read), once, never for a status', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { verdict: 'failed' } : {});
  const id = r.gw.addText(X, 'wamid.RD', 'read me');
  r.start();
  await waitFor(() => r.gc.count('wamid.RD') === 1);
  await r.pump(2000);
  await acked(r, id);
  assert.deepEqual(r.gw.reads, ['wamid.RD'], 'one read receipt across the repost');
});

test('two senders deliver in order within each sender and independently across senders', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.X1' && i === 0 ? { status: 500 } : {});
  const x1 = r.gw.addText(X, 'wamid.X1', 'x one');
  const x2 = r.gw.addText(X, 'wamid.X2', 'x two');
  const y1 = r.gw.addText(Y, 'wamid.Y1', 'y one');
  const y2 = r.gw.addText(Y, 'wamid.Y2', 'y two');
  r.start();
  await acked(r, y1);
  await acked(r, y2);
  assert.equal(r.gc.count('wamid.X2'), 0, 'X2 waits behind X1 while Y proceeds');
  await r.pump(1000);
  await acked(r, x1);
  await acked(r, x2);
  const order = r.gc.posts.map((p) => p.pmid);
  assert.ok(order.lastIndexOf('wamid.X1') < order.indexOf('wamid.X2'));
  assert.ok(order.indexOf('wamid.Y1') < order.indexOf('wamid.Y2'));
});

test('a 500 from gc retries on the ladder and then delivers once', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i === 0 ? { status: 500 } : {});
  const id = r.gw.addText(X, 'wamid.P5', 'five hundred');
  r.start();
  await waitFor(() => r.gc.count('wamid.P5') === 1);
  await stays(() => r.gc.count('wamid.P5') === 1, { what: 'waiting on the 1 s rung' });
  await r.pump(1000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.P5'), 2);
  assert.equal(r.gc.pastes.filter((p) => p.pmid === 'wamid.P5').length, 1, 'delivered once');
});

test('a 404 retries (gc answers it during a city restart)', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid, m, i) => (i < 2 ? { status: 404 } : {});
  const id = r.gw.addText(X, 'wamid.P4', 'four oh four');
  r.start();
  await waitFor(() => r.gc.count('wamid.P4') === 1);
  await r.pump(7000);
  await acked(r, id);
  assert.equal(r.gc.count('wamid.P4'), 3);
});

test('a 400 dead-letters one 0600 line with the envelope and the chain moves on', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid) => (pmid === 'wamid.Q1' ? { status: 400 } : {});
  const q1 = r.gw.addText(X, 'wamid.Q1', 'bad');
  const q2 = r.gw.addText(X, 'wamid.Q2', 'good');
  r.start();
  await acked(r, q1);
  await acked(r, q2);
  const file = path.join(r.dir, '.gc', 'whatsapp', 'inbound_dead_letter', `${X}.jsonl`);
  const dead = fs.readFileSync(file, 'utf8').trim().split('\n').map((l) => JSON.parse(l));
  assert.equal(dead.length, 1);
  assert.equal(dead[0].reason, 'rejected');
  assert.equal(dead[0].envelope.message.text.body, 'bad');
  assert.equal((fs.statSync(file).mode & 0o777).toString(8), '600');
  assert.equal(r.gc.count('wamid.Q1'), 1, 'never retried');
});

test('the ack names delivery ids, follows the accepted line on disk, and never comes on a pending verdict', async (t) => {
  const r = await setup(t);
  r.gc.policy = (pmid) => (pmid === 'wamid.A1' ? { verdict: 'pending' } : {});
  const a1 = r.gw.addText(X, 'wamid.A1', 'pending one'); // sender X, id 1
  const b1 = r.gw.addText(Y, 'wamid.B1', 'delivered one'); // sender Y, id 2
  r.start();
  await acked(r, b1);
  assert.deepEqual(r.gw.ackCalls.flatMap((c) => c.ids), [b1], 'the later id acked alone, never cumulatively');
  await r.pump(60000, { stepMs: 5000 });
  assert.equal(r.gw.held(a1), true, 'the pending one is never acked');
  const receiptId = lines(r, 'wamid.A1', 'accepted')[0].receipt_id;
  r.gc.conclude(receiptId, { status: 'delivered', delivered_bytes: 11, expected_bytes: 11 });
  await r.pump(5000);
  await acked(r, a1);
  assert.deepEqual(r.gw.ackCalls.map((c) => c.ids), [[b1], [a1]], 'each id as it confirms');
  await waitFor(() => lines(r, 'wamid.A1', 'acknowledged').length === 1, { what: 'the acknowledged line after the confirmed ack' });
  const j = r.journal().filter((l) => l.rid === rid('wamid.A1')).map((l) => l.t);
  assert.ok(j.lastIndexOf('accepted') < j.indexOf('acknowledged'));
});

test('a failed-status delivery posts as <wamid>:status:failed beside the outbound record; other statuses are acked without a POST', async (t) => {
  const r = await setup(t);
  r.gc.records(X).push({ Sequence: 1, Kind: 'outbound', ProviderMessageID: 'wamid.out.1', Text: 'the mayor wrote', CreatedAt: new Date().toISOString() });
  r.gc.conversations.get(X).nextSequence = 2;
  r.gw.bound.add(X);
  const read = r.gw.addDelivery({ kind: 'status', wa_id: X, message: { id: 'wamid.out.1', status: 'read', timestamp: '1793815924', recipient_id: X } });
  const failed = r.gw.addDelivery({ kind: 'status', wa_id: X, message: { id: 'wamid.out.1', status: 'failed', timestamp: '1793815925', recipient_id: X, errors: [{ code: 131047, title: 'Re-engagement message' }] } });
  r.start();
  await acked(r, read);
  await acked(r, failed);
  assert.equal(r.gc.count('wamid.out.1:status:read'), 0);
  assert.equal(r.gc.count('wamid.out.1:status:failed'), 1);
  assert.deepEqual(r.gc.records(X).map((rec) => [rec.Kind, rec.ProviderMessageID]), [['outbound', 'wamid.out.1'], ['inbound', 'wamid.out.1:status:failed']]);
  assert.equal(r.gc.postsFor('wamid.out.1:status:failed')[0].text, '[delivery failed: 131047 Re-engagement message]');
});

for (const verdict of ['failed', 'partial']) {
  test(`a ${verdict} receipt reposts three times, then dead-letters and acks: four POSTs in all`, async (t) => {
    const r = await setup(t);
    r.gc.policy = () => ({ verdict });
    const id = r.gw.addText(X, `wamid.${verdict}`, verdict);
    r.start();
    await waitFor(() => r.gc.count(`wamid.${verdict}`) === 1);
    await r.pump(25000, { stepMs: 1000 });
    await acked(r, id);
    assert.equal(r.gc.count(`wamid.${verdict}`), 4);
    assert.equal(r.deadLetters(X)[0].reason, 'repost_bound');
    assert.deepEqual(lines(r, `wamid.${verdict}`, 'posted').map((p) => p.reposts), [0, 1, 2, 3]);
  });
}

test('a no_route receipt dead-letters the envelope and acks at once with no repost', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ noMembers: true });
  const id = r.gw.addText(X, 'wamid.NR', 'nobody routed');
  r.start();
  await acked(r, id);
  assert.equal(r.gc.count('wamid.NR'), 1);
  assert.equal(r.deadLetters(X)[0].reason, 'no_route');
});

test('every acknowledged entry is compacted away; the watermark stays', async (t) => {
  const r = await setup(t, { WHATSAPP_COMPACT_MS: '1000' });
  const id = r.gw.addText(X, 'wamid.CP', 'compact me');
  r.start();
  await acked(r, id);
  await r.pumpUntil(() => lines(r, 'wamid.CP').length === 0, { what: 'the rid compacted away' });
  assert.equal(r.journal().filter((l) => l.t === 'watermark' && l.conversation_id === X).length, 1);
});

test('a 401 stops polling and sets the doctor fact', async (t) => {
  const r = await setup(t);
  r.gw.revoked = true;
  r.start();
  await waitFor(() => r.gw.requests.some((q) => q.route === 'GET /v1/inbox'));
  const health = await waitFor(async () => {
    const answer = await adapterRequest(r.socket, 'GET', '/healthz').catch(() => undefined);
    return answer?.json?.gateway_auth && answer.json.gateway_auth !== 'ok' ? answer : undefined;
  });
  assert.match(health.json.gateway_auth, /revoked|not enrolled/);
  const polls = r.gw.requests.filter((q) => q.route === 'GET /v1/inbox').length;
  await r.pump(30000, { stepMs: 2000 });
  await stays(() => r.gw.requests.filter((q) => q.route === 'GET /v1/inbox').length === polls, { what: 'no further poll' });
});

test('the journal is 0600 in a 0700 directory and holds the full envelope before the POST', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ withhold: true });
  r.gw.addText(X, 'wamid.ENV', 'the envelope');
  r.start();
  await waitFor(() => r.gc.count('wamid.ENV') === 1);
  const file = path.join(r.dir, '.gc', 'whatsapp', 'inbound_journal.jsonl');
  assert.equal((fs.statSync(file).mode & 0o777).toString(8), '600');
  assert.equal((fs.statSync(path.dirname(file)).mode & 0o777).toString(8), '700');
  const [received, posted] = lines(r, 'wamid.ENV');
  assert.equal(received.t, 'received');
  assert.equal(received.envelope.message.text.body, 'the envelope');
  assert.equal(posted.t, 'posted');
});
