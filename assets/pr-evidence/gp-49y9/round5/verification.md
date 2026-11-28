# Round 5 verification

Code and tests at `5c848b89e6e3631598d9cd0e0a748957b658dbcd`. Go 1.27.3, Python 3.15.0, Node 24.21.0. Suites ran one at a time with the city toolchain first on PATH. Tests used fakes.

Round 4 recorded 22 passing packages, 787 top-level passes, 48,960 subtest passes and two expected skips. Python recorded 465 passes, 250 subtest passes and 127 skips.

| Command | Exit | Result |
| --- | ---: | --- |
| `env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json` | 0 | 25 pass, 1 skip packages, 980 pass, 2 skip top-level, 49482 pass subtests |
| `env -u GC_BIN -u NEVER_SILENT go test -race ./internal/inbound ./internal/catchup ./internal/config ./internal/ledger ./internal/server -p=1 -count=1 -timeout=30m` | 0 | 5 packages passed under the race detector |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/config -run '^(TestJoinReachbackValidation\|TestCoalesceWindowSettingBounds)$'` | 0 | 2 pass top-level, 16 pass subtests |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/server -run '^(TestStatusJevKeyIsOnlyPresence\|TestCoalescingStatus)$'` | 0 | 2 pass top-level, 0 subtests |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/catchup -run 'Join\|Reach\|FirstRead\|NewSpace\|RetainedWindow\|History'` | 0 | 25 pass top-level, 63 pass subtests |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/inbound -run 'History\|FirstRead\|Arrival'` | 0 | 14 pass top-level, 16 pass subtests |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/inbound -run 'Media\|Cleanup\|Deletion\|Edit'` | 0 | 20 pass top-level, 34 pass subtests |
| `env -u GC_BIN -u NEVER_SILENT go test ./internal/inbound -run '^TestNineArrivalsDeliverWhileJevIsHeld$' -count=3` | 0 | 1 named test, 3 passes |
| `gofmt -l .` | 0 | empty output |
| `env -u GC_BIN -u NEVER_SILENT go vet ./...` | 0 | empty output |
| `env -u GC_BIN -u NEVER_SILENT make test 'PYTEST_DIRS=tests gchat/tests' 'PYTHON=env -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTHON python3'` | 0 | 600 passed, 128 skipped, 263 subtests passed in 94.05s (0:01:34). tests 220 / pass 219 / fail 0 |

Focused family counts use matching terminal events from the full JSON run. The commands above were then run separately as shown. The Python command ran at the repository root. All Go commands ran in `gchat/adapter`.

Skipped top-level tests: TestRealGCContract, TestRealGCReplyReminder.

## Named results from the full run

- R14 (7 passes): `TestApplicationPostsAHeldTurnAtItsDeadline`, `TestDeliveryWakeApart`, `TestDeliveryWakeFailureLog`, `TestDeliveryWakeReceiptWhileMailIsHeld`, `TestDeliveryWakeSlow`, `TestDeliveryWakeWire`, `TestWindowTimerOwnsDeadline`.
- R15 (7 passes): `TestCoalescedKindReadersRetireOldHistory`, `TestCoalescingKeepsHistoryBoundary`, `TestUnansweredMergedTurnKeepsItsNoticeAcrossHistoryRetirement`, `TestUnansweredPostExistingLedger`, `TestUnansweredPostNoticeAcrossLiveFirstRead`, `TestUnsentMergedTurnIsRetiredWholeByTheHistoryBound`, `TestWithdrawnRecordKeepsHistorySpeaker`.
- R16 (7 passes): `TestCoalescedKindReadersRetireOldHistory`, `TestCoalescingKeepsHistoryBoundary`, `TestRetiredMergedDeletionPutsTheDeletedMarkBack`, `TestUnansweredMergedTurnKeepsItsNoticeAcrossHistoryRetirement`, `TestUnansweredPostExistingLedger`, `TestUnansweredPostNoticeAcrossLiveFirstRead`, `TestWithdrawnRecordKeepsHistorySpeaker`.
- R17 (1 passes): `TestRetiredMergedDeletionPutsTheDeletedMarkBack`.
- R18 (4 passes): `TestHeldActsSurviveARestart`, `TestNewKeepsTheUnansweredMemoryAndRecoversHeldActs`, `TestUnansweredPostOneTurn`, `TestUnansweredPostOrder`.
- R19 (10 passes): `TestOwnerChangeDeliveryClass`, `TestOwnerChangeKeepsDeliveryState`, `TestPollNeverPostsAHeldAct`, `TestRefusedMergedTurnStillRefreshesOnAWithdrawal`, `TestRetryNeverSendsWithdrawnText`, `TestUnansweredMergedTurnIsClassifiedBeforeAWithdrawal`, `TestUnansweredPostDeletedAfterFanout`, `TestUnansweredPostDeletedWithoutFanout`, `TestUnansweredPostEditedBeforeRecovery`, `TestUnansweredPostInterruptedWithoutDeletion`.
- Rounds 2 and 3 (19 passes): `TestCatchUpReportsPartialSuccess`, `TestCatchUpRetryCountsEveryAct`, `TestCoalescedKindReadersRetireOldHistory`, `TestCoalescedLedgerCompatibility`, `TestDueWindowNeverRefusesAnotherSpace`, `TestFromLineIsJudgedAsItDisplays`, `TestHistoryPreservesFoldedLiteralEditPrefix`, `TestMalformedHeldRecordIsOneUnresolved`, `TestMergedTurnCannotBeForged`, `TestOwnerChangeDeliveryClass`, `TestOwnerChangeKeepsDeliveryState`, `TestRetriedSingletonKeepsProvenance`, `TestRetryNeverSendsWithdrawnText`, `TestRetryTranscriptKeepsFirstText`, `TestSupersededIntentNeverRepostsOldText`, `TestWindowOffIsToday`, `TestWithdrawalReachesEveryStoredCopy`, `TestWithdrawalSaveSurvivesRestart`, `TestWithdrawnRecordKeepsHistorySpeaker`.
- Round 4 (3 passes): `TestFromLineWholeClassIsDelivered`, `TestNoticeFollowsEveryEnrolledMember`, `TestQuoteMergedHeadersWholeDisplayClass`.
- Original six conflict hunks (7 passes): `TestCoalesceWindowSettingBounds`, `TestCoalescingStatus`, `TestEditCleanupCheckpointRejectsOlderRevisions`, `TestJoinReachbackValidation`, `TestRetryNeverSendsWithdrawnText`, `TestStatusJevKeyIsOnlyPresence`, `TestSupersededHoldsARefreshedMemberBehindAnEditCheckpoint`.
- Updated tests or fixtures (8 passes): `TestCoalescedKindReadersRetireOldHistory`, `TestOffParityWithTheBase`, `TestOffParityWithTheBaseNeverSilentOn`, `TestOwnerChangeKeepsDeliveryState`, `TestRetriedSingletonKeepsProvenance`, `TestSenderFillCredentialRefusal`, `TestSenderFillResourceUnauthorized`, `TestSenderFillStalledWarning`.
- Arrival and withdrawal (2 passes): `TestArrivalObservationAndEditMediaStepOrder`, `TestWithdrawalReachesEveryStoredCopy`.
