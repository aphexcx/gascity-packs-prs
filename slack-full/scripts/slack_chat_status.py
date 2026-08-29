#!/usr/bin/env python3
"""Read-only diagnostics for the slack pack: adapters, bindings, recent traffic.

Replaces the curl-jq one-liners that pile up while debugging:

    GET /extmsg/adapters
    GET /extmsg/bindings?session_id=...
    GET /events?type=extmsg.inbound
    GET /events?type=extmsg.outbound

Default output is a human-readable summary. ``--json`` prints the same
data as a single object for scripting (declared to gc via
``commands/status/schemas/``). ``--session`` narrows the binding +
recent-activity views to one session.

``--timeout`` (default ``$GC_SLACK_STATUS_TIMEOUT`` or 60) is a
per-request socket *inactivity* timeout: an attempt fails once the
daemon has been silent that long (a response that keeps trickling bytes
is not cut off). A failed attempt is retried once; a second timeout
exits 2 with a ``daemon busy (timeout after Ns)`` line on stderr — and
a ``daemon_busy`` failure object on stdout under ``--json`` — so
monitors can tell "daemon loaded" from "slack broken".
"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
from typing import Any

import slack_intake_common as common

# A status probe must stay meaningful while the daemon is grinding through
# session spawns: 30s was observed timing out on citadel under 5 concurrent
# spawns while real traffic still flowed (gp-aut). Longer default + one
# retry per fetch; a second stall aborts with a distinct "daemon busy"
# verdict instead of a traceback or a false "no traffic" rendering.
DEFAULT_TIMEOUT = 60.0
EXIT_DAEMON_BUSY = 2


def _usable_timeout(value: float) -> bool:
    # nan/inf pass a bare "> 0" check and then blow up inside socket setup.
    return math.isfinite(value) and value > 0


def _default_timeout() -> float:
    raw = os.environ.get("GC_SLACK_STATUS_TIMEOUT", "").strip()
    if raw:
        try:
            value = float(raw)
        except ValueError:
            value = 0.0
        if _usable_timeout(value):
            return value
    return DEFAULT_TIMEOUT


def _get_with_retry(url: str, timeout: float) -> dict[str, Any]:
    """GET with ONE retry when the daemon times out mid-answer.

    One stall under load shouldn't fail the probe; a second stall
    propagates GCAPITimeout so main() can report daemon-busy.
    """
    try:
        return common._request("GET", url, csrf=False, timeout=timeout)
    except common.GCAPITimeout:
        return common._request("GET", url, csrf=False, timeout=timeout)


def _city_url(suffix: str) -> str:
    return f"{common.gc_api_base()}/v0/city/{common.gc_city_name()}{suffix}"


def _events(event_type: str, limit: int, since: str,
            timeout: float) -> list[dict[str, Any]]:
    """Fetch a slice of events. Returns [] on transport failure or empty.

    GCAPITimeout is NOT degraded to []: an empty answer from a busy
    daemon reads as "no traffic", which is exactly the false alarm this
    probe must never produce.
    """
    qs = [f"type={event_type}", f"limit={limit}"]
    if since:
        qs.append(f"since={since}")
    url = _city_url("/events?" + "&".join(qs))
    try:
        res = _get_with_retry(url, timeout)
    except common.GCAPITimeout:
        raise
    except common.GCAPIError:
        return []
    return list(res.get("items") or [])


def _adapters(timeout: float) -> list[dict[str, Any]]:
    try:
        res = _get_with_retry(_city_url("/extmsg/adapters"), timeout)
    except common.GCAPITimeout:
        raise
    except common.GCAPIError:
        return []
    return list(res.get("items") or [])


def _bindings_for_session(session_id: str,
                          timeout: float) -> list[dict[str, Any]]:
    try:
        res = _get_with_retry(
            _city_url(f"/extmsg/bindings?session_id={session_id}"), timeout)
    except common.GCAPITimeout:
        raise
    except common.GCAPIError:
        return []
    return list(res.get("items") or [])


def collect_status(*, session: str, since: str, limit: int,
                   timeout: float = DEFAULT_TIMEOUT) -> dict[str, Any]:
    """Gather the read-only state used by both human and JSON renderers."""
    adapters = _adapters(timeout)
    inbound = _events("extmsg.inbound", limit, since, timeout)
    outbound = _events("extmsg.outbound", limit, since, timeout)

    if session:
        inbound = [
            e for e in inbound
            if (e.get("payload") or {}).get("target_session") == session
        ]
        outbound = [
            e for e in outbound
            if (e.get("payload") or {}).get("session") == session
        ]
        bindings = _bindings_for_session(session, timeout)
    else:
        bindings = []

    return {
        "adapters": adapters,
        "session": session,
        "bindings": bindings,
        "events": {
            "since": since or None,
            "limit": limit,
            "inbound": inbound,
            "outbound": outbound,
        },
    }


def _fmt_event(direction: str, evt: dict[str, Any]) -> str:
    payload = evt.get("payload") or {}
    ts = (evt.get("ts") or evt.get("emitted_at") or evt.get("created_at") or "")[11:19]
    conv = payload.get("conversation_id") or "?"
    if direction == "in":
        target = payload.get("target_session") or payload.get("actor") or "?"
        return f"in   {ts:>8}  {conv}  → {target}"
    target = payload.get("session") or evt.get("subject") or "?"
    return f"out  {ts:>8}  {conv}  ← {target}"


def format_status(status: dict[str, Any]) -> str:
    lines: list[str] = []

    adapters = status["adapters"]
    if adapters:
        lines.append("Adapters:")
        for a in adapters:
            provider = a.get("provider") or "?"
            account = a.get("account_id") or "?"
            name = a.get("name") or ""
            tail = f" (name={name})" if name else ""
            lines.append(f"  {provider}/{account}{tail}")
    else:
        lines.append("Adapters:  (none registered — slack inbound + outbound publishing won't work)")

    events = status["events"]
    inbound = events["inbound"]
    outbound = events["outbound"]
    window = events["since"] or f"last {events['limit']}"
    lines.append("")
    lines.append(f"Events ({window}):")
    lines.append(f"  inbound:  {len(inbound)}")
    lines.append(f"  outbound: {len(outbound)}")

    if status["session"]:
        lines.append("")
        lines.append(f"Session {status['session']}:")
        bindings = status["bindings"]
        if not bindings:
            lines.append("  bindings: (none)")
        else:
            lines.append("  bindings:")
            for b in bindings:
                conv = b.get("Conversation") or {}
                cid = conv.get("conversation_id") or "?"
                kind = conv.get("kind") or "?"
                bstatus = b.get("Status") or "?"
                lines.append(f"    {cid}  kind={kind}  status={bstatus}")

    recent = []
    for evt in inbound[-5:]:
        recent.append(("in", evt))
    for evt in outbound[-5:]:
        recent.append(("out", evt))
    recent.sort(key=lambda pair: pair[1].get("ts")
                or pair[1].get("emitted_at")
                or pair[1].get("created_at") or "")
    if recent:
        lines.append("")
        lines.append("Recent activity:")
        for direction, evt in recent[-10:]:
            lines.append("  " + _fmt_event(direction, evt))

    return "\n".join(lines)


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Show slack pack status: adapters, bindings, recent traffic",
    )
    parser.add_argument("--session", default="",
                        help="Restrict bindings + activity to a single session id")
    parser.add_argument("--since", default="",
                        help="Event window (e.g. 5m, 1h). Default: most recent --limit events.")
    parser.add_argument("--limit", type=int, default=50,
                        help="Max events to scan per direction. Default: 50")
    parser.add_argument("--json", dest="as_json", action="store_true",
                        help="Emit machine-readable JSON")
    parser.add_argument("--timeout", type=float, default=None,
                        help="Seconds to wait per daemon request (retried once "
                             "on timeout). Default: $GC_SLACK_STATUS_TIMEOUT "
                             f"or {DEFAULT_TIMEOUT:g}")
    args = parser.parse_args(argv)

    if args.limit < 1:
        raise SystemExit("--limit must be a positive integer")
    timeout = args.timeout if args.timeout is not None else _default_timeout()
    if not _usable_timeout(timeout):
        raise SystemExit("--timeout must be a positive, finite number of seconds")

    try:
        status = collect_status(
            session=args.session.strip(),
            since=args.since.strip(),
            limit=args.limit,
            timeout=timeout,
        )
    except common.GCAPITimeout as exc:
        message = f"daemon busy (timeout after {exc.timeout:g}s)"
        print(f"gc slack status: {message}; the gc daemon is serving other "
              "work — retry shortly or raise --timeout / GC_SLACK_STATUS_TIMEOUT",
              file=sys.stderr)
        if args.as_json:
            print(json.dumps({
                "schema_version": "1",
                "ok": False,
                "error": {
                    "code": "daemon_busy",
                    "message": message,
                    "exit_code": EXIT_DAEMON_BUSY,
                },
            }, indent=2, sort_keys=True))
        return EXIT_DAEMON_BUSY
    except common.GCAPIError as exc:
        raise SystemExit(str(exc)) from exc

    if args.as_json:
        print(json.dumps({"schema_version": "1", "ok": True, **status},
                         indent=2, sort_keys=True))
    else:
        print(format_status(status))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
