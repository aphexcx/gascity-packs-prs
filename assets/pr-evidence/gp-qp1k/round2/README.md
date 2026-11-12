# Lost-answer replay follow-up

Scope: the delta after `3ee467b02858e5d2419d3f1159a66cc80d5b54b7` on draft
PR 98. R4.1 is this PR's incomplete activity attribution, not another PR's bug.
All fixtures are invented; no live adapter state, event logs or conversations
were read, and no test mail was sent to a live city.

## Fix

Before sending words, WhatsApp durably records the gateway key, attempt time
and session alongside its existing proof history. An unknown outcome keeps
that attempt across retries and restarts. When a proof first arrives, the
adapter records activity at the earliest uncertain attempt's time and session,
then atomically replaces the attempt with the proof. A known connection or
gateway refusal removes only that attempt; it cannot erase an older uncertain
attempt. Existing proof history still wins over any retry's attribution.

One helper covers text chunks (`outbound.js:492`), captioned files
(`outbound.js:619`), including recovery probes, and update templates
(`outbound.js:718`). Captionless files and owner notices keep their existing
paths. The switch, owner lines, mail bodies and timings are unchanged.

## Red and green class test

The committed `never-silent lost-answer replay class` table is in
`whatsapp/adapter/test/never-silent.test.js:128`. Each row rebuilds the publisher
and notice engine from durable fake state between the first attempt and retry.
Replay rows also change the retry caller's session and assert that activity
keeps the original time and session. Each row verifies one provider send,
then polls twice to verify the owner/mail counts stay at one or zero.

Run from `whatsapp/adapter/test`:

```sh
pnpm exec node --test --test-concurrency=2 --test-name-pattern='never-silent lost-answer replay class'
```

| Send kind | New message's cause | At base, test added only | After fix |
| --- | --- | --- | --- |
| Text chunk | Definite failure for three minutes | FAIL: 0 owner lines / 0 mails | PASS: 1 / 1 |
| Captioned file | Definite failure for three minutes | FAIL: 0 / 0 | PASS: 1 / 1 |
| Update template | Definite failure for three minutes | FAIL: 0 / 0 | PASS: 1 / 1 |
| Text chunk | Delivered, health `session_down` | FAIL: 0 / 0 | PASS: 1 / 1 |
| Captioned file | Delivered, health `session_down` | FAIL: 0 / 0 | PASS: 1 / 1 |
| Update template | Delivered, health `session_down` | FAIL: 0 / 0 | PASS: 1 / 1 |
| Text chunk | First connection refused; retry sends fresh words | PASS: 0 / 0 | PASS: 0 / 0 |
| Captioned file | First connection refused; retry sends fresh words | PASS: 0 / 0 | PASS: 0 / 0 |
| Update template | First connection refused; retry sends fresh words | PASS: 0 / 0 | PASS: 0 / 0 |

[Red transcript](class-red.log): six replay rows fail and three controls pass.
The Node runner reports 27 tests, 20 pass, 7 fail: it also counts the failed
parent and 17 discovery entries for files with no matching tests.
[Green transcript](class-green.log): all nine rows pass; runner 27 pass, 0 fail.
The failed assertions in the red run are the required owner/mail counts.
Absolute workspace paths in the transcript are replaced with `<workspace>`.

Contract item 2 is **DONE** with the definite-failure rows, alongside its
existing stopped/pending/unreachable fixtures. Item 3 is **DONE** with the
health rows and fresh-send controls, alongside its existing health fixtures.

## Other adapters

**Google Chat: this send-replay case is prevented.** Text and captioned uploads
share `internal/chat/chat.go:188`, `CreateMessageWithAttachmentsAndTime`, which
parses the provider's original `CreateTime` at line 225, including replay after
a lost answer. `internal/server/server.go:666` only records words when that
time is nonzero and passes it to `Act` at line 671. An omitted provider time
stays unknown; the retry's clock is never substituted. No source change needed.

**Slack: this send-replay case is prevented.** Text acceptance uses Slack's
original message timestamp (`main.go:2769`); recovered receipts do the same
in `recordDeliveredPublish` (`main.go:3315`). `ingestion_session_act.go:200`
parses the provider timestamp without a current-time fallback. Captioned-file
retries allocate a fresh upload/file ID (`main.go:2913`) before completion
(`main.go:2945`); they cannot replay a previously completed upload through
this handler. Their success therefore represents newly posted words. The
reaction path also excludes `already_reacted` from activity (`main.go:3289`).
No source change needed, so neither Go module was rerun for this follow-up.

## Verification

- `pnpm exec node --version`: v24.21.0. No unsupported-engine warning.
- Class test: nine rows pass, including all restart rows; runner 27 pass.
- `make test PYTEST_DIRS=whatsapp/tests`: 57 passed.
- `pnpm test` from `whatsapp/adapter`: 284 tests, 283 passed, 1 failed,
  0 cancelled/skipped. The unchanged pipeline test "on gc's first acceptance
  the owner's message is marked read (POST /v1/read), once, never for a status"
  timed out waiting for delivery 1's acknowledgement under concurrent load.
  Its isolated directory-based run,
  `pnpm exec node --test --test-concurrency=2 --test-name-pattern='first acceptance'`,
  passed in 219 ms (runner 18 pass including 17 discovery entries). No timeout
  or pipeline implementation was changed. The previously reported file-send
  timeout did not recur. pnpm printed its missing-node_modules advisory after
  the failed run; this adapter's tests import Node built-ins only.
- One fresh DEEP self-review is limited to this follow-up's delta. The mayor's
  whole-PR gate remains a separate handoff.
