// outbound.test.js — integration tests for the publish pipeline
// (src/outbound.js), with a fake WS client (sendMessage / uploadMedia /
// sendMediaMessage captured), a fake gc (postOutbound capture), and real
// files on disk for the media admission path. Covers the jg-d0xr outbound
// media surface: upload → media_id → aibot_send_msg, size/format
// rejection, idempotent resume across failed stages, the shared
// idempotency map that dedups gc's transcript-recording callback, the
// per-chat send chain across BOTH endpoints, dm/room kind resolution, and
// the relocated text-publish behavior (chunking, retry dedup).

import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import {
  chunkText,
  createConversationKindStore,
  createOutboundPublisher,
  outboundChunkBytes,
} from '../src/outbound.js';

// --- fixtures ------------------------------------------------------------

const pngBytes = Buffer.concat([
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  Buffer.from('IHDR fake image payload'),
]);
const jpgBytes = Buffer.concat([Buffer.from([0xff, 0xd8, 0xff, 0xe0]), Buffer.alloc(16, 1)]);
const gifBytes = Buffer.concat([Buffer.from('GIF89a'), Buffer.alloc(16, 2)]);
const mp4Bytes = Buffer.concat([
  Buffer.from([0x00, 0x00, 0x00, 0x18]),
  Buffer.from('ftypisom'),
  Buffer.alloc(16, 3),
]);
const movBytes = Buffer.concat([
  Buffer.from([0x00, 0x00, 0x00, 0x14]),
  Buffer.from('ftypqt  '),
  Buffer.alloc(16, 4),
]);

function tmpDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wecom-outbound-test-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function writeFixture(dir, name, bytes) {
  const p = path.join(dir, name);
  fs.writeFileSync(p, bytes);
  return p;
}

// fakeRes captures what a handler writes; json() parses the body.
function fakeRes() {
  return {
    statusCode: null,
    body: '',
    headersSent: false,
    writeHead(code) { this.statusCode = code; this.headersSent = true; return this; },
    end(chunk) { if (chunk !== undefined) this.body += chunk; return this; },
    json() { return JSON.parse(this.body); },
  };
}

// makePublisher builds a publisher over a fake WS client. calls[] records
// every provider interaction in order; outboundPosts[] records transcript
// recordings the publisher attempted against gc.
function makePublisher(overrides = {}) {
  const calls = [];
  const outboundPosts = [];
  const cfg = {
    cityName: 'jadegate',
    provider: 'wecom',
    botId: 'BOT_1',
    gcAPIBase: 'http://gc.test:9443',
    imageMaxBytes: 10 * 1024 * 1024,
    videoMaxBytes: 10 * 1024 * 1024,
    uploadTimeoutMs: 5000,
    ...overrides.cfg,
  };
  let mediaSeq = 0;
  const publisher = createOutboundPublisher({
    cfg,
    log: () => {},
    sendMessage: overrides.sendMessage
      ?? (async (chatid, body) => {
        calls.push({ op: 'sendMessage', chatid, body });
        return { headers: { req_id: `TEXT_${calls.length}` } };
      }),
    uploadMedia: overrides.uploadMedia
      ?? (async (buffer, options) => {
        calls.push({ op: 'uploadMedia', bytes: buffer.length, options });
        mediaSeq += 1;
        return { type: options.type, media_id: `MEDIA_${mediaSeq}`, created_at: '0' };
      }),
    sendMediaMessage: overrides.sendMediaMessage
      ?? (async (chatid, type, mediaId) => {
        calls.push({ op: 'sendMediaMessage', chatid, type, mediaId });
        return { headers: { req_id: `MSG_${mediaId}` } };
      }),
    postOutbound: overrides.postOutbound
      ?? (async (target, body) => {
        outboundPosts.push({ target, body });
        // Shape gc actually returns: an untagged Go OutboundResult.
        return { Receipt: { Delivered: true }, TranscriptEntry: { ID: 'tr-1' } };
      }),
    kindStore: overrides.kindStore ?? null,
    publishStatesCap: overrides.publishStatesCap ?? 512,
  });
  return { publisher, calls, outboundPosts, cfg };
}

async function publishMedia(publisher, body) {
  const res = fakeRes();
  await publisher.handlePublishMedia({}, res, JSON.stringify(body));
  return res;
}

async function publishText(publisher, body) {
  const res = fakeRes();
  await publisher.handlePublish({}, res, JSON.stringify(body));
  return res;
}

// --- image happy path ------------------------------------------------------

test('an image publish uploads, sends, and answers a delivered receipt', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls, outboundPosts } = makePublisher();

  const res = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });

  assert.equal(res.statusCode, 200);
  const out = res.json();
  assert.equal(out.delivered, true);
  assert.equal(out.message_id, 'MSG_MEDIA_1');
  assert.equal(out.media_id, 'MEDIA_1');
  assert.equal(out.transcript_recorded, true);
  assert.deepEqual(calls.map((c) => c.op), ['uploadMedia', 'sendMediaMessage']);
  assert.equal(calls[0].bytes, pngBytes.length);
  assert.deepEqual(calls[0].options, { type: 'image', filename: 'photo.png' });
  assert.equal(calls[1].type, 'image');
  assert.equal(calls[1].mediaId, 'MEDIA_1');
  assert.equal(calls[1].chatid, 'zhang_san');
  assert.equal(outboundPosts.length, 1);
});

test('the transcript recording POST carries the full conversation ref and attachment metadata', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, '截图.png', pngBytes);
  const { publisher, outboundPosts } = makePublisher();

  await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    text: '这是截图',
  });

  assert.equal(outboundPosts.length, 1);
  const { target, body } = outboundPosts[0];
  assert.equal(target, 'http://gc.test:9443/v0/city/jadegate/extmsg/outbound');
  assert.equal(body.session_id, 'sess-mayor');
  assert.deepEqual(body.conversation, {
    scope_id: 'jadegate',
    provider: 'wecom',
    account_id: 'BOT_1',
    conversation_id: 'zhang_san',
    kind: 'dm',
  });
  assert.ok(body.idempotency_key.length > 0);
  assert.match(body.text, /^\[image sent\] 截图\.png \(image\/png, \d+ bytes, sha256 [0-9a-f]{12}\) — source: /);
  assert.ok(body.text.endsWith('\n这是截图'));
});

test("gc's recording callback to /publish answers from the settled receipt without re-sending", async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls, outboundPosts } = makePublisher();

  const res = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  const key = outboundPosts[0].body.idempotency_key;

  // Simulate gc's HTTPAdapter callback: POST /publish with the transcript
  // text under the same idempotency key.
  const cbRes = await publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: outboundPosts[0].body.text,
    idempotency_key: key,
  });
  assert.equal(cbRes.statusCode, 200);
  assert.equal(cbRes.json().delivered, true);
  assert.equal(cbRes.json().message_id, res.json().message_id);
  // No markdown message was ever sent — the media send is the only traffic.
  assert.deepEqual(calls.map((c) => c.op), ['uploadMedia', 'sendMediaMessage']);
});

test('a caption goes out as a follow-up markdown message on the same chat', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'demo.mp4', mp4Bytes);
  const { publisher, calls } = makePublisher();

  const res = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'video',
    text: '看看这个视频',
  });

  assert.equal(res.statusCode, 200);
  const out = res.json();
  assert.equal(out.delivered, true);
  assert.ok(out.caption_message_id);
  assert.deepEqual(calls.map((c) => c.op), ['uploadMedia', 'sendMediaMessage', 'sendMessage']);
  assert.equal(calls[1].type, 'video');
  assert.deepEqual(calls[2].body, { msgtype: 'markdown', markdown: { content: '看看这个视频' } });
});

test('a filename without an extension gains the detected one for the upload', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo', pngBytes);
  const { publisher, calls } = makePublisher();

  await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(calls[0].options.filename, 'photo.png');
});

test('a .jpeg filename is not double-suffixed for jpg content', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'pic.jpeg', jpgBytes);
  const { publisher, calls } = makePublisher();

  await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(calls[0].options.filename, 'pic.jpeg');
});

// --- admission rejections ----------------------------------------------------

test('an oversized image is rejected with the cap, the size, and the override env', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'big.png', pngBytes);
  const { publisher, calls } = makePublisher({ cfg: { imageMaxBytes: 16 } });

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 400);
  const out = res.json();
  assert.equal(out.delivered, false);
  assert.match(out.error, /too large: \d+ bytes > the 16-byte WeCom image cap/);
  assert.match(out.error, /downscale\/re-encode/);
  assert.match(out.error, /WECOM_IMAGE_MAX_BYTES/);
  assert.equal(calls.length, 0);
});

test('non-image content behind an image flag is rejected by magic bytes', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'fake.png', Buffer.from('%PDF-1.7 not an image at all'));
  const { publisher, calls } = makePublisher();

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 400);
  assert.match(res.json().error, /accept jpg\/jpeg, png, or gif — .* has pdf content/);
  assert.equal(calls.length, 0);
});

test('a mov video is rejected — WeCom video messages are mp4 only', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'clip.mov', movBytes);
  const { publisher } = makePublisher();

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'video',
  });
  assert.equal(res.statusCode, 400);
  assert.match(res.json().error, /accept mp4 — .* has mov content/);
});

test('gif is accepted for images and png is rejected for videos', async (t) => {
  const dir = tmpDir(t);
  const gif = writeFixture(dir, 'anim.gif', gifBytes);
  const png = writeFixture(dir, 'still.png', pngBytes);
  const { publisher } = makePublisher();

  const ok = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' }, file_path: gif, media_kind: 'image',
  });
  assert.equal(ok.statusCode, 200);

  const bad = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' }, file_path: png, media_kind: 'video',
  });
  assert.equal(bad.statusCode, 400);
  assert.match(bad.json().error, /accept mp4/);
});

test('missing file, empty file, relative path, symlink, and bad media_kind all 400', async (t) => {
  const dir = tmpDir(t);
  const empty = writeFixture(dir, 'empty.png', Buffer.alloc(0));
  const real = writeFixture(dir, 'real.png', pngBytes);
  const link = path.join(dir, 'link.png');
  fs.symlinkSync(real, link);
  const { publisher, calls } = makePublisher();
  const convo = { conversation_id: 'zhang_san' };

  const missing = await publishMedia(publisher, { conversation: convo, file_path: path.join(dir, 'nope.png'), media_kind: 'image' });
  assert.equal(missing.statusCode, 400);
  assert.match(missing.json().error, /cannot open/);

  const emptyRes = await publishMedia(publisher, { conversation: convo, file_path: empty, media_kind: 'image' });
  assert.equal(emptyRes.statusCode, 400);
  assert.match(emptyRes.json().error, /is empty/);

  const rel = await publishMedia(publisher, { conversation: convo, file_path: 'relative/photo.png', media_kind: 'image' });
  assert.equal(rel.statusCode, 400);
  assert.match(rel.json().error, /must be absolute/);

  const sym = await publishMedia(publisher, { conversation: convo, file_path: link, media_kind: 'image' });
  assert.equal(sym.statusCode, 400);
  assert.match(sym.json().error, /symlink/);

  const badKind = await publishMedia(publisher, { conversation: convo, file_path: real, media_kind: 'voice' });
  assert.equal(badKind.statusCode, 400);
  assert.match(badKind.json().error, /media_kind must be one of: image, video/);

  assert.equal(calls.length, 0);
});

// --- failure resume under one idempotency key ---------------------------------

test('a failed send resumes at the failed stage on retry — never a second upload', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let uploads = 0;
  let sendAttempts = 0;
  const { publisher, calls } = makePublisher({
    uploadMedia: async (buffer, options) => {
      uploads += 1;
      return { media_id: `MEDIA_${uploads}` };
    },
    sendMediaMessage: async (chatid, type, mediaId) => {
      sendAttempts += 1;
      if (sendAttempts === 1) throw new Error('ws not connected');
      calls.push({ op: 'sendMediaMessage', chatid, type, mediaId });
      return { headers: { req_id: 'MSG_OK' } };
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-resume-1',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'provider_error');
  assert.match(first.json().error, /ws not connected/);

  const second = await publishMedia(publisher, body);
  assert.equal(second.statusCode, 200);
  assert.equal(second.json().delivered, true);
  assert.equal(uploads, 1, 'the media must not be uploaded twice');
  assert.equal(calls.at(-1).mediaId, 'MEDIA_1');
});

test('a failed caption resumes without re-sending the already-delivered media', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let mediaSends = 0;
  let captionAttempts = 0;
  const { publisher } = makePublisher({
    sendMediaMessage: async () => {
      mediaSends += 1;
      return { headers: { req_id: 'MSG_1' } };
    },
    sendMessage: async () => {
      captionAttempts += 1;
      if (captionAttempts === 1) throw new Error('ack timeout');
      return { headers: { req_id: 'CAPTION_OK' } };
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    text: 'caption',
    idempotency_key: 'key-resume-2',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);

  const second = await publishMedia(publisher, body);
  assert.equal(second.statusCode, 200);
  assert.equal(second.json().caption_message_id, 'CAPTION_OK');
  assert.equal(mediaSends, 1, 'the image must not be shown to the user twice');
  assert.equal(captionAttempts, 2);
});

test('a settled media key answers retries from the receipt without new sends', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher();
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-settled',
  };

  const first = await publishMedia(publisher, body);
  const second = await publishMedia(publisher, body);
  assert.equal(second.statusCode, 200);
  assert.equal(second.json().message_id, first.json().message_id);
  assert.deepEqual(calls.map((c) => c.op), ['uploadMedia', 'sendMediaMessage']);
});

// --- owner claim under concurrent retries (finding 5) ----------------------------

// Codex jg-d0xr finding 5: claimPublishState was async and left installing
// state.promise to the caller, so every waiter resumed by one in-flight
// failure claimed ownership at once — one resumed media send but TWO
// postOutbound transcript recordings (duplicate transcript entries and
// fanout). The owner promise now installs synchronously inside the claim.
test('after an in-flight media failure only ONE waiting retry acquires the send', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  let failFirst;
  const firstSendGate = new Promise((r) => { failFirst = r; });
  const { publisher, calls, outboundPosts } = makePublisher({
    sendMediaMessage: async (chatid, type, mediaId) => {
      sendAttempts += 1;
      if (sendAttempts === 1) {
        await firstSendGate;
        throw new Error('ws not connected');
      }
      calls.push({ op: 'sendMediaMessage', chatid, type, mediaId });
      return { headers: { req_id: 'MSG_OK' } };
    },
  });
  const body = {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-owner-race',
  };

  const p1 = publishMedia(publisher, body);
  // Let p1 reach the blocked send before the retries arrive and wait.
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
  const p2 = publishMedia(publisher, body);
  const p3 = publishMedia(publisher, body);
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
  failFirst();
  const [r1, r2, r3] = await Promise.all([p1, p2, p3]);

  assert.equal(r1.statusCode, 502);
  assert.equal(r2.statusCode, 200);
  assert.equal(r3.statusCode, 200);
  assert.equal(r2.json().message_id, r3.json().message_id);
  assert.equal(sendAttempts, 2, 'exactly one waiter may resume the send');
  assert.equal(outboundPosts.length, 1, 'a duplicate owner records the transcript twice');
});

test('after an in-flight text failure only ONE waiting retry re-sends', async (t) => {
  let sendAttempts = 0;
  let failFirst;
  const firstSendGate = new Promise((r) => { failFirst = r; });
  const { publisher } = makePublisher({
    sendMessage: async () => {
      sendAttempts += 1;
      if (sendAttempts === 1) {
        await firstSendGate;
        throw new Error('ws not connected');
      }
      return { headers: { req_id: 'TEXT_OK' } };
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    text: 'once only',
    idempotency_key: 'key-text-owner-race',
  };

  const p1 = publishText(publisher, body);
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
  const p2 = publishText(publisher, body);
  const p3 = publishText(publisher, body);
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
  failFirst();
  const [r1, r2, r3] = await Promise.all([p1, p2, p3]);

  assert.equal(r1.statusCode, 502);
  assert.equal(r2.statusCode, 200);
  assert.equal(r3.statusCode, 200);
  assert.equal(sendAttempts, 2, 'the message must not be re-sent by both waiters');
});

// --- transcript recording edge cases -------------------------------------------

test('no session_id: delivered, transcript not recorded, note says why', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, outboundPosts } = makePublisher();

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 200);
  const out = res.json();
  assert.equal(out.delivered, true);
  assert.equal(out.transcript_recorded, false);
  assert.match(out.transcript_note, /no session_id/);
  assert.equal(outboundPosts.length, 0);
});

test('a gc recording failure downgrades to a note — delivery already happened', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher } = makePublisher({
    postOutbound: async () => {
      const err = new Error('422 Unprocessable Entity: no active binding for conversation wecom/zhang_san');
      err.status = 422;
      throw err;
    },
  });

  const res = await publishMedia(publisher, {
    session_id: 'sess-rogue',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 200);
  const out = res.json();
  assert.equal(out.delivered, true);
  assert.equal(out.transcript_recorded, false);
  assert.match(out.transcript_note, /no active binding/);
});

test('a 200 OutboundResult without a TranscriptEntry still counts as not recorded', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher } = makePublisher({
    // gc's transcript append is non-fatal on its side: a 200 can come
    // back with no entry (e.g. hydration-pending conversation).
    postOutbound: async () => ({ Receipt: { Delivered: true }, TranscriptEntry: null }),
  });

  const res = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 200);
  const out = res.json();
  assert.equal(out.delivered, true);
  assert.equal(out.transcript_recorded, false);
  assert.match(out.transcript_note, /recorded no transcript entry/);
});

// --- kind resolution -----------------------------------------------------------

test('kind resolution: explicit kind > learned store > wr-prefix heuristic > dm', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const kindStore = createConversationKindStore({
    filePath: path.join(dir, 'kinds.json'),
    log: () => {},
  });
  // The store learned from inbound that 'ambiguous_id' is a room.
  kindStore.observe({ body: { chattype: 'group', chatid: 'ambiguous_id' } });
  const { publisher, outboundPosts } = makePublisher({ kindStore });

  const cases = [
    // explicit kind wins over the learned room
    [{ conversation_id: 'ambiguous_id', kind: 'dm' }, 'dm'],
    // learned store
    [{ conversation_id: 'ambiguous_id' }, 'room'],
    // wr prefix heuristic for a never-seen group chatid
    [{ conversation_id: 'wrNEVERSEEN' }, 'room'],
    // plain userid defaults to dm
    [{ conversation_id: 'li_si' }, 'dm'],
  ];
  for (const [conversation, expected] of cases) {
    outboundPosts.length = 0;
    const res = await publishMedia(publisher, {
      session_id: 'sess-mayor',
      conversation,
      file_path: file,
      media_kind: 'image',
    });
    assert.equal(res.statusCode, 200);
    assert.equal(outboundPosts[0].body.conversation.kind, expected,
      `conversation ${JSON.stringify(conversation)} should record as ${expected}`);
  }
});

test('an invalid conversation.kind is rejected up front', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher } = makePublisher();
  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san', kind: 'channel' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 400);
  assert.match(res.json().error, /kind must be "dm" or "room"/);
});

// --- conversation-kind store ----------------------------------------------------

test('the kind store learns from inbound frames and survives a restart', async (t) => {
  const dir = tmpDir(t);
  const filePath = path.join(dir, 'kinds.json');
  const store = createConversationKindStore({ filePath, log: () => {} });

  store.observe({ body: { chattype: 'single', from: { userid: 'zhang_san' } } });
  store.observe({ body: { chattype: 'group', chatid: 'wrROOM_1' } });
  store.observe({ body: null }); // hostile/no-op frames are ignored
  store.observe({ body: { chattype: 'single', from: {} } });

  assert.equal(store.lookup('zhang_san'), 'dm');
  assert.equal(store.lookup('wrROOM_1'), 'room');
  assert.equal(store.lookup('never_seen'), undefined);

  const reloaded = createConversationKindStore({ filePath, log: () => {} });
  assert.equal(reloaded.lookup('zhang_san'), 'dm');
  assert.equal(reloaded.lookup('wrROOM_1'), 'room');
});

test('a corrupt kind-store file starts empty instead of crashing', async (t) => {
  const dir = tmpDir(t);
  const filePath = path.join(dir, 'kinds.json');
  fs.writeFileSync(filePath, '{not json');
  const store = createConversationKindStore({ filePath, log: () => {} });
  assert.equal(store.size(), 0);
  store.observe({ body: { chattype: 'single', from: { userid: 'ok' } } });
  assert.equal(store.lookup('ok'), 'dm');
});

test('the kind store evicts oldest entries past its cap', async (t) => {
  const dir = tmpDir(t);
  const store = createConversationKindStore({
    filePath: path.join(dir, 'kinds.json'),
    log: () => {},
    cap: 2,
  });
  store.observe({ body: { chattype: 'single', from: { userid: 'a' } } });
  store.observe({ body: { chattype: 'single', from: { userid: 'b' } } });
  store.observe({ body: { chattype: 'single', from: { userid: 'c' } } });
  assert.equal(store.lookup('a'), undefined);
  assert.equal(store.lookup('b'), 'dm');
  assert.equal(store.lookup('c'), 'dm');
});

// --- per-chat ordering across endpoints ------------------------------------------

test('a text publish queues behind an in-flight media send to the same chat', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let releaseUpload;
  const uploadGate = new Promise((r) => { releaseUpload = r; });
  const order = [];
  const { publisher } = makePublisher({
    uploadMedia: async () => {
      order.push('upload-start');
      await uploadGate;
      order.push('upload-done');
      return { media_id: 'MEDIA_SLOW' };
    },
    sendMediaMessage: async () => {
      order.push('media-send');
      return { headers: { req_id: 'M' } };
    },
    sendMessage: async () => {
      order.push('text-send');
      return { headers: { req_id: 'T' } };
    },
  });

  const mediaP = publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  // Give the media publish a tick to claim the chain before the text lands.
  await new Promise((r) => setImmediate(r));
  const textP = publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: 'after the image',
  });
  await new Promise((r) => setImmediate(r));
  releaseUpload();
  await Promise.all([mediaP, textP]);

  assert.deepEqual(order, ['upload-start', 'upload-done', 'media-send', 'text-send']);
});

// --- relocated text publish regressions -------------------------------------------

test('a text publish sends one markdown message and returns its receipt', async (t) => {
  const { publisher, calls } = makePublisher();
  const res = await publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: '**build** is green',
  });
  assert.equal(res.statusCode, 200);
  assert.equal(res.json().delivered, true);
  assert.deepEqual(calls, [{
    op: 'sendMessage',
    chatid: 'zhang_san',
    body: { msgtype: 'markdown', markdown: { content: '**build** is green' } },
  }]);
});

test('long text chunks at the UTF-8 byte cap and a keyed retry never re-sends', async (t) => {
  const { publisher, calls } = makePublisher();
  const text = '很长的消息。'.repeat(400); // 6 chars ≈ 18 UTF-8 bytes each → > 1 chunk
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    text,
    idempotency_key: 'key-text-1',
  };
  const first = await publishText(publisher, body);
  assert.equal(first.statusCode, 200);
  const expectedChunks = chunkText(text);
  assert.ok(expectedChunks.length > 1);
  assert.equal(calls.length, expectedChunks.length);
  for (const chunk of expectedChunks) {
    assert.ok(new TextEncoder().encode(chunk).length <= outboundChunkBytes);
  }
  assert.equal(expectedChunks.join(''), text);

  const retry = await publishText(publisher, body);
  assert.equal(retry.statusCode, 200);
  assert.equal(calls.length, expectedChunks.length, 'a keyed retry must not re-send chunks');
});

test('a text publish with a missing body 400s like before', async (t) => {
  const { publisher } = makePublisher();
  const res = await publishText(publisher, { conversation: { conversation_id: 'zhang_san' } });
  assert.equal(res.statusCode, 400);
  const bad = fakeRes();
  await publisher.handlePublish({}, bad, 'not json');
  assert.equal(bad.statusCode, 400);
});
