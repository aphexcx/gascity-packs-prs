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

After reading the review context and before writing any finding, read its
`## Earlier Review Rounds` section when it has one, and every review-fix
summary that section lists, whatever its round; round 1 has none. For each
finding you would report, look for its line in those summaries; when several
rounds carry a line for one finding, the latest round's line counts. A line
whose outcome names no commit (WRONG, INTENDED, DOES NOT MATTER, or FIXED by
a proof command and its result) is an answer: check it against the current
commit, by the line or reason it gives or by the proof command and its
result, which you may run again. When the answer holds, do not report the
finding: list it once in your report as ANSWER ACCEPTED with the round of the
answer. When it does not hold, report the finding as ANSWER REJECTED with the
round of the answer and the reason the answer fails. When you cannot tell
whether the answer covers what you see (another case of the class, another
line, another criterion), the answer is rejected: report the finding as
ANSWER REJECTED and say what it does not cover. A line that reads FIXED with
a commit is no answer: judge the current commit as you would without it. A
line without an outcome is no answer: the finding stands. A listed summary
that is missing or cannot be read answers nothing: say so in your report and
report as you would without it. An accepted answer is no reason for
`iterate`: with no finding left but accepted answers, record `approve`.

Write findings under the build artifact root. Required findings must be tied to
specific changed files or artifacts and must explain the smallest useful fix.

Read the changed files where the review context puts them, per source anchor
(the context carries one record per source anchor): that anchor's `work_dir`,
or, when the source anchor has no `work_dir` and records `gc.work_branch`, the
recorded branch and commit read from your OWN lane (`git -C "$GC_DIR" show
<commit>:<path>`; every worktree of the rig shares its refs). Read the CURRENT
commit first from `gc.review_commit` on that source anchor bead (`gc bd show
<source-anchor-id> --json`; written by the review setup, refreshed by the fix
lane after every fix commit on that item; the loop re-runs this lane after a
fix) and fall back to that anchor's record in the review context file only
when that key is absent; there is no workflow-wide review commit, each item is
inspected at its own. Never enter
another agent's lane; to run a command in the branch case, inspect the
recorded commit DETACHED in your own lane as the acceptance lane does, and as
it does prove the lane FIRST, before any `switch` or detach, with the boundary
test `do-work/prepare-worktree` step 4 applies (resolved `git -C "$GC_DIR"
rev-parse --show-toplevel` equals `$GC_DIR`; neither the rig root nor inside
it; `rev-parse --git-common-dir` is the rig root's `.git`). A reviewer role
with no configured `work_dir` starts in the RIG ROOT, the human checkout: when
any part fails, this lane switches and detaches nothing there (a detach run
there moves the human checkout's HEAD), inspects by `git -C "$GC_DIR" show
<commit>:<path>` and `git -C "$GC_DIR" log -1 <commit>` only, and if a proof
command needs a checkout at the commit closes with `gc.outcome=fail`,
`gc.failure_class=no-lane` and the reason "no lane for this role", naming the
fix: a `work_dir` for this role (README, Worker workspaces). Only when all
three parts hold: `git -C "$GC_DIR" switch --detach --no-overwrite-ignore
<commit>` (resolving the branch first with `git -C "$GC_DIR" rev-parse
--verify "refs/heads/<gc.work_branch>"`, the full ref, since a bare name
resolves tag-first, when the context carries only the branch; fail closed when
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
