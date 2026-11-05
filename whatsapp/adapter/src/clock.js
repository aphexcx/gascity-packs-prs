// clock.js — the adapter's one clock, and the test-only kill points.
//
// Every schedule in the adapter (the poll, the receipt read-back, the
// in-doubt grace, the 24-hour pending deadline, the watermark read, the
// keeper's marks) is a function of journaled times and this clock, never of
// a timer's memory. In production the clock is Date.now(). The tests set
// WHATSAPP_TEST_CLOCK to a file holding epoch milliseconds that they
// advance; the file is read at every call, so a SIGKILLed child restarted
// on the same file comes back at the advanced time. Unset in production.

import { readFileSync } from 'node:fs';

export function createClock(env = process.env) {
  const file = env.WHATSAPP_TEST_CLOCK;
  if (!file) return () => Date.now();
  return () => {
    const value = Number(readFileSync(file, 'utf8').trim());
    if (!Number.isFinite(value)) throw new Error(`WHATSAPP_TEST_CLOCK ${file} does not hold epoch milliseconds`);
    return value;
  };
}

// killPoint is a test-only failpoint: when WHATSAPP_TEST_KILL_AT names this
// point (optionally "<point>@<rid>"), the process SIGKILLs itself there, so
// the crash-recovery tests place a kill exactly between two journal lines.
// Unset in production; a no-op then.
export function killPoint(name, rid = '', env = process.env) {
  const spec = env.WHATSAPP_TEST_KILL_AT;
  if (!spec) return;
  const [point, only] = spec.split('@');
  if (point !== name) return;
  if (only && only !== rid) return;
  process.kill(process.pid, 'SIGKILL');
}
