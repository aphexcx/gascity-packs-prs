'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { initialState, reduceEvents, decide, allowed, POLICY } = require('../reachable.cjs');
const NOW = Date.parse('2026-01-02T12:00:00Z');
const opts = { now: NOW, agent: 'mayor', session: 'session-example', mode: 'enforce', aliases: ['chat-example', 'reachable'], caughtUp: true };
function waiting(n = 1) {
  const state = initialState();
  for (let i = 0; i < n; i++) state.waiting.push({ provider: 'example', conversation_id: `room-${i}`, at: NOW - 60000 + i * 1000, seq: i + 1, refusals: 0, released: false });
  return state;
}
const pre = id => ({ hook_event_name: 'PreToolUse', tool_name: 'Bash', tool_input: { command: 'make test' }, tool_use_id: id });

test('8: exact refusal, one line per conversation in oldest-first order', () => {
  const state = waiting(3);
  const out = decide(state, pre('one'), opts);
  const lines = out.hookSpecificOutput.permissionDecisionReason.split('\n');
  assert.equal(lines.length, 3);
  assert.equal(lines[0], 'A person has waited 60 seconds for an answer in example/room-0. Answer there now in one line: say what you are doing and when they will hear more. Then go on. If no words are needed, run: gc reachable pass example/room-0. A job that will run past about 30 seconds does not belong in this session: start it as a background subagent, or hand it to a worker as a bead.');
  assert.match(lines[1], /^A person has waited 59 seconds/);
});

test('10,13: at most three refusals per message; duplicate call is identical and counted once', () => {
  const state = waiting();
  const first = decide(state, pre('one'), opts);
  assert.deepEqual(decide(state, pre('one'), opts), first);
  assert.equal(state.waiting[0].refusals, 1);
  assert.ok(decide(state, pre('two'), opts).hookSpecificOutput);
  assert.ok(decide(state, pre('three'), opts).hookSpecificOutput);
  assert.deepEqual(decide(state, pre('four'), opts), {});
  assert.equal(state.log.filter(e => e.type === 'let_go').length, 1);
});

test('7,4: Stop clears all waiting, and a duplicate Stop does not clear a later message', () => {
  const state = waiting();
  const stop = { hook_event_name: 'Stop', session_id: 'harness-example', prompt_id: 'turn-example' };
  decide(state, stop, opts);
  assert.equal(state.waiting.length, 0);
  state.waiting.push(...waiting().waiting);
  decide(state, stop, opts);
  assert.equal(state.waiting.length, 1);
});

test('12: long calls record tool name and waiting at start or while running, never the command', () => {
  const state = initialState();
  decide(state, pre('slow'), opts);
  const during = { seq: 1, type: 'extmsg.inbound', ts: new Date(NOW + 10000).toISOString(), payload: { provider: 'example', conversation_id: 'room-example', target_agent: 'mayor', actor: 'PRIVATE NAME', text: 'PRIVATE MESSAGE' } };
  reduceEvents(state, [during], { ...opts, now: NOW + 31000 });
  const post = { ...pre('slow'), hook_event_name: 'PostToolUse', duration_ms: 31000 };
  assert.deepEqual(decide(state, post, { ...opts, now: NOW + 31000 }), {});
  decide(state, post, { ...opts, now: NOW + 31000 });
  assert.equal(state.log.filter(e => e.type === 'long_call').length, 1);
  assert.equal(state.log.at(-1).waiting, true);
  assert.equal(state.log.at(-1).tool, 'Bash');
  assert.doesNotMatch(JSON.stringify(state), /PRIVATE|make test/);
});

test('15: observe records would refuse, off and incomplete catch-up never refuse', () => {
  const state = waiting();
  assert.deepEqual(decide(state, pre('observe'), { ...opts, mode: 'observe' }), {});
  assert.equal(state.log[0].type, 'would_refuse');
  assert.deepEqual(decide(waiting(), pre('off'), { ...opts, mode: 'off' }), {});
  assert.deepEqual(decide(waiting(), pre('catching-up'), { ...opts, caughtUp: false }), {});
});

test('9: exact measured read-only tool names each pass', () => {
  assert.ok(POLICY.readOnly.length >= 5);
  for (const tool_name of POLICY.readOnly) assert.equal(allowed({ tool_name, tool_input: {} }, opts.aliases), true, tool_name);
  assert.equal(allowed({ tool_name: 'Write', tool_input: {} }, opts.aliases), false);
});

for (const tool_name of POLICY.readOnly) {
  test(`9 table row: ${tool_name} is allowed during a wait`, () => {
    assert.deepEqual(decide(waiting(), { ...pre(`read-${tool_name}`), tool_name, tool_input: {} }, opts), {});
  });
}

test('9: imported channel aliases, own verbs and handoff pass; shell composition is work', () => {
  for (const command of ["gc chat-example reply-current --text 'hello; this is quoted'", 'gc reachable pass example/room-0', 'gc handoff', 'gc handoff "context cycle"']) {
    assert.equal(allowed({ tool_name: 'Bash', tool_input: { command } }, opts.aliases), true, command);
  }
  for (const command of ['gc chat-example publish x; make test', 'gc chat-example publish $(make test)', 'gc chat-example publish "$(make test)"', 'gc chat-example publish x && make test', 'gc chat-example publish x\nmake test', 'gc chat-example publish x > file', 'gc unknown publish x', 'gc handoff; make test', 'gc chat-example publish `make test`']) {
    assert.equal(allowed({ tool_name: 'Bash', tool_input: { command } }, opts.aliases), false, command);
  }
});
