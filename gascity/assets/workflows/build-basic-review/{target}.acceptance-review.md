Run the starter factory acceptance review lane.

Review the implementation against the requirements, acceptance criteria,
implementation plan, decomposition, and task summaries. Focus on correctness:
did the factory build the requested behavior, and did it avoid out-of-scope
changes?

Read the review context first and evaluate the implementation source
anchor/worktree recorded there. The launcher rig root is not the review target
for build-basic; it may still contain the original fixture until publish. Do not
mark acceptance as `iterate` merely because the root checkout is unchanged when
the recorded source anchor/worktree implements the requested behavior and its
proof commands pass.

The review context names the workspace either as the source anchor's
`work_dir` or, when the source anchor has no `work_dir` and records
`gc.work_branch`, as a branch and a commit id. In the branch case work from
your OWN lane and never enter another agent's directory: read with `git -C
"$GC_DIR" log -1 <commit>` and `git -C "$GC_DIR" show <commit>:<path>` (every
worktree of the rig shares its refs); to run proof commands, inspect the
recorded commit DETACHED in your own lane: `git -C "$GC_DIR" switch --detach
--no-overwrite-ignore <commit>` (the commit id the review setup recorded; if
the context carries only the branch, resolve it first with `git -C "$GC_DIR"
rev-parse "<gc.work_branch>"`), and fail closed when git refuses (an ignored
file in your lane colliding with a tracked path, or a commit missing from the
repository): never `--force`, never a stash, never remove the colliding file.
A review lane never takes the branch itself: git allows one worktree per
branch, only the writers (implement, then the fix lane) hold it, one at a
time, and three reviewers detached at the same commit never contend and never
block the fix lane. A `work_dir` naming an agent lane
(`.worktrees/<rig>/lane-*`) is invalid: write an iterate finding against
review setup instead of entering it.

Write findings under the build artifact root. Required findings must include
the relevant requirement or task reference plus the file, command, or artifact
that proves the issue.

Close with `gc.outcome=pass`,
`code_review.acceptance_verdict=approve|iterate`, and
`code_review.output_path=<acceptance review report path>`.

Use explicit close metadata so the review loop can detect the lane result:

```bash
gc bd update "$CLAIMED_BEAD_ID" \
  --set-metadata 'gc.outcome=pass' \
  --set-metadata 'code_review.acceptance_verdict=approve' \
  --set-metadata 'code_review.output_path=<acceptance review report path>'
gc bd close "$CLAIMED_BEAD_ID" --reason 'Build-basic acceptance review approved.'
```

If you find required fixes, set
`code_review.acceptance_verdict=iterate` instead of `approve` and explain the
smallest required fix in the report and close reason.

Do not set `code_review.verdict` or `code_review.report_path`; synthesis and
fix application own the final review verdict.

Do not invoke provider-native subagents. You are the starter factory acceptance
review lane.
