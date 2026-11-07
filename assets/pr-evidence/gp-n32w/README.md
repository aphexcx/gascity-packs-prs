# gp-n32w: Hou acknowledgement reactions

Draft PR: https://github.com/aphexcx/gascity-packs-prs/pull/80

Base: `2372b4f262aad8e847c416267ba3092d31a083fd` (`fork2/main`, fetched
2026-11-07). Branch: `gp-n32w-ack-reaction`. This base includes the ingestion
and registration fixes from #68 and #72. The six commits cover port,
lifecycle reconciliation, model/config tests, docs/doctor, design note and
delivery evidence. Every commit carries `Bead: gp-n32w`.

The read-only source record on citadel is
the citadel record `hou-ack-reaction-patch-20261107/pull.out` (a city-side file, not in this repo).
Extracted SHA-256 prefixes match the record: `ack_reaction.go`
`09dadd0426a98af9`, `ack_model.go` `fef17bc1ea5ede15`, and
`ack_model_test.go` `dcf061e2ac6c71d0`. The implementation files have only
attribution-header changes; the imported model test is byte-identical.

## Validation

Go version: `go1.27.3 darwin/arm64`. Python: shared relay venv, Python 3.12,
with pytest import proved at
the shared relay venv's `site-packages/pytest/__init__.py`.
No Node tests were used.

From `slack-full/adapter`:

```text
go test ./... -count=1 -json
  adapter: PASS (94.593s); redact: PASS (0.128s)
  1458 top-level tests passed; 3 skipped
  12891 passing cases including nested subtests
go vet ./...
  exit 0; empty output
gofmt -l .
  final exit 0; empty output
go test ./... -run 'Test(Ack|ParseAck|PickAck)' -count=1 -json
  28 passing cases
go test -race ./... -run 'Test(Ack|ParseAck|PickAck)' -count=1
  both packages PASS; no race reports
```

The full and focused runs set a closed loopback sentinel
`ACK_MODEL_BASE_URL=http://127.0.0.1:1/v1` and
`HTTP_PROXY=HTTPS_PROXY=http://127.0.0.1:1`, with
`NO_PROXY=127.0.0.1,localhost` for local fixtures. Imported model tests use
`httptest` servers or loopback failures, never a live gateway or key.

From `slack-full`:

```text
<the shared relay venv>/bin/python -m pytest tests -q
914 passed, 2 skipped, 2 warnings in 47.70s
```

Both warnings are existing Python 3.12 `os.fork()` deprecations in
`test_slack_company_outbound.py:698`. Existing Python tests were not changed;
the new doctor suite adds seven cases. The initial module-wide gofmt check
found a four-line indentation defect already in base
`company_thread_ambient.go:218`; the final commit applies only gofmt there.
The full Go suite ran before that whitespace-only cleanup; the final
formatting check ran after it. Logs and raw runtime records stay local in
this directory; the compact checked-in output is in `validation.txt`.

## Behavior and self-review

- Unset acknowledgement leaves routing and the existing busy lifecycle
  unchanged. With busy disabled too, targeted and ambient messages add no
  reactions. With busy enabled, targeted messages retain only their existing
  hourglass. No acknowledgement goroutine starts when the setting is empty.
- Auto mode adds a receipt and a separate targeted hourglass; publishing the
  reply removes only the hourglass. The two asynchronous adds can arrive in
  either order, so the test checks both without imposing a false ordering.
- Ambient messages receive only the acknowledgement. The existing
  `msg.BotID != "" || msg.Subtype == "bot_message"` guard at
  `slack-full/adapter/main.go:4035` filters adapter and other bot posts before
  the hook. Human subtype/user and empty-content guards also precede it.
- Redelivery's `already_reacted` response is accepted with zero log lines.
  Other Slack API failures are logged. A missing model-key setting produces
  one startup warning and keywords on each message without a model call.
- Fixed, auto and model choices must differ from the configured busy emoji
  to have separate lifetimes; this operator constraint is documented. The
  default hourglass is outside the auto/model sets. Model timeouts bound
  inference, not preceding channel lookup or subsequent Slack delivery.
- Self-review covered the base diff, source fidelity, bot guards, off-mode
  routing, error/fallback paths, timeouts, async lifetime tests and credential
  handling. Mayor's Codex gate and Fable read are separate pending reviews.

## Design note sources

See `design-note.md` (347 words) and the PR body's comparison.

| Fact | Read source |
| --- | --- |
| Default busy emoji and lifecycle | `slack-full/adapter/busy_reaction.go:35`; `main.go:3344`, `main.go:4766`, `main.go:5061` |
| Agent reaction instruction | PR #79 SHA `c7b243784153a8576009ed890e4647ce2f97b4f4`, `gchat/skills/google-chat/SKILL.md:42` |
| Slack agent react command | `slack-full/commands/react.sh:9`; `slack-full/scripts/slack_chat_react.py:25` |
| Human-arrival hook, fixed/auto/model choices | `slack-full/adapter/main.go:4073` |
| Keyword priority/table | `slack-full/adapter/ack_reaction.go:25` |
| Defaults and request bounds | `slack-full/adapter/ack_model.go:28`, `ack_model.go:152` |
| Fallback for model failure | `slack-full/adapter/ack_model.go:217` |
| Local endpoint example | Assigned bead gp-n32w, scope 5 (city record, not a live endpoint probe) |

Recommendation: keep the default off, trial `auto` on Hou with the busy
hourglass, then evaluate local model quality and latency. The PR asks Afik
whether to adopt (C) on every city with which mode or keep it Hou-only.
