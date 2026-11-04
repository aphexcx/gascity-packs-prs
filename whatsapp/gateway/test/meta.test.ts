/**
 * The Cloud API wire (src/meta.ts) and the window arithmetic (src/window.ts), against Meta's own
 * example payloads (test/fixtures, sources in test/fixtures/SOURCES.md) and a fixed signature
 * vector computed outside this code.
 */
import { describe, expect, it } from "vitest";
import {
  GraphError,
  graphMarkRead,
  graphMedia,
  graphSend,
  parseWebhook,
  readBody,
  sameSecret,
  sendBody,
  sha256Hex,
  signatureHeader,
  timingSafeEqual,
  verifySignature,
} from "../src/meta.js";
import { movesWindow, WINDOW_MARGIN_SECONDS, windowClosesAt, windowOpen } from "../src/window.js";
import audio from "./fixtures/audio.json";
import image from "./fixtures/image.json";
import reaction from "./fixtures/reaction.json";
import statusFailed from "./fixtures/status-failed.json";
import statusSent from "./fixtures/status-sent.json";
import text from "./fixtures/text.json";
import { fakeGraph, PNID } from "./harness.js";

const enc = (s: string) => new TextEncoder().encode(s);

// The vector: HMAC-SHA256 of these exact bytes under this secret, computed with Python's hmac
// module (python3 -I -c 'import hmac,hashlib; ...'), never by the code under test.
const VECTOR_BODY = '{"object":"whatsapp_business_account","entry":[]}';
const VECTOR_SECRET = "test-app-secret-vector";
const VECTOR_HEADER = "sha256=84759f4c2dc11cd9d9d12e538d3d1c41af6cf19a40975f946220aab685275d2f";

describe("signature (X-Hub-Signature-256 over the raw bytes)", () => {
  it("a known body and secret give the known sha256= header", async () => {
    expect(await signatureHeader(enc(VECTOR_BODY), VECTOR_SECRET)).toBe(VECTOR_HEADER);
    expect(await verifySignature(enc(VECTOR_BODY), VECTOR_HEADER, VECTOR_SECRET)).toBe(true);
  });

  it("a header in upper-case hex still verifies (the digest, not its spelling)", async () => {
    const upper = `sha256=${VECTOR_HEADER.slice(7).toUpperCase()}`;
    expect(await verifySignature(enc(VECTOR_BODY), upper, VECTOR_SECRET)).toBe(true);
  });

  it("one tampered byte fails", async () => {
    const tampered = enc(VECTOR_BODY);
    tampered[10] = (tampered[10] ?? 0) ^ 0x01;
    expect(await verifySignature(tampered, VECTOR_HEADER, VECTOR_SECRET)).toBe(false);
  });

  it("a missing header, a wrong prefix, a short digest, non-hex and a wrong secret all fail", async () => {
    const body = enc(VECTOR_BODY);
    expect(await verifySignature(body, null, VECTOR_SECRET)).toBe(false);
    expect(await verifySignature(body, "", VECTOR_SECRET)).toBe(false);
    expect(await verifySignature(body, VECTOR_HEADER.replace("sha256=", "sha1="), VECTOR_SECRET)).toBe(false);
    expect(await verifySignature(body, VECTOR_HEADER.slice(0, -2), VECTOR_SECRET)).toBe(false);
    expect(await verifySignature(body, `sha256=${"zz".repeat(32)}`, VECTOR_SECRET)).toBe(false);
    expect(await verifySignature(body, VECTOR_HEADER, "another-secret")).toBe(false);
    expect(await verifySignature(body, VECTOR_HEADER, "")).toBe(false);
  });

  it("the constant-time compare and the secret compare answer exactly equality", async () => {
    expect(timingSafeEqual(enc("abc"), enc("abc"))).toBe(true);
    expect(timingSafeEqual(enc("abc"), enc("abd"))).toBe(false);
    expect(timingSafeEqual(enc("abc"), enc("abcd"))).toBe(false);
    expect(await sameSecret("s3cret", "s3cret")).toBe(true);
    expect(await sameSecret("s3cret", "s3creT")).toBe(false);
    expect(await sameSecret("", "")).toBe(false); // an unset secret never matches
    expect(await sha256Hex("wgc_example")).toBe("4c78f3731b7490e5aeb1ccce558779e50f75676a1fe1451f70e6b326c7cedd6d");
  });
});

describe("webhook payloads (Meta's reference examples)", () => {
  it("a text message: sender, wamid, Meta's timestamp, the body and the profile name", () => {
    const b = parseWebhook(text, PNID);
    expect(b.wrongNumber).toBe(0);
    expect(b.statuses).toEqual([]);
    expect(b.messages).toHaveLength(1);
    const m = b.messages[0]!;
    expect(m).toMatchObject({
      wamid: "wamid.HBgLMTY1MDM4Nzk0MzkVAgASGBQzQTRBNjU5OUFFRTAzODEwMTQ0RgA=",
      from: "16505551234",
      timestamp: 1749416383,
      type: "text",
      profileName: "Sheena Nelson",
      text: "Does it come in another color?",
      mediaId: null,
    });
    expect(m.raw).toEqual(text.entry[0]!.changes[0]!.value.messages[0]);
  });

  it("a reaction carries no text and no media", () => {
    const m = parseWebhook(reaction, PNID).messages[0]!;
    expect(m.type).toBe("reaction");
    expect(m.text).toBeNull();
    expect(m.mediaId).toBeNull();
    expect((m.raw.reaction as Record<string, string>).emoji).toBe("\u{1F44D}");
  });

  it("an image and a voice note carry their media id", () => {
    expect(parseWebhook(image, PNID).messages[0]).toMatchObject({ type: "image", mediaId: "1003383421387256" });
    const voice = parseWebhook(audio, PNID).messages[0]!;
    expect(voice).toMatchObject({ type: "audio", mediaId: "1908647269898587" });
    expect((voice.raw.audio as Record<string, unknown>).voice).toBe(true);
  });

  it("statuses: sent and failed, with the recipient and Meta's object whole", () => {
    const sent = parseWebhook(statusSent, PNID);
    expect(sent.messages).toEqual([]);
    expect(sent.statuses).toHaveLength(1);
    expect(sent.statuses[0]).toMatchObject({
      wamid: "wamid.HBgLMTY1MDM4Nzk0MzkVAgASGBQzQUFERjg0NDEzNDdFODU3MUMxMAA=",
      status: "sent",
      recipientId: "16505551234",
    });
    const failed = parseWebhook(statusFailed, PNID).statuses[0]!;
    expect(failed.status).toBe("failed");
    expect((failed.raw.errors as Array<{ code: number }>)[0]!.code).toBe(131049);
  });

  it("a payload for another phone number id is counted and yields nothing", () => {
    const b = parseWebhook(text, "999999999999999");
    expect(b.messages).toEqual([]);
    expect(b.wrongNumber).toBe(1);
  });

  it("other objects, other fields and malformed entries are ignored, never thrown", () => {
    expect(parseWebhook({ object: "page", entry: [] }, PNID)).toMatchObject({ messages: [], statuses: [] });
    const templateUpdate = { object: "whatsapp_business_account", entry: [{ changes: [{ field: "message_template_status_update", value: {} }] }] };
    expect(parseWebhook(templateUpdate, PNID).ignored).toBe(1);
    for (const junk of [null, 7, "x", [], { entry: "x" }, { object: "whatsapp_business_account", entry: [{ changes: [null, 3] }] }]) {
      expect(() => parseWebhook(junk, PNID)).not.toThrow();
    }
    const noId = JSON.parse(JSON.stringify(text));
    delete noId.entry[0].changes[0].value.messages[0].id;
    expect(parseWebhook(noId, PNID).messages).toEqual([]);
  });
});

describe("send bodies (Meta's documented shapes)", () => {
  it("text, reaction and template", () => {
    expect(sendBody("16505551234", { type: "text", text: "hello" })).toEqual({
      messaging_product: "whatsapp",
      recipient_type: "individual",
      to: "16505551234",
      type: "text",
      text: { body: "hello", preview_url: false },
    });
    expect(sendBody("16505551234", { type: "reaction", messageId: "wamid.X", emoji: "\u{1F600}" })).toEqual({
      messaging_product: "whatsapp",
      recipient_type: "individual",
      to: "16505551234",
      type: "reaction",
      reaction: { message_id: "wamid.X", emoji: "\u{1F600}" },
    });
    expect(
      sendBody("16505551234", { type: "template", name: "dynasty_update", language: "en_US", params: ["The shipment cleared."] }),
    ).toEqual({
      messaging_product: "whatsapp",
      recipient_type: "individual",
      to: "16505551234",
      type: "template",
      template: {
        name: "dynasty_update",
        language: { code: "en_US" },
        components: [{ type: "body", parameters: [{ type: "text", text: "The shipment cleared." }] }],
      },
    });
  });

  it("mark-as-read carries the typing indicator", () => {
    expect(readBody("wamid.IN")).toEqual({
      messaging_product: "whatsapp",
      status: "read",
      message_id: "wamid.IN",
      typing_indicator: { type: "text" },
    });
  });
});

describe("the Graph client", () => {
  const client = (fetch: typeof globalThis.fetch) => ({
    base: "https://graph.facebook.com",
    version: "v26.0",
    phoneNumberId: PNID,
    accessToken: "tok",
    fetch,
  });

  it("send posts to /<version>/<phone number id>/messages with the token and returns the wamid", async () => {
    const g = fakeGraph();
    const wamid = await graphSend(client(g.fetch), sendBody("16505551234", { type: "text", text: "hi" }));
    expect(wamid).toMatch(/^wamid\.OUT/);
    expect(g.calls[0]!.url.href).toBe(`https://graph.facebook.com/v26.0/${PNID}/messages`);
    expect(g.calls[0]!.authorization).toBe("Bearer tok");
  });

  it("a Graph error becomes a GraphError with Meta's status and code; a network failure has status 0", async () => {
    const g = fakeGraph();
    g.failNext({ status: 400, body: { error: { code: 131047, message: "Re-engagement message" } } }, "network");
    const first = await graphSend(client(g.fetch), {}).catch((e: unknown) => e);
    expect(first).toBeInstanceOf(GraphError);
    expect(first).toMatchObject({ status: 400, code: 131047 });
    const second = await graphSend(client(g.fetch), {}).catch((e: unknown) => e);
    expect(second).toMatchObject({ status: 0, code: null });
  });

  it("an answer without a message id is an error, never an empty wamid", async () => {
    const fetch = (async () => Response.json({ messages: [] })) as unknown as typeof globalThis.fetch;
    await expect(graphSend(client(fetch), {})).rejects.toBeInstanceOf(GraphError);
  });

  it("mark-as-read posts the read body", async () => {
    const g = fakeGraph();
    await graphMarkRead(client(g.fetch), "wamid.IN");
    expect(g.reads()[0]!.body).toEqual(readBody("wamid.IN"));
  });

  it("media: mints the URL for the phone number id, then fetches the bytes with the token", async () => {
    const g = fakeGraph();
    g.media.set("777", { mime: "image/jpeg", bytes: new Uint8Array([1, 2, 3]) });
    const res = await graphMedia(client(g.fetch), "777");
    expect(res.headers.get("content-type")).toBe("image/jpeg");
    expect(new Uint8Array(await res.arrayBuffer())).toEqual(new Uint8Array([1, 2, 3]));
    expect(g.calls[0]!.url.href).toBe(`https://graph.facebook.com/v26.0/777?phone_number_id=${PNID}`);
    expect(g.calls[1]!.url.hostname).toBe("lookaside.fbsbx.com");
    expect(g.calls.map((c) => c.authorization)).toEqual(["Bearer tok", "Bearer tok"]);
  });

  it("media: a URL that is not https is refused before the token is sent", async () => {
    const calls: string[] = [];
    const fetch = (async (input: RequestInfo | URL) => {
      calls.push(String(input instanceof Request ? input.url : input));
      return Response.json({ url: "http://evil.example/x", mime_type: "image/jpeg" });
    }) as unknown as typeof globalThis.fetch;
    await expect(graphMedia(client(fetch), "777")).rejects.toBeInstanceOf(GraphError);
    expect(calls).toHaveLength(1);
  });
});

describe("the 24-hour window (120-second margin)", () => {
  it("closes at last inbound + 24 h - 120 s; null when the phone never wrote", () => {
    expect(WINDOW_MARGIN_SECONDS).toBe(120);
    expect(windowClosesAt(1000)).toBe(1000 + 86_400 - 120);
    expect(windowClosesAt(null)).toBeNull();
  });

  it("open strictly before the close, closed at and after it, closed when the phone never wrote", () => {
    const close = 1000 + 86_400 - 120;
    expect(windowOpen(1000, 1000)).toBe(true);
    expect(windowOpen(1000, close - 1)).toBe(true);
    expect(windowOpen(1000, close)).toBe(false);
    expect(windowOpen(1000, 1000 + 86_400 + 60)).toBe(false);
    expect(windowOpen(null, 1000)).toBe(false);
  });

  it("a customer message moves it; a reaction, a system message and an unknown type do not", () => {
    for (const t of ["text", "image", "audio", "video", "document", "sticker", "location", "contacts", "interactive", "button"]) {
      expect(movesWindow(t)).toBe(true);
    }
    for (const t of ["reaction", "system", "unsupported", "unknown", "request_welcome", ""]) {
      expect(movesWindow(t)).toBe(false);
    }
  });
});
