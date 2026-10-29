// Diagnostic only: never source the credential file or print its contents.
import { createHash } from 'node:crypto';
import { readFileSync, statSync } from 'node:fs';
import http from 'node:http';
import { createRequire } from 'node:module';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const pack = process.env.GC_PACK_DIR || path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
function finish(code, message) {
  console.log(message);
  process.exitCode = code;
}

function dependencies() {
  const adapter = path.join(pack, 'adapter');
  try {
    const expected = createHash('sha256').update(readFileSync(path.join(adapter, 'pnpm-lock.yaml'))).digest('hex');
    const installed = readFileSync(path.join(adapter, 'node_modules/.gc-deps-ok'), 'utf8').trim();
    if (installed !== expected) throw new Error('stale');
    createRequire(path.join(adapter, 'package.json')).resolve('@wecom/aibot-node-sdk');
  } catch {
    return finish(2, 'dependencies missing or lockfile marker stale; run adapter/run.sh --deps-only in this pack');
  }
  finish(0, 'installed SDK and lockfile hash marker match');
}

function env() {
  const e = process.env;
  const secrets = e.GC_SERVICE_SECRETS_DIR || (e.GC_CITY_PATH && path.join(e.GC_CITY_PATH, '.gc/services/wecom/secrets'));
  const file = e.GC_WECOM_ADAPTER_ENV || (secrets ? path.join(secrets, 'env') : path.join(e.HOME || '', '.config/gc-wecom-adapter/env'));
  let text;
  try { text = readFileSync(file, 'utf8'); }
  catch { return finish(2, 'adapter env file missing or unreadable; set GC_WECOM_ADAPTER_ENV or create the per-city wecom secrets/env file'); }
  const names = ['WECOM_BOT_ID', 'WECOM_BOT_SECRET', 'GC_CITY_NAME', 'GC_API_BASE_URL'];
  const values = Object.fromEntries(names.map(name => [name, e[name] || '']));
  const dynamic = new Set();
  for (const line of text.split('\n')) {
    const match = line.match(/^\s*(?:export\s+)?([A-Z_]+)=(.*)$/);
    if (!match || !names.includes(match[1])) continue;
    const [, name, raw] = match;
    // The runtime sources shell. Only literal assignments are verifiable
    // here; expansion or commands need an operator check, never execution.
    const literal = raw.trim().match(/^(?:'([^']*)'|"([^"$`\\]*)"|([^\s'"$`\\;|&<>#()]*))\s*(?:#.*)?$/);
    if (!literal) { dynamic.add(name); continue; }
    dynamic.delete(name);
    values[name] = literal[1] ?? literal[2] ?? literal[3];
  }
  const missing = names.filter(name => !values[name] && !dynamic.has(name));
  if (missing.length) return finish(2, `missing adapter env vars: ${missing.join(' ')}`);
  if (dynamic.size) return finish(1, `cannot verify shell expressions without executing them: ${[...dynamic].join(' ')}`);
  finish(0, `adapter env vars present: ${names.join(' ')} (values withheld)`);
}

function socket() {
  const socketPath = process.env.GC_SERVICE_SOCKET;
  if (!socketPath) return finish(1, 'GC_SERVICE_SOCKET is unset; pass the running service socket path from its startup log to check /healthz');
  if (!path.isAbsolute(socketPath) || Buffer.byteLength(socketPath) >= 104) {
    return finish(2, 'GC_SERVICE_SOCKET must be an absolute Unix socket path shorter than 104 bytes');
  }
  try {
    if (!statSync(socketPath).isSocket()) throw new Error('not a socket');
  } catch { return finish(2, 'GC_SERVICE_SOCKET does not name an existing Unix socket'); }
  let done = false;
  const end = (code, message) => {
    if (done) return;
    done = true;
    clearTimeout(timer);
    finish(code, message);
    request.destroy();
  };
  const request = http.get({ socketPath, path: '/healthz' }, response => {
    response.resume();
    end(response.statusCode === 200 ? 0 : 2,
      response.statusCode === 200 ? 'Unix socket /healthz returns 200 (local readiness only; bot authentication not proven)' : 'Unix socket /healthz did not return 200');
  });
  const timer = setTimeout(() => end(2, 'Unix socket /healthz timed out'), 2000);
  request.on('error', () => end(2, 'Unix socket /healthz connection failed'));
}

const checks = { dependencies, env, socket };
if (!checks[process.argv[2]]) finish(2, 'unknown doctor check');
else checks[process.argv[2]]();
