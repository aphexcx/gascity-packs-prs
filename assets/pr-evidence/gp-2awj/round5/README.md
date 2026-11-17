# PR 104 round 5: brought current with main

Bead `gp-qvid`. Same draft PR 104 and remote branch `gp-2awj`; no new PR.
PR 111 was confirmed merged before any edit. The lane started at the required
reviewed head.

| Revision | Full SHA |
| --- | --- |
| Reviewed round 4 head | `972ff72c09d458a83acaf98c9cf9bbd6db70b9cb` |
| Main tip fetched at the start | `1a5be02947899847d3d7334f91d12fabf6aff6ed` |
| Round 5 merge commit | `07478a223aed7627df1cbb2a22c41b8fcd3a4894` |
| Tested and reviewed code head | `c28a54791abf320c4553672c03f4079aebf30531` |

This evidence is a separate commit after the code head. The READY and PR record
the final delivery SHA. The merge has the reviewed PR head as its first parent
and the fetched main tip as its second parent; no rebase or force push.

## Merge resolution and the shared files

The measurement and actual merge agree: one conflicted file, two hunks;
seven other files auto-merged. There was no changelog conflict. No source
change follows the merge; the next commit adds tests only.

| File or hunk | Main wanted | PR 104 wanted | Merged result and proof |
| --- | --- | --- | --- |
| `gchat/adapter/internal/app/app.go`, application struct | `identityDiscoveryHeld` for the legacy identity hold | `arrival`, `watch`, `stateDir` | Exact union: 30 branch fields, 28 main fields, 31 merged fields, with every name and type retained; gofmt aligns them. `TestLegacyIdentityHoldResumesInterruptedOutbound` and the round 4 reaction tests pass. |
| `gchat/adapter/internal/app/app.go`, `server.New` | `LedgerReady: a.loaded.Load` | `Stopwatch: a.watch` | Both arguments retained. `TestJoinBeforeLedgerLoadPreservesExistingState`, the round 4 reaction cases, and the full suite pass. |
| `gchat/adapter/internal/inbound/inbound.go` | Membership joins start the bounded first read; identity accessors serve catch-up | Observe acknowledgements only at the live entry point; resolve room kind | Auto-merge preserves both. `TestFirstReadAcknowledgementBoundary` gives exactly one turn, no acknowledgement and no stopwatch inbound line for catch-up alone; with live delivery before the read it gives one turn, one acknowledgement and one inbound line. |
| `gchat/adapter/internal/app/workers.go` | Freeze retry holds health at catching-up while identity is unknown | Wire arrival on each client generation and pass `Ack` to inbound | Both remain. During the hold `observeArrivals` returns at the empty identity, so no acknowledgement is attempted. The named hold, adding-message and round 4 runs pass. |
| `gchat/adapter/internal/chat/chat.go` | `GetMembership` for the inventory bound | Arrival placement bookkeeping, shared reaction pace, and `MessageCreated` | `GetMembership` calls the membership API directly with its existing timeout and never enters reaction pacing. Successful message creation still calls `MessageCreated`. Membership, first-read, round 4 and full-suite tests pass. |
| `gchat/adapter/internal/config/config.go` | Validate and expose `join_reachback` | Read acknowledgement options from the environment | Both load together. `TestArrivalAndJoinReachbackLoadTogether` checks 10 minutes plus eyes and rejects 61 minutes by key; the original environment-precedence and reachback tests remain unchanged. |
| `gchat/adapter/internal/fakes/fakegoogle/fakegoogle.go` | Membership model and recorded history requests | Opt-in repeated-reaction conflicts | Separate paths survive unchanged. `TestMembershipModel`, `TestMembershipCatchupAndJoinShareModel`, and `TestRepeatedReactionConflictsIsOptIn` pass. |
| `gchat/adapter/internal/server/server.go` | `LedgerReady` and POST `/join` | `Stopwatch`, reaction timing, and key-presence status | Both options remain; key status still reads `Config.Acknowledgement`. Named join and key-status tests and the round 4 reaction cases pass. |
| `gchat/README.md` | Join permissions and workflow; first-read rules | Acknowledgement exclusions and stopwatch behavior | Read top to bottom. The arrival section excludes replays, and the catch-up section describes the bounded history read; neither promises an acknowledgement for catch-up. No conflicting passage found; no edit made. |
| Shared copies | Preserve common acknowledgement and stopwatch contracts | Keep both adapters byte-identical | All 11 files under `internal/ack` and `internal/stopwatch` compare equal with `cmp`; the redaction-copy Python test also passes. |

The spec names stopwatch reaction tests in `internal/server/status_test.go`;
that file contains status tests, not a stopwatch reaction test. The actual
round 4 reaction tests are in `internal/app/round4_reaction_counts_test.go`.
The new first-read test explicitly checks persisted stopwatch inbound and
reaction counts. No nonexistent test is claimed as run.

## New tests and deliberate failures

`TestFirstReadAcknowledgementBoundary` drives the real inbound processor,
catch-up runner, arrival wiring, stopwatch writer and local fake servers.
The membership event joins the room, and the adding mention is two seconds
before that join. The catch-up-only case observes one turn, zero reactions,
and zero stopwatch inbound/reaction lines. The live-before-first-read case
observes one turn across both paths, one reaction, and one inbound/reaction line.

`TestArrivalAndJoinReachbackLoadTogether` loads the switch and a 10-minute
reachback together, then checks that 61 minutes is refused with the setting key.

[red.txt](red.txt) records five expected failures: acknowledging catch-up,
losing reachback, ignoring acknowledgement configuration, losing the reachback
limit, and removing duplicate-delivery guards. The last produces two deliveries
and fails the live-before-first-read case. The original code passes after all
overlays; no production fix was needed.

## Test preservation and name union

All 20 PR test files changed since the shared base and all 17 main test files
changed since that base are byte-identical to their respective parents at the
merge. No existing test body or expectation was edited for this round.
The only existing test file changed after the merge is
`internal/config/arrival_test.go`: imports and one new function were added;
`TestArrivalEnvironmentPrecedence` is unchanged.

In each tree, from `gchat/adapter`:
`env -u GC_BIN -u NEVER_SILENT -u ACK_REACTION -u GC_TEMPLATE go test -p 1 -list '.*' ./...`.
Both parents were read from separate `git archive` extractions under `<tmp>`;
neither was checked out in the lane.

| Tree | Names |
| --- | ---: |
| PR head | 722 |
| Main tip | 737 |
| Merged code plus the two new tests | 775 |

The merged set contains the union of the two parents. Missing names: `[]`.
Exactly two names were added: the new tests above.

## Validation

[green.txt](green.txt) records every exact invocation, working directory,
count, exit code and duration. Go counts include subtests.
Go: `go1.27.3 darwin/arm64`. `pnpm exec node --version`: `v24.21.0`.
Python: 3.13.13; pytest 9.2.0, imported from the shared uv environment.

| Run | Result | Exit |
| --- | --- | ---: |
| named inbound parity / acknowledgements / own reaction / both switches | 11 passed | 0 |
| `full-gchat` | 2217 passed; 0 failed; 2 skipped | 0 |
| `new-green-after-overlays` | 4 passed; 0 failed; 0 skipped | 0 |
| `named-app` | 10 passed; 0 failed; 0 skipped | 0 |
| `named-catchup` | 31 passed; 0 failed; 0 skipped | 0 |
| `named-config` | 8 passed; 0 failed; 0 skipped | 0 |
| `named-main-app` | 1 passed; 0 failed; 0 skipped | 0 |
| `named-server` | 89 passed; 0 failed; 0 skipped | 0 |
| `named-shared` | 18 passed; 0 failed; 0 skipped | 0 |
| `named-hold` | 3 passed; 0 failed; 0 skipped | 0 |
| `named-status` | 1 passed; 0 failed; 0 skipped | 0 |
| `named-fake` | 3 passed; 0 failed; 0 skipped | 0 |
| `gofmt` | empty output | 0 |
| `vet` | clean | 0 |
| `names-pr` | 722 names | 0 |
| `names-main` | 737 names | 0 |
| `names-merge` | 775 names | 0 |
| `base-parity` | 5 passed; 0 failed; 0 skipped | 0 |
| `python-root` | 111 passed in 1.85s | 0 |
| `python-gchat` | 52 passed, 101 subtests passed in 20.66s | 0 |
| `python-slack` | 986 passed, 3 skipped, 2 warnings, 10 subtests passed in 50.72s | 0 |
| `pair-final` | 3 passed; 0 failed; 0 skipped | 0 |

The full gchat run passed without a failed case, so no alternating load table
was needed. The two Go skips require an explicitly configured real-gc contract;
`GC_BIN` stayed unset. All channel traffic used local fakes.

The unchanged off-parity file also passes against current main with the
recorded round 2 base hook: five tests/subtests, exit 0. Expectations were
not changed to follow main.

The dispatch comment directed Python suites to run directly because the
repository Makefile's reachable-test prerequisite has a known timing failure.
The Makefile form was therefore not run in this round. The direct commands
retain the required four root files and the whole gchat suite; the whole
slack-full Python suite was also run as directed. Its two warnings come from
the existing `os.fork()` test at `tests/test_slack_company_outbound.py:698`.
No unsupported-engine warning appeared.

`git diff --stat 68884bf15089e79230bc88c55b591033e3fe927f 1a5be02947899847d3d7334f91d12fabf6aff6ed -- slack-full whatsapp`
is empty, exit 0. The Slack and WhatsApp adapter suites were therefore not
required or rerun.

## Rulings 1 to 8

| Item | Status and proof |
| --- | --- |
| 1: PR behavior | DONE: unchanged tests, named runs and full suite pass; parity also passes against current main. |
| 2: main behavior | DONE: join, reachback, membership, discovery, startup-hold and join-route tests pass unchanged. |
| 3: where they meet | DONE: the per-file table, both new tests, five mutation failures and 11 equal shared files. |
| 4: no new behavior | DONE: only the two forced hunks were resolved; later code commit adds tests only. Four substantive added lines in the remerge resolution retain the branch's three fields and combine the server options. |
| 5: merge | DONE: two-parent merge of the start's main tip; one conflicted file, two hunks, seven auto-merged files; no changelog conflict. |
| 6: public repository | DONE: added content uses invented test identifiers and path placeholders. public-repo check: done, 0 things changed. |
| 7: name union | DONE: 722 / 737 / 775; missing set empty. |
| 8: PR body | DONE: the switch-unset section follows current main; round 5 section and review trailers updated, with the mayor's gate pending. |

## Self-review and handoff

One native self-review, class STANDARD, covers
`git show --remerge-diff 07478a223aed7627df1cbb2a22c41b8fcd3a4894`,
the later test commit, the rows above, and the PR body draft.
Counts: BLOCKER 0 / MAJOR 0 / MINOR 0 / NIT 0. No finding requires a fix.
The inaccurate test-file reference in the spec is recorded above as a scope
reference correction; it is not evidence that a nonexistent test passed.

The prior round's findings were not reopened. No worker Codex gate was run.
The mayor's gate on the whole PR delta against main and second-model read
remain pending. PR 104 stays draft; no merge, deployment or live-service
operation was performed. PR 109 remains based on `gp-2awj` and was read as
OPEN, draft and MERGEABLE; nothing on it was changed.
