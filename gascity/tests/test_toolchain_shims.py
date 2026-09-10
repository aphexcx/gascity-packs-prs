"""Contract tests for gascity/assets/scripts/toolchain/{pnpm,node}.

Each test installs the wrappers into a throwaway shim directory the way the
README recipe does (`node` once, `npm` and `npx` as links to it), points
GC_TOOLCHAIN_DIR at a throwaway toolchain, and drives them with fake
programs: a fake pnpm that records every call and answers the wrapper's
read-only sync question the way pnpm's checkDepsStatus does (from a state
its own installs write over the lockfile, every manifest and the workspace
membership; `--lockfile-only` and the cleanup built-ins leave it out of
sync), fake node trees, and a fake nodejs.org served over file://. Nothing
touches the network or the machine's toolchain.
"""

from __future__ import annotations

import hashlib
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

CHECK = "--config.verify-deps-before-run=error exec true"

FAKE_PNPM = r"""#!/bin/sh
# A stand-in for pnpm 11.20: records `<cwd>|<verify env>|<argv>` per call,
# honours -C/--dir/--prefix and --ignore-workspace (the working directory is
# the whole project: no lockfile above it is read), answers the read-only sync question from the
# state its installs write (lockfile + manifests + workspace membership),
# installs (optionally slowly, optionally failing), and lets the cleanup
# built-ins and --lockfile-only leave the tree out of sync.
printf '%s|%s|%s\n' "$(pwd -P)" "${pnpm_config_verify_deps_before_run-unset}" "$*" >> "$FAKE_PNPM_LOG"
printf 'PATH0=%s\n' "${PATH%%:*}" >> "$FAKE_PNPM_LOG"
start="$(pwd)"
want=""
ignore_workspace=0
for a in "$@"; do
    if [ -n "$want" ]; then cd "$start" && cd "$a" || exit 3; want=""; continue; fi
    case "$a" in
        --ignore-workspace) ignore_workspace=1 ;;
        -C|--dir|--prefix) want=1 ;;
        -C=*) cd "$start" && cd "${a#-C=}" || exit 3 ;;
        --dir=*) cd "$start" && cd "${a#--dir=}" || exit 3 ;;
        --prefix=*) cd "$start" && cd "${a#--prefix=}" || exit 3 ;;
        --) break ;;
    esac
done
lane() {
    d="$(pwd -P)"
    [ "$ignore_workspace" -eq 0 ] || { printf '%s\n' "$d"; return; }
    while [ ! -f "$d/pnpm-lock.yaml" ] && [ "$d" != / ]; do d="${d%/*}"; [ -n "$d" ] || d=/; done
    if [ -f "$d/pnpm-lock.yaml" ]; then printf '%s\n' "$d"; else pwd -P; fi
}
fp() {
    L="$(lane)"
    (cd "$L" && for f in pnpm-lock.yaml pnpm-workspace.yaml .npmrc package.json packages/*/package.json server/*/package.json; do
        [ -f "$f" ] && { printf '%s ' "$f"; shasum -a 256 < "$f"; }
    done) | shasum -a 256
}
case "$*" in
    "--config.verify-deps-before-run=error exec true"|"--ignore-workspace --config.verify-deps-before-run=error exec true")
        if [ "${FAKE_PNPM_PROBE_ERROR-}" = 1 ]; then echo "EACCES: permission denied, open '.pnpm-workspace-state-v1.json'" >&2; exit 2; fi
        L="$(lane)"
        if [ -f "$L/node_modules/.fake-state" ] && [ "$(cat "$L/node_modules/.fake-state")" = "$(fp)" ]; then exit 0; fi
        # pnpm 11.20 prints this on STDOUT
        echo " ERR_PNPM_VERIFY_DEPS_BEFORE_RUN  Your node_modules are out of sync (fake)"
        exit 1 ;;
esac
first=""
skip=""
after_flag=""
for a in "$@"; do
    if [ -n "$skip" ]; then skip=""; continue; fi
    if [ -n "$after_flag" ]; then
        after_flag=""
        case "$a" in true|false) continue ;; esac     # a boolean option's literal value
    fi
    case "$a" in
        --) break ;;
        -C|--dir|--prefix|-F|--filter|--loglevel|--reporter|--child-concurrency|--network-concurrency|with) skip=1 ;;
        -*) after_flag=1 ;;
        recursive|multi|m|pm) ;;
        *) first="$a"; break ;;
    esac
done
[ -n "$first" ] || for a in "$@"; do case "$a" in install|i|ci|add|update|it|install-test) first="$a"; break ;; esac; done   # `pnpm -- install`
case "$first" in
    install|i|ci|add|update|it|install-test)
        [ -z "${FAKE_PNPM_SLEEP-}" ] || sleep "$FAKE_PNPM_SLEEP"
        if [ "${FAKE_PNPM_FAIL_INSTALL-}" = 1 ]; then echo "fake pnpm: install failed" >&2; exit 7; fi
        L="$(lane)"
        mkdir -p "$L/node_modules" && : > "$L/node_modules/.fake-installed"
        [ -f "$L/pnpm-lock.yaml" ] || echo "lockfileVersion: '9.0'" > "$L/pnpm-lock.yaml"
        case "$*" in
            *--lockfile-only*) echo "# resolved $(date +%s%N)" >> "$L/pnpm-lock.yaml" ;;   # the lockfile moves, the tree does not
            *) fp > "$L/node_modules/.fake-state" ;;
        esac
        if [ -n "${FAKE_PNPM_HOOK-}" ]; then "$FAKE_PNPM_HOOK" run build || exit 9; fi
        [ -z "${FAKE_PNPM_TAKEOVER_PID-}" ] || echo "$FAKE_PNPM_TAKEOVER_PID" > "$L/node_modules/.gc-lane-deps.lock/pid"
        echo "fake pnpm: installed"
        exit 0 ;;
    clean|purge|prune|dedupe|rebuild|remove|rm|link|unlink|patch)
        L="$(lane)"
        rm -f "$L/node_modules/.fake-state"
        echo "fake pnpm: mutated"
        exit 0 ;;
esac
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

    def __init__(self, root: pathlib.Path, *, real_node: bool = False) -> None:
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
        if real_node:
            # the pnpm wrapper reads a manifest with node: give it the machine's real one
            machine_node = shutil.which("node") or "/usr/bin/false"
            write_exec(self.bin / "node", f'#!/bin/sh\nexec {machine_node} "$@"\n')
        # fake pinned pnpm at the place the shim installs it
        self.fake_pnpm = self.toolchain / "pnpm" / "v11.20.0" / "node_modules" / ".bin" / "pnpm"
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

    def all_calls(self) -> list[str]:
        if not self.log.exists():
            return []
        return [line for line in self.log.read_text(encoding="utf-8").splitlines() if not line.startswith("PATH0=")]

    @staticmethod
    def is_check(call: str) -> bool:
        return call.split("|", 2)[2] in (CHECK, "--ignore-workspace " + CHECK)

    def calls(self) -> list[str]:
        """Every pnpm call except the wrapper's read-only sync questions."""
        return [c for c in self.all_calls() if not self.is_check(c)]

    def argv(self) -> list[str]:
        return [c.split("|", 2)[2] for c in self.calls()]

    def checks(self) -> list[str]:
        return [c for c in self.all_calls() if self.is_check(c)]

    def installs(self) -> list[str]:
        return [c for c in self.calls() if c.split("|", 2)[2] in ("install --frozen-lockfile", "--ignore-workspace install --frozen-lockfile")]

    def reset(self) -> None:
        self.log.unlink(missing_ok=True)

    def project(self, name: str = "proj", lock: str = "lockfileVersion: '9.0'\n") -> pathlib.Path:
        p = self.root / name
        p.mkdir(parents=True)
        (p / "package.json").write_text('{"name":"p","private":true}\n', encoding="utf-8")
        (p / "pnpm-lock.yaml").write_text(lock, encoding="utf-8")
        return p


def in_sync(proj: pathlib.Path) -> bool:
    return (proj / "node_modules" / ".fake-state").exists()


class PnpmShimVerdictTests(unittest.TestCase):
    """Round 10 of the codex gate: a probe failure that is not pnpm's verdict
    is not a reason to install; a package script overrides five built-ins."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self.tmp.name), real_node=True)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_the_lane_is_the_workspace_root_even_with_per_package_lockfiles(self) -> None:
        ws = self.fx.root / "ws"
        ws.mkdir()
        (ws / "pnpm-workspace.yaml").write_text("packages:\n  - packages/*\nsharedWorkspaceLockfile: false\n", encoding="utf-8")
        (ws / "package.json").write_text('{"name":"ws","private":true}\n', encoding="utf-8")
        a = self.fx.project("ws/packages/a")
        b = self.fx.project("ws/packages/b")
        lock = ws / "node_modules" / ".gc-lane-deps.lock"
        holder = subprocess.Popen(["sleep", "30"])
        try:
            lock.mkdir(parents=True)
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            # both packages contend for the WORKSPACE lock, not their own lockfile directories
            ra = self.fx.run("pnpm", "install", cwd=a, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
            rb = self.fx.run("pnpm", "exec", "vitest", cwd=b, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        for r in (ra, rb):
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn(f"has held {lock.resolve()}", r.stderr)
        self.assertFalse((a / "node_modules" / ".gc-lane-deps.lock").exists())
        self.assertFalse((b / "node_modules" / ".gc-lane-deps.lock").exists())

    def test_help_and_version_requests_run_as_is(self) -> None:
        proj = self.fx.project()
        for argv in (["run", "--help"], ["install", "--help"], ["-h", "exec", "vitest"], ["add", "x", "--help"], ["-v"], ["--version", "run", "build"]):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|{' '.join(argv)}"], argv)
            self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists(), argv)
        # after `exec` pnpm reads no options (measured on 11.20: `pnpm exec -h` is
        # 'Command "-h" not found'), so `-h` there is the bin name: a project command
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "-h", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.fx.checks()), 1)     # asked (the fake's `add` above left the tree in sync)
        self.assertEqual(self.fx.argv(), ["exec -h"])

    def test_audit_fix_and_workspace_root_are_resolved_before_the_lock_and_the_override(self) -> None:
        ws = self.fx.root / "ws"
        ws.mkdir()
        (ws / "pnpm-workspace.yaml").write_text("packages:\n  - packages/*\n", encoding="utf-8")
        (ws / "package.json").write_text('{"name":"ws","private":true}\n', encoding="utf-8")
        (ws / "pnpm-lock.yaml").write_text("lockfileVersion: '9.0'\n", encoding="utf-8")
        member = ws / "packages" / "a"
        member.mkdir(parents=True)
        (member / "package.json").write_text('{"name":"a","scripts":{"clean":"rimraf dist"}}\n', encoding="utf-8")
        lock = ws / "node_modules" / ".gc-lane-deps.lock"
        holder = subprocess.Popen(["sleep", "30"])
        try:
            lock.mkdir(parents=True)
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            # `pnpm -w clean` from a member that defines clean: the ROOT has no such script,
            # so it is the built-in workspace cleanup, under the workspace lock
            r1 = self.fx.run("pnpm", "-w", "clean", cwd=member, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
            r2 = self.fx.run("pnpm", "audit", "--fix", cwd=ws, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
            r3 = self.fx.run("pnpm", "audit", cwd=ws, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        for r in (r1, r2):
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertIn(f"has held {lock.resolve()}", r.stderr)
        self.assertEqual(r3.returncode, 0, r3.stderr)  # a plain audit is informational
        self.assertEqual(self.fx.argv(), ["audit"])
        # without -w, the member's own clean script is a project command
        self.fx.reset()
        r = self.fx.run("pnpm", "clean", cwd=member)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.argv()[-1], "clean")
        self.assertGreaterEqual(len(self.fx.checks()), 1)

    def test_a_probe_error_that_is_not_a_verdict_runs_the_command_as_is(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, FAKE_PNPM_PROBE_ERROR="1", GC_TOOLCHAIN_LANE_DEPS_WAIT="3")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("could not judge the lane", r.stderr)
        self.assertIn("EACCES", r.stderr)
        self.assertEqual(self.fx.argv(), ["exec vitest"])
        self.assertFalse((proj / "node_modules").exists())  # no lock taken, nothing installed

    def test_a_package_script_named_like_a_built_in_is_a_project_command_unless_pm_forces_the_built_in(self) -> None:
        proj = self.fx.project()
        (proj / "package.json").write_text('{"name":"p","private":true,"scripts":{"clean":"rimraf dist","deploy":"echo","setup":"echo","rb":"echo"}}\n', encoding="utf-8")
        for argv in (["clean"], ["deploy"], ["setup", "--flag"], ["rb"]):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            # a fresh lane: the script's dependencies are installed first, then the script runs
            self.assertEqual(self.fx.argv()[-1], " ".join(argv), argv)
            self.assertGreaterEqual(len(self.fx.checks()), 1, argv)
        self.assertTrue(in_sync(proj))
        # the manifest that decides is the one of the directory pnpm acts on, even when
        # that directory is named after the command: B has no `clean` script, so B's
        # built-in runs under B's lock, not A's script
        other = self.fx.project("other")
        self.fx.run("pnpm", "exec", "vitest", cwd=other)
        self.fx.reset()
        r = self.fx.run("pnpm", "clean", "--dir", "../other", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|clean --dir ../other"])
        self.assertFalse(in_sync(other))
        self.assertTrue(in_sync(proj))
        # `purge` and `rebuild` are not scripts here: still built-ins
        for argv in (["purge"], ["rebuild"], ["pm", "clean"]):
            self.fx.run("pnpm", "exec", "vitest", cwd=proj)
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|{' '.join(argv)}"], argv)
            self.assertFalse(in_sync(proj), argv)


class PnpmShimTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self.tmp.name), real_node=True)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_pnpm_own_in_command_install_is_off_and_argv_reaches_pnpm(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", "run", "--reporter=dot", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        last = self.fx.calls()[-1]
        self.assertEqual(last.split("|", 2)[1], "false")
        self.assertTrue(last.endswith("|exec vitest run --reporter=dot"), last)
        # the sync question is asked with the flag and without the environment value
        self.assertEqual([c.split("|", 2)[1] for c in self.fx.checks()], ["unset", "unset"])

    def test_info_commands_run_as_is_and_ask_nothing(self) -> None:
        proj = self.fx.project()
        for argv in (["store", "path"], ["--version"], [], ["config", "get", "x"], ["outdated"], ["-r", "list"], ["with", "current", "list"], ["dlx", "cowsay"]):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertEqual(self.fx.argv(), [" ".join(argv)], argv)
            self.assertEqual(self.fx.checks(), [], argv)
        self.assertFalse((proj / "node_modules").exists())

    def test_first_project_command_asks_pnpm_installs_once_and_runs(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "eslint", ".", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertIn("installing lane dependencies once", r.stderr)
        self.assertEqual(self.fx.argv(), ["install --frozen-lockfile", "exec eslint ."])
        self.assertEqual(self.fx.calls()[0], f"{proj.resolve()}|false|install --frozen-lockfile")
        # asked before the lock and again under it, then never during the command
        self.assertEqual(len(self.fx.checks()), 2)
        self.assertTrue(in_sync(proj))
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists())

    def test_later_commands_ask_once_and_do_not_install(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "test", cwd=proj)
        for argv in (["exec", "vitest"], ["run", "lint"], ["agreement:pdf", "a.md"], ["vitest", "run"], ["test", "--", "-C", "x"]):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, r.stderr)
            self.assertNotIn("installing", r.stderr)
            self.assertEqual(self.fx.argv(), [" ".join(argv)], argv)
            self.assertEqual(len(self.fx.checks()), 1, argv)

    def test_anything_that_makes_pnpm_say_out_of_sync_installs_exactly_once_more(self) -> None:
        proj = self.fx.project()
        (proj / "pnpm-lock.yaml").write_text("lockfileVersion: '9.0'\n\nimporters:\n\n  .:\n    dependencies: {}\n\n  packages/app:\n    dependencies: {}\n", encoding="utf-8")
        app = proj / "packages" / "app"
        app.mkdir(parents=True)
        (app / "package.json").write_text('{"name":"app"}\n', encoding="utf-8")
        self.fx.run("pnpm", "test", cwd=proj)
        self.assertEqual(len(self.fx.installs()), 1)
        edits = (
            (proj / "pnpm-lock.yaml", "lockfileVersion: '9.0'\nchanged: true\n"),
            (proj / "package.json", '{"name":"p","private":true,"dependencies":{"new":"1"}}\n'),
            (app / "package.json", '{"name":"app","dependencies":{"x":"1"}}\n'),
            (proj / "pnpm-workspace.yaml", "packages:\n  - packages/*\n"),
            (proj / ".npmrc", "node-linker=hoisted\n"),
            (proj / "packages" / "new" / "package.json", '{"name":"new"}\n'),  # a workspace member pnpm sees
        )
        for path, text in edits:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(text, encoding="utf-8")
            self.fx.reset()
            r = self.fx.run("pnpm", "test", cwd=proj)
            self.assertEqual(r.returncode, 0, (path, r.stderr))
            self.assertEqual(self.fx.argv(), ["install --frozen-lockfile", "test"], path)
            self.fx.reset()
            self.fx.run("pnpm", "test", cwd=proj)
            self.assertEqual(self.fx.argv(), ["test"], path)
        (proj / "src.ts").write_text("export {}\n", encoding="utf-8")
        self.fx.reset()
        self.fx.run("pnpm", "test", cwd=proj)
        self.assertEqual(self.fx.argv(), ["test"])

    def test_command_from_a_subdirectory_installs_at_the_lockfile_root(self) -> None:
        proj = self.fx.project()
        sub = proj / "server" / "hocuspocus"
        sub.mkdir(parents=True)
        r = self.fx.run("pnpm", "exec", "tsc", cwd=sub)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [f"{proj.resolve()}|false|install --frozen-lockfile"])
        self.assertEqual(self.fx.checks()[0].split("|", 1)[0], str(sub.resolve()))
        self.assertTrue(in_sync(proj))
        self.assertFalse((sub / "node_modules").exists())

    def test_no_lockfile_above_means_no_question_and_no_install(self) -> None:
        d = self.fx.root / "plain"
        d.mkdir()
        r = self.fx.run("pnpm", "exec", "node", "-e", "1", cwd=d)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.all_calls(), [f"{d.resolve()}|false|exec node -e 1"])

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
        ran = sorted(a for a in self.fx.argv() if a != "install --frozen-lockfile")
        self.assertEqual(ran, ["exec eslint", "exec tsc", "exec vitest"])
        waited = [r for r in results if "waiting for another caller's lane install" in r.stderr]
        self.assertEqual(len(waited), 2, [r.stderr for r in results])

    def test_failed_install_leaves_the_lane_out_of_sync_and_the_command_does_not_run(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, FAKE_PNPM_FAIL_INSTALL="1")
        self.assertEqual(r.returncode, 1)
        self.assertIn("lane install failed", r.stderr)
        self.assertIn("exit 7", r.stderr)
        self.assertIn("this command did not run", r.stderr)
        self.assertEqual(self.fx.argv(), ["install --frozen-lockfile"])
        self.assertFalse(in_sync(proj))
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists())
        # the next caller asks pnpm again and installs, rather than trusting a half tree
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

    def test_a_caller_whose_lane_is_already_in_sync_never_touches_the_lock(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "test", cwd=proj)
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir()
        holder = subprocess.Popen(["sleep", "30"])
        try:
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            self.fx.reset()
            r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="5")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.argv(), ["exec vitest"])
        self.assertNotIn("waiting", r.stderr)

    def test_lane_deps_can_be_switched_off_by_environment_or_sidecar(self) -> None:
        proj = self.fx.project()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS="off")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|exec vitest"])
        (self.fx.shims / "toolchain.env").write_text("LANE_DEPS=off\n", encoding="utf-8")
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|exec vitest"])
        # environment wins over the sidecar
        self.fx.reset()
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
        self.assertEqual(len(self.fx.installs()), 1)  # a non-"off" LANE_DEPS value keeps the lane path on

    def test_shim_directory_is_first_on_the_path_pnpm_runs_with(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "--version", cwd=proj)
        path0 = [l for l in self.fx.log.read_text(encoding="utf-8").splitlines() if l.startswith("PATH0=")]
        self.assertEqual(path0, [f"PATH0={self.fx.shims.resolve()}"])

    def test_pnpm_install_is_keyed_by_version(self) -> None:
        proj = self.fx.project()
        other = self.fx.toolchain / "pnpm" / "v9.9.9" / "node_modules" / ".bin" / "pnpm"
        write_exec(other, '#!/bin/sh\necho "fake pnpm 9.9.9 $*"\n')
        r = self.fx.run("pnpm", "--version", cwd=proj, GC_TOOLCHAIN_PNPM_VERSION="9.9.9")
        self.assertEqual(r.stdout, "fake pnpm 9.9.9 --version\n", r.stderr)
        (self.fx.shims / "toolchain.env").write_text("PNPM_VERSION=9.9.9\n", encoding="utf-8")
        r = self.fx.run("pnpm", "--version", cwd=proj)
        self.assertEqual(r.stdout, "fake pnpm 9.9.9 --version\n", r.stderr)
        r = self.fx.run("pnpm", "--version", cwd=proj, GC_TOOLCHAIN_PNPM_VERSION="11.20.0")
        self.assertEqual(r.stdout, "fake pnpm ran: --version\n", r.stderr)


class PnpmShimMutationTests(unittest.TestCase):
    """Explicit dependency commands: where they run, what they hold, what
    pnpm says afterwards."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self.tmp.name), real_node=True)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_explicit_dependency_commands_run_in_place_under_the_lock_and_pnpm_judges_the_result(self) -> None:
        proj = self.fx.project()
        pkg = proj / "packages" / "app"
        pkg.mkdir(parents=True)
        for argv, cwd in (
            (["install"], proj),
            (["add", "-D", "x"], pkg),          # a workspace package stays that package
            (["--filter", "app", "install"], proj),
            (["-C", "packages/app", "add", "y"], proj),
            (["add", "--dir", "packages/app", "z"], proj),
            (["--prefix=packages/app", "install"], proj),
            (["with", "current", "install"], proj),
        ):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=cwd)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            # the explicit command itself, in the caller's directory, never a frozen install before it
            self.assertEqual(self.fx.all_calls(), [f"{cwd.resolve()}|false|{' '.join(argv)}"], argv)
            self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists())
            self.assertFalse((pkg / "node_modules").exists(), argv)
        # a successful explicit install leaves pnpm saying in sync: the next check runs as is
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=pkg)
        self.assertEqual(self.fx.argv(), ["exec vitest"])
        # an install that certifies nothing (`--lockfile-only` after a manifest edit moves the
        # lockfile, not the tree) makes the next project command install
        (proj / "package.json").write_text('{"name":"p","private":true,"dependencies":{"new":"1"}}\n', encoding="utf-8")
        self.fx.reset()
        r = self.fx.run("pnpm", "install", "--lockfile-only", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=pkg)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.calls(), [f"{proj.resolve()}|false|install --frozen-lockfile", f"{pkg.resolve()}|false|exec vitest"])

    def test_every_built_in_that_can_change_node_modules_holds_the_lock_and_pnpm_notices_afterwards(self) -> None:
        proj = self.fx.project()
        for argv in (["clean"], ["purge"], ["pm", "clean"], ["with", "current", "clean"], ["recursive", "prune"], ["-r", "rebuild"], ["m", "dedupe"], ["remove", "x"], ["--reporter=silent", "purge"]):
            self.fx.run("pnpm", "exec", "vitest", cwd=proj)
            self.assertTrue(in_sync(proj))
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|{' '.join(argv)}"], argv)
            self.assertFalse(in_sync(proj), argv)
            self.fx.reset()
            self.fx.run("pnpm", "exec", "vitest", cwd=proj)
            self.assertEqual(self.fx.argv(), ["install --frozen-lockfile", "exec vitest"], argv)

    def test_failed_explicit_install_after_a_lockfile_switch_reinstalls_on_the_way_back(self) -> None:
        proj = self.fx.project()
        self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        lock_a = (proj / "pnpm-lock.yaml").read_text(encoding="utf-8")
        (proj / "pnpm-lock.yaml").write_text(lock_a + "b: true\n", encoding="utf-8")
        r = self.fx.run("pnpm", "install", cwd=proj, FAKE_PNPM_FAIL_INSTALL="1")
        self.assertEqual(r.returncode, 7)  # pnpm's own status, passed through
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock").exists())
        (proj / "pnpm-lock.yaml").write_text(lock_a, encoding="utf-8")
        # the tree still matches lockfile A here; pnpm decides, and it says in sync
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.argv(), ["exec vitest"])

    def test_explicit_install_waits_behind_a_live_lock_and_project_commands_wait_behind_an_explicit_install(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        holder = subprocess.Popen(["sleep", "30"])
        try:
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            r = self.fx.run("pnpm", "add", "x", cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 1)
        self.assertIn(f"another lane install (pid {holder.pid}) has held", r.stderr)
        self.assertEqual(self.fx.calls(), [])
        (lock / "pid").unlink()
        lock.rmdir()
        results: list[subprocess.CompletedProcess[str]] = []
        guard = threading.Lock()

        def call(argv: list[str], sleep: str) -> None:
            r = self.fx.run("pnpm", *argv, cwd=proj, FAKE_PNPM_SLEEP=sleep)
            with guard:
                results.append(r)

        t1 = threading.Thread(target=call, args=(["install"], "2"))
        t2 = threading.Thread(target=call, args=(["exec", "vitest"], "0"))
        t1.start()
        time.sleep(0.5)
        t2.start()
        t1.join(timeout=60)
        t2.join(timeout=60)
        for r in results:
            self.assertEqual(r.returncode, 0, r.stderr)
        # the explicit install finished first and left the lane in sync: no frozen install followed
        self.assertEqual(self.fx.argv(), ["install", "exec vitest"])

    def test_first_install_in_a_directory_without_a_lockfile_makes_that_directory_the_lane(self) -> None:
        d = self.fx.root / "fresh"
        d.mkdir()
        (d / "package.json").write_text('{"name":"f","private":true}\n', encoding="utf-8")
        r = self.fx.run("pnpm", "install", cwd=d)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((d / "pnpm-lock.yaml").exists())
        self.assertFalse((d / "node_modules" / ".gc-lane-deps.lock").exists())
        self.fx.reset()
        r = self.fx.run("pnpm", "test", cwd=d)
        self.assertEqual(self.fx.argv(), ["test"])


class PnpmShimGateRoundTests(unittest.TestCase):
    """Rows for the codex gate rounds 1 to 8 that are not covered above."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self.tmp.name), real_node=True)

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def test_lifecycle_script_that_reenters_the_wrapper_during_the_install_does_not_deadlock(self) -> None:
        proj = self.fx.project()
        r = self.fx.run(
            "pnpm", "exec", "vitest", cwd=proj,
            FAKE_PNPM_HOOK=str(self.fx.shims / "pnpm"), GC_TOOLCHAIN_LANE_DEPS_WAIT="8",
        )
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertNotIn("waiting for another caller", r.stderr)
        self.assertEqual(self.fx.argv(), ["install --frozen-lockfile", "run build", "exec vitest"])
        # the descendant asked pnpm nothing: the parent holds the lane
        self.assertEqual(len(self.fx.checks()), 2)
        # the bypass is scoped to that lane: a sibling project still gets its own install
        other = self.fx.project("other")
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=other, GC_TOOLCHAIN_LANE_DEPS_INSTALLING=str(proj.resolve()))
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.argv(), ["install --frozen-lockfile", "exec vitest"])

    def test_two_waiters_seeing_one_dead_owner_clear_it_once_and_never_move_a_live_lock(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        results: list[subprocess.CompletedProcess[str]] = []
        guard = threading.Lock()

        def call(name: str) -> None:
            r = self.fx.run(
                "pnpm", "exec", name, cwd=proj,
                FAKE_PNPM_SLEEP="3", GC_TOOLCHAIN_TEST_PAUSE_BEFORE_RECLAIM="1",
            )
            with guard:
                results.append(r)

        threads = [threading.Thread(target=call, args=(n,)) for n in ("vitest", "eslint")]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=60)
        self.assertEqual(len(results), 2)
        for r in results:
            self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.fx.installs()), 1, self.fx.calls())
        aside = [p for p in (proj / "node_modules").iterdir() if p.name.startswith(".gc-lane-deps.lock.stale-")]
        self.assertEqual(len(aside), 1, aside)
        self.assertEqual((aside[0] / "pid").read_text(encoding="utf-8").strip(), str(dead.pid))
        self.assertFalse(lock.exists())
        self.assertFalse((proj / "node_modules" / ".gc-lane-deps.lock.reclaim").exists())

    def test_reclaim_reread_finds_the_lock_live_again_and_moves_nothing(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        holder = subprocess.Popen(["sleep", "30"])
        try:
            proc = subprocess.Popen(
                [str(self.fx.shims / "pnpm"), "exec", "vitest"], cwd=str(proj),
                env=self.fx.env(GC_TOOLCHAIN_TEST_PAUSE_IN_RECLAIM="2", GC_TOOLCHAIN_LANE_DEPS_WAIT="4"),
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            )
            time.sleep(1)
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            out, err = proc.communicate(timeout=60)
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(proc.returncode, 1, err)
        self.assertNotIn("moved aside", err)
        self.assertIn(f"another lane install (pid {holder.pid}) has held", err)
        self.assertTrue(lock.exists())
        self.assertEqual((lock / "pid").read_text(encoding="utf-8").strip(), str(holder.pid))

    def test_stuck_reclaim_lock_fails_closed_when_old(self) -> None:
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        reclaim = proj / "node_modules" / ".gc-lane-deps.lock.reclaim"
        reclaim.mkdir()
        old = time.time() - 180
        os.utime(reclaim, (old, old))
        r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="3")
        self.assertEqual(r.returncode, 1)
        self.assertIn("stale reclaim lock", r.stderr)
        self.assertEqual(self.fx.calls(), [])
        self.assertTrue(lock.exists())

    def test_ignore_workspace_makes_the_standalone_project_the_lane_for_probe_lock_and_install(self) -> None:
        """Round 15: a standalone fixture project inside a workspace, run with
        --ignore-workspace, is probed, locked and installed as itself; without
        the flag the same directory belongs to the workspace above it."""
        ws = self.fx.root / "ws"
        ws.mkdir()
        (ws / "pnpm-workspace.yaml").write_text("packages:\n  - packages/*\n", encoding="utf-8")
        (ws / "package.json").write_text('{"name":"ws","private":true}\n', encoding="utf-8")
        (ws / "pnpm-lock.yaml").write_text("lockfileVersion: '9.0'\n", encoding="utf-8")
        fixture = self.fx.project("ws/fixtures/standalone")
        r = self.fx.run("pnpm", "--ignore-workspace", "exec", "vitest", cwd=fixture)
        self.assertEqual(r.returncode, 0, r.stderr)
        # every question and the one install carry the flag and run in the fixture
        self.assertEqual(
            self.fx.checks(),
            [f"{fixture.resolve()}|unset|--ignore-workspace {CHECK}"] * 2,
        )
        self.assertEqual(self.fx.calls(), [
            f"{fixture.resolve()}|false|--ignore-workspace install --frozen-lockfile",
            f"{fixture.resolve()}|false|--ignore-workspace exec vitest",
        ])
        self.assertTrue(in_sync(fixture))
        self.assertFalse((ws / "node_modules").exists())  # the workspace was neither locked nor installed
        self.assertFalse((fixture / "node_modules" / ".gc-lane-deps.lock").exists())
        # the prepared project stays prepared: one question, no install
        self.fx.reset()
        r = self.fx.run("pnpm", "--ignore-workspace", "run", "test", cwd=fixture)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(len(self.fx.checks()), 1)
        self.assertEqual(self.fx.argv(), ["--ignore-workspace run test"])
        # without the flag the lane is the workspace root: the install lands there
        other = self.fx.project("ws/fixtures/other")
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "vitest", cwd=other)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [f"{ws.resolve()}|false|install --frozen-lockfile"])
        self.assertFalse(in_sync(other))
        # a mutate with the flag holds the fixture's own lock, never the workspace's:
        # a live holder on the workspace lock does not stop it, one on the fixture does
        ws_lock = ws / "node_modules" / ".gc-lane-deps.lock"
        fx_lock = fixture / "node_modules" / ".gc-lane-deps.lock"
        holder = subprocess.Popen(["sleep", "30"])
        try:
            ws_lock.mkdir(parents=True)
            (ws_lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            self.fx.reset()
            r1 = self.fx.run("pnpm", "--ignore-workspace", "add", "left-pad", cwd=fixture, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
            fx_lock.mkdir(parents=True)
            (fx_lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            r2 = self.fx.run("pnpm", "--ignore-workspace", "add", "left-pad", cwd=fixture, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
            r3 = self.fx.run("pnpm", "--ignore-workspace", "exec", "vitest", cwd=fixture, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r1.returncode, 0, r1.stderr)
        self.assertEqual(r2.returncode, 1, r2.stderr)
        self.assertIn(f"has held {fx_lock.resolve()}", r2.stderr)
        self.assertEqual(r3.returncode, 0, r3.stderr)  # prepared: pnpm says yes, no lock needed
        self.assertEqual(self.fx.argv(), ["--ignore-workspace add left-pad", "--ignore-workspace exec vitest"])
        # every spelling pnpm 11.20 honours (measured: the flag, =true, a literal true after
        # it; =false, a literal false and --no-ignore-workspace undo it; the last one wins;
        # after `run` as well), and -w moves nothing under the flag (pnpm refuses it outside
        # a workspace)
        for n, argv in enumerate((
            ["--ignore-workspace=true", "exec", "vitest"],
            ["--ignore-workspace", "true", "exec", "vitest"],
            ["run", "--ignore-workspace", "test"],
            ["--ignore-workspace", "-w", "exec", "vitest"],
            ["-w", "--ignore-workspace", "exec", "vitest"],
            ["--ignore-workspace=false", "--ignore-workspace", "exec", "vitest"],
            ["--no-ignore-workspace", "--ignore-workspace", "exec", "vitest"],
        )):
            proj = self.fx.project(f"ws/fixtures/f{n}")
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.installs(), [f"{proj.resolve()}|false|--ignore-workspace install --frozen-lockfile"], argv)
            self.assertTrue(in_sync(proj), argv)
            self.assertEqual(self.fx.argv()[-1], " ".join(argv), argv)
        # the forms pnpm does not read (measured) select nothing here either
        for argv in (
            ["--ignore-workspace", "false", "exec", "vitest"],
            ["--ignore-workspace", "--no-ignore-workspace", "exec", "vitest"],
            ["--ignore-workspace", "--ignore-workspace=false", "exec", "vitest"],
            ["--config.ignore-workspace=true", "exec", "vitest"],
        ):
            proj = self.fx.project(f"ws/fixtures/g{len(argv)}{argv[1][:4]}")
            (ws / "node_modules" / ".fake-state").unlink(missing_ok=True)
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.installs(), [f"{ws.resolve()}|false|install --frozen-lockfile"], argv)
            self.assertFalse(in_sync(proj), argv)
        # a standalone directory with no lockfile of its own is no lane: the command runs as is
        bare = ws / "fixtures" / "bare"
        bare.mkdir()
        (bare / "package.json").write_text('{"name":"bare"}\n', encoding="utf-8")
        self.fx.reset()
        r = self.fx.run("pnpm", "--ignore-workspace", "exec", "vitest", cwd=bare)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.all_calls(), [f"{bare.resolve()}|false|--ignore-workspace exec vitest"])

    def test_option_values_are_read_by_pnpm_own_table_so_an_install_behind_them_is_a_mutate(self) -> None:
        """Round 16: `--child-concurrency 1 install`, `--recursive false
        install` and their kin are installs (pnpm's exploratory parse
        swallows the value, a boolean swallows a literal true/false), so
        they hold the lane lock and never run the frozen install first."""
        proj = self.fx.project()
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        holder = subprocess.Popen(["sleep", "60"])
        try:
            lock.mkdir(parents=True)
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            mutates = (
                ["--child-concurrency", "1", "install"],
                ["--recursive", "false", "install"],
                ["-r", "false", "install"],
                ["--frozen-lockfile", "false", "install"],
                ["--use-stderr", "false", "install"],
                ["--color", "auto", "install"],
                ["--link-workspace-packages", "deep", "install"],
                ["--network-concurrency", "4", "add", "x"],
                ["--loglevel", "warn", "install"],
                ["--reporter", "append-only", "install"],
                ["-s", "install"],                       # -s is --reporter=silent: no value
                ["-rw", "install"],                      # a run of one-letter shorthands
                ["--child-conc", "1", "install"],        # a unique prefix of an option name
                ["--no-frozen-lockfile", "install"],
                ["--no-frozen-lockfile", "true", "install"],
                ["--", "install"],                       # `--` ends the options; the command follows
                ["--filter", "--dir", str(proj), "install"],   # a string option never swallows an option-like token
                ["--store-dir", "--", "install"],        # nor a lone `--`
                ["--child-concurrency", "--", "install"],
                ["--dir", str(proj), "install"],
            )
            for argv in mutates:
                self.fx.reset()
                r = self.fx.run("pnpm", *argv, cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="1")
                self.assertEqual(r.returncode, 1, (argv, r.stderr))
                self.assertIn(f"has held {lock.resolve()}", r.stderr, argv)
                self.assertEqual(self.fx.all_calls(), [], argv)
            # the same values in front of a project command wait for the lock too (a project
            # command asks pnpm first: the fake's answer is stale here, so the lock is wanted)
            for argv in (["--child-concurrency", "1", "exec", "vitest"], ["--recursive", "false", "run", "test"], ["-r", "false", "test"]):
                self.fx.reset()
                r = self.fx.run("pnpm", *argv, cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="1")
                self.assertEqual(r.returncode, 1, (argv, r.stderr))
                self.assertEqual(self.fx.argv(), [], argv)
                self.assertEqual(len(self.fx.checks()), 1, argv)
            # `--recursive=maybe install`: nopt leaves `maybe` positional, so pnpm's command is
            # `maybe` (a script name), never install: no lock, pnpm's own error
            self.fx.reset()
            r = self.fx.run("pnpm", "--recursive=maybe", "install", cwd=proj, GC_TOOLCHAIN_LANE_DEPS_WAIT="1")
            self.assertEqual(r.returncode, 1, r.stderr)
            self.assertEqual(self.fx.argv(), [])   # project: asked pnpm, stale, waited for the lock
        finally:
            holder.kill()
            holder.wait()
        # without a holder the install runs where typed, arguments unchanged, once
        self.fx.reset()
        r = self.fx.run("pnpm", "--child-concurrency", "1", "install", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.all_calls(), [f"{proj.resolve()}|false|--child-concurrency 1 install"])
        self.assertFalse(lock.exists())
        # `run`'s own options up to the script name: --resume-from and --workspace-concurrency
        # take a value there, so the script is the token after; help anywhere before it is info
        self.fx.reset()
        r = self.fx.run("pnpm", "run", "--resume-from", "pkg", "--workspace-concurrency", "2", "--dir", str(proj), "build", cwd=self.fx.root)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.checks()[0].split("|", 1)[0], str(proj.resolve()))
        # after `exec` pnpm reads no options: `exec -C x vitest` runs the bin `-C`, so the
        # target is the working directory, never x
        other = self.fx.project("other")
        self.fx.reset()
        r = self.fx.run("pnpm", "exec", "--dir", str(other), "vitest", cwd=proj)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.checks()[0].split("|", 1)[0], str(proj.resolve()))
        self.assertFalse((other / "node_modules").exists())
        # an abbreviated global option and a shorthand prefix are read as pnpm reads them
        ws = self.fx.root / "ws"
        ws.mkdir()
        (ws / "pnpm-workspace.yaml").write_text("packages:\n  - packages/*\n", encoding="utf-8")
        (ws / "package.json").write_text('{"name":"ws","private":true}\n', encoding="utf-8")
        (ws / "pnpm-lock.yaml").write_text("lockfileVersion: '9.0'\n", encoding="utf-8")
        fixture = self.fx.project("ws/fixtures/standalone")
        self.fx.reset()
        r = self.fx.run("pnpm", "--ignore-w", "--verb", "exec", "vitest", cwd=fixture)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [f"{fixture.resolve()}|false|--ignore-workspace install --frozen-lockfile"])
        self.assertEqual(self.fx.argv()[-1], "--ignore-w --verb exec vitest")

    def test_dir_wins_over_prefix_wherever_it_stands_and_a_refused_lock_fails_at_once(self) -> None:
        """Round 17: pnpm acts in --dir (-C) whatever its place beside --prefix
        (measured on 11.20), so that is the lane the wrapper prepares and locks;
        a lock mkdir that node_modules refuses is a permission failure, not a
        holder to wait ten minutes for."""
        a = self.fx.project("a")
        b = self.fx.project("b")
        for argv in (
            ["--dir", "a", "--prefix", "b", "exec", "vitest"], ["--prefix", "b", "--dir", "a", "exec", "vitest"],
            ["-C", "a", "--prefix", "b", "run", "test"], ["--prefix", "b", "-C=a", "test"],
            ["--prefix", "b", "--dir", "b", "--dir", "a", "exec", "vitest"],
        ):
            (a / "node_modules" / ".fake-state").unlink(missing_ok=True)
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=self.fx.root)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.installs(), [f"{a.resolve()}|false|install --frozen-lockfile"], argv)
            self.assertEqual(self.fx.checks()[0].split("|", 1)[0], str(a.resolve()), argv)
            self.assertFalse((b / "node_modules").exists(), argv)
        # --prefix alone still selects; the last of two --prefix wins
        self.fx.reset()
        r = self.fx.run("pnpm", "--prefix", "a", "--prefix", "b", "exec", "vitest", cwd=self.fx.root)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertEqual(self.fx.installs(), [f"{b.resolve()}|false|install --frozen-lockfile"])
        # a mutate holds the --dir project's lock, not the --prefix one's
        lock_a = a / "node_modules" / ".gc-lane-deps.lock"
        holder = subprocess.Popen(["sleep", "30"])
        try:
            lock_a.mkdir(parents=True)
            (lock_a / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            r = self.fx.run("pnpm", "--prefix", "b", "--dir", "a", "add", "x", cwd=self.fx.root, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 1, r.stderr)
        self.assertIn(f"has held {lock_a.resolve()}", r.stderr)
        # node_modules that refuses the lock: the command fails at once, naming the refusal,
        # never waiting LANE_DEPS_WAIT for a holder that does not exist
        c = self.fx.project("c")
        (c / "node_modules").mkdir()
        (c / "node_modules").chmod(0o555)
        self.fx.reset()
        try:
            started = time.monotonic()
            r = self.fx.run("pnpm", "exec", "vitest", cwd=c)
            elapsed = time.monotonic() - started
        finally:
            (c / "node_modules").chmod(0o755)
        self.assertEqual(r.returncode, 1, r.stderr)
        self.assertIn("cannot create", r.stderr)
        self.assertIn("Permission denied", r.stderr)
        self.assertNotIn("waiting for another caller", r.stderr)
        self.assertLess(elapsed, 20)
        self.assertEqual(self.fx.argv(), [])

    def test_a_lock_taken_over_by_another_pid_is_not_released_by_the_first(self) -> None:
        proj = self.fx.project()
        holder = subprocess.Popen(["sleep", "30"])
        try:
            r = self.fx.run("pnpm", "exec", "vitest", cwd=proj, FAKE_PNPM_TAKEOVER_PID=str(holder.pid))
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 0, r.stderr)
        lock = proj / "node_modules" / ".gc-lane-deps.lock"
        self.assertTrue(lock.exists())
        self.assertEqual((lock / "pid").read_text(encoding="utf-8").strip(), str(holder.pid))

    def test_option_values_and_prefixes_never_stand_in_for_the_subcommand(self) -> None:
        proj = self.fx.project()
        for argv in (
            ["--filter", "app", "install"], ["-F", "app", "add", "x"], ["--filter=app", "install"],
            ["--loglevel", "warn", "--reporter", "silent", "install", "--frozen-lockfile"],
            ["-r", "--filter", "app", "outdated"], ["with", "current", "list"],
        ):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.argv(), [" ".join(argv)], argv)
            self.assertEqual(self.fx.checks(), [], argv)
        for argv in (["--filter", "app", "run", "test"], ["--loglevel", "warn", "vitest", "run"], ["with", "current", "exec", "vitest"]):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.argv()[-1], " ".join(argv), argv)
            self.assertGreaterEqual(len(self.fx.checks()), 1, argv)

    def test_directory_options_anywhere_in_pnpm_option_scope_select_the_lane(self) -> None:
        parent = self.fx.root / "parent"
        parent.mkdir()
        front = self.fx.project("parent/frontend")
        # a project command from a directory without a lockfile installs the target's lane
        for argv in (
            ["-C", "frontend", "test"], ["--dir", "frontend", "test"], ["--dir=frontend", "run", "lint"],
            [f"-C={front}", "exec", "vitest"], ["--prefix", "frontend", "test"], ["--prefix=frontend", "test"],
            ["run", "--dir", "frontend", "build"], ["-C", "frontend", "exec", "vitest"],
            ["run", "--dir=frontend", "--if-present", "lint"], ["run-script", "-C", "frontend", "test"],
            ["--child-concurrency", "1", "-C", "frontend", "test"], ["run", "--resume-from", "frontend", "-C", "frontend", "build"],
        ):   # (`exec -C frontend vitest` is not among them: after `exec` pnpm reads no options, measured; see round 16)
            (front / "node_modules" / ".fake-state").unlink(missing_ok=True)
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=parent)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.calls(), [f"{front.resolve()}|false|install --frozen-lockfile", f"{parent.resolve()}|false|{' '.join(argv)}"], argv)
            self.assertEqual(self.fx.checks()[0].split("|", 1)[0], str(front.resolve()), argv)
            self.assertFalse((parent / "node_modules").exists(), argv)
        # an explicit mutation targeting another project holds that project's lock and runs where typed
        lock = front / "node_modules" / ".gc-lane-deps.lock"
        holder = subprocess.Popen(["sleep", "30"])
        try:
            lock.mkdir()
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            self.fx.reset()
            r = self.fx.run("pnpm", "add", "--dir", "frontend", "x", cwd=parent, GC_TOOLCHAIN_LANE_DEPS_WAIT="2")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.returncode, 1)
        self.assertIn(f"another lane install (pid {holder.pid}) has held {lock.resolve()}", r.stderr)
        self.assertEqual(self.fx.calls(), [])
        (lock / "pid").unlink()
        lock.rmdir()
        # the last -C wins, and a first install without a lockfile makes the target the lane
        fresh = parent / "fresh"
        fresh.mkdir()
        r = self.fx.run("pnpm", "-C", "frontend", "-C", "fresh", "install", cwd=parent)
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertTrue((fresh / "pnpm-lock.yaml").exists())

    def test_arguments_after_the_script_or_bin_name_belong_to_it(self) -> None:
        proj = self.fx.project()
        elsewhere = self.fx.project("elsewhere")
        for argv in (["run", "build", "--dir", "../elsewhere"], ["exec", "vitest", "-C", "../elsewhere"], ["vitest", "run", "--dir=../elsewhere"], ["test", "--", "-C", "../elsewhere"], ["run", "build", "--prefix", "../elsewhere"]):
            self.fx.reset()
            r = self.fx.run("pnpm", *argv, cwd=proj)
            self.assertEqual(r.returncode, 0, (argv, r.stderr))
            self.assertEqual(self.fx.argv()[-1], " ".join(argv), argv)
            self.assertEqual(self.fx.checks()[0].split("|", 1)[0], str(proj.resolve()), argv)
            self.assertFalse((elsewhere / "node_modules").exists(), argv)


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

    def test_an_interrupted_install_lock_is_moved_aside_and_the_install_proceeds(self) -> None:
        dist = fake_dist(self.fx.root / "dist", "1.2.3")
        lock = self.node_root / ".installing-v1.2.3"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST=dist)
        self.assertEqual(r.stdout, "downloaded node 1.2.3 -v\n", r.stderr)
        self.assertIn("moved aside an interrupted node v1.2.3 install lock", r.stderr)
        self.assertFalse(lock.exists())
        aside = [p for p in self.node_root.iterdir() if p.name.startswith(".installing-v1.2.3.stale-")]
        self.assertEqual(len(aside), 1)
        # a live lock is waited for; the caller falls through after the wait when nothing appears
        lock.mkdir()
        holder = subprocess.Popen(["sleep", "30"])
        try:
            (lock / "pid").write_text(f"{holder.pid}\n", encoding="utf-8")
            fake_node_tree(self.node_root, "9.9.9")  # unrelated version
            r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST="file:///nonexistent")
        finally:
            holder.kill()
            holder.wait()
        self.assertEqual(r.stdout, "downloaded node 1.2.3 -v\n")  # already installed above: no wait needed

    def test_two_waiters_seeing_one_dead_install_owner_clear_it_once_and_install_once(self) -> None:
        """Round 15: both callers read the dead owner before either reclaims;
        the reclaim lock lets one move it aside, the other re-reads a live
        lock and waits, and one tree is installed, never a nested one."""
        dist = fake_dist(self.fx.root / "dist", "1.2.3")
        lock = self.node_root / ".installing-v1.2.3"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        results: list[subprocess.CompletedProcess[str]] = []
        guard = threading.Lock()

        def call() -> None:
            r = self.fx.run(
                "node", "-v",
                GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST=dist,
                GC_TOOLCHAIN_TEST_PAUSE_BEFORE_RECLAIM="1",
            )
            with guard:
                results.append(r)

        threads = [threading.Thread(target=call) for _ in range(2)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=60)
        self.assertEqual(len(results), 2)
        for r in results:
            self.assertEqual(r.stdout, "downloaded node 1.2.3 -v\n", r.stderr)
        self.assertEqual(sum(r.stderr.count("moved aside") for r in results), 1, [r.stderr for r in results])
        self.assertEqual(sum(r.stderr.count("installed node v1.2.3") for r in results), 1, [r.stderr for r in results])
        aside = [p for p in self.node_root.iterdir() if p.name.startswith(".installing-v1.2.3.stale-")]
        self.assertEqual(len(aside), 1, aside)
        self.assertEqual((aside[0] / "pid").read_text(encoding="utf-8").strip(), str(dead.pid))
        # one install tree, no lock, no reclaim lock, no stage left behind
        self.assertEqual(sorted(p.name for p in self.node_root.iterdir()), [aside[0].name, "v1.2.3"])
        self.assertEqual(sorted(p.name for p in (self.node_root / "v1.2.3").iterdir()), ["bin"])

    def test_a_stuck_reclaim_lock_is_reported_and_the_wrapper_falls_through(self) -> None:
        dist = fake_dist(self.fx.root / "dist", "1.2.3")
        lock = self.node_root / ".installing-v1.2.3"
        lock.mkdir(parents=True)
        dead = subprocess.Popen(["true"])
        dead.wait()
        (lock / "pid").write_text(f"{dead.pid}\n", encoding="utf-8")
        reclaim = self.node_root / ".installing-v1.2.3.reclaim"
        reclaim.mkdir()
        old = time.time() - 180
        os.utime(reclaim, (old, old))
        r = self.fx.run("node", "-v", GC_TOOLCHAIN_NODE_VERSION="1.2.3", GC_TOOLCHAIN_NODE_DIST=dist)
        self.assertEqual(r.stdout, "machine node -v\n", r.stderr)
        self.assertIn("stale reclaim lock", r.stderr)
        self.assertIn("using the node on PATH", r.stderr)
        self.assertTrue(lock.exists())
        self.assertTrue(reclaim.exists())
        self.assertFalse((self.node_root / "v1.2.3").exists())

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
