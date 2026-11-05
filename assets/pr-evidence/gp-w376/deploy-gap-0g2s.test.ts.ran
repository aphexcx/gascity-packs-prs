/**
 * Run only by evidence.py (next to this file). That script copies it into whatsapp/gateway/test/,
 * with src/ written back to the deployed Worker (392f9c09, PR 73) and migrations 0001, 0002 and
 * 0003 applied: the gap between README.md's Deploy steps 3 and 4. It covers what 0003 changes
 * under the old code: the dedup key without the city, and the media grant.
 */
import { env } from "cloudflare:test";
import { expect, it } from "vitest";
import { mediaPayload, phone, setup, statusPayload, textPayload, uniq } from "./harness.js";

it("the deployed Worker on the v3 schema: a redelivered message and a redelivered status are each one row, and an owner's image is served to its city", async () => {
  const t = setup();
  const city = await t.enrollCity();
  const wa = phone();
  await env.DB.prepare(
    "INSERT INTO phones (conversation_id, wa_id, city_id, bound_at, bound_wamid, last_inbound_at) VALUES (?1, ?1, ?2, ?3, ?4, ?3)",
  )
    .bind(wa, city.id, t.clock.now, `wamid.${uniq()}`)
    .run();
  const wamid = `wamid.${uniq()}`;
  const text = textPayload(wa, "an owner during the gap", { wamid, ts: t.clock.now });
  expect((await t.webhook(text)).status).toBe(200);
  expect((await t.webhook(text)).status).toBe(200);
  expect((await t.inbox(city)).body.deliveries.filter((d: any) => d.message?.id === wamid)).toHaveLength(1);
  const sent = await t.send(city, { to: wa, idempotency_key: uniq(), type: "text", text: "a reply during the gap" });
  expect(sent.status).toBe(200);
  const status = statusPayload(sent.body.message_id, "delivered", wa, t.clock.now);
  expect((await t.webhook(status)).status).toBe(200);
  expect((await t.webhook(status)).status).toBe(200);
  expect((await t.inbox(city)).body.deliveries.filter((d: any) => d.kind === "status" && d.message?.id === sent.body.message_id)).toHaveLength(1);
  const mediaId = `8${Date.now()}${Math.floor(Math.random() * 1000)}`;
  t.graph.media.set(mediaId, { mime: "image/jpeg", bytes: new Uint8Array([255, 216, 255]) });
  expect((await t.webhook(mediaPayload("image", wa, mediaId, { wamid: `wamid.${uniq()}`, ts: t.clock.now }))).status).toBe(200);
  expect((await t.call("GET", `/v1/media/${mediaId}`, { token: city.token })).status).toBe(200);
});
