Prepare the build-basic starter factory review.

Gather the requirements artifact, implementation plan, decomposition artifact,
implementation summary, changed-file summaries, task evidence, and verification
commands into one review context file under the build artifact root. Record that
path on the workflow root as `gc.build.code_review_context_path`.

The implementation source of truth is the closed source anchor/worktree recorded
by the implementation summary and task evidence. Include the source anchor id,
its `work_dir`, changed files, commit id, and proof commands in the context. The
launcher rig root may remain unchanged until an explicit publish step; do not
present an unchanged root checkout as a review failure when the source
anchor/worktree contains the verified implementation.

When the source anchor has no `work_dir` and records `gc.work_branch`
(`prepare-worktree` ran in a lane and handed the item over by branch; a
directory is per agent and is never handed to another agent), record the
BRANCH (`gc.work_branch`) and the COMMIT id (`git -C "$GC_DIR" rev-parse
"<gc.work_branch>"`, read from your own lane: every worktree of the rig shares
its refs) in the context in place of a directory; the review and fix lanes
resolve those in their own lanes. Never enter another agent's lane, and never
record one as the workspace: a persisted `work_dir` that names an agent lane
(`.worktrees/<rig>/lane-*`) is invalid, and so is a recorded branch that is
missing from the repository; close this setup bead with `gc.outcome=fail` and
say which, instead of pointing the review at a directory it must not enter.

This starter factory intentionally uses only three review lanes so new users can
see fanout/fanin without a large reviewer roster.

Do not invoke provider-native subagents. Gas City graph lanes are the
delegation mechanism.

Close this setup bead with `gc.outcome=pass` only after the review context path
is recorded.
