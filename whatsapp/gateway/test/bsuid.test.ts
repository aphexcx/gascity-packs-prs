/**
 * Meta's business-scoped user ids (bead gp-ac0u) on a real D1 in workerd with a fake Graph API. An
 * owner is known by a user_id (a BSUID), a wa_id, or both. The shapes are Meta's (test/fixtures/
 * SOURCES.md, "Business-scoped user ids"); the rules are README.md's "Business-scoped user ids".
 */
import { applyD1Migrations, env } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import { bsuid, changeAs, DAY, HOUR, phone, setup, statusAs, statusPayload, T0, textAs, uniq } from "./harness.js";

const CLOSE = DAY - 120;
const CANNED = "This number serves Dynasty cities. Ask your city for an invite code.";

/** The owner rows naming any of these ids, as D1 holds them. */
async function owners(...ids: string[]) {
  const { results } = await env.DB.prepare(
    `SELECT conversation_id, wa_id, user_id, username, city_id FROM phones
     WHERE conversation_id IN (SELECT value FROM json_each(?1)) OR wa_id IN (SELECT value FROM json_each(?1))
        OR user_id IN (SELECT value FROM json_each(?1))
     ORDER BY bound_at, conversation_id`,
  )
    .bind(JSON.stringify(ids))
    .all();
  return results;
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
    expect(await owners(user)).toEqual([{ conversation_id: user, wa_id: null, user_id: user, username: "taylor.hou", city_id: a.id }]);
    expect((await t.window(a)).body.windows).toEqual([{ conversation_id: user, user_id: user, last_inbound_at: T0, window_closes_at: T0 + CLOSE }]);
  });

  it("an owner bound with both ids is one row keyed by its wa_id; a later message carrying only its user_id routes to it and moves its window", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const welcome = t.graph.sends().at(-1)!.body!;
    expect(welcome.to).toBe(wa);
    expect("recipient" in welcome).toBe(false);
    t.clock.now = T0 + 2 * HOUR;
    const { wamid } = await t.sayAs({ user }, "no phone this time");
    const d = (await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === wamid);
    expect(d).toMatchObject({ kind: "message", wa_id: null, user_id: user, conversation_id: wa });
    expect(await owners(wa, user)).toEqual([{ conversation_id: wa, wa_id: wa, user_id: user, username: null, city_id: city.id }]);
    expect((await t.window(city)).body.windows).toEqual([
      { conversation_id: wa, user_id: user, last_inbound_at: T0 + 2 * HOUR, window_closes_at: T0 + 2 * HOUR + CLOSE },
    ]);
  });

  it("an owner bound by wa_id alone gains its user_id from the first message carrying both, never a second row; a user_id-only message then routes to it", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city); // today's shape: no user_id anywhere
    const user = bsuid();
    expect(await owners(wa)).toEqual([{ conversation_id: wa, wa_id: wa, user_id: null, username: null, city_id: city.id }]);
    const both = await t.sayAs({ wa, user, username: "owner.one" }, "both ids");
    expect((await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === both.wamid)).toMatchObject({
      wa_id: wa,
      user_id: user,
      username: "owner.one",
      conversation_id: wa,
    });
    expect(await owners(wa, user)).toEqual([{ conversation_id: wa, wa_id: wa, user_id: user, username: "owner.one", city_id: city.id }]);
    const only = await t.sayAs({ user }, "only the BSUID");
    expect((await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === only.wamid)).toMatchObject({
      wa_id: null,
      user_id: user,
      conversation_id: wa,
    });
    expect(t.graph.sends().filter((c) => c.body?.recipient === user)).toHaveLength(0); // the owner, never a stranger
  });

  it("the window moves on a message from either id: a user_id-only message reopens it for a send to the wa_id, a wa_id-only one for a send to the BSUID", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    t.clock.now = T0 + DAY + HOUR;
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "closed" })).status).toBe(422);
    await t.sayAs({ user }, "back");
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "by phone" })).status).toBe(200);
    t.clock.now += DAY + HOUR;
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "closed again" })).status).toBe(422);
    await t.say(wa, "by phone, today's shape");
    expect((await t.send(city, { to: user, idempotency_key: uniq(), type: "text", text: "by BSUID" })).status).toBe(200);
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(t.clock.now);
  });

  it("a message from an owner's phone carrying another user_id is not the owner (the number is someone else's now): never routed, it hears the canned line", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const owner = bsuid();
    await t.joinAs(city, { wa, user: owner });
    const { wamid } = await t.sayAs({ wa, user: bsuid() }, "is this the pizza place?");
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
    expect(t.graph.sends().filter((c) => c.body?.to === wa && c.body?.text?.body === CANNED)).toHaveLength(1);
    expect(await owners(wa)).toEqual([{ conversation_id: wa, wa_id: wa, user_id: owner, username: null, city_id: city.id }]);
  });

  it("one person bound twice (once by phone, once by BSUID): a message carrying both ids goes to the user_id's row, takes no id from the other, and Meta gets 200", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const wa = await t.join(a); // today's shape: no user_id
    const user = bsuid();
    await t.joinAs(b, { user });
    const { res, wamid } = await t.sayAs({ wa, user }, "which city hears this?");
    expect(res.status).toBe(200);
    expect((await t.inbox(b)).body.deliveries.find((d: any) => d.message?.id === wamid)).toMatchObject({ conversation_id: user });
    expect((await t.inbox(a)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
    expect(await owners(wa, user)).toEqual([
      { conversation_id: wa, wa_id: wa, user_id: null, username: null, city_id: a.id },
      { conversation_id: user, wa_id: null, user_id: user, username: null, city_id: b.id },
    ]);
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
  it("a send to a BSUID reaches Meta in `recipient`, byte for byte and with no `to`; a send to the wa_id goes in `to`", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    await t.joinAs(city, { wa, user });
    const bodies = [
      { type: "text", text: "hello by BSUID" },
      { type: "reaction", reaction: { message_id: "wamid.X", emoji: "\u{1F44D}" } },
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
    expect(byPhone.to).toBe(wa);
    expect("recipient" in byPhone).toBe(false);
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
    expect(toStranger().at(-1)!.body).toMatchObject({ to: wa, text: { body: CANNED } });
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
    expect(statuses.map((d: any) => [d.message.id, d.message.status, d.wa_id, d.user_id, d.conversation_id, d.username, d.profile_name])).toEqual([
      [byUser, "delivered", null, user, wa, null, null],
      [byPhone, "read", wa, user, wa, null, null],
      [byPhone, "failed", wa, null, wa, null, null],
      [raced, "sent", null, user, wa, null, null],
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
    expect(notice).toMatchObject({ kind: "message", wa_id: wa, user_id: user, conversation_id: wa });
    expect(notice.message).toMatchObject({
      type: "system",
      system: { type: "user_changed_number", user_id: newUser, previous_user_id: user, wa_id: newWa },
    });
    expect((await t.inbox(other)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
    expect(t.graph.sends()).toHaveLength(calls); // no canned line and no welcome
    expect((await t.window(city)).body.windows).toEqual([{ conversation_id: wa, user_id: newUser, last_inbound_at: T0, window_closes_at: T0 + CLOSE }]);
    expect(await owners(wa, newWa, user, newUser)).toEqual([{ conversation_id: wa, wa_id: newWa, user_id: newUser, username: null, city_id: city.id }]);

    t.clock.now = T0 + 2 * HOUR;
    const next = await t.sayAs({ wa: newWa, user: newUser }, "my new phone");
    expect((await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === next.wamid)).toMatchObject({
      wa_id: newWa,
      user_id: newUser,
      conversation_id: wa,
    });
    // The box still holds the old number as the conversation id: the send goes to the owner's current number.
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "got it" })).status).toBe(200);
    expect(t.graph.sends().at(-1)!.body).toMatchObject({ to: newWa, text: { body: "got it" } });
    expect(t.graph.sends().some((c) => c.body?.to === wa && c.body?.text?.body === "got it")).toBe(false);
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
    expect(await owners(user, newUser)).toEqual([{ conversation_id: user, wa_id: null, user_id: newUser, username: null, city_id: city.id }]);
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

  it("an owner known by phone and BSUID whose number changes with no phone shared keeps no phone: its old number is never sent to", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = phone();
    const user = bsuid();
    const newUser = bsuid();
    await t.joinAs(city, { wa, user });
    await t.webhook(changeAs("user_changed_user_id", { previousUser: user, newUser }, { wamid: `wamid.${uniq()}`, ts: t.clock.now }));
    expect(await owners(wa, user, newUser)).toEqual([{ conversation_id: wa, wa_id: null, user_id: newUser, username: null, city_id: city.id }]);
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "after the change" })).status).toBe(200);
    const sent = t.graph.sends().at(-1)!.body!;
    expect(sent.recipient).toBe(newUser);
    expect("to" in sent).toBe(false);
    const r = await t.admin("GET", "/v1/admin/cities");
    expect(r.body.cities.find((c: any) => c.id === city.id).phones_last4).toEqual(["id"]);
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
    await t.ack(city, [joined, acked]);
    t.clock.now += 7 * DAY;
    await t.gateway.maintain();
    const { results } = await env.DB.prepare(
      "SELECT body, wa_id, user_id, username, conversation_id, profile_name FROM deliveries WHERE city_id = ?",
    )
      .bind(city.id)
      .all();
    expect(results).toHaveLength(3);
    for (const row of results) expect(row).toEqual({ body: null, wa_id: null, user_id: null, username: null, conversation_id: null, profile_name: null });
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

describe("BSUID: migration 0002 over schema v1 rows", () => {
  it("a v1 phone keeps its wa_id as its key, a held delivery gains its conversation_id, a stranger's row keeps its key, and user_id is unique where present", async () => {
    const db = env.MIGRATION_DB;
    const all = env.TEST_MIGRATIONS;
    expect(all.map((m) => m.name)).toEqual(["0001_init.sql", "0002_bsuid.sql"]);
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
      { conversation_id: wa, wa_id: wa, user_id: null, username: null, city_id: "city_m", profile_name: "Ana", bound_at: 10, bound_wamid: "wamid.B", last_inbound_at: 20 },
    ]);
    expect((await db.prepare("SELECT wamid, wa_id, user_id, username, conversation_id FROM deliveries ORDER BY id").all()).results).toEqual([
      { wamid: "wamid.H", wa_id: wa, user_id: null, username: null, conversation_id: wa },
      { wamid: "wamid.A", wa_id: null, user_id: null, username: null, conversation_id: null },
    ]);
    expect((await db.prepare("SELECT sender_key, last_replied_at FROM unknown_senders").all()).results).toEqual([
      { sender_key: "5511000000000", last_replied_at: 40 },
    ]);
    expect((await db.prepare("SELECT idempotency_key, conversation_id FROM sends").all()).results).toEqual([{ idempotency_key: "k1", conversation_id: null }]);
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
    await expect(
      db.prepare("INSERT INTO phones (wa_id, city_id, bound_at, bound_wamid) VALUES (?, 'city_m', 1, 'wamid.6')").bind(phone()).run(),
    ).rejects.toThrow(/NOT NULL/);
  });
});
