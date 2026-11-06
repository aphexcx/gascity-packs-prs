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
 * Owners: Meta names a WhatsApp user by a wa_id (a phone number) and a user_id (a business-scoped
 * user id); since early April 2026 every message carries the user_id. An owner row is keyed by its
 * conversation_id, set once at the bind and never changed. A message routes by its user_id alone,
 * never by its number, and every send goes to a user_id: the design of record,
 * whatsapp/docs/design-bsuid-pairing-and-spans.md (README.md, "Business-scoped user ids").
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
 * How long a notice's link, an unbind's record and a mark naming no binding are kept, from their
 * stamp: no message they may decide (one Meta stamped at or before it) passes the stale filter
 * later than this.
 */
export const EVIDENCE_SECONDS = TOMBSTONE_SECONDS;
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
 * The ids a message names its sender by. A number-change notice is routed by its previous user_id
 * alone: never by a contacts entry the parse matched by its old number, which can be the number's
 * next holder (the codex self-review r2, finding 1).
 */
function senderOf(m: InboundMessage): { wa: string | null; user: string | null } {
  return { wa: m.from, user: m.change ? m.change.previousUserId : m.fromUserId };
}

/*
 * Who a message is from: the design of record (whatsapp/docs/design-bsuid-pairing-and-spans.md,
 * section 2). Every decision below reads its sender's user_id and Meta's stamp of the message,
 * never a phone number. A message naming no user_id (and a notice naming no previous_user_id)
 * reaches no city and binds nothing: a number can change hands without the gateway seeing it, and
 * the pairing that would say so may still be in flight.
 *
 * A binding (a phones row) answers to its current user_id and to each user_id a chain of notice
 * links ends at it from (links: old id -> new id, at the notice's stamp). It answers to an id from
 * its invite text (bound_stamp) or that id's last unbind, whichever is later, until the notice
 * that replaces the id. One id, one binding: a chain stops at the first id a binding answers to
 * now, a link to an id another binding answers to is never recorded, and a code binds only an id
 * no binding answers to.
 *
 * walkFrom(id) is that chain: each id from the given one along the links, with `until`, the stamp
 * of the notice that replaced the given id (NULL while it is current), stopping at the first id
 * that is some binding's current user_id. Every fragment below joins it to phones. It has no
 * depth limit: an id has at most one link out (links' key), so the chain is a path, and UNION's
 * set semantics end it should Meta ever link back into it. A depth limit would read a long chain
 * as unowned and let a code bind its first id a second time (the codex self-review r1, finding 3).
 */
const walkFrom = (id: string) =>
  `WITH RECURSIVE walk(id, until) AS (
     SELECT ${id}, NULL
     UNION
     SELECT l.new_id, COALESCE(walk.until, l.stamp) FROM walk JOIN links l ON l.old_id = walk.id
     WHERE NOT EXISTS (SELECT 1 FROM phones h WHERE h.user_id = walk.id))`;
/** The binding the user id answers to now, at any stamp: the one it is, or its chain ends at. */
const holder = (id: string) => `(${walkFrom(id)} SELECT p.conversation_id FROM walk JOIN phones p ON p.user_id = walk.id LIMIT 1)`;
/**
 * The unbind floor of a user id: the gateway's clock at the last unbind of any id linked to it,
 * in either direction (its link component), or NULL. One fragment, read by every decision that
 * asks whether the owner behind an id was unbound: the route, the stranger test and the re-key.
 * An unbind records the ids its binding answered to; a notice that lands after it, linking an
 * earlier or a later id to one of them, joins that id to the record's component, so what the
 * owner wrote under any of its ids before the unbind reaches no city and binds nothing (point 2;
 * the codex self-review r1 finding 1 and r2 findings 2 and 3). Two people share a component only
 * if Meta reissues a retired user id; the floor then fails closed for both.
 */
const floorOf = (id: string) =>
  `(WITH RECURSIVE comp(id) AS (
      SELECT ${id}
      UNION
      SELECT CASE WHEN l.old_id = comp.id THEN l.new_id ELSE l.old_id END FROM comp JOIN links l ON l.old_id = comp.id OR l.new_id = comp.id)
    SELECT MAX(u.unbound_at) FROM comp JOIN unbinds u ON u.user_id = comp.id)`;
/**
 * The binding a message routes to: the one answering to its user id at Meta's stamp, or NULL. The
 * stamp is at or after the binding's invite text and after the id's last unbind, and at or before
 * the notice that replaced the id (when one did).
 */
const route = (id: string, stamp: string) =>
  `(${walkFrom(id)} SELECT p.conversation_id FROM walk JOIN phones p ON p.user_id = walk.id
     WHERE (walk.until IS NULL OR ${stamp} <= walk.until) AND ${stamp} >= COALESCE(p.bound_stamp, 0)
       AND ${stamp} > COALESCE(${floorOf(id)}, -1)
     LIMIT 1)`;
/**
 * A stranger: a user id no binding answers to, writing after the id's last unbind. Its live code
 * binds; any other text hears the canned line. A message under a known id that no span holds
 * (stamped before the binding's invite text, or after the notice that replaced the id, or at or
 * before the id's unbind) is no stranger's: it reaches no city, binds nothing and is counted.
 */
const stranger = (id: string, stamp: string) =>
  `(${id} IS NOT NULL AND ${holder(id)} IS NULL AND ${stamp} > COALESCE(${floorOf(id)}, -1))`;
/**
 * The re-key, for the binding whose current user id is ?1: along the links from it, to the newest
 * id no other binding answers to, the one with no further link to follow. A link is reached when
 * its old id becomes a binding's, by the notice that links to it or by a bind under it, and both
 * run this one statement (point 2). No depth limit, as walkFrom; a chain that loops has no end,
 * and the binding keeps its id. It follows only a link Meta stamped after the unbind floor of the
 * binding's id: a notice from before an unbind is the unbound owner's, never the binding's that
 * holds the id now (the codex self-review r2, finding 3).
 */
const REKEY = `UPDATE phones SET user_id = COALESCE((
     WITH RECURSIVE fwd(id) AS (
       SELECT ?1
       UNION
       SELECT l.new_id FROM fwd JOIN links l ON l.old_id = fwd.id
       WHERE l.stamp > COALESCE(${floorOf("?1")}, -1) AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = l.new_id))
     SELECT f.id FROM fwd f
     WHERE NOT EXISTS (SELECT 1 FROM links l WHERE l.old_id = f.id AND l.stamp > COALESCE(${floorOf("?1")}, -1)
                         AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = l.new_id))
     LIMIT 1), ?1)
   WHERE user_id = ?1`;
/** The number a binding shows (the admin view, a delivery omitting it): its id's newest mark. */
const shownNumber = (row: string) =>
  `(CASE WHEN ${row}.user_id IS NULL THEN ${row}.wa_id
         ELSE (SELECT k.number FROM marks k WHERE k.user_id = ${row}.user_id ORDER BY k.stamp DESC, k.number LIMIT 1) END)`;

interface OwnerRow {
  conversation_id: string;
  user_id: string | null;
  city_id: string;
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
  /** Statement indexes whose one changed row means "a message that reached no city and is no stranger's, counted". */
  unrouted: number[];
  /** Number-change notices naming no previous user id: they link nothing, and the owner needs a new invite. */
  noPreviousId: number;
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
      ["notices_without_previous_id", plan.noPreviousId],
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
      // The welcome goes to the binding's current user id, read after the commit (a link waiting
      // from the bound id may have re-keyed it).
      const bound = await db
        .prepare("SELECT c.name, p.user_id FROM phones p JOIN cities c ON c.id = p.city_id WHERE p.bound_wamid = ?")
        .bind(b.wamid)
        .first<{ name: string; user_id: string | null }>();
      if (await tell(bound?.user_id ?? b.id, `You're connected to ${bound?.name ?? "your city"}'s mayor. Say hi.`, "welcome")) welcomed += 1;
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
      unrouted: plan.unrouted.filter(committed).length,
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

  /** One gateway-authored text (the welcome, the canned line) to a user id, in `recipient`. Never throws. */
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
      unrouted: [],
      noPreviousId: 0,
      orphanedStatuses: 0,
      stale: 0,
      overCap: false,
    };
    // Meta retries a delivery for up to 7 days, and a body-less row dedups a wamid for 8 days from
    // its receipt (never earlier than Meta's stamp). A message stamped longer ago than that could
    // be a copy whose dedup row is gone, so it is dropped and counted, never delivered again.
    const fresh = messages.filter((m) => m.timestamp === null || m.timestamp > now - TOMBSTONE_SECONDS);
    plan.stale = messages.length - fresh.length;
    // A payload's number-change notices are planned first: a message in the same payload under the
    // id a notice brings routes by the link the notice records, whatever order Meta listed them in.
    messages = [...fresh.filter((m) => m.change), ...fresh.filter((m) => !m.change)];
    const each = async <T>(sql: string, values: string[], ...extra: unknown[]): Promise<T[]> =>
      values.length
        ? (await db.prepare(sql).bind(JSON.stringify(values), ...extra).all<T>()).results
        : [];

    // Reads taken before the write, for the 5,000 cap and the stranger paths only: the bindings
    // whose current user_id a message names. The write itself routes from the tables as they
    // stand inside its own transaction, through the links as well.
    const ids = messages.map(senderOf);
    const userIds = [...new Set(ids.map((s) => s.user).filter((v): v is string => v !== null))];
    const rows = (
      await each<OwnerRow>("SELECT conversation_id, user_id, city_id FROM phones WHERE user_id IN (SELECT value FROM json_each(?1))", userIds)
    );
    const owners = ids.map((s) => rows.find((r) => s.user !== null && r.user_id === s.user));
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
      if (wa === null && user === null && !m.change) continue; // parseWebhook yields only a notice naming no id
      plan.groupStarts.push(plan.statements.length);
      // The window's clock and the invite's expiry read Meta's stamp clamped to the gateway's
      // clock (at); who held an id reads Meta's own stamps (stamp), all on one clock.
      const at = Math.min(m.timestamp ?? now, now);
      const stamp = m.timestamp ?? now;
      const body = JSON.stringify(m.raw);
      const owned = owners[i] !== undefined;
      // Only a user id binds, hears the welcome or the canned line, or counts a code attempt:
      // every send goes to a user id (design note, section 2, point 3).
      const code = owned || user === null ? null : inviteCodeIn(m);
      if (wa !== null && m.fromUserId !== null && !m.change) {
        // Meta pairs this number with this user id at the stamp: the number's high-water mark
        // moves to it, unless Meta stamped the message below the mark (a late retry). The mark
        // decides only which binding shows the number, never a route (point 4), so it moves
        // whoever sent the message: an owner, a stranger, a code text.
        push(
          db
            .prepare(
              `INSERT INTO marks (number, user_id, stamp) VALUES (?1, ?2, ?3)
               ON CONFLICT (number) DO UPDATE SET user_id = excluded.user_id, stamp = excluded.stamp WHERE excluded.stamp >= marks.stamp`,
            )
            .bind(wa, m.fromUserId, stamp),
        );
      }
      if (code !== null && user !== null) {
        // A stranger's code attempt: counted per day and once per wamid (a redelivery of any text
        // already counted is not a second attempt), then the bind, conditional on a code that was
        // live when Meta stamped the text and on the attempt count, never on this read. The wamid
        // is recorded after the count, so the count reads it unrecorded the first time only. A
        // stranger is keyed by its user id.
        push(
          db
            .prepare(
              `INSERT INTO unknown_senders (sender_key, code_attempts, attempts_since)
               SELECT ?1, 1, ?2 WHERE ${stranger("?1", "?5")}
               ON CONFLICT (sender_key) DO UPDATE SET
                 code_attempts = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN code_attempts
                                      WHEN attempts_since <= ?3 THEN 1 ELSE code_attempts + 1 END,
                 attempts_since = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN attempts_since
                                       WHEN attempts_since <= ?3 THEN ?2 ELSE attempts_since END`,
            )
            .bind(user, now, now - DAY, m.wamid, stamp),
        );
        push(db.prepare("INSERT OR IGNORE INTO code_attempt_wamids (wamid, counted_at) VALUES (?1, ?2)").bind(m.wamid, now));
        // The bind: only a stranger's user id, one no binding answers to now (by its own id or a
        // chain of links, so a previous id of a standing binding never binds a second one) and
        // Meta stamped after the id's last unbind (a text the owner sent while bound elsewhere is
        // that binding's, never a new one's: the design note's T8). The number beside it never
        // matters: a phone number Meta pairs with a new user id is no claim on it (no adoption,
        // point 5). The row holds no number (the mark shows one); its conversation_id is the wa_id,
        // unless that number is already another owner's conversation_id (a recycled number), else
        // the user_id (README rule 1). It answers to the id from this text's stamp. The rows below
        // find it by the wamid that bound it.
        const bindIndex = push(
          db
            .prepare(
              `INSERT INTO phones (conversation_id, wa_id, user_id, username, city_id, profile_name, bound_at, bound_wamid, last_inbound_at, bound_stamp)
               SELECT k.id, NULL, ?2, ?3, i.city_id, ?4, ?5, ?6, ?7, ?10
               FROM invite_codes i,
                    (SELECT CASE WHEN ?1 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE conversation_id = ?1)
                                 THEN ?1 ELSE ?2 END AS id) k
               WHERE i.code = ?8 AND i.used_at IS NULL AND i.expires_at > ?7
                 AND COALESCE((SELECT code_attempts FROM unknown_senders WHERE sender_key = ?2), 0) <= ?9
                 AND NOT EXISTS (SELECT 1 FROM phones p WHERE p.conversation_id = k.id)
                 AND ${stranger("?2", "?10")}
               ON CONFLICT DO NOTHING`,
            )
            .bind(wa, user, m.username, m.profileName, now, m.wamid, at, code, MAX_CODE_ATTEMPTS, stamp),
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
        // A link already waiting from the bound id (its notice landed before this text) is reached
        // now: the new binding is re-keyed along it, as a notice's own landing would.
        push(db.prepare(REKEY).bind(user));
        plan.binds.push({ index: bindIndex, id: user, wamid: m.wamid });
      }
      // The message itself, routed to the binding answering to its user id when Meta stamped it,
      // as the tables stand inside this transaction (point 1). After a bind by this very text its
      // wamid is already the joined row's, so this is one row. A wamid any city already holds is
      // that city's: a retry after the phone moved cities is no new row. A number the message
      // omits is the one the binding shows (its mark).
      push(
        db
          .prepare(
            `INSERT OR IGNORE INTO deliveries
               (city_id, received_at, kind, wamid, status, wa_id, user_id, username, conversation_id, profile_name, body)
             SELECT p.city_id, ?1, 'message', ?2, '', COALESCE(?3, ${shownNumber("p")}), COALESCE(?4, p.user_id), ?5, p.conversation_id, ?6, ?7
             FROM phones p WHERE p.conversation_id = ${route("?4", "?8")}`,
          )
          .bind(now, m.wamid, wa, user, m.username, m.profileName, body, stamp),
      );
      // A message that reaches no city and is no stranger's hears nothing and is counted, in the
      // stats and on the payload's log line: one naming no user id, a known id's message no span
      // holds (stamped before its binding's invite text, after the notice that replaced the id, or
      // at or before the id's unbind), and a notice no binding answers to (point 7: counted, never
      // held or refused).
      plan.unrouted.push(push(
        db
          .prepare(
            `INSERT INTO stats (name, value) SELECT 'unrouted_messages', 1
             WHERE ${route("?1", "?2")} IS NULL AND (?3 OR NOT ${stranger("?1", "?2")})
             ON CONFLICT (name) DO UPDATE SET value = value + 1`,
          )
          .bind(user, stamp, m.change ? 1 : 0),
      ));
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
      // The routed binding's window moves on any of its messages, under its current id or a
      // previous one, never back. Its names follow a message under its current id only: a retry
      // under a previous id is older than them. No id is ever filled or taken: a row's user id
      // changes only by a notice's link (point 5, no adoption).
      push(
        db
          .prepare(
            `UPDATE phones SET
               last_inbound_at = CASE WHEN ?3 THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,
               profile_name = CASE WHEN user_id = ?5 THEN COALESCE(?1, profile_name) ELSE profile_name END,
               username = CASE WHEN user_id = ?5 THEN COALESCE(?2, username) ELSE username END
             WHERE conversation_id = ${route("?5", "?6")}`,
          )
          .bind(m.profileName, m.username, movesWindow(m.type) ? 1 : 0, at, user, stamp),
      );
      if (m.change) {
        // A number change (point 2). The notice becomes a link from its previous user id to its
        // new one, whatever order Meta delivers notices in: one link per old id, so a replay
        // changes nothing, and never a link to an id another binding answers to (one id, one
        // binding). A notice naming no previous user id links nothing: the owner is re-invited,
        // and the count tells the operator so.
        const { previousUserId, userId, waId } = m.change;
        if (previousUserId !== null && userId !== null) {
          push(
            db
              .prepare(
                `INSERT OR IGNORE INTO links (old_id, new_id, stamp)
                 SELECT ?1, ?2, ?3 WHERE ?1 <> ?2 AND ${holder("?2")} IS NULL`,
              )
              .bind(previousUserId, userId, stamp),
          );
          // A link a binding reaches re-keys it: from its current id, along the links, to the
          // newest id no other binding answers to. A link waiting for its old id's own notice is
          // reached when that one lands, by this same statement.
          push(db.prepare(REKEY).bind(previousUserId));
        }
        // The mark: the new number shows the new id from the notice's stamp, the old number shows
        // nobody, each unless Meta already paired it later (point 4).
        if (waId !== null && userId !== null) {
          push(
            db
              .prepare(
                `INSERT INTO marks (number, user_id, stamp) VALUES (?1, ?2, ?3)
                 ON CONFLICT (number) DO UPDATE SET user_id = excluded.user_id, stamp = excluded.stamp WHERE excluded.stamp >= marks.stamp`,
              )
              .bind(waId, userId, stamp),
          );
        }
        if (wa !== null && wa !== waId) {
          push(
            db
              .prepare(
                `INSERT INTO marks (number, user_id, stamp) VALUES (?1, NULL, ?2)
                 ON CONFLICT (number) DO UPDATE SET user_id = NULL, stamp = excluded.stamp WHERE excluded.stamp >= marks.stamp`,
              )
              .bind(wa, stamp),
          );
        }
        if (previousUserId === null) plan.noPreviousId += 1;
      }
      if (!owned && !m.change && user !== null) {
        // A stranger hears the canned line at its user id at most once per 7 days, claimed before
        // it is sent. A number change is Meta's notice, not the user writing: never answered. A
        // message naming no user id hears nothing: the gateway sends only to user ids.
        const cannedIndex = push(
          db
            .prepare(
              `INSERT INTO unknown_senders (sender_key, last_replied_at)
               SELECT ?1, ?2 WHERE ${stranger("?1", "?4")}
               ON CONFLICT (sender_key) DO UPDATE SET last_replied_at = excluded.last_replied_at
               WHERE unknown_senders.last_replied_at IS NULL OR unknown_senders.last_replied_at <= ?3`,
            )
            .bind(user, now, now - CANNED_EVERY_SECONDS, stamp),
        );
        plan.canned.push({ index: cannedIndex, id: user });
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
    // `to` names THIS city's owners by their conversation_id (the id the box files one under), by
    // their current user_id, or by a number one shows: the number whose newest pairing (its mark)
    // is with the owner's current user_id. The rows are every owner it names.
    const { results: named } = await db
      .prepare(
        `SELECT conversation_id, user_id, last_inbound_at FROM phones p
         WHERE city_id = ?2 AND (conversation_id = ?1 OR user_id = ?1
           OR EXISTS (SELECT 1 FROM marks k WHERE k.number = ?1 AND k.user_id = p.user_id)) LIMIT 2`,
      )
      .bind(to, cityId)
      .all<{ conversation_id: string; user_id: string | null; last_inbound_at: number | null }>();
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
    // holds, and the number another shows now: a recycled number) is refused, and nothing is
    // sent: the box may mean either, and a guess hands one owner's message to the other. The box
    // resends to the owner's current id. (gp-0g2s; the mayor's rule R1.)
    // The box files an owner under its conversation_id, so the answer names the owner `to` keys by
    // its current user_id (null when it has none), and the box can resend at once; GET /v1/window
    // shows the same id. Both rows are this city's own owners; the answer carries neither the
    // number nor the other owner. (gp-w376; the Fable read r1 MINOR 2.)
    if (named.length > 1) {
      log({ event: "send_refused", reason: "ambiguous_recipient" });
      const keyed = named.find((o) => o.conversation_id === to);
      throw new HttpError(409, "ambiguous_recipient", {
        hint: "send to the owner's current id",
        owner_user_id: keyed?.user_id ?? null,
      });
    }
    const owner = named[0]!;
    // Every send goes to the owner's current user id, in Meta's `recipient`, whatever `to` named
    // it by (design note, section 2, point 3): a number can change hands without the gateway
    // seeing it, a user id cannot. An owner with none (one PR 73 bound by its number alone, which
    // the mayor unbinds and re-invites at the deploy) has no address.
    const address = owner.user_id;
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
      // Only the last four digits of a phone number ever leave D1 on this route: the number the
      // owner shows (its user id's newest mark). An owner that shows none shows as "id", never
      // any part of its user_id.
      db.prepare(
        `SELECT city_id, CASE WHEN n IS NULL THEN 'id' ELSE substr(n, -4) END AS last4
         FROM (SELECT p.city_id, p.bound_at, p.conversation_id, ${shownNumber("p")} AS n FROM phones p)
         ORDER BY bound_at, conversation_id`,
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

  /**
   * The operator's unbind (README, "Unbind an owner"): every binding `id` names (its
   * conversation_id, its current user_id, or the number a binding PR 73 made holds) is deleted,
   * and every user id it answered to in the last 8 days, its current one and each its links
   * reached, is recorded first, in the same batch. A message Meta stamped at or before that is the
   * unbound binding's: it reaches no city and binds nothing, so no later binding takes what the
   * owner wrote before the unbind (the design note's T8).
   */
  async function unbindOwner(request: Request): Promise<Response> {
    await requireAdmin(request);
    const body = await readJson(request);
    const id = boundedString(body.id, 1, 256);
    if (!id) throw new HttpError(400, "bad_request");
    const now = deps.now();
    const [, gone] = await db.batch([
      db
        .prepare(
          `INSERT OR IGNORE INTO unbinds (user_id, unbound_at)
           WITH RECURSIVE answered(id) AS (
             SELECT user_id FROM phones WHERE (conversation_id = ?1 OR user_id = ?1 OR wa_id = ?1) AND user_id IS NOT NULL
             UNION
             SELECT l.old_id FROM answered JOIN links l ON l.new_id = answered.id)
           SELECT id, ?2 FROM answered`,
        )
        .bind(id, now),
      db.prepare("DELETE FROM phones WHERE conversation_id = ?1 OR user_id = ?1 OR wa_id = ?1").bind(id),
    ]);
    log({ event: "owner_unbound", bindings: changes(gone) });
    return json({ unbound: changes(gone) });
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
    "/v1/admin/unbind": { POST: (r) => unbindOwner(r) },
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
   * counted code wamids Meta can no longer redeliver, and the evidence no message Meta may still
   * deliver can need (8 days from its stamp): notice links, unbind records, and marks that name no
   * binding (a mark naming a binding's current id is the number it shows, kept while it is).
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
      db.prepare("DELETE FROM code_attempt_wamids WHERE counted_at <= ?").bind(now - CODE_WAMIDS_KEEP_SECONDS),
      db.prepare("DELETE FROM links WHERE stamp <= ?").bind(now - EVIDENCE_SECONDS),
      db.prepare("DELETE FROM unbinds WHERE unbound_at <= ?").bind(now - EVIDENCE_SECONDS),
      db
        .prepare(
          `DELETE FROM marks WHERE stamp <= ?1
             AND (user_id IS NULL OR NOT EXISTS (SELECT 1 FROM phones p WHERE p.user_id = marks.user_id))`,
        )
        .bind(now - EVIDENCE_SECONDS),
    ]);
    const [bodies, rows, mediaRows, strangers, invitesGone, enrollGone, sendsGone, orphansGone, idsLeft, codeWamidsGone, linksGone, unbindsGone, marksGone] =
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
      code_wamids_deleted: codeWamidsGone ?? 0,
      links_deleted: linksGone ?? 0,
      unbinds_deleted: unbindsGone ?? 0,
      marks_deleted: marksGone ?? 0,
    };
  }

  return { handle, maintain };
}
