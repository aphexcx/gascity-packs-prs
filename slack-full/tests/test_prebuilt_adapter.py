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


if __name__ == "__main__":
    unittest.main()
