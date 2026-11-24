# Round 5 merge of main into PR 106

| Revision | Commit |
| --- | --- |
| Prior PR head and first parent | `48c4097b1e48660b3c1373572e71bd7bce44d065` |
| Main and second parent | `7876f9768281dc85036160c9dddd71173401b5e3` |
| Merge | `21e12b6a17a69f72a7d24f42d6ac8df2ff8d3431` |
| Old merge base | `863e9aa88795101b45734f3fc0fbdcbbba2d6229` |

Git attempted to combine changes in 14 files. Nine merged automatically and five had conflicts, with ten diff3 regions. The four commits added to main after the resolution was specified left that set unchanged. [merge.txt](merge.txt) contains Git's output and the three sides of each region. Remove the initial `>` from each quoted line to recover Git's diff3 text. The table uses paths under `gchat/adapter/internal/`.

| Hunk | PR side | Main side | Resolution |
| --- | --- | --- | --- |
| 1 · app/app.go | Catch-up import | Acknowledgement and arrival imports | Kept all three imports. |
| 2 · app/app.go | Mutex for notice state | Arrival service, timer writer, state directory and identity discovery flag | Kept all fields and aligned them with gofmt. |
| 3 · config/config.go | Optional first-start window | Join reach-back duration | Kept both settings. |
| 4 · config/config.go | FirstStartWindow | JoinReachback | Kept each accessor and its default. |
| 5 · config/config.go | Window validation | Reach-back validation | Each configuration key keeps its own bounds and error. |
| 6 · ledger/ledger.go | History floor and last inventory | Join requests and attempts | Kept all four fields and their JSON tags. |
| 7 · ledger/ledger.go | HistoryStart | Revised JoinedAt description and FirstRead | Kept main's join description and first-read state, then HistoryStart. |
| 8 · catchup/catchup.go | Update SpacesListedAt | Advance LastSpaceList only for a later list | Kept each write with its original condition. |
| 9 · catchup/catchup.go | Bound both streams | Reach back on the first message read | Events use the room bound. A first message read can cross the join within its margin and above the installation floor. |
| 10 · catchup/catchup_test.go | Cursors equal the fixture's recorded epoch | Cursors equal the time the room was found | The recorded-list fixture uses the epoch. A separate fixture without a recorded list exercises the time-found fallback. |

### Rulings and test edits

| Ruling | Production behavior | Regression check |
| --- | --- | --- |
| A | `begin` keeps the installation floor and the room bound separately. `HistoryRead(floor)` still computes the same maximum of the floor, join and history start. | The pair test joins one minute above the floor, with mentions on opposite sides of the floor. It checks the first request's lower bound, backlog and delivered message ID. |
| B | For a listed bare room, start at the latest of the two recorded list times and the floor. Accept a valid membership time at or above that bound. Use the time found if all these are absent. Retire detached attempts directly after starting the first read. | Sweeps retain the first-start window and retirement counts. The no-list fixture exercises the fallback. |
| C | A first read can admit a created message before the join only in a DM or for an identity mention, strictly above its lower time and the installation floor. An independent history start still applies. | Live delivery and recovery share the filter, including a live copy that arrives before the first read records the message. |
| D | An open read with an existing cursor resumes. A cursorless open read restarts at its history start. | The legacy-token test checks the decoded lower bound and still checks that no membership request or first-read state was created. |
| E | The parity fixture supplies a saved list time and the live event time through JSON, so the test can also compile at its historical base. | Both variants retain their expected deliveries and state. |

| Existing test or fixture | Before | After |
| --- | --- | --- |
| TestRetainedWindowUsesGoogleClock | `f.r.begin(s, frozen)` | `f.r.begin(s, frozen, f.opts.Ledger.HistoryFloor)`. The fixture sets a list time and leaves the floor at zero. |
| TestNewSpaceUsesDefaultWake | Main expected both cursors equal to `frozen`. | Both equal `time.Unix(0, 0)`, the shared fixture's saved list. |
| TestMembershipReadThatMayPassFailsTheFreeze | Inherited the shared fixture's saved list. | `f.opts.Ledger.SpacesListedAt = time.Time{}`. The time-found assertion stays intact. |
| TestFloorBoundsEveryNewSpace | `!s.JoinedAt.IsZero()` failed the test. | `!s.JoinedAt.Equal(floor)` fails the test. Cursor, lower-bound and turn assertions are the same. |
| TestTheMessageThatAddsTheIdentityIsATurn | First start used a zero history window. | `window := "1h"` and `f.cfg.Settings.FirstStartHistory = &window` model a running installation. The fixture's adding message precedes its first connection by about two seconds. All assertions are retained. |
| TestLegacyOpenReadKeepsItsBounds | Required the original encoded token. | Require successful decode, a nonzero history start and a message lower equal to it. |
| TestOffParityWithTheBase and its never-silent variant | Fresh empty ledger and untimed event changes. | JSON supplies `spaces_listed_at` one hour before setup and `EventTime` at each step. The expected values are identical. |

### Patch identity

`git diff -U0 OLD_BASE OLD_HEAD` and `git diff -U0 MAIN MERGE` were each piped to `git patch-id --stable`. The original five-file exclusion yielded `780a5a5a3d345d153f980b88ba6844f87f82255f` before and `b4d0c82a334585c6201edac711f4135e73882913` after. Seven additional paths contain explicitly ruled changes. Excluding those paths as well yields `d5ea5b7eb908a14e9cc06aabc2603fc2d01c44d5` on both sides. Both comparisons also exclude this evidence directory.

The comparison with Git's unresolved tree `6d26a20759b7f53c152ce2a810e90ea1a6b07ad6` finds the five conflicted files and exactly seven other paths. Those seven are `gchat/CHANGELOG.md`, `gchat/README.md`, `app/keyholder_test.go`, `catchup/history_floor_test.go`, `catchup/join_test.go`, `inbound/history.go` and `inbound/offparity_test.go` (Go paths relative to `gchat/adapter/internal/`). The [per-file table](patch-ids.tsv) covers all 373 paths in either delta, including the other named files. Eleven patch IDs differ. The resolved application struct and imports keep the application's original PR delta byte-for-byte under the zero-context check.

An independent reconstruction started a temporary Git index at the merge and replaced just those seven paths with their automatically merged entries from the unresolved tree. The resulting tree, `f8ae8f2182b6194f48a87446d5a30e5d72e48328`, differs from the unresolved tree in precisely the original five files. The original five-exclusion calculation against that reconstruction yields `780a5a5a3d345d153f980b88ba6844f87f82255f`, identical to the PR before this round.

### Changed hunks in the seven ruled files

These are the inclusive added-line ranges from the zero-context diff against the unresolved tree. None of the four main commits after `9bcab163` touched any of the twelve paths. The nine files that merged automatically show 82 combined, zero-context hunks at the resolved head (`git show -c -U0`).

| File | Lines at the merge | Ruling that orders the change |
| --- | --- | --- |
| gchat/CHANGELOG.md | 5 | B(5) requests one CHANGELOG line under Unreleased. |
| gchat/README.md | 643–644, 648–649 | D qualifies preservation of open reads by the presence of a cursor. B(5) requires the installation floor alongside the recorded inventory. |
| gchat/adapter/internal/app/keyholder_test.go | 323–325 | A gives the fixture in TestTheMessageThatAddsTheIdentityIsATurn an explicit one-hour window after replacing the identity token. |
| gchat/adapter/internal/catchup/history_floor_test.go | 41 | B(4) changes the join assertion in TestFloorBoundsEveryNewSpace from zero to the floor. This is the file's sole changed line. |
| gchat/adapter/internal/catchup/join_test.go | 89–118, 250–251, 331–332 | A asks for TestFirstReadNeverReachesBelowTheInstallationFloor with mentions on opposite sides of the floor. B(4) clears the shared list time in TestMembershipReadThatMayPassFailsTheFreeze. D replaces encoded-token equality with successful decode, a nonzero history start and a matching message lower. |
| gchat/adapter/internal/inbound/history.go | 257–267, 296–298 | C requires a private firstReadAdmits filter and clears only the before_history_floor cause for qualifying messages in Processor.history. |
| gchat/adapter/internal/inbound/offparity_test.go | 246–248, 271–275 | E requires JSON to set the ledger's list time and the time of each change so both parity variants still run at their base. |

The README paragraphs and CHANGELOG entry below are copied from the merge.

~~~~text
The identity's join event also triggers a read, and the hourly inventory sweep
recovers rooms discovered without an event. A room's first message read starts
`join_reachback` before its join time (default `"5m"`, maximum `"60m"`;
`"0s"` disables it). Negative or invalid durations are refused. Before the
join, only messages mentioning the identity or messages in a direct message
can become turns. Other pre-join messages are discarded before queuing and use
no delivery budget. Space events still start at the join. The saved lower bound
survives a restart in the middle of the read; subsequent reads use the normal
cursors and overlap. A known room with cursors never gains this reach-back,
including when the identity leaves and rejoins. Existing open reads with a cursor
are preserved. A cursorless open read starts at the history start.

For a listed room with no row, or a bare row with no join, cursors or open read,
the same inventory refresh gets the identity's membership once. Its creation
time supplies the join bound, no earlier than the last successful inventory
or the installation floor (`time_source history_floor`).
An absent/invalid time, a state other than JOINED, or HTTP 403/404 uses that
recorded inventory time; without one, it uses the time found. The same first-read
margin applies. Temporary failures fail the refresh without saving a new room;
the existing retry handles them. Newly discovered rooms never start at 1970.

- Bound a newly listed room's first read by the installation floor while allowing its adding message within the join margin.
~~~~

### Verification

Go 1.27.3 on darwin/arm64, Node v24.21.0 and Python 3.13.13 with pytest 9.2.0 through the Makefile's uv fallback. The shell Python is 3.15.0. The commands ran serially with fake services. Go test counts below use pass/fail/skip order for each of packages, top-level tests and subtests. The package skip category includes packages with no test files.

| Command or set | Result |
| --- | --- |
| `gofmt -l .` and `env -u GC_BIN -u NEVER_SILENT go vet ./...` | Both exited 0, empty output. |
| `git diff --check` and marker search in gchat | Clean diff, zero conflict markers. |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 -json ./internal/catchup` | package 1/0/0, test 69/0/0, subtest 136/0/0 |
| `env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json` | package 25/0/1, test 827/0/2, subtest 1722/0/0 |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/inbound` (inbound) | exit 0, 1 package(s) passed (127 top-level tests and 265 subtests in the corresponding full-suite JSON) |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/app -run '^(TestTheMessageThatAddsTheIdentityIsATurn\|TestJoinedSpaceHistoryIsSweptWithoutMessageEvents\|TestFirstStartWindowReachesBack\|TestNoOldActIsEverATurn\|TestReleasedLedgerCursorlessSpacesStartAtLoad\|TestCatchupCadence)$'` (app-targets) | exit 0, 1 package(s) passed (6 top-level tests and 64 subtests in the corresponding full-suite JSON) |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/catchup -run 'Join\|Reach\|Membership\|FirstRead\|Discovery\|NewSpace\|RetainedWindow'` (main-catchup) | exit 0, 1 package(s) passed (18 top-level tests and 39 subtests in the corresponding full-suite JSON) |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/config -run '^TestJoinReachbackValidation$'` (main-config) | exit 0, 1 package(s) passed (1 top-level tests and 7 subtests in the corresponding full-suite JSON) |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/app -run '^TestTheMessageThatAddsTheIdentityIsATurn$'` (main-app) | exit 0, 1 package(s) passed (1 top-level tests and 0 subtests in the corresponding full-suite JSON) |
| `env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/inbound -run '^TestOffParityWithTheBase(NeverSilentOn)?$'` (base104-parity) | exit 0, 1 package passed, both parity variants at the base |
| `env -u GC_BIN -u NEVER_SILENT go test -race ./internal/app ./internal/catchup ./internal/config ./internal/ledger -p=1 -count=1 -timeout=30m` (race) | exit 0, 4 package(s) passed (215 top-level tests and 386 subtests in the corresponding full-suite JSON) |
| `env -u GC_BIN -u NEVER_SILENT go test -list '.*' ./...` (names-pr) | exit 0, 739 package-qualified names |
| `env -u GC_BIN -u NEVER_SILENT go test -list '.*' ./...` (names-main) | exit 0, 775 package-qualified names |
| `env -u GC_BIN -u NEVER_SILENT go test -list '.*' ./...` (names-merge) | exit 0, 829 package-qualified names |
| `env -u GC_BIN -u NEVER_SILENT make test 'PYTEST_DIRS=tests gchat/tests'` (make-test) | exit 2, # tests 93 · # pass 92 · # fail 0 · # skipped 1 · 1 failed, 585 passed, 128 skipped, 262 subtests passed in 77.93s (0:01:17) |
| `env -u GC_BIN -u NEVER_SILENT -u GC_TEMPLATE -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests gchat/tests -q` (make-clean) | exit 0, 586 passed, 128 skipped, 262 subtests passed in 89.35s (0:01:29) |
| `env -u GC_BIN -u NEVER_SILENT go test ./internal/inbound -run '^TestMessageWithoutActTimeNeverUsesPublishTime$' -count=1 -p=1 -timeout=5m -json` (renamed-timestamp) | exit 0, package 1/0/0, test 1/0/0, subtest 6/0/0 |

The [command manifest](checks.tsv) gives individual invocations and exits, including the 25 commands copied unchanged from round 2. The full suite and the catch-up-only run are also listed.

The 25 unchanged round-2 commands passed 24 top-level tests and 171 subtests. Command 13 selected zero tests because round 4 renamed TestMessageWithoutActTimeUsesPublishTime to TestMessageWithoutActTimeNeverUsesPublishTime. Running the current selector separately passed 1 test and 6 subtests. Together they cover 25 tests and 177 subtests, matching round 4. Its full first run reported 22 passing packages, 736 passing and one failing top-level test, 1,490 passing and seven failing subtests, and two optional live skips. Its clean-environment Python run reported 464 passed, 250 subtests and 127 skipped.

The package-qualified name sets from `go test -list '.*' ./...` contain 739 tests at the prior PR head, 775 at main, and 829 at the merge. The merge contains their entire union. Counting only distinct bare names gives 738, 775, 828 respectively. The only added test is TestFirstReadNeverReachesBelowTheInstallationFloor.

### Red and green checks

Each red run applies a Go overlay to the verified merge. [Overlay changes](overlays.diff) shows just the removed or reverted lines. The selected tests pass at the merge in the full run.

| Overlay | Exit | Failing top-level tests |
| --- | --- | --- |
| pr-last | 1 | TestOlderHistoryOfAJoinedRoomIsNeverATurn, TestFirstReadNeverReachesBelowTheInstallationFloor, TestTheReachBackHappensOnce, TestRoomFoundWithoutAnEventNeverReadsFromTheEpoch, TestFirstReadLogsCountConcludedPendingTurn, TestFirstReadCountSurvivesSaveFailureAfterDelivery, TestFirstReadAcknowledgementBoundary |
| main-last | 1 | TestFloorBoundsEveryNewSpace, TestFirstReadNeverReachesBelowTheInstallationFloor |
| app-fixture | 1 | TestTheMessageThatAddsTheIdentityIsATurn |
| listed | 1 | TestCursorlessSpaceStartsWhereItWasFirstSeen, TestCursorlessMigrationRetiresDetachedAttempts, TestFloorBoundsEveryNewSpace, TestRunningSweepCountsRetiredDeliveries, TestFirstStartWindowReachesBack |
| retire | 1 | TestCursorlessMigrationRetiresDetachedAttempts, TestRunningSweepCountsRetiredDeliveries |
| skip | 1 | TestOlderHistoryOfAJoinedRoomIsNeverATurn, TestFirstReadNeverReachesBelowTheInstallationFloor, TestTheReachBackHappensOnce, TestRoomFoundWithoutAnEventNeverReadsFromTheEpoch, TestFirstReadLogsCountConcludedPendingTurn, TestFirstReadCountSurvivesSaveFailureAfterDelivery, TestFirstReadAcknowledgementBoundary, TestTheMessageThatAddsTheIdentityIsATurn |
| recorded-only | 1 | TestFirstReadAcknowledgementBoundary |
| default-wake | 1 | TestNewSpaceUsesDefaultWake |
| no-list | 1 | TestMembershipReadThatMayPassFailsTheFreeze |
| join-floor | 1 | TestFloorBoundsEveryNewSpace |
| open | 1 | TestLegacyOpenReadKeepsItsBounds |
| parity | 1 | TestOffParityWithTheBase, TestOffParityWithTheBaseNeverSilentOn |

The new pair test delivered zero turns with the PR's join clamp last and queued a message from before the installation floor with main's unclamped reach-back. Removing the inbound exception also delivered zero turns. At the merge it delivered exactly the newer mention. Restricting admission to messages already recorded by the first read gave zero turns across the live copy and the first read. Removing retirement delivered a detached attempt and left the missing-time counter at zero.

The full Go suite ran once. All 25 packages with tests passed. The arrival package has no test files. The two optional live-GC contract tests skipped. The 3,000-message end-to-end recovery drained its backlog. None of the full-run cases needed isolated load comparisons.

The strict Make invocation passed reachable's Node suite (92 passed, one skip). Its Python invocation returned one failure in the nested Make discovery check because the command-line directory override reached the child Make process. The documented clean-environment invocation passed 586 Python tests and 262 subtests with 128 skips.

### Self-review

Reviewed the merge's remerge diff, the diff3 regions, the nine automatically merged files, the twelve-path accounting and the PR-body addition. Independent settings, application fields and ledger fields survive the union. The first-read, retirement and delivery changes correspond to the rulings above. The application retains notice-state ordering alongside the arrival service, and the status handler retains both the acknowledgement-key status and the history floor. The docs and the test expectations agree with the decode, membership and delivery paths. The complete diff against main passes `git diff --check`.

The DEEP self-review found 0 blockers, 0 majors, 0 minors and 0 nits in the resolution. The whole-PR gate and second-model read are separate handoff steps.
