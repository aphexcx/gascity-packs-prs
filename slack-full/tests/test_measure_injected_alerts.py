"""Synthetic fixtures: no transcript content belongs in the repository."""

from datetime import datetime, timezone
import importlib.util
import json
import os
from pathlib import Path


spec = importlib.util.spec_from_file_location(
    "measure_injected_alerts",
    Path(__file__).resolve().parents[1] / "scripts" / "measure_injected_alerts.py",
)
measure = importlib.util.module_from_spec(spec)
spec.loader.exec_module(measure)


def reminder(body="private synthetic payload 🌱"):
    return (
        "<system-reminder>\nNew message in shared conversation slack/C1:\n\n"
        "- Example (U1): [thread reply — reply with --thread-ts 100.1]\n"
        "[slack thread_ts=100.1]\n"
        "Thread context (1 earlier message):\n@example: parent placeholder\n\n---\n\n"
        f"{body}\n\nMessage id: 100.2 (in thread 100.1)\n\n"
        "Reply: gc slack reply-current --conversation-id C1 --reply-to 100.1 --body-file <file>\n"
        "</system-reminder>"
    )


def test_partition_preserves_utf8_and_separates_context_from_message():
    body = "private synthetic payload 🌱\nReply: this is part of the payload"
    text = reminder(body)
    counts = measure.classify(text)
    assert sum(counts.values()) == len(text.encode())
    assert counts["message"] == len(body.encode())
    assert counts["author"] == len("- Example (U1): ")
    assert counts["thread_context"] == len(
        "Thread context (1 earlier message):\n@example: parent placeholder\n\n---\n\n"
    )
    assert counts["reply"] == len(
        "Reply: gc slack reply-current --conversation-id C1 --reply-to 100.1 --body-file <file>"
    )


def test_report_counts_deliveries_and_batches_without_leaking_text(tmp_path):
    transcripts = tmp_path / "transcripts"
    transcripts.mkdir()
    block = reminder()
    rows = [
        {"type": "queue-operation", "content": block},
        {"type": "api-request-blob", "message": {"content": block}},
        {"type": "user", "uuid": "one", "sessionId": "session", "timestamp": "2026-10-20T12:00:00Z", "message": {"content": "<wrapper>\n" + block}},
        {"type": "attachment", "uuid": "two", "sessionId": "session", "timestamp": "2026-10-20T12:01:00Z", "rendered": [{"content": block + "\n" + block}]},
        {"type": "assistant"},
        {"type": "user", "uuid": "old", "sessionId": "session", "timestamp": "2026-10-01T12:00:00Z", "message": {"content": block}},
    ]
    rows.append(rows[2])  # duplicate serialization of the same record
    fixture = transcripts / "fixture.jsonl"
    fixture.write_text("".join(json.dumps(r) + "\n" for r in rows))
    stamp = datetime(2026, 10, 21, 23, tzinfo=timezone.utc).timestamp()
    os.utime(fixture, (stamp, stamp))
    report = measure.measure(transcripts, tmp_path / "scratch", datetime(2026, 10, 21, 23, tzinfo=timezone.utc))
    assert report["messages"] == 3
    assert report["record_batch_histogram"] == {1: 1, 2: 1}
    assert report["before_assistant_batch_histogram"] == {3: 1}
    assert report["excluded_outside_timestamp_window"] == 1
    assert report["daily_reply_repetition"]["2026-10-20"]["exact_repeats_after_first"] == 2
    assert "private synthetic payload" not in json.dumps(report)
    assert len((tmp_path / "scratch" / "blocks.jsonl").read_text().splitlines()) == 3


def test_projection_retains_first_context_per_session_and_all_payload():
    rows = [
        {"timestamp": "2026-10-20T12:00:00Z", "session": "one", "text": reminder()},
        {"timestamp": "2026-10-20T12:01:00Z", "session": "one", "text": reminder()},
        {"timestamp": "2026-10-20T12:02:00Z", "session": "two", "text": reminder()},
    ]
    total = sum(len(row["text"].encode()) for row in rows)
    result = measure.project_cuts(rows, total)
    assert result["thread_context_policy_counts"] == {"first_retained": 2, "repeat_removed": 1}
    assert result["savings_bytes"]["thread_context"] == measure.classify(reminder())["thread_context"]
    assert result["smallest_tokens_per_day"] > result["full_set_tokens_per_day"]
    assert result["full_set_tokens_per_day"] > 0
    assert "private synthetic payload" not in json.dumps(result)
