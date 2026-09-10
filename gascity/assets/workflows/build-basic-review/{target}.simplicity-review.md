Run the starter factory simplicity review lane.

Review the implementation for maintainability, readable boundaries,
unnecessary abstractions, accidental broad changes, and obvious future
maintenance risk. Keep this lane beginner-friendly: flag only concrete issues
that a new factory user can understand and act on.

Before inspecting files, read `gc.build.code_review_context_path` from the
workflow root bead and use its `## Implementation Worktrees` section as the
authority for code under review. `gc.work_dir` is the launcher rig root, not the
implementation worktree. Do not inspect or edit the launcher checkout. Resolve
relative file paths against the listed implementation worktree, run
`cd "$WORKTREE"`, and verify `pwd -P` equals that worktree before running any
command.

Contract: `gc.work_dir` is the launcher rig root, not the implementation worktree.

Write findings under the build artifact root. Required findings must be tied to
specific changed files or artifacts and must explain the smallest useful fix.

Read the changed files where the review context puts them: the source
anchor's `work_dir`, or, when the source anchor has no `work_dir` and records
`gc.work_branch`, the recorded branch and commit read from your OWN lane
(`git -C "$GC_DIR" show <commit>:<path>`; every worktree of the rig shares its
refs). Never enter another agent's lane; to run a command in the branch case,
inspect the recorded commit DETACHED in your own lane as the acceptance lane
does (`git -C "$GC_DIR" switch --detach --no-overwrite-ignore <commit>`,
resolving the branch first with `git -C "$GC_DIR" rev-parse
"<gc.work_branch>"` when the context carries only the branch; fail closed when
git refuses). A review lane never takes the branch itself: git allows one
worktree per branch, only the writers hold it, and reviewers detached at one
commit never contend.

Close with `gc.outcome=pass`,
`code_review.simplicity_verdict=approve|iterate`, and
`code_review.output_path=<simplicity review report path>`.

Use explicit close metadata so the review loop can detect the lane result:

```bash
gc bd update "$CLAIMED_BEAD_ID" \
  --set-metadata 'gc.outcome=pass' \
  --set-metadata 'code_review.simplicity_verdict=approve' \
  --set-metadata 'code_review.output_path=<simplicity review report path>'
gc bd close "$CLAIMED_BEAD_ID" --reason 'Build-basic simplicity review approved.'
```

If you find required fixes, set
`code_review.simplicity_verdict=iterate` instead of `approve` and explain the
smallest required fix in the report and close reason.

Do not set `code_review.verdict` or `code_review.report_path`; synthesis and
fix application own the final review verdict.

Do not invoke provider-native subagents. You are the starter factory simplicity
review lane.
