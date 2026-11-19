// PR 104 round 3, item 4: ACK_REACTION and NEVER_SILENT both on.
//
// R4: the reaction the adapter places by itself (the acknowledgement) is never
// words or a reaction from the addressed session. With the mayor's session
// silent and main's never-silent conditions for one cause met, the owner still
// gets the never-silent line.
// R5: that owner line is never the mayor's words. It writes no stopwatch words
// line and no words-attempt marker, and no acknowledgement reaction answers it.
//
// The real adapter process against the fakes, as never-silent-integration.test.js
// drives it; GC_BIN is a fake gc that only records its arguments.

import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { rig, sleep, waitFor } from './fakes.js';
import { OWNER_LINES } from '../src/never-silent.js';

const OWNER = 'example-owner';
const MESSAGE = 'example-addressed';

async function setup(t) {
  const r = await rig();
  t.after(() => r.close());
  const bin = path.join(r.dir, 'fake-gc');
  const mail = path.join(r.dir, 'mail.jsonl');
  fs.writeFileSync(bin, `#!${process.execPath}\nrequire('fs').appendFileSync(${JSON.stringify(mail)}, JSON.stringify(process.argv.slice(2))+'\\n');\n`, { mode: 0o755 });
  r.options = { NEVER_SILENT: 'on', NEVER_SILENT_OPERATOR_CONTEXT: 'example-operator', GC_BIN: bin, ACK_REACTION: 'eyes' };
  r.health = (state) => {
    fs.mkdirSync(path.join(r.dir, '.gc'), { recursive: true });
    const now = new Date(r.clock.now()).toISOString();
    fs.writeFileSync(path.join(r.dir, '.gc', 'mayor-health.json'), JSON.stringify({ state, since: now, checked_at: now }));
  };
  // The stopwatch and its markers live in GC_SERVICE_STATE_ROOT, else the
  // adapter's state directory (src/index.js main).
  r.stateDir = path.join(r.dir, '.gc', 'whatsapp');
  r.stopwatch = () => fs.readdirSync(r.stateDir).filter((f) => /^stopwatch-\d{4}-\d{2}\.jsonl$/.test(f))
    .flatMap((f) => fs.readFileSync(path.join(r.stateDir, f), 'utf8').split('\n').filter(Boolean).map((line) => JSON.parse(line)));
  r.markers = () => {
    const dir = path.join(r.stateDir, 'words-attempts');
    return fs.existsSync(dir) ? fs.readdirSync(dir) : [];
  };
  return r;
}

const reactions = (r) => r.gw.sends.filter((s) => s.type === 'reaction');
const texts = (r) => r.gw.sends.filter((s) => s.type === 'text');

test('both switches on: the acknowledgement is not the session\'s answer, and the owner line is not words (R4, R5)', { timeout: 60000 }, async (t) => {
  const r = await setup(t);
  r.start(r.options);
  r.gw.addText(OWNER, MESSAGE, 'fixture words');

  // (a) The message is delivered and gets the acknowledgement reaction.
  await waitFor(() => r.journal().some((line) => line.t === 'accepted' && line.verdict === 'delivered'), { what: 'the delivery' });
  await waitFor(() => reactions(r).length === 1, { what: 'the acknowledgement' });
  assert.equal(reactions(r)[0].reaction.message_id, MESSAGE);
  assert.equal(reactions(r)[0].reaction.emoji, '\u{1F440}');
  await waitFor(() => r.stopwatch().some((e) => e.event === 'reaction' && e.message === MESSAGE && e.actor === 'adapter'), { what: 'the reaction line' });

  // The mayor's session sends no words and no reaction. Three minutes pass
  // and fresh health evidence says the session is down: main's conditions
  // for session_down are met, so the owner line is posted.
  r.clock.advance(180000);
  r.health('session_down');
  await r.pumpUntil(() => texts(r).length === 1, { stepMs: 1000, what: 'the never-silent owner line' });
  assert.equal(texts(r)[0].to, OWNER);
  assert.equal(texts(r)[0].text, OWNER_LINES.session_down);

  // (b) Let anything the line might start finish, then read the stopwatch.
  await r.pump(10000, { stepMs: 1000, realMs: 50 });
  await sleep(300);
  const events = r.stopwatch();
  assert.deepEqual(events.filter((e) => e.event === 'words'), [], 'the owner line wrote a stopwatch words line');
  assert.deepEqual(r.markers(), [], 'the owner line wrote a words-attempt marker');
  assert.equal(reactions(r).length, 1, 'a second acknowledgement was placed');
  assert.deepEqual(events.filter((e) => e.event === 'inbound').map((e) => e.message), [MESSAGE]);
  assert.deepEqual(events.filter((e) => e.event === 'reaction').map((e) => [e.message, e.actor]), [[MESSAGE, 'adapter']]);
  assert.equal(texts(r).length, 1);
});
