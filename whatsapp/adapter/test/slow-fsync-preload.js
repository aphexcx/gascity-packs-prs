// A preload for the adapter process (node --import), used by
// slow-flush.test.js: every fsync and fdatasync the process makes on a
// stopwatch file, an attempt marker or a marker directory takes
// WA_SLOW_FSYNC_MS (200 ms by default) longer: fs.fsyncSync, fs.fsync,
// fs.fdatasyncSync, fs.fdatasync, and a FileHandle's sync and datasync.
// WA_SLOW_FSYNC_MATCH replaces the path rule ('.' slows every flush).
// WA_SLOW_FSYNC_LOG names a file that gets one line per slowed flush, so a
// test can show the preload took effect.
//
// Not every flush: the inbound journal's write-ahead flush before the POST to
// the city is the base's own design (journal.js), so slowing it would slow
// every tree alike and measure nothing of the stopwatch.

import fs from 'node:fs';
import fsp from 'node:fs/promises';
import { syncBuiltinESMExports } from 'node:module';
import { fileURLToPath } from 'node:url';

const ms = Number(process.env.WA_SLOW_FSYNC_MS || 200);
const rule = new RegExp(process.env.WA_SLOW_FSYNC_MATCH
  || '(?:/stopwatch-\\d{4}-\\d{2}\\.jsonl|/(?:ack|words)-attempts(?:/[0-9a-f]{64})?)$');
const slow = new Set();
const name = (file) => (file instanceof URL ? fileURLToPath(file) : String(file));
const note = (file, fd) => { if (rule.test(name(file))) slow.add(fd); else slow.delete(fd); };
const block = () => Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
const later = () => new Promise((resolve) => setTimeout(resolve, ms));
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
    slowed(method); setTimeout(() => original.call(this, fd, cb), ms);
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
