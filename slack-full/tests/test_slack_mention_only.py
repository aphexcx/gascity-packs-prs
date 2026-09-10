"""Tests for the mention-only room binding mode (jg-vobf70).

Pack-side contract under test:

* ``gc slack bind-room C session --mentions-only`` registers the session
  with the adapter (POST /mention-only) instead of gc, records the
  binding in the pack config, and refuses the group-participant flags.
* The reply tooling resolves mention-only deliveries: the latest-inbound
  scan merges gc events with the adapter's delivery log (newest wins),
  ``reply-current --thread-current`` / ``--turn-ts`` anchor on them and
  publish via the adapter (the session holds no gc binding), and
  ``upload --thread-current`` posts bindingless into the delivering room.
* ``gc slack status`` lists mention-only bindings from the pack config and
  the adapter's registry file.
"""

from __future__ import annotations

import json
import pathlib
import sys
from typing import Any

import pytest

PACK_DIR = pathlib.Path(__file__).resolve().parent.parent
SCRIPTS_DIR = PACK_DIR / "scripts"
sys.path.insert(0, str(SCRIPTS_DIR))

ADAPTER_BASE = "http://127.0.0.1:8372/v0/city/test-city/svc/slack"


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch, tmp_path: pathlib.Path) -> None:
    monkeypatch.setenv("GC_CITY_NAME", "test-city")
    monkeypatch.setenv("GC_CITY_PATH", str(tmp_path))
    monkeypatch.setenv("GC_API_BASE_URL", "http://127.0.0.1:8372")
    monkeypatch.setenv("SLACK_WORKSPACE_ID", "T0TESTWS")
    monkeypatch.setenv("GC_SESSION_ID", "jg-mayor-1")
    monkeypatch.delenv("GC_SESSION_NAME", raising=False)
    monkeypatch.delenv("GC_SLACK_ADAPTER_ENV", raising=False)
    monkeypatch.delenv("SLACK_MENTION_ONLY_BINDINGS_FILE", raising=False)


def _import(*names: str):
    for name in ("slack_intake_common",) + names:
        sys.modules.pop(name, None)
    import slack_intake_common  # type: ignore
    mods = [slack_intake_common]
    for name in names:
        mods.append(__import__(name))
    return mods


def _delivery(channel: str, ts: str, thread_ts: str = "", received_at: str = "2026-09-10T08:00:00Z",
              reason: str = "bot_mention") -> dict[str, Any]:
    return {
        "session_id": "jg-mayor-1",
        "channel_id": channel,
        "ts": ts,
        "thread_ts": thread_ts,
        "reason": reason,
        "received_at": received_at,
    }


# --- bind-room --mentions-only ---------------------------------------------------

def test_bind_room_mentions_only_registers_with_adapter_not_gc(
        monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture) -> None:
    common, bind = _import("slack_chat_bind_room")
    gc_posts: list[tuple[str, dict]] = []
    adapter_calls: list[dict[str, Any]] = []

    monkeypatch.setattr(common, "gc_post", lambda path, body: gc_posts.append((path, body)) or {"ID": "never"})
    monkeypatch.setattr(common, "gc_get", lambda path: {
        "items": [{"id": "jg-mayor-1", "alias": "mayor", "session_name": "mayor"}],
    } if path == "/sessions" else {})

    def fake_register(**kw: Any) -> dict[str, Any]:
        adapter_calls.append(kw)
        return {"stored": True, "channel_id": kw["channel_id"], "binding": kw}

    monkeypatch.setattr(common, "register_mention_only_via_adapter", fake_register)
    nudges: list[tuple[str, str, str]] = []
    monkeypatch.setattr(bind, "deliver_protocol_nudge",
                        lambda sid, cid, template=bind.PROTOCOL_NUDGE_TEMPLATE, **f: nudges.append((sid, cid, template)))

    rc = bind.main(["C0B2Y13DRMK", "mayor", "--mentions-only"])
    assert rc == 0
    # No gc group, participant or binding call of any kind.
    assert gc_posts == []
    assert adapter_calls == [{
        "channel_id": "C0B2Y13DRMK",
        "session_id": "jg-mayor-1",
        "session_name": "mayor",
        "handle": "mayor",
    }]
    # The nudge is the mention-only variant, addressed by resolved id.
    assert nudges == [("jg-mayor-1", "C0B2Y13DRMK", bind.MENTION_ONLY_NUDGE_TEMPLATE)]
    assert "mention-only" in bind.MENTION_ONLY_NUDGE_TEMPLATE.lower()

    out = json.loads(capsys.readouterr().out)
    assert out["delivery_mode"] == "mentions_only"
    assert out["mention_only_participants"] == [
        {"handle": "mayor", "session_name": "mayor", "session_id": "jg-mayor-1"},
    ]

    cfg = common.load_pack_config()
    rec = cfg["bindings"]["room:C0B2Y13DRMK"]
    assert rec["delivery_mode"] == "mentions_only"
    assert rec["conversation"]["conversation_id"] == "C0B2Y13DRMK"
    assert rec["mention_only_participants"][0]["session_id"] == "jg-mayor-1"


def test_bind_room_mentions_only_keeps_existing_ambient_record(
        monkeypatch: pytest.MonkeyPatch) -> None:
    common, bind = _import("slack_chat_bind_room")
    cfg = common.load_pack_config()
    cfg["bindings"]["room:C1"] = {
        "kind": "room",
        "conversation": {"conversation_id": "C1"},
        "group_id": "jg-grp",
        "participants": [{"handle": "ops", "session_name": "ops-session"}],
    }
    common.save_pack_config(cfg)

    monkeypatch.setattr(common, "gc_post", lambda *_a, **_k: pytest.fail("gc must not be called"))
    monkeypatch.setattr(common, "gc_get", lambda path: {"items": []})
    monkeypatch.setattr(common, "register_mention_only_via_adapter", lambda **kw: {"stored": True})
    monkeypatch.setattr(bind, "deliver_protocol_nudge", lambda *a, **k: None)

    assert bind.main(["C1", "mayor", "--mentions-only", "--handle", "chief=mayor"]) == 0
    rec = common.load_pack_config()["bindings"]["room:C1"]
    # Ambient participants and group untouched; mention-only list is a sibling.
    assert rec["participants"] == [{"handle": "ops", "session_name": "ops-session"}]
    assert rec["group_id"] == "jg-grp"
    assert "delivery_mode" not in rec
    assert rec["mention_only_participants"] == [
        {"handle": "chief", "session_name": "mayor", "session_id": "mayor"},
    ]


def test_bind_room_mentions_only_rejects_session_already_ambient_here(
        monkeypatch: pytest.MonkeyPatch) -> None:
    """codex r1 P1: a gc group member is woken for every message, so
    layering mention-only on top would keep those wakes AND add injections."""
    common, bind = _import("slack_chat_bind_room")
    cfg = common.load_pack_config()
    cfg["bindings"]["room:C1"] = {
        "kind": "room",
        "conversation": {"conversation_id": "C1"},
        "group_id": "jg-grp",
        "participants": [{"handle": "mayor", "session_name": "mayor"}],
    }
    common.save_pack_config(cfg)
    monkeypatch.setattr(common, "gc_get", lambda path: {
        "items": [{"id": "jg-mayor-1", "alias": "mayor", "session_name": "mayor"}],
    } if path == "/sessions" else {"items": []})
    monkeypatch.setattr(common, "register_mention_only_via_adapter",
                        lambda **kw: pytest.fail("adapter must not be called on a conflict"))
    # By name and by resolved id alike.
    for who in ("mayor", "jg-mayor-1"):
        with pytest.raises(SystemExit) as exc:
            bind.main(["C1", who, "--mentions-only"])
        assert "AMBIENTLY" in str(exc.value)

    # A gc-side binding (binding owner / bind-dm) to the same conversation
    # is a conflict too, even with no pack-config participant record.
    cfg["bindings"] = {}
    common.save_pack_config(cfg)
    monkeypatch.setattr(common, "gc_get", lambda path: {"items": [
        {"Status": "active", "Conversation": {"ConversationID": "C1"}},
    ]} if path.startswith("/extmsg/bindings") else {"items": []})
    with pytest.raises(SystemExit) as exc:
        bind.main(["C1", "mayor", "--mentions-only"])
    assert "AMBIENTLY" in str(exc.value)


def test_bind_room_ambient_rejects_session_already_mention_only_here(
        monkeypatch: pytest.MonkeyPatch) -> None:
    common, bind = _import("slack_chat_bind_room")
    cfg = common.load_pack_config()
    cfg["bindings"]["room:C1"] = {
        "kind": "room",
        "conversation": {"conversation_id": "C1"},
        "delivery_mode": "mentions_only",
        "mention_only_participants": [
            {"handle": "mayor", "session_name": "mayor", "session_id": "jg-mayor-1"},
        ],
    }
    common.save_pack_config(cfg)
    monkeypatch.setattr(common, "gc_get", lambda path: {"items": []})
    monkeypatch.setattr(common, "gc_post", lambda *a, **k: pytest.fail("gc must not be called on a conflict"))
    with pytest.raises(SystemExit) as exc:
        bind.main(["C1", "mayor"])
    assert "MENTION-ONLY" in str(exc.value)

    # A different session binds ambiently fine, and the mention-only record
    # for mayor rides along on the rewritten pack-config record.
    monkeypatch.setattr(common, "gc_post", lambda path, body: {"ID": "grp-1"} if path == "/extmsg/groups" else {"ID": "p-1"})
    monkeypatch.setattr(bind, "deliver_protocol_nudge", lambda *a, **k: None)
    assert bind.main(["C1", "ops-session"]) == 0
    rec = common.load_pack_config()["bindings"]["room:C1"]
    assert rec["participants"] == [{"handle": "ops-session", "session_name": "ops-session"}]
    assert rec["mention_only_participants"][0]["session_id"] == "jg-mayor-1"


@pytest.mark.parametrize("flag", [
    ["--enable-peer-fanout"],
    ["--allow-untargeted-publication"],
    ["--max-peer-triggered-publishes", "3"],
    ["--max-total-peer-deliveries", "9"],
    ["--default-handle", "mayor"],
    ["--binding-owner", "mayor"],
])
def test_bind_room_mentions_only_rejects_group_flags(
        monkeypatch: pytest.MonkeyPatch, flag: list[str]) -> None:
    common, bind = _import("slack_chat_bind_room")
    monkeypatch.setattr(common, "gc_post", lambda *_a, **_k: pytest.fail("gc must not be called"))
    monkeypatch.setattr(common, "register_mention_only_via_adapter",
                        lambda **kw: pytest.fail("adapter must not be called"))
    with pytest.raises(SystemExit) as exc:
        bind.main(["C1", "mayor", "--mentions-only", *flag])
    assert flag[0] in str(exc.value)


def test_bind_room_mentions_only_adapter_without_support_fails_loudly(
        monkeypatch: pytest.MonkeyPatch) -> None:
    common, bind = _import("slack_chat_bind_room")
    monkeypatch.setattr(common, "gc_get", lambda path: {"items": []})

    def refuse(**kw: Any) -> dict[str, Any]:
        raise common.AdapterError("POST .../mention-only -> 404: not found")

    monkeypatch.setattr(common, "register_mention_only_via_adapter", refuse)
    with pytest.raises(SystemExit) as exc:
        bind.main(["C1", "mayor", "--mentions-only"])
    assert "mention-only support" in str(exc.value)


def test_register_mention_only_posts_through_adapter_proxy(
        monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()
    captured: dict[str, Any] = {}

    def fake_request(method: str, url: str, body: dict[str, Any] | None = None,
                     *, csrf: bool = True, timeout: float = 30.0) -> dict[str, Any]:
        captured.update(method=method, url=url, body=body, csrf=csrf)
        return {"stored": True}

    monkeypatch.setattr(common, "_request", fake_request)
    res = common.register_mention_only_via_adapter(
        channel_id="C1", session_id="jg-1", session_name="mayor", handle="mayor")
    assert res == {"stored": True}
    assert captured["method"] == "POST"
    assert captured["url"] == ADAPTER_BASE + "/mention-only"
    assert captured["csrf"] is True
    assert captured["body"] == {
        "channel_id": "C1", "session_id": "jg-1", "session_name": "mayor", "handle": "mayor",
    }


# --- latest-inbound resolution -----------------------------------------------------

def test_latest_inbound_prefers_newer_mention_only_delivery(
        monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()
    gc_event = {"type": "extmsg.inbound", "ts": "2026-09-10T07:00:00+00:00",
                "payload": {"provider": "slack", "conversation_id": "D1", "target_session": "jg-mayor-1"}}
    monkeypatch.setattr(common, "_scan_gc_inbound_events", lambda sid: gc_event)
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter",
                        lambda sid: [_delivery("C1", "2.000", "1.000", "2026-09-10T08:00:00Z")])

    event = common.find_latest_inbound_for_session("jg-mayor-1")
    payload = event["payload"]
    assert payload["mention_only"] is True
    assert payload["conversation_id"] == "C1"
    assert payload["message_id"] == "2.000"
    assert payload["thread_ts"] == "1.000"
    assert payload["target_session"] == "jg-mayor-1"

    # Thread lookup anchors on the adapter record — no transcript needed.
    monkeypatch.setattr(common, "_transcript_items_desc",
                        lambda *a, **k: pytest.fail("mention-only must not hit the transcript"))
    mid, root, conv = common.find_latest_inbound_thread_for_session("jg-mayor-1")
    assert (mid, root) == ("2.000", "1.000")
    assert conv["conversation_id"] == "C1"
    assert conv["kind"] == "room"
    assert conv["mention_only"] == "1"


def test_latest_inbound_keeps_gc_event_when_newer(monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()
    gc_event = {"type": "extmsg.inbound", "ts": "2026-09-10T09:00:00+08:00",  # 01:00Z
                "payload": {"provider": "slack", "conversation_id": "D1", "target_session": "jg-mayor-1"}}
    monkeypatch.setattr(common, "_scan_gc_inbound_events", lambda sid: gc_event)
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter",
                        lambda sid: [_delivery("C1", "2.000", received_at="2026-09-10T00:30:00.123456789Z")])
    assert common.find_latest_inbound_for_session("jg-mayor-1") is gc_event


def test_latest_inbound_falls_back_to_mention_only_when_gc_has_nothing(
        monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()
    monkeypatch.setattr(common, "_scan_gc_inbound_events", lambda sid: None)
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter",
                        lambda sid: [_delivery("C1", "3.000")])
    event = common.find_latest_inbound_for_session("jg-mayor-1")
    assert event["payload"]["message_id"] == "3.000"
    # And nothing at all when both sources are empty / unreachable.
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter", lambda sid: [])
    assert common.find_latest_inbound_for_session("jg-mayor-1") is None


def test_mention_only_deliveries_tolerate_transport_and_shape_problems(
        monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()

    def refuse(method: str, url: str, body=None, *, csrf: bool = True, timeout: float = 30.0):
        raise common.GCAPIError("GET ... -> 404")

    monkeypatch.setattr(common, "_request", refuse)
    assert common.mention_only_deliveries_via_adapter("jg-mayor-1") == []

    def odd_shape(method: str, url: str, body=None, *, csrf: bool = True, timeout: float = 30.0):
        assert method == "GET" and url.startswith(ADAPTER_BASE + "/mention-only/deliveries?")
        return {"items": [
            {"channel_id": "C1", "ts": "1.000"},           # valid
            {"channel_id": "", "ts": "2.000"},             # dropped
            {"ts": "3.000"},                               # dropped
            "garbage",                                     # dropped
            {"channel_id": "C1", "ts": 4},                 # dropped (non-string ts)
        ]}

    monkeypatch.setattr(common, "_request", odd_shape)
    got = common.mention_only_deliveries_via_adapter("jg-mayor-1")
    assert [d["ts"] for d in got] == ["1.000"]
    assert common.mention_only_deliveries_via_adapter("") == []


def test_mention_only_delivery_by_ts(monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter", lambda sid: [
        _delivery("C1", "5.000", "4.000"), _delivery("C1", "6.000"),
    ])
    assert common.mention_only_delivery_by_ts("jg-mayor-1", "C1", "5.000") == (
        "4.000", common._mention_only_conversation("C1"))
    assert common.mention_only_delivery_by_ts("jg-mayor-1", "C1", "6.000")[0] == ""
    assert common.mention_only_delivery_by_ts("jg-mayor-1", "C2", "5.000") is None
    assert common.mention_only_delivery_by_ts("jg-mayor-1", "C1", "9.000") is None


def test_session_is_mention_only_in_reads_registry(monkeypatch: pytest.MonkeyPatch) -> None:
    (common,) = _import()
    monkeypatch.setattr(common, "list_mention_only_via_adapter", lambda channel_id="": {
        "C1": [{"session_id": "jg-mayor-1", "session_name": "mayor", "handle": "mayor"}],
    } if channel_id in ("", "C1") else {})
    monkeypatch.setattr(common, "session_identity_candidates", lambda sid: {sid})
    assert common.session_is_mention_only_in("jg-mayor-1", "C1")
    assert common.session_is_mention_only_in("mayor", "C1")
    assert not common.session_is_mention_only_in("jg-other", "C1")
    assert not common.session_is_mention_only_in("jg-mayor-1", "C2")

    def refuse(channel_id: str = ""):
        raise common.AdapterError("no such endpoint")

    monkeypatch.setattr(common, "list_mention_only_via_adapter", refuse)
    assert not common.session_is_mention_only_in("jg-mayor-1", "C1")


# --- reply-current -------------------------------------------------------------------

def test_reply_current_thread_current_on_mention_only_delivery_goes_via_adapter(
        monkeypatch: pytest.MonkeyPatch, capsys: pytest.CaptureFixture) -> None:
    common, rc = _import("slack_chat_reply_current")
    posts: list[tuple[str, str, dict]] = []

    def fake_request(method: str, url: str, body: dict[str, Any] | None = None,
                     *, csrf: bool = True, timeout: float = 30.0) -> dict[str, Any]:
        if method == "POST":
            posts.append((method, url, body or {}))
            return {"delivered": True, "message_id": "9.000"}
        return {}

    monkeypatch.setattr(common, "_request", fake_request)
    monkeypatch.setattr(common, "_scan_gc_inbound_events", lambda sid: None)
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter",
                        lambda sid: [_delivery("C0B2Y13DRMK", "2.000", "1.000")])
    monkeypatch.setattr(common, "look_up_binding",
                        lambda _sid: pytest.fail("binding lookup must not run"))

    code = rc.main(["--session", "jg-mayor-1", "--body", "on it", "--thread-current"])
    assert code == 0
    assert len(posts) == 1
    method, url, body = posts[0]
    assert url == ADAPTER_BASE + "/publish", "must publish via the adapter — the session has no gc binding here"
    assert body["conversation"]["conversation_id"] == "C0B2Y13DRMK"
    assert body["conversation"]["kind"] == "room"
    assert body["reply_to_message_id"] == "1.000", "threads at the ROOT of the delivering thread"
    err = capsys.readouterr().err
    assert "mention-only room" in err


def test_reply_current_turn_ts_resolves_mention_only_delivery(
        monkeypatch: pytest.MonkeyPatch) -> None:
    common, rc = _import("slack_chat_reply_current")
    posts: list[dict[str, Any]] = []

    def fake_request(method: str, url: str, body: dict[str, Any] | None = None,
                     *, csrf: bool = True, timeout: float = 30.0) -> dict[str, Any]:
        if method == "POST":
            posts.append({"url": url, "body": body or {}})
            return {"delivered": True, "message_id": "9.000"}
        return {}

    monkeypatch.setattr(common, "_request", fake_request)
    monkeypatch.setattr(common, "find_inbound_thread_by_ts", lambda conv, ts: None)
    monkeypatch.setattr(common, "find_latest_inbound_thread_for_session",
                        lambda sid: pytest.fail("--turn-ts must not scan"))
    monkeypatch.setattr(common, "mention_only_deliveries_via_adapter",
                        lambda sid: [_delivery("C1", "7.000", "6.000")])

    code = rc.main(["--session", "jg-mayor-1", "--conversation-id", "C1",
                    "--turn-ts", "7.000", "--body", "reply"])
    assert code == 0
    assert posts[0]["url"] == ADAPTER_BASE + "/publish"
    assert posts[0]["body"]["reply_to_message_id"] == "6.000"


def test_reply_current_explicit_conversation_falls_back_to_adapter_only_when_gc_refuses(
        monkeypatch: pytest.MonkeyPatch) -> None:
    common, rc = _import("slack_chat_reply_current")
    posts: list[str] = []

    def fake_request(method: str, url: str, body: dict[str, Any] | None = None,
                     *, csrf: bool = True, timeout: float = 30.0) -> dict[str, Any]:
        if method == "POST":
            posts.append(url)
            if url.endswith("/extmsg/outbound"):
                raise common.GCAPIError("POST .../extmsg/outbound -> 404: no binding")
            return {"delivered": True, "message_id": "9.000"}
        return {}

    monkeypatch.setattr(common, "_request", fake_request)
    monkeypatch.setattr(common, "find_latest_inbound_for_session", lambda sid: None)
    monkeypatch.setattr(common, "find_latest_inbound_thread_for_session", lambda sid: None)
    monkeypatch.setattr(common, "look_up_binding", lambda sid: None)
    monkeypatch.setattr(common, "session_is_mention_only_in", lambda sid, cid: cid == "C1")

    # Mention-only room: gc refuses, adapter takes over.
    assert rc.main(["--session", "jg-mayor-1", "--conversation-id", "C1",
                    "--body", "hi", "--reply-to", "1.000"]) == 0
    assert [u.rsplit("/", 1)[-1] for u in posts] == ["outbound", "publish"]

    # Not a mention-only room: gc's refusal stays a failure (no double route).
    posts.clear()
    code = rc.main(["--session", "jg-mayor-1", "--conversation-id", "C2",
                    "--body", "hi", "--reply-to", "1.000"])
    assert code == 1
    assert [u.rsplit("/", 1)[-1] for u in posts] == ["outbound"]


# --- upload --------------------------------------------------------------------------

def test_upload_thread_current_on_mention_only_delivery_posts_bindingless(
        monkeypatch: pytest.MonkeyPatch, tmp_path: pathlib.Path, capsys: pytest.CaptureFixture) -> None:
    common, upload = _import("slack_chat_upload")
    captured: dict[str, Any] = {}

    def fake_request(method: str, url: str, body: dict[str, Any] | None = None,
                     *, csrf: bool = True, timeout: float = 30.0) -> dict[str, Any]:
        captured.update(method=method, url=url, body=body)
        return {"delivered": True, "file_id": "F1"}

    monkeypatch.setattr(common, "_request", fake_request)
    monkeypatch.setattr(common, "look_up_binding",
                        lambda _sid: pytest.fail("binding lookup must not run for a mention-only delivery"))
    mo_conv = common._mention_only_conversation("C0B2Y13DRMK")
    monkeypatch.setattr(common, "find_latest_inbound_message_id_for_session",
                        lambda _sid: ("2.000", mo_conv))
    f = tmp_path / "shot.png"
    f.write_bytes(b"\x89PNG")

    assert upload.main(["--file", str(f), "--session", "jg-mayor-1", "--thread-current"]) == 0
    assert captured["url"] == ADAPTER_BASE + "/publish-file"
    assert captured["body"]["conversation"]["conversation_id"] == "C0B2Y13DRMK"
    assert captured["body"]["conversation"]["kind"] == "room"
    assert captured["body"]["reply_to_message_id"] == "2.000"
    assert "mention-only room" in capsys.readouterr().err


# --- status --------------------------------------------------------------------------

def test_status_lists_mention_only_bindings_from_config_and_registry_file(
        monkeypatch: pytest.MonkeyPatch, tmp_path: pathlib.Path) -> None:
    common, status = _import("slack_chat_status")
    cfg = common.load_pack_config()
    cfg["bindings"]["room:C1"] = {
        "kind": "room",
        "conversation": {"conversation_id": "C1"},
        "delivery_mode": "mentions_only",
        "mention_only_participants": [
            {"handle": "mayor", "session_name": "mayor", "session_id": "jg-mayor-1"},
        ],
    }
    common.save_pack_config(cfg)
    reg = tmp_path / ".gc" / "slack" / "mention_only_bindings.json"
    reg.parent.mkdir(parents=True)
    reg.write_text(json.dumps({"version": 1, "channels": {
        "C1": [{"session_id": "jg-mayor-1", "session_name": "mayor", "handle": "mayor"}],
        "C2": [{"session_id": "jg-ops-1", "handle": "ops"}],
    }}))
    monkeypatch.setattr(common, "_request", lambda method, url, body=None, *, csrf=True, timeout=30.0: {"items": []})

    st = status.collect_status(session="", since="", limit=10)
    rows = st["mention_only_bindings"]
    assert [(r["conversation_id"], r["session_id"], r["source"]) for r in rows] == [
        ("C1", "jg-mayor-1", "pack-config+registry"),
        ("C2", "jg-ops-1", "registry"),
    ]
    text = status.format_status(st)
    assert "Mention-only room bindings" in text
    assert "C1  → mayor (jg-mayor-1)  handle=@mayor" in text
    assert "C2  → jg-ops-1  handle=@ops" in text

    # --session narrows by id or name.
    narrowed = status.collect_status(session="mayor", since="", limit=10)["mention_only_bindings"]
    assert [r["conversation_id"] for r in narrowed] == ["C1"]
    # No bindings → no section.
    reg.unlink()
    cfg["bindings"] = {}
    common.save_pack_config(cfg)
    st = status.collect_status(session="", since="", limit=10)
    assert st["mention_only_bindings"] == []
    assert "Mention-only" not in status.format_status(st)
