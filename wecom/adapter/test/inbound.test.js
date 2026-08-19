// inbound.test.js — integration tests for the frame → gc pipeline
// (src/inbound.js), with a fake WS (frames injected directly), a fake
// downloader, and a fake gc (postInbound capture). Covers the wiring the
// media.js unit tests cannot: hydration starting before the conversation
// chain unblocks, concurrent-replay dedup, cleanup after delivery and
// after deterministic rejection, the extmsg POST shape with attachments,
// and text/voice regressions (jg-c7j codex round-1).

import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { test } from 'node:test';
import { fileURLToPath, pathToFileURL } from 'node:url';

import { createInboundPipeline, renderText } from '../src/inbound.js';

const testAesKey = crypto.randomBytes(32).toString('base64');

function tmpMediaDir(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'wecom-inbound-test-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

// makePipeline builds a pipeline against a fake gc: posts[] captures every
// delivered body; postInbound can be overridden to hang or reject.
function makePipeline(t, overrides = {}) {
  const mediaDir = overrides.mediaDir ?? tmpMediaDir(t);
  const posts = [];
  const cfg = {
    cityName: 'jadegate',
    provider: 'wecom',
    botId: 'BOT_1',
    gcAPIBase: 'http://gc.test:9443',
    mediaDir,
    mediaMaxBytes: 1024 * 1024,
    mediaUrlTtlMs: 270000,
    ...overrides.cfg,
  };
  const pipeline = createInboundPipeline({
    cfg,
    log: () => {},
    downloadFile: overrides.downloadFile
      ?? (async () => ({ buffer: Buffer.from('bytes'), filename: 'a.txt' })),
    transcribe: overrides.transcribe ?? null,
    gate: overrides.gate ?? null,
    quota: overrides.quota ?? null,
    postInbound: overrides.postInbound
      ?? (async (target, body) => { posts.push({ target, body }); }),
  });
  return { pipeline, posts, mediaDir, cfg };
}

function frame(msg) {
  return { body: msg };
}

function textMessage(overrides = {}) {
  return {
    msgid: `T_${Math.random().toString(36).slice(2)}`,
    chattype: 'single',
    from: { userid: 'zhang_san' },
    msgtype: 'text',
    text: { content: '你好' },
    create_time: 1755500000,
    ...overrides,
  };
}

function fileMessage(overrides = {}) {
  return {
    msgid: `F_${Math.random().toString(36).slice(2)}`,
    chattype: 'single',
    from: { userid: 'zhang_san' },
    msgtype: 'file',
    file: { url: 'https://wwcdn.example/f1', aeskey: testAesKey },
    create_time: 1755500000,
    ...overrides,
  };
}

// --- regressions: non-media paths stay byte-identical ------------------------

test('a text frame POSTs the unchanged extmsg shape (no attachments key)', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  const msg = textMessage({ msgid: 'T_1' });
  await pipeline.enqueueInbound(frame(msg));
  assert.equal(posts.length, 1);
  assert.equal(posts[0].target, 'http://gc.test:9443/v0/city/jadegate/extmsg/inbound');
  assert.deepEqual(posts[0].body.message, {
    provider_message_id: 'T_1',
    conversation: {
      scope_id: 'jadegate',
      provider: 'wecom',
      account_id: 'BOT_1',
      conversation_id: 'zhang_san',
      kind: 'dm',
    },
    actor: { id: 'zhang_san', display_name: 'zhang_san', is_bot: false },
    text: '你好',
    dedup_key: 'T_1',
    received_at: new Date(1755500000 * 1000).toISOString(),
  });
});

test('a voice frame delivers the server-side transcript and never downloads', async (t) => {
  let downloads = 0;
  const { pipeline, posts } = makePipeline(t, {
    downloadFile: async () => { downloads++; return { buffer: Buffer.from('x') }; },
  });
  await pipeline.enqueueInbound(frame(textMessage({
    msgtype: 'voice',
    voice: { content: '早上好' },
  })));
  assert.equal(downloads, 0);
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.message.text, '[voice] 早上好');
  assert.ok(!('attachments' in posts[0].body.message));
});

test('a group frame keys the conversation by chatid as a room', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(textMessage({ chattype: 'group', chatid: 'wrCHAT_9' })));
  assert.equal(posts[0].body.message.conversation.conversation_id, 'wrCHAT_9');
  assert.equal(posts[0].body.message.conversation.kind, 'room');
});

// --- media wiring -------------------------------------------------------------

test('a file frame POSTs attachments[] with the file:// URL and the block text', async (t) => {
  const { pipeline, posts, mediaDir } = makePipeline(t, {
    downloadFile: async () => ({ buffer: Buffer.from('%PDF-1.7 body'), filename: '季度报告.pdf' }),
  });
  const msg = fileMessage({ msgid: 'F_SHAPE' });
  await pipeline.enqueueInbound(frame(msg));
  assert.equal(posts.length, 1);
  const delivered = posts[0].body.message;
  const dest = path.join(mediaDir, 'zhang_san', 'F_SHAPE-季度报告.pdf');
  assert.deepEqual(delivered.attachments, [
    { provider_id: 'F_SHAPE', url: pathToFileURL(dest).href, mime_type: 'application/pdf' },
  ]);
  assert.equal(fs.readFileSync(fileURLToPath(delivered.attachments[0].url), 'utf8'), '%PDF-1.7 body');
  assert.match(delivered.text, /^\[file message\]\n\[1 WeCom file attached\]/);
  assert.ok(delivered.text.includes(`saved to ${dest}; Read that path to view it`));
});

test('hydration starts immediately even while the conversation chain is blocked', async (t) => {
  // Message A's gc delivery hangs (simulated outage); message B (a file in
  // the SAME conversation) must still download right away — its URL dies
  // in ~5 minutes, long before A's retry loop would let B's bridge run.
  let releaseA;
  const aDelivered = new Promise((r) => { releaseA = r; });
  let downloadStartedResolve;
  const downloadStarted = new Promise((r) => { downloadStartedResolve = r; });
  const posts = [];
  const { pipeline } = makePipeline(t, {
    postInbound: async (target, body) => {
      if (body.message.provider_message_id === 'T_BLOCKER') await aDelivered;
      posts.push(body.message.provider_message_id);
    },
    downloadFile: async () => {
      downloadStartedResolve();
      return { buffer: Buffer.from('img'), filename: 'p.png' };
    },
  });

  const done1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_BLOCKER' })));
  const done2 = pipeline.enqueueInbound(frame(fileMessage({ msgid: 'F_QUEUED' })));

  // The download must begin while T_BLOCKER is still undelivered.
  await downloadStarted;
  assert.deepEqual(posts, []);

  releaseA();
  await done1;
  await done2;
  assert.deepEqual(posts, ['T_BLOCKER', 'F_QUEUED']);
});

test('a replayed frame mid-download reuses the hydration and posts once', async (t) => {
  let downloads = 0;
  let releaseDownload;
  const held = new Promise((r) => { releaseDownload = r; });
  const { pipeline, posts } = makePipeline(t, {
    downloadFile: async () => {
      downloads++;
      await held;
      return { buffer: Buffer.from('img'), filename: 'p.png' };
    },
  });
  const msg = fileMessage({ msgid: 'F_REPLAY' });
  const first = pipeline.enqueueInbound(frame(msg));
  const replay = pipeline.enqueueInbound(frame(msg)); // SDK replay of the same msgid
  releaseDownload();
  await Promise.all([first, replay]);
  assert.equal(downloads, 1, 'replay must not download the bytes twice');
  assert.equal(posts.length, 1, 'replay must not double-post');
  // A LATER replay (after delivery) is dropped by the seen set.
  await pipeline.enqueueInbound(frame(msg));
  assert.equal(posts.length, 1);
  assert.equal(downloads, 1);
});

test('delivery success and deterministic rejection both clean the maps', async (t) => {
  const reject400 = Object.assign(new Error('400 Bad Request: nope'), { status: 400 });
  const { pipeline, posts } = makePipeline(t, {
    postInbound: async (target, body) => {
      if (body.message.provider_message_id === 'F_BAD') throw reject400;
      posts.push(body.message.provider_message_id);
    },
  });

  await pipeline.enqueueInbound(frame(fileMessage({ msgid: 'F_OK' })));
  await pipeline.enqueueInbound(frame(fileMessage({ msgid: 'F_BAD', from: { userid: 'li_si' } })));

  const stats = pipeline.stats();
  assert.equal(stats.hydrations, 0, 'hydration entries must not outlive their bridge');
  assert.equal(stats.inflight, 0);
  assert.equal(stats.chains, 0);
  assert.equal(stats.seen, 2, 'both ids marked seen (rejection is deterministic)');

  // The rejected id never re-posts on replay.
  await pipeline.enqueueInbound(frame(fileMessage({ msgid: 'F_BAD', from: { userid: 'li_si' } })));
  assert.deepEqual(posts, ['F_OK']);
});

test('a failed download still delivers the message with placeholder + note', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    downloadFile: async () => { throw new Error('socket hang up'); },
  });
  await pipeline.enqueueInbound(frame(fileMessage({ msgid: 'F_FAIL' })));
  assert.equal(posts.length, 1);
  const delivered = posts[0].body.message;
  assert.ok(!('attachments' in delivered));
  assert.match(delivered.text, /^\[file message\]/);
  assert.ok(delivered.text.includes('download failed'));
});

test('messages in different conversations bridge concurrently, same conversation in order', async (t) => {
  const order = [];
  let releaseFirst;
  const firstHeld = new Promise((r) => { releaseFirst = r; });
  const { pipeline } = makePipeline(t, {
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id === 'T_A1') await firstHeld;
      order.push(id);
    },
  });
  const a1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_A1', from: { userid: 'alice' } })));
  const a2 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_A2', from: { userid: 'alice' } })));
  const b1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_B1', from: { userid: 'bob' } })));
  await b1; // bob's conversation is not blocked behind alice's
  assert.deepEqual(order, ['T_B1']);
  releaseFirst();
  await Promise.all([a1, a2]);
  assert.deepEqual(order, ['T_B1', 'T_A1', 'T_A2']);
});

test('renderText regression table', () => {
  assert.equal(renderText({ msgtype: 'text', text: { content: 'hi' } }), 'hi');
  assert.equal(renderText({ msgtype: 'voice', voice: { content: '转写' } }), '[voice] 转写');
  assert.equal(renderText({ msgtype: 'voice', voice: {} }), '[voice message]');
  assert.equal(renderText({ msgtype: 'image' }), '[image message]');
  assert.equal(renderText({ msgtype: 'file' }), '[file message]');
  assert.equal(renderText({ msgtype: 'video' }), '[video message]');
  assert.equal(renderText({
    msgtype: 'mixed',
    mixed: { msg_item: [{ msgtype: 'text', text: { content: '两张' } }, { msgtype: 'image' }] },
  }), '两张[image]');
  assert.equal(renderText({ msgtype: 'sticker' }), '[sticker message]');
});
