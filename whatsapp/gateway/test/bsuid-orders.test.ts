/**
 * The design of record's acceptance tests (whatsapp/docs/design-bsuid-pairing-and-spans.md,
 * section 4, T1 to T8; Afik's yes 06:16:17Z 11/6), the spec of bead gp-lv49. Each row runs its
 * order as Meta sends it (every message names its sender's user id, Meta's shape since early April
 * 2026), and again number-only, where nothing may reach a city or bind. The world, unless a row
 * says otherwise: C holds P in W, with and without a user id; A is in X by a, B in Y by b.
 *
 * Every row is a table. Each case records what it observed, and a row fails on the list of cases
 * whose observation differs from the Expected column, so a failing run names every failing order.
 * The same file runs unchanged at PR 76's draft (e39c0b90) for the RED record.
 */
import { describe, expect, it } from "vitest";
import {
  bsuid,
  changeAs,
  type City,
  flakyDb,
  HOUR,
  imageAs,
  legacyBind,
  numberOnly,
  phone,
  setup,
  T0,
  textAs,
  uniq,
} from "./harness.js";

const T1 = T0 + HOUR;
const T2 = T0 + 2 * HOUR;
const T3 = T0 + 3 * HOUR;
const T4 = T0 + 4 * HOUR;
const T5 = T0 + 5 * HOUR;
const LATER = HOUR / 4;
const CANNED = "This number serves Dynasty cities. Ask your city for an invite code.";
const TEMPLATE = { type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } };

type T = ReturnType<typeof setup>;
type Form = "Meta's shape" | "number-only";
const FORMS: Form[] = ["Meta's shape", "number-only"];
type CKind = "with a user id" | "without a user id";
const C_KINDS: CKind[] = ["with a user id", "without a user id"];

/** One message as the test sends it. */
interface Msg {
  payload: unknown;
  wamid: string;
  ts: number;
  mediaId?: string;
}

const inForm = <P>(form: Form, payload: P): P => (form === "number-only" ? numberOnly(payload) : payload);

function text(form: Form, ids: { wa?: string | null; user?: string | null }, body: string, ts: number): Msg {
  const wamid = `wamid.${uniq()}`;
  return { payload: inForm(form, textAs(ids, body, { wamid, ts })), wamid, ts };
}

function image(t: T, form: Form, ids: { wa?: string | null; user?: string | null }, ts: number): Msg {
  const wamid = `wamid.${uniq()}`;
  const mediaId = `8${ts}${Math.floor(Math.random() * 1_000_000)}`;
  t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
  return { payload: inForm(form, imageAs(ids, mediaId, { wamid, ts })), wamid, ts, mediaId };
}

/** A number-change notice: the owner leaves `from` and `prev` for `newWa` and `next`. */
function notice(form: Form, ids: { from: string | null; prev: string; next: string; newWa: string | null }, ts: number): Msg {
  const wamid = `wamid.${uniq()}`;
  const type = ids.newWa ? "user_changed_number" : "user_changed_user_id";
  const payload = changeAs(type, { from: ids.from, previousUser: ids.prev, newUser: ids.next, newWa: ids.newWa }, { wamid, ts });
  return { payload: inForm(form, payload), wamid, ts };
}

/** Meta's delivery at the gateway's clock `at` (the message's own stamp when omitted). */
async function deliver(t: T, m: Msg, at = m.ts): Promise<number> {
  t.clock.now = at;
  return (await t.webhook(m.payload)).status;
}

/** Meta's first delivery, at the message's stamp, whose D1 write fails: Meta will retry. */
async function failFirst(t: T, flaky: ReturnType<typeof flakyDb>, m: Msg): Promise<number> {
  t.clock.now = m.ts;
  flaky.arm(0);
  return (await t.webhook(m.payload)).status;
}

/** The names of the cities whose inbox holds the message's wamid (as any kind). */
async function where(t: T, cities: Record<string, City>, m: Msg): Promise<string[]> {
  const out: string[] = [];
  for (const [name, city] of Object.entries(cities)) {
    const ids = (await t.inbox(city, "?limit=200")).body.deliveries.map((d: any) => d.message?.id);
    if (ids.includes(m.wamid)) out.push(name);
  }
  return out;
}

/** The names of the cities the media proxy serves the message's media id to. */
async function grants(t: T, cities: Record<string, City>, m: Msg): Promise<string[]> {
  const out: string[] = [];
  for (const [name, city] of Object.entries(cities)) {
    if ((await t.call("GET", `/v1/media/${m.mediaId}`, { token: city.token })).status === 200) out.push(name);
  }
  return out;
}

/** A city's template send to `to`: its status, and where the Graph call it made named its recipient. */
async function sentTo(t: T, city: City, to: string) {
  const calls = t.graph.sends().length;
  const r = await t.send(city, { to, idempotency_key: uniq(), ...TEMPLATE });
  const body = t.graph.sends().length > calls ? t.graph.sends().at(-1)!.body! : null;
  return { status: r.status, recipient: body?.recipient ?? null, to: body?.to ?? null };
}

/** A binding as D1 holds it, or null. */
async function rowOf(t: T, conversationId: string) {
  return t.deps.db
    .prepare("SELECT conversation_id, wa_id, user_id, city_id, bound_wamid FROM phones WHERE conversation_id = ?")
    .bind(conversationId)
    .first();
}

/** The city a user id is bound in now, as its current id, or null. */
async function boundIn(t: T, user: string): Promise<string | null> {
  return (await t.deps.db.prepare("SELECT city_id FROM phones WHERE user_id = ?").bind(user).first<{ city_id: string }>())?.city_id ?? null;
}

async function bindings(t: T): Promise<number> {
  return (await t.deps.db.prepare("SELECT COUNT(*) AS n FROM phones").first<{ n: number }>())?.n ?? 0;
}

/** The world of section 4: C holds P in W, with or without a user id; A is in X by a, B in Y by b. */
async function world(t: T, cKind: CKind) {
  const x = await t.enrollCity("X City");
  const y = await t.enrollCity("Y City");
  const w = await t.enrollCity("W City");
  const p = phone();
  // C's own user id: the one Meta names C by on every message. A row without one (PR 73's binding
  // by number alone) never learned it.
  const c = bsuid();
  if (cKind === "with a user id") await t.joinAs(w, { wa: p, user: c });
  else await legacyBind(t.deps.db, w.id, p);
  // README rule 1 (gp-g02t): a binding is keyed by its user id; PR 73's row by its number.
  const cKey = cKind === "with a user id" ? c : p;
  const cRow = await rowOf(t, cKey);
  // A real row to compare against: C's "unchanged" checks are never null against null.
  expect(cRow).toMatchObject({ conversation_id: cKey, user_id: cKind === "with a user id" ? c : null, city_id: w.id });
  const a = bsuid();
  await t.joinAs(x, { user: a });
  const b = bsuid();
  await t.joinAs(y, { user: b });
  return { x, y, w, p, c, cKey, a, b, cRow, cities: { x, y, w } };
}

function* permutations<V>(items: V[]): Generator<V[]> {
  if (items.length <= 1) {
    yield items;
    return;
  }
  for (const [i, head] of items.entries()) {
    for (const rest of permutations([...items.slice(0, i), ...items.slice(i + 1)])) yield [head, ...rest];
  }
}

/**
 * The cases whose observation is not the expected one, one line each naming only what differs; a
 * row's assertion is that there are none, so a failing run prints every wrong case in full.
 */
class Table {
  readonly wrong: string[] = [];
  check(name: string, observed: unknown, expected: unknown) {
    if (JSON.stringify(observed) === JSON.stringify(expected)) return;
    const isRecord = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v);
    const diff =
      isRecord(observed) && isRecord(expected)
        ? Object.keys(expected)
            .filter((k) => JSON.stringify(observed[k]) !== JSON.stringify(expected[k]))
            .map((k) => `${k} ${JSON.stringify(observed[k])} (expected ${JSON.stringify(expected[k])})`)
            .join("; ")
        : `${JSON.stringify(observed)} (expected ${JSON.stringify(expected)})`;
    this.wrong.push(`${name}: ${diff}`);
  }
  assertNone() {
    expect(this.wrong, `wrong cases:\n${this.wrong.join("\n")}\n`).toEqual([]);
  }
}

describe("T1, round 4b finding 1 (R1, R2, R2b, R6, R7): P paired with a and b in every order, first writes failing", () => {
  for (const cKind of C_KINDS) {
    for (const form of FORMS) {
      it(`C ${cKind}, ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : "a's to X, b's to Y, media likewise"}; X's sends reach a, never P's holder`, async () => {
        const table = new Table();
        for (const order of permutations([0, 1, 2, 3])) {
          const flaky = flakyDb();
          const t = setup({ db: flaky.db });
          const { x, y, p, a, b, cKey, cRow, cities } = await world(t, cKind);
          const before = await bindings(t);
          const events = [
            text(form, { wa: p, user: a }, "A at T1", T1),
            text(form, { wa: p, user: b }, "B at T2", T2),
            image(t, form, { wa: p, user: a }, T3),
            image(t, form, { wa: p, user: b }, T4),
          ];
          const statuses: number[] = [];
          for (const e of events) statuses.push(await failFirst(t, flaky, e));
          for (const i of order) statuses.push(await deliver(t, events[i]!, T5));
          const [a1, b2, a3, b4] = events as [Msg, Msg, Msg, Msg];
          const observed = {
            statuses,
            a1: await where(t, cities, a1),
            b2: await where(t, cities, b2),
            a3: await where(t, cities, a3),
            b4: await where(t, cities, b4),
            a3Media: await grants(t, cities, a3),
            b4Media: await grants(t, cities, b4),
            // A send names a number in `to`, a user id in `recipient` (meta.ts recipientOf).
            xToA: await sentTo(t, x, a),
            // The number P, sent from X: refused, or to a; never to P's holder.
            xToPReachesOnlyA: await sentTo(t, x, p).then((s) => s.status !== 200 || (s.recipient === a && s.to === null)),
            yToB: await sentTo(t, y, b),
            cUnchanged: JSON.stringify(await rowOf(t, cKey)) === JSON.stringify(cRow),
            newBindings: (await bindings(t)) - before,
          };
          const meta = form === "Meta's shape";
          table.check(`order ${order.map((i) => ["A1", "B2", "A3", "B4"][i]).join(" ")}`, observed, {
            statuses: [500, 500, 500, 500, 200, 200, 200, 200],
            a1: meta ? ["x"] : [],
            b2: meta ? ["y"] : [],
            a3: meta ? ["x"] : [],
            b4: meta ? ["y"] : [],
            a3Media: meta ? ["x"] : [],
            b4Media: meta ? ["y"] : [],
            xToA: { status: 200, recipient: a, to: null },
            xToPReachesOnlyA: true,
            yToB: { status: 200, recipient: b, to: null },
            cUnchanged: true,
            newBindings: 0,
          });
        }
        table.assertNone();
      });
    }
  }
});

describe("T2, round 4b finding 2 (R3, R3b, R3c): A on Q, Q re-paired; C's image from P fails; P paired with a; C unbound; the retry", () => {
  const orders = [
    { how: "R3: A bound on (Q, a), a stranger pairs Q at T1; C unbound before the retry", notice: false, cStands: false },
    { how: "R3b: the same with C standing", notice: false, cStands: true },
    { how: "R3c: A's notice at T1 moves A to Q, which phone-only C2 holds in V; C unbound", notice: true, cStands: false },
  ];
  for (const order of orders) {
    for (const cKind of C_KINDS) {
      for (const form of FORMS) {
        it(`${order.how}; C ${cKind}, ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : "never X"}`, async () => {
          const flaky = flakyDb();
          const t = setup({ db: flaky.db });
          const x = await t.enrollCity("X City");
          const w = await t.enrollCity("W City");
          const v = await t.enrollCity("V City");
          const p = phone();
          const q = phone();
          const c = bsuid();
          if (cKind === "with a user id") await t.joinAs(w, { wa: p, user: c });
          else await legacyBind(t.deps.db, w.id, p);
          const a = bsuid();
          let aNow = a;
          const sentInOrder: Msg[] = [];
          if (!order.notice) {
            await t.joinAs(x, { wa: q, user: a });
            const stranger = text(form, { wa: q, user: bsuid() }, "Q is someone else's now", T1);
            expect(await deliver(t, stranger)).toBe(200);
            sentInOrder.push(stranger);
          } else {
            await t.joinAs(x, { user: a });
            await legacyBind(t.deps.db, v.id, q, T0);
            aNow = bsuid();
            const change = notice(form, { from: null, prev: a, next: aNow, newWa: q }, T1);
            expect(await deliver(t, change)).toBe(200);
            sentInOrder.push(change);
          }
          const before = await bindings(t);
          const cImage = image(t, form, { wa: p, user: c }, T2);
          expect(await failFirst(t, flaky, cImage)).toBe(500);
          const pairing = text(form, { wa: p, user: aNow }, "A pairs P", T3);
          expect(await deliver(t, pairing)).toBe(200);
          sentInOrder.push(pairing);
          // The operator unbinds C by an id its binding is known by: P for C's PR 73 row; its user
          // id for C with one, whose binding holds no number (gp-g02t: keyed by its user id).
          if (!order.cStands) await t.unbind(cKind === "with a user id" ? c : p);
          expect(await deliver(t, cImage, T3 + LATER)).toBe(200);
          sentInOrder.push(cImage);
          const cities = { x, w, v };
          const table = new Table();
          if (form === "Meta's shape") {
            table.check("C's image", { in: (await where(t, cities, cImage)).includes("x"), media: (await grants(t, cities, cImage)).includes("x") }, { in: false, media: false });
          } else {
            for (const [i, m] of sentInOrder.entries()) table.check(`message ${i + 1}`, await where(t, cities, m), []);
            table.check("C's image's media", await grants(t, cities, cImage), []);
            table.check("bindings", (await bindings(t)) - before, order.cStands ? 0 : -1);
          }
          table.assertNone();
        });
      }
    }
  }
});

describe("T3, round 4b finding 3 (R4, R4b, R8): a newcomer's code text beside P fails; the code dies; the retry", () => {
  const deaths = ["X re-mints its code", "another user spends the code", "the newcomer binds with X's re-minted code"] as const;
  for (const death of deaths) {
    for (const cKind of C_KINDS) {
      for (const form of FORMS) {
        it(`${death}; C ${cKind}, ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : "the retry reaches no city; C unchanged; the live code binds to X"}`, async () => {
          const flaky = flakyDb();
          const t = setup({ db: flaky.db });
          const { x, p, cKey, cRow, cities } = await world(t, cKind);
          const before = await bindings(t);
          const n = bsuid();
          const k = await t.invite(x);
          const kText = text(form, { wa: p, user: n }, k, T1);
          expect(await failFirst(t, flaky, kText)).toBe(500);
          const table = new Table();
          let live: Msg | null = null;
          t.clock.now = T1 + LATER;
          if (death === "X re-mints its code") {
            await t.invite(x);
          } else if (death === "another user spends the code") {
            expect(await deliver(t, text(form, { wa: phone(), user: bsuid() }, k, T1 + LATER))).toBe(200);
          } else {
            const k2 = await t.invite(x);
            live = text(form, { wa: p, user: n }, k2, T2);
            expect(await deliver(t, live)).toBe(200);
          }
          expect(await deliver(t, kText, T2 + LATER)).toBe(200);
          table.check("the retry", await where(t, cities, kText), []);
          table.check("C after the retry", JSON.stringify(await rowOf(t, cKey)) === JSON.stringify(cRow), true);
          if (!live) {
            t.clock.now = T3;
            live = text(form, { wa: p, user: n }, await t.invite(x), T3);
            expect(await deliver(t, live)).toBe(200);
          }
          const meta = form === "Meta's shape";
          table.check("the live code text", await where(t, cities, live), meta ? ["x"] : []);
          table.check("the newcomer's binding", await boundIn(t, n), meta ? x.id : null);
          table.check("C at the end", JSON.stringify(await rowOf(t, cKey)) === JSON.stringify(cRow), true);
          table.check("new bindings", (await bindings(t)) - before, (meta ? 1 : 0) + (meta && death === "another user spends the code" ? 1 : 0));
          table.assertNone();
        });
      }
    }
  }
});

describe("T4, round 4b finding 4 (R9): the newcomer texts a six-digit typo, then X's live code", () => {
  for (const cKind of C_KINDS) {
    for (const form of FORMS) {
      it(`C ${cKind}, ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : "the typo reaches no city; the code binds to X; C unchanged"}`, async () => {
        const t = setup();
        const { x, p, cKey, cRow, cities } = await world(t, cKind);
        const before = await bindings(t);
        const n = bsuid();
        const k = await t.invite(x);
        const typo = text(form, { wa: p, user: n }, k === "000000" ? "000001" : "000000", T1);
        expect(await deliver(t, typo)).toBe(200);
        const live = text(form, { wa: p, user: n }, k, T2);
        expect(await deliver(t, live)).toBe(200);
        const meta = form === "Meta's shape";
        const table = new Table();
        table.check("the typo", await where(t, cities, typo), []);
        table.check("the live code text", await where(t, cities, live), meta ? ["x"] : []);
        table.check("the newcomer's binding", await boundIn(t, n), meta ? x.id : null);
        table.check("C", JSON.stringify(await rowOf(t, cKey)) === JSON.stringify(cRow), true);
        table.check("new bindings", (await bindings(t)) - before, meta ? 1 : 0);
        table.assertNone();
      });
    }
  }
});

describe("T5, gate r3 (round 4 scope 1): P paired with a (T2); A's image (T3) fails; A's notice (T4); the retry, C standing", () => {
  for (const timing of ["the image's first write fails, retried after the notice", "the image on time"] as const) {
    for (const cKind of C_KINDS) {
      for (const form of FORMS) {
        it(`${timing}; C ${cKind}, ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : "X, with media; never W"}`, async () => {
          const flaky = flakyDb();
          const t = setup({ db: flaky.db });
          const { p, a, cities } = await world(t, cKind);
          const before = await bindings(t);
          const pairing = text(form, { wa: p, user: a }, "A at T2", T2);
          expect(await deliver(t, pairing)).toBe(200);
          const aImage = image(t, form, { wa: p, user: a }, T3);
          if (timing === "the image on time") expect(await deliver(t, aImage)).toBe(200);
          else expect(await failFirst(t, flaky, aImage)).toBe(500);
          const change = notice(form, { from: p, prev: a, next: bsuid(), newWa: phone() }, T4);
          expect(await deliver(t, change)).toBe(200);
          if (timing !== "the image on time") expect(await deliver(t, aImage, T4 + LATER)).toBe(200);
          const meta = form === "Meta's shape";
          const table = new Table();
          table.check("A's image", await where(t, cities, aImage), meta ? ["x"] : []);
          table.check("A's image's media", await grants(t, cities, aImage), meta ? ["x"] : []);
          if (!meta) {
            table.check("the pairing", await where(t, cities, pairing), []);
            table.check("the notice", await where(t, cities, change), []);
          }
          table.check("new bindings", (await bindings(t)) - before, 0);
          table.assertNone();
        });
      }
    }
  }
});

describe("T6, new: a→a2 (T1) fails until a2→a3 (T2) lands; then a→a2 is replayed", () => {
  for (const form of FORMS) {
    it(`${form}: ${form === "number-only" ? "nothing reaches a city or re-keys" : "A answers to a3, and replays change nothing"}`, async () => {
      const flaky = flakyDb();
      const t = setup({ db: flaky.db });
      const x = await t.enrollCity("X City");
      const [p, q, r] = [phone(), phone(), phone()];
      const [a, a2, a3] = [bsuid(), bsuid(), bsuid()];
      await t.joinAs(x, { wa: p, user: a });
      const first = notice(form, { from: p, prev: a, next: a2, newWa: q }, T1);
      expect(await failFirst(t, flaky, first)).toBe(500);
      const second = notice(form, { from: q, prev: a2, next: a3, newWa: r }, T2);
      expect(await deliver(t, second)).toBe(200);
      expect(await deliver(t, first, T2 + LATER)).toBe(200);
      const calls = t.graph.sends().length;
      const m = text(form, { wa: r, user: a3 }, "A under a3", T3);
      expect(await deliver(t, m)).toBe(200);
      const canned = () => t.graph.sends().slice(calls).filter((s) => s.body?.text?.body === CANNED).length;
      const meta = form === "Meta's shape";
      const table = new Table();
      table.check("A's message under a3", await where(t, { x }, m), meta ? ["x"] : []);
      table.check("canned lines", canned(), 0);
      table.check("A's current user id", (await t.window(x)).body.windows.map((o: any) => o.user_id), [meta ? a3 : a]);
      // X's box files A under the id A bound with (README rule 1: its user id then, gp-g02t), as
      // GET /v1/window names it; the send reaches A's current id.
      const aConversation = (await t.window(x)).body.windows[0]?.conversation_id;
      table.check("A's conversation id", aConversation, a);
      table.check("X's send to A's conversation", await sentTo(t, x, aConversation), { status: 200, recipient: meta ? a3 : a, to: null });
      // Meta replays both notices; then A writes again.
      expect(await deliver(t, first, T3 + LATER)).toBe(200);
      expect(await deliver(t, second, T3 + LATER)).toBe(200);
      const again = text(form, { wa: r, user: a3 }, "A under a3 again", T4);
      expect(await deliver(t, again)).toBe(200);
      table.check("after the replays", await where(t, { x }, again), meta ? ["x"] : []);
      table.check("A's current user id after the replays", (await t.window(x)).body.windows.map((o: any) => o.user_id), [meta ? a3 : a]);
      table.check("canned lines after the replays", canned(), 0);
      if (!meta) for (const [name, n] of [["first notice", first], ["second notice", second]] as const) table.check(name, await where(t, { x }, n), []);
      table.assertNone();
    });
  }
});

describe("T7, new: P's pairing with a (T2) fails; A's image (T3) arrives on time; T2 lands", () => {
  for (const cKind of C_KINDS) {
    for (const form of FORMS) {
      it(`C ${cKind}, ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : "X, with media; never W"}`, async () => {
        const flaky = flakyDb();
        const t = setup({ db: flaky.db });
        const { p, a, cities } = await world(t, cKind);
        const before = await bindings(t);
        const pairing = text(form, { wa: p, user: a }, "A at T2", T2);
        expect(await failFirst(t, flaky, pairing)).toBe(500);
        const aImage = image(t, form, { wa: p, user: a }, T3);
        expect(await deliver(t, aImage)).toBe(200);
        expect(await deliver(t, pairing, T3 + LATER)).toBe(200);
        const meta = form === "Meta's shape";
        const table = new Table();
        table.check("A's image", await where(t, cities, aImage), meta ? ["x"] : []);
        table.check("A's image's media", await grants(t, cities, aImage), meta ? ["x"] : []);
        table.check("the pairing", await where(t, cities, pairing), meta ? ["x"] : []);
        table.check("new bindings", (await bindings(t)) - before, 0);
        table.assertNone();
      });
    }
  }
});

describe("T8, new: A's code text for Y fails, before or after A binds to X; A's message to X fails; X unbinds A, or a notice replaces a; both retried", () => {
  for (const when of ["before", "after"] as const) {
    for (const end of ["X unbinds A", "a notice replaces a"] as const) {
      for (const retry of ["the code text first", "the message first"] as const) {
        for (const form of FORMS) {
          it(`the code text ${when} A binds to X; ${end}; ${retry}; ${form}: ${form === "number-only" ? "nothing reaches a city or binds" : `never Y; ${end === "X unbinds A" ? "no city once A is unbound" : "X while A stands"}`}`, async () => {
            const flaky = flakyDb();
            const t = setup({ db: flaky.db });
            const x = await t.enrollCity("X City");
            const y = await t.enrollCity("Y City");
            const p = phone();
            const a = bsuid();
            const ky = await t.invite(y);
            let yText: Msg;
            if (when === "before") {
              yText = text(form, { wa: p, user: a }, ky, T1);
              expect(await failFirst(t, flaky, yText)).toBe(500);
              t.clock.now = T2;
              expect(await deliver(t, text(form, { wa: p, user: a }, await t.invite(x), T2))).toBe(200);
            } else {
              t.clock.now = T1;
              expect(await deliver(t, text(form, { wa: p, user: a }, await t.invite(x), T1))).toBe(200);
              yText = text(form, { wa: p, user: a }, ky, T2);
              expect(await failFirst(t, flaky, yText)).toBe(500);
            }
            const m3 = text(form, { wa: p, user: a }, "for X's mayor only", T3);
            expect(await failFirst(t, flaky, m3)).toBe(500);
            t.clock.now = T3 + 2 * LATER;
            if (end === "X unbinds A") await t.unbind(form === "number-only" ? p : a);
            else expect(await deliver(t, notice(form, { from: p, prev: a, next: bsuid(), newWa: phone() }, T3 + 2 * LATER))).toBe(200);
            const retries = retry === "the code text first" ? [yText, m3] : [m3, yText];
            for (const m of retries) expect(await deliver(t, m, T4)).toBe(200);
            const meta = form === "Meta's shape";
            const stands = end === "a notice replaces a";
            const table = new Table();
            table.check("A's message to X", await where(t, { x, y }, m3), meta && stands ? ["x"] : []);
            table.check("Y holds anything of A's", (await where(t, { y }, yText)).length + (await where(t, { y }, m3)).length, 0);
            table.check("bindings in Y", (await t.deps.db.prepare("SELECT COUNT(*) AS n FROM phones WHERE city_id = ?").bind(y.id).first<{ n: number }>())?.n, 0);
            if (!meta) table.check("bindings of P anywhere", (await t.deps.db.prepare("SELECT COUNT(*) AS n FROM phones WHERE conversation_id = ?1 OR wa_id = ?1").bind(p).first<{ n: number }>())?.n, 0);
            table.assertNone();
          });
        }
      }
    }
  }
});
