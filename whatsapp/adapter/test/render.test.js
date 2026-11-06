// The fixtures: each gateway delivery to its ExternalInboundMessage.

import assert from 'node:assert/strict';
import test from 'node:test';

import { conversationOf, numberOf, providerMessageId, renderMessage, ridFor } from '../src/render.js';

const CITY = 'test-city';
const WA = '5511987654321';
const TS = '1793815924';

function delivery(message, kind = 'message', extra = {}) {
  return { id: 7, received_at: '2026-11-04T18:12:05Z', kind, wa_id: WA, profile_name: 'Taylor', message: { from: WA, id: 'wamid.F1', timestamp: TS, ...message }, ...extra };
}

const fakeMedia = {
  calls: [],
  async hydrate(args) {
    this.calls.push(args);
    const path = `/city/.gc/whatsapp-media/inbound/${args.conversationId}/${args.wamid}-${args.media.filename ?? args.kind}`;
    return { ok: true, path, attachment: { provider_id: args.media.id, url: `file://${path}`, mime_type: args.media.mime_type }, transcript: args.kind === 'audio' ? 'hello from a voice note' : undefined };
  },
  describe(results) {
    return `[${results.length} WhatsApp file(s) attached]\n${results.map((r) => `saved to ${r.path}; Read that path to view it${r.transcript ? `\ntranscript: ${r.transcript}` : ''}`).join('\n')}`;
  },
};

// The inbound path renders with its received line's key (conversationOf).
async function render(d, opts = {}) {
  return renderMessage(d, { city: CITY, conversationId: conversationOf(d), media: fakeMedia, ...opts });
}

// PR 76's inbox shape: the owner's conversation_id beside the number shown.
const P = '5511900000001';
const BSUID = 'US.13491208655302741918';
function v2(message, { conversation_id, wa_id, kind = 'message', profile_name = 'Taylor' }) {
  return {
    id: 9, received_at: 1793815924, kind, wa_id, user_id: BSUID, username: 'taylor', conversation_id, profile_name,
    message: { ...(wa_id ? { from: wa_id } : {}), from_user_id: BSUID, id: 'wamid.V2', timestamp: TS, ...message },
  };
}

test('the owner\'s key: PR 76\'s conversation_id when the field is there (null or empty is no key, never the number); the contract v1\'s wa_id only when it is not', () => {
  const text = { type: 'text', text: { body: 'x' } };
  assert.deepEqual(
    [
      conversationOf(v2(text, { conversation_id: P, wa_id: '5511900000077' })),
      conversationOf(v2(text, { conversation_id: BSUID, wa_id: null })),
      conversationOf(v2(text, { conversation_id: null, wa_id: P })),
      conversationOf(v2(text, { conversation_id: '', wa_id: P })),
      conversationOf(delivery(text)),
      conversationOf({ ...delivery(text), wa_id: null }),
      conversationOf({ ...delivery(text), wa_id: 5511987654321 }),
      conversationOf(undefined),
    ],
    [P, BSUID, '', '', WA, '', '', ''],
  );
  assert.deepEqual([numberOf(v2(text, { conversation_id: BSUID, wa_id: null })), numberOf(delivery(text))], ['', WA], 'the number, display only');
});

test('PR 76: the conversation and the actor are the owner\'s conversation_id whatever number the delivery shows; the media directory too', async () => {
  const moved = await render(v2({ type: 'image', image: { id: 'm9', mime_type: 'image/jpeg', sha256: 'abc' } }, { conversation_id: P, wa_id: '5511900000077' }));
  assert.equal(moved.conversation.conversation_id, P);
  assert.deepEqual(moved.actor, { id: P, display_name: 'Taylor', is_bot: false });
  assert.equal(fakeMedia.calls.at(-1).conversationId, P, 'the media store files the owner\'s files under its conversation');
  const bare = await render(v2({ type: 'text', text: { body: 'x' } }, { conversation_id: BSUID, wa_id: null, profile_name: '' }));
  assert.equal(bare.conversation.conversation_id, BSUID);
  assert.deepEqual(bare.actor, { id: BSUID, display_name: BSUID, is_bot: false }, 'no profile name and no number: the id itself');
});

test('text: the envelope, the actor, the dedup key and the message time', async () => {
  const m = await render(delivery({ type: 'text', text: { body: 'bom dia' } }));
  assert.deepEqual(m.conversation, { scope_id: CITY, provider: 'whatsapp', account_id: 'dynasty', conversation_id: WA, kind: 'dm' });
  assert.deepEqual(m.actor, { id: WA, display_name: 'Taylor', is_bot: false });
  assert.equal(m.provider_message_id, 'wamid.F1');
  assert.equal(m.dedup_key, 'whatsapp:dynasty:wamid.F1');
  assert.equal(m.text, 'bom dia');
  assert.equal(m.received_at, new Date(Number(TS) * 1000).toISOString());
});

test('the profile name falls back to the number the delivery shows', async () => {
  const m = await render(delivery({ type: 'text', text: { body: 'x' } }, 'message', { profile_name: '' }));
  assert.equal(m.actor.display_name, WA);
  const shown = await render(v2({ type: 'text', text: { body: 'x' } }, { conversation_id: BSUID, wa_id: P, profile_name: '' }));
  assert.deepEqual(shown.actor, { id: BSUID, display_name: P, is_bot: false });
});

test('image and document: the attachment record and the saved-path block', async () => {
  const image = await render(delivery({ type: 'image', image: { id: 'm1', mime_type: 'image/jpeg', sha256: 'abc', caption: 'the shipment' } }));
  assert.match(image.text, /^the shipment\n\n\[1 WhatsApp file\(s\) attached\]/);
  assert.match(image.text, /Read that path to view it/);
  assert.deepEqual(image.attachments, [{ provider_id: 'm1', url: `file:///city/.gc/whatsapp-media/inbound/${WA}/wamid.F1-image`, mime_type: 'image/jpeg' }]);
  const doc = await render(delivery({ type: 'document', document: { id: 'm2', mime_type: 'application/pdf', sha256: 'def', filename: 'invoice.pdf' } }));
  assert.match(doc.text, /wamid\.F1-invoice\.pdf/);
});

test('voice note: transcribed inline', async () => {
  const voice = await render(delivery({ type: 'audio', audio: { id: 'm3', mime_type: 'audio/ogg; codecs=opus', sha256: 'aa', voice: true } }));
  assert.match(voice.text, /transcript: hello from a voice note/);
  assert.equal(fakeMedia.calls.at(-1).media.voice, true);
});

test('location, reaction, button reply, list reply, quick-reply button, unsupported', async () => {
  const loc = await render(delivery({ type: 'location', location: { latitude: -23.55, longitude: -46.63, name: 'Loja', address: 'Rua A, 1' } }));
  assert.equal(loc.text, '[location -23.55, -46.63, Loja, Rua A, 1]');
  const reaction = await render(delivery({ type: 'reaction', reaction: { message_id: 'wamid.out.9', emoji: '👍' } }));
  assert.equal(reaction.text, '[reaction 👍 to wamid.out.9]');
  const button = await render(delivery({ type: 'interactive', interactive: { type: 'button_reply', button_reply: { id: 'ok-1', title: 'OK' } } }));
  assert.equal(button.text, 'OK [ok-1]');
  const list = await render(delivery({ type: 'interactive', interactive: { type: 'list_reply', list_reply: { id: 'l-2', title: 'Tuesday' } } }));
  assert.equal(list.text, 'Tuesday [l-2]');
  const quick = await render(delivery({ type: 'button', button: { text: 'Yes', payload: 'yes' } }));
  assert.equal(quick.text, 'Yes [yes]');
  const unsupported = await render(delivery({ type: 'unsupported', errors: [{ code: 131051 }] }));
  assert.equal(unsupported.text, '[unsupported WhatsApp message: unsupported]');
  const contacts = await render(delivery({ type: 'contacts', contacts: [] }));
  assert.equal(contacts.text, '[unsupported WhatsApp message: contacts]');
});

test('a failed status: provider_message_id <wamid>:status:failed and the error line', async () => {
  const d = { id: 8, kind: 'status', wa_id: WA, profile_name: '', message: { id: 'wamid.out.3', status: 'failed', timestamp: TS, recipient_id: WA, errors: [{ code: 131047, title: 'Re-engagement message' }] } };
  assert.equal(providerMessageId(d), 'wamid.out.3:status:failed');
  const m = await render(d);
  assert.equal(m.provider_message_id, 'wamid.out.3:status:failed');
  assert.equal(m.dedup_key, ridFor('wamid.out.3:status:failed'));
  assert.equal(m.text, '[delivery failed: 131047 Re-engagement message]');
  const delivered = { ...d, message: { ...d.message, status: 'delivered' } };
  assert.equal(providerMessageId(delivered), 'wamid.out.3:status:delivered', 'a delivered status never collides either');
});

test('joined: "<name> joined from <last four digits>", or no number shown', async () => {
  const m = await render(delivery({ type: 'text', text: { body: '482913' } }, 'joined', { profile_name: 'Taylor' }));
  assert.equal(m.text, 'Taylor joined from 4321');
  assert.equal(m.provider_message_id, 'wamid.F1');
  const bare = await render(v2({ type: 'text', text: { body: '482913' } }, { kind: 'joined', conversation_id: BSUID, wa_id: null }));
  assert.equal(bare.text, 'Taylor joined (no phone number shown)');
});

test('the marked copy carries the one marker line before the text', async () => {
  const m = await render(delivery({ type: 'text', text: { body: 'bom dia' } }), { marked: true });
  assert.equal(m.text, '[redelivery wamid.F1: a first copy may have reached you; if you already answered it, do nothing]\nbom dia');
  assert.equal(m.provider_message_id, 'wamid.F1', 'the same id, so gc keeps one transcript record');
});

test('a reply carries reply_to_message_id from the context', async () => {
  const m = await render(delivery({ type: 'text', text: { body: 'yes' }, context: { id: 'wamid.out.1' } }));
  assert.equal(m.reply_to_message_id, 'wamid.out.1');
});
