/**
 * Run only by evidence.py (next to this file). That script copies it into whatsapp/gateway/test/,
 * with src/ written back to the stack base (392f9c09, the Worker that is running) and both
 * migrations applied. This is the gap that README.md's Deploy step 3 describes, between
 * `migrations apply` and `deploy`.
 */
import { env } from "cloudflare:test";
import { expect, it } from "vitest";
import { phone, setup, statusPayload, uniq } from "./harness.js";

it("the v1 Worker on the v2 schema: a bound owner's message, a send, its status and the window work; a stranger's text and an invite text answer 500", async () => {
  const t = setup();
  const city = await t.enrollCity();
  const wa = phone();
  // An owner bound before the migration, as 0002 leaves it.
  await env.DB.prepare(
    "INSERT INTO phones (conversation_id, wa_id, city_id, bound_at, bound_wamid, last_inbound_at) VALUES (?1, ?1, ?2, ?3, ?4, ?3)",
  )
    .bind(wa, city.id, t.clock.now, `wamid.${uniq()}`)
    .run();
  const owner = await t.say(wa, "an owner during the gap");
  expect(owner.res.status).toBe(200);
  expect((await t.inbox(city)).body.deliveries.some((d: any) => d.message?.id === owner.wamid)).toBe(true);
  const sent = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a reply during the gap" });
  expect(sent.status).toBe(200);
  expect((await t.webhook(statusPayload(sent.body.message_id, "delivered", wa, t.clock.now))).status).toBe(200);
  expect((await t.inbox(city)).body.deliveries.some((d: any) => d.kind === "status" && d.message?.id === sent.body.message_id)).toBe(true);
  expect((await t.window(city)).body.windows).toEqual([expect.objectContaining({ conversation_id: wa, last_inbound_at: t.clock.now })]);
  expect((await t.say(phone(), "a stranger during the gap")).res.status).toBe(500);
  const code = await t.invite(city);
  expect((await t.say(phone(), code)).res.status).toBe(500);
});
