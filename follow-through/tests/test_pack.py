"""Exercise the installed surfaces without sending to a real city."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tomllib

PACK = Path(__file__).resolve().parents[1]
ROOT = PACK.parent


def test_node_behavior_suite():
    node = shutil.which("node")
    assert node, "follow-through requires Node.js 20 or newer"
    result = subprocess.run([node, "--test"], cwd=PACK / "test", capture_output=True, text=True, timeout=120)
    assert result.returncode == 0, result.stdout + result.stderr


def test_pack_has_only_local_commands_order_and_doctor(tmp_path):
    manifest = tomllib.loads((PACK / "pack.toml").read_text())
    assert manifest == {"pack": {"name": "follow-through", "version": "0.0.1", "schema": 2}}
    order = tomllib.loads((PACK / "orders/follow-through-tick.toml").read_text())["order"]
    assert order["trigger"] == "cooldown"
    assert order["interval"] == "1m"
    assert order["exec"] == "$ORDER_DIR/scripts/follow-through-tick.sh"
    commands = {p.parent.name for p in (PACK / "commands").glob("*/command.toml")}
    assert commands == {"consent", "hours", "add", "list", "done", "drop", "remind", "status"}
    state_dir = tmp_path / "city/.gc/runtime/packs/follow-through"
    env = {**os.environ, "GC_PACK_STATE_DIR": str(state_dir), "GC_BIN": "/no-real-gc-allowed"}
    # Commands and the order exercise the actual wrappers over one state file.
    subprocess.run([str(PACK / "commands/consent.sh"), "slack/example", "--off"], env=env, check=True, capture_output=True)
    subprocess.run([str(PACK / "orders/scripts/follow-through-tick.sh")], env=env, check=True, capture_output=True)
    result = subprocess.run([str(PACK / "commands/status.sh"), "--json"], env=env, check=True, capture_output=True, text=True)
    snapshot = json.loads(result.stdout)
    assert snapshot["owners"] == 1
    assert snapshot["last_tick_at"] is not None
    assert (state_dir / "state.json").exists()
    result = subprocess.run([str(PACK / "doctor/check.sh")], env=env, capture_output=True, text=True)
    assert result.returncode == 0, result.stdout + result.stderr


def test_20_python_suite_is_in_make_and_ci():
    assert "follow-through/tests" in (ROOT / "Makefile").read_text().split("PYTEST_DIRS ?= ", 1)[1].splitlines()[0]
    assert any("python3 -m pytest " in line and "follow-through/tests" in line for line in (ROOT / ".github/workflows/ci.yml").read_text().splitlines())


def test_readme_explains_delivery_and_privacy():
    text = (PACK / "README.md").read_text()
    for fact in ["minute and a half", "turn ends", "GC_PACK_STATE_DIR", "nothing leaves the box", "120 seconds"]:
        assert fact in text
