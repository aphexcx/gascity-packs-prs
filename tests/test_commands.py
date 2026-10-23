"""Exercise the pack's shell entrypoints without Google or a running city."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


PACK = Path(__file__).resolve().parents[1]


class AuthCommandTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.pack = self.root / "pack"
        self.pack.mkdir()
        if (PACK / "commands").exists():
            shutil.copytree(PACK / "commands", self.pack / "commands")
        adapter = self.pack / "adapter"
        adapter.mkdir()
        binary = adapter / "gc-gchat-adapter"
        binary.write_text('#!/bin/sh\nprintf "%s\\n" "$@" "env=${GC_GCHAT_ADAPTER_ENV:-}" "secrets=${GC_SERVICE_SECRETS_DIR:-}" "project=${GCHAT_PROJECT_ID:-}"\n')
        binary.chmod(0o755)
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.root),
                    "GC_PACK_DIR": str(self.pack)}

    def run_auth(self, *args):
        return subprocess.run(["sh", str(self.pack / "commands/auth.sh"), *args],
                              env=self.env, text=True, capture_output=True, timeout=10)

    def test_explicit_env_and_check_forwarding(self):
        env = self.root / "custom secrets/env"
        env.parent.mkdir()
        env.write_text("GCHAT_PROJECT_ID=example\n")
        self.env.update(GC_GCHAT_ADAPTER_ENV=str(env), GC_SERVICE_SECRETS_DIR="/unused")
        result = self.run_auth("--check")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("auth\n--check\n", result.stdout)
        self.assertIn(f"env={env}\n", result.stdout)
        self.assertIn("project=example\n", result.stdout)

    def test_city_context_resolves_service_secrets(self):
        secrets = self.root / "examplecity/.gc/services/gchat/secrets"
        secrets.mkdir(parents=True)
        (secrets / "env").write_text("GCHAT_PROJECT_ID=example\n")
        self.env["GC_CITY_PATH"] = str(self.root / "examplecity")
        result = self.run_auth()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn(f"secrets={secrets}\n", result.stdout)

    def test_home_fallback_preserves_standalone_settings_layout(self):
        secrets = self.root / ".config/gc-gchat-adapter"
        secrets.mkdir(parents=True)
        (secrets / "env").write_text("GCHAT_PROJECT_ID=example\n")
        result = self.run_auth()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("env=\nsecrets=\n", result.stdout)

    def test_missing_env_names_file_and_required_keys(self):
        self.env["GC_GCHAT_ADAPTER_ENV"] = str(self.root / "missing")
        result = self.run_auth("--check")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Missing secrets file:", result.stderr)
        self.assertIn("GCHAT_OAUTH_CLIENT_FILE", result.stderr)


if __name__ == "__main__":
    unittest.main()
