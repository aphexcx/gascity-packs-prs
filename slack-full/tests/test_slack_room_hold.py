"""Mention-only CLI behavior against a stateful fake of gc's public API."""

import copy
import fcntl
import hashlib
import importlib
import json
import pathlib
import sys
import threading
import urllib.error
import urllib.parse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "scripts"))

CONV = {"scope_id": "test-city", "provider": "slack", "account_id": "T1",
        "conversation_id": "C1", "kind": "room"}


def record(ident, label, **metadata):
    row = {"id": ident, "status": "open", "labels": [label], "metadata": {
        "schema_version": "1", **{k: v for k, v in CONV.items() if k != "kind"},
        "conversation_kind": "room", **metadata,
    }}
    if label in ("gc:extmsg-group", "gc:extmsg-binding"):
        keys = ("scope_id", "provider", "account_id", "conversation_id",
                "parent_conversation_id", "conversation_kind")
        wire = json.dumps([row["metadata"].get(k, "") for k in keys], separators=(",", ":"))
        prefix = "extmsg:group:root:v1:" if label == "gc:extmsg-group" else "extmsg:binding:conv:v1:"
        row["labels"].append(prefix + hashlib.sha256(wire.encode()).hexdigest())
    return row


class FakeGC:
    def __init__(self):
        self.rows = []
        self.posts = []
        self.partial = False
        self.page_size = 200
        self.after_ensure = None
        self.after_group_binding_read = None
        self.group_lookup_override = None
        self.inbounds = 0
        self.queries = []

    def inbound(self):
        self.inbounds += 1
        if not any("gc:extmsg-group" in r["labels"] or "gc:extmsg-binding" in r["labels"]
                   for r in self.rows):
            self.binding()

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
        if url.path == "/svc/slack/never-silent/status":
            return {"enabled": False, "health": {"state": "unknown"}, "outages": [], "owner_lines_seven_days": 0}
        if url.path == "/sessions":
            return {"items": [{"id": "s-mayor", "alias": "mayor", "session_name": "mayor"}]}
        if url.path == "/beads":
            self.queries.append(q["label"])
            rows = [r for r in self.rows if q["label"] in r["labels"]]
            start = int(q.get("cursor", "0"))
            end = start + self.page_size
            result = {"items": copy.deepcopy(rows[start:end]), "partial": self.partial,
                      "next_cursor": str(end) if end < len(rows) else ""}
            if (q["label"].startswith("extmsg:binding:conv:v1:") and self.after_group_binding_read
                    and any("gc:extmsg-group" in r["labels"] for r in self.rows)):
                self.after_group_binding_read()
            return result
        if url.path == "/extmsg/groups":
            if self.group_lookup_override:
                return self.group_lookup_override
            if any("gc:extmsg-group" in r["labels"] and all(
                r["metadata"].get("conversation_kind" if k == "kind" else k, "") == q.get(k, "")
                for k in ("scope_id", "provider", "account_id", "conversation_id",
                          "parent_conversation_id", "kind")) for r in self.rows):
                return {"ID": "g1", "RootConversation": CONV}
            common = importlib.import_module("slack_intake_common")
            raise common.GCAPIError("extmsg-group-not-found") from urllib.error.HTTPError(
                path, 404, "Not Found", {}, None)
        if url.path == "/extmsg/bindings":
            return {"items": []}  # Agent-name bindings are absent here in gc.
        if url.path.startswith(("/events", "/extmsg/adapters")):
            return {"items": []}
        raise AssertionError(path)

    def post(self, path, body):
        self.posts.append((path, copy.deepcopy(body)))
        if path == "/extmsg/groups":
            if not any("gc:extmsg-group" in r["labels"]
                       and r["metadata"].get("conversation_id") == body["root_conversation"]["conversation_id"]
                       for r in self.rows):
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
    def request(method, url, body=None, **kwargs):
        assert method == "GET"
        parsed = urllib.parse.urlsplit(url)
        assert parsed.path.startswith("/v0/city/test-city/")
        return api.get(parsed.path.removeprefix("/v0/city/test-city") + "?" + parsed.query)
    monkeypatch.setattr(common, "_request", request)
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
    assert rec["unbind_filter"] == out["unbind_filter"] == bind.room_hold.UNBIND_FILTER
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
    api.binding(expires_at="2000-01-01T00:00:00Z")["id"] = "expired"
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
    assert held["unreadable"] == {}
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
    assert unknown["unreadable"] == {"mention_only_bindings.C1": "incomplete gc routing inventory"}
    assert status.main([]) == 2
    assert status.main(["--json"]) == 2


@pytest.mark.parametrize("authority", [["--group-only"], ["--binding-owner", "ops"]])
def test_ambient_bind_drops_recorded_hold(app, authority):
    _, bind, _, common, _ = app
    bind.main(["C1", "mayor", "--mentions-only"])
    bind.main(["C1", "ops", *authority])
    rec = common.load_pack_config()["bindings"]["room:C1"]
    assert rec["group_held"] is False
    assert rec["participants"] == [{"handle": "ops", "session_name": "ops"}]
    assert rec["mention_only_participants"][0]["session_name"] == "mayor"


def test_group_is_verified_before_unbind_and_blocks_inbound_default_route(app, monkeypatch):
    api, bind, _, common, _ = app
    api.inbound()
    assert len(api.rows) == 1  # Default route really creates a sticky binding without a group.
    def post(path, body):
        result = api.post(path, body)
        if path == "/extmsg/unbind":
            assert any("gc:extmsg-group" in r["labels"] for r in api.rows)
            api.inbound()
            assert not any("gc:extmsg-binding" in r["labels"] for r in api.rows)
        return result
    api.after_ensure = api.inbound
    monkeypatch.setattr(common, "gc_post", post)
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    assert api.inbounds == 3
    assert [path for path, _ in api.posts] == ["/extmsg/groups", "/extmsg/unbind"]


@pytest.mark.parametrize("replacement", ["same-agent", "different-agent", "session", "second", "new-id"])
def test_changed_binding_aborts_unbind_with_partial_result(app, capsys, replacement):
    api, bind, _, common, _ = app
    original = api.binding()
    def change():
        api.after_group_binding_read = None
        if replacement == "second":
            api.binding(agent="ops", caller="api")["id"] = "b2"
        elif replacement == "new-id":
            original["id"] = "b2"
        else:
            api.rows.remove(original)
            api.binding(agent="" if replacement == "session" else (
                "ops" if replacement == "different-agent" else "mayor"), caller="api",
                **({"session_id": "ops"} if replacement == "session" else {}))["id"] = "b2"
    api.after_group_binding_read = change
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 1
    captured = capsys.readouterr()
    out = json.loads(captured.out)
    assert out["hold"] == "partial (binding changed during the hold; re-run bind-room)"
    assert "hold: partial" in captured.err
    assert out["group_id"] == "g1" and out["group_held"] is False
    assert out["unbound_bindings"] == []
    assert [path for path, _ in api.posts] == ["/extmsg/groups"]
    rec = common.load_pack_config()["bindings"]["room:C1"]
    assert rec["hold_status"] == out["hold"] and rec["group_id"] == "g1"
    assert rec["unbind_filter"] == out["unbind_filter"]


def test_concurrent_bind_room_runs_serialize_and_second_observes_hold(app, monkeypatch):
    api, bind, _, common, registrations = app
    api.binding()
    ensured, release, second_started = threading.Event(), threading.Event(), threading.Event()
    real_lock = bind.room_hold.bind_room_lock
    def pause():
        ensured.set()
        assert release.wait(5)
    api.after_ensure = pause
    @contextmanager
    def observed_lock(path):
        if ensured.is_set():
            second_started.set()
        with real_lock(path):
            yield
    monkeypatch.setattr(bind.room_hold, "bind_room_lock", observed_lock)
    with ThreadPoolExecutor(max_workers=2) as pool:
        first = pool.submit(bind.main, ["C1", "mayor", "--mentions-only"])
        try:
            assert ensured.wait(5)
            # Verify an actual kernel lock, not just invocation ordering.
            with (common.pack_state_dir() / "room-hold.lock").open("a") as lock:
                with pytest.raises(BlockingIOError):
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            second = pool.submit(bind.main, ["C1", "mayor", "--mentions-only"])
            assert second_started.wait(5)
            assert len(registrations) == 1
        finally:
            release.set()
        assert first.result(timeout=5) == second.result(timeout=5) == 0
    assert len(registrations) == 2
    assert [path for path, _ in api.posts] == ["/extmsg/groups", "/extmsg/unbind"]
    assert common.load_pack_config()["bindings"]["room:C1"]["group_held"] is True


def test_authoritative_group_missing_from_inventory_refuses_before_ensure(app):
    api, bind, _, common, _ = app
    api.group_lookup_override = {"ID": "relocated-group", "DefaultHandle": "ops"}
    with pytest.raises(SystemExit, match="group lookup disagrees"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert not api.posts
    assert common.load_pack_config()["bindings"]["room:C1"]["group_held"] is False


def test_group_only_clears_a_previously_recorded_binding_owner(app):
    _, bind, _, common, _ = app
    bind.main(["C1", "ops", "--binding-owner", "ops"])
    assert common.load_pack_config()["bindings"]["room:C1"]["binding_owner"] == "ops"
    bind.main(["C1", "ops", "--group-only"])
    assert common.load_pack_config()["bindings"]["room:C1"]["binding_owner"] is None


@pytest.mark.parametrize("http_code", [404, 503])
def test_group_lookup_failure_is_not_absence(app, monkeypatch, http_code):
    api, bind, _, common, _ = app
    def get(path):
        if path.startswith("/extmsg/groups?"):
            raise common.GCAPIError("city-not-found or unavailable") from urllib.error.HTTPError(
                path, http_code, "Unavailable", {}, None)
        return api.get(path)
    monkeypatch.setattr(common, "gc_get", get)
    with pytest.raises(SystemExit, match="group hold failed"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert not api.posts


def test_mismatched_ensured_group_refuses_to_unbind(app, monkeypatch):
    api, bind, _, common, _ = app
    api.binding()
    def post(path, body):
        result = api.post(path, body)
        return {"ID": "another-group"} if path == "/extmsg/groups" else result
    monkeypatch.setattr(common, "gc_post", post)
    with pytest.raises(SystemExit, match="group identity changed"):
        bind.main(["C1", "mayor", "--mentions-only"])
    assert [path for path, _ in api.posts] == ["/extmsg/groups"]


@pytest.mark.parametrize("conv,expected", [
    (CONV, "167ab5279c069572d7d3cf3cb101a95294168a2bfd55a172a697ab305dd5923f"),
    ({**CONV, "provider": " Slack ", "kind": " ROOM ", "scope_id": "test-city "},
     "167ab5279c069572d7d3cf3cb101a95294168a2bfd55a172a697ab305dd5923f"),
    ({**CONV, "scope_id": "city<&>", "account_id": "Té", "conversation_id": "C\u2028\u2029Z"},
     "923fcaa9fb3ff0cbe6e11324afca6c7f2f8ab03ac58008a1031d1f6042bcc1cb"),
])
def test_room_labels_match_go_json_hash_vectors(app, conv, expected):
    # Golden values produced by Go encoding/json + crypto/sha256, matching
    # gc labels.go:197-201 (including HTML and Unicode separator escaping).
    _, bind, _, _, _ = app
    assert bind.room_hold._conversation_hash(conv) == expected


def test_room_queries_ignore_foreign_malformed_records(app):
    api, bind, status, _, _ = app
    for label in ("gc:extmsg-group", "gc:extmsg-binding"):
        foreign = record("foreign-" + label, label, conversation_id="C-other")
        foreign["metadata"]["schema_version"] = "future-version"
        api.rows.append(foreign)
    assert bind.main(["C1", "mayor", "--mentions-only"]) == 0
    snapshot = status.collect_status(session="", since="", limit=10)
    assert snapshot["unreadable"] == {}
    assert snapshot["mention_only_bindings"][0]["group_held"] is True
    assert set(api.queries) == {
        "extmsg:group:root:v1:167ab5279c069572d7d3cf3cb101a95294168a2bfd55a172a697ab305dd5923f",
        "extmsg:binding:conv:v1:167ab5279c069572d7d3cf3cb101a95294168a2bfd55a172a697ab305dd5923f",
        "extmsg:group:participant:v1:g1",
    }


@pytest.mark.parametrize("mode", ["held", "opt-out", "ambient"])
def test_nudge_only_claims_a_verified_hold(app, monkeypatch, mode):
    api, bind, _, _, _ = app
    if mode == "ambient":
        api.group()
        api.participant()
    nudges = []
    monkeypatch.setattr(bind, "deliver_protocol_nudge",
                        lambda sid, cid, template, **kw: nudges.append(
                            template.format(conversation_id=cid, **kw)))
    assert bind.main(["C1", "mayor", "--mentions-only"] + (
        ["--no-group-hold"] if mode == "opt-out" else [])) == 0
    assert ("ambient lines reach no gc session" in nudges[0]) == (mode == "held")


def test_status_names_explicit_agent_binding(app):
    api, bind, status, _, _ = app
    api.binding(agent="ops", caller="api")
    bind.main(["C1", "mayor", "--mentions-only"])
    snapshot = status.collect_status(session="", since="", limit=10)
    assert "not held: explicit agent bindings ops" in status.format_status(snapshot)
    assert snapshot["mention_only_bindings"][0]["explicit_agent_bindings"] == ["ops"]


def test_missing_city_context_is_an_unreadable_room_hold(app, monkeypatch):
    _, bind, status, common, _ = app
    bind.main(["C1", "mayor", "--mentions-only"])
    def no_city():
        raise common.GCAPIError("city context unavailable")
    monkeypatch.setattr(common, "gc_city_name", no_city)
    row = status._mention_only_bindings("")[0]
    assert row["group_held"] is None and row["hold_error"] == "city context unavailable"
