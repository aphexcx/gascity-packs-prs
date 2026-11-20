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
