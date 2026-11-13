// gateway.js — the box side of the gateway contract v1 (the gateway spec's
// SCOPE item 3, frozen; bead A implements the other side at
// whatsapp/gateway/). Every call but enroll and healthz carries the per-city
// bearer minted at enrollment; the box never holds a Meta secret.
//
// Deadline 15 seconds per call. The caller schedules retries on the ladder
// (1, 5, 15, 60 seconds) for 5xx, 404, 408, 429 and no answer; a 401 is
// flagged err.auth and is never retried in a loop (the doctor row reads red
// "not enrolled: run gc whatsapp enroll").

import { requestJSON } from './http.js';

export function createGatewayClient({ url, credential, timeoutMs = 15000, fetchImpl = fetch } = {}) {
  const base = String(url || '').replace(/\/+$/, '');
  const bearer = () => (typeof credential === 'function' ? credential() : credential) || '';

  async function call(method, path, { body, rawBody, auth = true, expect, uploadHeaders, captureResponse, activityAttempt } = {}) {
    if (!base) throw Object.assign(new Error('WHATSAPP_GATEWAY_URL is not set'), { config: true });
    const headers = { ...uploadHeaders };
    if (activityAttempt) headers['X-GC-Send-Attempt'] = activityAttempt;
    if (auth) {
      const token = bearer();
      if (!token) throw Object.assign(new Error('not enrolled: run gc whatsapp enroll'), { status: 401, auth: true });
      headers.Authorization = `Bearer ${token}`;
    }
    try {
      return await requestJSON(`${base}${path}`, { method, headers, body, rawBody, timeoutMs, expect,
        fetchImpl: captureResponse ? async (...args) => {
          const response = await fetchImpl(...args);
          captureResponse(response);
          return response;
        } : fetchImpl,
      });
    } catch (err) {
      if (err.status === 401) err.auth = true;
      throw err;
    }
  }

  return {
    base,
    enroll: ({ code, name }) => call('POST', '/v1/enroll', { body: { code, name }, auth: false }),
    createInvite: () => call('POST', '/v1/invites', { body: {} }),
    inbox: ({ after, limit = 200 } = {}) => {
      const q = new URLSearchParams();
      if (after !== undefined && after !== null) q.set('after', String(after));
      q.set('limit', String(limit));
      return call('GET', `/v1/inbox?${q}`);
    },
    ack: (ids) => call('POST', '/v1/inbox/ack', { body: { ids } }),
    send: async ({ activity_attempt_id, ...body }) => {
      let replayed, acceptedAttempt;
      const answer = await call('POST', '/v1/send', { body, activityAttempt: activity_attempt_id, captureResponse: (response) => {
        const flag = response.headers.get('x-gc-replayed');
        if (flag === 'true' || flag === 'false') replayed = flag === 'true';
        acceptedAttempt = response.headers.get('x-gc-send-attempt');
      } });
      return { ...answer, ...(replayed === undefined ? {} : { replayed }), ...(acceptedAttempt ? { activity_attempt_id: acceptedAttempt } : {}) };
    },
    upload: (file, key) => call('POST', '/v1/uploads', { rawBody: file.bytes, uploadHeaders: {
      'Content-Type': file.mime, 'Content-Length': String(file.size),
      'X-Filename': encodeURIComponent(file.name), 'X-Content-SHA256': file.sha256, 'Idempotency-Key': key,
    } }),
    read: (messageId) => call('POST', '/v1/read', { body: { message_id: messageId } }),
    window: () => call('GET', '/v1/window'),
    media: (mediaId) => call('GET', `/v1/media/${encodeURIComponent(mediaId)}`, { expect: 'bytes' }),
    healthz: () => call('GET', '/healthz', { auth: false }),
  };
}

// The gateway's paging contract: after omitted returns the oldest
// unacknowledged; cursor is the last id of the page; a short page ends it.
export async function readWholeInbox(gateway, { limit = 200, maxPages = 50 } = {}) {
  const all = [];
  let after;
  for (let page = 0; page < maxPages; page++) {
    const answer = await gateway.inbox({ after, limit });
    const deliveries = Array.isArray(answer?.deliveries) ? answer.deliveries : [];
    all.push(...deliveries);
    if (deliveries.length < limit) break;
    const cursor = answer.cursor ?? deliveries[deliveries.length - 1]?.id;
    if (cursor === undefined || cursor === after) break;
    after = cursor;
  }
  return all;
}
