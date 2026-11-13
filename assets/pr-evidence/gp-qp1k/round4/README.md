# Round four: outage watch preconditions and committed replay activity

Base: `1c9304c617a71b9ec4abc0618ca6a0a872a839ca`.
Implementation: `535332c3`; test expectation correction: `4c9f757e`.
The review and verification records name the full commits.
This is a delta-only follow-up on draft PR 98. All fixtures are invented.

## Regressions, red before implementation

| Condition | Base result | Fixed result |
| --- | --- | --- |
| Google Chat lock missing or replaced, with and without credentials | All four rows rewrite the replacement notice state and send operator mail | All four preserve the file contents and inode; zero owner lines and zero operator mail over two later ticks |
| Google Chat credential hold, space known and unknown | Both rows reserve during the hold and post zero lines after sign-in | Both reserve nothing during the 45-second hold, mail the operator once, and post exactly one line after sign-in |
| Google Chat resolved ledger outcome before registration | One owner line and one operator mail; no local reconciliation | Reconciled while registration still returns 503; zero lines and zero mail |
| WhatsApp committed proof replay after notice-state write failure | Three template rows fail the activity assertion; six text/file controls pass | All nine rows pass; the template restores the original answer time and the uncertain-stop outage clears |

The Google Chat tests are `TestNeverSilentGateLockLoss`,
`TestNeverSilentGateCredentialHold`, and
`TestNeverSilentGateReconcilesBeforeRegistration`. Their green run also covers
the previous round's two startup tests: 13 passing test events, counting parent
tests and subtests.

The WhatsApp test is `never-silent committed proof replay repairs the template
activity after a notice write fault`. Its focused red run has six passes and
four failures (three template rows plus their parent); the focused green run
has ten passes and no failures. Text and captioned-file retries remain controls
for the separately deferred storage-recovery issue: their saved progress skips
the gateway and their missing activity remains unchanged.

`gchat-red.log` includes an initial lock-test fixture error: the replacement
engine lacked its required fake sender. `gchat-lock-red.log` is the corrected
lock test, still against the unchanged base implementation. The credential
and reconciliation failures in the first log are valid regressions. The
WhatsApp controls were calibrated to the existing captioned-file error kind;
after the fix, the expected answered-message behavior was corrected for the
failed-result template row. The red activity assertion precedes that assertion
and is unchanged.

## Implementation boundaries

The Google Chat watch checks ownership and reconciles local ledger outcomes
before each poll. Its engine guards saves and effects with the same ownership
check, including the final owner-dispatch check. Losing the lock permanently
ends the watch. Provider readiness uses the client's actual credential source
and the shared credential-hold flag; it gates owner reservations while operator
mail continues.

Local ledger reconciliation is extracted from the inbound recovery pass without
changing its cases: committed session words, resolution and withdrawal,
delivered and whole-paste outcomes, unresolved stops, and no-route outcomes.
It performs no network operations and does not wait for registration.

WhatsApp's replay branch retires the speculative retry and returns any activity
already committed for that conversation and provider proof. It preserves the
original session and time. A replay without such proof or a recognized pending
attempt still invents no activity. The previous switch-enablement class remains
part of the full class run.

Owner lines, mail bodies, timing constants, switches, gateway and migrations are
unchanged. The Slack adapter is unchanged. The general notice-state write
recovery issue and the Slack registration-budget note remain outside this round.

## Validation

Toolchains: Go 1.27.3 and Node v24.21.0 as reported by `pnpm exec node --version`.
Every Go test and vet command unsets `GC_BIN`; fixture tests install their own
fake command where needed. No test uses a live conversation or live city mail.
Command manifests record exits; `verification.txt` records final suite counts.
Focused red and green logs are included alongside this file.

The Google Chat full run hit the unchanged
`TestCommandPreparationFailureSettlesExistingIntent/not-ready` and `/send-lock`
200 ms fixture deadlines. The test passes all three rows in isolation; its
source is unchanged and is not adjusted here. The unchanged 3,000-message
catch-up test also exhausted its package-derived time budget at 2,526 messages;
its isolated rerun delivered all 3,000 and passed in 324 seconds with an explicit
30-minute test timeout. Isolated logs and manifests are included. No existing
tests needed changes.

Final full-run counts: Google Chat 1,898 pass, five failed events (the three
deadline/budget rows and two parents), two skips; all failed cases pass isolated.
Slack: 13,188 pass, three skips. WhatsApp: 338 pass; its class alone: 78 pass.
WhatsApp Python: 57 pass. Both Go vet runs are clean.

The fresh DEEP self-review is scoped to the delta from the base above. Its
record and any corrections are described in `self-review-1.md`.
