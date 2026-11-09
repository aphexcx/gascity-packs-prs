"""Mention-only CLI behavior against a stateful fake of gc's public API."""

import copy
import importlib
import json
import pathlib
import sys
import urllib.parse

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "scripts"))

CONV = {"scope_id": "test-city", "provider": "slack", "account_id": "T1",
        "conversation_id": "C1", "kind": "room"}


def record(ident, label, **metadata):
    return {"id": ident, "status": "open", "labels": [label], "metadata": {
        "schema_version": "1", **{k: v for k, v in CONV.items() if k != "kind"},
        "conversation_kind": "room", **metadata,
    }}


class FakeGC:
    def __init__(self):
        self.rows = []
        self.posts = []
        self.partial = False
        self.page_size = 200
        self.after_ensure = None

    def group(self):
        self.rows.append(record("g1", "gc:extmsg-group", default_handle=""))

    def participant(self, session="ops"):
        self.rows.append(record("p1", "extmsg:group:participant:v1:g1",
                                group_id="g1", session_id=session, session_name=session))

    def binding(self, agent="mayor", caller="extmsg-default-route", **metadata):
        row = record("b1", "gc:extmsg-binding", agent_name=agent,
                     created_by_kind="controller", created_by_id=caller, **metadata)
        self.rows.append(row)
        return row

    def get(self, path):
        url = urllib.parse.urlsplit(path)
        q = dict(urllib.parse.parse_qsl(url.query))
        if url.path == "/sessions":
            return {"items": [{"id": "s-mayor", "alias": "mayor", "session_name": "mayor"}]}
        if url.path == "/beads":
            rows = [r for r in self.rows if q["label"] in r["labels"]]
            start = int(q.get("cursor", "0"))
            end = start + self.page_size
            return {"items": copy.deepcopy(rows[start:end]), "partial": self.partial,
                    "next_cursor": str(end) if end < len(rows) else ""}
        if url.path == "/extmsg/bindings":
            return {"items": []}  # Agent-name bindings are absent here in gc.
        if url.path.startswith(("/events", "/extmsg/adapters")):
            return {"items": []}
        raise AssertionError(path)

    def post(self, path, body):
        self.posts.append((path, copy.deepcopy(body)))
        if path == "/extmsg/groups":
            if not any("gc:extmsg-group" in r["labels"] for r in self.rows):
                self.group()
            if self.after_ensure:
                self.after_ensure()
            return {"ID": "g1"}
        if path == "/extmsg/unbind":
            removed = [r for r in self.rows if "gc:extmsg-binding" in r["labels"]
                       and r["metadata"].get("agent_name") == body.get("agent_name")]
            self.rows = [r for r in self.rows if r not in removed]
            return {"unbound": [{"ID": r["id"], "AgentName": r["metadata"]["agent_name"]}
                                for r in removed]}
        if path == "/extmsg/participants":
            self.participant(body["session_id"])
            return {"ID": "p1"}
        if path == "/extmsg/bind":
            self.binding(agent="", caller="api", session_id=body["session_id"])
            return {"ID": "b1"}
        raise AssertionError(path)


@pytest.fixture
def app(monkeypatch, tmp_path):
    monkeypatch.setenv("GC_CITY_NAME", "test-city")
    monkeypatch.setenv("GC_CITY_PATH", str(tmp_path))
    monkeypatch.setenv("SLACK_WORKSPACE_ID", "T1")
    monkeypatch.delenv("GC_SLACK_ADAPTER_ENV", raising=False)
    monkeypatch.delenv("SLACK_MENTION_ONLY_BINDINGS_FILE", raising=False)
    for name in ("slack_intake_common", "slack_chat_bind_room", "slack_chat_status"):
        sys.modules.pop(name, None)
    common = importlib.import_module("slack_intake_common")
    bind = importlib.import_module("slack_chat_bind_room")
    status = importlib.import_module("slack_chat_status")
    api = FakeGC()
    registrations = []
    monkeypatch.setattr(common, "gc_get", api.get)
    monkeypatch.setattr(common, "gc_post", api.post)
    monkeypatch.setattr(common, "list_mention_only_via_adapter",
                        lambda channel: {channel: [r for r in registrations if r["channel_id"] == channel]})
    monkeypatch.setattr(common, "register_mention_only_via_adapter",
                        lambda **kw: registrations.append(kw) or {"stored": True})
    monkeypatch.setattr(bind, "deliver_protocol_nudge", lambda *a, **kw: None)
    return api, bind, status, common, registrations


@pytest.mark.parametrize("sticky", [False, True])
def test_empty_room_is_held_and_only_default_route_is_unbound(app, capsys, sticky):
    api, bind, _, common, registrations = app
    if sticky:
        api.binding()
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["group_held"] is True
    assert out["unbound_bindings"] == ([{"ID": "b1", "AgentName": "mayor"}] if sticky else [])
    assert api.posts == [("/extmsg/groups", {"root_conversation": CONV, "mode": "launcher"})] + (
        [("/extmsg/unbind", {"conversation": CONV, "agent_name": "mayor"})] if sticky else [])
    assert len(registrations) == 1
    rec = common.load_pack_config()["bindings"]["room:C1"]
    assert rec["group_held"] is True and rec["group_id"] == "g1"
    # Re-running the command preserves the existing empty group.
    api.posts.clear()
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    assert api.posts == []


@pytest.mark.parametrize("ambient", ["participant", "session", "agent"])
def test_ambient_routes_are_preserved_even_without_local_config(app, capsys, ambient):
    api, bind, _, common, _ = app
    if ambient == "participant":
        api.group()
        api.participant()
        api.binding()
    elif ambient == "session":
        api.binding(agent="", caller="api", session_id="ops")
    else:
        api.binding(agent="ops", caller="api")
    before = copy.deepcopy(api.rows)
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert out["group_held"] is False and out["unbound_bindings"] == []
    assert api.posts == [] and api.rows == before
    assert common.load_pack_config()["bindings"]["room:C1"]["ambient_participants"] == ["ops"]


def test_opt_out_registers_only_and_does_not_inspect_or_mutate_gc_routes(app, capsys):
    api, bind, _, common, registrations = app
    api.partial = True  # No inventory reads should be attempted.
    api.binding()
    assert bind.main(["C1", "mayor", "--mentions-only", "--no-group-hold"]) == 0
    out = json.loads(capsys.readouterr().out)
    assert not out["group_held"] and out["unbound_bindings"] == []
    assert api.posts == [] and registrations
    assert common.load_pack_config()["bindings"]["room:C1"]["group_held"] is False


def test_participant_added_during_ensure_prevents_unbind(app, capsys):
    api, bind, _, _, _ = app
    api.binding()
    api.after_ensure = api.participant
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    assert json.loads(capsys.readouterr().out)["group_held"] is False
    assert [p for p, _ in api.posts] == ["/extmsg/groups"]


def test_live_group_member_cannot_register_mention_only(app):
    api, bind, _, _, registrations = app
    api.group()
    api.participant("s-mayor")
    with pytest.raises(SystemExit, match="already bound AMBIENTLY"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert not registrations and not api.posts


def test_partial_inventory_fails_before_registration(app):
    api, bind, _, _, registrations = app
    api.partial = True
    with pytest.raises(SystemExit, match="incomplete gc routing inventory"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert not registrations and not api.posts


def test_pagination_and_exact_conversation_identity_preserve_late_ambient_binding(app, capsys):
    api, bind, _, _, _ = app
    api.page_size = 1
    api.binding(account_id="T-other")
    api.binding(agent="", caller="api", session_id="ops")
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    assert json.loads(capsys.readouterr().out)["group_held"] is False
    assert api.posts == []


def test_failed_unbind_never_records_a_successful_hold(app, monkeypatch):
    api, bind, _, common, registrations = app
    api.binding()
    def post(path, body):
        if path == "/extmsg/unbind":
            raise common.GCAPIError("unavailable")
        return api.post(path, body)
    monkeypatch.setattr(common, "gc_post", post)
    with pytest.raises(SystemExit, match="registered, but group hold failed"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert registrations
    assert common.load_pack_config()["bindings"]["room:C1"]["group_held"] is False


def test_missing_group_after_ensure_never_removes_the_default_binding(app, monkeypatch):
    api, bind, _, common, _ = app
    api.binding()
    monkeypatch.setattr(common, "gc_post", lambda path, body: {"ID": "missing"})
    with pytest.raises(SystemExit, match="ensured group is not visible"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert len(api.rows) == 1 and api.rows[0]["metadata"]["agent_name"] == "mayor"
    assert common.load_pack_config()["bindings"]["room:C1"]["group_held"] is False


def test_status_uses_live_routing_and_reports_unreadable_inventory(app):
    api, bind, status, _, _ = app
    bind.main(["C1", "mayor", "--mentions-only"])
    def snapshot():
        return status.collect_status(session="", since="", limit=10)
    held = snapshot()
    assert held["mention_only_bindings"][0]["group_held"] is True
    assert "held (zero-participant group)" in status.format_status(held)
    api.participant()
    ambient = snapshot()
    assert ambient["mention_only_bindings"][0]["group_held"] is False
    assert "not held: ambient participants ops" in status.format_status(ambient)
    api.partial = True
    unknown = snapshot()
    assert unknown["mention_only_bindings"][0]["group_held"] is None
    assert "unknown: incomplete gc routing inventory" in status.format_status(unknown)


@pytest.mark.parametrize("authority", [["--group-only"], ["--binding-owner", "ops"]])
def test_ambient_bind_drops_recorded_hold(app, authority):
    _, bind, _, common, _ = app
    bind.main(["C1", "mayor", "--mentions-only"])
    bind.main(["C1", "ops", *authority])
    rec = common.load_pack_config()["bindings"]["room:C1"]
    assert rec["group_held"] is False
    assert rec["participants"] == [{"handle": "ops", "session_name": "ops"}]
    assert rec["mention_only_participants"][0]["session_name"] == "mayor"
