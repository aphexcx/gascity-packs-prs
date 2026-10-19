// Exercise the real entrypoint, SDK, ws and TLS stack against local servers.
// Certificates are temporary: trust their CA, but keep hostname checks on.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
import http from 'node:http';
import https from 'node:https';
import { createRequire } from 'node:module';
import os from 'node:os';
import path from 'node:path';
import { after, before, test } from 'node:test';
import { setTimeout as delay } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

const require = createRequire(import.meta.url);
const { WebSocketServer } = createRequire(require.resolve('@wecom/aibot-node-sdk'))('ws');
const entrypoint = fileURLToPath(new URL('../src/index.js', import.meta.url));
const wsHost = 'openws.work.weixin.qq.com';
let fixtureDir;
let trusted;
let wrongHostname;

function certificate(name, hosts) {
  const keyPath = path.join(fixtureDir, `${name}.key`);
  const certPath = path.join(fixtureDir, `${name}.pem`);
  const configPath = path.join(fixtureDir, `${name}.cnf`);
  fs.writeFileSync(configPath, `[req]
prompt = no
distinguished_name = dn
x509_extensions = ext
[dn]
CN = ${hosts[0]}
[ext]
basicConstraints = critical,CA:TRUE
subjectAltName = ${hosts.map((host) => `DNS:${host}`).join(',')}
`);
  execFileSync('openssl', [
    'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:prime256v1',
    '-nodes', '-sha256', '-days', '1', '-config', configPath,
    '-keyout', keyPath, '-out', certPath,
  ], { stdio: 'ignore' });
  return { key: fs.readFileSync(keyPath), cert: fs.readFileSync(certPath), certPath };
}

before(() => {
  fixtureDir = fs.mkdtempSync(path.join(os.tmpdir(), 'wecom-ws-test-'));
  trusted = certificate('trusted', [wsHost, 'localhost']);
  wrongHostname = certificate('wrong-hostname', ['localhost']);
});
after(() => {
  if (fixtureDir) fs.rmSync(fixtureDir, { recursive: true, force: true });
});

async function stubServer(t, cert = trusted, onFrame = () => true) {
  const server = https.createServer({ key: cert.key, cert: cert.cert });
  const wss = new WebSocketServer({ server });
  const connections = [];
  wss.on('connection', (socket, req) => {
    const connection = { socket, sni: req.socket.servername, host: req.headers.host, frames: [] };
    connections.push(connection);
    socket.on('message', (data) => {
      const frame = JSON.parse(data);
      connection.frames.push(frame);
      if (onFrame(frame, connection, connections) !== false) acknowledge(connection, frame);
    });
  });
  t.after(async () => {
    for (const client of wss.clients) client.terminate();
    wss.close();
    await new Promise((resolve) => server.close(resolve));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  return { connections, port: server.address().port };
}

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

function startAdapter(t, { port, hostname = wsHost, address, cert = trusted, nodeArgs = [], extraEnv = {} }) {
  const dir = fs.mkdtempSync(path.join(fixtureDir, 'adapter-'));
  const socketPath = path.join(dir, 's');
  // A fresh environment prevents real service credentials, proxy settings or
  // TLS overrides from influencing these loopback-only tests.
  const env = {
    PATH: process.env.PATH,
    HOME: dir,
    NODE_EXTRA_CA_CERTS: cert.certPath,
    WECOM_BOT_ID: 'local-test-bot',
    WECOM_BOT_SECRET: 'local-test-placeholder',
    GC_CITY_NAME: 'local-test-city',
    WECOM_WS_URL: `wss://${hostname}:${port}`,
    WECOM_MEDIA_DIR: path.join(dir, 'media'),
    LISTEN_INTERNAL: '127.0.0.1:0',
    REGISTER_ON_START: 'false',
    WECOM_LIVENESS_STALL_AFTER_MS: '0',
    GC_SERVICE_SOCKET: socketPath,
    GC_SERVICE_URL_PREFIX: '/wecom',
    GC_API_BASE_URL: 'http://127.0.0.1:1',
    WECOM_OUTBOUND_MEDIA_ROOT: fs.realpathSync(dir),
    ...extraEnv,
  };
  if (address !== undefined) env.WECOM_WS_CONNECT_ADDRESS = address;
  const child = spawn(process.execPath, [...nodeArgs, entrypoint], { env, stdio: ['ignore', 'pipe', 'pipe'] });
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
  });
  return {
    dir: fs.realpathSync(dir),
    child,
    exited,
    logs: () => logs,
    request(endpoint, body) {
      return new Promise((resolve, reject) => {
        const req = http.request({ socketPath, path: endpoint, method: 'POST', agent: false,
          headers: { 'Content-Type': 'application/json' } }, (res) => {
          let data = '';
          res.setEncoding('utf8');
          res.on('data', (chunk) => { data += chunk; });
          res.on('end', () => resolve({ status: res.statusCode, body: JSON.parse(data) }));
          res.on('error', reject);
        });
        req.on('error', reject);
        req.end(JSON.stringify(body));
      });
    },
    async waitForLog(text, count = 1) {
      const deadline = Date.now() + 8000;
      while (logs.split(text).length - 1 < count) {
        assert.equal(child.exitCode, null, `adapter exited early: ${logs}`);
        assert.ok(Date.now() < deadline, `waiting for ${text}: ${logs}`);
        await delay(10);
      }
    },
  };
}

for (const [label, nodeArgs] of [
  ['family autoselection', ['--network-family-autoselection']],
  ['single-address lookup', ['--no-network-family-autoselection']],
]) {
  test(`pinned TLS connection authenticates and reconnects with hostname intact (${label})`, async (t) => {
    const server = await stubServer(t);
    const adapter = startAdapter(t, { ...server, address: '127.0.0.1', nodeArgs });
    await adapter.waitForLog('wecom long connection authenticated');
    server.connections[0].socket.terminate();
    await adapter.waitForLog('wecom long connection authenticated', 2);

    assert.equal(server.connections.length, 2);
    for (const connection of server.connections) {
      assert.equal(connection.sni, wsHost);
      assert.equal(connection.host, `${wsHost}:${server.port}`);
      assert.equal(connection.frames[0].cmd, 'aibot_subscribe');
    }
    assert.equal(adapter.logs().split('ws connect address pinned to 127.0.0.1').length - 1, 2);
    assert.doesNotMatch(adapter.logs(), /WARNING:/);
  });
}

test('another WebSocket hostname resolves normally despite a configured pin', async (t) => {
  const server = await stubServer(t);
  // Nothing listens on 127.0.0.2: a lookup override for localhost would fail.
  const adapter = startAdapter(t, { ...server, hostname: 'localhost', address: '127.0.0.2' });
  await adapter.waitForLog('wecom long connection authenticated');
  assert.equal(server.connections[0].sni, 'localhost');
  assert.doesNotMatch(adapter.logs(), /ws connect address pinned|WARNING:/);
});

for (const [label, address] of [['unset', undefined], ['empty', '']]) {
  test(`${label} address preserves normal DNS and authentication`, async (t) => {
    const server = await stubServer(t);
    const adapter = startAdapter(t, { ...server, hostname: 'localhost', address });
    await adapter.waitForLog('wecom long connection authenticated');
    assert.equal(server.connections[0].frames[0].cmd, 'aibot_subscribe');
    assert.equal(server.connections[0].sni, 'localhost');
    assert.doesNotMatch(adapter.logs(), /ws connect address pinned|WARNING:/);
  });
}

for (const [address, loggedValue] of [
  ...['::1', 'localhost', '256.1.2.3', '127.1', '127.0.0.1:443', ' 127.0.0.1 ', ' ']
    .map((address) => [address, JSON.stringify(address)]),
  ['127.0.0.1"\n', '"127.0.0.1\\"\\n"'],
  ['x'.repeat(65), `"${'x'.repeat(64)}"`],
]) {
  test(`invalid address ${JSON.stringify(address)} warns once and uses DNS across reconnects`, async (t) => {
    const server = await stubServer(t);
    const adapter = startAdapter(t, { ...server, hostname: 'localhost', address });
    await adapter.waitForLog('wecom long connection authenticated');
    server.connections[0].socket.terminate();
    await adapter.waitForLog('wecom long connection authenticated', 2);
    assert.equal(adapter.logs().split(`WARNING: WECOM_WS_CONNECT_ADDRESS ${loggedValue} must be an IPv4 address; ignoring`).length - 1, 1);
    assert.equal(server.connections.length, 2);
    assert.doesNotMatch(adapter.logs(), /ws connect address pinned/);
  });
}

test('a pin still rejects a trusted certificate for the wrong hostname', async (t) => {
  const server = await stubServer(t, wrongHostname);
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', cert: wrongHostname });
  await adapter.waitForLog('Hostname/IP does not match');
  assert.equal(server.connections.length, 0);
  assert.doesNotMatch(adapter.logs(), /wecom long connection authenticated/);
});

// Publish → real HTTP listener → publisher → real SDK → scripted WS peer.
// Timing knobs are env overrides, exercising production configuration too.
const remediationEnv = {
  WECOM_LIVENESS_RECONNECT: 'true',
  WECOM_LIVENESS_STALL_AFTER_MS: '500',
  WECOM_LIVENESS_TICK_MS: '20',
  WECOM_LIVENESS_RECONNECT_MIN_INTERVAL_MS: '10000',
  WECOM_RECONNECT_DRAIN_TIMEOUT_MS: '2000',
};
const textPublish = (key = 'text-key', text = 'hello') => ({
  conversation: { conversation_id: 'local-user' }, text, idempotency_key: key,
});
const sends = (server) => server.connections.flatMap((c) => c.frames.filter((f) => f.cmd === 'aibot_send_msg'));

test('remediation waits for an in-flight publish to ack, then reconnects exactly once', async (t) => {
  const server = await stubServer(t, trusted, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: remediationEnv });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = adapter.request('/publish', textPublish());
  await waitFor(() => sends(server).length === 1, 'publish frame');
  await adapter.waitForLog('waiting up to 2000 ms for in-flight publishes/uploads');
  await delay(80); // several more ticks must not queue more closes
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
  acknowledge(server.connections[0], sends(server)[0]);
  assert.equal((await result).body.delivered, true);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.equal(sends(server).length, 1);
  assert.equal(adapter.logs().split('waiting up to').length - 1, 1);
  assert.equal(server.connections.length, 2);
});

test('socket loss retries only the interrupted chunk with the same wire id after authentication', async (t) => {
  let lost;
  let subscription;
  const server = await stubServer(t, trusted, (frame, connection, connections) => {
    if (frame.cmd === 'aibot_send_msg' && connections.length === 1
      && connection.frames.filter((f) => f.cmd === 'aibot_send_msg').length === 2) {
      lost = frame;
      connection.socket.terminate();
      return false;
    }
    if (frame.cmd === 'aibot_subscribe' && connections.length === 2) {
      subscription = frame;
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1' });
  await adapter.waitForLog('wecom long connection authenticated');
  const request = textPublish('multi-chunk', 'a'.repeat(3800) + 'b'.repeat(3800) + 'last');
  const result = adapter.request('/publish', request);
  await waitFor(() => subscription, 'reconnect subscription');
  await delay(50);
  assert.equal(sends(server).length, 2, 'open but unauthenticated sockets cannot retry');
  acknowledge(server.connections[1], subscription);
  const response = await result;
  assert.equal(response.status, 200);
  assert.equal(response.body.delivered, true);
  const frames = sends(server);
  assert.equal(frames.length, 4);
  assert.deepEqual(frames[2], lost, 'the retry preserves the entire wire frame');
  assert.equal(new Set(frames.map((f) => f.headers.req_id)).size, 3, 'each chunk has its own id');
  assert.equal((await adapter.request('/publish', request)).body.delivered, true);
  assert.equal(sends(server).length, 4, 'HTTP same-key retry returns the receipt without sending');
});

test('quiet inbound with a recent outbound ack skips remediation until that evidence expires', async (t) => {
  const server = await stubServer(t);
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: {
    ...remediationEnv,
    WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '1200',
    WECOM_LIVENESS_RECONNECT_MIN_INTERVAL_MS: '700',
    WECOM_LIVENESS_ALARM_REPEAT_MS: '100',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  assert.equal((await adapter.request('/publish', textPublish())).body.delivered, true);
  await adapter.waitForLog('INBOUND LIVENESS ALARM', 2);
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /forcing a WS reconnect/);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  await delay(200);
  assert.equal(server.connections.length, 2, 'reconnect cadence still limits dead sockets');
  await adapter.waitForLog('wecom long connection authenticated', 3);
});

test('a second socket loss stops retrying and retains delivery_unknown and same-key refusal', async (t) => {
  const server = await stubServer(t, trusted, (frame, connection) => {
    if (frame.cmd === 'aibot_send_msg') {
      connection.socket.terminate();
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1' });
  await adapter.waitForLog('wecom long connection authenticated');
  const request = textPublish('twice-lost');
  const result = await adapter.request('/publish', request);
  assert.equal(result.status, 502);
  assert.equal(result.body.delivered, false);
  assert.equal(result.body.failure_kind, 'delivery_unknown');
  assert.equal(result.body.chunk, 1);
  assert.equal(result.body.chunks_delivered, 0);
  assert.equal(sends(server).length, 2);
  assert.deepEqual(sends(server)[0], sends(server)[1]);
  const refused = await adapter.request('/publish', request);
  assert.equal(refused.status, 409);
  assert.equal(refused.body.failure_kind, 'delivery_unknown');
  await adapter.waitForLog('wecom long connection authenticated', 3);
  assert.equal(sends(server).length, 2, 'no third attempt after another authentication');
});

test('reconnect authentication wait is bounded and preserves the original failure', async (t) => {
  const server = await stubServer(t, trusted, (frame, connection) => {
    if (frame.cmd === 'aibot_send_msg') {
      connection.socket.terminate();
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: {
    WECOM_PUBLISH_RECONNECT_WAIT_MS: '100',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = await adapter.request('/publish', textPublish());
  assert.equal(result.status, 502);
  assert.equal(result.body.failure_kind, 'delivery_unknown');
  assert.equal(sends(server).length, 1);
  await adapter.waitForLog('wecom long connection authenticated', 2);
  assert.equal(sends(server).length, 1);
});

test('provider rejection is not retried and its acknowledgement suppresses a quiet-inbound cycle', async (t) => {
  const server = await stubServer(t, trusted, (frame, connection) => {
    if (frame.cmd === 'aibot_send_msg') {
      connection.socket.send(JSON.stringify({ headers: frame.headers, errcode: 40001, errmsg: 'test refusal' }));
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: {
    ...remediationEnv, WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '10000',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = await adapter.request('/publish', textPublish());
  assert.equal(result.status, 502);
  assert.equal(result.body.failure_kind, 'provider_error');
  assert.equal(result.body.errcode, 40001);
  await adapter.waitForLog('INBOUND LIVENESS ALARM');
  await delay(80);
  assert.equal(sends(server).length, 1);
  assert.equal(server.connections.length, 1);
  assert.doesNotMatch(adapter.logs(), /forcing a WS reconnect/);
});

test('an acknowledgement timeout alone does not retry the message', async (t) => {
  const server = await stubServer(t, trusted, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1' });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = await adapter.request('/publish', textPublish());
  assert.equal(result.status, 502);
  assert.equal(result.body.failure_kind, 'delivery_unknown');
  assert.equal(sends(server).length, 1);
  assert.equal(server.connections.length, 1);
});

test('media and caption each retry their own interrupted frame without uploading again', async (t) => {
  const server = await stubServer(t, trusted, (frame, connection, connections) => {
    if (frame.cmd === 'aibot_send_msg'
      && ((frame.body.msgtype === 'file' && connections.length === 1)
        || (frame.body.msgtype === 'markdown' && connections.length === 2))) {
      connection.socket.terminate();
      return false;
    }
  });
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: {
    WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS: 'true',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const filePath = path.join(adapter.dir, 'file.bin');
  fs.writeFileSync(filePath, 'file bytes');
  const result = await adapter.request('/publish-media', {
    conversation: { conversation_id: 'local-user' },
    file_path: filePath, media_kind: 'file', text: 'caption', idempotency_key: 'media-key',
  });
  assert.equal(result.status, 200);
  assert.equal(result.body.delivered, true);
  const frames = sends(server);
  assert.equal(frames.length, 4);
  assert.deepEqual(frames[0], frames[1]);
  assert.deepEqual(frames[2], frames[3]);
  assert.notEqual(frames[0].headers.req_id, frames[2].headers.req_id);
  assert.equal(server.connections.flatMap((c) => c.frames).filter((f) => f.cmd === 'aibot_upload_media_init').length, 1);
});

test('the drain deadline forces a close of a hung send, whose single retry can succeed', async (t) => {
  const server = await stubServer(t, trusted, (frame, connection, connections) => {
    if (frame.cmd === 'aibot_send_msg' && connections.length === 1) return false;
  });
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: {
    ...remediationEnv, WECOM_RECONNECT_DRAIN_TIMEOUT_MS: '100',
  } });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = await adapter.request('/publish', textPublish());
  assert.equal(result.status, 200);
  assert.equal(result.body.delivered, true);
  assert.equal(server.connections.length, 2);
  assert.equal(sends(server).length, 2);
  assert.deepEqual(sends(server)[0], sends(server)[1]);
  assert.match(adapter.logs(), /waiting up to 100 ms/);
});

test('SIGTERM waits for an active publish acknowledgement before the manual close', async (t) => {
  const server = await stubServer(t, trusted, (frame) => frame.cmd !== 'aibot_send_msg');
  const adapter = startAdapter(t, { ...server, address: '127.0.0.1' });
  await adapter.waitForLog('wecom long connection authenticated');
  const result = adapter.request('/publish', textPublish());
  await waitFor(() => sends(server).length === 1, 'publish frame');
  adapter.child.kill('SIGTERM');
  await adapter.waitForLog('waiting up to 30000 ms');
  assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
  acknowledge(server.connections[0], sends(server)[0]);
  assert.equal((await result).body.delivered, true);
  assert.deepEqual(await adapter.exited, [0, null]);
  assert.equal(server.connections.length, 1);
});

for (const engine of ['adapter', 'sdk']) {
  test(`remediation waits for a retained ${engine} upload after its HTTP deadline`, async (t) => {
    let chunk;
    const server = await stubServer(t, trusted, (frame) => {
      if (frame.cmd === 'aibot_upload_media_chunk') { chunk = frame; return false; }
    });
    const adapter = startAdapter(t, { ...server, address: '127.0.0.1', extraEnv: {
      ...remediationEnv,
      WECOM_UPLOAD_ENGINE: engine,
      WECOM_UPLOAD_TIMEOUT_MS: '100',
      WECOM_LIVENESS_OUTBOUND_ACK_FRESH_MS: '100',
    } });
    await adapter.waitForLog('wecom long connection authenticated');
    const filePath = path.join(adapter.dir, 'file.bin');
    fs.writeFileSync(filePath, 'file bytes');
    const result = await adapter.request('/upload-probe', { file_path: filePath, media_kind: 'file' });
    assert.equal(result.status, 502);
    assert.equal(result.body.uploaded, false);
    assert.ok(chunk);
    await adapter.waitForLog('waiting up to 2000 ms');
    assert.doesNotMatch(adapter.logs(), /WebSocket connection manually closed/);
    acknowledge(server.connections[0], chunk);
    await adapter.waitForLog('wecom long connection authenticated', 2);
    assert.equal(server.connections[0].frames.filter((f) => f.cmd === 'aibot_upload_media_finish').length, 1);
  });
}
