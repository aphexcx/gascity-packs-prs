// The port of Slack's secret redaction (slack-full/adapter/redact/redact.go)
// against the shared fixtures, as slack-full/adapter/redact/redact_test.go
// runs them: each case's want, and the want unchanged on a second pass.
// A failure names the case and the lengths only: the fixtures assemble
// credential shapes from parts, so a printed value would hold a whole one.

import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { redactText, untranslated } from '../src/redact.js';

const cases = JSON.parse(fs.readFileSync(new URL('./fixtures/redaction-cases.json', import.meta.url), 'utf8'));

test('every pattern of patterns.json translates to a JavaScript RegExp', () => {
  assert.deepEqual(untranslated(), []);
});

test('the shared fixtures redact exactly as Slack does', () => {
  let checked = 0;
  for (const c of cases) {
    const text = c.text_parts ? c.text_parts.join('') : c.text;
    const got = redactText(text);
    assert.ok(got === c.want, `${c.name}: not the want (got ${got.length} chars, want ${c.want.length})`);
    const again = redactText(c.want);
    assert.ok(again === c.want, `${c.name}: not idempotent (got ${again.length} chars, want ${c.want.length})`);
    checked++;
  }
  assert.ok(checked >= 100, `only ${checked} fixture cases`);
});

test('redaction comes before the cut', () => {
  // redact_test.go TestRedactBeforeClip.
  const text = `${'. '.repeat(205)}${'a'.repeat(100)}`;
  assert.ok(!redactText(text).slice(0, 420).includes('aaaa'), 'a partial secret survived clipping');
});
