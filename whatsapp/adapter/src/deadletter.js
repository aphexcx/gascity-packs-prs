// deadletter.js — <city>/.gc/whatsapp/inbound_dead_letter/<wa_id>.jsonl
// (0700/0600), one line per dead-lettered request id holding its envelope.
//
// The dead-letter file line is written BEFORE the dead journal line (the
// gate r7's second rule), and the write is idempotent per rid: the index of
// every rid already in a file is loaded at start, so a kill between the file
// line and the journal line is repeated on restart with no second file line.

import fs from 'node:fs';
import path from 'node:path';

import { readLines } from './journal.js';

export const DEAD_LETTER_DIR = 'inbound_dead_letter';

export function safeWaId(waId) {
  const digits = String(waId ?? '').replace(/[^0-9A-Za-z_-]/g, '');
  return digits || 'unknown';
}

export function createDeadLetters({ stateDir }) {
  const dir = path.join(stateDir, DEAD_LETTER_DIR);
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  try { fs.chmodSync(dir, 0o700); } catch { /* best effort */ }
  const index = new Map();
  for (const name of fs.readdirSync(dir)) {
    if (!name.endsWith('.jsonl')) continue;
    const file = path.join(dir, name);
    const { lines, tornAt } = readLines(file);
    if (tornAt >= 0) fs.truncateSync(file, tornAt);
    for (const line of lines) if (line?.rid && !index.has(line.rid)) index.set(line.rid, line);
  }

  return {
    dir,
    has: (rid) => index.has(rid),
    get: (rid) => index.get(rid),
    entries: () => [...index.values()],
    count: () => index.size,
    // Returns true when a line was written, false when the rid already had one.
    write(waId, line) {
      if (index.has(line.rid)) return false;
      const file = path.join(dir, `${safeWaId(waId)}.jsonl`);
      const fd = fs.openSync(file, 'a', 0o600);
      try {
        fs.writeSync(fd, `${JSON.stringify(line)}\n`);
        fs.fsyncSync(fd);
      } finally {
        fs.closeSync(fd);
      }
      fs.chmodSync(file, 0o600);
      index.set(line.rid, line);
      return true;
    },
  };
}
