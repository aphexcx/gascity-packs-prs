import fs from 'node:fs';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { reserve } from './arrival.js';

export function releaseId(root = path.dirname(fileURLToPath(import.meta.url))) {
  const files = [];
  const visit = (dir) => { for (const entry of fs.readdirSync(dir, { withFileTypes: true })) { const file = path.join(dir, entry.name); if (entry.isDirectory()) visit(file); else if (entry.isFile() && entry.name.endsWith('.js')) files.push(file); } };
  visit(root); const hash = createHash('sha256');
  for (const file of files.sort()) hash.update(path.relative(root, file)).update('\0').update(fs.readFileSync(file)).update('\0');
  return hash.digest('hex').slice(0, 12);
}

const eventKey = (e) => e.message && ['inbound', 'reaction', 'words'].includes(e.event) ? `${e.event}\0${e.event === 'words' ? '' : e.conversation}\0${e.message}` : '';

export function createStopwatch({ stateDir, release = releaseId(), clock = Date.now, log = () => {} }) {
  const seen = new Set(); let warned = false;
  const warn = () => { if (!warned) { warned = true; log('stopwatch: state file unavailable; adapter continues'); } };
  try {
    for (const file of fs.readdirSync(stateDir).filter((name) => /^stopwatch-\d{4}-\d{2}\.jsonl$/.test(name)).sort()) {
      for (const line of fs.readFileSync(path.join(stateDir, file), 'utf8').split('\n')) {
        try { const key = eventKey(JSON.parse(line)); if (key) seen.add(key); } catch { /* torn line */ }
      }
    }
  } catch (err) { if (err.code !== 'ENOENT') warn(); }
  const record = (input) => {
    // The whitelist prevents an accidental envelope spread from persisting text.
    const e = { event: input.event, channel: 'whatsapp', conversation: input.conversation, at: input.at };
    for (const field of ['message', 'thread', 'message_at', 'actor', 'choice']) if (input[field] !== undefined) e[field] = input[field];
    if (e.event === 'inbound') e.release = release;
    if (e.event === 'words') e.acceptance_uncertain = Boolean(input.acceptance_uncertain);
    const key = eventKey(e); if (key && seen.has(key)) return;
    const at = new Date(e.at); if (!Number.isFinite(at.getTime())) return;
    try {
      fs.mkdirSync(stateDir, { recursive: true, mode: 0o700 });
      const fd = fs.openSync(path.join(stateDir, `stopwatch-${at.toISOString().slice(0, 7)}.jsonl`), 'a+', 0o600);
      try {
        fs.fchmodSync(fd, 0o600); const size = fs.fstatSync(fd).size;
        if (size) { const last = Buffer.alloc(1); fs.readSync(fd, last, 0, 1, size - 1); if (last[0] !== 10) fs.writeSync(fd, '\n'); }
        fs.writeSync(fd, `${JSON.stringify(e)}\n`); fs.fsyncSync(fd);
      } finally { fs.closeSync(fd); }
      if (key) seen.add(key);
    } catch { warn(); }
  };
  return {
    record,
    // A failed reservation makes the time uncertain without failing a send.
    attempt(key) { try { return !reserve(stateDir, 'words-attempts', key); } catch { warn(); return true; } },
    words(conversation, message, uncertain) { record({ event: 'words', conversation, message, at: new Date(clock()).toISOString(), acceptance_uncertain: uncertain }); },
  };
}
