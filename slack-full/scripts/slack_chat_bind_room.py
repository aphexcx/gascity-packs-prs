#!/usr/bin/env python3
"""Bind a Slack room/channel to one or more named gc sessions.

Creates a launcher-mode conversation group rooted at the Slack channel
and adds each named session as a participant. With ``--enable-peer-fanout``
or any of the related fanout flags, the group is created with a fanout
policy preserved on the group record.

Why a group instead of a binding per session: gc bindings are 1:1 by
conversation (a second ``Bind`` call returns ``ErrBindingConflict``).
Memberships are 1:N and are what drives peer-fanout system reminders
(``extmsgNotifyMembers``). The simplest way to create N memberships
through the public API today is via group participants.
"""

from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import urllib.parse
from typing import Any

import slack_intake_common as common


# Reply protocol delivered to each newly-bound participant. Sent on every
# bind (idempotent — duplicate nudges are harmless). Without this, a
# participant session that respawned after the original bind would lose
# the protocol contract and revert to its baseline prompt's reply path.
PROTOCOL_NUDGE_TEMPLATE = """<system-reminder>
Slack channel binding established for {conversation_id}.
You are a slack-bound agent in this conversation.

Reply protocol when you receive a `New message in shared conversation slack/...` reminder:

  1. **FIRST**: react with writing_hand — BEFORE you compose anything, BEFORE you
     read context, BEFORE you think about the reply.
       gc slack react --emoji writing_hand
     This signals to the human that you are actively working on the message.
     The adapter already placed 👀 on the message when it enqueued the inbound
     (transport-level ack: "queued for the agent"). Your ✍️ is the agent-level
     signal: "I am processing." Replying first means the human waits in silence
     until your full reply lands, even when you have an instant answer.

  2. THEN compose your reply to a tmpfile.

  3. THEN publish as a threaded reply (NOT publish-to-channel):
       gc slack reply-current --body-file <tmpfile> --thread-current

  4. THEN ack so the inbound is marked read:
       gc transcript read --ack

The order is non-negotiable even when you have an instant answer. Even
when the inbound is a re-ping of an active thread. Even when it's a
"ping" or "ack". React first, every time.

Use `gc slack publish-to-channel --conversation-id ... --no-thread` ONLY
for explicit top-level status broadcasts initiated by you, never as a
reply to an inbound. The writing_hand react is for inbound-replies only —
proactive posts (e.g. surfacing slung work completion) skip the react and
just publish-to-thread.
</system-reminder>
"""


# Reply protocol for a MENTION-ONLY participant (jg-vobf70). The session is
# not a gc member of the room: the adapter injects a reminder only when the
# message @mentions the bot user, addresses the session's handle, or replies
# in a thread the session posted in — and the reply tooling resolves those
# injections through the adapter's delivery log, so the same react-first /
# threaded-reply protocol applies. Sent on every mention-only bind, like the
# ambient nudge.
MENTION_ONLY_NUDGE_TEMPLATE = """<system-reminder>
Slack MENTION-ONLY room binding established for {conversation_id}.
You are bound to this room in mention-only mode: you are woken only when a
message @mentions your bot user, addresses your handle (@{handle}: ...), or is
a reply in a thread you posted in. Everything else in the room stays silent
for you — read it on demand with:
  gc slack read --conversation-id {conversation_id}

Reply protocol when you receive a `Slack mention-only room delivery` reminder:

  1. **FIRST**: react with writing_hand — BEFORE you compose anything:
       gc slack react --emoji writing_hand
     (the reminder also carries the explicit --conversation-id/--message-id form)

  2. THEN compose your reply to a tmpfile.

  3. THEN publish as a threaded reply:
       gc slack reply-current --body-file <tmpfile> --thread-current
     (the reminder carries the explicit --conversation-id/--reply-to form;
     posts go through the slack adapter directly — you hold no channel
     binding here, so peers are not fanned out)

The order is non-negotiable even when you have an instant answer. React
first, every time.

Use `gc slack publish-to-channel --conversation-id {conversation_id} --no-thread`
ONLY for explicit top-level status broadcasts initiated by you, never as a
reply to an inbound. Replies you post in threads make follow-ups in those
threads reach you without a mention.
</system-reminder>
"""


def deliver_protocol_nudge(
    session_id: str,
    conversation_id: str,
    template: str = PROTOCOL_NUDGE_TEMPLATE,
    **fields: str,
) -> None:
    """Send the slack reply-protocol nudge to a session via `gc session nudge`.

    Best-effort — failures (session asleep, unknown target, gc binary
    missing) are logged to stderr and do not abort the bind. The nudge is
    idempotent so re-delivery on every bind is safe.
    """
    body = template.format(conversation_id=conversation_id, **fields)
    try:
        result = subprocess.run(
            ["gc", "session", "nudge", session_id, body],
            capture_output=True,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.TimeoutExpired) as exc:
        sys.stderr.write(
            f"warn: protocol nudge to {session_id} failed: {exc}\n"
        )
        return
    if result.returncode != 0:
        sys.stderr.write(
            f"warn: protocol nudge to {session_id} returned "
            f"rc={result.returncode}: {result.stderr.strip()[:200]}\n"
        )


def _slack_workspace_id() -> str:
    val = os.environ.get("SLACK_WORKSPACE_ID", "").strip()
    if not val:
        raise SystemExit("SLACK_WORKSPACE_ID must be set in the slack adapter env")
    return val


def _parse_handle_overrides(values: list[str]) -> dict[str, str]:
    """Parse repeated ``--handle handle=session`` flags.

    Returns a map of session_name -> handle. Raises SystemExit on
    malformed input. Handles are normalized to lowercase on the gc
    side; we don't pre-normalize here because the API does it for us.
    """
    overrides: dict[str, str] = {}
    for raw in values:
        if "=" not in raw:
            raise SystemExit(f"--handle expects HANDLE=SESSION, got: {raw!r}")
        handle, _, session = raw.partition("=")
        handle = handle.strip()
        session = session.strip()
        if not handle or not session:
            raise SystemExit(f"--handle expects HANDLE=SESSION, got: {raw!r}")
        if session in overrides:
            raise SystemExit(f"--handle session {session!r} specified twice")
        overrides[session] = handle
    return overrides


def _default_handle_for_session(session_name: str) -> str:
    """Derive a participant handle from a session alias or id.

    For aliases like ``geo/oversight-rig.project-lead``, take the last
    dot-separated segment of the path tail (``project-lead``) and
    prefix with the directory (``geo-project-lead``). For unstructured
    ids like ``gc-83347`` we fall back to the raw id; the caller is
    expected to override via ``--handle`` in that case.
    """
    if "/" in session_name:
        head, tail = session_name.split("/", 1)
        last = tail.rsplit(".", 1)[-1]
        return f"{head}-{last}"
    if "." in session_name:
        return session_name.rsplit(".", 1)[-1]
    return session_name


def resolve_session_identity(session_name: str) -> tuple[str, str]:
    """Resolve an operator-supplied session name or id to (id, name).

    The adapter's mention-only lane injects into the session by id and
    matches the session's own posts by the id /publish carries, so a
    name-bound participant must be resolved to gc's id at bind time.
    Consults ``GET /sessions``; when the session is not listed (gc
    unreachable, or a session that is not running yet) the literal
    string is used for both and a warning is printed — gc's
    session-message endpoint accepts names too.
    """
    try:
        res = common.gc_get("/sessions")
    except common.GCAPIError as exc:
        sys.stderr.write(f"warn: could not resolve session {session_name!r} via gc /sessions ({exc}); using it verbatim\n")
        return session_name, session_name
    for entry in res.get("items", []) or []:
        sid = (entry.get("id") or "").strip()
        alias = (entry.get("alias") or "").strip()
        sname = (entry.get("session_name") or "").strip()
        if session_name in (sid, alias, sname) and sid:
            return sid, (alias or sname or session_name)
    sys.stderr.write(f"warn: session {session_name!r} not found in gc /sessions; using it verbatim\n")
    return session_name, session_name


def build_conversation_ref(
    *, conversation_id: str, kind: str, workspace_id: str, scope_id: str
) -> dict[str, str]:
    return {
        "scope_id": scope_id,
        "provider": "slack",
        "account_id": workspace_id,
        "conversation_id": conversation_id,
        "kind": kind,
    }


def build_fanout_policy(args: argparse.Namespace) -> dict[str, Any] | None:
    """Translate CLI flags into a FanoutPolicy dict, or None if no flag set."""
    any_set = (
        args.enable_peer_fanout
        or args.allow_untargeted_publication
        or args.max_peer_triggered_publishes
        or args.max_total_peer_deliveries
    )
    if not any_set:
        return None
    return {
        "enabled": bool(args.enable_peer_fanout),
        "allow_untargeted_publication": bool(args.allow_untargeted_publication),
        "max_peer_triggered_publishes": int(args.max_peer_triggered_publishes or 0),
        "max_total_peer_deliveries": int(args.max_total_peer_deliveries or 0),
    }


def build_participants(
    sessions: list[str],
    overrides: dict[str, str],
    default_handle: str,
) -> list[tuple[str, str]]:
    """Return [(handle, session_name), ...] in the order sessions were given.

    Raises SystemExit on duplicate handles or empty input.
    """
    if not sessions:
        raise SystemExit("at least one session is required")
    out: list[tuple[str, str]] = []
    seen: set[str] = set()
    for session in sessions:
        handle = overrides.get(session) or _default_handle_for_session(session)
        if not handle:
            raise SystemExit(f"could not derive handle for session {session!r}")
        if handle in seen:
            raise SystemExit(
                f"duplicate handle {handle!r}; pass --handle to disambiguate")
        seen.add(handle)
        out.append((handle, session))
    if default_handle and default_handle not in seen:
        raise SystemExit(
            f"--default-handle {default_handle!r} does not match any participant handle "
            f"({sorted(seen)})")
    return out


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(
        description="Bind a Slack room/channel to one or more named gc sessions",
    )
    parser.add_argument("conversation_id", help="Slack channel id (e.g. C0123ROOM01)")
    parser.add_argument("session_names", nargs="+", help="gc session name or id")
    parser.add_argument("--kind", default="room", choices=("room",),
                        help="Conversation kind. Default: room")
    parser.add_argument("--mode", default="launcher", choices=("launcher",),
                        help="Group mode. Default: launcher")
    parser.add_argument("--default-handle", default="",
                        help="Default participant handle for untargeted messages "
                             "(must match one of the participant handles)")
    parser.add_argument("--handle", action="append", default=[],
                        metavar="HANDLE=SESSION",
                        help="Override the handle assigned to a session (repeatable)")
    parser.add_argument("--enable-peer-fanout", action="store_true",
                        help="Set FanoutPolicy.enabled = true on the group")
    parser.add_argument("--allow-untargeted-publication", action="store_true",
                        help="Set FanoutPolicy.allow_untargeted_publication = true")
    parser.add_argument("--max-peer-triggered-publishes", type=int, default=0,
                        help="Cap peer-triggered publishes per inbound (0 = unlimited)")
    parser.add_argument("--max-total-peer-deliveries", type=int, default=0,
                        help="Cap total peer deliveries per inbound (0 = unlimited)")
    parser.add_argument("--mentions-only", action="store_true",
                        help="Bind the listed sessions in MENTION-ONLY mode: the adapter "
                             "wakes each one only for messages that @mention the bot user, "
                             "address its handle (`@handle: ...`), or reply in a thread it "
                             "posted in; everything else in the room is dropped for it "
                             "(still readable via `gc slack read`). The sessions are "
                             "registered with the adapter (POST /mention-only), NOT added "
                             "as gc group participants — ambient participants bound in a "
                             "separate invocation are unaffected. Incompatible with the "
                             "peer-fanout flags, --default-handle and --binding-owner.")
    parser.add_argument("--no-protocol-nudge", action="store_true",
                        help="Skip auto-delivery of the slack reply-protocol nudge "
                             "(react first, threaded reply, ack) to each newly-bound "
                             "participant. By default the nudge is sent on every bind, "
                             "which is idempotent and safe — disable only if a caller "
                             "is composing its own protocol delivery.")
    parser.add_argument("--binding-owner", default="",
                        metavar="SESSION",
                        help="Also bind this session to the conversation as the publisher "
                             "for /extmsg/outbound. Required to make outbound publishes "
                             "work; without it, peer-fanout still fires but publishes need "
                             "a separate /extmsg/bind call. Should refer semantically to one "
                             "of the participants. Pass the gc-id (e.g. gc-77139) when the "
                             "binding will be looked up by gc-id (e.g. resolve_rig_channel.py); "
                             "pass the participant alias when the rest of the system reads "
                             "the binding by alias. The script does NOT cross-check the owner "
                             "against the participant list — this is intentional so gc-ids "
                             "can be used alongside alias-based participants.")
    args = parser.parse_args(argv)

    workspace_id = _slack_workspace_id()
    city = common.gc_city_name()
    overrides = _parse_handle_overrides(args.handle)
    participants = build_participants(args.session_names, overrides, args.default_handle)
    if args.mentions_only:
        return _main_mentions_only(args, workspace_id=workspace_id, city=city,
                                   participants=participants)
    default_handle = args.default_handle or participants[0][0]
    binding_owner = args.binding_owner.strip()
    conv = build_conversation_ref(
        conversation_id=args.conversation_id,
        kind=args.kind,
        workspace_id=workspace_id,
        scope_id=city,
    )
    fanout_policy = build_fanout_policy(args)

    # The mirror of the mention-only conflict check: a session registered
    # mention-only in this room must not also become a gc member (it would
    # be woken for every message AND injected on mentions).
    pre_cfg = common.load_pack_config()
    pre_key = f"{args.kind}:{args.conversation_id}"
    # The binding owner becomes an ambient recipient through /extmsg/bind
    # even when it is not a positional participant (codex r2 P2).
    ambient_candidates = [s for _, s in participants]
    if binding_owner and binding_owner not in ambient_candidates:
        ambient_candidates.append(binding_owner)
    mo_conflicts = mention_only_conflicts(pre_cfg, pre_key, ambient_candidates, args.conversation_id)
    if mo_conflicts:
        raise SystemExit(
            f"{', '.join(mo_conflicts)}: already bound MENTION-ONLY to {args.conversation_id}. "
            "Remove that binding first (DELETE …/svc/slack/mention-only?channel_id=&session_id=) "
            "before binding it ambiently.")

    group_body: dict[str, Any] = {
        "root_conversation": conv,
        "mode": args.mode,
        "default_handle": default_handle,
    }
    if fanout_policy is not None:
        group_body["fanout_policy"] = fanout_policy

    try:
        group = common.gc_post("/extmsg/groups", group_body)
    except common.GCAPIError as exc:
        raise SystemExit(f"ensure group: {exc}") from exc
    group_id = group.get("ID", "")
    if not group_id:
        raise SystemExit(f"ensure group: response missing ID: {group!r}")

    participant_records: list[dict[str, Any]] = []
    for handle, session in participants:
        try:
            res = common.gc_post(
                "/extmsg/participants",
                {"group_id": group_id, "handle": handle, "session_id": session, "public": True},
            )
        except common.GCAPIError as exc:
            raise SystemExit(f"upsert participant {handle}={session}: {exc}") from exc
        participant_records.append(res)

    binding_record: dict[str, Any] | None = None
    if binding_owner:
        try:
            binding_record = common.gc_post(
                "/extmsg/bind",
                {"session_id": binding_owner, "conversation": conv},
            )
        except common.GCAPIError as exc:
            raise SystemExit(f"bind {binding_owner}: {exc}") from exc

    cfg = common.load_pack_config()
    cfg.setdefault("bindings", {})
    binding_key = f"{args.kind}:{args.conversation_id}"
    # Other sessions' mention-only registrations for this room ride along
    # on the rewritten record (they live in the adapter's registry either
    # way; this keeps `gc slack status` truthful).
    prior_rec = cfg["bindings"].get(binding_key) or {}
    prior_mention_only = prior_rec.get("mention_only_participants")
    # gc's /extmsg/participants UPSERTS into the group: participants from
    # earlier invocations stay members, so the record keeps them too (by
    # session_name; this invocation's handles win). gc offers no
    # participant listing, so this record is what the mention-only
    # conflict check reads (codex r2 P2).
    merged_participants: dict[str, dict[str, str]] = {}
    for p in prior_rec.get("participants") or []:
        if isinstance(p, dict) and p.get("session_name"):
            merged_participants[p["session_name"]] = {"handle": p.get("handle", ""), "session_name": p["session_name"]}
    for h, sname in participants:
        merged_participants[sname] = {"handle": h, "session_name": sname}
    cfg["bindings"][binding_key] = {
        "kind": args.kind,
        "conversation": conv,
        "group_id": group_id,
        "default_handle": default_handle,
        "fanout_policy": fanout_policy,
        "participants": list(merged_participants.values()),
        "binding_owner": binding_owner or (prior_rec.get("binding_owner") or None),
        "binding_record": (binding_record or {}).get("ID", "") or None,
    }
    if prior_mention_only:
        cfg["bindings"][binding_key]["mention_only_participants"] = prior_mention_only
    common.save_pack_config(cfg)

    # Deliver the protocol nudge to each participant session so respawned
    # sessions don't lose the reply contract. Best-effort, opt-out via flag.
    nudged: list[str] = []
    nudge_failures: list[str] = []
    if not args.no_protocol_nudge:
        for _, session in participants:
            try:
                deliver_protocol_nudge(session, args.conversation_id)
                nudged.append(session)
            except Exception as exc:  # noqa: BLE001 — best-effort, never abort bind
                nudge_failures.append(f"{session}: {exc}")

    print(json.dumps({
        "binding_key": binding_key,
        "group_id": group_id,
        "default_handle": default_handle,
        "fanout_policy": fanout_policy,
        "participants": participant_records,
        "binding_owner": binding_owner or None,
        "binding_record": binding_record,
        "protocol_nudge": {
            "delivered_to": nudged,
            "failures": nudge_failures,
            "skipped": args.no_protocol_nudge,
        },
    }, indent=2, default=str))
    return 0


def _session_aliases(session: str) -> set[str]:
    """Every identifier a session may be recorded under (literal + gc id/name)."""
    ids = {session}
    try:
        sid, name = resolve_session_identity(session)
        ids |= {sid, name}
    except Exception:  # noqa: BLE001 — best-effort widening only
        pass
    return {i for i in ids if i}


def ambient_conflicts(cfg: dict[str, Any], binding_key: str, sessions: list[str]) -> list[str]:
    """Sessions already recorded as AMBIENT participants of this room.

    A gc group member is woken for every message, so registering it
    mention-only on top would keep the ambient wakes AND add injections
    (codex r1 P1). Checked against the pack config's participant list;
    the gc-side binding owner is checked by the caller.
    """
    rec = (cfg.get("bindings") or {}).get(binding_key) or {}
    ambient = {p.get("session_name") for p in rec.get("participants") or [] if isinstance(p, dict)}
    owner = rec.get("binding_owner")
    if owner:
        ambient.add(owner)
    ambient.discard(None)
    if not ambient:
        return []
    out: list[str] = []
    for session in sessions:
        if _session_aliases(session) & ambient:
            out.append(session)
    return out


def mention_only_conflicts(
    cfg: dict[str, Any], binding_key: str, sessions: list[str], conversation_id: str = ""
) -> list[str]:
    """Sessions still registered MENTION-ONLY in this room (the mirror check
    for an ambient bind).

    The pack config is the local record; the adapter's registry is what is
    enforced. When the adapter answers, a session the adapter no longer
    lists (removed via DELETE /mention-only) is reconciled out of the pack
    record instead of blocking the rebind forever (codex r2 P2). When the
    adapter is unreachable the local record stands.
    """
    rec = (cfg.get("bindings") or {}).get(binding_key) or {}
    entries = [p for p in rec.get("mention_only_participants") or [] if isinstance(p, dict)]
    if not entries:
        return []
    if conversation_id:
        try:
            # A room absent from the answer means "no bindings there"; only
            # a transport failure leaves the local record unverified.
            live = common.list_mention_only_via_adapter(conversation_id).get(conversation_id) or []
        except (common.AdapterError, common.GCAPIError):
            live = None
        if live is not None:
            live_ids = set()
            for b in live:
                if isinstance(b, dict):
                    live_ids |= {b.get("session_id") or "", b.get("session_name") or ""}
            live_ids.discard("")
            kept = [p for p in entries
                    if {p.get("session_name") or "", p.get("session_id") or ""} & live_ids]
            if len(kept) != len(entries):
                if kept:
                    rec["mention_only_participants"] = kept
                else:
                    rec.pop("mention_only_participants", None)
                    if rec.get("delivery_mode") == "mentions_only" and not rec.get("participants"):
                        cfg["bindings"].pop(binding_key, None)
                common.save_pack_config(cfg)
            entries = kept
    mo: set[str] = set()
    for p in entries:
        mo |= {p.get("session_name") or "", p.get("session_id") or ""}
    mo.discard("")
    if not mo:
        return []
    return [s for s in sessions if _session_aliases(s) & mo]


def gc_binding_to_conversation(session: str, conversation_id: str) -> bool:
    """True when gc holds an active extmsg binding of session to conversation_id
    (a `--binding-owner`, or a bind-dm). Best-effort: gc unreachable → False."""
    try:
        res = common.gc_get(f"/extmsg/bindings?session_id={urllib.parse.quote(session, safe='')}")
    except common.GCAPIError:
        return False
    for entry in res.get("items", []) or []:
        if entry.get("Status") != "active":
            continue
        conv = entry.get("Conversation") or {}
        cid = conv.get("ConversationID") or conv.get("conversation_id") or ""
        if cid == conversation_id:
            return True
    return False


def mention_only_incompatible_flags(args: argparse.Namespace) -> list[str]:
    """Flags that only make sense for a gc group participant."""
    bad: list[str] = []
    if args.enable_peer_fanout:
        bad.append("--enable-peer-fanout")
    if args.allow_untargeted_publication:
        bad.append("--allow-untargeted-publication")
    if args.max_peer_triggered_publishes:
        bad.append("--max-peer-triggered-publishes")
    if args.max_total_peer_deliveries:
        bad.append("--max-total-peer-deliveries")
    if (args.default_handle or "").strip():
        bad.append("--default-handle")
    if (args.binding_owner or "").strip():
        bad.append("--binding-owner")
    return bad


def record_mention_only_binding(
    cfg: dict[str, Any],
    *,
    binding_key: str,
    kind: str,
    conv: dict[str, str],
    records: list[dict[str, str]],
) -> dict[str, Any]:
    """Merge mention-only participants into the pack config's binding record.

    An existing AMBIENT record for the same room keeps its participants /
    group untouched; the mention-only list is a sibling field, upserted by
    session_name. A room with no record yet gets a minimal one whose
    ``delivery_mode`` says what it is.
    """
    cfg.setdefault("bindings", {})
    rec = cfg["bindings"].get(binding_key)
    if not isinstance(rec, dict):
        rec = {"kind": kind, "conversation": conv, "delivery_mode": "mentions_only"}
        cfg["bindings"][binding_key] = rec
    existing = [r for r in rec.get("mention_only_participants") or [] if isinstance(r, dict)]
    by_name = {r.get("session_name"): r for r in existing}
    for r in records:
        by_name[r["session_name"]] = r
    rec["mention_only_participants"] = [by_name[k] for k in sorted(by_name)]
    if not rec.get("participants"):
        rec["delivery_mode"] = "mentions_only"
    return rec


def _main_mentions_only(
    args: argparse.Namespace,
    *,
    workspace_id: str,
    city: str,
    participants: list[tuple[str, str]],
) -> int:
    """`gc slack bind-room ... --mentions-only` (jg-vobf70).

    1. Reject flags that only apply to gc group participants.
    2. Resolve each session to its gc id (the adapter injects by id).
    3. POST /mention-only on the adapter for each (channel, session, handle).
    4. Record the binding under the pack config (inspectable via
       `gc slack status` and the registry file).
    5. Deliver the mention-only reply-protocol nudge.

    No gc group, participant, or /extmsg/bind call is made: a gc member
    would be woken for every message, which is exactly what this mode
    exists to avoid.
    """
    bad = mention_only_incompatible_flags(args)
    if bad:
        raise SystemExit(
            "--mentions-only cannot be combined with " + ", ".join(bad)
            + " (those configure gc group participants; a mention-only session is not one)")

    conv = build_conversation_ref(
        conversation_id=args.conversation_id,
        kind=args.kind,
        workspace_id=workspace_id,
        scope_id=city,
    )
    cfg = common.load_pack_config()
    binding_key = f"{args.kind}:{args.conversation_id}"
    sessions = [session for _, session in participants]
    conflicts = ambient_conflicts(cfg, binding_key, sessions)
    conflicts += [s for s in sessions if s not in conflicts
                  and gc_binding_to_conversation(s, args.conversation_id)]
    if conflicts:
        raise SystemExit(
            f"{', '.join(conflicts)}: already bound AMBIENTLY to {args.conversation_id} "
            "(gc group participant / binding owner) — a gc member is woken for every "
            "message, so mention-only cannot be layered on top. Remove the ambient "
            "membership first (gc extmsg participants / bindings), then re-run.")

    registered: list[dict[str, Any]] = []
    records: list[dict[str, str]] = []
    for handle, session in participants:
        session_id, session_name = resolve_session_identity(session)
        try:
            res = common.register_mention_only_via_adapter(
                channel_id=args.conversation_id,
                session_id=session_id,
                session_name=session_name,
                handle=handle,
            )
        except common.AdapterError as exc:
            raise SystemExit(
                f"register mention-only {handle}={session}: {exc}\n"
                "(the running adapter must include mention-only support — "
                "POST /mention-only — restart it on the current pack build)") from exc
        registered.append(res)
        records.append({"handle": handle, "session_name": session_name, "session_id": session_id})

    rec = record_mention_only_binding(
        cfg, binding_key=binding_key, kind=args.kind, conv=conv, records=records)
    common.save_pack_config(cfg)

    nudged: list[str] = []
    nudge_failures: list[str] = []
    if not args.no_protocol_nudge:
        for r in records:
            try:
                deliver_protocol_nudge(
                    r["session_id"], args.conversation_id,
                    template=MENTION_ONLY_NUDGE_TEMPLATE, handle=r["handle"])
                nudged.append(r["session_id"])
            except Exception as exc:  # noqa: BLE001 — best-effort, never abort bind
                nudge_failures.append(f"{r['session_id']}: {exc}")

    print(json.dumps({
        "binding_key": binding_key,
        "delivery_mode": "mentions_only",
        "conversation_id": args.conversation_id,
        "mention_only_participants": rec.get("mention_only_participants", []),
        "adapter": registered,
        "protocol_nudge": {
            "delivered_to": nudged,
            "failures": nudge_failures,
            "skipped": args.no_protocol_nudge,
        },
    }, indent=2, default=str))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
