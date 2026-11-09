// render.js — one gateway delivery to gc's ExternalInboundMessage
// (internal/extmsg/types.go:85-95 at gc's fork head f22b3ed4a).
//
// A delivery is {id, received_at, kind: message|status|joined, wa_id,
// user_id, username, conversation_id, profile_name, message} (PR 76's inbox,
// gateway.ts inbox(); the gateway contract v1 of PR 73 has no user_id,
// username or conversation_id); message is the Meta Cloud API object as the
// webhook gave it. The request id of every delivery is
// whatsapp:dynasty:<provider_message_id>, where provider_message_id is the
// wamid for a message or a joined delivery and <wamid>:status:<status> for a
// failed-status delivery, so a status never collides with the outbound
// transcript record of the message it reports on.

import { conversationRef } from './gc.js';
import { lastFour } from './ledger.js';

const MEDIA_TYPES = new Set(['image', 'document', 'audio', 'video', 'sticker']);

// THE OWNER'S KEY (gp-g02t; the mayor's gate r4 MAJOR 1 on PR 76). One
// function names the owner a delivery belongs to. The inbound path calls it
// once, when it writes the delivery's received line, and every later step
// reads the key from that line (the journal's fold: conversationId): the gc
// conversation and the actor, the chain, the lookup boundary and the
// watermark, the paste probe, the dead letter, the window ledger, the
// keeper's bind and the media directory.
//   - PR 76's gateway gives every delivery the field: the owner's
//     conversation_id, fixed at the bind and never changed (a number change
//     keeps it; two owners bound at once never share it, even on a recycled
//     number). A delivery that names it
//     null or empty has no owner: no key, left unacknowledged.
//   - The gateway contract v1 (PR 73) has no such field: its wa_id is that
//     contract's one owner key. The boxes run it until the gateway's deploy,
//     since this adapter rolls first (whatsapp/gateway/README.md, Deploy).
// PR 76's wa_id is only the number the message, or the owner's newest
// pairing, shows: NULL for an owner known by user id alone, a new number
// after a number change, the same number for two owners after a recycle. It
// is display (numberOf), never a key.
export function conversationOf(delivery) {
  if (!delivery || typeof delivery !== 'object') return '';
  const key = Object.hasOwn(delivery, 'conversation_id') ? delivery.conversation_id : delivery.wa_id;
  return typeof key === 'string' ? key : '';
}

// The number a delivery shows, for display only ('' when it shows none).
export function numberOf(delivery) {
  return typeof delivery?.wa_id === 'string' ? delivery.wa_id : '';
}

export function providerMessageId(delivery) {
  const message = delivery?.message ?? {};
  const wamid = typeof message.id === 'string' ? message.id.trim() : '';
  if (!wamid) return '';
  if (delivery.kind === 'status') return `${wamid}:status:${String(message.status ?? '').trim()}`;
  return wamid;
}

export const ridFor = (pmid) => `whatsapp:dynasty:${pmid}`;
export const pmidOfRid = (rid) => String(rid).replace(/^whatsapp:dynasty:/, '');

// Logs and operator surfaces show an id's last four letters or digits, never
// the id (ledger.js lastFour, the one rule).
export { lastFour };

export function statusOf(delivery) {
  return String(delivery?.message?.status ?? '').trim();
}

export function markerLine(pmid) {
  return `[redelivery ${pmid}: a first copy may have reached you; if you already answered it, do nothing]`;
}

function receivedAt(delivery) {
  const ts = Number(delivery?.message?.timestamp);
  if (Number.isFinite(ts) && ts > 0) return new Date(ts * 1000).toISOString();
  const at = delivery?.received_at;
  if (typeof at === 'number') return new Date(at < 1e12 ? at * 1000 : at).toISOString();
  if (typeof at === 'string' && !Number.isNaN(Date.parse(at))) return new Date(Date.parse(at)).toISOString();
  return new Date(0).toISOString();
}

// The one inbound-message instant the window ledger uses (epoch ms).
export function inboundAtMs(delivery) {
  return Date.parse(receivedAt(delivery));
}

function oneLine(value) {
  return String(value ?? '').replace(/\s+/g, ' ').trim();
}

async function mediaText(type, message, conversationId, media) {
  const item = message[type] ?? {};
  const caption = typeof item.caption === 'string' ? item.caption : '';
  if (!media) return { text: caption || `[${type} received; media store unavailable]`, attachments: [] };
  const result = await media.hydrate({
    conversationId,
    wamid: String(message.id ?? ''),
    kind: type,
    media: { ...item, voice: type === 'audio' ? Boolean(item.voice) : undefined },
  });
  if (result?.transient) {
    // Thrown before the posted line: the step retries on the ladder and the
    // gateway keeps the delivery; a permanent failure posts with its note.
    throw Object.assign(new Error(`media download did not complete (${result.note ?? 'transient'}); retrying`), { transientMedia: true });
  }
  const block = media.describe([result]);
  const text = [caption, block].filter(Boolean).join('\n\n');
  return { text, attachments: result?.attachment ? [result.attachment] : [] };
}

// conversationId names the media store's directory (the owner's, from the
// received line's key).
export async function renderText(delivery, media, conversationId = '') {
  const message = delivery?.message ?? {};
  if (delivery.kind === 'status') {
    const error = Array.isArray(message.errors) ? message.errors[0] ?? {} : {};
    return { text: `[delivery failed: ${oneLine(error.code)} ${oneLine(error.title)}]`.replace(/\s+\]/, ']'), attachments: [] };
  }
  if (delivery.kind === 'joined') {
    const name = oneLine(delivery.profile_name) || 'An owner';
    const number = numberOf(delivery);
    return { text: number ? `${name} joined from ${lastFour(number)}` : `${name} joined (no phone number shown)`, attachments: [] };
  }
  const type = String(message.type ?? '');
  switch (type) {
    case 'text':
      return { text: String(message.text?.body ?? ''), attachments: [] };
    case 'reaction':
      return { text: `[reaction ${message.reaction?.emoji ?? ''} to ${message.reaction?.message_id ?? ''}]`, attachments: [] };
    case 'location': {
      const loc = message.location ?? {};
      const parts = [`${loc.latitude}, ${loc.longitude}`];
      if (loc.name) parts.push(oneLine(loc.name));
      if (loc.address) parts.push(oneLine(loc.address));
      return { text: `[location ${parts.join(', ')}]`, attachments: [] };
    }
    case 'interactive': {
      const reply = message.interactive?.button_reply ?? message.interactive?.list_reply ?? {};
      return { text: `${oneLine(reply.title)} [${oneLine(reply.id)}]`, attachments: [] };
    }
    case 'button':
      return { text: `${oneLine(message.button?.text)} [${oneLine(message.button?.payload)}]`, attachments: [] };
    default:
      if (MEDIA_TYPES.has(type)) return mediaText(type, message, conversationId, media);
      return { text: `[unsupported WhatsApp message: ${oneLine(type) || 'unknown'}]`, attachments: [] };
  }
}

// The ExternalInboundMessage gc takes. marked prefixes the one marker line of
// the in-doubt rule's single marked copy; gc's transcript dedup on
// provider_message_id keeps it out of the durable record. conversationId is
// the key of the delivery's received line (THE OWNER'S KEY above): the gc
// conversation and the actor id are the owner's, whatever number it shows.
export async function renderMessage(delivery, { city, conversationId, marked = false, media } = {}) {
  const pmid = providerMessageId(delivery);
  const key = String(conversationId ?? '');
  const { text, attachments } = await renderText(delivery, media, key);
  const message = {
    provider_message_id: pmid,
    conversation: conversationRef(city, key),
    actor: { id: key, display_name: oneLine(delivery.profile_name) || numberOf(delivery) || key, is_bot: false },
    text: marked ? `${markerLine(pmid)}\n${text}` : text,
    dedup_key: ridFor(pmid),
    received_at: receivedAt(delivery),
  };
  if (attachments.length) message.attachments = attachments;
  const context = delivery?.message?.context?.id;
  if (typeof context === 'string' && context) message.reply_to_message_id = context;
  return message;
}
