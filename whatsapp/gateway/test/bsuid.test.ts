/**
 * Meta's business-scoped user ids (bead gp-ac0u) on a real D1 in workerd with a fake Graph API. An
 * owner is known by a user_id (a BSUID), a wa_id, or both. The shapes are Meta's (test/fixtures/
 * SOURCES.md, "Business-scoped user ids"); the rules are README.md's "Business-scoped user ids".
 */
import { applyD1Migrations, env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import {
  bsuid,
  changeAs,
  type City,
  DAY,
  flakyDb,
  HOUR,
  imageAs,
  legacyBind,
  mediaPayload,
  numberOnly,
  phone,
  setup,
  statusAs,
  statusPayload,
  T0,
  textAs,
  textPayload,
  uniq,
  userOf,
} from "./harness.js";

const CLOSE = DAY - 120;
const CANNED = "This number serves Dynasty cities. Ask your city for an invite code.";

/**
 * The owner rows naming any of these ids, as D1 holds them: by conversation_id, by current
 * user_id, or by the number the owner shows. `shows` is that number: the newest mark of the
 * owner's current user id (gp-lv49: a row holds no number; the mark only picks which binding
 * shows one), or, for a row PR 73 bound by its number alone, that number.
 */
async function owners(...ids: string[]) {
  const { results } = await env.DB.prepare(
    `SELECT conversation_id, shows, user_id, username, city_id FROM (
       SELECT p.*, CASE WHEN p.user_id IS NULL THEN p.wa_id
                        ELSE (SELECT k.number FROM marks k WHERE k.user_id = p.user_id ORDER BY k.stamp DESC, k.number LIMIT 1) END AS shows
       FROM phones p)
     WHERE conversation_id IN (SELECT value FROM json_each(?1)) OR shows IN (SELECT value FROM json_each(?1))
        OR user_id IN (SELECT value FROM json_each(?1))
     ORDER BY bound_at, conversation_id`,
  )
    .bind(JSON.stringify(ids))
    .all();
  return results;
}

/**
 * An owner keyed by its phone number P that also answers to a user id, the shape the 409 rule
 * (rule 7) guards, written straight to D1 with its mark. Since gp-g02t no binding this code makes
 * is keyed by a number (rule 1: the user id); PR 73's rows are, and a test of the rule seeds one.
 */
async function numberKeyed(cityId: string, p: string, user: string, at: number) {
  await env.DB.batch([
    env.DB.prepare(
      "INSERT INTO phones (conversation_id, wa_id, user_id, city_id, bound_at, bound_wamid, bound_stamp) VALUES (?, NULL, ?, ?, ?, ?, ?)",
    ).bind(p, user, cityId, at, `wamid.${uniq()}`, at),
    env.DB.prepare("INSERT INTO marks (number, user_id, stamp) VALUES (?, ?, ?)").bind(p, user, at),
  ]);
}

describe("BSUID: binding and routing", () => {
  it("an invite text carrying only a user_id binds the owner by it, welcomes it at the BSUID in `recipient`, and its messages reach only that city", async () => {
    const t = setup();
    const a = await t.enrollCity("Hou Dynasty");
    const b = await t.enrollCity();
    await t.join(b);
    const user = bsuid();
    const code = await t.invite(a);
    const join = await t.sayAs({ user, username: "taylor.hou" }, code, { name: "Taylor" });
    expect(join.res.status).toBe(200);
    expect(t.graph.sends().filter((c) => c.body?.recipient === user).map((c) => c.body)).toEqual([
      {
        messaging_product: "whatsapp",
        recipient_type: "individual",
        recipient: user,
        type: "text",
        text: { body: "You're connected to Hou Dynasty's mayor. Say hi.", preview_url: false },
      },
    ]);
    const joined = (await t.inbox(a)).body.deliveries;
    expect(joined).toHaveLength(1);
    expect(joined[0]).toMatchObject({ kind: "joined", wa_id: null, user_id: user, username: "taylor.hou", conversation_id: user, profile_name: "Taylor" });
    expect(joined[0].message.id).toBe(join.wamid);

    const next = await t.sayAs({ user }, "a username owner writes");
    expect(next.res.status).toBe(200);
    const msg = (await t.inbox(a)).body.deliveries.find((d: any) => d.message?.id === next.wamid);
    expect(msg).toMatchObject({ kind: "message", wa_id: null, user_id: user, username: null, conversation_id: user });
    expect(msg.message.text.body).toBe("a username owner writes");
    expect((await t.inbox(b)).body.deliveries.some((d: any) => d.message?.id === next.wamid)).toBe(false);
    expect(await owners(user)).toEqual([{ conversation_id: user, shows: null, user_id: user, username: "taylor.hou", city_id: a.id }]);
    expect((await t.window(a)).body.windows).toEqual([{ conversation_id: user, user_id: user, last_inbound_at: T0, window_closes_at: T0 + CLOSE }]);
  });

  it("an owner bound with both ids is one row keyed by its user id (gp-g02t), welcomed at its user id; a later message carrying only its user_id routes to it and moves its window", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    // Every send goes to a user id, the welcome included (design note, section 2, point 3).
    const welcome = t.graph.sends().at(-1)!.body!;
    expect(welcome.recipient).toBe(user);
    expect("to" in welcome).toBe(false);
    t.clock.now = T0 + 2 * HOUR;
    const { wamid } = await t.sayAs({ user }, "no phone this time");
    const d = (await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === wamid);
    // The delivery carries the number the owner shows (its mark) although Meta omitted it: a box
    // keyed by wa_id (the adapter at PR 74) still files it.
    expect(d).toMatchObject({ kind: "message", wa_id: wa, user_id: user, conversation_id: user });
    expect(d.message.from).toBeUndefined(); // Meta's object exactly as received
    expect(await owners(wa, user)).toEqual([{ conversation_id: user, shows: wa, user_id: user, username: null, city_id: city.id }]);
    expect((await t.window(city)).body.windows).toEqual([
      { conversation_id: user, user_id: user, last_inbound_at: T0 + 2 * HOUR, window_closes_at: T0 + 2 * HOUR + CLOSE },
    ]);
  });

  it("an owner PR 73 bound by its number alone never adopts a user id: a message carrying both ids is a stranger's, one naming the number alone reaches no city, and the row never changes (no adoption, point 5)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    await legacyBind(env.DB, city.id, wa);
    const row = await owners(wa);
    expect(row).toEqual([{ conversation_id: wa, shows: wa, user_id: null, username: null, city_id: city.id }]);
    const user = bsuid();
    const both = await t.sayAs({ wa, user, username: "owner.one" }, "both ids");
    const legacy = await t.webhook(numberOnly(textPayload(wa, "the number alone", { wamid: `wamid.${uniq()}`, ts: t.clock.now })));
    expect(legacy.status).toBe(200);
    expect((await t.inbox(city)).body.deliveries).toEqual([]);
    expect(await owners(wa)).toEqual(row);
    expect(await owners(user)).toEqual([]);
    // The user id is a stranger's: it hears the canned line there, and can join by a code.
    expect(t.graph.sends().map((c) => [c.body?.recipient, c.body?.text?.body])).toEqual([[user, CANNED]]);
    expect(both.res.status).toBe(200);
  });

  it("the window moves on a message under the owner's user id, whether or not it names the number; a number-only message moves nothing; every send reaches the user id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    t.clock.now = T0 + DAY + HOUR;
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "closed" })).status).toBe(422);
    await t.sayAs({ user }, "back");
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "by phone" })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: user, text: { body: "by phone" } });
    t.clock.now += DAY + HOUR;
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "closed again" })).status).toBe(422);
    expect((await t.webhook(numberOnly(textPayload(wa, "the number alone", { wamid: `wamid.${uniq()}`, ts: t.clock.now })))).status).toBe(200);
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "still closed" })).status).toBe(422);
    await t.sayAs({ wa, user }, "both ids");
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "by BSUID" })).status).toBe(200);
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(t.clock.now);
  });

  it("a message from an owner's phone carrying another user_id is someone else's (a recycled number): never routed, it hears the canned line at its user id, the owner stops showing the number and is sent to by its user_id, and the newcomer can join", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const other = await t.enrollCity("Other City");
    const wa = phone();
    const owner = bsuid();
    const newcomer = bsuid();
    await t.joinAs(city, { wa, user: owner });
    const { wamid } = await t.sayAs({ wa, user: newcomer }, "is this the pizza place?");
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
    expect(t.graph.sends().filter((c) => c.body?.recipient === newcomer && c.body?.text?.body === CANNED)).toHaveLength(1);
    expect(await owners(wa, owner)).toEqual([{ conversation_id: owner, shows: null, user_id: owner, username: null, city_id: city.id }]);
    // The owner's city still writes to the conversation it holds (its user id, gp-g02t); it reaches
    // the owner, never the number.
    expect((await t.send(city, { to: owner, idempotency_key: uniq(), type: "text", text: "for the owner" })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: owner, text: { body: "for the owner" } });
    // The newcomer joins another city; like every binding, it is keyed by its user_id (gp-g02t),
    // and welcomed at it.
    await t.joinAs(other, { wa, user: newcomer });
    expect(await owners(newcomer)).toEqual([{ conversation_id: newcomer, shows: wa, user_id: newcomer, username: null, city_id: other.id }]);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: newcomer, text: { body: "You're connected to Other City's mayor. Say hi." } });
    expect((await t.inbox(other)).body.deliveries.map((d: any) => [d.kind, d.conversation_id])).toEqual([["joined", newcomer]]);
  });

  it("gp-g02t (M3): a new binding is keyed by its user id, never the number: a PR 73 owner A keyed by P is unbound, then B binds on P: B is keyed by its user id and its messages never land in A's conversation P; a new owner showing a number is keyed by its user id too", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    await legacyBind(env.DB, city.id, wa); // A, keyed by P (PR 73)
    expect((await t.unbind(wa)).route).toBe("admin");
    t.clock.now += HOUR;
    const b = bsuid();
    await t.joinAs(city, { wa, user: b });
    const { wamid } = await t.sayAs({ wa, user: b }, "B, on A's old number");
    t.clock.now += HOUR;
    const wa2 = phone();
    const c = bsuid();
    await t.joinAs(city, { wa: wa2, user: c });
    expect(await owners(wa, b, wa2, c)).toEqual([
      { conversation_id: b, shows: wa, user_id: b, username: null, city_id: city.id },
      { conversation_id: c, shows: wa2, user_id: c, username: null, city_id: city.id },
    ]);
    const filed = (await t.inbox(city)).body.deliveries.map((d: any) => [d.kind, d.conversation_id]);
    expect(filed).toEqual([["joined", b], ["message", b], ["joined", c]]);
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid)).toMatchObject({ wa_id: wa, user_id: b, conversation_id: b });
    expect((await t.window(city)).body.windows.map((w: any) => w.conversation_id).sort()).toEqual([b, c].sort());
  });

  it("one person bound twice (once by PR 73, by phone alone; once by BSUID): a message carrying both ids goes to the user_id's row, takes no id from the other, and Meta gets 200", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const wa = phone();
    await legacyBind(env.DB, a.id, wa);
    const user = bsuid();
    await t.joinAs(b, { user });
    const { res, wamid } = await t.sayAs({ wa, user }, "which city hears this?");
    expect(res.status).toBe(200);
    expect((await t.inbox(b)).body.deliveries.find((d: any) => d.message?.id === wamid)).toMatchObject({ conversation_id: user });
    expect((await t.inbox(a)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
    // The PR 73 row keeps its number; the message's pairing makes the number the BSUID binding's
    // to show as well (the mark: display only, never a route).
    expect(await owners(wa, user)).toEqual([
      { conversation_id: wa, shows: wa, user_id: null, username: null, city_id: a.id },
      { conversation_id: user, shows: wa, user_id: user, username: null, city_id: b.id },
    ]);
  });

  it("an image from an owner known only by user_id is delivered, and its media is served to that city alone", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const other = await t.enrollCity();
    const user = bsuid();
    await t.joinAs(city, { user });
    const mediaId = `9${Date.now()}${Math.floor(Math.random() * 1000)}`;
    t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
    const wamid = `wamid.${uniq()}`;
    const p = mediaPayload("image", "unused", mediaId, { wamid, ts: t.clock.now });
    const value = p.entry[0].changes[0].value;
    delete value.contacts[0].wa_id;
    value.contacts[0].user_id = user;
    delete value.messages[0].from;
    value.messages[0].from_user_id = user;
    expect((await t.webhook(p)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid)).toMatchObject({ wa_id: null, user_id: user, conversation_id: user });
    const mine = await t.gateway.handle(new Request(`https://whatsapp.test/v1/media/${mediaId}`, { headers: { authorization: `Bearer ${city.token}` } }));
    expect(mine.status).toBe(200);
    expect(new Uint8Array(await mine.arrayBuffer())).toEqual(new Uint8Array([255, 216, 255]));
    expect((await t.call("GET", `/v1/media/${mediaId}`, { token: other.token })).status).toBe(404);
  });

  it("a delivery the v1 Worker wrote after migration 0002 (no conversation_id) reads its wa_id as its conversation_id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const wamid = `wamid.${uniq()}`;
    await env.DB.prepare(
      "INSERT INTO deliveries (city_id, received_at, kind, wamid, status, wa_id, profile_name, body) VALUES (?, ?, 'message', ?, '', ?, 'Ana', ?)",
    )
      .bind(city.id, t.clock.now, wamid, wa, JSON.stringify({ id: wamid, type: "text", text: { body: "v1" } }))
      .run();
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid)).toMatchObject({ wa_id: wa, user_id: null, conversation_id: wa });
  });

  it("a message naming neither a wa_id nor a user_id is skipped, and Meta still gets 200", async () => {
    const t = setup();
    const city = await t.enrollCity();
    await t.join(city);
    const before = (await t.held(city)).length;
    const calls = t.graph.sends().length;
    const r = await t.webhook(textAs({}, "from nobody", { wamid: `wamid.${uniq()}`, ts: t.clock.now }));
    expect(r.status).toBe(200);
    expect((await t.held(city)).length).toBe(before);
    expect(t.graph.sends()).toHaveLength(calls);
  });
});

describe("BSUID: sends", () => {
  it("a send to a BSUID reaches Meta in `recipient`, byte for byte and with no `to`; so does a send naming the number the owner shows (point 3)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const target = (await t.held(city)).find((d) => d.kind === "joined")!.wamid;
    const bodies = [
      { type: "text", text: "hello by BSUID" },
      { type: "reaction", reaction: { message_id: target, emoji: "\u{1F44D}" } },
      { type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } },
    ];
    for (const body of bodies) {
      const r = await t.send(city, { to: user, idempotency_key: uniq(), ...body });
      expect(r.status).toBe(200);
      const sent = t.graph.sends().at(-1)!.body!;
      expect(sent).toMatchObject({ messaging_product: "whatsapp", recipient_type: "individual", recipient: user, type: body.type });
      expect("to" in sent).toBe(false);
    }
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "by phone" })).status).toBe(200);
    const byPhone = t.graph.sends().at(-1)!.body!;
    expect(byPhone.recipient).toBe(user);
    expect("to" in byPhone).toBe(false);
  });

  it("a malformed BSUID is 400 without a Graph call; a well-formed one that is no owner of this city is 403", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const mine = bsuid();
    const theirs = bsuid();
    await t.joinAs(a, { user: mine });
    await t.joinAs(b, { user: theirs });
    const calls = t.graph.sends().length;
    for (const to of [
      `us.${mine.slice(3)}`,
      mine.replace(".", ""),
      "US.",
      `US.${"1".repeat(129)}`,
      "US.1349-1208",
      "USA.1349",
      "U.1349",
      "US..1349",
      ` ${mine}`,
      `${mine} `,
      "US.ENT.",
      `${mine}!`,
    ]) {
      const r = await t.send(a, { to, idempotency_key: uniq(), type: "text", text: "x" });
      expect([to, r.status, r.body]).toEqual([to, 400, { error: "bad_request" }]);
    }
    for (const to of [theirs, bsuid(), "US.ENT.11815799212886844830", `US.${"9".repeat(128)}`]) {
      const r = await t.send(a, { to, idempotency_key: uniq(), type: "text", text: "x" });
      expect([to, r.status, r.body]).toEqual([to, 403, { error: "not_bound" }]);
    }
    expect(t.graph.sends()).toHaveLength(calls);
    expect((await t.send(a, { to: mine, idempotency_key: uniq(), type: "text", text: "mine" })).status).toBe(200);
  });

  it("a `to` naming two owners of the city (one's retained key, the other's current number) answers 409 ambiguous_recipient and reaches neither; each owner's current ids reach that owner alone (gp-0g2s, the mayor's R1)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    // A keyed by P (numberKeyed: the shape the rule guards), then A changes number to Q.
    const p = phone();
    const a = bsuid();
    await numberKeyed(city.id, p, a, t.clock.now);
    const q = phone();
    const a2 = bsuid();
    expect((await t.webhook(changeAs("user_changed_number", { from: p, previousUser: a, newUser: a2, newWa: q }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    const template = { type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } };
    // A send to A's key before the number has a new holder: one owner, sent to A's current user id.
    const early = uniq();
    const first = await t.send(city, { to: p, idempotency_key: early, ...template });
    expect(first.status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: a2 });
    // B joins the same city on the recycled P; P is A's key, so B's key is B's BSUID.
    const b = bsuid();
    await t.joinAs(city, { wa: p, user: b });
    expect(await owners(p, q, a2, b)).toEqual([
      { conversation_id: p, shows: q, user_id: a2, username: null, city_id: city.id },
      { conversation_id: b, shows: p, user_id: b, username: null, city_id: city.id },
    ]);
    const calls = t.graph.sends().length;
    // A repeat of a key Meta already took answers its first answer (the contract's "the SAME answer
    // again"): it makes no new call, so there is nothing to route.
    expect(await t.send(city, { to: p, idempotency_key: early, ...template })).toMatchObject({ status: 200, body: { message_id: first.body.message_id } });
    expect(t.graph.sends()).toHaveLength(calls);
    const key = uniq();
    const r = await t.send(city, { to: p, idempotency_key: key, ...template });
    // The answer names the owner the box files under `to` (its conversation_id) by its current
    // user_id, so the box resends at once (gp-w376; the Fable read r1 MINOR 2). It names nothing
    // else: not the number, not the other owner, not the owner's ids from before its change.
    expect([r.status, r.body]).toEqual([409, { error: "ambiguous_recipient", hint: "send to the owner's current id", owner_user_id: a2 }]);
    expect(t.graph.sends()).toHaveLength(calls);
    for (const needle of [p, q, a, b]) expect(JSON.stringify(r.body)).not.toContain(needle);
    expect(t.logs).toContainEqual({ event: "send_refused", reason: "ambiguous_recipient" });
    // Where the box finds A's current id: GET /v1/window. A's newest delivery is the notice, which
    // still carries the old one (self-review r1 S6; README rule 7).
    expect((await t.window(city)).body.windows.find((w: any) => w.conversation_id === p).user_id).toBe(a2);
    expect((await t.inbox(city)).body.deliveries.filter((d: any) => d.conversation_id === p).at(-1)).toMatchObject({ user_id: a });
    // The refusal spends no key: the same key sent to B's own id goes, and reaches B alone.
    expect((await t.send(city, { to: b, idempotency_key: key, ...template })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: b, type: "template" });
    expect("to" in t.graph.sends().at(-1)!.body!).toBe(false);
    expect((await t.send(city, { to: q, idempotency_key: uniq(), ...template })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: a2, type: "template" });
    expect((await t.send(city, { to: a2, idempotency_key: uniq(), ...template })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: a2, type: "template" });
    // Nothing went to a number at all.
    expect(t.graph.sends().slice(calls).some((c) => "to" in c.body!)).toBe(false);
    for (const line of t.logs.map((e) => JSON.stringify(e))) for (const needle of [p, q, a, a2, b]) expect(line).not.toContain(needle);
  });

  it("the 409's owner_user_id is the owner `to` keys whatever order D1 reads the two rows in (gp-w376; the self-review's NIT)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const p = phone();
    const q = phone();
    const a2 = bsuid();
    const b = bsuid();
    // The recycled-number pair, written straight to D1 with B's row first: B shows P now (P's
    // newest pairing is with b), and A (keyed by P, its number at its bind) shows Q.
    const row = env.DB.prepare(
      "INSERT INTO phones (conversation_id, wa_id, user_id, city_id, bound_at, bound_wamid, bound_stamp) VALUES (?, NULL, ?, ?, ?, ?, ?)",
    );
    const mark = env.DB.prepare("INSERT INTO marks (number, user_id, stamp) VALUES (?, ?, ?)");
    await env.DB.batch([
      row.bind(b, b, city.id, T0, `wamid.${uniq()}`, T0),
      row.bind(p, a2, city.id, T0, `wamid.${uniq()}`, T0),
      mark.bind(p, b, T0),
      mark.bind(q, a2, T0),
    ]);
    const r = await t.send(city, {
      to: p,
      idempotency_key: uniq(),
      type: "template",
      template: { name: "dynasty_update", language: "en_US", params: ["x"] },
    });
    expect([r.status, r.body]).toEqual([409, { error: "ambiguous_recipient", hint: "send to the owner's current id", owner_user_id: a2 }]);
    expect(t.graph.sends()).toHaveLength(0);
  });

  it("the recycled number in ANOTHER city is no ambiguity: each city's `to: P` names its own one owner (gp-0g2s)", async () => {
    const t = setup();
    const first = await t.enrollCity();
    const second = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await numberKeyed(first.id, p, a, t.clock.now); // A keyed by P (the shape the rule guards)
    const q = phone();
    expect((await t.webhook(changeAs("user_changed_number", { from: p, previousUser: a, newUser: bsuid(), newWa: q }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    const b = bsuid();
    await t.joinAs(second, { wa: p, user: b });
    const template = { type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } };
    const a2 = (await owners(p)).find((o: any) => o.city_id === first.id)!.user_id;
    expect((await t.send(first, { to: p, idempotency_key: uniq(), ...template })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: a2 }); // the retained key goes to A's current user id
    expect((await t.send(second, { to: p, idempotency_key: uniq(), ...template })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: b }); // the number B shows: to B's user id
  });

  it("Meta's refusal of a BSUID recipient (131062: an authentication template) reads meta_error and frees the key", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const user = bsuid();
    await t.joinAs(city, { user });
    const key = uniq();
    t.graph.failNext({ status: 400, body: { error: { code: 131062, message: "Business-scoped User ID (BSUID) recipients are not supported for this message." } } });
    const r = await t.send(city, { to: user, idempotency_key: key, type: "template", template: { name: "login_code", language: "en_US", params: ["123456"] } });
    expect(r.status).toBe(502);
    expect(r.body).toEqual({ error: "meta_error", meta_status: 400, meta_code: 131062 });
    const again = await t.send(city, { to: user, idempotency_key: key, type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } });
    expect(again.status).toBe(200);
  });
});

describe("BSUID: strangers", () => {
  it("a stranger is known by its user_id: one canned line in `recipient`, and none again within 7 days when a later message also carries its phone", async () => {
    const t = setup();
    const user = bsuid();
    const wa = phone();
    const toStranger = () => t.graph.sends().filter((c) => c.body?.recipient === user || c.body?.to === wa);
    await t.sayAs({ user }, "hello?");
    expect(toStranger().map((c) => c.body)).toEqual([
      { messaging_product: "whatsapp", recipient_type: "individual", recipient: user, type: "text", text: { body: CANNED, preview_url: false } },
    ]);
    await t.sayAs({ wa, user }, "still me, now with a phone");
    t.clock.now += 6 * DAY;
    await t.sayAs({ user }, "six days on");
    expect(toStranger()).toHaveLength(1);
    t.clock.now += DAY;
    await t.sayAs({ wa, user }, "a week later");
    expect(toStranger()).toHaveLength(2);
    expect(toStranger().at(-1)!.body).toMatchObject({ recipient: user, text: { body: CANNED } });
  });

  it("five wrong codes from one user_id stop its sixth, the right one included, whichever ids each text carries", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const user = bsuid();
    const wa = phone();
    for (let i = 0; i < 5; i++) {
      const wrong = String((Number(code) + 1 + i) % 1_000_000).padStart(6, "0");
      await t.sayAs(i % 2 ? { wa, user } : { user }, wrong);
    }
    await t.sayAs({ wa, user }, code);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(0);
    expect(await owners(wa, user)).toEqual([]);
  });
});

describe("BSUID: statuses", () => {
  it("statuses of a send to the BSUID and of a send to the phone reach the city with the owner's conversation_id; a raced one is adopted with its user_id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const byUser = (await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "to the BSUID" })).body.message_id;
    expect((await t.webhook(statusAs(byUser, "delivered", { user }, t.clock.now))).status).toBe(200);
    const byPhone = (await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "to the phone" })).body.message_id;
    expect((await t.webhook(statusAs(byPhone, "read", { wa, user }, t.clock.now))).status).toBe(200);
    // Today's failed shape: a recipient_id and no recipient_user_id (Meta omits it for a send to a phone).
    expect((await t.webhook(statusPayload(byPhone, "failed", wa, t.clock.now))).status).toBe(200);
    t.graph.hooks.onSend = async (w) => {
      t.graph.hooks.onSend = null;
      expect((await t.webhook(statusAs(w, "sent", { user }, t.clock.now))).status).toBe(200);
    };
    const raced = (await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "raced" })).body.message_id;
    const statuses = (await t.inbox(city)).body.deliveries.filter((d: any) => d.kind === "status");
    // The username is the status's contacts entry's (Meta's example user, "pablomorales"); today's
    // status shape carries no contacts.
    expect(statuses.map((d: any) => [d.message.id, d.message.status, d.wa_id, d.user_id, d.conversation_id, d.username, d.profile_name])).toEqual([
      [byUser, "delivered", null, user, user, "pablomorales", null],
      [byPhone, "read", wa, user, user, "pablomorales", null],
      [byPhone, "failed", wa, null, user, null, null],
      [raced, "sent", null, user, user, "pablomorales", null],
    ]);
  });
});

describe("BSUID: a number change", () => {
  it("user_changed_number reaches the owner's city as a system message, moves no window, re-keys the owner, and its next message by the new ids keeps the conversation", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const other = await t.enrollCity();
    await t.join(other);
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const newWa = phone();
    const newUser = bsuid();
    t.clock.now = T0 + HOUR;
    const calls = t.graph.sends().length;
    const wamid = `wamid.${uniq()}`;
    const r = await t.webhook(changeAs("user_changed_number", { from: wa, previousUser: user, newUser, newWa }, { wamid, ts: t.clock.now }));
    expect(r.status).toBe(200);
    const notice = (await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid);
    expect(notice).toMatchObject({ kind: "message", wa_id: wa, user_id: user, conversation_id: user });
    expect(notice.message).toMatchObject({
      type: "system",
      system: { type: "user_changed_number", user_id: newUser, previous_user_id: user, wa_id: newWa },
    });
    expect((await t.inbox(other)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
    expect(t.graph.sends()).toHaveLength(calls); // no canned line and no welcome
    expect((await t.window(city)).body.windows).toEqual([{ conversation_id: user, user_id: newUser, last_inbound_at: T0, window_closes_at: T0 + CLOSE }]);
    expect(await owners(wa, newWa, user, newUser)).toEqual([{ conversation_id: user, shows: newWa, user_id: newUser, username: null, city_id: city.id }]);

    t.clock.now = T0 + 2 * HOUR;
    const next = await t.sayAs({ wa: newWa, user: newUser }, "my new phone");
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === next.wamid)).toMatchObject({
      wa_id: newWa,
      user_id: newUser,
      conversation_id: user,
    });
    // The box holds the owner's conversation id (its user id at the bind, gp-g02t), which the change
    // left as it was: the send goes to the owner's current user id.
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "got it" })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: newUser, text: { body: "got it" } });
    expect(t.graph.sends().some((c) => "to" in c.body! && c.body?.text?.body === "got it")).toBe(false);
    // The old number alone is a stranger now: it may be someone else's.
    const old = await t.say(wa, "hello from the old number");
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === old.wamid)).toBe(false);
  });

  it("user_changed_user_id, with no phone shared, routes by the previous user_id and re-keys; a send to the old key goes to the new BSUID; an unknown user's notice is dropped silently", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const user = bsuid();
    const newUser = bsuid();
    await t.joinAs(city, { user });
    const wamid = `wamid.${uniq()}`;
    expect((await t.webhook(changeAs("user_changed_user_id", { previousUser: user, newUser }, { wamid, ts: t.clock.now }))).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid)).toMatchObject({
      kind: "message",
      wa_id: null,
      user_id: user,
      conversation_id: user,
    });
    expect(await owners(user, newUser)).toEqual([{ conversation_id: user, shows: null, user_id: newUser, username: null, city_id: city.id }]);
    const next = await t.sayAs({ user: newUser }, "same me");
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === next.wamid)).toMatchObject({
      user_id: newUser,
      conversation_id: user,
    });
    for (const to of [user, newUser]) {
      expect((await t.send(city, { to, idempotency_key: uniq(), type: "text", text: "ok" })).status).toBe(200);
      expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: newUser });
    }
    const calls = t.graph.sends().length;
    const stray = await t.webhook(changeAs("user_changed_user_id", { previousUser: bsuid(), newUser: bsuid() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }));
    expect(stray.status).toBe(200);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a notice Meta delivers twice is one delivery and one re-key; a notice and the owner's first message under the new ids in one payload reach the owner's conversation with no canned line", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const newWa = phone();
    const newUser = bsuid();
    const wamid = `wamid.${uniq()}`;
    const notice = changeAs("user_changed_number", { from: wa, previousUser: user, newUser, newWa }, { wamid, ts: t.clock.now });
    expect((await t.webhook(notice)).status).toBe(200);
    expect((await t.webhook(notice)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.filter((d: any) => d.message?.id === wamid)).toHaveLength(1);
    expect(await owners(wa, newWa, user, newUser)).toEqual([{ conversation_id: user, shows: newWa, user_id: newUser, username: null, city_id: city.id }]);

    // A second owner changes number; Meta's payload carries the notice and the first message after it.
    const wa2 = phone();
    const user2 = bsuid();
    await t.joinAs(city, { wa: wa2, user: user2 });
    const newWa2 = phone();
    const newUser2 = bsuid();
    const both = changeAs("user_changed_number", { from: wa2, previousUser: user2, newUser: newUser2, newWa: newWa2 }, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const textWamid = `wamid.${uniq()}`;
    const text = textAs({ wa: newWa2, user: newUser2 }, "my new phone", { wamid: textWamid, ts: t.clock.now }).entry[0].changes[0].value;
    both.entry[0].changes[0].value.contacts = text.contacts;
    both.entry[0].changes[0].value.messages.push(text.messages[0]);
    const calls = t.graph.sends().length;
    expect((await t.webhook(both)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === textWamid)).toMatchObject({ user_id: newUser2, conversation_id: user2 });
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a number change never makes a payload fail: a new number another owner holds beside its own user_id moves to the changer; a new user_id another owner holds, or a new number an owner holds with no user_id, is never taken", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const notice = (from: string | null, previousUser: string, newUser: string, newWa: string | null) =>
      t.webhook(changeAs("user_changed_number", { from, previousUser, newUser, newWa }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }));

    // A's new number is B's old one (a recycled number): A gets it, B keeps its user_id and no number.
    const a = { wa: phone(), user: bsuid() };
    const b = { wa: phone(), user: bsuid() };
    await t.joinAs(city, a);
    await t.joinAs(city, b);
    const aUser = bsuid();
    expect((await notice(a.wa, a.user, aUser, b.wa)).status).toBe(200);
    expect(await owners(aUser)).toEqual([{ conversation_id: a.user, shows: b.wa, user_id: aUser, username: null, city_id: city.id }]);
    expect(await owners(b.user)).toEqual([{ conversation_id: b.user, shows: null, user_id: b.user, username: null, city_id: city.id }]);

    // C's new user_id is D's (Meta never reuses one; the guard holds anyway): no binding changes.
    // The notice still moves the marks (point 4: display only): C's old number shows nobody, the
    // new number shows D's id.
    const c = { wa: phone(), user: bsuid() };
    const d = bsuid();
    await t.joinAs(city, c);
    await t.joinAs(city, { user: d });
    const cNew = phone();
    expect((await notice(c.wa, c.user, d, cNew)).status).toBe(200);
    expect(await owners(c.user)).toEqual([{ conversation_id: c.user, shows: null, user_id: c.user, username: null, city_id: city.id }]);
    expect(await owners(d)).toEqual([{ conversation_id: d, shows: cNew, user_id: d, username: null, city_id: city.id }]);
    // The link to an id another binding answers to links nothing (one id, one binding).
    expect(await env.DB.prepare("SELECT COUNT(*) AS n FROM links WHERE old_id = ?").bind(c.user).first()).toEqual({ n: 0 });

    // E's new number is F's, bound by PR 73 before Meta sent user ids: F's row is never touched;
    // E shows the number (Meta's newest pairing of it), which routes nothing.
    const e = { wa: phone(), user: bsuid() };
    await t.joinAs(city, e);
    const f = phone();
    await legacyBind(env.DB, city.id, f);
    const eUser = bsuid();
    expect((await notice(e.wa, e.user, eUser, f)).status).toBe(200);
    expect(await owners(eUser)).toEqual([{ conversation_id: e.user, shows: f, user_id: eUser, username: null, city_id: city.id }]);
    expect(await owners(f)).toContainEqual({ conversation_id: f, shows: f, user_id: null, username: null, city_id: city.id });
  });

  it("an owner known by phone and BSUID whose number changes with no phone shared keeps no phone: its old number is never sent to", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    const newUser = bsuid();
    await t.joinAs(city, { wa, user });
    await t.webhook(changeAs("user_changed_user_id", { previousUser: user, newUser }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }));
    expect(await owners(wa, user, newUser)).toEqual([{ conversation_id: user, shows: null, user_id: newUser, username: null, city_id: city.id }]);
    // The old number names no owner now (its mark went with the change); the conversation does.
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "to the old number" })).status).toBe(403);
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "after the change" })).status).toBe(200);
    const sent = t.graph.sends().at(-1)!.body!;
    expect(sent.recipient).toBe(newUser);
    expect("to" in sent).toBe(false);
    const r = await t.admin("GET", "/v1/admin/cities");
    expect(r.body.cities.find((c: any) => c.id === city.id).phones_last4).toEqual(["id"]);
  });
});

describe("BSUID: a number change links the old user id to the new one through Meta's retries (gp-0g2s; gp-lv49)", () => {
  /** The wamid of a payload's one message. */
  const wamidOf = (p: any): string => p.entry[0].changes[0].value.messages[0].id;
  const notice = (from: string | null, previousUser: string, newUser: string, newWa: string | null, ts: number) =>
    changeAs(newWa ? "user_changed_number" : "user_changed_user_id", { from, previousUser, newUser, newWa }, { wamid: `wamid.${uniq()}`, ts });

  it("a message sent before the change whose first delivery failed, retried after the notice committed, reaches the owner's conversation: no canned line, no id changes, its window moves", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const newWa = phone();
    const newUser = bsuid();
    // The gate's order: the owner writes at T0 + 1 h and the gateway's write fails (Meta will retry),
    // the number change commits at T0 + 2 h, and Meta's retry arrives at T0 + 3 h under the old ids.
    t.clock.now = T0 + HOUR;
    const before = textAs({ wa, user }, "sent before the change", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR, name: "A, before the change" });
    flaky.arm(0);
    expect((await t.webhook(before)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    const change = notice(wa, user, newUser, newWa, t.clock.now);
    expect((await t.webhook(change)).status).toBe(200);
    t.clock.now = T0 + 3 * HOUR;
    const calls = t.graph.sends().length;
    expect((await t.webhook(before)).status).toBe(200);
    const d = (await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === wamidOf(before));
    expect(d).toMatchObject({ kind: "message", wa_id: wa, user_id: user, conversation_id: user });
    expect(d.message.text.body).toBe("sent before the change");
    // Under a previous id, the retry is older than the binding's names: it changes none of them.
    expect(await env.DB.prepare("SELECT profile_name FROM phones WHERE conversation_id = ?").bind(user).first()).toEqual({ profile_name: "Owner Name" });
    expect(t.graph.sends()).toHaveLength(calls); // no canned line, least of all to the old number
    expect(await owners(wa, newWa, user, newUser)).toEqual([{ conversation_id: user, shows: newWa, user_id: newUser, username: null, city_id: city.id }]);
    expect((await t.window(city)).body.windows).toEqual([
      { conversation_id: user, user_id: newUser, last_inbound_at: T0 + HOUR, window_closes_at: T0 + HOUR + CLOSE },
    ]);
    // A second retry of the same message is still one delivery.
    expect((await t.webhook(before)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.filter((x: any) => x.message?.id === wamidOf(before))).toHaveLength(1);
    // Meta redelivers the notice: one delivery, still no window move, no second re-key.
    expect((await t.webhook(change)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.filter((x: any) => x.message?.id === wamidOf(change))).toHaveLength(1);
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(T0 + HOUR);
    expect(await owners(wa, newWa, user, newUser)).toEqual([{ conversation_id: user, shows: newWa, user_id: newUser, username: null, city_id: city.id }]);
  });

  it("a retry landing after the owner already wrote under its new ids leaves the window at the newer message (self-review r1 S7)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    const before = textAs({ wa: p, user: a }, "before", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(before)).status).toBe(500);
    const q = phone();
    const a2 = bsuid();
    t.clock.now = T0 + 2 * HOUR;
    expect((await t.webhook(notice(p, a, a2, q, t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 3 * HOUR;
    await t.sayAs({ wa: q, user: a2 }, "after, new ids");
    t.clock.now = T0 + 4 * HOUR;
    expect((await t.webhook(before)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === wamidOf(before))).toBe(true);
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(T0 + 3 * HOUR);
  });

  it("with no phone shared (user_changed_user_id) a retry under the previous user_id is the owner's, even when its text is another city's live invite code; a retry naming the old number alone reaches no city and hears nothing", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const other = await t.enrollCity();
    // Known by phone and BSUID, keyed by the phone; Meta later names it by the user_id alone.
    const wa1 = phone();
    const user = bsuid();
    const newUser = bsuid();
    await t.joinAs(city, { wa: wa1, user });
    const wa2 = phone();
    const user2 = bsuid();
    await t.joinAs(city, { wa: wa2, user: user2 });
    t.clock.now = T0 + HOUR;
    // The owner passes on another city's invite code: an owner's message, never an attempt or a bind.
    const otherCode = await t.invite(other);
    const byId = textAs({ user }, otherCode, { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(byId)).status).toBe(500);
    const byPhone = numberOnly(textPayload(wa2, "the number alone, before the change", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR }));
    flaky.arm(0);
    expect((await t.webhook(byPhone)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    expect((await t.webhook(notice(null, user, newUser, null, t.clock.now))).status).toBe(200);
    expect((await t.webhook(notice(wa2, user2, bsuid(), phone(), t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 3 * HOUR;
    const calls = t.graph.sends().length;
    expect((await t.webhook(byId)).status).toBe(200);
    expect((await t.webhook(byPhone)).status).toBe(200);
    const deliveries = (await t.inbox(city)).body.deliveries;
    expect(deliveries.find((x: any) => x.message?.id === wamidOf(byId))).toMatchObject({ kind: "message", wa_id: null, user_id: user, conversation_id: user });
    // A number can change hands unseen: one naming no user id is no binding's (point 1).
    expect(deliveries.some((x: any) => x.message?.id === wamidOf(byPhone))).toBe(false);
    expect(t.graph.sends()).toHaveLength(calls);
    // The owner's own join counted its one attempt; the retries add none and claim no canned line.
    expect(
      (
        await env.DB.prepare("SELECT sender_key, code_attempts, last_replied_at FROM unknown_senders WHERE sender_key IN (?, ?)")
          .bind(user, wa2)
          .all()
      ).results,
    ).toEqual([{ sender_key: user, code_attempts: 1, last_replied_at: null }]);
    expect(await owners(wa1, user, newUser)).toEqual([{ conversation_id: user, shows: null, user_id: newUser, username: null, city_id: city.id }]);
    expect((await t.inbox(other)).body.deliveries).toEqual([]);
    expect(await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(otherCode).first()).toEqual({ used_at: null });
  });

  it("a retry under the old user id reaches the owner and never the person who holds the old number now; one naming the number alone reaches no city; it takes nothing from that person, whose own messages stay theirs", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    const before = textAs({ wa: p, user: a }, "before the change", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(before)).status).toBe(500);
    const legacy = numberOnly(textPayload(p, "before the change, the number alone", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR, name: "A, before" }));
    flaky.arm(0);
    expect((await t.webhook(legacy)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    const q = phone();
    const a2 = bsuid();
    expect((await t.webhook(notice(p, a, a2, q, t.clock.now))).status).toBe(200);
    // The old number is someone else's now, and that person joins the same city.
    t.clock.now = T0 + 3 * HOUR;
    const b = bsuid();
    await t.joinAs(city, { wa: p, user: b }, { name: "B" });
    t.clock.now = T0 + 4 * HOUR;
    expect((await t.webhook(before)).status).toBe(200);
    expect((await t.webhook(legacy)).status).toBe(200);
    const deliveries = (await t.inbox(city)).body.deliveries;
    expect(deliveries.find((x: any) => x.message?.id === wamidOf(before))).toMatchObject({ conversation_id: a, user_id: a });
    // Meta shows the number alone: the pairing that would say whose it was may still be in flight,
    // so it is nobody's (point 1), neither A's nor B's.
    expect(deliveries.some((x: any) => x.message?.id === wamidOf(legacy))).toBe(false);
    expect(await owners(p, q, a, a2, b)).toEqual([
      { conversation_id: a, shows: q, user_id: a2, username: null, city_id: city.id },
      { conversation_id: b, shows: p, user_id: b, username: null, city_id: city.id },
    ]);
    expect(await env.DB.prepare("SELECT profile_name FROM phones WHERE conversation_id = ?").bind(b).first()).toEqual({ profile_name: "B" });
    const mine = await t.sayAs({ wa: p, user: b }, "the number's holder now");
    expect((await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === mine.wamid)).toMatchObject({ conversation_id: b });
  });

  it("one number two owners left in turn: each retry goes to the owner its user id named when Meta stamped it, whatever the order of their bind wamids (self-review r1 S8)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    // The earlier binding's wamid sorts after the later one's, so no key order stands in for time.
    await t.joinAs(city, { wa: p, user: a }, { wamid: `wamid.ZZZ${uniq()}` });
    t.clock.now = T0 + HOUR;
    const fromA = textAs({ wa: p, user: a }, "A's", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(fromA)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    expect((await t.webhook(notice(p, a, bsuid(), phone(), t.clock.now))).status).toBe(200);
    // While the number is nobody's, someone writes from it under their own user id; that write fails too.
    const between = textAs({ wa: p, user: bsuid() }, "the number between owners", { wamid: `wamid.${uniq()}`, ts: T0 + 2 * HOUR + HOUR / 2 });
    t.clock.now = T0 + 2 * HOUR + HOUR / 2;
    flaky.arm(0);
    expect((await t.webhook(between)).status).toBe(500);
    t.clock.now = T0 + 3 * HOUR;
    const b = bsuid();
    await t.joinAs(city, { wa: p, user: b }, { wamid: `wamid.AAA${uniq()}` });
    t.clock.now = T0 + 4 * HOUR;
    const fromB = textAs({ wa: p, user: b }, "B's", { wamid: `wamid.${uniq()}`, ts: T0 + 4 * HOUR });
    flaky.arm(0);
    expect((await t.webhook(fromB)).status).toBe(500);
    t.clock.now = T0 + 5 * HOUR;
    expect((await t.webhook(notice(p, b, bsuid(), phone(), t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 6 * HOUR;
    expect((await t.webhook(fromA)).status).toBe(200);
    expect((await t.webhook(fromB)).status).toBe(200);
    const calls = t.graph.sends().length;
    expect((await t.webhook(between)).status).toBe(200);
    const deliveries = (await t.inbox(city)).body.deliveries;
    expect(deliveries.find((x: any) => x.message?.id === wamidOf(fromA))).toMatchObject({ conversation_id: a });
    expect(deliveries.find((x: any) => x.message?.id === wamidOf(fromB))).toMatchObject({ conversation_id: b });
    // A third person's user id, no binding's: a stranger's, neither owner's.
    expect(deliveries.some((x: any) => x.message?.id === wamidOf(between))).toBe(false);
    expect(t.graph.sends().slice(calls).map((c) => c.body?.text?.body)).toEqual([CANNED]);
  });

  it("the old number's message Meta stamped after the change is not the owner's, nor is one naming the number beside another user_id: each a stranger's", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    expect((await t.webhook(notice(p, a, bsuid(), phone(), t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 2 * HOUR;
    const later = await t.say(p, "a new holder of the old number, under their own user id");
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === later.wamid)).toBe(false);
    expect(t.graph.sends().filter((c) => c.body?.recipient === userOf(p) && c.body?.text?.body === CANNED)).toHaveLength(1);
    // Meta stamped it before the change, but under another person's user id: it is theirs.
    const other = await t.sayAs({ wa: p, user: bsuid() }, "someone else's", { ts: T0 + HOUR - 60 });
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === other.wamid)).toBe(false);
  });

  it("an unbind records every user id the binding answered to: once the operator unbinds the owner and the old number binds again, a retry under the old user id reaches no city and takes nothing from the new holder", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const first = await t.enrollCity();
    const second = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(first, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    const before = textAs({ wa: p, user: a }, "before the change", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(before)).status).toBe(500);
    const legacy = numberOnly(textPayload(p, "before the change, the number alone", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR, name: "A's name" }));
    flaky.arm(0);
    expect((await t.webhook(legacy)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    const q = phone();
    const a2 = bsuid();
    expect((await t.webhook(notice(p, a, a2, q, t.clock.now))).status).toBe(200);
    // README, "Unbind an owner": the record covers a2 and, through the notice's link, a.
    await t.unbind(a2);
    expect((await env.DB.prepare("SELECT user_id FROM unbinds WHERE user_id IN (?, ?) ORDER BY user_id").bind(a, a2).all()).results.map((r: any) => r.user_id)).toEqual(
      [a, a2].sort(),
    );
    // Retried while nobody holds the number: it names no user id, so it is nobody's and hears
    // nothing; it goes nowhere.
    t.clock.now = T0 + 2 * HOUR + HOUR / 2;
    const quiet = t.graph.sends().length;
    expect((await t.webhook(legacy)).status).toBe(200);
    expect(t.graph.sends()).toHaveLength(quiet);
    t.clock.now = T0 + 3 * HOUR;
    const c = bsuid();
    await t.joinAs(second, { wa: p, user: c }, { name: "C" }); // keyed by its user id (gp-g02t), never the number
    expect(await owners(p)).toEqual([{ conversation_id: c, shows: p, user_id: c, username: null, city_id: second.id }]);
    t.clock.now = T0 + 4 * HOUR;
    const calls = t.graph.sends().length;
    expect((await t.webhook(before)).status).toBe(200);
    // Under a, stamped before the unbind: the unbound binding's, never C's (self-review r1 S2); the
    // number alone: nobody's.
    expect((await t.webhook(legacy)).status).toBe(200);
    for (const city of [first, second]) {
      for (const m of [before, legacy]) {
        expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === wamidOf(m))).toBe(false);
      }
    }
    expect(await owners(p)).toEqual([{ conversation_id: c, shows: p, user_id: c, username: null, city_id: second.id }]);
    expect(await env.DB.prepare("SELECT profile_name FROM phones WHERE conversation_id = ?").bind(c).first()).toEqual({ profile_name: "C" });
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a notice whose first delivery failed, landing after the old number's next holder joined, takes nothing from that holder, and the owner's retry under its old user id still reaches it (self-review r1 S3)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    const legacy = textAs({ wa: p, user: a }, "A, before the change", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(legacy)).status).toBe(500);
    const q = phone();
    const a2 = bsuid();
    t.clock.now = T0 + 2 * HOUR;
    const late = notice(p, a, a2, q, T0 + 2 * HOUR);
    flaky.arm(0);
    expect((await t.webhook(late)).status).toBe(500);
    // B, the number's next holder, joins first: P's newest pairing is B's, so B shows it.
    t.clock.now = T0 + 3 * HOUR;
    const b = bsuid();
    await t.joinAs(city, { wa: p, user: b });
    t.clock.now = T0 + 4 * HOUR;
    expect((await t.webhook(late)).status).toBe(200);
    expect(await owners(p, q, a, a2, b)).toEqual([
      { conversation_id: a, shows: q, user_id: a2, username: null, city_id: city.id },
      { conversation_id: b, shows: p, user_id: b, username: null, city_id: city.id },
    ]);
    expect((await t.webhook(legacy)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === wamidOf(legacy))).toMatchObject({ conversation_id: a });
    const calls = t.graph.sends().length;
    const mine = await t.sayAs({ wa: p, user: b }, "B's");
    expect((await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === mine.wamid)).toMatchObject({ conversation_id: b });
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a notice Meta redelivers after the owner changed number again takes nothing from the old number's next holder (self-review r1 S4)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const other = await t.enrollCity();
    const a = bsuid();
    await t.joinAs(city, { user: a }); // a username owner, keyed by its BSUID
    const p = phone();
    await t.sayAs({ wa: p, user: a }, "now with my phone");
    t.clock.now = T0 + HOUR;
    const q = phone();
    const a2 = bsuid();
    const first = notice(p, a, a2, q, t.clock.now);
    expect((await t.webhook(first)).status).toBe(200);
    // B binds on P in another city under its own user id; P is nobody's key, so B's key is P.
    t.clock.now = T0 + 2 * HOUR;
    await t.join(other, p);
    t.clock.now = T0 + 3 * HOUR;
    expect((await t.webhook(notice(q, a2, bsuid(), phone(), t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 4 * HOUR;
    expect((await t.webhook(first)).status).toBe(200);
    expect(await owners(p)).toEqual([{ conversation_id: userOf(p), shows: p, user_id: userOf(p), username: null, city_id: other.id }]);
    const s = await t.send(other, { to: p, idempotency_key: uniq(), type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } });
    expect(s.status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ recipient: userOf(p) });
  });

  it("what a number's previous holders sent, under their own user ids, never reaches the binding that shows the number now (self-review r1)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    // A, a username owner, is first shown with phone P at T0 + 2 h (one owner, two ids, one row).
    const a = bsuid();
    await t.joinAs(city, { user: a });
    const p = phone();
    // C, bound on R, changes number to Q at T0 + 2 h.
    const c = bsuid();
    const r = phone();
    await t.joinAs(city, { wa: r, user: c });
    const q = phone();
    // Before then, P's and Q's previous holders wrote under their own user ids; both writes failed.
    t.clock.now = T0 + HOUR;
    const fromP = textPayload(p, "P's previous holder", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(fromP)).status).toBe(500);
    const fromQ = textPayload(q, "Q's previous holder", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(fromQ)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    await t.sayAs({ wa: p, user: a }, "now with my phone");
    expect((await t.webhook(notice(r, c, bsuid(), q, t.clock.now))).status).toBe(200);
    // Both bound at T0: owners() orders them by their keys, two user ids since gp-g02t.
    const both = await owners(p, q);
    expect(both).toHaveLength(2);
    expect(both).toEqual(expect.arrayContaining([
      { conversation_id: c, shows: q, user_id: expect.any(String), username: null, city_id: city.id },
      { conversation_id: a, shows: p, user_id: a, username: null, city_id: city.id },
    ]));
    t.clock.now = T0 + 2 * HOUR + HOUR / 4;
    expect((await t.webhook(fromP)).status).toBe(200);
    expect((await t.webhook(fromQ)).status).toBe(200);
    // C moves on from Q; Q's previous holder is still no binding's.
    t.clock.now = T0 + 2 * HOUR + HOUR / 2;
    const cNow = (await owners(c))[0]!.user_id as string;
    expect((await t.webhook(notice(q, cNow, bsuid(), phone(), t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 3 * HOUR;
    expect((await t.webhook(fromQ)).status).toBe(200);
    for (const m of [fromP, fromQ]) {
      expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === wamidOf(m))).toBe(false);
    }
  });

  /*
   * gp-w376's orders (PR 76's codex gate r2 MAJOR, gateway.ts:823 at e39c0b90), re-cut by gp-lv49.
   * Owner A binds at T0, and its records first name number P at T2, in one of the ways a binding
   * comes to a number after its bind; at T3, P is another binding's in the records; at T4, A's next
   * notice names P as A's old number. PR 76's draft rebuilt who held P from that history and routed
   * a number-only message by it. The design of record routes by the user id alone (section 2,
   * point 1): whatever P's history, a late message naming P alone reaches no city and grants no
   * media, and is counted; P's holder in another city keeps its binding.
   */
  const T1 = T0 + HOUR;
  const T2 = T0 + 2 * HOUR;
  const T3 = T0 + 3 * HOUR;
  const T4 = T0 + 4 * HOUR;
  type Late = { t: ReturnType<typeof setup>; city: City; other: City; p: string };
  const lateNumber: Array<{
    how: string;
    /** Binds A at T0, brings P into A's records at T2 and leaves P another binding's (in `other`) by T3. Returns A's user_id at T4 and any other city it made. */
    run(x: Late): Promise<{ aUser: string; elsewhere?: City[] }>;
  }> = [
    {
      how: "the gate's sequence: a number change gives A the number P at T2, and B takes P at T3",
      async run({ t, city, other, p }) {
        const w0 = phone();
        const a = bsuid();
        await t.joinAs(city, { wa: w0, user: a });
        t.clock.now = T2;
        const a2 = bsuid();
        expect((await t.webhook(notice(w0, a, a2, p, T2))).status).toBe(200);
        expect(await owners(a2)).toEqual([{ conversation_id: a, shows: p, user_id: a2, username: null, city_id: city.id }]);
        t.clock.now = T3;
        await t.joinAs(other, { wa: p, user: bsuid() }, { name: "B" }); // P's newest pairing is B's
        expect((await owners(a2))[0]).toMatchObject({ shows: null });
        return { aUser: a2 };
      },
    },
    {
      how: "a message shows P beside A's user_id at T2, and B takes P at T3",
      async run({ t, city, other, p }) {
        const a = bsuid();
        await t.joinAs(city, { user: a });
        t.clock.now = T2;
        await t.sayAs({ wa: p, user: a }, "now with my phone");
        expect(await owners(a)).toEqual([{ conversation_id: a, shows: p, user_id: a, username: null, city_id: city.id }]);
        t.clock.now = T3;
        await t.joinAs(other, { wa: p, user: bsuid() }, { name: "B" });
        expect((await owners(a))[0]).toMatchObject({ shows: null });
        return { aUser: a };
      },
    },
    {
      how: "a number change names P at T2 while C, an owner PR 73 bound by P alone at T1 + 30 min, holds it",
      async run({ t, city, other, p }) {
        const w0 = phone();
        const a = bsuid();
        await t.joinAs(city, { wa: w0, user: a });
        await legacyBind(env.DB, other.id, p, T1 + HOUR / 2);
        t.clock.now = T2;
        const a2 = bsuid();
        expect((await t.webhook(notice(w0, a, a2, p, T2))).status).toBe(200);
        expect(await owners(a2)).toEqual([{ conversation_id: a, shows: p, user_id: a2, username: null, city_id: city.id }]);
        return { aUser: a2 };
      },
    },
    {
      how: "a number change at T2 shares no number; B takes P at T3, and A's next notice names P as A's old number (the mutation check's survivor)",
      async run({ t, city, other, p }) {
        const w0 = phone();
        const a = bsuid();
        await t.joinAs(city, { wa: w0, user: a });
        t.clock.now = T2;
        const a2 = bsuid();
        expect((await t.webhook(notice(w0, a, a2, null, T2))).status).toBe(200);
        expect(await owners(a2)).toEqual([{ conversation_id: a, shows: null, user_id: a2, username: null, city_id: city.id }]);
        t.clock.now = T3;
        await t.joinAs(other, { wa: p, user: bsuid() }, { name: "B" });
        return { aUser: a2 };
      },
    },
    {
      how: "the Fable read r1's reproduction: P was C's, bound in a third city at T0 and unbound after T1; a message shows P beside A's user_id at T2; B takes P at T3",
      async run({ t, city, other, p }) {
        const a = bsuid();
        await t.joinAs(city, { user: a });
        const third = await t.enrollCity();
        const c = bsuid();
        await t.joinAs(third, { wa: p, user: c });
        t.clock.now = T1 + HOUR / 2;
        await t.unbind(c); // README, "Unbind an owner"
        t.clock.now = T2;
        await t.sayAs({ wa: p, user: a }, "now with my phone");
        expect(await owners(a)).toEqual([{ conversation_id: a, shows: p, user_id: a, username: null, city_id: city.id }]);
        t.clock.now = T3;
        await t.joinAs(other, { wa: p, user: bsuid() }, { name: "B" });
        expect((await owners(a))[0]).toMatchObject({ shows: null });
        return { aUser: a, elsewhere: [third] };
      },
    },
  ];

  for (const row of lateNumber) {
    it(`a number's history never routes: messages naming P alone reach no city, whatever bound or showed P (gp-w376's orders, gp-lv49): ${row.how}`, async () => {
      const t = setup();
      const city = await t.enrollCity();
      const other = await t.enrollCity();
      const p = phone();
      const { aUser, elsewhere = [] } = await row.run({ t, city, other, p });
      const holderRow = async () =>
        env.DB.prepare("SELECT conversation_id, wa_id, user_id, city_id, profile_name FROM phones WHERE city_id = ?").bind(other.id).first();
      const holder = await holderRow();
      expect(holder).not.toBeNull();
      t.clock.now = T4;
      expect((await t.webhook(notice(p, aUser, bsuid(), phone(), T4))).status).toBe(200);
      // Meta delivers late what P's holders sent at T1 and at T2 + 30 min, a text and an image each,
      // naming P alone.
      const sent = (ts: number) => {
        const mediaId = `5${ts}${Math.floor(Math.random() * 1_000_000)}`;
        t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
        return {
          text: numberOnly(textPayload(p, `P's holder at ${ts}`, { wamid: `wamid.${uniq()}`, ts, name: "P's holder" })),
          image: numberOnly(mediaPayload("image", p, mediaId, { wamid: `wamid.${uniq()}`, ts })),
          mediaId,
        };
      };
      const late = [sent(T1), sent(T2 + HOUR / 2)];
      t.clock.now = T4 + HOUR;
      const unroutedBefore = await t.stat("unrouted_messages");
      const logsBefore = t.logs.length;
      const calls = t.graph.sends().length;
      for (const m of late) {
        expect((await t.webhook(m.text)).status).toBe(200);
        expect((await t.webhook(m.image)).status).toBe(200);
      }
      const wamids = late.flatMap((m) => [wamidOf(m.text), wamidOf(m.image)]);
      for (const c of [city, other, ...elsewhere]) {
        const held = (await t.inbox(c)).body.deliveries.map((x: any) => x.message?.id);
        expect(held.filter((id: string) => wamids.includes(id))).toEqual([]);
        for (const m of late) expect((await t.call("GET", `/v1/media/${m.mediaId}`, { token: c.token })).status).toBe(404);
      }
      // Every one is counted, in the stats row and in each payload's log line (the Fable read r1
      // NOTE 5), and none is answered: the gateway sends only to user ids.
      expect((await t.stat("unrouted_messages")) - unroutedBefore).toBe(4);
      expect(t.logs.slice(logsBefore).filter((e) => e.event === "webhook").map((e) => e.unrouted)).toEqual([1, 1, 1, 1]);
      expect(t.graph.sends()).toHaveLength(calls);
      // P's holder keeps its binding and its name.
      expect(await holderRow()).toEqual(holder);
    });
  }

  /*
   * gp-w376, the lane's fresh self-review (CRITICAL, reproduced at eaf93d2b), re-cut by gp-lv49.
   * A, known by user_id alone, never holds P: C holds P. A's notice then names P as A's old number.
   * What C sent under C's user id reaches C's city while C stands, and no city once an operator
   * unbinds C; never A's, whatever A's notice named.
   */
  for (const shown of [true, false]) {
    it(`a number the owner's notice names as its old one routes nothing to it (Meta ${shown ? "showed it beside the owner's user_id" : "never showed it with the owner"}): what its holder sent reaches the holder's city while it stands, and no city once it is unbound (gp-w376)`, async () => {
      const t = setup();
      const x = await t.enrollCity();
      const w = await t.enrollCity();
      const a = bsuid();
      await t.joinAs(x, { user: a });
      const p = phone();
      const c = bsuid();
      await t.joinAs(w, { wa: p, user: c }); // C holds P from T0
      const mediaId = `4${Date.now()}${Math.floor(Math.random() * 1_000_000)}`;
      t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
      const whileC = textAs({ wa: p, user: c }, "C's, read while C stands", { wamid: `wamid.${uniq()}`, ts: T1 });
      const image = imageAs({ wa: p, user: c }, mediaId, { wamid: `wamid.${uniq()}`, ts: T1 });
      if (shown) {
        // Meta shows P beside A's user_id: A shows P from then (display only); C's binding is untouched.
        t.clock.now = T2;
        await t.sayAs({ wa: p, user: a }, "my phone, says Meta");
        expect(await owners(a)).toEqual([{ conversation_id: a, shows: p, user_id: a, username: null, city_id: x.id }]);
        expect(await owners(c)).toEqual([{ conversation_id: c, shows: null, user_id: c, username: null, city_id: w.id }]);
      }
      t.clock.now = T4;
      const a2 = bsuid();
      expect((await t.webhook(notice(p, a, a2, phone(), T4))).status).toBe(200);
      // While C stands, C's message stamped T1 is C's.
      t.clock.now = T4 + HOUR / 4;
      expect((await t.webhook(whileC)).status).toBe(200);
      expect((await t.inbox(w)).body.deliveries.find((d: any) => d.message?.id === wamidOf(whileC))).toMatchObject({ conversation_id: c });
      // README, "Unbind an owner"; then Meta delivers C's image stamped T1, late.
      t.clock.now = T4 + HOUR / 2;
      await t.unbind(c);
      t.clock.now = T4 + HOUR;
      expect((await t.webhook(image)).status).toBe(200);
      const xHeld = (await t.inbox(x)).body.deliveries.map((d: any) => d.message?.id);
      expect(xHeld).not.toContain(wamidOf(image));
      expect(xHeld).not.toContain(wamidOf(whileC));
      expect((await t.inbox(w)).body.deliveries.map((d: any) => d.message?.id)).not.toContain(wamidOf(image));
      for (const city of [x, w]) expect((await t.call("GET", `/v1/media/${mediaId}`, { token: city.token })).status).toBe(404);
    });
  }

  it("a notice that gives another owner a holder's number moves only what the holder shows: what the holder sent under its own user id reaches it, before and after its own notice (gp-w376)", async () => {
    const t = setup();
    const x = await t.enrollCity();
    const y = await t.enrollCity();
    // X binds on (P, x) at T0.
    const p = phone();
    const xUser = bsuid();
    await t.joinAs(x, { wa: p, user: xUser });
    const a = bsuid();
    await t.joinAs(y, { wa: phone(), user: a });
    const fromX = textAs({ wa: p, user: xUser }, "X's", { wamid: `wamid.${uniq()}`, ts: T1, name: "X" });
    // A's number change at T2 shares P: P's newest pairing is A's new id, so A shows P, X none.
    t.clock.now = T2;
    const aKey = (await owners(a))[0]!.conversation_id as string;
    const a2 = bsuid();
    expect((await t.webhook(notice(aKey, a, a2, p, T2))).status).toBe(200);
    expect((await owners(xUser))[0]).toMatchObject({ shows: null });
    expect((await owners(a2))[0]).toMatchObject({ shows: p });
    // X's own notice at T4 replaces xUser: X answers to it until then.
    t.clock.now = T4;
    expect((await t.webhook(notice(p, xUser, bsuid(), phone(), T4))).status).toBe(200);
    t.clock.now = T4 + HOUR;
    expect((await t.webhook(fromX)).status).toBe(200);
    expect((await t.inbox(x)).body.deliveries.find((d: any) => d.message?.id === wamidOf(fromX))).toMatchObject({ conversation_id: xUser, wa_id: p });
    expect((await t.inbox(y)).body.deliveries.some((d: any) => d.message?.id === wamidOf(fromX))).toBe(false);
  });

  it("a stranger's stale message naming the number beside its own user_id never takes it from a holder that bound after the message's stamp (gp-0g2s)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    t.clock.now = T0 + HOUR;
    const stale = textAs({ wa: p, user: bsuid() }, "the number's earlier user", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(stale)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    const b = bsuid();
    await t.joinAs(city, { wa: p, user: b });
    t.clock.now = T0 + 3 * HOUR;
    expect((await t.webhook(stale)).status).toBe(200);
    expect(await owners(b)).toEqual([{ conversation_id: b, shows: p, user_id: b, username: null, city_id: city.id }]);
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === wamidOf(stale))).toBe(false);
  });

  it("a late notice never takes its new number from a holder that showed it after the notice's stamp (self-review r1)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    const q = phone();
    const a2 = bsuid();
    t.clock.now = T0 + HOUR;
    const late = notice(p, a, a2, q, T0 + HOUR);
    flaky.arm(0);
    expect((await t.webhook(late)).status).toBe(500);
    // D shows Q beside its own user_id at T0 + 2 h and joins.
    t.clock.now = T0 + 2 * HOUR;
    const d = bsuid();
    await t.joinAs(city, { wa: q, user: d });
    t.clock.now = T0 + 3 * HOUR;
    expect((await t.webhook(late)).status).toBe(200);
    expect(await owners(p, q, a2, d)).toEqual([
      { conversation_id: a, shows: null, user_id: a2, username: null, city_id: city.id },
      { conversation_id: d, shows: q, user_id: d, username: null, city_id: city.id },
    ]);
  });

  it("when the records disagree (the number's next holder showed it before the old owner's notice), a message naming the number alone goes to nobody, and each owner's own messages go to it (self-review r1)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    // At T0 + 2 h, B shows P beside its own user_id and joins: P's newest pairing is B's.
    t.clock.now = T0 + 2 * HOUR;
    const b = bsuid();
    await t.joinAs(city, { wa: p, user: b }, { name: "B" });
    // A message from P alone, stamped T0 + 2.5 h; its write fails.
    t.clock.now = T0 + 2 * HOUR + HOUR / 2;
    const between = numberOnly(textPayload(p, "whose is this?", { wamid: `wamid.${uniq()}`, ts: t.clock.now, name: "unsure" }));
    flaky.arm(0);
    expect((await t.webhook(between)).status).toBe(500);
    // A writes in Meta's BSUID shape at the same moment; that write fails too.
    const fromA = textAs({ wa: p, user: a }, "A, by BSUID", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    flaky.arm(0);
    expect((await t.webhook(fromA)).status).toBe(500);
    // A's notice says A left P at T0 + 3 h, after B's pairing: the records disagree, and P shows
    // nobody from then (point 4; display only). B's binding is untouched, on the notice's first
    // landing or when Meta redelivers it after A's re-key.
    t.clock.now = T0 + 3 * HOUR;
    const change = notice(p, a, bsuid(), phone(), t.clock.now);
    expect((await t.webhook(change)).status).toBe(200);
    expect(await owners(b)).toEqual([{ conversation_id: b, shows: null, user_id: b, username: null, city_id: city.id }]);
    expect((await t.webhook(change)).status).toBe(200);
    expect(await owners(b)).toEqual([{ conversation_id: b, shows: null, user_id: b, username: null, city_id: city.id }]);
    t.clock.now = T0 + 4 * HOUR;
    const calls = t.graph.sends().length;
    expect((await t.webhook(between)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === wamidOf(between))).toBe(false);
    expect(t.graph.sends()).toHaveLength(calls);
    // Nor does it touch B's row.
    expect(await env.DB.prepare("SELECT profile_name FROM phones WHERE conversation_id = ?").bind(b).first()).toEqual({ profile_name: "B" });
    // A's own message names A's previous user_id, which no one else ever holds: it is A's, and its
    // pairing with P, stamped below the notice, moves no mark.
    expect((await t.webhook(fromA)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === wamidOf(fromA))).toMatchObject({ conversation_id: a, user_id: a });
    expect(await owners(b)).toEqual([{ conversation_id: b, shows: null, user_id: b, username: null, city_id: city.id }]);
    // B changes number too; the number alone still goes to nobody.
    t.clock.now = T0 + 5 * HOUR;
    expect((await t.webhook(notice(p, b, bsuid(), phone(), t.clock.now))).status).toBe(200);
    t.clock.now = T0 + 6 * HOUR;
    expect((await t.webhook(between)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.message?.id === wamidOf(between))).toBe(false);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("the cron deletes a notice's link, an unbind's record and a mark naming no binding at the horizon, 8 days (TOMBSTONE_SECONDS) after their stamp; a mark naming a binding's current id stays", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    t.clock.now = T0 + HOUR;
    const gone = { wa: phone(), user: bsuid() };
    await t.joinAs(city, gone);
    const newUser = bsuid();
    const newWa = phone();
    expect((await t.webhook(notice(wa, user, newUser, newWa, t.clock.now))).status).toBe(200);
    expect(await t.unbind(gone.user)).toEqual({ route: "admin" });
    const evidence = async () => ({
      links: (await env.DB.prepare("SELECT old_id FROM links WHERE old_id = ?").bind(user).all()).results.length,
      unbinds: (await env.DB.prepare("SELECT user_id FROM unbinds WHERE user_id = ?").bind(gone.user).all()).results.length,
      // wa: cleared by the notice; gone.wa: names the unbound binding; newWa: names the owner's current id.
      marks: (
        await env.DB.prepare("SELECT number FROM marks WHERE number IN (?, ?, ?) ORDER BY number").bind(wa, gone.wa, newWa).all<{ number: string }>()
      ).results.map((r) => r.number),
    });
    expect(await evidence()).toEqual({ links: 1, unbinds: 1, marks: [wa, gone.wa, newWa].sort() });
    t.clock.now = T0 + HOUR + 8 * DAY - 1;
    await t.gateway.maintain();
    expect(await evidence()).toEqual({ links: 1, unbinds: 1, marks: [wa, gone.wa, newWa].sort() });
    t.clock.now = T0 + HOUR + 8 * DAY;
    const report = await t.gateway.maintain();
    // Storage is shared across this file's tests, so other tests' evidence may go in the same run.
    expect(report.links_deleted).toBeGreaterThanOrEqual(1);
    expect(report.unbinds_deleted).toBeGreaterThanOrEqual(1);
    expect(report.marks_deleted).toBeGreaterThanOrEqual(2);
    expect(await evidence()).toEqual({ links: 0, unbinds: 0, marks: [newWa] });
    // The owner keeps its current id and its number past the horizon.
    expect(await owners(newUser)).toEqual([{ conversation_id: user, shows: newWa, user_id: newUser, username: null, city_id: city.id }]);
  });
});

describe("BSUID: retention", () => {
  it("the ack and the 7-day cron clear a delivery's user_id, username and conversation_id with its body", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const user = bsuid();
    await t.joinAs(city, { user, username: "kept.too.long" });
    await t.sayAs({ user, username: "kept.too.long" }, "acked");
    await t.sayAs({ user, username: "kept.too.long" }, "expires");
    const [joined, acked] = (await t.inbox(city)).body.deliveries.map((d: any) => d.id);
    const rows = async () =>
      (
        await env.DB.prepare("SELECT id, body, wa_id, user_id, username, conversation_id, profile_name FROM deliveries WHERE city_id = ? ORDER BY id")
          .bind(city.id)
          .all<Record<string, unknown>>()
      ).results;
    const cleared = { body: null, wa_id: null, user_id: null, username: null, conversation_id: null, profile_name: null };
    await t.ack(city, [joined, acked]);
    // The ack itself clears them, before any cron.
    const afterAck = await rows();
    expect(afterAck.filter((r) => r.id === joined || r.id === acked)).toEqual([
      { id: joined, ...cleared },
      { id: acked, ...cleared },
    ]);
    expect(afterAck.find((r) => r.id !== joined && r.id !== acked)).toMatchObject({ user_id: user, username: "kept.too.long", conversation_id: user });
    t.clock.now += 7 * DAY;
    await t.gateway.maintain();
    const afterCron = await rows();
    expect(afterCron).toHaveLength(3);
    for (const row of afterCron) expect(row).toEqual({ id: row.id, ...cleared });
  });

  it("the cron clears the ids a v1 Worker's ack left on a body-less row (the conversation_id migration 0002 wrote)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const wamid = `wamid.${uniq()}`;
    // What a v1 ack leaves: body, wa_id and profile_name cleared, the conversation_id it never knew of kept.
    await env.DB.prepare(
      "INSERT INTO deliveries (city_id, received_at, kind, wamid, status, conversation_id, acked_at) VALUES (?, ?, 'message', ?, '', ?, ?)",
    )
      .bind(city.id, t.clock.now, wamid, wa, t.clock.now)
      .run();
    const report = await t.gateway.maintain();
    expect(report.stray_ids_cleared).toBeGreaterThanOrEqual(1);
    expect(await env.DB.prepare("SELECT conversation_id FROM deliveries WHERE wamid = ?").bind(wamid).first()).toEqual({ conversation_id: null });
  });
});

describe("BSUID: the operator's view and the logs", () => {
  it("the admin listing shows a phone's last four digits and 'id' for an owner with no phone, never a full id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const user = bsuid();
    await t.joinAs(city, { user });
    const r = await t.admin("GET", "/v1/admin/cities");
    expect(r.status).toBe(200);
    const row = r.body.cities.find((c: any) => c.id === city.id);
    expect(row.phones).toBe(2);
    expect([...row.phones_last4].sort()).toEqual([wa.slice(-4), "id"].sort());
    const text = JSON.stringify(r.body);
    for (const needle of [wa, user, user.slice(3), user.slice(-8)]) expect(text).not.toContain(needle);
  });

  it("no log line carries a user_id, a username or a phone on the BSUID paths", async () => {
    const t = setup();
    const city = await t.enrollCity("Hou Dynasty");
    const user = bsuid();
    const wa = phone();
    const stranger = bsuid();
    const newUser = bsuid();
    const newWa = phone();
    await t.joinAs(city, { user, username: "secret.username" }, { name: "Taylor Owner" });
    await t.sayAs({ wa, user, username: "secret.username" }, "the vault code is 4242");
    await t.sayAs({ user: stranger, username: "stranger.name" }, "who is this");
    t.graph.failNext({ status: 400, body: { error: { code: 131062, message: `BSUID recipients are not supported: ${user}` } } });
    await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "a private reply" });
    t.graph.failNext("network");
    await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "a private reply 2" });
    await t.send(city, { to: stranger, idempotency_key: uniq(), type: "text", text: "a private reply to a stranger" });
    await t.send(city, { to: `us.${user.slice(3)}`, idempotency_key: uniq(), type: "text", text: "a private reply, malformed" });
    const out = (await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a private reply 3" })).body.message_id;
    await t.webhook(statusAs(out, "delivered", { wa, user }, t.clock.now));
    await t.webhook(changeAs("user_changed_number", { from: wa, previousUser: user, newUser, newWa }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }));
    t.clock.now += 2 * DAY;
    await t.send(city, { to: newUser, idempotency_key: uniq(), type: "text", text: "a private reply, too late" });
    await t.gateway.maintain();
    const events = new Set(t.logs.map((e) => e.event));
    for (const e of ["webhook", "send", "send_failed", "send_refused"]) expect(events).toContain(e);
    const lines = t.logs.map((e) => JSON.stringify(e));
    for (const needle of [user, user.slice(3), stranger, stranger.slice(3), newUser, newUser.slice(3), wa, newWa, "secret.username", "stranger.name", "Taylor", "4242", "private reply"]) {
      for (const line of lines) expect(line).not.toContain(needle);
    }
  });
});

describe("BSUID: the operator's unbind, a notice naming no previous id, and an owner with no user id (gp-lv49)", () => {
  const template = { type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } };

  it("POST /v1/admin/unbind, by any id the owner is known by, deletes the binding and records every user id it answered to; an id no binding has unbinds nothing; a malformed body is 400; the log line names no id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    // By the conversation_id (its first user id, gp-g02t): an owner that changed number twice
    // answered to three user ids.
    const p = phone();
    const [a, a2, a3] = [bsuid(), bsuid(), bsuid()];
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    expect((await t.webhook(changeAs("user_changed_number", { from: p, previousUser: a, newUser: a2, newWa: phone() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    expect((await t.webhook(changeAs("user_changed_user_id", { previousUser: a2, newUser: a3 }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    t.clock.now = T0 + 2 * HOUR;
    const r = await t.admin("POST", "/v1/admin/unbind", { id: a });
    expect([r.status, r.body]).toEqual([200, { unbound: 1 }]);
    expect(await owners(a, a3)).toEqual([]);
    const recorded = await env.DB.prepare("SELECT user_id, unbound_at FROM unbinds WHERE user_id IN (?, ?, ?) ORDER BY user_id").bind(a, a2, a3).all();
    expect(recorded.results).toEqual([a, a2, a3].sort().map((user_id) => ({ user_id, unbound_at: T0 + 2 * HOUR })));
    // By the current user id, and by a PR 73 row's number (it records no user id: it has none).
    const b = bsuid();
    await t.joinAs(city, { user: b });
    expect((await t.admin("POST", "/v1/admin/unbind", { id: b })).body).toEqual({ unbound: 1 });
    const legacy = phone();
    await legacyBind(env.DB, city.id, legacy);
    expect((await t.admin("POST", "/v1/admin/unbind", { id: legacy })).body).toEqual({ unbound: 1 });
    expect(await owners(b, legacy)).toEqual([]);
    expect((await t.admin("POST", "/v1/admin/unbind", { id: bsuid() })).body).toEqual({ unbound: 0 });
    for (const body of [{}, { id: "" }, { id: 7 }, { id: "x".repeat(257) }]) {
      expect((await t.admin("POST", "/v1/admin/unbind", body)).status).toBe(400);
    }
    const lines = t.logs.filter((e) => e.event === "owner_unbound");
    expect(lines.map((e) => e.bindings)).toEqual([1, 1, 1, 0]);
    for (const line of lines.map((e) => JSON.stringify(e))) for (const needle of [p, a, a2, a3, b, legacy]) expect(line).not.toContain(needle);
  });

  it("a notice naming no previous user id reaches no city, links nothing and is counted, in notices_without_previous_id and unrouted_messages; the owner keeps its id until the mayor re-invites it", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const p = phone();
    const a = bsuid();
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    const bare = changeAs("user_changed_number", { from: p, previousUser: a, newUser: bsuid(), newWa: phone() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    delete bare.entry[0].changes[0].value.messages[0].system.previous_user_id;
    const before = { noPrevious: await t.stat("notices_without_previous_id"), unrouted: await t.stat("unrouted_messages") };
    expect((await t.webhook(bare)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
    expect(await t.stat("notices_without_previous_id")).toBe(before.noPrevious + 1);
    expect(await t.stat("unrouted_messages")).toBe(before.unrouted + 1);
    expect((await t.window(city)).body.windows.map((w: any) => w.user_id)).toEqual([a]);
    expect((await t.admin("GET", "/v1/admin/cities")).body.counters.notices_without_previous_id).toBeGreaterThanOrEqual(1);
    expect(t.logs.at(-1)).toMatchObject({ event: "webhook", unrouted: 1 });
  });

  it("a code text under a user id whose number-change notice already landed binds and is re-keyed along the waiting link: the owner answers to its newest id, and the welcome goes there (point 2: links in any order)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity("Hou Dynasty");
    const [p, q] = [phone(), phone()];
    const [a2, a3] = [bsuid(), bsuid()];
    const code = await t.invite(city);
    // The owner texts the code under a2 at T1; the write fails.
    t.clock.now = T0 + HOUR;
    const joining = textAs({ wa: p, user: a2 }, code, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    flaky.arm(0);
    expect((await t.webhook(joining)).status).toBe(500);
    // The owner's number change a2 -> a3 at T2 lands first: no binding answers to a2 yet, so its
    // link waits.
    t.clock.now = T0 + 2 * HOUR;
    expect((await t.webhook(changeAs("user_changed_number", { from: p, previousUser: a2, newUser: a3, newWa: q }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    // Meta's retry of the code text binds the owner, and the waiting link is reached.
    t.clock.now = T0 + 2 * HOUR + HOUR / 4;
    const calls = t.graph.sends().length;
    expect((await t.webhook(joining)).status).toBe(200);
    expect(await owners(a3)).toEqual([{ conversation_id: a2, shows: q, user_id: a3, username: null, city_id: city.id }]);
    expect(t.graph.sends().slice(calls).map((c) => [c.body?.recipient, c.body?.text?.body])).toEqual([[a3, "You're connected to Hou Dynasty's mayor. Say hi."]]);
    // The owner writes under its newest id: its own city, no canned line.
    t.clock.now = T0 + 3 * HOUR;
    const next = await t.sayAs({ wa: q, user: a3 }, "my new phone");
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === next.wamid)).toMatchObject({ conversation_id: a2, user_id: a3 });
    expect(t.graph.sends().slice(calls)).toHaveLength(1);
  });

  it("a replaced user id answers until its notice and never after: a message under it that Meta stamped after the notice reaches no city, hears nothing and is counted", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const p = phone();
    const [a, a2] = [bsuid(), bsuid()];
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    expect((await t.webhook(changeAs("user_changed_number", { from: p, previousUser: a, newUser: a2, newWa: phone() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    t.clock.now = T0 + 2 * HOUR;
    const unrouted = await t.stat("unrouted_messages");
    const calls = t.graph.sends().length;
    const after = await t.sayAs({ user: a }, "under the replaced id, after its notice");
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === after.wamid)).toBe(false);
    expect(await t.stat("unrouted_messages")).toBe(unrouted + 1);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("an unbind's record holds even when a later notice links its id to another binding (a retired id reissued): what Meta stamped before the unbind reaches no city", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const x = await t.enrollCity();
    const y = await t.enrollCity();
    const [a, b] = [bsuid(), bsuid()];
    await t.joinAs(y, { user: b }); // Y's owner, from T0
    await t.joinAs(x, { user: a }); // X's owner, from T0
    t.clock.now = T0 + HOUR;
    const m = textAs({ user: a }, "for X's mayor", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    flaky.arm(0);
    expect((await t.webhook(m)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    await t.unbind(a);
    // Meta's page does not say a retired user id is never issued again: Y's owner's notice names a.
    t.clock.now = T0 + 3 * HOUR;
    expect((await t.webhook(changeAs("user_changed_user_id", { previousUser: b, newUser: a }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    expect(await owners(a)).toEqual([{ conversation_id: b, shows: null, user_id: a, username: null, city_id: y.id }]);
    t.clock.now = T0 + 4 * HOUR;
    expect((await t.webhook(m)).status).toBe(200);
    const wamid = m.entry[0].changes[0].value.messages[0].id;
    for (const city of [x, y]) expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
  });

  it("a notice landing after an unbind extends the unbind's record to the id it reveals: what the owner wrote under the new id while bound binds nothing and reaches no city (codex self-review r1, finding 1)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const x = await t.enrollCity();
    const y = await t.enrollCity();
    const [p, q] = [phone(), phone()];
    const [a, a2] = [bsuid(), bsuid()];
    await t.joinAs(x, { wa: p, user: a });
    // A changes number at T1; the notice's write fails. Under a2 A then texts Y's live code (T2)
    // and sends an image (T3); both writes fail. X answered to a2 from T1, though the gateway
    // cannot know it yet.
    const ky = await t.invite(y);
    const change = changeAs("user_changed_number", { from: p, previousUser: a, newUser: a2, newWa: q }, { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    const code = textAs({ wa: q, user: a2 }, ky, { wamid: `wamid.${uniq()}`, ts: T0 + 2 * HOUR });
    const mediaId = `3${Date.now()}${Math.floor(Math.random() * 1_000_000)}`;
    t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
    const image = imageAs({ wa: q, user: a2 }, mediaId, { wamid: `wamid.${uniq()}`, ts: T0 + 3 * HOUR });
    for (const [m, at] of [[change, T0 + HOUR], [code, T0 + 2 * HOUR], [image, T0 + 3 * HOUR]] as const) {
      t.clock.now = at;
      flaky.arm(0);
      expect((await t.webhook(m)).status).toBe(500);
    }
    // X's operator unbinds A: the record names a, the only id the gateway knows.
    t.clock.now = T0 + 4 * HOUR;
    await t.unbind(a);
    // Meta retries all three, the notice first.
    t.clock.now = T0 + 5 * HOUR;
    for (const m of [change, code, image]) expect((await t.webhook(m)).status).toBe(200);
    expect(await env.DB.prepare("SELECT COUNT(*) AS n FROM phones WHERE city_id = ?").bind(y.id).first()).toEqual({ n: 0 });
    const wamids = [code, image].map((m) => m.entry[0].changes[0].value.messages[0].id);
    for (const city of [x, y]) {
      expect((await t.inbox(city)).body.deliveries.filter((d: any) => wamids.includes(d.message?.id))).toEqual([]);
      expect((await t.call("GET", `/v1/media/${mediaId}`, { token: city.token })).status).toBe(404);
    }
    expect(await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(ky).first()).toEqual({ used_at: null });
  });

  it("a chain of number changes of any length within the retry window still leads to its binding: no depth limit makes an owned id a stranger's (codex self-review r1, finding 3)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const x = await t.enrollCity();
    const y = await t.enrollCity();
    const ids = Array.from({ length: 21 }, () => bsuid());
    await t.joinAs(x, { user: ids[0]! });
    // Before any change: A's message to X and a code text for Y under the first id; both writes fail.
    const ky = await t.invite(y);
    const early = textAs({ user: ids[0]! }, "for X's mayor", { wamid: `wamid.${uniq()}`, ts: T0 + 60 });
    const code = textAs({ user: ids[0]! }, ky, { wamid: `wamid.${uniq()}`, ts: T0 + 120 });
    for (const m of [early, code]) {
      flaky.arm(0);
      expect((await t.webhook(m)).status).toBe(500);
    }
    // Twenty number changes, a minute apart.
    for (let i = 1; i < ids.length; i++) {
      t.clock.now = T0 + HOUR + i * 60;
      expect((await t.webhook(changeAs("user_changed_user_id", { previousUser: ids[i - 1]!, newUser: ids[i]! }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    }
    expect((await t.window(x)).body.windows.map((w: any) => w.user_id)).toEqual([ids.at(-1)]);
    t.clock.now = T0 + 2 * HOUR;
    for (const m of [early, code]) expect((await t.webhook(m)).status).toBe(200);
    const held = (await t.inbox(x)).body.deliveries.map((d: any) => d.message?.id);
    expect(held).toContain(early.entry[0].changes[0].value.messages[0].id);
    expect(await env.DB.prepare("SELECT COUNT(*) AS n FROM phones WHERE city_id = ?").bind(y.id).first()).toEqual({ n: 0 });
  });

  it("a notice routes by its previous_user_id alone, never by a contacts entry matched by its old number (codex self-review r2, finding 1)", async () => {
    const t = setup();
    const w = await t.enrollCity();
    const x = await t.enrollCity();
    const p = phone();
    const c = bsuid();
    await t.joinAs(w, { wa: p, user: c }); // C holds P in W
    const a = bsuid();
    await t.joinAs(x, { user: a }); // A in X
    for (const withPrevious of [true, false]) {
      const n = changeAs("user_changed_number", { from: p, previousUser: a, newUser: bsuid(), newWa: phone() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
      const value = n.entry[0].changes[0].value;
      value.contacts = [{ profile: { name: "C" }, wa_id: p, user_id: c }]; // another holder's entry for the old number
      if (!withPrevious) delete value.messages[0].system.previous_user_id;
      expect((await t.webhook(n)).status).toBe(200);
      const wamid = value.messages[0].id;
      expect((await t.inbox(w)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
      expect((await t.inbox(x)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(withPrevious);
    }
  });

  it("an unbind covers the earlier ids a late notice links to the unbound id: an invite text the owner sent under its earlier id binds nothing (codex self-review r2, finding 2)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const x = await t.enrollCity();
    const y = await t.enrollCity();
    const [a, b] = [bsuid(), bsuid()];
    const kx = await t.invite(x);
    const invite = textAs({ user: a }, kx, { wamid: `wamid.${uniq()}`, ts: T0 + 60 });
    const change = changeAs("user_changed_user_id", { previousUser: a, newUser: b }, { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    for (const [m, at] of [[invite, T0 + 60], [change, T0 + HOUR]] as const) {
      t.clock.now = at;
      flaky.arm(0);
      expect((await t.webhook(m)).status).toBe(500);
    }
    t.clock.now = T0 + 2 * HOUR;
    await t.joinAs(y, { user: b }); // under b, a stranger's until the notice lands (point 7)
    t.clock.now = T0 + 3 * HOUR;
    await t.unbind(b);
    t.clock.now = T0 + 4 * HOUR;
    for (const m of [change, invite]) expect((await t.webhook(m)).status).toBe(200);
    expect(await env.DB.prepare("SELECT COUNT(*) AS n FROM phones WHERE city_id = ?").bind(x.id).first()).toEqual({ n: 0 });
    expect(await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(kx).first()).toEqual({ used_at: null });
  });

  it("a re-key never follows a link stamped at or before its old id's unbind: a retired id reissued to another binding does not carry it on to the first owner's new id (codex self-review r2, finding 3)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const x = await t.enrollCity();
    const z = await t.enrollCity();
    const [a, a2, b] = [bsuid(), bsuid(), bsuid()];
    await t.joinAs(x, { user: a }); // A in X
    await t.joinAs(z, { user: b }); // B in Z
    t.clock.now = T0 + HOUR;
    const aChange = changeAs("user_changed_user_id", { previousUser: a, newUser: a2 }, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    flaky.arm(0);
    expect((await t.webhook(aChange)).status).toBe(500);
    t.clock.now = T0 + 2 * HOUR;
    await t.unbind(a);
    t.clock.now = T0 + 3 * HOUR;
    expect((await t.webhook(changeAs("user_changed_user_id", { previousUser: b, newUser: a }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    t.clock.now = T0 + 4 * HOUR;
    expect((await t.webhook(aChange)).status).toBe(200);
    expect((await t.window(z)).body.windows.map((w: any) => w.user_id)).toEqual([a]);
    const mine = await t.sayAs({ user: a2 }, "A, under its own new id");
    expect((await t.inbox(z)).body.deliveries.some((d: any) => d.message?.id === mine.wamid)).toBe(false);
  });

  it("the cron never erases an unbind's reach while a message it decides can still arrive: an expiring link leaves its component's floor on both its ids (codex self-review r3, finding 1)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const x = await t.enrollCity();
    const y = await t.enrollCity();
    const [a, b] = [bsuid(), bsuid()];
    await t.joinAs(x, { user: a });
    const change = changeAs("user_changed_user_id", { previousUser: a, newUser: b }, { wamid: `wamid.${uniq()}`, ts: T0 + DAY });
    t.clock.now = T0 + DAY;
    flaky.arm(0);
    expect((await t.webhook(change)).status).toBe(500);
    t.clock.now = T0 + 3 * DAY;
    const ky = await t.invite(y);
    const code = textAs({ user: b }, ky, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    flaky.arm(0);
    expect((await t.webhook(code)).status).toBe(500);
    t.clock.now = T0 + 4 * DAY;
    await t.unbind(a);
    t.clock.now = T0 + 5 * DAY;
    expect((await t.webhook(change)).status).toBe(200);
    t.clock.now = T0 + 9 * DAY;
    await t.gateway.maintain(); // the link (stamped day 1) is past its 8 days
    t.clock.now = T0 + 9 * DAY + HOUR;
    expect((await t.webhook(code)).status).toBe(200); // stamped day 3: within its own 8 days
    expect(await env.DB.prepare("SELECT COUNT(*) AS n FROM phones WHERE city_id = ?").bind(y.id).first()).toEqual({ n: 0 });
  });

  it("links that loop (a -> b and b -> a, neither bound) leave a binding on its own id, however Meta replays them (codex self-review r3, finding 2)", async () => {
    const t = setup();
    const x = await t.enrollCity();
    const [a, b] = [bsuid(), bsuid()];
    t.clock.now = T0 + HOUR;
    const ab = changeAs("user_changed_user_id", { previousUser: a, newUser: b }, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const ba = changeAs("user_changed_user_id", { previousUser: b, newUser: a }, { wamid: `wamid.${uniq()}`, ts: t.clock.now + 60 });
    for (const n of [ab, ba]) expect((await t.webhook(n)).status).toBe(200);
    t.clock.now = T0 + 2 * HOUR;
    await t.joinAs(x, { user: a });
    const current = async () => (await t.window(x)).body.windows.map((w: any) => w.user_id);
    expect(await current()).toEqual([a]);
    for (const n of [ba, ab, ba]) {
      expect((await t.webhook(n)).status).toBe(200);
      expect(await current()).toEqual([a]);
    }
  });

  it("a notice naming no id at all (no `from`, no previous_user_id) is counted too, in notices_without_previous_id and unrouted_messages (fresh review r1, F3)", async () => {
    const t = setup();
    await t.enrollCity();
    const bare = changeAs("user_changed_user_id", { previousUser: bsuid(), newUser: bsuid() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    delete bare.entry[0].changes[0].value.messages[0].system.previous_user_id;
    const before = { noPrevious: await t.stat("notices_without_previous_id"), unrouted: await t.stat("unrouted_messages") };
    expect((await t.webhook(bare)).status).toBe(200);
    expect(await t.stat("notices_without_previous_id")).toBe(before.noPrevious + 1);
    expect(await t.stat("unrouted_messages")).toBe(before.unrouted + 1);
  });

  it("a payload carrying the owner's first message under its new id before the notice that links it still reaches the owner's city: a payload's notices are planned first (fresh review r1, F4)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const [p, q] = [phone(), phone()];
    const [a, a2] = [bsuid(), bsuid()];
    await t.joinAs(city, { wa: p, user: a });
    t.clock.now = T0 + HOUR;
    const both = textAs({ wa: q, user: a2 }, "my new phone", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const notice = changeAs("user_changed_number", { from: p, previousUser: a, newUser: a2, newWa: q }, { wamid: `wamid.${uniq()}`, ts: t.clock.now - 60 });
    both.entry[0].changes[0].value.messages.push(notice.entry[0].changes[0].value.messages[0]);
    const calls = t.graph.sends().length;
    expect((await t.webhook(both)).status).toBe(200);
    const text = both.entry[0].changes[0].value.messages[0].id;
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === text)).toMatchObject({ conversation_id: a, user_id: a2 });
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a notice whose previous user id no binding answers to reaches no city, hears nothing and is counted", async () => {
    const t = setup();
    await t.enrollCity();
    const unrouted = await t.stat("unrouted_messages");
    const calls = t.graph.sends().length;
    expect((await t.webhook(changeAs("user_changed_number", { from: phone(), previousUser: bsuid(), newUser: bsuid(), newWa: phone() }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
    expect(await t.stat("unrouted_messages")).toBe(unrouted + 1);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("an owner PR 73 bound by its number alone has no address: a send to it is 403 not_bound and never reaches Meta", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    await legacyBind(env.DB, city.id, wa);
    const calls = t.graph.sends().length;
    const r = await t.send(city, { to: wa, idempotency_key: uniq(), ...template });
    expect([r.status, r.body]).toEqual([403, { error: "not_bound" }]);
    expect(t.graph.sends()).toHaveLength(calls);
  });
});

describe("BSUID: migrations 0002 and 0003 over schema v1 rows", () => {
  it("a v1 phone keeps its wa_id as its key, a held delivery gains its conversation_id, a stranger's row keeps its key, user_id is unique where present, and a wamid is one row whichever city", async () => {
    const db = env.MIGRATION_DB;
    const all = env.TEST_MIGRATIONS;
    expect(all.map((m) => m.name)).toEqual(["0001_init.sql", "0002_bsuid.sql", "0003_retries.sql", "0004_uploads.sql", "0005_send_attempt.sql"]);
    await applyD1Migrations(db, all.slice(0, 1));
    const wa = phone();
    await db.batch([
      db.prepare("INSERT INTO cities (id, name, box_name, created_at) VALUES ('city_m', 'M', 'm-box', 1)"),
      db
        .prepare("INSERT INTO phones (wa_id, city_id, profile_name, bound_at, bound_wamid, last_inbound_at) VALUES (?, 'city_m', 'Ana', 10, 'wamid.B', 20)")
        .bind(wa),
      db
        .prepare("INSERT INTO deliveries (city_id, received_at, kind, wamid, status, wa_id, profile_name, body) VALUES ('city_m', 30, 'message', 'wamid.H', '', ?, 'Ana', '{}')")
        .bind(wa),
      db.prepare(
        "INSERT INTO deliveries (city_id, received_at, kind, wamid, status, wa_id, profile_name, body, acked_at) VALUES ('city_m', 31, 'message', 'wamid.A', '', NULL, NULL, NULL, 32)",
      ),
      db.prepare("INSERT INTO unknown_senders (wa_id, last_replied_at) VALUES ('5511000000000', 40)"),
      db.prepare("INSERT INTO sends (city_id, idempotency_key, wamid, state, type, created_at) VALUES ('city_m', 'k1', 'wamid.S', 'sent', 'text', 50)"),
      db.prepare("INSERT INTO orphan_statuses (wamid, status, received_at, recipient_id, body) VALUES ('wamid.O', 'sent', 60, ?, '{}')").bind(wa),
    ]);
    await applyD1Migrations(db, all);
    expect((await db.prepare("SELECT * FROM phones").all()).results).toEqual([
      {
        conversation_id: wa,
        wa_id: wa,
        user_id: null,
        username: null,
        city_id: "city_m",
        profile_name: "Ana",
        bound_at: 10,
        bound_wamid: "wamid.B",
        last_inbound_at: 20,
        // 0003: a row bound before it has no invite stamp; it holds no user id, so nothing routes to it.
        bound_stamp: null,
      },
    ]);
    expect((await db.prepare("SELECT wamid, wa_id, user_id, username, conversation_id FROM deliveries ORDER BY id").all()).results).toEqual([
      { wamid: "wamid.H", wa_id: wa, user_id: null, username: null, conversation_id: wa },
      { wamid: "wamid.A", wa_id: null, user_id: null, username: null, conversation_id: null },
    ]);
    expect((await db.prepare("SELECT sender_key, last_replied_at FROM unknown_senders").all()).results).toEqual([
      { sender_key: "5511000000000", last_replied_at: 40 },
    ]);
    expect((await db.prepare("SELECT idempotency_key, conversation_id, activity_attempt_id FROM sends").all()).results).toEqual([
      { idempotency_key: "k1", conversation_id: null, activity_attempt_id: null },
    ]);
    expect((await db.prepare("SELECT wamid, recipient_id, recipient_user_id FROM orphan_statuses").all()).results).toEqual([
      { wamid: "wamid.O", recipient_id: wa, recipient_user_id: null },
    ]);
    // user_id is UNIQUE where present: a second owner without one is fine, a second with the same one is refused.
    const user = bsuid();
    const insert = (key: string, waId: string | null, userId: string | null, wamid: string) =>
      db
        .prepare("INSERT INTO phones (conversation_id, wa_id, user_id, city_id, bound_at, bound_wamid) VALUES (?, ?, ?, 'city_m', 1, ?)")
        .bind(key, waId, userId, wamid)
        .run();
    const other = phone();
    await insert(other, other, null, "wamid.2");
    await insert(user, null, user, "wamid.3");
    await expect(insert("k4", null, user, "wamid.4")).rejects.toThrow(/UNIQUE/);
    await expect(insert("k5", wa, null, "wamid.5")).rejects.toThrow(/UNIQUE/);
    // 0003 over the same rows: the rebuilt dedup key refuses a second city's copy of a held wamid,
    // still takes another status of it, and its new tables are empty.
    const copy = (city: string, wamid: string, status: string) =>
      db
        .prepare("INSERT INTO deliveries (city_id, received_at, kind, wamid, status, body) VALUES (?, 70, ?, ?, ?, '{}')")
        .bind(city, status ? "status" : "message", wamid, status)
        .run();
    await expect(copy("city_other", "wamid.H", "")).rejects.toThrow(/UNIQUE/);
    await expect(copy("city_m", "wamid.H", "")).rejects.toThrow(/UNIQUE/);
    await copy("city_m", "wamid.S", "sent");
    await expect(copy("city_other", "wamid.S", "sent")).rejects.toThrow(/UNIQUE/);
    for (const table of ["links", "unbinds", "marks", "code_attempt_wamids"]) {
      expect(await db.prepare(`SELECT COUNT(*) AS n FROM ${table}`).first()).toEqual({ n: 0 });
    }
    await expect(
      db.prepare("INSERT INTO phones (wa_id, city_id, bound_at, bound_wamid) VALUES (?, 'city_m', 1, 'wamid.6')").bind(phone()).run(),
    ).rejects.toThrow(/NOT NULL/);
  });

  it("0003 carries each stranger's last counted code text into the retry dedup (gp-w376); a stranger is keyed by its user id now (gp-lv49), so Meta's redelivery of it counts once under that id, however often it comes, and the v1 count under the phone is left alone", async () => {
    const db = env.BACKFILL_DB;
    const all = env.TEST_MIGRATIONS;
    await applyD1Migrations(db, all.slice(0, 1));
    // Two strangers with four wrong codes each in the open day, as the deployed (v1) Worker
    // writes them: the count, the day's start, and the wamid of the last code text it counted.
    // A third stranger heard only the canned line.
    const since = T0 - 2 * HOUR;
    const s1 = phone();
    const s2 = phone();
    const w1 = `wamid.${uniq()}`;
    const w2 = `wamid.${uniq()}`;
    const v1Stranger = db.prepare("INSERT INTO unknown_senders (wa_id, code_attempts, attempts_since, last_code_wamid) VALUES (?, 4, ?, ?)");
    await db.batch([
      v1Stranger.bind(s1, since, w1),
      v1Stranger.bind(s2, since, w2),
      db.prepare("INSERT INTO unknown_senders (wa_id, last_replied_at) VALUES (?, ?)").bind(phone(), since),
    ]);
    await applyD1Migrations(db, all);
    const t = setup({ db });
    const city = await t.enrollCity();
    const attempts = async (key: string) =>
      (await db.prepare("SELECT code_attempts FROM unknown_senders WHERE sender_key = ?").bind(key).first<{ code_attempts: number }>())?.code_attempts;
    const bound = async (user: string) =>
      (await db.prepare("SELECT COUNT(*) AS n FROM phones WHERE user_id = ?").bind(user).first<{ n: number }>())?.n;
    const wrongFor = (code: string) => String((Number(code) + 1) % 1_000_000).padStart(6, "0");
    const code = await t.invite(city);
    // Meta redelivers each one's fourth after the deploy, naming the sender's user id (its shape
    // since April 2026): one attempt under that id, however often it comes; the v1 count, keyed
    // by the phone, is left as it was.
    for (const [s, w] of [
      [s1, w1],
      [s2, w2],
    ] as const) {
      for (let i = 0; i < 2; i++) expect((await t.say(s, wrongFor(code), { wamid: w, ts: since + HOUR })).res.status).toBe(200);
      expect(await attempts(userOf(s))).toBe(1);
      expect(await attempts(s)).toBe(4);
    }
    // s1's next text is the right code: two attempts under its user id, and it binds.
    await t.say(s1, code);
    expect(await attempts(userOf(s1))).toBe(2);
    expect(await bound(userOf(s1))).toBe(1);
    expect((await t.inbox(city)).body.deliveries.map((d: any) => [d.kind, d.user_id])).toEqual([["joined", userOf(s1)]]);
    // The migration counted each last wamid no later than a day after its day's start (the v1
    // Worker opens a new day for an attempt a day after the last one opened), so the cron keeps
    // it at least as long as a row the gateway writes when it counts; a redelivery keeps it.
    expect(
      (await db.prepare("SELECT wamid, counted_at FROM code_attempt_wamids WHERE wamid IN (?, ?) ORDER BY wamid").bind(w1, w2).all()).results,
    ).toEqual([w1, w2].sort().map((wamid) => ({ wamid, counted_at: since + DAY })));
  });
});
