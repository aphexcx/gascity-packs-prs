# Held-branch restart verification

Base commit `4d6911d880b9d11851843ab5ca7ec1face56bcfc`. The fixtures use a temporary bare remote and linked worktrees. The branch-tip commit in the dirty-restart fixture has never been pushed.

| Restart case | Observed result with the change |
| --- | --- |
| Build rewrites a tracked lock file | The lane checks out the bead branch at the original commit. Its new worktree contains the committed lock file. The detached aside contains the rewritten bytes and untracked notes. The warning identifies the aside and the lock file. |
| Build rewrite plus a separately staged and edited file | The aside preserves the staged version in its index and the subsequent edit on disk. The warning lists both tracked paths. |
| Dirty lane during an unfinished merge | Git refuses to detach. The aside keeps the branch, merge head and staged merge result. The lane starts detached and reports the holder. |
| Another worktree, a different lane's aside, or a same-lane aside from an earlier run holds the branch | The holder keeps the branch. The lane starts detached at its tip. |
| An older detached aside contains a commit absent from the branch | The aside retains its commit and the actual branch holder stays attached. |

The previous `test_dirty_lane_on_the_same_bead_restarts_detached_at_its_tip` now checks that the restarted lane takes the branch. The script compares the exact worktree it moved in this invocation, its symbolic HEAD, and its commit against the local branch ref. A remote-tracking ref cannot establish whether the moved worktree still holds this branch or whether work has been preserved.

## Red and green runs

The verify wrapper clears `GC_TEMPLATE`. Python was 3.12.13 and Node was 24.21.0. The focused command was

```sh
verify python3 -m pytest tests gascity/tests \
  -k 'dirty_lane_on_the_same_bead or dirty_restart_preserves or preexisting_asides or detached_commit_in_an_older_aside or bead_branch_checked_out_elsewhere or held_branch_keeps or restamp_never or relaunched_item_reuses_the_recorded_branch' \
  -q -p no:cacheprovider
```

With only test changes on the base, pytest reported 8 failures, 4 passes, 1,196 deselections and 64 passing subtests. Both dirty-restart cases observed a detached lane. The worker instructions and README failed their open-PR assertions. The foreign and pre-existing holder cases passed. After the implementation and the additional unfinished-merge case, the same selection reported 9 passes, 1,196 deselections and 68 passing subtests.

The first `verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` run reported 1 failure, 1,075 passes, 128 skips and 10,678 passing subtests. The unchanged claim-retry fixture exceeded its two-second subprocess deadline. The isolated command below reported 1 pass and 543 deselections in 0.93 seconds.

```sh
verify -C gascity python3 -m pytest tests -k test_city_claim_command_preserves_unreadable_claim_after_bounded_retries -q -p no:cacheprovider
```

## Worker handoff

Both routes to a held branch use the claimed bead's PR metadata and recorded handoff. Workers leave in-progress work open for a recorded open PR or an inconclusive PR state. The tests also pin the existing failure outcome when the local record establishes that the branch has no open PR.

## Self-review

- A filename prefix also matches older asides. The release check uses the worktree path recorded by the successful move under the repository lock.
- An unfinished merge can prevent detachment. The fallback test checks the merge head and staged file in the aside.
- Checkout hooks can rewrite build files. The detach command disables hooks and submodule recursion for the preserved worktree.
- A missing PR URL can reflect an interrupted handoff. The role leaves that case open for the mayor when the local records cannot establish the state.

## Second round

The comparison starts at `7ca4519b08d079d0e2eb71e98d0ad8a729455628`. The worker-role checks rendered eight prompts with the installed gc in isolated fixture cities. The recovery notice, role and rollout checks failed on that commit. The new hook test passed there. Removing only `-c core.hooksPath=/dev/null` from the detach in a scratch copy made the test fail because the checkout hook wrote into the aside.

| Case | Observation |
| --- | --- |
| Startup files | A same-bead restart preserved bytes and permissions for every listed destination. The fresh lane got independent copies of merged hook JSON, untracked instructions and scripts. A staged addition, a staged deletion with replacement bytes, and a file outside the managed destinations stayed in the aside. |
| Checkout collision | The replacement checkout refused a target that tracked the untracked instructions. The aside retained the original bytes. |
| Copy failure | An injected copy error exposed a partially populated replacement in the first implementation. Preparing the copies before moving the original let a retry recover all files. |
| Recovery notice | The helper names the aside in the current start's log. Each rendered role directs the worker there before writing and accounts for staged, unstaged and untracked work. |
| Held branch | Claim names the open or inconclusive PR handoff as an exception. Continue ends the turn with the claim still assigned for the mayor's ruling. |
| Rollout | The Worker workspaces section and Unreleased entry both require updating the city's helper copy with the role import. |
| Checkout hook | A real post-checkout hook records the null object ID, new HEAD and branch-checkout flag in the fresh lane, with and without startup files. The detached aside has no marker. |

With the final file fixtures against the round-one helper, the startup-file and collision selection recorded 32 failures, 2 passes and 61 deselections, including individual destination subtests. The worker-role, rollout, recovery-notice and checkout-hook selection recorded 11 failures and one pass. The hook row was the pass.

The focused command uses the installed gc for the isolated render fixtures.

```sh
verify env GC_TEST_BIN="$(command -v gc)" python3 -m pytest tests gascity/tests \
  -k 'dirty_lane_on_the_same_bead or dirty_restart or staged_start_file or switch_that_would_overwrite_an_ignored_file or preexisting_asides or detached_commit_in_an_older_aside or bead_branch_checked_out_elsewhere or held_branch_keeps or held_branch_handoff or worker_restart_documents or role_prompts_render_public_worker_fragment or restamp_never or relaunched_item_reuses_the_recorded_branch' \
  -q -p no:cacheprovider
```

`verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` returned 1,086 passes, 128 skips and 10,698 passing subtests. The verify skill's Reachable command returned 218 passes and its one documented archive-replay skip. The runs used Python 3.12.13 and Node 24.21.0.

The verify skill's full Python command across fourteen test directories returned 3,125 passes, 132 skips, two warnings and 10,927 passing subtests at `5200b0a4`.

### Worker review follow-up

The DEEP review of `5200b0a4` returned BLOCK. Its three findings reproduced at that commit in 11 failing checks, with the unseeded checkout-hook control passing.

- An unfinished claim is released by drain acknowledgement. The held-branch handoff now ends the turn with the bead still assigned. The render fixtures check that draining applies after close.
- A seeded replacement uses `checkout-index`, which omits the checkout hook. After a successful replacement checkout the helper runs the hook in the fresh lane with Git's worktree-add arguments. Git versions predating the hook command resolve the executable through `git rev-parse --git-path`.
- A long tracked-edit listing displaced the recovery path from the recorded stderr tail. With 80 staged files the notice now appears in the last 4 KiB.

After these corrections the focused command above returned 34 passes, 1,181 deselections and 99 passing subtests. The seeded-hook check also passed with the Git hook builtin withheld, exercising the executable-hook fallback.

The full Gas City rerun returned 1,087 passes, 128 skips and 10,698 passing subtests. The PR verification table records the full Python run for the handoff commit.

## Third round

The regression selection ran against `f14928569e60bf0ecc994b7229243115e924a5b0` with only the test changes applied. Pytest reported 18 failures, one pass and 20 passing subtests. On the revised helper and role, the same selection returned 14 passes and 25 passing subtests. Both runs deselected 1,202 tests.

| Case | Red at the comparison commit | Green after the change |
| --- | --- | --- |
| Recovery ownership | In four fixtures the helper omitted the aside's ownership and branch. Eight rendered roles allowed recovery without checking ownership. | The same-ticket restart names the original branch after detaching the aside. The cross-ticket restart names the previous ticket's branch. Plain directories and already-detached worktrees report an unknown branch and take the other-ticket path. All eight roles restrict copying to the own-ticket sentence and direct the worker to report the other aside to the mayor. |
| Directory permissions | Five directory-mode subtests failed while all 20 file checks passed. | All 25 subtests passed, including a private configuration directory, implicit parent directories and an empty read-only scripts directory. |
| Held-branch handoff | The fragment check found different wording in Claim and Continue. | Claim quotes the instruction to “end this turn” in Continue. The fragment and all eight rendered roles check it. |

The regression command used the installed gc for the isolated prompt fixtures.

```sh
verify env GC_TEST_BIN="$(command -v gc)" python3 -m pytest tests gascity/tests \
  -k 'dirty_lane_is_moved_aside_never_reset or nonempty_non_worktree_dir_is_moved_aside or dirty_restart_logs_the_recovery_path or detached_lane_reports_another or dirty_restart_copies_effective_start_files or role_prompts_render_public_worker_fragment or held_branch_handoff_is_an_exception' \
  -q -p no:cacheprovider
```
