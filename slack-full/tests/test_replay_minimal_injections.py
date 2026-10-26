import importlib.util
from pathlib import Path

MODULE = Path(__file__).parents[1] / "scripts" / "replay_minimal_injections.py"
spec = importlib.util.spec_from_file_location("replay_minimal", MODULE)
replay = importlib.util.module_from_spec(spec)
spec.loader.exec_module(replay)


def reminder(body, kind="human"):
    return ("<system-reminder>\nNew message in shared conversation slack/C_TEST:\n\n"
            f"- Example ({kind}): {body}\n\nMessage id: 100.3 (in thread 100.1)\n\n"
            "Reply: gc slack reply-current --conversation-id C_TEST --reply-to 100.1 --body-file <file>\n</system-reminder>")


def test_replay_preserves_new_payload_and_removes_context():
    text = reminder('[thread reply — reply with --thread-ts 100.1; parent @Example: "parent"]\n'
                    '[slack thread_ts=100.1]\nThread context (1 earlier message):\n@Example: prior\n\n---\n\n'
                    'new text\n\n[peer-bot context — read only]\npeer-bot Peer posted in C_TEST at ts 100.2: old\n[end peer-bot context]')
    rows, counts = replay.prepare([{"text": text}])
    unit = rows[-1]["units"][0]
    assert unit["body"] == "new text"
    assert unit["anchor"] == '[reply in thread 100.1; parent @Example: "parent"]'
    assert unit["thread"] == "100.1"
    assert unit["preamble"] == "Thread context (1 earlier message):\n@Example: prior\n\n---\n\n"
    assert counts["baseline_bytes"] == len(text.encode())
    assert rows[-1]["foot"].endswith("reply-to 100.1\n</system-reminder>")
    assert counts["peer_arrivals"] == 1
    assert rows[0]["units"][0]["body"] == "old"
    assert rows[0]["units"][0]["ts"] == "100.2"
    assert rows[0]["units"][0]["is_bot"]


def test_replay_keeps_each_batch_member_coordinates():
    text = reminder("[2 messages in C_TEST, coalesced. Reply with --reply-to 100.1]\n\n"
                    "[100.2] First (in thread 100.1): first body\n\n[100.3] Second: second body")
    rows, counts = replay.prepare([{"text": text}])
    assert counts["batches"] == 1
    assert counts["members"] == 2
    assert [(u["ts"], u["thread"], u["body"]) for u in rows[0]["units"]] == [
        ("100.2", "100.1", "first body"), ("100.3", "", "second body")]


def test_replay_keeps_immediate_peer_payload():
    text = reminder("[peer-bot context — read only]\npeer-bot Peer posted in C_TEST (thread 100.1) at ts 100.3: new peer text\n[end peer-bot context]", "agent")
    rows, _ = replay.prepare([{"text": text}])
    assert rows[0]["units"][0]["body"] == "new peer text"
    assert rows[0]["units"][0]["is_bot"]
