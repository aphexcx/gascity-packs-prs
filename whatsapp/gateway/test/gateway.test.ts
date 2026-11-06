/**
 * The gateway's contract v1 (README.md, "The contract"), route by route, on a real D1 in workerd
 * with a fake Graph API. Every case of the bead's test list is here, with the 11/4 additions (the
 * hold until the ack by id, the no-op ack of a deleted id, the no-op repeated ack).
 */
import { createScheduledController, env, SELF } from "cloudflare:test";
import { describe, expect, it, vi } from "vitest";
import { HOLD_SECONDS, MAX_BODY_BYTES, MAX_PENDING } from "../src/gateway.js";
import { signatureHeader } from "../src/meta.js";
import worker from "../src/index.js";
import {
  bsuid,
  DAY,
  flakyDb,
  HOUR,
  mediaPayload,
  PNID,
  phone,
  reactionPayload,
  SECRETS,
  setup,
  statusPayload,
  T0,
  textAs,
  textPayload,
  uniq,
  userOf,
} from "./harness.js";

const CLOSE = DAY - 120; // the window's length after the margin

describe("Meta side: the verify handshake", () => {
  it("echoes hub.challenge only for the verify token", async () => {
    const t = setup();
    const ok = await t.call("GET", `/meta/webhook?hub.mode=subscribe&hub.verify_token=${SECRETS.verify}&hub.challenge=1158201444`);
    expect(ok.status).toBe(200);
    expect(ok.body).toBe("1158201444");
    const bad = await t.call("GET", "/meta/webhook?hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=1158201444");
    expect(bad.status).toBe(403);
    const mode = await t.call("GET", `/meta/webhook?hub.mode=unsubscribe&hub.verify_token=${SECRETS.verify}&hub.challenge=1`);
    expect(mode.status).toBe(403);
  });

  it("refuses the handshake while no verify token is configured", async () => {
    const t = setup({ config: { metaVerifyToken: "" } });
    const r = await t.call("GET", "/meta/webhook?hub.mode=subscribe&hub.verify_token=&hub.challenge=1");
    expect(r.status).toBe(403);
  });
});

describe("Meta side: the signature", () => {
  it("a tampered byte answers 401 and stores nothing", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const before = (await t.held(city)).length;
    const payload = textPayload(wa, "hello", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const raw = new TextEncoder().encode(JSON.stringify(payload));
    const good = await signatureHeader(raw, SECRETS.app);
    // Tamper inside the message text, so the payload stays one the gateway would store: only the
    // signature check stands between it and the inbox.
    const at = new TextDecoder().decode(raw).indexOf('"hello"') + 1;
    expect(at).toBeGreaterThan(0);
    raw[at] = "j".charCodeAt(0);
    const r = await t.call("POST", "/meta/webhook", { raw, headers: { "x-hub-signature-256": good } });
    expect(r.status).toBe(401);
    expect((await t.held(city)).length).toBe(before);
    // The same tampered bytes signed properly would have been stored (the check is what refused it).
    const resigned = await t.call("POST", "/meta/webhook", { raw, headers: { "x-hub-signature-256": await signatureHeader(raw, SECRETS.app) } });
    expect(resigned.status).toBe(200);
    expect((await t.held(city)).length).toBe(before + 1);
  });

  it("a missing header answers 401 and stores nothing", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const before = (await t.held(city)).length;
    const r = await t.webhook(textPayload(wa, "hello", { wamid: `wamid.${uniq()}`, ts: t.clock.now }), null);
    expect(r.status).toBe(401);
    expect((await t.held(city)).length).toBe(before);
  });

  it("a body over 1.9 MB answers 413", async () => {
    const t = setup();
    const big = new Uint8Array(MAX_BODY_BYTES + 1).fill(0x20);
    const r = await t.call("POST", "/meta/webhook", { raw: big, headers: { "x-hub-signature-256": "sha256=00" } });
    expect(r.status).toBe(413);
  });
});

describe("Meta side: routing by phone", () => {
  it("a payload for another phone number id is dropped and counted", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const before = (await t.held(city)).length;
    const counted = async () =>
      (await env.DB.prepare("SELECT value FROM stats WHERE name = 'dropped_wrong_number'").first<{ value: number }>())?.value ?? 0;
    const was = await counted();
    const r = await t.webhook(textPayload(wa, "hi", { wamid: `wamid.${uniq()}`, ts: t.clock.now, pnid: "999999999999999" }));
    expect(r.status).toBe(200);
    expect((await t.held(city)).length).toBe(before);
    expect(await counted()).toBe(was + 1);
  });

  it("a bound sender's text lands in exactly its city's inbox and in no other", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const waA = await t.join(a);
    await t.join(b);
    const { res, wamid } = await t.say(waA, "only for A", { name: "Ana" });
    expect(res.status).toBe(200);
    const inA = (await t.inbox(a)).body.deliveries.filter((d: any) => d.kind === "message");
    expect(inA).toHaveLength(1);
    expect(inA[0]).toMatchObject({ kind: "message", wa_id: waA, profile_name: "Ana" });
    expect(inA[0].message).toMatchObject({ id: wamid, type: "text", text: { body: "only for A" } });
    const inB = (await t.inbox(b)).body.deliveries;
    expect(inB.some((d: any) => d.message?.id === wamid)).toBe(false);
  });

  it("a live invite code binds the phone, marks the code used, appends the joined entry and sends the welcome once", async () => {
    const t = setup();
    const city = await t.enrollCity("Hou Dynasty");
    const code = await t.invite(city);
    const wa = phone();
    const { res, wamid } = await t.say(wa, ` ${code} `, { name: "Taylor" });
    expect(res.status).toBe(200);
    const deliveries = (await t.inbox(city)).body.deliveries;
    expect(deliveries).toHaveLength(1);
    expect(deliveries[0]).toMatchObject({ kind: "joined", wa_id: wa, profile_name: "Taylor" });
    expect(deliveries[0].message.id).toBe(wamid);
    const used = await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(code).first<{ used_at: number }>();
    expect(used?.used_at).toBe(t.clock.now);
    const welcome = t.graph.sends();
    expect(welcome).toHaveLength(1);
    expect(welcome[0]!.body).toMatchObject({ recipient: userOf(wa), type: "text", text: { body: "You're connected to Hou Dynasty's mayor. Say hi." } });
    // The code is spent: a second phone texting it is a stranger, who hears the canned line and
    // never a welcome.
    const other = phone();
    await t.say(other, code);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(1);
    expect(t.graph.sends().filter((c) => c.body?.recipient === userOf(other)).map((c) => c.body?.text.body)).toEqual([
      "This number serves Dynasty cities. Ask your city for an invite code.",
    ]);
  });

  it("a payload carrying the invite text twice binds once and welcomes once", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wa = phone();
    const wamid = `wamid.${uniq()}`;
    const payload = textPayload(wa, code, { wamid, ts: t.clock.now });
    const value = payload.entry[0].changes[0].value;
    value.messages.push(JSON.parse(JSON.stringify(value.messages[0])));
    expect((await t.webhook(payload)).status).toBe(200);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(1);
    expect(t.graph.sends().filter((c) => c.body?.recipient === userOf(wa))).toHaveLength(1);
  });

  it("an unknown sender gets the canned line once and silence on the next message within 7 days", async () => {
    const t = setup();
    const stranger = phone();
    await t.say(stranger, "hello?");
    await t.say(stranger, "anyone?");
    t.clock.now += 6 * DAY;
    await t.say(stranger, "still there?");
    const lines = t.graph.sends().filter((c) => c.body?.recipient === userOf(stranger));
    expect(lines).toHaveLength(1);
    expect(lines[0]!.body?.text.body).toBe("This number serves Dynasty cities. Ask your city for an invite code.");
    t.clock.now += DAY + 1;
    await t.say(stranger, "a week later");
    expect(t.graph.sends().filter((c) => c.body?.recipient === userOf(stranger))).toHaveLength(2);
  });

  it("a Meta redelivery of the same wamid yields one delivery", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const { wamid } = await t.say(wa, "once");
    const again = await t.say(wa, "once", { wamid });
    expect(again.res.status).toBe(200);
    const rows = (await t.inbox(city)).body.deliveries.filter((d: any) => d.message?.id === wamid);
    expect(rows).toHaveLength(1);
  });

  it("a message Meta stamped longer ago than any dedup row lives (8 days) is dropped and counted; 7.5 days old still lands", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.clock.now = T0 + 9 * DAY;
    const old = await t.say(wa, "from long ago", { ts: T0 + DAY - 1 });
    expect(old.res.status).toBe(200);
    const late = await t.say(wa, "a late first delivery", { ts: T0 + 9 * DAY - Math.floor(7.5 * DAY) });
    const ids = (await t.inbox(city)).body.deliveries.map((d: any) => d.message?.id);
    expect(ids).not.toContain(old.wamid);
    expect(ids).toContain(late.wamid);
    const stale = await env.DB.prepare("SELECT value FROM stats WHERE name = 'stale_messages'").first<{ value: number }>();
    expect(stale?.value ?? 0).toBeGreaterThanOrEqual(1);
  });

  it("a payload too big for one D1 batch lands whole, every message once", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const payload = textPayload(wa, "m0", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const value = payload.entry[0].changes[0].value;
    const first = value.messages[0];
    for (let i = 1; i < 250; i++) value.messages.push({ ...first, id: `wamid.${uniq()}`, text: { body: `m${i}` } });
    expect((await t.webhook(payload)).status).toBe(200);
    expect((await t.webhook(payload)).status).toBe(200);
    const got = [
      ...(await t.inbox(city, "?limit=200")).body.deliveries,
      ...(await t.inbox(city, "?limit=200&after=" + (await t.inbox(city, "?limit=200")).body.cursor)).body.deliveries,
    ].filter((d: any) => d.kind === "message");
    expect(got.map((d: any) => d.message.text.body)).toEqual(Array.from({ length: 250 }, (_, i) => `m${i}`));
  });

  it("a split payload whose second batch fails: one message's bind is never cut in two, and the redelivery binds once and welcomes once", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const owner = await t.join(city);
    const code = await t.invite(city);
    const stranger = phone();
    // 99 messages of a bound owner (4 statements each since gp-0g2s = 396), then a stranger's
    // invite text whose 10 statements would straddle a cut at 400. Re-cut these counts whenever a
    // message's statements change: the mutation check's "naive batch slicing" survives otherwise.
    const payload = textPayload(owner, "b0", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const value = payload.entry[0].changes[0].value;
    const first = value.messages[0];
    for (let i = 1; i < 99; i++) value.messages.push({ ...first, id: `wamid.${uniq()}`, text: { body: `b${i}` } });
    const inviteWamid = `wamid.${uniq()}`;
    value.messages.push({ ...first, from: stranger, from_user_id: userOf(stranger), id: inviteWamid, text: { body: code } });
    value.contacts.push({ profile: { name: "New Owner" }, wa_id: stranger, user_id: userOf(stranger) });
    const welcomesBefore = t.graph.sends().filter((c) => c.body?.recipient === userOf(stranger)).length;
    flaky.arm(1); // the first batch commits, the second fails
    expect((await t.webhook(payload)).status).toBe(500);
    const boundAfterFailure = await env.DB.prepare("SELECT city_id FROM phones WHERE user_id = ?").bind(userOf(stranger)).first();
    expect(boundAfterFailure).toBeNull();
    const codeAfterFailure = await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(code).first<{ used_at: number | null }>();
    expect(codeAfterFailure?.used_at).toBeNull();
    expect(t.graph.sends().filter((c) => c.body?.recipient === userOf(stranger))).toHaveLength(welcomesBefore);

    expect((await t.webhook(payload)).status).toBe(200);
    const all = (await t.inbox(city, "?limit=200")).body.deliveries.concat(
      (await t.inbox(city, `?limit=200&after=${(await t.inbox(city, "?limit=200")).body.cursor}`)).body.deliveries,
    );
    const joined = all.filter((d: any) => d.kind === "joined" && d.wa_id === stranger);
    expect(joined).toHaveLength(1);
    expect(joined[0].message.id).toBe(inviteWamid);
    expect(all.filter((d: any) => d.message?.id === inviteWamid)).toHaveLength(1);
    expect(all.filter((d: any) => d.kind === "message" && d.wa_id === owner).map((d: any) => d.message.text.body)).toEqual(
      Array.from({ length: 99 }, (_, i) => `b${i}`),
    );
    const usedNow = await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(code).first<{ used_at: number | null }>();
    expect(usedNow?.used_at).toBe(t.clock.now);
    expect(t.graph.sends().filter((c) => c.body?.recipient === userOf(stranger))).toHaveLength(welcomesBefore + 1);
  });

  it("a webhook whose D1 write fails answers 500 with nothing stored, and its redelivery yields one row", async () => {
    const ok = setup();
    const city = await ok.enrollCity();
    const wa = await ok.join(city);
    const failing: D1Database = {
      prepare: (q: string) => env.DB.prepare(q),
      batch: async () => {
        throw new Error("D1_ERROR: storage unavailable");
      },
      exec: (q: string) => env.DB.exec(q),
      dump: () => env.DB.dump(),
      withSession: (c?: string) => env.DB.withSession(c as never),
    } as unknown as D1Database;
    const broken = setup({ db: failing });
    const wamid = `wamid.${uniq()}`;
    const before = (await ok.held(city)).length;
    const r = await broken.say(wa, "durable", { wamid });
    expect(r.res.status).toBe(500);
    expect((await ok.held(city)).length).toBe(before);
    const again = await ok.say(wa, "durable", { wamid });
    expect(again.res.status).toBe(200);
    expect((await ok.inbox(city)).body.deliveries.filter((d: any) => d.message?.id === wamid)).toHaveLength(1);
  });

  it("an invite text whose welcome send fails still binds and answers 200; its redelivery is one row and no second bind", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wa = phone();
    t.graph.failNext({ status: 500, body: { error: { code: 1, message: "An unknown error occurred" } } });
    const first = await t.say(wa, code);
    expect(first.res.status).toBe(200);
    const bound = await env.DB.prepare("SELECT city_id FROM phones WHERE user_id = ?").bind(userOf(wa)).first<{ city_id: string }>();
    expect(bound?.city_id).toBe(city.id);
    const again = await t.say(wa, code, { wamid: first.wamid });
    expect(again.res.status).toBe(200);
    const rows = (await t.inbox(city)).body.deliveries;
    expect(rows).toHaveLength(1);
    expect(rows[0].kind).toBe("joined");
    expect(t.graph.sends()).toHaveLength(1); // the failed welcome; no second welcome
    const failed = await env.DB.prepare("SELECT value FROM stats WHERE name = 'welcome_failed'").first<{ value: number }>();
    expect(failed?.value ?? 0).toBeGreaterThanOrEqual(1);
  });

  it("a phone bound to one city texting another city's code stays put; the code stays live", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const wa = await t.join(a);
    const codeB = await t.invite(b);
    await t.say(wa, codeB);
    const bound = await env.DB.prepare("SELECT city_id FROM phones WHERE user_id = ?").bind(userOf(wa)).first<{ city_id: string }>();
    expect(bound?.city_id).toBe(a.id);
    expect((await t.inbox(b)).body.deliveries).toHaveLength(0);
    const live = await env.DB.prepare("SELECT used_at FROM invite_codes WHERE code = ?").bind(codeB).first<{ used_at: number | null }>();
    expect(live?.used_at).toBeNull();
  });

  it("minting a new invite code retires the city's previous live one", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const first = await t.invite(city);
    const second = await t.invite(city);
    expect(second).not.toBe(first);
    const wa = phone();
    await t.say(wa, first);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(0);
    await t.say(wa, second);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(1);
  });

  it("an invite code expires after 24 hours", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const r = await t.call("POST", "/v1/invites", { token: city.token });
    expect(r.body.code).toMatch(/^\d{6}$/);
    expect(r.body.expires_at).toBe(t.clock.now + DAY);
    t.clock.now += DAY;
    await t.say(phone(), r.body.code);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(0);
  });

  it("five wrong codes in a day stop a phone's code attempts, the right one included", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wa = phone();
    for (let i = 0; i < 5; i++) {
      const wrong = String((Number(code) + 1 + i) % 1_000_000).padStart(6, "0");
      await t.say(wa, wrong);
    }
    await t.say(wa, code);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(0);
    t.clock.now += DAY + 1;
    const fresh = await t.invite(city);
    await t.say(wa, fresh);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(1);
  });
});

describe("Meta side: invite-code edges", () => {
  it("a wrong-code text Meta redelivers is one attempt, not two", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wa = phone();
    for (let i = 0; i < 4; i++) {
      const wrong = String((Number(code) + 1 + i) % 1_000_000).padStart(6, "0");
      const { wamid } = await t.say(wa, wrong);
      await t.say(wa, wrong, { wamid });
    }
    await t.say(wa, code); // the fifth attempt, within the limit
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
  });

  it("an invite text Meta stamped before the code expired binds on a redelivery after the expiry", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const city = await t.enrollCity();
    const code = await t.invite(city); // expires T0 + 24 h
    const wa = phone();
    const sentAt = T0 + DAY - 10;
    t.clock.now = sentAt;
    flaky.arm(0);
    const first = await t.say(wa, code, { ts: sentAt });
    expect(first.res.status).toBe(500);
    t.clock.now = T0 + DAY + HOUR;
    const again = await t.say(wa, code, { ts: sentAt, wamid: first.wamid });
    expect(again.res.status).toBe(200);
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
  });

  it("redeliveries of two wrong-code texts interleaved (A, B, A, B, A) are two attempts, and the owner's right code then binds (gp-0g2s)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wa = phone();
    const wrong = (i: number) => String((Number(code) + 1 + i) % 1_000_000).padStart(6, "0");
    const a = `wamid.${uniq()}`;
    const b = `wamid.${uniq()}`;
    for (const [wamid, text] of [
      [a, wrong(0)],
      [b, wrong(1)],
      [a, wrong(0)],
      [b, wrong(1)],
      [a, wrong(0)],
    ] as const) {
      expect((await t.say(wa, text, { wamid })).res.status).toBe(200);
    }
    expect(await env.DB.prepare("SELECT code_attempts FROM unknown_senders WHERE sender_key = ?").bind(userOf(wa)).first()).toEqual({ code_attempts: 2 });
    await t.say(wa, code);
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
  });

  it("the limit and the day are unchanged by the dedup: three more wrong codes after A, B, A, B, A make five, and the right code is then the sixth (gp-0g2s)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wa = phone();
    const wrong = (i: number) => String((Number(code) + 1 + i) % 1_000_000).padStart(6, "0");
    const a = `wamid.${uniq()}`;
    const b = `wamid.${uniq()}`;
    for (const [wamid, text] of [
      [a, wrong(0)],
      [b, wrong(1)],
      [a, wrong(0)],
      [b, wrong(1)],
      [a, wrong(0)],
    ] as const) {
      await t.say(wa, text, { wamid });
    }
    for (let i = 2; i < 5; i++) await t.say(wa, wrong(i));
    await t.say(wa, code);
    expect((await t.inbox(city)).body.deliveries).toHaveLength(0);
    t.clock.now += DAY + 1;
    await t.say(wa, await t.invite(city));
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
  });

  it("the cron deletes a counted wrong code's wamid once Meta can no longer redeliver it: 8 days (TOMBSTONE_SECONDS) after it was counted (gp-0g2s)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const code = await t.invite(city);
    const wrong = String((Number(code) + 1) % 1_000_000).padStart(6, "0");
    const { wamid } = await t.say(phone(), wrong);
    const kept = async () => (await env.DB.prepare("SELECT wamid FROM code_attempt_wamids WHERE wamid = ?").bind(wamid).all()).results.length;
    expect(await kept()).toBe(1);
    t.clock.now = T0 + 8 * DAY - 1;
    await t.gateway.maintain();
    expect(await kept()).toBe(1);
    t.clock.now = T0 + 8 * DAY;
    expect((await t.gateway.maintain()).code_wamids_deleted).toBeGreaterThanOrEqual(1);
    expect(await kept()).toBe(0);
  });
});

describe("Meta side: a wamid belongs to the city that first received it (gp-0g2s)", () => {
  it("after the phone is unbound and joins another city, Meta's retry of an acknowledged image reaches no city again and grants the new city no media; the joining text likewise; Meta gets 200", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const wa = phone();
    const codeA = await t.invite(a);
    const joining = await t.say(wa, codeA);
    const joinPayload = textPayload(wa, codeA, { wamid: joining.wamid, ts: t.clock.now });
    const mediaId = `7${Date.now()}${Math.floor(Math.random() * 1000)}`;
    t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
    const wamid = `wamid.${uniq()}`;
    const image = mediaPayload("image", wa, mediaId, { wamid, ts: t.clock.now });
    expect((await t.webhook(image)).status).toBe(200);
    const held = (await t.inbox(a)).body.deliveries;
    expect(held.map((d: any) => d.kind)).toEqual(["joined", "message"]);
    expect((await t.ack(a, held.map((d: any) => d.id))).status).toBe(200);
    // README, "Unbind an owner"; then the phone joins city B by B's invite code.
    await t.unbind(wa);
    t.clock.now += HOUR;
    await t.join(b, wa);
    t.clock.now += HOUR;
    expect((await t.webhook(image)).status).toBe(200);
    expect((await t.webhook(joinPayload)).status).toBe(200);
    expect((await t.inbox(b)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
    expect((await env.DB.prepare("SELECT city_id FROM media WHERE media_id = ?").bind(mediaId).all()).results).toEqual([{ city_id: a.id }]);
    expect((await t.call("GET", `/v1/media/${mediaId}`, { token: b.token })).status).toBe(404);
    expect((await t.call("POST", "/v1/read", { token: b.token, body: { message_id: wamid } })).status).toBe(404);
    // City A still holds only its acknowledged rows: the retries are no new delivery anywhere.
    expect((await t.inbox(a)).body.deliveries).toHaveLength(0);
  });

  it("a message whose first delivery failed, retried after the owner moved to another city, reaches neither city and grants no media: the new binding holds only what Meta stamped after it (self-review r1 S1a)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const first = await t.enrollCity();
    const second = await t.enrollCity();
    // Known by phone and BSUID: the text names both (Meta's shape), the image the phone's user id.
    const wa = phone();
    const user = userOf(wa);
    await t.joinAs(first, { wa, user });
    t.clock.now = T0 + HOUR;
    const text = textAs({ wa, user }, "for the first city's mayor", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(text)).status).toBe(500);
    const mediaId = `6${Date.now()}${Math.floor(Math.random() * 1000)}`;
    t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
    const image = mediaPayload("image", wa, mediaId, { wamid: `wamid.${uniq()}`, ts: T0 + HOUR });
    flaky.arm(0);
    expect((await t.webhook(image)).status).toBe(500);
    // README, "Unbind an owner": it records the owner's user id, so what the owner wrote before
    // the unbind is the unbound binding's, never the next one's.
    await t.unbind(user);
    t.clock.now = T0 + 2 * HOUR;
    await t.joinAs(second, { wa, user });
    t.clock.now = T0 + 3 * HOUR;
    const calls = t.graph.sends().length;
    expect((await t.webhook(text)).status).toBe(200);
    expect((await t.webhook(image)).status).toBe(200);
    expect((await t.inbox(second)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
    expect((await t.inbox(first)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]); // its own join, nothing more
    expect((await t.call("GET", `/v1/media/${mediaId}`, { token: second.token })).status).toBe(404);
    expect(t.graph.sends()).toHaveLength(calls); // no canned line: the number is an owner's
    const unrouted = await env.DB.prepare("SELECT value FROM stats WHERE name = 'unrouted_messages'").first<{ value: number }>();
    expect(unrouted?.value ?? 0).toBeGreaterThanOrEqual(2);
    // The owner's own messages in the new city, stamped after its join, are delivered as ever.
    const now = await t.say(wa, "for the second city");
    expect((await t.inbox(second)).body.deliveries.some((d: any) => d.message?.id === now.wamid)).toBe(true);
  });

  it("the same for another person who binds the number next (under their own user id), and for a stranger's text sent before it joined (self-review r1 S1b)", async () => {
    const flaky = flakyDb();
    const t = setup({ db: flaky.db });
    const first = await t.enrollCity();
    const second = await t.enrollCity();
    const wa = await t.join(first);
    t.clock.now = T0 + HOUR;
    const before = textPayload(wa, "A's words for city one", { wamid: `wamid.${uniq()}`, ts: T0 + HOUR, name: "A" });
    flaky.arm(0);
    expect((await t.webhook(before)).status).toBe(500);
    await t.unbind(wa);
    // Someone else, on the recycled number under their own user id, still a stranger, writes; the
    // write fails; then they join the second city.
    const b = bsuid();
    t.clock.now = T0 + 2 * HOUR;
    const hello = textAs({ wa, user: b }, "hello, who is this?", { wamid: `wamid.${uniq()}`, ts: T0 + 2 * HOUR, name: "B" });
    flaky.arm(0);
    expect((await t.webhook(hello)).status).toBe(500);
    t.clock.now = T0 + 3 * HOUR;
    await t.joinAs(second, { wa, user: b });
    t.clock.now = T0 + 4 * HOUR;
    const calls = t.graph.sends().length;
    expect((await t.webhook(before)).status).toBe(200);
    expect((await t.webhook(hello)).status).toBe(200);
    expect((await t.inbox(second)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("the same holds for a retry of a message A never acknowledged: it stays A's, held for A alone", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const wa = await t.join(a);
    const said = await t.say(wa, "still held for A");
    const payload = textPayload(wa, "still held for A", { wamid: said.wamid, ts: t.clock.now });
    await t.unbind(wa);
    t.clock.now += HOUR;
    await t.join(b, wa);
    expect((await t.webhook(payload)).status).toBe(200);
    expect((await t.inbox(a)).body.deliveries.filter((d: any) => d.message?.id === said.wamid)).toHaveLength(1);
    expect((await t.inbox(b)).body.deliveries.map((d: any) => d.kind)).toEqual(["joined"]);
  });
});

describe("City side: the inbox, the hold and the ack by id", () => {
  it("a poll with no after returns every unacknowledged delivery again, oldest first", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "one");
    await t.say(wa, "two");
    const first = (await t.inbox(city)).body;
    const second = (await t.inbox(city)).body;
    expect(first.deliveries.map((d: any) => d.id)).toEqual(second.deliveries.map((d: any) => d.id));
    const ids = first.deliveries.map((d: any) => d.id);
    expect(ids).toEqual([...ids].sort((x: number, y: number) => x - y));
    expect(first.deliveries.map((d: any) => d.kind)).toEqual(["joined", "message", "message"]);
    expect(first.cursor).toBe(ids[ids.length - 1]);
    expect(typeof first.deliveries[0].received_at).toBe("number");
  });

  it("after and limit page the held deliveries; an empty page keeps the cursor", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    for (const s of ["a", "b", "c"]) await t.say(wa, s);
    const page1 = (await t.inbox(city, "?limit=2")).body;
    expect(page1.deliveries).toHaveLength(2);
    const page2 = (await t.inbox(city, `?after=${page1.cursor}&limit=2`)).body;
    expect(page2.deliveries).toHaveLength(2);
    const page3 = (await t.inbox(city, `?after=${page2.cursor}`)).body;
    expect(page3).toEqual({ deliveries: [], cursor: page2.cursor });
    for (const q of ["?limit=0", "?limit=201", "?after=-1", "?after=x", "?limit=1.5"]) {
      expect((await t.inbox(city, q)).status).toBe(400);
    }
  });

  it("ack deletes exactly the listed ids and leaves an unlisted earlier id in the inbox", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "earlier");
    await t.say(wa, "later");
    const [joined, earlier, later] = (await t.inbox(city)).body.deliveries.map((d: any) => d.id);
    const r = await t.ack(city, [joined, later]);
    expect(r.status).toBe(200);
    expect(r.body).toEqual({ ok: true });
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.id)).toEqual([earlier]);
  });

  it("HOLD: a delivery is held, body and all, and returned by every poll until the ack names its id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const { wamid } = await t.say(wa, "held until acked");
    let id = 0;
    for (let i = 0; i < 5; i++) {
      t.clock.now += HOUR;
      const d = (await t.inbox(city)).body.deliveries.find((x: any) => x.message?.id === wamid);
      expect(d?.message.text.body).toBe("held until acked");
      id = d.id;
    }
    await t.ack(city, [id]);
    expect((await t.inbox(city)).body.deliveries.some((x: any) => x.id === id)).toBe(false);
  });

  it("ACK OF A DELETED ID: an ack for an id the cron already deleted is a no-op 200", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "will expire");
    const ids = (await t.inbox(city)).body.deliveries.map((d: any) => d.id);
    t.clock.now += HOLD_SECONDS + 1;
    await t.gateway.maintain();
    expect((await t.inbox(city)).body.deliveries).toEqual([]);
    t.clock.now += 2 * DAY; // past the body-less row's last day too: the id is gone from D1
    await t.gateway.maintain();
    const left = await env.DB.prepare("SELECT COUNT(*) AS n FROM deliveries WHERE city_id = ?").bind(city.id).first<{ n: number }>();
    expect(left?.n).toBe(0);
    const r = await t.ack(city, ids);
    expect(r.status).toBe(200);
    expect(r.body).toEqual({ ok: true });
  });

  it("REPEATED ACK: acking an id an earlier ack already deleted is the same no-op, and touches nothing else", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "first");
    await t.say(wa, "second");
    const [joined, first, second] = (await t.inbox(city)).body.deliveries.map((d: any) => d.id);
    expect((await t.ack(city, [first])).status).toBe(200);
    const again = await t.ack(city, [first]);
    expect(again.status).toBe(200);
    expect(again.body).toEqual({ ok: true });
    expect((await t.inbox(city)).body.deliveries.map((d: any) => d.id)).toEqual([joined, second]);
  });

  it("an ack naming another city's id is a no-op for it: the other city still holds it", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    await t.join(a);
    await t.join(b);
    const idB = (await t.inbox(b)).body.deliveries[0].id;
    expect((await t.ack(a, [idB])).status).toBe(200);
    expect((await t.inbox(b)).body.deliveries.map((d: any) => d.id)).toEqual([idB]);
  });

  it("a malformed ack answers 400 and deletes nothing", async () => {
    const t = setup();
    const city = await t.enrollCity();
    await t.join(city);
    for (const ids of [undefined, [], "1", [0], [-1], [1.5], ["1"], Array.from({ length: 501 }, (_, i) => i + 1)]) {
      expect((await t.ack(city, ids)).status).toBe(400);
    }
    expect((await t.inbox(city)).body.deliveries).toHaveLength(1);
  });

  it("a delivery never acknowledged survives the ack of other ids until the cron deletes it at 7 days", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const { wamid } = await t.say(wa, "never acked");
    const keep = (await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid).id;
    for (let day = 1; day <= 6; day++) {
      t.clock.now = T0 + day * DAY;
      const { wamid: other } = await t.say(wa, `day ${day}`);
      const otherId = (await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === other).id;
      await t.ack(city, [otherId]);
      await t.gateway.maintain();
      expect((await t.inbox(city)).body.deliveries.some((d: any) => d.id === keep)).toBe(true);
    }
    t.clock.now = T0 + HOLD_SECONDS;
    await t.gateway.maintain();
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.id === keep)).toBe(false);
  });

  it("an acknowledged wamid redelivered by Meta is still one row: no second delivery", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const { wamid } = await t.say(wa, "acked then redelivered");
    const id = (await t.inbox(city)).body.deliveries.find((d: any) => d.message?.id === wamid).id;
    await t.ack(city, [id]);
    t.clock.now += 6 * DAY;
    await t.say(wa, "acked then redelivered", { wamid });
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === wamid)).toBe(false);
  });

  it("over 5,000 held for one city Meta gets 503 and nothing is stored; below it again, 200", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await env.DB.prepare(
      `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
       INSERT INTO deliveries (city_id, received_at, kind, wamid, status, body)
       SELECT ?, ?, 'message', 'wamid.fill' || i, '', '{}' FROM n`,
    )
      .bind(MAX_PENDING - 1, city.id, t.clock.now)
      .run();
    const wamid = `wamid.${uniq()}`;
    const r = await t.say(wa, "over the cap", { wamid });
    expect(r.res.status).toBe(503);
    const stored = await env.DB.prepare("SELECT COUNT(*) AS n FROM deliveries WHERE wamid = ?").bind(wamid).first<{ n: number }>();
    expect(stored?.n).toBe(0);
    const first = (await t.inbox(city, "?limit=1")).body.deliveries[0].id;
    await t.ack(city, [first]);
    expect((await t.say(wa, "over the cap", { wamid })).res.status).toBe(200);
  });

  it("the cron deletes bodies older than 7 days and keeps phones", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "old");
    t.clock.now += HOLD_SECONDS + 1;
    const report = await t.gateway.maintain();
    expect(report.bodies_expired).toBeGreaterThanOrEqual(2);
    expect((await t.inbox(city)).body.deliveries).toEqual([]);
    const kept = await env.DB.prepare("SELECT city_id FROM phones WHERE user_id = ?").bind(userOf(wa)).first<{ city_id: string }>();
    expect(kept?.city_id).toBe(city.id);
    const bodies = await env.DB.prepare("SELECT COUNT(*) AS n FROM deliveries WHERE city_id = ? AND body IS NOT NULL").bind(city.id).first<{ n: number }>();
    expect(bodies?.n).toBe(0);
  });
});

describe("City side: statuses of the city's sends", () => {
  it("sent, delivered and read for one wamid land as three status rows; a redelivery of one makes no fourth", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "hi mayor");
    const sent = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "hello owner" });
    const out = sent.body.message_id as string;
    for (const s of ["sent", "delivered", "read"]) {
      expect((await t.webhook(statusPayload(out, s, wa, t.clock.now))).status).toBe(200);
    }
    await t.webhook(statusPayload(out, "delivered", wa, t.clock.now));
    const statuses = (await t.inbox(city)).body.deliveries.filter((d: any) => d.kind === "status");
    expect(statuses.map((d: any) => d.message.status)).toEqual(["sent", "delivered", "read"]);
    expect(statuses.every((d: any) => d.message.id === out && d.wa_id === wa && d.profile_name === null)).toBe(true);
  });

  it("a status for a wamid no city sent reaches no city", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const before = (await t.inbox(city)).body.deliveries.length;
    await t.webhook(statusPayload(`wamid.${uniq()}`, "sent", wa, t.clock.now));
    expect((await t.inbox(city)).body.deliveries).toHaveLength(before);
  });
});

describe("City side: send", () => {
  it("a send to an unbound phone, or to another city's phone, answers 403 without a Graph call", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const waB = await t.join(b);
    const calls = t.graph.sends().length;
    const unbound = await t.send(a, { to: phone(), idempotency_key: uniq(), type: "text", text: "x" });
    expect(unbound.status).toBe(403);
    expect(unbound.body).toEqual({ error: "not_bound" });
    const theirs = await t.send(a, { to: waB, idempotency_key: uniq(), type: "text", text: "x" });
    expect(theirs.status).toBe(403);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a repeated idempotency_key makes one Graph call and answers the same message_id", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const before = t.graph.sends().length;
    const key = uniq();
    const one = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "once" });
    const two = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "once" });
    expect(one.status).toBe(200);
    expect(two.status).toBe(200);
    expect(two.body).toEqual(one.body);
    expect(t.graph.sends()).toHaveLength(before + 1);
  });

  it("the same idempotency_key in two cities is two sends (the key is per city)", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const waA = await t.join(a);
    const waB = await t.join(b);
    const key = uniq();
    const one = await t.send(a, { to: waA, idempotency_key: key, type: "text", text: "a" });
    const two = await t.send(b, { to: waB, idempotency_key: key, type: "text", text: "b" });
    expect(one.body.message_id).not.toBe(two.body.message_id);
  });

  it("Meta's refusal (a 4xx answer) records nothing, so a retry under the same key sends", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const key = uniq();
    t.graph.failNext({ status: 400, body: { error: { code: 131056, message: "Pair rate limit hit" } } });
    const failed = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "retry me" });
    expect(failed.status).toBe(502);
    expect(failed.body).toEqual({ error: "meta_error", meta_status: 400, meta_code: 131056 });
    const retried = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "retry me" });
    expect(retried.status).toBe(200);
    expect(retried.body.message_id).toMatch(/^wamid\./);
  });

  it("an outcome Meta never confirmed (no answer, a 5xx) spends the key: its repeat answers outcome_unknown with no second Graph call", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    for (const failure of ["network", { status: 500, body: { error: { code: 2, message: "Service temporarily unavailable" } } }] as const) {
      const key = uniq();
      t.graph.failNext(failure);
      const first = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "did it go?" });
      expect(first.status).toBe(502);
      expect(first.body.error).toBe("outcome_unknown");
      const calls = t.graph.sends().length;
      t.clock.now += 120;
      const again = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "did it go?" });
      expect(again.status).toBe(409);
      expect(again.body).toEqual({ error: "outcome_unknown" });
      expect(t.graph.sends()).toHaveLength(calls);
    }
  });

  it("Meta's own outside-window answer (131047) reads window_closed", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.graph.failNext({ status: 400, body: { error: { code: 131047, message: "Re-engagement message" } } });
    const r = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "late" });
    expect(r.status).toBe(422);
    expect(r.body).toEqual({ error: "window_closed" });
  });

  it("a key whose first request is still at Meta answers 409 in_flight; a claim abandoned past 60 s reads outcome_unknown and is never sent again", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const key = uniq();
    await env.DB.prepare("INSERT INTO sends (city_id, idempotency_key, wamid, state, type, created_at) VALUES (?, ?, NULL, 'claimed', 'text', ?)")
      .bind(city.id, key, t.clock.now)
      .run();
    const calls = t.graph.sends().length;
    const busy = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "x" });
    expect(busy.status).toBe(409);
    expect(busy.body).toEqual({ error: "in_flight" });
    t.clock.now += 61;
    const abandoned = await t.send(city, { to: wa, idempotency_key: key, type: "text", text: "x" });
    expect(abandoned.status).toBe(409);
    expect(abandoned.body).toEqual({ error: "outcome_unknown" });
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a status Meta delivers before the send has recorded its wamid is held apart, then adopted into the city by that send", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.graph.hooks.onSend = async (wamid) => {
      t.graph.hooks.onSend = null;
      expect((await t.webhook(statusPayload(wamid, "sent", wa, t.clock.now))).status).toBe(200);
    };
    const sent = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "raced" });
    expect(sent.status).toBe(200);
    const statuses = (await t.inbox(city)).body.deliveries.filter((d: any) => d.kind === "status");
    expect(statuses.map((d: any) => [d.message.id, d.message.status])).toEqual([[sent.body.message_id, "sent"]]);
    const left = await env.DB.prepare("SELECT COUNT(*) AS n FROM orphan_statuses WHERE wamid = ?").bind(sent.body.message_id).first<{ n: number }>();
    expect(left?.n).toBe(0);
    // A status for a wamid no city ever sent stays apart and is dropped by the cron after a day.
    const stray = `wamid.${uniq()}`;
    await t.webhook(statusPayload(stray, "sent", wa, t.clock.now));
    t.clock.now += DAY;
    await t.gateway.maintain();
    const gone = await env.DB.prepare("SELECT COUNT(*) AS n FROM orphan_statuses WHERE wamid = ?").bind(stray).first<{ n: number }>();
    expect(gone?.n).toBe(0);
  });

  it("refuses malformed sends with 400 and over-long text with text_too_long", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const bad = [
      {},
      { to: wa, type: "text", text: "no key" },
      { to: wa, idempotency_key: uniq(), type: "image", text: "x" },
      { to: wa, idempotency_key: uniq(), type: "text" },
      { to: wa, idempotency_key: uniq(), type: "text", text: "" },
      { to: wa, idempotency_key: uniq(), type: "reaction", reaction: { emoji: "x" } },
      { to: wa, idempotency_key: uniq(), type: "template", template: { name: "Bad Name", language: "en_US", params: [] } },
      { to: "not-a-phone", idempotency_key: uniq(), type: "text", text: "x" },
    ];
    for (const body of bad) expect((await t.send(city, body)).status).toBe(400);
    const long = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "x".repeat(4097) });
    expect(long.status).toBe(400);
    expect(long.body).toEqual({ error: "text_too_long" });
    const exact = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "\u{1F600}".repeat(4096) });
    expect(exact.status).toBe(200);
  });
});

describe("City side: the 24-hour window", () => {
  it("a text just before the close sends; at the close and at 24h01 it answers window_closed without a Graph call", async () => {
    // The bead's line says 23h59, but the frozen 120 s margin closes the window at 23h58m, so the
    // last allowed second is the close minus 1 (README, "The window").
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const t0 = T0 + HOUR;
    t.clock.now = t0;
    await t.say(wa, "the owner writes", { ts: t0 });
    t.clock.now = t0 + CLOSE - 1;
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "in time" })).status).toBe(200);
    const calls = t.graph.sends().length;
    t.clock.now = t0 + CLOSE;
    const atClose = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "late" });
    expect(atClose.status).toBe(422);
    expect(atClose.body).toEqual({ error: "window_closed" });
    t.clock.now = t0 + DAY + 60;
    const lateText = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a day late" });
    expect(lateText.status).toBe(422);
    expect(lateText.body).toEqual({ error: "window_closed" });
    const late = await t.send(city, { to: wa, idempotency_key: uniq(), type: "reaction", reaction: { message_id: "wamid.X", emoji: "\u{1F44D}" } });
    expect(late.status).toBe(422);
    expect(t.graph.sends()).toHaveLength(calls);
  });

  it("a template at 24h01 sends and leaves the window closed; the owner's next message reopens it", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.clock.now = T0 + DAY + 60;
    const tpl = await t.send(city, {
      to: wa,
      idempotency_key: uniq(),
      type: "template",
      template: { name: "dynasty_update", language: "en_US", params: ["The shipment cleared."] },
    });
    expect(tpl.status).toBe(200);
    const last = t.graph.sends().at(-1)!;
    expect(last.body).toMatchObject({ type: "template", template: { name: "dynasty_update" } });
    const calls = t.graph.sends().length;
    const after = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "and another thing" });
    expect(after.status).toBe(422);
    expect(t.graph.sends()).toHaveLength(calls);
    await t.say(wa, "ok", { ts: t.clock.now });
    expect((await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "thanks" })).status).toBe(200);
  });

  it("GET /v1/window: close = last inbound + 24 h - 120 s, moves on the owner's message, not on any send; null for a phone that never wrote; no other city's phone", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const wa = await t.join(a); // the invite text is the owner's first message
    const waB = await t.join(b);
    const silent = phone();
    await env.DB.prepare(
      "INSERT INTO phones (conversation_id, wa_id, city_id, profile_name, bound_at, bound_wamid, last_inbound_at) VALUES (?1, ?1, ?2, NULL, ?3, ?4, NULL)",
    )
      .bind(silent, a.id, t.clock.now, `wamid.${uniq()}`)
      .run();
    const read = async () =>
      (await t.window(a)).body.windows as Array<{ conversation_id: string; user_id: string | null; last_inbound_at: number | null; window_closes_at: number | null }>;
    let w = await read();
    expect(w.map((x) => x.conversation_id).sort()).toEqual([silent, wa].sort());
    expect(w.some((x) => x.conversation_id === waB)).toBe(false);
    expect(w.find((x) => x.conversation_id === silent)).toEqual({ conversation_id: silent, user_id: null, last_inbound_at: null, window_closes_at: null });
    expect(w.find((x) => x.conversation_id === wa)).toEqual({ conversation_id: wa, user_id: userOf(wa), last_inbound_at: T0, window_closes_at: T0 + CLOSE });

    t.clock.now = T0 + 2 * HOUR;
    const sends = [
      await t.send(a, { to: wa, idempotency_key: uniq(), type: "text", text: "a text" }),
      await t.send(a, { to: wa, idempotency_key: uniq(), type: "reaction", reaction: { message_id: "wamid.X", emoji: "\u{1F44D}" } }),
      await t.send(a, { to: wa, idempotency_key: uniq(), type: "template", template: { name: "dynasty_update", language: "en_US", params: ["x"] } }),
    ];
    expect(sends.map((s) => s.status)).toEqual([200, 200, 200]);
    w = await read();
    expect(w.find((x) => x.conversation_id === wa)?.last_inbound_at).toBe(T0);

    t.clock.now = T0 + 3 * HOUR;
    await t.say(wa, "the owner again", { ts: T0 + 3 * HOUR });
    w = await read();
    expect(w.find((x) => x.conversation_id === wa)).toEqual({ conversation_id: wa, user_id: userOf(wa), last_inbound_at: T0 + 3 * HOUR, window_closes_at: T0 + 3 * HOUR + CLOSE });
  });

  it("an owner's reaction is delivered and moves the window (the keeper's thumbs-up acknowledgement)", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.clock.now = T0 + 5 * HOUR;
    const wamid = `wamid.${uniq()}`;
    await t.webhook(reactionPayload(wa, "wamid.OUT1", { wamid, ts: t.clock.now }));
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === wamid && d.message.type === "reaction")).toBe(true);
    const w = (await t.window(city)).body.windows[0];
    expect(w.last_inbound_at).toBe(T0 + 5 * HOUR);
  });

  it("a system notice is delivered but does not move the window", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.clock.now = T0 + 5 * HOUR;
    const payload = textPayload(wa, "ignored", { wamid: `wamid.${uniq()}`, ts: t.clock.now });
    const m = payload.entry[0].changes[0].value.messages[0];
    delete m.text;
    m.type = "system";
    m.system = { body: "User changed number", type: "user_changed_number" };
    await t.webhook(payload);
    expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.type === "system")).toBe(true);
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(T0);
  });

  it("Meta's timestamp drives the clock: a redelivery hours late does not stretch the window, a timestamp ahead of the gateway's clock is clamped", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    t.clock.now = T0 + 6 * HOUR;
    await t.say(wa, "sent at T0+1h, delivered late", { ts: T0 + HOUR });
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(T0 + HOUR);
    await t.say(wa, "from the future", { ts: T0 + 100 * HOUR });
    expect((await t.window(city)).body.windows[0].last_inbound_at).toBe(T0 + 6 * HOUR);
  });
});

describe("City side: read, media", () => {
  it("read marks a delivered message read with the typing indicator; another city's message is 404", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const waA = await t.join(a);
    const { wamid } = await t.say(waA, "read me");
    const ok = await t.call("POST", "/v1/read", { token: a.token, body: { message_id: wamid } });
    expect(ok.status).toBe(200);
    expect(ok.body).toEqual({ ok: true });
    expect(t.graph.reads().at(-1)!.body).toEqual({ messaging_product: "whatsapp", status: "read", message_id: wamid, typing_indicator: { type: "text" } });
    const other = await t.call("POST", "/v1/read", { token: b.token, body: { message_id: wamid } });
    expect(other.status).toBe(404);
  });

  it("media streams the bytes with Meta's mime type to its city; another city's media id is 404", async () => {
    const t = setup();
    const a = await t.enrollCity();
    const b = await t.enrollCity();
    const waA = await t.join(a);
    const mediaId = String(Date.now()) + String(Math.floor(Math.random() * 1000));
    t.graph.media.set(mediaId, { mime: "audio/ogg; codecs=opus", bytes: new Uint8Array([79, 103, 103, 83]) });
    await t.webhook(mediaPayload("audio", waA, mediaId, { wamid: `wamid.${uniq()}`, ts: t.clock.now }));
    const res = await t.gateway.handle(new Request(`https://whatsapp.test/v1/media/${mediaId}`, { headers: { authorization: `Bearer ${a.token}` } }));
    expect(res.status).toBe(200);
    expect(res.headers.get("content-type")).toBe("audio/ogg; codecs=opus");
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(new Uint8Array([79, 103, 103, 83]));
    const theirs = await t.call("GET", `/v1/media/${mediaId}`, { token: b.token });
    expect(theirs.status).toBe(404);
    const unknown = await t.call("GET", "/v1/media/123456", { token: a.token });
    expect(unknown.status).toBe(404);
    t.clock.now += 7 * DAY;
    expect((await t.call("GET", `/v1/media/${mediaId}`, { token: a.token })).status).toBe(404);
  });
});

describe("Credentials and enrollment", () => {
  it("enroll mints a city and one credential for a live code, once; a used code is 403", async () => {
    const t = setup();
    const code = await t.enrollCode("Creekview Roofing");
    const first = await t.call("POST", "/v1/enroll", { body: { code, name: "creekview-box" } });
    expect(first.status).toBe(200);
    expect(Object.keys(first.body).sort()).toEqual(["city_id", "credential"]);
    const again = await t.call("POST", "/v1/enroll", { body: { code, name: "creekview-box" } });
    expect(again.status).toBe(403);
    const stored = await env.DB.prepare("SELECT token_sha256 FROM credentials WHERE city_id = ?").bind(first.body.city_id).all<{ token_sha256: string }>();
    expect(stored.results).toHaveLength(1);
    expect(stored.results[0]!.token_sha256).not.toContain(first.body.credential);
  });

  it("an unknown or expired enrollment code is 403; a malformed enroll is 400", async () => {
    const t = setup();
    expect((await t.call("POST", "/v1/enroll", { body: { code: "AAAA-BBBB-CCCC-DDDD", name: "x" } })).status).toBe(403);
    const code = await t.enrollCode("Late City");
    t.clock.now += 3 * DAY;
    expect((await t.call("POST", "/v1/enroll", { body: { code, name: "x" } })).status).toBe(403);
    expect((await t.call("POST", "/v1/enroll", { body: { code } })).status).toBe(400);
    expect((await t.call("POST", "/v1/enroll", { raw: "not json" })).status).toBe(400);
  });

  it("a revoked credential is 401; so are an unknown one and none", async () => {
    const t = setup();
    const city = await t.enrollCity();
    expect((await t.inbox(city)).status).toBe(200);
    await env.DB.prepare("UPDATE credentials SET revoked_at = ? WHERE city_id = ?").bind(t.clock.now, city.id).run();
    const revoked = await t.inbox(city);
    expect(revoked.status).toBe(401);
    expect(revoked.body).toEqual({ error: "unauthorized" });
    expect((await t.call("GET", "/v1/inbox", { token: "wgc_not-a-real-credential-0000000000" })).status).toBe(401);
    for (const path of ["/v1/inbox", "/v1/window"]) expect((await t.call("GET", path)).status).toBe(401);
    for (const path of ["/v1/invites", "/v1/inbox/ack", "/v1/send", "/v1/read"]) expect((await t.call("POST", path, { body: {} })).status).toBe(401);
  });

  it("admin routes need ADMIN_TOKEN; an unset admin token opens nothing", async () => {
    const t = setup();
    expect((await t.call("POST", "/v1/admin/enroll-codes", { body: { city_name: "x" } })).status).toBe(401);
    expect((await t.call("POST", "/v1/admin/enroll-codes", { token: "wrong", body: { city_name: "x" } })).status).toBe(401);
    expect((await t.call("POST", "/v1/admin/unbind", { body: { id: "x" } })).status).toBe(401);
    expect((await t.call("POST", "/v1/admin/unbind", { token: "wrong", body: { id: "x" } })).status).toBe(401);
    const unset = setup({ config: { adminToken: "" } });
    expect((await unset.call("GET", "/v1/admin/cities", { token: "" })).status).toBe(401);
    expect((await unset.call("GET", "/v1/admin/cities", { headers: { authorization: "Bearer " } })).status).toBe(401);
    expect((await unset.call("POST", "/v1/admin/unbind", { headers: { authorization: "Bearer " }, body: { id: "x" } })).status).toBe(401);
    const minted = await t.admin("POST", "/v1/admin/enroll-codes", { city_name: "Nationwide EMS" });
    expect(minted.status).toBe(200);
    expect(minted.body.code).toMatch(/^[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}$/);
    expect(minted.body.expires_at).toBe(t.clock.now + 3 * DAY);
  });

  it("admin cities lists counts and the last four digits of each phone, never a number in clear", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    const r = await t.admin("GET", "/v1/admin/cities");
    expect(r.status).toBe(200);
    const row = r.body.cities.find((c: any) => c.id === city.id);
    expect(row).toMatchObject({ name: city.name, phones: 1, phones_last4: [wa.slice(-4)], held: 1, credentials: 1 });
    expect(JSON.stringify(r.body)).not.toContain(wa);
  });
});

describe("Health, logs, the entry point", () => {
  it("healthz answers ok, the held total and the last webhook time, with no phone and no body", async () => {
    const t = setup();
    const city = await t.enrollCity();
    const wa = await t.join(city);
    await t.say(wa, "secret plans");
    const r = await t.call("GET", "/healthz");
    expect(r.status).toBe(200);
    expect(Object.keys(r.body).sort()).toEqual(["last_webhook_at", "ok", "pending_total"]);
    expect(r.body.ok).toBe(true);
    expect(r.body.pending_total).toBeGreaterThanOrEqual(2);
    expect(r.body.last_webhook_at).toBe(t.clock.now);
    expect(JSON.stringify(r.body)).not.toContain(wa);
  });

  it("no log line carries a phone number, a message body, a token or a secret", async () => {
    const t = setup();
    const city = await t.enrollCity("Hou Dynasty");
    const code = await t.invite(city);
    const wa = phone();
    const stranger = phone();
    await t.say(wa, code, { name: "Taylor Owner" });
    await t.say(wa, "the vault code is 4242");
    await t.say(stranger, "who is this");
    t.graph.failNext({ status: 400, body: { error: { code: 131047, message: `Re-engagement message to ${wa}` } } });
    await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a private reply" });
    await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a private reply 2" });
    t.graph.failNext("network");
    await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a private reply 3" });
    await t.send(city, { to: stranger, idempotency_key: uniq(), type: "text", text: "a private reply to a stranger" });
    t.clock.now += 2 * DAY;
    await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a private reply, too late" });
    await t.webhook(textPayload(wa, "bad sig", { wamid: `wamid.${uniq()}`, ts: t.clock.now }), "sha256=00");
    const broken = setup({ db: { prepare: (q: string) => env.DB.prepare(q), batch: async () => { throw new Error(`D1 down ${wa}`); } } as unknown as D1Database });
    await broken.say(wa, "the vault code is 4242");
    await t.gateway.maintain();
    const lines = [...t.logs, ...broken.logs].map((e) => JSON.stringify(e));
    const events = new Set([...t.logs, ...broken.logs].map((e) => e.event));
    for (const e of ["webhook", "send", "send_failed", "send_refused", "webhook_refused", "webhook_store_failed", "invite_minted", "city_enrolled"]) {
      expect(events).toContain(e);
    }
    expect(lines.length).toBeGreaterThan(0);
    for (const needle of [wa, wa.slice(2), stranger, "4242", "vault", "private reply", "Taylor", code, city.token, SECRETS.app, SECRETS.access, SECRETS.verify, SECRETS.admin]) {
      for (const line of lines) expect(line).not.toContain(needle);
    }
  });

  it("the Worker entry's own console lines (a webhook, a refused handshake, the cron) carry no phone, body, token or secret", async () => {
    const spy = vi.spyOn(console, "log").mockImplementation(() => {});
    try {
      // A bound sender, so the entry makes no Graph call (the entry's fetch is the real one).
      const wa = phone();
      const cityId = `city_entry_${uniq()}`;
      await env.DB.batch([
        env.DB.prepare("INSERT INTO cities (id, name, box_name, created_at) VALUES (?, 'Entry City', 'entry-box', 0)").bind(cityId),
        env.DB.prepare(
          "INSERT INTO phones (conversation_id, wa_id, city_id, profile_name, bound_at, bound_wamid, last_inbound_at) VALUES (?1, ?1, ?2, NULL, 0, ?3, NULL)",
        ).bind(wa, cityId, `wamid.${uniq()}`),
      ]);
      const payload = textPayload(wa, "entry body text 9876", { wamid: `wamid.${uniq()}`, ts: Math.floor(Date.now() / 1000) });
      const raw = new TextEncoder().encode(JSON.stringify(payload));
      const res = await worker.fetch(
        new Request("https://whatsapp.dynasty.computer/meta/webhook", {
          method: "POST",
          body: raw,
          headers: { "x-hub-signature-256": await signatureHeader(raw, env.META_APP_SECRET) },
        }),
        env,
      );
      expect(res.status).toBe(200);
      await worker.fetch(new Request(`https://whatsapp.dynasty.computer/meta/webhook?hub.mode=subscribe&hub.verify_token=nope-${wa}&hub.challenge=1`), env);
      await worker.scheduled(createScheduledController({ scheduledTime: Date.now(), cron: "17 4 * * *" }), env);
      const lines = spy.mock.calls.map((c) => c.map(String).join(" "));
      expect(lines.some((l) => l.includes('"event":"maintenance"'))).toBe(true);
      expect(lines.some((l) => l.includes('"event":"webhook"'))).toBe(true);
      for (const needle of [wa, "9876", "entry body", "nope-", env.META_APP_SECRET, env.META_ACCESS_TOKEN, env.META_VERIFY_TOKEN, env.ADMIN_TOKEN]) {
        for (const line of lines) expect(line).not.toContain(needle);
      }
    } finally {
      spy.mockRestore();
    }
  });

  it("the Worker entry answers healthz, the handshake with its configured secret, and 404 on an unknown route", async () => {
    const health = await SELF.fetch("https://whatsapp.dynasty.computer/healthz");
    expect(health.status).toBe(200);
    expect(((await health.json()) as { ok: boolean }).ok).toBe(true);
    const hs = await SELF.fetch(`https://whatsapp.dynasty.computer/meta/webhook?hub.mode=subscribe&hub.verify_token=${env.META_VERIFY_TOKEN}&hub.challenge=42`);
    expect(await hs.text()).toBe("42");
    const nope = await SELF.fetch("https://whatsapp.dynasty.computer/v2/anything");
    expect(nope.status).toBe(404);
    const method = await SELF.fetch("https://whatsapp.dynasty.computer/v1/inbox", { method: "DELETE" });
    expect(method.status).toBe(405);
    expect(PNID).toBe(env.META_PHONE_NUMBER_ID);
  });
});
