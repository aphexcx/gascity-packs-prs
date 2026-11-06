// conversation.test.js: the box adapter on the gateway's stable
// conversation_id (gp-g02t, PR 76 round 2; the mayor's gate r4 MAJOR 1 and
// the Opus read r1 NOTE 5 at af7e2d9b). Against a real child on the fakes.
//
// PR 76's gateway routes by the WhatsApp user id, never the number (the
// design of record, whatsapp/docs/design-bsuid-pairing-and-spans.md). Its
// inbox (gateway.ts inbox()) gives every delivery the owner's conversation_id,
// fixed at the bind and never changed, and a wa_id that is only the number the
// message (or the owner's newest pairing) shows: NULL for an owner known by
// user id alone, a NEW number after a number change, and the SAME number for
// two owners after a recycle. So the box keys every conversation, ack and
// journal line by conversation_id, and carries the number for display only.
//
// The three shapes of the bead, each RED at af7e2d9b:
//   1. a NULL wa_id (an owner known by user id alone): acknowledged and
//      filed, never left unacknowledged until the gateway's 7-day cron;
//   2. a wa_id changed between two deliveries of one conversation_id: one gc
//      conversation, one ordered chain, one watermark;
//   3. a recycled number under two conversation_ids: two gc conversations,
//      and the mayor's reply in each reaches its own owner.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

import { adapterRequest, rid, rig, stays, waitFor } from './fakes.js';

const P = '5511900000001'; // owner A's number at the bind: A's conversation_id
const P2 = '5511900000077'; // owner A's new number, after a number change
const A_USER = 'US.13491208655302741918'; // owner A's user id (Meta's example BSUID)
const B_USER = 'US.70412958836127405533'; // owner B's user id and conversation_id: B joined on the recycled P
const C_USER = 'US.55500011122233344455'; // owner C, known by user id alone (no number shown)
const Q = '5511900000002'; // an owner of the gateway contract v1 (PR 73)

async function setup(t, extra = {}) {
  const r = await rig({ extra });
  t.after(() => r.close());
  return r;
}
const acked = (r, id) => waitFor(() => r.gw.acked.has(id), { what: `the ack of delivery ${id}` });
const lines = (r, pmid, type) => r.journal().filter((l) => l.rid === rid(pmid) && (!type || l.t === type));
const postOf = (r, pmid) => r.gc.posts.find((p) => p.pmid === pmid)?.message;
const filedUnder = (r, pmid) => postOf(r, pmid)?.conversation?.conversation_id;
const ts = (r) => String(Math.floor(r.clock.now() / 1000));

// A text in PR 76's inbox shape: Meta's message as the webhook gave it (from
// only when Meta shows a number; from_user_id always, since April 2026), the
// delivery's ids as gateway.ts writes them.
function ownerText(r, { conversation, number = null, user, wamid, body, profile = 'Owner', kind = 'message' }) {
  const message = { id: wamid, timestamp: ts(r), type: 'text', text: { body }, from_user_id: user };
  if (number) message.from = number;
  return r.gw.addDelivery({ kind, wa_id: number, user_id: user, conversation_id: conversation, profile_name: profile, message });
}

const reply = (r, conversationId, text, key) => adapterRequest(r.socket, 'POST', '/publish', {
  session_id: 's-mayor',
  conversation: { scope_id: 'test-city', provider: 'whatsapp', account_id: 'dynasty', conversation_id: conversationId, kind: 'dm' },
  text,
  idempotency_key: key,
});
const update = (r, to, text, key) => adapterRequest(r.socket, 'POST', '/publish-update', { to, text, idempotency_key: key });

// --- shape 1: a NULL wa_id ---------------------------------------------------------

test('shape 1 (a NULL wa_id): a message from an owner the gateway knows by user id alone is acknowledged, filed under its conversation_id with that id as the actor, and opens the window the mayor\'s reply and update read', async (t) => {
  const r = await setup(t);
  const id = ownerText(r, { conversation: C_USER, user: C_USER, wamid: 'wamid.C1', body: 'hello from a username', profile: 'Carla' });
  r.start();
  await acked(r, id);
  const post = postOf(r, 'wamid.C1');
  assert.deepEqual(
    { conversation: post.conversation, actor: post.actor },
    {
      conversation: { scope_id: 'test-city', provider: 'whatsapp', account_id: 'dynasty', conversation_id: C_USER, kind: 'dm' },
      actor: { id: C_USER, display_name: 'Carla', is_bot: false },
    },
  );
  const received = lines(r, 'wamid.C1', 'received')[0];
  assert.equal(received.conversation_id, C_USER, 'the received line carries the conversation_id');
  // The window ledger is keyed by the same id the mayor replies to: an update
  // inside the open window is refused (a free-form reply is cheaper).
  const early = await update(r, C_USER, 'Weekly numbers are in.', 'u-C1');
  assert.equal(early.json.metadata?.reason, 'window_open', `the ledger knows ${C_USER}'s window is open (${JSON.stringify(early.json)})`);
  const answer = await reply(r, C_USER, 'hi Carla', 'k-C1');
  assert.equal(answer.json.delivered, true, JSON.stringify(answer.json));
  assert.deepEqual(r.gw.sends.map((s) => s.to), [C_USER], 'the reply goes to the conversation_id');
});

test('shape 1 (a NULL wa_id): a join from an owner known by user id alone binds the invite\'s keeper fields to its conversation_id and is acknowledged', async (t) => {
  const r = await setup(t);
  r.start();
  const invite = await waitFor(async () => {
    const answer = await adapterRequest(r.socket, 'POST', '/invite', { proactive_consent: true }).catch(() => undefined);
    return answer?.status === 200 ? answer : undefined;
  }, { what: 'POST /invite' });
  const id = ownerText(r, { kind: 'joined', conversation: C_USER, user: C_USER, wamid: 'wamid.CJ1', body: String(invite.json.code), profile: 'Carla' });
  await acked(r, id);
  const status = await waitFor(async () => {
    const s = (await adapterRequest(r.socket, 'GET', '/status')).json;
    return s.keeper?.conversations?.[C_USER]?.consent === true ? s : undefined;
  }, { what: 'the invite bound to C\'s conversation_id' });
  assert.equal(status.keeper.invites_pending, 0);
  assert.deepEqual(Object.keys(status.keeper.conversations), [C_USER], 'one keeper row, under the conversation_id (never "null")');
  assert.equal(filedUnder(r, 'wamid.CJ1'), C_USER);
});

// --- shape 2: a wa_id changed within one conversation_id -------------------------------

test('shape 2 (a number change): two deliveries of one conversation_id from two numbers land in ONE gc conversation, in order on one chain, under one watermark', async (t) => {
  const r = await setup(t);
  // The first copy of A1 meets a gc 500: A2, from A's new number, waits behind it.
  r.gc.policy = (pmid, m, i) => (pmid === 'wamid.A1' && i === 0 ? { status: 500 } : {});
  const a1 = ownerText(r, { conversation: P, number: P, user: A_USER, wamid: 'wamid.A1', body: 'before the change', profile: 'Ana' });
  const a2 = ownerText(r, { conversation: P, number: P2, user: A_USER, wamid: 'wamid.A2', body: 'after the change', profile: 'Ana' });
  r.start();
  await waitFor(() => r.gc.count('wamid.A1') === 1, { what: 'A1\'s first post' });
  await stays(() => r.gc.count('wamid.A2') === 0, { what: 'A2 waits behind A1: one owner, one chain' });
  await r.pump(1000);
  await acked(r, a1);
  await acked(r, a2);
  const order = r.gc.posts.map((p) => p.pmid);
  assert.ok(order.lastIndexOf('wamid.A1') < order.indexOf('wamid.A2'), `A1 before A2 (${order})`);
  assert.deepEqual(
    {
      filed: [filedUnder(r, 'wamid.A1'), filedUnder(r, 'wamid.A2')],
      actors: [postOf(r, 'wamid.A1').actor.id, postOf(r, 'wamid.A2').actor.id],
      records: r.gc.records(P).map((x) => [x.Sequence, x.ProviderMessageID]),
      underNewNumber: r.gc.records(P2).length,
      a2Boundary: lines(r, 'wamid.A2', 'posted')[0]?.boundary,
      watermarks: r.journal().filter((l) => l.t === 'watermark').map((l) => [l.conversation_id, l.sequence]),
    },
    {
      filed: [P, P],
      actors: [P, P],
      records: [[1, 'wamid.A1'], [2, 'wamid.A2']],
      underNewNumber: 0,
      a2Boundary: 1,
      watermarks: [[P, 1], [P, 2]],
    },
  );
});

// --- shape 3: a recycled number under two conversation_ids ------------------------------

test('shape 3 (a recycled number): owner B on the number owner A was bound by never shares A\'s gc conversation; the mayor\'s reply and update in B\'s conversation go to B\'s id', async (t) => {
  const r = await setup(t);
  const a = ownerText(r, { conversation: P, number: P2, user: A_USER, wamid: 'wamid.RA1', body: 'A, from the new number', profile: 'Ana' });
  const b = ownerText(r, { conversation: B_USER, number: P, user: B_USER, wamid: 'wamid.RB1', body: 'B, on the recycled number', profile: 'Bruno' });
  r.start();
  await acked(r, a);
  await acked(r, b);
  assert.deepEqual(
    {
      filed: { A: filedUnder(r, 'wamid.RA1'), B: filedUnder(r, 'wamid.RB1') },
      actors: { A: postOf(r, 'wamid.RA1').actor, B: postOf(r, 'wamid.RB1').actor },
      inA: r.gc.records(P).map((x) => x.ProviderMessageID),
      inB: r.gc.records(B_USER).map((x) => x.ProviderMessageID),
      underTheNumbers: [r.gc.records(P2).length],
    },
    {
      filed: { A: P, B: B_USER },
      actors: { A: { id: P, display_name: 'Ana', is_bot: false }, B: { id: B_USER, display_name: 'Bruno', is_bot: false } },
      inA: ['wamid.RA1'],
      inB: ['wamid.RB1'],
      underTheNumbers: [0],
    },
  );
  const early = await update(r, B_USER, 'B: invoice 4471 is in the portal', 'u-RB1');
  assert.equal(early.json.metadata?.reason, 'window_open', `the ledger keys B's window by B's conversation_id (${JSON.stringify(early.json)})`);
  const answer = await reply(r, B_USER, 'B: your order 4471 ships tomorrow', 'k-RB1');
  assert.equal(answer.json.delivered, true, JSON.stringify(answer.json));
  assert.deepEqual(r.gw.sends.map((s) => s.to), [B_USER], 'B\'s reply goes to B\'s id, never the number A was bound by');
});

// --- the journal format: the PR 74 adapter's lines are tolerated, not migrated ----------

test('the journal: a received line the PR 74 adapter wrote (wa_id only, its gc conversation) is read as that conversation; its acceptance raises that conversation\'s watermark and counts in the boundary of the owner\'s next delivery; its ack goes with no second post', async (t) => {
  const r = await setup(t);
  const start = r.clock.now();
  // The delivery the PR 74 adapter took from PR 73's gateway, posted and
  // delivered, its ack not yet confirmed, killed before its watermark line.
  const old = r.gw.addDelivery({ wa_id: P, profile_name: 'Ana', message: { from: P, id: 'wamid.OLD', timestamp: ts(r), type: 'text', text: { body: 'before the upgrade' } } });
  const envelope = r.gw.deliveries.get(old);
  r.gc.records(P).push({
    ID: `rec-${P}-1`,
    Conversation: { scope_id: 'test-city', provider: 'whatsapp', account_id: 'dynasty', conversation_id: P, kind: 'dm' },
    Sequence: 1,
    Kind: 'inbound',
    ProviderMessageID: 'wamid.OLD',
    Actor: { id: P, display_name: 'Ana', is_bot: false },
    Text: 'before the upgrade',
    CreatedAt: new Date(start).toISOString(),
  });
  r.gc.conversations.get(P).nextSequence = 2;
  const stateDir = path.join(r.dir, '.gc', 'whatsapp');
  fs.mkdirSync(stateDir, { recursive: true, mode: 0o700 });
  const oldLines = [
    { t: 'genesis', at: start - 3600000 },
    { t: 'received', rid: rid('wamid.OLD'), delivery_id: old, kind: 'message', wa_id: P, profile_name: 'Ana', envelope, received_at: envelope.received_at, at: start },
    { t: 'posted', rid: rid('wamid.OLD'), at: start, reposts: 0, boundary: 0, marked: false },
    { t: 'accepted', rid: rid('wamid.OLD'), receipt_id: 'rcpt-old', verdict: 'delivered', sequence: 1, provider_message_id: 'wamid.OLD', members: ['s-mayor'], via: 'post', at: start },
  ];
  fs.writeFileSync(path.join(stateDir, 'inbound_journal.jsonl'), oldLines.map((l) => `${JSON.stringify(l)}\n`).join(''), { mode: 0o600 });
  // After the gateway's deploy: A writes from a new number, under the same conversation_id.
  const fresh = ownerText(r, { conversation: P, number: P2, user: A_USER, wamid: 'wamid.NEW', body: 'after the upgrade', profile: 'Ana' });
  r.start();
  await acked(r, old);
  await acked(r, fresh);
  assert.deepEqual(
    {
      oldPosts: r.gc.count('wamid.OLD'),
      newFiled: filedUnder(r, 'wamid.NEW'),
      newBoundary: lines(r, 'wamid.NEW', 'posted')[0]?.boundary,
      records: r.gc.records(P).map((x) => [x.Sequence, x.ProviderMessageID]),
      startWatermark: r.journal().find((l) => l.t === 'watermark')?.conversation_id,
    },
    {
      oldPosts: 0,
      newFiled: P,
      newBoundary: 1,
      records: [[1, 'wamid.OLD'], [2, 'wamid.NEW']],
      startWatermark: P,
    },
  );
});

// --- the contract versions --------------------------------------------------------------

test('the contract: a PR 73 delivery (no conversation_id field) is filed under its wa_id; a delivery naming conversation_id null is left unacknowledged and never filed under its wa_id', async (t) => {
  const r = await setup(t);
  const v1 = r.gw.addText(Q, 'wamid.V1', 'from the PR 73 gateway');
  const nul = r.gw.addDelivery({ conversation_id: null, wa_id: P2, user_id: null, message: { from: P2, id: 'wamid.NUL', timestamp: ts(r), type: 'text', text: { body: 'no conversation' } } });
  r.start();
  await acked(r, v1);
  assert.equal(filedUnder(r, 'wamid.V1'), Q);
  await waitFor(() => r.adapter.log().includes('no conversation id'), { what: 'the refusal logged' });
  await stays(() => r.gw.held(nul) && r.gc.count('wamid.NUL') === 0 && lines(r, 'wamid.NUL').length === 0, { what: 'held, never posted, never journaled' });
});
