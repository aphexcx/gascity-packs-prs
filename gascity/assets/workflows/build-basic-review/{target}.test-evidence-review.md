Run the starter factory test evidence review lane.

Check that each accepted task recorded an intended behavior, first verification
command, proof command, changed files, and remaining risks. Verify that the
commands actually cover the acceptance criteria claimed by the requirements and
plan.

Write concrete findings under the build artifact root. Distinguish missing
proof from real product defects so the fix lane can either run the missing
command or change code.

Read the changed files where the review context puts them: the source
anchor's `work_dir`, or, when the source anchor has no `work_dir` and records
`gc.work_branch`, the recorded branch and commit read from your OWN lane
(`git -C "$GC_DIR" show <commit>:<path>`; every worktree of the rig shares its
refs). Never enter another agent's lane; to run a command in the branch case,
put your own lane on the recorded branch as the acceptance lane does (the
boundary test, then `git -C "$GC_DIR" switch --no-overwrite-ignore
"<gc.work_branch>"`, fail closed when git refuses).

Close with `gc.outcome=pass`,
`code_review.test_evidence_verdict=approve|iterate`, and
`code_review.output_path=<test evidence report path>`.

Use explicit close metadata so the review loop can detect the lane result:

```bash
gc bd update "$CLAIMED_BEAD_ID" \
  --set-metadata 'gc.outcome=pass' \
  --set-metadata 'code_review.test_evidence_verdict=approve' \
  --set-metadata 'code_review.output_path=<test evidence report path>'
gc bd close "$CLAIMED_BEAD_ID" --reason 'Build-basic test evidence review approved.'
```

If proof is missing or insufficient, set
`code_review.test_evidence_verdict=iterate` instead of `approve` and explain
whether the fix lane should run missing proof commands or change code.

Do not set `code_review.verdict` or `code_review.report_path`; synthesis and
fix application own the final review verdict.

Do not invoke provider-native subagents. You are the starter factory test
evidence review lane.
