// The gateway lane's contract-v1 readings (bead comments on gp-9hf1, 18:20Z
// and 19:04Z 11/4): every wire time is integer epoch SECONDS; a send can
// answer 409 in_flight (retry, same key) or outcome_unknown (the key is
// spent: check the chat, resend under a fresh key).

import assert from 'node:assert/strict';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';

import { createGatewayClient } from '../src/gateway.js';
import { parseInstant } from '../src/keeper.js';
import { createLedger, toMs } from '../src/ledger.js';
import { createPublisher } from '../src/outbound.js';

test('instants: epoch seconds, epoch milliseconds and ISO strings all read as the same instant', () => {
  const ms = Date.parse('2026-11-04T18:00:00Z');
  assert.equal(parseInstant(ms / 1000), ms);
  assert.equal(parseInstant(String(ms / 1000)), ms);
  assert.equal(parseInstant(ms), ms);
  assert.equal(parseInstant('2026-11-04T18:00:00Z'), ms);
  assert.equal(parseInstant(null), null);
  assert.equal(toMs(ms / 1000), ms);
});

function startGateway(answer) {
  const sends = [];
  const server = http.createServer((req, res) => {
    let body = '';
    req.on('data', (c) => { body += c; });
    req.on('end', () => {
      sends.push(JSON.parse(body || '{}'));
      const [status, json] = answer(sends.length);
      res.writeHead(status, { 'Content-Type': 'application/json' }).end(JSON.stringify(json));
    });
  });
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve({ server, sends, url: `http://127.0.0.1:${server.address().port}` })));
}

async function publishAgainst(t, answer) {
  const gw = await startGateway(answer);
  t.after(() => gw.server.close());
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wa-readings-'));
  const clock = () => Date.parse('2026-11-04T18:00:00Z');
  const ledger = createLedger({ stateDir: dir, clock });
  const publisher = createPublisher({
    stateDir: dir,
    gateway: createGatewayClient({ url: gw.url, credential: 'c' }),
    gc: { recordOutbound: async () => ({}) },
    city: 'test-city',
    clock,
    ledger,
  });
  const body = JSON.stringify({
    session_id: 's-mayor',
    conversation: { scope_id: 'test-city', provider: 'whatsapp', account_id: 'dynasty', conversation_id: '5511900000001', kind: 'dm' },
    text: 'hello',
    idempotency_key: 'k-readings',
  });
  return { answer: await publisher.handlePublish(body), gw };
}

test('a send answered 502 outcome_unknown is a permanent failure telling the mayor to check the chat and resend under a fresh key', async (t) => {
  const { answer } = await publishAgainst(t, () => [502, { error: 'outcome_unknown', meta_status: 500 }]);
  assert.equal(answer.status, 200);
  assert.equal(answer.json.delivered, false);
  assert.equal(answer.json.failure_kind, 'permanent');
  assert.equal(answer.json.metadata.reason, 'outcome_unknown');
  assert.match(answer.json.metadata.hint, /check the chat/);
  assert.match(answer.json.metadata.hint, /fresh idempotency key/);
});

test('a send answered 409 in_flight is transient: retry with the same key', async (t) => {
  const { answer } = await publishAgainst(t, () => [409, { error: 'in_flight' }]);
  assert.equal(answer.json.failure_kind, 'transient');
  assert.equal(answer.json.metadata.reason, 'in_flight');
});

test('a send answered 502 meta_error (Meta refused; nothing sent) is transient: the same key sends on a retry', async (t) => {
  const { answer } = await publishAgainst(t, () => [502, { error: 'meta_error', meta_status: 400, meta_code: 131026 }]);
  assert.equal(answer.json.failure_kind, 'transient');
});
