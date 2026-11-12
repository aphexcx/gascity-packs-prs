import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { rig, adapterRequest, waitFor } from './fakes.js';
import { OWNER_LINES } from '../src/never-silent.js';
import { openJournal } from '../src/journal.js';

test('never-silent integration: primary delivered resolution clears a stale sidecar stop after restart', async (t) => {
  for (const via of ['operator', 'probe']) {
    const r = await setup(t);
    r.gc.policy = () => ({ verdict: 'pending', terminal: true, deliveredBytes: 0 });
    r.start(r.options);
    r.gw.addText('example-owner', 'example-crash-resolution', 'fixture words');
    await waitFor(() => r.journal().some((line) => line.t === 'held_terminal'));
    const sidecar = path.join(r.dir, '.gc', 'whatsapp', 'never-silent.json');
    await waitFor(() => Object.values(JSON.parse(fs.readFileSync(sidecar)).messages).some((m) => m.stopped !== undefined));
    await r.adapter.kill();
    r.clock.advance(1000);
    const journal = openJournal({ stateDir: path.join(r.dir, '.gc', 'whatsapp'), clock: r.clock.now });
    const rid = r.journal().find((line) => line.t === 'held_terminal').rid;
    journal.append({ t: 'accepted', rid, verdict: 'delivered', via, at: r.clock.now() });
    journal.close();
    await r.restart(r.options);
    await r.pump(200000, { stepMs: 10000 });
    assert.equal(r.gw.sends.length, 0, via);
    await r.close();
  }
});

async function setup(t) {
  const r = await rig();
  t.after(() => r.close());
  const bin = path.join(r.dir, 'fake-gc');
  const mail = path.join(r.dir, 'mail.jsonl');
  fs.writeFileSync(bin, `#!${process.execPath}\nrequire('fs').appendFileSync(${JSON.stringify(mail)}, JSON.stringify(process.argv.slice(2))+'\\n');\n`, { mode: 0o755 });
  r.options = { NEVER_SILENT: 'on', NEVER_SILENT_OPERATOR_CONTEXT: 'example-operator', GC_BIN: bin };
  r.mails = () => fs.existsSync(mail) ? fs.readFileSync(mail, 'utf8').trim().split('\n').map(JSON.parse) : [];
  r.health = (state) => {
    fs.mkdirSync(path.join(r.dir, '.gc'), { recursive: true });
    fs.writeFileSync(path.join(r.dir, '.gc', 'mayor-health.json'), JSON.stringify({ state, since: new Date(r.clock.now()).toISOString(), checked_at: new Date(r.clock.now()).toISOString() }));
  };
  return r;
}

test('never-silent integration: terminal whole-paste pending remains delivered across restart', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'pending', terminal: true, concludedPending: true });
  r.start(r.options);
  r.gw.addText('example-owner', 'example-terminal-whole', 'fixture words');
  await waitFor(() => r.journal().some((line) => line.t === 'held_terminal'));
  await r.restart(r.options);
  r.health('ok');
  await r.pump(190000, { stepMs: 10000 });
  assert.equal(r.gw.sends.length, 0);
  r.health('session_down');
  await waitFor(() => r.gw.sends.length === 1);
  assert.equal(r.gw.sends[0].text, OWNER_LINES.session_down);
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.deepEqual(status.json.never_silent.outages.map((o) => o.cause), ['session_down']);
});

test('never-silent integration: refused message posts once without reply bookkeeping and mails fake gc', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ status: 400 });
  r.start(r.options);
  r.gw.addText('example-owner', 'example-message', 'CANARY_CUSTOMER_TEXT');
  await waitFor(() => r.gw.sends.length === 1);
  assert.equal(r.gw.sends[0].text, OWNER_LINES.not_delivered);
  assert.equal(r.gw.sends[0].to, 'example-owner');
  assert.equal(r.gc.outbound.length, 0);
  await waitFor(() => r.mails().length === 2);
  const remote = r.mails().find((args) => args.includes('--context'));
  assert.ok(remote.includes('example-operator'));
  assert.ok(remote.includes('--dedup'));
  assert.doesNotMatch(JSON.stringify(remote), /CANARY_CUSTOMER_TEXT|example-owner|example-message/);
  t.diagnostic(JSON.stringify(remote));
  const status = await adapterRequest(r.socket, 'GET', '/status');
  assert.equal(status.json.never_silent.owner_lines_seven_days, 1);
  await r.restart(r.options);
  await r.pump(10000);
  assert.equal(r.gw.sends.length, 1);
});

test('never-silent integration: whole-paste pending uses health; attributed words suppress a line', async (t) => {
  const r = await setup(t);
  r.gc.policy = () => ({ verdict: 'pending', concludedPending: true });
  r.start(r.options);
  r.gw.addText('example-unanswered', 'example-pending', 'fixture words');
  r.gw.addText('example-answered', 'example-replied', 'fixture words');
  await waitFor(() => r.gc.pastes.length === 2);
  await r.pump(10000);
  const answer = await adapterRequest(r.socket, 'POST', '/publish', { session_id: 's-mayor', conversation: { conversation_id: 'example-answered' }, text: 'A fixture reply', idempotency_key: 'fixture-reply' });
  assert.equal(answer.json.delivered, true);
  r.clock.advance(180000);
  r.health('session_down');
  await waitFor(() => r.gw.sends.length === 2);
  assert.equal(r.gw.sends[1].to, 'example-unanswered');
  assert.equal(r.gw.sends[1].text, OWNER_LINES.session_down);
});

test('never-silent integration: crash after admission or terminal acceptance retains notice eligibility', async (t) => {
  for (const boundary of ['after-received', 'after-accepted']) {
    const r = await setup(t);
    r.gc.policy = () => boundary === 'after-received' ? { status: 400 } : { verdict: 'pending', terminal: true, deliveredBytes: 0 };
    r.gw.addText('example-owner', 'example-crash', 'fixture words');
    const first = r.start({ ...r.options, WHATSAPP_TEST_KILL_AT: boundary });
    assert.equal((await first.exited).signal, 'SIGKILL');
    await r.restart(r.options);
    await r.pumpUntil(() => r.gw.sends.length === 1, { stepMs: 10000 });
    assert.equal(r.gw.sends[0].text, OWNER_LINES.not_delivered);
    await r.close();
  }
});
