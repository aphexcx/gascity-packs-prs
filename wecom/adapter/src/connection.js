// Coordinate adapter-initiated closes with outbound work. Construct before
// connect(): an open socket is not usable until subscription is acknowledged.
import crypto from 'node:crypto';

export function createOutboundConnection(wsClient, {
  drainTimeoutMs = 30_000,
  reconnectWaitMs = 30_000,
  now = Date.now,
  log = () => {},
} = {}) {
  let active = 0;
  let authenticated = false;
  let epoch = 0;
  let lastAckAt = null;
  let stopping = false;
  let closed = false;
  let cycle = null;
  const idleWaiters = new Set();
  const connectionWaiters = new Set();

  wsClient.on('authenticated', () => {
    authenticated = true;
    epoch++;
    for (const wake of [...connectionWaiters]) wake();
  });
  wsClient.on('disconnected', () => {
    authenticated = false;
    lastAckAt = null; // evidence from the old socket cannot protect a new one
  });

  const noteAck = () => {
    if (authenticated && wsClient.isConnected) lastAckAt = now();
  };
  async function trackActivity(operation) {
    active++;
    try {
      return await operation();
    } finally {
      active--;
      if (active === 0) for (const finish of [...idleWaiters]) finish();
    }
  }

  // Run the close synchronously as the last operation settles, avoiding a
  // gap between observing idle and closing in which a new send could start.
  function whenIdle(action) {
    if (active === 0) return Promise.resolve(action());
    log(`outbound: waiting up to ${drainTimeoutMs} ms for in-flight publishes/uploads before closing the WebSocket`);
    return new Promise((resolve, reject) => {
      const finish = () => {
        clearTimeout(timer);
        idleWaiters.delete(finish);
        try { resolve(action()); } catch (err) { reject(err); }
      };
      const timer = setTimeout(finish, drainTimeoutMs);
      idleWaiters.add(finish);
    });
  }

  function waitForReconnect(previousEpoch) {
    return new Promise((resolve) => {
      let timer;
      const finish = (ok) => {
        clearTimeout(timer);
        connectionWaiters.delete(wake);
        resolve(ok);
      };
      const wake = () => {
        if (closed) finish(false);
        else if (authenticated && wsClient.isConnected && epoch > previousEpoch) finish(true);
      };
      connectionWaiters.add(wake);
      timer = setTimeout(() => finish(false), reconnectWaitMs);
      wake(); // authentication may have beaten the rejected send's continuation
    });
  }

  function sendMessage(chatid, body, { idempotencyKey, frameKey = 'text:0' } = {}) {
    // Distinct chunks/stages need distinct ids; a retry must use the EXACT
    // same wire id and body. Hash caller keys to bound ids and avoid logging
    // caller content in the SDK. Keyless sends still retain one random id.
    const suffix = idempotencyKey
      ? crypto.createHash('sha256').update(JSON.stringify([chatid, idempotencyKey, frameKey])).digest('hex')
      : crypto.randomUUID();
    const frame = { headers: { req_id: `aibot_send_msg_${suffix}` } };
    const fullBody = { chatid, ...body };
    return trackActivity(async () => {
      const sentEpoch = epoch;
      try {
        const ack = await wsClient.reply(frame, fullBody, 'aibot_send_msg');
        noteAck();
        return ack;
      } catch (err) {
        if (typeof err?.errcode === 'number') noteAck(); // refusals also prove the socket is alive
        const message = String(err?.message ?? '');
        const cancelled = (message.startsWith('WebSocket connection closed (')
          || message.startsWith('Connection manually closed,'))
          && message.endsWith(`, reply for reqId: ${frame.headers.req_id} cancelled`);
        // Timeouts, provider refusals and pre-write failures retain the
        // publisher's existing classification. Only socket loss retries.
        if (!cancelled || typeof err?.errcode === 'number' || closed) throw err;
        if (!await waitForReconnect(sentEpoch)) throw err;
        log('publish: retrying interrupted frame once after WebSocket authentication');
        try {
          const ack = await wsClient.reply(frame, fullBody, 'aibot_send_msg');
          noteAck();
          return ack;
        } catch (retryError) {
          if (typeof retryError?.errcode === 'number') noteAck();
          // Even a pre-write refusal of attempt two cannot prove attempt
          // one was not delivered. Preserve its delivery-unknown shape.
          throw err;
        }
      }
    });
  }

  function cycleSocket() {
    if (stopping) return;
    return new Promise((resolve) => {
      let cycled = false;
      const connectOnce = () => {
        if (cycled) return;
        cycled = true;
        clearTimeout(fallback);
        wsClient.off('disconnected', deferredConnect);
        if (!stopping) wsClient.connect();
        resolve();
      };
      // SDK 1.0.7 nulls its socket AFTER emitting disconnected. Reconnect
      // one macrotask later, or its close handler erases the fresh socket.
      const deferredConnect = () => setTimeout(connectOnce, 0);
      wsClient.once('disconnected', deferredConnect);
      const fallback = setTimeout(connectOnce, 3000);
      authenticated = false;
      lastAckAt = null;
      wsClient.disconnect();
    });
  }

  function reconnect() {
    if (stopping) return Promise.resolve();
    if (!cycle) cycle = whenIdle(cycleSocket).finally(() => { cycle = null; });
    return cycle;
  }

  function close() {
    stopping = true; // also cancels a reconnect already waiting for idle
    return whenIdle(() => {
      closed = true;
      authenticated = false;
      lastAckAt = null;
      wsClient.disconnect();
      for (const wake of [...connectionWaiters]) wake();
    });
  }

  return { sendMessage, trackActivity, noteAck, reconnect, close, lastAckAt: () => lastAckAt };
}
