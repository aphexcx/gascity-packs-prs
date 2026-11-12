import { spawn } from 'node:child_process';
import path from 'node:path';

export const MAIL_TIMEOUT_MS = 15_000;
export const MAX_MAIL_BODY_BYTES = 65_536;

export function assertMailBody(body) {
  if (Buffer.byteLength(body) > MAX_MAIL_BODY_BYTES) throw new Error('mail body exceeds 64 KiB');
  return body;
}

export function sendMail(raise, env = process.env) {
  assertMailBody(raise.body);
  return new Promise(resolve => {
    const args = ['mail', 'send', env.FOLLOW_THROUGH_AGENT || 'mayor', '--notify', '--dedup', raise.key,
      '-s', raise.subject, '-m', raise.body];
    // No shell, no owner transport, no API. Never copy gc output (which can
    // contain message text) into operator-facing errors or status.
    const child = spawn(env.GC_BIN || 'gc', args, { env, stdio: 'ignore', timeout: MAIL_TIMEOUT_MS, killSignal: 'SIGKILL' });
    child.once('error', e => resolve({ ok: false, unknown: false, error: `mail spawn ${e.code || 'error'}` }));
    child.once('close', (code, signal) => resolve({ ok: code === 0, unknown: code !== 0,
      error: code === 0 ? null : `mail ${signal || `exit ${code}`}` }));
  });
}

// The inbox-only send dedup cannot confirm an archived message. Query the
// city message store by our UUID key, including closed and ephemeral rows.
// A failed or malformed lookup is unknown, never permission to send again.
export function lookupMail(raise, env = process.env) {
  const city = env.GC_CITY_PATH || env.GC_CITY;
  if (!city || !path.isAbsolute(city)) return Promise.resolve({ ok: false, error: 'mail history needs GC_CITY_PATH' });
  return new Promise(resolve => {
    const args = ['bd', '--city', city, 'list', '--all', '--include-infra', '--include-ephemeral',
      '--type', 'message', '--metadata-field', `mail.dedup_key=${raise.key}`, '--limit', '1', '--brief', '--json'];
    const child = spawn(env.GC_BIN || 'gc', args, { env, stdio: ['ignore', 'pipe', 'ignore'],
      timeout: MAIL_TIMEOUT_MS, killSignal: 'SIGKILL' });
    let output = '';
    let bytes = 0;
    child.stdout.on('data', chunk => {
      bytes += chunk.length;
      if (bytes > MAX_MAIL_BODY_BYTES) { child.kill('SIGKILL'); return; }
      output += chunk.toString('utf8');
    });
    child.once('error', () => resolve({ ok: false, error: 'mail history unavailable' }));
    child.once('close', code => {
      if (code !== 0 || bytes > MAX_MAIL_BODY_BYTES) return resolve({ ok: false, error: 'mail history unavailable' });
      try {
        const rows = JSON.parse(output);
        if (!Array.isArray(rows) || rows.length > 1 || rows.some(r => !r || typeof r.id !== 'string' ||
            r.issue_type !== 'message' || r.metadata?.['mail.dedup_key'] !== raise.key)) throw new Error();
        resolve({ ok: true, found: rows.length > 0, id: rows[0]?.id });
      } catch { resolve({ ok: false, error: 'mail history invalid response' }); }
    });
  });
}
