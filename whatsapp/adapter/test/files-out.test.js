import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import test from 'node:test';
import { createPublisher, PROGRESS_FILE } from '../src/outbound.js';
import { createGatewayClient } from '../src/gateway.js';
import { prepareFile, outboundRoots, sniffMime, readAllowedFile, descriptorPath } from '../src/outbound-file.js';
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
test('a parent-directory swap cannot authorize the descriptor of a secret file', async (t) => {
  const { r, publish } = await direct(t);
  const root = fs.realpathSync(r.dir);
  const folder = path.join(root, 'out'), parked = path.join(root, 'parked');
  const file = write(root, 'out/report.txt', Buffer.from('ordinary report'));
  const secret = write(root, '.gc/keys/report.txt', Buffer.from('private credential contents'));
  const stat = fs.statSync, open = fs.openSync, read = fs.readSync;
  let swapped = false, targetFd, reads = 0;
  const swap = () => { if (!swapped) { fs.renameSync(folder, parked); fs.symlinkSync(path.dirname(secret), folder); swapped = true; } };
  const restore = () => { if (swapped) { fs.unlinkSync(folder); fs.renameSync(parked, folder); swapped = false; } };
  t.after(restore);
  t.mock.method(fs, 'statSync', (p, ...args) => {
    if (p !== file) return stat(p, ...args);
    swap(); const result = stat(p, ...args);
    if (targetFd !== undefined) restore(); // old final pathname check sees the innocent directory again
    return result;
  });
  t.mock.method(fs, 'openSync', (p, ...args) => {
    const fd = open(p, ...args);
    if (p === file) { targetFd = fd; restore(); }
    return fd;
  });
  t.mock.method(fs, 'readSync', (fd, ...args) => { if (fd === targetFd) reads++; return read(fd, ...args); });
  const answer = await publish(request(file));
  assert.equal(answer.status, 400);
  assert.match(answer.json.error, /opened descriptor/);
  assert.equal(reads, 0, 'no secret byte is read, let alone uploaded');
  assert.equal(r.gw.requests.length, 0);
});
test('hard links and unavailable descriptor inspection fail closed', async (t) => {
  const { r } = await direct(t), file = write(r.dir, 'photo.jpg');
  fs.linkSync(file, path.join(r.dir, 'alias.jpg'));
  assert.throws(() => readAllowedFile(file, { roots: [fs.realpathSync(r.dir)] }), /hard-linked/);
  assert.throws(() => descriptorPath(1073741824), /inspection unavailable/);
});
test('a file changed during the read is refused before upload', async (t) => {
  const { r, publish } = await direct(t), file = write(r.dir, 'photo.jpg');
  const read = fs.readSync;
  t.mock.method(fs, 'readSync', (...args) => {
    const count = read(...args);
    fs.appendFileSync(file, 'changed during read');
    return count;
  });
  const answer = await publish(request(file));
  assert.equal(answer.status, 400);
  assert.match(answer.json.error, /changed while reading/);
  assert.equal(r.gw.requests.length, 0);
});
test('the after-read descriptor check catches a moved parent despite stable pathname checks', async (t) => {
  const { r, publish } = await direct(t), root = fs.realpathSync(r.dir);
  const folder = path.join(root, 'out'), moved = path.join(root, '.gc/keys');
  const file = write(root, 'out/photo.jpg');
  fs.mkdirSync(path.dirname(moved));
  const read = fs.readSync, realpath = fs.realpathSync;
  let swapped = false;
  t.mock.method(fs, 'readSync', (...args) => {
    const count = read(...args);
    if (!swapped) { fs.renameSync(folder, moved); fs.symlinkSync(moved, folder); swapped = true; }
    return count;
  });
  // A concurrent swap can make each pathname lookup see the old name. The
  // descriptor must still be checked independently after reading the bytes.
  t.mock.method(fs, 'realpathSync', (p, ...args) => p === file && swapped ? file : realpath(p, ...args));
  const answer = await publish(request(file));
  assert.equal(answer.status, 400);
  assert.match(answer.json.error, /descriptor moved while reading/);
  assert.equal(r.gw.requests.length, 0);
});
test('the update endpoint refuses files and captions even with otherwise valid template text', async (t) => {
  const { r, publisher } = await direct(t);
  r.clock.advance(25 * 3600000);
  for (const extra of [{ file: write(r.dir, 'photo.jpg') }, { caption: 'photo' }, { caption_file: '/caption.txt' }]) {
    const answer = await publisher.handlePublishUpdate(JSON.stringify({ to: OWNER, text: 'An update',
      idempotency_key: 'file-update', ...extra }));
    assert.equal(answer.status, 400);
    assert.equal(answer.json.error, 'files cannot use the update template');
  }
  assert.equal(r.gw.requests.length, 0);
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
  assert.match(closed.json.error, /same --idempotency-key file-key/);
  assert.equal(r.gw.uploadCalls.length, 0);
  r.gw.lastInbound.set(OWNER, r.clock.now());
  r.gw.uploadFailures.push(500);
  assert.equal((await publish(body)).json.delivered, false);
  assert.equal(r.gw.sends.length, 0);
  assert.equal((await publish(body)).json.delivered, true);
  assert.equal(r.gw.uploadCalls.length, 1);
  assert.equal(r.gw.sends.length, 1);
});
for (const phase of ['before upload', 'after upload']) {
  test(`an unwritable journal ${phase} pauses the file send until the same key can be saved`, async (t) => {
    const { r } = await direct(t), body = request(write(r.dir, 'photo.jpg'));
    const real = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
    let failWrites = true;
    const gateway = { ...real, async upload(...args) {
      const answer = await real.upload(...args);
      if (failWrites && phase === 'after upload') fs.chmodSync(r.dir, 0o500);
      return answer;
    } };
    const make = () => createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY,
      clock: r.clock.now, gateway, gc: { recordOutbound: async () => ({ TranscriptEntry: {} }) } });
    const publisher = make();
    if (phase === 'before upload') fs.chmodSync(r.dir, 0o500);
    try {
      const failed = await publisher.handlePublish(JSON.stringify(body));
      assert.equal(failed.json.failure_kind, 'transient');
      assert.equal(failed.json.metadata.reason, 'outbound_progress_unavailable');
      assert.equal(r.gw.sends.length, 0);
      assert.equal(r.gw.uploadCalls.length, phase === 'before upload' ? 0 : 1);
    } finally { fs.chmodSync(r.dir, 0o700); failWrites = false; }
    const recovered = await make().handlePublish(JSON.stringify(body));
    assert.equal(recovered.json.delivered, true);
    assert.equal((await publisher.handlePublish(JSON.stringify(body))).json.message_id, recovered.json.message_id);
    assert.equal(r.gw.uploadCalls.length, 1);
    assert.equal(r.gw.sends.length, 1);
  });
}
for (const damage of ['removed', 'unreadable']) {
  test(`a ${damage} journal recovers a lost send reply after 25 hours without uploading`, async (t) => {
    const { r } = await direct(t), body = request(write(r.dir, 'photo.jpg'));
    const real = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
    let firstId, loseReply = true;
    const gateway = { ...real, async send(payload) {
      const sent = await real.send(payload);
      if (loseReply) { firstId = sent.message_id; throw Object.assign(new Error('lost reply'), { noAnswer: true }); }
      return sent;
    } };
    const make = () => createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY,
      clock: r.clock.now, gateway, gc: { recordOutbound: async () => ({ TranscriptEntry: {} }) } });
    assert.equal((await make().handlePublish(JSON.stringify(body))).json.failure_kind, 'transient');
    const journal = path.join(r.dir, PROGRESS_FILE);
    if (damage === 'removed') fs.unlinkSync(journal); else fs.writeFileSync(journal, '{unreadable');
    r.clock.advance(25 * 3600000); loseReply = false;
    const before = r.gw.requests.length;
    const recovered = await make().handlePublish(JSON.stringify(body));
    assert.equal(recovered.json.delivered, true);
    assert.equal(recovered.json.message_id, firstId);
    assert.equal(r.gw.uploadCalls.length, 1);
    assert.equal(r.gw.sends.length, 1);
    assert.equal(r.gw.requests.slice(before).filter((q) => q.route === 'POST /v1/uploads').length, 0);
  });
}
test('a recovery probe cannot send when the window reopens between lookup and probe', async (t) => {
  const { r } = await direct(t), body = request(write(r.dir, 'photo.jpg'));
  r.clock.advance(25 * 3600000);
  const real = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
  const gateway = { ...real, async send(payload) {
    r.gw.lastInbound.set(OWNER, r.clock.now());
    return real.send(payload);
  } };
  const publisher = createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY,
    clock: r.clock.now, gateway, gc: { recordOutbound: async () => ({ TranscriptEntry: {} }) } });
  const refused = await publisher.handlePublish(JSON.stringify(body));
  assert.equal(refused.json.failure_kind, 'transient');
  assert.equal(refused.json.metadata.reason, 'window_changed');
  assert.equal(r.gw.uploadCalls.length, 0);
  assert.equal(r.gw.sends.length, 0);
  assert.equal((await publisher.handlePublish(JSON.stringify(body))).json.delivered, true);
  assert.equal(r.gw.sends.length, 1);
});
test('a recovery probe with an unknown send outcome never claims nothing was sent', async (t) => {
  const { r } = await direct(t), body = request(write(r.dir, 'photo.jpg'));
  r.clock.advance(25 * 3600000);
  const real = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
  const gateway = { ...real, async send() {
    throw Object.assign(new Error('lost send outcome'), { status: 409, json: { error: 'outcome_unknown' } });
  } };
  const publisher = createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY, clock: r.clock.now, gateway });
  const refused = await publisher.handlePublish(JSON.stringify(body));
  assert.equal(refused.json.metadata.reason, 'outcome_unknown');
  assert.match(refused.json.error, /check the chat/);
  assert.doesNotMatch(refused.json.error, /Nothing was sent|kept the upload/);
  assert.equal(r.gw.uploadCalls.length, 0);
});
test('signature parsing distinguishes audio/video tracks and rejects non-Opus Ogg', () => {
  assert.equal(sniffMime(mp4()), 'video/mp4');
  assert.equal(sniffMime(mp4('soun')), 'audio/mp4');
  assert.equal(sniffMime(mp4('text')), null);
  const ogg = Buffer.alloc(47); ogg.write('OggS'); ogg[26] = 1; ogg[27] = 19; ogg.write('OpusHead', 28); ogg[37] = 1;
  assert.equal(sniffMime(ogg), 'audio/ogg');
  ogg[37] = 2; assert.equal(sniffMime(ogg), null);
});

function legacyOffice(stream) {
  // A CFB v3 header, two linked directory sectors, one FAT; stream is in
  // directory entry 4, beyond the first 512-byte sector.
  const b = Buffer.alloc(2048);
  Buffer.from('d0cf11e0a1b11ae1', 'hex').copy(b);
  b.writeUInt16LE(0x3e, 24); b.writeUInt16LE(3, 26); b.writeUInt16LE(0xfffe, 28);
  b.writeUInt16LE(9, 30); b.writeUInt16LE(6, 32); b.writeUInt32LE(1, 44);
  b.writeUInt32LE(0, 48); b.writeUInt32LE(4096, 56); b.writeUInt32LE(0xfffffffe, 60); b.writeUInt32LE(0xfffffffe, 68);
  b.fill(0xff, 76, 512); b.writeUInt32LE(1, 76);
  const entry = (offset, name, kind) => {
    const text = Buffer.from(`${name}\0`, 'utf16le'); text.copy(b, offset); b.writeUInt16LE(text.length, offset + 64); b[offset + 66] = kind; b[offset + 67] = 1;
    b.writeUInt32LE(0xffffffff, offset + 68); b.writeUInt32LE(0xffffffff, offset + 72); b.writeUInt32LE(0xffffffff, offset + 76);
    b.writeUInt32LE(0xfffffffe, offset + 116);
  };
  entry(512, 'Root Entry', 5); b.writeUInt32LE(4, 512 + 76);
  entry(1536, stream, 2);
  b.fill(0xff, 1024, 1536); b.writeUInt32LE(2, 1024); b.writeUInt32LE(0xfffffffd, 1028); b.writeUInt32LE(0xfffffffe, 1032);
  return b;
}
for (const [name, mime] of [['WordDocument', 'application/msword'], ['Workbook', 'application/vnd.ms-excel'], ['PowerPoint Document', 'application/vnd.ms-powerpoint']]) {
  test(`legacy Office follows later directory sectors for ${name}`, () => {
    const bytes = legacyOffice(name);
    assert.equal(sniffMime(bytes), mime);
    bytes.writeUInt32LE(0, 1032); assert.equal(sniffMime(bytes), null, 'directory cycles are refused');
    bytes.writeUInt32LE(9999, 1032); assert.equal(sniffMime(bytes), null, 'out-of-file chains are refused');
  });
}
test('legacy Office identifies root streams and ignores embedded Office storage children', () => {
  const bytes = legacyOffice('WordDocument');
  // Root's child tree contains the Word stream (entry 4) and an embedded
  // storage (entry 1). The Workbook (entry 2) is that storage's child only.
  const entry = (id, name, type, child = 0xffffffff) => {
    const p = 512 + id * 128, text = Buffer.from(`${name}\0`, 'utf16le');
    text.copy(bytes, p); bytes.writeUInt16LE(text.length, p + 64); bytes[p + 66] = type;
    bytes.writeUInt32LE(0xffffffff, p + 68); bytes.writeUInt32LE(0xffffffff, p + 72); bytes.writeUInt32LE(child, p + 76);
  };
  entry(1, 'EmbeddedObject', 1, 2); entry(2, 'Workbook', 2);
  bytes.writeUInt32LE(1, 1536 + 72);
  assert.equal(sniffMime(bytes), 'application/msword');
  bytes.writeUInt32LE(4, 512 + 128 + 72);
  assert.equal(sniffMime(bytes), null, 'cycles in root sibling trees are refused');
});
test('file receipts survive age and count pruning to recover after the window closes', async (t) => {
  const { r, publish } = await direct(t), body = request(write(r.dir, 'photo.jpg'), 'retained-file');
  const first = await publish(body);
  r.clock.advance(8 * 86400000);
  const journalPath = path.join(r.dir, PROGRESS_FILE), saved = JSON.parse(fs.readFileSync(journalPath));
  for (let i = 0; i < 1025; i++) saved.entries[`text-${i}`] = { endpoint: 'text', updated_at: r.clock.now() };
  fs.writeFileSync(journalPath, JSON.stringify(saved));
  const gateway = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
  const make = () => createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY, clock: r.clock.now, gateway,
    gc: { recordOutbound: async () => ({ TranscriptEntry: {} }) } });
  const other = '5511900000002'; r.gw.bound.add(other); r.gw.lastInbound.set(other, r.clock.now());
  const text = { session_id: 'session-test', conversation: { conversation_id: other }, text: 'Trigger pruning', idempotency_key: 'pruner' };
  assert.equal((await make().handlePublish(JSON.stringify(text))).json.delivered, true);
  const pruned = JSON.parse(fs.readFileSync(journalPath));
  assert.ok(pruned.entries['retained-file']);
  assert.equal(Object.values(pruned.entries).filter((e) => e.endpoint !== 'file').length, 1024);
  const recovered = await make().handlePublish(JSON.stringify(body));
  assert.equal(recovered.json.message_id, first.json.message_id);
  assert.equal(recovered.json.delivered, true);
  assert.equal(r.gw.uploadCalls.length, 1);
  assert.equal(r.gw.sends.length, 2); // the file and the other owner's text only
});
test('a day-29 send with a lost reply survives day-31 pruning and a closed window', async (t) => {
  const { r } = await direct(t), body = request(write(r.dir, 'photo.jpg'), 'delayed-file');
  const realGateway = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
  let phase = 'before-send', delivered;
  const gateway = { ...realGateway, async send(payload) {
    if (phase === 'before-send') throw new Error('stop after journal, before send');
    const sent = await realGateway.send(payload);
    if (phase === 'after-send') { delivered = sent; throw new Error('lost send reply'); }
    return sent;
  } };
  const make = () => createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY, clock: r.clock.now, gateway,
    gc: { recordOutbound: async () => ({ TranscriptEntry: {} }) } });
  assert.equal((await make().handlePublish(JSON.stringify(body))).json.delivered, false);
  assert.equal(r.gw.uploadCalls.length, 1); assert.equal(r.gw.sends.length, 0);
  r.clock.advance(29 * 86400000); r.gw.lastInbound.set(OWNER, r.clock.now()); phase = 'after-send';
  assert.equal((await make().handlePublish(JSON.stringify(body))).json.delivered, false);
  assert.equal(r.gw.sends.length, 1);
  r.clock.advance(2 * 86400000); phase = 'normal';
  const other = '5511900000002'; r.gw.bound.add(other); r.gw.lastInbound.set(other, r.clock.now());
  await make().handlePublish(JSON.stringify({ session_id: 'session-test', conversation: { conversation_id: other },
    text: 'Trigger pruning', idempotency_key: 'delayed-pruner' }));
  const recovered = await make().handlePublish(JSON.stringify(body));
  assert.equal(recovered.json.delivered, true);
  assert.equal(recovered.json.message_id, delivered.message_id);
  assert.equal(r.gw.uploadCalls.length, 1); assert.equal(r.gw.sends.length, 2);
});
test('an expired journal handle is permanent, but a completed send is still recovered', async (t) => {
  const { r, publish } = await direct(t), file = write(r.dir, 'photo.jpg'), body = request(file, 'expired-key');
  await publish(body);
  const fileName = path.join(r.dir, PROGRESS_FILE), saved = JSON.parse(fs.readFileSync(fileName));
  const entry = saved.entries['expired-key'];
  // The crash boundary after send but before recording its receipt.
  delete entry.receipt; delete entry.recording; entry.delivered = 0; entry.message_ids = [];
  r.clock.advance(31 * 86400000); r.gw.lastInbound.set(OWNER, r.clock.now());
  fs.writeFileSync(fileName, JSON.stringify(saved));
  const gateway = createGatewayClient({ url: r.gw.url, credential: CREDENTIAL });
  const make = () => createPublisher({ stateDir: r.dir, cityDir: r.dir, city: CITY, clock: r.clock.now, gateway, gc: { recordOutbound: async () => ({ TranscriptEntry: {} }) } });
  assert.equal((await make().handlePublish(JSON.stringify(body))).json.delivered, true, 'stored send wins over expired grant');
  // The earlier crash boundary: uploaded, but no send reached the gateway.
  r.gw.sendAnswers.clear(); fs.writeFileSync(fileName, JSON.stringify(saved));
  const failed = await make().handlePublish(JSON.stringify(body));
  assert.equal(failed.json.failure_kind, 'permanent');
  assert.equal(failed.json.metadata.reason, 'media_expired');
  assert.match(failed.json.error, /fresh key/);
  assert.equal(r.gw.uploadCalls.length, 1);
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
    if (point === 'after-media-send') {
      fs.unlinkSync(path.join(r.dir, '.gc', 'whatsapp', PROGRESS_FILE));
      r.clock.advance(25 * 3600000);
    }
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
