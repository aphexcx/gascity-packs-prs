import { env, SELF, runInDurableObject, evictDurableObject } from "cloudflare:test";
import { afterEach, describe, expect, it } from "vitest";
import wakeSource from "../src/wake.ts?raw";
import { setup, changeAs, flakyDb, statusAs, textAs, uniq, type City } from "./harness.js";
import { createWake } from "../src/wake-client.js";
import { sha256Hex } from "../src/meta.js";
import worker from "../src/index.js";

const sockets: WebSocket[] = [];
const frames: unknown[] = [];
afterEach(() => {
  for (const socket of sockets.splice(0)) socket.close(1000);
  for (const frame of frames.splice(0)) expect(["wake", "pong"]).toContain(frame);
});

function request(token?: string, upgrade = true) {
  const headers = new Headers();
  if (token) headers.set("authorization", `Bearer ${token}`);
  if (upgrade) headers.set("upgrade", "websocket");
  return new Request("https://gateway.test/v1/wake", { headers });
}

function receive(socket: WebSocket): Promise<MessageEvent> {
  return new Promise((resolve) => socket.addEventListener("message", resolve, { once: true }));
}

function harness(db = env.DB) {
  const h = setup({ db });
  h.deps.wake = createWake(env.CITY_WAKE);
  return h;
}

async function open(city: City, headers: Record<string, string> = {}, query = "") {
  const req = request(city.token);
  for (const [key, value] of Object.entries(headers)) req.headers.set(key, value);
  const response = await SELF.fetch(new Request(`${req.url}${query}`, req));
  expect(response.status).toBe(101);
  const socket = response.webSocket!;
  socket.accept();
  sockets.push(socket);
  const received: unknown[] = [];
  socket.addEventListener("message", (event) => { frames.push(event.data); received.push(event.data); });
  return { socket, received };
}

const owner = () => ({ user: `US.Test${uniq()}` });

async function joined(h = harness()) {
  const city = await h.enrollCity();
  const ids = owner();
  await h.joinAs(city, ids);
  return { h, city, ids };
}

async function barrier(socket: WebSocket) {
  // The auto-response follows all earlier frames; a pong is a deterministic silence barrier.
  const answer = receive(socket);
  socket.send("ping");
  expect((await answer).data).toBe("pong");
}

describe("held connection", () => {
  it.each(["missing", "unknown", "revoked"])("refuses a %s bearer before upgrade", async (kind) => {
    const h = setup();
    const city = await h.enrollCity();
    if (kind === "revoked") await env.DB.prepare("UPDATE credentials SET revoked_at = 1 WHERE city_id = ?").bind(city.id).run();
    const res = await SELF.fetch(request(kind === "missing" ? undefined : kind === "unknown" ? "invented-unknown" : city.token));
    expect(res.status).toBe(401);
    expect(await res.json()).toEqual({ error: "unauthorized" });
    expect(res.webSocket).toBeNull();
  });

  it("requires an upgrade from a live bearer and preserves method routing", async () => {
    const city = await setup().enrollCity();
    const res = await SELF.fetch(request(city.token, false));
    expect(res.status).toBe(426);
    expect(await res.json()).toEqual({ error: "upgrade_required" });
    expect((await worker.fetch(new Request(request(city.token, false), { method: "POST" }), env)).status).toBe(405);
  });

  it("holds a real hibernating socket and automatically answers ping without storage or an alarm", async () => {
    const city = await setup().enrollCity();
    const res = await SELF.fetch(request(city.token));
    expect(res.status).toBe(101);
    const socket = res.webSocket!;
    sockets.push(socket);
    socket.accept();
    socket.addEventListener("message", (event) => { frames.push(event.data); });
    const pong = receive(socket);
    socket.send("ping");
    expect((await pong).data).toBe("pong");
    const namespace = (env as any).CITY_WAKE;
    await runInDurableObject(namespace.get(namespace.idFromName(city.id)), async (_instance, state) => {
      expect(state.getWebSockets()).toHaveLength(1);
      expect(await state.storage.list()).toEqual(new Map());
      expect(await state.storage.getAlarm()).toBeNull();
    });
  });

  it.each(["joined", "message", "status", "adopted status", "link retry"])("wakes after the %s delivery commit", async (site) => {
    const { h, city, ids } = await joined();
    if (site === "link retry") {
      const next = owner();
      await h.webhook(changeAs("user_changed_user_id", { previousUser: ids.user, newUser: next.user }, { wamid: `wamid.notice${uniq()}`, ts: h.clock.now + 10 }));
      h.clock.now += 20;
      expect(await env.DB.prepare("SELECT COUNT(*) FROM phones WHERE user_id = ?").bind(ids.user).first("COUNT(*)")).toBe(0);
    }
    const { socket } = await open(city);
    const wake = receive(socket);
    if (site === "joined") await h.joinAs(city, owner());
    if (site === "message") await h.sayAs(ids, "invented message");
    if (site === "status" || site === "adopted status") {
      if (site === "adopted status") h.graph.hooks.onSend = async (wamid) => {
        expect((await h.webhook(statusAs(wamid, "delivered", ids, h.clock.now))).status).toBe(200);
      };
      const sent = await h.send(city, { to: ids.user, type: "text", text: "invented reply", idempotency_key: uniq() });
      expect(sent.status).toBe(200);
      if (site === "status") await h.webhook(statusAs(sent.body.message_id, "delivered", ids, h.clock.now));
    }
    if (site === "link retry") {
      // Old user id absent from phones, but its historical span still routes this retry.
      await h.sayAs(ids, "invented old-span retry", { ts: h.clock.now - 15 });
    }
    expect((await wake).data).toBe("wake");
    const namespace = env.CITY_WAKE;
    await runInDurableObject(namespace.get(namespace.idFromName(city.id)), async (_instance, state) => {
      expect(await state.storage.list()).toEqual(new Map());
      expect(await state.storage.getAlarm()).toBeNull();
    });
  });

  it("an adopted status wakes once while a lost-answer replay keeps the accepted send attempt", async () => {
    const { h, city, ids } = await joined();
    const peer = await open(city);
    h.graph.hooks.onSend = async (wamid) => {
      expect((await h.webhook(statusAs(wamid, "delivered", ids, h.clock.now))).status).toBe(200);
    };
    const body = { to: ids.user, type: "text", text: "invented reply", idempotency_key: uniq() };
    const before = h.graph.sends().length;
    const wake = receive(peer.socket);
    const first = await h.call("POST", "/v1/send", { token: city.token, body,
      headers: { "x-gc-send-attempt": "example-original-attempt" } });
    expect(first.status).toBe(200);
    expect((await wake).data).toBe("wake");
    expect(first.headers.get("x-gc-replayed")).toBe("false");
    expect(first.headers.get("x-gc-send-attempt")).toBe("example-original-attempt");
    // Treat the first answer as lost; a retry's proposed attempt cannot turn old words into new activity.
    const replay = await h.call("POST", "/v1/send", { token: city.token, body,
      headers: { "x-gc-send-attempt": "example-retry-attempt" } });
    expect(replay.status).toBe(200);
    expect(replay.body).toEqual(first.body);
    expect(replay.headers.get("x-gc-replayed")).toBe("true");
    expect(replay.headers.get("x-gc-send-attempt")).toBe("example-original-attempt");
    await barrier(peer.socket);
    expect(peer.received.filter((frame) => frame === "wake")).toHaveLength(1);
    expect(h.graph.sends()).toHaveLength(before + 1);
    expect((await h.held(city)).filter((row) => row.kind === "status" && row.wamid === first.body.message_id)).toHaveLength(1);
  });

  it("wakes committed rows even when a later webhook batch fails", async () => {
    const flaky = flakyDb();
    const { h, city, ids } = await joined(harness(flaky.db));
    const payload = textAs(ids, "invented bulk", { wamid: `wamid.bulk${uniq()}`, ts: h.clock.now });
    const messages = payload.entry[0].changes[0].value.messages;
    const first = messages[0];
    for (let i = 0; i < 160; i++) messages.push({ ...first, id: `wamid.bulk${uniq()}` });
    const { socket } = await open(city);
    const wake = receive(socket);
    flaky.arm(1);
    expect((await h.webhook(payload)).status).toBe(500);
    expect((await h.held(city)).length).toBeGreaterThan(1);
    expect((await wake).data).toBe("wake");
  });

  it("keeps cities separate and ignores spoofed routing and credential headers", async () => {
    const a = await joined();
    const b = await joined(a.h);
    const sa = await open(a.city, { "x-wake-credential": await sha256Hex(b.city.token), "x-wake-city": b.city.id },
      `?city=${b.city.id}&credential_hash=${await sha256Hex(b.city.token)}`);
    const sb = await open(b.city);
    const wake = receive(sb.socket);
    await a.h.sayAs(b.ids, "invented city B message");
    expect((await wake).data).toBe("wake");
    await barrier(sa.socket);
    expect(sa.received).toEqual(["pong"]);
    const next = receive(sa.socket);
    await a.h.sayAs(a.ids, "invented city A message");
    expect((await next).data).toBe("wake");
  });

  it("closes a SQL-revoked credential with 4401 without wake while a second credential reads the held row", async () => {
    const { h, city, ids } = await joined();
    const second = { ...city, token: `invented-second-${uniq()}` };
    await env.DB.prepare("INSERT INTO credentials(token_sha256, city_id, created_at) VALUES (?, ?, ?)")
      .bind(await sha256Hex(second.token), city.id, h.clock.now).run();
    const old = await open(city);
    const live = await open(second);
    const closed = new Promise<CloseEvent>((resolve) => old.socket.addEventListener("close", resolve, { once: true }));
    const wake = receive(live.socket);
    await env.DB.prepare("UPDATE credentials SET revoked_at = ? WHERE token_sha256 = ?")
      .bind(h.clock.now, await sha256Hex(city.token)).run();
    await h.sayAs(ids, "invented after revocation");
    expect((await closed).code).toBe(4401);
    expect(old.received).toEqual([]);
    expect((await wake).data).toBe("wake");
    expect((await h.inbox(second)).body.deliveries).toHaveLength(2);
  });

  it.each(["throws", "hangs"])("bounds a wake that %s, holds the row, and logs counts only", async (failure) => {
    const { h, city, ids } = await joined();
    const before = await h.stat("wake_failed");
    h.deps.wake!.notify = () => failure === "throws" ? Promise.reject(new Error(`sensitive-${city.token}`)) : new Promise(() => {});
    const started = performance.now();
    expect((await h.sayAs(ids, "invented wake failure")).res.status).toBe(200);
    expect(performance.now() - started).toBeLessThan(2300);
    expect((await h.inbox(city)).body.deliveries).toHaveLength(2);
    expect(await h.stat("wake_failed")).toBe(before + 1);
    expect(h.logs.filter((line) => line.event === "wake_failed")).toEqual([{ event: "wake_failed", count: 1 }]);
  });

  it.each(["webhook", "adopted status", "rollback inbox"])("bounds stalled failure accounting on %s", async (site) => {
    const { h, city, ids } = await joined();
    let counterAttempts = 0;
    h.deps.db = {
      prepare: (query: string) => {
        if (query.includes("VALUES ('wake_failed',")) return {
          bind: () => ({ run: () => { counterAttempts += 1; return new Promise(() => {}); } }),
        } as unknown as D1PreparedStatement;
        return env.DB.prepare(query);
      },
      batch: (statements: D1PreparedStatement[]) => env.DB.batch(statements),
      exec: (query: string) => env.DB.exec(query),
    } as unknown as D1Database;
    h.deps.wake = {
      enabled: site !== "rollback inbox", open: () => { throw new Error("unused"); },
      notify: () => Promise.reject(new Error("invented notify failure")),
    };
    if (site === "adopted status") h.graph.hooks.onSend = async (wamid) => {
      expect((await h.webhook(statusAs(wamid, "delivered", ids, h.clock.now))).status).toBe(200);
    };
    const response = site === "webhook" ? h.sayAs(ids, "invented accounting failure").then(({ res }) => res)
      : site === "adopted status" ? h.send(city, { to: ids.user, type: "text", text: "invented reply", idempotency_key: uniq() })
      : h.inbox(city);
    let timer: ReturnType<typeof setTimeout> | undefined;
    try {
      const result = await Promise.race([response, new Promise<undefined>((resolve) => { timer = setTimeout(resolve, 2300); })]);
      expect(result?.status).toBe(200);
      expect(counterAttempts).toBe(1);
      expect(h.logs.filter((line) => line.event === "wake_failed")).toEqual([{ event: "wake_failed", count: 1 }]);
      h.deps.wake = undefined;
      expect((await h.inbox(city)).body.deliveries).toHaveLength(site === "rollback inbox" ? 1 : 2);
    } finally { if (timer !== undefined) clearTimeout(timer); }
  });

  it("closes the oldest of five sockets and accepts only ping frames", async () => {
    const city = await setup().enrollCity();
    const oldest = await open(city);
    const closed = new Promise<CloseEvent>((resolve) => oldest.socket.addEventListener("close", resolve, { once: true }));
    const rest: Awaited<ReturnType<typeof open>>[] = [];
    for (let i = 0; i < 3; i++) rest.push(await open(city));
    await evictDurableObject(env.CITY_WAKE.get(env.CITY_WAKE.idFromName(city.id)));
    rest.push(await open(city));
    expect((await closed).code).toBe(1008);
    for (const peer of rest) await barrier(peer.socket);
    const invalid = new Promise<CloseEvent>((resolve) => rest[0]!.socket.addEventListener("close", resolve, { once: true }));
    rest[0]!.socket.send("invented-invalid-frame");
    expect((await invalid).code).toBe(1008);
  });

  it("has no timers or alarms in the object source", () => {
    expect(wakeSource).not.toMatch(/\b(?:setTimeout|setInterval|setAlarm)\b/);
  });

  it("preserves newest-first socket enumeration across hibernation with only hash attachments", async () => {
    const city = await setup().enrollCity();
    const hashes: string[] = [];
    for (let i = 0; i < 3; i++) {
      const token = `invented-order-${uniq()}`;
      const hash = await sha256Hex(token);
      hashes.push(hash);
      await env.DB.prepare("INSERT INTO credentials(token_sha256, city_id, created_at) VALUES (?, ?, 1)").bind(hash, city.id).run();
      await open({ ...city, token });
    }
    const stub = env.CITY_WAKE.get(env.CITY_WAKE.idFromName(city.id));
    const order = () => runInDurableObject(stub, (_instance, state) => state.getWebSockets().map((socket) => hashes.indexOf(socket.deserializeAttachment())));
    const before = await order();
    await evictDurableObject(stub);
    expect({ before, after: await order() }).toEqual({ before: [2, 1, 0], after: [2, 1, 0] });
  });

  it.each(["on", "off", "missing"])("preserves ordinary inbox and ack with no socket when the feature is %s", async (mode) => {
    const { h, city, ids } = await joined();
    const settings = { ...env, WAKE: mode === "off" ? "off" : "on", CITY_WAKE: mode === "missing" ? undefined : env.CITY_WAKE };
    const response = await worker.fetch(request(city.token, mode !== "on"), settings);
    if (mode === "on") {
      expect(response.status).toBe(426);
    } else {
      expect(response.status).toBe(404);
      expect(await response.json()).toEqual({ error: "not_found" });
    }
    h.deps.wake = createWake(settings.CITY_WAKE, settings.WAKE);
    expect((await h.sayAs(ids, "invented polling message")).res.status).toBe(200);
    const inbox = await h.inbox(city);
    expect(inbox.body.deliveries).toHaveLength(2);
    expect((await h.ack(city, inbox.body.deliveries.map((row: any) => row.id))).status).toBe(200);
    expect((await h.inbox(city)).body.deliveries).toEqual([]);
  });

  it.each(["bad signature", "pending cap", "stranger"])("attempts no wake when nothing is stored: %s", async (cause) => {
    const { h, city, ids } = await joined();
    let attempts = 0;
    h.deps.wake!.notify = async () => { attempts += 1; };
    if (cause === "pending cap") await env.DB.prepare(
      `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 4999)
       INSERT INTO deliveries (city_id, received_at, kind, wamid, status, body)
       SELECT ?, ?, 'message', ? || i, '', '{}' FROM n`,
    ).bind(city.id, h.clock.now, `wamid.invented-cap${uniq()}`).run();
    const payload = textAs(cause === "stranger" ? owner() : ids, "invented ignored text", { wamid: `wamid.ignored${uniq()}`, ts: h.clock.now });
    expect((await h.webhook(payload, cause === "bad signature" ? "sha256=00" : undefined)).status)
      .toBe(cause === "bad signature" ? 401 : cause === "pending cap" ? 503 : 200);
    expect(attempts).toBe(0);
  });

  it("adds one post-commit lookup and no write statements on a successful wake", async () => {
    const prepared: string[] = [];
    const db = {
      prepare: (query: string) => { prepared.push(query); return env.DB.prepare(query); },
      batch: (statements: D1PreparedStatement[]) => env.DB.batch(statements),
      exec: (query: string) => env.DB.exec(query),
    } as unknown as D1Database;
    const { h, city, ids } = await joined(harness(db));
    const wake = h.deps.wake;
    h.deps.wake = undefined;
    prepared.length = 0;
    await h.sayAs(ids, "invented poll-only delivery");
    const baseline = [...prepared];
    h.deps.wake = wake;
    prepared.length = 0;
    await h.sayAs(ids, "invented held delivery");
    const writes = (queries: string[]) => queries.filter((query) => /^\s*(?:INSERT|UPDATE|DELETE)/.test(query));
    expect(writes(prepared)).toEqual(writes(baseline));
    expect(prepared).toHaveLength(baseline.length + 1);
    expect((await h.inbox(city)).body.deliveries).toHaveLength(3);
  });

  it("rejects a binary client frame with 1008", async () => {
    const peer = await open(await setup().enrollCity());
    const closed = new Promise<CloseEvent>((resolve) => peer.socket.addEventListener("close", resolve, { once: true }));
    peer.socket.send(new Uint8Array([1, 2, 3]));
    expect((await closed).code).toBe(1008);
  });

  it.each(["message", "inbox"])("WAKE off closes existing sockets with 4503 on the next %s without a wake", async (trigger) => {
    const { h, city, ids } = await joined();
    const peer = await open(city);
    const closed = new Promise<CloseEvent>((resolve) => peer.socket.addEventListener("close", resolve, { once: true }));
    h.deps.wake = createWake(env.CITY_WAKE, "off");
    const started = performance.now();
    if (trigger === "message") expect((await h.sayAs(ids, "invented switch-off message")).res.status).toBe(200);
    else expect((await h.inbox(city)).status).toBe(200);
    expect((await closed).code).toBe(4503);
    expect(performance.now() - started).toBeLessThan(2300);
    expect(peer.received).toEqual([]);
    expect((await h.inbox(city)).body.deliveries).toHaveLength(trigger === "message" ? 2 : 1);
  });

  it.each(["throws", "hangs"])("bounds a rollback close that %s without changing the inbox response", async (failure) => {
    const { h, city } = await joined();
    h.deps.wake = createWake(env.CITY_WAKE, "off");
    const before = await h.stat("wake_failed");
    h.deps.wake!.notify = () => failure === "throws" ? Promise.reject(new Error("invented close failure")) : new Promise(() => {});
    const started = performance.now();
    const response = await h.inbox(city);
    expect(response.status).toBe(200);
    expect(response.body.deliveries).toHaveLength(1);
    expect(performance.now() - started).toBeLessThan(2300);
    expect(await h.stat("wake_failed")).toBe(before + 1);
    expect(h.logs.filter((line) => line.event === "wake_failed")).toEqual([{ event: "wake_failed", count: 1 }]);
  });
});
