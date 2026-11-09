"""Prebuilt service startup without a build toolchain or Google credentials."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import unittest

PACK = Path(__file__).resolve().parents[1]


class PrebuiltAdapterTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.adapter = self.root / "pack/adapter"
        self.adapter.mkdir(parents=True)
        shutil.copy(PACK / "adapter/run.sh", self.adapter)
        shutil.copy(PACK / "adapter/build.sh", self.adapter)
        shutil.copytree(PACK / "commands", self.root / "pack/commands")
        shutil.copy(PACK / "pack.toml", self.root / "pack/pack.toml")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        # Deliberately exclude Go and any installed adapter from PATH.
        for name in ("bash", "dirname", "basename", "find", "sed"):
            (self.bin / name).symlink_to(shutil.which(name))
        self.env_file = self.root / "env"
        self.env_file.write_text("GCHAT_PROJECT_ID=from-file\n")
        self.env = {"PATH": str(self.bin), "HOME": str(self.root),
                    "GC_GCHAT_ADAPTER_ENV": str(self.env_file)}

    def stub(self, path, marker):
        path.write_text('#!/bin/bash\nif [[ "${1:-}" == version ]]; then\n'
                        '  echo 0.5.0; exit 0\nfi\n'
                        f'printf "%s\\n" "{marker}" "$@" "project=$GCHAT_PROJECT_ID"\n')
        path.chmod(0o755)

    def run_adapter(self):
        return subprocess.run([str(self.adapter / "run.sh")], cwd=self.root,
                              env=self.env, text=True, capture_output=True, timeout=10)

    def test_path_binary_precedes_local_build(self):
        self.stub(self.bin / "gc-gchat-adapter", "PATH")
        self.stub(self.adapter / "gc-gchat-adapter", "LOCAL")
        result = self.run_adapter()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "PATH\nserve\nproject=from-file\n")
        self.assertIn(f"using {self.bin}/gc-gchat-adapter (version 0.5.0)", result.stderr)

    def test_override_precedes_path_and_accepts_relative_filename(self):
        self.stub(self.bin / "gc-gchat-adapter", "PATH")
        self.stub(self.root / "chosen binary", "OVERRIDE")
        self.env["GC_GCHAT_ADAPTER_BIN"] = "chosen binary"
        result = self.run_adapter()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "OVERRIDE\nserve\nproject=from-file\n")

    def test_relative_path_is_resolved_before_local_directory_change(self):
        self.stub(self.bin / "gc-gchat-adapter", "PATH")
        self.env["PATH"] = "bin"
        self.assertEqual(self.run_adapter().stdout, "PATH\nserve\nproject=from-file\n")

    def test_invalid_override_does_not_fall_back(self):
        self.stub(self.bin / "gc-gchat-adapter", "PATH")
        self.env["GC_GCHAT_ADAPTER_BIN"] = str(self.root / "absent")
        result = self.run_adapter()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("is not an executable file", result.stderr)
        self.assertEqual(result.stdout, "")

    def test_shell_function_cannot_shadow_path_binary(self):
        self.stub(self.bin / "gc-gchat-adapter", "PATH")
        self.env["BASH_FUNC_gc-gchat-adapter%%"] = "() { exit 89; }"
        self.assertEqual(self.run_adapter().stdout, "PATH\nserve\nproject=from-file\n")

    def test_missing_secrets_still_fails_before_prebuilt_exec(self):
        self.stub(self.bin / "gc-gchat-adapter", "PATH")
        self.env_file.unlink()
        result = self.run_adapter()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Missing secrets file:", result.stderr)
        self.assertEqual(result.stdout, "")

    def test_no_binary_and_no_go_has_clear_error(self):
        result = self.run_adapter()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("ERROR: no Go toolchain found", result.stderr)

    def test_local_binary_remains_fallback(self):
        self.stub(self.adapter / "gc-gchat-adapter", "LOCAL")
        result = self.run_adapter()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "LOCAL\nserve\nproject=from-file\n")

    def test_version_only_change_rebuilds_service_and_command_binary(self):
        binary = self.adapter / "gc-gchat-adapter"
        self.stub(binary, "LOCAL")
        # Record the version seen by the rebuild without needing Go.
        (self.adapter / "build.sh").write_text(
            '#!/bin/bash\nsed -n \'s/^version = "\\([^"]*\\)".*/\\1/p\' ../pack.toml > rebuilt-version\n')
        manifest = self.root / "pack/pack.toml"
        manifest.write_text('[pack]\nversion = "9.9.9"\n')
        future = time.time() + 60
        os.utime(binary, (future, future))
        os.utime(manifest, (future + 60, future + 60))
        marker = self.adapter / "rebuilt-version"
        self.env["GC_PACK_DIR"] = str(self.root / "pack")
        for command in ([str(self.adapter / "run.sh")],
                        ["sh", "-c", '. "$1"; gchat_build', "sh",
                         str(self.root / "pack/commands/runtime.sh")]):
            with self.subTest(command=command[0]):
                # The command launcher deliberately remains POSIX sh.
                if command[0] == "sh":
                    command[0] = shutil.which("sh")
                result = subprocess.run(command, cwd=self.root, env=self.env,
                                        text=True, capture_output=True, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(marker.read_text(), "9.9.9\n")
                marker.unlink()


if __name__ == "__main__":
    unittest.main()
