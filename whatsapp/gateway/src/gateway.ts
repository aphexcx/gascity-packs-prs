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
/**
 * How long a body-less row stays to dedup a late Meta redelivery. Meta retries for up to 7 days: an
 * assumption (README.md, "Business-scoped user ids", rule 5, "The horizon"). The horizon's safety
 * rests on these 8 days, not on Meta's 7: the stale filter drops whatever Meta stamped longer ago.
 */
export const TOMBSTONE_SECONDS = 8 * DAY;
/**
 * How long an owner's ids from before a number change stay its aliases, from the change Meta
 * stamped: no message an alias may route (one Meta stamped at or before the change) can pass the
 * stale filter later than this.
 */
export const ALIAS_SECONDS = TOMBSTONE_SECONDS;
/** How long a counted wrong code's wamid is kept: as long as Meta may redeliver it. */
const CODE_WAMIDS_KEEP_SECONDS = TOMBSTONE_SECONDS;
/**
 * Meta keeps a webhook media id for 7 days. That is the media id's lifetime, not Meta's retry window
 * (an assumption: README.md, "Business-scoped user ids", rule 5, "The horizon").
 */
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

/*
 * Who a message is from. Every SQL fragment below takes a sender's ids and Meta's stamp of the
 * message (SQL parameters wa, user and stamp).
 *
 * An id is held by one binding (a phones row) over a span of Meta's time:
 * - A current user_id since the binding's own invite text (bound_stamp).
 * - A current wa_id since the invite text too, or, when the number came later (a message carrying
 *   it beside the user_id, or a number change), since that message (wa_since).
 * - A previous id (phone_aliases) from its since until the number change that replaced it. A
 *   previous number's since is where the owner came to it (wa_since), and the bind only when no
 *   message or change brought it a number since (gp-w376).
 * - A paired number (phone_aliases, open: changed_at = OPEN; gp-byp8): one Meta named beside the
 *   binding's current user_id, by a message or by its number change, while another row held it at
 *   that stamp, so the binding's row could not take it. Only a phone-only owner can still hold it
 *   then (rule 2 strips a row with another user_id). It is held from where the owner came to it
 *   (the earliest such stamp, or the change that brought the owner to its number) until the span
 *   ends: at the owner's next number change, when Meta names the number beside another current
 *   user_id (rule 2), or, once an operator unbound the owner, at the next cron. An ended span is a
 *   previous id for the alias horizon. Like any previous id it holds only a message that names no
 *   user_id. It stays open when the owner's row later takes the number: one binding, held twice.
 * A message routes to the one binding whose span holds its stamp, or to nobody. It never routes to
 * a binding that took the id after Meta stamped the message (a later holder of a recycled number,
 * or the same person bound again elsewhere). Two bindings holding one stamp is a conflict: the
 * records disagree, and the message goes to nobody, counted with the conflict as its reason.
 *
 * Late evidence refuses, never routes (gp-sv2d; Afik's (A), 11:20Z 11/5). A previous id or paired
 * number holds its stamps from since and routes them from routes_from. A span the gateway learned
 * on time routes from its start: the owner's own number change, or a pairing under the binding's
 * current user_id stamped at or after the start of the span it opens or falls in. What it learns
 * late only refuses: a pairing under a previous user_id (Meta delivers it after the owner's change)
 * is kept as a span that never routes, and a pairing stamped before a span's start moves only
 * where the span holds from, never where it routes from. A stamp that only a refuse-only part holds
 * goes to nobody, counted; one another binding holds too is a conflict. That fails closed, the cost
 * (A) accepts: once the other row's owner is unbound, the owner's own messages stamped in a late
 * part go to nobody, counted, never to the owner. A message that arrives late is still routed by
 * the spans as they stand when it lands (a retry of the owner's text under a previous user_id goes
 * by that id's previous holding, learned on time from the change); only the pairing it carries is
 * late evidence.
 *
 * Which id is matched: the user_id when the message names one (Meta gives one to no one else),
 * else the wa_id. A current wa_id also takes a message naming a user_id when its row holds none
 * (one owner, two ids, one row). It never takes a row holding another user_id: a phone number Meta
 * reports beside a different user_id is someone else's (a recycled number).
 */
/** Any row holding the ids now, whatever its span: a sender some binding knows. */
const ownerMatch = (wa: string, user: string) => `(user_id = ${user} OR (wa_id = ${wa} AND (user_id IS NULL OR ${user} IS NULL)))`;
/** A row holding the ids now that held them already when Meta stamped the message. */
const heldAt = (wa: string, user: string, stamp: string) =>
  `((user_id = ${user} AND COALESCE(bound_stamp, 0) <= ${stamp})
     OR (wa_id = ${wa} AND (user_id IS NULL OR ${user} IS NULL) AND COALESCE(wa_since, bound_stamp, 0) <= ${stamp}))`;
/** The one owner of a match: the user_id's row before the wa_id's (they differ only for two rows). */
const ownerFirst = (user: string) => `ORDER BY CASE WHEN user_id = ${user} THEN 0 ELSE 1 END LIMIT 1`;
const ownerKey = (wa: string, user: string, stamp: string) =>
  `(SELECT conversation_id FROM phones WHERE ${heldAt(wa, user, stamp)} ${ownerFirst(user)})`;
/**
 * The holdings in phone_aliases of the message's id whose spans hold the stamp: its previous ids
 * and its paired numbers, each with whether it routes the stamp (gp-sv2d: a refuse-only part, one
 * learned late, holds and never routes). No expiry test is needed: a message stamped at or before
 * changed_at that passes the stale filter arrives before changed_at + ALIAS_SECONDS, and an open
 * span has not ended.
 */
const otherHolding = (wa: string, user: string, stamp: string) =>
  `(SELECT conversation_id, bound_wamid, routes_from <= ${stamp} AS routes FROM phone_aliases
    WHERE id = COALESCE(${user}, ${wa}) AND since <= ${stamp} AND changed_at >= ${stamp})`;
/**
 * Every binding whose span holds the stamp, each with whether that holding routes it: the current
 * holder the message matches (it routes), and each other holding. When that holder is a phone-only
 * row the message reached by its number beside a user_id no row holds (rule 3: one owner, two ids),
 * each binding's span of the number holds it too (gp-byp8): Meta has paired that number with
 * another binding, so the phone-only row's claim is in doubt. That only ever adds a second holder:
 * it refuses, and never routes.
 */
const holdings = (wa: string, user: string, stamp: string) =>
  `(SELECT conversation_id, bound_wamid, 1 AS routes FROM phones WHERE conversation_id = ${ownerKey(wa, user, stamp)}
    UNION ALL SELECT conversation_id, bound_wamid, routes FROM ${otherHolding(wa, user, stamp)}
    UNION ALL SELECT conversation_id, bound_wamid, 0 FROM phone_aliases
    WHERE id = ${wa} AND since <= ${stamp} AND changed_at >= ${stamp}
      AND NOT EXISTS (SELECT 1 FROM phones WHERE user_id = ${user}) AND ${ownerKey(wa, user, stamp)} IS NOT NULL)`;
/** How many bindings hold the stamp. A binding is its bind wamid: a later binding under the same key is another owner. */
const holders = (wa: string, user: string, stamp: string) => `(SELECT COUNT(DISTINCT bound_wamid) FROM ${holdings(wa, user, stamp)})`;
/**
 * The owner a message routes to: the one binding whose span holds the stamp, while it stands, and
 * through a holding that routes it. Nobody when no span holds it, when that binding is gone (an
 * operator unbound it: a previous holding never routes on to whoever holds the id now), when two
 * bindings hold it (a conflict), or when the one binding holds it only by what it learned late
 * (gp-sv2d: refuse-only). One binding may hold a stamp twice (a paired number up to the stamp its
 * row took it): it is one binding, and it routes the stamp when any of its holdings does.
 */
const routeKey = (wa: string, user: string, stamp: string) =>
  `(SELECT p.conversation_id FROM phones p JOIN ${holdings(wa, user, stamp)} h
      ON p.conversation_id = h.conversation_id AND p.bound_wamid = h.bound_wamid AND h.routes
    WHERE ${holders(wa, user, stamp)} = 1 LIMIT 1)`;
/**
 * Two bindings hold the stamp: the records disagree, and the message goes to nobody. A number's
 * next holder showed it before the old owner's change notice landed, or a phone-only owner holds a
 * number Meta paired with another binding's user_id (gp-byp8).
 */
const conflict = (wa: string, user: string, stamp: string) => `(${holders(wa, user, stamp)} > 1)`;
/**
 * A sender no binding knows: no row holds its ids now, and no other holding spans the stamp. A
 * known sender whose message no span holds is no stranger: the message is dropped, counted and
 * never answered.
 */
const stranger = (wa: string, user: string, stamp: string) =>
  `(NOT EXISTS ${otherHolding(wa, user, stamp)} AND NOT EXISTS (SELECT 1 FROM phones WHERE ${ownerMatch(wa, user)}))`;
/**
 * A sender an invite code may bind: a stranger, or (gp-sv2d) a user_id no row holds now and no span
 * holds at the stamp, named beside a number a phone-only row holds. Rule 3 would make that text the
 * phone-only row's message; an invite code makes its sender a newcomer instead, whose attempts are
 * counted as a stranger's and whom the bind keys by the user_id.
 */
const newcomer = (wa: string, user: string, stamp: string) =>
  `(${stranger(wa, user, stamp)} OR (${user} IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE user_id = ${user}) AND NOT EXISTS ${otherHolding(wa, user, stamp)}))`;
/**
 * A newcomer's invite code that was live at the text's stamp and is still unspent after the bind
 * (gp-sv2d): the bind was refused (the day's attempts used up, or ids another owner holds), so the
 * text is no one's message. Rule 3 never gives it to a phone-only row: the other city's code would
 * reach that row's city, and the row would take the sender's user_id. A text with no code (NULL) is
 * never one, nor is a known sender's (an owner's retry under a previous user_id is its message,
 * whatever it says).
 */
const refusedCode = (code: string, at: string, wa: string, user: string, stamp: string) =>
  `(EXISTS (SELECT 1 FROM invite_codes WHERE code = ${code} AND used_at IS NULL AND expires_at > ${at})
     AND ${newcomer(wa, user, stamp)})`;
/**
 * The paired numbers (gp-byp8). Each statement takes a number (?1), the current user_id Meta
 * named beside it (?2) and Meta's stamp (?3).
 *
 * pairingEnds: Meta names the number beside a current user_id at the stamp, so every other
 * standing binding's open span of it ends there, kept for the alias horizon (?4): rule 2 for
 * paired numbers, the number is someone else's now. As in rule 2, a span opened after the stamp (a
 * late retry) or a previous user_id (no one else ever holds it) ends nothing. The user_id's own
 * span stays open when its row takes the number (one binding, held twice), so if rule 2 later
 * strips the row's number, the span still holds what the owner sent until then. A span whose
 * binding an operator unbound is left to the cron: the owner's own retry, landing after the unbind,
 * names a user_id no row holds now, and must not end the span below what the owner sent next.
 *
 * The pairing (paired): the row holding the user_id at the stamp lacks a number, and another row
 * held the number at the stamp, so the row's fill could not take it. Meta's evidence that the
 * number was the binding's at the stamp, kept by the two statements below.
 *
 * pairingWidens: the evidence belongs to the binding's first span of the number that ends at or
 * after the stamp, open or ended. A pairing stamped before that span's start is late (gp-sv2d,
 * late (ii)): the span holds from the stamp, and still routes only from where it did, so the
 * stamps it adds only refuse. A pairing a later span's end already covers (Meta's replay of
 * history) never reopens anything, and a replay of the pairing that opened a span changes nothing.
 *
 * pairingOpens: when no span of the binding's ends at or after the stamp, the number is the row's
 * paired number from here: an open span, from where the owner came to it (wa_since: its number
 * change, when that brought the owner to the number Meta had not shared yet) or this stamp,
 * whichever is earlier, but never before the second after the binding's last ended span of it.
 * It is on-time evidence: it routes from that start.
 *
 * pairingHistoryWidens, pairingHistory: the user_id is a previous one that the binding held at the
 * stamp (Meta delivers the pairing after the owner's number change, at the alias's changed_at).
 * It is late (gp-sv2d, late (i)), so it only refuses. It belongs to the binding's first span of
 * the number that ends at or after the stamp and by that change (what the owner held under that
 * user_id), which holds from the stamp when that is earlier and routes from where it did; an end
 * already recorded there stands (the codex self-review r4). Only when there is none is it kept as
 * a span from the stamp up to the change, for the alias horizon, that never routes. A pairing that
 * lands late is never lost (the lane's self-review r1), never extends a span past its recorded
 * end, and never routes a message (the codex self-review r5).
 *
 * So a binding's open span of a number starts after every ended one, and every statement that
 * ends an open span ends it at or after its start (the codex self-review r1). Each end still merges
 * into a row of the binding's that already ends at that stamp (UPDATE OR REPLACE), keeping the
 * earlier start of each kind (ended), so the merged span holds and routes exactly what the two did,
 * and whatever order Meta delivers in, no end fails a payload on the key (id, bound_wamid,
 * changed_at).
 */
const OPEN = 253_402_300_799; // 9999-12-31T23:59:59Z: a span that has not ended, or a refuse-only span's routes_from
/**
 * What an open span keeps when it ends at `at`: for since and for routes_from, the earlier of its
 * own and that of the binding's span of the number already ending there. Both spans end at `at`,
 * so the earlier start of each is exactly the union of the two: it holds what either held, and
 * routes what either routed, never more (gp-sv2d).
 */
const ended = (at: string) =>
  (["since", "routes_from"] as const)
    .map((col) => `${col} = MIN(${col}, COALESCE((SELECT c.${col} FROM phone_aliases c WHERE c.id = phone_aliases.id AND c.bound_wamid = phone_aliases.bound_wamid AND c.changed_at = ${at}), ${col}))`)
    .join(", ");
const pairingEnds = `UPDATE OR REPLACE phone_aliases SET changed_at = ?3, expires_at = ?4, ${ended("?3")}
   WHERE id = ?1 AND changed_at = ${OPEN} AND since <= ?3
     AND NOT EXISTS (SELECT 1 FROM phone_aliases x WHERE x.id = ?2)
     AND EXISTS (SELECT 1 FROM phones p WHERE p.conversation_id = phone_aliases.conversation_id
                   AND p.bound_wamid = phone_aliases.bound_wamid AND p.user_id IS NOT ?2)`;
const paired = `conversation_id = ${ownerKey("?1", "?2", "?3")} AND NOT EXISTS ${otherHolding("?1", "?2", "?3")}
     AND wa_id IS NULL AND user_id = ?2
     AND EXISTS (SELECT 1 FROM phones o WHERE o.wa_id = ?1 AND COALESCE(o.wa_since, o.bound_stamp, 0) <= ?3)`;
const pairingWidens = `UPDATE phone_aliases SET since = MIN(since, ?3)
   WHERE id = ?1 AND (conversation_id, bound_wamid) IN (SELECT conversation_id, bound_wamid FROM phones WHERE ${paired})
     AND changed_at = (SELECT MIN(h.changed_at) FROM phone_aliases h
                       WHERE h.id = ?1 AND h.bound_wamid = phone_aliases.bound_wamid AND h.changed_at >= ?3)`;
const pairingOpens = `INSERT OR IGNORE INTO phone_aliases (id, conversation_id, bound_wamid, since, changed_at, expires_at, routes_from)
   SELECT ?1, conversation_id, bound_wamid, start, ${OPEN}, ${OPEN}, start FROM (
     SELECT conversation_id, bound_wamid,
            MAX(MIN(COALESCE(wa_since, ?3), ?3),
                COALESCE((SELECT MAX(h.changed_at) + 1 FROM phone_aliases h WHERE h.id = ?1 AND h.bound_wamid = phones.bound_wamid), 0)) AS start
     FROM phones
     WHERE ${paired}
       AND NOT EXISTS (SELECT 1 FROM phone_aliases h WHERE h.id = ?1 AND h.bound_wamid = phones.bound_wamid AND h.changed_at >= ?3))`;
const pairingHistoryWidens = `UPDATE phone_aliases SET since = MIN(since, ?3)
   WHERE id = ?1
     AND changed_at = (SELECT MIN(h.changed_at) FROM phone_aliases h
                       WHERE h.id = ?1 AND h.bound_wamid = phone_aliases.bound_wamid AND h.changed_at >= ?3)
     AND EXISTS (SELECT 1 FROM phone_aliases a WHERE a.id = ?2 AND a.since <= ?3
                   AND a.conversation_id = phone_aliases.conversation_id AND a.bound_wamid = phone_aliases.bound_wamid
                   AND phone_aliases.changed_at <= a.changed_at)`;
const pairingHistory = `INSERT OR IGNORE INTO phone_aliases (id, conversation_id, bound_wamid, since, changed_at, expires_at, routes_from)
   SELECT ?1, conversation_id, bound_wamid, ?3, changed_at, expires_at, ${OPEN} FROM phone_aliases a
   WHERE id = ?2 AND since <= ?3
     AND NOT EXISTS (SELECT 1 FROM phone_aliases h WHERE h.id = ?1 AND h.bound_wamid = a.bound_wamid
                       AND h.changed_at >= ?3 AND h.changed_at <= a.changed_at)`;

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
  /**
   * Statement indexes that count a known sender's message no binding routed: one changed row, or
   * two when two bindings held its stamp (the conflict, counted as its reason).
   */
  unrouted: number[];
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
    const wrote = (index: number) => (index < results.length ? changes(results[index]) : 0);
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
      unrouted: plan.unrouted.filter((i) => wrote(i) >= 1).length,
      unrouted_conflicts: plan.unrouted.filter((i) => wrote(i) === 2).length,
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
      unrouted: [],
      orphanedStatuses: 0,
      stale: 0,
      overCap: false,
    };
    // Meta retries a delivery for up to 7 days (an assumption: README.md, "Business-scoped user
    // ids", rule 5, "The horizon"), and a body-less row dedups a wamid for 8 days from its receipt
    // (never earlier than Meta's stamp). A message stamped longer ago than 8 days could be a copy
    // whose dedup row is gone, so it is dropped and counted, never delivered again, whatever
    // Meta's window is.
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
    // Who may send an invite code: a sender no row holds, or (gp-sv2d) a user_id no row holds beside
    // a number a phone-only row holds, whose text rule 3 would otherwise make that row's. Any other
    // owner's text is its message, whatever it says.
    const mayBind = ids.map((s, i) => owners[i] === undefined || (owners[i]?.user_id === null && s.user !== null));
    const codes = [
      ...new Set(messages.filter((_, i) => mayBind[i]).map(inviteCodeIn).filter((c): c is string => c !== null)),
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
      // The window's clock and the invite's expiry read Meta's stamp clamped to the gateway's
      // clock (at); who held an id reads Meta's own stamps (stamp), all on one clock.
      const at = Math.min(m.timestamp ?? now, now);
      const stamp = m.timestamp ?? now;
      const body = JSON.stringify(m.raw);
      const owned = owners[i] !== undefined;
      const code = mayBind[i] ? inviteCodeIn(m) : null;
      if (wa !== null && user !== null && !m.change) {
        // Meta names this phone number beside this user_id at the stamp: a row that held the
        // number since before then, beside another user_id, holds a number that was someone
        // else's by then (a recycled number), and loses it, so its city's sends never reach the
        // new holder. The pairing is history, and strips nobody, when the row took the number after
        // the stamp (a late retry) or when the user_id is a previous one (a retry of what its owner
        // sent before a number change; no one else ever holds it). A change notice's ids are the
        // ones the owner leaves at its stamp, never a claim on the number; the change below
        // records them. Setting NULL never hits a UNIQUE. The row keeps where it held the number
        // from (wa_since), where its next notice's old number starts (gp-w376).
        push(
          db
            .prepare(
              `UPDATE phones SET wa_id = NULL, wa_since = COALESCE(wa_since, bound_stamp, 0)
               WHERE wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2 AND COALESCE(wa_since, bound_stamp, 0) <= ?3
                 AND NOT EXISTS (SELECT 1 FROM phone_aliases WHERE id = ?2)`,
            )
            .bind(wa, user, stamp),
        );
      }
      if (code !== null) {
        // A newcomer's code attempt (a stranger's, or gp-sv2d's user_id beside a phone-only row's
        // number): counted per day and once per wamid (a redelivery of any text already counted is
        // not a second attempt), then the bind, conditional on a code that was live when Meta
        // stamped the text and on the attempt count, never on this read. The wamid is recorded
        // after the count, so the count reads it unrecorded the first time only.
        push(
          db
            .prepare(
              `INSERT INTO unknown_senders (sender_key, code_attempts, attempts_since)
               SELECT ?1, 1, ?2 WHERE ${newcomer("?5", "?6", "?7")}
               ON CONFLICT (sender_key) DO UPDATE SET
                 code_attempts = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN code_attempts
                                      WHEN attempts_since <= ?3 THEN 1 ELSE code_attempts + 1 END,
                 attempts_since = CASE WHEN EXISTS (SELECT 1 FROM code_attempt_wamids WHERE wamid = ?4) THEN attempts_since
                                       WHEN attempts_since <= ?3 THEN ?2 ELSE attempts_since END`,
            )
            .bind(strangerKey, now, now - DAY, m.wamid, wa, user, stamp),
        );
        push(db.prepare("INSERT OR IGNORE INTO code_attempt_wamids (wamid, counted_at) VALUES (?1, ?2)").bind(m.wamid, now));
        // A new owner row holds every id the text named; no id may already name another owner,
        // now or by another holding (the owner's own text from before a number change is its
        // message, and so is a text from its paired number). Its conversation_id is the wa_id,
        // unless that number is already another owner's conversation_id (a recycled number), else
        // the user_id. It holds its ids from this text's stamp. The rows below find it by the
        // wamid that bound it.
        // A number a phone-only row holds is the one exception (gp-sv2d; the Fable read r2 CRITICAL
        // 2): the text binds by its user_id alone, keyed by it, and the number stays that row's.
        // Meta paired the two at the stamp, so the number is the new owner's paired number from
        // here (pairingOpens below): contested, so the number alone goes to nobody while both
        // stand. A phone-only text (no user_id) is never bound that way: it would hold no id.
        const bindIndex = push(
          db
            .prepare(
              `INSERT INTO phones (conversation_id, wa_id, user_id, username, city_id, profile_name, bound_at, bound_wamid, last_inbound_at, bound_stamp)
               SELECT k.id, k.wa, ?2, ?3, i.city_id, ?4, ?5, ?6, ?7, ?11
               FROM invite_codes i,
                    (SELECT CASE WHEN ?1 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones WHERE conversation_id = ?1 OR wa_id = ?1)
                                 THEN ?1 ELSE ?2 END AS id,
                            CASE WHEN EXISTS (SELECT 1 FROM phones WHERE wa_id = ?1) THEN NULL ELSE ?1 END AS wa) k
               WHERE i.code = ?8 AND i.used_at IS NULL AND i.expires_at > ?7 AND k.id IS NOT NULL
                 AND COALESCE((SELECT code_attempts FROM unknown_senders WHERE sender_key = ?9), 0) <= ?10
                 AND NOT EXISTS (SELECT 1 FROM phones p WHERE p.conversation_id = k.id OR p.user_id = ?2
                                   OR (p.wa_id = ?1 AND (p.user_id IS NOT NULL OR ?2 IS NULL)))
                 AND NOT EXISTS ${otherHolding("?1", "?2", "?11")}
               ON CONFLICT DO NOTHING`,
            )
            .bind(wa, user, m.username, m.profileName, now, m.wamid, at, code, strangerKey, MAX_CODE_ATTEMPTS, stamp),
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
      // The message itself, routed to the binding that held the sender's id when Meta stamped it,
      // as the tables stand inside this transaction. After a bind by this very text its wamid is
      // already the joined row's, so this is one row. A wamid any city already holds is that
      // city's: a retry after the phone moved cities is no new row. An id the message omits is the
      // owner row's (a known phone stays on the delivery when Meta names the owner by user_id
      // alone). A live invite code whose bind was refused is no one's message (gp-sv2d).
      push(
        db
          .prepare(
            `INSERT OR IGNORE INTO deliveries
               (city_id, received_at, kind, wamid, status, wa_id, user_id, username, conversation_id, profile_name, body)
             SELECT city_id, ?1, 'message', ?2, '', COALESCE(?3, wa_id), COALESCE(?4, user_id), ?5, conversation_id, ?6, ?7
             FROM phones WHERE conversation_id = ${routeKey("?3", "?4", "?8")} AND NOT ${refusedCode("?9", "?10", "?3", "?4", "?8")}`,
          )
          .bind(now, m.wamid, wa, user, m.username, m.profileName, body, stamp, code, at),
      );
      // A known sender's message that no binding routes (the owner unbound since, a binding newer
      // than the message, or two bindings holding its stamp) reaches no city and hears nothing; it
      // is counted, in the stats and on the payload's log line. A conflict is counted again as its
      // reason (unrouted_conflicts), by the same statement. A wamid a city already holds (Meta's
      // redelivery of a message delivered before the records changed) is that city's, not counted.
      plan.unrouted.push(push(
        db
          .prepare(
            `INSERT INTO stats (name, value)
             SELECT name, 1 FROM (SELECT 'unrouted_messages' AS name UNION ALL SELECT 'unrouted_conflicts' WHERE ${conflict("?1", "?2", "?3")})
             WHERE ${routeKey("?1", "?2", "?3")} IS NULL AND NOT ${stranger("?1", "?2", "?3")}
               AND NOT EXISTS (SELECT 1 FROM deliveries WHERE wamid = ?4 AND status = '')
             ON CONFLICT (name) DO UPDATE SET value = value + 1`,
          )
          .bind(wa, user, stamp, m.wamid),
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
      if (movesWindow(m.type)) {
        // A message a previous holding or a paired number routes moves the owner's window like any
        // of its messages; it changes none of the owner's ids or names, which are newer than the
        // message or were never shown beside its id.
        push(
          db
            .prepare(
              `UPDATE phones SET last_inbound_at = MAX(COALESCE(last_inbound_at, 0), ?1)
               WHERE conversation_id = ${routeKey("?2", "?3", "?4")} AND EXISTS ${otherHolding("?2", "?3", "?4")}`,
            )
            .bind(at, wa, user, stamp),
        );
      }
      // The owner's window moves on a message from either id. An id the owner row lacks is filled
      // the first time a message carries it beside the other (one owner, two ids, one row), unless
      // another row already holds it; a number filled is held from this message's stamp. A
      // message another holding spans (above: routed by it, or held by two bindings), that no
      // binding routes (no span holds it, or the phone-only row's rule-3 claim is in doubt), or a
      // live invite code whose bind was refused (gp-sv2d), touches no row here.
      push(
        db
          .prepare(
            `UPDATE phones SET
               profile_name = COALESCE(?1, profile_name),
               username = COALESCE(?2, username),
               last_inbound_at = CASE WHEN ?3 THEN MAX(COALESCE(last_inbound_at, 0), ?4) ELSE last_inbound_at END,
               wa_since = CASE WHEN wa_id IS NULL AND ?5 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.wa_id = ?5)
                               THEN ?7 ELSE wa_since END,
               wa_id = CASE WHEN wa_id IS NULL AND ?5 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.wa_id = ?5)
                            THEN ?5 ELSE wa_id END,
               user_id = CASE WHEN user_id IS NULL AND ?6 IS NOT NULL AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = ?6)
                              THEN ?6 ELSE user_id END
             WHERE conversation_id = ${routeKey("?5", "?6", "?7")} AND NOT EXISTS ${otherHolding("?5", "?6", "?7")}
               AND NOT ${refusedCode("?8", "?4", "?5", "?6", "?7")}`,
          )
          .bind(m.profileName, m.username, movesWindow(m.type) ? 1 : 0, at, wa, user, stamp, code),
      );
      if (wa !== null && user !== null && !m.change) {
        // The message names the number beside a user_id (gp-byp8): every other binding's span of
        // it ends here; and if a row still held the number at this stamp, so the fill above could
        // not take it, the pairing opens the user_id row's paired number, or widens the span it
        // belongs to, or, when the user_id is a previous one, is kept on the span the owner's
        // change ended. A widening or a previous user_id's pairing is late evidence: it only
        // refuses (gp-sv2d).
        push(db.prepare(pairingEnds).bind(wa, user, stamp, stamp + ALIAS_SECONDS));
        push(db.prepare(pairingWidens).bind(wa, user, stamp));
        push(db.prepare(pairingOpens).bind(wa, user, stamp));
        push(db.prepare(pairingHistoryWidens).bind(wa, user, stamp));
        push(db.prepare(pairingHistory).bind(wa, user, stamp));
      }
      if (m.change?.userId) {
        // A number change: the owner's ids become the new ones (the old phone number is no longer
        // the owner's, so no new number shared means none held), its conversation_id stays. A row
        // holding the new number beside another user_id loses it first, as above. An id another
        // row still holds is never taken from it.
        // The owner is the binding that holds the notice's old ids now (ownerKey) and that the
        // notice routes to (routeKey): the one that held them at its stamp, alone. A notice Meta
        // redelivers after the re-key, or after the old number found a new holder, names nobody
        // and changes nothing: its old user_id is only a previous holding now, which routes but
        // is no current holder (the codex self-review r3: a replayed first notice must never
        // roll the owner back from its latest ids). So does one whose old number a phone-only row
        // holds while another binding's span of it holds the stamp (gp-byp8: the notice of an
        // owner an operator unbound never re-keys the phone-only row).
        // First, each id the change replaces becomes a previous holding of the owner's binding,
        // for Meta's retries of what the owner sent before it: the ids the row holds, and the old
        // number the notice names (one the row already lost to its next holder, rule 2, was still
        // the owner's until the change). Only when the re-key below takes place (no other row
        // holds the new user_id).
        // A number is held from the stamp that brought the owner to it, never from the bind when
        // it came later, when it may have been anyone's (gp-w376). The row's current number: w,
        // its wa_since (the message that filled it, or the change), else the bind it came with.
        // The old number the notice names when the row holds it no longer: s, the wa_since that
        // rule 2's strip, the fill or a change left on the row. A row with none was never given a
        // number by the records (no message showed one beside the owner's user_id, or each that
        // did came before another row's holding of it), so that number is no previous holding of
        // the owner: it may have been the other row's all along.
        // Before that, the owner's paired numbers end at the change (gp-byp8): each is a previous
        // holding from its start until the change, for the alias horizon, and the notice's old
        // number when it is one of them keeps that start (the insert below ignores a second row).
        // A span that starts after the change (a late notice) is left open: no end precedes a start.
        push(
          db
            .prepare(
              `UPDATE OR REPLACE phone_aliases SET changed_at = ?2, expires_at = ?3, ${ended("?2")}
               WHERE changed_at = ${OPEN} AND since <= ?2 AND (conversation_id, bound_wamid) IN (
                 SELECT conversation_id, bound_wamid FROM phones WHERE conversation_id = ${ownerKey("?4", "?5", "?2")} AND conversation_id = ${routeKey("?4", "?5", "?2")}
                   AND NOT EXISTS (SELECT 1 FROM phones x WHERE x.user_id = ?1 AND x.conversation_id <> phones.conversation_id))`,
            )
            .bind(m.change.userId, stamp, stamp + ALIAS_SECONDS, wa, user),
        );
        push(
          db
            .prepare(
              `WITH o AS (
                 SELECT conversation_id, bound_wamid, wa_id, user_id,
                        COALESCE(bound_stamp, 0) AS b, COALESCE(wa_since, bound_stamp, 0) AS w, wa_since AS s
                 FROM phones WHERE conversation_id = ${ownerKey("?3", "?4", "?5")} AND conversation_id = ${routeKey("?3", "?4", "?5")}
                   AND NOT EXISTS (SELECT 1 FROM phones x WHERE x.user_id = ?1 AND x.conversation_id <> phones.conversation_id))
               INSERT OR IGNORE INTO phone_aliases (id, conversation_id, bound_wamid, since, changed_at, expires_at, routes_from)
               SELECT c.id, o.conversation_id, o.bound_wamid, c.since, ?5, ?6, c.since FROM o, (
                 SELECT wa_id AS id, w AS since FROM o
                 UNION ALL SELECT user_id, b FROM o
                 UNION ALL SELECT ?3, s FROM o WHERE ?3 IS NOT o.wa_id AND s IS NOT NULL
               ) c
               WHERE c.id IS NOT NULL AND c.id IS NOT ?1 AND c.id IS NOT ?2`,
            )
            .bind(m.change.userId, m.change.waId, wa, user, stamp, stamp + ALIAS_SECONDS),
        );
        if (m.change.waId) {
          push(
            db
              .prepare(
                `UPDATE phones SET wa_id = NULL, wa_since = COALESCE(wa_since, bound_stamp, 0)
                 WHERE wa_id = ?1 AND user_id IS NOT NULL AND user_id <> ?2 AND COALESCE(wa_since, bound_stamp, 0) <= ?3`,
              )
              .bind(m.change.waId, m.change.userId, stamp),
          );
        }
        // The re-key writes the change's stamp to wa_since whether or not the row takes the new
        // number (Meta shared none, or another row holds it): whatever number the owner has from
        // here, it has had it only since this stamp, which is where a later notice's old number
        // starts (above).
        push(
          db
            .prepare(
              `UPDATE phones SET
                 user_id = ?1,
                 wa_since = ?5,
                 wa_id = CASE WHEN ?2 IS NOT NULL AND NOT EXISTS
                                (SELECT 1 FROM phones o WHERE o.wa_id = ?2 AND o.conversation_id <> phones.conversation_id)
                              THEN ?2 ELSE NULL END
               WHERE conversation_id = ${ownerKey("?3", "?4", "?5")} AND conversation_id = ${routeKey("?3", "?4", "?5")}
                 AND NOT EXISTS (SELECT 1 FROM phones o WHERE o.user_id = ?1 AND o.conversation_id <> phones.conversation_id)`,
            )
            .bind(m.change.userId, m.change.waId, wa, user, stamp),
        );
        if (m.change.waId) {
          // The change names the new number beside the new user_id (gp-byp8): every other
          // binding's span of it ends here, and if a row (a phone-only owner; the strip above took
          // it from any other) still held it at this stamp, so the re-key gave the owner none, it
          // is the owner's paired number from this stamp. Every span the owner can have of its new
          // number starts at or after this stamp (the new user_id names it from here), so a replay
          // of the notice has nothing to widen, and pairingOpens reopens nothing.
          push(db.prepare(pairingEnds).bind(m.change.waId, m.change.userId, stamp, stamp + ALIAS_SECONDS));
          push(db.prepare(pairingOpens).bind(m.change.waId, m.change.userId, stamp));
        }
      }
      if (!owned && !m.change) {
        // A stranger hears the canned line at most once per 7 days (CANNED_EVERY_SECONDS, the
        // gateway's own cadence, not Meta's retry window: README.md, rule 5, "The horizon"),
        // claimed before it is sent. A number change is Meta's notice, not the user writing: never
        // answered. Only a sender no binding holds now and no span holds at the stamp is a stranger
        // (README.md, rule 6).
        const cannedIndex = push(
          db
            .prepare(
              `INSERT INTO unknown_senders (sender_key, last_replied_at)
               SELECT ?1, ?2 WHERE ${stranger("?4", "?5", "?6")}
               ON CONFLICT (sender_key) DO UPDATE SET last_replied_at = excluded.last_replied_at
               WHERE unknown_senders.last_replied_at IS NULL OR unknown_senders.last_replied_at <= ?3`,
            )
            .bind(strangerKey, now, now - CANNED_EVERY_SECONDS, wa, user, stamp),
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
    // the owner's current id. (gp-0g2s; the mayor's rule R1.)
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
   * aliases past their horizon and counted code wamids Meta can no longer redeliver. A paired
   * number's open span whose binding an operator unbound ends at the cron's clock (gp-byp8): until
   * then it held every later stamp, so nothing the owner sent before the unbind reached the other
   * row's city, and it is kept for the alias horizon from there. One that starts after the clock
   * waits for the next run.
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
      db
        .prepare(
          `UPDATE OR REPLACE phone_aliases SET changed_at = ?1, expires_at = ?2, ${ended("?1")}
           WHERE changed_at = ${OPEN} AND since <= ?1 AND NOT EXISTS (SELECT 1 FROM phones p
             WHERE p.conversation_id = phone_aliases.conversation_id AND p.bound_wamid = phone_aliases.bound_wamid)`,
        )
        .bind(now, now + ALIAS_SECONDS),
    ]);
    const [bodies, rows, mediaRows, strangers, invitesGone, enrollGone, sendsGone, orphansGone, idsLeft, aliasesGone, codeWamidsGone, pairingsEnded] =
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
      unbound_pairings_ended: pairingsEnded ?? 0,
    };
  }

  return { handle, maintain };
}
