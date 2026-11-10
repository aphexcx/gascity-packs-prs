import { env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import types from "../../media-types.json";
import { setup, uniq, userOf, statusPayload } from "./harness.js";
import { multipartStream, UPLOAD_SECONDS } from "../src/uploads.js";

const encoder = new TextEncoder();
const bytes = encoder.encode("private-file-bytes-755k");
function headers(mime = "image/jpeg", length = bytes.length, key = uniq()) {
  return { "content-type": mime, "content-length": String(length), "x-filename": "sample.jpg",
    "x-content-sha256": "a".repeat(64), "idempotency-key": key };
}
const uploadCalls = (s: ReturnType<typeof setup>) => s.graph.calls.filter((c) => c.url.pathname.endsWith("/media"));

describe("files out", () => {
  it("streams multipart to Meta and persists only metadata; upload and send retries are once", async () => {
    const s = setup(), city = await s.enrollCity(), owner = await s.join(city);
    const h = headers();
    const first = await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h });
    expect(first.status).toBe(200);
    expect(first.body.expires_at).toBe(s.clock.now + UPLOAD_SECONDS);
    expect(await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h })).toMatchObject({ body: first.body });
    const uploads = uploadCalls(s);
    expect(uploads).toHaveLength(1);
    expect(uploads[0]!.body!.chunks.length).toBeGreaterThan(1);
    expect(uploads[0]!.body!.complete).toBe(true);
    expect(new TextDecoder().decode(uploads[0]!.body!.chunks[1])).toBe("private-file-bytes-755k");
    const rows = await env.DB.prepare("SELECT * FROM uploads WHERE city_id = ?").bind(city.id).all();
    expect(JSON.stringify(rows.results)).not.toContain("private-file-bytes-755k");
    expect(rows.results[0]).toMatchObject({ bytes: bytes.length, mime: "image/jpeg", state: "ready" });
    const send = { to: owner, idempotency_key: uniq(), type: "image", media: { id: first.body.media_id, caption: "hello" } };
    const sent = await s.call("POST", "/v1/send", { token: city.token, body: send });
    expect(sent.status).toBe(200);
    const count = s.graph.sends().length;
    s.clock.now += 86400;
    expect((await s.call("POST", "/v1/send", { token: city.token, body: send })).body).toEqual(sent.body);
    expect(s.graph.sends()).toHaveLength(count);
    expect(s.graph.sends().at(-1)!.body).toMatchObject({ type: "image", recipient: userOf(owner), image: { caption: "hello" } });
    expect(s.graph.sends().at(-1)!.body!.image.id).not.toBe(first.body.media_id);
    const status = statusPayload(sent.body.message_id, "delivered", owner, s.clock.now);
    await s.webhook(status); await s.webhook(status);
    const held = await s.call("GET", "/v1/inbox", { token: city.token });
    expect(held.body.deliveries.filter((d: any) => d.kind === "status" && d.message.id === sent.body.message_id)).toHaveLength(1);
  });

  for (const [mime, rule] of Object.entries(types)) {
    it(`refuses ${mime} over its ${rule.maxBytes} cap before Meta`, async () => {
      const s = setup(), city = await s.enrollCity();
      const out = await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: headers(mime, rule.maxBytes + 1) });
      expect(out).toMatchObject({ status: 413, body: { error: "media_too_large" } });
      expect(s.graph.calls).toHaveLength(0);
    });
  }
  for (const value of [undefined, "", "0", "-1", "3.5", "abc"]) {
    it(`refuses missing/invalid length ${value} before Meta`, async () => {
      const s = setup(), city = await s.enrollCity(), h: Record<string, string> = headers();
      if (value === undefined) delete h["content-length"]; else h["content-length"] = value;
      // A stream prevents Request from inferring a byte length for a Buffer body.
      const raw = new ReadableStream({ start(c) { c.enqueue(bytes); c.close(); } });
      const out = await s.call("POST", "/v1/uploads", { token: city.token, raw, headers: h });
      expect(out.status).toBe(411);
      expect(s.graph.calls).toHaveLength(0);
    });
  }
  it("refuses chunked, encoded, off-list and unauthenticated uploads without Meta", async () => {
    const s = setup(), city = await s.enrollCity();
    for (const [extra, status] of [[{ "transfer-encoding": "chunked" }, 411], [{ "content-encoding": "gzip" }, 400], [{ "content-type": "image/gif" }, 415]] as const) {
      expect((await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: { ...headers(), ...extra } })).status).toBe(status);
    }
    expect((await s.call("POST", "/v1/uploads", { raw: bytes, headers: headers() })).status).toBe(401);
    expect(s.graph.calls).toHaveLength(0);
  });
  for (const length of [bytes.length - 3, bytes.length + 3]) {
    it(`aborts a mismatched stream declared ${length}: no handle and no retry`, async () => {
      const s = setup(), city = await s.enrollCity(), h = headers("image/jpeg", length);
      const raw = () => new ReadableStream({ start(c) { c.enqueue(bytes); c.close(); } });
      const out = await s.call("POST", "/v1/uploads", { token: city.token, raw: raw(), headers: h });
      expect(out).toMatchObject({ status: 400, body: { error: "media_length_mismatch" } });
      expect(out.body.media_id).toBeUndefined();
      const call = uploadCalls(s)[0]!;
      expect(call.body!.complete).toBe(false);
      expect(call.body!.chunks.slice(1).reduce((n: number, b: Uint8Array) => n + b.length, 0)).toBe(Math.min(length, bytes.length));
      expect((await s.call("POST", "/v1/uploads", { token: city.token, raw: raw(), headers: h })).body.error).toBe("media_length_mismatch");
      expect(uploadCalls(s)).toHaveLength(1);
      expect((await env.DB.prepare("SELECT meta_id, state FROM uploads WHERE city_id = ?").bind(city.id).first())).toEqual({ meta_id: null, state: "length_mismatch" });
    });
  }
  it("does not pre-read the file when building the multipart stream", async () => {
    let reads = 0;
    const raw = new ReadableStream<Uint8Array>({ pull(c) { reads++; c.enqueue(bytes); c.close(); } }, { highWaterMark: 0 });
    const stream = multipartStream(raw, bytes.length, "image/jpeg", "photo.jpg");
    expect(reads).toBe(0);
    const reader = stream.body.getReader();
    await reader.read();
    expect(reads).toBe(0); // framing first; bytes only as the consumer requests them
    await reader.read();
    expect(reads).toBe(1);
    await reader.cancel();
  });
  it("enforces city, kind, expiry, caption and window grants", async () => {
    const s = setup(), a = await s.enrollCity(), b = await s.enrollCity();
    const owner = await s.join(a), other = await s.join(b);
    const up = await s.call("POST", "/v1/uploads", { token: a.token, raw: bytes, headers: headers() });
    const send = (token: string, to: string, id = up.body.media_id, type = "image", media = {}) => s.call("POST", "/v1/send", {
      token, body: { to, type, idempotency_key: uniq(), media: { id, ...media } },
    });
    const n = s.graph.sends().length;
    expect((await send(b.token, other)).status).toBe(404);
    expect((await send(a.token, owner, "unknown")).status).toBe(404);
    expect((await send(a.token, owner, up.body.media_id, "video")).status).toBe(404);
    expect((await send(a.token, owner, up.body.media_id, "audio", { caption: "no" })).status).toBe(400);
    expect((await send(a.token, owner, up.body.media_id, "image", { caption: "x".repeat(1025) })).status).toBe(400);
    expect((await send(a.token, owner, up.body.media_id, "image", { filename: "no" })).status).toBe(400);
    s.clock.now += 86400;
    expect((await send(a.token, owner)).body.error).toBe("window_closed");
    expect(s.graph.sends()).toHaveLength(n);
    s.clock.now += UPLOAD_SECONDS;
    await s.say(owner, "open");
    expect((await send(a.token, owner)).body.error).toBe("media_not_found");
  });
  for (const [kind, mime] of [["document", "application/pdf"], ["audio", "audio/ogg"], ["video", "video/mp4"]]) {
    it(`sends ${kind} through its upload grant`, async () => {
      const s = setup(), city = await s.enrollCity(), owner = await s.join(city);
      const up = await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: headers(mime) });
      const media = { id: up.body.media_id, ...(kind === "document" ? { filename: "invoice.pdf", caption: "invoice" } : {}) };
      expect((await s.call("POST", "/v1/send", { token: city.token, body: { to: owner, type: kind, media, idempotency_key: uniq() } })).status).toBe(200);
      expect(s.graph.sends().at(-1)!.body!.type).toBe(kind);
    });
  }
  for (const status of [400, 500]) {
    it(`handles Meta ${status} on upload and send with text's uncertainty rule`, async () => {
      const s = setup(), city = await s.enrollCity(), owner = await s.join(city), h = headers();
      s.graph.failNext({ status, body: { error: { code: 100 } } });
      const first = await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h });
      expect(first.body.error).toBe(status === 400 ? "meta_error" : "outcome_unknown");
      const retry = await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h });
      expect(retry.status).toBe(status === 400 ? 200 : 409);
      expect(uploadCalls(s)).toHaveLength(status === 400 ? 2 : 1);
      const up = status === 400 ? retry : await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: headers() });
      const body = { to: owner, idempotency_key: uniq(), type: "image", media: { id: up.body.media_id } };
      s.graph.failNext({ status, body: { error: { code: 100 } } });
      expect((await s.call("POST", "/v1/send", { token: city.token, body })).body.error).toBe(status === 400 ? "meta_error" : "outcome_unknown");
      expect((await s.call("POST", "/v1/send", { token: city.token, body })).status).toBe(status === 400 ? 200 : 409);
    });
  }
  it("binds upload keys to file metadata", async () => {
    const s = setup(), city = await s.enrollCity(), h = headers();
    await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h });
    expect((await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: { ...h, "x-filename": "other.jpg" } })).body.error).toBe("idempotency_conflict");
    expect(uploadCalls(s)).toHaveLength(1);
  });
  it("serializes simultaneous upload claims and never retries an abandoned claim", async () => {
    const s = setup(), city = await s.enrollCity(), h = headers();
    let release!: () => void, reached!: () => void;
    const barrier = new Promise<void>((r) => { release = r; });
    const atMeta = new Promise<void>((r) => { reached = r; });
    s.graph.hooks.onUpload = async () => { reached(); await barrier; };
    const first = s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h });
    await atMeta;
    try {
      expect((await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h })).body.error).toBe("in_flight");
    } finally { release(); }
    expect((await first).status).toBe(200);
    expect(uploadCalls(s)).toHaveLength(1);
    await env.DB.prepare("UPDATE uploads SET state = 'claimed', meta_id = NULL, created_at = ? WHERE city_id = ?")
      .bind(s.clock.now - 181, city.id).run();
    expect((await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h })).body.error).toBe("outcome_unknown");
    expect(uploadCalls(s)).toHaveLength(1);
  });
  it("a failed upload grant write returns no handle and leaves a spent claim", async () => {
    let failReady = true;
    const db = {
      prepare(sql: string) {
        const stmt = env.DB.prepare(sql);
        if (!sql.startsWith("UPDATE uploads SET state = 'ready'")) return stmt;
        return { bind(...args: unknown[]) {
          const bound = stmt.bind(...args);
          return { async run() { if (failReady) { failReady = false; throw new Error("simulated write failure"); } return bound.run(); } };
        } } as D1PreparedStatement;
      },
      batch: (statements: D1PreparedStatement[]) => env.DB.batch(statements),
      exec: (sql: string) => env.DB.exec(sql),
    } as unknown as D1Database;
    const s = setup({ db }), city = await s.enrollCity(), h = headers();
    const out = await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h });
    expect(out.status).toBe(500); expect(out.body.media_id).toBeUndefined();
    s.clock.now += 181;
    expect((await s.call("POST", "/v1/uploads", { token: city.token, raw: bytes, headers: h })).body.error).toBe("outcome_unknown");
    expect(uploadCalls(s)).toHaveLength(1);
  });
  it("cancels an early-rejected or unconsumed body without yielding a handle", async () => {
    const s = setup(), city = await s.enrollCity();
    for (const status of [400, 500, 200]) {
      let cancelled = false, pulls = 0;
      const input = new ReadableStream<Uint8Array>({ pull(c) { pulls++; c.enqueue(bytes); }, cancel() { cancelled = true; } }, { highWaterMark: 0 });
      s.deps.fetch = (async (_url, init) => {
        // Model fetch retaining ownership even though Meta already answered.
        const reader = (init!.body as ReadableStream<Uint8Array>).getReader();
        await reader.read(); // multipart framing only
        return Response.json(status === 200 ? { id: "unconsumed" } : { error: { code: 100 } }, { status });
      }) as typeof fetch;
      const out = await s.call("POST", "/v1/uploads", { token: city.token, raw: input, headers: headers() });
      expect(out.status).toBe(502);
      expect(out.body.error).toBe(status === 400 ? "meta_error" : "outcome_unknown");
      expect(out.body.media_id).toBeUndefined();
      expect(pulls).toBe(0);
      expect(cancelled).toBe(true);
    }
  });
});
