'use strict';

const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { fixture, inbound } = require('../test-support/fixture.cjs');

test('R2-R8: v2 state without last_readable_seq resumes from its cursor', t => {
  const f = fixture(t);
  f.append(inbound(1));
  f.cmd('status');
  const state = f.state();
  delete state.last_readable_seq;
  fs.writeFileSync(path.join(f.dir, 'state.json'), JSON.stringify(state));
  f.append(inbound(undefined, { target_agent: 'other' }), { ...inbound(4), type: 'example.tick' });
  assert.equal(f.cmd('status').skipped_rows.last_seq, 1);
  assert.equal(f.state().version, 2);
  assert.equal(f.state().last_readable_seq, 4);
});
