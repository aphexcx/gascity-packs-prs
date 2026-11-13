// A slowed flush never delays a delivery or a send (PR 104 round 2, item 5).
// The real adapter process on the fakes, with a preload (node --import,
// slow-fsync-preload.js) that adds 200 ms to every flush made for the
// stopwatch or an attempt marker. Measured with the switch off and on:
// from the fake gateway handing out a delivery to the fake city receiving its
// POST, and from a publish request to the fake gateway receiving its send.
// Each stays under 100 ms (the median of three, so one slow scheduling slot
// on a loaded host is not read as a flush on the path). A publish of many
// chunks is measured chunk by chunk: each chunk's send follows the previous
// one within the limit, so flushes piling up in Node's pool never delay a
// later chunk's marker create.

import test from 'node:test';
import assert from 'node:assert/strict';
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
    const chunked = async (n) => {
      const tag = `synthetic-chunked-${n}-`;
      const text = Array.from({ length: 2800 }, (_, i) => `${tag}${i}`).join(' ');
      const start = performance.now();
      const answer = await adapterRequest(r.socket, 'POST', '/publish', { session_id: 'session-example', conversation: { conversation_id: OWNER }, text, idempotency_key: `publish-example-chunked-${n}` });
      assert.equal(answer.json.delivered, true, `chunked publish ${n} delivered`);
      const times = marks.texts.filter((s) => s.text.includes(tag)).map((s) => s.at);
      assert.ok(times.length > 4, `chunked publish ${n} sent ${times.length} chunks`);
      return Math.max(...times.map((at, i) => at - (i ? times[i - 1] : start)));
    };
    await deliver('warm');
    const deliveries = []; const sends = [];
    for (let i = 0; i < SAMPLES; i++) deliveries.push(await deliver(i));
    for (let i = 0; i < SAMPLES; i++) sends.push(await publish(i));
    const chunks = [];
    for (let i = 0; i < SAMPLES; i++) chunks.push(await chunked(i));
    const shown = (values) => values.map((v) => `${Math.round(v)} ms`).join(', ');
    // The control: the preload did slow the stopwatch's and the markers' flushes.
    await waitFor(() => slowed() > 0, { what: 'a slowed flush (the preload in effect)' });
    t.diagnostic(`delivery: ${shown(deliveries)}; send: ${shown(sends)}; longest gap between chunks: ${shown(chunks)}; slowed flushes: ${slowed()}`);
    assert.ok(median(deliveries) < LIMIT_MS, `a delivery waited on a flush: ${shown(deliveries)}`);
    assert.ok(median(sends) < LIMIT_MS, `a send waited on a flush: ${shown(sends)}`);
    assert.ok(median(chunks) < LIMIT_MS, `a later chunk's send waited on flushes in the pool: ${shown(chunks)}`);
  });
}
