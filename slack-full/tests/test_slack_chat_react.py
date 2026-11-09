"""Reaction CLI attribution survives the actual adapter request boundary."""

import io
import json
import pathlib
import sys

import pytest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent.parent / "scripts"))


@pytest.mark.parametrize("explicit", [False, True])
@pytest.mark.parametrize("override", [False, True])
def test_react_sends_calling_session(monkeypatch, explicit, override):
    for name in ("slack_chat_react", "slack_intake_common"):
        sys.modules.pop(name, None)
    import slack_chat_react as react

    common = react.common
    monkeypatch.setenv("GC_SESSION_ID", "s-current")
    monkeypatch.setattr(common, "adapter_publish_url", lambda: "http://adapter/publish")
    monkeypatch.setattr(common, "_adapter_csrf_headers", lambda: {})
    seen = []

    def latest(session):
        seen.append(session)
        return "100.1", {"conversation_id": "C1"}

    captured = []

    def request(req, timeout):
        assert req.full_url == "http://adapter/react"
        captured.append(json.loads(req.data))
        return io.BytesIO(b'{"delivered":true}')

    monkeypatch.setattr(common, "find_latest_inbound_message_id_for_session", latest)
    monkeypatch.setattr(common.urllib.request, "urlopen", request)
    args = ["--conversation-id", "C1", "--message-id", "100.1"] if explicit else []
    if override:
        args += ["--session", "s-override"]
    assert react.main(args) == 0
    expected = "s-override" if override else "s-current"
    assert captured[0]["session_id"] == expected
    assert captured[0]["conversation"] == {"conversation_id": "C1"}
    assert captured[0]["message_id"] == "100.1"
    assert seen == ([] if explicit else [expected])


def test_explicit_operator_reaction_has_no_session_proof(monkeypatch):
    for name in ("slack_chat_react", "slack_intake_common"):
        sys.modules.pop(name, None)
    import slack_chat_react as react

    monkeypatch.delenv("GC_SESSION_ID", raising=False)
    monkeypatch.delenv("GC_SESSION_NAME", raising=False)
    captured = []
    monkeypatch.setattr(react.common, "react_via_adapter", lambda **kw: captured.append(kw) or {"delivered": True})
    assert react.main(["--conversation-id", "C1", "--message-id", "100.1"]) == 0
    assert captured[0]["session_id"] == ""
    with pytest.raises(SystemExit, match="neither GC_SESSION_ID nor GC_SESSION_NAME"):
        react.main([])
