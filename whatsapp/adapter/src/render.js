// render.js — one gateway delivery to gc's ExternalInboundMessage
// (internal/extmsg/types.go:85-95 at gc's fork head f22b3ed4a).
//
// A delivery is {id, received_at, kind: message|status|joined, wa_id,
// profile_name, message} (the gateway contract v1); message is the Meta
// Cloud API object as the webhook gave it. The request id of every delivery
// is whatsapp:dynasty:<provider_message_id>, where provider_message_id is
// the wamid for a message or a joined delivery and <wamid>:status:<status>
// for a failed-status delivery, so a status never collides with the
// outbound transcript record of the message it reports on.

import { conversationRef } from './gc.js';

const MEDIA_TYPES = new Set(['image', 'document', 'audio', 'video', 'sticker']);

export function providerMessageId(delivery) {
  const message = delivery?.message ?? {};
  const wamid = typeof message.id === 'string' ? message.id.trim() : '';
  if (!wamid) return '';
  if (delivery.kind === 'status') return `${wamid}:status:${String(message.status ?? '').trim()}`;
  return wamid;
}

export const ridFor = (pmid) => `whatsapp:dynasty:${pmid}`;
export const pmidOfRid = (rid) => String(rid).replace(/^whatsapp:dynasty:/, '');

export function lastFour(waId) {
  const digits = String(waId ?? '').replace(/\D/g, '');
  return digits.slice(-4) || '????';
}

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

async function mediaText(type, message, delivery, media) {
  const item = message[type] ?? {};
  const caption = typeof item.caption === 'string' ? item.caption : '';
  if (!media) return { text: caption || `[${type} received; media store unavailable]`, attachments: [] };
  const result = await media.hydrate({
    waId: String(delivery.wa_id),
    wamid: String(message.id ?? ''),
    kind: type,
    media: { ...item, voice: type === 'audio' ? Boolean(item.voice) : undefined },
  });
  const block = media.describe([result]);
  const text = [caption, block].filter(Boolean).join('\n\n');
  return { text, attachments: result?.attachment ? [result.attachment] : [] };
}

export async function renderText(delivery, media) {
  const message = delivery?.message ?? {};
  if (delivery.kind === 'status') {
    const error = Array.isArray(message.errors) ? message.errors[0] ?? {} : {};
    return { text: `[delivery failed: ${oneLine(error.code)} ${oneLine(error.title)}]`.replace(/\s+\]/, ']'), attachments: [] };
  }
  if (delivery.kind === 'joined') {
    const name = oneLine(delivery.profile_name) || 'An owner';
    return { text: `${name} joined from ${lastFour(delivery.wa_id)}`, attachments: [] };
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
      if (MEDIA_TYPES.has(type)) return mediaText(type, message, delivery, media);
      return { text: `[unsupported WhatsApp message: ${oneLine(type) || 'unknown'}]`, attachments: [] };
  }
}

// The ExternalInboundMessage gc takes. marked prefixes the one marker line of
// the in-doubt rule's single marked copy; gc's transcript dedup on
// provider_message_id keeps it out of the durable record.
export async function renderMessage(delivery, { city, marked = false, media } = {}) {
  const pmid = providerMessageId(delivery);
  const waId = String(delivery.wa_id ?? '');
  const { text, attachments } = await renderText(delivery, media);
  const message = {
    provider_message_id: pmid,
    conversation: conversationRef(city, waId),
    actor: { id: waId, display_name: oneLine(delivery.profile_name) || waId, is_bot: false },
    text: marked ? `${markerLine(pmid)}\n${text}` : text,
    dedup_key: ridFor(pmid),
    received_at: receivedAt(delivery),
  };
  if (attachments.length) message.attachments = attachments;
  const context = delivery?.message?.context?.id;
  if (typeof context === 'string' && context) message.reply_to_message_id = context;
  return message;
}
