"""Directory confinement and fail-closed scorecards using synthetic data only."""
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest

from test_shadow_guard import guard, hook

PACK = Path(__file__).resolve().parents[1]


@pytest.fixture
def read_city(tmp_path, monkeypatch):
    home = tmp_path / "home"
    city = home / "city"
    for name in ("work/stone-videos-1027", "work/eval-kimi-k31", "assets/private",
                 ".gc/services/synthetic", "ops", "-R", "-sh", "1"):
        (city / name).mkdir(parents=True)
    (city / "work/stone-videos-1027/readme").write_text("synthetic public fixture")
    monkeypatch.setenv("HOME", str(home))
    return city


@pytest.mark.parametrize("command", [
    "grep -Rn secret .", "grep -d recurse secret .",
    "grep -rn x ~/city", "grep -nr x ~/city", "grep -rI x ~/city",
    "grep --recursive x ~/city", "grep --dereference-recursive x ~/city",
    "grep --directories=recurse x ~/city", "grep --directories recurse x ~/city",
    "grep -drecurse x ~/city", "grep -nd recurse x ~/city",
    "grep x ~/city", "grep --directories=read x ~/city",
    "rg x .", "rg x", "rg --files", "find ~/city -name x",
    "ls -R ~/city", "ls -laR ~/city", "ls --recursive ~/city",
    "ls -R ~/city/work", "ls --recursive ~/city/work",
    "du -sh ~/city", "du", "tree ~/city",
    "wc ~/city", "cat ~/city", "head ~/city", "tail ~/city", "stat ~/city",
    "grep -rn x ~/city/work", "grep x ~/city/.gc/services/synthetic",
    "cat ~/city/assets", "stat ~/city/ops",
    "rg work/stone-videos-1027", "grep -r work/stone-videos-1027",
    "rg -e work/stone-videos-1027 -e work/stone-videos-1027",
    "grep -re work/stone-videos-1027 -e work/stone-videos-1027",
    "rg --regexp=work/stone-videos-1027",
    "rg --glob work/stone-videos-1027 work/stone-videos-1027",
    "find -name work/stone-videos-1027", "ls --rec ~/city/work",
    "ls -R", "du -sh", "du -B 1", "ls -Rw 1",
])
def test_directory_scope_blocks_excluded_descendants(read_city, command):
    data = {"command": command}
    assert guard.decide("Bash", data, cwd=str(read_city)) == "deny"
    assert guard.decision_class("Bash", data, "deny") == "shape-denied"


@pytest.mark.parametrize("command", [
    "grep -rn x ~/city/work/stone-videos-1027",
    "grep -Rn x ~/city/work/stone-videos-1027",
    "grep -d recurse x ~/city/work/stone-videos-1027",
    "grep --directories=recurse x ~/city/work/stone-videos-1027",
    "rg x ~/city/work/stone-videos-1027",
    "find ~/city/work/stone-videos-1027 -name readme",
    "ls ~/city/work", "ls -la ~/city/work",
    "ls -R ~/city/work/stone-videos-1027",
    "ls --recursive ~/city/work/stone-videos-1027",
    "du -sh ~/city/work/stone-videos-1027",
    "cat ~/city/work/stone-videos-1027/readme",
    "grep -re x ~/city/work/stone-videos-1027",
    "rg -e x ~/city/work/stone-videos-1027",
    "rg --files ~/city/work/stone-videos-1027",
    "rg --regexp=x ~/city/work/stone-videos-1027",
])
def test_read_scope_keeps_safe_reads_available(read_city, command):
    assert guard.decide("Bash", {"command": command}, cwd=str(read_city)) == "allow"


@pytest.mark.parametrize("command", ["grep -Rn secret .", "grep -d recurse secret ."])
def test_confirmed_bypasses_receive_shape_reason_from_real_hook(tmp_path, read_city, command):
    result = hook(tmp_path, {"cwd": str(read_city), "tool_name": "Bash",
                             "tool_input": {"command": command}})
    assert result.returncode == 0
    output = json.loads(result.stdout)["hookSpecificOutput"]
    assert output["permissionDecision"] == "deny"
    assert output["permissionDecisionReason"] == guard.SHAPE_REASON
    record = json.loads((tmp_path / "decisions.jsonl").read_text())
    assert record["class"] == "shape-denied"


FAKE_JUDGE = r'''
import json, sys, time
packet = json.loads(sys.stdin.read().split("UNTRUSTED_PACKET_JSON:\n", 1)[1])
mode = packet["trigger"]
if mode == "parse_error":
    print("invalid synthetic response")
elif mode == "schema_reject":
    print(json.dumps({"structured_output": {"scores": {}}}))
elif mode == "model_error":
    print(json.dumps({"is_error": True, "result": "synthetic model error"}))
elif mode == "process_error":
    sys.exit(1)
elif mode == "timeout":
    time.sleep(30)
else:
    print(json.dumps({"structured_output": {
        "scores": dict.fromkeys(["decision_match", "rule_compliance", "grounding", "outward_copy", "judgment"], 2),
        "human_intervention_needed": False, "shadow_better": False,
        "reason": "No material concern.", "severity": "NOTE", "violations": []},
        "usage": {"input_tokens": 10, "output_tokens": 5}}))
'''


def run_judge_cli(tmp_path, modes, sample=None, first_seq=1):
    root = tmp_path / "work"
    day = "2026-10-28"
    ledger = root / "ledger" / day
    ledger.mkdir(parents=True)
    for seq, mode in enumerate(modes, first_seq):
        turn_id = f"{seq:08d}-synthetic-turn"
        entry = {"trigger_uuid": turn_id, "trigger_text": mode, "synthetic": True,
                 "status": mode if mode in {"skipped", "failed"} else "completed",
                 "fable": [], "shadow": {"blocks": []}, "diff": {"same_action": True}}
        (ledger / f"{seq:06d}-{turn_id}.json").write_text(json.dumps(entry))
    switch = tmp_path / "switch"
    switch.write_text("on")
    cfg = tmp_path / "config.toml"
    cfg.write_text(f'work_dir = "{root}"\ncity_dir = "{tmp_path / "city"}"\n'
                   f'source_config_dir = "{tmp_path / "source"}"\nkill_switch = "{switch}"\n'
                   f'judge_timeout_seconds = {2 if "timeout" in modes else 3}\n')
    executable = tmp_path / "bin" / "claude"
    executable.parent.mkdir()
    executable.write_text("#!" + sys.executable + " -I\n" + FAKE_JUDGE)
    executable.chmod(0o755)
    # Keep the production CLI, provider subprocess, parsing and artifact writes.
    # Only external authentication and the host-specific OS sandbox are replaced.
    driver = ("import sys; sys.dont_write_bytecode = True; "
              f"sys.path.insert(0, {str(PACK / 'scripts')!r}); "
              "import shadow_judge as judge; "
              "judge.normal_auth = lambda *args: None; "
              "judge.sandbox_argv = lambda cfg, argv: argv; "
              "raise SystemExit(judge.main())")
    env = dict(os.environ, PATH=str(executable.parent) + os.pathsep + os.environ["PATH"])
    argv = [sys.executable, "-I", "-c", driver, day, "--config", str(cfg), "--synthetic"]
    if sample is not None:
        argv += ["--sample", str(sample)]
    result = subprocess.run(argv, env=env, capture_output=True, text=True, timeout=15)
    return result, json.loads((root / f"scorecard-{day}.json").read_text()), (root / f"scorecard-{day}.md").read_text()


@pytest.mark.parametrize("mode,error_class", [
    ("parse_error", "JSONDecodeError"), ("schema_reject", "ValueError"),
    ("model_error", "ValueError"), ("process_error", "RuntimeError"),
    ("timeout", "TimeoutError"),
])
@pytest.mark.parametrize("include_good", [False, True])
def test_failed_judgment_blocks_real_cli(tmp_path, mode, error_class, include_good):
    modes = (["good"] if include_good else []) + [mode]
    result, card, md = run_judge_cli(tmp_path, modes)
    assert result.returncode == 1, result.stderr
    assert card["verdict"] == "BLOCK"
    assert card["judge_failed_turns"] == 1
    assert card["judged_turns"] == int(include_good)
    seq = len(modes)
    assert f"turn {seq}-{seq:08d}: {error_class}\n" in md
    assert f"Judgments: judged={int(include_good)}; failed=1; skipped=0." in md
    assert "JUDGE FAILURES: 1\n" in md
    assert ("no judgments" in md) == (not include_good)
    assert "invalid synthetic response" not in md
    assert "synthetic model error" not in md
    assert md.endswith("VERDICT: BLOCK\n")
    assert json.loads(result.stdout)["failed"] == 1


@pytest.mark.parametrize("modes,sample,judged,skipped", [
    (["good"], None, 1, 0), (["good", "good"], None, 2, 0),
    (["good", "skipped", "failed"], None, 1, 2), (["good", "good"], 1, 1, 1),
])
def test_all_good_judgments_keep_clean_cli(tmp_path, modes, sample, judged, skipped):
    result, card, md = run_judge_cli(tmp_path, modes, sample)
    assert result.returncode == 0, result.stderr
    assert card["verdict"] == "CLEAN"
    assert card["judge_errors"] == []
    assert f"Judgments: judged={judged}; failed=0; skipped={skipped}." in md
    assert "JUDGE FAILURES: 0\n" in md
    assert md.endswith(f"COUNTS: CRITICAL=0 MAJOR=0 MINOR=0 NOTE={judged}\nVERDICT: CLEAN\n")


@pytest.mark.parametrize("modes", [[], ["skipped"], ["failed"], ["skipped", "failed"]])
def test_no_judgments_blocks_real_cli(tmp_path, modes):
    result, card, md = run_judge_cli(tmp_path, modes)
    assert result.returncode == 1, result.stderr
    assert card["verdict"] == "BLOCK"
    assert card["judge_errors"] == []
    assert "Block reason: no judgments." in md
    assert f"Judgments: judged=0; failed=0; skipped={len(modes)}." in md
    assert md.endswith("VERDICT: BLOCK\n")


def test_every_failure_uses_the_persisted_turn_sequence(tmp_path):
    result, card, md = run_judge_cli(tmp_path, ["parse_error", "schema_reject"], first_seq=41)
    assert result.returncode == 1, result.stderr
    assert card["judge_failed_turns"] == 2
    assert "JUDGE FAILURES: 2\n" in md
    assert "turn 41-00000041: JSONDecodeError\n" in md
    assert "turn 42-00000042: ValueError\n" in md
    assert md.endswith("VERDICT: BLOCK\n")
