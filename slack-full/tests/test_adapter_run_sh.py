"""Behavioral tests for adapter/run.sh — the [[service]] entrypoint.

run.sh is the pack's defense against pin-bump outages: the adapter
binary is a gitignored build artifact, and ``gc import install``
re-materializes the pack cache git-only, so a service command pointing
straight at the binary dies on every pin bump. run.sh is checked in and
rebuilds the binary on missing before exec'ing it.

These tests run the real script in a temp dir that mimics a fresh
git-only materialization, with a stub ``go`` toolchain on PATH so they
are fast and hermetic (no real compile, no network):

  * missing binary -> exactly one ``go build`` -> built binary exec'd,
  * existing gc-slack-adapter -> exec'd directly, no build (idempotency),
  * env file is sourced and reaches the adapter process,
  * missing env file warns but still starts,
  * pack.toml keeps pointing the service at a checked-in command.
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
# Stub Go toolchain: records every invocation (with its cwd, so tests
# can pin WHERE the build ran), and on `build` writes an executable at
# the -o target that prints a marker instead of compiling.
echo "go $* cwd=$PWD" >> "$GO_STUB_LOG"
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

    def run(
        env_file: pathlib.Path | None = None,
        extra_env: dict | None = None,
        drop_env: list[str] | None = None,
    ):
        env = dict(os.environ)
        env["PATH"] = f"{stub_bin}:{env['PATH']}"
        env["GO_STUB_LOG"] = str(go_log)
        env["GC_SLACK_ADAPTER_ENV"] = str(env_file or tmp_path / "no-such-env")
        env.update(extra_env or {})
        for key in drop_env or []:
            env.pop(key, None)
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
    # Exactly one build, of the package (".") FROM the adapter dir —
    # the colocated sources, not whatever cwd the supervisor used.
    builds = go_invocations()
    assert len(builds) == 1, builds
    assert " . cwd=" in builds[0], builds[0]
    build_cwd = builds[0].split(" cwd=", 1)[1]
    assert pathlib.Path(build_cwd).resolve() == adapter.resolve(), builds[0]
    # Binary published atomically at its canonical path, no temp left.
    assert (adapter / "gc-slack-adapter").exists()
    assert not list(adapter.glob("gc-slack-adapter.build.*"))


def test_existing_binary_skips_build(harness):
    adapter, run, go_invocations = harness
    prebuilt = adapter / "gc-slack-adapter"
    prebuilt.write_text("#!/usr/bin/env bash\necho PREBUILT_RAN\n")
    prebuilt.chmod(0o755)
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


def test_starts_without_home_set(harness):
    """Supervisor environments may not set HOME; under `set -u` an
    unguarded $HOME would abort before the fast path ever ran."""
    adapter, run, go_invocations = harness
    proc = run(drop_env=["HOME", "XDG_CONFIG_HOME", "GC_SLACK_ADAPTER_ENV"])
    assert proc.returncode == 0, proc.stderr
    assert "STUB_ADAPTER_RAN" in proc.stdout


def test_stale_build_temp_is_ignored(harness):
    """A crash mid-build (SIGKILL, host loss) can leave a PID-suffixed
    temp behind; it must never be exec'd, and a fresh start must still
    build and run the real binary."""
    adapter, run, go_invocations = harness
    stale = adapter / "gc-slack-adapter.build.99999"
    stale.write_text("#!/usr/bin/env bash\necho STALE_TEMP_RAN\n")
    stale.chmod(0o755)
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "STALE_TEMP_RAN" not in proc.stdout
    assert "STUB_ADAPTER_RAN" in proc.stdout
    assert len(go_invocations()) == 1


def test_pack_service_command_is_the_checked_in_run_sh():
    """The [[service]] command must stay a checked-in path — pointing it
    back at the gitignored binary reintroduces the stranded-service
    outage on the next pin bump."""
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
    # Not gitignored (check-ignore: 1 = not ignored; 0 = ignored;
    # anything else = git itself failed and proves nothing):
    ignored = subprocess.run(
        ["git", "check-ignore", str(rel)],
        cwd=PACK_DIR,
        capture_output=True,
        text=True,
    )
    assert ignored.returncode == 1, (
        f"{rel}: git check-ignore exited {ignored.returncode} "
        f"(0 means gitignored — service would strand): {ignored.stderr}"
    )
    # And actually tracked, so a materialization really ships it:
    tracked = subprocess.run(
        ["git", "ls-files", "--error-unmatch", str(rel)],
        cwd=PACK_DIR,
        capture_output=True,
        text=True,
    )
    assert tracked.returncode == 0, f"{rel} is not tracked by git: {tracked.stderr}"
