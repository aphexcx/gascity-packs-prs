'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { initialState, decide } = require('../reachable.cjs');
const { NOW } = require('../test-support/fixture.cjs');
const opts = { now: NOW, mode: 'enforce', session: 'session-example', aliases: ['talk', 'reachable'] };
const bash = command => ({ tool_name: 'Bash', tool_input: { command } });
const writeFile = (file_path, content) => ({ tool_name: 'Write', tool_input: { file_path, content } });
const reasons = {
  work_tool: 'this tool does work',
  not_talk: 'the shell command is not one gc talk command',
  chained: 'the shell command chains, pipes, redirects, groups or backgrounds commands outside quotes',
  expansion: 'the shell command can expand $ or a backquote into a command',
  glob: 'the shell command has an unquoted glob, tilde, brace or comment character',
  line_break: 'the shell command breaks a line outside quotes',
  unclosed: 'the shell command leaves a quote open or ends in a backslash',
  cd_form: 'only one cd DIR && may stand before the talk command',
  write_place: 'a Write runs here only for a reply body of at most 64 KiB under the temp directory',
};
function attempt(input, mode = 'enforce') {
  const state = initialState();
  state.waiting.push({ provider: 'example', conversation_id: 'room-a', seq: 1, at: NOW - 60000, refusals: 0, released: false });
  const result = decide(state, { hook_event_name: 'PreToolUse', tool_use_id: 'first', ...input }, { ...opts, mode });
  return { result, state };
}
const talk = 'gc talk reply-current';
const passing = [
  "cd /tmp/example && gc talk reply-current --body 'on it'",
  'gc talk reply-current --body-file /tmp/example-reply.md',
  'gc talk publish --to example/room-a --text "It costs $5 and $USER pays"',
  talk + " --body 'line one\nline two'", talk + ' --body "line one\r\nline two"',
  'cd "$HOME" && gc handoff', talk + " 'quoted; punctuation?'",
  talk + " 'it'\\''s ready'", talk + ' escaped\\;semicolon', '  gc\ttalk\tupload\tfile',
  'gc reachable pass example/room-a', 'gc reachable status',
];
for (const [i, command] of passing.entries()) test('R2: permitted reply ' + i, () => {
  assert.deepEqual(attempt(bash(command)).result, {});
});
const refused = [
  ['; next', 'chained'], [' && next', 'chained'], [' || next', 'chained'], [' | next', 'chained'],
  [' (next)', 'chained'], [' { next; }', 'glob'], [' $(next)', 'expansion'], [' "`next`"', 'expansion'],
  ['\nnext', 'line_break'], ['\rnext', 'line_break'], [' \\\nnext', 'line_break'],
  [' <<body', 'chained'], [' <<<body', 'chained'], [' >file', 'chained'], [' &', 'chained'],
  [' <(next)', 'chained'], [' $NAME', 'expansion'], [' "$(next)"', 'expansion'],
  [' "$' + '{name}"', 'expansion'], [' "$[1]"', 'expansion'], [' "$NAME[1]"', 'expansion'],
  ...['*', '?', '[', ']', '~', '#'].map(c => [' ' + c, 'glob']),
  [" 'unfinished", 'unclosed'], [' \\', 'unclosed'],
].map(([suffix, reason]) => [talk + suffix, reason]);
refused.push(
  ['X=1 ' + talk, 'not_talk'], ['env X=1 ' + talk, 'not_talk'], ["sh -c 'gc handoff'", 'not_talk'],
  ['/usr/bin/gc talk reply-current', 'not_talk'], ['gc absent status', 'not_talk'],
  ['cd && ' + talk, 'cd_form'], ['cd a b && ' + talk, 'cd_form'], ['cd a && cd b && ' + talk, 'cd_form'],
  ['cd a && unrelated', 'cd_form'], ['cd a && ' + talk + ' && next', 'chained'],
);
for (const [i, [command, reason]] of refused.entries()) test('R2: refused shell form ' + i + ' gives ' + reason, () => {
  const out = attempt(bash(command)).result.hookSpecificOutput;
  assert.equal(out?.permissionDecision, 'deny');
  assert.equal(out.permissionDecisionReason.split('\n').at(-1), 'Refused because ' + reasons[reason] + '.');
});
test('R2: a reply-shaped refusal never consumes an attempt in either mode', () => {
  for (const mode of ['observe', 'enforce']) {
    for (const prefix of ['', '  ', 'cd dir && ', "cd 'a b'; "]) {
      const { state } = attempt(bash(prefix + talk + ' ?'), mode);
      assert.equal(state.waiting[0].refusals, 0);
      assert.equal(state.log[0].reply_shaped, true);
    }
    assert.equal(attempt(bash('gc talkative work'), mode).state.waiting[0].refusals, 1);
  }
});
test('R2: only a bounded Write inside a real temp directory prepares a reply', t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'reachable-body-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  for (const file of [path.join(dir, 'new', 'body'), '/tmp/example-reply.md']) {
    assert.deepEqual(attempt(writeFile(file, 'x'.repeat(200))).result, {});
  }
  const link = path.join(dir, 'link');
  fs.symlinkSync(path.dirname(process.execPath), link);
  for (const input of [
    writeFile(link, ''), writeFile(path.join(link, 'new'), ''), writeFile(process.execPath, ''),
    writeFile(path.join(dir, 'large'), 'é'.repeat(32769)), { tool_name: 'Edit' }, { tool_name: 'NotebookEdit' },
  ]) {
    const code = input.tool_name === 'Write' ? 'write_place' : 'work_tool';
    const out = attempt(input).result.hookSpecificOutput;
    assert.equal(out?.permissionDecisionReason.split('\n').at(-1), 'Refused because ' + reasons[code] + '.');
  }
});
