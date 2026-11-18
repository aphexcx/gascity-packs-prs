'use strict';
// Diagnostic only: node archive-pipe-probe.cjs <source.cjs> <pipe-flags.node>.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const zlib = require('node:zlib');
const { execFileSync } = require('node:child_process');
const source = fs.readFileSync(process.argv[2], 'utf8');
const worker = source.slice(source.indexOf('function archiveWorker() {'), source.indexOf('\nfunction archiveSlice('));
const addon = path.resolve(process.argv[3]);
const options = { timeout: 2000, maxBuffer: 8 * 1024 * 1024 + 65536, stdio: ['ignore', 'pipe', 'pipe'] };
const inspect = `const flags = require(process.argv[4]); const fs = require('node:fs');
const snapshots = [{stage: 'start', ...flags()}];
fs.writeFileSync(process.argv[5], JSON.stringify(snapshots));
const write = fs.writeSync;
fs.writeSync = function(fd, ...args) {
  try { return write(fd, ...args); }
  catch (error) {
    if (fd === 1) {
      snapshots.push({stage: error.code, ...flags()});
      fs.writeFileSync(process.argv[5], JSON.stringify(snapshots));
    }
    throw error;
  }
};`;
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'archive-pipe-probe-'));
try {
  const archive = path.join(dir, 'archive.gz');
  const flags = path.join(dir, 'flags.json');
  fs.writeFileSync(archive, zlib.gzipSync(Buffer.alloc(9 * 1024 * 1024, 120)));
  for (let run = 1; run <= 40; run++) {
    try {
      const bytes = execFileSync(process.execPath, ['-e', inspect + `(${worker})()`, archive, '0', String(8 * 1024 * 1024), addon, flags], options);
      console.log(JSON.stringify({node: process.version, run, status: 0, bytes: bytes.length, flags: JSON.parse(fs.readFileSync(flags))}));
    } catch (error) {
      console.log(JSON.stringify({node: process.version, run, status: error.status, stderr: error.stderr.toString(), bytes: error.stdout.length, flags: JSON.parse(fs.readFileSync(flags))}));
    }
  }
} finally {
  fs.rmSync(dir, {recursive: true, force: true});
}
