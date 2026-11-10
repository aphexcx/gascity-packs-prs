// Files out: validate the real path and immutable byte snapshot BEFORE any network call.
// No package dependency. These are format signatures, not decoders or transcoders.
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';

export const mediaTypes = JSON.parse(fs.readFileSync(new URL('../../media-types.json', import.meta.url), 'utf8'));
const reject = (message) => { throw new Error(message); };
const inside = (root, file) => { const rel = path.relative(root, file); return rel !== '' && rel !== '..' && !rel.startsWith(`..${path.sep}`) && !path.isAbsolute(rel); };

// Inspect the OPEN descriptor's kernel pathname, never re-resolve the pathname
// used to open it. O_NOFOLLOW alone cannot fence a swapped parent directory.
// Linux exposes this through procfs; Darwin's shipped lsof queries the vnode
// path (PROC_PIDFDVNODEPATHINFO). Unsupported/unavailable inspection fails closed.
export function descriptorPath(fd) {
  let name;
  try {
    if (process.platform === 'linux') name = fs.readlinkSync(`/proc/self/fd/${fd}`);
    else if (process.platform === 'darwin') {
      const result = execFileSync('/usr/sbin/lsof', ['-nP', '-a', '-p', String(process.pid), '-d', String(fd), '-Fn0'],
        { encoding: 'utf8', timeout: 5000, maxBuffer: 65536, stdio: ['ignore', 'pipe', 'pipe'] });
      const fields = result.split('\0').map((s) => s.replace(/^\n/, ''));
      const names = fields.filter((s) => s.startsWith('n'));
      if (!fields.includes(`p${process.pid}`) || !fields.includes(`f${fd}`) || names.length !== 1) reject('descriptor lookup failed');
      name = names[0].slice(1);
    }
  } catch { reject('file refused: kernel descriptor path inspection unavailable'); }
  if (!name || !path.isAbsolute(name) || /[\x00-\x1f\x7f]/.test(name)) reject('file refused: kernel descriptor path inspection unavailable');
  return name;
}

function denied(file) {
  const parts = file.split(path.sep).map((p) => p.toLowerCase());
  const name = parts.at(-1);
  if (parts.some((p) => ['secrets', '.ssh', '.config'].includes(p))) return 'a denied directory (secrets, .ssh or .config)';
  if (parts.some((p, i) => p === '.gc' && ['services', 'keys'].includes(parts[i + 1]))) return 'a denied directory (.gc/services or .gc/keys)';
  if (name.endsWith('.env') || name.endsWith('.pem') || name.startsWith('id_')) return 'a secret/key filename (.env, .pem or id_)';
  return '';
}

export function outboundRoots({ cityDir, mediaDir, configured } = {}) {
  if (configured !== undefined && (typeof configured !== 'string' || !configured.trim())) reject('WHATSAPP_OUTBOUND_ROOTS must contain absolute directories');
  const roots = configured === undefined ? [cityDir, mediaDir].filter(Boolean) : configured.split(path.delimiter);
  const resolved = [];
  for (const root of roots) {
    if (!path.isAbsolute(root)) reject('WHATSAPP_OUTBOUND_ROOTS must contain absolute directories');
    try {
      const real = fs.realpathSync(root);
      if (!fs.statSync(real).isDirectory()) reject('WHATSAPP_OUTBOUND_ROOTS entry is not a directory');
      resolved.push(real);
    } catch (err) {
      // The default inbound store need not exist before the first incoming file.
      if (configured === undefined && root === mediaDir && err.code === 'ENOENT') continue;
      reject('WHATSAPP_OUTBOUND_ROOTS entry cannot be resolved');
    }
  }
  if (!resolved.length) reject('no allowed outbound roots configured');
  return resolved;
}

export function readAllowedFile(filename, { roots, maxBytes = 100000000 } = {}) {
  if (typeof filename !== 'string' || !path.isAbsolute(filename)) reject('file path must be absolute');
  const supplied = path.resolve(filename);
  const reason = denied(supplied);
  if (reason) reject(`file refused: ${reason}`);
  const real = fs.realpathSync(supplied);
  const realReason = denied(real);
  if (realReason) reject(`file refused: ${realReason}`);
  if (!roots?.some((root) => inside(root, real))) reject('file refused: resolved path is outside WHATSAPP_OUTBOUND_ROOTS');
  const before = fs.statSync(real);
  if (!before.isFile()) reject('file must be a regular file');
  if (before.size <= 0 || before.size > maxBytes) reject(`file size must be 1..${maxBytes} bytes`);
  const fd = fs.openSync(real, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  try {
    const opened = fs.fstatSync(fd);
    if (!opened.isFile() || opened.ino !== before.ino || opened.dev !== before.dev || opened.size !== before.size
      || fs.realpathSync(supplied) !== real || fs.realpathSync(real) !== real) reject('file changed while opening; retry');
    if (opened.nlink !== 1) reject('file refused: hard-linked files are not allowed');
    if (descriptorPath(fd) !== real) reject('file refused: opened descriptor is outside the validated path');
    // A bounded read of the opened inode, never a second path open or unbounded readFile.
    const bytes = Buffer.alloc(opened.size);
    let offset = 0;
    while (offset < bytes.length) {
      const n = fs.readSync(fd, bytes, offset, bytes.length - offset, offset);
      if (!n) reject('file changed while reading; retry');
      offset += n;
    }
    const after = fs.fstatSync(fd);
    const current = fs.statSync(real);
    if (after.size !== opened.size || after.mtimeMs !== opened.mtimeMs || after.ctimeMs !== opened.ctimeMs
      || current.ino !== opened.ino || current.dev !== opened.dev || fs.realpathSync(supplied) !== real) reject('file changed while reading; retry');
    if (descriptorPath(fd) !== real) reject('file refused: opened descriptor moved while reading');
    return { bytes, name: path.basename(real), realPath: real };
  } finally { fs.closeSync(fd); }
}

const ascii = (b, start, end) => b.toString('ascii', start, end);
const starts = (b, hex) => b.subarray(0, hex.length / 2).equals(Buffer.from(hex, 'hex'));

function zipKind(b) {
  // Read bounded central-directory entries, not arbitrary filename-looking bytes.
  let end = -1;
  for (let i = b.length - 22; i >= Math.max(0, b.length - 65557); i--) {
    if (b.readUInt32LE(i) === 0x06054b50 && i + 22 + b.readUInt16LE(i + 20) === b.length) { end = i; break; }
  }
  if (end < 0 || b.readUInt16LE(end + 4) || b.readUInt16LE(end + 6)) return null;
  const count = b.readUInt16LE(end + 10);
  let pos = b.readUInt32LE(end + 16);
  const finish = pos + b.readUInt32LE(end + 12);
  if (finish !== end || count > 10000) return null;
  const names = new Set();
  for (let i = 0; i < count; i++) {
    if (pos + 46 > finish || b.readUInt32LE(pos) !== 0x02014b50 || (b.readUInt16LE(pos + 8) & 1)) return null;
    const n = b.readUInt16LE(pos + 28), extra = b.readUInt16LE(pos + 30), comment = b.readUInt16LE(pos + 32);
    if (pos + 46 + n + extra + comment > finish) return null;
    names.add(b.toString('utf8', pos + 46, pos + 46 + n));
    pos += 46 + n + extra + comment;
  }
  if (pos !== finish || !names.has('[Content_Types].xml')) return null;
  const hits = [['word/document.xml', 'application/vnd.openxmlformats-officedocument.wordprocessingml.document'],
    ['xl/workbook.xml', 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'],
    ['ppt/presentation.xml', 'application/vnd.openxmlformats-officedocument.presentationml.presentation']].filter(([n]) => names.has(n));
  return hits.length === 1 ? hits[0][1] : null;
}

function compoundKind(b) {
  // Follow the CFB FAT/DIFAT directory chain, including later directory sectors.
  // Never follow an out-of-file sector, repeated sector or an unbounded chain.
  if (b.length < 512 || b.readUInt16LE(28) !== 0xfffe) return null;
  const shift = b.readUInt16LE(30);
  if (![9, 12].includes(shift)) return null;
  const sector = 2 ** shift, total = b.length / sector - 1, entries = sector / 4;
  if (!Number.isInteger(total) || total < 1) return null;
  const valid = (sid) => sid < total;
  const at = (sid) => (sid + 1) * sector;
  const fatCount = b.readUInt32LE(44), difatCount = b.readUInt32LE(72);
  if (!fatCount || fatCount > total || difatCount > total) return null;
  const fat = [];
  for (let i = 0; i < 109; i++) { const sid = b.readUInt32LE(76 + i * 4); if (sid !== 0xffffffff) fat.push(sid); }
  let difat = b.readUInt32LE(68);
  const seenDifat = new Set();
  for (let i = 0; i < difatCount; i++) {
    if (!valid(difat) || seenDifat.has(difat)) return null;
    seenDifat.add(difat);
    for (let j = 0; j < entries - 1; j++) { const sid = b.readUInt32LE(at(difat) + j * 4); if (sid !== 0xffffffff) fat.push(sid); }
    difat = b.readUInt32LE(at(difat) + sector - 4);
  }
  const fatSectors = new Set(fat);
  if (fat.length !== fatCount || fat.some((sid) => !valid(sid)) || fatSectors.size !== fat.length) return null;
  const names = [], seen = new Set();
  let dir = b.readUInt32LE(48);
  while (dir !== 0xfffffffe) {
    if (!valid(dir) || seen.has(dir) || fatSectors.has(dir) || seenDifat.has(dir)) return null;
    seen.add(dir);
    for (let p = at(dir); p < at(dir) + sector; p += 128) {
      const n = b.readUInt16LE(p + 64);
      if (n >= 2 && n <= 64 && n % 2 === 0 && b[p + 66] === 2) names.push(b.toString('utf16le', p, p + n - 2));
    }
    const fatSector = fat[Math.floor(dir / entries)];
    if (fatSector === undefined) return null;
    dir = b.readUInt32LE(at(fatSector) + (dir % entries) * 4);
  }
  const hits = [['WordDocument', 'application/msword'], ['Workbook', 'application/vnd.ms-excel'],
    ['Book', 'application/vnd.ms-excel'], ['PowerPoint Document', 'application/vnd.ms-powerpoint']].filter(([n]) => names.includes(n));
  const kinds = [...new Set(hits.map(([, mime]) => mime))];
  return kinds.length === 1 ? kinds[0] : null;
}

function mp4Kind(b) {
  const tracks = new Set();
  let brand = '';
  let boxes = 0;
  function walk(start, end, depth) {
    if (depth > 8) return false;
    for (let p = start; p < end;) {
      if (p + 8 > end || ++boxes > 10000) return false;
      let size = b.readUInt32BE(p), head = 8;
      const type = ascii(b, p + 4, p + 8);
      if (size === 1) { if (p + 16 > end) return false; const big = b.readBigUInt64BE(p + 8); if (big > BigInt(b.length)) return false; size = Number(big); head = 16; }
      if (size === 0) size = end - p;
      if (size < head || p + size > end) return false;
      if (type === 'ftyp' && size >= head + 8) brand = ascii(b, p + head, p + head + 4);
      if (['moov', 'trak', 'mdia'].includes(type) && !walk(p + head, p + size, depth + 1)) return false;
      if (type === 'hdlr' && size >= head + 12) tracks.add(ascii(b, p + head + 8, p + head + 12));
      p += size;
    }
    return true;
  }
  if (!walk(0, b.length, 0) || !brand) return null;
  if (tracks.has('vide')) return brand.startsWith('3g') ? 'video/3gpp' : 'video/mp4';
  return tracks.has('soun') ? 'audio/mp4' : null;
}

export function sniffMime(b) {
  if (starts(b, 'ffd8ff')) return 'image/jpeg';
  if (starts(b, '89504e470d0a1a0a') && b.length >= 33 && ascii(b, 12, 16) === 'IHDR'
    && b[24] === 8 && [2, 6].includes(b[25])) return 'image/png';
  if (b.length >= 20 && ascii(b, 0, 4) === 'RIFF' && ascii(b, 8, 12) === 'WEBP') {
    if (b.readUInt32LE(4) + 8 !== b.length) return null;
    let image = false;
    for (let p = 12; p + 8 <= b.length;) {
      const kind = ascii(b, p, p + 4), n = b.readUInt32LE(p + 4);
      if (p + 8 + n > b.length || ['ANIM', 'ANMF'].includes(kind)) return null;
      if (kind === 'VP8X' && (n < 10 || (b[p + 8] & 2) || b.readUIntLE(p + 12, 3) >= 4096 || b.readUIntLE(p + 15, 3) >= 4096)) return null;
      if (kind === 'VP8 ' || kind === 'VP8L') image = true;
      p += 8 + n + (n % 2);
    }
    return image ? 'image/webp' : null;
  }
  if (ascii(b, 0, 5) === '%PDF-') return 'application/pdf';
  if (ascii(b, 0, 6) === '#!AMR\n') return 'audio/amr';
  if (ascii(b, 0, 4) === 'OggS' && b.length >= 47 && b[4] === 0) {
    const start = 27 + b[26];
    return start + 19 <= b.length && ascii(b, start, start + 8) === 'OpusHead' && b[start + 9] === 1 ? 'audio/ogg' : null;
  }
  if (b.length >= 7 && b[0] === 0xff && (b[1] & 0xf6) === 0xf0) return 'audio/aac';
  let mp3 = 0;
  if (ascii(b, 0, 3) === 'ID3' && b.length >= 10 && b.subarray(6, 10).every((v) => v < 128)) {
    mp3 = 10 + ((b[6] << 21) | (b[7] << 14) | (b[8] << 7) | b[9]);
  }
  if (b.length >= mp3 + 4 && b[mp3] === 0xff && (b[mp3 + 1] & 0xe0) === 0xe0
    && (b[mp3 + 1] & 6) !== 0 && (b[mp3 + 2] & 0xf0) !== 0xf0 && (b[mp3 + 2] & 0x0c) !== 0x0c) return 'audio/mpeg';
  if (b.length >= 12 && ascii(b, 4, 8) === 'ftyp') return mp4Kind(b);
  if (starts(b, '504b0304')) return zipKind(b);
  if (starts(b, 'd0cf11e0a1b11ae1')) return compoundKind(b);
  try {
    const text = new TextDecoder('utf-8', { fatal: true }).decode(b);
    if (text && !/[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]/.test(text)) return 'text/plain';
  } catch { /* not plain UTF-8 */ }
  return null;
}

export function prepareFile(filename, options) {
  const file = readAllowedFile(filename, options);
  if (file.name.length > 255 || /[\x00-\x1f\x7f]/.test(file.name)) reject('file name is not supported');
  const mime = sniffMime(file.bytes), rule = mediaTypes[mime];
  if (!rule) reject('unrecognized or unsupported file signature');
  if (!rule.extensions.includes(path.extname(file.name).toLowerCase())) reject('file extension does not match its signature');
  if (file.bytes.length > rule.maxBytes) reject(`file exceeds the ${rule.maxBytes} byte cap for ${rule.kind}`);
  const sha256 = crypto.createHash('sha256').update(file.bytes).digest('hex');
  return { ...file, mime, kind: rule.kind, size: file.bytes.length, sha256 };
}

export function readCaption(filename, options) {
  const { bytes } = readAllowedFile(filename, { ...options, maxBytes: 4096 });
  return new TextDecoder('utf-8', { fatal: true }).decode(bytes);
}

export function fileTranscript(file, caption = '') {
  return `[file sent: ${JSON.stringify(file.name)}; ${file.size} bytes; sha256 ${file.sha256}]${caption ? `\n${caption}` : ''}`;
}
