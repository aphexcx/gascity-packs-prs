// Off parity (PR 104 round 2, item 11; round 3, item 3): with ACK_REACTION
// unset, the adapter behaves as it does at the base on the same inputs. Since
// round 3 the base is main at 205d0aecedf944792f1e3b3fbc420b9e5ad910d8 (round
// 2's was c99694367081b2b3e98a58aafaf1320e091621fe). One script, replayed
// against the fakes through the real adapter process; every observation is
// compared with the values the base produces, written below. This file runs
// byte for byte at the base and at the tip; newer wiring is reached only
// through offparity-hook.js (the base's copy is kept under
// assets/pr-evidence/gp-2awj/round2/base-hooks/). The script runs twice:
// NEVER_SILENT unset, and NEVER_SILENT=on, where the adapter process wires
// main's never-silent itself and the state file is read without its times.
//
// THE ONLY DIFFERENCES ALLOWED, each removed before the comparison:
// - the stopwatch's files stopwatch-YYYY-MM.jsonl in the state directory
//   (contract item 15: the stopwatch records whether ACK_REACTION is on or off);
// - the words-attempts directory and its markers (RULING 1 of round 1: the
//   words line's acceptance_uncertain);
// - the health field jev_api_key (contract item 10: status says whether the
//   key is set);
// - the stopwatch's own warn-once lines, which a healthy script never prints
//   (contract item 19).

import test from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import path from 'node:path';
import { adapterRequest, rig, sleep, waitFor } from './fakes.js';
import { hookEnv, tree } from './offparity-hook.js';

const OWNER = 'owner-example';
const OTHER = 'other-example';
const JPEG = Buffer.concat([Buffer.from([0xff, 0xd8, 0xff, 0xe0]), Buffer.alloc(96, 7)]);
const PDF = Buffer.from('%PDF-1.7\nsynthetic report bytes\n%%EOF');
const LONG = 'synthetic words. '.repeat(300); // two chunks of at most 4096 characters
const STOPWATCH_FILE = /^stopwatch-\d{4}-\d{2}\.jsonl$/;
const STOPWATCH_WARNINGS = [/^stopwatch: /];

// The script's observations at the base c99694367081b2b3e98a58aafaf1320e091621fe,
// recorded by a run of this file there with OFFPARITY_PRINT=1 and the base's
// hook; the base's own run of this file proves them.
const BASE = {
  "turns": [
    {
      "conversation": "owner-example",
      "message": "message-example-1",
      "actor": "owner-example",
      "attachments": [],
      "text": "hello from the owner"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-2",
      "actor": "owner-example",
      "attachments": [
        "image/jpeg"
      ],
      "text": "the shop front\n\n[1 WhatsApp file attached]\n  1. message-example-2-image.jpg (image/jpeg) saved to <dir>/.gc/whatsapp-media/inbound/owner-example/message-example-2-image.jpg; Read that path to view it"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-3",
      "actor": "owner-example",
      "attachments": [],
      "text": "OK [ok-1]"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-4",
      "actor": "owner-example",
      "attachments": [],
      "text": "[reaction 👍 to message-example-out]"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-5",
      "actor": "owner-example",
      "attachments": [],
      "text": "[unsupported WhatsApp message: system]"
    }
  ],
  "city": {
    "GET /extmsg/transcript": 1,
    "POST /extmsg/adapters": 1,
    "POST /extmsg/inbound": 5
  },
  "gateway": {
    "GET /v1/media/media-example-1": 1,
    "GET /v1/window": 5,
    "POST /v1/read": 5,
    "POST /v1/send": 4,
    "POST /v1/uploads": 1
  },
  "polled": true,
  "acknowledged": [
    1,
    2,
    3,
    4,
    5,
    6
  ],
  "sends": [
    {
      "type": "text",
      "to": "owner-example",
      "key": "text:<sha256>:0",
      "chars": 4096
    },
    {
      "type": "text",
      "to": "owner-example",
      "key": "text:<sha256>:1",
      "chars": 1004
    },
    {
      "type": "document",
      "to": "owner-example",
      "key": "file-send:<sha256>",
      "chars": null
    },
    {
      "type": "template",
      "to": "other-example",
      "key": "template:<sha256>",
      "chars": null
    }
  ],
  "reads": [
    "message-example-1",
    "message-example-2",
    "message-example-3",
    "message-example-4",
    "message-example-5"
  ],
  "jev": 0,
  "answers": [
    {
      "status": 200,
      "delivered": true
    },
    {
      "status": 200,
      "delivered": true
    },
    {
      "status": 200,
      "delivered": true
    }
  ],
  "health": {
    "status": 200,
    "json": {
      "ok": true,
      "gateway_auth": "ok",
      "city": "test-city"
    }
  },
  "log": [
    "internal listener on UDS <dir>/s.sock",
    "journal compacted (start): 0 acknowledged entr(ies) dropped, 1 line(s) kept",
    "media …mple image: saved (100 bytes)",
    "publish to …mple delivered (2 chunks)",
    "registered with gc as provider=whatsapp account=dynasty callback=<gc>/v0/city/test-city/svc/whatsapp",
    "update to …mple sent as the dynasty_update template",
    "watermark read (schedule): 1 watermark(s), 0 gap(s) standing"
  ],
  "state": [
    "whatsapp/",
    "whatsapp/inbound_dead_letter/",
    "whatsapp/inbound_journal.jsonl",
    "whatsapp/keeper.json",
    "whatsapp/ledger.json",
    "whatsapp/outbound_progress.json",
    "whatsapp/updates.jsonl",
    "whatsapp-media/",
    "whatsapp-media/inbound/",
    "whatsapp-media/inbound/owner-example/",
    "whatsapp-media/inbound/owner-example/message-example-2-image.jpg"
  ],
  "neverSilent": [
    "<no state file>"
  ]
};

// The script's observations with NEVER_SILENT=on at the base, main at
// 205d0aecedf944792f1e3b3fbc420b9e5ad910d8, recorded as BASE was.
const BASE_NEVER_SILENT_ON = {
  "turns": [
    {
      "conversation": "owner-example",
      "message": "message-example-1",
      "actor": "owner-example",
      "attachments": [],
      "text": "hello from the owner"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-2",
      "actor": "owner-example",
      "attachments": [
        "image/jpeg"
      ],
      "text": "the shop front\n\n[1 WhatsApp file attached]\n  1. message-example-2-image.jpg (image/jpeg) saved to <dir>/.gc/whatsapp-media/inbound/owner-example/message-example-2-image.jpg; Read that path to view it"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-3",
      "actor": "owner-example",
      "attachments": [],
      "text": "OK [ok-1]"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-4",
      "actor": "owner-example",
      "attachments": [],
      "text": "[reaction 👍 to message-example-out]"
    },
    {
      "conversation": "owner-example",
      "message": "message-example-5",
      "actor": "owner-example",
      "attachments": [],
      "text": "[unsupported WhatsApp message: system]"
    }
  ],
  "city": {
    "GET /extmsg/transcript": 1,
    "POST /extmsg/adapters": 1,
    "POST /extmsg/inbound": 5
  },
  "gateway": {
    "GET /v1/media/media-example-1": 1,
    "GET /v1/window": 5,
    "POST /v1/read": 5,
    "POST /v1/send": 4,
    "POST /v1/uploads": 1
  },
  "polled": true,
  "acknowledged": [
    1,
    2,
    3,
    4,
    5,
    6
  ],
  "sends": [
    {
      "type": "text",
      "to": "owner-example",
      "key": "text:<sha256>:0",
      "chars": 4096
    },
    {
      "type": "text",
      "to": "owner-example",
      "key": "text:<sha256>:1",
      "chars": 1004
    },
    {
      "type": "document",
      "to": "owner-example",
      "key": "file-send:<sha256>",
      "chars": null
    },
    {
      "type": "template",
      "to": "other-example",
      "key": "template:<sha256>",
      "chars": null
    }
  ],
  "reads": [
    "message-example-1",
    "message-example-2",
    "message-example-3",
    "message-example-4",
    "message-example-5"
  ],
  "jev": 0,
  "answers": [
    {
      "status": 200,
      "delivered": true
    },
    {
      "status": 200,
      "delivered": true
    },
    {
      "status": 200,
      "delivered": true
    }
  ],
  "health": {
    "status": 200,
    "json": {
      "ok": true,
      "gateway_auth": "ok",
      "city": "test-city"
    }
  },
  "log": [
    "internal listener on UDS <dir>/s.sock",
    "journal compacted (start): 0 acknowledged entr(ies) dropped, 1 line(s) kept",
    "media …mple image: saved (100 bytes)",
    "publish to …mple delivered (2 chunks)",
    "registered with gc as provider=whatsapp account=dynasty callback=<gc>/v0/city/test-city/svc/whatsapp",
    "update to …mple sent as the dynasty_update template",
    "watermark read (schedule): 1 watermark(s), 0 gap(s) standing"
  ],
  "state": [
    "whatsapp/",
    "whatsapp/inbound_dead_letter/",
    "whatsapp/inbound_journal.jsonl",
    "whatsapp/keeper.json",
    "whatsapp/ledger.json",
    "whatsapp/never-silent-outbound.json",
    "whatsapp/never-silent.json",
    "whatsapp/outbound_progress.json",
    "whatsapp/updates.jsonl",
    "whatsapp-media/",
    "whatsapp-media/inbound/",
    "whatsapp-media/inbound/owner-example/",
    "whatsapp-media/inbound/owner-example/message-example-2-image.jpg"
  ],
  "neverSilent": [
    "acts [{\"at\":\"<t>\",\"conversation\":\"owner-example\",\"session\":\"session-example\"}]",
    "message whatsapp:dynasty:message-example-1 {\"conversation\":\"owner-example\",\"created\":\"<t>\",\"delivered\":\"<t>\",\"id\":\"whatsapp:dynasty:message-example-1\",\"observed\":\"<t>\",\"targets\":[\"s-mayor\"]}",
    "message whatsapp:dynasty:message-example-2 {\"conversation\":\"owner-example\",\"created\":\"<t>\",\"delivered\":\"<t>\",\"id\":\"whatsapp:dynasty:message-example-2\",\"observed\":\"<t>\",\"targets\":[\"s-mayor\"]}",
    "message whatsapp:dynasty:message-example-3 {\"conversation\":\"owner-example\",\"created\":\"<t>\",\"delivered\":\"<t>\",\"id\":\"whatsapp:dynasty:message-example-3\",\"observed\":\"<t>\",\"targets\":[\"s-mayor\"]}",
    "outages []",
    "owners {}",
    "posted []"
  ]
};

async function fakeJev(t) {
  const jev = { calls: 0 };
  const server = http.createServer((req, res) => { jev.calls++; req.resume(); res.writeHead(500).end(); });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise((resolve) => { server.closeAllConnections?.(); server.close(resolve); }));
  jev.url = `http://127.0.0.1:${server.address().port}`;
  return jev;
}

// never-silent's state file without its times: a number that reads as epoch
// milliseconds is <t>. Keys sorted; one line per top-level field and one per
// tracked message.
function neverSilentState(stateDir) {
  const file = path.join(stateDir, 'never-silent.json');
  if (!fs.existsSync(file)) return ['<no state file>'];
  const canon = (v) => {
    if (typeof v === 'number' && v >= 1e12) return '<t>';
    if (Array.isArray(v)) return v.map(canon);
    if (v && typeof v === 'object') return Object.fromEntries(Object.keys(v).sort().map((k) => [k, canon(v[k])]));
    return v;
  };
  const doc = JSON.parse(fs.readFileSync(file, 'utf8'));
  const out = [];
  for (const [key, value] of Object.entries(doc)) {
    if (key === 'messages') for (const [id, m] of Object.entries(value)) out.push(`message ${id} ${JSON.stringify(canon(m))}`);
    else out.push(`${key} ${JSON.stringify(canon(value))}`);
  }
  return out.sort();
}

function listing(root) {
  const out = [];
  const walk = (dir, rel) => {
    if (!fs.existsSync(dir)) return;
    for (const entry of fs.readdirSync(dir, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
      const name = rel ? `${rel}/${entry.name}` : entry.name;
      // The temp file of an atomic write (src/ledger.js writeFileAtomic) that
      // the script's closing SIGKILL interrupted: the kill's timing, not
      // behaviour. With NEVER_SILENT=on the state is written every tick.
      if (/^\.tmp-\d+-[0-9a-f]+$/.test(entry.name)) continue;
      if (entry.isDirectory()) {
        if (entry.name === 'words-attempts') continue; // RULING 1, allowed
        out.push(`${name}/`);
        walk(path.join(dir, entry.name), name);
      } else if (!STOPWATCH_FILE.test(entry.name)) out.push(name); // item 15, allowed
    }
  };
  walk(root, '');
  return out;
}

function counts(requests, ignore = []) {
  const out = {};
  for (const { route } of requests) if (!ignore.includes(route)) out[route] = (out[route] ?? 0) + 1;
  return Object.fromEntries(Object.entries(out).sort(([a], [b]) => a.localeCompare(b)));
}

function logLines(text, r, jev) {
  return text.split('\n').filter(Boolean)
    .map((line) => line.replace(/^\d{4}-\d{2}-\d{2}T[\d:.]+Z /, ''))
    .map((line) => line.split(r.dir).join('<dir>').split(r.gw.url).join('<gateway>').split(r.gc.url).join('<gc>').split(jev.url).join('<jev>'))
    .filter((line) => !STOPWATCH_WARNINGS.some((rule) => rule.test(line))); // item 19, allowed
}

async function runScript(t, extra = {}) {
  const jev = await fakeJev(t);
  // A key and a local base are set so that a stray choice would land on the fake.
  const r = await rig({ extra: { ...hookEnv(), JEV_API_BASE_URL: jev.url, JEV_API_KEY: 'test-credential', ...extra } });
  t.after(() => r.close());
  const ts = String(Math.floor(r.clock.now() / 1000));
  const message = (id, type, body) => ({ from: OWNER, id, timestamp: ts, type, ...body });
  r.gw.media.set('media-example-1', { bytes: JPEG, mime: 'image/jpeg' });
  r.gw.bound.add(OTHER);
  const first = message('message-example-1', 'text', { text: { body: 'hello from the owner' } });
  const ids = [
    r.gw.addDelivery({ wa_id: OWNER, message: first }),
    r.gw.addDelivery({ wa_id: OWNER, message: message('message-example-2', 'image', { image: { id: 'media-example-1', mime_type: 'image/jpeg', sha256: crypto.createHash('sha256').update(JPEG).digest('base64'), caption: 'the shop front' } }) }),
    r.gw.addDelivery({ wa_id: OWNER, message: message('message-example-3', 'interactive', { interactive: { type: 'button_reply', button_reply: { id: 'ok-1', title: 'OK' } } }) }),
    r.gw.addDelivery({ wa_id: OWNER, message: message('message-example-4', 'reaction', { reaction: { message_id: 'message-example-out', emoji: '\u{1F44D}' } }) }),
    r.gw.addDelivery({ wa_id: OWNER, message: message('message-example-5', 'system', { system: { body: 'a system notice' } }) }),
    r.gw.addDelivery({ wa_id: OWNER, message: first }), // the redelivery of the first text
  ];
  r.start();
  await waitFor(() => ids.every((id) => r.gw.acked.has(id)), { what: 'every delivery acknowledged' }).catch((err) => {
    const log = logLines(r.adapter.log(), r, jev).join(' | ');
    throw new Error(`${err.message}: acknowledged ${[...r.gw.acked].join(',')}; posted ${r.gc.posts.map((p) => p.pmid).join(',')}; log ${log}`);
  });
  const publish = (body) => adapterRequest(r.socket, 'POST', '/publish', { session_id: 'session-example', conversation: { conversation_id: OWNER }, ...body });
  const file = path.join(r.dir, 'report.pdf');
  fs.writeFileSync(file, PDF);
  const answers = [
    await publish({ text: LONG, idempotency_key: 'publish-example-text' }),
    await publish({ file, idempotency_key: 'publish-example-file' }),
    await adapterRequest(r.socket, 'POST', '/publish-update', { to: OTHER, text: 'a synthetic update', idempotency_key: 'publish-example-update' }),
  ].map((a) => ({ status: a.status, delivered: a.json?.delivered }));
  const health = await adapterRequest(r.socket, 'GET', '/healthz');
  await sleep(300); // let any work the script started finish
  await r.adapter.kill();
  const { jev_api_key: _key, ...rest } = health.json; // item 10, allowed
  return {
    // The turns raised, in order: conversation, message id, actor, the
    // attachments' kinds, and the text (the temp directory written <dir>).
    turns: r.gc.posts.map((p) => ({ conversation: p.message.conversation?.conversation_id, message: p.pmid, actor: p.message.actor?.id,
      attachments: (p.message.attachments ?? []).map((a) => a.mime_type), text: p.text.split(r.dir).join('<dir>') })),
    city: counts(r.gc.requests),
    gateway: counts(r.gw.requests, ['GET /v1/inbox', 'POST /v1/inbox/ack']),
    polled: r.gw.requests.some((q) => q.route === 'GET /v1/inbox'),
    acknowledged: [...r.gw.acked].sort((a, b) => a - b),
    sends: r.gw.sends.map((s) => ({ type: s.type, to: s.to, key: s.idempotency_key.replace(/[0-9a-f]{64}/, '<sha256>'), chars: s.text?.length ?? null })),
    reads: r.gw.reads,
    jev: jev.calls,
    answers,
    health: { status: health.status, json: rest },
    // Sorted: two workers may log in either order on a loaded host.
    log: logLines(r.adapter.log(), r, jev).sort(),
    state: listing(path.join(r.dir, '.gc')),
    neverSilent: neverSilentState(path.join(r.dir, '.gc', 'whatsapp')),
  };
}

test(`with ACK_REACTION unset the script observes as at the base (${tree})`, async (t) => {
  const observed = await runScript(t);
  if (process.env.OFFPARITY_PRINT) t.diagnostic(JSON.stringify(observed));
  for (const key of Object.keys(BASE)) assert.deepEqual(observed[key], BASE[key], `${key} differs from the base`);
});

// Round 3, item 3: NEVER_SILENT=on with ACK_REACTION unset. Nothing in the
// script fails and no health file exists, so no line is due.
test(`with ACK_REACTION unset and NEVER_SILENT=on the script observes as at the base (${tree})`, async (t) => {
  const observed = await runScript(t, { NEVER_SILENT: 'on' });
  if (process.env.OFFPARITY_PRINT) t.diagnostic(JSON.stringify(observed));
  for (const key of Object.keys(BASE_NEVER_SILENT_ON)) assert.deepEqual(observed[key], BASE_NEVER_SILENT_ON[key], `${key} differs from the base`);
});

test(`an ACK_SCOPE that is neither addressed nor all starts as at the base (1c, ${tree})`, async (t) => {
  const r = await rig({ extra: { ...hookEnv(), ACK_SCOPE: 'everyone' } });
  t.after(() => r.close());
  const id = r.gw.addText(OWNER, 'message-example-scope', 'synthetic');
  r.start();
  let exited = null;
  r.adapter.exited.then((e) => { exited = e; });
  await waitFor(() => exited || (r.gc.registrations.length === 1 && r.gw.acked.has(id)), { what: 'a start or an exit' });
  assert.equal(exited, null, `the adapter refused to start: ${logLines(r.adapter.log(), r, { url: '<none>' }).filter((l) => l.startsWith('fatal')).join(' ')}`);
  assert.equal(r.gc.count('message-example-scope'), 1);
});

