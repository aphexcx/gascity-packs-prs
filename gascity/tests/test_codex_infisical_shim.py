"""Contract tests for gascity/assets/scripts/codex-infisical-shim.sh.

Each test installs the shim the way a city does (a copy named `codex` in its
own directory, optionally with a `codex.env` beside it), puts a fake `codex`
further down PATH that records its argv and environment, and runs the shim
with a throwaway HOME. One row per contract line in the script header.
"""

from __future__ import annotations

import os
import pathlib
import re
import shutil
import stat
import subprocess
import tempfile
import unittest


PACK = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = PACK / "assets" / "scripts" / "codex-infisical-shim.sh"
README = PACK / "README.md"
SYSTEM_PATH = "/usr/bin:/bin"

FAKE_CODEX = """#!/bin/sh
out="$SHIM_TEST_OUT"
printf '%s\\n' "$0" > "$out/argv0"
: > "$out/argv"
for a in "$@"; do printf '%s\\n' "$a" >> "$out/argv"; done
printf '%s\\n' "$PATH" > "$out/path"
printf '%s\\n' "${INFISICAL_TOKEN-<unset>}" > "$out/token"
printf '%s\\n' "${SHIM_TEST_MARKER-<unset>}" > "$out/marker"
printf '%s\\n' "${INFISICAL_UNIVERSAL_AUTH_CLIENT_ID-<unset>}" > "$out/client_id"
printf '%s\\n' "${entry-<unset>}" > "$out/entry"
printf '%s\\n' "${line-<unset>}" > "$out/line"
exit 0
"""


class Fixture:
    def __init__(self, root: pathlib.Path) -> None:
        self.root = root
        self.home = root / "home"
        self.shim_dir = root / "shims" / "codex-astra"
        self.bin = root / "bin"
        self.out = root / "out"
        for d in (self.home, self.shim_dir, self.bin, self.out):
            d.mkdir(parents=True)
        self.shim = self.shim_dir / "codex"
        shutil.copyfile(SCRIPT, self.shim)
        self.shim.chmod(0o755)
        self.fake = self.write_exec(self.bin / "codex", FAKE_CODEX)

    def write_exec(self, path: pathlib.Path, body: str) -> pathlib.Path:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(body, encoding="utf-8")
        path.chmod(path.stat().st_mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
        return path

    def write_token_sh(self, body: str) -> pathlib.Path:
        d = self.home / ".config" / "infisical-agent"
        d.mkdir(parents=True, exist_ok=True)
        p = d / "token.sh"
        p.write_text(body, encoding="utf-8")
        return p

    def write_sidecar(self, text: str) -> pathlib.Path:
        p = self.shim_dir / "codex.env"
        p.write_text(text, encoding="utf-8")
        return p

    def run(self, *args: str, path: str | None = None, env_extra: dict[str, str] | None = None,
            argv0: str | None = None, cwd: pathlib.Path | None = None,
            timeout: float = 20.0) -> subprocess.CompletedProcess[str]:
        env = {
            "HOME": str(self.home),
            "PATH": path if path is not None else f"{self.shim_dir}:{self.bin}:{SYSTEM_PATH}",
            "SHIM_TEST_OUT": str(self.out),
            **(env_extra or {}),
        }
        return subprocess.run(
            [argv0 or str(self.shim), *args],
            cwd=str(cwd or self.root),
            env=env,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def recorded(self, name: str) -> str:
        return (self.out / name).read_text(encoding="utf-8").rstrip("\n")

    def recorded_argv(self) -> list[str]:
        text = (self.out / "argv").read_text(encoding="utf-8")
        return text.split("\n")[:-1] if text else []

    def ran_fake(self) -> bool:
        return (self.out / "argv").exists()


def readme_section() -> str:
    text = README.read_text(encoding="utf-8")
    return text.split("## Codex provider shim", 1)[1].split("\n## ", 1)[0]


def fenced_blocks(section: str, lang: str) -> list[str]:
    return re.findall(rf"```{lang}\n(.*?)```", section, re.S)


class CodexInfisicalShimTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.fx = Fixture(pathlib.Path(self._tmp.name).resolve())

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def assert_exec_ok(self, proc: subprocess.CompletedProcess[str], argv: list[str]) -> None:
        self.assertEqual(proc.returncode, 0, f"stdout={proc.stdout!r}\nstderr={proc.stderr!r}")
        self.assertTrue(self.fx.ran_fake(), "the real codex did not run")
        self.assertEqual(self.fx.recorded_argv(), argv)

    def assert_refused(self, proc: subprocess.CompletedProcess[str], needle: str) -> None:
        self.assertEqual(proc.returncode, 127, f"stdout={proc.stdout!r}\nstderr={proc.stderr!r}")
        self.assertIn(needle, proc.stderr)
        self.assertFalse(self.fx.ran_fake(), "the fake codex ran although the shim should have refused")

    # --- canonical file and recipe ----------------------------------------------------

    def test_canonical_file_is_executable_bash_and_documents_its_install(self) -> None:
        self.assertTrue(os.access(SCRIPT, os.X_OK), "canonical shim must carry the executable bit")
        text = SCRIPT.read_text(encoding="utf-8")
        self.assertTrue(text.startswith("#!/bin/bash\n"))
        for needle in (
            'mkdir -p "$CITY/.gc/shims/codex-astra"',
            "install -m 0755 path/to/gascity/assets/scripts/codex-infisical-shim.sh",
            '"$CITY/.gc/shims/codex-astra/codex"',
            "CODEX_SHIM_PATH_PREPEND",
            "CODEX_SHIM_EXEC",
            "INFISICAL_PROJECT_ID",
            "| md5 -q",
            "= <that md5>",
        ):
            self.assertIn(needle, text)
        self.assertNotIn("set -x", text)
        self.assertNotIn("eval ", text)

    def test_readme_carries_the_install_recipe(self) -> None:
        section = readme_section()
        for needle in (
            "assets/scripts/codex-infisical-shim.sh",
            'mkdir -p "$CITY/.gc/shims/codex-astra"',
            ".gc/shims/codex-astra/codex",
            "codex.env",
            "CODEX_SHIM_PATH_PREPEND",
            "CODEX_SHIM_EXEC",
            "resume_command",
            "INFISICAL_PROJECT_ID",
            "| md5 -q",
            "= <that md5>",
            "fail-open",
        ):
            self.assertIn(needle, section)

    def test_readme_install_recipe_works_from_a_fresh_directory(self) -> None:
        if shutil.which("md5") is None:
            self.skipTest("md5 (macOS) not on PATH")
        blocks = fenced_blocks(readme_section(), "sh")
        self.assertEqual(len(blocks), 2, "README section: one install block, one verify block")
        install, verify = blocks
        city = self.fx.root / "fresh-city"
        city.mkdir()
        install = install.replace("path/to/gascity/", f"{PACK}/")
        proc = subprocess.run(
            ["/bin/sh", "-e", "-c", install], cwd=str(self.fx.root), env={"PATH": SYSTEM_PATH, "CITY": str(city)},
            capture_output=True, text=True,
        )
        self.assertEqual(proc.returncode, 0, proc.stderr)
        installed = city / ".gc" / "shims" / "codex-astra" / "codex"
        self.assertEqual(installed.read_bytes(), SCRIPT.read_bytes())
        self.assertTrue(os.access(installed, os.X_OK))
        self.assertEqual(
            (installed.parent / "codex.env").read_text(encoding="utf-8"),
            "CODEX_SHIM_PATH_PREPEND=/abs/path/to/city/.gc/shims/toolchain\nCODEX_SHIM_EXEC=npx -y @openai/codex@0.153.3\n",
        )
        # The verify block: the recorded md5 is a literal, so the check fails when
        # the installed file is missing instead of matching two empty strings.
        canonical_md5 = subprocess.run(["md5", "-q", str(SCRIPT)], capture_output=True, text=True, check=True).stdout.strip()
        check = verify.splitlines()[-1].replace("<that md5>", canonical_md5)
        self.assertTrue(check.startswith('test "$(md5 -q "$CITY/.gc/shims/codex-astra/codex")" ='), check)
        env = {"PATH": f"{SYSTEM_PATH}:{os.path.dirname(shutil.which('md5'))}", "CITY": str(city)}
        self.assertEqual(subprocess.run(["/bin/sh", "-c", check], env=env).returncode, 0)
        env["CITY"] = str(self.fx.root / "no-such-city")
        self.assertNotEqual(subprocess.run(["/bin/sh", "-c", check], env=env, capture_output=True).returncode, 0)
        self.assertEqual(subprocess.run(["/bin/sh", "-c", check], env={"PATH": "/nonexistent", "CITY": str(city)},
                                        capture_output=True).returncode, 1, "a missing md5 tool must fail the check")

    # --- fail-open token, helper isolated ---------------------------------------------

    def test_absent_token_sh_execs_codex_with_argv_intact_and_no_warning(self) -> None:
        proc = self.fx.run("-p", "city", "--model", "gpt-6-astra", "a b", "", "-", "*")
        self.assert_exec_ok(proc, ["-p", "city", "--model", "gpt-6-astra", "a b", "", "-", "*"])
        self.assertEqual(proc.stderr, "")
        self.assertEqual(self.fx.recorded("token"), "<unset>")

    def test_present_token_sh_is_sourced_and_only_its_token_crosses_over(self) -> None:
        self.fx.write_token_sh(
            "set -a\nINFISICAL_UNIVERSAL_AUTH_CLIENT_ID=cid\nset +a\n"
            "export INFISICAL_TOKEN=tok-marker-123\nexport SHIM_TEST_MARKER=must-not-cross\n"
            "echo 'helper noise' ; echo \"$INFISICAL_TOKEN\"\n"
            "unset INFISICAL_UNIVERSAL_AUTH_CLIENT_ID\n"
        )
        proc = self.fx.run("resume", "sess-1")
        self.assert_exec_ok(proc, ["resume", "sess-1"])
        self.assertEqual(proc.stderr, "")
        self.assertEqual(proc.stdout, "", "nothing the helper prints may reach the session output")
        self.assertEqual(self.fx.recorded("token"), "tok-marker-123")
        self.assertEqual(self.fx.recorded("marker"), "<unset>")
        self.assertEqual(self.fx.recorded("client_id"), "<unset>")

    def test_helper_that_leaves_client_secret_exported_does_not_leak_it(self) -> None:
        self.fx.write_token_sh("export INFISICAL_UNIVERSAL_AUTH_CLIENT_ID=cid\nexport INFISICAL_TOKEN=tok\n")
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("token"), "tok")
        self.assertEqual(self.fx.recorded("client_id"), "<unset>")

    def test_failing_token_sh_warns_once_unsets_the_token_and_still_execs(self) -> None:
        self.fx.write_token_sh(
            "export SHIM_TEST_MARKER=partial\nINFISICAL_TOKEN=short\nexport INFISICAL_TOKEN\n"
            "echo 'infisical-agent: universal-auth login failed' >&2\nreturn 1 2>/dev/null || exit 1\n"
        )
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(
            proc.stderr,
            "codex-infisical-shim: WARN Infisical machine-identity login failed; INFISICAL_TOKEN unset\n",
        )
        self.assertEqual(self.fx.recorded("token"), "<unset>")
        self.assertEqual(self.fx.recorded("marker"), "<unset>")

    def test_helper_that_exits_cannot_end_the_shim(self) -> None:
        self.fx.write_token_sh("export INFISICAL_TOKEN=tok\nexit 1\n")
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(proc.stderr.count("WARN"), 1)
        self.assertEqual(self.fx.recorded("token"), "<unset>")

    def test_helper_that_succeeds_without_a_token_warns(self) -> None:
        self.fx.write_token_sh("true\n")
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(proc.stderr.count("WARN"), 1)
        self.assertEqual(self.fx.recorded("token"), "<unset>")

    def test_helper_that_rewrites_argv_cannot_change_what_codex_receives(self) -> None:
        self.fx.write_token_sh("set -- dropped --by helper\nexport INFISICAL_TOKEN=tok\n")
        proc = self.fx.run("-p", "city", "keep")
        self.assert_exec_ok(proc, ["-p", "city", "keep"])
        self.assertEqual(self.fx.recorded("token"), "tok")

    def test_a_set_token_is_kept_and_token_sh_is_not_sourced(self) -> None:
        self.fx.write_token_sh("export INFISICAL_TOKEN=minted\ntouch \"$SHIM_TEST_OUT/sourced\"\n")
        proc = self.fx.run("-p", "city", env_extra={"INFISICAL_TOKEN": "already-there"})
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("token"), "already-there")
        self.assertFalse((self.fx.out / "sourced").exists())

    def test_an_empty_token_is_minted_like_an_unset_one(self) -> None:
        self.fx.write_token_sh("export INFISICAL_TOKEN=minted\n")
        proc = self.fx.run("-p", "city", env_extra={"INFISICAL_TOKEN": ""})
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("token"), "minted")

    def test_unreadable_token_sh_is_treated_as_absent(self) -> None:
        if os.geteuid() == 0:
            self.skipTest("root reads everything")
        p = self.fx.write_token_sh("export INFISICAL_TOKEN=minted\n")
        p.chmod(0)
        try:
            proc = self.fx.run("-p", "city")
        finally:
            p.chmod(0o644)
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(proc.stderr, "")
        self.assertEqual(self.fx.recorded("token"), "<unset>")

    def test_unset_home_skips_the_helper(self) -> None:
        proc = subprocess.run(
            [str(self.fx.shim), "-p", "city"], cwd=str(self.fx.root),
            env={"PATH": f"{self.fx.shim_dir}:{self.fx.bin}:{SYSTEM_PATH}", "SHIM_TEST_OUT": str(self.fx.out)},
            capture_output=True, text=True, timeout=20,
        )
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(proc.stderr, "")

    # --- never execs itself -----------------------------------------------------------

    def test_shim_directory_is_removed_from_path_and_the_next_codex_runs(self) -> None:
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), f"{self.fx.bin}:{SYSTEM_PATH}")
        self.assertEqual(pathlib.Path(self.fx.recorded("argv0")).resolve(), self.fx.fake.resolve())

    def test_invoked_by_bare_name_through_path_lookup_still_finds_the_real_codex(self) -> None:
        proc = self.fx.run("-p", "city", argv0="codex")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), f"{self.fx.bin}:{SYSTEM_PATH}")

    def test_symlinked_and_relative_aliases_of_the_shim_directory_are_pruned_too(self) -> None:
        alias = self.fx.root / "alias-dir"
        alias.symlink_to(self.fx.shim_dir, target_is_directory=True)
        rel = os.path.relpath(self.fx.shim_dir, self.fx.root)
        path = f"{self.fx.shim_dir}:{alias}:{rel}:{self.fx.bin}:{SYSTEM_PATH}"
        proc = self.fx.run("-p", "city", path=path)
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), f"{self.fx.bin}:{SYSTEM_PATH}")

    def test_empty_path_entries_are_kept_unless_they_are_the_shim_directory(self) -> None:
        proc = self.fx.run("-p", "city", path=f":{self.fx.shim_dir}:{self.fx.bin}::{SYSTEM_PATH}:")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), f":{self.fx.bin}::{SYSTEM_PATH}:")
        # An empty entry means the current directory; from inside the shim's
        # directory it resolves to the shim and is pruned like any other alias.
        proc = self.fx.run("-p", "city", path=f":{self.fx.bin}:{SYSTEM_PATH}", cwd=self.fx.shim_dir)
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), f"{self.fx.bin}:{SYSTEM_PATH}")

    def test_no_codex_left_on_path_is_an_error_not_a_loop(self) -> None:
        proc = self.fx.run("-p", "city", path=f"{self.fx.shim_dir}:{SYSTEM_PATH}")
        self.assert_refused(proc, "ERROR no 'codex' on PATH after removing the shim's directory")

    def test_a_symlink_to_the_shim_elsewhere_on_path_is_refused_not_execd(self) -> None:
        link_dir = self.fx.root / "elsewhere"
        link_dir.mkdir()
        (link_dir / "codex").symlink_to(self.fx.shim)
        proc = self.fx.run("-p", "city", path=f"{self.fx.shim_dir}:{link_dir}:{self.fx.bin}:{SYSTEM_PATH}")
        self.assert_refused(proc, "resolves to the shim itself")

    def test_exec_override_naming_the_shim_itself_is_refused(self) -> None:
        proc = self.fx.run("-p", "city", env_extra={"CODEX_SHIM_EXEC": str(self.fx.shim)})
        self.assert_refused(proc, "resolves to the shim itself")

    def test_no_utility_on_path_still_refuses_self_exec_and_still_execs_an_absolute_target(self) -> None:
        proc = self.fx.run("-p", "city", path="/nonexistent", env_extra={"CODEX_SHIM_EXEC": str(self.fx.shim)})
        self.assert_refused(proc, "resolves to the shim itself")
        proc = self.fx.run("-p", "city", path="/nonexistent", env_extra={"CODEX_SHIM_EXEC": str(self.fx.fake)})
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), "/nonexistent")

    def test_shim_that_cannot_locate_itself_refuses(self) -> None:
        # bash -c 'source' style invocation: $0 is a bare name that is not on PATH.
        proc = subprocess.run(
            ["/bin/bash", str(self.fx.shim), "-p", "city"], cwd=str(self.fx.root),
            env={"HOME": str(self.fx.home), "PATH": "/nonexistent", "SHIM_TEST_OUT": str(self.fx.out),
                 "CODEX_SHIM_EXEC": str(self.fx.fake)},
            capture_output=True, text=True, timeout=20,
        )
        self.assert_exec_ok(proc, ["-p", "city"])  # a path with a slash locates itself without PATH
        proc = subprocess.run(
            ["/bin/bash", "-c", '. "$1"', "codex-not-on-path", str(self.fx.shim)], cwd=str(self.fx.root),
            env={"HOME": str(self.fx.home), "PATH": "/nonexistent", "SHIM_TEST_OUT": str(self.fx.out / "none"),
                 "CODEX_SHIM_EXEC": str(self.fx.fake)},
            capture_output=True, text=True, timeout=20,
        )
        self.assertEqual(proc.returncode, 127, proc.stderr)
        self.assertIn("cannot locate the shim itself", proc.stderr)

    # --- settings: environment over <shim>.env ----------------------------------------

    def test_sidecar_sets_the_exec_command_and_the_path_prepend(self) -> None:
        tc = self.fx.root / "toolchain"
        pinned = self.fx.write_exec(tc / "pinned-codex", FAKE_CODEX)
        self.fx.write_sidecar(
            "# per-city settings\n\n"
            f"CODEX_SHIM_PATH_PREPEND={tc}\n"
            "CODEX_SHIM_EXEC=pinned-codex --pinned-flag\n"
        )
        proc = self.fx.run("-p", "city", "x")
        self.assert_exec_ok(proc, ["--pinned-flag", "-p", "city", "x"])
        self.assertEqual(proc.stderr, "")
        self.assertEqual(pathlib.Path(self.fx.recorded("argv0")).resolve(), pinned.resolve())
        self.assertEqual(self.fx.recorded("path"), f"{tc}:{self.fx.bin}:{SYSTEM_PATH}")

    def test_environment_wins_over_the_sidecar(self) -> None:
        other = self.fx.write_exec(self.fx.root / "other" / "env-codex", FAKE_CODEX)
        self.fx.write_sidecar("CODEX_SHIM_EXEC=never-runs\nCODEX_SHIM_PATH_PREPEND=/nonexistent-sidecar\n")
        proc = self.fx.run("-p", "city", env_extra={
            "CODEX_SHIM_EXEC": "env-codex",
            "CODEX_SHIM_PATH_PREPEND": str(other.parent),
        })
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(pathlib.Path(self.fx.recorded("argv0")).resolve(), other.resolve())
        self.assertTrue(self.fx.recorded("path").startswith(f"{other.parent}:"))
        self.assertNotIn("/nonexistent-sidecar", self.fx.recorded("path"))

    def test_an_empty_environment_value_still_wins_over_the_sidecar(self) -> None:
        self.fx.write_sidecar("CODEX_SHIM_EXEC=never-runs\nCODEX_SHIM_PATH_PREPEND=/nonexistent-sidecar\n")
        proc = self.fx.run("-p", "city", env_extra={"CODEX_SHIM_EXEC": "", "CODEX_SHIM_PATH_PREPEND": ""})
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(pathlib.Path(self.fx.recorded("argv0")).resolve(), self.fx.fake.resolve())
        self.assertEqual(self.fx.recorded("path"), f"{self.fx.bin}:{SYSTEM_PATH}")

    def test_a_blank_exec_override_never_runs_a_caller_argument(self) -> None:
        proc = self.fx.run("/bin/echo", "WRONG_EXECUTABLE", env_extra={"CODEX_SHIM_EXEC": "   "})
        self.assert_exec_ok(proc, ["/bin/echo", "WRONG_EXECUTABLE"])
        self.assertEqual(pathlib.Path(self.fx.recorded("argv0")).resolve(), self.fx.fake.resolve())
        self.fx.write_sidecar("CODEX_SHIM_EXEC=\n")
        proc = self.fx.run("/bin/echo", "WRONG_EXECUTABLE")
        self.assert_exec_ok(proc, ["/bin/echo", "WRONG_EXECUTABLE"])

    def test_sidecar_is_not_evaluated_and_quotes_are_stripped(self) -> None:
        canary = self.fx.root / "canary"
        self.fx.write_sidecar(f'CODEX_SHIM_EXEC="$(touch {canary})"\n')
        proc = self.fx.run("-p", "city")
        self.assert_refused(proc, "ERROR no '$(touch' on PATH")
        self.assertFalse(canary.exists(), "sidecar value was evaluated by a shell")
        self.fx.write_sidecar("CODEX_SHIM_EXEC='codex --quoted'\n")
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["--quoted", "-p", "city"])

    def test_exec_override_words_are_not_globbed(self) -> None:
        (self.fx.root / "glob-a").touch()
        proc = self.fx.run("-p", "city", env_extra={"CODEX_SHIM_EXEC": "codex --pattern glob-*"})
        self.assert_exec_ok(proc, ["--pattern", "glob-*", "-p", "city"])

    def test_unknown_sidecar_keys_warn_and_are_ignored(self) -> None:
        self.fx.write_sidecar("BOGUS=1\nno-equals-line\nCODEX_SHIM_EXEC=codex --ok\n")
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["--ok", "-p", "city"])
        self.assertIn("WARN ignoring unknown key BOGUS", proc.stderr)
        self.assertIn("WARN ignoring line without '='", proc.stderr)

    def test_prepend_is_pruned_when_it_names_the_shim_directory(self) -> None:
        self.fx.write_sidecar(f"CODEX_SHIM_PATH_PREPEND={self.fx.shim_dir}\n")
        proc = self.fx.run("-p", "city")
        self.assert_exec_ok(proc, ["-p", "city"])
        self.assertEqual(self.fx.recorded("path"), f"{self.fx.bin}:{SYSTEM_PATH}")

    # --- environment preservation -----------------------------------------------------

    def test_inherited_exports_named_like_scratch_variables_reach_codex_unchanged(self) -> None:
        self.fx.write_sidecar("CODEX_SHIM_EXEC=codex --ok\n")
        self.fx.write_token_sh("export INFISICAL_TOKEN=tok\n")
        proc = self.fx.run("-p", "city", env_extra={"entry": "keep-me", "line": "keep-line", "PATH_PREPEND": "keep-p"})
        self.assert_exec_ok(proc, ["--ok", "-p", "city"])
        self.assertEqual(self.fx.recorded("entry"), "keep-me")
        self.assertEqual(self.fx.recorded("line"), "keep-line")


if __name__ == "__main__":
    unittest.main()
