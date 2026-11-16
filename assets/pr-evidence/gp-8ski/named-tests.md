# Named Go test receipts

Base production: `68884bf15089e79230bc88c55b591033e3fe927f` with only the final tests and fake Google overlaid in the same lane. Implementation: `bd69c499486c036f465233bac71dc4464e67333a`.

Three existing-behavior pins pass at both revisions: standing scope lists, application start/restart/refresh, and auth check. `TestMembershipModel` verifies the new fake scaffold itself. The other twenty named tests fail against baseline production and pass with the implementation. No named run skipped a test.

| Test | Baseline exit | Implementation exit | Top-level / subtests |
| --- | ---: | ---: | ---: |
| TestJoinAnswersTable | 1 | 0 | 1 / 12 |
| TestJoinMakesOneWriteForOneRoom | 1 | 0 | 1 / 0 |
| TestJoinRefusesWithoutTheOwnersYes | 1 | 0 | 1 / 13 |
| TestJoinReadsBeforeItWrites | 1 | 0 | 1 / 0 |
| TestJoinIsIdempotent | 1 | 0 | 1 / 2 |
| TestJoinSaysWhatHappened | 1 | 0 | 1 / 10 |
| TestJoinRecord | 1 | 0 | 1 / 0 |
| TestJoinCheck | 1 | 0 | 1 / 0 |
| TestMissingPermissionChangesNothing | 1 | 0 | 1 / 7 |
| TestJoinRequestReplacementAndRestart | 1 | 0 | 1 / 0 |
| TestStandingScopesAreUnchanged | 0 | 0 | 1 / 0 |
| TestJoinRejectsAmbiguousRequests | 1 | 0 | 1 / 0 |
| TestJoinConcurrentApprovalIsUsedOnce | 1 | 0 | 1 / 0 |
| TestJoinDurableApprovalFailureStopsWrite | 1 | 0 | 1 / 0 |
| TestJoinApprovalCannotCrossIdentityOrUsePartialCode | 1 | 0 | 1 / 5 |
| TestJoinFailureNeedsFreshApproval | 1 | 0 | 1 / 0 |
| TestJoinPermissionFailureKeepsPendingRequest | 1 | 0 | 1 / 0 |
| TestJoinUnknownReadNeverAsksForApproval | 1 | 0 | 1 / 0 |
| TestJoinRecordEveryAttemptWithoutMessageText | 1 | 0 | 1 / 0 |
| TestNothingAsksForTheWriteScopeAtStart | 0 | 0 | 1 / 0 |
| TestJoinRefusalDoesNotNotify | 1 | 0 | 1 / 0 |
| TestJoinLedgerCompatibility | 1 | 0 | 1 / 0 |
| TestMembershipModel | 0 | 0 | 1 / 0 |
| TestAuthCheckKeepsStandingScopes | 0 | 0 | 1 / 0 |

## Exact invocations

Each command below ran once against each revision; counts and exit status are in the table above.

```sh
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinAnswersTable$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinMakesOneWriteForOneRoom$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinRefusesWithoutTheOwnersYes$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinReadsBeforeItWrites$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinIsIdempotent$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinSaysWhatHappened$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinRecord$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinCheck$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestMissingPermissionChangesNothing$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinRequestReplacementAndRestart$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/token -run ^TestStandingScopesAreUnchanged$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinRejectsAmbiguousRequests$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinConcurrentApprovalIsUsedOnce$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinDurableApprovalFailureStopsWrite$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinApprovalCannotCrossIdentityOrUsePartialCode$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinFailureNeedsFreshApproval$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinPermissionFailureKeepsPendingRequest$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinUnknownReadNeverAsksForApproval$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run ^TestJoinRecordEveryAttemptWithoutMessageText$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/app -run ^TestNothingAsksForTheWriteScopeAtStart$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/app -run ^TestJoinRefusalDoesNotNotify$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/ledger -run ^TestJoinLedgerCompatibility$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/fakes/fakegoogle -run ^TestMembershipModel$ -count=1 -timeout=2m -json
env -u GC_BIN -u NEVER_SILENT go test ./cmd/gc-gchat-adapter -run ^TestAuthCheckKeepsStandingScopes$ -count=1 -timeout=2m -json
```

