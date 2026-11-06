// THE EVIDENCE RULE and the receipt verdicts, in process.

import assert from 'node:assert/strict';
import test from 'node:test';

import { pasteFound, probeShape } from '../src/evidence.js';
import { readbackInterval, verdictOf } from '../src/inbound.js';
import { pasteText } from './fakes.js';

const WA = '5511900000001';
const ID = 'wamid.E1';
const paste = (over = {}) => pasteText({ conversationId: WA, text: 'the owner writes', pmid: ID, ...over });

test('FOUND: format conversation and a user turn with the header and the last id line', () => {
  const answer = { format: 'conversation', turns: [{ role: 'user', text: paste() }] };
  assert.equal(probeShape(answer), 'log');
  assert.equal(pasteFound(answer, WA, ID), true);
  const threaded = { format: 'conversation', turns: [{ role: 'user', text: paste({ thread: 'wamid.parent' }) }] };
  assert.equal(pasteFound(threaded, WA, ID), true, 'the id line may be followed by " (in thread "');
});

test('a format text answer is a snapshot and is NOT FOUND whatever the pane shows (the gate r6 first finding)', () => {
  const answer = { format: 'text', turns: [{ role: 'output', text: paste() }] };
  assert.equal(probeShape(answer), 'pane');
  assert.equal(pasteFound(answer, WA, ID), false);
});

test('a tool result, an assistant turn or an output turn quoting the id is NOT FOUND', () => {
  for (const turn of [
    { role: 'user', text: `[result] ${paste()}` },
    { role: 'assistant', text: paste() },
    { role: 'output', text: paste() },
    { role: 'system', text: paste() },
  ]) assert.equal(pasteFound({ format: 'conversation', turns: [turn] }, WA, ID), false, turn.role);
});

test('a later paste whose BODY quotes the id line while its own id line names another wamid is NOT FOUND', () => {
  const quoting = pasteText({ conversationId: WA, text: `see\nMessage id: ${ID}\nabove`, pmid: 'wamid.OTHER' });
  assert.equal(pasteFound({ format: 'conversation', turns: [{ role: 'user', text: quoting }] }, WA, ID), false);
});

test('the header must name whatsapp/<conversation_id> exactly', () => {
  const other = pasteText({ conversationId: '5511900000009', text: 'x', pmid: ID });
  assert.equal(pasteFound({ format: 'conversation', turns: [{ role: 'user', text: other }] }, WA, ID), false);
  const body = `New message in shared conversation whatsapp/${WA}:\n\nMessage id: ${ID}`;
  assert.equal(pasteFound({ format: 'conversation', turns: [{ role: 'user', text: body }] }, WA, ID), true);
  assert.equal(pasteFound({ format: 'conversation', turns: [] }, WA, ID), false);
  assert.equal(pasteFound(undefined, WA, ID), false);
});

test('verdicts: the terminal flag is read first; partial kept apart; no_route; unsupported', () => {
  assert.deepEqual(verdictOf({ status: 'pending', terminal: true }), { verdict: 'pending', terminal: true });
  assert.deepEqual(verdictOf({ status: 'pending' }), { verdict: 'pending', terminal: false });
  assert.equal(verdictOf({ status: 'delivered', delivered_bytes: 5, expected_bytes: 5, members: [{ status: 'delivered', delivered_bytes: 5, expected_bytes: 5 }] }).verdict, 'delivered');
  assert.equal(verdictOf({ status: 'delivered', delivered_bytes: 3, expected_bytes: 5 }).verdict, 'failed');
  assert.equal(verdictOf({ status: 'delivered', members: [{ status: 'pending' }] }).verdict, 'pending', 'a concluded hold is not delivered');
  assert.equal(verdictOf({ status: 'partial' }).verdict, 'partial');
  assert.equal(verdictOf({ status: 'failed' }).verdict, 'failed');
  assert.equal(verdictOf({ status: 'no_route', delivered_bytes: 0, expected_bytes: 0 }).verdict, 'no_route');
  assert.equal(verdictOf(undefined).verdict, 'unsupported');
});

test('the read-back schedule: 5 s for 10 min, 60 s to the hour, then 5 min, no cutoff', () => {
  assert.equal(readbackInterval(0), 5000);
  assert.equal(readbackInterval(9 * 60 * 1000), 5000);
  assert.equal(readbackInterval(30 * 60 * 1000), 60000);
  assert.equal(readbackInterval(2 * 60 * 60 * 1000), 300000);
  assert.equal(readbackInterval(30 * 60 * 60 * 1000), 300000);
});
