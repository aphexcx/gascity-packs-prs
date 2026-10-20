"""Exercise the operator CLI wrappers against a git-only fixture pack."""

from __future__ import annotations

import os
import pathlib
import shutil
import subprocess

import pytest

PACK_DIR = pathlib.Path(__file__).resolve().parent.parent
VERBS = (
    "import-app",
    "map-channel",
    "map-rig",
    "sync-commands",
    "enable-room-launch",
    "post-message",
)
ARGS = ("--marker", "two words", "", "*.json")
GO_STUB = """#!/bin/sh
set -eu
printf '%s\\n' "$PWD" "$@" > "$GO_STUB_LOG"
[ -f go.mod ] && [ -f main.go ]
[ "$1" = build ] && [ "$2" = -o ] && [ "$4" = . ]
# Go accepts an empty mktemp output, but refuses to overwrite a script.
if [ -s "$3" ] && [ "$(head -c 2 "$3")" = '#!' ]; then
  echo "$3 already exists and is not an object file" >&2
  exit 1
fi
# Like Go, create missing output parents, including for a relative -o path.
case "$3" in
  */*) /bin/mkdir -p "${3%/*}" ;;
esac
if [ "${GO_STUB_FAIL:-}" = 1 ]; then
  printf 'partial build output' > "$3"
  echo 'simulated compile failure' >&2
  exit 23
fi
printf '%s\\n' '#!/bin/sh' 'printf "<%s>\\n" "$@"' \
  'exit "${CLI_STUB_EXIT:-0}"' > "$3"
/bin/chmod +x "$3"
"""


@pytest.fixture()
def harness(tmp_path):
    pack = tmp_path / "fixture pack"
    shutil.copytree(PACK_DIR / "commands", pack / "commands")
    (pack / "cli").mkdir()
    for name in ("go.mod", "main.go"):
        shutil.copyfile(PACK_DIR / "cli" / name, pack / "cli" / name)
    stub_bin = tmp_path / "stubbin"
    stub_bin.mkdir()
    # A controlled PATH makes "Go absent" independent of the host toolchain.
    for name in ("head", "chmod", "mktemp", "mv", "rm"):
        (stub_bin / name).symlink_to(shutil.which(name))
    go = stub_bin / "go"
    go.write_text(GO_STUB)
    go.chmod(0o755)
    go_log = tmp_path / "go.log"

    def run(verb="post-message", extra_env=None):
        env = {
            "PATH": str(stub_bin),
            "GC_PACK_DIR": str(pack),
            "GO_STUB_LOG": str(go_log),
        }
        env.update(extra_env or {})
        return subprocess.run(
            [str(pack / "commands" / f"{verb}.sh"), *ARGS],
            env=env,
            cwd=tmp_path,
            capture_output=True,
            text=True,
            timeout=10,
        )

    return pack, stub_bin, go_log, run


def assert_temporary_build(pack, go_log):
    cwd, command, output_flag, output, package = go_log.read_text().splitlines()
    assert (cwd, command, output_flag, package) == (
        str(pack / "cli"), "build", "-o", "."
    )
    target = pathlib.Path(output)
    if not target.is_absolute():
        target = pathlib.Path(cwd) / target
    assert target.parent == pack / "cli"
    assert target.name.startswith(".gc-slack-cli.build.")
    assert not target.exists()
    assert not list((pack / "cli").glob(".gc-slack-cli.build.*"))


@pytest.mark.parametrize("verb", VERBS)
def test_cli_missing_binary_builds_and_execs(harness, verb):
    pack, _, go_log, run = harness
    proc = run(verb, {"CLI_STUB_EXIT": "17"})
    assert proc.returncode == 17, proc.stderr
    assert proc.stdout.splitlines() == [f"<{arg}>" for arg in (verb, *ARGS)]
    assert_temporary_build(pack, go_log)
    assert os.access(pack / "cli" / "gc-slack-cli", os.X_OK)
    assert "binary missing at " in proc.stderr
    assert "rebuilding from source" in proc.stderr


@pytest.mark.parametrize("verb", VERBS)
def test_cli_existing_binary_execs_without_go(harness, verb):
    pack, stub_bin, go_log, run = harness
    # A real native executable: a #! script belongs to the legacy-shim case.
    (pack / "cli" / "gc-slack-cli").symlink_to(shutil.which("echo"))
    (stub_bin / "go").unlink()
    proc = run(verb)
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout == " ".join((verb, *ARGS)) + "\n"
    assert proc.stderr == ""
    assert not go_log.exists()


def test_cli_legacy_env_wrapper_is_rebuilt(harness):
    pack, _, go_log, run = harness
    binary = pack / "cli" / "gc-slack-cli"
    binary.write_text("#!/bin/sh\necho LEGACY_WRAPPER_RAN\nexit 99\n")
    binary.chmod(0o755)
    proc = run()
    assert proc.returncode == 0, proc.stderr
    assert "LEGACY_WRAPPER_RAN" not in proc.stdout
    assert proc.stdout.splitlines() == [f"<{arg}>" for arg in ("post-message", *ARGS)]
    assert go_log.exists()
    assert_temporary_build(pack, go_log)
    assert "rebuilding from source" in proc.stderr


def test_cli_missing_go_names_manual_build(harness):
    pack, stub_bin, go_log, run = harness
    (stub_bin / "go").unlink()
    proc = run()
    assert proc.returncode != 0
    assert "no Go toolchain found" in proc.stderr
    assert "manual fix:" in proc.stderr
    assert str(pack / "cli") in proc.stderr
    assert "mktemp .gc-slack-cli.build.XXXXXX" in proc.stderr
    assert 'go build -o "$tmp" . && mv -f "$tmp" gc-slack-cli' in proc.stderr
    assert not go_log.exists()
    assert proc.stdout == ""


def test_cli_failed_binary_inspection_never_execs_legacy_wrapper(harness):
    pack, stub_bin, go_log, run = harness
    binary = pack / "cli" / "gc-slack-cli"
    binary.write_text("#!/bin/sh\necho LEGACY_WRAPPER_RAN\nexit 99\n")
    binary.chmod(0o755)
    (stub_bin / "head").unlink()
    (stub_bin / "go").unlink()
    proc = run()
    assert proc.returncode != 0
    assert "LEGACY_WRAPPER_RAN" not in proc.stdout
    assert "no Go toolchain found" in proc.stderr
    assert "manual fix:" in proc.stderr
    assert not go_log.exists()


@pytest.mark.parametrize("legacy_wrapper", (False, True), ids=("missing", "legacy"))
def test_cli_build_failure_cleans_temp_and_preserves_wrapper(harness, legacy_wrapper):
    pack, _, go_log, run = harness
    binary = pack / "cli" / "gc-slack-cli"
    original = b"#!/bin/sh\necho LEGACY_WRAPPER_RAN\nexit 99\n"
    if legacy_wrapper:
        binary.write_bytes(original)
        binary.chmod(0o755)
    proc = run(extra_env={"GO_STUB_FAIL": "1"})
    assert proc.returncode != 0
    assert "simulated compile failure" in proc.stderr
    assert "go build failed" in proc.stderr
    assert "manual fix:" in proc.stderr
    assert 'go build -o "$tmp" . && mv -f "$tmp" gc-slack-cli' in proc.stderr
    assert_temporary_build(pack, go_log)
    if legacy_wrapper:
        assert binary.read_bytes() == original
        assert binary.stat().st_mode & 0o777 == 0o755
    else:
        assert not binary.exists()
    assert proc.stdout == ""


def test_cli_manual_build_replaces_legacy_wrapper(harness):
    pack, stub_bin, go_log, run = harness
    binary = pack / "cli" / "gc-slack-cli"
    original = b"#!/bin/sh\necho LEGACY_WRAPPER_RAN\nexit 99\n"
    binary.write_bytes(original)
    binary.chmod(0o755)
    failed = run(extra_env={"GO_STUB_FAIL": "1"})
    assert failed.returncode != 0
    remedy = failed.stderr.split("manual fix: ", 1)[1].splitlines()[0]
    repaired = subprocess.run(
        ["/bin/sh", "-c", remedy],
        env={
            "PATH": str(stub_bin),
            "GO_STUB_LOG": str(go_log),
        },
        capture_output=True,
        text=True,
        timeout=10,
    )
    assert repaired.returncode == 0, repaired.stderr
    assert binary.read_bytes() != original
    assert os.access(binary, os.X_OK)
    assert_temporary_build(pack, go_log)


def test_cli_go_on_relative_path_survives_build_cd(harness):
    _, _, go_log, run = harness
    proc = run(extra_env={"PATH": "stubbin"})
    assert proc.returncode == 0, proc.stderr
    assert go_log.exists()


def test_cli_relative_pack_dir_builds_without_stray_output(harness):
    pack, _, go_log, run = harness
    before = set(pack.rglob("*"))
    # The harness runs from pack.parent, outside both the pack and cli/.
    proc = run(extra_env={"GC_PACK_DIR": pack.name})
    outputs = {
        str(path.relative_to(pack)): path.stat().st_size
        for path in (pack / "cli").rglob("*") if path.is_file()
    }
    assert proc.returncode == 0, f"{proc.stderr}\nCLI files: {outputs}"
    assert proc.stdout.splitlines() == [f"<{arg}>" for arg in ("post-message", *ARGS)]
    binary = pack / "cli" / "gc-slack-cli"
    assert os.access(binary, os.X_OK)
    assert binary.stat().st_size > 0
    assert_temporary_build(pack, go_log)
    assert set(pack.rglob("*")) == before | {binary}


@pytest.mark.parametrize("verb", VERBS)
def test_cli_wrapper_requires_pack_context(harness, verb):
    _, _, go_log, run = harness
    proc = run(verb, {"GC_PACK_DIR": ""})
    assert proc.returncode != 0
    assert "missing Gas City pack context" in proc.stderr
    assert not go_log.exists()


def test_cli_build_heals_doctor_binaries_row(harness):
    pack, _, _, run = harness
    (pack / "adapter").mkdir()
    (pack / "adapter" / "gc-slack-adapter").symlink_to(shutil.which("echo"))

    def doctor():
        return subprocess.run(
            [str(PACK_DIR / "doctor" / "check-binaries.sh")],
            env={"GC_PACK_DIR": str(pack), "PATH": "/usr/bin:/bin"},
            capture_output=True,
            text=True,
            timeout=10,
        )

    before = doctor()
    assert before.returncode == 2
    assert "pack binaries not built: cli/gc-slack-cli" in before.stdout
    assert "gc slack" in before.stdout
    assert "builds itself" in before.stdout
    assert "gc slack map-channel with no arguments" in before.stdout
    assert "go build -o gc-slack-cli ." not in before.stdout
    assert run().returncode == 0
    after = doctor()
    assert after.returncode == 0
    assert after.stdout.strip() == "adapter and CLI binaries built"
