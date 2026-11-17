'use strict';
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const ROOT = path.resolve(__dirname, '..');

const rule = `## Stay reachable
Your first act on any message from a person is one line back, in the conversation where they wrote: what you understood and what happens next. When the message only confirms, thanks or closes, a reaction is the whole answer.
A job you expect to run past about 30 seconds does not run in this session. A short job (a lookup, a draft, a search) goes to a background subagent, and its result comes back to you here. A long job, a job with many steps, a job that writes to the owner's systems, or a job that must survive a restart goes to a worker as a bead, where this city has a worker. The bead says who asked and in which conversation. Where the city has no worker, use a background subagent and tell the person plainly that a restart would lose the job.
While a job runs elsewhere, you answer people. When it reports back, you check the result and tell the person who asked, where they asked.
If a person writes while you are in the middle of something, answer them before your next step. A check refuses your next step once someone has waited more than 30 seconds.
Handing a job off is not finishing it. Done means the result came back and you checked it.`;

test('1: pack has no service or dependencies and six explicit commands', () => {
  const manifest = fs.readFileSync(path.join(ROOT, 'pack.toml'), 'utf8');
  assert.match(manifest, /name = "reachable"/);
  assert.doesNotMatch(manifest, /\[\[?service/);
  for (const verb of ['pass', 'exempt', 'mode', 'status', 'install', 'uninstall']) {
    assert.match(fs.readFileSync(path.join(ROOT, 'commands', verb, 'command.toml'), 'utf8'), /run = "run.sh"/);
    assert.match(fs.readFileSync(path.join(ROOT, 'commands', verb, 'run.sh'), 'utf8'), new RegExp(`reachable.cjs" ${verb} `));
  }
  assert.match(fs.readFileSync(path.join(ROOT, 'doctor/reachable/doctor.toml'), 'utf8'), /run = "run.sh"/);
});

test('18: fragment is byte for byte the specified standing rule', () => {
  const fragment = fs.readFileSync(path.join(ROOT, 'template-fragments/reachable.template.md'), 'utf8');
  assert.equal(fragment, `{{ define "reachable" -}}\n${rule}\n{{- end }}\n`);
});

test('19: README documents both prompt options, modes, limitations and verification', () => {
  const readme = fs.readFileSync(path.join(ROOT, 'README.md'), 'utf8');
  for (const expected of ['inject_fragments_append', 'append_fragments', 'mention-only', '24 hours', 'observe', 'enforce', 'fails open', 'node --test']) assert.ok(readme.includes(expected), expected);
});

module.exports = { rule };
