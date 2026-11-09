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
        shutil.copytree(PACK / "doctor", self.root / "pack/doctor")
        shutil.copy(PACK / "pack.toml", self.root / "pack/pack.toml")
        self.bin = self.root / "bin"
        self.bin.mkdir()
        # Deliberately exclude Go and any installed adapter from PATH.
        for name in ("bash", "dirname", "basename", "find", "sed", "printenv"):
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

    def test_prebuilt_local_path_keeps_version_log_and_launch_directory(self):
        binary = self.adapter / "gc-gchat-adapter"
        self.stub(binary, "PREBUILT")
        with binary.open("a") as stream:
            stream.write('printf "cwd=%s\\n" "$PWD"\n')
        for source in ("override", "path"):
            with self.subTest(source=source):
                if source == "override":
                    self.env["GC_GCHAT_ADAPTER_BIN"] = str(binary)
                else:
                    self.env.pop("GC_GCHAT_ADAPTER_BIN")
                    self.env["PATH"] = f"{self.adapter}:{self.bin}"
                result = self.run_adapter()
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"cwd={self.root}\n", result.stdout)
                self.assertIn(f"using {binary} (version 0.5.0)", result.stderr)

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

    def helper_fixture(self):
        self.env["GC_PACK_DIR"] = str(self.root / "pack")
        self.env_file.write_text(
            "GCHAT_PROJECT_ID=from-file\nGCHAT_PUBSUB_TOPIC=topic\n"
            "GCHAT_PUBSUB_SUBSCRIPTION=sub\nGCHAT_OAUTH_CLIENT_FILE=client.json\n"
            "GC_CITY_NAME=test\nGC_API_BASE_URL=http://unused\n")
        (self.adapter / "build.sh").write_text(
            "#!/bin/sh\necho BUILD_MUST_NOT_RUN >&2\nexit 97\n")

    def run_helper(self, entry):
        return subprocess.run(
            [shutil.which("sh"), str(self.root / "pack" / entry), "--check"],
            cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)

    def helper_stub(self, path, marker):
        self.stub(path, marker)
        # Doctor stops at auth with this marker, before network/config checks.
        with path.open("a") as stream:
            stream.write('exit "${CHECK_EXIT:-0}"\n')

    def assert_helper_selected(self, entry, marker):
        self.env["CHECK_EXIT"] = "23" if entry.startswith("doctor/") else "0"
        result = self.run_helper(entry)
        self.assertEqual(result.returncode, 2 if entry.startswith("doctor/") else 0,
                         result.stderr)
        output = result.stderr if entry.startswith("doctor/") else result.stdout
        self.assertIn(f"{marker}\nauth\n--check\nproject=from-file", output)
        self.assertNotIn("BUILD_MUST_NOT_RUN", result.stderr)
        self.assertFalse((self.adapter / "gc-gchat-adapter").exists())

    def test_auth_path_binary_without_go_or_local_binary(self):
        self.helper_fixture()
        self.helper_stub(self.bin / "gc-gchat-adapter", "PATH")
        self.assert_helper_selected("commands/auth.sh", "PATH")

    def test_doctor_path_binary_without_go_or_local_binary(self):
        self.helper_fixture()
        self.helper_stub(self.bin / "gc-gchat-adapter", "PATH")
        self.assert_helper_selected("doctor/gchat/run.sh", "PATH")

    def test_helpers_override_precedes_path(self):
        self.helper_fixture()
        self.helper_stub(self.bin / "gc-gchat-adapter", "PATH")
        self.helper_stub(self.root / "chosen binary", "OVERRIDE")
        self.env["GC_GCHAT_ADAPTER_BIN"] = "chosen binary"
        for entry in ("commands/auth.sh", "doctor/gchat/run.sh"):
            with self.subTest(entry=entry):
                self.assert_helper_selected(entry, "OVERRIDE")

    def test_keyholder_enrollment_uses_prebuilt_without_building(self):
        self.helper_fixture()
        self.helper_stub(self.bin / "gc-gchat-adapter", "PATH")
        with self.env_file.open("a") as stream:
            stream.write("GCHAT_TOKEN_SOURCE=keyholder\n")
        self.stub(self.bin / "boxboxbox-google", "ENROLL")
        result = subprocess.run(
            [shutil.which("sh"), str(self.root / "pack/commands/auth.sh")],
            cwd=self.root, env=self.env, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "ENROLL\nenroll\nproject=from-file\n")
        self.assertNotIn("BUILD_MUST_NOT_RUN", result.stderr)

    def test_helpers_invalid_override_never_falls_back(self):
        self.helper_fixture()
        self.helper_stub(self.bin / "gc-gchat-adapter", "PATH")
        nonexec = self.root / "nonexec"
        nonexec.write_text("not executable")
        for override in ("absent", "nonexec", "bin", ""):
            self.env["GC_GCHAT_ADAPTER_BIN"] = override
            for entry in ("commands/auth.sh", "doctor/gchat/run.sh"):
                with self.subTest(override=override, entry=entry):
                    result = self.run_helper(entry)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertIn("is not an executable file", result.stderr)
                    self.assertNotIn("BUILD_MUST_NOT_RUN", result.stderr)
                    self.assertEqual(result.stdout, "")

    def test_helpers_empty_path_entries_mean_current_directory(self):
        self.helper_fixture()
        self.helper_stub(self.root / "gc-gchat-adapter", "CWD")
        for search_path in (f":{self.bin}", f"{self.bin}:", f"{self.bin}::{self.bin}"):
            self.env["PATH"] = search_path
            for entry in ("commands/auth.sh", "doctor/gchat/run.sh"):
                with self.subTest(path=search_path, entry=entry):
                    self.assert_helper_selected(entry, "CWD")

    def test_path_walk_skips_directories_and_nonexecutables(self):
        self.helper_fixture()
        first = self.root / "first"
        second = self.root / "second"
        first.mkdir()
        second.mkdir()
        (first / "gc-gchat-adapter").mkdir()
        (second / "gc-gchat-adapter").write_text("not executable")
        self.helper_stub(self.bin / "gc-gchat-adapter", "PATH")
        self.env["PATH"] = f"{first}:{second}:{self.bin}"
        self.assert_helper_selected("commands/auth.sh", "PATH")

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
            '#!/bin/bash\necho build-diagnostic\nsed -n \'s/^version = "\\([^"]*\\)".*/\\1/p\' ../pack.toml > rebuilt-version\n')
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
