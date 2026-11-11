// The fake gateway pins every box-side route of the gateway contract v1 (the
// gateway spec's SCOPE item 3, frozen; whatsapp/gateway/ implements it), and
// the adapter's client calls exactly those routes.

import assert from 'node:assert/strict';
import test from 'node:test';

import { createGatewayClient } from '../src/gateway.js';
import { CREDENTIAL, FAKE_GATEWAY_ROUTES, startFakeGateway } from './fakes.js';

// City side and the unauthenticated health check, verbatim from the contract:
// "POST /v1/enroll {code, name} with NO bearer", "POST /v1/invites",
// "GET /v1/inbox?after=<id>&limit=<1..200>", "POST /v1/inbox/ack {ids:[...]}",
// "POST /v1/send {to, idempotency_key, type: text|reaction|template, ...}",
// "POST /v1/read {message_id}", "GET /v1/window", "GET /v1/media/<media_id>",
// "GET /healthz {ok, pending_total, last_webhook_at}". The Meta side
// (/meta/webhook) and the operator side (/v1/admin/*) are the gateway's own
// and never called by a box.
const CONTRACT_V1_BOX_ROUTES = [
  'GET /healthz',
  'POST /v1/enroll',
  'POST /v1/invites',
  'GET /v1/inbox',
  'POST /v1/inbox/ack',
  'POST /v1/send',
  'POST /v1/read',
  'GET /v1/window',
  'GET /v1/media/<media_id>',
];
const FILES_OUT_ROUTES = ['POST /v1/uploads'];

test('the fake gateway serves v1 plus the additive files-out route', () => {
  assert.deepEqual([...FAKE_GATEWAY_ROUTES].sort(), [...CONTRACT_V1_BOX_ROUTES, ...FILES_OUT_ROUTES].sort());
});

test('every contract route answers on the fake through the adapter\'s client', async (t) => {
  const gw = await startFakeGateway();
  t.after(() => gw.close());
  gw.media.set('m1', { bytes: Buffer.from('jpeg'), mime: 'image/jpeg' });
  const id = gw.addText('5511900000001', 'wamid.C1', 'hi');
  const client = createGatewayClient({ url: gw.url, credential: CREDENTIAL });
  assert.equal((await client.healthz()).ok, true);
  assert.equal((await client.enroll({ code: 'enroll-ok', name: 'Hou Dynasty' })).credential, 'cred-minted-at-enroll');
  assert.match((await client.createInvite()).code, /^\d{6}$/);
  assert.deepEqual((await client.inbox({})).deliveries.map((d) => d.id), [id]);
  assert.deepEqual(await client.ack([id]), { ok: true });
  assert.deepEqual((await client.inbox({})).deliveries, [], 'an acked delivery is gone');
  assert.equal((await client.send({ to: '5511900000001', idempotency_key: 'k1', type: 'text', text: 'hello' })).message_id, 'wamid.out.1');
  assert.deepEqual(await client.read('wamid.C1'), { ok: true });
  const windows = (await client.window()).windows;
  assert.equal(windows[0].conversation_id, '5511900000001');
  assert.equal(windows[0].window_closes_at, windows[0].last_inbound_at + 86400 - 120, 'epoch seconds, the 120 s margin');
  const media = await client.media('m1');
  assert.equal(media.bytes.toString(), 'jpeg');
  assert.equal(media.contentType, 'image/jpeg');
  const up = await client.upload({ bytes: Buffer.from('jpeg'), size: 4, name: 'photo.jpg', mime: 'image/jpeg', sha256: 'a'.repeat(64) }, 'upload-k1');
  assert.equal(up.media_id, 'media.out.1');
  const unauth = createGatewayClient({ url: gw.url, credential: 'wrong' });
  await assert.rejects(unauth.inbox({}), (err) => err.status === 401 && err.auth === true);
  const seen = new Set(gw.requests.map((q) => q.route.replace(/^GET \/v1\/media\/.+$/, 'GET /v1/media/<media_id>')));
  for (const route of [...CONTRACT_V1_BOX_ROUTES, ...FILES_OUT_ROUTES]) assert.ok(seen.has(route), `${route} was exercised`);
});
