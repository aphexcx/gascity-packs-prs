'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { fixture, inbound, write } = require('../test-support/fixture.cjs');
for (const alias of ['stay', 'reachable']) {
  test(`R6: ${alias} gives its own pass command with the dictated conversation line`, t => {
    const f = fixture(t);
    const dir = path.join(f.city, 'reachable');
    fs.mkdirSync(dir); fs.writeFileSync(path.join(dir, 'pack.toml'), '[pack]\nname = "reachable"\n');
    f.options.imports = alias + '\t./reachable';
    f.cmd('install'); f.cmd('mode', ['enforce']); f.append(inbound(1));
    const reason = f.hook(write('work')).hookSpecificOutput?.permissionDecisionReason;
    assert.equal(reason.split('\n')[0], `A person has waited 60 seconds for an answer in example/room-a. Answer there now in one line: say what you are doing and when they will hear more. Then go on. If no words are needed, run: gc ${alias} pass example/room-a. A job that will run past about 30 seconds does not belong in this session: start it as a background subagent, or hand it to a worker as a bead.`);
    assert.deepEqual(f.hook({ ...write('reply'), tool_name: 'Bash', tool_input: { command: `gc ${alias} pass example/room-a` } }), {});
  });
}
