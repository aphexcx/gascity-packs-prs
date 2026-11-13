# Lost-answer replay follow-up

Scope: the delta after `3ee467b02858e5d2419d3f1159a66cc80d5b54b7` on draft
PR 98. R4.1 is this PR's incomplete activity attribution, not another PR's bug.
All fixtures are invented; no live adapter state, event logs or conversations
were read, and no test mail was sent to a live city.

## Fix

Before sending words, WhatsApp durably records the gateway key, attempt time,
session and opaque attempt ID alongside its existing proof history. Unknown
outcomes keep those attempts across retries and restarts. When a proof first
arrives, the gateway's accepted attempt ID selects the original time and
session; the adapter atomically replaces pending attempts with that proof.
A known connection or
gateway refusal removes only that attempt; it cannot erase an older uncertain
attempt. Existing proof history still wins over any retry's attribution.

One helper covers text chunks (`outbound.js:507`), captioned files
(`outbound.js:634`), including recovery probes, and update templates
(`outbound.js:733`). Captionless files and owner notices keep their existing
paths. The switch, owner lines, mail bodies and timings are unchanged.

The gateway's optional `X-GC-Replayed` response header distinguishes a stored
answer from fresh acceptance, including after its 30-day idempotency cleanup.
The JSON body remains identical on replay. `X-GC-Send-Attempt` supplies an
opaque request ID, stored with the gateway claim and returned on every answer
for that accepted send. It identifies the correct local attempt even when a
fresh send after cleanup also loses its answer and a different session retries.
The token contains no session or conversation data. Older gateways omit the
headers, so the adapter conservatively keeps its earliest uncertain attribution.
Local journal failures return a transient failure so the same key can resume
when storage recovers.

Rollout requires applying nullable migration `0005_send_attempt.sql`, deploying
the gateway, then deploying the adapter. Existing records remain valid with
NULL, and older gateway code ignores the added column. Recognizing fresh words
after key expiry requires these gateway headers; no deployment is performed
by this PR.

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
[Final green transcript](class-complete-green.log): all nine rows pass; runner 27 pass, 0 fail.
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
- Final `pnpm test` from `whatsapp/adapter`: **299 passed, 0 failed,
  0 cancelled/skipped**, including all original and review regressions.
- Before the review fixes, `pnpm test` from `whatsapp/adapter`: 284 tests, 283 passed, 1 failed,
  0 cancelled/skipped. The unchanged pipeline test "on gc's first acceptance
  the owner's message is marked read (POST /v1/read), once, never for a status"
  timed out waiting for delivery 1's acknowledgement under concurrent load.
  Its isolated directory-based run,
  `pnpm exec node --test --test-concurrency=2 --test-name-pattern='first acceptance'`,
  passed in 219 ms (runner 18 pass including 17 discovery entries). No timeout
  or pipeline implementation was changed. The previously reported file-send
  timeout did not recur. pnpm printed its missing-node_modules advisory after
  the failed run; this adapter's tests import Node built-ins only.
- A second full `pnpm test` run passed 282/284, including the earlier failing
  acknowledgement test. Two different unchanged cases timed out: "(keeper r2
  M2, n10) /consent and /hours match a phone by its digits..." while waiting
  for the keeper's window read, and "(6b) the receipt read back as unknown
  (gc restarted, transcript kept)..." while waiting for its acceptance.
  From the test directory,
  `pnpm exec node --test --test-concurrency=1 --test-name-pattern='keeper r2 M2, n10|^\(6b\)'`
  passed both in 207 ms and 304 ms respectively (18 runner entries including
  16 unmatched discovery entries). All nine replay rows passed in both full
  runs. These changing integration timeouts are reported as load-sensitive
  verification limits; no implementation or deadline was changed for them.
- Gateway `pnpm test`: 274 tests passed in five files, including retention,
  accepted-attempt replay and migration of existing sends with NULL attempt IDs.
  `pnpm typecheck` passed.
- Replay plus review-regression filter: 41 runner entries passed. It covers
  the nine original replay/control rows, six journal-fault rows, six
  retention-boundary rows, and gateway-header decoding.

## Fresh DEEP self-review

One fresh gpt-6-astra review covered only
`3ee467b02858e5d2419d3f1159a66cc80d5b54b7..856279a5427f7d2d9d2af48735363a7bfbe23dbe`.
Its verdict was **BLOCK**, with two MAJOR findings. Both were reproduced and
fixed within this delta:

1. Activity-journal write failures were returned as permanent gateway
   refusals. Six fault-injection rows (before dispatch and after acceptance,
   for all three send kinds) were red, then green. They assert transient
   classification and successful same-key recovery across restart with one
   provider send. See `never-silent.test.js:193`.
2. An uncertain attempt could outlive the gateway's 30-day key retention and
   backdate a genuinely fresh reply. Three retention-boundary rows were red,
   then green, including expired captioned-file progress and a changed retry
   session. See `never-silent.test.js:222`. The gateway now reports replay in
   an optional response header; its test confirms replay even after 31 days
   before cleanup, then fresh acceptance after actual cleanup. Header decoding
   and the gateway's existing replay contract were also red, then green.
   Extending this case to lose the fresh answer too reproduced the same
   attribution class in all three send kinds. The durable accepted-attempt
   token closes it without relying on wall-clock guesses or the retry session.
   [That red run](retention-lost-answer-red.log) has three failing lost-answer
   rows, three passing received-answer controls, and a failed parent.

[Review regression red transcript](review-regressions-red.log): 28 runner
entries, 16 pass / 12 fail (nine scenario rows, their two parents, and header
decoding). [Final green transcript](review-regressions-final-green.log): 41 pass, including
the original class. No outside-delta finding was reported. No second review
was started; the final fixes await the mayor's whole-PR gate and Fable read.
