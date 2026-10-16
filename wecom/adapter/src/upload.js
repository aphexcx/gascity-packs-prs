// upload.js — adapter-owned chunked media upload over the WeCom long
// connection (jg-bw5gsw). Replaces @wecom/aibot-node-sdk 1.0.7's
// uploadMedia for /publish-media; the SDK's version stays reachable as
// the WECOM_UPLOAD_ENGINE=sdk rollback.
//
// WHY: the SDK's uploader turns a slow uplink into a failed upload, and a
// failed upload into a wedged connection. It writes up to four 512 KiB
// chunks at once (each a ~700 KB base64 frame), and every frame's 5000 ms
// acknowledgement clock starts when the frame is QUEUED on the socket, not
// when its bytes leave. Below ~560 KB/s the later frames of a 2 MB file
// cannot be acknowledged in time; the SDK then queues a DUPLICATE of each
// timed-out chunk behind the original, twice, and gives init/finish no
// retry at all. The backlog it builds (up to 4 × file size) delays the
// next attempt's init, text sends, and heartbeats on the same socket —
// so they time out too, and a missed pong cycles the connection. WeCom
// receives and acknowledges every one of those frames, just late:
// service.log 2026-10-15 11:22–11:23Z shows 15 timed-out frames followed
// by exactly 15 "Received unknown frame" late acks
// (work/wecom-media-failures-1016/findings-v0.1.md; the SDK's own
// issue WecomTeam/aibot-node-sdk#27 reports the same shape).
//
// What this module does instead:
//
//   - ONE upload frame in flight across all uploads (a FIFO lane): the
//     next init/chunk/finish is written only after the previous one is
//     acknowledged or abandoned, so a frame's acknowledgement clock
//     measures that frame, never a queue of others.
//   - Per-frame acknowledgement deadline scaled to the frame's bytes:
//     ackBaseMs + bytes / floorBytesPerSec (defaults 10 s + 64 KiB/s,
//     so a 700 KB chunk frame gets ~21 s and init/finish ~10 s).
//   - Bounded retries with exponential backoff for init, every chunk, and
//     finish. Re-sending a chunk index is safe: WeCom documents that a
//     duplicate chunk is ignored (developer.work.weixin.qq.com/document/
//     path/101463). A retry does not abandon the earlier attempts: each
//     keeps listening for its acknowledgement (orphanAckWindowMs), and the
//     FIRST acknowledgement from any attempt of a frame completes it — the
//     late ack the SDK used to drop as "Received unknown frame".
//   - A dropped connection does not fail the upload outright: the
//     uploader waits (bounded) for re-authentication and restarts from
//     init on the new connection (whether an upload_id survives a
//     reconnect is undocumented), at most `restarts` times.
//   - Configurable chunk size (≤ 512 KiB, the protocol cap), raised
//     automatically so a file never needs more than 100 chunks.
//   - Failures carry a stable shape (stage, attempts, cause) that
//     media.js describeProviderError renders, instead of the SDK's
//     free-text wrapper that the allowlist withholds as "unrecognized".
//   - One summary line per upload (bytes, chunks, frames, retries,
//     elapsed, effective rate, slowest acknowledgement) — never the
//     filename, upload_id, or media_id.

import crypto from 'node:crypto';

// WeCom long-connection upload limits (document/path/101463): a chunk is
// at most 512 KB before base64, and an upload has at most 100 chunks.
export const MAX_CHUNK_BYTES = 512 * 1024;
export const MIN_CHUNK_BYTES = 64 * 1024;
export const MAX_CHUNKS = 100;

export const UPLOAD_CMD = {
  init: 'aibot_upload_media_init',
  chunk: 'aibot_upload_media_chunk',
  finish: 'aibot_upload_media_finish',
};

export const uploadDefaults = {
  chunkBytes: MAX_CHUNK_BYTES, // the SDK's wire format; 256 KiB is the tuning step once probed
  ackBaseMs: 10_000,
  floorBytesPerSec: 64 * 1024,
  frameAttempts: 4, // one try + three retries per frame
  backoffBaseMs: 1_000, // 1 s, 2 s, 4 s …
  backoffMaxMs: 8_000,
  reconnectWaitMs: 30_000,
  restarts: 2,
  // How long an attempt keeps listening for a late acknowledgement after
  // its own deadline passed (the transport's SDK-level timer). The SDK
  // retains the frame (its base64 chunk) until then; the 10/15 incident's
  // late acks all arrived within ~35 s.
  orphanAckWindowMs: 3 * 60 * 1000,
};

// Approximate JSON envelope around a frame body (cmd, headers.req_id,
// field names) — only used to size the acknowledgement timeout.
const FRAME_OVERHEAD_BYTES = 256;

const defaultSleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

// UploadStageError is the uploader's failure: which stage gave up, after
// how many attempts, and the last underlying error. The message is for
// humans and tests; the log/response sink renders the structured fields
// (media.js describeProviderError), never the message text.
export class UploadStageError extends Error {
  constructor(stage, attempts, cause) {
    const causeText = cause instanceof Error ? cause.message
      : (cause && typeof cause.errcode === 'number' ? `errcode ${cause.errcode}` : String(cause));
    super(`media upload failed at ${stage} after ${attempts} attempt${attempts === 1 ? '' : 's'}: ${causeText}`);
    this.name = 'UploadStageError';
    this.uploadStage = stage;
    this.uploadAttempts = attempts;
    this.cause = cause;
  }
}

// Thrown inside one upload run when the connection it started on is gone;
// the outer loop restarts from init (bounded) or surfaces `cause`.
class ConnectionReplaced extends Error {
  constructor(stage, cause) {
    super('connection replaced during upload');
    this.stage = stage;
    this.cause = cause;
  }
}

// planChunks picks the chunk size for one file: the configured size,
// clamped to the protocol range and raised so the file fits in
// MAX_CHUNKS. Throws for an empty file or one beyond 100 × 512 KiB.
export function planChunks(totalSize, chunkBytes = uploadDefaults.chunkBytes) {
  if (!Number.isInteger(totalSize) || totalSize <= 0) {
    throw new UploadStageError('plan', 0, new Error(`cannot upload ${totalSize} bytes`));
  }
  let size = Math.min(Math.max(Math.floor(chunkBytes) || MAX_CHUNK_BYTES, MIN_CHUNK_BYTES), MAX_CHUNK_BYTES);
  if (Math.ceil(totalSize / size) > MAX_CHUNKS) size = Math.ceil(totalSize / MAX_CHUNKS);
  if (size > MAX_CHUNK_BYTES) {
    throw new UploadStageError('plan', 0, new Error(
      `file too large: ${totalSize} bytes needs more than ${MAX_CHUNKS} chunks of ${MAX_CHUNK_BYTES} bytes`));
  }
  return { chunkBytes: size, totalChunks: Math.ceil(totalSize / size) };
}

// ackTimeoutFor sizes one frame's acknowledgement deadline.
export function ackTimeoutFor(frameBytes, { ackBaseMs, floorBytesPerSec }) {
  return ackBaseMs + Math.ceil((frameBytes * 1000) / floorBytesPerSec);
}

// firstAck settles with the first acknowledgement among `attempts` (all
// attempts of one frame), rejects with the newest rejection once every
// attempt has failed, or rejects with a deadline error after `ms`.
function firstAck(attempts, ms) {
  return new Promise((resolve, reject) => {
    let open = attempts.length;
    const timer = setTimeout(() => reject(new Error(`upload frame acknowledgement timed out after ${ms}ms`)), ms);
    for (const attempt of attempts) {
      attempt.then((ack) => {
        clearTimeout(timer);
        resolve(ack);
      }, (err) => {
        open -= 1;
        if (open === 0) {
          clearTimeout(timer);
          reject(err);
        }
      });
    }
  });
}

// createLane is a FIFO mutex: run(fn) starts fn only after every earlier
// fn settled. A rejection releases the lane like a resolution does.
export function createLane() {
  let tail = Promise.resolve();
  return {
    run(fn) {
      const result = tail.then(fn);
      tail = result.then(() => {}, () => {});
      return result;
    },
  };
}

// isConnectionLoss reports whether an SDK send error means the frame
// never got (or lost) its connection — as opposed to a slow or negative
// acknowledgement on a live one. Shapes are the SDK 1.0.7 texts that
// media.js already allowlists.
function isConnectionLoss(err) {
  const msg = err instanceof Error ? err.message : '';
  return /^WebSocket not connected, unable to send data$/.test(msg)
    || /, reply for reqId: .+ cancelled$/.test(msg);
}

// createMediaUploader returns { upload(buffer, {type, filename}), inFlight() }.
//
// transport (see createSdkUploadTransport for the production one):
//   sendFrame(cmd, body, listenMs) → Promise<ack frame>; rejects on a
//                     non-zero errcode (the ack frame itself), a dead
//                     connection, or nothing heard for listenMs. The
//                     uploader enforces its own, shorter deadline.
//   isConnected()     → boolean
//   connectionEpoch() → number, incremented on every authentication
//   waitForConnection(ms) → Promise<boolean>, true once authenticated
export function createMediaUploader({
  transport,
  options = {},
  log = () => {},
  sleep = defaultSleep,
  now = Date.now,
}) {
  const o = { ...uploadDefaults, ...options };
  const lane = createLane();
  let active = 0;

  const backoff = (attempt) => Math.min(o.backoffBaseMs * 2 ** (attempt - 1), o.backoffMaxMs);

  async function ensureConnected(stage) {
    if (transport.isConnected()) return;
    if (await transport.waitForConnection(o.reconnectWaitMs)) return;
    throw new UploadStageError(stage, 0, new Error('WebSocket not connected, unable to send data'));
  }

  // sendFrame sends one frame through the lane with retries; any
  // attempt's acknowledgement completes it. A connection that changed (or
  // dropped) since the run's init raises ConnectionReplaced so the run
  // restarts from init.
  async function sendFrame(stage, cmd, body, frameBytes, epoch, stats) {
    const ackTimeoutMs = ackTimeoutFor(frameBytes, o);
    const attempts = [];
    let lateAck = null; // an earlier attempt acknowledged during a backoff
    let lastErr;
    for (let attempt = 1; attempt <= o.frameAttempts; attempt++) {
      if (lateAck) return lateAck.ack;
      if (!transport.isConnected() || transport.connectionEpoch() !== epoch) {
        throw new ConnectionReplaced(stage, lastErr ?? new Error('WebSocket not connected, unable to send data'));
      }
      try {
        return await lane.run(async () => {
          if (lateAck) return lateAck.ack; // acknowledged while queued for the lane
          const t0 = now();
          let sent;
          try {
            sent = Promise.resolve(transport.sendFrame(cmd, body, o.orphanAckWindowMs));
          } catch (err) {
            sent = Promise.reject(err);
          }
          sent.then((ack) => { lateAck ??= { ack }; }, () => {}); // also: never an unhandled rejection
          attempts.push(sent);
          stats.frames += 1;
          try {
            return await firstAck(attempts, ackTimeoutMs);
          } finally {
            stats.slowestAckMs = Math.max(stats.slowestAckMs, now() - t0);
          }
        });
      } catch (err) {
        lastErr = err;
        if (isConnectionLoss(err)) throw new ConnectionReplaced(stage, err);
        if (attempt < o.frameAttempts) {
          stats.retries += 1;
          await sleep(backoff(attempt));
        }
      }
    }
    throw new UploadStageError(stage, o.frameAttempts, lastErr);
  }

  async function uploadOnce(buffer, type, filename, plan, md5, stats) {
    await ensureConnected('init');
    const epoch = transport.connectionEpoch();
    const init = await sendFrame('init', UPLOAD_CMD.init, {
      type,
      filename,
      total_size: buffer.length,
      total_chunks: plan.totalChunks,
      md5,
    }, FRAME_OVERHEAD_BYTES + Buffer.byteLength(filename ?? ''), epoch, stats);
    const uploadId = init?.body?.upload_id;
    if (!uploadId) throw new UploadStageError('init', 1, new Error('media upload returned no upload_id'));

    for (let i = 0; i < plan.totalChunks; i++) {
      const start = i * plan.chunkBytes;
      const data = buffer.subarray(start, Math.min(start + plan.chunkBytes, buffer.length)).toString('base64');
      await sendFrame(`chunk ${i + 1}/${plan.totalChunks}`, UPLOAD_CMD.chunk, {
        upload_id: uploadId,
        chunk_index: i,
        base64_data: data,
      }, FRAME_OVERHEAD_BYTES + data.length, epoch, stats);
    }

    const fin = await sendFrame('finish', UPLOAD_CMD.finish, { upload_id: uploadId }, FRAME_OVERHEAD_BYTES, epoch, stats);
    const mediaId = fin?.body?.media_id;
    if (!mediaId) throw new UploadStageError('finish', 1, new Error('media upload returned no media_id'));
    return {
      type: fin.body.type ?? type,
      media_id: mediaId,
      created_at: fin.body.created_at ?? new Date().toISOString(),
    };
  }

  function summary(stats, outcome) {
    const elapsedMs = now() - stats.startedAt;
    const rate = elapsedMs > 0 ? Math.round((stats.bytes / 1024) / (elapsedMs / 1000)) : 0;
    return `media upload ${outcome}: ${stats.bytes} bytes, ${stats.chunks}×${Math.round(stats.chunkBytes / 1024)}KiB chunks, `
      + `${stats.frames} frames, ${stats.retries} retries, ${stats.restarts} restarts, `
      + `${(elapsedMs / 1000).toFixed(1)}s (${rate} KiB/s), slowest ack ${(stats.slowestAckMs / 1000).toFixed(1)}s`;
  }

  async function upload(buffer, { type, filename }) {
    const plan = planChunks(buffer.length, o.chunkBytes);
    const md5 = crypto.createHash('md5').update(buffer).digest('hex');
    const stats = {
      bytes: buffer.length,
      chunks: plan.totalChunks,
      chunkBytes: plan.chunkBytes,
      frames: 0,
      retries: 0,
      restarts: 0,
      slowestAckMs: 0,
      startedAt: now(),
    };
    active += 1;
    try {
      for (;;) {
        try {
          const result = await uploadOnce(buffer, type, filename, plan, md5, stats);
          log(summary(stats, 'ok'));
          return result;
        } catch (err) {
          if (err instanceof ConnectionReplaced && stats.restarts < o.restarts) {
            stats.restarts += 1;
            continue; // the next run waits for re-authentication itself
          }
          const failure = err instanceof ConnectionReplaced
            ? new UploadStageError(err.stage, stats.restarts + 1, err.cause)
            : err;
          const where = failure instanceof UploadStageError ? ` at ${failure.uploadStage}` : '';
          log(summary(stats, `FAILED${where}`));
          throw failure;
        }
      }
    } finally {
      active -= 1;
    }
  }

  return {
    upload,
    // Number of uploads currently running — the liveness watchdog defers
    // a forced reconnect cycle while this is non-zero.
    inFlight: () => active,
  };
}

// createSdkUploadTransport adapts an @wecom/aibot-node-sdk 1.0.7 WSClient
// to the transport contract above, or returns null when the client lacks
// the internals it needs (a different SDK build) — the caller then falls
// back to the SDK's own uploadMedia and logs why.
//
// Listening window: SDK 1.0.7 keeps ONE acknowledgement timeout
// (WsConnectionManager.replyAckTimeout, 5000 ms) and reads it
// synchronously when it writes a frame whose req_id queue is empty —
// always the case for a fresh req_id, so sendReply arms that frame's
// timer before returning. Swapping the field around the call therefore
// sets this frame's window only; every other send keeps the SDK's
// 5000 ms. The uploader's own (shorter) deadline decides retries; the
// long window just keeps a late acknowledgement matchable instead of an
// "unknown frame". test/upload.test.js pins this against the real
// WsConnectionManager so an SDK upgrade that breaks it fails loudly.
export function createSdkUploadTransport(wsClient, { generateReqId } = {}) {
  const manager = wsClient?.wsManager;
  if (!manager || typeof manager.sendReply !== 'function' || typeof manager.replyAckTimeout !== 'number') {
    return null;
  }
  const reqId = generateReqId
    ?? ((prefix) => `${prefix}_${Date.now()}_${crypto.randomBytes(4).toString('hex')}`);
  // The SDK's isConnected turns true when the socket OPENS, before the
  // subscribe frame is acknowledged; an upload frame written in that
  // window is not on an authenticated connection. Track authentication
  // from the client's own events — so build this transport before
  // wsClient.connect(), or the first connection is never counted.
  let authenticated = false;
  let epoch = 0;
  const waiters = new Set();
  wsClient.on('authenticated', () => {
    authenticated = true;
    epoch += 1;
    for (const wake of [...waiters]) wake(true);
  });
  wsClient.on('disconnected', () => {
    authenticated = false;
  });
  const isConnected = () => authenticated && wsClient.isConnected === true;
  return {
    sendFrame(cmd, body, listenMs) {
      const previous = manager.replyAckTimeout;
      manager.replyAckTimeout = listenMs;
      try {
        return manager.sendReply(reqId(cmd), body, cmd);
      } finally {
        manager.replyAckTimeout = previous;
      }
    },
    isConnected,
    connectionEpoch: () => epoch,
    waitForConnection(ms) {
      if (isConnected()) return Promise.resolve(true);
      return new Promise((resolve) => {
        let timer = null;
        const wake = (ok) => {
          if (timer) clearTimeout(timer);
          waiters.delete(wake);
          resolve(ok);
        };
        waiters.add(wake);
        timer = setTimeout(() => wake(false), ms);
        timer.unref?.();
      });
    },
  };
}
