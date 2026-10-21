// HTTP publishes through the real entrypoint and SDK to a scripted WS peer.
// No credentials or external services: each child gets a fresh environment.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
import http from 'node:http';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';
import { createOutboundConnection } from '../src/connection.js';

const require = createRequire(import.meta.url);
const { WSClient } = require('@wecom/aibot-node-sdk');
const { WebSocketServer } = createRequire(require.resolve('@wecom/aibot-node-sdk'))('ws');
const entrypoint = fileURLToPath(new URL('../src/index.js', import.meta.url));
const remediationEnv = {
  WECOM_LIVENESS_RECONNECT: 'true',
  WECOM_LIVENESS_STALL_AFTER_MS: '1000',
  WECOM_LIVENESS_TICK_MS: '20',
  WECOM_LIVENESS_RECONNECT_MIN_INTERVAL_MS: '10000',
  WECOM_RECONNECT_DRAIN_TIMEOUT_MS: '2000',
};
const textPublish = (key = 'text-key', text = 'hello') => ({
  conversation: { conversation_id: 'local-user' }, text, idempotency_key: key,
});
const frames = (server, cmd) => server.connections.flatMap((c) => c.frames.filter((f) => f.cmd === cmd));
const sends = (server) => frames(server, 'aibot_send_msg');

function acknowledge(connection, frame) {
  const body = frame.cmd === 'aibot_upload_media_init' ? { upload_id: 'test-upload' }
    : frame.cmd === 'aibot_upload_media_finish' ? { media_id: 'test-media' } : {};
  connection.socket.send(JSON.stringify({ headers: frame.headers, errcode: 0, errmsg: 'ok', body }));
}

async function waitFor(predicate, label) {
  const deadline = Date.now() + 8000;
  while (!predicate()) {
    assert.ok(Date.now() < deadline, `waiting for ${label}`);
    await delay(10);
  }
}

async function stubServer(t, onFrame = () => true, options = {}) {
  const wss = new WebSocketServer({ port: 0, host: '127.0.0.1', ...options });
  const connections = [];
  wss.on('connection', (socket) => {
    const connection = { socket, frames: [] };
    connections.push(connection);
    socket.on('message', (data) => {
      const frame = JSON.parse(data);
      connection.frames.push(frame);
      if (onFrame(frame, connection, connections) !== false) acknowledge(connection, frame);
    });
  });
  t.after(async () => {
    for (const client of wss.clients) client.terminate();
    await new Promise((resolve) => wss.close(resolve));
  });
  await once(wss, 'listening');
  return { connections, port: wss.address().port };
}

function startAdapter(t, { port, extraEnv = {} }) {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'wecom-live-')));
  const socketPath = path.join(dir, 's');
  const child = spawn(process.execPath, [entrypoint], { env: {
    PATH: process.env.PATH,
    HOME: dir,
    WECOM_BOT_ID: 'local-test-bot',
    WECOM_BOT_SECRET: 'local-test-placeholder',
    GC_CITY_NAME: 'local-test-city',
    WECOM_WS_URL: `ws://127.0.0.1:${port}`,
    WECOM_MEDIA_DIR: path.join(dir, 'media'),
    REGISTER_ON_START: 'false',
    WECOM_LIVENESS_STALL_AFTER_MS: '0',
    GC_SERVICE_SOCKET: socketPath,
    GC_SERVICE_URL_PREFIX: '/wecom',
    GC_API_BASE_URL: 'http://127.0.0.1:1',
    WECOM_OUTBOUND_MEDIA_ROOT: dir,
    ...extraEnv,
  }, stdio: ['ignore', 'pipe', 'pipe'] });
  let logs = '';
  child.stdout.on('data', (data) => { logs += data; });
  child.stderr.on('data', (data) => { logs += data; });
  const exited = once(child, 'exit');
  t.after(async () => {
    if (child.exitCode === null && child.signalCode === null) {
      child.kill('SIGTERM');
      const killTimer = setTimeout(() => child.kill('SIGKILL'), 3000);
      try { await exited; } finally { clearTimeout(killTimer); }
    }
    fs.rmSync(dir, { recursive: true, force: true });
  });
  function openRequest(endpoint, body, options = {}) {
    let req;
    const result = new Promise((resolve, reject) => {
      req = http.request({ socketPath, path: endpoint, method: 'POST', agent: false,
        headers: { 'Content-Type': 'application/json' }, ...options }, (res) => {
        let data = '';
        res.setEncoding('utf8');
        res.on('data', (chunk) => { data += chunk; });
        res.on('end', () => resolve({ status: res.statusCode,
          body: endpoint === '/healthz' ? data : JSON.parse(data) }));
        res.on('error', reject);
      });
      req.on('error', reject);
      if (body !== undefined) req.end(JSON.stringify(body));
    });
    return { req, result };
  }
  return {
    dir, child, exited, openRequest,
    logs: () => logs,
    request: (endpoint, body) => openRequest(endpoint, body).result,
    health: () => {
      const { req, result } = openRequest('/healthz', undefined, { method: 'GET' });
      req.end();
      return result;
    },
    waitForLog: (text, count = 1) => waitFor(() => {
      assert.equal(child.exitCode, null, `adapter exited early: ${logs}`);
      return logs.split(text).length - 1 >= count;
    }, text),
  };
}

test('remediation waits for an in-flight publish acknowledgement, then reconnects exactly once', async (t) => {
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, extraEnv: remediationEnv });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = adapter.request('/publish', textPublish());
  await waitFor(() => sends(server).length === 1, 'publish frame');
  await adapter.waitForLog('waiting up to 2000 ms for in-flight publishes/uploads');
  await delay(80); // multiple ticks must not queue duplicate cycles
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
  acknowledge(server.connections[0], sends(server)[0]);
  assert.equal((await result).body.delivered, true);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.equal(sends(server).length, 1);
  assert.equal(adapter.logs().split('waiting up to').length - 1, 1);
  assert.equal(server.connections.length, 2);
});

test('a process that never authenticates still reports stalled inbound liveness', async (t) => {
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_subscribe');
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_LIVENESS_RECONNECT: 'false',
  } });
  await waitFor(() => frames(server, 'aibot_subscribe').length === 1, 'initial subscription');
  assert.match((await adapter.health()).body, /inbound_liveness=ok/);
  await adapter.waitForLog('INBOUND LIVENESS ALARM');
  const health = await adapter.health();
  assert.equal(health.status, 200);
  assert.match(health.body, /^ok\ninbound_liveness=stalled/);
  assert.doesNotMatch(adapter.logs(), /wecom long connection authenticated/);
});

test('first authentication clears a startup stall and grants a full inbound grace period', async (t) => {
  let subscription;
  const server = await stubServer(t, (frame, connection) => {
    if (frame.cmd === 'aibot_subscribe' && !subscription) {
      subscription = { frame, connection };
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_LIVENESS_RECONNECT: 'false',
  } });
  await waitFor(() => subscription, 'initial subscription');
  await adapter.waitForLog('INBOUND LIVENESS ALARM');
  const acknowledgedAt = Date.now();
  acknowledge(subscription.connection, subscription.frame);
  await adapter.waitForLog('wecom long connection authenticated');
  assert.match(adapter.logs(), /RECOVERED — first authentication cleared a .* startup stall/);
  assert.match((await adapter.health()).body, /inbound_liveness=ok/);
  await delay(700); // 300 ms margin below the new grace period
  assert.match((await adapter.health()).body, /inbound_liveness=ok/);
  await adapter.waitForLog('INBOUND LIVENESS ALARM', 2);
  assert.ok(Date.now() - acknowledgedAt >= 1000, 'the grace period starts at authentication');
});

test('close before ack returns delivery_unknown without replaying any chunk after authentication', async (t) => {
  const server = await stubServer(t, (frame, connection) => {
    if (frame.cmd === 'aibot_send_msg'
      && connection.frames.filter((f) => f.cmd === 'aibot_send_msg').length === 2) {
      connection.socket.terminate();
      return false;
    }
  });
  const adapter = startAdapter(t, server);
  await adapter.waitForLog('wecom long connection authenticated');
  const request = textPublish('multi-chunk', 'a'.repeat(3800) + 'b'.repeat(3800) + 'last');
  const response = await adapter.request('/publish', request);
  assert.equal(response.status, 502);
  assert.equal(response.body.delivered, false);
  assert.equal(response.body.failure_kind, 'delivery_unknown');
  assert.equal(response.body.chunk, 2);
  assert.equal(response.body.chunks_delivered, 1);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  const refused = await adapter.request('/publish', request);
  assert.equal(refused.status, 409);
  assert.equal(refused.body.failure_kind, 'delivery_unknown');
  assert.equal(sends(server).length, 2);
  for (const frame of sends(server)) {
    assert.match(frame.headers.req_id, /^aibot_send_msg_\d+_[a-f0-9]{8}$/);
  }
});

test('fresh outbound ack suppresses cycles until it expires; inbound alarms remain', async (t) => {
  const server = await stubServer(t);
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv,
    WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '2000',
    WECOM_LIVENESS_ALARM_REPEAT_MS: '100',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  assert.equal((await adapter.request('/publish', textPublish())).body.delivered, true);
  await adapter.waitForLog('INBOUND LIVENESS ALARM', 2);
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /forcing a WS reconnect/);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  await delay(100);
  assert.equal(server.connections.length, 2, 'reconnect cadence still applies');
});

test('inbound silence at 3× the stall threshold cycles despite fresh outbound acks', async (t) => {
  const server = await stubServer(t);
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '60000',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const authenticatedAt = Date.now();
  assert.equal((await adapter.request('/publish', textPublish())).body.delivered, true);
  await adapter.waitForLog('INBOUND LIVENESS ALARM');
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /forcing a WS reconnect/);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.ok(Date.now() - authenticatedAt >= 2700, 'suppression lasts until the hard ceiling');
  assert.ok(Date.now() - authenticatedAt < 4500, 'fresh acks cannot mask dead pushes');
});

test('zero outbound-ack freshness disables suppression through production env parsing', async (t) => {
  const server = await stubServer(t);
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '0',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const authenticatedAt = Date.now();
  assert.equal((await adapter.request('/publish', textPublish())).body.delivered, true);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.ok(Date.now() - authenticatedAt < 2400, 'must cycle before the 3000 ms hard ceiling');
});

test('provider rejection is not retried and its ack suppresses a quiet-inbound cycle', async (t) => {
  const server = await stubServer(t, (frame, connection) => {
    if (frame.cmd === 'aibot_send_msg') {
      connection.socket.send(JSON.stringify({ headers: frame.headers, errcode: 40001, errmsg: 'test refusal' }));
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '10000',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const response = await adapter.request('/publish', textPublish());
  assert.equal(response.status, 502);
  assert.equal(response.body.failure_kind, 'provider_error');
  assert.equal(response.body.errcode, 40001);
  await adapter.waitForLog('INBOUND LIVENESS ALARM');
  assert.equal(sends(server).length, 1);
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /forcing a WS reconnect/);
});

for (const lostType of ['file', 'markdown']) {
  test(`socket loss during ${lostType === 'file' ? 'media send' : 'caption'} does not replay or upload again`, async (t) => {
    const server = await stubServer(t, (frame, connection) => {
      if (frame.cmd === 'aibot_send_msg' && frame.body.msgtype === lostType) {
        connection.socket.terminate();
        return false;
      }
    });
    const adapter = startAdapter(t, { ...server, extraEnv: { WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS: 'true' } });
    await adapter.waitForLog('wecom long connection authenticated');
    const filePath = path.join(adapter.dir, 'file.bin');
    fs.writeFileSync(filePath, 'file bytes');
    const request = { conversation: { conversation_id: 'local-user' },
      file_path: filePath, media_kind: 'file', text: 'caption', idempotency_key: 'media-key' };
    const response = await adapter.request('/publish-media', request);
    assert.equal(response.status, 502);
    assert.equal(response.body.failure_kind, 'delivery_unknown');
    await adapter.waitForLog('wecom long connection authenticated', 2);
    assert.equal((await adapter.request('/publish-media', request)).status, 409);
    assert.equal(sends(server).length, lostType === 'file' ? 1 : 2);
    assert.equal(frames(server, 'aibot_upload_media_init').length, 1);
    for (const frame of sends(server)) {
      assert.match(frame.headers.req_id, /^aibot_send_msg_\d+_[a-f0-9]{8}$/);
    }
  });
}

test('the drain deadline closes a hung send with 502 delivery_unknown and no retry', async (t) => {
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_RECONNECT_DRAIN_TIMEOUT_MS: '100',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = adapter.request('/publish', textPublish());
  await adapter.waitForLog('waiting up to 100 ms');
  const waitingAt = Date.now();
  const response = await result;
  assert.equal(response.status, 502);
  assert.equal(response.body.failure_kind, 'delivery_unknown');
  assert.ok(Date.now() - waitingAt < 1000, 'does not wait for the 5 s SDK ack timeout');
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.equal(sends(server).length, 1);
});

test('close waits for the SDK receiver to finish a compressed inbound frame', async (t) => {
  const server = await stubServer(t, undefined, { perMessageDeflate: true });
  const client = new WSClient({
    botId: 'local-test-bot', secret: 'local-test-placeholder',
    wsUrl: `ws://127.0.0.1:${server.port}`,
    logger: { debug() {}, info() {}, warn() {}, error() {} },
  });
  const connection = createOutboundConnection(client);
  t.after(() => client.disconnect());
  const authenticated = once(client, 'authenticated');
  client.connect();
  await authenticated;

  // Hold the real inflater's completion at the async boundary. terminate()
  // closes TCP here, but ws must still deliver this frame before close.
  const extension = client.wsManager.ws._extensions['permessage-deflate'];
  assert.ok(extension, 'the local peer negotiated compression');
  const decompress = extension.decompress.bind(extension);
  let release;
  const inflating = new Promise((resolve) => {
    extension.decompress = (data, fin, callback) => decompress(data, fin, (...args) => {
      release = () => { release = null; callback(...args); };
      resolve();
    });
  });
  t.after(() => release?.());
  const events = [];
  client.on('message.text', (frame) => events.push(frame.body.text.content));
  client.on('disconnected', () => events.push('disconnected'));
  server.connections[0].socket.send(JSON.stringify({
    cmd: 'aibot_msg_callback', headers: { req_id: 'compressed-inbound' }, body: {
      msgid: 'compressed-inbound', chattype: 'single', from: { userid: 'local-user' },
      msgtype: 'text', text: { content: 'received before close' },
    },
  }), { compress: true });
  await inflating;
  let closed = false;
  const closing = connection.close().then(() => { closed = true; events.push('drained'); });
  await delay(20);
  assert.equal(closed, false, 'terminate alone does not prove intake is drained');
  release();
  await closing;
  assert.deepEqual(events, ['received before close', 'disconnected', 'drained']);
});

test('SIGTERM stops admission, drains an acknowledged publish, and exits cleanly', async (t) => {
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, server);
  await adapter.waitForLog('wecom long connection authenticated');
  const result = adapter.request('/publish', textPublish());
  await waitFor(() => sends(server).length === 1, 'publish frame');
  // This request is connected before shutdown but has an unfinished body.
  const partial = adapter.openRequest('/publish', undefined, {
    headers: { 'Content-Type': 'application/json', Expect: '100-continue' },
  });
  partial.req.flushHeaders();
  await once(partial.req, 'continue'); // the listener has accepted this request
  partial.req.write('{');
  adapter.child.kill('SIGTERM');
  await adapter.waitForLog('waiting up to 1500 ms');
  const late = await adapter.request('/publish', textPublish('late'));
  assert.equal(late.status, 503);
  assert.equal(late.body.failure_kind, 'not_sent');
  assert.equal((await adapter.health()).status, 200);
  partial.req.end(JSON.stringify(textPublish('partial')).slice(1));
  const refused = await partial.result;
  assert.equal(refused.status, 503);
  assert.equal(refused.body.failure_kind, 'not_sent');
  assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
  acknowledge(server.connections[0], sends(server)[0]);
  assert.equal((await result).body.delivered, true);
  assert.deepEqual(await adapter.exited, [0, null]);
  assert.equal(sends(server).length, 1);
});

test('SIGTERM keeps gc media receipt callbacks open while refusing new publishes', async (t) => {
  let transcriptRequest;
  let transcriptResponse;
  const gc = http.createServer((req, res) => {
    assert.match(req.url, /\/extmsg\/outbound$/);
    let body = '';
    req.setEncoding('utf8');
    req.on('data', (data) => { body += data; });
    req.on('end', () => {
      transcriptRequest = JSON.parse(body);
      transcriptResponse = res;
    });
  });
  gc.listen(0, '127.0.0.1');
  await once(gc, 'listening');
  t.after(async () => {
    gc.closeAllConnections();
    await new Promise((resolve) => gc.close(resolve));
  });
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, extraEnv: {
    GC_API_BASE_URL: `http://127.0.0.1:${gc.address().port}`,
    WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS: 'true',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const filePath = path.join(adapter.dir, 'shutdown.bin');
  fs.writeFileSync(filePath, 'media delivered during shutdown');
  const request = { conversation: { conversation_id: 'local-user' },
    session_id: 'local-session', file_path: filePath, media_kind: 'file', idempotency_key: 'shutdown-media' };
  const publish = adapter.request('/publish-media', request);
  await waitFor(() => sends(server).length === 1, 'media send');
  const shutdownAt = Date.now();
  adapter.child.kill('SIGTERM');
  await adapter.waitForLog('waiting up to 1500 ms');
  assert.equal((await adapter.health()).status, 200);
  for (const [endpoint, body] of [
    ['/publish', textPublish('late')],
    ['/publish-media', { ...request, idempotency_key: 'late-media' }],
  ]) {
    const refused = await adapter.request(endpoint, body);
    assert.equal(refused.status, 503);
    assert.equal(refused.body.delivered, false);
    assert.equal(refused.body.failure_kind, 'not_sent');
  }
  acknowledge(server.connections[0], sends(server)[0]);
  await waitFor(() => transcriptRequest, 'media transcript request to gc');
  // A known key alone cannot admit an ordinary text publish during drain.
  for (const mismatch of [
    { ...transcriptRequest, text: 'new text with a media key' },
    { ...transcriptRequest, conversation: { conversation_id: 'another-user' } },
    { ...transcriptRequest, idempotency_key: 'another-key' },
  ]) {
    const refused = await adapter.request('/publish', mismatch);
    assert.equal(refused.status, 503);
    assert.equal(refused.body.failure_kind, 'not_sent');
  }
  const callback = await adapter.request('/publish', transcriptRequest);
  assert.equal(callback.status, 200);
  assert.equal(callback.body.delivered, true);
  transcriptResponse.writeHead(200, { 'Content-Type': 'application/json' })
    .end(JSON.stringify({ TranscriptEntry: { message_id: callback.body.message_id } }));
  const result = await publish;
  assert.equal(result.status, 200);
  assert.equal(result.body.delivered, true);
  assert.equal(result.body.transcript_recorded, true);
  assert.deepEqual(await adapter.exited, [0, null]);
  assert.ok(Date.now() - shutdownAt < 1800, 'media receipt drain must beat gc SIGKILL with margin');
  assert.equal(sends(server).length, 1, 'receipt callbacks never send transcript text to WeCom');
  assert.equal(frames(server, 'aibot_upload_media_init').length, 1);
  assert.equal(server.connections.length, 1, 'shutdown must not reconnect');
});

test('SIGTERM with an unacked publish returns 502 and exits after the bounded drain', async (t) => {
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, server);
  await adapter.waitForLog('wecom long connection authenticated');
  const result = adapter.request('/publish', textPublish());
  await waitFor(() => sends(server).length === 1, 'publish frame');
  const shutdownAt = Date.now();
  adapter.child.kill('SIGTERM');
  await adapter.waitForLog('waiting up to 1500 ms');
  const late = await adapter.request('/publish', textPublish('late'));
  assert.equal(late.status, 503);
  assert.equal(late.body.failure_kind, 'not_sent');
  const response = await result;
  assert.equal(response.status, 502);
  assert.equal(response.body.failure_kind, 'delivery_unknown');
  assert.deepEqual(await adapter.exited, [0, null]);
  const elapsed = Date.now() - shutdownAt;
  assert.ok(elapsed >= 1200 && elapsed < 1800, `shutdown took ${elapsed} ms`);
  assert.equal(sends(server).length, 1);
});

test('shutdown awaits a late inbound delivery after the outbound drain stops the socket', async (t) => {
  let inboundResponse;
  let inboundBody = '';
  const gc = http.createServer((req, res) => {
    assert.match(req.url, /\/extmsg\/inbound$/);
    req.setEncoding('utf8');
    req.on('data', (data) => { inboundBody += data; });
    req.on('end', () => { inboundResponse = res; });
  });
  gc.listen(0, '127.0.0.1');
  await once(gc, 'listening');
  t.after(async () => {
    gc.closeAllConnections();
    await new Promise((resolve) => gc.close(resolve));
  });
  const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, extraEnv: {
    GC_API_BASE_URL: `http://127.0.0.1:${gc.address().port}`,
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const publish = adapter.request('/publish', textPublish());
  await waitFor(() => sends(server).length === 1, 'publish frame');
  adapter.child.kill('SIGTERM');
  await adapter.waitForLog('waiting up to 1500 ms');
  // There was no inbound work at shutdown: the initial flush has finished.
  server.connections[0].socket.send(JSON.stringify({
    cmd: 'aibot_msg_callback', headers: { req_id: 'late-inbound-test' }, body: {
      msgid: 'late-inbound', chattype: 'single', from: { userid: 'local-user' },
      msgtype: 'text', text: { content: 'arrived during outbound drain' },
      create_time: Math.floor(Date.now() / 1000),
    },
  }));
  await waitFor(() => inboundResponse, 'late inbound delivery to gc');
  assert.match(JSON.parse(inboundBody).message.text, /arrived during outbound drain/);
  const socketClosed = once(server.connections[0].socket, 'close');
  acknowledge(server.connections[0], sends(server)[0]);
  assert.equal((await publish).body.delivered, true);
  await socketClosed;
  await delay(300);
  assert.equal(adapter.child.exitCode, null, 'exit must await gc accepting the late inbound');
  const acceptedAt = Date.now();
  inboundResponse.writeHead(200, { 'Content-Type': 'application/json' })
    .end(JSON.stringify({ delivery: { status: 'delivered' } }));
  assert.deepEqual(await adapter.exited, [0, null]);
  assert.ok(Date.now() - acceptedAt < 300, 'exit must follow the gc response, not the hard exit timer');
  assert.equal(server.connections.length, 1, 'shutdown must not reconnect');
});

for (const respond of [true, false]) {
  test(`shutdown flushes buffered inbound concurrently with outbound drain (${respond ? 'gc recovers' : 'gc hangs'})`, async (t) => {
    let inboundResponse;
    let inboundBody = '';
    const gc = http.createServer((req, res) => {
      req.setEncoding('utf8');
      req.on('data', (data) => { inboundBody += data; });
      req.on('end', () => { inboundResponse = res; });
    });
    gc.listen(0, '127.0.0.1');
    await once(gc, 'listening');
    t.after(async () => {
      gc.closeAllConnections();
      await new Promise((resolve) => gc.close(resolve));
    });
    const server = await stubServer(t, (frame) => frame.cmd !== 'aibot_send_msg');
    const adapter = startAdapter(t, { ...server, extraEnv: {
      GC_API_BASE_URL: `http://127.0.0.1:${gc.address().port}`,
    } });
    await adapter.waitForLog('wecom long connection authenticated');
    server.connections[0].socket.send(JSON.stringify({
      cmd: 'aibot_msg_callback', headers: { req_id: 'inbound-test' }, body: {
        msgid: 'buffered-inbound', chattype: 'single', from: { userid: 'local-user' },
        msgtype: 'text', text: { content: 'buffered before shutdown' }, create_time: Math.floor(Date.now() / 1000),
      },
    }));
    await waitFor(() => fs.existsSync(path.join(adapter.dir, 'conversation-kinds.json')), 'inbound observed');
    assert.equal(inboundResponse, undefined, 'inbound is still inside its coalescing window');
    const publish = adapter.request('/publish', textPublish());
    await waitFor(() => sends(server).length === 1, 'publish frame');
    const shutdownAt = Date.now();
    adapter.child.kill('SIGTERM');
    await waitFor(() => inboundResponse, 'inbound flush request');
    assert.match(inboundBody, /buffered before shutdown/);
    const response = await publish;
    assert.equal(response.status, 502);
    assert.equal(response.body.failure_kind, 'delivery_unknown');
    assert.equal(adapter.child.exitCode, null, 'shutdown still awaits the inbound flush');
    if (respond) {
      inboundResponse.writeHead(200, { 'Content-Type': 'application/json' })
        .end(JSON.stringify({ delivery: { status: 'delivered' } }));
    }
    assert.deepEqual(await adapter.exited, [0, null]);
    const elapsed = Date.now() - shutdownAt;
    // Allow 300 ms for scheduling around the production 1900 ms hard exit.
    assert.ok(elapsed < 2200, `shutdown took ${elapsed} ms`);
    if (respond) {
      assert.doesNotMatch(adapter.logs(), /shutdown hard exit/);
    } else {
      const pendingLog = adapter.logs().split('\n').filter((line) => line.includes('shutdown hard exit with pending inbound work: '));
      assert.equal(pendingLog.length, 1);
      const stats = JSON.parse(pendingLog[0].split('pending inbound work: ')[1]);
      assert.ok(stats.chains > 0);
      assert.ok(stats.pending > 0);
    }
  });
}

for (const engine of ['adapter', 'sdk']) {
  test(`remediation defers a retained ${engine} upload beyond both drain and hard-ceiling deadlines`, async (t) => {
    const chunks = [];
    const server = await stubServer(t, (frame) => {
      if (frame.cmd === 'aibot_upload_media_chunk') { chunks.push(frame); return false; }
    });
    const adapter = startAdapter(t, { ...server, extraEnv: {
      ...remediationEnv,
      WECOM_UPLOAD_ENGINE: engine,
      WECOM_UPLOAD_TIMEOUT_MS: '100',
      WECOM_RECONNECT_DRAIN_TIMEOUT_MS: '100',
      WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '0',
    } });
    await adapter.waitForLog('wecom long connection authenticated');
    const filePath = path.join(adapter.dir, 'file.bin');
    fs.writeFileSync(filePath, Buffer.alloc(600000));
    const response = await adapter.request('/upload-probe', { file_path: filePath, media_kind: 'file' });
    assert.equal(response.status, 502);
    assert.equal(response.body.uploaded, false);
    await adapter.waitForLog('reconnect cycle deferred');
    await delay(2300); // idle > 3× stall threshold; a progressing upload still wins
    assert.equal(server.connections.length, 1);
    assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
    acknowledge(server.connections[0], chunks[0]);
    await waitFor(() => chunks.length === 2, 'second upload chunk');
    acknowledge(server.connections[0], chunks[1]);
    await adapter.waitForLog('wecom long connection authenticated', 2);
    assert.equal(frames(server, 'aibot_upload_media_finish').length, 1);
    assert.equal(adapter.logs().split('reconnect cycle deferred').length - 1, 1);
  });
}

test('an upload admitted during a publish drain is protected beyond that drain deadline', async (t) => {
  let chunk;
  const server = await stubServer(t, (frame) => {
    if (frame.cmd === 'aibot_upload_media_chunk') { chunk = frame; return false; }
    return frame.cmd !== 'aibot_send_msg';
  });
  const adapter = startAdapter(t, { ...server, extraEnv: {
    ...remediationEnv, WECOM_RECONNECT_DRAIN_TIMEOUT_MS: '400',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const publish = adapter.request('/publish', textPublish());
  await adapter.waitForLog('waiting up to 400 ms');
  const filePath = path.join(adapter.dir, 'file.bin');
  fs.writeFileSync(filePath, 'file bytes');
  const upload = adapter.request('/upload-probe', { file_path: filePath, media_kind: 'file' });
  await waitFor(() => chunk, 'upload chunk');
  await delay(700); // 300 ms beyond the drain deadline
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
  acknowledge(server.connections[0], chunk);
  assert.equal((await upload).body.uploaded, true);
  assert.equal((await publish).body.failure_kind, 'delivery_unknown');
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.equal(sends(server).length, 1);
});
