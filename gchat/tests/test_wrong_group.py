"""Replies carry the question's reference; surrounding activity cannot address them."""
from test_commands import PACK, ProxyFixture

A, B = "spaces/AAAA", "spaces/BBBB"
QUESTION = A + "/messages/QUESTION"


def inbound(seq, space, target="s1"):
    return {"seq": seq, "payload": {"provider": "gchat", "target_session": target,
                                    "conversation_id": space}}


class WrongGroupTest(ProxyFixture):
    def test_reference_is_the_address_despite_surrounding_activity(self):
        cases = [
            ("B before A", [inbound(2, A), inbound(1, B)], []),
            ("B after A", [inbound(1, A), inbound(2, B)], []),
            ("newest first default route", [inbound(2, A, ""), inbound(1, B)], []),
            ("oldest first default route", [inbound(1, B), inbound(2, A, "")], []),
            ("bindings end in B", [], []),
            ("thread of B", [], ["--thread", B + "/threads/OTHER"]),
            ("space of B", [], ["--conversation-id", B]),
        ]
        self.env["GC_SESSION_ID"] = "s1"
        self.env["GC_SESSION_NAME"] = "ignored-name"
        for name, rows, flags in cases:
            with self.subTest(name=name):
                self.requests.clear()
                self.responses[self.prefix + "/events"] = (200, {"items": rows})
                self.responses[self.prefix + "/extmsg/bindings"] = (200, {"items": [
                    {"Status": "active", "Conversation": {"provider": "gchat", "conversation_id": space}}
                    for space in (A, B)]})
                # The proxy supplies the server's outcome, tested separately by
                # Go HTTP tests. The wrapper must forward every assertion intact.
                if flags:
                    self.respond("/commands/publish", {"error": "address disagrees with --to"}, 400)
                else:
                    self.respond("/commands/publish", {"delivered": True, "recorded": True,
                                 "conversation_id": A, "in_reply_to": QUESTION})
                result = self.run_command("reply-current", "--to", QUESTION, "--body", "answer", *flags)
                self.assertEqual(result.returncode == 0, not flags, result.stderr)
                self.assertEqual(len(self.requests), 1)
                method, path, _, data, _ = self.requests[0]
                self.assertEqual((method, path), ("POST", self.service + "/commands/publish"))
                self.assertEqual(data["to"], QUESTION)
                self.assertEqual(data["conversation_id"], B if flags[:1] == ["--conversation-id"] else "")
                self.assertEqual(data["thread"], B + "/threads/OTHER" if flags[:1] == ["--thread"] else "")

    def test_all_senders_forward_reference_spellings(self):
        file = self.root / "upload.txt"
        file.write_text("file")
        refs = [QUESTION, "gchat:" + QUESTION, QUESTION + "#deleted",
                QUESTION + "#updated#2026-11-12T01:02:03.123Z",
                QUESTION + "/reactions/R#created", QUESTION + "/reactions/R#deleted"]
        for verb in ("reply-current", "publish", "upload"):
            for ref in refs:
                with self.subTest(verb=verb, ref=ref):
                    self.requests.clear()
                    route = "/commands/upload" if verb == "upload" else "/commands/publish"
                    self.respond(route, {"delivered": True, "recorded": True})
                    args = ["--file", str(file)] if verb == "upload" else []
                    result = self.run_command(verb, "--to", ref, "--top-level", "--body", "answer", *args)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(len(self.requests), 1)
                    self.assertEqual(self.requests[0][0], "POST")
                    self.assertEqual(self.requests[0][3]["to"], ref)
                    self.assertTrue(self.requests[0][3]["top_level"])

    def test_malformed_references_and_missing_targets_make_no_request(self):
        file = self.root / "upload.txt"
        file.write_text("file")
        self.env["GC_SESSION_NAME"] = "must-not-resolve"
        for verb in ("reply-current", "publish", "upload"):
            for ref in ("spaces/AAAA", "https://chat.google.com/" + QUESTION, QUESTION + "#bogus",
                        QUESTION + "\n", QUESTION + "#updated#2026-02-30T01:02:03Z", ""):
                with self.subTest(verb=verb, ref=ref):
                    args = ["--file", str(file)] if verb == "upload" else []
                    if ref:
                        args += ["--to", ref]
                    result = self.run_command(verb, "--body", "answer", *args)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertEqual(self.requests, [])

    def test_command_and_skill_texts_do_not_promise_a_moving_target(self):
        for directory in (PACK / "commands", PACK / "skills"):
            for path in directory.rglob("*"):
                if path.is_file():
                    self.assertNotIn("latest", path.read_text(), str(path))
