import { spawn } from 'node:child_process';

export function sendMail(raise, env = process.env) {
  return new Promise(resolve => {
    const args = ['mail', 'send', env.FOLLOW_THROUGH_AGENT || 'mayor', '--notify', '--dedup', raise.key,
      '-s', raise.subject, '-m', raise.body];
    // No shell, no owner transport, no API. Never copy gc output (which can
    // contain message text) into operator-facing errors or status.
    const child = spawn(env.GC_BIN || 'gc', args, { env, stdio: 'ignore', timeout: 15_000, killSignal: 'SIGKILL' });
    child.once('error', e => resolve({ ok: false, error: `mail spawn ${e.code || 'error'}` }));
    child.once('close', (code, signal) => resolve({ ok: code === 0, error: code === 0 ? null : `mail ${signal || `exit ${code}`}` }));
  });
}
