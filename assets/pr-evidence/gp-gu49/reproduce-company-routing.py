"""Offline characterization of STANDARD review P1; never posts to Slack.

Run from the repository root:
uv run --no-project --with pytest python assets/pr-evidence/gp-gu49/reproduce-company-routing.py
"""

import contextlib
import io
import os
from pathlib import Path
import runpy
import tempfile

import pytest


ROOT = Path(__file__).resolve().parents[3]
fixtures = runpy.run_path(str(ROOT / "slack-full/tests/test_slack_chat_reply_current.py"))

for flag in ("--turn-ts", "--reply-to"):
    with tempfile.TemporaryDirectory() as directory, pytest.MonkeyPatch.context() as patch:
        # Keep every state file and synthetic credential under this temp city.
        for key in list(os.environ):
            if key.startswith("SLACK_") or key == "GC_SLACK_ADAPTER_ENV":
                patch.delenv(key)
        patch.setenv("GC_CITY_PATH", directory)
        patch.setenv("GC_CITY_NAME", "test-city")
        patch.setenv("GC_API_BASE_URL", "http://127.0.0.1:1")
        patch.setenv("GC_SESSION_ID", "gc-test-session")
        rc, company_posts, legacy_posts = fixtures["_company_pointer_and_mention_only"](
            patch,
            Path(directory),
            pointer_delivered_at="2026-09-10T09:00:00Z",
            delivery_received_at="2026-09-10T08:00:00Z",
        )
        # The existing fixture intercepts both Slack and gc HTTP boundaries.
        body_file = Path(directory) / "reply.txt"
        body_file.write_text("answer for the explicitly named inbound")
        args = ["--conversation-id", "C_INBOUND", flag, "100.000001",
                "--body-file", str(body_file)]
        try:
            with contextlib.redirect_stdout(io.StringIO()):
                code = rc.main(args)
        except SystemExit as exc:
            assert flag == "--turn-ts"
            assert "live company turn" in str(exc)
            assert not company_posts and not legacy_posts
            print(f"{flag}: refused before posting: {exc}")
        else:
            assert flag == "--reply-to" and code == 0
            assert len(company_posts) == 1 and not legacy_posts
            payload = company_posts[0]["payload"]
            assert payload["channel"] == "C0AAAAAAA"
            assert payload["thread_ts"] == "1700000000.000100"
            print(f"{flag}: requested channel=C_INBOUND thread=100.000001")
            print(f"{flag}: captured channel={payload['channel']} thread={payload['thread_ts']}")
            print("Confirmed wrong-conversation routing; all network calls intercepted.")
