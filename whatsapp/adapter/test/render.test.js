// The fixtures: each gateway delivery to its ExternalInboundMessage.

import assert from 'node:assert/strict';
import test from 'node:test';

import { providerMessageId, renderMessage, ridFor } from '../src/render.js';

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
    const path = `/city/.gc/whatsapp-media/inbound/${args.waId}/${args.wamid}-${args.media.filename ?? args.kind}`;
    return { ok: true, path, attachment: { provider_id: args.media.id, url: `file://${path}`, mime_type: args.media.mime_type }, transcript: args.kind === 'audio' ? 'hello from a voice note' : undefined };
  },
  describe(results) {
    return `[${results.length} WhatsApp file(s) attached]\n${results.map((r) => `saved to ${r.path}; Read that path to view it${r.transcript ? `\ntranscript: ${r.transcript}` : ''}`).join('\n')}`;
  },
};

async function render(d, opts = {}) {
  return renderMessage(d, { city: CITY, media: fakeMedia, ...opts });
}

test('text: the envelope, the actor, the dedup key and the message time', async () => {
  const m = await render(delivery({ type: 'text', text: { body: 'bom dia' } }));
  assert.deepEqual(m.conversation, { scope_id: CITY, provider: 'whatsapp', account_id: 'dynasty', conversation_id: WA, kind: 'dm' });
  assert.deepEqual(m.actor, { id: WA, display_name: 'Taylor', is_bot: false });
  assert.equal(m.provider_message_id, 'wamid.F1');
  assert.equal(m.dedup_key, 'whatsapp:dynasty:wamid.F1');
  assert.equal(m.text, 'bom dia');
  assert.equal(m.received_at, new Date(Number(TS) * 1000).toISOString());
});

test('the profile name falls back to the wa_id', async () => {
  const m = await render(delivery({ type: 'text', text: { body: 'x' } }, 'message', { profile_name: '' }));
  assert.equal(m.actor.display_name, WA);
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

test('joined: "<name> joined from <last four digits>"', async () => {
  const m = await render(delivery({ type: 'text', text: { body: '482913' } }, 'joined', { profile_name: 'Taylor' }));
  assert.equal(m.text, 'Taylor joined from 4321');
  assert.equal(m.provider_message_id, 'wamid.F1');
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
