"""Run from gascity/ to check the gate's deadline and process-group cleanup."""

import importlib.util
import os
from pathlib import Path
import tempfile
import time
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("shim_tests", Path("tests/test_toolchain_shims.py"))
m = importlib.util.module_from_spec(spec)
spec.loader.exec_module(m)
with tempfile.TemporaryDirectory() as root:
    fx = m.Fixture(Path(root))
    project = fx.project()
    try:
        with fx.start("pnpm", "exec", "vitest", cwd=project, hold="install") as call:
            call.wait_for_gate()
            group = call.proc.pid
            with patch.object(m.time, "monotonic", side_effect=[0, 61]):
                call.wait_for_wait()
    except AssertionError as error:
        assert "lane-install wait message within 60s" in str(error)
    else:
        raise AssertionError("missing wait message did not fail")
    deadline = time.monotonic() + 5
    while True:
        try:
            os.killpg(group, 0)
        except ProcessLookupError:
            break
        assert time.monotonic() < deadline, "caller group survived cleanup"
        time.sleep(0.02)
    print("PASS: missing wait message reports the 60s bound; entire held caller group is gone")
