import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { createPublisher } from '../src/outbound.js';
import { createGatewayClient } from '../src/gateway.js';
import { startFakeGateway, tempDir, createTestClock, CREDENTIAL, rig, waitFor } from './fakes.js';

const OWNER = 'example-owner', TARGET = 'wamid.example-inbound';
const body = (extra = {}) => ({ session_id: 's-mayor', conversation: { conversation_id: OWNER },
  text: 'fixture reply', idempotency_key: 'quote-key', reply_to_message_id: TARGET, ...extra });

async function setup(t) {
  const dir = tempDir('wa-quote-'), clock = createTestClock(dir);
  const gw = await startFakeGateway({ clock });
  t.after(async () => { await gw.close(); fs.rmSync(dir, { recursive: true, force: true }); });
  gw.addText(OWNER, TARGET, 'fixture request');
  const gateway = createGatewayClient({ url: gw.url, credential: CREDENTIAL });
  const r = { gw, gateway, dir, clock };
  r.restart = () => { r.publisher = createPublisher({ stateDir: path.join(dir, 'state'), city: 'test-city',
    cityDir: dir, gateway, clock: clock.now, gc: {} }); };
  r.restart();
  r.publish = (b = body()) => r.publisher.handlePublish(JSON.stringify(b));
  return r;
}

test('quote goes on the first chunk only and reports the first answer', async (t) => {
  const r = await setup(t);
  r.gw.quoteHeaders = { 'x-gc-quoted': 'true' };
  const sent = await r.publish(body({ text: 'a'.repeat(9000) }));
  assert.equal(sent.json.delivered, true);
  assert.equal(sent.json.metadata.quoted, 'true');
  assert.equal(sent.json.metadata.quote_reason, '');
  assert.deepEqual(r.gw.sends.map((s) => s.context), [{ message_id: TARGET }, undefined, undefined]);
});

test('quote rides on a file message', async (t) => {
  const r = await setup(t);
  const file = path.join(r.dir, 'fixture.txt');
  fs.writeFileSync(file, 'fixture attachment');
  r.gw.quoteHeaders = { 'x-gc-quoted': 'true' };
  const answer = await r.publish(body({ text: undefined, file }));
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.metadata.quoted, 'true');
  assert.deepEqual(r.gw.sends[0].context, { message_id: TARGET });
});

for (const id of ['{message_ts}', 'two words', 'a\nb', 'a\u0000b', 'a\u007fb', 'x'.repeat(257), null, 1]) {
  test(`invalid quote id ${JSON.stringify(id).slice(0, 35)} sends plain`, async (t) => {
    const r = await setup(t);
    const answer = await r.publish(body({ reply_to_message_id: id }));
    assert.equal(answer.json.delivered, true);
    assert.equal(answer.json.metadata.quoted, 'false');
    assert.equal(answer.json.metadata.quote_reason, 'invalid_id');
    assert.equal(r.gw.sends[0].context, undefined);
  });
}

for (const [headers, quoted, reason] of [
  [{ 'x-gc-quoted': 'true' }, 'true', ''],
  [{ 'x-gc-quoted': 'false', 'x-gc-quote-reason': 'unknown_message' }, 'false', 'unknown_message'],
  [{ 'x-gc-quoted': 'false', 'x-gc-quote-reason': 'meta_refused' }, 'false', 'meta_refused'],
  [{ 'x-gc-quoted': 'unknown', 'x-gc-replayed': 'true' }, 'unknown', 'replayed'],
  [{}, 'false', 'gateway_without_quotes'],
]) test(`quote receipt reports ${reason || 'applied'}`, async (t) => {
  const r = await setup(t);
  r.gw.quoteHeaders = headers;
  const answer = await r.publish();
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.metadata.quoted, quoted);
  assert.equal(answer.json.metadata.quote_reason, reason);
});

test('quote on an update is accepted but omitted with the template reason', async (t) => {
  const r = await setup(t);
  const answer = await r.publisher.handlePublishUpdate(JSON.stringify({ to: OWNER, text: 'fixture update',
    idempotency_key: 'update-key', reply_to_message_id: TARGET }));
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.metadata.quoted, 'false');
  assert.equal(answer.json.metadata.quote_reason, 'template');
  assert.equal(r.gw.sends[0].context, undefined);
});

for (const retryId of [undefined, 'wamid.different']) test(`quote remains the first request after failure/restart (${retryId ?? 'omitted'})`, async (t) => {
  const r = await setup(t);
  const send = r.gateway.send;
  let first;
  r.gateway.send = async (request) => { first = request; throw Object.assign(new Error('refused'), { status: 500 }); };
  assert.equal((await r.publish()).json.delivered, false);
  r.gateway.send = send;
  r.restart();
  const answer = await r.publish(body({ reply_to_message_id: retryId }));
  assert.equal(answer.json.delivered, true);
  assert.deepEqual(r.gw.sends[0].context, { message_id: TARGET });
  assert.equal(r.gw.sends[0].idempotency_key, first.idempotency_key);
  const count = r.gw.sends.length;
  assert.deepEqual(await r.publish(body({ reply_to_message_id: 'wamid.third' })), answer);
  assert.equal(r.gw.sends.length, count);
});

test('pin: no quote keeps the exact existing wire and receipt', async (t) => {
  const r = await setup(t);
  const answer = await r.publish(body({ reply_to_message_id: undefined }));
  const { owner, at, ...wire } = r.gw.sends[0];
  assert.deepEqual(Object.keys(wire).sort(), ['idempotency_key', 'text', 'to', 'type']);
  assert.equal(wire.text, 'fixture reply');
  assert.deepEqual(answer.json, { message_id: 'wamid.out.1',
    conversation: { scope_id: 'test-city', provider: 'whatsapp', account_id: 'dynasty', conversation_id: OWNER, kind: 'dm' },
    delivered: true, idempotency_key: 'quote-key', metadata: { chunks: '1', message_ids: 'wamid.out.1' } });
});

for (const id of ['keeper:fixture:1', 'wamid.failed:status:failed']) test(`synthetic reply target ${id} never blocks delivery`, async (t) => {
  const r = await setup(t);
  r.gw.quoteHeaders = { 'x-gc-quoted': 'false', 'x-gc-quote-reason': 'unknown_message' };
  const answer = await r.publish(body({ reply_to_message_id: id }));
  assert.equal(answer.json.delivered, true);
  assert.equal(answer.json.metadata.quote_reason, 'unknown_message');
});

test('registered reply template renders the inbound message id in the fake gc paste', async (t) => {
  const r = await rig();
  t.after(() => r.close());
  r.start();
  await waitFor(() => r.gc.registrations.length === 1);
  r.gc.policy = () => ({ logPaste: true });
  r.gw.addText(OWNER, TARGET, 'fixture input');
  await waitFor(() => r.gc.pastes.length > 0);
  const pasted = JSON.stringify([...r.gc.logs.values()]);
  assert.ok(pasted.includes(`--to ${OWNER} --reply-to ${TARGET} --text-file`));
});
