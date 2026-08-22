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
    // The suite pins exact envelopes; the once-per-conversation reply
    // how-to (production default ON) is exercised by its own tests.
    replyHelpOnce: false,
    ...(overrides.deps ?? {}),
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

// --- codex round-2: backlog admission instead of live-entry eviction ---------

test('a full backlog refuses NEW media and never evicts live entries — replays stay single-download', async (t) => {
  // Simulated gc outage: deliveries for H_1/H_2 hang, keeping both bridges
  // (and their hydration entries) live. Cap = 2 for the test (prod 512).
  // Codex's round-1 probe: at cap, the oldest LIVE entry was evicted, so
  // replaying it re-downloaded every attachment (513 msgs -> 1,026
  // downloads). Now the cap refuses NEW admissions instead.
  let releaseOutage;
  const outage = new Promise((r) => { releaseOutage = r; });
  const posts = [];
  let downloads = 0;
  const mediaDir = tmpMediaDir(t);
  const p = createInboundPipeline({
    cfg: {
      cityName: 'jadegate', provider: 'wecom', botId: 'BOT_1',
      gcAPIBase: 'http://gc.test:9443', mediaDir,
      mediaMaxBytes: 1024 * 1024, mediaUrlTtlMs: 270000,
    },
    log: () => {},
    downloadFile: async () => {
      downloads++;
      return { buffer: Buffer.from('img'), filename: 'p.png' };
    },
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id === 'H_1' || id === 'H_2') await outage;
      posts.push({ id, text: body.message.text });
    },
    hydrationsCap: 2,
  });

  const h1 = fileMessage({ msgid: 'H_1', from: { userid: 'u1' } });
  const h2 = fileMessage({ msgid: 'H_2', from: { userid: 'u2' } });
  const b1 = p.enqueueInbound(frame(h1));
  const b2 = p.enqueueInbound(frame(h2));
  // Let both hydrations complete (downloads done, bridges hung in outage).
  for (let i = 0; i < 200 && downloads < 2; i++) await new Promise((r) => setTimeout(r, 5));
  assert.equal(downloads, 2);
  assert.equal(p.stats().hydrations, 2, 'both live entries retained');

  // Cap reached: a THIRD media message is refused ingestion (no eviction,
  // no download) but still delivers, with the backlog note.
  await p.enqueueInbound(frame(fileMessage({ msgid: 'H_3', from: { userid: 'u3' } })));
  assert.equal(downloads, 2, 'backlog-refused message must not download');
  assert.equal(p.stats().hydrations, 2, 'live entries were NOT evicted for the new message');
  const h3 = posts.find((x) => x.id === 'H_3');
  assert.ok(h3.text.includes('hydration backlog is full'));
  assert.ok(h3.text.includes('re-send'));

  // Replaying the LIVE messages mid-outage reuses their hydrations: the
  // round-1 bug would have re-downloaded here.
  p.enqueueInbound(frame(h1));
  p.enqueueInbound(frame(h2));
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(downloads, 2, 'replays of live entries must not re-download');

  // Outage ends: bridges settle, entries drain, and NEW media is admitted
  // (and downloaded) again.
  releaseOutage();
  await Promise.all([b1, b2]);
  assert.equal(p.stats().hydrations, 0);
  assert.deepEqual(posts.map((x) => x.id).sort(), ['H_1', 'H_2', 'H_3']);
  assert.equal(posts.filter((x) => x.id === 'H_1').length, 1, 'single POST per message');
  await p.enqueueInbound(frame(fileMessage({ msgid: 'H_4', from: { userid: 'u4' } })));
  assert.equal(downloads, 3, 'admission reopens once the backlog drains');
});

// --- codex round-3: queued-bridge sweep protection, sticky refusals ----------

test('the TTL sweep never evicts a hydration whose bridge is queued behind another delivery', async (t) => {
  // Msg A (text) hangs in delivery, blocking the conversation chain. Msg B
  // (media, SAME conversation) hydrates immediately but its bridge is
  // QUEUED — inflight is not set for it yet. The fake clock then jumps
  // past the TTL and another message triggers a sweep: B's entry must
  // survive (pending ownership opened at enqueue), so B's replay reuses
  // it instead of re-downloading.
  let clock = 1_000_000_000;
  let downloads = 0;
  let releaseA;
  const aHeld = new Promise((r) => { releaseA = r; });
  const posts = [];
  const mediaDir = tmpMediaDir(t);
  const p = createInboundPipeline({
    cfg: {
      cityName: 'jadegate', provider: 'wecom', botId: 'BOT_1',
      gcAPIBase: 'http://gc.test:9443', mediaDir,
      mediaMaxBytes: 1024 * 1024, mediaUrlTtlMs: 270000,
    },
    log: () => {},
    now: () => clock,
    downloadFile: async () => {
      downloads++;
      return { buffer: Buffer.from('img'), filename: 'p.png' };
    },
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id === 'T_A') await aHeld;
      posts.push(id);
    },
  });

  const msgB = fileMessage({ msgid: 'F_B', from: { userid: 'zhang_san' } });
  const bridgeA = p.enqueueInbound(frame(textMessage({ msgid: 'T_A', from: { userid: 'zhang_san' } })));
  const bridgeB = p.enqueueInbound(frame(msgB));
  for (let i = 0; i < 200 && downloads < 1; i++) await new Promise((r) => setTimeout(r, 5));
  assert.equal(downloads, 1);
  assert.equal(p.stats().hydrations, 1);

  // 31 minutes pass (past the 30min TTL); a new media message triggers the
  // sweep on its admission path.
  clock += 31 * 60 * 1000;
  await p.enqueueInbound(frame(fileMessage({ msgid: 'F_OTHER', from: { userid: 'li_si' } })));
  assert.equal(p.stats().hydrations, 1, "B's queued-live entry must survive the sweep");

  // Replay B mid-outage: must reuse the surviving hydration.
  const replayB = p.enqueueInbound(frame(msgB));
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(downloads, 2, 'replay must not re-download (1 = B, 2 = F_OTHER)');

  releaseA();
  await Promise.all([bridgeA, bridgeB, replayB]);
  assert.deepEqual(posts, ['F_OTHER', 'T_A', 'F_B']);
  assert.equal(p.stats().hydrations, 0);
  assert.equal(p.stats().pending, 0);
});

test('a refused msgid replays into the SAME refusal after capacity clears — no download, no leak', async (t) => {
  // M_FULL (media) hangs in delivery and holds the whole cap (1). F_REF
  // (media, msgid M) arrives behind a hanging same-conversation message:
  // refused, refusal note queued. Capacity then clears (M_FULL delivers)
  // while F_REF's bridge is STILL queued — a replay of M must get the
  // cached refusal, not a fresh hydration whose download the queued
  // refusal note never delivers (and whose entry then leaks).
  let downloads = 0;
  const posts = [];
  let releaseFull;
  const fullHeld = new Promise((r) => { releaseFull = r; });
  let releaseHold;
  const holdHeld = new Promise((r) => { releaseHold = r; });
  const mediaDir = tmpMediaDir(t);
  const p = createInboundPipeline({
    cfg: {
      cityName: 'jadegate', provider: 'wecom', botId: 'BOT_1',
      gcAPIBase: 'http://gc.test:9443', mediaDir,
      mediaMaxBytes: 1024 * 1024, mediaUrlTtlMs: 270000,
    },
    log: () => {},
    downloadFile: async () => {
      downloads++;
      return { buffer: Buffer.from('img'), filename: 'p.png' };
    },
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id === 'M_FULL') await fullHeld;
      if (id === 'T_HOLD') await holdHeld;
      posts.push({ id, text: body.message.text });
    },
    hydrationsCap: 1,
  });

  // Fill the cap with a live entry.
  const bridgeFull = p.enqueueInbound(frame(fileMessage({ msgid: 'M_FULL', from: { userid: 'u9' } })));
  for (let i = 0; i < 200 && downloads < 1; i++) await new Promise((r) => setTimeout(r, 5));
  assert.equal(p.stats().hydrations, 1);

  // Block u1's chain, then enqueue the media message that gets refused.
  const bridgeHold = p.enqueueInbound(frame(textMessage({ msgid: 'T_HOLD', from: { userid: 'u1' } })));
  const refMsg = fileMessage({ msgid: 'M_REF', from: { userid: 'u1' } });
  const bridgeRef = p.enqueueInbound(frame(refMsg));
  assert.equal(downloads, 1, 'refused message must not download');
  assert.equal(p.stats().refusals, 1);

  // Capacity clears while the refusal bridge is still queued behind T_HOLD.
  releaseFull();
  await bridgeFull;
  assert.equal(p.stats().hydrations, 0, 'capacity has cleared');

  // Replay of the refused msgid: must get the SAME cached refusal.
  const replayRef = p.enqueueInbound(frame(refMsg));
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(downloads, 1, 'replay of a refused msgid must not be admitted while pending');
  assert.equal(p.stats().hydrations, 0, 'no replacement hydration to leak');

  // Unblock the chain: the refusal note delivers exactly once; nothing leaks.
  releaseHold();
  await Promise.all([bridgeHold, bridgeRef, replayRef]);
  const refPosts = posts.filter((x) => x.id === 'M_REF');
  assert.equal(refPosts.length, 1);
  assert.ok(refPosts[0].text.includes('hydration backlog is full'));
  assert.equal(p.stats().hydrations, 0);
  assert.equal(p.stats().refusals, 0, 'refusal cache drains with the pending lifetime');
  assert.equal(p.stats().pending, 0);
  assert.equal(downloads, 1, 'the refused media was never downloaded');

  // Once nothing is pending for M_REF... a fresh frame re-evaluates
  // admission (capacity is free now) — but M_REF was delivered (seen), so
  // a late replay is simply dropped.
  await p.enqueueInbound(frame(refMsg));
  assert.equal(downloads, 1);
  assert.equal(posts.filter((x) => x.id === 'M_REF').length, 1);
});

// --- codex round-4: seen-set churn, setup-throw and bridge-throw hygiene ------

test('2,048-message churn cannot evict a pending msgid from the seen set (no double POST)', async (t) => {
  // M is DELIVERED (seen), then a replay of M queues behind a hanging
  // same-conversation delivery (pending). 2,100 unique deliveries then
  // churn the seen set past its 2,048 cap: the round-3 FIFO would evict M,
  // and the queued replay bridge would POST M a second time (gc does not
  // consume dedup_key). Pending msgids must survive eviction; the set
  // re-trims once M's last pending frame settles.
  const posts = [];
  let releaseHold;
  const holdHeld = new Promise((r) => { releaseHold = r; });
  const { pipeline } = makePipeline(t, {
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id === 'T_HOLD') await holdHeld;
      posts.push(id);
    },
  });

  // Deliver M, then block its conversation and queue a replay of M.
  const m = textMessage({ msgid: 'M_KEEP', from: { userid: 'u1' } });
  await pipeline.enqueueInbound(frame(m));
  assert.deepEqual(posts, ['M_KEEP']);
  const holdBridge = pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_HOLD', from: { userid: 'u1' } })));
  const replayBridge = pipeline.enqueueInbound(frame(m));

  // Churn: 2,100 unique messages across other conversations, all delivered.
  const churn = [];
  for (let i = 0; i < 2100; i++) {
    churn.push(pipeline.enqueueInbound(frame(textMessage({
      msgid: `C_${i}`,
      from: { userid: `churn_${i}` },
    }))));
  }
  await Promise.all(churn);
  // Pending msgids are exempt from the cap: 2,048 non-pending marks plus
  // the retained M_KEEP — one above the cap, M_KEEP kept.
  assert.equal(pipeline.stats().seen, 2049);

  // Unblock: the queued replay must be dropped by the retained seen mark.
  releaseHold();
  await Promise.all([holdBridge, replayBridge]);
  assert.equal(posts.filter((id) => id === 'M_KEEP').length, 1, 'exactly one POST for the churned msgid');
  // Re-trim on the replay's settle: M_KEEP (now the oldest non-pending
  // mark) rolls off and the set returns to its cap — the standard FIFO
  // tradeoff for ids older than 2,048 newer deliveries.
  assert.equal(pipeline.stats().seen, 2048);
  assert.equal(pipeline.stats().pending, 0);
});

test('pending marks hold the seen set above cap and re-trim on the last settle', async (t) => {
  // Tiny-cap variant pinning the two halves of the r4 fix numerically:
  // (1) when EVERY retained entry is pending, the set exceeds the cap
  // rather than evicting one; (2) the moment their last pending frames
  // settle, removePending re-trims back to the cap.
  const posts = [];
  const holds = new Map(); // msgid → release
  const { pipeline } = makePipeline(t, {
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (holds.has(id)) await new Promise((r) => { const prev = holds.get(id); holds.set(id, () => { prev?.(); r(); }); });
      posts.push(id);
    },
  });
  // Rebuild with cap 2 (makePipeline has no knob for it).
  let posts2 = [];
  const holdsHeld = new Map(); // holder msgid → resolve
  const p = createInboundPipeline({
    cfg: {
      cityName: 'jadegate', provider: 'wecom', botId: 'BOT_1',
      gcAPIBase: 'http://gc.test:9443', mediaDir: tmpMediaDir(t),
      mediaMaxBytes: 1024 * 1024, mediaUrlTtlMs: 270000,
    },
    log: () => {},
    downloadFile: async () => ({ buffer: Buffer.from('x'), filename: 'p.png' }),
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id.startsWith('HOLD_')) await new Promise((r) => holdsHeld.set(id, r));
      posts2.push(id);
    },
    seenMsgIdCap: 2,
  });

  // Deliver A and B (seen = 2 = cap), then block both conversations and
  // queue a replay of each (both msgids pending).
  const a = textMessage({ msgid: 'A', from: { userid: 'ua' } });
  const b = textMessage({ msgid: 'B', from: { userid: 'ub' } });
  await p.enqueueInbound(frame(a));
  await p.enqueueInbound(frame(b));
  const holdA = p.enqueueInbound(frame(textMessage({ msgid: 'HOLD_A', from: { userid: 'ua' } })));
  const holdB = p.enqueueInbound(frame(textMessage({ msgid: 'HOLD_B', from: { userid: 'ub' } })));
  const replayA = p.enqueueInbound(frame(a));
  const replayB = p.enqueueInbound(frame(b));

  // A third delivery overflows the cap — but A and B are both pending, so
  // the set grows past the cap instead of evicting either.
  await p.enqueueInbound(frame(textMessage({ msgid: 'C', from: { userid: 'uc' } })));
  assert.equal(p.stats().seen, 3, 'all-pending retention holds the set above the cap');

  // Drain: the replays are dropped by the retained marks (single POST per
  // msgid) and the re-trim on their last settle brings the set back to cap.
  holdsHeld.get('HOLD_A')();
  holdsHeld.get('HOLD_B')();
  await Promise.all([holdA, holdB, replayA, replayB]);
  assert.equal(posts2.filter((id) => id === 'A').length, 1);
  assert.equal(posts2.filter((id) => id === 'B').length, 1);
  assert.equal(p.stats().seen, 2, 're-trim on pending drain restores the cap');
  assert.equal(p.stats().pending, 0);
});

test('mixed-order eviction: cap governs the non-pending count, not the front slot', async (t) => {
  // Codex r5 probe ordering: [A(non-pending), B(pending)] + C with cap 2.
  // The r4 fast path evicted A because it sat at the front and was
  // non-pending — although the non-pending population (A, C) was exactly
  // at the cap — and A's replay then double-POSTed (posts=[A,B,C,A]).
  const posts = [];
  const holdsHeld = new Map();
  const p = createInboundPipeline({
    cfg: {
      cityName: 'jadegate', provider: 'wecom', botId: 'BOT_1',
      gcAPIBase: 'http://gc.test:9443', mediaDir: tmpMediaDir(t),
      mediaMaxBytes: 1024 * 1024, mediaUrlTtlMs: 270000,
    },
    log: () => {},
    downloadFile: async () => ({ buffer: Buffer.from('x'), filename: 'p.png' }),
    postInbound: async (target, body) => {
      const id = body.message.provider_message_id;
      if (id.startsWith('HOLD_')) await new Promise((r) => holdsHeld.set(id, r));
      posts.push(id);
    },
    seenMsgIdCap: 2,
  });

  // A delivered and fully settled — the oldest NON-PENDING mark.
  const a = textMessage({ msgid: 'A', from: { userid: 'ua' } });
  await p.enqueueInbound(frame(a));
  // B delivered, then kept PENDING by a replay queued behind a held
  // delivery in its conversation.
  const b = textMessage({ msgid: 'B', from: { userid: 'ub' } });
  await p.enqueueInbound(frame(b));
  const holdB = p.enqueueInbound(frame(textMessage({ msgid: 'HOLD_B', from: { userid: 'ub' } })));
  const replayB = p.enqueueInbound(frame(b));

  // C arrives: non-pending population is (A, C) = cap — nothing may evict.
  await p.enqueueInbound(frame(textMessage({ msgid: 'C', from: { userid: 'uc' } })));
  assert.equal(p.stats().seen, 3, 'A retained although it sits at the front');
  // The double-POST from the r5 probe must not reproduce.
  await p.enqueueInbound(frame(a));
  assert.deepEqual(posts.filter((id) => id === 'A'), ['A'], 'replay of A deduped');

  // D pushes the non-pending population to (A, C, D) > cap: A — the
  // oldest NON-PENDING — is the one evicted; pending B is untouchable.
  await p.enqueueInbound(frame(textMessage({ msgid: 'D', from: { userid: 'ud' } })));
  assert.equal(p.stats().seen, 3, 'A evicted; B(pending)+C+D remain');

  // Drain B's conversation: its replay dedups (single POST), B re-enters
  // cap accounting on settle and rolls off via the cursor rewind; the set
  // converges to the cap.
  holdsHeld.get('HOLD_B')();
  await Promise.all([holdB, replayB]);
  assert.equal(posts.filter((id) => id === 'B').length, 1, 'single POST for B');
  assert.equal(p.stats().seen, 2, 'set converges to cap after all settles');
  assert.equal(p.stats().pending, 0);
});

test('a synchronous setup throw releases the pending refcount and rethrows', async (t) => {
  // A throwing clock makes startHydration (sweep timestamp) blow up
  // synchronously inside enqueueInbound — before the bridge exists, so no
  // settled bridge would ever decrement the refcount.
  const mediaDir = tmpMediaDir(t);
  let calls = 0;
  const p = createInboundPipeline({
    cfg: {
      cityName: 'jadegate', provider: 'wecom', botId: 'BOT_1',
      gcAPIBase: 'http://gc.test:9443', mediaDir,
      mediaMaxBytes: 1024 * 1024, mediaUrlTtlMs: 270000,
    },
    log: () => {},
    now: () => {
      calls++;
      if (calls === 1) throw new Error('clock backend unavailable');
      return Date.now();
    },
    downloadFile: async () => ({ buffer: Buffer.from('img'), filename: 'p.png' }),
    postInbound: async () => {},
  });
  assert.throws(
    () => p.enqueueInbound(frame(fileMessage({ msgid: 'F_THROW' }))),
    /clock backend unavailable/,
  );
  assert.equal(p.stats().pending, 0, 'setup failure must not leak the pending mark');
  // The pipeline still works afterwards.
  await p.enqueueInbound(frame(fileMessage({ msgid: 'F_AFTER' })));
  assert.equal(p.stats().pending, 0);
  assert.equal(p.stats().hydrations, 0);
});

test('an exception inside the bridge body still cleans the owner hydration entry', async (t) => {
  // create_time 1e15 seconds is JSON-valid but 1e18 ms is outside the
  // Date range: toISOString() throws AFTER hydration was consumed and
  // BEFORE the POST block — only a function-wide finally reaches it.
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(fileMessage({
    msgid: 'F_BADTIME',
    create_time: 1e15,
  })));
  assert.equal(posts.length, 0, 'the malformed frame was dropped, not delivered');
  const stats = pipeline.stats();
  assert.equal(stats.hydrations, 0, 'owner entry must not outlive the throwing bridge');
  assert.equal(stats.pending, 0);
  assert.equal(stats.inflight, 0);
});

// --- empty-payload surfacing (jg-p1mk item 2) --------------------------------

test('a voice frame with no transcript delivers an explicit empty-payload marker, loudly logged', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, { deps: { log: (...a) => logs.push(a.join(' ')) } });
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'V_EMPTY',
    msgtype: 'voice',
    voice: {},
  })));
  assert.equal(posts.length, 1, 'the message still delivers — never dropped');
  const text = posts[0].body.message.text;
  assert.match(text, /^\[voice message\]\n\[voice payload empty — 语音转写失败\/内容缺失/);
  assert.equal(logs.filter((l) => l.includes('EMPTY PAYLOAD')).length, 1);
});

test('a media frame with no download URL delivers an explicit empty-payload marker', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, { deps: { log: (...a) => logs.push(a.join(' ')) } });
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'I_EMPTY',
    msgtype: 'image',
    image: {},
  })));
  assert.equal(posts.length, 1);
  assert.match(posts[0].body.message.text, /^\[image message\]\n\[image payload empty — 内容缺失/);
  assert.equal(logs.filter((l) => l.includes('EMPTY PAYLOAD')).length, 1);
});

test('a mixed frame whose image items carry no URL surfaces the missing count', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'M_EMPTY',
    msgtype: 'mixed',
    mixed: { msg_item: [{ msgtype: 'text', text: { content: '看这个' } }, { msgtype: 'image', image: {} }] },
  })));
  assert.equal(posts.length, 1);
  assert.match(posts[0].body.message.text, /\[1 image\(s\) in this mixed message carried no download URL — 内容缺失/);
});

test('a healthy voice frame gains no marker (regression)', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, { deps: { log: (...a) => logs.push(a.join(' ')) } });
  await pipeline.enqueueInbound(frame(textMessage({
    msgtype: 'voice',
    voice: { content: '在路上' },
  })));
  assert.equal(posts[0].body.message.text, '[voice] 在路上');
  assert.equal(logs.filter((l) => l.includes('EMPTY PAYLOAD')).length, 0);
});

test('a text frame rendering to nothing is dropped WITH a log line, never silently', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, { deps: { log: (...a) => logs.push(a.join(' ')) } });
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_BLANK', text: { content: '' } })));
  assert.equal(posts.length, 0);
  assert.equal(logs.filter((l) => l.includes('EMPTY PAYLOAD') && l.includes('T_BLANK')).length, 1);
});

// --- feedback events (jg-mlfs) -----------------------------------------------

function feedbackEvent(overrides = {}, fb = {}) {
  return {
    msgid: `E_${Math.random().toString(36).slice(2)}`,
    chattype: 'single',
    from: { userid: 'afik' },
    msgtype: 'event',
    create_time: 1755500000,
    event: { eventtype: 'feedback_event', feedback_event: { id: 'fb-abc123.0', type: 1, ...fb } },
    ...overrides,
  };
}

test('a 👍 feedback event forwards as a lightweight signal with the feedback id', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(feedbackEvent({ msgid: 'E_UP' })));
  assert.equal(posts.length, 1);
  const m = posts[0].body.message;
  assert.equal(m.text, '[user feedback] 👍 praise from afik on bot reply feedback_id=fb-abc123.0');
  assert.equal(m.conversation.conversation_id, 'afik');
  assert.equal(m.dedup_key, 'E_UP');
});

test('a 👎 feedback event carries the reasons and the free-text criticism', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(feedbackEvent({}, {
    type: 2,
    content: '能再详细一些么',
    inaccurate_reason_list: [2, 4],
  })));
  const text = posts[0].body.message.text;
  assert.match(text, /👎 negative from afik/);
  assert.match(text, /incomplete information, data\/analysis problems/);
  assert.match(text, /能再详细一些么/);
});

test('a feedback withdrawal renders as withdrawn', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(feedbackEvent({}, { type: 3 })));
  assert.match(posts[0].body.message.text, /feedback withdrawn from afik/);
});

test('a replayed feedback event dedups on the event msgid', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  const evt = feedbackEvent({ msgid: 'E_DUP' });
  await pipeline.enqueueInbound(frame(evt));
  await pipeline.enqueueInbound(frame(evt));
  assert.equal(posts.length, 1);
});

// --- inbound burst coalescing (jg-p1mk item 3) -------------------------------

test('two same-chat texts inside the window deliver as ONE combined inbound', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 40 } });
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'C_1', text: { content: '好的' } })));
  const p2 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'C_2', text: { content: '明天上午到' }, create_time: 1755500005 })));
  assert.equal(posts.length, 0, 'nothing delivers before the window closes');
  await Promise.all([p1, p2]);
  assert.equal(posts.length, 1);
  const m = posts[0].body.message;
  assert.equal(m.text, [
    '[2 WeCom messages coalesced, in arrival order]',
    'zhang_san: 好的',
    'zhang_san: 明天上午到',
  ].join('\n'));
  assert.equal(m.provider_message_id, 'C_2');
  assert.equal(m.dedup_key, 'wecom-batch-C_1-C_2-2');
  assert.equal(m.received_at, new Date(1755500005 * 1000).toISOString());
  assert.equal(m.actor.id, 'zhang_san');
});

test('a lone message in the window delivers byte-identical to the immediate path', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 20 } });
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'T_SOLO' })));
  assert.equal(posts.length, 1);
  assert.deepEqual(posts[0].body.message, {
    provider_message_id: 'T_SOLO',
    conversation: {
      scope_id: 'jadegate',
      provider: 'wecom',
      account_id: 'BOT_1',
      conversation_id: 'zhang_san',
      kind: 'dm',
    },
    actor: { id: 'zhang_san', display_name: 'zhang_san', is_bot: false },
    text: '你好',
    dedup_key: 'T_SOLO',
    received_at: new Date(1755500000 * 1000).toISOString(),
  });
});

test('different conversations never share a batch', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 30 } });
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'D_1', from: { userid: 'li_si' } })));
  const p2 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'D_2', from: { userid: 'wang_wu' } })));
  await Promise.all([p1, p2]);
  assert.equal(posts.length, 2);
  const convs = posts.map((p) => p.body.message.conversation.conversation_id).sort();
  assert.deepEqual(convs, ['li_si', 'wang_wu']);
});

test('media and text in one batch stay in arrival order with attachments concatenated', async (t) => {
  const { pipeline, posts, mediaDir } = makePipeline(t, {
    deps: { coalesceWindowMs: 40 },
    downloadFile: async () => ({ buffer: Buffer.from('%PDF-1.7 x'), filename: 'r.pdf' }),
  });
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'X_1', text: { content: '文件来了' } })));
  const p2 = pipeline.enqueueInbound(frame(fileMessage({ msgid: 'X_2' })));
  await Promise.all([p1, p2]);
  assert.equal(posts.length, 1);
  const m = posts[0].body.message;
  const lines = m.text.split('\n');
  assert.equal(lines[0], '[2 WeCom messages coalesced, in arrival order]');
  assert.equal(lines[1], 'zhang_san: 文件来了');
  assert.equal(lines[2], 'zhang_san: [file message]');
  assert.match(m.text, /\[1 WeCom file attached\]/);
  const dest = path.join(mediaDir, 'zhang_san', 'X_2-r.pdf');
  assert.deepEqual(m.attachments, [
    { provider_id: 'X_2', url: pathToFileURL(dest).href, mime_type: 'application/pdf' },
  ]);
});

test('a full buffer flushes early instead of waiting out the window', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    deps: { coalesceWindowMs: 60_000, coalesceMaxBatch: 3 },
  });
  const all = [
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'B_1' }))),
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'B_2' }))),
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'B_3' }))),
  ];
  await Promise.all(all);
  assert.equal(posts.length, 1, 'the cap flushed the batch with a 60s window still open');
  assert.equal(posts[0].body.message.dedup_key, 'wecom-batch-B_1-B_3-3');
});

test('a replayed msgid inside the window collapses to one copy in the batch', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 40 } });
  const msg = textMessage({ msgid: 'R_1' });
  const p1 = pipeline.enqueueInbound(frame(msg));
  const p2 = pipeline.enqueueInbound(frame(msg)); // SDK replay in-window
  const p3 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'R_2', text: { content: '第二条' } })));
  await Promise.all([p1, p2, p3]);
  assert.equal(posts.length, 1);
  const text = posts[0].body.message.text;
  assert.equal(text.split('\n').length, 3, 'header + exactly two member lines');
  assert.equal(posts[0].body.message.dedup_key, 'wecom-batch-R_1-R_2-2');
});

test('flushAll drains an open buffer immediately (shutdown path)', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 60_000 } });
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'S_1' })));
  assert.equal(posts.length, 0);
  await pipeline.flushAll();
  await p1;
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.message.provider_message_id, 'S_1');
});

test('batches flush in order behind an earlier in-flight delivery (chain preserved)', async (t) => {
  let releaseFirst;
  const firstGate = new Promise((r) => { releaseFirst = r; });
  const order = [];
  const { pipeline } = makePipeline(t, {
    deps: { coalesceWindowMs: 30 },
    postInbound: async (target, body) => {
      if (body.message.provider_message_id === 'O_1') await firstGate;
      order.push(body.message.provider_message_id);
    },
  });
  // Batch 1 (O_1) flushes and its POST hangs; batch 2 (O_2) flushes into
  // the chain behind it and must not overtake.
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'O_1' })));
  await new Promise((r) => setTimeout(r, 50)); // window 1 closes, POST in flight
  const p2 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'O_2', text: { content: '后来的' } })));
  await new Promise((r) => setTimeout(r, 50)); // window 2 closes while O_1 still hangs
  assert.deepEqual(order, []);
  releaseFirst();
  await Promise.all([p1, p2]);
  assert.deepEqual(order, ['O_1', 'O_2']);
});

test('a batch member already delivered is skipped; the rest still deliver', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 30 } });
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'K_1' }))); // delivered solo
  assert.equal(posts.length, 1);
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'K_1' }))); // late replay
  const p2 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'K_2', text: { content: '新消息' } })));
  await Promise.all([p1, p2]);
  assert.equal(posts.length, 2);
  assert.equal(posts[1].body.message.text, '新消息', 'the replay collapsed; the fresh message delivered alone');
  assert.equal(posts[1].body.message.dedup_key, 'K_2');
});

test('pipeline state drains clean after coalesced deliveries', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 25 } });
  await Promise.all([
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'Z_1' }))),
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'Z_2' }))),
    pipeline.enqueueInbound(frame(fileMessage({ msgid: 'Z_3' }))),
  ]);
  assert.equal(posts.length, 1);
  const stats = pipeline.stats();
  assert.equal(stats.pending, 0);
  assert.equal(stats.hydrations, 0);
  assert.equal(stats.inflight, 0);
  assert.equal(stats.buffered, 0);
  assert.equal(stats.chains, 0);
});

test('a hostile member (throwing create_time) drops alone; batch neighbors still deliver', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, {
    deps: { coalesceWindowMs: 30, log: (...a) => logs.push(a.join(' ')) },
  });
  const p1 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'H_1', text: { content: '正常消息' } })));
  const p2 = pipeline.enqueueInbound(frame(textMessage({ msgid: 'H_BAD', create_time: 1e15 })));
  await Promise.all([p1, p2]);
  assert.equal(posts.length, 1, 'the healthy neighbor still delivered');
  assert.equal(posts[0].body.message.text, '正常消息');
  assert.equal(posts[0].body.message.dedup_key, 'H_1');
  assert.equal(logs.filter((l) => l.includes('H_BAD') && l.includes('dropped from batch')).length, 1);
  const stats = pipeline.stats();
  assert.equal(stats.pending, 0);
  assert.equal(stats.hydrations, 0);
});

// --- once-per-conversation reply how-to (jg-p1mk add D) ------------------------

test('the full reply how-to rides the first delivery per conversation, then never again', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { replyHelpOnce: true } });
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_1' })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_2', text: { content: '第二条' } })));
  assert.equal(posts.length, 2);
  const first = posts[0].body.message.text;
  assert.match(first, /^你好\n\n\[conversation zhang_san — full reply how-to, sent once per chat per adapter session\]/);
  assert.match(first, /gc wecom publish --chat zhang_san --text-file <path>/);
  assert.match(first, /WECOM_OUTBOUND_MEDIA_ROOT/);
  assert.equal(posts[1].body.message.text, '第二条', 'second delivery carries no help block');
});

test('each conversation gets its own help block; a rejected first delivery re-arms it', async (t) => {
  let rejectNext = true;
  const posts = [];
  const { pipeline } = makePipeline(t, {
    deps: { replyHelpOnce: true },
    postInbound: async (target, body) => {
      if (rejectNext) {
        rejectNext = false;
        const err = new Error('400 Bad Request');
        err.status = 400;
        throw err;
      }
      posts.push(body.message);
    },
  });
  // First delivery for li_si is deterministically rejected — the session
  // never saw the help, so the next delivery must carry it again.
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_A', from: { userid: 'li_si' } })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_B', from: { userid: 'li_si' }, text: { content: '再来' } })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_C', from: { userid: 'wang_wu' } })));
  assert.equal(posts.length, 2);
  assert.match(posts[0].text, /conversation li_si — full reply how-to/);
  assert.match(posts[1].text, /conversation wang_wu — full reply how-to/);
});

test('a coalesced first delivery appends the help block once, after the batch', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { replyHelpOnce: true, coalesceWindowMs: 30 } });
  await Promise.all([
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_X1', text: { content: '一' } }))),
    pipeline.enqueueInbound(frame(textMessage({ msgid: 'RH_X2', text: { content: '二' } }))),
  ]);
  assert.equal(posts.length, 1);
  const text = posts[0].body.message.text;
  assert.match(text, /^\[2 WeCom messages coalesced, in arrival order\]\nzhang_san: 一\nzhang_san: 二\n\n\[conversation zhang_san — full reply how-to/);
  assert.equal(text.match(/full reply how-to/g).length, 1);
});

// --- voice ASR-repeat dedup (jg-p1mk add C) ------------------------------------

test('a 2x-repeated voice transcript collapses to one block with the marker, counts logged', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, { deps: { log: (...a) => logs.push(a.join(' ')) } });
  const block = '我已经在路上了大概十分钟到';
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'ASR_2X',
    msgtype: 'voice',
    voice: { content: block + block },
  })));
  assert.equal(posts[0].body.message.text, `[voice] ${block} (ASR重复×2已折叠)`);
  const line = logs.find((l) => l.includes('voice ASR repeat collapsed'));
  assert.ok(line, 'the collapse must log counts');
  assert.match(line, /×2 \(26 → 13 chars\)/);
  assert.ok(!line.includes(block), 'transcript content must never reach the log');
});

test('a 3x repeat joined by newlines collapses too', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  const block = '明天上午的演示我会提前把机器人架好';
  await pipeline.enqueueInbound(frame(textMessage({
    msgtype: 'voice',
    voice: { content: [block, block, block].join('\n') },
  })));
  assert.equal(posts[0].body.message.text, `[voice] ${block} (ASR重复×3已折叠)`);
});

test('short doubled phrases and honest transcripts pass through untouched', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  await pipeline.enqueueInbound(frame(textMessage({ msgtype: 'voice', voice: { content: '好的好的' } })));
  await pipeline.enqueueInbound(frame(textMessage({ msgtype: 'voice', voice: { content: '今天的测试很顺利，明天继续' } })));
  assert.equal(posts[0].body.message.text, '[voice] 好的好的');
  assert.equal(posts[1].body.message.text, '[voice] 今天的测试很顺利，明天继续');
});

test('a nested 4x repeat collapses to the base block in one delivery', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  const block = '请把今天的会议纪要发给我一份';
  await pipeline.enqueueInbound(frame(textMessage({
    msgtype: 'voice',
    voice: { content: block.repeat(4) },
  })));
  assert.equal(posts[0].body.message.text, `[voice] ${block} (ASR重复×4已折叠)`);
});

// --- peer-bot context buffering (jg-p1mk add A) --------------------------------

function groupText(overrides = {}) {
  return textMessage({ chattype: 'group', chatid: 'wrROOM_1', ...overrides });
}

test('a peer-bot room post never wakes the session; it rides ahead of the next human delivery', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, {
    cfg: { peerBotUserIds: ['citadel_bot'] },
    deps: { log: (...a) => logs.push(a.join(' ')) },
  });
  await pipeline.enqueueInbound(frame(groupText({
    msgid: 'P_1',
    from: { userid: 'citadel_bot' },
    text: { content: '状态: 部署完成' },
  })));
  assert.equal(posts.length, 0, 'a peer post alone must not deliver');
  assert.equal(pipeline.stats().peerContexts, 1);
  assert.equal(logs.filter((l) => l.includes('not waking the session')).length, 1);

  await pipeline.enqueueInbound(frame(groupText({ msgid: 'H_1', text: { content: '大家好' } })));
  assert.equal(posts.length, 1);
  const text = posts[0].body.message.text;
  assert.equal(text, [
    '[peer-bot context since the last delivery — 1 post; read-only, no reply expected]',
    'citadel_bot: 状态: 部署完成',
    '',
    '大家好',
  ].join('\n'));
  assert.equal(pipeline.stats().peerContexts, 0);
});

test('peer posts in other conversations or from humans are unaffected', async (t) => {
  const { pipeline, posts } = makePipeline(t, { cfg: { peerBotUserIds: ['citadel_bot'] } });
  // Same userid in a DM is NOT peer-buffered (group-only contract).
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'DM_1', from: { userid: 'citadel_bot' }, text: { content: 'dm直达' } })));
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.message.text, 'dm直达');
});

test('the peer buffer caps with a dropped count and dedups replayed msgids', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    cfg: { peerBotUserIds: ['citadel_bot'] },
    deps: { peerContextCap: 2 },
  });
  const peer = (msgid, content) => pipeline.enqueueInbound(frame(groupText({
    msgid, from: { userid: 'citadel_bot' }, text: { content },
  })));
  await peer('PC_1', '一');
  await peer('PC_1', '一'); // replay — must not double
  await peer('PC_2', '二');
  await peer('PC_3', '三'); // evicts PC_1 past the cap of 2
  await pipeline.enqueueInbound(frame(groupText({ msgid: 'H_2', text: { content: '人类消息' } })));
  const text = posts[0].body.message.text;
  assert.match(text, /peer-bot context since the last delivery — 2 posts; 1 older dropped at the 2-item cap/);
  assert.ok(!text.includes('一'), 'evicted post is gone');
  assert.match(text, /citadel_bot: 二\ncitadel_bot: 三/);
});

test('a rejected carrying delivery restores the peer context for the next one', async (t) => {
  let rejectNext = true;
  const delivered = [];
  const { pipeline } = makePipeline(t, {
    cfg: { peerBotUserIds: ['citadel_bot'] },
    postInbound: async (target, body) => {
      if (rejectNext) {
        rejectNext = false;
        const err = new Error('400 Bad Request');
        err.status = 400;
        throw err;
      }
      delivered.push(body.message.text);
    },
  });
  await pipeline.enqueueInbound(frame(groupText({ msgid: 'PR_1', from: { userid: 'citadel_bot' }, text: { content: '上下文' } })));
  await pipeline.enqueueInbound(frame(groupText({ msgid: 'HR_1', text: { content: '被拒绝的' } })));
  await pipeline.enqueueInbound(frame(groupText({ msgid: 'HR_2', text: { content: '成功的' } })));
  assert.equal(delivered.length, 1);
  assert.match(delivered[0], /peer-bot context since the last delivery — 1 post/);
  assert.match(delivered[0], /citadel_bot: 上下文/);
  assert.match(delivered[0], /成功的/);
});

// --- codex jg-p1mk round-1 regressions -----------------------------------------

test('a DM userid colliding with a group chatid never shares a batch (codex r1 f1)', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 30 } });
  const dm = pipeline.enqueueInbound(frame(textMessage({
    msgid: 'COLL_DM',
    from: { userid: 'wrCollision' },
    text: { content: '私聊内容' },
  })));
  const room = pipeline.enqueueInbound(frame(textMessage({
    msgid: 'COLL_ROOM',
    chattype: 'group',
    chatid: 'wrCollision',
    text: { content: '群聊内容' },
  })));
  await Promise.all([dm, room]);
  assert.equal(posts.length, 2, 'one delivery per conversation, never a merged batch');
  const byKind = Object.fromEntries(posts.map((p) => [p.body.message.conversation.kind, p.body.message]));
  assert.equal(byKind.dm.text, '私聊内容');
  assert.equal(byKind.room.text, '群聊内容');
});

test('a hostile mixed member (non-array msg_item) drops alone; a valid media sibling still delivers with no hydration leak (codex r1 f2)', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    deps: { coalesceWindowMs: 30 },
    downloadFile: async () => ({ buffer: Buffer.from('img'), filename: 'p.png' }),
  });
  const bad = pipeline.enqueueInbound(frame(textMessage({
    msgid: 'HOSTILE_MIXED',
    msgtype: 'mixed',
    mixed: { msg_item: { length: 0 } },
  })));
  const good = pipeline.enqueueInbound(frame(fileMessage({ msgid: 'GOOD_FILE' })));
  await Promise.all([bad, good]);
  assert.equal(posts.length, 1);
  assert.match(posts[0].body.message.text, /\[file message\]/);
  assert.equal(posts[0].body.message.attachments.length, 1);
  const stats = pipeline.stats();
  assert.equal(stats.hydrations, 0, 'the valid sibling\'s owner hydration entry must not leak');
  assert.equal(stats.pending, 0);
});

test('reply-help marks are per (kind, id) — a DM and a colliding room each get their own block', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { replyHelpOnce: true } });
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RHK_1', from: { userid: 'wrX' } })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'RHK_2', chattype: 'group', chatid: 'wrX' })));
  assert.equal(posts.length, 2);
  assert.match(posts[0].body.message.text, /full reply how-to/);
  assert.match(posts[1].body.message.text, /full reply how-to/);
});

// --- codex jg-p1mk round-2 regressions -----------------------------------------

test('a numeric sender in one same-room batch member cannot break the combine or lose siblings (codex r2 f1)', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 30 } });
  const good = pipeline.enqueueInbound(frame(textMessage({
    msgid: 'NS_GOOD', chattype: 'group', chatid: 'wrNS', text: { content: '健康消息' },
  })));
  const bad = pipeline.enqueueInbound(frame(textMessage({
    msgid: 'NS_BAD', chattype: 'group', chatid: 'wrNS', from: { userid: 7 }, text: { content: '数字发送者' },
  })));
  await Promise.all([good, bad]);
  assert.equal(posts.length, 1);
  const text = posts[0].body.message.text;
  // Both members render (String coercion), so the batch keeps both —
  // the essential guarantee is no thrown combine and no lost sibling.
  assert.match(text, /健康消息/);
  assert.match(text, /7: 数字发送者/);
});

test('a hostile peer frame cannot throw through the emitter or leave an empty buffer entry (codex r2 f2)', async (t) => {
  const logs = [];
  const { pipeline, posts } = makePipeline(t, {
    cfg: { peerBotUserIds: ['citadel_bot'] },
    deps: { log: (...a) => logs.push(a.join(' ')) },
  });
  // Non-array msg_item makes renderText throw — must be contained.
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'PH_1',
    chattype: 'group',
    chatid: 'wrROOM_9',
    from: { userid: 'citadel_bot' },
    msgtype: 'mixed',
    mixed: { msg_item: { length: 0 } },
  })));
  assert.equal(pipeline.stats().peerContexts, 0, 'no empty buffer entry left behind');
  assert.equal(logs.filter((l) => l.includes('unrenderable')).length, 1);
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'PH_H', chattype: 'group', chatid: 'wrROOM_9', text: { content: '人类' } })));
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.message.text, '人类', 'no phantom peer block');
});

test('the peer conversation map is bounded — LRU conversation evicted past the cap (codex r2 f7)', async (t) => {
  const logs = [];
  const { pipeline } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    deps: { peerConversationsCap: 2, log: (...a) => logs.push(a.join(' ')) },
  });
  for (const room of ['wrA', 'wrB', 'wrC']) {
    await pipeline.enqueueInbound(frame(textMessage({
      msgid: `PB_${room}`, chattype: 'group', chatid: room, from: { userid: 'pb' }, text: { content: room },
    })));
  }
  assert.equal(pipeline.stats().peerContexts, 2);
  assert.equal(logs.filter((l) => l.includes('evicted the least-recently-touched conversation g:wrA')).length, 1);
});

test('a peer post arriving after a buffered human message never rides as its prior context (codex r2 f8 / r3 f2)', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    deps: { coalesceWindowMs: 40 },
  });
  const human = pipeline.enqueueInbound(frame(textMessage({
    msgid: 'ORD_H', chattype: 'group', chatid: 'wrO', text: { content: '请求' },
  })));
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'ORD_P', chattype: 'group', chatid: 'wrO', from: { userid: 'pb' }, text: { content: '响应' },
  })));
  await human;
  // The strict take-bound: the peer post postdates the batch's newest
  // frame, so it stays buffered rather than decorating a delivery it
  // did not precede.
  assert.equal(posts.length, 1);
  assert.equal(posts[0].body.message.text, '请求');
  // It rides the NEXT human delivery — as prior context, which by then
  // it truly is.
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'ORD_H2', chattype: 'group', chatid: 'wrO', text: { content: '后续' },
  })));
  assert.equal(posts.length, 2);
  const text = posts[1].body.message.text;
  assert.ok(text.indexOf('响应') < text.indexOf('后续'), `peer context precedes the next human message:\n${text}`);
  assert.match(text, /peer-bot context since the last delivery/);
});

test('restoration after rejection dedups a peer replay buffered mid-POST (codex r2 f9)', async (t) => {
  let rejectNext = true;
  const delivered = [];
  let pipelineRef;
  const { pipeline } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    postInbound: async (target, body) => {
      if (rejectNext) {
        rejectNext = false;
        // Replay of the taken peer msgid lands while the POST is in flight.
        await pipelineRef.enqueueInbound(frame(textMessage({
          msgid: 'DUP_P', chattype: 'group', chatid: 'wrD', from: { userid: 'pb' }, text: { content: '重复上下文' },
        })));
        const err = new Error('400 Bad Request');
        err.status = 400;
        throw err;
      }
      delivered.push(body.message.text);
    },
  });
  pipelineRef = pipeline;
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'DUP_P', chattype: 'group', chatid: 'wrD', from: { userid: 'pb' }, text: { content: '重复上下文' },
  })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'DUP_H1', chattype: 'group', chatid: 'wrD', text: { content: '第一次' } })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'DUP_H2', chattype: 'group', chatid: 'wrD', text: { content: '第二次' } })));
  assert.equal(delivered.length, 1);
  assert.equal((delivered[0].match(/重复上下文/g) ?? []).length, 1, 'the replayed peer line appears exactly once');
});

test('flushAll in draining mode also awaits in-chain batches and late frames (codex r2 f4)', async (t) => {
  let releaseFirst;
  const gate = new Promise((r) => { releaseFirst = r; });
  const delivered = [];
  const { pipeline } = makePipeline(t, {
    deps: { coalesceWindowMs: 60_000 },
    postInbound: async (target, body) => {
      if (body.message.provider_message_id === 'DR_1') await gate;
      delivered.push(body.message.provider_message_id);
    },
  });
  // DR_1 buffered; flush it into a chain whose POST hangs, then start
  // the drain and enqueue a late frame mid-drain.
  pipeline.enqueueInbound(frame(textMessage({ msgid: 'DR_1' })));
  const drain = pipeline.flushAll();
  pipeline.enqueueInbound(frame(textMessage({ msgid: 'DR_2', text: { content: '迟到' } })));
  setTimeout(releaseFirst, 20);
  await drain;
  assert.deepEqual(delivered, ['DR_1', 'DR_2'], 'the drain returned only after chains and late frames settled');
});

test('the ASR guards protect short deliberate repeats (codex r2 f6)', async (t) => {
  const { pipeline, posts } = makePipeline(t);
  const phrase = '重要的事情说三遍'; // 8 chars — under the 10-char block floor
  await pipeline.enqueueInbound(frame(textMessage({
    msgtype: 'voice',
    voice: { content: phrase + phrase + phrase },
  })));
  assert.equal(posts[0].body.message.text, `[voice] ${phrase}${phrase}${phrase}`);
});

// --- codex jg-p1mk round-3 regressions -----------------------------------------

test('every provider-controlled envelope string is coerced — numeric ids never reach gc as numbers (codex r3 f1)', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { coalesceWindowMs: 30 } });
  await Promise.all([
    pipeline.enqueueInbound(frame(textMessage({
      msgid: 'NC_1', chattype: 'group', chatid: 'wrNC', text: { content: '正常' },
    }))),
    pipeline.enqueueInbound(frame(textMessage({
      msgid: 9001, chattype: 'group', chatid: 'wrNC', from: { userid: 7 }, text: { content: '数字' },
    }))),
  ]);
  assert.equal(posts.length, 1);
  const m = posts[0].body.message;
  assert.equal(typeof m.provider_message_id, 'string');
  assert.equal(m.provider_message_id, '9001');
  assert.equal(typeof m.actor.id, 'string');
  assert.equal(m.actor.id, '7');
  assert.equal(typeof m.actor.display_name, 'string');
  assert.equal(typeof m.conversation.conversation_id, 'string');
  assert.equal(typeof m.dedup_key, 'string');
});

test('a peer post arriving after a NEWER human message is not stolen by an older queued batch (codex r3 f2)', async (t) => {
  let releaseH0;
  const gate = new Promise((r) => { releaseH0 = r; });
  const delivered = [];
  const { pipeline } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    deps: { coalesceWindowMs: 25 },
    postInbound: async (target, body) => {
      if (body.message.provider_message_id === 'ST_H0') await gate;
      delivered.push(body.message.text);
    },
  });
  const room = (over) => textMessage({ chattype: 'group', chatid: 'wrST', ...over });
  // H0's batch flushes and its POST hangs.
  const h0 = pipeline.enqueueInbound(frame(room({ msgid: 'ST_H0', text: { content: '第零' } })));
  await new Promise((r) => setTimeout(r, 40));
  // H1's batch flushes into the chain behind H0.
  const h1 = pipeline.enqueueInbound(frame(room({ msgid: 'ST_H1', text: { content: '第一' } })));
  await new Promise((r) => setTimeout(r, 40));
  // H2 buffers; peer P arrives AFTER H2's frame, then H2's window closes.
  const h2 = pipeline.enqueueInbound(frame(room({ msgid: 'ST_H2', text: { content: '第二' } })));
  await pipeline.enqueueInbound(frame(room({ msgid: 'ST_P', from: { userid: 'pb' }, text: { content: '同伴回应' } })));
  releaseH0();
  await Promise.all([h0, h1, h2]);
  assert.equal(delivered.length, 3);
  assert.ok(!delivered[1].includes('同伴回应'), `H1 must not steal the peer post:\n${delivered[1]}`);
  assert.ok(!delivered[2].includes('同伴回应'), 'P postdates H2 too — it stays buffered');
  // P rides the next human delivery as genuinely-prior context.
  await pipeline.enqueueInbound(frame(room({ msgid: 'ST_H3', text: { content: '第三' } })));
  assert.equal(delivered.length, 4);
  assert.ok(delivered[3].indexOf('同伴回应') < delivered[3].indexOf('第三'), delivered[3]);
});

test('the before/after split anchors on the first VALID member, not a dropped malformed one (codex r3 f2)', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    deps: { coalesceWindowMs: 30 },
  });
  const room = (over) => textMessage({ chattype: 'group', chatid: 'wrSP', ...over });
  const bad = pipeline.enqueueInbound(frame(room({ msgid: 'SP_BAD', create_time: 1e15 })));
  await pipeline.enqueueInbound(frame(room({ msgid: 'SP_P', from: { userid: 'pb' }, text: { content: '上下文' } })));
  const good = pipeline.enqueueInbound(frame(room({ msgid: 'SP_H', text: { content: '人类消息' } })));
  await Promise.all([bad, good]);
  assert.equal(posts.length, 1);
  const text = posts[0].body.message.text;
  assert.ok(text.indexOf('上下文') < text.indexOf('人类消息'), `peer arrived before the valid human, must render above:\n${text}`);
  assert.match(text, /peer-bot context since the last delivery/);
});

test('peer-conversation LRU eviction is deterministic under same-millisecond ties (codex r3 f3)', async (t) => {
  const logs = [];
  const { pipeline } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    deps: { peerConversationsCap: 2, log: (...a) => logs.push(a.join(' ')) },
  });
  const peer = (room, content) => pipeline.enqueueInbound(frame(textMessage({
    msgid: `LR_${room}_${content}`, chattype: 'group', chatid: room, from: { userid: 'pb' }, text: { content },
  })));
  await peer('wrA', '1'); // A buffered
  await peer('wrB', '1'); // B buffered
  await peer('wrA', '2'); // A touched — B is now least recently touched
  await peer('wrC', '1'); // over cap: B must evict, not A
  assert.equal(logs.filter((l) => l.includes('evicted the least-recently-touched conversation g:wrB')).length, 1);
});

// --- codex jg-p1mk round-4 regressions -----------------------------------------

test('numeric identity fields normalize at intake — msgid 0 dedups and media provider_id is a string (codex r4 f1/f2)', async (t) => {
  const { pipeline, posts } = makePipeline(t, {
    downloadFile: async () => ({ buffer: Buffer.from('%PDF-1.7 z'), filename: 'n.pdf' }),
  });
  await pipeline.enqueueInbound(frame(fileMessage({ msgid: 12345 })));
  assert.equal(posts.length, 1);
  const att = posts[0].body.message.attachments[0];
  assert.equal(typeof att.provider_id, 'string');
  assert.equal(att.provider_id, '12345');

  // msgid 0 delivers once and its replay dedups — truthiness gates must
  // see the normalized string "0".
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 0, text: { content: '零' } })));
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 0, text: { content: '零' } })));
  assert.equal(posts.length, 2, 'the replay of msgid 0 must not double-post');
  assert.equal(posts[1].body.message.provider_message_id, '0');
});

test('a numeric peer-bot userid matches the string allowlist after intake normalization (codex r4 f2)', async (t) => {
  const { pipeline, posts } = makePipeline(t, { cfg: { peerBotUserIds: ['7'] } });
  await pipeline.enqueueInbound(frame(textMessage({
    msgid: 'PN_1', chattype: 'group', chatid: 'wrPN', from: { userid: 7 }, text: { content: '机器人' },
  })));
  assert.equal(posts.length, 0, 'the numeric-userid peer post must buffer, not wake');
  assert.equal(pipeline.stats().peerContexts, 1);
});

test('the reply-help cache is bounded — eviction re-sends, never grows unbounded (codex r4 f3)', async (t) => {
  const { pipeline, posts } = makePipeline(t, { deps: { replyHelpOnce: true, replyHelpCap: 2 } });
  for (const u of ['u1', 'u2', 'u3']) {
    await pipeline.enqueueInbound(frame(textMessage({ msgid: `HC_${u}`, from: { userid: u } })));
  }
  // u1 was evicted at the cap of 2 — its next delivery re-sends the block.
  await pipeline.enqueueInbound(frame(textMessage({ msgid: 'HC_u1b', from: { userid: 'u1' }, text: { content: '再见' } })));
  assert.equal(posts.length, 4);
  assert.match(posts[3].body.message.text, /full reply how-to/);
});

test('restore keeps the ORIGINAL copy of a mid-POST peer replay, preserving arrival order (codex r4 f4)', async (t) => {
  let rejectNext = true;
  const delivered = [];
  let pipelineRef;
  const room = (over) => textMessage({ chattype: 'group', chatid: 'wrRO', ...over });
  const { pipeline } = makePipeline(t, {
    cfg: { peerBotUserIds: ['pb'] },
    postInbound: async (target, body) => {
      if (rejectNext) {
        rejectNext = false;
        // P_old replays and P_new arrives while the POST is in flight.
        await pipelineRef.enqueueInbound(frame(room({ msgid: 'RO_OLD', from: { userid: 'pb' }, text: { content: '旧帖' } })));
        await pipelineRef.enqueueInbound(frame(room({ msgid: 'RO_NEW', from: { userid: 'pb' }, text: { content: '新帖' } })));
        const err = new Error('400 Bad Request');
        err.status = 400;
        throw err;
      }
      delivered.push(body.message.text);
    },
  });
  pipelineRef = pipeline;
  await pipeline.enqueueInbound(frame(room({ msgid: 'RO_OLD', from: { userid: 'pb' }, text: { content: '旧帖' } })));
  await pipeline.enqueueInbound(frame(room({ msgid: 'RO_H1', text: { content: '被拒' } })));
  await pipeline.enqueueInbound(frame(room({ msgid: 'RO_H2', text: { content: '成功' } })));
  assert.equal(delivered.length, 1);
  const text = delivered[0];
  assert.equal((text.match(/旧帖/g) ?? []).length, 1);
  assert.ok(text.indexOf('旧帖') < text.indexOf('新帖'), `original arrival order must survive the rejection cycle:\n${text}`);
});
