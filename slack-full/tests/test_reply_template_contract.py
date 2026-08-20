"""Cross-language contract tests for the gp-729 reply-instruction texts.

The Go adapter registers a one-line reply template with gc (rendered
into every inbound reminder) and delivers a fuller once-per-channel
how-to block. Both name `gc slack` CLI invocations that are parsed on
the Python side — these tests pin the Go-side command shapes to flags
the Python scripts actually accept, so a flag rename in either language
breaks loudly here instead of stranding agents with dead instructions.
"""

from __future__ import annotations

import re
from pathlib import Path

PACK_DIR = Path(__file__).resolve().parents[1]
COALESCER_GO = PACK_DIR / "adapter" / "inbound_coalescer.go"
REPLY_CURRENT_PY = PACK_DIR / "scripts" / "slack_chat_reply_current.py"
REACT_PY = PACK_DIR / "scripts" / "slack_chat_react.py"


def _go_source() -> str:
    return COALESCER_GO.read_text(encoding="utf-8")


def _template_line() -> str:
    match = re.search(
        r'slackReplyInstructionsTemplate = "([^"]+)"', _go_source()
    )
    assert match, "slackReplyInstructionsTemplate const not found in inbound_coalescer.go"
    return match.group(1)


def _argparse_flags(script: Path) -> set[str]:
    return set(re.findall(r'add_argument\(\s*"(--[a-z-]+)"', script.read_text(encoding="utf-8")))


def test_registered_template_flags_exist_on_reply_current() -> None:
    template = _template_line()
    assert "gc slack reply-current" in template
    assert "{conversation_id}" in template, "gc substitutes this placeholder per reminder"

    flags = set(re.findall(r"--[a-z-]+", template))
    known = _argparse_flags(REPLY_CURRENT_PY)
    missing = flags - known
    assert not missing, f"template names flags reply-current does not parse: {missing}"


def test_help_block_flags_exist_on_reply_current_and_react() -> None:
    go_src = _go_source()
    match = re.search(r"func replyHelpBlock.*?return fmt\.Sprintf\((.*?)\n\}", go_src, re.S)
    assert match, "replyHelpBlock not found"
    block = match.group(1)

    assert "gc slack reply-current" in block
    assert "gc slack react" in block

    reply_flags = {f for f in re.findall(r"--[a-z-]+", block) if f != "--emoji"}
    known_reply = _argparse_flags(REPLY_CURRENT_PY)
    missing = reply_flags - known_reply
    assert not missing, f"help block names flags reply-current does not parse: {missing}"

    assert "--emoji" in _argparse_flags(REACT_PY), "help block instructs `gc slack react --emoji`"


def test_template_is_single_line() -> None:
    # The whole point of gp-729 item 3: the per-message form is ONE line.
    assert "\\n" not in _template_line()
