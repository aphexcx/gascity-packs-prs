# Round 3: brought current with main

Bead: gp-d7jm. Same draft PR 111 and remote branch gp-8ski.
All adapter requests in this round use test fakes. Nothing is deployed.
Real Google invitation acceptance remains UNPROVEN.

- Reviewed PR head: `3d504990d47bbc00743d42cf1611523f80f0f779`.
- Main tip fetched and merged: `226a0f34049fc5148a531530a063cdc063348235`.
- Merge base: `68884bf15089e79230bc88c55b591033e3fe927f`.
- Merge commit: `0ba341334adf667a1b199054623438c3ce02ed86`.
- Merge parents are the reviewed PR head, then the recorded main tip.
- Two conflicted files, one conflict hunk each; five other files changed on
  both sides and merged automatically. No additional overlap was found.
- No rebase, force push, new PR, ready flip, merge of the PR, or deployment.

## Resolution

Paths in the tables are relative to `gchat/`.

| File / hunk | Main wanted | PR wanted | Resolution and reason |
| --- | --- | --- | --- |
| `CHANGELOG.md`, Unreleased | First-read catch-up entry | Invitation-acceptance entry | Keep every line verbatim, join first, then main's entry and the rest of main. Against the merged main tip, only the eight-line join bullet is added. |
| `adapter/internal/fakes/fakegoogle/fakegoogle.go`, constructor | Memberships and per-path error maps | Memberships map for join reads and writes | Initialize both maps once. Remove the duplicate Memberships declaration and unreachable membership GET branch from the automatic merge; use one handler. |

The non-test source resolution is 19 changed lines against git's trial
merge, excluding conflict markers: 14 removed, five added. The five additions
move main's GET request recording and per-path error handling into
`membership.go`; the per-path error takes precedence over the existing global
read error. The existing `no invitation` 404 detail remains. No test asserts
either side's competing 404 detail. No other production behavior changes.

## Where both sides meet

| File | Main wanted | PR wanted | Merged behavior and regression proof |
| --- | --- | --- | --- |
| `adapter/internal/fakes/fakegoogle/fakegoogle.go` (conflicted) | GET records MembershipRequests; per-path error with UNKNOWN; stored membership or 404; retain HistoryRequests | Space GET; one membership handler; MembershipCalls for all requests; global read error; self-create invitation transitions | One Memberships map and one route. `TestMembershipCatchupAndJoinShareModel` verifies error precedence, both logs, and INVITED → POST → JOINED. `TestMembershipModel`, the server join suite and catch-up membership tests exercise both consumers. |
| `adapter/internal/chat/chat.go` (automatic) | GetMembership with a 30-second context bound and generated API error mapping | Preserve httpClient and the separate ReadOwnMembership/PrepareJoinSelf API | Both readers remain unchanged and distinct. The named catch-up suite and server join suite exercise each; race covers chat and both consumers. |
| `adapter/internal/ledger/ledger.go` (automatic) | LastSpaceList and FirstRead, including deep clone of PreJoinMessages | JoinRequests and JoinAttempts, including clone copies | All four survive save/load and clone. `TestJoinAndFirstReadLedgerRoundTripClone` checks whole-ledger equality and proves that mutations to all four on a clone do not reach the loaded original. Existing join compatibility and catch-up tests remain. |
| `adapter/internal/app/app.go` (automatic) | identityDiscoveryHeld; legacy identity hold is catching-up, with publish open | LedgerReady uses loaded.Load, preventing pre-load saves | Both remain. During the hold the ledger is loaded, so /join passes its state gate rather than returning state_unavailable. Its existing permission/source/identity checks still apply: desktop credentials refuse join; an unknown managed identity yields no_owner_yes for a room join, while --check can check permission. `TestLegacyIdentityDiscoveryKeepsPublishingAvailable`, `TestLegacyIdentityHoldResumesInterruptedOutbound`, `TestJoinBeforeLedgerLoadPreservesExistingState`, and the named join suite cover these constituent contracts. No new startup behavior is claimed. |
| `adapter/internal/app/keyholder_test.go` (automatic) | Identity-bearing fixtures, thread-safe notice capture, adding-mention turn and joined-room recovery tests | Fake key holder records requested scopes and can refuse join permission | Both sets of fixtures and assertions remain; no test body edited in this round. Full app suite plus named `TestTheMessageThatAddsTheIdentityIsATurn` pass; join scope/refusal tests use the same fake. |
| `README.md` (automatic) | First-read bound, join_reachback setting, inventory fallback and discovery/read logs | Join setup, one-code approval, separate optional permission and unchanged startup | Read all 664 merged lines in order. The join/setup text and catch-up text coexist without a new contradiction. Named join and catch-up/config runs verify their behavior; changelog and document diffs preserve both additions. No prose correction needed. |

No existing test body or fixture was edited by this resolution. Only the two
new tests and their imports were appended to the existing membership and
ledger test files. Main's own fixture and notice synchronization changes are
retained as merged history.

## Verification

Go: `go version go1.27.3 darwin/arm64`. The code tested is the merge commit
above; subsequent commits contain evidence only. Every test run unsets
GC_BIN and NEVER_SILENT. Suites run sequentially. Raw logs and the completion
manifest remain outside the public branch; the manifest was read before
recording these results.

From `gchat/adapter`, `gofmt -l .` exits 0 with empty output, and
`env -u GC_BIN -u NEVER_SILENT go vet ./...` exits 0.

| Run | Passing top-level tests | Passing subtests | Skips | Packages passing | Exit |
| --- | ---: | ---: | ---: | ---: | ---: |
| Full | 734 | 1,314 | 2 | 22 | 0 |
| Named server join | 23 | 69 | 0 | 1 | 0 |
| Existing membership model | 1 | 0 | 0 | 1 | 0 |
| Ledger Join | 2 | 0 | 0 | 1 | 0 |
| Named catch-up | 14 | 17 | 0 | 1 | 0 |
| Reach-back config | 1 | 7 | 0 | 1 | 0 |
| Adding message becomes a turn | 1 | 0 | 0 | 1 | 0 |
| New membership pair | 1 | 0 | 0 | 1 | 0 |
| New ledger pair | 1 | 0 | 0 | 1 | 0 |
| Race (both sides' packages) | 238 | 355 | 0 | 4 | 0 |

Exact invocations, in table order, all from `gchat/adapter`:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^TestJoin|^TestMissingPermissionChangesNothing$' -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/fakes/fakegoogle -run '^TestMembershipModel$' -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/ledger -run Join -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test -p 1 -count=1 ./internal/catchup -run 'Join|Reach|Membership|FirstRead|Discovery|RoomFound|KnownRoom|OlderHistory|LegacyOpenRead' -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/config -run '^TestJoinReachbackValidation$' -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/app -run '^TestTheMessageThatAddsTheIdentityIsATurn$' -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/fakes/fakegoogle -run '^TestMembershipCatchupAndJoinShareModel$' -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/ledger -run '^TestJoinAndFirstReadLedgerRoundTripClone$' -count=1 -json
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/server ./internal/chat ./internal/catchup ./internal/fakes/fakegoogle -p=1 -count=1 -timeout=30m -json
```

The full run contains 736 top-level tests, including the two optional live-gc
skips, `TestRealGCContract` and `TestRealGCReplyReminder`. No case failed;
therefore the load-comparison fallback was not needed. The full run took
1,207.51 seconds, including the durable 3,000-message recovery scenario.

[red.txt](red.txt) and [green.txt](green.txt) record the two new tests against
the unresolved trial merge and the resolution. The fake could not compile
at the trial merge; the ledger's automatic merge already passed its new test.
Both pass at the resolution, together and individually by name.

The test-name check used `git archive` for both historical trees into `<tmp>`;
neither was checked out in the lane. From each tree's `gchat/adapter`:

```sh
env -u GC_BIN -u NEVER_SILENT go test -list '.*' ./...
```

| Tree | Listed tests (package/name pairs) | Exit |
| --- | ---: | ---: |
| Reviewed PR head | 715 | 0 |
| Main tip merged | 705 | 0 |
| Resolved merge | 736 | 0 |

Missing from the union: **empty**. The only names added beyond both parents
are the two new regression tests above. Counts include the optional live-gc
tests even though the full test run skips their execution.

The changelog check against the recorded main tip has eight added lines and
zero removed lines. `git diff --check` passes. The conflict-marker scan
`rg -n '^(<<<<<<<|=======|>>>>>>>)' gchat` returns 1 with no matches.
All five automatically merged shared files match git's trial-merge tree
byte for byte.

The race run took 167.29 seconds, with no race reports.

From the repository root, the prescribed scoped make command was attempted:

```sh
env -u GC_BIN -u NEVER_SILENT make test PYTEST_DIRS=gchat/tests
```

Main now makes `test` depend unconditionally on `test-reachable`, so Python
does not run if that Node prerequisite fails. Two attempts failed before
Python started:

| Node used by make | Reachable results | Failure | Make exit |
| --- | --- | --- | ---: |
| Shell Node 26.12.1 | 90 passed, one failed, zero skipped | 256 MiB fixture CLI p95 118.58 ms exceeds 100 ms | 2 |
| City Node 24.21.0, toolchain first on PATH | 89 passed, two failed, zero skipped | Same p95 assertion at 108.21 ms; archive replay also reports EAGAIN on write | 2 |

`pnpm exec node --version` confirms the city Node is v24.21.0. Neither log
contains an unsupported-engine warning. `Makefile` and all `reachable/` files
are byte-identical to the main tip merged. These are outside the resolution;
no reachable change or further suite run was made. The gchat full Go run
had no failed case, so its alternating merge/main load-failure protocol was
not triggered. The root make result is explicitly not a clean verification.

Using Python 3.13.13 and the prior round's clean-environment runner, restricted to the
requested gchat directory, the wrappers pass **52 tests and 100 subtests**,
zero failures or skips, exit 0:

```sh
env -u GC_BIN -u NEVER_SILENT -u GC_TEMPLATE -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTEST_ARGS -u PYTEST_RUNNER uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gchat/tests -q
```

This direct run replaces only the Python execution blocked by the inherited
make prerequisite. The previous round's nested-make failure was not reached.

## Review and public-repository check

Native self-review, class STANDARD, covers `git show --remerge-diff` on the
merge, the evidence-only follow-up, the six shared-file rows, and the updated
PR body. **CLEAN in the resolution: zero BLOCKER, MAJOR, MINOR or NIT
findings.** The membership error precedence, both request logs, map
initialization, serialized fake access, ledger clone isolation, unchanged
client readers and startup readiness were traced against the tests and
receipts. No correction commit was needed. The inherited make prerequisite
failures above remain disclosed validation limits, not silently counted as
passing checks or repaired outside scope.

No separate Codex subprocess review was run in this round. Round 3 still
awaits the mayor's whole-PR gate and the second-model merge read; this receipt
does not claim either verdict. Earlier review findings remain at their
recorded dispositions.

public-repo check: done, 0 things changed

No redactions were needed in the authored source, fixtures, evidence or PR
body. Added content contains no private names, contact details, local home
paths, credentials, conversation coordinates or unrelated bead IDs. Raw test
logs stay outside the public branch.

## Contract items

| Item | State | Proof |
| --- | --- | --- |
| 1. Preserve the reviewed PR | DONE | All 715 names retained; unchanged existing test bodies; named join/fake/ledger and full suite pass. |
| 2. Preserve main | DONE | All 705 names retained; named catch-up/config/adding-message tests and full suite pass. |
| 3. Test the meeting | DONE | Six rows above; two new regression tests; distinct client readers and startup gate preserved; full README read. |
| 4. No other behavior | DONE | 19 non-test source resolution lines; only fake route/model reconciliation; no existing test edits or out-of-scope fixes. |
| 5. Merge history and changelog | DONE | Two-parent no-ff merge at the recorded start tip; two conflicted and five automatic shared files; eight-line changelog addition only; no markers. |
| 6. Public repository | DONE | Authored additions and PR body checked; zero redactions required. |
| 7. Name check | DONE | 715 / 705 / 736; missing union empty. |
| 8. PR body | DONE | Existing body retained; round-3 section adds the tip, resolutions and suite counts; trailer names gp-d7jm and marks round-3 gate/read pending. Remote head, draft state and mergeability are read back at handoff. |

Remaining: the mayor's gate, second-model read and PR merge. The root make
prerequisite failures are reported for disposition; no deployed service or
state was touched.
