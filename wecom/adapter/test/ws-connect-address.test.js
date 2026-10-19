// Exercise the real entrypoint, SDK, ws and TLS stack against local servers.
// Certificates are temporary: trust their CA, but keep hostname checks on.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { once } from 'node:events';
import fs from 'node:fs';
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

async function stubServer(t, cert = trusted) {
  const server = https.createServer({ key: cert.key, cert: cert.cert });
  const wss = new WebSocketServer({ server });
  const connections = [];
  wss.on('connection', (socket, req) => {
    const connection = { socket, sni: req.socket.servername, host: req.headers.host, frames: [] };
    connections.push(connection);
    socket.on('message', (data) => {
      const frame = JSON.parse(data);
      connection.frames.push(frame);
      socket.send(JSON.stringify({ headers: frame.headers, errcode: 0, errmsg: 'ok' }));
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

function startAdapter(t, { port, hostname = wsHost, address, cert = trusted, nodeArgs = [] }) {
  const dir = fs.mkdtempSync(path.join(fixtureDir, 'adapter-'));
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
    logs: () => logs,
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

for (const address of ['::1', 'localhost', '256.1.2.3', '127.1', '127.0.0.1:443', ' 127.0.0.1 ', ' ']) {
  test(`invalid address ${JSON.stringify(address)} warns once and uses DNS across reconnects`, async (t) => {
    const server = await stubServer(t);
    const adapter = startAdapter(t, { ...server, hostname: 'localhost', address });
    await adapter.waitForLog('wecom long connection authenticated');
    server.connections[0].socket.terminate();
    await adapter.waitForLog('wecom long connection authenticated', 2);
    assert.equal(adapter.logs().split('WARNING: WECOM_WS_CONNECT_ADDRESS must be an IPv4 address; ignoring').length - 1, 1);
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
