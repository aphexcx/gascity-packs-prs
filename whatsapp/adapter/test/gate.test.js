// gate.test.js — the codex gate r1 findings on PR 74 (the record
// /Users/tailor512/city/assets/ops/codex-gates/gp-9hf1-pr74-r1-ba9155312.txt,
// 22:08Z 11/4; the round-2 bead gp-wf4e). Each test is RED at ba915531 and
// GREEN at the round's head.
//
// The inbound findings drive the engine IN PROCESS on the fakes (real HTTP,
// the real journal on disk, the engine's own tick loop), so a test can hold
// one gc read, fail one journal append or fail the keeper's write exactly
// where the gate names it, and a "restart" is a second engine on the same
// state directory.

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

import { createDeadLetters } from '../src/deadletter.js';
import { createGatewayClient } from '../src/gateway.js';
import { conversationRef, createGcClient } from '../src/gc.js';
import { createInbound } from '../src/inbound.js';
import { JOURNAL_FILE, ackOwed, entryState, openJournal } from '../src/journal.js';
import { createLedger } from '../src/ledger.js';
import { createPublisher } from '../src/outbound.js';
import {
  CITY, CREDENTIAL, createTestClock, deadLetterLines, rid, sleep, startFakeGateway, startFakeGc, stays, tempDir, waitFor,
} from './fakes.js';

const A = '5511900000001';
const B = '5511900000002';
const POST_TIMEOUT_MS = 2000; // the in-process gc client deadline
const GRACE_MS = POST_TIMEOUT_MS + 10000 + 1000; // the in-doubt grace (deadline + 10 s) and a second
const MARKER = (pmid) => `[redelivery ${pmid}: a first copy may have reached you; if you already answered it, do nothing]`;

// --- the in-process world ------------------------------------------------------

async function world(t) {
  const dir = tempDir('wa-g-');
  const clock = createTestClock(dir);
  const gw = await startFakeGateway({ clock });
  const gc = await startFakeGc({ clock });
  // <dir>/.gc/whatsapp, so the fakes' journalLines/deadLetterLines read it.
  const w = { dir, clock, gw, gc, stateDir: path.join(dir, '.gc', 'whatsapp'), engines: [] };
  t.after(async () => {
    for (const e of w.engines) await e.stop();
    await gw.close();
    await gc.close();
    fs.rmSync(dir, { recursive: true, force: true });
  });
  return w;
}

// One engine on the world's state dir, ticking every 10 ms. failAppend(line)
// true makes that journal append throw ENOSPC (the line is not written);
// crashOnFail stops the engine at that instant, before the throw, as a
// SIGKILL between two journal lines would.
function startEngine(w, { keeper, ledger, wrapGc = (c) => c, failAppend, crashOnFail = false } = {}) {
  const real = openJournal({ stateDir: w.stateDir, clock: w.clock.now });
  const engine = { journal: real, logs: [], failures: 0 };
  let journal = real;
  if (failAppend) {
    journal = Object.create(real);
    journal.append = (line) => {
      if (failAppend(line)) {
        engine.failures += 1;
        if (crashOnFail) engine.crash();
        throw Object.assign(new Error('ENOSPC: no space left on device, write (injected)'), { code: 'ENOSPC' });
      }
      return real.append(line);
    };
  }
  const inbound = createInbound({
    journal,
    deadLetters: createDeadLetters({ stateDir: w.stateDir }),
    gateway: createGatewayClient({ url: w.gw.url, credential: CREDENTIAL, timeoutMs: 2000 }),
    gc: wrapGc(createGcClient({ base: w.gc.url, city: CITY, postTimeoutMs: POST_TIMEOUT_MS, readTimeoutMs: 10000 })),
    city: CITY,
    clock: w.clock.now,
    log: (line) => engine.logs.push(String(line)),
    keeper,
    ledger,
    pollMs: 0,
    gcPostTimeoutMs: POST_TIMEOUT_MS,
    watermarkCheckMs: 60000,
    compactMs: 60 * 60 * 1000,
  });
  let running = true;
  engine.inbound = inbound;
  engine.crash = () => { running = false; inbound.stop(); };
  const loop = (async () => {
    while (running) {
      try { await inbound.tick(); } catch (err) { engine.logs.push(`tick failed: ${err.message}`); }
      await sleep(10);
    }
  })();
  engine.stop = async () => {
    engine.crash();
    await loop;
    await sleep(50); // in-flight steps finish against the open journal
    real.close();
  };
  w.engines.push(engine);
  return engine;
}

// The first append for rid after its received line (once).
function failFirstAfterReceived(ridToFail) {
  let received = false;
  let done = false;
  return (line) => {
    if (done || line.rid !== ridToFail) return false;
    if (line.t === 'received') { received = true; return false; }
    if (!received) return false;
    done = true;
    return true;
  };
}

async function deliver(w, waId, pmid, body = pmid) {
  const id = w.gw.addText(waId, pmid, body);
  await waitFor(() => w.gw.acked.has(id), { what: `the ack of ${pmid}` });
  return id;
}

async function pumpUntil(w, predicate, { stepMs = 1000, realMs = 30, timeoutMs = 20000, what = 'condition' } = {}) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (await predicate()) return;
    if (Date.now() > deadline) throw new Error(`timed out pumping for ${what}`);
    w.clock.advance(stepMs);
    await sleep(realMs);
  }
}

function loseJournal(w) {
  const file = path.join(w.stateDir, JOURNAL_FILE);
  fs.renameSync(file, `${file}.lost`);
}

const readLogs = (engine) => engine.logs.filter((l) => l.startsWith('watermark read (')).length;

// A gc client whose head read of one conversation (GET transcript, order
// desc, limit 1: the read after a detection) is held until released, or
// answers 500.
function controlledHeadRead(conversationId) {
  const ctl = { mode: null, held: 0, failed: 0, answered: 0, release: () => {} };
  ctl.wrap = (gc) => ({
    ...gc,
    async transcript(q) {
      const head = q.order === 'desc' && q.conversationId === conversationId;
      if (head && ctl.mode === 'fail') {
        ctl.failed += 1;
        throw Object.assign(new Error('HTTP 500 from gc (injected)'), { status: 500 });
      }
      if (head && ctl.mode === 'hold') {
        ctl.mode = null; // one read held
        ctl.held += 1;
        await new Promise((resolve) => { ctl.release = resolve; });
        const answer = await gc.transcript(q);
        ctl.answered += 1;
        return answer;
      }
      return gc.transcript(q);
    },
  });
  return ctl;
}

// The gate's MAJOR 1 setting: A and B each delivered once, a backup, A's
// newer record, B1 posted with its answer lost; then gc ALONE is restored to
// the backup: A's watermark (sequence 2) is gone from the store while B's
// (sequence 1) reads intact, and B1's record is gone with the restore.
async function majorOneSetting(w, eng, head, mode) {
  w.gc.policy = (pmid, m, i) => (pmid === 'wamid.GB1' && i === 0 ? { withhold: true } : {});
  await deliver(w, A, 'wamid.GA0');
  await deliver(w, B, 'wamid.GB0');
  const backup = w.gc.backup();
  await deliver(w, A, 'wamid.GA1');
  const b1 = w.gw.addText(B, 'wamid.GB1', 'delivered, the answer lost');
  await waitFor(() => w.gc.count('wamid.GB1') === 1, { what: 'B1 posted, its answer withheld' });
  head.mode = mode;
  w.gc.restore(backup);
  // A2 is re-issued sequence 2; the poll that brought it asks for the
  // all-watermark read, which finds A's record 2 is not A1: DETECTED.
  await deliver(w, A, 'wamid.GA2');
  return b1;
}

// --- MAJOR 1 (inbound.js:420) ----------------------------------------------------

test('gate r1 MAJOR 1 (inbound.js:420): a rollback detected while its head read is held stays detected however far acceptances advance that watermark meanwhile; B\'s in-doubt entry takes the marked path', async (t) => {
  const w = await world(t);
  const head = controlledHeadRead(A);
  const eng = startEngine(w, { wrapGc: head.wrap });
  const b1 = await majorOneSetting(w, eng, head, 'hold');
  await waitFor(() => head.held === 1, { what: 'the head read of A held: the rollback detected' });
  // Concurrent acceptances while the read is held: A3 and A4 take sequences
  // 3 and 4, and the watermark advances past the detected one.
  await deliver(w, A, 'wamid.GA3');
  await deliver(w, A, 'wamid.GA4');
  await waitFor(() => eng.journal.watermarks.get(A)?.provider_message_id === 'wamid.GA4', { what: 'A\'s watermark advanced to A4' });
  const reads = readLogs(eng);
  head.release();
  await waitFor(() => head.answered === 1 && readLogs(eng) > reads, { what: 'the held read finished' });
  w.clock.advance(GRACE_MS);
  await waitFor(() => w.gw.acked.has(b1), { what: 'B1 resolved and acked' });
  const posts = w.gc.postsFor('wamid.GB1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.GB1')), `never an unmarked duplicate: ${JSON.stringify(posts[1].text)}`);
  const gap = eng.journal.gaps.get(A);
  assert.ok(gap, 'A\'s detected rollback is on record');
  assert.equal(gap.watermark_sequence, 2, 'the gap names the watermark the read found missing');
  assert.equal(eng.journal.gaps.has(B), false, 'B\'s own watermark reads intact');
  t.diagnostic(`gap: watermark ${gap.watermark_sequence}, head ${gap.head_sequence}; A's watermark now ${eng.journal.watermarks.get(A).sequence}`);
});

test('gate r1 MAJOR 1 (inbound.js:420): a rollback detected whose head read FAILS stays detected while acceptances advance that watermark; the gap is written with the head unknown', async (t) => {
  const w = await world(t);
  const head = controlledHeadRead(A);
  const eng = startEngine(w, { wrapGc: head.wrap });
  const b1 = await majorOneSetting(w, eng, head, 'fail');
  await waitFor(() => head.failed >= 1, { what: 'the head read of A answered 500 after the detection' });
  await deliver(w, A, 'wamid.GA3');
  await deliver(w, A, 'wamid.GA4');
  await waitFor(() => eng.journal.watermarks.get(A)?.provider_message_id === 'wamid.GA4', { what: 'A\'s watermark advanced to A4' });
  head.mode = null; // gc heals
  w.clock.advance(GRACE_MS);
  await waitFor(() => w.gw.acked.has(b1), { what: 'B1 resolved and acked' });
  const posts = w.gc.postsFor('wamid.GB1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.GB1')), `never an unmarked duplicate: ${JSON.stringify(posts[1].text)}`);
  const gap = eng.journal.gaps.get(A);
  assert.ok(gap, 'A\'s detected rollback is on record');
  assert.equal(gap.watermark_sequence, 2);
  assert.equal(gap.head_sequence, null, 'the head the failed read could not give is unknown, never invented');
});

test('gate r1 MAJOR 1 (inbound.js:420): a crash while the detection\'s head read is held, after acceptances advanced that watermark: the restart still holds the gap; B\'s in-doubt entry takes the marked path', async (t) => {
  const w = await world(t);
  const head = controlledHeadRead(A);
  const first = startEngine(w, { wrapGc: head.wrap });
  const b1 = await majorOneSetting(w, first, head, 'hold');
  await waitFor(() => head.held === 1, { what: 'the head read of A held: the rollback detected' });
  await deliver(w, A, 'wamid.GA3');
  await deliver(w, A, 'wamid.GA4');
  await waitFor(() => first.journal.watermarks.get(A)?.provider_message_id === 'wamid.GA4', { what: 'A\'s watermark advanced to A4' });
  await first.stop(); // the crash: the held read never returns to this process
  const second = startEngine(w);
  w.clock.advance(GRACE_MS);
  await waitFor(() => w.gw.acked.has(b1), { what: 'B1 resolved and acked after the restart' });
  const posts = w.gc.postsFor('wamid.GB1');
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER('wamid.GB1')), `never an unmarked duplicate: ${JSON.stringify(posts[1].text)}`);
  assert.equal(second.journal.gaps.get(A)?.watermark_sequence, 2, 'the detection survived the crash');
  assert.equal(second.journal.gaps.get(A).head_sequence, null, 'its head was never read');
});

// --- MAJOR 2 (inbound.js:284) ----------------------------------------------------

test('gate r1 MAJOR 2: ONE classifier decides what a new rid is, fresh only on positive proof (the table)', async () => {
  const { classifyNew } = await import('../src/inbound.js');
  assert.equal(typeof classifyNew, 'function', 'one exported classifier decides a new rid');
  const G = 1_793_000_000_000;
  const M = 60000; // LOST_JOURNAL_MARGIN_MS
  const rows = [
    ['a dead-letter file line names it (a journal that is gone dead-lettered it)', { receivedAtMs: G + 5, genesisAt: G, deadLetter: { reason: 'in_doubt' }, now: G }, { recovery: 'dead', recovery_reason: 'in_doubt' }],
    ['taken by the gateway before the genesis less the margin', { receivedAtMs: G - M - 1, genesisAt: G, now: G }, { recovery: 'in_doubt', recovery_at: G - M - 1 }],
    ['exactly at the margin: fresh', { receivedAtMs: G - M, genesisAt: G, now: G }, {}],
    ['after the genesis: fresh', { receivedAtMs: G + 5, genesisAt: G, now: G + 5 }, {}],
    ['no gateway receipt time: never provably fresh', { receivedAtMs: Number.NaN, genesisAt: G, now: G + 7 }, { recovery: 'in_doubt', recovery_at: G + 7 }],
    ['no genesis: never provably fresh', { receivedAtMs: G + 5, genesisAt: undefined, now: G + 7 }, { recovery: 'in_doubt', recovery_at: G + 5 }],
  ];
  for (const [why, input, want] of rows) assert.deepEqual(classifyNew(input), want, why);
});

test('gate r1 MAJOR 2: the classification rides IN the received line; the fold applies it from that one line, live and on replay', () => {
  const dir = tempDir('wa-g-fold-');
  const G = 1_793_000_000_000;
  const j = openJournal({ stateDir: dir, clock: () => G });
  const line = (n, extra) => ({ t: 'received', rid: `r${n}`, delivery_id: n, kind: 'message', wa_id: A, envelope: { id: n }, received_at: 1, ...extra });
  j.append(line(1, { recovery: 'in_doubt', recovery_at: G - 300000 }));
  j.append(line(2, { recovery: 'dead', recovery_reason: 'no_route' }));
  j.append(line(3, {}));
  const check = (journal, when) => {
    const one = journal.entries.get('r1');
    assert.equal(entryState(one), 'posted', `${when}: in doubt from its first step`);
    assert.equal(one.firstPosted.at, G - 300000, `${when}: the grace and the pending deadline run from the gateway's receipt`);
    assert.equal(one.firstPosted.boundary, 0, `${when}: the lookup reads from zero`);
    assert.equal(one.markedPosted, false);
    assert.equal(entryState(journal.entries.get('r2')), 'dead', `${when}: dead from its first step`);
    assert.equal(journal.entries.get('r2').dead.reason, 'no_route');
    assert.equal(ackOwed(journal.entries.get('r2')), true, `${when}: its ack is owed`);
    assert.equal(entryState(journal.entries.get('r3')), 'received', `${when}: fresh`);
  };
  check(j, 'live');
  j.close();
  const replay = openJournal({ stateDir: dir, clock: () => G });
  check(replay, 'replay');
  assert.equal(replay.compact(() => true).droppedEntries, 0);
  replay.close();
  check(openJournal({ stateDir: dir, clock: () => G }), 'after a compaction');
  fs.rmSync(dir, { recursive: true, force: true });
});

// A delivery gc took (pending) under a journal that is then lost; a new
// journal is born two minutes later, so the gateway's receipt time is
// before its genesis.
async function lostJournalPending(w, pmid) {
  w.gc.policy = (p, m, i) => (p === pmid && i === 0 ? { verdict: 'pending' } : {});
  const id = w.gw.addText(A, pmid, 'posted by a journal that is gone');
  const first = startEngine(w);
  await waitFor(() => first.journal.entries.get(rid(pmid))?.accepted, { what: 'gc took it (pending) under the first journal' });
  await first.stop();
  loseJournal(w);
  w.gc.restart();
  w.clock.advance(2 * 60000);
  return id;
}

test('gate r1 MAJOR 2 (inbound.js:284): a delivery older than the journal\'s genesis, a crash right after its received line, a restart: the in-doubt rule from zero, never a plain post', async (t) => {
  const w = await world(t);
  const pmid = 'wamid.GL1';
  const id = await lostJournalPending(w, pmid);
  const second = startEngine(w, { failAppend: failFirstAfterReceived(rid(pmid)), crashOnFail: true });
  await waitFor(() => second.failures === 1, { what: 'the crash right after the received line' });
  await second.stop();
  startEngine(w); // the restart
  await waitFor(() => w.gw.acked.has(id), { what: 'resolved and acked after the restart' });
  const posts = w.gc.postsFor(pmid);
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER(pmid)), `never an unmarked repost: ${JSON.stringify(posts[1].text)}`);
  assert.ok(w.gc.requests.some((q) => q.route === 'GET /extmsg/transcript' && q.query.after_sequence === '0' && (q.returned ?? []).includes(pmid)),
    'the lookup from zero found the record');
});

test('gate r1 MAJOR 2 (inbound.js:284): the same delivery when the append after its received line FAILS and the engine runs on: never a plain post', async (t) => {
  const w = await world(t);
  const pmid = 'wamid.GL2';
  const id = await lostJournalPending(w, pmid);
  const eng = startEngine(w, { failAppend: failFirstAfterReceived(rid(pmid)) });
  await pumpUntil(w, () => w.gw.acked.has(id), { what: 'resolved and acked' });
  assert.equal(eng.failures, 1, 'one append failed');
  const posts = w.gc.postsFor(pmid);
  assert.equal(posts.length, 2);
  assert.ok(posts[1].text.startsWith(MARKER(pmid)), `never an unmarked repost: ${JSON.stringify(posts[1].text)}`);
});

test('gate r1 MAJOR 2 (the same two writes): a delivery a lost journal dead-lettered, the append after its received line FAILS: dead and acked, never posted again', async (t) => {
  const w = await world(t);
  const pmid = 'wamid.GL3';
  w.gc.policy = (p) => (p === pmid ? { status: 400 } : {});
  w.gw.ackBehaviour = [500];
  const id = w.gw.addText(A, pmid, 'rejected once');
  const first = startEngine(w);
  await waitFor(() => first.journal.entries.get(rid(pmid))?.dead && w.gw.ackCalls.length >= 1, { what: 'dead-lettered, its ack failed' });
  await first.stop();
  loseJournal(w);
  w.gw.ackBehaviour = ['ok'];
  w.clock.advance(2 * 60000);
  startEngine(w, { failAppend: failFirstAfterReceived(rid(pmid)) });
  await waitFor(() => w.gw.acked.has(id), { what: 'the owed ack' });
  await stays(() => w.gc.count(pmid) === 1, { forMs: 600, what: 'never posted again' });
  assert.equal(deadLetterLines(w.dir, A).length, 1, 'one dead-letter line');
});

// --- MAJOR 3 (outbound.js:398) ---------------------------------------------------

async function publisherWorld(t) {
  const dir = tempDir('wa-g3-');
  const clock = createTestClock(dir);
  const gw = await startFakeGateway({ clock });
  gw.bound.add(A);
  const pub = createPublisher({
    stateDir: path.join(dir, 'state'),
    gateway: createGatewayClient({ url: gw.url, credential: CREDENTIAL, timeoutMs: 2000 }),
    gc: createGcClient({ base: 'http://127.0.0.1:9', city: CITY }),
    city: CITY,
    clock: clock.now,
  });
  t.after(async () => {
    await gw.close();
    fs.rmSync(dir, { recursive: true, force: true });
  });
  const publish = (text, key) => pub.handlePublish(JSON.stringify({ session_id: 's-mayor', conversation: conversationRef(CITY, A), text, idempotency_key: key }));
  const update = (text, key) => pub.handlePublishUpdate(JSON.stringify({ to: A, text, idempotency_key: key }));
  const sendKeys = () => gw.requests.filter((q) => q.route === 'POST /v1/send').map((q) => q.body.idempotency_key);
  return { clock, gw, publish, update, sendKeys };
}

test('gate r1 MAJOR 3 (outbound.js:398): an update keyed K:0, the window opening, then a text keyed K: the text is SENT, never answered with the template\'s receipt', async (t) => {
  const s = await publisherWorld(t);
  const update = await s.update('Weekly numbers are in.', 'K:0');
  assert.equal(update.json.delivered, true);
  s.gw.lastInbound.set(A, s.clock.now()); // the owner writes: the window opens
  const reply = await s.publish('Thanks; the detail follows.', 'K');
  assert.equal(reply.json.delivered, true);
  const texts = s.gw.sends.filter((x) => x.type === 'text');
  assert.equal(texts.length, 1, `the text reached Meta (gateway keys ${JSON.stringify(s.sendKeys())})`);
  assert.equal(texts[0].text, 'Thanks; the detail follows.');
  assert.notEqual(reply.json.message_id, update.json.message_id, 'the text\'s own message id, not the template\'s');
  assert.equal(new Set(s.sendKeys()).size, s.sendKeys().length, 'every send under its own gateway key');
});

test('gate r1 MAJOR 3 (outbound.js:398): the reverse, a two-chunk text keyed K, then an update keyed K:1: the template is SENT, never answered with chunk 1\'s receipt', async (t) => {
  const s = await publisherWorld(t);
  s.gw.lastInbound.set(A, s.clock.now());
  const reply = await s.publish(`${'a'.repeat(4096)}${'b'.repeat(10)}`, 'K');
  assert.equal(reply.json.delivered, true);
  assert.equal(reply.json.metadata.chunks, '2');
  const update = await s.update('Weekly numbers are in.', 'K:1');
  assert.equal(update.json.delivered, true);
  const templates = s.gw.sends.filter((x) => x.type === 'template');
  assert.equal(templates.length, 1, `the template reached Meta (gateway keys ${JSON.stringify(s.sendKeys())})`);
  assert.notEqual(update.json.message_id, reply.json.message_id, 'the template\'s own message id, not chunk 1\'s');
});

// --- MINOR (inbound.js:197) ------------------------------------------------------

test('gate r1 MINOR (inbound.js:197): a join whose keeper bind fails is not retired: held at the gateway and in the journal across a restart, bound and acked once the keeper heals', async (t) => {
  const w = await world(t);
  const keeper = {
    failing: true,
    attempts: 0,
    bound: null,
    bindInvite(code, waId) {
      this.attempts += 1;
      if (this.failing) throw new Error('EIO: i/o error, write keeper.json (injected)');
      this.bound = { code, waId };
      return true;
    },
  };
  const pmid = 'wamid.GJ1';
  const id = w.gw.addDelivery({
    kind: 'joined',
    wa_id: A,
    message: { from: A, id: pmid, timestamp: String(Math.floor(w.clock.now() / 1000)), type: 'text', text: { body: '123456' } },
  });
  const first = startEngine(w, { keeper });
  await waitFor(() => first.journal.entries.get(rid(pmid))?.accepted?.verdict === 'delivered', { what: 'gc delivered the join' });
  assert.ok(keeper.attempts >= 1, 'the bind was tried');
  await stays(() => w.gw.held(id), { forMs: 600, what: 'the join held at the gateway while its keeper bind is owed' });
  await first.stop();
  const second = startEngine(w, { keeper });
  await stays(() => w.gw.held(id) && second.journal.entries.has(rid(pmid)), { forMs: 400, what: 'held, its envelope in the journal, after a restart' });
  const attempts = keeper.attempts;
  keeper.failing = false; // the keeper heals
  await waitFor(() => w.gw.acked.has(id), { what: 'the ack once the bind is saved' });
  assert.ok(keeper.attempts > attempts, 'the bind was retried');
  assert.deepEqual(keeper.bound, { code: '123456', waId: A });
});

// --- the Fable read r1's MINOR 1 (inbound.js:50, read in takeDelivery) -----------

test('Fable read r1 MINOR 1 (inbound.js:50): an order moves the window ledger as the gateway counts it (its reading 7); a system notice does not', async (t) => {
  const w = await world(t);
  const ledger = createLedger({ stateDir: w.stateDir, clock: w.clock.now });
  startEngine(w, { ledger });
  const ts = String(Math.floor(w.clock.now() / 1000));
  const order = w.gw.addDelivery({ wa_id: A, message: { from: A, id: 'wamid.GO1', timestamp: ts, type: 'order', order: { catalog_id: 'c1', product_items: [] } } });
  const notice = w.gw.addDelivery({ wa_id: B, message: { from: B, id: 'wamid.GS1', timestamp: ts, type: 'system', system: { body: 'a system notice' } } });
  await waitFor(() => w.gw.acked.has(order) && w.gw.acked.has(notice), { what: 'both delivered and acked' });
  assert.equal(ledger.windowOpen(A), true, 'an order opens the window in the ledger, as at the gateway');
  assert.equal(ledger.windowOpen(B), false, 'a system notice does not');
});
