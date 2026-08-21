"""Behavioral tests for adapter/run.sh — the [[service]] entrypoint.

run.sh is the pack's defense against gp-d7l-class outages: the adapter
binary is a gitignored build artifact, and ``gc import install``
re-materializes the pack cache git-only, so a service command pointing
straight at the binary dies on every pin bump. run.sh is checked in and
rebuilds the binary on missing before exec'ing it.

These tests run the real script in a temp dir that mimics a fresh
git-only materialization, with a stub ``go`` toolchain on PATH so they
are fast and hermetic (no real compile, no network):

  * missing binary -> exactly one ``go build`` -> built binary exec'd,
  * existing gc-slack-adapter.real -> exec'd directly, no build
    (idempotency),
  * dev-checkout ``go build -o gc-slack-adapter`` output -> exec'd,
  * legacy env-wrapper shim (starts "#!") without .real -> rebuilt, not
    exec'd (the shim would bounce into the missing .real),
  * env file is sourced and reaches the adapter process.
"""

from __future__ import annotations

import os
import pathlib
import shutil
import subprocess

import pytest

PACK_DIR = pathlib.Path(__file__).resolve().parent.parent
RUN_SH = PACK_DIR / "adapter" / "run.sh"

GO_STUB = """#!/usr/bin/env bash
# Stub Go toolchain: records every invocation, and on `build` writes an
# executable at the -o target that prints a marker instead of compiling.
echo "go $*" >> "$GO_STUB_LOG"
if [ "$1" = "version" ]; then
  echo "go version go0.0-stub"
  exit 0
fi
if [ "$1" = "build" ]; then
  out=""
  prev=""
  for a in "$@"; do
    [ "$prev" = "-o" ] && out="$a"
    prev="$a"
  done
  [ -n "$out" ] || { echo "go stub: no -o target" >&2; exit 2; }
  printf '%s\\n' '#!/usr/bin/env bash' \\
    'echo "STUB_ADAPTER_RAN pwd=$PWD MARKER_VAR=${MARKER_VAR:-unset}"' > "$out"
  chmod +x "$out"
  exit 0
fi
echo "go stub: unexpected invocation: $*" >&2
exit 2
"""


@pytest.fixture()
def harness(tmp_path: pathlib.Path):
    """Adapter dir holding only run.sh (a git-only materialization) plus
    a stub `go` prepended to PATH."""
    adapter = tmp_path / "adapter"
    adapter.mkdir()
    shutil.copy(RUN_SH, adapter / "run.sh")
    (adapter / "run.sh").chmod(0o755)

    stub_bin = tmp_path / "stubbin"
    stub_bin.mkdir()
    go = stub_bin / "go"
    go.write_text(GO_STUB)
    go.chmod(0o755)

    go_log = tmp_path / "go-invocations.log"

    def run(env_file: pathlib.Path | None = None, extra_env: dict | None = None):
        env = dict(os.environ)
        env["PATH"] = f"{stub_bin}:{env['PATH']}"
        env["GO_STUB_LOG"] = str(go_log)
        env["GC_SLACK_ADAPTER_ENV"] = str(env_file or tmp_path / "no-such-env")
        env.update(extra_env or {})
        return subprocess.run(
            [str(adapter / "run.sh")],
            capture_output=True,
            text=True,
            env=env,
            timeout=30,
        )

    def go_invocations() -> list[str]:
        if not go_log.exists():
            return []
        return [l for l in go_log.read_text().splitlines() if l.startswith("go build")]

    return adapter, run, go_invocations


def test_missing_binary_is_rebuilt_and_execd(harness):
    adapter, run, go_invocations = harness
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "STUB_ADAPTER_RAN" in proc.stdout
    # Loud logging: the self-heal explains itself on stderr.
    assert "rebuilding from source" in proc.stderr
    assert "gc import install" in proc.stderr
    # Exactly one build, of the package in the adapter dir.
    builds = go_invocations()
    assert len(builds) == 1, builds
    assert builds[0].endswith(" .")
    # Canonical binary published atomically at the .real path, no temp left.
    assert (adapter / "gc-slack-adapter.real").exists()
    assert not list(adapter.glob("gc-slack-adapter.real.build.*"))


def test_existing_real_binary_skips_build(harness):
    adapter, run, go_invocations = harness
    real = adapter / "gc-slack-adapter.real"
    real.write_text("#!/usr/bin/env bash\necho PREBUILT_RAN\n")
    real.chmod(0o755)
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "PREBUILT_RAN" in proc.stdout
    assert go_invocations() == []


def test_self_heal_is_idempotent_across_restarts(harness):
    adapter, run, go_invocations = harness
    first = run()
    second = run()
    assert first.returncode == 0 and second.returncode == 0
    assert "STUB_ADAPTER_RAN" in second.stdout
    # Only the first start built; the restart exec'd the existing binary.
    assert len(go_invocations()) == 1


def test_dev_checkout_binary_is_execd(harness):
    adapter, run, go_invocations = harness
    # `go build -o gc-slack-adapter .` output: an executable that does NOT
    # start with "#!" (the shell's ENOEXEC fallback still runs this text
    # file, standing in for a real Mach-O/ELF binary).
    legacy = adapter / "gc-slack-adapter"
    legacy.write_text("echo DEV_BINARY_RAN\n")
    legacy.chmod(0o755)
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "DEV_BINARY_RAN" in proc.stdout
    assert go_invocations() == []


def test_legacy_shim_without_real_falls_through_to_build(harness):
    adapter, run, go_invocations = harness
    # The pre-gp-d7l deployment pattern: gc-slack-adapter is an env-loading
    # wrapper shim exec'ing gc-slack-adapter.real. With .real missing the
    # shim is a trap — run.sh must rebuild instead of exec'ing it.
    shim = adapter / "gc-slack-adapter"
    shim.write_text('#!/bin/bash\nexec "$(dirname "$0")/gc-slack-adapter.real" "$@"\n')
    shim.chmod(0o755)
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "STUB_ADAPTER_RAN" in proc.stdout
    assert len(go_invocations()) == 1


def test_env_file_is_sourced_into_adapter_env(harness):
    adapter, run, go_invocations = harness
    env_file = adapter.parent / "envfile"
    env_file.write_text("MARKER_VAR=from-env-file\n")
    proc = run(env_file=env_file)
    assert proc.returncode == 0, proc.stderr
    assert "MARKER_VAR=from-env-file" in proc.stdout
    # And no missing-env-file warning when the file exists.
    assert "env file not found" not in proc.stderr


def test_missing_env_file_warns_but_still_starts(harness):
    adapter, run, go_invocations = harness
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "env file not found" in proc.stderr
    assert "STUB_ADAPTER_RAN" in proc.stdout


def test_pack_service_command_is_the_checked_in_run_sh():
    """The [[service]] command must stay a checked-in path — pointing it
    back at the gitignored binary reintroduces the gp-d7l outage."""
    try:
        import tomllib
    except ModuleNotFoundError:  # pragma: no cover - py<3.11
        pytest.skip("tomllib unavailable")
    with open(PACK_DIR / "pack.toml", "rb") as fh:
        pack = tomllib.load(fh)
    services = {s["name"]: s for s in pack.get("service", [])}
    assert "slack" in services, "slack [[service]] block missing from pack.toml"
    command = services["slack"]["process"]["command"]
    assert command == ["./adapter/run.sh"], command
    rel = pathlib.Path(command[0])
    target = PACK_DIR / rel
    assert target.exists() and os.access(target, os.X_OK)
    # Checked in, not gitignored:
    ignored = subprocess.run(
        ["git", "check-ignore", str(rel)],
        cwd=PACK_DIR,
        capture_output=True,
        text=True,
    )
    assert ignored.returncode != 0, f"{rel} is gitignored — service would strand"
