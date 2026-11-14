import assert from 'node:assert/strict';
import path from 'node:path';
import test from 'node:test';
import { createLink } from '../src/link.js';
import { createInbound } from '../src/inbound.js';
import { createGatewayClient } from '../src/gateway.js';
import { createGcClient } from '../src/gc.js';
import { openJournal } from '../src/journal.js';
import { createDeadLetters } from '../src/deadletter.js';
import { loadConfig } from '../src/index.js';
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

// gp-urx1 item 4: a socket that opens and is closed at once is a failure like
// any other; only a link that stayed up (its first pong) restarts the ladder.
test('held: only a pong restarts the reconnect ladder; an open alone does not (gp-urx1)', () => {
  let now = 0;
  const sockets = [];
  class Socket extends EventTarget {
    constructor() { super(); sockets.push(this); this.answers = false; }
    send(data) { if (data === 'ping' && this.answers) this.dispatchEvent(new MessageEvent('message', { data: 'pong' })); }
    close() {}
  }
  const link = createLink({ gatewayURL: 'https://invented.invalid', credential: 'invented-token', clock: () => now, WebSocketImpl: Socket });
  const reconnectsAfter = (ms) => {
    const count = sockets.length;
    now += ms - 1;
    link.tick();
    assert.equal(sockets.length, count, `no reconnect before ${ms} ms`);
    now += 1;
    link.tick();
    assert.equal(sockets.length, count + 1, `a reconnect at ${ms} ms`);
  };
  link.tick();
  assert.equal(sockets.length, 1);
  for (const rung of [1000, 5000, 15000]) {
    sockets.at(-1).dispatchEvent(new Event('open'));
    assert.equal(link.status().state, 'up');
    sockets.at(-1).dispatchEvent(new Event('close'));
    reconnectsAfter(rung);
  }
  // This socket stays up and answers its first ping; then it drops.
  const steady = sockets.at(-1);
  steady.answers = true;
  steady.dispatchEvent(new Event('open'));
  now += 30000;
  link.tick();
  assert.equal(link.status().state, 'up');
  steady.dispatchEvent(new Event('close'));
  reconnectsAfter(1000);
  link.stop();
});

// gp-urx1 item 2: one parse of the box's switch, through the adapter's config.
// [value, link on, value known]; false, 0 and no stay on (ruling B).
test('held: WHATSAPP_HELD_CONNECTION is off in any case and with spaces; absent and empty are on (gp-urx1)', () => {
  const table = [
    [undefined, true, true], ['', true, true], ['   ', true, true], ['on', true, true], ['ON', true, true], [' On ', true, true],
    ['off', false, true], ['OFF', false, true], ['Off', false, true], [' off', false, true], ['off ', false, true], ['\tOFF\n', false, true],
    ['false', true, false], ['0', true, false], ['no', true, false], ['disabled', true, false],
  ];
  const saved = process.env;
  try {
    const seen = table.map(([value]) => {
      process.env = { GC_CITY_NAME: CITY, GC_API_BASE_URL: 'http://127.0.0.1:9', WHATSAPP_GATEWAY_URL: 'https://invented.invalid' };
      if (value !== undefined) process.env.WHATSAPP_HELD_CONNECTION = value;
      const cfg = loadConfig();
      return [value, cfg.heldConnection, cfg.heldConnectionUnknown === ''];
    });
    assert.deepEqual(seen, table);
  } finally {
    process.env = saved;
  }
});
