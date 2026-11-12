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


def test_reaction_vocabulary_contract() -> None:
    prompt = FRAGMENT.read_text(encoding="utf-8")
    normalized = " ".join(prompt.split())

    assert prompt.startswith('{{ define "slack-v0" -}}\n')
    assert prompt.endswith("{{- end }}\n")

    respond = prompt.index("## When to respond")
    reactions = prompt.index("## Reactions")
    reply = prompt.index("## How to reply")
    assert respond < reactions < reply

    section = prompt[reactions:reply]
    for name in ("eyes", "mag", "+1", "white_check_mark", "alarm_clock"):
        assert f"| `{name}` |" in section, name
    assert "gc slack react [--emoji NAME] [--session SID]" in section
    assert (
        "gc slack react --conversation-id Cxxx --message-id 1234.5678 [--emoji NAME]"
        in section
    )

    assert (
        "a message to you that only confirms, thanks, agrees or closes a topic "
        "gets 👍 as the whole answer. No text follows it."
    ) in normalized
