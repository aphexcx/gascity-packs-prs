/**
 * The test harness: a gateway on the real D1 (miniflare's SQLite in workerd) with a test clock, a
 * fake Graph API that records every call, signed Meta webhooks built from Meta's own fixtures, and
 * helpers for the city side. Each test makes its own cities and phones, so tests never share a
 * row they assert on.
 */
import { env } from "cloudflare:test";
import { createGateway, type Deps, type GatewayConfig, type LogEvent } from "../src/gateway.js";
import { signatureHeader } from "../src/meta.js";
import audioFixture from "./fixtures/audio.json";
import imageFixture from "./fixtures/image.json";
import reactionFixture from "./fixtures/reaction.json";
import statusFixture from "./fixtures/status-sent.json";
import textFixture from "./fixtures/text.json";

export const PNID = "106540352242922";
export const SECRETS = {
  app: "test-app-secret-harness",
  verify: "test-verify-token-harness",
  access: "test-access-token-harness",
  admin: "test-admin-token-harness",
};
export const T0 = 1_800_000_000;
export const HOUR = 3600;
export const DAY = 86_400;
const ORIGIN = "https://whatsapp.test";

let counter = 0;
export function uniq(): string {
  counter += 1;
  return `${counter}${crypto.randomUUID().replaceAll("-", "").slice(0, 10)}`;
}

/** A fresh Brazilian-shaped wa_id (13 digits) no other test uses. */
export function phone(): string {
  const n = crypto.getRandomValues(new Uint32Array(2));
  return `55${String(n[0]).padStart(10, "0").slice(0, 6)}${String(n[1]).padStart(10, "0").slice(0, 5)}`;
}

export interface GraphCall {
  url: URL;
  method: string;
  authorization: string | null;
  body: Record<string, any> | null;
}

type Failure = { status: number; body: unknown } | "network";

/** Meta's Graph API, faked: answers sends, mark-as-read, the media mint and the media bytes. */
export function fakeGraph() {
  const calls: GraphCall[] = [];
  const failures: Failure[] = [];
  const media = new Map<string, { mime: string; bytes: Uint8Array }>();
  /** Runs after Meta "accepts" a send and before the answer leaves (a status can race the send). */
  const hooks: { onSend: ((wamid: string) => Promise<void>) | null } = { onSend: null };
  let sent = 0;
  const impl = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const req = new Request(input, init);
    const url = new URL(req.url);
    const body = req.method === "POST" ? ((await req.json()) as Record<string, any>) : null;
    calls.push({ url, method: req.method, authorization: req.headers.get("authorization"), body });
    const failure = failures.shift();
    if (failure === "network") throw new TypeError("fetch failed");
    if (failure) return Response.json(failure.body, { status: failure.status });
    if (url.hostname === "graph.facebook.com" && url.pathname.endsWith("/messages")) {
      if (body?.status === "read") return Response.json({ success: true });
      sent += 1;
      const wamid = `wamid.OUT${sent}x${uniq()}`;
      if (hooks.onSend) await hooks.onSend(wamid);
      return Response.json({
        messaging_product: "whatsapp",
        contacts: [{ input: body?.to, wa_id: body?.to }],
        messages: [{ id: wamid }],
      });
    }
    if (url.hostname === "graph.facebook.com") {
      const id = url.pathname.split("/").pop() ?? "";
      const m = media.get(id);
      if (!m) {
        return Response.json({ error: { code: 100, message: "Unsupported get request" } }, { status: 400 });
      }
      return Response.json({
        messaging_product: "whatsapp",
        url: `https://lookaside.fbsbx.com/whatsapp_business/attachments/?mid=${id}`,
        mime_type: m.mime,
        sha256: "x",
        file_size: m.bytes.length,
        id,
      });
    }
    if (url.hostname === "lookaside.fbsbx.com") {
      const m = media.get(url.searchParams.get("mid") ?? "");
      if (!m) return new Response("gone", { status: 404 });
      return new Response(m.bytes, { headers: { "content-type": m.mime } });
    }
    return new Response("no such host in the fake", { status: 599 });
  };
  return {
    fetch: impl as typeof fetch,
    calls,
    media,
    hooks,
    failNext(...f: Failure[]) {
      failures.push(...f);
    },
    /** Message sends only (text, reaction, template), not mark-as-read and not media. */
    sends(): GraphCall[] {
      return calls.filter((c) => c.url.pathname.endsWith("/messages") && c.body?.status !== "read");
    },
    reads(): GraphCall[] {
      return calls.filter((c) => c.body?.status === "read");
    },
  };
}

/**
 * The real D1 behind a switch: arm(k) lets k more batch() calls through, then fails the next one
 * (once). Everything else is the real database.
 */
export function flakyDb() {
  let allow: number | null = null;
  const db = {
    prepare: (q: string) => env.DB.prepare(q),
    batch: async (statements: D1PreparedStatement[]) => {
      if (allow !== null) {
        if (allow === 0) {
          allow = null;
          throw new Error("D1_ERROR: storage unavailable");
        }
        allow -= 1;
      }
      return env.DB.batch(statements);
    },
    exec: (q: string) => env.DB.exec(q),
  } as unknown as D1Database;
  return {
    db,
    arm(k: number) {
      allow = k;
    },
  };
}

export interface Answer {
  status: number;
  body: any;
  headers: Headers;
}

export interface City {
  id: string;
  token: string;
  name: string;
}

interface InboundOpts {
  wamid?: string;
  ts?: number;
  name?: string;
  pnid?: string;
}

function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T;
}

/** Meta's text fixture, re-addressed. */
export function textPayload(from: string, text: string, o: InboundOpts & { wamid: string; ts: number }) {
  const p = clone(textFixture) as any;
  const value = p.entry[0].changes[0].value;
  value.metadata.phone_number_id = o.pnid ?? PNID;
  value.contacts[0].wa_id = from;
  value.contacts[0].profile.name = o.name ?? "Owner Name";
  Object.assign(value.messages[0], { from, id: o.wamid, timestamp: String(o.ts) });
  value.messages[0].text.body = text;
  return p;
}

export function reactionPayload(from: string, target: string, o: { wamid: string; ts: number }) {
  const p = clone(reactionFixture) as any;
  const value = p.entry[0].changes[0].value;
  value.contacts[0].wa_id = from;
  Object.assign(value.messages[0], { from, id: o.wamid, timestamp: String(o.ts) });
  value.messages[0].reaction.message_id = target;
  return p;
}

export function mediaPayload(kind: "image" | "audio", from: string, mediaId: string, o: { wamid: string; ts: number }) {
  const p = clone(kind === "image" ? imageFixture : audioFixture) as any;
  const value = p.entry[0].changes[0].value;
  value.contacts[0].wa_id = from;
  Object.assign(value.messages[0], { from, id: o.wamid, timestamp: String(o.ts) });
  value.messages[0][kind].id = mediaId;
  return p;
}

export function statusPayload(wamid: string, status: string, recipient: string, ts: number) {
  const p = clone(statusFixture) as any;
  Object.assign(p.entry[0].changes[0].value.statuses[0], {
    id: wamid,
    status,
    recipient_id: recipient,
    timestamp: String(ts),
  });
  return p;
}

export function setup(opts: { config?: Partial<GatewayConfig>; db?: D1Database } = {}) {
  const clock = { now: T0 };
  const graph = fakeGraph();
  const logs: LogEvent[] = [];
  const config: GatewayConfig = {
    metaAccessToken: SECRETS.access,
    metaAppSecret: SECRETS.app,
    metaVerifyToken: SECRETS.verify,
    metaPhoneNumberId: PNID,
    adminToken: SECRETS.admin,
    graphVersion: "v26.0",
    displayName: "Dynasty",
    ...opts.config,
  };
  const deps: Deps = {
    db: opts.db ?? env.DB,
    config,
    now: () => clock.now,
    fetch: graph.fetch,
    log: (e) => logs.push(e),
  };
  const gateway = createGateway(deps);

  async function call(
    method: string,
    path: string,
    o: { token?: string | null; body?: unknown; raw?: BodyInit; headers?: Record<string, string> } = {},
  ): Promise<Answer> {
    const headers: Record<string, string> = { ...o.headers };
    if (o.token) headers.authorization = `Bearer ${o.token}`;
    let body: BodyInit | undefined = o.raw;
    if (o.body !== undefined) {
      body = JSON.stringify(o.body);
      headers["content-type"] = "application/json";
    }
    const res = await gateway.handle(new Request(`${ORIGIN}${path}`, { method, headers, body }));
    const type = res.headers.get("content-type") ?? "";
    const parsed = type.includes("json") ? await res.json() : await res.text();
    return { status: res.status, body: parsed, headers: res.headers };
  }

  const admin = (method: string, path: string, body?: unknown) =>
    call(method, path, { token: SECRETS.admin, body });

  async function enrollCode(cityName: string): Promise<string> {
    const r = await admin("POST", "/v1/admin/enroll-codes", { city_name: cityName });
    if (r.status !== 200) throw new Error(`enroll-codes answered ${r.status}`);
    return r.body.code as string;
  }

  async function enrollCity(name = `City ${uniq()}`): Promise<City> {
    const code = await enrollCode(name);
    const r = await call("POST", "/v1/enroll", { body: { code, name: `box-${uniq()}` } });
    if (r.status !== 200) throw new Error(`enroll answered ${r.status}`);
    return { id: r.body.city_id, token: r.body.credential, name };
  }

  async function webhook(payload: unknown, signature?: string | null): Promise<Answer> {
    const raw = new TextEncoder().encode(JSON.stringify(payload));
    const header = signature === undefined ? await signatureHeader(raw, SECRETS.app) : signature;
    return call("POST", "/meta/webhook", {
      raw,
      headers: header === null ? {} : { "x-hub-signature-256": header },
    });
  }

  async function say(from: string, text: string, o: InboundOpts = {}) {
    const wamid = o.wamid ?? `wamid.IN${uniq()}`;
    const res = await webhook(textPayload(from, text, { ...o, wamid, ts: o.ts ?? clock.now }));
    return { res, wamid };
  }

  async function invite(city: City): Promise<string> {
    const r = await call("POST", "/v1/invites", { token: city.token });
    if (r.status !== 200) throw new Error(`invites answered ${r.status}`);
    return r.body.code as string;
  }

  /** Binds a fresh phone to the city by its invite code; returns the phone. */
  async function join(city: City, wa = phone()): Promise<string> {
    const code = await invite(city);
    const { res } = await say(wa, code);
    if (res.status !== 200) throw new Error(`join webhook answered ${res.status}`);
    return wa;
  }

  const inbox = (city: City, query = "") => call("GET", `/v1/inbox${query}`, { token: city.token });
  const ack = (city: City, ids: unknown) => call("POST", "/v1/inbox/ack", { token: city.token, body: { ids } });
  const send = (city: City, body: Record<string, unknown>) =>
    call("POST", "/v1/send", { token: city.token, body });
  const window = (city: City) => call("GET", "/v1/window", { token: city.token });

  /** Every held delivery of the city, oldest first, read straight from D1. */
  async function held(city: City) {
    const { results } = await deps.db
      .prepare("SELECT id, kind, wamid, status FROM deliveries WHERE city_id = ? AND body IS NOT NULL ORDER BY id")
      .bind(city.id)
      .all<{ id: number; kind: string; wamid: string; status: string }>();
    return results;
  }

  return {
    clock,
    graph,
    logs,
    config,
    deps,
    gateway,
    call,
    admin,
    enrollCode,
    enrollCity,
    webhook,
    say,
    invite,
    join,
    inbox,
    ack,
    send,
    window,
    held,
  };
}
