// upload.test.js — unit tests for the adapter-owned media uploader
// (src/upload.js, jg-bw5gsw), against a scripted fake transport and — for
// the SDK seam — the real @wecom/aibot-node-sdk 1.0.7 WsConnectionManager.
// Pins the contract that fixes the 10/12–10/15 upload failures: one frame
// outstanding across all uploads, size-scaled per-frame deadlines,
// bounded retries (init and finish included) where any attempt's
// acknowledgement completes the frame, reconnect-aware resumes of the
// upload session (init again only when the server refuses it), the
// overall budget, the 100-chunk plan, allowlist-renderable failures, and
// a summary log line that never carries the filename or provider ids.

import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import { EventEmitter } from 'node:events';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { mock, test } from 'node:test';
import { setImmediate } from 'node:timers/promises';

import AiBot, { WsConnectionManager } from '@wecom/aibot-node-sdk';

import { describeProviderError } from '../src/media.js';
import { createInboundLiveness } from '../src/liveness.js';
import { createOutboundPublisher } from '../src/outbound.js';
import {
  ackTimeoutFor,
  createMediaUploader,
  createSdkUploadTransport,
  MAX_CHUNKS,
  planChunks,
  UPLOAD_CMD,
  UploadStageError,
  uploadDefaults,
} from '../src/upload.js';

const KiB = 1024;

// makeTransport is a scripted WeCom long connection. Each frame is
// answered by `script(frame)` → { ack } | { reject } | 'silent' |
// { lateAckMs } (default: a well-formed ack after 1 ms). It tracks how
// many frames await an answer at once.
function makeTransport(script = () => null) {
  const t = {
    connected: true,
    epoch: 1,
    frames: [],
    outstanding: 0,
    maxOutstanding: 0,
    waiters: [],
    defaultAck(frame) {
      if (frame.cmd === UPLOAD_CMD.init) return { headers: {}, errcode: 0, body: { upload_id: `UP-${t.epoch}` } };
      if (frame.cmd === UPLOAD_CMD.finish) {
        return { headers: {}, errcode: 0, body: { media_id: `MEDIA-${frame.body.upload_id}`, type: 'file', created_at: '1' } };
      }
      return { headers: {}, errcode: 0 };
    },
    sendFrame(cmd, body, listenMs) {
      if (!t.connected) return Promise.reject(new Error('WebSocket not connected, unable to send data'));
      const frame = { cmd, body, listenMs, attempt: t.frames.filter((f) => f.cmd === cmd && f.body.chunk_index === body.chunk_index).length + 1 };
      t.frames.push(frame);
      const decision = script(frame) ?? {};
      if (decision === 'silent') return new Promise(() => {});
      t.outstanding += 1;
      t.maxOutstanding = Math.max(t.maxOutstanding, t.outstanding);
      return new Promise((resolve, reject) => {
        setTimeout(() => {
          t.outstanding -= 1;
          if (decision.reject) reject(decision.reject);
          else resolve(decision.ack ?? t.defaultAck(frame));
        }, decision.lateAckMs ?? 1);
      });
    },
    isConnected: () => t.connected,
    connectionEpoch: () => t.epoch,
    waitForConnection(ms) {
      if (t.connected) return Promise.resolve(true);
      return new Promise((resolve) => {
        const timer = setTimeout(() => resolve(false), ms);
        t.waiters.push(() => { clearTimeout(timer); resolve(true); });
      });
    },
    drop() {
      t.connected = false;
    },
    reconnect() {
      t.connected = true;
      t.epoch += 1;
      for (const wake of t.waiters.splice(0)) wake();
    },
  };
  return t;
}

// makeUploader wires a fast uploader: tiny deadlines, instant backoff
// (recorded), and captured logs.
function makeUploader(transport, options = {}) {
  const sleeps = [];
  const logs = [];
  const uploader = createMediaUploader({
    transport,
    options: { ackBaseMs: 40, floorBytesPerSec: 1e12, reconnectWaitMs: 200, ...options },
    log: (line) => logs.push(line),
    sleep: async (ms) => { sleeps.push(ms); },
  });
  return { uploader, sleeps, logs };
}

const bytes = (n) => crypto.randomBytes(n);
const chunkFrames = (t, index) => t.frames.filter((f) => f.cmd === UPLOAD_CMD.chunk && f.body.chunk_index === index);
const cmds = (t) => t.frames.map((f) => (f.cmd === UPLOAD_CMD.chunk ? `chunk${f.body.chunk_index}` : f.cmd.replace('aibot_upload_media_', '')));

// Flush promise continuations after advancing the mocked clock, so the
// next frame/backoff timer is armed before the next tick.
async function advance(t, ms) {
  t.mock.timers.tick(ms);
  await setImmediate();
}

// --- planning ----------------------------------------------------------------

test('planChunks keeps the configured size, raises it to fit 100 chunks, and refuses the impossible', () => {
  assert.deepEqual(planChunks(1024 * KiB), { chunkBytes: 512 * KiB, totalChunks: 2 });
  assert.deepEqual(planChunks(600 * KiB, 256 * KiB), { chunkBytes: 256 * KiB, totalChunks: 3 });
  // Below the floor is clamped up; above the protocol cap clamped down.
  assert.equal(planChunks(10, 1).chunkBytes, 64 * KiB);
  assert.equal(planChunks(10, 4096 * KiB).chunkBytes, 512 * KiB);
  // A 20 MiB file at 64 KiB would need 320 chunks — raised to fit 100.
  const big = planChunks(20 * 1024 * KiB, 64 * KiB);
  assert.equal(big.totalChunks, MAX_CHUNKS);
  assert.ok(big.chunkBytes <= 512 * KiB);
  assert.throws(() => planChunks(0), UploadStageError);
  assert.throws(() => planChunks(512 * KiB * MAX_CHUNKS + 1), /more than 100 chunks/);
});

test('ackTimeoutFor scales a frame deadline with its bytes', () => {
  const o = { ackBaseMs: 10_000, floorBytesPerSec: 64 * KiB };
  assert.equal(ackTimeoutFor(0, o), 10_000);
  // A 512 KiB chunk is ~700 KB of base64: ~10.7 s on top of the base.
  assert.equal(ackTimeoutFor(Math.ceil((512 * KiB * 4) / 3), o), 10_000 + Math.ceil((Math.ceil((512 * KiB * 4) / 3) * 1000) / (64 * KiB)));
  assert.equal(uploadDefaults.chunkBytes, 512 * KiB, 'the default stays wire-identical to the SDK');
});

// --- the happy path ------------------------------------------------------------

test('an upload sends init, every chunk in order, and finish — and the chunks reassemble to the file', async () => {
  const t = makeTransport();
  const { uploader } = makeUploader(t, { chunkBytes: 64 * KiB });
  const buf = bytes(150 * KiB);
  const out = await uploader.upload(buf, { type: 'file', filename: 'a.bin' });

  assert.equal(out.media_id, 'MEDIA-UP-1');
  assert.deepEqual(cmds(t), ['init', 'chunk0', 'chunk1', 'chunk2', 'finish']);
  const init = t.frames[0].body;
  assert.equal(init.total_size, buf.length);
  assert.equal(init.total_chunks, 3);
  assert.equal(init.md5, crypto.createHash('md5').update(buf).digest('hex'));
  const joined = Buffer.concat(t.frames
    .filter((f) => f.cmd === UPLOAD_CMD.chunk)
    .map((f) => Buffer.from(f.body.base64_data, 'base64')));
  assert.ok(joined.equals(buf));
  assert.ok(t.frames.every((f) => f.listenMs === uploadDefaults.orphanAckWindowMs));
});

test('one frame is outstanding at a time, even across concurrent uploads', async () => {
  const t = makeTransport(() => ({ lateAckMs: 3 }));
  const { uploader } = makeUploader(t, { chunkBytes: 64 * KiB });
  const [a, b] = await Promise.all([
    uploader.upload(bytes(200 * KiB), { type: 'file', filename: 'a.bin' }),
    uploader.upload(bytes(200 * KiB), { type: 'file', filename: 'b.bin' }),
  ]);
  assert.ok(a.media_id && b.media_id);
  assert.equal(t.maxOutstanding, 1);
  assert.equal(t.frames.length, 2 * (1 + 4 + 1));
});

test('inFlight counts running uploads for the liveness deferral', async () => {
  const t = makeTransport(() => ({ lateAckMs: 5 }));
  const { uploader } = makeUploader(t);
  assert.equal(uploader.inFlight(), 0);
  const running = uploader.upload(bytes(10 * KiB), { type: 'file', filename: 'a.bin' });
  assert.equal(uploader.inFlight(), 1);
  await running;
  assert.equal(uploader.inFlight(), 0);
});

// --- retries -----------------------------------------------------------------

test('a chunk that misses its deadline is re-sent with backoff under the same chunk_index', async () => {
  const t = makeTransport((f) => (f.cmd === UPLOAD_CMD.chunk && f.body.chunk_index === 1 && f.attempt === 1 ? 'silent' : null));
  const { uploader, sleeps, logs } = makeUploader(t, { chunkBytes: 64 * KiB });
  const out = await uploader.upload(bytes(150 * KiB), { type: 'file', filename: 'a.bin' });
  assert.ok(out.media_id);
  assert.equal(chunkFrames(t, 1).length, 2);
  assert.deepEqual(sleeps, [uploadDefaults.backoffBaseMs]);
  assert.match(logs.at(-1), /^media upload ok: .* 1 retries, 0 restarts/);
});

test('a late acknowledgement from an earlier attempt completes the frame (no third send)', async () => {
  // Attempt 1 is acknowledged after its 40 ms deadline (the 10/15 shape:
  // WeCom got every frame, just late); attempt 2 is never answered.
  const t = makeTransport((f) => {
    if (f.cmd !== UPLOAD_CMD.chunk || f.body.chunk_index !== 0) return null;
    return f.attempt === 1 ? { lateAckMs: 70 } : 'silent';
  });
  const { uploader } = makeUploader(t, { chunkBytes: 64 * KiB });
  const out = await uploader.upload(bytes(100 * KiB), { type: 'file', filename: 'a.bin' });
  assert.ok(out.media_id);
  assert.equal(chunkFrames(t, 0).length, 2);
});

test('init and finish are retried too (the SDK gave them a single 5 s try)', async () => {
  const t = makeTransport((f) => {
    if (f.cmd === UPLOAD_CMD.init && f.attempt === 1) return { reject: { headers: {}, errcode: 6000, errmsg: 'busy' } };
    if (f.cmd === UPLOAD_CMD.finish && f.attempt === 1) return 'silent';
    return null;
  });
  const { uploader, sleeps } = makeUploader(t);
  const out = await uploader.upload(bytes(10 * KiB), { type: 'image', filename: 'a.png' });
  assert.ok(out.media_id);
  assert.deepEqual(cmds(t), ['init', 'init', 'chunk0', 'finish', 'finish']);
  assert.equal(sleeps.length, 2);
});

test('exhausted retries fail with a stage-shaped error the allowlist renders', async () => {
  const t = makeTransport((f) => (f.cmd === UPLOAD_CMD.chunk && f.body.chunk_index === 1 ? 'silent' : null));
  const { uploader, sleeps, logs } = makeUploader(t, { chunkBytes: 64 * KiB });
  const err = await uploader.upload(bytes(150 * KiB), { type: 'file', filename: 'a.bin' }).then(
    () => assert.fail('expected a failure'),
    (e) => e,
  );
  assert.ok(err instanceof UploadStageError);
  assert.equal(err.uploadStage, 'chunk 2/3');
  assert.equal(err.uploadAttempts, uploadDefaults.frameAttempts);
  assert.deepEqual(sleeps, [1000, 2000, 4000]);
  assert.equal(chunkFrames(t, 1).length, uploadDefaults.frameAttempts);
  assert.equal(describeProviderError(err),
    'media upload failed at chunk 2/3 after 4 attempts: the upload frame acknowledgement timed out');
  assert.match(logs.at(-1), /^media upload FAILED at chunk 2\/3: /);
  assert.equal(uploader.inFlight(), 0);
});

test('a negative acknowledgement keeps its errcode, never its errmsg', async () => {
  const t = makeTransport((f) => (f.cmd === UPLOAD_CMD.init
    ? { reject: { headers: {}, errcode: 45009, errmsg: 'quota exceeded for secret-plan.pdf' } }
    : null));
  const { uploader, logs } = makeUploader(t);
  const err = await uploader.upload(bytes(10 * KiB), { type: 'file', filename: 'secret-plan.pdf' }).catch((e) => e);
  const rendered = describeProviderError(err);
  assert.equal(rendered, 'media upload failed at init after 4 attempts: provider rejected the message: errcode 45009');
  assert.ok(!logs.join('\n').includes('secret-plan'));
});

// --- the connection ------------------------------------------------------------

test('a dropped connection waits for re-authentication and resumes the session at the first unacknowledged chunk', async () => {
  let dropped = false;
  const t = makeTransport((f) => {
    if (!dropped && f.cmd === UPLOAD_CMD.chunk && f.body.chunk_index === 1) {
      dropped = true;
      t.drop();
      setTimeout(() => t.reconnect(), 10);
      return { reject: new Error('WebSocket connection closed (code: 1006), reply for reqId: aibot_upload_media_chunk_1_x cancelled') };
    }
    return null;
  });
  const { uploader, logs } = makeUploader(t, { chunkBytes: 64 * KiB });
  const out = await uploader.upload(bytes(150 * KiB), { type: 'file', filename: 'a.bin' });
  // WeCom documents that an unexpired session survives a reconnect
  // (101463 「断线重传」): same upload_id, no second init, chunk 0 not resent.
  assert.equal(out.media_id, 'MEDIA-UP-1');
  assert.deepEqual(cmds(t), ['init', 'chunk0', 'chunk1', 'chunk1', 'chunk2', 'finish']);
  assert.ok(t.frames.filter((f) => f.cmd !== UPLOAD_CMD.init).every((f) => f.body.upload_id === 'UP-1'));
  assert.match(logs.at(-1), /1 restarts \(1 resumed\)/);
});

test('a resumed session the server refuses starts over from init, without retrying the refused frame', async () => {
  let dropped = false;
  const t = makeTransport((f) => {
    if (!dropped && f.cmd === UPLOAD_CMD.chunk && f.body.chunk_index === 1) {
      dropped = true;
      t.drop();
      setTimeout(() => t.reconnect(), 5);
      return { reject: new Error('WebSocket connection closed (code: 1006), reply for reqId: r cancelled') };
    }
    if (f.cmd === UPLOAD_CMD.chunk && f.body.upload_id === 'UP-1' && dropped) {
      return { reject: { headers: {}, errcode: 846611, errmsg: 'invalid upload_id' } };
    }
    return null;
  });
  const { uploader, sleeps } = makeUploader(t, { chunkBytes: 64 * KiB });
  const out = await uploader.upload(bytes(150 * KiB), { type: 'file', filename: 'a.bin' });
  assert.equal(out.media_id, 'MEDIA-UP-2');
  assert.deepEqual(cmds(t), ['init', 'chunk0', 'chunk1', 'chunk1', 'init', 'chunk0', 'chunk1', 'chunk2', 'finish']);
  assert.deepEqual(sleeps, [], 'the refused resume frame is not retried with backoff');
});

test('an upload that outlives its budget stops before the next frame', async () => {
  let clock = 0;
  const t = makeTransport(() => { clock += 400; return null; });
  const uploader = createMediaUploader({
    transport: t,
    options: { ackBaseMs: 40, floorBytesPerSec: 1e12, chunkBytes: 64 * KiB, maxUploadMs: 1000 },
    now: () => clock,
    sleep: async () => {},
  });
  const err = await uploader.upload(bytes(300 * KiB), { type: 'file', filename: 'a.bin' }).catch((e) => e);
  assert.ok(err instanceof UploadStageError);
  assert.match(describeProviderError(err), /^media upload failed at chunk \d+\/5 after 0 attempts: the media upload exceeded its time budget$/);
  assert.ok(t.frames.length < 7, 'stopped before sending every frame');
});

test('an upload whose budget expires in the lane queue fails without transmitting', async (t) => {
  t.mock.timers.enable({ apis: ['Date', 'setTimeout'], now: 1000 });
  const transport = makeTransport((f) => (
    f.cmd === UPLOAD_CMD.finish && f.attempt === 1 ? { lateAckMs: 1001 } : null
  ));
  const { uploader, logs, sleeps } = makeUploader(transport, { maxUploadMs: 1000, ackBaseMs: 5000 });
  const first = uploader.upload(bytes(10), { type: 'file', filename: 'first.bin' });
  await advance(t, 0);
  await advance(t, 1); // init acknowledged
  await advance(t, 1); // chunk acknowledged; finish holds the lane
  const queued = uploader.upload(bytes(10), { type: 'file', filename: 'queued.bin' }).catch((err) => err);
  await advance(t, 0);
  assert.equal(uploader.inFlight(), 2);
  assert.deepEqual(cmds(transport), ['init', 'chunk0', 'finish']);

  await advance(t, 1001); // finish acknowledged after the queued upload's budget
  assert.deepEqual(cmds(transport), ['init', 'chunk0', 'finish'], 'the queued init never reaches the transport');
  assert.ok((await first).media_id);
  const err = await queued;
  assert.ok(err instanceof UploadStageError);
  assert.equal(describeProviderError(err),
    'media upload failed at init after 0 attempts: the media upload exceeded its time budget');
  assert.deepEqual(sleeps, [], 'budget exhaustion is not retried');
  assert.match(logs.at(-1), /^media upload FAILED at init: 10 bytes, 1×512KiB chunks, 0 frames, 0 retries, /);
  assert.equal(uploader.inFlight(), 0);

  const next = uploader.upload(bytes(10), { type: 'file', filename: 'next.bin' });
  await advance(t, 0);
  for (let i = 0; i < 3; i++) await advance(t, 1);
  assert.ok((await next).media_id, 'the failed queued upload releases the lane');
});

test('budget expiry during retry backoff releases publisher admission and liveness deferral', async (t) => {
  t.mock.timers.enable({ apis: ['Date', 'setTimeout'], now: 1000 });
  let initSent;
  const transport = makeTransport((f) => {
    if (f.cmd === UPLOAD_CMD.init) initSent?.();
    return f.cmd === UPLOAD_CMD.chunk && f.attempt === 1 ? 'silent' : null;
  });
  const logs = [];
  const uploader = createMediaUploader({
    transport,
    options: { maxUploadMs: 1000, ackBaseMs: 40, floorBytesPerSec: 1e12 },
    log: (line) => logs.push(line),
  }); // use the real retry sleep, driven by mock timers
  let reconnects = 0;
  const liveness = createInboundLiveness({
    stallAfterMs: 1,
    reconnect: () => { reconnects += 1; },
    reconnectDeferral: () => (uploader.inFlight() > 0 ? 'upload in flight' : null),
  });
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), 'wecom-upload-budget-')));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const file = path.join(dir, 'a.bin');
  fs.writeFileSync(file, bytes(10));
  const publisher = createOutboundPublisher({
    cfg: { outboundMediaRoot: dir, fileMaxBytes: 1024, uploadMaxConcurrent: 1, uploadMaxQueue: 0 },
    uploadMedia: uploader.upload,
  });
  function probe() {
    const response = {};
    const started = new Promise((resolve) => { initSent = resolve; });
    const result = publisher.handleUploadProbe({}, {
      writeHead(status) { response.status = status; },
      end(body) { response.body = JSON.parse(body); },
    }, JSON.stringify({ file_path: file, media_kind: 'file' }));
    return { response, result, started };
  }

  const first = probe();
  await first.started; // real file I/O completes before advancing upload timers
  await advance(t, 1); // init acknowledged; chunk goes unanswered
  await advance(t, 41); // acknowledgement deadline starts the real backoff
  assert.deepEqual(cmds(transport), ['init', 'chunk0']);
  liveness.tick();
  assert.equal(reconnects, 0, 'the running upload defers a reconnect');
  const busy = probe();
  await busy.result;
  assert.equal(busy.response.status, 429, 'the backoff still owns the admission slot');

  await advance(t, 1000); // retry wakes after the upload budget
  assert.deepEqual(cmds(transport), ['init', 'chunk0'], 'no retry is transmitted after expiry');
  await first.result;
  assert.equal(first.response.status, 502);
  assert.equal(first.response.body.error,
    'media upload failed at chunk 1/1 after 0 attempts: the media upload exceeded its time budget');
  assert.match(logs.at(-1), /^media upload FAILED at chunk 1\/1: 10 bytes, 1×512KiB chunks, 2 frames, 1 retries, /);
  assert.equal(uploader.inFlight(), 0);
  liveness.tick();
  assert.equal(reconnects, 1, 'budget failure releases the liveness deferral');

  const next = probe();
  await next.started;
  for (let i = 0; i < 3; i++) await advance(t, 1);
  await next.result;
  assert.equal(next.response.status, 200, 'a fresh upload can acquire the released admission slot');
  assert.equal(next.response.body.uploaded, true);
});

for (const stage of ['init', 'chunk']) {
  test(`budget expiry during reconnect prevents a resumed ${stage} transmission`, async (t) => {
    t.mock.timers.enable({ apis: ['Date', 'setTimeout'], now: 1000 });
    const transport = makeTransport((f) => {
      if (f.cmd !== UPLOAD_CMD[stage]) return null;
      transport.drop();
      return { reject: new Error('WebSocket connection closed (code: 1006), reply for reqId: x cancelled') };
    });
    const { uploader, logs, sleeps } = makeUploader(transport, { maxUploadMs: 1000, reconnectWaitMs: 2000 });
    const running = uploader.upload(bytes(10), { type: 'file', filename: 'a.bin' }).catch((err) => err);
    await advance(t, 0);
    await advance(t, 1);
    if (stage === 'chunk') await advance(t, 1);
    const beforeReconnect = cmds(transport);
    assert.equal(transport.waiters.length, 1, 'the upload is waiting for re-authentication');
    await advance(t, 1000);
    transport.reconnect();
    await advance(t, 0);
    assert.deepEqual(cmds(transport), beforeReconnect);
    const err = await running;
    assert.ok(err instanceof UploadStageError);
    assert.equal(err.uploadStage, stage === 'init' ? 'init' : 'chunk 1/1');
    assert.equal(err.uploadAttempts, 0);
    assert.match(describeProviderError(err), /the media upload exceeded its time budget$/);
    assert.match(logs.at(-1), /^media upload FAILED at /);
    assert.deepEqual(sleeps, []);
    assert.equal(uploader.inFlight(), 0);
  });
}

test('restarts after connection loss are bounded', async () => {
  const t = makeTransport((f) => {
    if (f.cmd !== UPLOAD_CMD.chunk) return null;
    t.drop();
    setTimeout(() => t.reconnect(), 5);
    return { reject: new Error('WebSocket connection closed (code: 1006), reply for reqId: x cancelled') };
  });
  const { uploader } = makeUploader(t);
  const err = await uploader.upload(bytes(10 * KiB), { type: 'file', filename: 'a.bin' }).catch((e) => e);
  assert.ok(err instanceof UploadStageError);
  assert.equal(t.frames.filter((f) => f.cmd === UPLOAD_CMD.init).length, 1, 'each reconnect resumed the session');
  assert.equal(chunkFrames(t, 0).length, uploadDefaults.restarts + 1);
  assert.equal(describeProviderError(err),
    'media upload failed at chunk 1/1 after 3 attempts: the connection dropped before the acknowledgement arrived');
});

test('no connection within the wait fails at init without writing a frame', async () => {
  const t = makeTransport();
  t.connected = false;
  const { uploader } = makeUploader(t, { reconnectWaitMs: 20 });
  const err = await uploader.upload(bytes(10 * KiB), { type: 'file', filename: 'a.bin' }).catch((e) => e);
  assert.equal(t.frames.length, 0);
  assert.equal(describeProviderError(err),
    'media upload failed at init after 0 attempts: the WebSocket is not connected (nothing was written)');
});

test('an upload waits for a connection that comes back in time', async () => {
  const t = makeTransport();
  t.connected = false;
  setTimeout(() => t.reconnect(), 10);
  const { uploader } = makeUploader(t);
  const out = await uploader.upload(bytes(10 * KiB), { type: 'file', filename: 'a.bin' });
  assert.ok(out.media_id);
});

test('the summary line carries sizes and timings, never the filename or provider ids', async () => {
  const t = makeTransport();
  const { uploader, logs } = makeUploader(t);
  await uploader.upload(bytes(10 * KiB), { type: 'file', filename: 'quarterly-secret.pdf' });
  const all = logs.join('\n');
  assert.match(all, /^media upload ok: 10240 bytes, 1×512KiB chunks, 3 frames, 0 retries, 0 restarts \(0 resumed\), /);
  assert.ok(!all.includes('quarterly-secret'));
  assert.ok(!all.includes('UP-1'));
  assert.ok(!all.includes('MEDIA-'));
});

// --- the SDK seam (real WsConnectionManager, SDK 1.0.7) ----------------------------

// sdkClient builds a WSClient-shaped emitter around a REAL
// WsConnectionManager whose socket is a capture stub.
function sdkClient() {
  const manager = new WsConnectionManager({ debug() {}, info() {}, warn() {}, error() {} });
  const written = [];
  manager.ws = {
    readyState: 1, // ws OPEN
    send: (data) => written.push(JSON.parse(data)),
    terminate() {},
    removeAllListeners() {},
  };
  const client = new EventEmitter();
  client.wsManager = manager;
  Object.defineProperty(client, 'isConnected', { get: () => manager.isConnected });
  return { client, manager, written };
}

test('the SDK transport arms the real 1.0.7 timer with the listen window and leaves other sends at 5000 ms', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { client, manager, written } = sdkClient();
  const transport = createSdkUploadTransport(client, { generateReqId: AiBot.generateReqId });
  assert.ok(transport);

  const upload = transport.sendFrame(UPLOAD_CMD.chunk, { upload_id: 'u', chunk_index: 0, base64_data: 'AA==' }, 60_000);
  assert.equal(manager.replyAckTimeout, 5000, 'the swap is scoped to the one frame');
  const text = manager.sendReply(AiBot.generateReqId('aibot_send_msg'), { chatid: 'c' }, 'aibot_send_msg');
  let uploadSettled = false;
  upload.then(() => { uploadSettled = true; }, () => { uploadSettled = true; });
  const textOutcome = text.then(() => 'acked', (e) => e.message);

  t.mock.timers.tick(5001);
  assert.match(await textOutcome, /^Reply ack timeout \(5000ms\)/);
  await Promise.resolve();
  assert.equal(uploadSettled, false, 'the upload frame is still listening past 5 s');

  // Its acknowledgement arrives at ~20 s — matched, not an unknown frame.
  t.mock.timers.tick(15_000);
  const reqId = written[0].headers.req_id;
  assert.match(reqId, /^aibot_upload_media_chunk_/);
  manager.handleFrame({ headers: { req_id: reqId }, errcode: 0, body: {} });
  const ack = await upload;
  assert.equal(ack.headers.req_id, reqId);
});

test('the SDK transport counts authentication, not the socket opening, as connected', async () => {
  const { client } = sdkClient();
  const transport = createSdkUploadTransport(client);
  assert.equal(transport.isConnected(), false, 'socket open, subscribe not yet acknowledged');
  const waiting = transport.waitForConnection(1000);
  client.emit('authenticated');
  assert.equal(await waiting, true);
  assert.equal(transport.isConnected(), true);
  assert.equal(transport.connectionEpoch(), 1);
  client.emit('disconnected', 'code: 1006');
  assert.equal(transport.isConnected(), false);
  client.emit('authenticated');
  assert.equal(transport.connectionEpoch(), 2);
  assert.equal(await transport.waitForConnection(1), true);
});

test('the SDK transport declines a client without the 1.0.7 internals', () => {
  assert.equal(createSdkUploadTransport(new EventEmitter()), null);
  const client = new EventEmitter();
  client.wsManager = { sendReply() {} }; // no numeric replyAckTimeout
  assert.equal(createSdkUploadTransport(client), null);
});

test('waitForConnection gives up after its window', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { client } = sdkClient();
  const transport = createSdkUploadTransport(client);
  const waiting = transport.waitForConnection(5);
  // The socket stub cannot keep the unref'd deadline alive; drive it explicitly.
  t.mock.timers.tick(6);
  assert.equal(await waiting, false);
});

// Guard: node:test's mock must not leak real-timer state between files.
test.after(() => mock.timers.reset());
