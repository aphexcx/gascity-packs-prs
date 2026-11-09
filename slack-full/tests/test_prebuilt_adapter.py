"""Prebuilt startup; also run in a Linux container with no Go installed."""
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

PACK = Path(__file__).resolve().parents[1]


class PrebuiltAdapterTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.adapter = self.root / "adapter"
        self.adapter.mkdir()
        shutil.copy(PACK / "adapter/run.sh", self.adapter)
        shutil.copy(PACK / "adapter/go.mod", self.adapter)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        for name in ("bash", "dirname", "basename"):
            (self.bin / name).symlink_to(shutil.which(name))
        self.env_file = self.root / "env"
        self.env_file.write_text("MARKER=from-file\n")
        self.env = {"PATH": str(self.bin), "HOME": str(self.root),
                    "GC_SLACK_ADAPTER_ENV": str(self.env_file)}

    def stub(self, path, marker):
        path.write_text(f'#!/bin/bash\nprintf "%s\\n" "{marker}" "$@" "marker=$MARKER"\n')
        path.chmod(0o755)

    def run_adapter(self):
        return subprocess.run([str(self.adapter / "run.sh"), "--test-arg", "two words"],
                              cwd=self.root, env=self.env, text=True,
                              capture_output=True, timeout=10)

    def test_path_binary_precedes_local_and_preserves_arguments(self):
        self.stub(self.bin / "gc-slack-adapter", "PATH")
        self.stub(self.adapter / "gc-slack-adapter", "LOCAL")
        result = self.run_adapter()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "PATH\n--test-arg\ntwo words\nmarker=from-file\n")
        self.assertIn(f"using {self.bin}/gc-slack-adapter", result.stderr)

    def test_override_precedes_path_and_accepts_relative_filename(self):
        self.stub(self.bin / "gc-slack-adapter", "PATH")
        self.stub(self.root / "chosen binary", "OVERRIDE")
        self.env["GC_SLACK_ADAPTER_BIN"] = "chosen binary"
        result = self.run_adapter()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.startswith("OVERRIDE\n"), result.stdout)

    def test_relative_path_binary(self):
        self.stub(self.bin / "gc-slack-adapter", "PATH")
        self.env["PATH"] = "bin"
        self.assertTrue(self.run_adapter().stdout.startswith("PATH\n"))

    def test_invalid_override_does_not_fall_back(self):
        self.stub(self.bin / "gc-slack-adapter", "PATH")
        self.env["GC_SLACK_ADAPTER_BIN"] = str(self.root / "absent")
        result = self.run_adapter()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("is not an executable file", result.stderr)
        self.assertEqual(result.stdout, "")

    def test_shell_function_cannot_shadow_path_binary(self):
        self.stub(self.bin / "gc-slack-adapter", "PATH")
        self.env["BASH_FUNC_gc-slack-adapter%%"] = "() { exit 89; }"
        self.assertTrue(self.run_adapter().stdout.startswith("PATH\n"))

    def test_missing_explicit_env_still_fails(self):
        self.stub(self.bin / "gc-slack-adapter", "PATH")
        self.env_file.unlink()
        result = self.run_adapter()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("does not exist", result.stderr)
        self.assertEqual(result.stdout, "")

    def test_local_binary_remains_fallback(self):
        self.stub(self.adapter / "gc-slack-adapter", "LOCAL")
        result = self.run_adapter()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.startswith("LOCAL\n"))

    def test_no_binary_and_no_go_has_clear_error(self):
        # The launcher intentionally probes these conventional install paths.
        # Exercise genuine absence in the no-Go Linux proof container.
        candidates = ("/opt/homebrew/bin/go", "/usr/local/go/bin/go", "/usr/local/bin/go",
                      "/usr/bin/go", "/bin/go")
        if any(Path(path).exists() for path in candidates):
            self.skipTest("requires a host without Go at the launcher's fallback paths")
        result = self.run_adapter()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ERROR: no Go toolchain found", result.stderr)


class PrebuiltCLITest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.pack = self.root / "fixture pack"
        shutil.copytree(PACK / "commands", self.pack / "commands")
        shutil.copytree(PACK / "doctor", self.pack / "doctor")
        (self.pack / "cli").mkdir()
        (self.pack / "adapter").mkdir()
        self.bin = self.root / "bin"
        self.bin.mkdir()
        # Inspect signatures without exposing Go or installed pack binaries.
        (self.bin / "head").symlink_to(shutil.which("head"))
        self.env = {"PATH": str(self.bin), "GC_PACK_DIR": str(self.pack)}
        self.args = ["--marker", "two words", "", "*.json"]

    def stub(self, path, marker):
        # No #! signature: sh's executable-text fallback keeps this portable
        # on macOS and the no-Go Linux image without compiling test binaries.
        path.write_text(f': prebuilt test stand-in\nprintf "<%s>\\n" "{marker}" "$@"\nexit 17\n')
        path.chmod(0o755)

    def run_cli(self, entry="_cli"):
        return subprocess.run(
            [shutil.which("sh"), str(self.pack / "commands" / f"{entry}.sh"), *self.args],
            cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)

    def assert_selected(self, marker, entry="_cli"):
        result = self.run_cli(entry)
        self.assertEqual(result.returncode, 17, result.stderr)
        args = self.args if entry == "_cli" else [entry, *self.args]
        self.assertEqual(result.stdout.splitlines(), [f"<{arg}>" for arg in (marker, *args)])
        self.assertEqual(result.stderr, "")
        self.assertFalse((self.pack / "cli/gc-slack-cli").exists())

    def test_cli_path_binary_without_go_preserves_argv_and_exit(self):
        self.stub(self.bin / "gc-slack-cli", "PATH")
        self.assert_selected("PATH")

    def test_import_app_uses_prebuilt_cli(self):
        self.stub(self.bin / "gc-slack-cli", "PATH")
        self.assert_selected("PATH", "import-app")

    def test_cli_override_precedes_path_with_relative_filename(self):
        self.stub(self.bin / "gc-slack-cli", "PATH")
        self.stub(self.root / "chosen binary", "OVERRIDE")
        self.env["GC_SLACK_CLI_BIN"] = "chosen binary"
        self.assert_selected("OVERRIDE")

    def test_cli_invalid_override_never_falls_back(self):
        self.stub(self.bin / "gc-slack-cli", "PATH")
        (self.root / "nonexec").write_text("not executable")
        for override in ("absent", "nonexec", "bin"):
            with self.subTest(override=override):
                self.env["GC_SLACK_CLI_BIN"] = override
                result = self.run_cli()
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("is not an executable file", result.stderr)
                self.assertNotIn("rebuilding", result.stderr)
                self.assertEqual(result.stdout, "")

    def test_cli_empty_and_relative_path_entries(self):
        self.stub(self.root / "gc-slack-cli", "CWD")
        (self.root / "head").symlink_to(shutil.which("head"))
        for search_path in (f":{self.bin}", f"{self.bin}:", f"{self.bin}::{self.bin}", ".", ""):
            with self.subTest(path=search_path):
                self.env["PATH"] = search_path
                self.assert_selected("CWD")

    def test_cli_path_walk_skips_directories_and_nonexecutables(self):
        first = self.root / "first"
        second = self.root / "second"
        first.mkdir()
        second.mkdir()
        (first / "gc-slack-cli").mkdir()
        (second / "gc-slack-cli").write_text("not executable")
        self.stub(self.bin / "gc-slack-cli", "PATH")
        self.env["PATH"] = f"{first}:{second}:{self.bin}"
        self.assert_selected("PATH")

    def test_cli_empty_override_uses_path(self):
        self.stub(self.bin / "gc-slack-cli", "PATH")
        self.env["GC_SLACK_CLI_BIN"] = ""
        self.assert_selected("PATH")

    def legacy(self, path):
        path.write_text('#!/bin/sh\necho LEGACY_WRAPPER_RAN\nexit 99\n')
        path.chmod(0o755)

    def test_cli_local_legacy_wrapper_on_path_is_rebuilt_atomically(self):
        local = self.pack / "cli/gc-slack-cli"
        self.legacy(local)
        self.env["PATH"] = f"{local.parent}:{self.bin}"
        for name in ("mktemp", "mv", "rm", "cp", "chmod"):
            (self.bin / name).symlink_to(shutil.which(name))
        rebuilt = self.root / "rebuilt"
        self.stub(rebuilt, "REBUILT")
        self.env["REBUILT"] = str(rebuilt)
        go = self.bin / "go"
        go.write_text('#!/bin/sh\nset -eu\n'
                      '[ "$1" = build ] && [ "$2" = -o ] && [ "$4" = . ]\n'
                      'case "$3" in "$PWD"/.gc-slack-cli.build.*) ;; *) exit 98 ;; esac\n'
                      '[ ! -s "$3" ]\ncp "$REBUILT" "$3"\nchmod +x "$3"\n')
        go.chmod(0o755)
        result = self.run_cli()
        self.assertEqual(result.returncode, 17, result.stderr)
        self.assertEqual(result.stdout.splitlines(), [f"<{arg}>" for arg in ("REBUILT", *self.args)])
        self.assertIn("rebuilt", result.stderr)
        self.assertEqual(local.read_bytes(), rebuilt.read_bytes())
        self.assertEqual(list(local.parent.glob(".gc-slack-cli.build.*")), [])

    def test_cli_local_legacy_wrapper_on_path_without_go_is_not_run(self):
        local = self.pack / "cli/gc-slack-cli"
        self.legacy(local)
        self.env["PATH"] = f"{local.parent}:{self.bin}"
        result = self.run_cli()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("no Go toolchain found", result.stderr)
        self.assertIn("manual fix:", result.stderr)

    def test_cli_path_legacy_wrapper_is_skipped_for_later_binary(self):
        self.legacy(self.root / "gc-slack-cli")
        self.stub(self.bin / "gc-slack-cli", "PATH")
        self.env["PATH"] = f"{self.root}:{self.bin}"
        self.assert_selected("PATH")

    def test_cli_failed_signature_read_skips_candidate(self):
        self.stub(self.bin / "gc-slack-cli", "PATH")
        (self.bin / "head").unlink()
        (self.bin / "head").write_text('#!/bin/sh\nexit 1\n')
        (self.bin / "head").chmod(0o755)
        result = self.run_cli()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(result.stdout, "")
        self.assertIn("no Go toolchain found", result.stderr)

    def test_cli_explicit_override_accepts_shebang_script(self):
        chosen = self.root / "chosen"
        self.legacy(chosen)
        self.env["GC_SLACK_CLI_BIN"] = str(chosen)
        result = self.run_cli()
        self.assertEqual(result.returncode, 99, result.stderr)
        self.assertEqual(result.stdout, "LEGACY_WRAPPER_RAN\n")

    def test_cli_path_symlink_to_executable(self):
        self.stub(self.root / "chosen", "SYMLINK")
        (self.bin / "gc-slack-cli").symlink_to(self.root / "chosen")
        self.assert_selected("SYMLINK")

    def run_doctor(self):
        return subprocess.run(
            [shutil.which("sh"), str(self.pack / "doctor/check-binaries.sh")],
            cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)

    def test_doctor_accepts_prebuilt_path_without_local_files(self):
        for kind in ("adapter", "cli"):
            self.stub(self.bin / f"gc-slack-{kind}", kind)
        result = self.run_doctor()
        self.assertEqual(result.returncode, 0, result.stderr)
        for kind in ("adapter", "cli"):
            self.assertIn(f"{kind}: prebuilt {self.bin}/gc-slack-{kind}", result.stdout)
            self.assertFalse((self.pack / kind / f"gc-slack-{kind}").exists())

    def test_doctor_accepts_local_binaries_with_empty_path(self):
        for kind in ("adapter", "cli"):
            self.stub(self.pack / kind / f"gc-slack-{kind}", kind)
        # Empty PATH searches cwd; supply only the signature-reading utility.
        (self.root / "head").symlink_to(shutil.which("head"))
        self.env["PATH"] = ""
        result = self.run_doctor()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("adapter: local\ncli: local\n", result.stdout)

    def test_doctor_missing_binaries_exits_two_with_install_advice(self):
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertIn("pack binaries not built: adapter/gc-slack-adapter cli/gc-slack-cli", result.stdout)
        for name in ("GC_SLACK_ADAPTER_BIN", "GC_SLACK_CLI_BIN", "PATH"):
            self.assertIn(name, result.stdout)

    def test_doctor_overrides_accept_scripts_and_take_precedence(self):
        for kind in ("adapter", "cli"):
            chosen = self.root / f"chosen {kind}"
            self.legacy(chosen)
            self.env[f"GC_SLACK_{kind.upper()}_BIN"] = chosen.name
            self.stub(self.bin / f"gc-slack-{kind}", "PATH")
        result = self.run_doctor()
        self.assertEqual(result.returncode, 0, result.stderr)
        for kind in ("adapter", "cli"):
            self.assertIn(f"{kind}: prebuilt {self.root.resolve()}/chosen {kind}", result.stdout)

    def test_doctor_empty_overrides_use_path(self):
        for kind in ("adapter", "cli"):
            self.stub(self.bin / f"gc-slack-{kind}", kind)
            self.env[f"GC_SLACK_{kind.upper()}_BIN"] = ""
        self.assertEqual(self.run_doctor().returncode, 0)

    def test_doctor_invalid_overrides_do_not_fall_back(self):
        for kind in ("adapter", "cli"):
            self.stub(self.bin / f"gc-slack-{kind}", kind)
            self.stub(self.pack / kind / f"gc-slack-{kind}", kind)
        for kind in ("adapter", "cli"):
            with self.subTest(kind=kind):
                self.env[f"GC_SLACK_{kind.upper()}_BIN"] = "absent"
                result = self.run_doctor()
                self.assertEqual(result.returncode, 2)
                self.assertIn("is not an executable file", result.stderr)
                self.assertNotIn(f"{kind}: ", result.stdout)
                del self.env[f"GC_SLACK_{kind.upper()}_BIN"]

    def test_doctor_rejects_path_and_local_legacy_wrappers(self):
        for kind in ("adapter", "cli"):
            self.legacy(self.bin / f"gc-slack-{kind}")
            self.legacy(self.pack / kind / f"gc-slack-{kind}")
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertNotIn("LEGACY_WRAPPER_RAN", result.stdout)


if __name__ == "__main__":
    unittest.main()
