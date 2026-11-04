// gc.js — the gc API calls this adapter makes, at the wire shapes of gc's
// fork integration head f22b3ed4a5b090e6960373ffc9e7a414aa085309 (the same
// shapes at 92a7f383f unless noted):
//
//   POST /v0/city/{city}/extmsg/inbound {message}            -> InboundResult
//     (Go field names: TranscriptEntry.Sequence, TranscriptEntry.ProviderMessageID,
//     Duplicate; one tagged field, delivery: {receipt_id, status, terminal?,
//     delivered_bytes, expected_bytes, members[{session_id, status, terminal?}]}:
//     internal/extmsg/inbound.go:17-44, inbound_delivery.go:64-76)
//   GET  /v0/city/{city}/extmsg/inbound/receipts/{id}      -> {receipt_id, state: pending|concluded|unknown, delivery?}
//     (inbound_receipt_store.go:47-56; 200 in every state)
//   GET  /v0/city/{city}/extmsg/transcript?...             -> {items: [ConversationTranscriptRecord], total}
//     (after_sequence EXCLUSIVE, huma_types_extmsg.go:183; the record has no
//     JSON tags, types.go:252-268: Sequence, ProviderMessageID, CreatedAt, Kind)
//   GET  /v0/city/{city}/sessions                          -> {items: [{id, template, alias, state, running, ...}]}
//   GET  /v0/city/{city}/session/{id}/transcript           -> {format: conversation|text, turns: [{role, text, timestamp?}]}
//     (huma_handlers_sessions_query.go:255-286 log, 306-325 pane peek)
//   POST /v0/city/{city}/extmsg/adapters                   (registration)
//   POST /v0/city/{city}/extmsg/outbound                   (outbound transcript)

import { requestJSON } from './http.js';

export function createGcClient({ base, city, postTimeoutMs = 30000, readTimeoutMs = 15000, fetchImpl = fetch } = {}) {
  const root = `${String(base || '').replace(/\/+$/, '')}/v0/city/${encodeURIComponent(city || '')}`;
  const headers = { 'X-GC-Request': 'gc-whatsapp-adapter' };
  const get = (path, timeoutMs = readTimeoutMs) => requestJSON(`${root}${path}`, { headers, timeoutMs, fetchImpl });
  const post = (path, body, timeoutMs = readTimeoutMs) => requestJSON(`${root}${path}`, { method: 'POST', headers, body, timeoutMs, fetchImpl });

  return {
    root,
    // The client deadline (30 s) is past gc's 15-second receipt budget, so an
    // expiry means the connection or gc, never the budget.
    postInbound: (message) => post('/extmsg/inbound', { message }, postTimeoutMs),
    receipt: (id) => get(`/extmsg/inbound/receipts/${encodeURIComponent(id)}`),
    transcript: ({ scopeId, conversationId, after, limit, order = 'asc' }) => {
      const q = new URLSearchParams({
        scope_id: scopeId,
        provider: 'whatsapp',
        account_id: 'dynasty',
        conversation_id: conversationId,
        kind: 'dm',
        order,
      });
      if (after !== undefined && after !== null) q.set('after_sequence', String(after));
      if (limit !== undefined) q.set('limit', String(limit));
      return get(`/extmsg/transcript?${q}`);
    },
    sessions: () => get('/sessions'),
    sessionTranscript: (id) => get(`/session/${encodeURIComponent(id)}/transcript`),
    register: (body) => post('/extmsg/adapters', body),
    recordOutbound: (body) => post('/extmsg/outbound', body),
  };
}

// The conversation ref every inbound envelope, keeper event and outbound
// transcript uses: account id = the stable alias dynasty (design line 13).
export function conversationRef(city, waId) {
  return {
    scope_id: city,
    provider: 'whatsapp',
    account_id: 'dynasty',
    conversation_id: String(waId),
    kind: 'dm',
  };
}

// gc's record carries Go field names; read either spelling defensively.
export function recordField(record, name) {
  if (!record || typeof record !== 'object') return undefined;
  if (name in record) return record[name];
  const snake = name.replace(/([a-z])([A-Z])/g, '$1_$2').toLowerCase();
  return record[snake];
}
