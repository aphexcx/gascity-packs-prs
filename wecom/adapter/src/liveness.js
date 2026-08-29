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
//     A dead-but-ready connection is fixed by a cycle; a genuinely
//     quiet chat gets a cheap re-auth. Off by default — cycling is a
//     behavior change the operator must opt into.
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
//   alarmRepeatMs / reconnectMinIntervalMs / now / log — test knobs.
export function createInboundLiveness(opts = {}) {
  const {
    stallAfterMs = 0,
    reconnect = null,
    alarmRepeatMs = livenessAlarmRepeatMs,
    reconnectMinIntervalMs = Math.max(stallAfterMs, livenessReconnectMinIntervalMs),
    now = Date.now,
    log = () => {},
  } = opts;

  const startedAt = now();
  let lastInboundAt = null;
  let stalledSince = null;
  let lastAlarmAt = null;
  let lastReconnectAt = null;
  let framesTotal = 0;
  let alarmsTotal = 0;
  let reconnectsTotal = 0;
  let timer = null;

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
        ? 'the reconnect remediation is enabled and will cycle the connection'
        : 'set WECOM_LIVENESS_RECONNECT=true to force a reconnect cycle on stall';
      log(`INBOUND LIVENESS ALARM: no inbound WeCom frame for ${Math.round(idle / 1000)}s `
        + `(threshold ${Math.round(stallAfterMs / 1000)}s) — either the chat is quiet or the long connection `
        + `is ready-but-dead (the 8/19 signature: socket up, pongs flowing, zero pushes). WeCom has no `
        + `history API, so anything sent during a real stall is NOT recoverable; ${remediation}`);
    }
    if (reconnect && (lastReconnectAt === null || t - lastReconnectAt >= reconnectMinIntervalMs)) {
      lastReconnectAt = t;
      reconnectsTotal++;
      log('inbound liveness: forcing a WS reconnect cycle (stall remediation)');
      try {
        reconnect();
      } catch (err) {
        log(`inbound liveness: reconnect hook failed: ${err.message}`);
      }
    }
  }

  function start(tickMs = livenessTickMs) {
    if (timer || stallAfterMs <= 0) return;
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
