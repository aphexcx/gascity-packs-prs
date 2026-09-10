Apply build-basic starter review findings.

Use implementation target {{implementation_target}} for any code changes. Read
the starter review synthesis. If all three review lanes approve, write a no-op
review summary. If required fixes or missing evidence remain, make the smallest
focused changes, run the relevant proof commands, and write the review-fix
summary under the build artifact root.

Apply fixes to the implementation source anchor/worktree named in the review
context, not to the launcher rig root. An unchanged root checkout is not itself
a required fix for build-basic; publish owns propagation beyond the source
anchor. If the only reported issue is "implementation exists in the worktree but
not the root checkout" and the source anchor/worktree passes the requirements,
record a no-op fix summary and set `code_review.verdict=done`.

Before editing or running proof commands, read `gc.build.code_review_context_path`
from the workflow root bead and use its `## Implementation Worktrees` section as
the authority for writable code. `gc.work_dir` is the launcher rig root, not the
implementation worktree. Do not inspect or edit the launcher checkout. Select
the implementation worktree for each finding from the source anchor/worktree
recorded in the review context, run `cd "$WORKTREE"`, and verify `pwd -P` equals
that worktree before making changes. Resolve all relative paths in synthesis
findings against the selected worktree. If a required fix cannot be tied to an
implementation worktree, write an iterate summary explaining the missing
worktree evidence and do not patch the launcher root. If multiple worktrees are
listed and a finding is ambiguous, leave it as iterate until the owning worktree
is explicit.

Contract: `gc.work_dir` is the launcher rig root, not the implementation worktree.

When the review context records a branch and a commit instead of a `work_dir`
(in its `## Implementation Worktrees` section: the source anchor has no
`work_dir` and records `gc.work_branch`, the item was handed over by branch,
and a directory is per agent), the workspace for
fixes is your OWN lane, `$GC_DIR`, put on that branch exactly as
`do-work/implement` does for the lane case: prove the lane first with the
boundary test `do-work/prepare-worktree` step 4 applies (resolved `git -C
"$GC_DIR" rev-parse --show-toplevel` equals `$GC_DIR`; neither the rig root
nor inside it; `rev-parse --git-common-dir` is the rig root's `.git`), then,
unless `git -C "$GC_DIR" branch --show-current` already prints that branch,
`git -C "$GC_DIR" switch --no-overwrite-ignore "<gc.work_branch>"`; when any
part fails or git refuses (an ignored file in your lane colliding with a path
the branch tracks), fail this step before editing: never `--force`, never a
stash, never remove the colliding file. Then `WORKTREE="$GC_DIR"`, `cd
"$WORKTREE"`, verify `pwd -P` equals it before changing anything, and commit
the fix on that branch. Never enter another agent's lane: a `work_dir` naming
a lane other than `$GC_DIR` is invalid, fail closed.

Set `code_review.verdict=done` only when acceptance, test evidence, and
simplicity all approve after this pass. Set `code_review.verdict=iterate` when
required fixes remain.

Always close with `gc.outcome=pass`,
`code_review.verdict=done|iterate`,
`code_review.report_path=<starter review summary path>`, and
`code_review.output_path=<starter review summary path>`.

Use the exact claimed bead id when updating metadata. Do not pass freeform notes
or additional positional arguments to `gc bd update`; unquoted words can resolve to
unrelated beads. Use this command shape:

```bash
gc bd update "$CLAIMED_BEAD_ID" \
  --set-metadata 'gc.outcome=pass' \
  --set-metadata 'code_review.verdict=done' \
  --set-metadata 'code_review.report_path=<starter review summary path>' \
  --set-metadata 'code_review.output_path=<starter review summary path>'
gc bd close "$CLAIMED_BEAD_ID" --reason 'Build-basic starter review approved.'
```

Do not invoke provider-native subagents. This starter factory graph lane is the
fix delegation mechanism.
