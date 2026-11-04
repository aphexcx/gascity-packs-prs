/**
 * The 24-hour customer service window, per phone. Meta: "When a WhatsApp user messages you or
 * calls you, a 24-hour timer called a customer service window starts", and only template messages
 * may be sent outside it (developers.facebook.com/docs/whatsapp/pricing, read 2026-11-04).
 *
 * The clock is the phone's last_inbound_at: Meta's own timestamp of the owner's last customer
 * message. Only an inbound customer message moves it. No send of any type moves it, a template
 * included, so a template leaves a closed window closed until the owner writes again. The 120-second
 * margin keeps a send that leaves at the edge from reaching Meta after the close. GET /v1/window
 * answers with the same arithmetic the send check uses, so the box and the gateway never disagree on
 * whether a send will be refused.
 */
export const WINDOW_SECONDS = 86_400;
export const WINDOW_MARGIN_SECONDS = 120;

/** The first second at which a free-form send is refused; null when the phone never wrote. */
export function windowClosesAt(lastInboundAt: number | null): number | null {
  return lastInboundAt === null ? null : lastInboundAt + WINDOW_SECONDS - WINDOW_MARGIN_SECONDS;
}

export function windowOpen(lastInboundAt: number | null, now: number): boolean {
  const close = windowClosesAt(lastInboundAt);
  return close !== null && now < close;
}

/**
 * The message types that are the owner writing to the business. A reaction, a system notice and
 * a type Meta marks unsupported are delivered to the city but do not move the clock. Meta's pages
 * name only "messages or calls" as triggers, so the conservative reading: a wrong refusal costs a
 * template, while a wrong open costs a send that Meta refuses with 131047.
 */
const CUSTOMER_MESSAGE_TYPES = new Set([
  "text",
  "image",
  "audio",
  "video",
  "document",
  "sticker",
  "location",
  "contacts",
  "interactive",
  "button",
  "order",
]);

export function movesWindow(messageType: string): boolean {
  return CUSTOMER_MESSAGE_TYPES.has(messageType);
}
