// http.js — one JSON request helper with a deadline, and the error classes
// every caller branches on.
//
// The classes matter more than the transport: an HTTP ANSWER (any status)
// means the server saw the request and decided; NO ANSWER (a timeout, a
// reset, a refusal, a body that never finished) means the caller cannot know
// whether the request was acted on. The inbound post to gc treats the two
// differently (design line 114): the ladder re-posts only on an HTTP answer
// (5xx, 404, 408, 429: gc answers those before any fan-out), while no answer
// leaves the entry in posted for the in-doubt rule.

export const ladderMs = [1000, 5000, 15000, 60000];

export function ladderDelay(attempt) {
  return ladderMs[Math.min(Math.max(attempt, 0), ladderMs.length - 1)];
}

// An HTTP answer that a repeat of the same request may cure.
export function isRetryableAnswer(err) {
  return Number.isInteger(err?.status) && (err.status >= 500 || [404, 408, 429].includes(err.status));
}

// Safe to repeat: a read, or a write the server makes idempotent (the ack by
// id, the send under an idempotency key).
export function isTransient(err) {
  return Boolean(err?.noAnswer) || isRetryableAnswer(err);
}

// A failure in the connect phase means the request never left: no server
// can have acted on it. Anything later (a reset, a deadline) is no answer.
const CONNECT_CODES = new Set(['ECONNREFUSED', 'ENOENT', 'ENOTFOUND', 'EAI_AGAIN', 'EHOSTUNREACH', 'ENETUNREACH', 'UND_ERR_CONNECT_TIMEOUT']);

export async function requestJSON(url, {
  method = 'GET',
  headers = {},
  body,
  rawBody,
  timeoutMs = 15000,
  signal,
  fetchImpl = fetch,
  expect = 'json',
} = {}) {
  const controller = new AbortController();
  const cancel = () => controller.abort(signal.reason);
  if (signal?.aborted) cancel();
  else signal?.addEventListener('abort', cancel, { once: true });
  const timer = setTimeout(() => controller.abort(new Error(`deadline ${timeoutMs}ms`)), timeoutMs);
  timer.unref?.();
  try {
    let resp;
    try {
      const init = { method, headers: { ...headers }, signal: controller.signal };
      if (body !== undefined) {
        init.headers['Content-Type'] = 'application/json';
        init.body = JSON.stringify(body);
      } else if (rawBody !== undefined) {
        init.body = rawBody;
      }
      resp = await fetchImpl(url, init);
    } catch (err) {
      const code = err?.cause?.code ?? err?.code;
      const notSent = CONNECT_CODES.has(code);
      throw Object.assign(new Error(`no answer from ${redactURL(url)}: ${code ?? err?.message ?? err}`), { noAnswer: true, notSent, cause: err });
    }
    let payload;
    try {
      payload = expect === 'bytes' ? Buffer.from(await resp.arrayBuffer()) : await resp.text();
    } catch (err) {
      // Headers arrived but the body never finished: the server may have
      // acted. Never read that as a refusal.
      throw Object.assign(new Error(`answer from ${redactURL(url)} cut off (${resp.status})`), { noAnswer: true, partialStatus: resp.status, cause: err });
    }
    if (!resp.ok) {
      const text = expect === 'bytes' ? payload.toString('utf8') : payload;
      let json;
      try { json = JSON.parse(text); } catch { json = undefined; }
      // The body stays on err.body / err.json, never in the message: a
      // message is logged, and a gc error body can echo the inbound text.
      throw Object.assign(new Error(`HTTP ${resp.status} from ${redactURL(url)}`), {
        status: resp.status,
        body: text,
        json,
      });
    }
    if (expect === 'bytes') return { bytes: payload, contentType: resp.headers.get('content-type') || '' };
    if (!payload) return {};
    try {
      return JSON.parse(payload);
    } catch {
      throw Object.assign(new Error(`non-JSON answer from ${redactURL(url)} (${resp.status})`), { status: resp.status, malformed: true });
    }
  } finally {
    clearTimeout(timer);
    signal?.removeEventListener('abort', cancel);
  }
}

// Never log a query string: it can carry a wa_id.
export function redactURL(url) {
  try {
    const u = new URL(url);
    return `${u.origin}${u.pathname}`;
  } catch {
    return '<url>';
  }
}
