/**
 * The gateway: the one place that holds the Dynasty number's Meta keys. It receives Meta's webhook,
 * verifies the signature, routes each owner's message to the city its phone is bound to, holds each
 * delivery until that city acknowledges it by id, and sends each city's replies with the 24-hour
 * window enforced. The contract (v1) is README.md's "The contract"; the bead spec is the record.
 *
 * Durability: the Worker keeps nothing between requests. Meta is answered 200 only after every D1
 * write for the payload has committed in one batch (a failure answers 500 and Meta redelivers), and
 * the UNIQUE (wamid, status) index makes any redelivery one row, of the city that first received
 * it. Routing is decided INSIDE that transaction (INSERT ... SELECT ... FROM phones), never from a
 * read taken before it, so a bind that lands between the read and the write cannot misroute a
 * message.
 *
 * Owners: Meta names a WhatsApp user by a wa_id (a phone number), a user_id (a business-scoped
 * user id), or both. An owner row is keyed by its conversation_id, set once at the bind and never
 * changed; its wa_id and user_id are the ids Meta names it by now (README.md, "Business-scoped user
 * ids").
 *
 * Logs: fixed event names and counts only. Never a body, a phone number, a user id, a username, a
 * profile name, a code, a token or a secret, and never an error's message (a parse error can quote
 * its input).
 */
import {
  BSUID,
  GraphError,
  graphMarkRead,
  graphMedia,
  graphSend,
  type GraphClient,
  type InboundMessage,
  type InboundStatus,
  MAX_TEXT_CHARS,
  parseWebhook,
  sameSecret,
  type SendRequest,
  sendBody,
  sha256Hex,
  verifySignature,
} from "./meta.js";
import { movesWindow, windowClosesAt, windowOpen } from "./window.js";

const DAY = 86_400;

/** Meta's bodies are stored whole; D1 holds a value up to 2 MB. */
export const MAX_BODY_BYTES = 1_900_000;
/** Held deliveries per city before Meta is told to retry later (503). */
export const MAX_PENDING = 5000;
/** How long a delivery's body is held without an ack. */
export const HOLD_SECONDS = 7 * DAY;
/** How long a body-less row stays to dedup a late Meta redelivery (Meta retries up to 7 days). */
export const TOMBSTONE_SECONDS = 8 * DAY;
/**
 * How long an owner's ids from before a number change stay its aliases, from the change Meta
 * stamped: no message an alias may route (one Meta stamped at or before the change) can pass the
 * stale filter later than this.
 */
export const ALIAS_SECONDS = TOMBSTONE_SECONDS;
/** How long a counted wrong code's wamid is kept: as long as Meta may redeliver it. */
const CODE_WAMIDS_KEEP_SECONDS = TOMBSTONE_SECONDS;
/** Meta keeps a webhook media id for 7 days. */
export const MEDIA_SECONDS = 7 * DAY;
export const INVITE_SECONDS = DAY;
export const ENROLL_SECONDS = 3 * DAY;
/** An unknown sender hears the canned line at most once per this. */
export const CANNED_EVERY_SECONDS = 7 * DAY;
/** Invite-code texts a stranger may send per day; the sixth is ignored, a right code included. */
export const MAX_CODE_ATTEMPTS = 5;
/**
 * A send claim with no wamid this old reads as outcome unknown, never in flight: the Worker that
 * held it died somewhere between the claim and the record, maybe after Meta took the message.
 */
export const SEND_CLAIM_STALE_SECONDS = 60;
const SENDS_KEEP_SECONDS = 30 * DAY;
const SPENT_CODES_KEEP_SECONDS = 7 * DAY;
const ENROLL_CODES_KEEP_SECONDS = 30 * DAY;
/** A status no send claimed within this is the gateway's own (the welcome, the canned line) or lost. */
const ORPHAN_SECONDS = DAY;
/** The gateway's own texts (the welcome, the canned line) must not hold Meta's answer long. */
const TELL_TIMEOUT_MS = 8000;
/**
 * Statements per D1 batch. A payload that needs more is written in several batches, in order,
 * and a batch boundary never falls inside one message's statements (its bind, its code, its
 * joined row and its delivery commit together). A failure after an early batch committed answers
 * 500; Meta's redelivery finds those messages' rows already there, each one row.
 */
const MAX_BATCH_STATEMENTS = 400;
const MAX_JSON_BYTES = 256 * 1024;
const MAX_ACK_IDS = 500;
const META_OUTSIDE_WINDOW = 131047;

export type LogEvent = Record<string, string | number | boolean | null>;

export interface GatewayConfig {
  metaAccessToken: string;
  metaAppSecret: string;
  metaVerifyToken: string;
  metaPhoneNumberId: string;
  adminToken: string;
  graphVersion: string;
  displayName: string;
  graphBase?: string;
}

export interface Deps {
  db: D1Database;
  config: GatewayConfig;
  /** Epoch seconds. */
  now(): number;
  fetch: typeof fetch;
  log(event: LogEvent): void;
}

class HttpError extends Error {
  readonly status: number;
  readonly code: string;
  readonly extra: Record<string, unknown>;
  constructor(status: number, code: string, extra: Record<string, unknown> = {}) {
    super(code);
    this.status = status;
    this.code = code;
    this.extra = extra;
  }
}

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);
const json = (data: unknown, status = 200) =>
  Response.json(data, { status, headers: { "cache-control": "no-store" } });
const fail = (status: number, error: string, extra: Record<string, unknown> = {}) => json({ error, ...extra }, status);
const errorName = (e: unknown) => (e instanceof Error ? e.name : typeof e);
const changes = (r: D1Result | undefined) => r?.meta.changes ?? 0;
const decoder = new TextDecoder();

const CROCKFORD = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";
const ENROLL_CODE = /^[0-9A-HJKMNP-TV-Z]{16}$/;
const INVITE_TEXT = /^\s*(\d{6})\s*$/;

function randomBytes(n: number): Uint8Array {
  return crypto.getRandomValues(new Uint8Array(n));
}

function base64url(bytes: Uint8Array): string {
  return btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

/** 16 Crockford base32 characters (80 bits), stored bare, shown as XXXX-XXXX-XXXX-XXXX. */
function newEnrollCode(): string {
  return [...randomBytes(16)].map((b) => CROCKFORD[b & 31]).join("");
}

function showEnrollCode(code: string): string {
  return code.match(/.{4}/g)?.join("-") ?? code;
}

/** A uniform 6-digit code (rejection sampling, no modulo bias). */
function newInviteCode(): string {
  const limit = Math.floor(0x1_0000_0000 / 1_000_000) * 1_000_000;
  for (;;) {
    const n = crypto.getRandomValues(new Uint32Array(1))[0] ?? limit;
    if (n < limit) return String(n % 1_000_000).padStart(6, "0");
  }
}

function inviteCodeIn(m: InboundMessage): string | null {
  if (m.type !== "text" || m.text === null) return null;
  return INVITE_TEXT.exec(m.text)?.[1] ?? null;
}

function cleanName(v: unknown): string | null {
  if (typeof v !== "string") return null;
  const s = v.trim();
  // biome-ignore lint/suspicious/noControlCharactersInRegex: a name carries no control characters
  return s.length >= 1 && s.length <= 100 && !/[\u0000-\u001f\u007f]/.test(s) ? s : null;
}

function boundedString(v: unknown, min: number, max: number): string | null {
  return typeof v === "string" && v.length >= min && v.length <= max ? v : null;
}

function bearer(request: Request): string | null {
  return /^Bearer +(\S+)\s*$/i.exec(request.headers.get("authorization") ?? "")?.[1] ?? null;
}

function intParam(url: URL, name: string, fallback: number, min: number, max: number): number {
  const raw = url.searchParams.get(name);
  if (raw === null) return fallback;
  if (!/^\d{1,16}$/.test(raw)) throw new HttpError(400, "bad_request");
  const n = Number(raw);
  if (!Number.isSafeInteger(n) || n < min || n > max) throw new HttpError(400, "bad_request");
  return n;
}

/**
 * The request body, read as a stream and abandoned the moment it passes max bytes (null), so an
 * unauthenticated sender cannot make the isolate buffer more than the cap, declared length or not.
 */
async function readCapped(request: Request, max: number): Promise<Uint8Array | null> {
  if (Number(request.headers.get("content-length") ?? "0") > max) return null;
  if (!request.body) return new Uint8Array(0);
  const reader = request.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    total += value.byteLength;
    if (total > max) {
      await reader.cancel();
      return null;
    }
    chunks.push(value);
  }
  const out = new Uint8Array(total);
  let offset = 0;
  for (const c of chunks) {
    out.set(c, offset);
    offset += c.byteLength;
  }
  return out;
}

async function readJson(request: Request): Promise<Obj> {
  const buf = await readCapped(request, MAX_JSON_BYTES);
  if (!buf) throw new HttpError(413, "too_large");
  let v: unknown;
  try {
    v = JSON.parse(decoder.decode(buf));
  } catch {
    throw new HttpError(400, "bad_request");
  }
  if (!isObj(v)) throw new HttpError(400, "bad_request");
  return v;
}

/** A send's `to`: a user_id exactly as given, or a phone number's digits (a leading + dropped). */
function parseTo(v: unknown): string {
  if (typeof v !== "string") throw new HttpError(400, "bad_request");
  if (BSUID.test(v)) return v;
  const s = v.replace(/^\+/, "");
  if (!/^\d{6,20}$/.test(s)) throw new HttpError(400, "bad_request");
  return s;
}

/**
 * The ids a message names its sender by. A number-change notice carries no from_user_id: the
 * previous user_id is the one the gateway knows the owner by.
 */
function senderOf(m: InboundMessage): { wa: string | null; user: string | null } {
  return { wa: m.from, user: m.fromUserId ?? m.change?.previousUserId ?? null };
}

/**
 * The phones rows a sender's ids (SQL parameters wa and user) name: the row holding the user_id,
 * or the row holding the wa_id unless that row's user_id is another user's. A phone number Meta
 * reports beside a different user_id is someone else's now (a recycled number), never the owner.
 */
const ownerMatch = (wa: string, user: string) => `(user_id = ${user} OR (wa_id = ${wa} AND (user_id IS NULL OR ${user} IS NULL)))`;
/** The one owner of a match: the user_id's row before the wa_id's (they differ only for two rows). */
const ownerFirst = (user: string) => `ORDER BY CASE WHEN user_id = ${user} THEN 0 ELSE 1 END LIMIT 1`;
const ownerKey = (wa: string, user: string) =>
  `(SELECT conversation_id FROM phones WHERE ${ownerMatch(wa, user)} ${ownerFirst(user)})`;
/**
 * The owner that held an id (SQL parameters wa, user) when Meta stamped a message (parameter at),
 * by the aliases a number change keeps: Meta's retry of what the owner sent under its old ids. A
 * user_id is matched alone (Meta gives one to no one else); a wa_id only when the message names no
 * user_id, the rule ownerMatch applies. The alias picked is the first change at or after the stamp,
 * the binding that held the id then, and it names nobody once that binding is gone (an operator's
 * unbind), whoever binds under the same key later. No expiry test is needed: a message stamped at
 * or before changed_at that passes the stale filter arrives before changed_at + ALIAS_SECONDS.
 */
const aliasKey = (wa: string, user: string, at: string) =>
  `(SELECT p.conversation_id FROM phones p JOIN
      (SELECT conversation_id, bound_wamid FROM phone_aliases
       WHERE (id = ${user} OR (id = ${wa} AND ${user} IS NULL)) AND changed_at >= ${at} ORDER BY changed_at LIMIT 1) a
    ON p.conversation_id = a.conversation_id AND p.bound_wamid = a.bound_wamid)`;
/**
 * The owner a message routes to: the one an alias says held its id when Meta stamped it, else the
 * one Meta names by those ids now. The stamp decides first: a number Meta shows alone on a message
 * stamped before its owner's number change was that owner's then, whoever holds it now.
 */
const routeKey = (wa: string, user: string, at: string) => `COALESCE(${aliasKey(wa, user, at)}, ${ownerKey(wa, user)})`;
/** A sender that is no owner's: no current id and no alias names it for this message. */
const stranger = (wa: string, user: string, at: string) => `(${routeKey(wa, user, at)} IS NULL)`;

interface OwnerRow {
  conversation_id: string;
  wa_id: string | null;
  user_id: string | null;
  city_id: string;
}

/** ownerMatch and ownerFirst over rows already read (the plan's reads, never its writes). */
function ownerIn(rows: OwnerRow[], ids: { wa: string | null; user: string | null }): OwnerRow | undefined {
  return (
    rows.find((r) => ids.user !== null && r.user_id === ids.user) ??
    rows.find((r) => ids.wa !== null && r.wa_id === ids.wa && (r.user_id === null || ids.user === null))
  );
}

function parseSend(body: Obj): SendRequest {
  switch (body.type) {
    case "text": {
      const text = typeof body.text === "string" ? body.text : null;
      if (!text) throw new HttpError(400, "bad_request");
      if ([...text].length > MAX_TEXT_CHARS) throw new HttpError(400, "text_too_long");
      return { type: "text", text };
    }
    case "reaction": {
      const r = isObj(body.reaction) ? body.reaction : null;
      const messageId = boundedString(r?.message_id, 1, 256);
      const emoji = boundedString(r?.emoji, 0, 32);
      if (!messageId || emoji === null) throw new HttpError(400, "bad_request");
      return { type: "reaction", messageId, emoji };
    }
    case "template": {
      const t = isObj(body.template) ? body.template : null;
      const name = typeof t?.name === "string" && /^[a-z0-9_]{1,512}$/.test(t.name) ? t.name : null;
      const language =
        typeof t?.language === "string" && /^[A-Za-z]{2,3}(_[A-Za-z0-9]{2,4})?$/.test(t.language) ? t.language : null;
      const params = Array.isArray(t?.params) ? t.params : null;
      if (!name || !language || !params || params.length > 20) throw new HttpError(400, "bad_request");
      if (!params.every((p) => boundedString(p, 1, 1024) !== null)) throw new HttpError(400, "bad_request");
      return { type: "template", name, language, params: params as string[] };
    }
    default:
      throw new HttpError(400, "bad_request");
  }
}

interface Plan {
  statements: D1PreparedStatement[];
  /** The index where each group starts: one message's (or one status's) statements, never split. */
  groupStarts: number[];
  /** Statement indexes whose one changed row means "the text whose wamid this is bound an owner, told at id". */
  binds: Array<{ index: number; id: string; wamid: string }>;
  /** Statement indexes whose one changed row means "send this stranger the canned line now". */
  canned: Array<{ index: number; id: string }>;
  /** Statuses no send has claimed yet (read before the write): kept apart for adoption. */
  orphanedStatuses: number;
  /** Messages Meta stamped longer ago than any dedup row lives: dropped and counted. */
  stale: number;
  overCap: boolean;
}

export function createGateway(deps: Deps) {
  const { db, config: cfg } = deps;
  const log = (e: LogEvent) => deps.log(e);

  function graph(): GraphClient {
    if (!cfg.metaAccessToken || !cfg.metaPhoneNumberId) throw new HttpError(503, "not_configured");
    return {
      base: cfg.graphBase ?? "https://graph.facebook.com",
      version: cfg.graphVersion,
      phoneNumberId: cfg.metaPhoneNumberId,
      accessToken: cfg.metaAccessToken,
      fetch: deps.fetch,
    };
  }

  async function bump(name: string, by = 1): Promise<void> {
    try {
      await db
        .prepare("INSERT INTO stats (name, value) VALUES (?1, ?2) ON CONFLICT (name) DO UPDATE SET value = value + excluded.value")
        .bind(name, by)
        .run();
    } catch (e) {
      log({ event: "stats_write_failed", stat: name, error: errorName(e) });
    }
  }

  const bumpStatement = (name: string, by: number) =>
    db
      .prepare("INSERT INTO stats (name, value) VALUES (?1, ?2) ON CONFLICT (name) DO UPDATE SET value = value + excluded.value")
      .bind(name, by);

  async function cityOf(request: Request): Promise<string> {
    const token = bearer(request);
    if (!token) throw new HttpError(401, "unauthorized");
    const row = await db
      .prepare("SELECT city_id FROM credentials WHERE token_sha256 = ? AND revoked_at IS NULL")
      .bind(await sha256Hex(token))
      .first<{ city_id: string }>();
    if (!row) throw new HttpError(401, "unauthorized");
    return row.city_id;
  }

  async function requireAdmin(request: Request): Promise<void> {
    const token = bearer(request);
    if (!token || !(await sameSecret(token, cfg.adminToken))) throw new HttpError(401, "unauthorized");
  }

  // ---------------------------------------------------------------- Meta side

  async function handshake(_request: Request, url: URL): Promise<Response> {
    const mode = url.searchParams.get("hub.mode");
    const token = url.searchParams.get("hub.verify_token") ?? "";
    if (mode !== "subscribe" || !(await sameSecret(token, cfg.metaVerifyToken))) {
      log({ event: "handshake_refused" });
      return fail(403, "forbidden");
    }
    log({ event: "handshake" });
    return new Response(url.searchParams.get("hub.challenge") ?? "", {
      status: 200,
      headers: { "content-type": "text/plain; charset=utf-8", "cache-control": "no-store" },
    });
  }

  async function webhook(request: Request): Promise<Response> {
    if (!cfg.metaAppSecret || !cfg.metaPhoneNumberId) {
      log({ event: "webhook_refused", reason: "not_configured" });
      return fail(503, "not_configured");
    }
    const raw = await readCapped(request, MAX_BODY_BYTES);
    if (!raw) {
      log({ event: "webhook_refused", reason: "too_large" });
      return fail(413, "too_large");
    }
    if (!(await verifySignature(raw, request.headers.get("x-hub-signature-256"), cfg.metaAppSecret))) {
      log({ event: "webhook_refused", reason: "signature" });
      return fail(401, "bad_signature");
    }
    let payload: unknown;
    try {
      payload = JSON.parse(decoder.decode(raw));
    } catch {
      log({ event: "webhook_refused", reason: "bad_json" });
      return fail(400, "bad_request");
    }
    const batch = parseWebhook(payload, cfg.metaPhoneNumberId);
    const now = deps.now();
    const plan = await planWebhook(batch.messages, batch.statuses, now);
    if (plan.overCap) {
      log({ event: "webhook_deferred", reason: "pending_cap" });
      return fail(503, "pending_cap");
    }
    plan.groupStarts.push(plan.statements.length);
    plan.statements.push(
      db
        .prepare("INSERT INTO stats (name, value) VALUES ('last_webhook_at', ?1) ON CONFLICT (name) DO UPDATE SET value = excluded.value")
        .bind(now),
    );
    for (const [name, n] of [
      ["dropped_wrong_number", batch.wrongNumber],
      ["ignored_changes", batch.ignored],
      ["orphaned_statuses", plan.orphanedStatuses],
      ["stale_messages", plan.stale],
    ] as const) {
      if (n > 0) plan.statements.push(bumpStatement(name, n));
    }
    // Write in batches cut only at group boundaries. On a failure the batches already committed
    // stand (each message's rows are whole), their welcomes and canned lines still go below, and
    // Meta gets 500: its redelivery finds those rows and writes only the rest.
    const results: D1Result[] = [];
    let failed = false;
    for (const [from, to] of batchRanges(plan)) {
      try {
        results.push(...(await db.batch(plan.statements.slice(from, to))));
      } catch (e) {
        log({ event: "webhook_store_failed", error: errorName(e), committed_statements: results.length });
        failed = true;
        break;
      }
    }
    const committed = (index: number) => index < results.length && changes(results[index]) === 1;
    // Best effort from here: a failed send is counted, never a 5xx to Meta.
    let welcomed = 0;
    let canned = 0;
    for (const b of plan.binds) {
      if (!committed(b.index)) continue;
      const city = await db
        .prepare("SELECT c.name FROM phones p JOIN cities c ON c.id = p.city_id WHERE p.bound_wamid = ?")
        .bind(b.wamid)
        .first<{ name: string }>();
      if (await tell(b.id, `You're connected to ${city?.name ?? "your city"}'s mayor. Say hi.`, "welcome")) welcomed += 1;
    }
    for (const c of plan.canned) {
      if (!committed(c.index)) continue;
      if (await tell(c.id, `This number serves ${cfg.displayName} cities. Ask your city for an invite code.`, "canned")) {
        canned += 1;
      }
    }
    if (failed) return fail(500, "store_failed");
    log({
      event: "webhook",
      messages: batch.messages.length,
      statuses: batch.statuses.length,
      wrong_number: batch.wrongNumber,
      ignored: batch.ignored,
      orphaned_statuses: plan.orphanedStatuses,
      stale: plan.stale,
      bound: plan.binds.filter((b) => committed(b.index)).length,
      welcomed,
      canned,
    });
    return json({ ok: true });
  }

  /** [from, to) statement ranges of at most MAX_BATCH_STATEMENTS, cut only where a group starts. */
  function batchRanges(plan: Plan): Array<[number, number]> {
    const ranges: Array<[number, number]> = [];
    const ends = [...plan.groupStarts.slice(1), plan.statements.length];
    let from = 0;
    let to = 0;
    for (const end of ends) {
      if (end - from > MAX_BATCH_STATEMENTS && to > from) {
        ranges.push([from, to]);
        from = to;
      }
      to = end;
    }
    if (to > from) ranges.push([from, to]);
    return ranges;
  }

  /** One gateway-authored text (the welcome, the canned line) to a wa_id or a user_id. Never throws. */
  async function tell(id: string, text: string, kind: "welcome" | "canned"): Promise<boolean> {
    try {
      await graphSend({ ...graph(), timeoutMs: TELL_TIMEOUT_MS }, sendBody(id, { type: "text", text }));
      return true;
    } catch (e) {
      log({
        event: `${kind}_failed`,
        meta_status: e instanceof GraphError ? e.status : null,
        meta_code: e instanceof GraphError ? e.code : null,
      });
      await bump(`${kind}_failed`);
      return false;
    }
  }

  async function planWebhook(messages: InboundMessage[], statuses: InboundStatus[], now: number): Promise<Plan> {
    const plan: Plan = {
      statements: [],
      groupStarts: [],
      binds: [],
      canned: [],
      orphanedStatuses: 0,
      stale: 0,
      overCap: false,
    };
    // Meta retries a delivery for up to 7 days, and a body-less row dedups a wamid for 8 days from
    // its receipt (never earlier than Meta's stamp). A message stamped longer ago than that could
    // be a copy whose dedup row is gone, so it is dropped and counted, never delivered again.
    const fresh = messages.filter((m) => m.timestamp === null || m.timestamp > now - TOMBSTONE_SECONDS);
    plan.stale = messages.length - fresh.length;
    messages = fresh;
    const each = async <T>(sql: string, values: string[], ...extra: unknown[]): Promise<T[]> =>
      values.length
        ? (await db.prepare(sql).bind(JSON.stringify(values), ...extra).all<T>()).results
        : [];

    // Reads taken before the write, for the 5,000 cap and the stranger paths only. The write
    // itself routes from the tables as they stand inside its own transaction.
    const ids = messages.map(senderOf);
    const waIds = [...new Set(ids.map((s) => s.wa).filter((v): v is string => v !== null))];
    const userIds = [...new Set(ids.map((s) => s.user).filter((v): v is string => v !== null))];
    const rows =
      waIds.length || userIds.length
        ? (
            await db
              .prepare(
                `SELECT conversation_id, wa_id, user_id, city_id FROM phones
                 WHERE wa_id IN (SELECT value FROM json_each(?1)) OR user_id IN (SELECT value FROM json_each(?2))`,
              )
              .bind(JSON.stringify(waIds), JSON.stringify(userIds))
              .all<OwnerRow>()
          ).results
        : [];
    const owners = ids.map((s) => ownerIn(rows, s));
    const codes = [
      ...new Set(messages.filter((_, i) => !owners[i]).map(inviteCodeIn).filter((c): c is string => c !== null)),
    ];
    const codeCities = await each<{ city_id: string }>(
      "SELECT city_id FROM invite_codes WHERE code IN (SELECT value FROM json_each(?1)) AND used_at IS NULL",
      codes,
    );
    const statusCities = new Map(
      (
        await each<{ wamid: string; city_id: string }>(
          "SELECT wamid, city_id FROM sends WHERE wamid IN (SELECT value FROM json_each(?1))",
          [...new Set(statuses.map((s) => s.wamid))],
        )
      ).map((r) => [r.wamid, r.city_id]),
    );
    const cities = [
      ...new Set([
        ...owners.flatMap((o) => (o ? [o.city_id] : [])),
        ...codeCities.map((r) => r.city_id),
        ...statusCities.values(),
      ]),
    ];
    const held = await each<{ n: number }>(
      "SELECT COUNT(*) AS n FROM deliveries WHERE body IS NOT NULL AND city_id IN (SELECT value FROM json_each(?1)) GROUP BY city_id",
      cities,
    );
    if (held.some((r) => r.n >= MAX_PENDING)) {
      plan.overCap = true;
      return plan;
    }

    const push = (s: D1PreparedStatement) => plan.statements.push(s) - 1;
    for (const [i, m] of messages.entries()) {
      const { wa, user } = ids[i] ?? { wa: null, user: null };
      // The gateway's own texts go to the wa_id when Meta shares one, else the user_id. A stranger
      // is counted under its user_id when Meta sends one, which stays the same when Meta starts
      // sharing its phone.
      const address = wa ?? user;
      const strangerKey = user ?? wa;
      if (address === null || strangerKey === null) continue; // parseWebhook never yields one
      plan.groupStarts.push(plan.statements.length);
      const at = Math.min(m.timestamp ?? now, now);
      const body = JSON.stringify(m.raw);
      const owned = owners[i] !== undefined;
      const code = owned ? null : inviteCodeIn(m);
      if (wa !== null && user !== null) {
        // Meta names this phone number beside this user_id: a row holding the number beside
        // another user_id holds a number that is someone else's now (a recycled number), and loses
        // it, so its city's sends never reach the new holder. Setting NULL never hits a UNIQUE. A
        // user_id an alias holds is a previous one: the pairing is history (Meta's retry of a
        // message sent before a number change), never the number's holder today.
        push(
          db
            .prepare(
              `UPDATE phones SET wa_id = NULL WHERE wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2
                 AND NOT EXISTS (SELECT 1 FROM phone_aliases WHERE id = ?2)`,
            )
            .bind(wa, user),
        );
      }
      if (code !== null) {
        // A stranger's code attempt: counted per day and once per wamid (a redelivery of any text
        // already counted is not a second attempt), then the bind, conditional on a code that was
        // live when Meta stamped the text and on the attempt count, never on this read. The wamid
        // is recorded after the count, so the count reads it unrecorded the first time only.
        push(
          db
            .prepare(
              `INSERT INTO unknown_senders (sender_key, code_attempts, attempts_since)
               SELECT ?1, 1, ?2 WHERE ${stranger("?5", "?6", "?7")}
               ON CONFLICT (sender_key) DO UPDATE SET
                 code_attempts = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN code_attempts
                                      WHEN attempts_since <= ?3 THEN 1 ELSE code_attempts + 1 END,
                 attempts_since = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN attempts_since
                                       WHEN attempts_since <= ?3 THEN ?2 ELSE attempts_since END`,
            )
            .bind(strangerKey, now, now - DAY, m.wamid, wa, user, at),
        );
        push(db.prepare("INSERT OR IGNORE INTO code_attempt_wamids (wamid, counted_at) VALUES (?1, ?2)").bind(m.wamid, now));
        // A new owner row holds every id the text named; no id may already name another owner,
        // now or by an alias (the owner's own text from before a number change is its message).
        // Its conversation_id is the wa_id, unless that number is already another owner's
        // conversation_id (a recycled number), else the user_id. The rows below find it by the
        // wamid that bound it.
        const bindIndex = push(
          db
            .prepare(
              `INSERT INTO phones (conversation_id, wa_id, user_id, username, city_id, profile_name, bound_at, bound_wamid, last_inbound_at)
               SELECT k.id, ?1, ?2, ?3, i.city_id, ?4, ?5, ?6, ?7
               FROM invite_codes i,
                    (SELECT CASE WHEN ?1 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE conversation_id = ?1)
                                 THEN ?1 ELSE ?2 END AS id) k
               WHERE i.code = ?8 AND i.used_at IS NULL AND i.expires_at > ?7 AND k.id IS NOT NULL
                 AND COALESCE((SELECT code_attempts FROM unknown_senders WHERE sender_key = ?9), 0) <= ?10
                 AND NOT EXISTS (SELECT 1 FROM phones p WHERE p.conversation_id = k.id OR p.wa_id = ?1 OR p.user_id = ?2)
                 AND ${aliasKey("?1", "?2", "?7")} IS NULL
               ON CONFLICT DO NOTHING`,
            )
            .bind(wa, user, m.username, m.profileName, now, m.wamid, at, code, strangerKey, MAX_CODE_ATTEMPTS),
        );
        push(
          db
            .prepare(
              `UPDATE invite_codes SET used_at = ?1
               WHERE code = ?2 AND used_at IS NULL AND EXISTS (SELECT 1 FROM phones WHERE bound_wamid = ?3)`,
            )
            .bind(now, code, m.wamid),
        );
        push(
          db
            .prepare(
              `INSERT OR IGNORE INTO deliveries
                 (city_id, received_at, kind, wamid, status, wa_id, user_id, username, conversation_id, profile_name, body)
               SELECT city_id, ?1, 'joined', ?2, '', ?3, ?4, ?5, conversation_id, ?6, ?7 FROM phones WHERE bound_wamid = ?2`,
            )
            .bind(now, m.wamid, wa, user, m.username, m.profileName, body),
        );
        plan.binds.push({ index: bindIndex, id: address, wamid: m.wamid });
      }
      // The message itself, routed by the owner's ids as they stand inside this transaction, or
      // by an alias for Meta's retry of what the owner sent before a number change. After a bind
      // by this very text its wamid is already the joined row's, so this is one row. A wamid any
      // city already holds is that city's: a retry after the phone moved cities is no new row. An
      // id the message omits is the owner row's (a known phone stays on the delivery when Meta
      // names the owner by user_id alone).
      push(
        db
          .prepare(
            `INSERT OR IGNORE INTO deliveries
               (city_id, received_at, kind, wamid, status, wa_id, user_id, username, conversation_id, profile_name, body)
             SELECT city_id, ?1, 'message', ?2, '', COALESCE(?3, wa_id), COALESCE(?4, user_id), ?5, conversation_id, ?6, ?7
             FROM phones WHERE conversation_id = ${routeKey("?3", "?4", "?8")}`,
          )
          .bind(now, m.wamid, wa, user, m.username, m.profileName, body, at),
      );
      if (m.mediaId) {
        // The media goes to the city that holds the message's wamid, from when it first did: never
        // to a city that holds the phone only since (a retry after the phone moved cities).
        push(
          db
            .prepare(
              `INSERT OR IGNORE INTO media (city_id, media_id, received_at)
               SELECT city_id, ?1, received_at FROM deliveries WHERE wamid = ?2 AND status = ''`,
            )
            .bind(m.mediaId, m.wamid),
        );
      }
      if (movesWindow(m.type)) {
        // A message routed by an alias moves the owner's window like any of its messages; it
        // changes none of the owner's ids or names, which are newer than the message.
        push(
          db
            .prepare(
              `UPDATE phones SET last_inbound_at = MAX(COALESCE(last_inbound_at, 0), ?1)
               WHERE conversation_id = ${aliasKey("?2", "?3", "?1")}`,
            )
            .bind(at, wa, user),
        );
      }
      // The owner's window moves on a message from either id. An id the owner row lacks is filled
      // the first time a message carries it beside the other (one owner, two ids, one row), unless
      // another row already holds it. A message an alias routes is another owner's (above), so it
      // touches no row here.
      push(
        db
          .prepare(
            `UPDATE phones SET
               profile_name = COALESCE(?1, profile_name),
               username = COALESCE(?2, username),
               last_inbound_at = CASE WHEN ?3 THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,
               wa_id = CASE WHEN wa_id IS NULL AND ?5 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.wa_id = ?5)
                            THEN ?5 ELSE wa_id END,
               user_id = CASE WHEN user_id IS NULL AND ?6 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = ?6)
                              THEN ?6 ELSE user_id END
             WHERE conversation_id = ${ownerKey("?5", "?6")} AND ${aliasKey("?5", "?6", "?4")} IS NULL`,
          )
          .bind(m.profileName, m.username, movesWindow(m.type) ? 1 : 0, at, wa, user),
      );
      if (m.change?.userId) {
        // A number change: the owner's ids become the new ones (the old phone number is no longer
        // the owner's, so no new number shared means none held), its conversation_id stays. A row
        // holding the new number beside another user_id loses it first, as above. An id another
        // row still holds is never taken from it.
        // First, each id the change replaces becomes an alias of the owner's binding, for Meta's
        // retries of what the owner sent before it; only when the re-key below takes place (no
        // other row holds the new user_id).
        push(
          db
            .prepare(
              `INSERT OR IGNORE INTO phone_aliases (id, conversation_id, bound_wamid, changed_at, expires_at)
               SELECT c.id, c.conversation_id, c.bound_wamid, ?5, ?6 FROM (
                 SELECT user_id AS id, conversation_id, bound_wamid FROM phones
                 WHERE conversation_id = ${ownerKey("?3", "?4")} AND user_id IS NOT ?1
                 UNION ALL
                 SELECT wa_id AS id, conversation_id, bound_wamid FROM phones
                 WHERE conversation_id = ${ownerKey("?3", "?4")} AND wa_id IS NOT ?2
               ) c
               WHERE c.id IS NOT NULL
                 AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = ?1 AND o.conversation_id <> c.conversation_id)`,
            )
            .bind(m.change.userId, m.change.waId, wa, user, at, at + ALIAS_SECONDS),
        );
        if (m.change.waId) {
          push(
            db
              .prepare("UPDATE phones SET wa_id = NULL WHERE wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2")
              .bind(m.change.waId, m.change.userId),
          );
        }
        push(
          db
            .prepare(
              `UPDATE phones SET
                 user_id = ?1,
                 wa_id = CASE WHEN ?2 IS NOT NULL AND NOT EXISTS
                                (SELECT 1 FROM phones o WHERE o.wa_id = ?2 AND o.conversation_id <> phones.conversation_id)
                              THEN ?2 ELSE NULL END
               WHERE conversation_id = ${ownerKey("?3", "?4")}
                 AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = ?1 AND o.conversation_id <> phones.conversation_id)`,
            )
            .bind(m.change.userId, m.change.waId, wa, user),
        );
      }
      if (!owned && !m.change) {
        // A stranger hears the canned line at most once per 7 days, claimed before it is sent. A
        // number change is Meta's notice, not the user writing: never answered.
        const cannedIndex = push(
          db
            .prepare(
              `INSERT INTO unknown_senders (sender_key, last_replied_at)
               SELECT ?1, ?2 WHERE ${stranger("?4", "?5", "?6")}
               ON CONFLICT (sender_key) DO UPDATE SET last_replied_at = excluded.last_replied_at
               WHERE unknown_senders.last_replied_at IS NULL OR unknown_senders.last_replied_at <= ?3`,
            )
            .bind(strangerKey, now, now - CANNED_EVERY_SECONDS, wa, user, at),
        );
        plan.canned.push({ index: cannedIndex, id: address });
      }
    }
    for (const s of statuses) {
      plan.groupStarts.push(plan.statements.length);
      if (!statusCities.has(s.wamid)) plan.orphanedStatuses += 1;
      // Routed to the city whose send recorded this wamid; when no send has recorded it yet (Meta's
      // "sent" can beat the send's own write), kept apart for that send to adopt. Exactly one of the
      // two applies, decided inside the transaction.
      // The delivery's conversation_id is the owner the send went to (a v1 send recorded none and
      // went to the wa_id that is its owner's key).
      push(
        db
          .prepare(
            `INSERT OR IGNORE INTO deliveries
               (city_id, received_at, kind, wamid, status, wa_id, user_id, username, conversation_id, profile_name, body)
             SELECT city_id, ?1, 'status', ?2, ?3, ?4, ?5, ?7, COALESCE(conversation_id, ?4, ?5), NULL, ?6
             FROM sends WHERE wamid = ?2 LIMIT 1`,
          )
          .bind(now, s.wamid, s.status, s.recipientId, s.recipientUserId, JSON.stringify(s.raw), s.username),
      );
      push(
        db
          .prepare(
            `INSERT OR IGNORE INTO orphan_statuses (wamid, status, received_at, recipient_id, recipient_user_id, username, body)
             SELECT ?2, ?3, ?1, ?4, ?5, ?7, ?6 WHERE NOT EXISTS (SELECT 1 FROM sends WHERE wamid = ?2)`,
          )
          .bind(now, s.wamid, s.status, s.recipientId, s.recipientUserId, JSON.stringify(s.raw), s.username),
      );
    }
    return plan;
  }

  // ---------------------------------------------------------------- City side

  async function enroll(request: Request): Promise<Response> {
    const body = await readJson(request);
    const name = cleanName(body.name);
    if (typeof body.code !== "string" || !name) throw new HttpError(400, "bad_request");
    const code = body.code.toUpperCase().replace(/[\s-]/g, "");
    if (!ENROLL_CODE.test(code)) throw new HttpError(403, "forbidden");
    const now = deps.now();
    const cityId = `city_${[...randomBytes(12)].map((b) => b.toString(16).padStart(2, "0")).join("")}`;
    const credential = `wgc_${base64url(randomBytes(32))}`;
    const results = await db.batch([
      db
        .prepare(
          `INSERT INTO cities (id, name, box_name, created_at)
           SELECT ?1, city_name, ?2, ?3 FROM enroll_codes WHERE code = ?4 AND used_at IS NULL AND expires_at > ?3`,
        )
        .bind(cityId, name, now, code),
      db
        .prepare(
          `UPDATE enroll_codes SET used_at = ?1, city_id = ?2
           WHERE code = ?3 AND used_at IS NULL AND EXISTS (SELECT 1 FROM cities WHERE id = ?2)`,
        )
        .bind(now, cityId, code),
      db
        .prepare("INSERT INTO credentials (token_sha256, city_id, created_at) SELECT ?1, id, ?2 FROM cities WHERE id = ?3")
        .bind(await sha256Hex(credential), now, cityId),
    ]);
    if (changes(results[0]) !== 1) {
      log({ event: "enroll_refused" });
      throw new HttpError(403, "forbidden");
    }
    log({ event: "city_enrolled" });
    return json({ city_id: cityId, credential });
  }

  async function invites(request: Request): Promise<Response> {
    const cityId = await cityOf(request);
    for (let attempt = 0; attempt < 8; attempt++) {
      const now = deps.now();
      const code = newInviteCode();
      const results = await db.batch([
        db.prepare("DELETE FROM invite_codes WHERE city_id = ? AND used_at IS NULL").bind(cityId),
        db
          .prepare("INSERT OR IGNORE INTO invite_codes (code, city_id, created_at, expires_at) VALUES (?1, ?2, ?3, ?4)")
          .bind(code, cityId, now, now + INVITE_SECONDS),
      ]);
      if (changes(results[1]) === 1) {
        log({ event: "invite_minted" });
        return json({ code, expires_at: now + INVITE_SECONDS });
      }
    }
    log({ event: "invite_failed" });
    throw new HttpError(503, "no_code");
  }

  async function inbox(request: Request, url: URL): Promise<Response> {
    const cityId = await cityOf(request);
    const after = intParam(url, "after", 0, 0, Number.MAX_SAFE_INTEGER);
    const limit = intParam(url, "limit", 100, 1, 200);
    const { results } = await db
      .prepare(
        // A row the v1 Worker wrote (between migration 0002 and this code's deploy) has no
        // conversation_id; its wa_id is its owner's key.
        `SELECT id, received_at, kind, wa_id, user_id, username, COALESCE(conversation_id, wa_id) AS conversation_id, profile_name, body
         FROM deliveries WHERE city_id = ?1 AND body IS NOT NULL AND id > ?2 ORDER BY id LIMIT ?3`,
      )
      .bind(cityId, after, limit)
      .all<{
        id: number;
        received_at: number;
        kind: string;
        wa_id: string | null;
        user_id: string | null;
        username: string | null;
        conversation_id: string | null;
        profile_name: string | null;
        body: string;
      }>();
    const deliveries = results.map((r) => ({
      id: r.id,
      received_at: r.received_at,
      kind: r.kind,
      wa_id: r.wa_id,
      user_id: r.user_id,
      username: r.username,
      conversation_id: r.conversation_id,
      profile_name: r.profile_name,
      message: JSON.parse(r.body) as unknown,
    }));
    return json({ deliveries, cursor: deliveries.at(-1)?.id ?? after });
  }

  async function ack(request: Request): Promise<Response> {
    const cityId = await cityOf(request);
    const body = await readJson(request);
    const ids = body.ids;
    if (
      !Array.isArray(ids) ||
      ids.length < 1 ||
      ids.length > MAX_ACK_IDS ||
      !ids.every((i) => typeof i === "number" && Number.isSafeInteger(i) && i > 0)
    ) {
      throw new HttpError(400, "bad_request");
    }
    // Exactly the listed rows of THIS city lose their bodies; an id already acknowledged, already
    // past the hold, deleted, or another city's is a no-op. The body-less row keeps the wamid so a
    // late Meta redelivery is still one row.
    const r = await db
      .prepare(
        `UPDATE deliveries SET body = NULL, wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL,
           profile_name = NULL, acked_at = ?1
         WHERE city_id = ?2 AND body IS NOT NULL AND id IN (SELECT value FROM json_each(?3))`,
      )
      .bind(deps.now(), cityId, JSON.stringify(ids))
      .run();
    log({ event: "ack", listed: ids.length, acknowledged: changes(r) });
    return json({ ok: true });
  }

  async function send(request: Request): Promise<Response> {
    const cityId = await cityOf(request);
    const body = await readJson(request);
    const to = parseTo(body.to);
    const key = boundedString(body.idempotency_key, 1, 200);
    if (!key) throw new HttpError(400, "bad_request");
    const req = parseSend(body);
    const now = deps.now();
    // `to` names THIS city's owners by their conversation_id (the id the box files one under) or
    // by an id Meta names one by now (its wa_id or its user_id). The rows are every owner it names.
    const { results: named } = await db
      .prepare(
        `SELECT conversation_id, wa_id, user_id, last_inbound_at FROM phones
         WHERE city_id = ?2 AND (conversation_id = ?1 OR wa_id = ?1 OR user_id = ?1) LIMIT 2`,
      )
      .bind(to, cityId)
      .all<{ conversation_id: string; wa_id: string | null; user_id: string | null; last_inbound_at: number | null }>();
    if (named.length === 0) {
      log({ event: "send_refused", reason: "not_bound" });
      throw new HttpError(403, "not_bound");
    }
    // The key first: a repeat answers the first answer, whatever the window says now. A key whose
    // first call may have reached Meta without an answer saying so never makes a second call.
    const prior = await db
      .prepare("SELECT wamid, state, created_at FROM sends WHERE city_id = ? AND idempotency_key = ?")
      .bind(cityId, key)
      .first<{ wamid: string | null; state: string; created_at: number }>();
    if (prior) return repeatAnswer(prior, now);
    // A `to` naming two owners (one's conversation_id, kept from a phone number it no longer
    // holds, and another's current wa_id: a recycled number) is refused, and nothing is sent: the
    // box may mean either, and a guess hands one owner's message to the other. The box resends to
    // the owner's current id. The answer names neither owner. (gp-0g2s; the mayor's rule R1.)
    if (named.length > 1) {
      log({ event: "send_refused", reason: "ambiguous_recipient" });
      throw new HttpError(409, "ambiguous_recipient", { hint: "send to the owner's current id" });
    }
    const owner = named[0]!;
    // Meta gets `to` exactly as given while it is one of the owner's ids. A conversation_id the
    // owner no longer answers to (its phone number before a number change) is sent to the owner's
    // current id instead: never to a number that may be someone else's now.
    const address = to === owner.wa_id || to === owner.user_id ? to : (owner.wa_id ?? owner.user_id);
    if (!address) {
      log({ event: "send_refused", reason: "not_bound" });
      throw new HttpError(403, "not_bound");
    }
    if (req.type !== "template" && !windowOpen(owner.last_inbound_at, now)) {
      log({ event: "send_refused", reason: "window_closed", type: req.type });
      throw new HttpError(422, "window_closed");
    }
    const client = graph();
    const claim = await db
      .prepare(
        `INSERT INTO sends (city_id, idempotency_key, wamid, state, type, created_at, conversation_id)
         VALUES (?1, ?2, NULL, 'claimed', ?3, ?4, ?5)
         ON CONFLICT (city_id, idempotency_key) DO NOTHING`,
      )
      .bind(cityId, key, req.type, now, owner.conversation_id)
      .run();
    if (changes(claim) !== 1) {
      const raced = await db
        .prepare("SELECT wamid, state, created_at FROM sends WHERE city_id = ? AND idempotency_key = ?")
        .bind(cityId, key)
        .first<{ wamid: string | null; state: string; created_at: number }>();
      if (raced) return repeatAnswer(raced, now);
      throw new HttpError(409, "in_flight");
    }
    let wamid: string;
    try {
      wamid = await graphSend(client, sendBody(address, req));
    } catch (e) {
      if (!(e instanceof GraphError)) throw e;
      log({ event: "send_failed", type: req.type, meta_status: e.status, meta_code: e.code });
      if (e.status >= 400 && e.status < 500) {
        // Meta answered and refused: nothing was sent, so the key is free for a retry.
        await db
          .prepare("DELETE FROM sends WHERE city_id = ? AND idempotency_key = ? AND state = 'claimed' AND created_at = ?")
          .bind(cityId, key, now)
          .run();
        if (e.code === META_OUTSIDE_WINDOW) throw new HttpError(422, "window_closed");
        throw new HttpError(502, "meta_error", { meta_status: e.status, meta_code: e.code });
      }
      // No answer, a 5xx, or a 2xx without an id: Meta may have sent it. The key stays spent.
      await db
        .prepare("UPDATE sends SET state = 'unknown' WHERE city_id = ? AND idempotency_key = ? AND state = 'claimed'")
        .bind(cityId, key)
        .run();
      throw new HttpError(502, "outcome_unknown", { meta_status: e.status, meta_code: e.code });
    }
    try {
      // Record the wamid and adopt any status Meta delivered before this record existed.
      await db.batch([
        db
          .prepare("UPDATE sends SET wamid = ?1, state = 'sent' WHERE city_id = ?2 AND idempotency_key = ?3")
          .bind(wamid, cityId, key),
        db
          .prepare(
            `INSERT OR IGNORE INTO deliveries
               (city_id, received_at, kind, wamid, status, wa_id, user_id, username, conversation_id, profile_name, body)
             SELECT ?1, received_at, 'status', wamid, status, recipient_id, recipient_user_id, username, ?3, NULL, body
             FROM orphan_statuses WHERE wamid = ?2
             ORDER BY received_at, rowid`,
          )
          .bind(cityId, wamid, owner.conversation_id),
        db.prepare("DELETE FROM orphan_statuses WHERE wamid = ?").bind(wamid),
      ]);
    } catch (e) {
      // Meta took it; the answer stands. A repeat of the key reads outcome_unknown, never a resend.
      log({ event: "send_record_failed", type: req.type, error: errorName(e) });
    }
    log({ event: "send", type: req.type });
    return json({ message_id: wamid });
  }

  /** What a repeated idempotency key answers: the first message_id, or why there is none. */
  function repeatAnswer(prior: { wamid: string | null; state: string; created_at: number }, now: number): Response {
    if (prior.wamid) return json({ message_id: prior.wamid });
    if (prior.state === "claimed" && prior.created_at > now - SEND_CLAIM_STALE_SECONDS) {
      throw new HttpError(409, "in_flight");
    }
    throw new HttpError(409, "outcome_unknown");
  }

  async function read(request: Request): Promise<Response> {
    const cityId = await cityOf(request);
    const body = await readJson(request);
    const messageId = boundedString(body.message_id, 1, 256);
    if (!messageId) throw new HttpError(400, "bad_request");
    const known = await db
      .prepare("SELECT 1 AS one FROM deliveries WHERE city_id = ? AND wamid = ? AND kind IN ('message', 'joined') LIMIT 1")
      .bind(cityId, messageId)
      .first();
    if (!known) throw new HttpError(404, "not_found");
    try {
      await graphMarkRead(graph(), messageId);
    } catch (e) {
      if (!(e instanceof GraphError)) throw e;
      log({ event: "read_failed", meta_status: e.status, meta_code: e.code });
      throw new HttpError(502, "meta_error", { meta_status: e.status, meta_code: e.code });
    }
    return json({ ok: true });
  }

  async function windows(request: Request): Promise<Response> {
    const cityId = await cityOf(request);
    const { results } = await db
      .prepare("SELECT conversation_id, user_id, last_inbound_at FROM phones WHERE city_id = ? ORDER BY bound_at, conversation_id")
      .bind(cityId)
      .all<{ conversation_id: string; user_id: string | null; last_inbound_at: number | null }>();
    return json({
      windows: results.map((r) => ({
        conversation_id: r.conversation_id,
        user_id: r.user_id,
        last_inbound_at: r.last_inbound_at,
        window_closes_at: windowClosesAt(r.last_inbound_at),
      })),
    });
  }

  async function media(request: Request, mediaId: string): Promise<Response> {
    const cityId = await cityOf(request);
    if (!/^[A-Za-z0-9_.-]{1,128}$/.test(mediaId)) throw new HttpError(404, "not_found");
    const known = await db
      .prepare("SELECT 1 AS one FROM media WHERE city_id = ? AND media_id = ? AND received_at > ?")
      .bind(cityId, mediaId, deps.now() - MEDIA_SECONDS)
      .first();
    if (!known) throw new HttpError(404, "not_found");
    try {
      return await graphMedia(graph(), mediaId);
    } catch (e) {
      if (!(e instanceof GraphError)) throw e;
      log({ event: "media_failed", meta_status: e.status, meta_code: e.code });
      if (e.status >= 400 && e.status < 500) throw new HttpError(404, "not_found");
      throw new HttpError(502, "meta_error", { meta_status: e.status, meta_code: e.code });
    }
  }

  async function healthz(): Promise<Response> {
    const row = await db
      .prepare(
        `SELECT (SELECT COUNT(*) FROM deliveries WHERE body IS NOT NULL) AS pending_total,
                (SELECT value FROM stats WHERE name = 'last_webhook_at') AS last_webhook_at`,
      )
      .first<{ pending_total: number; last_webhook_at: number | null }>();
    return json({ ok: true, pending_total: row?.pending_total ?? 0, last_webhook_at: row?.last_webhook_at ?? null });
  }

  // ---------------------------------------------------------------- Operator side

  async function mintEnrollCode(request: Request): Promise<Response> {
    await requireAdmin(request);
    const body = await readJson(request);
    const cityName = cleanName(body.city_name);
    if (!cityName) throw new HttpError(400, "bad_request");
    const now = deps.now();
    const code = newEnrollCode();
    await db
      .prepare("INSERT INTO enroll_codes (code, city_name, created_at, expires_at) VALUES (?, ?, ?, ?)")
      .bind(code, cityName, now, now + ENROLL_SECONDS)
      .run();
    log({ event: "enroll_code_minted" });
    return json({ code: showEnrollCode(code), expires_at: now + ENROLL_SECONDS });
  }

  async function listCities(request: Request): Promise<Response> {
    await requireAdmin(request);
    const [cities, phones, stats] = await db.batch([
      db.prepare(
        `SELECT c.id, c.name, c.box_name, c.created_at,
           (SELECT COUNT(*) FROM phones p WHERE p.city_id = c.id) AS phones,
           (SELECT COUNT(*) FROM deliveries d WHERE d.city_id = c.id AND d.body IS NOT NULL) AS held,
           (SELECT COUNT(*) FROM credentials k WHERE k.city_id = c.id AND k.revoked_at IS NULL) AS credentials
         FROM cities c ORDER BY c.created_at, c.id`,
      ),
      // Only the last four digits of a phone number ever leave D1 on this route; an owner Meta
      // shares no phone number of shows as "id", never any part of its user_id.
      db.prepare(
        "SELECT city_id, CASE WHEN wa_id IS NULL THEN 'id' ELSE substr(wa_id, -4) END AS last4 FROM phones ORDER BY bound_at, conversation_id",
      ),
      db.prepare("SELECT name, value FROM stats ORDER BY name"),
    ]);
    const last4 = new Map<string, string[]>();
    for (const p of (phones?.results ?? []) as Array<{ city_id: string; last4: string }>) {
      last4.set(p.city_id, [...(last4.get(p.city_id) ?? []), p.last4]);
    }
    return json({
      cities: ((cities?.results ?? []) as Array<Record<string, unknown> & { id: string }>).map((c) => ({
        ...c,
        phones_last4: last4.get(c.id) ?? [],
      })),
      counters: Object.fromEntries(((stats?.results ?? []) as Array<{ name: string; value: number }>).map((s) => [s.name, s.value])),
    });
  }

  // ---------------------------------------------------------------- Router

  type Handler = (request: Request, url: URL) => Promise<Response>;
  const routes: Record<string, Partial<Record<string, Handler>>> = {
    "/healthz": { GET: () => healthz() },
    "/meta/webhook": { GET: handshake, POST: (r) => webhook(r) },
    "/v1/enroll": { POST: (r) => enroll(r) },
    "/v1/invites": { POST: (r) => invites(r) },
    "/v1/inbox": { GET: inbox },
    "/v1/inbox/ack": { POST: (r) => ack(r) },
    "/v1/send": { POST: (r) => send(r) },
    "/v1/read": { POST: (r) => read(r) },
    "/v1/window": { GET: (r) => windows(r) },
    "/v1/admin/enroll-codes": { POST: (r) => mintEnrollCode(r) },
    "/v1/admin/cities": { GET: (r) => listCities(r) },
  };
  const MEDIA_PREFIX = "/v1/media/";

  async function handle(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const path = url.pathname;
    const isMedia = path.startsWith(MEDIA_PREFIX);
    const route = isMedia ? "/v1/media/:id" : path;
    try {
      if (isMedia) {
        if (request.method !== "GET") return fail(405, "method_not_allowed");
        return await media(request, path.slice(MEDIA_PREFIX.length));
      }
      // Own properties only: a path or a method named like an Object.prototype key is not a route.
      const methods = Object.hasOwn(routes, path) ? routes[path] : undefined;
      if (!methods) return fail(404, "not_found");
      const handler = Object.hasOwn(methods, request.method) ? methods[request.method] : undefined;
      if (!handler) return fail(405, "method_not_allowed");
      return await handler(request, url);
    } catch (e) {
      if (e instanceof HttpError) return fail(e.status, e.code, e.extra);
      log({ event: "internal_error", route: Object.hasOwn(routes, route) || isMedia ? route : "unknown", error: errorName(e) });
      return fail(500, "internal");
    }
  }

  /**
   * The daily cron: bodies past the hold, body-less rows past the dedup day, aged codes and keys,
   * aliases past their horizon and counted code wamids Meta can no longer redeliver.
   */
  async function maintain(): Promise<Record<string, number>> {
    const now = deps.now();
    const results = await db.batch([
      db
        .prepare(
          `UPDATE deliveries SET body = NULL, wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL, profile_name = NULL
           WHERE body IS NOT NULL AND received_at <= ?`,
        )
        .bind(now - HOLD_SECONDS),
      db.prepare("DELETE FROM deliveries WHERE received_at <= ?").bind(now - TOMBSTONE_SECONDS),
      db.prepare("DELETE FROM media WHERE received_at <= ?").bind(now - MEDIA_SECONDS),
      db
        .prepare("DELETE FROM unknown_senders WHERE COALESCE(last_replied_at, 0) <= ? AND attempts_since <= ?")
        .bind(now - CANNED_EVERY_SECONDS, now - DAY),
      db.prepare("DELETE FROM invite_codes WHERE expires_at <= ?").bind(now - SPENT_CODES_KEEP_SECONDS),
      db.prepare("DELETE FROM enroll_codes WHERE expires_at <= ?").bind(now - ENROLL_CODES_KEEP_SECONDS),
      db.prepare("DELETE FROM sends WHERE created_at <= ?").bind(now - SENDS_KEEP_SECONDS),
      db.prepare("DELETE FROM orphan_statuses WHERE received_at <= ?").bind(now - ORPHAN_SECONDS),
      // A body-less row keeps no id: one the v1 Worker acknowledged after migration 0002 kept the
      // conversation_id the migration wrote.
      db.prepare(
        `UPDATE deliveries SET wa_id = NULL, user_id = NULL, username = NULL, conversation_id = NULL, profile_name = NULL
         WHERE body IS NULL AND COALESCE(wa_id, user_id, username, conversation_id, profile_name) IS NOT NULL`,
      ),
      db.prepare("DELETE FROM phone_aliases WHERE expires_at <= ?").bind(now),
      db.prepare("DELETE FROM code_attempt_wamids WHERE counted_at <= ?").bind(now - CODE_WAMIDS_KEEP_SECONDS),
    ]);
    const [bodies, rows, mediaRows, strangers, invitesGone, enrollGone, sendsGone, orphansGone, idsLeft, aliasesGone, codeWamidsGone] =
      results.map(changes);
    return {
      bodies_expired: bodies ?? 0,
      rows_deleted: rows ?? 0,
      media_deleted: mediaRows ?? 0,
      strangers_deleted: strangers ?? 0,
      invite_codes_deleted: invitesGone ?? 0,
      enroll_codes_deleted: enrollGone ?? 0,
      sends_deleted: sendsGone ?? 0,
      orphan_statuses_deleted: orphansGone ?? 0,
      stray_ids_cleared: idsLeft ?? 0,
      aliases_deleted: aliasesGone ?? 0,
      code_wamids_deleted: codeWamidsGone ?? 0,
    };
  }

  return { handle, maintain };
}
