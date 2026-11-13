// Slack's secret redaction, ported span for span from
// slack-full/adapter/redact/redact.go (Text), so the text the adapter sends to
// the Jev API passes the same redaction as Slack's. The rules are a byte copy
// of slack-full/adapter/redact/patterns.json (redact-patterns.json beside this
// file; tests/test_redaction_copies.py holds the copy equal), and the shared
// fixtures (test/fixtures/redaction-cases.json) hold the output equal.
//
// The patterns are written in Go's syntax. Two Go-only forms are translated at
// load, and nothing else: a named group (?P<name> becomes (?<name>, and an
// inline case-insensitive group (?i:...) becomes the same group with each
// letter as a two-case class. A pattern this port cannot translate exactly is
// left out and named by untranslated(): test/redact.test.js fails on it, and
// the adapter still starts.

import fs from 'node:fs';

const failed = [];

// Go's (?i) also folds k to the Kelvin sign and s to the long s; a two-case
// class cannot say that, so those letters are not translated.
function caseless(body) {
  let out = '';
  for (let i = 0; i < body.length; i++) {
    const c = body[i];
    if (c === '\\') {
      const width = body[i + 1] === 'x' ? 4 : 2;
      out += body.slice(i, i + width); i += width - 1; continue;
    }
    if (c === '[') {
      const close = body.indexOf(']', i + 1);
      const inner = close < 0 ? '' : body.slice(i + 1, close).replace(/\\x[0-9A-Fa-f]{2}|\\./g, '');
      if (close < 0 || /[A-Za-z]/.test(inner)) throw new Error('a letter class under (?i)');
      out += body.slice(i, close + 1); i = close; continue;
    }
    if (c === '(' && body[i + 1] === '?' && body[i + 2] !== ':') throw new Error('a group form under (?i)');
    if (/[A-Za-z]/.test(c)) {
      if (/[KkSs]/.test(c)) throw new Error('a letter Go folds beyond two cases');
      out += `[${c.toUpperCase()}${c.toLowerCase()}]`; continue;
    }
    out += c;
  }
  return out;
}

// The end of the group whose body starts at `from`, honoring escapes and classes.
function groupEnd(source, from) {
  let depth = 1;
  for (let i = from; i < source.length; i++) {
    const c = source[i];
    if (c === '\\') { i++; continue; }
    if (c === '[') { const close = source.indexOf(']', i + 1); if (close < 0) break; i = close; continue; }
    if (c === '(') depth++;
    if (c === ')' && --depth === 0) return i;
  }
  throw new Error('an unbalanced group');
}

export function translate(pattern) {
  let source = pattern.replaceAll('(?P<', '(?<');
  for (let at = source.indexOf('(?i:'); at >= 0; at = source.indexOf('(?i:')) {
    const end = groupEnd(source, at + 4);
    source = `${source.slice(0, at)}(?:${caseless(source.slice(at + 4, end))})${source.slice(end + 1)}`;
  }
  if (/\(\?[a-zA-Z]/.test(source)) throw new Error('another Go-only flag group');
  return source;
}

function compile() {
  let specs;
  try {
    specs = JSON.parse(fs.readFileSync(new URL('./redact-patterns.json', import.meta.url), 'utf8'));
  } catch {
    failed.push('redact-patterns.json');
    return [];
  }
  const rules = [];
  for (const spec of specs) {
    try {
      rules.push({
        kind: spec.kind,
        pattern: new RegExp(translate(spec.pattern), 'd'),
        allowlist: (spec.allowlist ?? []).map((allowed) => new RegExp(translate(allowed), 'g')),
        minDistinct: spec.min_distinct ?? 0,
      });
    } catch {
      failed.push(spec.kind);
    }
  }
  return rules;
}

const rules = compile();

export function untranslated() { return [...failed]; }

// Only the generic base64 rule sets a diversity floor. Padding is not entropy;
// numeric identifiers are excluded even when they contain many different digits.
function enoughDiversity(s, minimum) {
  if (!minimum) return true;
  const value = s.replace(/=+$/, '');
  const seen = new Set(); let digitsOnly = true;
  for (let i = 0; i < value.length; i++) {
    const c = value.charCodeAt(i);
    seen.add(c);
    if (c < 48 || c > 57) digitsOnly = false;
  }
  return !digitsOnly && seen.size >= minimum;
}

const tokenChar = (c) => (c >= 97 && c <= 122) || (c >= 65 && c <= 90) || (c >= 48 && c <= 57) || c === 95 || c === 45 || c === 43 || c === 47;

// Redacts before any cut. Every match boundary falls on an ASCII character,
// so Go's byte offsets and these UTF-16 offsets cut the same strings.
export function redactText(s) {
  // Every rule needs six consecutive ASCII token characters ("Bearer").
  let run = 0;
  for (let i = 0; i < s.length && run < 6; i++) run = tokenChar(s.charCodeAt(i)) ? run + 1 : 0;
  if (run < 6) return s;
  const matches = [];
  rules.forEach((rule, priority) => {
    for (let offset = 0; offset < s.length;) {
      // As Go matches s[offset:], anchors and \b see the slice's start.
      const m = rule.pattern.exec(s.slice(offset));
      if (!m) break;
      const token = m.indices.groups?.token;
      const start = offset + (token ? token[0] : m.index);
      const stop = offset + m.index + m[0].length;
      if (enoughDiversity(s.slice(start, stop), rule.minDistinct)) matches.push({ start, end: stop, priority });
      let next = stop;
      if (rule.kind === 'bearer') {
        // A token can itself be "Bearer", starting another credential.
        next = start + 'Bearer'.length;
      } else if (token) {
        // JWTs can overlap when a later segment is another eyJ header.
        const dot = s.slice(start, stop).indexOf('.');
        if (dot >= 0) next = start + dot;
      }
      offset = next > offset ? next : offset + 1;
    }
  });
  matches.sort((a, b) => (a.start - b.start) || (b.end - a.end) || (a.priority - b.priority));
  // Each rule's exceptions apply only to complete matches of that rule.
  const exempt = matches.map(() => false);
  rules.forEach((rule, priority) => {
    for (const allowed of rule.allowlist) {
      let b = 0;
      for (const found of s.matchAll(allowed)) {
        const span = [found.index, found.index + found[0].length];
        while (b < matches.length && matches[b].end <= span[0]) b++;
        let last = b; let blocked = false;
        while (last < matches.length && matches[last].start < span[1]) {
          const m = matches[last];
          if (m.priority !== priority || m.start < span[0] || m.end > span[1]) { blocked = true; break; }
          last++;
        }
        if (blocked) continue;
        for (let i = b; i < last; i++) exempt[i] = true;
      }
    }
  });
  const kept = matches.filter((_, i) => !exempt[i]);
  let out = ''; let end = 0;
  for (let i = 0; i < kept.length;) {
    const m = { ...kept[i] }; i++;
    while (i < kept.length && kept[i].start < m.end) { m.end = Math.max(m.end, kept[i].end); i++; }
    out += `${s.slice(end, m.start)}[REDACTED:${rules[m.priority].kind}:...${s.slice(Math.max(m.start, m.end - 4), m.end)}]`;
    end = m.end;
  }
  if (end === 0) return s;
  return out + s.slice(end);
}
