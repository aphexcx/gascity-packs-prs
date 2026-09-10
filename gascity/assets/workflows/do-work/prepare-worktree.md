
Resolve and publish the isolated worktree for this item. This is infrastructure
setup only. Do not edit source files in the launcher checkout.

1. Read current step bead metadata and get `gc.root_bead_id`; hard-fail if it is
   missing. Read that do-work root with `gc bd show <root-bead-id> --json`.
2. Resolve `<source-anchor-id>` from the do-work root:
   - read root metadata `gc.input_convoy_id`; hard-fail if it is missing
   - verify `gc.input_convoy_id` matches rendered runtime convoy `{{convoy_id}}`
   - read that input convoy with `gc bd show <input-convoy-id> --json`
   - if input convoy metadata has `gc.synthetic_kind=drain-unit-convoy`, use
     input convoy metadata `gc.drain_member_id`
   - do not use the synthetic drain-unit convoy id as `<source-anchor-id>`;
     hard-fail if the selected source anchor id equals the synthetic input convoy id
   - otherwise use `<input-convoy-id>` as the source anchor
   - if root metadata also has `gc.drain_member_id`, it must match the selected
     drain member
3. Validate context path {{context_path}}, files ownership, and verification
   policy for the resolved source anchor.
4. Check the workspace gc gave this session before creating anything. When
   `$GC_DIR` is already a git worktree of the rig on a branch (the agent's
   lane, prepared by its `pre_start`: `git -C "$GC_DIR" rev-parse
   --is-inside-work-tree` prints `true` and `git -C "$GC_DIR" branch
   --show-current` prints a branch name, or the `pre_start` log says so, and
   `$GC_DIR` is not the rig root), this MAY be the lane case. Prove it with
   the boundary test below before recording a branch or detaching anything:
   those two probes also succeed from any SUBDIRECTORY of a checkout, and a
   subdirectory of a human checkout must never be detached. In the lane case
   this step creates nothing and hands no directory to anyone: a lane is per
   agent (this one is the run operator's), and the item's BRANCH is the
   handoff. Every worktree of the rig shares its refs, so a branch made
   visible here is reachable from the implementation worker's own lane.
   - Boundary test. Resolve every path through symlinks before comparing
     (`cd <path> && pwd -P`, or `realpath`; on macOS `/tmp` resolves to
     `/private/tmp`). ALL three must hold:
     1. The canonical git top-level of `$GC_DIR` IS `$GC_DIR`: `git -C
        "$GC_DIR" rev-parse --show-toplevel`, resolved, equals `$GC_DIR`,
        resolved. A subdirectory of any checkout fails this.
     2. That top-level is NOT the rig root and is NOT inside the rig root
        (the launcher checkout: `gc.work_dir` on the workflow root bead read
        in step 1, resolved). Equality fails, and so does a prefix match of
        the resolved rig root path plus a path separator.
     3. `$GC_DIR` belongs to the rig's repository: `git -C "$GC_DIR"
        rev-parse --git-common-dir`, resolved, is the same directory as the
        rig root's `.git` (`git -C <rig root> rev-parse --git-common-dir`,
        resolved). A worktree of another repository fails this.
     When any part fails, this is NOT the lane case: record no branch, run
     no `switch` in `$GC_DIR`, detach nothing (a `switch --detach` in a
     subdirectory of the rig checkout would detach the human checkout's
     HEAD), and continue with step 5 exactly as before (the per-item
     worktree under `$(pwd)/worktrees/<source-anchor-id>`, then step 6
     persists `work_dir`).
   - Resolve the item's branch: `BRANCH="$(git -C "$GC_DIR" branch
     --show-current)"` is the branch `pre_start` put this lane on. If it
     prints nothing (the lane is detached), use `BRANCH=<source-anchor-id>`
     and create it from HEAD with `git -C "$GC_DIR" branch "$BRANCH" HEAD`
     (reuse the branch when it already exists in the repository).
   - Record the branch on the source anchor with
     `gc bd update <source-anchor-id> --set-metadata gc.work_branch=<branch>`.
     This overwrites a claim-time stamp (older `gc` builds stamp the rig
     root's branch). For synthetic drain-unit convoys, stamp the original
     drain member/source anchor, never the synthetic drain-unit convoy.
   - Detach this lane from the branch with `git -C "$GC_DIR" switch --detach`.
     git allows one worktree per branch, so detaching frees the branch for
     the next role's lane; HEAD stays at the same commit and untracked files
     (staged skills, hooks) stay in place.
   - Verify before closing this step with `gc.outcome=pass`: the source
     anchor's `gc.work_branch` equals `$BRANCH`, `git -C "$GC_DIR" rev-parse
     --verify "refs/heads/$BRANCH"` succeeds, and `git -C "$GC_DIR" branch
     --show-current` prints nothing.
   Do NOT persist `work_dir` in the lane case: step 6 is skipped. A directory
   is per agent and is never handed to another agent; the next role works in
   its own lane on the recorded branch. The source anchor then has no
   `work_dir` and records `gc.work_branch`, and every later step that reads
   `work_dir` (implement, close-source-anchor, the review setup and its review
   and fix lanes) resolves that case in its OWN lane: it switches its lane onto
   the branch with `git switch --no-overwrite-ignore "<gc.work_branch>"` to
   work, or reads the branch's commit with `git log -1` / `git show` to
   inspect, and never enters another agent's lane.
   Otherwise (the session started in the rig root; the role has no lane),
   continue with step 5.
5. Create or reuse a deterministic git worktree at
   `$(pwd)/worktrees/<source-anchor-id>`. If the path is missing, run
   `git worktree add "$WORKTREE" --detach HEAD`. If the path exists but is not
   the worktree for this repository, fail closed.
6. Persist the absolute path on the source anchor with
   `gc bd update <source-anchor-id> --set-metadata work_dir=<absolute worktree path>`.
   For synthetic drain-unit convoys, never persist `work_dir` on the synthetic drain-unit convoy; the original drain member/source anchor is authoritative.
   Verify the source anchor now has `work_dir` before closing this step with
   `gc.outcome=pass`.
