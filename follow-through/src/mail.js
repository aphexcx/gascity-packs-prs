import { spawn } from 'node:child_process';

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
    child.once('error', e => resolve({ ok: false, error: `mail spawn ${e.code || 'error'}` }));
    child.once('close', (code, signal) => resolve({ ok: code === 0, error: code === 0 ? null : `mail ${signal || `exit ${code}`}` }));
  });
}
