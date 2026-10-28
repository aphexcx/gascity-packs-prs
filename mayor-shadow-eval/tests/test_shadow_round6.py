"""Indirect-read modes must be rejected by the real hook, without a sandbox."""
import hashlib
import json
import shutil
import subprocess

import pytest

from test_shadow_guard import guard, hook
from test_shadow_round3 import read_city


SAFE = "~/city/work/stone-videos-1027/file.txt"


@pytest.mark.parametrize("command,expected", [
    ("shasum checksums --check", "deny"),
    ("shasum -c list", "deny"),
    ("shasum checksums -c", "deny"),
    ("shasum checksums -bc", "deny"),
    ("shasum --check checksums", "deny"),
    ("shasum -c -", "deny"),
    ("shasum checksums -check", "deny"),
    ("du --files0-from=x", "deny"),
    ("du file --files0-from=x", "deny"),
    ("du file --files0-from x", "deny"),
    ("wc --files0-from x", "deny"),
    ("wc file --files0-from=x", "deny"),
    ("wc file --files0-from -", "deny"),
    ("tail --follow=name file", "deny"),
    ("tail file --follow=name", "deny"),
    ("tail file -f", "deny"),
    ("tail file -F", "deny"),
    ("grep -f list file", "deny"),
    ("grep pattern file --file=list", "deny"),
    ("rg -f list file", "deny"),
    ("rg pattern file --file=list", "deny"),
    ("md5 file -c digest", "deny"),
    ("md5 -c digest file", "deny"),
    ("md5 file --check=digest", "deny"),
    ("file file -f list", "deny"),
    ("ffprobe file -i list", "deny"),
    (f"shasum {SAFE}", "allow"),
    (f"shasum {SAFE} -a 256", "allow"),
    (f"md5 {SAFE} -q", "allow"),
    ("cat -", "allow"),
    ("shasum -- checksums --check", "allow"),
    ("cat -- --files0-from=x", "allow"),
    ("grep -e --file=list file", "allow"),
    ("rg -e -f file", "allow"),
    ("tail -- file --follow=name", "allow"),
])
def test_indirect_modes_through_real_hook(tmp_path, read_city, command, expected):
    (read_city / "work/stone-videos-1027/file.txt").write_text("synthetic public fixture\n")
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash",
                             "tool_input": {"command": command}})
    assert result.returncode == 0
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == expected
    record = json.loads((tmp_path / "decisions.jsonl").read_text())
    assert record["decision"] == expected
    if expected == "deny":
        assert output["permissionDecisionReason"] == guard.SHAPE_REASON
        assert record["class"] == "shape-denied"


@pytest.mark.parametrize("flag", ["--check", "-c"])
def test_installed_shasum_verifies_hidden_filename_but_hook_denies(tmp_path, read_city, flag):
    binary = shutil.which("shasum")
    if not binary:
        pytest.skip("installed shasum required for manifest integration")
    # The only filename in the manifest points at our synthetic excluded root.
    # No sandbox is involved: the hook alone must prevent this secondary read.
    payload = b"synthetic excluded fixture\n"
    (read_city / "assets/private/fixture.txt").write_bytes(payload)
    (read_city / "checksums").write_text(
        f"{hashlib.sha1(payload).hexdigest()}  assets/private/fixture.txt\n")
    actual = subprocess.run([binary, "checksums", flag], cwd=read_city,
                            capture_output=True, text=True, timeout=5)
    assert actual.returncode == 0, actual.stderr
    assert "assets/private/fixture.txt: OK" in actual.stdout
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash",
                             "tool_input": {"command": f"shasum checksums {flag}"}})
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == "deny"
    assert output["permissionDecisionReason"] == guard.SHAPE_REASON
