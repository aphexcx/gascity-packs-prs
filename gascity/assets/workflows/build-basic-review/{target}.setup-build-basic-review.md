Prepare the build-basic starter factory review.

Gather the requirements artifact, implementation plan, decomposition artifact,
implementation summary, changed-file summaries, task evidence, and verification
commands into one review context file under the build artifact root. Record that
path on the workflow root as `gc.build.code_review_context_path`, and record the
commit id the context carries on the workflow root as well:
`gc bd update "<workflow-root-id>" --set-metadata 'gc.build.review_commit=<commit>'`.
This setup runs ONCE, outside the review loop; the review lanes read
`gc.build.review_commit` first and fall back to the context file's commit, and
the fix lane refreshes both the context file and this key after every fix
commit, so each attempt of the loop reviews the CURRENT commit, never the one
this step saw.

The implementation source of truth is the closed source anchor/worktree recorded
by the implementation summary and task evidence. Include the source anchor id,
its `work_dir`, changed files, commit id, and proof commands in the context. The
launcher rig root may remain unchanged until an explicit publish step; do not
present an unchanged root checkout as a review failure when the source
anchor/worktree contains the verified implementation.

The review context must anchor every relative source path to the real
implementation worktree, not the launcher checkout. Read the launcher rig root
from the workflow root bead's `gc.work_dir`; this path is only the factory
launcher root and is not the code under review. Resolve implementation source
anchors from the implementation summary `trace.upstream` entries whose paths are
`beads/<source-anchor-id>`. For each source anchor, run
`gc bd show "<source-anchor-id>" --json`, handle both an object and a one-element
list, and read `metadata.work_dir`. Verify every `work_dir` is an absolute
existing git worktree and is different from the launcher root. If metadata is
missing but `<launcher-root>/worktrees/<source-anchor-id>` exists and is a git
worktree, record that recovered worktree and include a setup warning in the
context. If no implementation worktree can be resolved and the lane case in
the next paragraph does not apply, close this setup bead with
`gc.outcome=fail` and record the missing source-anchor/worktree evidence.

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

The context body must include an `## Implementation Worktrees` section before
the artifact excerpts. For each source anchor include:

- source anchor id
- absolute implementation worktree path, or, in the lane case, the recorded
  branch (`gc.work_branch`) and commit id in place of a path
- launcher root path for contrast
- changed files and proof commands from the item or aggregate implementation
  summary

When writing artifact excerpts, append the actual file contents with commands
such as `cat "$REQUIREMENTS_PATH"` outside any quoted heredoc. Do not write
literal command substitutions such as `$(cat ...)` or `$(date ...)` into the
review context. Before closing this setup bead, verify the generated context
does not contain literal shell substitutions, for example with
`rg -n '\$\((cat|date)' "$CONTEXT_PATH"`; any match is a setup failure to repair
before setting `gc.outcome=pass`.

This starter factory intentionally uses only three review lanes so new users can
see fanout/fanin without a large reviewer roster.

Do not invoke provider-native subagents. Gas City graph lanes are the
delegation mechanism.

Close this setup bead with `gc.outcome=pass` only after the review context path
and `gc.build.review_commit` are recorded.
