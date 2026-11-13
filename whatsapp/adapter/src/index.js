// index.js — process concerns: config, the gc registration, the tick loop,
// the internal listener (gc's /svc/whatsapp proxy), signals.
//
// Env (names only; values live in the env file run.sh sources):
//   GC_CITY_NAME, GC_API_BASE_URL     the gc city and its API (required)
//   GC_SERVICE_SOCKET, GC_SERVICE_URL_PREFIX   controller-injected (supervised)
//   LISTEN_INTERNAL                   TCP bind when no socket (dev); default 127.0.0.1:8791
//   GC_CITY_PATH                      the city root; state under <city>/.gc/whatsapp
//   WHATSAPP_STATE_DIR                override of the state dir (tests)
//   WHATSAPP_GATEWAY_URL, WHATSAPP_GATEWAY_CREDENTIAL   the gateway and the per-city credential
//   WHATSAPP_POLL_MS (2000), WHATSAPP_WATERMARK_CHECK_MS (60000), WHATSAPP_TICK_MS (250),
//   WHATSAPP_HELD_CONNECTION (on), WHATSAPP_HELD_POLL_MS (300000),
//   WHATSAPP_GC_POST_TIMEOUT_MS (30000), WHATSAPP_GATEWAY_TIMEOUT_MS (15000),
//   WHATSAPP_COMPACT_MS (600000), WHATSAPP_ROUTE_AGENT (mayor),
//   WHATSAPP_KEEPER_UPDATE_HOUR (10), WHATSAPP_CITY_TZ, WHATSAPP_UPDATE_LANGUAGE (en_US),
//   WHATSAPP_KEEPER_WINDOW_READ_MS (60000), WHATSAPP_KEEPER_WINDOW_STAMP_MS (60000: how
//   often a good window read alone is stamped in keeper.json for the doctor row),
//   WHATSAPP_MEDIA_DIR and WHATSAPP_MEDIA_* limits, ELEVENLABS_API_KEY
//   WHATSAPP_TEST_CLOCK, WHATSAPP_TEST_KILL_AT   test-only; unset in production

import fs from 'node:fs';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { pathToFileURL } from 'node:url';

import { createClock } from './clock.js';
import { createDeadLetters } from './deadletter.js';
import { createGatewayClient } from './gateway.js';
import { createGcClient } from './gc.js';
import { createInbound } from './inbound.js';
import { openJournal } from './journal.js';
import { createKeeper } from './keeper.js';
import { createLedger, lastFour } from './ledger.js';
import { createMedia, describeAttachments } from './media.js';
import { createPublisher, outboundChunkChars } from './outbound.js';
import { createNeverSilent } from './never-silent.js';

const MAX_BODY_BYTES = 1024 * 1024;

function getenv(name, fallback = '') {
  const value = process.env[name];
  return value === undefined || value === '' ? fallback : value;
}

function intEnv(name, fallback) {
  const raw = getenv(name);
  if (raw === '') return fallback;
  const value = Number(raw);
  if (!Number.isFinite(value) || value < 0) throw new Error(`${name} must be a non-negative number`);
  return value;
}

export function loadConfig() {
  const cityPath = getenv('GC_CITY_PATH', path.join(os.homedir(), 'city'));
  const cfg = {
    cityName: getenv('GC_CITY_NAME'),
    gcAPIBase: getenv('GC_API_BASE_URL').replace(/\/+$/, ''),
    serviceSocket: getenv('GC_SERVICE_SOCKET'),
    serviceURLPrefix: getenv('GC_SERVICE_URL_PREFIX').replace(/\/+$/, ''),
    listenInternal: getenv('LISTEN_INTERNAL', '127.0.0.1:8791'),
    registerOnStart: getenv('REGISTER_ON_START', 'true') !== 'false',
    stateDir: getenv('WHATSAPP_STATE_DIR', path.join(cityPath, '.gc', 'whatsapp')),
    cityDir: cityPath,
    neverSilent: getenv('NEVER_SILENT') === 'on',
    mayorHealthFile: getenv('MAYOR_HEALTH_FILE', path.join(cityPath, '.gc', 'mayor-health.json')),
    operatorContext: getenv('NEVER_SILENT_OPERATOR_CONTEXT'),
    operatorAgent: getenv('NEVER_SILENT_OPERATOR_AGENT', 'mayor'),
    gcBin: getenv('GC_BIN', 'gc'),
    outboundRootConfig: process.env.WHATSAPP_OUTBOUND_ROOTS,
    mediaDir: getenv('WHATSAPP_MEDIA_DIR', path.join(cityPath, '.gc', 'whatsapp-media', 'inbound')),
    gatewayURL: getenv('WHATSAPP_GATEWAY_URL'),
    credential: getenv('WHATSAPP_GATEWAY_CREDENTIAL'),
    pollMs: intEnv('WHATSAPP_POLL_MS', 2000),
    heldConnection: getenv('WHATSAPP_HELD_CONNECTION', 'on') !== 'off',
    heldPollMs: intEnv('WHATSAPP_HELD_POLL_MS', 300000),
    watermarkCheckMs: intEnv('WHATSAPP_WATERMARK_CHECK_MS', 60000),
    tickMs: intEnv('WHATSAPP_TICK_MS', 250),
    gcPostTimeoutMs: intEnv('WHATSAPP_GC_POST_TIMEOUT_MS', 30000),
    gatewayTimeoutMs: intEnv('WHATSAPP_GATEWAY_TIMEOUT_MS', 15000),
    compactMs: intEnv('WHATSAPP_COMPACT_MS', 10 * 60 * 1000),
    keeperWindowReadMs: intEnv('WHATSAPP_KEEPER_WINDOW_READ_MS', 60000),
    keeperWindowStampMs: intEnv('WHATSAPP_KEEPER_WINDOW_STAMP_MS', 60000),
    routeAgent: getenv('WHATSAPP_ROUTE_AGENT', 'mayor'),
    keeperUpdateHour: intEnv('WHATSAPP_KEEPER_UPDATE_HOUR', 10),
    cityTz: getenv('WHATSAPP_CITY_TZ', Intl.DateTimeFormat().resolvedOptions().timeZone),
    updateLanguage: getenv('WHATSAPP_UPDATE_LANGUAGE', 'en_US'),
    mediaMaxBytes: intEnv('WHATSAPP_MEDIA_MAX_BYTES', 100 * 1024 * 1024),
    mediaQuotaBytes: intEnv('WHATSAPP_MEDIA_QUOTA_BYTES', 10 * 1024 * 1024 * 1024),
    mediaMinFreeBytes: intEnv('WHATSAPP_MEDIA_MIN_FREE_BYTES', 5 * 1024 * 1024 * 1024),
    transcribeTimeoutMs: intEnv('WHATSAPP_TRANSCRIBE_TIMEOUT_MS', 180000),
    mediaDownloadTimeoutMs: intEnv('WHATSAPP_MEDIA_DOWNLOAD_TIMEOUT_MS', 120000),
    scribeURL: getenv('WHATSAPP_SCRIBE_URL', 'https://api.elevenlabs.io/v1/speech-to-text'),
    callbackURL: '',
  };
  const missing = ['cityName', 'gcAPIBase', 'gatewayURL'].filter((k) => !cfg[k]);
  if (missing.length) {
    const names = { cityName: 'GC_CITY_NAME', gcAPIBase: 'GC_API_BASE_URL', gatewayURL: 'WHATSAPP_GATEWAY_URL' };
    throw new Error(`missing required env vars: ${missing.map((k) => names[k]).join(', ')}`);
  }
  if (cfg.serviceSocket) {
    if (!cfg.serviceURLPrefix) throw new Error('GC_SERVICE_SOCKET is set but GC_SERVICE_URL_PREFIX is empty; controller-injected env is incomplete');
    // gc's extmsg HTTP adapter appends /publish itself.
    cfg.callbackURL = cfg.gcAPIBase + cfg.serviceURLPrefix;
  } else {
    cfg.callbackURL = `http://${cfg.listenInternal}`;
  }
  return cfg;
}

function log(...args) {
  console.log(new Date().toISOString(), ...args);
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on('data', (chunk) => {
      size += chunk.length;
      if (size > MAX_BODY_BYTES) {
        reject(Object.assign(new Error('body too large'), { status: 413 }));
        req.destroy();
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
  });
}

function parseJSON(text) {
  try { return JSON.parse(text || '{}'); } catch { return undefined; }
}

export function createListener({ inbound, publisher, keeper, ledger, gateway, health, notices }) {
  const routes = {
    'GET /healthz': async () => ({ status: 200, json: health() }),
    'POST /publish': async (body) => publisher.handlePublish(body),
    'POST /publish-update': async (body) => publisher.handlePublishUpdate(body),
    'GET /status': async () => ({
      status: 200,
      json: { inbound: inbound.status(), keeper: keeper.status(), ledger: ledger.snapshot(), health: health(), never_silent: notices?.status() },
    }),
    'POST /inbound/resolve': async (body) => {
      const req = parseJSON(body);
      if (!req?.rid) return { status: 400, json: { error: 'rid is required' } };
      return inbound.resolve(String(req.rid), String(req.as ?? ''));
    },
    'POST /inbound/clear-gap': async (body) => {
      const req = parseJSON(body);
      if (!req?.conversation_id) return { status: 400, json: { error: 'conversation_id is required' } };
      return inbound.clearGap(String(req.conversation_id));
    },
    'POST /invite': async (body) => {
      const req = parseJSON(body) ?? {};
      const fields = { consent: Boolean(req.proactive_consent), hours: req.hours || '', tz: req.tz || '' };
      // Validate the keeper's fields before minting a code nobody can use.
      if (fields.hours || fields.tz) keeper.validateHours?.(fields.hours, fields.tz);
      const invite = await gateway.createInvite();
      keeper.recordInvite(String(invite.code), fields);
      return { status: 200, json: { code: invite.code, expires_at: invite.expires_at, keeper: fields } };
    },
    // /consent and /hours name a conversation by its id as gc shows it (a
    // phone number or a user id, typed in any form) and answer the last four
    // digits of the conversation the keeper matched (the verb prints those,
    // never the operator's input); an unknown one is refused 404 and nothing
    // is recorded.
    'POST /consent': async (body) => {
      const req = parseJSON(body);
      if (!req?.conversation_id || typeof req.on !== 'boolean') return { status: 400, json: { error: 'conversation_id and on (boolean) are required' } };
      const id = keeper.setConsent(String(req.conversation_id), req.on);
      return { status: 200, json: { ok: true, consent: req.on, conversation_last4: lastFour(id) } };
    },
    // hours "" clears the recorded hours (tz may then be omitted: the
    // recorded zone is kept; tz "" clears it too).
    'POST /hours': async (body) => {
      const req = parseJSON(body);
      if (!req?.conversation_id || typeof req.hours !== 'string') return { status: 400, json: { error: 'conversation_id and hours are required (hours "" clears them)' } };
      const hours = req.hours.trim();
      if (req.tz !== undefined && typeof req.tz !== 'string') return { status: 400, json: { error: 'tz must be a string' } };
      if (hours && !req.tz?.trim()) return { status: 400, json: { error: 'tz is required with hours' } };
      const id = keeper.setHours(String(req.conversation_id), hours, req.tz);
      return { status: 200, json: { ok: true, conversation_last4: lastFour(id), hours: hours || null } };
    },
  };

  return async (req, res) => {
    const { pathname } = new URL(req.url, 'http://adapter');
    const handler = routes[`${req.method} ${pathname}`];
    if (!handler) {
      res.writeHead(404, { 'Content-Type': 'application/json' }).end(JSON.stringify({ error: 'not found' }));
      return;
    }
    try {
      const body = req.method === 'POST' ? await readBody(req) : '';
      const { status, json } = await handler(body);
      res.writeHead(status, { 'Content-Type': 'application/json' }).end(JSON.stringify(json ?? {}));
    } catch (err) {
      // A refusal carries its status (the keeper's 400 and 404); a 401 from
      // the gateway is the adapter's upstream failing (502).
      const status = err.status === 413 ? 413
        : err.status === 401 ? 502
          : err.status === 400 || err.status === 404 ? err.status
            : (err.message && /hours|zone|tz/i.test(err.message) ? 400 : 500);
      res.writeHead(status, { 'Content-Type': 'application/json' }).end(JSON.stringify({ error: err.message }));
    }
  };
}

async function registerUntilDone(gc, cfg, isStopping) {
  let delay = 5000;
  while (!isStopping()) {
    try {
      await gc.register({
        provider: 'whatsapp',
        account_id: 'dynasty',
        name: 'whatsapp-adapter',
        callback_url: cfg.callbackURL,
        // gc renders this into every inbound paste (handler_extmsg.go:727-751);
        // file-based so reply text never has to survive shell quoting.
        reply_instructions: 'Reply: write the reply to a file, then run: gc whatsapp publish --to {conversation_id} --text-file <path>',
        capabilities: {
          SupportsChildConversations: false,
          SupportsAttachments: true,
          MaxMessageLength: outboundChunkChars,
        },
      });
      log(`registered with gc as provider=whatsapp account=dynasty callback=${cfg.callbackURL}`);
      return;
    } catch (err) {
      log(`adapter registration failed (${err.message}); retrying in ${delay / 1000}s`);
      await sleep(delay);
      delay = Math.min(delay * 2, 60000);
    }
  }
}

export async function main() {
  const cfg = loadConfig();
  const clock = createClock(process.env);
  const journal = openJournal({ stateDir: cfg.stateDir, clock });
  const deadLetters = createDeadLetters({ stateDir: cfg.stateDir });
  const gateway = createGatewayClient({ url: cfg.gatewayURL, credential: cfg.credential, timeoutMs: cfg.gatewayTimeoutMs });
  const gc = createGcClient({ base: cfg.gcAPIBase, city: cfg.cityName, postTimeoutMs: cfg.gcPostTimeoutMs });
  const ledger = createLedger({ stateDir: cfg.stateDir, clock, log });
  const keeper = createKeeper({
    stateDir: cfg.stateDir,
    clock,
    gateway,
    gc,
    city: cfg.cityName,
    log,
    updateHour: cfg.keeperUpdateHour,
    cityTz: cfg.cityTz,
    windowReadMs: cfg.keeperWindowReadMs,
    windowStampMs: cfg.keeperWindowStampMs,
  });
  // Media bytes get their own deadline: a 100 MB file outlives the 15 s
  // deadline of every other gateway call.
  const mediaGateway = createGatewayClient({ url: cfg.gatewayURL, credential: cfg.credential, timeoutMs: cfg.mediaDownloadTimeoutMs });
  const mediaStore = createMedia({
    mediaDir: cfg.mediaDir,
    gateway: mediaGateway,
    log,
    maxBytes: cfg.mediaMaxBytes,
    quotaBytes: cfg.mediaQuotaBytes,
    minFreeBytes: cfg.mediaMinFreeBytes,
    scribeURL: cfg.scribeURL,
    transcribeTimeoutMs: cfg.transcribeTimeoutMs,
  });
  const media = { hydrate: (args) => mediaStore.hydrate(args), describe: (results) => describeAttachments(results) };
  const notices = createNeverSilent({
    enabled: cfg.neverSilent, stateDir: cfg.stateDir, healthFile: cfg.mayorHealthFile,
    city: cfg.cityName, operatorContext: cfg.operatorContext, operatorAgent: cfg.operatorAgent,
    localAgent: cfg.routeAgent, gcBin: cfg.gcBin, clock, log,
    post: (message, line, canPost) => publisher.notice(message.conversation, line, `never-silent:${message.id}:${message.created}`, canPost),
  });
  const publisher = createPublisher({
    stateDir: cfg.stateDir,
    gateway,
    gc,
    city: cfg.cityName,
    clock,
    log,
    ledger,
    notices,
    onWindowClosed: (conversationId) => keeper.noteRefusedNudge(conversationId),
    updateLanguage: cfg.updateLanguage,
    cityDir: cfg.cityDir,
    mediaDir: cfg.mediaDir,
    outboundRootConfig: cfg.outboundRootConfig,
    mediaGateway,
  });

  let authState = cfg.credential ? 'unverified' : 'not enrolled';
  const inbound = createInbound({
    journal,
    deadLetters,
    gateway,
    gc,
    city: cfg.cityName,
    clock,
    log,
    media,
    ledger,
    keeper,
    notices,
    pollMs: cfg.pollMs,
    gcPostTimeoutMs: cfg.gcPostTimeoutMs,
    watermarkCheckMs: cfg.watermarkCheckMs,
    compactMs: cfg.compactMs,
    routeAgent: cfg.routeAgent,
    onAuthFailure: () => { authState = 'revoked or not enrolled'; },
    heldConnection: { gatewayURL: cfg.gatewayURL, credential: cfg.credential, enabled: cfg.heldConnection, pollMs: cfg.heldPollMs },
  });

  const health = () => ({
    ok: true,
    gateway_auth: inbound.authFailed ? authState : 'ok',
    city: cfg.cityName,
  });

  const server = http.createServer(createListener({ inbound, publisher, keeper, ledger, gateway, health, notices }));
  server.headersTimeout = 10000;
  server.requestTimeout = 60000;
  if (cfg.serviceSocket) {
    try { fs.unlinkSync(cfg.serviceSocket); } catch { /* ENOENT is fine */ }
    server.listen(cfg.serviceSocket, () => log(`internal listener on UDS ${cfg.serviceSocket}`));
  } else {
    const [host, port] = cfg.listenInternal.split(':');
    server.listen(Number(port), host, () => log(`internal listener on ${cfg.listenInternal}`));
  }

  let stopping = false;
  const stop = (signal) => {
    if (stopping) return;
    stopping = true;
    log(`${signal}: stopping`);
    inbound.stop();
    server.close();
    journal.close();
    setTimeout(() => process.exit(0), 200).unref();
  };
  process.on('SIGTERM', () => stop('SIGTERM'));
  process.on('SIGINT', () => stop('SIGINT'));

  if (!cfg.credential) log('WHATSAPP_GATEWAY_CREDENTIAL is not set: not enrolled (gc whatsapp enroll); polling will stop on the first 401');
  if (cfg.registerOnStart) void registerUntilDone(gc, cfg, () => stopping);

  while (!stopping) {
    try {
      await inbound.tick();
      void notices.poll().catch(() => log('never-silent: poll failed; durable state retained'));
    } catch (err) {
      log(`tick failed: ${err.message}`);
    }
    await sleep(cfg.tickMs);
  }
}

if (process.argv[1] && import.meta.url === pathToFileURL(fs.realpathSync(process.argv[1])).href) {
  main().catch((err) => {
    log(`fatal: ${err.message}`);
    process.exit(1);
  });
}
