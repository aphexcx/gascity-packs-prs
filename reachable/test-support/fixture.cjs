'use strict';

const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { command, runHook } = require('../reachable.cjs');

const NOW = Date.parse('2026-01-02T12:00:00Z');
const inbound = (seq, fields = {}, at = NOW - 60000) => ({
  seq, ts: new Date(at).toISOString(), type: 'extmsg.inbound',
  payload: { provider: 'example', conversation_id: 'room-a', target_agent: 'mayor', ...fields },
});
const write = id => ({ hook_event_name: 'PreToolUse', tool_use_id: id, tool_name: 'Write' });

function fixture(t, now = NOW) {
  const city = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-fixture-'));
  t.after(() => fs.rmSync(city, { recursive: true, force: true }));
  fs.mkdirSync(path.join(city, '.gc'));
  const events = path.join(city, '.gc/events.jsonl');
  fs.writeFileSync(events, '');
  const dir = path.join(city, '.gc/reachable');
  const options = { city, now, imports: '', prompt: '', env: { GC_AGENT: 'mayor', GC_SESSION_ID: 'session-example' } };
  const cmd = (verb, args = []) => command(verb, args, options);
  cmd('mode', ['enforce']);
  return {
    city, dir, options, cmd, events,
    append: (...rows) => fs.appendFileSync(events, rows.map(r => typeof r === 'string' ? r : JSON.stringify(r)).join('\n') + '\n'),
    state: () => JSON.parse(fs.readFileSync(path.join(dir, 'state.json'), 'utf8')),
    hook: input => runHook(input, options),
  };
}
module.exports = { fixture, inbound, write, NOW };
