"""Regression tests for the company-room agent response contract."""

from pathlib import Path


FRAGMENT = (
    Path(__file__).resolve().parents[1]
    / "template-fragments"
    / "slack-v0.template.md"
)


def test_thread_ambient_response_gate_and_native_identity_contract() -> None:
    prompt = FRAGMENT.read_text(encoding="utf-8")
    normalized = " ".join(prompt.lower().split())

    assert "`thread_ambient`" in prompt
    assert "distinct case-insensitive word" in normalized
    assert "directly and strongly relevant or actionable" in normalized
    assert "otherwise, do not post" in normalized
    assert "slack already attributes every reply to your agent identity" in normalized
    assert "do not prefix the message with your name or handle" in normalized

    assert "always prefix your slack message" not in normalized
    assert "prefixed with your identity" not in normalized
    assert "single asterisks" not in normalized


def test_mention_only_standing_rules_and_explicit_reply_form() -> None:
    prompt = FRAGMENT.read_text(encoding="utf-8")
    normalized = " ".join(prompt.lower().split())

    assert "mention-only mode" in normalized
    assert "@mentions of your bot user" in normalized
    assert "messages addressed to your handle" in normalized
    assert "replies in threads you posted in" in normalized
    assert "gc slack read --conversation-id <channel>" in prompt
    assert (
        "gc slack publish-to-channel --conversation-id <channel> "
        "--thread-ts <thread_ts> --body-file <file>"
    ) in prompt
    assert (
        "gc slack react --conversation-id <channel> "
        "--message-id <message_ts> --emoji writing_hand"
    ) in prompt
    assert "reply-current --thread-current" in prompt
    assert "upload --thread-current" in prompt
    assert "once gc has confirmed" in normalized
    assert "company-room pointer" in normalized
