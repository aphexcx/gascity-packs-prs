import fs from 'node:fs';
import fsp from 'node:fs/promises';
import path from 'node:path';
import { createHash } from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { createMarker, flushMarker, pruneMarkers } from './arrival.js';

const SOURCE_ROOT = path.dirname(fileURLToPath(import.meta.url));
// A words marker matters while the publisher may still retry: 7 days for a
// text, 30 for a file send (outbound.js); one day past the longer.
export const WORDS_MARKER_MAX_AGE_MS = 31 * 24 * 60 * 60 * 1000;
export const RELEASE_WARNING = 'stopwatch: release unavailable; recorded as unknown';

export function releaseId(root = SOURCE_ROOT) {
  const files = [];
  const visit = (dir) => { for (const entry of fs.readdirSync(dir, { withFileTypes: true })) { const file = path.join(dir, entry.name); if (entry.isDirectory()) visit(file); else if (entry.isFile() && entry.name.endsWith('.js')) files.push(file); } };
  visit(root); const hash = createHash('sha256');
  for (const file of files.sort()) hash.update(path.relative(root, file)).update('\0').update(fs.readFileSync(file)).update('\0');
  return hash.digest('hex').slice(0, 12);
}

const eventKey = (e) => e.message && ['inbound', 'reaction', 'words'].includes(e.event) ? `${e.event}\0${e.event === 'words' ? '' : e.conversation}\0${e.message}` : '';

// Lines are appended in order through node:fs/promises, off the caller's
// thread: record returns at once and no delivery or send waits on a flush.
// "First only" is decided at record, so a key is queued once. A line lost to
// a failed write is a lost measurement, never a lost message.
export function createStopwatch({ stateDir, release, releaseRoot = SOURCE_ROOT, clock = Date.now, log = () => {} }) {
  const seen = new Set(); let warned = false;
  const warn = () => { if (!warned) { warned = true; log('stopwatch: state file unavailable; adapter continues'); } };
  if (release === undefined) {
    // The stopwatch never breaks the adapter: an unhashable release is unknown.
    try { release = releaseId(releaseRoot); } catch { release = 'unknown'; log(RELEASE_WARNING); }
  }
  try {
    for (const file of fs.readdirSync(stateDir).filter((name) => /^stopwatch-\d{4}-\d{2}\.jsonl$/.test(name)).sort()) {
      for (const line of fs.readFileSync(path.join(stateDir, file), 'utf8').split('\n')) {
        try { const key = eventKey(JSON.parse(line)); if (key) seen.add(key); } catch { /* torn line */ }
      }
    }
  } catch (err) { if (err.code !== 'ENOENT') warn(); }
  const pending = new Set();
  const track = (work) => { pending.add(work); work.finally(() => pending.delete(work)); return work; };
  // One chain for every flush made for the stopwatch and the attempt markers
  // (the lines, the words markers, and the arrival task's ack markers). A real
  // slow fsync holds a thread of Node's four-thread pool; on one chain these
  // hold at most one, so a send's exclusive create never queues behind them.
  // The returned promise carries the work's own outcome; the chain goes on.
  let chain = Promise.resolve();
  const serial = (work) => {
    const run = chain.then(work);
    chain = run.catch(() => {});
    track(chain);
    return run;
  };
  const append = async (e, at) => {
    await fsp.mkdir(stateDir, { recursive: true, mode: 0o700 });
    const handle = await fsp.open(path.join(stateDir, `stopwatch-${at.toISOString().slice(0, 7)}.jsonl`), 'a+', 0o600);
    try {
      await handle.chmod(0o600); const { size } = await handle.stat();
      if (size) { const last = Buffer.alloc(1); await handle.read(last, 0, 1, size - 1); if (last[0] !== 10) await handle.write('\n'); }
      await handle.write(`${JSON.stringify(e)}\n`); await handle.sync();
    } finally { await handle.close(); }
  };
  const record = (input) => {
    // The whitelist prevents an accidental envelope spread from persisting text.
    const e = { event: input.event, channel: 'whatsapp', conversation: input.conversation, at: input.at };
    for (const field of ['message', 'thread', 'message_at', 'actor', 'choice']) if (input[field] !== undefined) e[field] = input[field];
    if (e.event === 'inbound') e.release = release;
    if (e.event === 'words') e.acceptance_uncertain = Boolean(input.acceptance_uncertain);
    const key = eventKey(e); if (key && seen.has(key)) return;
    const at = new Date(e.at); if (!Number.isFinite(at.getTime())) return;
    if (key) seen.add(key);
    serial(() => append(e, at)).catch(() => warn());
  };
  const prune = () => track(pruneMarkers(path.join(stateDir, 'words-attempts'), WORDS_MARKER_MAX_AGE_MS).catch(() => {}));
  prune();
  return {
    record,
    // RULING 1 of round 1: a send may wait for its marker to be created, never
    // for its flush, which follows off the send path. The time is uncertain
    // unless this attempt created the marker; a failed reservation makes it
    // uncertain without failing a send.
    async attempt(key) {
      let marker;
      try { marker = await createMarker(stateDir, 'words-attempts', key); } catch { warn(); return true; }
      if (!marker) return true;
      serial(() => flushMarker(marker)).catch(() => warn());
      return false;
    },
    words(conversation, message, uncertain) { record({ event: 'words', conversation, message, at: new Date(clock()).toISOString(), acceptance_uncertain: uncertain }); },
    prune,
    serial,
    // Waits until every queued line, marker flush and prune has settled.
    async flush() { while (pending.size) await Promise.all([...pending]); },
  };
}
