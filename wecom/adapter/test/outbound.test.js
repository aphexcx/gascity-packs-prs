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
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';

import {
  chunkText,
  createAttemptJournal,
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
  // realpath'd so fixture paths compare lexically against the confined
  // outbound-media root (macOS: /var/folders → /private/var/folders).
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'wecom-outbound-test-')));
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
    // Every tmpDir fixture lives under the OS temp dir, so this default
    // root confines the whole suite; confinement tests override it.
    outboundMediaRoot: fs.realpathSync(os.tmpdir()),
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
    ...(overrides.journal ? { journal: overrides.journal } : {}),
    ...(overrides.withUploadDeadline ? { withUploadDeadline: overrides.withUploadDeadline } : {}),
    publishStatesCap: overrides.publishStatesCap ?? 512,
  });
  return { publisher, calls, outboundPosts, cfg };
}

async function publishMedia(publisher, body) {
  const res = fakeRes();
  // The CLI generates one idempotency key per logical invocation
  // (finding 2) and the adapter refuses keyless media publishes; mirror
  // the CLI here so each helper call is one fresh invocation. Tests
  // exercising retries pass an explicit key.
  const withKey = body.idempotency_key
    ? body
    : { ...body, idempotency_key: `test-${crypto.randomUUID()}` };
  await publisher.handlePublishMedia({}, res, JSON.stringify(withKey));
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

test('missing file, empty file, relative path, and bad media_kind 400; symlink 403', async (t) => {
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

  // Symlinks are refused as a confinement violation (403 forbidden) since
  // the jg-d0xr finding-1 fix — they were a plain 400 before.
  const sym = await publishMedia(publisher, { conversation: convo, file_path: link, media_kind: 'image' });
  assert.equal(sym.statusCode, 403);
  assert.match(sym.json().error, /symlink/);

  const badKind = await publishMedia(publisher, { conversation: convo, file_path: real, media_kind: 'voice' });
  assert.equal(badKind.statusCode, 400);
  assert.match(badKind.json().error, /media_kind must be one of: image, video/);

  assert.equal(calls.length, 0);
});

// --- outbound-media root confinement (finding 1) -----------------------------------

// Codex jg-d0xr finding 1: /publish-media accepted EVERY absolute path
// the adapter could read — O_NOFOLLOW protected only the final component,
// so a compromised local caller could route arbitrary private files on
// the host to a WeCom chat through symlinked parents. The endpoint now
// fails closed without a configured root, confines lexically to the
// canonicalized root, and refuses symlinks at any depth.
test('media publishing fails closed when no outbound media root is configured', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher({ cfg: { outboundMediaRoot: '' } });

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 403);
  assert.equal(res.json().failure_kind, 'forbidden');
  assert.match(res.json().error, /WECOM_OUTBOUND_MEDIA_ROOT is not set/);
  assert.equal(calls.length, 0);
});

test('a readable file OUTSIDE the outbound media root is refused', async (t) => {
  const dir = tmpDir(t);
  const root = path.join(dir, 'root');
  fs.mkdirSync(root);
  const outside = writeFixture(dir, 'private.png', pngBytes); // sibling of root
  const inside = writeFixture(root, 'ok.png', pngBytes);
  const { publisher, calls } = makePublisher({ cfg: { outboundMediaRoot: root } });

  const refused = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: outside,
    media_kind: 'image',
  });
  assert.equal(refused.statusCode, 403);
  assert.match(refused.json().error, /must live under the outbound media root/);
  assert.equal(calls.length, 0);

  const traversal = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: path.join(root, '..', 'private.png'),
    media_kind: 'image',
  });
  assert.equal(traversal.statusCode, 403);

  const ok = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: inside,
    media_kind: 'image',
  });
  assert.equal(ok.statusCode, 200);
});

test('a symlinked PARENT directory inside the root is refused, even resolving inside it', async (t) => {
  const dir = tmpDir(t);
  const root = path.join(dir, 'root');
  const secretDir = path.join(dir, 'secret');
  fs.mkdirSync(root);
  fs.mkdirSync(secretDir);
  writeFixture(secretDir, 'private.png', pngBytes);
  // A dir-symlink escaping the root: lexically confined, physically not.
  fs.symlinkSync(secretDir, path.join(root, 'escape'));
  // And one that resolves back INSIDE the root — still refused: symlink
  // handling must be uniform or racing links become an oracle.
  const realDir = path.join(root, 'real');
  fs.mkdirSync(realDir);
  writeFixture(realDir, 'ok.png', pngBytes);
  fs.symlinkSync(realDir, path.join(root, 'alias'));
  const { publisher, calls } = makePublisher({ cfg: { outboundMediaRoot: root } });

  for (const p of [path.join(root, 'escape', 'private.png'), path.join(root, 'alias', 'ok.png')]) {
    const res = await publishMedia(publisher, {
      conversation: { conversation_id: 'zhang_san' },
      file_path: p,
      media_kind: 'image',
    });
    assert.equal(res.statusCode, 403, p);
    assert.match(res.json().error, /symlink/);
  }
  assert.equal(calls.length, 0);
});

test('a root that does not exist fails closed instead of open', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher({ cfg: { outboundMediaRoot: path.join(dir, 'nope') } });

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(res.statusCode, 403);
  assert.match(res.json().error, /does not resolve to an existing directory/);
  assert.equal(calls.length, 0);
});

// Round-2 finding 1: the round-1 walk lstat'ed every component and THEN
// re-opened the original pathname — a writer beneath the root could
// rename a checked directory into a symlink between the check and the
// open and exfiltrate any adapter-readable file. This test wins that race
// deterministically against the old code by hooking fs.promises.open (the
// old open path) to perform the swap at the exact moment; the fixed walk
// opens the file from the verified directory descriptor chain in one
// synchronous operation, so the hook never fires and the swap never lands.
test('a parent-dir symlink swap between check and open cannot escape the media root', async (t) => {
  const dir = tmpDir(t);
  const root = path.join(dir, 'root');
  const sub = path.join(root, 'sub');
  const secretDir = path.join(dir, 'secret');
  fs.mkdirSync(sub, { recursive: true });
  fs.mkdirSync(secretDir);
  const legit = writeFixture(sub, 'photo.png', pngBytes);
  const secretBytes = Buffer.concat([pngBytes, Buffer.from(' SECRET-HOST-FILE')]);
  fs.writeFileSync(path.join(secretDir, 'photo.png'), secretBytes);

  const uploads = [];
  const { publisher } = makePublisher({
    cfg: { outboundMediaRoot: root },
    uploadMedia: async (buffer) => {
      uploads.push(Buffer.from(buffer));
      return { media_id: 'MEDIA_RACE' };
    },
  });

  // The attacker's rename+symlink swap, armed to fire inside the old
  // check-to-open window.
  const swapParentToSymlink = () => {
    fs.renameSync(sub, `${sub}.stash`);
    fs.symlinkSync(secretDir, sub);
  };
  const realOpen = fs.promises.open;
  fs.promises.open = async (p, ...rest) => {
    if (p === legit) swapParentToSymlink();
    return realOpen.call(fs.promises, p, ...rest);
  };
  t.after(() => { fs.promises.open = realOpen; });

  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: legit,
    media_kind: 'image',
  });

  // Whatever the outcome (200 on the legit bytes, or a refusal if the
  // swap landed mid-walk), the secret bytes must never reach the provider.
  for (const uploaded of uploads) {
    assert.ok(!uploaded.equals(secretBytes),
      'an out-of-root file was uploaded: the check-to-open window is still exploitable');
  }
  if (res.statusCode === 200) {
    assert.equal(uploads.length, 1);
    assert.ok(uploads[0].equals(pngBytes), 'the confined file itself should have been read');
  }
});

// Round-2 finding 1, authorization clause: gc's binding check runs only
// after delivery (and only with a session_id), so the adapter enforces
// the strongest PRE-delivery target check it can — media only goes to
// conversations with inbound evidence — unless explicitly opened up.
test('media publishes require inbound evidence for the target conversation when gated', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const kindStore = createConversationKindStore({ filePath: path.join(dir, 'kinds.json'), log: () => {} });
  const { publisher, calls } = makePublisher({
    kindStore,
    cfg: { mediaRequireKnownConversation: true },
  });

  const refused = await publishMedia(publisher, {
    conversation: { conversation_id: 'never_seen' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(refused.statusCode, 403);
  assert.equal(refused.json().failure_kind, 'forbidden');
  assert.match(refused.json().error, /inbound history/);
  assert.match(refused.json().error, /WECOM_MEDIA_ALLOW_UNSEEN_CONVERSATIONS/);
  assert.equal(calls.length, 0, 'refused before any file or provider work');

  // Inbound traffic from the conversation unlocks it.
  kindStore.observe({ body: { chattype: 'single', from: { userid: 'zhang_san' } } });
  const ok = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  });
  assert.equal(ok.statusCode, 200);
});

// --- idempotency key requirement (finding 2) --------------------------------------

// Codex jg-d0xr finding 2: the adapter minted a fresh UUID per keyless
// request, so a rerun after a lost HTTP response sent and recorded the
// media AGAIN. The key now must come from the caller (the CLI generates
// one per logical invocation) and is echoed in every response so operator
// retries can reuse it.
test('a media publish without an idempotency key is refused before any provider work', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher();

  const res = fakeRes();
  await publisher.handlePublishMedia({}, res, JSON.stringify({
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
  }));
  assert.equal(res.statusCode, 400);
  assert.match(res.json().error, /idempotency_key is required/);
  assert.match(res.json().error, /--idempotency-key/);
  assert.equal(calls.length, 0);
});

test('media responses echo the idempotency key on success and on failure', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  const { publisher } = makePublisher({
    sendMediaMessage: async () => {
      sendAttempts += 1;
      if (sendAttempts === 1) throw new Error('ws not connected');
      return { headers: { req_id: 'MSG_OK' } };
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-echo',
  };

  const failed = await publishMedia(publisher, body);
  assert.equal(failed.statusCode, 502);
  assert.equal(failed.json().idempotency_key, 'key-echo');

  const ok = await publishMedia(publisher, body);
  assert.equal(ok.statusCode, 200);
  assert.equal(ok.json().idempotency_key, 'key-echo');
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
      // A DEFINITE failure — ack timeouts are delivery-unknown since the
      // finding-3 fix and refuse the retry (covered by their own test).
      if (captionAttempts === 1) throw new Error('ws not connected');
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
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-settled',
  };

  const first = await publishMedia(publisher, body);
  const second = await publishMedia(publisher, body);
  assert.equal(second.statusCode, 200);
  assert.equal(second.json().message_id, first.json().message_id);
  // The COMPLETE response is cached (finding 8): the retry keeps media_id
  // and the transcript outcome instead of degrading to a bare receipt.
  assert.deepEqual(second.json(), first.json());
  assert.equal(second.json().media_id, 'MEDIA_1');
  assert.equal(second.json().transcript_recorded, true);
  assert.deepEqual(calls.map((c) => c.op), ['uploadMedia', 'sendMediaMessage']);
});

// --- idempotency fingerprint (finding 4) and file-free settled retries (finding 7) —

// Codex jg-d0xr finding 4: state keyed by the key alone let a retried key
// carry a DIFFERENT chat/file/caption and inherit the previous attempt's
// latched media_id — file A delivered under B's request, B's metadata
// recorded for it. Every reuse must describe the identical send or 409.
test('a settled key retried with a different chat, file, or caption answers 409', async (t) => {
  const dir = tmpDir(t);
  const fileA = writeFixture(dir, 'a.png', pngBytes);
  const fileB = writeFixture(dir, 'b.png', jpgBytes);
  const { publisher, calls } = makePublisher();
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: fileA,
    media_kind: 'image',
    text: 'caption A',
    idempotency_key: 'key-fp-settled',
  };
  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 200);
  const callsAfterFirst = calls.length;

  for (const mutation of [
    { conversation: { conversation_id: 'li_si' } },
    { file_path: fileB },
    { text: 'caption B' },
  ]) {
    const res = await publishMedia(publisher, { ...body, ...mutation });
    assert.equal(res.statusCode, 409, JSON.stringify(mutation));
    assert.equal(res.json().failure_kind, 'idempotency_conflict');
    assert.equal(res.json().idempotency_key, 'key-fp-settled');
  }
  const identical = await publishMedia(publisher, body);
  assert.equal(identical.statusCode, 200);
  assert.equal(calls.length, callsAfterFirst, 'no provider traffic for conflicting or deduped retries');
});

test('a failed key retried with different bytes at the same path never rides the latched media_id', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  const { publisher, calls } = makePublisher({
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
    idempotency_key: 'key-fp-partial',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502, 'upload latched, send failed');

  // Same path, different content: the latched media_id belongs to the OLD
  // bytes — resuming would show the user content nobody asked to send.
  fs.writeFileSync(file, gifBytes);
  const swapped = await publishMedia(publisher, body);
  assert.equal(swapped.statusCode, 409);
  assert.match(swapped.json().error, /media digest/);
  assert.equal(sendAttempts, 1, 'the latched media_id must not be sent for different bytes');

  // Restoring the original bytes makes the retry the identical send again.
  fs.writeFileSync(file, pngBytes);
  const resumed = await publishMedia(publisher, body);
  assert.equal(resumed.statusCode, 200);
  assert.equal(calls.at(-1).mediaId, 'MEDIA_1', 'the resume rides the original upload');
});

// Codex jg-d0xr round-2 finding 9: cross-endpoint fingerprinting was
// asymmetric. Once the transcript seed was removed, /publish returned any
// settled MEDIA receipt without checking state.endpoint — a legitimate
// text publish reusing that key got 200 and sent NO text. Only the actual
// recording callback (matching conversation + exact transcript text) may
// consume a media receipt now; anything else is a 409 conflict.
test('after the seed is gone, a text publish reusing a media key is refused — not silently answered', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let transcriptText;
  const { publisher, calls } = makePublisher({
    postOutbound: async (target, body) => {
      transcriptText = body.text;
      return { Receipt: { Delivered: true }, TranscriptEntry: { ID: 'tr-1' } };
    },
  });
  const key = 'key-endpoint-asymmetry';
  const media = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: key,
  });
  assert.equal(media.statusCode, 200);
  assert.equal(publisher.stats().transcriptSeeds, 0, 'the seed window is closed');
  const callsAfterMedia = calls.length;

  // A DIFFERENT, legitimate text publish that happens to reuse the key.
  const text = await publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: 'a totally different reply',
    idempotency_key: key,
  });
  assert.equal(text.statusCode, 409, 'the text must not be silently dropped by returning the media receipt');
  assert.equal(text.json().failure_kind, 'idempotency_conflict');
  assert.equal(calls.length, callsAfterMedia, 'and it must not have re-sent the media either');

  // The ACTUAL recording callback (matching conversation + exact text)
  // still consumes the media receipt without re-sending — even after the
  // seed window closed.
  const callback = await publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: transcriptText,
    idempotency_key: key,
  });
  assert.equal(callback.statusCode, 200);
  assert.equal(callback.json().message_id, media.json().message_id);
  assert.equal(calls.length, callsAfterMedia, 'the matching callback sends nothing');
});

test('a callback for the right key but the WRONG conversation cannot consume a media receipt', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let transcriptText;
  const { publisher } = makePublisher({
    postOutbound: async (target, body) => {
      transcriptText = body.text;
      return { Receipt: { Delivered: true }, TranscriptEntry: { ID: 'tr-1' } };
    },
  });
  const key = 'key-wrong-convo';
  await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: key,
  });

  // Same transcript text but a different conversation → not the callback.
  const res = await publishText(publisher, {
    conversation: { conversation_id: 'li_si' },
    text: transcriptText,
    idempotency_key: key,
  });
  assert.equal(res.statusCode, 409);
});

test('a key settled by /publish cannot be replayed against /publish-media', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher();

  const text = await publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: 'plain text',
    idempotency_key: 'key-cross-endpoint',
  });
  assert.equal(text.statusCode, 200);

  const media = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-cross-endpoint',
  });
  assert.equal(media.statusCode, 409, 'returning the text receipt for a media publish lies to the caller');
  assert.deepEqual(calls.map((c) => c.op), ['sendMessage'], 'no media traffic under the replayed key');
});

// Codex jg-d0xr finding 7: the file was reopened and hashed BEFORE the
// settled-receipt check, so a retry after the original delivery response
// was lost 400'd if the file had since been deleted — instead of
// returning the cached success it is entitled to.
test('a settled key retried after the file is gone still answers the cached receipt', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher();
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-file-gone',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 200);
  fs.rmSync(file);

  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 200);
  assert.equal(retry.json().message_id, first.json().message_id);
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

// Codex jg-d0xr round-2 finding 6: on a gate/path refusal the owner woke
// same-key waiters AND releaseUntouchedState deleted the shared map entry.
// Each waiter had captured that state before awaiting, so it resumed
// owning an ORPHAN no longer present in publishStates — a parallel fresh
// claim then created a second state in the map and sent concurrently,
// duplicating the same key's media. The claim loop must refetch the map
// entry after every wake.
test('waiters woken by a refused owner rejoin the map instead of owning an orphan state', async (t) => {
  const dir = tmpDir(t);
  const real = writeFixture(dir, 'photo.png', pngBytes);
  const missing = path.join(dir, 'nope.png');
  let uploadCount = 0;
  let releaseUploads;
  const uploadHold = new Promise((r) => { releaseUploads = r; });
  const { publisher, calls } = makePublisher({
    uploadMedia: async () => {
      uploadCount += 1;
      await uploadHold;
      return { media_id: `MEDIA_${uploadCount}` };
    },
  });
  const bodyFor = (file) => ({
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-orphan-race',
  });
  const chase = async (n) => { for (let i = 0; i < n; i++) await new Promise((r) => setImmediate(r)); };

  // The owner is refused at the path stage (missing file) and retires the
  // untouched state; two same-key retries are already waiting on it.
  const p1 = publishMedia(publisher, bodyFor(missing));
  const p2 = publishMedia(publisher, bodyFor(real));
  const p3 = publishMedia(publisher, bodyFor(real));
  const r1 = await p1;
  assert.equal(r1.statusCode, 400);

  // A woken waiter becomes the new owner and blocks in the upload …
  while (uploadCount === 0) await chase(1);
  // … while a FRESH claim for the same key arrives. It must join the
  // in-flight send through the map, not start its own.
  const p4 = publishMedia(publisher, bodyFor(real));
  await chase(4);
  releaseUploads();
  const [r2, r3, r4] = await Promise.all([p2, p3, p4]);

  assert.equal(r2.statusCode, 200);
  assert.equal(r3.statusCode, 200);
  assert.equal(r4.statusCode, 200);
  assert.equal(uploadCount, 1, 'the orphaned owner and the fresh claim must not both upload');
  assert.equal(calls.filter((c) => c.op === 'sendMediaMessage').length, 1,
    'the media must reach the chat exactly once');
  assert.equal(r3.json().message_id, r2.json().message_id);
  assert.equal(r4.json().message_id, r2.json().message_id);
});

// --- global outbound admission (finding 9) ----------------------------------------

// Codex jg-d0xr finding 9: every concurrent request allocated and hashed
// up to 10MB before touching idempotency state and held the buffer
// through upload — no global bound. Admission now takes an upload-gate
// slot BEFORE any file I/O; the queue is capped and overflow answers 429.
test('upload admission is globally bounded: slot, bounded queue, then 429', async (t) => {
  const dir = tmpDir(t);
  const fileA = writeFixture(dir, 'a.png', pngBytes);
  const fileB = writeFixture(dir, 'b.png', pngBytes);
  const fileC = writeFixture(dir, 'c.png', pngBytes);
  let releaseFirstUpload;
  const firstUploadGate = new Promise((r) => { releaseFirstUpload = r; });
  let uploads = 0;
  const { publisher } = makePublisher({
    cfg: { uploadMaxConcurrent: 1, uploadMaxQueue: 1 },
    uploadMedia: async () => {
      uploads += 1;
      if (uploads === 1) await firstUploadGate;
      return { media_id: `MEDIA_${uploads}` };
    },
  });
  const bodyFor = (file, key, chat) => ({
    conversation: { conversation_id: chat },
    file_path: file,
    media_kind: 'image',
    idempotency_key: key,
  });

  const p1 = publishMedia(publisher, bodyFor(fileA, 'key-gate-1', 'chat_1'));
  while (uploads === 0) await new Promise((r) => setImmediate(r));
  const p2 = publishMedia(publisher, bodyFor(fileB, 'key-gate-2', 'chat_2'));
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));
  const p3 = publishMedia(publisher, bodyFor(fileC, 'key-gate-3', 'chat_3'));
  const r3 = await p3;
  assert.equal(r3.statusCode, 429, 'beyond the queue cap the request is refused before reading');
  assert.equal(r3.json().failure_kind, 'overloaded');
  assert.match(r3.json().error, /outbound upload capacity/);
  assert.equal(r3.json().idempotency_key, 'key-gate-3');

  releaseFirstUpload();
  const [r1, r2] = await Promise.all([p1, p2]);
  assert.equal(r1.statusCode, 200);
  assert.equal(r2.statusCode, 200, 'a queued waiter proceeds once the slot frees');
  assert.equal(uploads, 2);

  // The refused key was dropped untouched: retrying it later succeeds.
  const retry3 = await publishMedia(publisher, bodyFor(fileC, 'key-gate-3', 'chat_3'));
  assert.equal(retry3.statusCode, 200);
});

// Codex jg-d0xr round-2 finding 8: the round-1 refuse-over-evict policy
// wedged the SHARED cache — during an outage, cap-many keyed text sends
// that failed before delivering wedged every later legacy text publish
// into a 503 until restart. Text and media now have SEPARATE pools, the
// journal is the durable media source of truth, and a non-live media
// entry is evictable from memory (rehydrated on retry) — a new media key
// is refused only under genuine LIVE concurrency.
test('a media backlog never wedges legacy text delivery (separate pools)', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const journalPath = path.join(dir, 'attempts.json');
  let sendAttempts = 0;
  const { publisher } = makePublisher({
    publishStatesCap: 1, // media pool cap = 1
    journal: createAttemptJournal({ filePath: journalPath, log: () => {} }),
    sendMediaMessage: async () => {
      sendAttempts += 1;
      if (sendAttempts === 1) throw new Error('ws not connected');
      return { headers: { req_id: 'MSG_OK' } };
    },
  });
  const mediaBody = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-partial-A',
  };

  // Key A fails after upload — a non-live partial (journaled, resumable).
  const failed = await publishMedia(publisher, mediaBody);
  assert.equal(failed.statusCode, 502);

  // Legacy text is NEVER refused by a full media pool (the regression).
  const textOk = await publishText(publisher, {
    conversation: { conversation_id: 'li_si' },
    text: 'hello',
    idempotency_key: 'key-new-B',
  });
  assert.equal(textOk.statusCode, 200);

  // A new media key evicts the non-live partial from MEMORY (the journal
  // keeps it) and is admitted — no permanent wedge.
  const fileC = writeFixture(dir, 'c.png', jpgBytes);
  const mediaC = await publishMedia(publisher, {
    conversation: { conversation_id: 'wang_wu' },
    file_path: fileC,
    media_kind: 'image',
    idempotency_key: 'key-new-C',
  });
  assert.equal(mediaC.statusCode, 200);

  // The evicted partial's own retry rehydrates from the journal and
  // resumes without a second upload or a duplicate send.
  const resumed = await publishMedia(publisher, mediaBody);
  assert.equal(resumed.statusCode, 200);
  assert.equal(resumed.json().media_id, 'MEDIA_1', 'resumed from the journaled upload, not re-uploaded');
});

test('a media pool full of LIVE in-flight sends refuses a new media key (concurrency bound)', async (t) => {
  const dir = tmpDir(t);
  const fileA = writeFixture(dir, 'a.png', pngBytes);
  const fileB = writeFixture(dir, 'b.png', jpgBytes);
  let releaseSend;
  const sendHold = new Promise((r) => { releaseSend = r; });
  const { publisher } = makePublisher({
    publishStatesCap: 1,
    // Generous gate so the block is the STATE pool, not the upload slot.
    cfg: { uploadMaxConcurrent: 4, uploadMaxQueue: 4 },
    sendMediaMessage: async () => {
      await sendHold;
      return { headers: { req_id: 'MSG_OK' } };
    },
  });

  const p1 = publishMedia(publisher, {
    conversation: { conversation_id: 'chat_1' },
    file_path: fileA,
    media_kind: 'image',
    idempotency_key: 'key-live-A',
  });
  // Let A pass admission and block in the (live, promise-held) send.
  while (publisher.stats().publishStates === 0) await new Promise((r) => setImmediate(r));
  for (let i = 0; i < 4; i++) await new Promise((r) => setImmediate(r));

  const refused = await publishMedia(publisher, {
    conversation: { conversation_id: 'chat_2' },
    file_path: fileB,
    media_kind: 'image',
    idempotency_key: 'key-live-B',
  });
  assert.equal(refused.statusCode, 503, 'a pool full of live sends is a real concurrency bound');
  assert.equal(refused.json().failure_kind, 'overloaded');

  releaseSend();
  const r1 = await p1;
  assert.equal(r1.statusCode, 200);
});

// Codex jg-d0xr round-2 finding 7: settled media receipts were evicted
// from memory and the still-present journal entry was NEVER consulted
// again (journal.get was unused). After enough later publishes evicted a
// media receipt, a same-key retry was treated as new and RESENT. Keys now
// rehydrate lazily from the journal on every map miss.
test('a settled media receipt evicted from memory is rehydrated from the journal, not resent', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const journalPath = path.join(dir, 'attempts.json');
  const { publisher, calls } = makePublisher({
    publishStatesCap: 1,
    journal: createAttemptJournal({ filePath: journalPath, log: () => {} }),
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-settled-evicted',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 200);
  const sendsAfterFirst = calls.length;

  // Evict the settled receipt from memory with an unrelated media key
  // (media pool cap 1). The evicted key is still in the journal.
  const other = writeFixture(dir, 'other.png', jpgBytes);
  await publishMedia(publisher, {
    conversation: { conversation_id: 'li_si' },
    file_path: other,
    media_kind: 'image',
    idempotency_key: 'key-evictor',
  });

  // The original key retries: it MUST answer from the rehydrated journal
  // receipt, not upload+send the media a second time.
  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 200);
  assert.equal(retry.json().message_id, first.json().message_id);
  assert.equal(retry.json().media_id, first.json().media_id);
  const originalKeySends = calls.slice(sendsAfterFirst).filter(
    (c) => c.chatid === 'zhang_san',
  );
  assert.equal(originalKeySends.length, 0, 'the evicted-then-retried key must not re-send its media');
});

test('a late gc callback for an evicted media key is served from the journal, not sent as chat text', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const journalPath = path.join(dir, 'attempts.json');
  let transcriptText;
  const { publisher, calls } = makePublisher({
    publishStatesCap: 1,
    journal: createAttemptJournal({ filePath: journalPath, log: () => {} }),
    postOutbound: async (target, body) => {
      transcriptText = body.text;
      return { Receipt: { Delivered: true }, TranscriptEntry: { ID: 'tr-1' } };
    },
  });
  const key = 'key-late-callback';
  const first = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: key,
  });
  assert.equal(first.statusCode, 200);

  // Evict the media receipt from memory AND drop the seed (recording is
  // already done). Then gc retries the transcript callback LATE.
  const other = writeFixture(dir, 'other.png', jpgBytes);
  await publishMedia(publisher, {
    conversation: { conversation_id: 'li_si' },
    file_path: other,
    media_kind: 'image',
    idempotency_key: 'key-evictor-2',
  });
  const callsBefore = calls.length;

  const cb = await publishText(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    text: transcriptText,
    idempotency_key: key,
  });
  assert.equal(cb.statusCode, 200);
  assert.equal(cb.json().message_id, first.json().message_id);
  assert.equal(calls.length, callsBefore, 'the late callback must not send the transcript text as chat markdown');
});

// --- attempt journal, delivery-unknown, upload retention (finding 3) -----------------

// Codex jg-d0xr finding 3: stage latches lived only in memory (a restart
// forgot everything already delivered), lost acknowledgements were
// treated as retryable (a same-key retry could display the media twice),
// and a timed-out upload kept running in the SDK while a retry started a
// second one.
test('stage latches survive a restart: the retried key resumes without a second upload', async (t) => {
  const dir = tmpDir(t);
  const journalPath = path.join(dir, 'attempts.json');
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-restart-resume',
  };

  // Life 1: upload succeeds, send definitively fails, process "dies".
  const life1 = makePublisher({
    journal: createAttemptJournal({ filePath: journalPath }),
    sendMediaMessage: async () => { throw new Error('ws not connected'); },
  });
  const failed = await publishMedia(life1.publisher, body);
  assert.equal(failed.statusCode, 502);

  // Life 2: fresh publisher, same journal file.
  let life2Uploads = 0;
  const life2 = makePublisher({
    journal: createAttemptJournal({ filePath: journalPath }),
    uploadMedia: async () => {
      life2Uploads += 1;
      return { media_id: 'MEDIA_DUP' };
    },
  });
  const resumed = await publishMedia(life2.publisher, body);
  assert.equal(resumed.statusCode, 200);
  assert.equal(life2Uploads, 0, 'the journaled media_id must be reused across the restart');
  const sent = life2.calls.find((c) => c.op === 'sendMediaMessage');
  assert.equal(sent.mediaId, 'MEDIA_1', "life 1's upload is what gets sent");
});

test('a fully settled key answers from the journaled receipt after a restart', async (t) => {
  const dir = tmpDir(t);
  const journalPath = path.join(dir, 'attempts.json');
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-restart-settled',
  };

  const life1 = makePublisher({ journal: createAttemptJournal({ filePath: journalPath }) });
  const first = await publishMedia(life1.publisher, body);
  assert.equal(first.statusCode, 200);

  const life2 = makePublisher({ journal: createAttemptJournal({ filePath: journalPath }) });
  fs.rmSync(file); // and the file may be long gone (finding 7)
  const retry = await publishMedia(life2.publisher, body);
  assert.equal(retry.statusCode, 200);
  assert.equal(retry.json().message_id, first.json().message_id);
  assert.equal(life2.calls.length, 0, 'no provider traffic for a settled key after restart');
});

test('a lost acknowledgement classifies as delivery-unknown — never blindly re-sent', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  const { publisher } = makePublisher({
    sendMediaMessage: async () => {
      sendAttempts += 1;
      throw new Error('Reply ack timeout (10000ms) for reqId: SEND_MSG_1');
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-ack-lost',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'delivery_unknown');
  assert.match(first.json().error, /may or may not be visible/);
  assert.equal(first.json().idempotency_key, 'key-ack-lost');

  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 502);
  assert.equal(retry.json().failure_kind, 'delivery_unknown');
  assert.equal(sendAttempts, 1, 'an ambiguous send must NOT be repeated under the same key');
});

test('a crash between send-attempt and acknowledgement hydrates as delivery-unknown', async (t) => {
  const dir = tmpDir(t);
  const journalPath = path.join(dir, 'attempts.json');
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-crash-mid-send',
  };

  // Life 1: the send frame goes out and the process dies before any ack —
  // simulated by a sendMediaMessage that never settles.
  const life1Journal = createAttemptJournal({ filePath: journalPath });
  const life1 = makePublisher({
    journal: life1Journal,
    sendMediaMessage: () => new Promise(() => {}),
  });
  publishMedia(life1.publisher, body); // intentionally not awaited: it never settles
  while (!life1Journal.get('key-crash-mid-send')?.sendAttempted) {
    await new Promise((r) => setImmediate(r));
  }

  // Life 2: same journal file.
  const life2 = makePublisher({ journal: createAttemptJournal({ filePath: journalPath }) });
  const retry = await publishMedia(life2.publisher, body);
  assert.equal(retry.statusCode, 502);
  assert.equal(retry.json().failure_kind, 'delivery_unknown');
  assert.equal(life2.calls.length, 0, 'the maybe-displayed media must not be sent again');
});

// Codex jg-d0xr round-2 finding 4: only timeout-shaped errors were treated
// as ambiguous. The pinned SDK rejects already-written pending frames on
// socket loss as "WebSocket connection closed (…), reply for reqId …
// cancelled" — that did not match, sendAttempted was cleared, and a retry
// could display the message twice. Every post-write failure without an
// explicit negative acknowledgement is now delivery-unknown by default.
test('a socket-loss cancellation after the frame was written is delivery-unknown', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  const { publisher } = makePublisher({
    sendMediaMessage: async () => {
      sendAttempts += 1;
      // Verbatim SDK 1.0.7 clearPendingMessages rejection shape.
      throw new Error('WebSocket connection closed (code 1006, reason: ), reply for reqId: SEND_MSG_9 cancelled');
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-socket-loss',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'delivery_unknown');

  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 502);
  assert.equal(retry.json().failure_kind, 'delivery_unknown');
  assert.equal(sendAttempts, 1, 'a frame the socket may have carried must not be re-sent');
});

test('an unrecognized send failure defaults to delivery-unknown, never blind retry', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  const { publisher } = makePublisher({
    sendMediaMessage: async () => {
      sendAttempts += 1;
      throw new Error('something exploded mid-flight');
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-unrecognized',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'delivery_unknown');
  const retry = await publishMedia(publisher, body);
  assert.equal(retry.json().failure_kind, 'delivery_unknown');
  assert.equal(sendAttempts, 1);
});

test('an explicit provider errcode rejection stays retryable', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let sendAttempts = 0;
  const { publisher } = makePublisher({
    sendMediaMessage: async () => {
      sendAttempts += 1;
      if (sendAttempts === 1) {
        // The SDK rejects with the raw ack FRAME on errcode ≠ 0 — a
        // negative acknowledgement: the provider saw it and refused it.
        const frame = { errcode: 95001, errmsg: 'invalid chat' };
        throw frame; // eslint-disable-line no-throw-literal
      }
      return { headers: { req_id: 'MSG_OK' } };
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-nack',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'provider_error');
  assert.match(first.json().error, /errcode 95001/);

  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 200, 'a negative acknowledgement is a definite non-delivery');
  assert.equal(sendAttempts, 2);
});

test('a caption ack timeout is delivery-unknown too — the chunk is not re-sent', async (t) => {
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
      throw new Error('Reply ack timeout (10000ms) for reqId: SEND_MSG_2');
    },
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    text: 'ambiguous caption',
    idempotency_key: 'key-caption-ack-lost',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'delivery_unknown');

  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 502);
  assert.equal(retry.json().failure_kind, 'delivery_unknown');
  assert.equal(mediaSends, 1);
  assert.equal(captionAttempts, 1, 'a maybe-displayed caption chunk must not be repeated');
});

test('a timed-out upload is retained: retries re-await it instead of uploading again', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let uploads = 0;
  let resolveUpload;
  const slowUpload = new Promise((r) => { resolveUpload = r; });
  const { publisher, calls } = makePublisher({
    uploadMedia: async () => {
      uploads += 1;
      return slowUpload;
    },
    // A short real deadline standing in for withDeadline(p, uploadTimeoutMs).
    withUploadDeadline: (p) => Promise.race([
      p,
      new Promise((_, reject) => setTimeout(() => reject(new Error('media upload exceeded the 30ms wall-clock deadline')), 30)),
    ]),
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-upload-retained',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'provider_error', 'an upload timeout is retryable, not delivery-unknown');

  // Retry while the SDK upload is STILL running: it must wait on the same
  // upload, not start a second one.
  const second = await publishMedia(publisher, body);
  assert.equal(second.statusCode, 502);
  assert.equal(uploads, 1, 'a second concurrent upload of the same bytes must never start');

  // The abandoned upload finally succeeds; its media_id is latched.
  resolveUpload({ media_id: 'MEDIA_LATE' });
  await new Promise((r) => setImmediate(r));
  const third = await publishMedia(publisher, body);
  assert.equal(third.statusCode, 200);
  assert.equal(uploads, 1);
  assert.equal(calls.find((c) => c.op === 'sendMediaMessage').mediaId, 'MEDIA_LATE');
});

// --- upload admission under deadline abandonment (round-2 finding 5) -------------------

// Codex jg-d0xr round-2 finding 5: when withUploadDeadline rejected, a
// `finally` released the upload slot even though the retained uploadRun
// was still executing and still owned the media buffer — new keys could
// start more uploads after every deadline, so the claimed global bound
// was false under the exact timeout it was meant to handle. The slot now
// belongs to the uploadRun and frees only when the SDK settles it.
test('a deadline-abandoned upload keeps its admission slot until the SDK settles it', async (t) => {
  const dir = tmpDir(t);
  const fileA = writeFixture(dir, 'a.png', pngBytes);
  const fileB = writeFixture(dir, 'b.png', jpgBytes);
  let uploads = 0;
  let resolveHung;
  const hung = new Promise((r) => { resolveHung = r; });
  const { publisher } = makePublisher({
    cfg: { uploadMaxConcurrent: 1, uploadMaxQueue: 0 },
    uploadMedia: async () => {
      uploads += 1;
      if (uploads === 1) return hung;
      return { media_id: `MEDIA_${uploads}` };
    },
    withUploadDeadline: (p) => Promise.race([
      p,
      new Promise((_, reject) => setTimeout(() => reject(new Error('media upload exceeded the 20ms wall-clock deadline')), 20)),
    ]),
  });

  const first = await publishMedia(publisher, {
    conversation: { conversation_id: 'chat_1' },
    file_path: fileA,
    media_kind: 'image',
    idempotency_key: 'key-hung',
  });
  assert.equal(first.statusCode, 502);
  assert.equal(first.json().failure_kind, 'provider_error');

  // The hung upload still owns the buffer AND the slot: a NEW key must be
  // refused, not admitted to allocate another buffer and upload.
  const newKeyBody = {
    conversation: { conversation_id: 'chat_2' },
    file_path: fileB,
    media_kind: 'image',
    idempotency_key: 'key-after-deadline',
  };
  const second = await publishMedia(publisher, newKeyBody);
  assert.equal(second.statusCode, 429, 'the global upload bound must hold under the exact timeout it exists for');
  assert.equal(uploads, 1, 'no second upload may start while the retained one consumes the slot');

  // When the SDK finally settles the hung upload, the slot frees.
  resolveHung({ media_id: 'MEDIA_LATE' });
  await new Promise((r) => setImmediate(r));
  const third = await publishMedia(publisher, newKeyBody);
  assert.equal(third.statusCode, 200);
  assert.equal(uploads, 2);
});

test('a same-key retry attaches to the retained upload without another slot or file read', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let uploads = 0;
  let resolveHung;
  const hung = new Promise((r) => { resolveHung = r; });
  const { publisher, calls } = makePublisher({
    cfg: { uploadMaxConcurrent: 1, uploadMaxQueue: 0 },
    uploadMedia: async () => {
      uploads += 1;
      return hung;
    },
    withUploadDeadline: (p) => Promise.race([
      p,
      new Promise((_, reject) => setTimeout(() => reject(new Error('media upload exceeded the 20ms wall-clock deadline')), 20)),
    ]),
  });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-attach',
  };

  const first = await publishMedia(publisher, body);
  assert.equal(first.statusCode, 502);

  // Delete the file: the attach-resume needs neither the file nor a gate
  // slot — the retained upload owns the buffer (and, with queue 0 and one
  // slot consumed, acquiring would 429).
  fs.rmSync(file);
  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 502);
  assert.equal(retry.json().failure_kind, 'provider_error',
    'the attach-resume must wait on the retained upload — not 400 on the missing file or 429 on the gate');
  assert.equal(uploads, 1);

  // Late completion latches the media_id; a retry (file restored for the
  // digest check) rides the retained upload's result.
  resolveHung({ media_id: 'MEDIA_LATE' });
  await new Promise((r) => setImmediate(r));
  fs.writeFileSync(file, pngBytes);
  const third = await publishMedia(publisher, body);
  assert.equal(third.statusCode, 200);
  assert.equal(uploads, 1, 'the retained upload is the only upload that ever ran');
  assert.equal(calls.find((c) => c.op === 'sendMediaMessage').mediaId, 'MEDIA_LATE');
});

// --- journal durability: fail closed (round-2 finding 3) ------------------------------

// Codex jg-d0xr round-2 finding 3: journal persistence was fail-open —
// disk-full/permission/rename failures were logged and ignored, after
// which provider writes proceeded as though sendAttempted were durable;
// startup corruption was silently discarded and every key started fresh.
// Either path can re-send media users already saw.
test('a journal write failure stops the send before any provider work (fail closed)', async (t) => {
  const dir = tmpDir(t);
  const journalDir = path.join(dir, 'jdir');
  fs.mkdirSync(journalDir);
  const journalPath = path.join(journalDir, 'attempts.json');
  const journal = createAttemptJournal({ filePath: journalPath, log: () => {} });
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher({ journal });
  // Take the journal directory read-only: the fingerprint latch cannot be
  // made durable.
  fs.chmodSync(journalDir, 0o500);
  t.after(() => { try { fs.chmodSync(journalDir, 0o700); } catch { /* already restored */ } });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-journal-down',
  };

  const res = await publishMedia(publisher, body);
  assert.equal(res.statusCode, 503);
  assert.equal(res.json().failure_kind, 'journal_unavailable');
  assert.equal(res.json().idempotency_key, 'key-journal-down');
  assert.equal(calls.length, 0, 'no provider write may happen without a durable fingerprint');

  // Once the journal is writable again, the SAME key sends cleanly.
  fs.chmodSync(journalDir, 0o700);
  const retry = await publishMedia(publisher, body);
  assert.equal(retry.statusCode, 200);
});

test('a journal failure after upload refuses BEFORE the aibot_send_msg frame goes out', async (t) => {
  const dir = tmpDir(t);
  const journalDir = path.join(dir, 'jdir');
  fs.mkdirSync(journalDir);
  const journalPath = path.join(journalDir, 'attempts.json');
  const journal = createAttemptJournal({ filePath: journalPath, log: () => {} });
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher({
    journal,
    // The journal dies exactly between the upload and the send.
    uploadMedia: async () => {
      fs.chmodSync(journalDir, 0o500);
      return { media_id: 'MEDIA_J' };
    },
  });
  t.after(() => { try { fs.chmodSync(journalDir, 0o700); } catch { /* restored */ } });
  const body = {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-journal-mid',
  };

  const res = await publishMedia(publisher, body);
  assert.equal(res.statusCode, 503);
  assert.equal(res.json().failure_kind, 'journal_unavailable');
  assert.equal(calls.filter((c) => c.op === 'sendMediaMessage').length, 0,
    'sendAttempted must be durable before the frame may go out');

  // The on-disk journal never claimed an attempt, so a fresh life resumes
  // the key cleanly (re-uploading is safe: uploads are invisible).
  fs.chmodSync(journalDir, 0o700);
  const life2 = makePublisher({ journal: createAttemptJournal({ filePath: journalPath, log: () => {} }) });
  const retry = await publishMedia(life2.publisher, body);
  assert.equal(retry.statusCode, 200, JSON.stringify(retry.json()));
});

test('startup corruption quarantines the journal and fails closed instead of starting empty', async (t) => {
  const dir = tmpDir(t);
  const journalPath = path.join(dir, 'attempts.json');
  fs.writeFileSync(journalPath, '{definitely not json');
  const journal = createAttemptJournal({ filePath: journalPath, log: () => {} });
  assert.equal(journal.isDegraded(), true);
  assert.ok(fs.readdirSync(dir).some((f) => f.startsWith('attempts.json.corrupt-')),
    'the corrupt file must be preserved for inspection, not discarded');

  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher, calls } = makePublisher({ journal });
  const res = await publishMedia(publisher, {
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-degraded',
  });
  assert.equal(res.statusCode, 503);
  assert.equal(res.json().failure_kind, 'journal_unavailable');
  assert.match(res.json().error, /degraded/);
  assert.equal(calls.length, 0, 'a degraded journal must refuse media publishes entirely');
});

test('a corrupt main journal recovers from the rotated backup generation', async (t) => {
  const dir = tmpDir(t);
  const journalPath = path.join(dir, 'attempts.json');
  const j1 = createAttemptJournal({ filePath: journalPath, log: () => {} });
  j1.record('key-a', { fingerprint: { digest: 'd1' } });
  j1.record('key-a', { mediaId: 'MEDIA_A' });
  assert.ok(fs.existsSync(`${journalPath}.bak`), 'every persist rotates the previous generation to .bak');

  // External damage to the main file — the previous generation survives.
  fs.writeFileSync(journalPath, 'garbage{{{');
  const j2 = createAttemptJournal({ filePath: journalPath, log: () => {} });
  assert.equal(j2.isDegraded(), false);
  assert.equal(j2.get('key-a')?.fingerprint?.digest, 'd1', 'recovered from the backup generation');
  assert.ok(fs.readdirSync(dir).some((f) => f.startsWith('attempts.json.corrupt-')),
    'the damaged main file is quarantined, not overwritten');
});

test('a crash between the backup rotation and the final rename recovers from the fsynced tmp', async (t) => {
  const dir = tmpDir(t);
  const journalPath = path.join(dir, 'attempts.json');
  const j1 = createAttemptJournal({ filePath: journalPath, log: () => {} });
  j1.record('key-t', { fingerprint: { digest: 'old' } });
  // Simulate the crash window: main was already rotated away, and the
  // fsync'd tmp (the NEWEST generation) never got renamed into place.
  fs.renameSync(journalPath, `${journalPath}.bak`);
  fs.writeFileSync(`${journalPath}.tmp`, JSON.stringify({
    version: 2,
    entries: { 'key-t': { fingerprint: { digest: 'newest' }, mediaId: 'M_TMP' } },
  }));

  const j2 = createAttemptJournal({ filePath: journalPath, log: () => {} });
  assert.equal(j2.isDegraded(), false);
  assert.equal(j2.get('key-t')?.mediaId, 'M_TMP', 'the newest surviving generation wins');
});

// --- seeded receipt pinning (finding 6) ---------------------------------------------

// Codex jg-d0xr finding 6: the just-settled media receipt sat in the
// SHARED publishStates map, evictable under cap pressure while the
// /extmsg/outbound round-trip was still pending. When it got evicted,
// gc's recording callback missed the settled receipt and delivered the
// transcript text as a fresh publish — filenames and host source paths
// visibly sent into the chat. Seeds now live in their own pinned lookup
// for exactly the recording window, and cap pressure REFUSES new keys
// rather than evicting pinned or live entries.
test('cap pressure during the recording round-trip never turns the callback into a fresh publish', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  let publisherRef;
  let pressureRes;
  let callbackRes;
  const { publisher, calls } = makePublisher({
    publishStatesCap: 1,
    postOutbound: async (target, body) => {
      // While recording is pending: (a) an unrelated text publish exerts
      // cap pressure on the state table …
      pressureRes = fakeRes();
      await publisherRef.handlePublish({}, pressureRes, JSON.stringify({
        conversation: { conversation_id: 'li_si' },
        text: 'unrelated cap pressure',
        idempotency_key: 'key-pressure',
      }));
      // … then (b) gc's callback posts the transcript text back through
      // /publish under the media key.
      callbackRes = fakeRes();
      await publisherRef.handlePublish({}, callbackRes, JSON.stringify({
        conversation: { conversation_id: 'zhang_san' },
        text: body.text,
        idempotency_key: body.idempotency_key,
      }));
      return { Receipt: { Delivered: true }, TranscriptEntry: { ID: 'tr-1' } };
    },
  });
  publisherRef = publisher;

  const res = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-pinned-seed',
  });
  assert.equal(res.statusCode, 200);
  assert.equal(res.json().transcript_recorded, true);

  // The pinned seed was NOT evicted, and — round-2 finding 8 — the
  // unrelated TEXT publish is served from its own pool rather than
  // wedged: it no longer contends with the media state at all.
  assert.equal(pressureRes.statusCode, 200);
  // The callback answered straight from the seed: same receipt, no send.
  assert.equal(callbackRes.statusCode, 200);
  assert.equal(callbackRes.json().delivered, true);
  assert.equal(callbackRes.json().message_id, res.json().message_id);
  // The TRANSCRIPT TEXT (filename + host source path) must never reach
  // the chat as a message — that is the finding-6 leak. The unrelated
  // pressure text sending is fine (separate pool, finding 8).
  const transcriptLeaks = calls.filter(
    (c) => c.op === 'sendMessage' && /\[image sent\]/.test(c.body?.markdown?.content ?? ''),
  );
  assert.equal(transcriptLeaks.length, 0,
    'the transcript text (filename + host source path) must never reach the chat as a message');

  // The pin and the seed last exactly the recording window.
  assert.equal(publisher.stats().transcriptSeeds, 0);
  const afterRes = fakeRes();
  await publisher.handlePublish({}, afterRes, JSON.stringify({
    conversation: { conversation_id: 'li_si' },
    text: 'admitted now',
    idempotency_key: 'key-after',
  }));
  assert.equal(afterRes.statusCode, 200, 'settled unpinned entries are evictable again');
});

test('the seed is cleaned up even when recording fails', async (t) => {
  const dir = tmpDir(t);
  const file = writeFixture(dir, 'photo.png', pngBytes);
  const { publisher } = makePublisher({
    postOutbound: async () => { throw new Error('gc unreachable'); },
  });

  const res = await publishMedia(publisher, {
    session_id: 'sess-mayor',
    conversation: { conversation_id: 'zhang_san' },
    file_path: file,
    media_kind: 'image',
    idempotency_key: 'key-seed-cleanup',
  });
  assert.equal(res.statusCode, 200);
  assert.equal(res.json().transcript_recorded, false);
  assert.equal(publisher.stats().transcriptSeeds, 0);
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
  // Wait until the media publish actually claimed the chain (admission is
  // async since the finding-1 confinement walk) before the text lands.
  while (order.length === 0) await new Promise((r) => setImmediate(r));
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
