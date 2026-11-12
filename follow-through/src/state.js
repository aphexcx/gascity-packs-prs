import fs from 'node:fs';
import path from 'node:path';
import { randomUUID } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { setTimeout as delay } from 'node:timers/promises';

export function resolveStateDir(env = process.env) {
  const dir = env.GC_PACK_STATE_DIR;
  if (!dir) throw new Error('GC_PACK_STATE_DIR is required; run through the city pack command or order');
  if (!path.isAbsolute(dir)) throw new Error('GC_PACK_STATE_DIR must be absolute');
  return dir;
}

const emptyState = () => ({ version: 1, owners: {}, items: [], reminders: [], raises: [], wakes: {}, last_tick_at: null });

export function readState(dir) {
  let raw;
  try { raw = fs.readFileSync(path.join(dir, 'state.json'), 'utf8'); }
  catch (e) { if (e.code === 'ENOENT') return emptyState(); throw e; }
  try {
    const state = JSON.parse(raw);
    if (state.version !== 1 || !state.owners || !state.wakes ||
        !['items', 'reminders', 'raises'].every(k => Array.isArray(state[k]))) throw new Error('unsupported schema');
    return state;
  } catch (e) { throw new Error(`cannot read follow-through state: ${e.message}`); }
}

// Both state and lock tickets are published whole. fsync the containing
// directory as well as the file so a power loss cannot forget the rename.
function replace(file, data) {
  const tmp = `${file}.${randomUUID()}.tmp`;
  let fd;
  try {
    fd = fs.openSync(tmp, 'wx', 0o600);
    fs.writeFileSync(fd, JSON.stringify(data, null, 2) + '\n');
    fs.fsyncSync(fd);
    fs.closeSync(fd);
    fd = undefined;
    fs.renameSync(tmp, file);
    const parent = fs.openSync(path.dirname(file), 'r');
    try { fs.fsyncSync(parent); } finally { fs.closeSync(parent); }
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
    try { fs.unlinkSync(tmp); } catch (e) { if (e.code !== 'ENOENT') throw e; }
  }
}

let ownBirth;
function processBirth(pid) {
  if (pid === process.pid && ownBirth !== undefined) return ownBirth;
  let birth;
  try {
    if (process.platform === 'linux') {
      // Kernel start ticks plus boot identity distinguish recycled PIDs,
      // including a reboot. comm may itself contain spaces or parentheses.
      const stat = fs.readFileSync(`/proc/${pid}/stat`, 'utf8');
      const start = stat.slice(stat.lastIndexOf(')') + 2).split(/\s+/)[19];
      const boot = fs.readFileSync('/proc/sys/kernel/random/boot_id', 'utf8').trim();
      birth = `${boot}:${start}`;
    } else {
      // macOS ships /bin/ps. Do not inspect command arguments or environments.
      birth = execFileSync('/bin/ps', ['-p', String(pid), '-o', 'lstart='], {
        encoding: 'utf8', timeout: 2000, stdio: ['ignore', 'pipe', 'ignore'],
        env: { ...process.env, LC_ALL: 'C', TZ: 'UTC' },
      }).trim();
    }
  } catch (e) {
    try { process.kill(pid, 0); }
    catch (gone) { if (gone.code === 'ESRCH') return null; throw gone; }
    throw new Error(`cannot verify lock process ${pid}: ${e.code || 'process lookup failed'}`);
  }
  if (!birth) return null;
  if (pid === process.pid) ownBirth = birth;
  return birth;
}

function tickets(dir) {
  const rows = [];
  for (const name of fs.readdirSync(dir).filter(n => /^lock-\d+-[a-f0-9-]+\.json$/.test(n))) {
    const file = path.join(dir, name);
    let row;
    try { row = JSON.parse(fs.readFileSync(file, 'utf8')); }
    catch (e) { if (e.code === 'ENOENT') continue; throw e; }
    if (row.birth && processBirth(row.pid) === row.birth) rows.push({ ...row, name });
    else {
      // Every ticket name is unique and never reused. Removing a dead
      // process's ticket cannot unlink a new writer's lock.
      try { fs.unlinkSync(file); } catch (e) { if (e.code !== 'ENOENT') throw e; }
    }
  }
  return rows;
}

// Lamport's bakery mutex, using atomic files on a LOCAL filesystem. A
// choosing ticket is published before reading any other ticket. Every
// contender then waits for earlier tickets, and for choosing to finish.
// Unlike unlinking a shared stale lock, dead-PID cleanup has no ABA race.
async function lock(dir) {
  fs.mkdirSync(dir, { recursive: true, mode: 0o700 });
  fs.chmodSync(dir, 0o700);
  const name = `lock-${process.pid}-${randomUUID()}.json`;
  const file = path.join(dir, name);
  const birth = processBirth(process.pid);
  const release = () => {
    try { fs.unlinkSync(file); } catch (e) { if (e.code !== 'ENOENT') throw e; }
  };
  try {
    replace(file, { pid: process.pid, birth, number: 0 });
    const number = Math.max(0, ...tickets(dir).map(t => t.number)) + 1;
    replace(file, { pid: process.pid, birth, number });
    const deadline = performance.now() + 120_000;
    while (tickets(dir).some(t => t.name !== name &&
      (t.number === 0 || t.number < number || (t.number === number && t.name < name)))) {
      if (performance.now() >= deadline) throw new Error('state is busy: another follow-through process holds its lock');
      await delay(20);
    }
    return release;
  } catch (e) { release(); throw e; }
}

export async function withState(dir, action) {
  const release = await lock(dir);
  try {
    const state = readState(dir);
    return await action(state, () => replace(path.join(dir, 'state.json'), state));
  } finally { release(); }
}
