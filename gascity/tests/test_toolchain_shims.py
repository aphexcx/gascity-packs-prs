"""Contract tests for gascity/assets/scripts/toolchain/{pnpm,node}.

Each test installs the wrappers into a throwaway shim directory the way the
README recipe does (`node` once, `npm` and `npx` as links to it), points
GC_TOOLCHAIN_DIR at a throwaway toolchain, and drives them with fake
programs: a fake pnpm that records every call and can sleep or fail on
`install`, fake node trees, and a fake nodejs.org served over file://.
Nothing touches the network or the machine's toolchain.
"""

from __future__ import annotations

import hashlib
import io
import os
import pathlib
import platform
import shutil
import stat
import subprocess
import tarfile
import tempfile
import threading
import time
import unittest

TOOLCHAIN = pathlib.Path(__file__).resolve().parents[1] / "assets" / "scripts" / "toolchain"
PNPM_SHIM = TOOLCHAIN / "pnpm"
NODE_SHIM = TOOLCHAIN / "node"

FAKE_PNPM = r"""#!/bin/sh
# records: <cwd>|<verify env>|<argv...>
printf '%s|%s|%s\n' "$(pwd -P)" "${pnpm_config_verify_deps_before_run-unset}" "$*" >> "$FAKE_PNPM_LOG"
printf 'PATH0=%s\n' "${PATH%%:*}" >> "$FAKE_PNPM_LOG"
if [ "${1-}" = "install" ]; then
    [ -z "${FAKE_PNPM_SLEEP-}" ] || sleep "$FAKE_PNPM_SLEEP"
    if [ "${FAKE_PNPM_FAIL_INSTALL-}" = "1" ]; then
        echo "fake pnpm: install failed" >&2
        exit 7
    fi
    mkdir -p node_modules && : > node_modules/.fake-installed
    echo "fake pnpm: installed"
    exit 0
fi
echo "fake pnpm ran: $*"
"""


def sha256(path: pathlib.Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_exec(path: pathlib.Path, text: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)


class Fixture:
    """A shim dir with the wrappers, a toolchain dir, and a PATH with fakes."""

    def __init__(self, root: pathlib.Path) -> None:
        self.root = root
        self.shims = root / "shims"
        self.toolchain = root / "toolchain"
        self.bin = root / "bin"  # the "machine" PATH: fake node/npm, plus system tools
        self.shims.mkdir()
        self.toolchain.mkdir()
        self.bin.mkdir()
        shutil.copy2(PNPM_SHIM, self.shims / "pnpm")
        shutil.copy2(NODE_SHIM, self.shims / "node")
        (self.shims / "npm").symlink_to("node")
        (self.shims / "npx").symlink_to("node")
        for name in ("node", "npm", "npx"):
            write_exec(self.bin / name, f'#!/bin/sh\necho "machine {name} $*"\n')
        # fake pinned pnpm at the place the shim installs it
        self.fake_pnpm = self.toolchain / "pnpm" / "node_modules" / ".bin" / "pnpm"
        write_exec(self.fake_pnpm, FAKE_PNPM)
        self.log = root / "pnpm-calls.log"

    def env(self, **extra: str) -> dict[str, str]:
        env = {
            "PATH": f"{self.bin}:/usr/bin:/bin:/usr/sbin:/sbin",
            "HOME": str(self.root),
            "GC_TOOLCHAIN_DIR": str(self.toolchain),
            "FAKE_PNPM_LOG": str(self.log),
        }
        env.update(extra)
        return env

    def run(self, prog: str, *args: str, cwd: pathlib.Path | None = None, **extra: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [str(self.shims / prog), *args],
            cwd=str(cwd or self.root),
            env=self.env(**extra),
            capture_output=True,
            text=True,
        )

    def calls(self) -> list[str]:
        if not self.log.exists():
            return []
        return [line for line in self.log.read_text(encoding="utf-8").splitlines() if not line.startswith("PATH0=")]

    def installs(self) -> list[str]:
        return [c for c in self.calls() if c.split("|", 2)[2].startswith("install ")]

    def project(self, name: str = "proj", lock: str = "lockfileVersion: '9.0'\n") -> pathlib.Path:
        p = self.root / name
        p.mkdir()
        (p / "package.json").write_text('{"name":"p","private":true}\n', encoding="utf-8")
        (p / "pnpm-lock.yaml").write_text(lock, encoding="utf-8")
        return p


class PnpmShimTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self.tmp.name))

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_pnpm_own_dependency_check_is_off_and_argv_reaches_pnpm(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", "run", "--reporter=dot", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        calls = self.fx.calls()
        self.assertEqual(calls[-1].split("|", 2)[1], "false")
        self.assertTrue(calls[-1].endswith("|exec vitest run --reporter=dot"), calls)

    def test_package_management_commands_pass_through_without_a_lane_install(self) -> None:
        proj = self.fx.project()
        for argv in (["install"], ["install", "--frozen-lockfile"], ["add", "-D", "x"], ["store", "path"], ["--version"], []):
            self.fx.log.unlink(missing_ok=True)
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, r.stderr)
            # exactly the caller's own command reaches pnpm, nothing before it
            self.assertEqual([c.split("|", 2)[2] for c in self.fx.calls()], [" ".join(argv)], argv)
            self.assertNotIn("installing lane dependencies", r.stderr)
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps").exists())

    def test_first_project_command_installs_once_and_records_the_lockfile_hash(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "eslint", ".", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("installing lane dependencies once", r.stderr)
        calls = self.fx.calls()
        self.assertEqual(len(calls), 2, calls)
        self.assertEqual(calls[0], f"{proj.resolve()}|false|install --frozen-lockfile")
        self.assertTrue(calls[1].endswith("|exec eslint ."))
        marker = proj / "node_modules" / ".gc-lane-deps"
        self.assertEqual(marker.read_text(encoding="utf-8").strip(), sha256(proj / "pnpm-lock.yaml"))
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists())

    def test_later_commands_do_not_install_again(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "test", cwd=proj)
        for argv in (["exec", "vitest"], ["run", "lint"], ["agreement:pdf", "a.md"], ["vitest", "run"]):
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertNotIn("installing", r.stderr)
        self.assertEqual(len(self.fx.installs()), 1, self.fx.calls())

    def test_a_changed_lockfile_installs_exactly_once_more(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "test", cwd=proj)
        (proj / "pnpm-lock.yaml").write_text("lockfileVersion: '9.0'\nchanged: true\n", encoding="utf-8")
        self.fx.run("pnpm", "test", cwd=proj)
        self.fx.run("pnpm", "test", cwd=proj)
        self.assertEqual(len(self.fx.installs()), 2, self.fx.calls())
        marker = proj / "node_modules" / ".gc-lane-deps"
        self.assertEqual(marker.read_text(encoding="utf-8").strip(), sha256(proj / "pnpm-lock.yaml"))

    def test_command_from_a_subdirectory_installs_at_the_lockfile_root(self) -> None:
        proj = self.fx.project()
        sub = proj / "server" / "hocuspocus"
        sub.mkdir(parents=True)
        r = self.fx.run("pnpm", "exec", "tsc", cwd=sub)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [f"{proj.resolve()}|false|install --frozen-lockfile"])
        self.assertTrue((proj / "node_modules" / ".gc-lane-deps").exists())
        self.assertFalse((sub / "node_modules").exists())

    def test_no_lockfile_means_no_lane_install(self) -> None:
        d = self.fx.root / "plain"
        d.mkdir()
        r = self.fx.run("pnpm", "exec", "node", "-e", "1", cwd=d)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [])
        self.assertEqual(len(self.fx.calls()), 1)

    def test_concurrent_callers_wait_for_one_install_instead_of_racing_it(self) -> None:
        proj = self.fx.project()
        results: list[subprocess.CompletedProcess[str]] = []
        lock = threading.Lock()

        def call(name: str) -> None:
            r = self.fx.run("pnpm", "exec", name, cwd=proj, FAKE_PNPM_SLEEP="2")
            with lock:
                results.append(r)

        threads = [threading.Thread(target=call, args=(n,)) for n in ("vitest", "eslint", "tsc")]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=60)
        self.assertEqual(len(results), 3)
        for r in results:
            self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.fx.installs()), 1, self.fx.calls())
        ran = sorted(c.split("|", 2)[2] for c in self.fx.calls() if not c.endswith("install --frozen-lockfile"))
        self.assertEqual(ran, ["exec eslint", "exec tsc", "exec vitest"])
        waited = [r for r in results if "waiting for another caller's lane install" in r.stderr]
        self.assertEqual(len(waited), 2, [r.stderr for r in results])

    def test_failed_install_writes_no_marker_and_the_command_does_not_run(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, FAKE_PNPM_FAIL_INSTALL="1")
        self.assertEqual(r.returncode, 1)
        self.assertIn("lane install failed", r.stderr)
        self.assertIn("exit 7", r.stderr)
        self.assertIn("this command did not run", r.stderr)
        self.assertEqual(len(self.fx.calls()), 1, self.fx.calls())
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps").exists())
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists())
        # the next caller retries the install rather than trusting a half tree
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.fx.installs()), 2)

    def test_stale_lock_with_a_dead_owner_is_moved_aside_never_deleted(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("moved aside a stale lane install lock", r.stderr)
        self.assertEqual(len(self.fx.installs()), 1)
        aside = [p for p in (proj / "node_modules").iterdir() if p.name.startswith(".gc-lane-deps.lock.stale-")]
        self.assertEqual(len(aside), 1, aside)
        self.assertEqual((aside[0] / "pid").read_text(encoding="utf-8").strip(), str(dead.pid))
        self.assertFalse(lock.exists())

    def test_ownerless_lock_older_than_two_minutes_is_moved_aside(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        old = time.time() - 180
        os.utime(lock, (old, old))
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("stale lane install lock (owner pid none)", r.stderr)

    def test_live_lock_is_waited_for_then_fails_closed_naming_the_holder(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        holder = subprocess.Popen(["sleep", "30"])
        try:
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 1)
        self.assertIn(f"another lane install (pid {holder.pid}) has held", r.stderr)
        self.assertIn("this command did not run", r.stderr)
        self.assertEqual(self.fx.calls(), [])
        self.assertTrue(lock.exists())

    def test_a_waiter_that_finds_the_marker_written_meanwhile_runs_without_installing(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        holder = subprocess.Popen(["sleep", "30"])
        try:
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            # the "holder" finishes its install: marker appears, lock stays a moment
            (proj / "node_modules" / ".gc-lane-deps").write_text(sha256(proj / "pnpm-lock.yaml") + "\n", encoding="utf-8")
            r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="5")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [])

    def test_lane_deps_can_be_switched_off_by_environment_or_sidecar(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS="off")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [])
        (self.fx.shims / "toolchain.env").write_text("LANE_DEPS=off\n", encoding="utf-8")
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [])
        # environment wins over the sidecar
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS="on")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.fx.installs()), 1)

    def test_sidecar_values_are_read_as_text_never_evaluated(self) -> None:
        proj = self.fx.project()
        canary = self.fx.root / "canary"
        (self.fx.shims / "toolchain.env").write_text(
            f"LANE_DEPS_WAIT=$(touch {canary})\nLANE_DEPS=`touch {canary}`\n", encoding="utf-8"
        )
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(canary.exists())
        self.assertEqual(len(self.fx.installs()), 1)  # a non-"off" LANE_DEPS value keeps the install on

    def test_shim_directory_is_first_on_the_path_pnpm_runs_with(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "--version", cwd=proj)
        path0 = [l for l in self.fx.log.read_text(encoding="utf-8").splitlines() if l.startswith("PATH0=")]
        self.assertEqual(path0, [f"PATH0={self.fx.shims.resolve()}"])


def fake_node_tree(root: pathlib.Path, version: str) -> pathlib.Path:
    """A minimal node install tree at root/v<version>: bin/{node,npm,npx}."""
    tree = root / f"v{version}"
    for name in ("node", "npm", "npx"):
        write_exec(tree / "bin" / name, f'#!/bin/sh\necho "fake {name} {version} $*"\n')
    return tree


def fake_dist(root: pathlib.Path, version: str, *, corrupt_sum: bool = False) -> str:
    """A nodejs.org-shaped dist directory served over file://; returns its URL."""
    os_name = platform.system().lower()
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "x64", "amd64": "x64"}[platform.machine()]
    name = f"node-v{version}-{os_name}-{arch}"
    build = root / "build" / name
    for prog in ("node", "npm", "npx"):
        write_exec(build / "bin" / prog, f'#!/bin/sh\necho "downloaded {prog} {version} $*"\n')
    vdir = root / f"v{version}"
    vdir.mkdir(parents=True)
    tar_path = vdir / f"{name}.tar.gz"
    with tarfile.open(tar_path, "w:gz") as tf:
        tf.add(build, arcname=name)
    digest = sha256(tar_path)
    if corrupt_sum:
        digest = "0" * 64
    (vdir / "SHASUMS256.txt").write_text(f"{digest}  {name}.tar.gz\n{'1' * 64}  other.tar.gz\n", encoding="utf-8")
    return f"file://{root}"


class NodeShimTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self.tmp.name))
        self.node_root = self.fx.toolchain / "node"

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_without_a_pin_the_wrapper_is_transparent(self) -> None:
        for prog in ("node", "npm", "npx"):
            r = self.fx.run(prog, "--version", "-e", "1")
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(r.stdout, f"machine {prog} --version -e 1\n")
            self.assertEqual(r.stderr, "")

    def test_environment_pin_runs_the_installed_version(self) -> None:
        fake_node_tree(self.node_root, "9.9.9")
        r = self.fx.run("node", "-e", "1", GC_TOOLCHAIN_NODE_VERSION="9.9.9")
        self.assertEqual(r.stdout, "fake node 9.9.9 -e 1\n")
        self.assertEqual(r.stderr, "")
        r = self.fx.run("npm", "--version", GC_TOOLCHAIN_NODE_VERSION="v9.9.9")
        self.assertEqual(r.stdout, "fake npm 9.9.9 --version\n")
        r = self.fx.run("npx", "-y", "x", GC_TOOLCHAIN_NODE_VERSION="9.9.9")
        self.assertEqual(r.stdout, "fake npx 9.9.9 -y x\n")

    def test_nvmrc_above_the_working_directory_pins_and_a_major_picks_the_newest_installed(self) -> None:
        fake_node_tree(self.node_root, "9.9.9")
        fake_node_tree(self.node_root, "9.10.1")
        fake_node_tree(self.node_root, "10.0.0")
        repo = self.fx.root / "repo"
        deep = repo / "a" / "b"
        deep.mkdir(parents=True)
        (repo / ".nvmrc").write_text("9\n", encoding="utf-8")
        r = self.fx.run("node", "-v", cwd=deep)
        self.assertEqual(r.stdout, "fake node 9.10.1 -v\n", r.stderr)
        (repo / ".nvmrc").write_text("v9.9\n", encoding="utf-8")
        r = self.fx.run("node", "-v", cwd=deep)
        self.assertEqual(r.stdout, "fake node 9.9.9 -v\n", r.stderr)
        (repo / ".nvmrc").unlink()
        (repo / ".node-version").write_text("10.0.0", encoding="utf-8")
        r = self.fx.run("node", "-v", cwd=deep)
        self.assertEqual(r.stdout, "fake node 10.0.0 -v\n", r.stderr)

    def test_environment_wins_over_nvmrc_and_nvmrc_over_the_sidecar(self) -> None:
        fake_node_tree(self.node_root, "9.9.9")
        fake_node_tree(self.node_root, "10.0.0")
        fake_node_tree(self.node_root, "11.0.0")
        repo = self.fx.root / "repo"
        repo.mkdir()
        (self.fx.shims / "toolchain.env").write_text("NODE_VERSION=11.0.0\n", encoding="utf-8")
        r = self.fx.run("node", "-v", cwd=repo)
        self.assertEqual(r.stdout, "fake node 11.0.0 -v\n", r.stderr)
        (repo / ".nvmrc").write_text("10.0.0\n", encoding="utf-8")
        r = self.fx.run("node", "-v", cwd=repo)
        self.assertEqual(r.stdout, "fake node 10.0.0 -v\n", r.stderr)
        r = self.fx.run("node", "-v", cwd=repo, GC_TOOLCHAIN_NODE_VERSION="9.9.9")
        self.assertEqual(r.stdout, "fake node 9.9.9 -v\n", r.stderr)

    def test_unusable_nvmrc_spec_warns_and_falls_through(self) -> None:
        repo = self.fx.root / "repo"
        repo.mkdir()
        (repo / ".nvmrc").write_text("lts/*\n", encoding="utf-8")
        r = self.fx.run("node", "-v", cwd=repo)
        self.assertEqual(r.stdout, "machine node -v\n")
        self.assertIn("does not resolve", r.stderr)

    def test_missing_version_with_installs_off_warns_once_and_falls_through(self) -> None:
        r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="9.9.9", GC_TOOLCHAIN_NODE_INSTALL="off")
        self.assertEqual(r.returncode, 0)
        self.assertEqual(r.stdout, "machine node -v\n")
        self.assertIn("installs are off", r.stderr)
        self.assertIn("using the node on PATH", r.stderr)

    def test_unwritable_toolchain_warns_and_falls_through(self) -> None:
        self.node_root.mkdir()
        self.node_root.chmod(0o555)
        try:
            r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="9.9.9", GC_TOOLCHAIN_NODE_DIST="file:///nonexistent")
        finally:
            self.node_root.chmod(0o755)
        self.assertEqual(r.stdout, "machine node -v\n")
        self.assertIn("cannot write", r.stderr)
        self.assertEqual([p.name for p in self.node_root.iterdir()], [])

    def test_download_is_checksum_verified_installed_once_and_then_used_by_all_three_names(self) -> None:
        dist = fake_dist(self.fx.root / "dist", "1.2.3")
        r = self.fx.run("node", "-e", "1", GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST=dist)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(r.stdout, "downloaded node 1.2.3 -e 1\n")
        self.assertIn("installed node v1.2.3", r.stderr)
        self.assertTrue((self.node_root / "v1.2.3" / "bin" / "node").exists())
        self.assertEqual(sorted(p.name for p in self.node_root.iterdir()), ["v1.2.3"])  # no lock, no stage left
        for prog in ("node", "npm", "npx"):
            r = self.fx.run(prog, "--v", GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST="file:///nonexistent")
            self.assertEqual(r.stdout, f"downloaded {prog} 1.2.3 --v\n", r.stderr)
            self.assertEqual(r.stderr, "")

    def test_checksum_mismatch_installs_nothing_and_falls_through(self) -> None:
        dist = fake_dist(self.fx.root / "dist", "1.2.3", corrupt_sum=True)
        r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST=dist)
        self.assertEqual(r.stdout, "machine node -v\n")
        self.assertIn("checksum mismatch", r.stderr)
        self.assertFalse((self.node_root / "v1.2.3").exists())
        self.assertFalse((self.node_root / ".installing-v1.2.3").exists())

    def test_a_major_not_installed_resolves_from_the_dist_index(self) -> None:
        dist = fake_dist(self.fx.root / "dist", "1.2.3")
        (self.fx.root / "dist" / "index.json").write_text(
            '[{"version":"v2.0.0","lts":false},{"version":"v1.2.3","lts":"X"},{"version":"v1.1.9","lts":false}]',
            encoding="utf-8",
        )
        r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="1", GC_TOOLCHAIN_NODE_DIST=dist)
        self.assertEqual(r.stdout, "downloaded node 1.2.3 -v\n", r.stderr)

    def test_wrong_install_name_refuses(self) -> None:
        (self.fx.shims / "corepack").symlink_to("node")
        r = self.fx.run("corepack", "enable")
        self.assertEqual(r.returncode, 1)
        self.assertIn("install this file as node, npm or npx", r.stderr)

    def test_sidecar_is_read_as_text_never_evaluated(self) -> None:
        canary = self.fx.root / "canary"
        (self.fx.shims / "toolchain.env").write_text(f"NODE_VERSION=$(touch {canary})\n", encoding="utf-8")
        r = self.fx.run("node", "-v")
        self.assertEqual(r.stdout, "machine node -v\n")
        self.assertFalse(canary.exists())
        self.assertIn("is not X, X.Y or X.Y.Z", r.stderr)


if __name__ == "__main__":
    unittest.main()
