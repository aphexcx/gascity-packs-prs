"""Shared Go/Python fixtures and the real tail command with a fake gc read."""
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest

PACK = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(PACK / "scripts"))
from slack_redact import redact_text

FIXTURES = PACK / "tests/fixtures/redaction"


@pytest.mark.parametrize("case", json.loads((FIXTURES / "cases.json").read_text()),
                         ids=lambda case: case["name"])
def test_shared_patterns(case):
    assert redact_text(case["text"]) == case["want"]
    assert redact_text(case["want"]) == case["want"]


def test_tail_dm_and_clipping(tmp_path):
    event = json.loads((FIXTURES / "dm.json").read_text())["event"]
    secret = event["text"].split()[-1]
    # A second long token starts inside the 420-character output window and
    # ends beyond it: redaction must precede truncation.
    messages = [event, {"ts": "1792436307.0", "user": "U1",
                        "text": ". " * 205 + "a" * 100},
                {"ts": "1792436308.0", "user": "U1",
                 "text": "Bearer Bearer short.token-42"}]
    fake_gc = tmp_path / "gc"
    fake_gc.write_text("#!/usr/bin/env python3\nprint(" + repr(json.dumps({"messages": messages})) + ")\n")
    fake_gc.chmod(0o700)
    env = {**os.environ, "PATH": str(tmp_path) + os.pathsep + os.environ["PATH"],
           "ALLOW": "", "WITHHOLD": ""}
    result = subprocess.run([sys.executable, str(PACK / "scripts/slack_tail.py"),
                             "D0DM", "-", "3"], env=env, capture_output=True, text=True, check=True)
    assert secret not in result.stdout
    assert "[REDACTED:openai:...7z9Q]" in result.stdout
    assert "aaaa" not in result.stdout
    assert "short.token-42" not in result.stdout
    assert "[REDACTED:bearer:...n-42]" in result.stdout
    env["WITHHOLD"] = "U1"
    hidden = subprocess.run([sys.executable, str(PACK / "scripts/slack_tail.py"),
                             "D0DM"], env=env, capture_output=True, text=True, check=True)
    assert "[withheld author;" in hidden.stdout
    assert "Synthetic gateway" not in hidden.stdout
    assert secret not in hidden.stdout
