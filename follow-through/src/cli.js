#!/usr/bin/env node
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';
import { readState, resolveStateDir, withState } from './state.js';
import { consent, hours, addItem, closeItem, setReminder, cancelReminder, parseAt, parseDuration } from './model.js';
import { tick, status, doctor, iso } from './engine.js';

const string = { type: 'string' };
const boolean = { type: 'boolean' };
const specs = {
  consent: { on: boolean, off: boolean },
  hours: { hours: string, clear: boolean, tz: string },
  add: { conversation: string, summary: string, 'summary-file': string },
  list: { conversation: string, all: boolean, json: boolean },
  done: {}, drop: {},
  remind: { conversation: string, at: string, in: string, text: string, 'text-file': string, list: boolean, cancel: string, json: boolean },
  status: { json: boolean }, tick: {}, doctor: {},
};
const help = {
  consent: 'consent CONVERSATION --on | --off',
  hours: 'hours CONVERSATION --hours "Mon-Fri 09:00-18:00" --tz ZONE | --clear [--tz ZONE] | --tz ZONE',
  add: 'add --conversation PROVIDER/ID --summary TEXT | --summary-file PATH',
  list: 'list [--conversation PROVIDER/ID] [--all] [--json]',
  done: 'done ID', drop: 'drop ID',
  remind: 'remind --conversation PROVIDER/ID (--at RFC3339 | --in DURATION) (--text TEXT | --text-file PATH)\nremind --list [--json] | --cancel ID',
  status: 'status [--json]', tick: 'tick (scheduled order)', doctor: 'doctor (pack health check)',
};

function exactlyOne(values, names) {
  if (names.filter(n => values[n] !== undefined).length !== 1) throw new Error(`give exactly one of ${names.map(n => `--${n}`).join(', ')}`);
}

function inputText(values, name) {
  exactlyOne(values, [name, `${name}-file`]);
  return values[name] ?? fs.readFileSync(values[`${name}-file`], 'utf8');
}

const json = value => JSON.stringify(value, (key, v) => key.endsWith('_at') && typeof v === 'number' ? iso(v) : v, 2);
function printRows(rows, asJSON, text) {
  console.log(asJSON ? json(rows) : rows.length ? rows.map(text).join('\n') : 'None.');
}

export function statusText(snapshot) {
  return [
    `Owners: ${snapshot.owners}; consent on: ${snapshot.consent_on}; consenting without zone: ${snapshot.consent_on_without_zone}`,
    `Owners without zone: ${snapshot.owners_without_zone.join(', ') || 'none'}`,
    `Items: ${snapshot.items.open} open, ${snapshot.items.raised} raised, ${snapshot.items.closed} closed`,
    `Reminders set: ${snapshot.reminders_set}`,
    ...snapshot.next_wakes.map(row => `Next wake ${row.conversation}: ${row.at || 'none'}`),
    `Last tick: ${snapshot.last_tick_at || 'never'}`,
    `Failing raises: ${snapshot.failing_raises.length}`,
    ...snapshot.failing_raises.map(r => `${r.id}: ${r.kind} ${r.conversation} ${r.state}; attempts ${r.attempts}; ${r.error || 'unconfirmed send'}`),
  ].join('\n');
}

async function main(args) {
  const [verb, ...rest] = args;
  if (!verb || ['--help', '-h'].includes(verb)) {
    console.log('gc follow-through: durable open items and reminders\n' + Object.values(help).join('\n'));
    return;
  }
  if (!Object.hasOwn(specs, verb)) throw new Error(`unknown command: ${verb}`);
  const { values, positionals } = parseArgs({ args: rest, options: { ...specs[verb], help: { type: 'boolean', short: 'h' } }, allowPositionals: true, strict: true });
  if (values.help) { console.log(help[verb]); return; }
  const expectedPositionals = ['consent', 'hours', 'done', 'drop'].includes(verb) ? 1 : 0;
  if (positionals.length !== expectedPositionals) throw new Error(help[verb]);
  const dir = resolveStateDir();
  const now = Date.now();
  if (verb === 'tick') { await tick({ dir }); return; }
  if (verb === 'status') {
    const snapshot = status(readState(dir), now);
    console.log(values.json ? json(snapshot) : statusText(snapshot));
    return;
  }
  if (verb === 'doctor') {
    const problems = doctor(readState(dir), now);
    console.log(problems.length ? problems.join('\n') : 'follow-through: healthy');
    process.exitCode = problems.length ? 1 : 0;
    return;
  }
  if (verb === 'list') {
    const rows = readState(dir).items.filter(i => (values.all || ['open', 'raised'].includes(i.state)) && (!values.conversation || i.conversation === values.conversation));
    printRows(rows, values.json, i => `${i.id} ${i.state} ${i.conversation}: ${i.summary} (opened ${iso(i.opened_at)})`);
    return;
  }
  if (verb === 'remind' && values.list) {
    if (Object.keys(values).some(k => !['list', 'json', 'help'].includes(k))) throw new Error('--list cannot be combined with reminder creation or cancellation');
    printRows(readState(dir).reminders, values.json, r => `${r.id} ${r.state} ${r.conversation}: ${r.text} (due ${iso(r.due_at)})`);
    return;
  }
  await withState(dir, async (state, save) => {
    let output;
    if (verb === 'consent') {
      exactlyOne(values, ['on', 'off']);
      consent(state, positionals[0], values.on === true, now);
      output = `Consent ${values.on ? 'on' : 'off'} for ${positionals[0]}.`;
    } else if (verb === 'hours') {
      hours(state, positionals[0], values);
      output = `Hours and zone recorded for ${positionals[0]}.`;
    } else if (verb === 'add') output = addItem(state, values.conversation, inputText(values, 'summary'), now).id;
    else if (verb === 'done' || verb === 'drop') {
      closeItem(state, positionals[0], verb === 'done' ? 'done' : 'dropped', now);
      output = `${positionals[0]} ${verb === 'done' ? 'done' : 'dropped'}.`;
    } else if (verb === 'remind') {
      if (values.cancel !== undefined) {
        if (Object.keys(values).some(k => !['cancel', 'help'].includes(k))) throw new Error('--cancel cannot be combined with other reminder options');
        cancelReminder(state, values.cancel, now);
        output = `${values.cancel} cancelled.`;
      } else {
        if (values.json) throw new Error('--json is for remind --list');
        exactlyOne(values, ['at', 'in']);
        const due = values.at === undefined ? now + parseDuration(values.in) : parseAt(values.at);
        const reminder = setReminder(state, values.conversation, inputText(values, 'text'), due, now);
        const tz = state.owners[reminder.conversation].tz;
        const local = tz ? new Intl.DateTimeFormat('en-GB', { timeZone: tz, dateStyle: 'medium', timeStyle: 'long' }).format(new Date(due)) + ` (${tz})` : iso(due);
        output = `${reminder.id} ${local}`;
      }
    }
    save();
    console.log(output);
  });
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await main(process.argv.slice(2)).catch(error => {
    console.error(String(error.message).replace(/[\r\n\u2028\u2029]+/g, ' '));
    process.exitCode = 1;
  });
}
