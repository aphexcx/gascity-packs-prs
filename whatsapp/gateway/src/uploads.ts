import mediaTypes from "../../media-types.json";
import { GraphError, graphUpload, sha256Hex, type GraphClient } from "./meta.js";

export type MediaKind = "image" | "document" | "audio" | "video";
export const UPLOAD_SECONDS = 30 * 86_400;
export const UPLOAD_CLAIM_SECONDS = 180;
export const isMediaKind = (v: unknown): v is MediaKind =>
  v === "image" || v === "document" || v === "audio" || v === "video";
export const validFilename = (v: unknown): v is string =>
  typeof v === "string" && v.length > 0 && v.length <= 255 && !/[\x00-\x1f\x7f/\\]/.test(v) && v !== "." && v !== "..";

const json = (body: unknown, status = 200) => Response.json(body, { status, headers: { "cache-control": "no-store" } });
const fail = (status: number, error: string, extra = {}) => json({ error, ...extra }, status);
type Row = { fingerprint: string; media_id: string; meta_id: string | null; state: string; created_at: number; expires_at: number };

/** Never reads a whole file; only the current input chunk and multipart framing are held. */
export function multipartStream(input: ReadableStream<Uint8Array>, length: number, mime: string, filename: string) {
  const boundary = `wa-${crypto.randomUUID()}`;
  const encoder = new TextEncoder();
  const safeName = filename.replace(/[^\x20-\x7e]|["\\]/g, "_");
  const head = encoder.encode(`--${boundary}\r\nContent-Disposition: form-data; name="messaging_product"\r\n\r\nwhatsapp\r\n`
    + `--${boundary}\r\nContent-Disposition: form-data; name="type"\r\n\r\n${mime}\r\n`
    + `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="${safeName}"\r\nContent-Type: ${mime}\r\n\r\n`);
  const tail = encoder.encode(`\r\n--${boundary}--\r\n`);
  const reader = input.getReader();
  const state = { mismatch: false, complete: false, forwarded: 0 };
  let started = false;
  let excess = false;
  const body = new ReadableStream<Uint8Array>({
    async pull(controller) {
      if (!started) { started = true; controller.enqueue(head); return; }
      try {
        if (excess) throw new Error("media_length_mismatch");
        const next = await reader.read();
        if (next.done) {
          if (state.forwarded !== length) throw new Error("media_length_mismatch");
          state.complete = true;
          controller.enqueue(tail);
          controller.close();
          reader.releaseLock();
          return;
        }
        const remaining = length - state.forwarded;
        const count = Math.min(remaining, next.value.byteLength);
        excess = next.value.byteLength > remaining;
        // The permitted prefix is delivered before the next pull reports excess.
        if (count > 0) { controller.enqueue(next.value.subarray(0, count)); state.forwarded += count; }
        else if (excess) throw new Error("media_length_mismatch");
      } catch {
        state.mismatch = true;
        controller.error(new Error("media_length_mismatch"));
        await reader.cancel().catch(() => {});
      }
    },
    async cancel(reason) { await reader.cancel(reason).catch(() => {}); },
  }, { highWaterMark: 0 });
  return { body, state, contentType: `multipart/form-data; boundary=${boundary}` };
}

export async function upload(request: Request, cityId: string, db: D1Database, now: number, client: () => GraphClient) {
  const headers = request.headers;
  const rawLength = headers.get("content-length") ?? "";
  if (!/^[1-9]\d*$/.test(rawLength) || headers.has("transfer-encoding")) return fail(411, "length_required");
  const length = Number(rawLength);
  if (!Number.isSafeInteger(length)) return fail(411, "length_required");
  const mime = (headers.get("content-type") ?? "").toLowerCase();
  if (!Object.hasOwn(mediaTypes, mime)) return fail(415, "unsupported_media_type");
  const rule = mediaTypes[mime as keyof typeof mediaTypes];
  if (length > rule.maxBytes) return fail(413, "media_too_large");
  const key = headers.get("idempotency-key") ?? "";
  const sha = headers.get("x-content-sha256") ?? "";
  let name = "";
  try { name = decodeURIComponent(headers.get("x-filename") ?? ""); } catch { return fail(400, "bad_upload"); }
  if (!validFilename(name) || !/^[\x20-\x7e]{1,200}$/.test(key) || !/^[a-f0-9]{64}$/.test(sha)
    || headers.has("content-encoding") || !request.body) return fail(400, "bad_upload");
  const fingerprint = await sha256Hex(JSON.stringify([mime, name, length, sha]));
  const prior = () => db.prepare("SELECT fingerprint, media_id, meta_id, state, created_at, expires_at FROM uploads WHERE city_id = ? AND idempotency_key = ?")
    .bind(cityId, key).first<Row>();
  const repeat = (row: Row) => {
    if (row.fingerprint !== fingerprint) return fail(409, "idempotency_conflict");
    if (row.state === "length_mismatch") return fail(400, "media_length_mismatch");
    if (row.expires_at <= now) return fail(410, "media_expired");
    if (row.state === "ready" && row.meta_id) return json({ media_id: row.media_id, expires_at: row.expires_at });
    return fail(409, row.state === "claimed" && row.created_at > now - UPLOAD_CLAIM_SECONDS ? "in_flight" : "outcome_unknown");
  };
  const known = await prior();
  if (known) return repeat(known);
  const graph = client();
  const handle = crypto.randomUUID();
  const expires = now + UPLOAD_SECONDS;
  const claim = await db.prepare(`INSERT INTO uploads
    (city_id, idempotency_key, fingerprint, media_id, kind, mime, filename, bytes, sha256, state, created_at, expires_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'claimed', ?, ?) ON CONFLICT (city_id, idempotency_key) DO NOTHING`)
    .bind(cityId, key, fingerprint, handle, rule.kind, mime, name, length, sha, now, expires).run();
  if (claim.meta.changes !== 1) { const raced = await prior(); return raced ? repeat(raced) : fail(409, "in_flight"); }
  const stream = multipartStream(request.body, length, mime, name);
  const setState = (state: string) => db.prepare("UPDATE uploads SET state = ? WHERE city_id = ? AND idempotency_key = ?")
    .bind(state, cityId, key).run();
  let metaId: string;
  try {
    metaId = await graphUpload(graph, stream.body, stream.contentType);
    // A transport that does not consume the request must never mint a grant.
    if (!stream.state.complete || stream.state.mismatch) throw new GraphError(0, null);
  } catch (e) {
    if (!stream.body.locked) await stream.body.cancel().catch(() => {});
    if (stream.state.mismatch) { await setState("length_mismatch"); return fail(400, "media_length_mismatch"); }
    if (!(e instanceof GraphError)) throw e;
    if (e.status >= 400 && e.status < 500) {
      await db.prepare("DELETE FROM uploads WHERE city_id = ? AND idempotency_key = ?").bind(cityId, key).run();
      return fail(502, "meta_error", { meta_status: e.status, meta_code: e.code });
    }
    await setState("unknown");
    return fail(502, "outcome_unknown", { meta_status: e.status, meta_code: e.code });
  }
  // A failed write leaves the durable claim spent. Never return an unrecorded handle.
  await db.prepare("UPDATE uploads SET state = 'ready', meta_id = ? WHERE city_id = ? AND idempotency_key = ?")
    .bind(metaId, cityId, key).run();
  return json({ media_id: handle, expires_at: expires });
}
