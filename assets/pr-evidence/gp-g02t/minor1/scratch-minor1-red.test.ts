// SCRATCH (gp-g02t evidence, never committed under test/): the head's Worker-entry test with
// af7e2d9b's seed (a row with no user id), to show the old seed took the stranger path.
import { createScheduledController, env } from "cloudflare:test";
import { describe, expect, it, vi } from "vitest";
import { signatureHeader } from "../src/meta.js";
import worker from "../src/index.js";
import { phone, textPayload, uniq, userOf } from "./harness.js";

describe("gp-g02t MINOR 1 RED", () => {
  it("RED at af7e2d9b's seed: the Worker entry's own console lines (a webhook, a refused handshake, the cron) carry no phone, body, token or secret; the bound owner's message reaches its city and the entry calls no network", async () => {
    const spy = vi.spyOn(console, "log").mockImplementation(() => {});
    // The entry's Graph client is the global fetch (src/index.ts). Mocked, so the suite never
    // reaches graph.facebook.com, and asserted untouched below: a bound owner's message sends
    // nothing (no canned line, no welcome). (gp-g02t; the gate r4 MINOR 2, the Opus read r1
    // finding 1: with the seed this test had, a row with no user id, the payload's user id
    // made the message a stranger's and the canned line went out through the real fetch.)
    const net = vi.spyOn(globalThis, "fetch").mockImplementation(async () => new Response('{"error":{"message":"no network in this test"}}', { status: 500 }));
    try {
      // A bound owner, as this branch's bind writes one (gateway.ts bind: keyed by the binding
      // text's number, wa_id NULL, answering to its user id): the payload carries the same user
      // id (textPayload: userOf), so the message routes to the city.
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
      const delivered = await env.DB.prepare("SELECT kind, wa_id, user_id, conversation_id FROM deliveries WHERE city_id = ?").bind(cityId).all();
      expect(delivered.results).toEqual([{ kind: "message", wa_id: wa, user_id: userOf(wa), conversation_id: wa }]);
      await worker.fetch(new Request(`https://whatsapp.dynasty.computer/meta/webhook?hub.mode=subscribe&hub.verify_token=nope-${wa}&hub.challenge=1`), env);
      await worker.scheduled(createScheduledController({ scheduledTime: Date.now(), cron: "17 4 * * *" }), env);
      expect(net.mock.calls.map((c) => String(c[0] instanceof Request ? c[0].url : c[0]))).toEqual([]);
      const lines = spy.mock.calls.map((c) => c.map(String).join(" "));
      expect(lines.some((l) => l.includes('"event":"maintenance"'))).toBe(true);
      expect(lines.some((l) => l.includes('"event":"webhook"'))).toBe(true);
      for (const needle of [wa, "9876", "entry body", "nope-", env.META_APP_SECRET, env.META_ACCESS_TOKEN, env.META_VERIFY_TOKEN, env.ADMIN_TOKEN]) {
        for (const line of lines) expect(line).not.toContain(needle);
      }
    } finally {
      net.mockRestore();
      spy.mockRestore();
    }
  });

});
