import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import test from 'node:test';
import { createPublisher, PROGRESS_FILE } from '../src/outbound.js';
import { createGatewayClient } from '../src/gateway.js';
import { prepareFile, outboundRoots, sniffMime } from '../src/outbound-file.js';
import { rig, adapterRequest, waitFor, CITY, CREDENTIAL } from './fakes.js';

const OWNER = '5511900000001';
const box = (name, contents) => {
  const b = Buffer.alloc(8 + contents.length); b.writeUInt32BE(b.length); b.write(name, 4); contents.copy(b, 8); return b;
};
const handler = (type) => { const b = Buffer.alloc(24); b.write(type, 8); return box('hdlr', b); };
const mp4 = (type = 'vide') => Buffer.concat([box('ftyp', Buffer.from('isom\0\0\0\0isom')), box('moov', box('trak', box('mdia', handler(type)))), box('mdat', Buffer.from('fake encoded frames'))]);
const fixtures = [
  { kind: 'image', name: 'photo.jpg', bytes: Buffer.from('ffd8ffe000104a46494600010100000100010000ffd9', 'hex') },
  { kind: 'document', name: 'report.pdf', bytes: Buffer.from('%PDF-1.7\nprivate report bytes\n%%EOF') },
  { kind: 'audio', name: 'voice.amr', bytes: Buffer.from('#!AMR\nprivate audio bytes') },
  { kind: 'video', name: 'clip.mp4', bytes: mp4() },
];
const write = (dir, name, bytes = fixtures[0].bytes) => {
  const file = path.join(dir, name); fs.mkdirSync(path.dirname(file), { recursive: true }); fs.writeFileSync(file, bytes); return file;
};
const request = (file, key = 'file-key', extra = {}) => ({ session_id: 'session-test',
  conversation: { conversation_id: OWNER }, file, idempotency_key: key, metadata: { origin: 'cli' }, ...extra });

async function direct(t, options = {}) {
  const r = await rig(); t.after(() => r.close());
  r.gw.bound.add(OWNER); r.gw.lastInbound.set(OWNER, r.clock.now());
  const gateway = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
  const transcripts = [];
  let publisher;
  const gc = { async recordOutbound(body) {
    const callback = await publisher.handlePublish(JSON.stringify(body));
    assert.equal(callback.json.delivered, true, 'the gc callback resolves the stored media receipt');
    transcripts.push(body);
    return { TranscriptEntry: { id: transcripts.length } };
  } };
  publisher = createPublisher({ stateDir: r.dir, cityDir: r.dir, mediaDir: path.join(r.dir, 'incoming'),
    city: CITY, clock: r.clock.now, gateway, gc, ...options });
  return { r, publisher, transcripts, publish: (body) => publisher.handlePublish(JSON.stringify(body)) };
}

for (const f of fixtures) {
  test(`${f.kind}: upload, journal, send, transcript and retry`, async (t) => {
    const { r, publish, transcripts } = await direct(t);
    const file = write(r.dir, f.name, f.bytes);
    const body = request(file, `key-${f.kind}`, f.kind !== 'audio' ? { caption: 'Requested file' } : {});
    const first = await publish(body);
    assert.equal(first.json.delivered, true, JSON.stringify(first));
    assert.equal(first.json.transcript_recorded, true);
    assert.equal((await publish(body)).json.message_id, first.json.message_id);
    assert.equal(r.gw.uploadCalls.length, 1);
    assert.deepEqual(r.gw.uploadCalls[0].bytes, f.bytes);
    assert.equal(r.gw.sends.length, 1);
    assert.equal(r.gw.sends[0].type, f.kind);
    assert.equal(transcripts.length, 1);
    const digest = crypto.createHash('sha256').update(f.bytes).digest('hex');
    assert.ok(transcripts[0].text.includes(f.name));
    assert.ok(transcripts[0].text.includes(`${f.bytes.length} bytes`));
    assert.ok(transcripts[0].text.includes(digest));
    assert.ok(!transcripts[0].text.includes(f.bytes.toString('base64')));
    const journal = JSON.parse(fs.readFileSync(path.join(r.dir, PROGRESS_FILE)));
    const entry = journal.entries[body.idempotency_key];
    assert.equal(entry.media_id, 'media.out.1');
    assert.equal(entry.file.sha256, digest);
    assert.equal(entry.file.bytes, undefined);
    assert.equal(entry.receipt.delivered, true);
  });
}

for (const forbidden of ['.gc/services/photo.jpg', '.gc/keys/photo.jpg', 'secrets/photo.jpg',
  'nested/secrets/photo.jpg', '.ssh/photo.jpg', '.config/photo.jpg', 'key.env', 'key.pem', 'id_photo.jpg']) {
  test(`refuses denied path ${forbidden} without contacting gateway`, async (t) => {
    const { r, publish } = await direct(t);
    const result = await publish(request(write(r.dir, forbidden)));
    assert.equal(result.status, 400);
    assert.match(result.json.error, /denied|secret\/key/);
    assert.equal(r.gw.requests.length, 0);
  });
}
test('symlink escapes, denied aliases and denied resolved targets fail closed', async (t) => {
  const { r, publish } = await direct(t);
  const outside = `${r.dir}-outside`; fs.mkdirSync(outside); t.after(() => fs.rmSync(outside, { recursive: true, force: true }));
  fs.symlinkSync(write(outside, 'escape.jpg'), path.join(r.dir, 'escape.jpg'));
  fs.symlinkSync(write(r.dir, 'secrets/key.jpg'), path.join(r.dir, 'alias.jpg'));
  fs.symlinkSync(write(r.dir, 'safe.jpg'), path.join(r.dir, 'id_alias.jpg'));
  for (const name of ['escape.jpg', 'alias.jpg', 'id_alias.jpg']) assert.equal((await publish(request(path.join(r.dir, name), name))).status, 400);
  assert.equal(r.gw.requests.length, 0);
});
test('custom roots never exempt denied paths; invalid and empty configuration fails closed', async (t) => {
  const { r } = await direct(t);
  for (const configured of ['', '.', `${r.dir}${path.delimiter}`, path.join(r.dir, 'absent')]) {
    assert.throws(() => outboundRoots({ cityDir: r.dir, configured }), /OUTBOUND_ROOTS/);
  }
  const secret = write(r.dir, 'secrets/photo.jpg');
  const roots = outboundRoots({ configured: path.dirname(secret) });
  assert.throws(() => prepareFile(secret, { roots }), /denied/);
  assert.throws(() => prepareFile(r.dir, { roots: [r.dir] }), /outside|regular/);
});
test('checks signatures against extensions, actual size, captions and changed files before upload', async (t) => {
  const { r, publish } = await direct(t);
  assert.equal((await publish(request(write(r.dir, 'renamed.pdf')))).status, 400);
  assert.equal((await publish(request(write(r.dir, 'garbage.jpg', Buffer.from('not a jpeg'))))).status, 400);
  const large = write(r.dir, 'large.jpg'); fs.truncateSync(large, 5000001);
  assert.match((await publish(request(large))).json.error, /cap/);
  assert.equal((await publish(request(write(r.dir, 'audio.amr', fixtures[2].bytes), 'audio', { caption: 'not allowed' }))).status, 400);
  assert.equal((await publish(request(write(r.dir, 'long.jpg'), 'long', { caption: 'x'.repeat(1025) }))).status, 400);
  assert.equal(r.gw.requests.length, 0);
  const file = write(r.dir, 'valid.jpg');
  assert.equal((await publish(request(file))).json.delivered, true);
  fs.appendFileSync(file, 'changed');
  assert.equal((await publish(request(file))).status, 409);
  assert.equal(r.gw.uploadCalls.length, 1);
});
test('caption files use the same allow-list and their text enters the transcript', async (t) => {
  const { r, publish, transcripts } = await direct(t);
  const file = write(r.dir, 'photo.jpg');
  const caption = write(r.dir, 'caption.txt', Buffer.from('The report you asked for'));
  assert.equal((await publish(request(file, 'caption', { caption_file: caption }))).json.delivered, true);
  assert.match(transcripts[0].text, /The report you asked for/);
  const denied = write(r.dir, '.gc/keys/caption.txt', Buffer.from('secret'));
  assert.equal((await publish(request(file, 'secret', { caption_file: denied }))).status, 400);
  assert.equal(r.gw.uploadCalls.length, 1);
});
test('closed window uploads nothing, failed upload sends nothing, retry recovers', async (t) => {
  const { r, publish } = await direct(t), body = request(write(r.dir, 'photo.jpg'));
  r.clock.advance(86400000);
  const closed = await publish(body);
  assert.equal(closed.json.metadata.reason, 'window_closed');
  assert.match(closed.json.error, /cannot use the update template/);
  assert.equal(r.gw.uploadCalls.length, 0);
  r.gw.lastInbound.set(OWNER, r.clock.now());
  r.gw.uploadFailures.push(500);
  assert.equal((await publish(body)).json.delivered, false);
  assert.equal(r.gw.sends.length, 0);
  assert.equal((await publish(body)).json.delivered, true);
  assert.equal(r.gw.uploadCalls.length, 1);
  assert.equal(r.gw.sends.length, 1);
});
test('signature parsing distinguishes audio/video tracks and rejects non-Opus Ogg', () => {
  assert.equal(sniffMime(mp4()), 'video/mp4');
  assert.equal(sniffMime(mp4('soun')), 'audio/mp4');
  assert.equal(sniffMime(mp4('text')), null);
  const ogg = Buffer.alloc(47); ogg.write('OggS'); ogg[26] = 1; ogg[27] = 19; ogg.write('OpusHead', 28); ogg[37] = 1;
  assert.equal(sniffMime(ogg), 'audio/ogg');
  ogg[37] = 2; assert.equal(sniffMime(ogg), null);
});

async function ready(r) {
  await waitFor(async () => { try { return (await adapterRequest(r.socket, 'GET', '/healthz')).status === 200; } catch { return false; } }, { what: 'file adapter ready' });
}
for (const f of fixtures) for (const point of ['before-media-upload', 'after-media-upload', 'after-media-journal', 'after-media-send', 'after-media-receipt']) {
  test(`${f.kind} survives SIGKILL at ${point} with one upload and one send`, { timeout: 30000 }, async (t) => {
    const r = await rig(); t.after(() => r.close());
    r.gw.bound.add(OWNER); r.gw.lastInbound.set(OWNER, r.clock.now()); r.gc.recordOutbounds = true;
    const file = write(r.dir, f.name, f.bytes), body = request(file, `crash-${point}`);
    const first = r.start({ WHATSAPP_TEST_KILL_AT: point });
    await ready(r);
    const pending = adapterRequest(r.socket, 'POST', '/publish', body).catch(() => null);
    const exit = await first.exited;
    assert.equal(exit.signal, 'SIGKILL'); await pending;
    await r.restart(); await ready(r);
    const recovered = await adapterRequest(r.socket, 'POST', '/publish', body);
    assert.equal(recovered.json.delivered, true, JSON.stringify(recovered));
    assert.equal((await adapterRequest(r.socket, 'POST', '/publish', body)).json.message_id, recovered.json.message_id);
    assert.equal(r.gw.uploadCalls.length, 1);
    assert.deepEqual(r.gw.uploadCalls[0].bytes, f.bytes);
    assert.equal(r.gw.sends.length, 1);
    assert.equal(r.gc.outbound.length, 1);
    assert.ok(r.gc.outbound[0].text.includes(f.name));
  });
}
