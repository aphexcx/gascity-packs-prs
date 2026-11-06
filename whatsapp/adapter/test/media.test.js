// media.test.js — inbound media: download through the gateway's
// GET /v1/media/<id>, the webhook's sha256, the store layout and modes, the
// quota and the free-disk floor, and the voice-note transcript, against
// in-test fakes: a node:http gateway and a node:http ElevenLabs Scribe, both
// on 127.0.0.1:0.
//
// Run from whatsapp/adapter: node --test test/media.test.js

import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { pathToFileURL } from 'node:url';

import { createGatewayClient } from '../src/gateway.js';
import { createMedia, describeAttachments } from '../src/media.js';

const PHONE = '15551234567';
const CREDENTIAL = 'test-credential';

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
}

function readBody(req) {
  return new Promise((resolve) => {
    const parts = [];
    req.on('data', (c) => parts.push(c));
    req.on('end', () => resolve(Buffer.concat(parts)));
  });
}

// The fake gateway: GET /v1/media/<id> streams the bytes with Meta's mime
// type; 404 for an id it does not hold.
async function startFakeGateway(t) {
  const gw = { media: new Map(), gets: [] };
  const server = http.createServer(async (req, res) => {
    await readBody(req);
    if (req.headers.authorization !== `Bearer ${CREDENTIAL}`) {
      res.writeHead(401, { 'Content-Type': 'application/json' });
      return res.end('{"error":"unauthorized"}');
    }
    const m = /^\/v1\/media\/(.+)$/.exec(req.url);
    const id = m ? decodeURIComponent(m[1]) : '';
    gw.gets.push(id);
    const item = gw.media.get(id);
    if (!item) {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      return res.end('{"error":"not_found"}');
    }
    res.writeHead(200, { 'Content-Type': item.mime });
    return res.end(item.bytes);
  });
  const port = await listen(server);
  t.after(() => new Promise((r) => server.close(r)));
  gw.client = createGatewayClient({ url: `http://127.0.0.1:${port}`, credential: CREDENTIAL });
  return gw;
}

// The fake Scribe: POST /v1/speech-to-text, multipart, header xi-api-key.
async function startFakeScribe(t, { status = 200, text = 'hello from the voice note' } = {}) {
  const scribe = { calls: [] };
  const server = http.createServer(async (req, res) => {
    const raw = await readBody(req);
    scribe.calls.push({ url: req.url, key: req.headers['xi-api-key'], type: req.headers['content-type'], body: raw.toString('latin1') });
    if (status !== 200) {
      res.writeHead(status, { 'Content-Type': 'application/json' });
      return res.end('{"detail":"scribe is down"}');
    }
    res.writeHead(200, { 'Content-Type': 'application/json' });
    return res.end(JSON.stringify({ text, words: [] }));
  });
  const port = await listen(server);
  t.after(() => new Promise((r) => server.close(r)));
  scribe.url = `http://127.0.0.1:${port}/v1/speech-to-text`;
  return scribe;
}

function tempStore(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wa-media-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return path.join(dir, 'whatsapp-media', 'inbound');
}

const sha256 = (bytes, enc) => crypto.createHash('sha256').update(bytes).digest(enc);
const JPEG = Buffer.concat([Buffer.from([0xff, 0xd8, 0xff, 0xe0]), Buffer.alloc(96, 7)]);
const OGG = Buffer.concat([Buffer.from('OggS'), Buffer.alloc(60, 3)]);
const PDF = Buffer.concat([Buffer.from('%PDF-1.7\n'), Buffer.alloc(40, 1)]);

test('media saved with the webhook\'s mime and the sha256 checked', async (t) => {
  const gw = await startFakeGateway(t);
  const mediaDir = tempStore(t);
  gw.media.set('m-img', { bytes: JPEG, mime: 'image/jpeg' });
  gw.media.set('m-doc', { bytes: PDF, mime: 'application/pdf' });
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0 });

  // Meta's webhook carries the hash base64-encoded; hex is accepted too.
  const img = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.HBgLMTU1NTEyMzQ1NjcVAgA=', kind: 'image',
    media: { id: 'm-img', mime_type: 'image/jpeg', sha256: sha256(JPEG, 'base64'), caption: 'the shop front' },
  });
  assert.equal(img.ok, true, img.note);
  const expectedPath = path.join(mediaDir, PHONE, 'wamid.HBgLMTU1NTEyMzQ1NjcVAgA_-image.jpg');
  assert.equal(img.path, expectedPath);
  assert.deepEqual(fs.readFileSync(img.path), JPEG);
  assert.equal(fs.statSync(img.path).mode & 0o777, 0o600);
  assert.equal(fs.statSync(path.join(mediaDir, PHONE)).mode & 0o777, 0o700);
  assert.deepEqual(img.attachment, { provider_id: 'm-img', url: pathToFileURL(expectedPath).href, mime_type: 'image/jpeg' });

  const doc = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.DOC1', kind: 'document',
    media: { id: 'm-doc', mime_type: 'application/pdf', sha256: sha256(PDF, 'hex'), filename: '../Q3 report.pdf' },
  });
  assert.equal(doc.ok, true, doc.note);
  assert.equal(doc.path, path.join(mediaDir, PHONE, 'wamid.DOC1-_._Q3 report.pdf'), 'separators and leading dots defused');
  assert.equal(doc.attachment.mime_type, 'application/pdf');

  const block = describeAttachments([img, doc]);
  const lines = block.split('\n');
  assert.equal(lines[0], '[2 WhatsApp files attached]');
  assert.ok(block.includes(`saved to ${img.path}; Read that path to view it`));
  assert.ok(block.includes(`saved to ${doc.path}; Read that path to view it`));
  assert.ok(block.includes('(image/jpeg)'));
  assert.ok(!block.includes('the shop front'), 'the caption is the renderer\'s, never repeated in the block');
  assert.equal(media.describe([img]).split('\n')[0], '[1 WhatsApp file attached]');
});

test('a sha256 mismatch is refused with a note and nothing is saved', async (t) => {
  const gw = await startFakeGateway(t);
  const mediaDir = tempStore(t);
  gw.media.set('m-bad', { bytes: JPEG, mime: 'image/jpeg' });
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0 });
  const r = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.BAD', kind: 'image',
    media: { id: 'm-bad', mime_type: 'image/jpeg', sha256: sha256(Buffer.from('other bytes'), 'base64') },
  });
  assert.equal(r.ok, false);
  assert.match(r.note, /sha256/);
  assert.equal(r.path, undefined);
  assert.equal(fs.existsSync(path.join(mediaDir, PHONE, 'wamid.BAD-image.jpg')), false);
  assert.match(describeAttachments([r]), /sha256/);
});

test('a transcript inline for a voice note with a fake Scribe, cached beside the file', async (t) => {
  const gw = await startFakeGateway(t);
  const scribe = await startFakeScribe(t);
  const mediaDir = tempStore(t);
  gw.media.set('m-voice', { bytes: OGG, mime: 'audio/ogg; codecs=opus' });
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0, apiKey: () => 'k-test', scribeURL: scribe.url });
  const input = {
    conversationId: PHONE, wamid: 'wamid.VOICE1', kind: 'audio',
    media: { id: 'm-voice', mime_type: 'audio/ogg; codecs=opus', sha256: sha256(OGG, 'base64'), voice: true },
  };
  const r = await media.hydrate(input);
  assert.equal(r.ok, true, r.note);
  assert.equal(r.transcript, 'hello from the voice note');
  assert.equal(r.attachment.mime_type, 'audio/ogg; codecs=opus', 'the webhook\'s mime, verbatim');
  assert.ok(r.path.endsWith('wamid.VOICE1-voice.ogg'));
  assert.equal(scribe.calls.length, 1);
  assert.equal(scribe.calls[0].key, 'k-test');
  assert.match(scribe.calls[0].type, /^multipart\/form-data/);
  assert.match(scribe.calls[0].body, /name="model_id"\r\n\r\nscribe_v1/);
  assert.match(scribe.calls[0].body, /name="file"; filename="voice\.ogg"/);

  const block = describeAttachments([r]);
  assert.ok(block.includes('hello from the voice note'));
  assert.ok(block.includes(`saved to ${r.path}; Read that path to view it`));
  assert.equal(fs.statSync(`${r.path}.transcript.txt`).mode & 0o777, 0o600);

  // A re-hydrate reads the cached transcript: no second Scribe call.
  const again = await media.hydrate(input);
  assert.equal(again.transcript, 'hello from the voice note');
  assert.equal(scribe.calls.length, 1);
});

test('the degrade note without a key', async (t) => {
  const gw = await startFakeGateway(t);
  const scribe = await startFakeScribe(t);
  const mediaDir = tempStore(t);
  gw.media.set('m-voice', { bytes: OGG, mime: 'audio/ogg; codecs=opus' });
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0, apiKey: () => '', scribeURL: scribe.url });
  const r = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.VOICE2', kind: 'audio',
    media: { id: 'm-voice', mime_type: 'audio/ogg; codecs=opus', sha256: sha256(OGG, 'base64'), voice: true },
  });
  assert.equal(r.ok, true, 'the voice note is saved without a key');
  assert.equal(r.transcript, undefined);
  assert.equal(scribe.calls.length, 0);
  assert.ok(describeAttachments([r]).includes(`[voice note saved to ${r.path}; not transcribed: no key]`));
});

test('a Scribe failure degrades to the saved-not-transcribed note', async (t) => {
  const gw = await startFakeGateway(t);
  const scribe = await startFakeScribe(t, { status: 500 });
  const mediaDir = tempStore(t);
  gw.media.set('m-voice', { bytes: OGG, mime: 'audio/ogg' });
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0, apiKey: () => 'k-test', scribeURL: scribe.url });
  const r = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.VOICE3', kind: 'audio',
    media: { id: 'm-voice', mime_type: 'audio/ogg', sha256: sha256(OGG, 'hex'), voice: true },
  });
  assert.equal(r.ok, true);
  assert.equal(r.transcript, undefined);
  assert.equal(scribe.calls.length, 1);
  assert.match(describeAttachments([r]), new RegExp(`\\[voice note saved to ${r.path.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}; not transcribed: `));
});

test('a quota breach rejects the save with the in-message note', async (t) => {
  const gw = await startFakeGateway(t);
  const mediaDir = tempStore(t);
  // 60 bytes already in the store; the quota is 120; a 100-byte image breaches it.
  fs.mkdirSync(path.join(mediaDir, 'other'), { recursive: true });
  fs.writeFileSync(path.join(mediaDir, 'other', 'old.bin'), Buffer.alloc(60));
  gw.media.set('m-img', { bytes: JPEG, mime: 'image/jpeg' });
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0, quotaBytes: 120 });
  const r = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.Q1', kind: 'image',
    media: { id: 'm-img', mime_type: 'image/jpeg', sha256: sha256(JPEG, 'base64') },
  });
  assert.equal(r.ok, false);
  assert.match(r.note, /quota/);
  assert.equal(fs.existsSync(path.join(mediaDir, PHONE, 'wamid.Q1-image.jpg')), false);
  const block = describeAttachments([r]);
  assert.equal(block.split('\n')[0], '[1 WhatsApp file attached]');
  assert.match(block, /quota/);
});

test('the free-disk floor and the size cap reject the save with the in-message note', async (t) => {
  const gw = await startFakeGateway(t);
  const mediaDir = tempStore(t);
  gw.media.set('m-img', { bytes: JPEG, mime: 'image/jpeg' });
  const input = {
    conversationId: PHONE, wamid: 'wamid.F1', kind: 'image',
    media: { id: 'm-img', mime_type: 'image/jpeg', sha256: sha256(JPEG, 'base64') },
  };
  const floor = await createMedia({ mediaDir, gateway: gw.client, minFreeBytes: Number.MAX_SAFE_INTEGER }).hydrate(input);
  assert.equal(floor.ok, false);
  assert.match(floor.note, /free disk/);
  const cap = await createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0, maxBytes: 10 }).hydrate(input);
  assert.equal(cap.ok, false);
  assert.match(cap.note, /too large/);
  assert.equal(fs.existsSync(path.join(mediaDir, PHONE, 'wamid.F1-image.jpg')), false);
});

test('a re-hydrate of the same media reuses the file without a second download', async (t) => {
  const gw = await startFakeGateway(t);
  const mediaDir = tempStore(t);
  gw.media.set('m-img', { bytes: JPEG, mime: 'image/jpeg' });
  const input = {
    conversationId: PHONE, wamid: 'wamid.R1', kind: 'image',
    media: { id: 'm-img', mime_type: 'image/jpeg', sha256: sha256(JPEG, 'base64') },
  };
  const first = await createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0 }).hydrate(input);
  assert.equal(first.ok, true, first.note);
  // A restarted adapter (a fresh instance) re-hydrating the redelivered message.
  const second = await createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0 }).hydrate(input);
  assert.equal(second.ok, true);
  assert.equal(second.path, first.path);
  assert.deepEqual(second.attachment, first.attachment);
  assert.deepEqual(gw.gets, ['m-img'], 'one download');
});

test('nothing logged carries a phone number in clear, a wamid, a path, a transcript or the key', async (t) => {
  const gw = await startFakeGateway(t);
  const scribe = await startFakeScribe(t, { text: 'SECRET-TRANSCRIPT' });
  const mediaDir = tempStore(t);
  gw.media.set('m-voice', { bytes: OGG, mime: 'audio/ogg' });
  gw.media.set('m-img', { bytes: JPEG, mime: 'image/jpeg' });
  const lines = [];
  const media = createMedia({
    mediaDir, gateway: gw.client, minFreeBytes: 0, apiKey: () => 'SECRET-KEY', scribeURL: scribe.url,
    log: (line) => lines.push(String(line)),
  });
  await media.hydrate({ conversationId: PHONE, wamid: 'wamid.LOGVOICE', kind: 'audio', media: { id: 'm-voice', mime_type: 'audio/ogg', sha256: sha256(OGG, 'hex'), voice: true } });
  await media.hydrate({ conversationId: PHONE, wamid: 'wamid.LOGBAD', kind: 'image', media: { id: 'm-img', mime_type: 'image/jpeg', sha256: sha256(OGG, 'hex') } });
  await media.hydrate({ conversationId: PHONE, wamid: 'wamid.LOGGONE', kind: 'image', media: { id: 'm-gone', mime_type: 'image/jpeg' } });
  assert.ok(lines.length >= 3);
  for (const line of lines) {
    for (const secret of [PHONE, 'wamid.LOG', mediaDir, 'SECRET-TRANSCRIPT', 'SECRET-KEY', CREDENTIAL]) {
      assert.ok(!line.includes(secret), `logged ${secret}: ${line}`);
    }
  }
  assert.ok(lines.some((l) => l.includes('…4567')));
});

test('a gateway failure never rejects: ok false with a one-line note', async (t) => {
  const gw = await startFakeGateway(t);
  const mediaDir = tempStore(t);
  const media = createMedia({ mediaDir, gateway: gw.client, minFreeBytes: 0 });
  const missing = await media.hydrate({
    conversationId: PHONE, wamid: 'wamid.N1', kind: 'video',
    media: { id: 'm-gone', mime_type: 'video/mp4', sha256: sha256(JPEG, 'base64') },
  });
  assert.equal(missing.ok, false);
  assert.match(missing.note, /^video not saved: /);
  assert.ok(!missing.note.includes('\n'));

  const dead = createMedia({ mediaDir, gateway: createGatewayClient({ url: 'http://127.0.0.1:9', credential: CREDENTIAL }), minFreeBytes: 0 });
  const unreachable = await dead.hydrate({
    conversationId: PHONE, wamid: 'wamid.N2', kind: 'image', media: { id: 'm-img', mime_type: 'image/jpeg' },
  });
  assert.equal(unreachable.ok, false);
  const bad = await media.hydrate({ conversationId: PHONE, wamid: 'wamid.N3', kind: 'image', media: null });
  assert.equal(bad.ok, false);
});
