# Round 4: the join rechecks its identity

Bead: gp-bmx4. Same draft PR 111 and remote branch gp-8ski.
All adapter requests in this round use test fakes. Nothing is deployed.
Real Google invitation acceptance remains UNPROVEN.

- Base (round 3's head): `a5800f565dc1a7e0223a9109ffeec5c922fed5b2`.
- Test alone, red: `af6794ae636dc1996ffd1be17a1ce52dd6c41270`.
- Fix: `f6d53f845e2035151d28182a47ea1e764e181092`.
- Self-review fix to the test: `b10092eff709248fcb5c67f825d4008ad5d64a83`.
  This is the code head every suite below ran at. The commit that adds this
  directory changes evidence only.
- No main merge, rebase, force push, new PR, ready flip or deployment.

## The finding

Gate r3 found that `runJoin` read `c.JoinIdentity()` once, then waited on the
owner's-yes check, the join-scope mint and the prepare before it spent the
yes and sent its one membership create. If the client's identity turned
conflicted meanwhile, the join still spent the yes and sent a POST naming
the old id. `DeleteReaction` already rechecks before its write
(`gchat/adapter/internal/chat/chat.go:420-423`); the join did not.

## The two rechecks

Paths are relative to `gchat/adapter/internal/server/` at the code head.
One closure, `stillMine` (`join.go:198`), asks the same client again and
requires the same id the join read at `join.go:192`.

| Place | Where | Yes spent | Write | Answer |
| --- | --- | --- | --- | --- |
| (a) Inside the consumption update, before `p.Used = true` | `join.go:257-259`; the sentinel `errJoinIdentityChanged` (`join.go:168`) is told apart with `errors.Is` at `join.go:262` | No: `update` saves nothing when its closure errs | None | `no_owner_yes`: "The identity is not known safely yet. Nothing changed; try again after the adapter learns its identity." |
| (b) Immediately before the one create | `join.go:267-269`, before `create()` at `join.go:270` | Yes, by design | None | `no_owner_yes`: "The identity is not known safely yet. Nothing was sent to Google, but this owner's yes is now used; request approval again after the adapter learns its identity." |

The answer at (a) is the existing unknown-identity refusal, word for word.
It now comes from one `const` shared with `join.go:194`. Every other
closure error keeps its existing words at `join.go:265`. The words at (b)
are the mayor's ruling. After (b), a rerun with the same yes is refused with
"This request was already used.", as after any spent yes.

Changed source lines outside tests: `join.go` +18/-2. Of the 18 added, 13 are
code and 5 are comments or blank. The changelog adds one 4-line bullet
after this PR's join bullet. No other source file changed. No existing test
was edited.

## The table test

`TestJoinRechecksIdentityBeforeConsumingAndWriting`
(`join_edges_test.go:340`). Each row requests approval, posts the owner's
yes, holds one step, waits until the join is inside it, makes the identity
conflicted with `SetIdentityUserID("users/999")`, asserts that
`JoinIdentity()` now refuses, releases the step and reads the answer.
Every wait is bounded at 10 s.

| Row | What is held | After the fix |
| --- | --- | --- |
| `owner_yes_check` | The GET of the owner's message, in a test client's transport | no write; `no_owner_yes`, (a)'s words; Used=false |
| `mint` | The join-scope mint, in a second key holder | no write; `no_owner_yes`, (a)'s words; Used=false |
| `prepare` | The mint, then the ledger lock: the test takes it, releases the mint, and waits until the join's goroutine is parked in the consumption update's `lockLedger` before it injects | no write; `no_owner_yes`, (a)'s words; Used=false |
| `consumption_save` | The consumption update's ledger save, by a save guard that holds once | no write; `no_owner_yes`, (b)'s words; Used=true |

Red against the base code, every row answers `joined` with one POST naming
`users/123456789`. In the first three rows the yes is also spent. See
[red.txt](red.txt) (the test-only commit, and the final test run against
the base `join.go`) and [green.txt](green.txt).

### What each row proves

Each row was run against variants of `join.go` swapped in with
`go test -overlay`. The worktree was not edited.

| `join.go` variant | owner_yes_check | mint | prepare | consumption_save |
| --- | --- | --- | --- | --- |
| Base (a5800f56) | FAIL | FAIL | FAIL | FAIL |
| Head without (a) | FAIL | FAIL | FAIL | pass |
| Head without (b) | pass | pass | pass | FAIL |
| (a) moved out of the closure, just before `update` | pass | pass | FAIL | pass |
| Head | pass | pass | pass | pass |

The last variant is the self-review's MINOR (below). Before `b10092ef`, it
passed all four rows in three of three runs. Since then, the prepare row
fails it in three of three runs, while the head passes in three of three.

## Verification

Go: `go version go1.27.3 darwin/arm64`. Every run unsets GC_BIN and
NEVER_SILENT. Suites ran one at a time on a loaded box. Raw logs stay
outside the public branch.

From `gchat/adapter`, `gofmt -l .` exits 0 with empty output, and
`env -u GC_BIN -u NEVER_SILENT go vet ./...` exits 0.

| Run | Passing top-level tests | Passing subtests | Skips | Packages passing | Exit |
| --- | ---: | ---: | ---: | ---: | ---: |
| Full | 735 | 1,318 | 2 | 22 | 0 |
| Join suite | 24 | 73 | 0 | 1 | 0 |
| New test by name | 1 | 4 | 0 | 1 | 0 |
| Chat identity tests | 3 | 0 | 0 | 1 | 0 |
| Race (server, chat) | 151 | 229 | 0 | 2 | 0 |

Against round 3, the full run adds one top-level test and four subtests
(736 → 737 with the two optional live-gc skips, `TestRealGCContract` and
`TestRealGCReplyReminder`; 1,314 → 1,318). The join suite goes from 23
tests and 69 subtests to 24 and 73. No case failed in any run, so the
load-comparison fallback was not needed. The full run took 1,148 s, the
race run 97 s, with no race reports.

Exact invocations, in table order, all from `gchat/adapter`:

```sh
env -u GC_BIN -u NEVER_SILENT go test ./... -p=1 -count=1 -timeout=30m -json
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^TestJoin|^TestMissingPermissionChangesNothing$' -count=1 -v
env -u GC_BIN -u NEVER_SILENT go test ./internal/server -run '^TestJoinRechecksIdentityBeforeConsumingAndWriting$' -count=1 -v
env -u GC_BIN -u NEVER_SILENT go test ./internal/chat -run '^TestObservedIdentitySnapshotAndConflict$|^TestDeleteReactionRechecksIdentityAfterPacing$|^TestIdentityRejectsAliases$' -count=1 -v
env -u GC_BIN -u NEVER_SILENT go test -race ./internal/server ./internal/chat -p=1 -count=1 -timeout=30m -json
```

The variant runs used the new test's invocation with
`-overlay <tmp>/VARIANT.json`. Each file replaces
`internal/server/join.go` with one variant.

`make test` was not used. On main it is broken by another pack's timing
test, which is being fixed separately: `test` depends on `test-reachable`, whose p95 assertion
fails on this box before Python starts (round 3's README records it). The
gchat Python tests ran directly, in round 3's form. Using Python 3.13.13,
they pass **52 tests and 100 subtests**, with no failures or skips, exit 0:

```sh
env -u GC_BIN -u NEVER_SILENT -u GC_TEMPLATE -u MAKEFLAGS -u MFLAGS -u MAKEOVERRIDES -u PYTEST_DIRS -u PYTEST_ARGS -u PYTEST_RUNNER uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gchat/tests -q
```

## Self-review

The self-review was one native STANDARD pass, by a fresh Opus context, over
`git diff a5800f565dc1a7e0223a9109ffeec5c922fed5b2..f6d53f84`, the four
rows and the CHANGELOG. It found **0 BLOCKER, 0 MAJOR, 1 MINOR, 1 NIT.**

- **MINOR: FIXED in `b10092ef`.** The `prepare` row injected the conflict
  right after it released the mint. The mint's answer was then usually
  still in flight, so the row repeated the mint row and could not tell (a)
  inside the consumption update from a check placed just before it.
  - Reproduced: that variant passed all four rows in three of three runs.
  - The fix: the row now waits, with the ledger lock held, until the join's
    goroutine is parked in `update`'s `lockLedger` under `runJoin`, and
    only then injects the conflict. The variant now fails that row in three
    of three runs.
- **NIT: DOES NOT MATTER.** If the approval changes and the identity
  conflicts at the same moment, the approval check inside the closure runs
  first, so the answer is the existing approval-expired refusal. Neither
  answer spends the yes or writes, and the ruling places (a) only before
  `p.Used = true`.

The reviewer confirmed the lock order is safe. Recheck (a) takes the
client's `identityMu` read lock under the ledger lock. No path takes them
the other way round: the identity setters hold only `identityMu`, and the
app's identity persistence releases it before taking its own lock. The
race run agrees.

The mayor's codex gate on `a5800f56..HEAD` and the second-model read are
still pending. This receipt claims neither verdict. No codex round was run
in this lane.

## Public repository

public-repo check: done, 1 thing changed

The one change: a draft of this README named another city ticket for the
broken `make test`. That id was removed before the commit; the README now
says only that another pack's timing test is being fixed separately.

The diff, the commit messages, this directory and the PR text were scanned
for names, real ids, contact details, tokens and local paths. The test uses
only the fixtures' invented values: `users/999`, `users/123456789` (via
`fakegoogle.UserID`), `agent@example.com` and the fake key holder's token
string. Its socket directory is `/tmp`, as in the existing fixture. Logs
carry no home or temporary-folder paths.

## Contract items

| Item | State | Proof |
| --- | --- | --- |
| 1. The recheck | DONE | (a) `join.go:257-259` with sentinel and `errors.Is` at `:262`; (b) `join.go:267-269` before `create()` at `:270`; one `stillMine` closure at `:198`; words as ruled |
| 2. The class test, red first | DONE | Test-only commit `af6794ae` red in all four rows; fix `f6d53f84` green; the self-review's tightening `b10092ef` is red against the base `join.go` and green at the head; each recheck pinned by its own rows (variant table) |
| 3. Nothing else changes | DONE | `join.go` +18/-2, one changelog bullet; no other source, fake, scope, owner's-yes or answer-word change; no existing test edited; full run passes |
| 4. Public repository | DONE | Scan above; 1 thing changed (a city ticket id removed from the README draft) |
| 5. PR body | DONE | Round 4 section added, rest kept, trailer names gp-bmx4 and round 4 pending the mayor's gate |
