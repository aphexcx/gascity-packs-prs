// A slowed flush never delays a delivery or a send (PR 104 round 2, item 5).
// The real adapter process on the fakes, with a preload (node --import,
// slow-fsync-preload.js) that adds 200 ms to every flush made for the
// stopwatch or an attempt marker. Measured with the switch off and on:
// from the fake gateway handing out a delivery to the fake city receiving its
// POST, and from a publish request to the fake gateway receiving its send.
// Each stays under 100 ms (the median of three, so one slow scheduling slot
// on a loaded host is not read as a flush on the path). A second test holds
// every stopwatch and marker flush (a stuck disk, holding threads of Node's
// pool) and requires every chunk of a long publish to be sent while they are
// held: structural, so it does not move with the host's load.

import test from 'node:test';
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import fs from 'node:fs';
import path from 'node:path';
import { performance } from 'node:perf_hooks';
import { adapterRequest, rig, waitFor } from './fakes.js';

const PRELOAD = new URL('./slow-fsync-preload.js', import.meta.url).href;
const OWNER = 'owner-example';
const LIMIT_MS = 100;
const SAMPLES = 3;
const median = (values) => [...values].sort((a, b) => a - b)[Math.floor(values.length / 2)];

// Wall-clock marks taken inside the fakes, without changing fakes.js.
function mark(r) {
  const handed = new Map(); const posted = new Map(); const sent = new Map(); const texts = [];
  class Stamping extends Map {
    values() {
      const now = performance.now();
      for (const d of super.values()) if (!r.gw.acked.has(d.id) && !handed.has(d.id)) handed.set(d.id, now);
      return super.values();
    }
  }
  r.gw.deliveries = new Stamping(r.gw.deliveries);
  r.gc.posts.push = function (...items) {
    const now = performance.now();
    for (const p of items) if (!posted.has(p.pmid)) posted.set(p.pmid, now);
    return Array.prototype.push.apply(this, items);
  };
  r.gw.sends.push = function (...items) {
    const now = performance.now();
    for (const s of items) if (s.type === 'text' && !sent.has(s.text)) { sent.set(s.text, now); texts.push({ text: s.text, at: now }); }
    return Array.prototype.push.apply(this, items);
  };
  return { handed, posted, sent, texts };
}

for (const mode of ['', 'eyes']) {
  test(`a slowed flush never delays a delivery or a send (ACK_REACTION=${mode || 'unset'})`, async (t) => {
    const r = await rig({ extra: { NODE_OPTIONS: `--import=${PRELOAD}`, ...(mode ? { ACK_REACTION: mode } : {}) } });
    t.after(() => r.close());
    const marks = mark(r);
    const slowedLog = path.join(r.dir, 'slowed-flushes.log');
    const slowed = () => (fs.existsSync(slowedLog) ? fs.readFileSync(slowedLog, 'utf8').split('\n').filter(Boolean).length : 0);
    r.start({ WA_SLOW_FSYNC_LOG: slowedLog });
    await waitFor(() => r.gc.registrations.length === 1, { what: 'the registration' });
    const deliver = async (n) => {
      const pmid = `message-example-slow-${n}`;
      const id = r.gw.addText(OWNER, pmid, `synthetic ${n}`);
      await waitFor(() => r.gw.acked.has(id), { what: `the ack of delivery ${n}` });
      return marks.posted.get(pmid) - marks.handed.get(id);
    };
    const publish = async (n) => {
      const text = `synthetic reply ${n}`;
      const start = performance.now();
      const answer = await adapterRequest(r.socket, 'POST', '/publish', { session_id: 'session-example', conversation: { conversation_id: OWNER }, text, idempotency_key: `publish-example-slow-${n}` });
      assert.equal(answer.json.delivered, true, `publish ${n} delivered`);
      return marks.sent.get(text) - start;
    };
    await deliver('warm');
    const deliveries = []; const sends = [];
    for (let i = 0; i < SAMPLES; i++) deliveries.push(await deliver(i));
    for (let i = 0; i < SAMPLES; i++) sends.push(await publish(i));
    const shown = (values) => values.map((v) => `${Math.round(v)} ms`).join(', ');
    // The control: the preload did slow the stopwatch's and the markers' flushes.
    await waitFor(() => slowed() > 0, { what: 'a slowed flush (the preload in effect)' });
    t.diagnostic(`delivery: ${shown(deliveries)}; send: ${shown(sends)}; slowed flushes: ${slowed()}`);
    assert.ok(median(deliveries) < LIMIT_MS, `a delivery waited on a flush: ${shown(deliveries)}`);
    assert.ok(median(sends) < LIMIT_MS, `a send waited on a flush: ${shown(sends)}`);
  });
}

// Every chunk of a long publish is sent while the stopwatch's and the markers'
// flushes are held. Each held flush occupies a thread of Node's four-thread
// pool, as a stuck disk does; a send waits only for its marker's create, which
// needs a free pool thread. Flushes made in parallel would take every thread
// and stall the publish; on one chain they hold at most one.
for (const mode of ['', 'eyes']) {
  test(`every chunk of a publish is sent while the flushes are held (ACK_REACTION=${mode || 'unset'})`, async (t) => {
    const r = await rig({ extra: { NODE_OPTIONS: `--import=${PRELOAD}`, ...(mode ? { ACK_REACTION: mode } : {}) } });
    const fifo = path.join(r.dir, 'hold.fifo');
    const released = path.join(r.dir, 'released');
    execFileSync('mkfifo', [fifo]);
    const release = async () => {
      fs.writeFileSync(released, '');
      // Each open by a writer frees every reader blocked in open; closing it
      // ends their reads. Repeat until no reader holds the pipe (ENXIO).
      for (let i = 0; i < 200; i++) {
        let fd;
        try { fd = fs.openSync(fifo, fs.constants.O_WRONLY | fs.constants.O_NONBLOCK); } catch { break; }
        fs.closeSync(fd);
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    };
    t.after(async () => { await release(); await r.close(); });
    const heldLog = path.join(r.dir, 'held-flushes.log');
    const held = () => (fs.existsSync(heldLog) ? fs.readFileSync(heldLog, 'utf8').split('\n').filter((l) => l === 'held').length : 0);
    r.start({ WA_HOLD_FSYNC_FIFO: fifo, WA_HOLD_FSYNC_RELEASED: released, WA_SLOW_FSYNC_LOG: heldLog });
    await waitFor(() => r.gc.registrations.length === 1, { what: 'the registration' });
    // The owner writes first, which opens the conversation's window.
    const warm = r.gw.addText(OWNER, 'message-example-held', 'synthetic held');
    await waitFor(() => r.gw.acked.has(warm), { what: 'the ack of the owner\'s message' });
    const tag = 'synthetic-held-';
    const text = Array.from({ length: 2800 }, (_, i) => `${tag}${i}`).join(' ');
    // A liveness bound, not a speed limit: a stalled publish never answers.
    const stalled = new Promise((resolve) => { setTimeout(() => resolve({ json: { delivered: 'stalled' } }), 20000).unref(); });
    const answer = await Promise.race([adapterRequest(r.socket, 'POST', '/publish', { session_id: 'session-example', conversation: { conversation_id: OWNER }, text, idempotency_key: 'publish-example-held' }), stalled]);
    const chunks = r.gw.sends.filter((s) => s.type === 'text' && String(s.text).includes(tag)).length;
    t.diagnostic(`chunks sent: ${chunks}; flushes held: ${held()}; released: ${fs.existsSync(released)}`);
    assert.equal(fs.existsSync(released), false, 'the flushes were still held');
    assert.ok(held() > 0, 'a flush was held (the preload in effect)');
    assert.equal(answer.json.delivered, true, `every chunk sent while the flushes were held (${chunks} sent)`);
    assert.ok(chunks > 4, `a publish of many chunks (${chunks})`);
  });
}
