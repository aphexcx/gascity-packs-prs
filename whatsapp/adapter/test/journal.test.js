// The journal's fold and compaction, in process (the crash tests in
// recovery.test.js drive the same code through a real child).

import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';

import { createDeadLetters } from '../src/deadletter.js';
import { JOURNAL_FILE, ackOwed, entryState, openJournal, readLines } from '../src/journal.js';
import { tempDir } from './fakes.js';

const RID = 'whatsapp:dynasty:wamid.J1';

function received(rid = RID, conversationId = '5511900000001', deliveryId = 1) {
  return { t: 'received', rid, delivery_id: deliveryId, kind: 'message', conversation_id: conversationId, envelope: { id: deliveryId } };
}

// gp-g02t: the journal format moved from wa_id to conversation_id. A line the
// PR 74 adapter wrote is TOLERATED (never rewritten): it names the gc
// conversation that adapter posted under, its wa_id.
test('the PR 74 received line (wa_id only) folds as its gc conversation, beside the new lines, through a compaction and a replay', () => {
  const dir = tempDir();
  let now = 1000;
  const P = '5511900000001';
  const old = 'whatsapp:dynasty:wamid.OLD';
  const fresh = 'whatsapp:dynasty:wamid.NEW';
  const j = openJournal({ stateDir: dir, clock: () => now });
  j.append({ t: 'received', rid: old, delivery_id: 1, kind: 'message', wa_id: P, envelope: { id: 1, wa_id: P } });
  j.append({ t: 'accepted', rid: old, verdict: 'delivered', receipt_id: 'r1', sequence: 4, provider_message_id: 'wamid.OLD' });
  j.append(received(fresh, P, 2));
  j.append({ t: 'received', rid: 'whatsapp:dynasty:wamid.BSUID', delivery_id: 3, kind: 'message', conversation_id: 'US.13491208655302741918', wa_id: null, envelope: { id: 3 } });
  const keys = (jj) => [old, fresh, 'whatsapp:dynasty:wamid.BSUID'].map((rid) => jj.entries.get(rid).conversationId);
  assert.deepEqual(keys(j), [P, P, 'US.13491208655302741918'], 'the old line is its wa_id\'s conversation; a new line names its own, never its wa_id');
  assert.equal(j.watermarks.get(P)?.sequence, 4, 'the old line\'s acceptance raises its conversation\'s watermark');
  now = 2000;
  j.compact(() => false);
  const kept = readLines(path.join(dir, JOURNAL_FILE)).lines.find((l) => l.rid === old && l.t === 'received');
  assert.deepEqual([kept.wa_id, 'conversation_id' in kept], [P, false], 'compaction keeps the old line as written');
  j.close();
  const again = openJournal({ stateDir: dir, clock: () => now });
  assert.deepEqual(keys(again), [P, P, 'US.13491208655302741918'], 'the same after a replay');
  assert.equal(again.watermarks.get(P)?.sequence, 4);
  again.close();
});

test('the fold derives each state from the last progress line', () => {
  const dir = tempDir();
  let now = 1000;
  const j = openJournal({ stateDir: dir, clock: () => now });
  j.append(received());
  assert.equal(entryState(j.entries.get(RID)), 'received');
  j.append({ t: 'posted', rid: RID, reposts: 0, boundary: 0, marked: false });
  assert.equal(entryState(j.entries.get(RID)), 'posted');
  j.append({ t: 'accepted', rid: RID, verdict: 'failed', receipt_id: 'r1' });
  assert.equal(entryState(j.entries.get(RID)), 'accepted:failed');
  j.append({ t: 'posted', rid: RID, reposts: 1, boundary: 0, marked: false });
  assert.equal(entryState(j.entries.get(RID)), 'posted', 'a repost in flight is in doubt again');
  assert.equal(j.entries.get(RID).everAccepted, true, 'the journal remembers gc took it once');
  j.append({ t: 'accepted', rid: RID, verdict: 'pending', terminal: true, receipt_id: 'r2' });
  assert.equal(entryState(j.entries.get(RID)), 'held', 'an accepted-pending line with terminal IS held_terminal');
  j.append({ t: 'accepted', rid: RID, verdict: 'delivered', via: 'operator' });
  assert.equal(entryState(j.entries.get(RID)), 'accepted:delivered');
  assert.equal(ackOwed(j.entries.get(RID)), true);
  now = 2000;
  j.append({ t: 'acknowledged', rid: RID });
  assert.equal(entryState(j.entries.get(RID)), 'acknowledged');
  j.close();
});

test('every line is replayed in order after a restart, a torn tail dropped', () => {
  const dir = tempDir();
  const j = openJournal({ stateDir: dir, clock: () => 5 });
  j.append(received());
  j.append({ t: 'posted', rid: RID, reposts: 0, boundary: 3, marked: false });
  j.close();
  const file = path.join(dir, JOURNAL_FILE);
  fs.appendFileSync(file, '{"t":"accepted","rid":"whatsa'); // a kill mid-write
  const again = openJournal({ stateDir: dir, clock: () => 6 });
  assert.equal(entryState(again.entries.get(RID)), 'posted');
  assert.equal(again.entries.get(RID).firstPosted.boundary, 3);
  assert.equal(readLines(file).tornAt, -1, 'the torn tail was truncated away');
  assert.equal((fs.statSync(file).mode & 0o777).toString(8), '600');
  assert.equal((fs.statSync(dir).mode & 0o777).toString(8), '700');
  again.close();
});

test('accepted lines raise the watermark; a watermark line sets it; clear and rebase', () => {
  const dir = tempDir();
  const j = openJournal({ stateDir: dir, clock: () => 7 });
  j.append(received());
  j.append({ t: 'accepted', rid: RID, verdict: 'delivered', sequence: 4, provider_message_id: 'wamid.J1' });
  assert.deepEqual(j.watermarks.get('5511900000001').sequence, 4);
  j.append({ t: 'accepted', rid: RID, verdict: 'delivered', sequence: 2, provider_message_id: 'wamid.old' });
  assert.equal(j.watermarks.get('5511900000001').sequence, 4, 'accepted lines only raise');
  j.append({ t: 'watermark', conversation_id: '5511900000001', sequence: 1, provider_message_id: 'wamid.head', accepted_at: 'x', rebased_at: 9 });
  assert.equal(j.watermarks.get('5511900000001').sequence, 1, 'the rebase may lower it');
  j.append({ t: 'watermark_cleared', conversation_id: '5511900000001' });
  assert.equal(j.watermarks.has('5511900000001'), false);
  j.close();
});

test('THE GATE r7 RULE 2: compaction drops only acknowledged rids and keeps a dead rid whose ack is unconfirmed', () => {
  const dir = tempDir();
  let now = Date.parse('2026-11-04T00:00:00Z');
  const j = openJournal({ stateDir: dir, clock: () => now });
  const acked = 'whatsapp:dynasty:wamid.acked';
  const dead = 'whatsapp:dynasty:wamid.dead';
  const deadAcked = 'whatsapp:dynasty:wamid.deadacked';
  j.append(received(acked, '5511900000001', 1));
  j.append({ t: 'accepted', rid: acked, verdict: 'delivered', sequence: 5, provider_message_id: 'wamid.acked' });
  j.append({ t: 'acknowledged', rid: acked });
  j.append(received(dead, '5511900000002', 2));
  j.append({ t: 'dead', rid: dead, reason: 'in_doubt' });
  j.append(received(deadAcked, '5511900000002', 3));
  j.append({ t: 'dead', rid: deadAcked, reason: 'no_route' });
  j.append({ t: 'acknowledged', rid: deadAcked });
  j.append({ t: 'probe', rid: acked, session_id: 's', shape: 'pane', found: false });
  j.append({ t: 'restore_gap', conversation_id: '5511900000003', watermark_sequence: 9, head_sequence: 2, found_at: now });
  now += 60000;
  const result = j.compact();
  assert.equal(result.droppedEntries, 2);
  assert.equal(entryState(j.entries.get(dead)), 'dead', 'the dead rid survives: its ack is owed');
  assert.equal(j.entries.has(acked), false);
  assert.equal(j.entries.has(deadAcked), false);
  j.close();
  const replay = openJournal({ stateDir: dir, clock: () => now });
  assert.equal(entryState(replay.entries.get(dead)), 'dead', 'and survives a replay of the compacted file');
  assert.equal(ackOwed(replay.entries.get(dead)), true);
  assert.equal(replay.watermarks.get('5511900000001').sequence, 5, 'the watermark of a compacted rid is kept');
  assert.equal(replay.gaps.has('5511900000003'), true, 'a standing gap is kept');
  assert.equal(replay.fold.probes.length, 1, 'a probe line of the last 7 days is kept');
  replay.close();
  now += 8 * 24 * 60 * 60 * 1000;
  const later = openJournal({ stateDir: dir, clock: () => now });
  later.compact();
  assert.equal(later.fold.probes.length, 0, 'a probe line older than 7 days is dropped');
  assert.equal(entryState(later.entries.get(dead)), 'dead', 'the dead rid is still owed after any number of compactions');
  later.close();
});

test('the dead-letter store writes one line per rid and indexes the files at start', () => {
  const dir = tempDir();
  const d = createDeadLetters({ stateDir: dir });
  assert.equal(d.write('5511900000002', { rid: 'r1', reason: 'in_doubt' }), true);
  assert.equal(d.write('5511900000002', { rid: 'r1', reason: 'in_doubt' }), false, 'idempotent per rid');
  const again = createDeadLetters({ stateDir: dir });
  assert.equal(again.has('r1'), true);
  const file = path.join(dir, 'inbound_dead_letter', '5511900000002.jsonl');
  assert.equal(fs.readFileSync(file, 'utf8').trim().split('\n').length, 1);
  assert.equal((fs.statSync(file).mode & 0o777).toString(8), '600');
  assert.equal((fs.statSync(path.dirname(file)).mode & 0o777).toString(8), '700');
});

test('gp-g02t: each owner\'s dead letters keep its own file: two user ids that differ only by a period never share one; a name never hides or leaves the directory', () => {
  const dir = tempDir();
  const d = createDeadLetters({ stateDir: dir });
  d.write('US.ENT.A123', { rid: 'ra', reason: 'in_doubt' });
  d.write('US.ENTA123', { rid: 'rb', reason: 'in_doubt' });
  d.write('../..', { rid: 'rc', reason: 'in_doubt' });
  assert.deepEqual(fs.readdirSync(path.join(dir, 'inbound_dead_letter')).sort(), ['US.ENT.A123.jsonl', 'US.ENTA123.jsonl', '_.....jsonl']);
  const again = createDeadLetters({ stateDir: dir });
  assert.deepEqual(['ra', 'rb', 'rc'].map((rid) => again.has(rid)), [true, true, true]);
});
