/**
 * The Cloud API wire: the webhook signature, the payload parse, the send bodies, the Graph calls.
 * Shapes as Meta's reference pages show them (read 2026-11-04; test/fixtures/SOURCES.md).
 *
 * Nothing here logs. A GraphError carries Meta's HTTP status and numeric error code only, never
 * Meta's error text, which can quote the request.
 */

/** Meta's text limit: "Maximum 4096 characters" (counted here in code points). */
export const MAX_TEXT_CHARS = 4096;
const GRAPH_TIMEOUT_MS = 20_000;

/**
 * A business-scoped user id (BSUID): the user's ISO 3166 alpha-2 country code, a period, then up
 * to 128 alphanumerics, with ENT and a period between them on a parent id (US.13491208655302741918,
 * US.ENT.11815799212886844830). A request must carry it whole and unchanged.
 * (developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids,
 * read 2026-11-04.)
 */
export const BSUID = /^[A-Z]{2}\.(?:ENT\.)?[A-Za-z0-9]{1,128}$/;

/**
 * A user's number change, as Meta's system message reports it: the user_id Meta regenerated, the
 * one it replaced, and the new phone number when Meta shares it. user_changed_user_id is the same
 * change when no phone number can be shared.
 */
export interface IdChange {
  type: "user_changed_number" | "user_changed_user_id";
  userId: string | null;
  previousUserId: string | null;
  waId: string | null;
}

export interface InboundMessage {
  /** The wamid. */
  wamid: string;
  /** The sender's wa_id (its phone number); null when Meta shares none (a user with a username). */
  from: string | null;
  /** The sender's business-scoped user id; null on a payload from before Meta sent one. */
  fromUserId: string | null;
  /** Meta's timestamp of the message (epoch seconds), null when absent or malformed. */
  timestamp: number | null;
  type: string;
  profileName: string | null;
  /** The sender's WhatsApp username, when it has one. */
  username: string | null;
  /** Set on a number-change system message. */
  change: IdChange | null;
  /** text.body for a text message; null otherwise. */
  text: string | null;
  /** The media id of an image, audio, video, document or sticker; null otherwise. */
  mediaId: string | null;
  /** Meta's messages[] object exactly as received. */
  raw: Record<string, unknown>;
}

export interface InboundStatus {
  /** The outbound wamid the status reports on. */
  wamid: string;
  status: string;
  /** The recipient's wa_id; Meta omits it for a send to a user_id when the phone is not shared. */
  recipientId: string | null;
  /** The recipient's user_id; Meta omits it on a failed status of a send to a phone number. */
  recipientUserId: string | null;
  /** Meta's statuses[] object exactly as received. */
  raw: Record<string, unknown>;
}

export interface WebhookBatch {
  messages: InboundMessage[];
  statuses: InboundStatus[];
  /** Changes addressed to another phone number id (each counted once). */
  wrongNumber: number;
  /** Changes that are not the messages field (template status, account updates, ...). */
  ignored: number;
}

export type SendRequest =
  | { type: "text"; text: string }
  | { type: "reaction"; messageId: string; emoji: string }
  | { type: "template"; name: string; language: string; params: string[] };

export interface GraphClient {
  base: string;
  version: string;
  phoneNumberId: string;
  accessToken: string;
  fetch: typeof fetch;
  timeoutMs?: number;
}

/** A Graph call that did not succeed: status 0 when no HTTP answer came back. */
export class GraphError extends Error {
  readonly status: number;
  readonly code: number | null;
  constructor(status: number, code: number | null) {
    super(`graph ${status}${code === null ? "" : ` code ${code}`}`);
    this.name = "GraphError";
    this.status = status;
    this.code = code;
  }
}

const encoder = new TextEncoder();

function toHex(bytes: Uint8Array): string {
  let out = "";
  for (const b of bytes) out += b.toString(16).padStart(2, "0");
  return out;
}

function fromHex(hex: string): Uint8Array | null {
  if (hex.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(hex)) return null;
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = Number.parseInt(hex.slice(2 * i, 2 * i + 2), 16);
  return out;
}

/** Equality in time that depends only on the length, which is never secret here. */
export function timingSafeEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= (a[i] ?? 0) ^ (b[i] ?? 0);
  return diff === 0;
}

async function sha256(data: Uint8Array): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", data));
}

export async function sha256Hex(text: string): Promise<string> {
  return toHex(await sha256(encoder.encode(text)));
}

/**
 * A presented secret against the configured one, compared as SHA-256 digests in constant time, so
 * neither the length nor a prefix of the secret leaks. An unset secret never matches.
 */
export async function sameSecret(given: string, expected: string): Promise<boolean> {
  if (!expected || !given) return false;
  const [a, b] = await Promise.all([sha256(encoder.encode(given)), sha256(encoder.encode(expected))]);
  return timingSafeEqual(a, b);
}

async function hmac(raw: Uint8Array, secret: string): Promise<Uint8Array> {
  const key = await crypto.subtle.importKey("raw", encoder.encode(secret), { name: "HMAC", hash: "SHA-256" }, false, [
    "sign",
  ]);
  return new Uint8Array(await crypto.subtle.sign("HMAC", key, raw));
}

/** The X-Hub-Signature-256 value Meta sends for these bytes: "sha256=" + hex HMAC-SHA256. */
export async function signatureHeader(raw: Uint8Array, secret: string): Promise<string> {
  return `sha256=${toHex(await hmac(raw, secret))}`;
}

/** Verifies Meta's signature over the RAW request bytes (never a re-serialized parse). */
export async function verifySignature(raw: Uint8Array, header: string | null, secret: string): Promise<boolean> {
  if (!secret || !header) return false;
  const m = /^sha256=([0-9a-fA-F]{64})$/.exec(header.trim());
  const given = m?.[1] ? fromHex(m[1]) : null;
  if (!given) return false;
  return timingSafeEqual(await hmac(raw, secret), given);
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);
const arr = (v: unknown): unknown[] => (Array.isArray(v) ? v : []);
const str = (v: unknown): string | null => (typeof v === "string" && v.length > 0 ? v : null);

const MEDIA_TYPES = ["image", "audio", "video", "document", "sticker"] as const;

function epochSeconds(v: unknown): number | null {
  const s = typeof v === "number" ? String(v) : typeof v === "string" ? v : "";
  if (!/^\d{1,12}$/.test(s)) return null;
  return Number(s);
}

function idChange(m: Obj): IdChange | null {
  if (m.type !== "system" || !isObj(m.system)) return null;
  const type = m.system.type;
  if (type !== "user_changed_number" && type !== "user_changed_user_id") return null;
  return { type, userId: str(m.system.user_id), previousUserId: str(m.system.previous_user_id), waId: str(m.system.wa_id) };
}

/**
 * Reads a webhook payload into messages and statuses for the one configured business number.
 * Anything malformed is skipped, never thrown: a signed payload that is not what we expect is
 * counted, and Meta is still answered.
 *
 * A message names its sender by `from` (the wa_id) and `from_user_id` (the user_id). Either may be
 * absent; the contacts entry for the same user (matched by either id) supplies the other, the
 * profile name and the username. A message naming its sender by neither, and a number change
 * naming no previous user id, is skipped.
 */
export function parseWebhook(payload: unknown, phoneNumberId: string): WebhookBatch {
  const batch: WebhookBatch = { messages: [], statuses: [], wrongNumber: 0, ignored: 0 };
  if (!isObj(payload) || payload.object !== "whatsapp_business_account") return batch;
  for (const entry of arr(payload.entry)) {
    if (!isObj(entry)) continue;
    for (const change of arr(entry.changes)) {
      if (!isObj(change)) continue;
      if (change.field !== "messages" || !isObj(change.value)) {
        batch.ignored += 1;
        continue;
      }
      const value = change.value;
      const metadata = isObj(value.metadata) ? value.metadata : {};
      if (metadata.phone_number_id !== phoneNumberId) {
        batch.wrongNumber += 1;
        continue;
      }
      const people: Array<{ wa: string | null; user: string | null; name: string | null; username: string | null }> = [];
      for (const c of arr(value.contacts)) {
        if (!isObj(c)) continue;
        const profile = isObj(c.profile) ? c.profile : {};
        people.push({ wa: str(c.wa_id), user: str(c.user_id), name: str(profile.name), username: str(profile.username) });
      }
      for (const m of arr(value.messages)) {
        if (!isObj(m)) continue;
        const wamid = str(m.id);
        const type = str(m.type) ?? "unknown";
        const change = idChange(m);
        const ownFrom = str(m.from);
        const ownUser = str(m.from_user_id);
        const contact =
          people.find((p) => ownUser !== null && p.user === ownUser) ?? people.find((p) => ownFrom !== null && p.wa === ownFrom) ?? null;
        const from = ownFrom ?? contact?.wa ?? null;
        const fromUserId = ownUser ?? contact?.user ?? null;
        if (!wamid || (!from && !fromUserId && !change?.previousUserId)) continue;
        let mediaId: string | null = null;
        for (const t of MEDIA_TYPES) {
          const media = m[t];
          if (type === t && isObj(media)) mediaId = str(media.id);
        }
        const text = type === "text" && isObj(m.text) ? (typeof m.text.body === "string" ? m.text.body : null) : null;
        batch.messages.push({
          wamid,
          from,
          fromUserId,
          timestamp: epochSeconds(m.timestamp),
          type,
          profileName: contact?.name ?? null,
          username: contact?.username ?? null,
          change,
          text,
          mediaId,
          raw: m,
        });
      }
      for (const s of arr(value.statuses)) {
        if (!isObj(s)) continue;
        const wamid = str(s.id);
        const status = str(s.status);
        if (!wamid || !status) continue;
        batch.statuses.push({ wamid, status, recipientId: str(s.recipient_id), recipientUserId: str(s.recipient_user_id), raw: s });
      }
    }
  }
  return batch;
}

/**
 * Where a send names its recipient: a phone number (digits only) in `to`, any other id (a user_id)
 * in `recipient` with `to` omitted, exactly as given. Meta: "set `recipient` to the BSUID ... omit
 * the `to` property"; when both are present, `to` wins.
 */
function recipientOf(id: string): { to: string } | { recipient: string } {
  return /^\d+$/.test(id) ? { to: id } : { recipient: id };
}

/** The Graph body for a send (POST /<version>/<phone number id>/messages) to a wa_id or a user_id. */
export function sendBody(to: string, req: SendRequest): Record<string, unknown> {
  const head = { messaging_product: "whatsapp", recipient_type: "individual", ...recipientOf(to) };
  switch (req.type) {
    case "text":
      return { ...head, type: "text", text: { body: req.text, preview_url: false } };
    case "reaction":
      return { ...head, type: "reaction", reaction: { message_id: req.messageId, emoji: req.emoji } };
    case "template":
      return {
        ...head,
        type: "template",
        template: {
          name: req.name,
          language: { code: req.language },
          components: req.params.length
            ? [{ type: "body", parameters: req.params.map((text) => ({ type: "text", text })) }]
            : [],
        },
      };
  }
}

/** Mark-as-read with the typing indicator (dismissed on the reply or after 25 seconds). */
export function readBody(messageId: string): Record<string, unknown> {
  return { messaging_product: "whatsapp", status: "read", message_id: messageId, typing_indicator: { type: "text" } };
}

async function graphCall(g: GraphClient, url: string, init: RequestInit): Promise<Response> {
  const headers = new Headers(init.headers);
  headers.set("authorization", `Bearer ${g.accessToken}`);
  try {
    return await g.fetch(url, { ...init, headers, signal: AbortSignal.timeout(g.timeoutMs ?? GRAPH_TIMEOUT_MS) });
  } catch {
    throw new GraphError(0, null);
  }
}

async function graphFailure(res: Response): Promise<GraphError> {
  let code: number | null = null;
  try {
    const body = (await res.json()) as { error?: { code?: unknown } };
    if (typeof body.error?.code === "number") code = body.error.code;
  } catch {
    // An answer that is not JSON carries no code.
  }
  return new GraphError(res.status, code);
}

async function postMessages(g: GraphClient, body: Record<string, unknown>): Promise<Response> {
  const res = await graphCall(g, `${g.base}/${g.version}/${g.phoneNumberId}/messages`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!res.ok) throw await graphFailure(res);
  return res;
}

/** Sends one message; returns Meta's wamid for it. */
export async function graphSend(g: GraphClient, body: Record<string, unknown>): Promise<string> {
  const res = await postMessages(g, body);
  let wamid: string | null = null;
  try {
    const answer = (await res.json()) as { messages?: Array<{ id?: unknown }> };
    wamid = str(answer.messages?.[0]?.id);
  } catch {
    wamid = null;
  }
  if (!wamid) throw new GraphError(res.status, null);
  return wamid;
}

export async function graphMarkRead(g: GraphClient, messageId: string): Promise<void> {
  const res = await postMessages(g, readBody(messageId));
  await res.body?.cancel();
}

/**
 * Mints a media URL for the id (valid 5 minutes) and fetches the bytes with the token. The answer
 * is the download response with Meta's mime type. Only an https URL is fetched: the token never
 * goes anywhere else.
 */
export async function graphMedia(g: GraphClient, mediaId: string): Promise<Response> {
  const mint = await graphCall(
    g,
    `${g.base}/${g.version}/${encodeURIComponent(mediaId)}?phone_number_id=${encodeURIComponent(g.phoneNumberId)}`,
    { method: "GET" },
  );
  if (!mint.ok) throw await graphFailure(mint);
  let url: URL | null = null;
  let mime: string | null = null;
  try {
    const answer = (await mint.json()) as { url?: unknown; mime_type?: unknown };
    url = typeof answer.url === "string" ? new URL(answer.url) : null;
    mime = str(answer.mime_type);
  } catch {
    url = null;
  }
  if (!url || url.protocol !== "https:") throw new GraphError(502, null);
  const bytes = await graphCall(g, url.href, { method: "GET" });
  if (!bytes.ok) {
    await bytes.body?.cancel();
    throw new GraphError(bytes.status, null);
  }
  return new Response(bytes.body, {
    status: 200,
    headers: {
      "content-type": mime ?? bytes.headers.get("content-type") ?? "application/octet-stream",
      "cache-control": "no-store",
    },
  });
}
