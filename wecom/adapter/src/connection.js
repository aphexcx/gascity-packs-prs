import { randomUUID } from 'node:crypto';

// Coordinate adapter-initiated closes with outbound work. Construct before
// connect(): an open socket is not usable until subscription is acknowledged.
export function createOutboundConnection(wsClient, {
  drainTimeoutMs = 30_000,
  authWaitMs = 45_000,
  pongTimeoutMs = 10_000,
  now = Date.now,
  log = () => {},
} = {}) {
  let active = 0;
  let uploads = 0;
  let authenticated = false;
  let lastAckAt = null;
  let stopping = false;
  let cycle = null;
  const idleWaiters = new Set();
  const authWaiters = new Set();

  wsClient.on('authenticated', () => {
    authenticated = true;
    for (const wake of [...authWaiters]) wake();
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
      for (const finish of [...idleWaiters]) finish();
    }
  }

  function trackUpload(operation) {
    return trackActivity(async () => {
      uploads++;
      try { return await operation(); } finally { uploads--; }
    });
  }

  // Run the close synchronously as the last operation settles, avoiding a
  // gap between observing idle and closing in which a new send could start.
  // Remediation must also protect uploads admitted AFTER the wait began.
  function whenIdle(action, timeoutMs, deferUploads = false) {
    if (active === 0) return Promise.resolve(action());
    log(`outbound: waiting up to ${timeoutMs} ms for in-flight publishes/uploads before closing the WebSocket`);
    return new Promise((resolve, reject) => {
      let expired = false;
      const finish = () => {
        if (active > 0 && (!expired || (deferUploads && uploads > 0))) return;
        clearTimeout(timer);
        idleWaiters.delete(finish);
        try { resolve(action()); } catch (err) { reject(err); }
      };
      const timer = setTimeout(() => { expired = true; finish(); }, timeoutMs);
      idleWaiters.add(finish);
    });
  }

  function waitForAuthenticated() {
    const unavailable = () => new Error('WebSocket not connected, unable to send data');
    if (stopping) return Promise.reject(unavailable());
    if (authenticated && wsClient.isConnected) return Promise.resolve();
    return new Promise((resolve, reject) => {
      const finish = (err) => {
        clearTimeout(timer);
        authWaiters.delete(wake);
        if (err) reject(err); else resolve();
      };
      const wake = () => {
        if (stopping) finish(unavailable());
        else if (authenticated && wsClient.isConnected) finish();
      };
      const timer = setTimeout(() => finish(unavailable()), authWaitMs);
      authWaiters.add(wake);
      wake();
    });
  }

  // The pinned SDK exposes the ws transport here (also used by upload.js).
  // Match our nonce: unrelated pongs must not satisfy this bounded probe.
  function healthCheck() {
    const socket = wsClient.wsManager?.ws;
    // The SDK already owns reconnection while disconnected/authenticating.
    if (stopping || !authenticated || !wsClient.isConnected) return Promise.resolve(true);
    if (!socket?.ping) return Promise.resolve(true);
    return new Promise((resolve) => {
      const nonce = randomUUID();
      const finish = (healthy) => {
        clearTimeout(timer);
        socket.off('pong', pong);
        socket.off('close', failed);
        socket.off('error', failed);
        // Never cycle a replacement socket because an older probe failed.
        resolve(healthy || stopping || wsClient.wsManager?.ws !== socket || !authenticated);
      };
      const pong = (data) => { if (data.toString() === nonce) finish(true); };
      const failed = () => finish(false);
      const timer = setTimeout(failed, pongTimeoutMs);
      socket.on('pong', pong);
      socket.once('close', failed);
      socket.once('error', failed);
      try { socket.ping(nonce, (err) => { if (err) failed(); }); } catch { failed(); }
    });
  }

  function sendMessage(chatid, body) {
    return waitForAuthenticated().then(() => trackActivity(async () => {
      try {
        // Keep the SDK's wire format and request-id generation unchanged.
        // The publisher decides whether a keyed retry can resume.
        const ack = await wsClient.sendMessage(chatid, body);
        noteAck();
        return ack;
      } catch (err) {
        if (typeof err?.errcode === 'number') noteAck(); // refusals also prove the socket is alive
        throw err;
      }
    }));
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
      fallback.unref?.();
      authenticated = false;
      lastAckAt = null;
      wsClient.disconnect();
    });
  }

  function reconnect() {
    if (stopping) return Promise.resolve();
    if (!cycle) cycle = whenIdle(cycleSocket, drainTimeoutMs, true).finally(() => { cycle = null; });
    return cycle;
  }

  function close() {
    stopping = true; // also cancels a reconnect already waiting for idle
    for (const wake of [...authWaiters]) wake();
    return whenIdle(() => new Promise((resolve, reject) => {
      authenticated = false;
      lastAckAt = null;
      // terminate() can leave the receiver finishing a compressed frame.
      // Only disconnected proves intake has stopped; a missing event (e.g.
      // no socket at startup) must still fit gc's shutdown deadline.
      const finish = () => {
        clearTimeout(fallback);
        wsClient.off('disconnected', finish);
        resolve();
      };
      const fallback = setTimeout(finish, 100);
      wsClient.once('disconnected', finish);
      try {
        wsClient.disconnect();
      } catch (err) {
        clearTimeout(fallback);
        wsClient.off('disconnected', finish);
        reject(err);
      }
    }), Math.min(drainTimeoutMs, 1500));
  }

  return {
    sendMessage, trackActivity, trackUpload, noteAck, reconnect, close, healthCheck,
    lastAckAt: () => lastAckAt,
    reconnectDeferral: () => uploads > 0 ? `${uploads} media upload(s) in flight` : null,
  };
}
