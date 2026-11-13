"""Exercise the pack's shell entrypoints without Google or a running city."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit


PACK = Path(__file__).resolve().parents[1]


def isolated_path(root):
    tools = root / "tools"
    tools.mkdir()
    for name in ("sh", "bash", "dirname", "basename", "find", "sed", "printenv", "curl", "jq"):
        (tools / name).symlink_to(shutil.which(name))
    return str(tools)


class FixtureIsolationTest(unittest.TestCase):
    def test_no_go_fixture_names_live_ports_or_executes_gc_directly(self):
        for source in (PACK / "adapter").rglob("*_test.go"):
            if "contract" in source.parts:
                continue  # The isolated real-binary test is explicitly opt-in.
            text = source.read_text()
            for port in (8372, 8447):
                self.assertNotIn(":" + str(port), text, str(source))
            self.assertNotIn('exec.Command("gc"', text, str(source))
            self.assertNotIn('exec.CommandContext(t.Context(), "gc"', text, str(source))


class CommandHelpTest(unittest.TestCase):
    def test_every_command_has_gc_help_with_its_options(self):
        for command in (PACK / "commands").glob("*/command.toml"):
            with self.subTest(command=command.parent.name):
                help_file = command.with_name("help.md")
                self.assertTrue(help_file.is_file(), str(help_file))
                text = help_file.read_text()
                self.assertIn("gc gchat " + command.parent.name, text)
                self.assertIn("--", text)


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
        binary.write_text('#!/bin/sh\nprintf "%s\\n" "$@" "env=${GC_GCHAT_ADAPTER_ENV:-}" "secrets=${GC_SERVICE_SECRETS_DIR:-}" "project=${GCHAT_PROJECT_ID:-}" "state=${GCHAT_STATE_FILE:-}" "source=${GCHAT_TOKEN_SOURCE:-oauth}"\n')
        binary.chmod(0o755)
        self.env = {"PATH": isolated_path(self.root), "HOME": str(self.root),
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

    def test_service_and_auth_preserve_exported_state_override(self):
        env = self.root / "env"
        env.write_text("GCHAT_PROJECT_ID=example\nGCHAT_STATE_FILE=/from-file/state.json\nGCHAT_TOKEN_SOURCE=keyholder\n")
        override = str(self.root / "process 'quoted' $(touch SHOULD_NOT_EXIST).json")
        self.env.update(GC_GCHAT_ADAPTER_ENV=str(env), GCHAT_STATE_FILE=override, GCHAT_TOKEN_SOURCE="oauth")
        run = self.pack / "adapter/run.sh"
        shutil.copyfile(PACK / "adapter/run.sh", run)
        self.env["GC_GCHAT_ADAPTER_BIN"] = str(self.pack / "adapter/gc-gchat-adapter")
        for command in (["sh", str(self.pack / "commands/auth.sh"), "--check"],
                        ["bash", str(run)]):
            with self.subTest(command=command[-1]):
                result = subprocess.run(command, env=self.env, text=True, capture_output=True,
                                        cwd=self.root, timeout=10)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(f"state={override}\n", result.stdout)
                self.assertIn("project=example\n", result.stdout)
                self.assertIn("source=keyholder\n", result.stdout)
                self.assertFalse((self.root / "SHOULD_NOT_EXIST").exists())
                self.assertFalse((self.pack / "adapter/SHOULD_NOT_EXIST").exists())

    def test_auth_mode_comes_from_file_not_inherited_export(self):
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        enroll = bin_dir / "boxboxbox-google"
        enroll.write_text('#!/bin/sh\nprintf "%s\\n" enrolled\n')
        enroll.chmod(0o755)
        self.env["PATH"] = str(bin_dir) + os.pathsep + self.env["PATH"]
        env = self.root / "env"
        self.env["GC_GCHAT_ADAPTER_ENV"] = str(env)
        for mode, inherited in (("keyholder", "oauth"), ("oauth", "keyholder"), ("", "keyholder")):
            with self.subTest(file_mode=mode, inherited=inherited):
                env.write_text("GCHAT_PROJECT_ID=example\nGCHAT_STATE_FILE=/from-file/state.json\n" +
                               (f"GCHAT_TOKEN_SOURCE={mode}\n" if mode else ""))
                self.env.update(GCHAT_TOKEN_SOURCE=inherited, GCHAT_STATE_FILE="")
                result = self.run_auth()
                self.assertEqual(result.returncode, 0, result.stderr)
                if mode == "keyholder":
                    self.assertEqual(result.stdout, "enrolled\n")
                else:
                    self.assertIn("auth\n", result.stdout)
                    self.assertIn("source=oauth\n", result.stdout)
                    self.assertIn("state=/from-file/state.json\n", result.stdout)

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


class ProxyFixture(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.requests = []
        self.responses = {}
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                self.handle_request()

            def do_POST(self):
                self.handle_request()

            def handle_request(self):
                path = urlsplit(self.path)
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                owner.requests.append((self.command, path.path, parse_qs(path.query),
                                       json.loads(body) if body else None, dict(self.headers)))
                status, response = owner.responses.get(path.path, (404, {"error": "unexpected route"}))
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                if status == 302:
                    self.send_header("Location", f"http://127.0.0.1:{owner.server.server_port}/redirect-target")
                self.end_headers()
                self.wfile.write(json.dumps(response).encode())

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        thread.start()
        self.addCleanup(thread.join)
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.root),
                    "GC_PACK_DIR": str(PACK), "GC_CITY_NAME": "examplecity",
                    "GC_API_BASE_URL": f"http://127.0.0.1:{self.server.server_port}"}
        self.prefix = "/v0/city/examplecity"
        self.service = self.prefix + "/svc/gchat"

    def run_command(self, verb, *args):
        return subprocess.run(["sh", str(PACK / f"commands/{verb}.sh"), *args],
                              env=self.env, text=True, capture_output=True, timeout=10)

    def respond(self, route, value, status=200):
        self.responses[self.service + route] = (status, value)


class ProxyCommandsTest(ProxyFixture):
    def test_large_body_streams_without_argv_limit(self):
        body = self.root / "large.txt"
        body.write_text("text\n" * 300000)
        self.respond("/commands/publish", {"delivered": True, "recorded": True})
        result = self.run_command("publish", "--conversation-id", "spaces/AAAA", "--body-file", str(body))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.requests[-1][3]["text"], body.read_text())

    def test_missing_body_file_fails_before_reply_lookup(self):
        self.env["GC_SESSION_NAME"] = "example"
        result = self.run_command("reply-current", "--to", "spaces/AAAA/messages/QUESTION", "--body-file", str(self.root / "missing"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("body file", result.stderr)
        self.assertEqual(self.requests, [])

    def test_publish_preserves_body_and_reuses_supplied_key(self):
        body = self.root / "body with spaces.txt"
        body.write_text('hello "example"\nsecond line\n')
        self.respond("/commands/publish", {"delivered": True, "recorded": True})
        result = self.run_command("publish", "--conversation-id", "spaces/AAAA", "--thread",
                                  "spaces/AAAA/threads/1", "--session", "example-session",
                                  "--body-file", str(body), "--idempotency-key", "example-key")
        self.assertEqual(result.returncode, 0, result.stderr)
        method, path, _, data, headers = self.requests[-1]
        self.assertEqual((method, path), ("POST", self.service + "/commands/publish"))
        self.assertEqual(data["text"], body.read_text())
        self.assertEqual(data["session_id"], "example-session")
        self.assertEqual(data["thread"], "spaces/AAAA/threads/1")
        self.assertEqual(data["idempotency_key"], "example-key")
        self.assertEqual(headers["X-GC-Request"], "gc-gchat")
        self.assertIn("example-key", result.stderr)

    def test_send_prints_key_before_failure_and_reports_unrecorded(self):
        self.respond("/commands/publish", {"delivered": False, "recorded": False, "reason": "initializing"})
        result = self.run_command("publish", "--conversation-id", "spaces/AAAA", "--body", "hello")
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(self.requests, result.stderr)
        key = self.requests[-1][3]["idempotency_key"]
        self.assertTrue(key)
        self.assertIn(key, result.stderr)
        self.respond("/commands/publish", {"delivered": True, "recorded": False, "reason": "no active binding"})
        result = self.run_command("publish", "--conversation-id", "spaces/AAAA", "--body", "hello",
                                  "--idempotency-key", key)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("not recorded", result.stderr)
        self.assertIn("no active binding", result.stdout)

    def test_reply_requires_reference_before_any_gc_request(self):
        self.env["GC_SESSION_NAME"] = "example-session"
        for flags in ([], ["--conversation-id", "spaces/AAAA"]):
            with self.subTest(flags=flags):
                result = self.run_command("reply-current", "--body", "hello", *flags)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("--to", result.stderr)
                self.assertIn("Message id", result.stderr)
                self.assertEqual(len(result.stderr.splitlines()), 1)
                self.assertEqual(self.requests, [])

    def test_sending_scripts_have_no_destination_lookup(self):
        for path in (PACK / "commands").glob("*.sh"):
            text = path.read_text()
            self.assertNotIn("extmsg.inbound", text)
            self.assertNotIn("extmsg/bindings", text)
            self.assertNotIn("gchat_reply_target", text)

    def test_reply_explicit_target_needs_no_lookup(self):
        self.respond("/commands/publish", {"delivered": True, "recorded": False})
        result = self.run_command("reply-current", "--to", "spaces/AAAA/messages/QUESTION", "--body", "hello")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(self.requests), 1)

    def test_upload_uses_absolute_path_and_keeps_key(self):
        file = self.root / "example file.txt"
        file.write_text("example")
        self.respond("/commands/upload", {"delivered": True, "recorded": True})
        result = self.run_command("upload", "--conversation-id", "spaces/AAAA", "--file", str(file),
                                  "--body", "caption", "--idempotency-key", "upload-example")
        self.assertEqual(result.returncode, 0, result.stderr)
        # The wrapper sends the physical path: the service checks it against
        # its upload root without following symlinks.
        self.assertEqual(self.requests[-1][3]["path"], os.path.realpath(file))
        self.assertEqual(self.requests[-1][3]["idempotency_key"], "upload-example")

    def test_upload_curl_budget_scales_with_file_size(self):
        file = self.root / "large.bin"
        file.write_bytes(bytes(2 << 20))
        bin_dir = self.root / "bin"
        bin_dir.mkdir()
        args_file = self.root / "curl-args"
        wrapper = bin_dir / "curl"
        wrapper.write_text('#!/bin/sh\nprintf "%s\\n" "$@" > "$GCHAT_CURL_ARGS"\nexec "$GCHAT_REAL_CURL" "$@"\n')
        wrapper.chmod(0o755)
        self.env.update(GCHAT_REAL_CURL=shutil.which("curl"), GCHAT_CURL_ARGS=str(args_file),
                        PATH=str(bin_dir) + os.pathsep + self.env["PATH"])
        self.respond("/commands/upload", {"delivered": True, "recorded": True})
        result = self.run_command("upload", "--conversation-id", "spaces/AAAA", "--file", str(file))
        self.assertEqual(result.returncode, 0, result.stderr)
        args = args_file.read_text().splitlines()
        self.assertEqual(args[args.index("--max-time") + 1], "53")

    def test_react_read_spaces_resolve_and_state_routes(self):
        cases = [
            ("react", ["--message", "spaces/AAAA/messages/1", "--emoji", "👍", "--remove"], "/react", {"message": "spaces/AAAA/messages/1", "emoji": "👍", "remove": True}),
            ("spaces", ["--space", "spaces/AAAA", "--wake", "mentions"], "/spaces", {"id": "spaces/AAAA", "wake": "mentions"}),
            ("resolve", ["spaces/AAAA/messages/1#updated#time", "--accept"], "/resolve", {"id": "spaces/AAAA/messages/1#updated#time", "action": "accept"}),
            ("resolve", ["example-id", "--replay"], "/resolve", {"id": "example-id", "action": "replay"}),
            ("state", ["reset"], "/state", {"action": "reset", "file": ""}),
        ]
        for verb, args, route, payload in cases:
            with self.subTest(verb=verb, args=args):
                self.respond(route, {"ok": True})
                result = self.run_command(verb, *args)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(self.requests[-1][3], payload)
                self.assertEqual(self.requests[-1][4]["X-GC-Request"], "gc-gchat")
        self.respond("/read", [])
        result = self.run_command("read", "--space", "spaces/AAAA", "--thread", "spaces/AAAA/threads/a&b",
                                  "--since", "2026-10-22T00:00:00Z", "--limit", "25")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.requests[-1][0], "GET")
        self.assertEqual(self.requests[-1][2]["thread"], ["spaces/AAAA/threads/a&b"])

    def test_invalid_flags_do_not_send_and_http_errors_fail(self):
        for verb, args in [("publish", ["--body"]), ("react", ["--message", "x"]),
                           ("resolve", ["x", "--accept", "--replay"]),
                           ("spaces", ["--wake", "all"]), ("state", ["restore"]),
                           ("read", ["--space", "spaces/A", "--limit", "0"])]:
            result = self.run_command(verb, *args)
            self.assertNotEqual(result.returncode, 0, (verb, result.stdout))
        self.assertEqual(self.requests, [])
        for status, reason in [(503, "initializing"), (302, "redirected")]:
            with self.subTest(http_status=status):
                self.respond("/spaces", {"error": reason}, status)
                result = self.run_command("spaces")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(reason, result.stderr)
                self.assertFalse(any(request[1] == "/redirect-target" for request in self.requests))

    def test_status_separates_pending_unresolved_and_retained_counters(self):
        self.respond("/status", {"health": {"state": "catching-up", "reason": "history"},
            "subscription": {"state": "ACTIVE", "name": "subscriptions/example", "expiry": "2026-10-22T04:00:00Z"},
            "backlog": 2, "unresolved": 1, "pending": 1, "unrecorded": 0, "quarantine": 0,
            "counters": {"unavailable:spaces/AAAA": 3, "credential_refresh_restarts": 1},
            "deliveries": [{"id": "pending-id", "status": "pending", "reason": "receipt lookup failed beyond deadline; retry pending"},
                           {"id": "unresolved-id", "status": "unresolved", "reason": "receipt pending beyond deadline"}],
            "spaces": {"spaces/AAAA": {"wake": "all", "backlog": [], "page_token": json.dumps({"unavailable": "permission denied", "access_retry_after": "2026-10-22T01:00:00Z"})}}})
        result = self.run_command("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        for value in ("ACTIVE", "pending-id", "unresolved-id", "unavailable:spaces/AAAA", "permission denied", "credential_refresh_restarts"):
            self.assertIn(value, result.stdout)

    def test_status_text_without_retry_keys_prints_as_before(self):
        self.respond("/status", {"health": {"state": "ready", "reason": ""}, "subscription": {"state": "ACTIVE", "name": "subscriptions/example"},
            "backlog": 0, "unresolved": 1, "pending": 1, "unrecorded": 0, "quarantine": 0, "counters": {},
            "deliveries": [{"id": "pending-id", "status": "pending", "reason": ""},
                           {"id": "unresolved-id", "status": "unresolved", "reason": "delivery retries exhausted"}], "spaces": {}})
        result = self.run_command("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = result.stdout.splitlines()
        self.assertIn("Backlog: 0; pending: 1; unresolved: 1; unrecorded: 0; quarantine: 0", lines)
        self.assertIn("  pending pending-id: ", lines)
        self.assertIn("  unresolved unresolved-id: delivery retries exhausted", lines)
        self.assertNotIn("stuck", result.stdout)
        self.assertNotIn("age", result.stdout)

    def test_status_text_names_window_and_held_acts(self):
        self.respond("/status", {"health": {"state": "ready"}, "coalesce_window": "8s", "held_acts": 3})
        result = self.run_command("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Coalescing: 8s; held acts: 3", result.stdout.splitlines())

    def test_status_text_names_the_history_floor(self):
        value = {"health": {"state": "ready"}, "history_floor": "2030-01-02T03:04:05Z"}
        self.respond("/status", value)
        result = self.run_command("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = result.stdout.splitlines()
        self.assertEqual(lines[lines.index("Last event: none") + 1],
            "History: first start skipped messages written before 2030-01-02T03:04:05Z; gc gchat read still reads them")
        del value["history_floor"]
        self.respond("/status", value)
        result = self.run_command("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("History: first start", result.stdout)

    def test_status_text_shows_age_cause_and_next_attempt(self):
        self.respond("/status", {"health": {"state": "ready", "reason": ""}, "subscription": {"state": "ACTIVE", "name": "subscriptions/example"},
            "backlog": 0, "unresolved": 1, "pending": 2, "stuck": 1, "unrecorded": 0, "quarantine": 0, "counters": {},
            "deliveries": [{"id": "waiting-id", "status": "intent", "reason": "retry", "created": "2026-11-10T20:22:58Z", "age_seconds": 3725,
                            "cause": "draft_deferred", "attempts": 63, "next_attempt": "2026-11-10T21:25:03Z"},
                           {"id": "fresh-id", "status": "intent", "reason": "", "created": "2026-11-10T21:24:00Z", "age_seconds": 63, "cause": "behind_earlier"},
                           {"id": "unresolved-id", "status": "unresolved", "reason": "not delivered within the retry bound", "created": "2026-11-09T20:22:58Z",
                            "age_seconds": 90061, "cause": "draft_deferred", "attempts": 1440, "raised": "2026-11-09T20:24:58Z",
                            "raised_end": "2026-11-10T20:22:58Z", "fragment": True}], "spaces": {}})
        result = self.run_command("status")
        self.assertEqual(result.returncode, 0, result.stderr)
        lines = result.stdout.splitlines()
        self.assertIn("Backlog: 0; pending: 2; unresolved: 1; unrecorded: 0; quarantine: 0; stuck: 1", lines)
        self.assertIn("  intent waiting-id: retry; age 1h2m; cause draft_deferred; next attempt 2026-11-10T21:25:03Z; posts 63", lines)
        self.assertIn("  intent fresh-id: ; age 1m3s; cause behind_earlier; posts 0", lines)
        self.assertIn("  unresolved unresolved-id: not delivered within the retry bound; age 25h1m; cause draft_deferred; raised 2026-11-09T20:24:58Z; ended mail 2026-11-10T20:22:58Z; fragment in input box", lines)


class DoctorTest(ProxyFixture):
    # Use the same in-process proxy, but only inherit the fixture helpers.
    def setUp(self):
        super().setUp()
        self.env["PATH"] = isolated_path(self.root)
        self.pack = self.root / "pack"
        shutil.copytree(PACK / "commands", self.pack / "commands")
        if (PACK / "doctor").exists():
            shutil.copytree(PACK / "doctor", self.pack / "doctor")
        adapter = self.pack / "adapter"
        adapter.mkdir()
        binary = adapter / "gc-gchat-adapter"
        binary.write_text('#!/bin/sh\n[ "$1 $2" = "auth --check" ] || exit 2\nprintf "%s\\n" "${CHECK_OUTPUT:-ready}"\nexit "${CHECK_EXIT:-0}"\n')
        binary.chmod(0o755)
        gc = self.root / "gc"
        gc.write_text('#!/bin/sh\n[ "$*" = "config show --json" ] || exit 2\nprintf "%s\\n" "$CONFIG_JSON"\n')
        gc.chmod(0o755)
        env = self.root / "secrets/env"
        env.parent.mkdir()
        self.secrets = {"GCHAT_PROJECT_ID": "example", "GCHAT_PUBSUB_TOPIC": "projects/example/topics/chat",
                        "GCHAT_PUBSUB_SUBSCRIPTION": "projects/example/subscriptions/chat", "GCHAT_OAUTH_CLIENT_FILE": "/example/client.json",
                        "GC_CITY_NAME": "examplecity", "GC_API_BASE_URL": self.env["GC_API_BASE_URL"]}
        self.env_file = env
        env.write_text("".join(f"{key}={value}\n" for key, value in self.secrets.items()))
        self.env.update(GC_PACK_DIR=str(self.pack), GC_GCHAT_ADAPTER_ENV=str(env), GC_BIN=str(gc),
                        CONFIG_JSON=json.dumps({"config": {"ExtMsg": {"DefaultRoutes": [{"Provider": "gchat", "Agent": "mayor"}]}}}))
        self.respond("/status", {"subscription": {"state": "ACTIVE"}})

    def run_doctor(self):
        return subprocess.run(["sh", str(self.pack / "doctor/gchat/run.sh")], env=self.env,
                              text=True, capture_output=True, timeout=10)

    def test_doctor_stops_at_first_failure_and_checks_route_section(self):
        self.env_file.write_text("".join(f"{key}={value}\n" for key, value in self.secrets.items() if key != "GCHAT_PUBSUB_TOPIC"))
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2, result)
        self.assertIn("GCHAT_PUBSUB_TOPIC", result.stderr)
        self.assertEqual(self.requests, [])
        self.env_file.write_text("".join(f"{key}={value}\n" for key, value in self.secrets.items()))
        self.env.update(CHECK_EXIT="1", CHECK_OUTPUT="needs-sign-in")
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2, result)
        self.assertIn("needs-sign-in", result.stderr)
        self.assertEqual(self.requests, [])
        self.env.update(CHECK_EXIT="0", CHECK_OUTPUT="ready")
        self.respond("/status", {"subscription": {"state": "SUSPENDED"}})
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2, result)
        self.assertIn("SUSPENDED", result.stderr.splitlines()[0])
        self.assertEqual(result.stdout, "")
        self.respond("/status", {"subscription": {"state": "ACTIVE"}})
        self.env["CONFIG_JSON"] = json.dumps({"provider": "gchat", "ExtMsg": {"DefaultRoutes": []}})
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2, result)
        self.assertIn("extmsg.default_route", result.stderr.splitlines()[0])
        self.assertEqual(result.stdout, "")
        self.env["CONFIG_JSON"] = json.dumps({"config": {"ExtMsg": {"DefaultRoutes": [{"Provider": "gchat", "Agent": "mayor"}]}}})
        result = self.run_doctor()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("ACTIVE", result.stdout)
        self.assertEqual(len(result.stdout.splitlines()), 1)

    def test_missing_secrets_is_an_error_headline(self):
        self.env["GC_GCHAT_ADAPTER_ENV"] = str(self.root / "absent")
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, "")
        self.assertIn("Missing secrets", result.stderr.splitlines()[0])

    def test_unreachable_status_is_an_error_headline(self):
        self.respond("/status", {"error": "offline"}, 503)
        result = self.run_doctor()
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, "")
        self.assertIn("503", result.stderr.splitlines()[0])


if __name__ == "__main__":
    unittest.main()
