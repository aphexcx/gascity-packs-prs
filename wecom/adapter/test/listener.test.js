// listener.test.js — request hygiene of the internal HTTP surface
// (src/listener.js), against a real http.Server and a stub publisher.
// Pins the jg-d0xr finding-10 fixes: exact route matching, the
// Content-Length check, the streaming body cap with 413, and the
// multibyte-safe body decoding that moved here from index.js.

import assert from 'node:assert/strict';
import http from 'node:http';
import { test } from 'node:test';

import { createRequestListener, hardenServer } from '../src/listener.js';

function startServer(t, { maxBodyBytes } = {}) {
  const seen = [];
  const publisher = {
    handlePublish: async (req, res, body) => {
      seen.push({ endpoint: 'publish', body });
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true, endpoint: 'publish', bytes: Buffer.byteLength(body) }));
    },
    handlePublishMedia: async (req, res, body) => {
      seen.push({ endpoint: 'publish-media', body });
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true, endpoint: 'publish-media' }));
    },
  };
  const server = hardenServer(http.createServer(createRequestListener({
    publisher,
    maxBodyBytes,
  })));
  t.after(() => server.close());
  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      resolve({ seen, port: server.address().port, server });
    });
  });
}

// request performs one HTTP exchange; body may be a string or a list of
// chunks written sequentially. Resolves { status, text } — or rejects if
// the server severed the connection before answering.
function request(port, { method = 'POST', path = '/publish', headers = {}, chunks = [] }) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, method, path, headers }, (res) => {
      let text = '';
      res.setEncoding('utf8');
      res.on('data', (d) => { text += d; });
      res.on('end', () => resolve({ status: res.statusCode, text }));
    });
    req.on('error', reject);
    for (const chunk of chunks) req.write(chunk);
    req.end();
  });
}

test('routes match exact pathnames only', async (t) => {
  const { seen, port } = await startServer(t);

  assert.equal((await request(port, { method: 'GET', path: '/healthz', chunks: [] })).status, 200);
  assert.equal((await request(port, { method: 'GET', path: '/healthz?probe=1' })).status, 200);
  assert.equal((await request(port, { path: '/publish', chunks: ['{}'] })).status, 200);
  assert.equal((await request(port, { path: '/publish-media', chunks: ['{}'] })).status, 200);

  // The old startsWith routing matched all of these.
  for (const path of ['/publishx', '/publish/extra', '/publish-mediax', '/publish-media/x']) {
    assert.equal((await request(port, { path, chunks: ['{}'] })).status, 404, path);
  }
  assert.equal((await request(port, { method: 'POST', path: '/healthz' })).status, 404);
  assert.equal((await request(port, { method: 'GET', path: '/publish' })).status, 404);
  assert.deepEqual(seen.map((s) => s.endpoint), ['publish', 'publish-media']);
});

test('a declared Content-Length beyond the cap is refused up front', async (t) => {
  const { seen, port } = await startServer(t, { maxBodyBytes: 64 });

  const res = await request(port, {
    path: '/publish',
    headers: { 'Content-Length': '100000' },
    chunks: ['x'.repeat(16)], // never gets to send the rest
  }).catch((err) => ({ severed: err.code }));
  if (!res.severed) {
    assert.equal(res.status, 413);
  }
  assert.equal(seen.length, 0, 'the handler must never see an oversized request');
});

test('a streamed body beyond the cap is aborted with 413', async (t) => {
  const { seen, port } = await startServer(t, { maxBodyBytes: 64 });

  // Chunked transfer (no Content-Length) sneaks past the header check;
  // the streaming counter must still stop it.
  const res = await request(port, {
    path: '/publish',
    chunks: Array.from({ length: 8 }, () => 'y'.repeat(32)),
  }).catch((err) => ({ severed: err.code }));
  if (!res.severed) {
    assert.equal(res.status, 413);
  }
  assert.equal(seen.length, 0);
});

test('a body within the cap flows through intact, multibyte splits included', async (t) => {
  const { seen, port } = await startServer(t, { maxBodyBytes: 1024 });

  // Split a Chinese character's UTF-8 bytes across two writes: the
  // listener must reassemble it, not decode each chunk independently.
  const utf8 = Buffer.from('{"text":"你好"}', 'utf8');
  const res = await request(port, {
    path: '/publish',
    chunks: [utf8.subarray(0, 11), utf8.subarray(11)],
  });
  assert.equal(res.status, 200);
  assert.equal(seen.length, 1);
  assert.equal(seen[0].body, '{"text":"你好"}');
  assert.equal(JSON.parse(res.text).bytes, utf8.length);
});

test('hardenServer sets the header and request deadlines', async (t) => {
  const { server } = await startServer(t);
  assert.equal(server.headersTimeout, 10000);
  assert.equal(server.requestTimeout, 60000);
});
