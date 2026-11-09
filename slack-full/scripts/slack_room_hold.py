"""Inspect and hold room routing through gc's public API.

The extmsg group endpoint omits participants and its bindings endpoint is
session-only. Like reply-current, read the live labeled beads instead of the
pack's config mirror. Keep that storage knowledge and pagination here.
"""

from __future__ import annotations

import datetime as dt
import fcntl
import hashlib
import json
import urllib.error
import urllib.parse
from contextlib import contextmanager
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

Get = Callable[[str], dict[str, Any]]
Post = Callable[[str, dict[str, Any]], dict[str, Any]]
UNBIND_FILTER = "conversation+agent (gc exposes no binding id; see help)"
_CONVERSATION_KEYS = ("scope_id", "provider", "account_id", "conversation_id",
                      "parent_conversation_id", "kind")


class RoomHoldError(RuntimeError):
    """Routing could not be established safely; no success may be recorded."""


class RoomHoldChanged(RoomHoldError):
    """The verified group remains, but a changed binding must not be removed."""

    def __init__(self, state: RoomRouting, unbound: list[Any]):
        super().__init__("partial (binding changed during the hold; re-run bind-room)")
        self.state = state
        self.unbound = unbound


@contextmanager
def bind_room_lock(data_dir: Path):
    """Serialize pack bind-room reads, routing writes, and config persistence.

    Both ambient and mention-only commands take this lock. Direct gc callers
    and inbound default routing do not; this is not an atomic gc unbind.
    """
    data_dir.mkdir(parents=True, exist_ok=True)
    with (data_dir / "room-hold.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        try:
            yield
        finally:
            fcntl.flock(lock, fcntl.LOCK_UN)


def _records(get: Get, label: str) -> list[dict[str, Any]]:
    query = {"label": label, "limit": "200"}
    seen: set[str] = set()
    records = []
    for _ in range(100):
        page = get("/beads?" + urllib.parse.urlencode(query))
        if (not isinstance(page, dict) or page.get("partial")
                or page.get("partial_errors") or not isinstance(page.get("items"), list)):
            raise RoomHoldError("incomplete gc routing inventory")
        for row in page["items"]:
            if not isinstance(row, dict):
                raise RoomHoldError("malformed gc routing record")
            if row.get("status") == "closed":
                continue
            if not isinstance(row.get("metadata"), dict) or not row.get("id"):
                raise RoomHoldError("gc routing record lacks id or metadata")
            if row["metadata"].get("schema_version") != "1":
                raise RoomHoldError("unsupported gc routing record schema")
            if label not in (row.get("labels") or []):
                raise RoomHoldError("gc routing inventory ignored its label filter")
            records.append(row)
        cursor = page.get("next_cursor")
        if not cursor:
            return records
        if not isinstance(cursor, str) or cursor in seen:
            raise RoomHoldError("gc routing inventory repeated a page cursor")
        seen.add(cursor)
        query["cursor"] = cursor
    raise RoomHoldError("gc routing inventory exceeded 100 pages")


def _normalize(conv: dict[str, str]) -> dict[str, str]:
    return {k: conv.get(k, "").strip().lower() if k in ("provider", "kind")
            else conv.get(k, "").strip() for k in _CONVERSATION_KEYS}


def _conversation_hash(conv: dict[str, str]) -> str:
    """Match gc labels.go hashJoin: SHA256 of Go's compact JSON string array."""
    normalized = _normalize(conv)
    wire = json.dumps([normalized[k] for k in _CONVERSATION_KEYS],
                      ensure_ascii=False, separators=(",", ":"))
    # encoding/json escapes HTML characters and JavaScript line separators,
    # even though it otherwise emits UTF-8 rather than ASCII Unicode escapes.
    for char, escaped in (("&", r"\u0026"), ("<", r"\u003c"), (">", r"\u003e"),
                          ("\u2028", r"\u2028"), ("\u2029", r"\u2029")):
        wire = wire.replace(char, escaped)
    return hashlib.sha256(wire.encode("utf-8")).hexdigest()


def _same_room(metadata: dict[str, Any], conv: dict[str, str]) -> bool:
    return all(metadata.get(stored, "") == conv.get(wire, "") for stored, wire in (
        ("scope_id", "scope_id"), ("provider", "provider"),
        ("account_id", "account_id"), ("conversation_id", "conversation_id"),
        ("parent_conversation_id", "parent_conversation_id"),
        ("conversation_kind", "kind"),
    ))


@dataclass
class RoomRouting:
    group_id: str = ""
    ambient_participants: list[str] = field(default_factory=list)
    default_agents: list[str] = field(default_factory=list)
    explicit_agents: list[str] = field(default_factory=list)
    bindings: list[dict[str, Any]] = field(default_factory=list)

    @property
    def held(self) -> bool:
        return bool(self.group_id and not self.ambient_participants and not self.default_agents)

    @property
    def description(self) -> str:
        if self.ambient_participants:
            participants = [n for n in self.ambient_participants if n not in self.explicit_agents]
            routes = (["ambient participants " + ", ".join(participants)] if participants else [])
            if self.explicit_agents:
                routes.append("explicit agent bindings " + ", ".join(self.explicit_agents))
            return "not held: " + "; ".join(routes)
        if self.default_agents:
            return "not held: default-route binding " + ", ".join(self.default_agents)
        return "held (zero-participant group)" if self.held else "not held: no group"


def inspect_room(conv: dict[str, str], get: Get) -> RoomRouting:
    conv = _normalize(conv)
    if not all(conv.get(k) for k in ("scope_id", "provider", "account_id", "conversation_id", "kind")):
        raise RoomHoldError("room conversation identity is incomplete")
    digest = _conversation_hash(conv)
    groups = [r for r in _records(get, "extmsg:group:root:v1:" + digest)
              if _same_room(r["metadata"], conv)]
    if len(groups) > 1:
        raise RoomHoldError("multiple active gc groups for the room")
    state = RoomRouting(group_id=groups[0]["id"] if groups else "")
    ambient: set[str] = set()
    if state.group_id:
        for row in _records(get, "extmsg:group:participant:v1:" + state.group_id):
            m = row["metadata"]
            if m.get("group_id") != state.group_id or not m.get("session_id"):
                raise RoomHoldError("malformed gc group participant")
            ambient.add(m.get("session_name") or m["session_id"])
    defaults: set[str] = set()
    explicit: set[str] = set()
    for row in _records(get, "extmsg:binding:conv:v1:" + digest):
        m = row["metadata"]
        if not _same_room(m, conv):
            continue
        if m.get("expires_at"):
            try:
                expiry = dt.datetime.fromisoformat(m["expires_at"].replace("Z", "+00:00"))
                if expiry <= dt.datetime.now(dt.timezone.utc):
                    continue
            except (ValueError, TypeError) as exc:
                raise RoomHoldError("invalid gc binding expiry") from exc
        state.bindings.append({"id": row["id"], "metadata": m})
        if (m.get("agent_name") and not m.get("session_id")
                and m.get("created_by_kind") == "controller"
                and m.get("created_by_id") == "extmsg-default-route"):
            defaults.add(m["agent_name"])
        else:
            name = m.get("session_name") or m.get("session_id") or m.get("agent_name")
            if not name:
                raise RoomHoldError("gc binding has no target")
            ambient.add(name)
            if m.get("agent_name") and not m.get("session_id"):
                explicit.add(name)
    state.ambient_participants = sorted(ambient)
    state.default_agents = sorted(defaults)
    state.explicit_agents = sorted(explicit)
    state.bindings.sort(key=lambda row: json.dumps(row, sort_keys=True))
    return state


def hold_room(conv: dict[str, str], get: Get, post: Post) -> tuple[RoomRouting, list[Any]]:
    state = inspect_room(conv, get)
    if state.ambient_participants:
        return state, []
    group_id = state.group_id
    if not state.group_id:
        # GET /beads may not include a relocated messaging store. Ensure is
        # an upsert that resets group policy, so verify absence through gc's
        # authoritative lookup before attempting it. Only the specific
        # group-not-found response is absence; every other failure propagates.
        try:
            get("/extmsg/groups?" + urllib.parse.urlencode(conv))
        except RuntimeError as exc:
            cause = exc.__cause__  # gc_get preserves urllib's HTTPError.
            if not (isinstance(cause, urllib.error.HTTPError) and cause.code == 404
                    and "extmsg-group-not-found" in str(exc)):
                raise
        else:
            raise RoomHoldError("gc group lookup disagrees with routing inventory; refusing to ensure")
        group = post("/extmsg/groups", {"root_conversation": conv, "mode": "launcher"})
        group_id = group.get("ID")
        if not group_id:
            raise RoomHoldError("ensure group response lacks ID")
    # Re-read after ensuring the group. Never unbind an ambient participant
    # introduced while we were registering or creating the hold.
    state = inspect_room(conv, get)
    if state.ambient_participants:
        return state, []
    if not state.group_id:
        raise RoomHoldError("ensured group is not visible; refusing to unbind")
    if state.group_id != group_id:
        raise RoomHoldError("ensured group identity changed; refusing to unbind")
    unbound = []
    for agent in list(state.default_agents):
        # The group is verified BEFORE taking the deletion snapshot. New
        # inbounds cannot default-route an already-grouped room. Compare IDs
        # and metadata (including caller) immediately before each unbind;
        # comparing agent names alone misses a same-agent replacement.
        current = inspect_room(conv, get)
        if (current.bindings != state.bindings or current.group_id != group_id
                or current.ambient_participants != state.ambient_participants):
            raise RoomHoldChanged(current, unbound)
        # gc has no binding-id / expected-caller predicate. An external
        # same-agent rebind after this read remains a documented race.
        result = post("/extmsg/unbind", {"conversation": conv, "agent_name": agent})
        if not isinstance(result.get("unbound"), list):
            raise RoomHoldError("unbind response lacks removed bindings")
        unbound.extend(result["unbound"])
        state.bindings = [r for r in state.bindings if r["metadata"].get("agent_name") != agent]
    state = inspect_room(conv, get)
    if not state.held:
        raise RoomHoldError("room routing changed during hold: " + state.description)
    return state, unbound
