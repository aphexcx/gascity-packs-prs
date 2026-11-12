import assert from 'node:assert/strict';
import path from 'node:path';
import test from 'node:test';
import { createLink } from '../src/link.js';
import { createInbound } from '../src/inbound.js';
import { createGatewayClient } from '../src/gateway.js';
import { createGcClient } from '../src/gc.js';
import { openJournal } from '../src/journal.js';
import { createDeadLetters } from '../src/deadletter.js';
import { CITY, CREDENTIAL, rig, sleep, waitFor } from './fakes.js';

async function engine(t, { keeper, WebSocketImpl = globalThis.WebSocket } = {}) {
  const r = await rig();
  const stateDir = path.join(r.dir, '.gc', 'whatsapp');
  const journal = openJournal({ stateDir, clock: r.clock.now });
  const inbound = createInbound({ journal, deadLetters: createDeadLetters({ stateDir }),
    gateway: createGatewayClient({ url: r.gw.url, credential: CREDENTIAL, timeoutMs: 2000 }),
    gc: createGcClient({ base: r.gc.url, city: CITY, postTimeoutMs: 2000 }),
    city: CITY, clock: r.clock.now, keeper, pollMs: 2000,
    heldConnection: { gatewayURL: r.gw.url, credential: CREDENTIAL, enabled: true, WebSocketImpl },
  });
  let running = true;
  const loop = (async () => { while (running) { await inbound.tick(); await sleep(10); } })();
  t.after(async () => { running = false; inbound.stop(); await loop; await sleep(100); journal.close(); await r.close(); });
  return { ...r, inbound, journal };
}

test('held: a runtime without WebSocket polls at its original interval', async (t) => {
  const r = await engine(t, { WebSocketImpl: null });
  await waitFor(() => r.inbound.status().polls === 1);
  assert.equal(r.inbound.status().link.state, 'unavailable');
  const id = r.gw.addText('invented-owner', 'wamid.no-runtime', 'invented fallback');
  r.clock.advance(1999);
  await sleep(50);
  assert.equal(r.inbound.status().polls, 1);
  r.clock.advance(1);
  await waitFor(() => r.gw.acked.has(id));
  assert.equal(r.inbound.status().polls, 2);
  assert.equal(r.gw.link.attempts.length, 0);
});

test('held: an owed keeper bind retries at the original cadence without another inbox read', async (t) => {
  const keeper = { failing: true, attempts: 0,
    bindInvite() { this.attempts += 1; if (this.failing) throw new Error('injected keeper write failure'); return true; } };
  const r = await engine(t, { keeper });
  await waitFor(() => r.inbound.status().link.state === 'up');
  await sleep(100);
  const id = r.gw.addDelivery({ kind: 'joined', wa_id: 'invented-owner',
    message: { id: 'wamid.owed-bind', from: 'invented-owner', type: 'text', text: { body: '123456' }, timestamp: String(r.clock.now() / 1000) } });
  r.gw.link.wake();
  await waitFor(() => r.inbound.status().binds_owed === 1);
  const before = r.inbound.status().polls;
  const attempts = keeper.attempts;
  keeper.failing = false;
  r.clock.advance(2000);
  await waitFor(() => r.inbound.status().binds_owed === 0);
  assert.ok(keeper.attempts > attempts);
  assert.equal(r.inbound.status().polls, before);
  await waitFor(() => r.gw.acked.has(id));
});

test('held: URL conversion strips query and credentials, with no subprotocol', () => {
  for (const [input, output] of [['https://invented.invalid/path?secret=ignored', 'wss://invented.invalid/v1/wake'],
    ['http://invented.invalid', 'ws://invented.invalid/v1/wake']]) {
    let seen;
    class Client {
      constructor(url, options) { seen = { url: String(url), options }; }
      addEventListener() {}
      close() {}
    }
    const link = createLink({ gatewayURL: input, credential: 'invented-token', clock: () => 0, WebSocketImpl: Client });
    link.tick();
    assert.deepEqual(seen, { url: output, options: { headers: { Authorization: 'Bearer invented-token' } } });
    link.stop();
  }
});
