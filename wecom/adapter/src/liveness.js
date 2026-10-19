// liveness.js — inbound-liveness watchdog for the WeCom long connection
// (jg-p1mk item 1; mirrors slack-full's inbound_liveness.go design).
//
// WHY: on 2026-08-19 the WeCom WS sat ready-but-dead for ~40 minutes —
// the socket was up, heartbeat pongs flowed, /healthz answered ok, and
// outbound sends worked, but WeCom pushed no inbound frames. Nothing in
// the adapter failed, so nothing alarmed; messages sent to the bot in
// that window were simply never seen. The SDK's own missed-pong
// detection (WsConnectionManager.sendHeartbeat) only catches
// TRANSPORT-level death; an application-level push stall with a healthy
// socket is invisible to it — the same structural gap slack-full hit
// with the Events API on the same day.
//
// What this module does:
//
//   - Tracks a last-inbound watermark, stamped on every message frame
//     and every event callback the SDK delivers.
//   - A periodic tick compares idle time against a configurable stall
//     threshold (WECOM_LIVENESS_STALL_AFTER_MS; 0 disables). Past it,
//     the tracker flips to "stalled": one loud ALARM log line
//     immediately, repeated on a slow cadence while the stall persists,
//     and /healthz (see listener.js healthDetail) reports
//     inbound_liveness=stalled so monitoring can see the degraded state.
//   - Recovery is logged (with the stall duration) the moment any
//     inbound frame arrives.
//   - Optional remediation: WECOM_LIVENESS_RECONNECT=true wires a
//     reconnect hook that force-cycles the WS on stall (rate-limited).
//     Recent outbound acknowledgements suppress a new cycle: quiet
//     inbound alone is insufficient reason to tear down a live socket
//     until silence reaches 3× the stall threshold (acks cannot mask a
//     half-dead socket indefinitely).
//     The inbound alarm remains, since acks do not prove push delivery.
//
// What deliberately does NOT port from slack-full: the history probe
// and backfill. WeCom's aibot surface is push-only — there is no
// bot-readable message-history API to distinguish "quiet chat" from
// "dead push", and nothing to replay missed messages from (download
// URLs die in ~5 minutes; texts are never re-fetchable). A stall here
// is therefore a SUSPICION, not proof — the alarm text says so — and
// missed traffic is unrecoverable, which is exactly why surfacing the
// suspicion loudly (and optionally cycling the connection) matters.

// livenessTickMs is the watchdog's clock resolution.
export const livenessTickMs = 30 * 1000;

// livenessAlarmRepeatMs re-logs the alarm while a stall persists so a
// long outage is not a single easily-missed line.
export const livenessAlarmRepeatMs = 30 * 60 * 1000;

// livenessReconnectMinIntervalMs floors how often the optional
// remediation hook may cycle the connection: WeCom's tolerance for
// reconnect churn is undocumented, and a quiet night must not turn
// into a reconnect storm.
export const livenessReconnectMinIntervalMs = 30 * 60 * 1000;

// createInboundLiveness builds the tracker.
//
// opts:
//   stallAfterMs   idle threshold before the state flips to stalled;
//                  0 (default) disables the watchdog entirely.
//   reconnect      optional () => void; called (rate-limited) while
//                  stalled to force-cycle the WS. null = detection only.
//   lastOutboundAck optional () => timestamp|null for this socket's last ack
//   outboundAckFreshMs how long that ack suppresses a new reconnect;
//                  0 disables suppression, as does idle >= 3× stallAfterMs
//   reconnectDeferral optional () => string|null; a non-empty reason
//                  postpones a due cycle to a later tick (jg-bw5gsw: a
//                  cycle tears down any media upload in flight, and a
//                  quiet chat is exactly when the mayor sends files).
//                  Logged once per deferral run; does not consume the
//                  rate-limit window.
//   alarmRepeatMs / reconnectMinIntervalMs / now / log — test knobs.
export function createInboundLiveness(opts = {}) {
  const {
    stallAfterMs = 0,
    reconnect = null,
    reconnectDeferral = null,
    lastOutboundAck = () => null,
    outboundAckFreshMs = stallAfterMs,
    alarmRepeatMs = livenessAlarmRepeatMs,
    reconnectMinIntervalMs = Math.max(stallAfterMs, livenessReconnectMinIntervalMs),
    now = Date.now,
    log = () => {},
  } = opts;

  let startedAt = now();
  let lastInboundAt = null;
  let stalledSince = null;
  let lastAlarmAt = null;
  let lastReconnectAt = null;
  let framesTotal = 0;
  let alarmsTotal = 0;
  let reconnectsTotal = 0;
  let reconnectsDeferredTotal = 0;
  let deferralLogged = false;
  let timer = null;
  let reconnectPending = false;

  function noteInbound() {
    framesTotal++;
    const t = now();
    lastInboundAt = t;
    if (stalledSince !== null) {
      const stallSeconds = Math.round((t - stalledSince) / 1000);
      stalledSince = null;
      lastAlarmAt = null;
      log(`inbound liveness: RECOVERED — inbound frames resumed after a ${stallSeconds}s stall`);
    }
  }

  // tick runs one evaluation; production arms it on livenessTickMs via
  // start(), tests call it directly with a fake clock.
  function tick() {
    if (stallAfterMs <= 0) return;
    const t = now();
    // Silence before this process started is not this process's stall.
    const ref = Math.max(lastInboundAt ?? 0, startedAt);
    const idle = t - ref;
    if (idle < stallAfterMs) return;

    if (stalledSince === null) stalledSince = t - idle + stallAfterMs;
    if (lastAlarmAt === null || t - lastAlarmAt >= alarmRepeatMs) {
      lastAlarmAt = t;
      alarmsTotal++;
      const remediation = reconnect
        ? 'reconnect remediation is enabled; recent outbound acknowledgements suppress a new cycle only below 3× the stall threshold'
        : 'set WECOM_LIVENESS_RECONNECT=true to force a reconnect cycle on stall';
      log(`INBOUND LIVENESS ALARM: no inbound WeCom frame for ${Math.round(idle / 1000)}s `
        + `(threshold ${Math.round(stallAfterMs / 1000)}s) — either the chat is quiet or the long connection `
        + `is ready-but-dead (the 8/19 signature: socket up, pongs flowing, zero pushes). WeCom has no `
        + `history API, so anything sent during a real stall is NOT recoverable; ${remediation}`);
    }
    const ackAt = lastOutboundAck();
    if (idle < 3 * stallAfterMs && outboundAckFreshMs > 0
      && ackAt !== null && t - ackAt < outboundAckFreshMs) return;
    if (reconnectPending) return;
    if (reconnect && (lastReconnectAt === null || t - lastReconnectAt >= reconnectMinIntervalMs)) {
      let deferral = null;
      try {
        deferral = reconnectDeferral?.() || null;
      } catch (err) {
        log(`inbound liveness: reconnect deferral check failed (cycling anyway): ${err.message}`);
      }
      if (deferral) {
        if (!deferralLogged) {
          deferralLogged = true;
          reconnectsDeferredTotal++;
          log(`inbound liveness: reconnect cycle deferred — ${deferral}`);
        }
        return;
      }
      deferralLogged = false;
      lastReconnectAt = t;
      reconnectsTotal++;
      log('inbound liveness: forcing a WS reconnect cycle (stall remediation)');
      try {
        const result = reconnect();
        if (result?.then) {
          reconnectPending = true;
          Promise.resolve(result).catch((err) => {
            log(`inbound liveness: reconnect hook failed: ${err.message}`);
          }).finally(() => { reconnectPending = false; });
        }
      } catch (err) {
        log(`inbound liveness: reconnect hook failed: ${err.message}`);
      }
    }
  }

  function start(tickMs = livenessTickMs) {
    if (timer || stallAfterMs <= 0) return;
    // Production starts on the first authentication, so slow startup
    // cannot consume the inbound grace period. Reconnects do not reset it.
    startedAt = now();
    timer = setInterval(tick, tickMs);
    timer.unref?.();
  }

  function stop() {
    if (timer) clearInterval(timer);
    timer = null;
  }

  // state powers tests and healthzDetail.
  function state() {
    return {
      state: stallAfterMs <= 0 ? 'watchdog_off' : (stalledSince !== null ? 'stalled' : 'ok'),
      lastInboundAt,
      stalledSince,
      framesTotal,
      alarmsTotal,
      reconnectsTotal,
      reconnectsDeferredTotal,
    };
  }

  // healthzDetail renders the liveness line appended to /healthz (the
  // first body line stays exactly "ok" — supervisor contract).
  function healthzDetail() {
    const s = state();
    let line = `inbound_liveness=${s.state} frames=${s.framesTotal} alarms=${s.alarmsTotal}`;
    if (s.lastInboundAt !== null) line += ` last_inbound=${new Date(s.lastInboundAt).toISOString()}`;
    if (s.stalledSince !== null) line += ` stalled_since=${new Date(s.stalledSince).toISOString()}`;
    return line;
  }

  return { noteInbound, tick, start, stop, state, healthzDetail };
}
