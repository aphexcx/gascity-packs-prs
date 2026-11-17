// A preload for the adapter process (node --import), used by
// slow-flush.test.js: every fsync and fdatasync the process makes on a
// stopwatch file, an attempt marker or a marker directory takes
// WA_SLOW_FSYNC_MS (200 ms by default) longer: fs.fsyncSync, fs.fsync,
// fs.fdatasyncSync, fs.fdatasync, and a FileHandle's sync and datasync.
// A synchronous flush blocks the calling thread; an asynchronous one holds a
// thread of Node's libuv pool for that long, as a real slow fsync does, so a
// file create queued behind slow flushes in the pool is measured too.
// WA_SLOW_FSYNC_MATCH replaces the path rule ('.' slows every flush).
// WA_SLOW_FSYNC_LOG names a file that gets one line per slowed flush, so a
// test can show the preload took effect.
// HOLD MODE: with WA_HOLD_FSYNC_FIFO (a named pipe) set, an asynchronous
// slowed flush is held instead of slowed: it opens the pipe for reading and
// reads it to its end, which blocks one thread of Node's pool until the test
// releases it, as a stuck disk would. Once the file WA_HOLD_FSYNC_RELEASED
// exists, flushes are no longer held.
//
// Not every flush: the inbound journal's write-ahead flush before the POST to
// the city is the base's own design (journal.js), so slowing it would slow
// every tree alike and measure nothing of the stopwatch.

import crypto from 'node:crypto';
import fs from 'node:fs';
import fsp from 'node:fs/promises';
import { promisify } from 'node:util';
import { syncBuiltinESMExports } from 'node:module';
import { fileURLToPath } from 'node:url';

const ms = Number(process.env.WA_SLOW_FSYNC_MS || 200);
const rule = new RegExp(process.env.WA_SLOW_FSYNC_MATCH
  || '(?:/stopwatch-\\d{4}-\\d{2}\\.jsonl|/(?:ack|words)-attempts(?:/[0-9a-f]{64})?)$');
const slow = new Set();
const name = (file) => (file instanceof URL ? fileURLToPath(file) : String(file));
const note = (file, fd) => { if (rule.test(name(file))) slow.add(fd); else slow.delete(fd); };
const block = () => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
// Holding a pool thread: pbkdf2 runs on the pool; its iterations are
// calibrated once so that one call takes about ms.
const pbkdf2 = promisify(crypto.pbkdf2);
let iterations = 20000;
{
  const t0 = performance.now(); crypto.pbkdf2Sync('slow', 'flush', iterations, 32, 'sha512');
  iterations = Math.max(1000, Math.round(iterations * ms / Math.max(performance.now() - t0, 0.5)));
}
const timed = () => pbkdf2('slow', 'flush', iterations, 32, 'sha512');
const hold = async () => {
  if (fs.existsSync(process.env.WA_HOLD_FSYNC_RELEASED || '')) return;
  slowed('held');
  const pipe = await popen(process.env.WA_HOLD_FSYNC_FIFO, 'r');
  try { await pipe.readFile(); } finally { await pipe.close(); }
};
const later = () => (process.env.WA_HOLD_FSYNC_FIFO ? hold() : timed());
const appendFileSync = fs.appendFileSync;
const slowed = (what) => { if (process.env.WA_SLOW_FSYNC_LOG) appendFileSync(process.env.WA_SLOW_FSYNC_LOG, `${what}\n`); };

const openSync = fs.openSync;
fs.openSync = function (file, ...rest) { const fd = openSync.call(this, file, ...rest); note(file, fd); return fd; };
const open = fs.open;
fs.open = function (file, ...rest) {
  const cb = rest.pop();
  return open.call(this, file, ...rest, (err, fd) => { if (!err) note(file, fd); cb(err, fd); });
};
const popen = fsp.open;
fsp.open = async function (file, ...rest) { const handle = await popen.call(this, file, ...rest); note(file, handle.fd); return handle; };

for (const method of ['fsyncSync', 'fdatasyncSync']) {
  const original = fs[method];
  fs[method] = function (fd) { if (slow.has(fd)) { slowed(method); block(); } return original.call(this, fd); };
}
for (const method of ['fsync', 'fdatasync']) {
  const original = fs[method];
  fs[method] = function (fd, cb) {
    if (!slow.has(fd)) return original.call(this, fd, cb);
    slowed(method); later().then(() => original.call(this, fd, cb));
    return undefined;
  };
}
const probe = await popen(process.execPath, 'r');
const FileHandle = Object.getPrototypeOf(probe);
await probe.close();
for (const method of ['sync', 'datasync']) {
  const original = FileHandle[method];
  FileHandle[method] = async function () { if (slow.has(this.fd)) { slowed(`FileHandle.${method}`); await later(); } return original.call(this); };
}
syncBuiltinESMExports();
