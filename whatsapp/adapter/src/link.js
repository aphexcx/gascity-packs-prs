// A wake is a hint to use the ordinary inbox. All schedules use the adapter's clock.
const RECONNECT_MS = [1000, 5000, 15000, 60000, 300000];
const PING_MS = 30000;
const PONG_MS = 10000;

export function createLink({ gatewayURL, credential, clock, enabled = true,
  onRead = () => {}, onDown = () => {}, WebSocketImpl = globalThis.WebSocket } = {}) {
  let socket;
  let stopped = false;
  let failures = 0;
  let attempts = 0;
  let nextAttemptAt = 0;
  let pingAt = 0;
  let pongBy = null;
  let connectBy = 0;
  let state = 'down';
  let reason = '';
  let since = clock();
  let lastWakeAt = null;
  let wakes = 0;
  let reconnects = 0;

  if (!enabled) { state = 'disabled'; reason = 'disabled'; }
  else if (typeof WebSocketImpl !== 'function') { state = 'unavailable'; reason = 'no_websocket'; }

  function transition(next, why) {
    if (state !== next || reason !== why) since = clock();
    state = next;
    reason = why;
  }

  function disconnect(why) {
    const wasUp = state === 'up';
    const old = socket;
    socket = undefined;
    pongBy = null;
    transition('down', why);
    nextAttemptAt = clock() + RECONNECT_MS[Math.min(failures++, RECONNECT_MS.length - 1)];
    try { old?.close(); } catch { /* A broken socket cannot change inbox delivery. */ }
    if (wasUp) onDown();
  }

  function connect() {
    if (attempts++) reconnects += 1;
    transition('connecting', '');
    connectBy = clock() + PONG_MS;
    try {
      const url = new URL('/v1/wake', gatewayURL);
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
      url.username = '';
      url.password = '';
      const current = new WebSocketImpl(url, { headers: { Authorization: `Bearer ${credential}` } });
      socket = current;
      current.addEventListener('open', () => {
        if (socket !== current || stopped) return;
        failures = 0;
        pingAt = clock() + PING_MS;
        transition('up', '');
        onRead();
      });
      current.addEventListener('message', ({ data }) => {
        if (socket !== current || stopped || state !== 'up') return;
        if (data === 'wake') {
          lastWakeAt = clock();
          wakes += 1;
          onRead();
        } else if (data === 'pong') {
          if (pongBy !== null && clock() >= pongBy) { disconnect('pong_timeout'); return; }
          pongBy = null;
        } else disconnect('invalid_frame');
      });
      current.addEventListener('error', () => { if (socket === current && !stopped) disconnect('connect_error'); });
      current.addEventListener('close', () => { if (socket === current && !stopped) disconnect('closed'); });
    } catch { disconnect('connect_error'); }
  }

  return {
    get up() { return state === 'up'; },
    tick() {
      if (stopped || state === 'disabled' || state === 'unavailable') return;
      const now = clock();
      if (state === 'connecting' && now >= connectBy) { disconnect('connect_timeout'); return; }
      if (state === 'up') {
        if (pongBy !== null && now >= pongBy) { disconnect('pong_timeout'); return; }
        if (now >= pingAt && pongBy === null) {
          pongBy = now + PONG_MS;
          pingAt = now + PING_MS;
          try { socket.send('ping'); } catch { disconnect('send_error'); }
        }
      } else if (state === 'down' && now >= nextAttemptAt) connect();
    },
    stop(why = 'stopped') {
      if (stopped) return;
      stopped = true;
      const old = socket;
      socket = undefined;
      transition('stopped', why);
      try { old?.close(); } catch { /* Shutdown is final. */ }
    },
    status() {
      return { mode: state === 'up' ? 'held' : 'poll', state, since,
        last_wake_at: lastWakeAt, wakes, reconnects, reason };
    },
  };
}
