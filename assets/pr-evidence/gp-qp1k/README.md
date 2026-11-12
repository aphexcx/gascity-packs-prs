# Never-silent adapter evidence

This change adds opt-in owner notices and durable outage mail to Google Chat,
Slack and WhatsApp. `NEVER_SILENT` remains off by default. No pack version,
live adapter, customer conversation or deployed configuration was changed.

## Contract coverage

| Item | Result and verification |
| --- | --- |
| 1 | DONE: only newly admitted, addressed human messages are tracked. Adapter integration tests exclude replay, ambient, bot and edit traffic; WhatsApp excludes keeper/status/reaction events. |
| 2 | DONE: definite failures and transport failures have separate continuous waits; pending resets them. Stopped unknown deliveries wait three minutes without a session act before posting. Fake adapter tests and durable-engine tests exercise these paths. |
| 3 | DONE: all four health states and missing/malformed/stale/unknown evidence are tested; whole-paste pending requires conclusion. Session activity suppresses health notices. |
| 4 | DONE: three-minute failure/delivery waits, five-minute freshness, six-hour cooldown and mail retry bounds are named constants. Tests advance fake clocks. |
| 5 | DONE: fixed owner strings use the same conversation/thread and reply identity. Channel senders are separate from normal publishes. |
| 6 | DONE: the owner reservation is durable before posting. Aggregation, restart and refused-post tests verify at-most-once attempts within six hours. |
| 7 | DONE: per-message notification marks prevent reminders after the cooldown. No owner recovery message exists. |
| 8 | DONE: notices bypass session reply, transcript and publish bookkeeping. Partial successful session words and attributed reactions still suppress notices. |
| 9 | DONE: local reports quote successful owner notices. WhatsApp cause-one mail also survives owner refusal and uncertain-delivery grace. |
| 10 | DONE: durable per-adapter/per-cause outages aggregate messages and conversations. A missing operator context sends no remote mail. Fake gc executables verify arguments. |
| 11 | DONE: operator mail uses aggregate fields only. Canary tests verify that message and conversation coordinates do not escape. |
| 12 | DONE: cleared mail records start/end times, even if the start-mail retry lifetime expired. |
| 13 | DONE: start and cleared mails have independent durable pending/sent marks and one-minute retries for 24 hours. Restart tests verify successful sends are not repeated. |
| 14 | DONE: text and JSON status expose enabled state, health/freshness, open outage counts/sent marks and seven-day successful owner-post counts. |
| 15 | DONE: each pack README and CHANGELOG documents the feature. Pack versions are unchanged. |

Core tests are under each Go adapter's `internal/neversilent`, the Google Chat
inbound/server integration test directories, Slack's `never_silent_test.go`,
and WhatsApp's `never-silent.test.js` and `never-silent-integration.test.js`.
Every test uses temporary state and fake services; no test sends real mail.

Slack company rooms, registered-agent DMs and MPIMs have admission markers in
their primary receipts. Only local mayor targets qualify, and notices use that
target's normal bot identity. Legacy explicit targets other than mayor do not
qualify. Recovery reconciles primary records without enrolling historical traffic.

## Reproductions

The initial Go health, episode and adapter tests failed to compile before their
new interfaces existed. Those were construction checks, not runtime reproductions.
The later review regressions failed against the preceding implementation:

| Adapter | Runtime red reproduction | Passing regression |
| --- | --- | --- |
| Google Chat | Uncached named space posted outside the owner thread. | `TestNeverSilentFirstNoticeResolvesUncachedSpaceType` |
| Google Chat | Primary-only terminal transitions missed stops or retained resolved outages. | `TestNeverSilentReconcilesPrimaryTerminalTransitions` |
| Google Chat | Provider-created threads hid top-level session words. | Server publish/activity integration cases, plus `TestNeverSilentRepairsSuccessfulPublishActivityFromLedger` |
| Slack | Company room, DM and MPIM each produced zero expected owner posts. | `TestNeverSilentCompanyHumanRoutes` |
| Slack | An explicit other-agent turn posted a mayor notice; a primary journal reply failed to suppress it. | `TestNeverSilentSlackTransportAndEligibility`, `TestNeverSilentCompanyRecoveryAndSessionActivity` |
| WhatsApp | A notice queued behind a successful reply still sent: two sends instead of one. | `never-silent rechecks eligibility inside the publisher queue after session words` |
| WhatsApp | Primary-only operator resolution left a stopped sidecar entry eligible after restart. | `never-silent integration: primary delivered resolution clears a stale sidecar stop after restart` |
| WhatsApp | JSON status retained full synthetic conversation identifiers. | Status command fixture in `whatsapp/tests/test_pack.py` |

Focused final checks passed: Google Chat engine 14 top-level tests / 34 total
test events; Slack regressions 14 / 56 across two packages; WhatsApp 33 tests;
Slack Python company-activity tests 3 passed. Full-suite results are below.

## Posted fixture text

Captured test output is in [go-fixtures.txt](go-fixtures.txt) and
[whatsapp-fixtures.txt](whatsapp-fixtures.txt), including both mail forms,
recorded executable arguments, owner strings and canary checks.

Google Chat and Slack each log all three posted strings from
`TestHealthRequiresDeliveredAndNoSessionAct`. WhatsApp logs them from
`never-silent health states post the exact owner lines only after delivery`.
Each adapter posted these exact strings in its fake sender:

```text
I can't answer yet because no Claude account is signed in for me. The team that runs me has been told.
I've reached my usage limit for now and can't answer until it resets. The team that runs me has been told.
I can't read your message right now. The team that runs me has been told, and I'll answer as soon as I'm back.
```

The last string covers both `not_delivered` and `session_down`.
`TestEpisodesPersistAndAggregateByCause` captured this start and clear pair:

```text
Subject: never-silent: example-city: gchat: not_delivered
city: example-city
channel: gchat
cause: not_delivered — the message did not land and will not without recovery
started: 2026-11-12T08:03:00Z
conversations: 4
messages: 12
oldest_age_seconds: 180
owners_told: true (4 conversations)
status: gc gchat status --json

Subject: never-silent: example-city: gchat: not_delivered cleared
city: example-city
channel: gchat
cause: not_delivered — the message did not land and will not without recovery
started: 2026-11-12T08:03:00Z
ended: 2026-11-12T08:03:01Z
```

The fake executable in `TestFakeGCBinaryRecordsRemoteMailArguments` captured
these arguments, one per line (the body after `-m` is one argument):

```text
mail
send
--context
example-operator
mayor
--dedup
never-silent:1c8273ca7c37964ae254c130cf3ce455
-s
never-silent: example-city: gchat: not_delivered
-m
city: example-city
channel: gchat
cause: not_delivered — the message did not land and will not without recovery
started: 2026-11-12T08:00:00Z
conversations: 1
messages: 1
oldest_age_seconds: 0
owners_told: true (1 conversations)
status: gc gchat status --json
```

The same fake executable test runs in Slack. WhatsApp's integration test
`refused message posts once without reply bookkeeping and mails fake gc`
captures the remote argument array and checks `--context`, `--dedup`, and
the absence of synthetic customer canaries. Go's
`TestOperatorMailContainsNoCustomerCanaries` prints only the aggregate body
shown above and passes in both adapters. WhatsApp's
`mail retry marks survive restart and remote mail has no customer canaries`
also passes. No fixture contains customer text or a real conversation ID.

## Delivery classification

These are observation rules; the existing delivery/retry policy remains in
charge of retrying customer traffic.

- Google Chat: `internal/inbound/inbound.go:301` reconciles unresolved stops,
  committed `no_route`, and resolved/delivered records. `outcome` at line 1259
  observes failed and nonfragment held outcomes; exhausted fragmented retries
  stop uncertain at line 1316. `unresolved` at line 1342 excludes withdrawal
  and recognizes rejected POSTs and retry causes with no fragment as definite.
  The transport branch at line 1161 recognizes dial errors, city-not-found
  and 5xx, while 408/429 reset the failure streak.
- Slack: `never_silent.go:135` separates vouched delivery, concluded whole-paste
  pending, definite failed members and narrow unreachable transport.
  `stopNeverSilent` at line 192 covers normal and alias dead letters and the
  receipt-follow-up stop paths. `never_silent_recovery.go:26` reconciles company
  receipts; all eligible failed targets stop, with uncertainty unless all have
  definite failure evidence. `companyNoticeTransport` at line 105 distinguishes
  dial/5xx/city-not-found from timeout, missing session and rate limiting.
  Primary dead-letter replay at line 287 never revives a newer resolution.
- WhatsApp: `src/inbound.js:653` treats failed/no-route with no pasted bytes
  as definite; concluded pending with every whole payload pasted is delivered.
  Terminal holds at line 670 stop uncertain except for that delivered case.
  `definiteStop` at line 865 recognizes rejected, no-route and a repost bound
  backed by definite evidence; other dead letters stop uncertain. Startup
  reconciliation at line 194 restores primary delivery/hold/operator outcomes.

Pending always resets a continuous failure streak. Health notices require
delivered evidence and fresh health. Session words/reactions in the conversation
suppress both kinds of notice, including recovered attributed activity.

## Verification

Toolchains: Go 1.27.3 on darwin/arm64; Node v24.21.0 from
`pnpm exec node --version`. No unsupported-engine warning was observed.

- `make test`: 2,698 passed, 126 skipped, 10,734 subtests passed, with one
  failure in unchanged `PnpmShimTests.test_concurrent_callers_wait_for_one_install_instead_of_racing_it`.
  All three fake commands succeeded and exactly one install occurred; a caller
  scheduled after the two-second install did not print the expected second
  waiting diagnostic. From `gascity`,
  `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests -k PnpmShimTests -q`
  passed all 17 tests (513 deselected). The earlier full repository run passed
  2,696 tests before the three added company-activity cases. Two existing Python
  warnings concern `os.fork()` from a multithreaded test.
- `make test PYTEST_DIRS=whatsapp/tests`: 57 passed.
- WhatsApp `pnpm test` (the package script runs
  `node --test --test-concurrency=2 'test/*.test.js'`): 274 passed, no skips or
  failures, with no competing suite running.
- Slack at the round 2 revision, `go test ./... -count=1 -json`: 1,516 top-level tests and 13,178 total
  test events passed across three packages.
- Google Chat at the round 2 revision, `go test ./... -p 1 -count=1 -timeout=30m -json`: 678 top-level
  tests and 1,886 test events passed; 21 packages passed and the server package
  failed only unchanged `TestCommandPreparationFailureSettlesExistingIntent/send-lock`.
  Its 200 ms deadline starts before durable intent preparation and expired
  before the intended send-lock branch. The entire server package rerun,
  `go test ./internal/server -count=1 -json`, passed all 93 top-level / 217
  test events, including that case. The earlier R2 changed-package run passed
  249 top-level / 617 events across four packages. Before R2, the full suite
  passed 675 top-level / 1,882 events across all 22 packages.
- `go vet ./...` passed in both Go modules. Google Chat `gofmt -l .` was clean.
  Slack's same check reports only unchanged `company_thread_ambient.go`; its
  indentation difference was independently confirmed at the base commit.
  Every changed Go file is formatted. The scoped `git diff --check` passed.

The initial repository run exposed stale Slack status fixtures and the empty
session field on the Google Chat reaction command. Both were corrected before
the passing repository run. Earlier Google Chat and WhatsApp runs under
competing load had readiness/pipeline timeouts; isolated runs passed. No
production retry or test timing was changed to obtain the passing results.
The repository and WhatsApp Python checks preceded the R2 Go/JavaScript fixes;
R2 changed no Python code.

## Review

The fresh read-only self-review found 11 MAJOR issues and one MINOR issue.
All were fixed in `1d402bbe6ac6552abab6a29ad517e9aa251c7eaa`; a read-only
follow-up of that exact commit confirmed every disposition. Fault classes:

- Lost session activity: missing receipt targets, top-level Slack words and
  partial Google Chat publishes now suppress notices.
- Invalid delivery evidence: pending receipts require conclusion; member
  evidence outranks Slack summary status; stale definite evidence cannot skip
  an ambiguous stop's grace; definitive stopped Slack failures are immediate.
- Withdrawn messages: Google Chat deletion removes notice eligibility.
- Durable effects: cleared mail survives start expiry; Go network effects do
  not block activity recording; WhatsApp admission/stop observations survive
  crashes; local failure mail does not depend on owner-post success.

Regression tests reproduced the reported classes before their fixes. The
ambiguous-retry test was extended to assert the grace behavior. The mayor's
gate and cross-model read remain the subsequent handoff steps.

Native Codex DEEP round 1 reviewed `1d402bbe6ac6552abab6a29ad517e9aa251c7eaa`
and returned BLOCK: five MAJOR findings and one MINOR. Dispositions in this
revision:

1. FIXED — Missing company ingress integration: rooms, DMs and MPIMs now
   track only admitted human mayor targets, persist observations and select
   the acting bot token. The three-route integration reproduction passes.
2. FIXED — Missing Google space metadata: notice posting resolves an uncached
   space before choosing threading options. The uncached-space test passes.
3. FIXED — Lost conversation activity: top-level and cross-thread session
   publishes/reactions suppress notices; successful primary send records repair
   a missing observation after restart.
4. FIXED — Stale queued eligibility: WhatsApp rechecks inside the outbound
   conversation queue immediately before sending. The held-reply race passes.
5. FIXED — Divergent primary and observation stores: Google terminal/operator
   transitions, Slack normal/alias dead letters and company receipts, and
   WhatsApp probe/operator/terminal outcomes reconcile using original timestamps.
   Replay never re-enrolls historical traffic or revives a newer resolution.
6. FIXED (MINOR) — WhatsApp JSON privacy: keeper and ledger conversation maps
   use the same identifier redaction as text status.

An additional read-only advisory review of the working delta identified related
cases: company mayor-target filtering, corrupt historical records blocking
polling, company synthesis/result activity, Google `no_route` and successful
publish recovery, and WhatsApp terminal whole-paste pending. These were fixed
within the same classes and covered by focused tests. It was an uncommitted
working-tree read, not a verdict on a pinned commit.

Native Codex DEEP round 2 reviewed `22ae627abb1de07e6c6330043e078607b0b8bd48`
and returned BLOCK with five MAJOR findings. Each class was reproduced before
its production fix:

1. FIXED — Successful Slack words were coupled to readback and optional proof
   recording. Text posts now record activity immediately on acceptance;
   captioned uploads do likewise. Original session/time also survive in primary
   text receipts. `TestNeverSilentSuccessfulWordsBeforeReadbackAndRecovery`
   covers failed readback, captioned and captionless uploads, and restart.
2. FIXED — WhatsApp primary success could precede the activity observation.
   Chunk progress, media receipts and the template journal now retain original
   attributed activity, reconstructed before polling. The captioned-receipt
   SIGKILL integration test restarts without replay; the text, partial-text and
   template recovery test also checks that retries under another session do
   not answer a newer request.
3. FIXED — Terminal Google whole-paste pending receipts were treated as
   undelivered. A durable positive observation now coexists with the existing
   primary hold. `TestNeverSilentTerminalWholePasteIsDeliveredAcrossRestart`
   covers delivery-level and member-level terminal flags, unknown health and
   health eligibility after restart.
4. FIXED — A new pending attempt retained an old stopped marker. Both Go
   engines clear superseded terminal and positive evidence on newer results.
   `TestNewPendingSupersedesStoppedAndDeliveredEvidence` reproduces the class;
   the company recovery test includes the primary redrive path.
5. FIXED — Notice eligibility became stale during dispatch preparation. Both
   Go engines revalidate the current fixed line; senders check again after
   space lookup, rate waiting or identity selection, just before the request.
   `TestHealthChangeDuringOwnerSendDoesNotPostStaleLine` and
   `TestNeverSilentSessionActDuringSpaceLookupCancelsNotice` reproduce both
   stale copy and intervening session activity.

Native Codex DEEP round 3 reviewed `eb338e4ee2db32f51fda47a3b1102f1068e7ec3d`
and returned BLOCK with two MAJOR classes. Both were reproduced before fixing:

1. FIXED — Provider replays could make old actions look current. Google text
   and captioned-file creates now preserve the provider's original creation
   time, including acceptance followed by a failed local receipt write.
   `TestNeverSilentReplayedCreateRetainsOriginalActivityTime` reproduces both
   cases. WhatsApp retains original proof/session/time separately from prunable
   outbound progress; its replay test covers the seven-day age and 1,024-entry
   count bounds. The gateway returns only a message ID, so this local history
   supplies the original evidence. Slack keeps `already_reacted` successful
   for the reaction API, but it creates no fresh activity or journal entry.
   `TestNeverSilentRepeatedReactionIsNotNewActivity` includes a fresh reaction
   control and restart reconciliation.
2. FIXED — A withdrawal received before asynchronous enrollment was lost.
   Both Go engines now persist a removed-message record even for an unknown
   ID. Later tracking and primary recovery respect that record.
   `TestWithdrawalBeforeEnrollmentSurvivesRestart` reproduced the shared fault.
   `TestNeverSilentCompanyHumanRoutes` also reproduced it for rooms, DMs and
   MPIMs with occupied dispatch slots, deletion, observer restart and receipt
   sweep; all three pass after the fix.

After these fixes, Google `go test ./internal/neversilent ./internal/chat
./internal/server -count=1 -json` passed 144 top-level / 320 test events.
Slack `go test . ./internal/neversilent -run
'TestNeverSilent|TestWithdrawalBeforeEnrollment|TestHandleReact' -count=1 -json`
passed 15 top-level / 70 events. The full-suite results above are labeled by
their prior stage where applicable; later full checks are recorded in READY.

## Source research and limits

At pinned gc `a8217556`, `cmd/gc/cmd_mail.go:1544` accepts remote `--dedup`,
but the remote-dispatch branch at 1787–1791 does not forward the key. The flag
is passed; durable adapter sent marks provide the actual guard. An ambiguous
command result can still be retried, so this is not a distributed exactly-once
mail guarantee.

The gc request budget is 15 seconds (`internal/api/handler_extmsg.go:375`),
and its background context is 30 seconds (`internal/api/server.go:24`). A
pre-landing error is failed; errors after landing become pending
(`internal/extmsg/inbound_member.go:90`). No live cold-start latency or bare-shell
delivery was measured. Suspension remains a reconciler concern
(`cmd/gc/session_reconciler.go:1975`, `:4393`); all combinations of wake and
drained/quarantined/suspended state were not measured live. Closed session rows
are rejected by `internal/session/chat.go:470`; the resulting external routing
answer depends on the binding's resolution and remains unmeasured end to end.
Slack's direct-message failed/unconfirmed branch restores its buffered retry
copies (`slack-full/adapter/main.go`, direct-message delivery branch).

The host's gc reports `dev` without a usable revision in its build metadata;
equality to the pinned source is unverified. Nothing here probes the hub or
creates a health writer. The independent writer, seat-cap detection, operator
wake/alerting and deployments are outside this change.
