"""Exercise option arguments and directory confinement through the actual hook."""
import json
import shlex
import subprocess

import pytest

from test_shadow_guard import guard, hook
from test_shadow_round3 import read_city


SAFE = "~/city/work/stone-videos-1027"


@pytest.mark.parametrize("command,expected", [
    ("grep -e -- -r secret .", "deny"),
    ("grep -e -- -R secret ~/city", "deny"),
    ("grep --regexp=-- -r secret .", "deny"),
    ("grep -e-- -rn secret .", "deny"),
    ("grep -e -- -d recurse secret .", "deny"),
    ("grep -f -- -r .", "deny"),
    ("rg -e -- secret .", "deny"),
    ("ls -- -R", "allow"),
    (f"grep -e -- secret {SAFE}/file.txt", "allow"),
    (f"grep -rn x {SAFE}", "allow"),
    (f"grep -e-r {SAFE}/file.txt", "allow"),
    (f"grep -eR {SAFE}/file.txt", "allow"),
    (f"grep --regexp -- {SAFE}/file.txt", "allow"),
    (f"grep -ne-- {SAFE}/file.txt", "allow"),
    (f"grep --color -e -- {SAFE}/file.txt", "allow"),
    (f"grep --color=never -e -- {SAFE}/file.txt", "allow"),
    ("grep -e -- -r", "deny"),
    ("grep --color -e -- -r", "deny"),
    (f"grep -e -- -r {SAFE}", "allow"),
    (f"rg -e -- {SAFE}", "allow"),
    (f"rg --regexp=-- {SAFE}", "allow"),
    (f"rg -e-- {SAFE}", "allow"),
    ("rg --regexp -- .", "deny"),
    ("find -- ~/city -name x", "deny"),
    (f"find -- {SAFE} -name file.txt", "allow"),
    ("find -name --", "deny"),
    ("find --", "deny"),
    ("tail -- -f", "allow"),
    ("ls -- --recursive", "allow"),
    ("ls ~/city/work", "allow"),
    ("ls -- ~/city/work", "deny"),
])
def test_option_boundaries_in_real_hook(tmp_path, read_city, command, expected):
    (read_city / "work/stone-videos-1027/file.txt").write_text("synthetic public fixture")
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


@pytest.mark.parametrize("exe,options", [
    ("grep", "-m 1 -A 1 -B1 -C1"),
    ("grep", "--max-count=1 --after-context 1 --before-context=1 --context 1"),
    ("grep", "--include -- --exclude -- --exclude-dir --"),
    ("grep", "--include=-- --exclude=-- --exclude-dir=--"),
    ("grep", "-d recurse"), ("grep", "--directories=recurse"),
    ("grep", "-drecurse"), ("grep", "-ndrecurse"),
    ("rg", "-g -- -t txt -A1 -B 1 -C1 -m 1 --max-depth 1"),
    ("rg", "--glob=-- --type txt --after-context=1 --before-context 1 --context=1 --max-count 1 --max-depth=1"),
    ("rg", "-E utf8"), ("rg", "--encoding=utf8"),
])
@pytest.mark.parametrize("target,expected", [(".", "deny"), (SAFE, "allow")])
def test_option_arguments_cannot_hide_directory_scope(tmp_path, read_city, exe, options, target, expected):
    command = f"{exe} {options} -e -- {target}"
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash", "tool_input": {"command": command}})
    assert result.returncode == 0
    assert json.loads(result.stdout)["hookSpecificOutput"]["permissionDecision"] == expected


@pytest.mark.parametrize("exe", ["ls", "du", "wc", "cat", "head", "tail", "stat"])
def test_literal_directory_after_terminator_still_confined(tmp_path, read_city, exe):
    # -R is an operand here. Its escaping symlink must be checked, even for ls.
    (read_city / "-R/private-link").symlink_to(read_city / "work/eval-kimi-k31", target_is_directory=True)
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash",
                             "tool_input": {"command": f"{exe} -- -R"}})
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == "deny"
    assert output["permissionDecisionReason"] == guard.SHAPE_REASON


@pytest.mark.parametrize("command", [
    f"{exe} --unknown {SAFE}" for exe in ("grep", "rg", "find", "ls", "du", "wc", "cat", "head", "tail", "stat")
] + [
    f"grep -e x -Zunknown {SAFE}", f"grep --regexp {SAFE} --unknown",
    f"grep -m -- -r {SAFE}", f"grep -A -- -r {SAFE}", f"grep -d -- -r {SAFE}",
    f"rg --max-depth -- {SAFE}", f"rg -m -- {SAFE}",
    f"grep --recursive=yes -e x {SAFE}", f"rg --files=yes {SAFE}",
    f"grep -f {SAFE}/file.txt {SAFE}", f"grep --file={SAFE}/file.txt {SAFE}",
    f"rg -f {SAFE}/file.txt {SAFE}", f"rg --file={SAFE}/file.txt {SAFE}",
    "grep -e", "grep --regexp", "grep -m", "rg -g", "rg --max-depth", "head -n", "tail -c",
    f"ls -RT 1 {SAFE}", f"ls -Rw 1 {SAFE}", f"ls -RI 1 {SAFE}",
    f"stat -f -- {SAFE}",
])
def test_unknown_missing_or_unsafe_options_fail_closed(tmp_path, read_city, command):
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash", "tool_input": {"command": command}})
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == "deny"
    assert output["permissionDecisionReason"] == guard.SHAPE_REASON


@pytest.mark.parametrize("command", [
    "grep -e -- -r secret .", "grep -e-- -rn secret .",
    "grep --regexp=-- -R secret .", "grep -e -- -d recurse secret .",
])
def test_real_grep_traversal_is_blocked_before_execution(tmp_path, read_city, command):
    # Execute only against synthetic fixtures to demonstrate what the guarded
    # command would otherwise disclose. No real excluded directory is read.
    marker = "-- synthetic excluded-directory marker"
    (read_city / "assets/private/fixture.txt").write_text(marker + "\n")
    result = subprocess.run(shlex.split(command), cwd=read_city, capture_output=True, text=True, timeout=5)
    assert marker in result.stdout
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash", "tool_input": {"command": command}})
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == "deny"
    assert output["permissionDecisionReason"] == guard.SHAPE_REASON
