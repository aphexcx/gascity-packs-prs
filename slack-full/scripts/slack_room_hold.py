"""Inspect and hold room routing through gc's public API.

The extmsg group endpoint omits participants and its bindings endpoint is
session-only. Like reply-current, read the live labeled beads instead of the
pack's config mirror. Keep that storage knowledge and pagination here.
"""

from __future__ import annotations

import datetime as dt
import urllib.parse
from dataclasses import dataclass, field
from typing import Any, Callable

Get = Callable[[str], dict[str, Any]]
Post = Callable[[str, dict[str, Any]], dict[str, Any]]


class RoomHoldError(RuntimeError):
    """Routing could not be established safely; no success may be recorded."""


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

    @property
    def held(self) -> bool:
        return bool(self.group_id and not self.ambient_participants and not self.default_agents)

    @property
    def description(self) -> str:
        if self.ambient_participants:
            return "not held: ambient participants " + ", ".join(self.ambient_participants)
        if self.default_agents:
            return "not held: default-route binding " + ", ".join(self.default_agents)
        return "held (zero-participant group)" if self.held else "not held: no group"


def inspect_room(conv: dict[str, str], get: Get) -> RoomRouting:
    if not all(conv.get(k) for k in ("scope_id", "provider", "account_id", "conversation_id", "kind")):
        raise RoomHoldError("room conversation identity is incomplete")
    groups = [r for r in _records(get, "gc:extmsg-group") if _same_room(r["metadata"], conv)]
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
    for row in _records(get, "gc:extmsg-binding"):
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
        if (m.get("agent_name") and not m.get("session_id")
                and m.get("created_by_kind") == "controller"
                and m.get("created_by_id") == "extmsg-default-route"):
            defaults.add(m["agent_name"])
        else:
            name = m.get("session_name") or m.get("session_id") or m.get("agent_name")
            if not name:
                raise RoomHoldError("gc binding has no target")
            ambient.add(name)
    state.ambient_participants = sorted(ambient)
    state.default_agents = sorted(defaults)
    return state


def hold_room(conv: dict[str, str], get: Get, post: Post) -> tuple[RoomRouting, list[Any]]:
    state = inspect_room(conv, get)
    if state.ambient_participants:
        return state, []
    if not state.group_id:
        group = post("/extmsg/groups", {"root_conversation": conv, "mode": "launcher"})
        if not group.get("ID"):
            raise RoomHoldError("ensure group response lacks ID")
    # Re-read after ensuring the group. Never unbind an ambient participant
    # introduced while we were registering or creating the hold.
    state = inspect_room(conv, get)
    if state.ambient_participants:
        return state, []
    if not state.group_id:
        raise RoomHoldError("ensured group is not visible; refusing to unbind")
    unbound = []
    for agent in state.default_agents:
        # Filter by agent as well as conversation: a concurrent session binding
        # must never be swept away by a conversation-only unbind.
        result = post("/extmsg/unbind", {"conversation": conv, "agent_name": agent})
        if not isinstance(result.get("unbound"), list):
            raise RoomHoldError("unbind response lacks removed bindings")
        unbound.extend(result["unbound"])
    state = inspect_room(conv, get)
    if not state.held:
        raise RoomHoldError("room routing changed during hold: " + state.description)
    return state, unbound
