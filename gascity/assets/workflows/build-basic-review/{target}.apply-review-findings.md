Apply build-basic starter review findings.

Before applying fixes, read the review findings for this round. Use this
child's `gc.attempt` as the round number (the first review is round 1).
Keep a copy of each input report under the build artifact root.
Key the copies by workflow, review loop and round. Reuse them on retry.

From round 3 onward, compare each blocking finding with the blocking findings
in all earlier rounds of this loop. Blocking findings are the synthesis's
required fixes and missing evidence; a residual risk or a PRECONDITION item is
never one. A pair belongs to the same family if it
concerns the same class of fault and the earlier class-level fix, stated in
one sentence, would close both findings. A different guard, input or invariant
is a different fault, even in the same file. Treat an uncertain comparison as a
match. On a match, apply no fix. In the handoff identify both findings, their
rounds and the preserved report paths. Set `gc.build.repair_status=family_stop`,
`gc.failure_class=review_family_stop` and `gc.restart.reason=review_family_stop`.

Otherwise process this pass normally. If its verdict would be `iterate` on
the controller's last allowed attempt, record the last report, fixes, tests
and outstanding review in the handoff. Set `gc.build.repair_status=exhausted`,
`gc.failure_class=review_repair_failed` and `gc.restart.reason=review_repair_exhausted`.

For either exit, write one terminal handoff alongside the preserved reports,
keyed by workflow, loop and round. Reuse its path on retry. Include the reason,
outstanding findings and preserved report paths. After writing it, record
these values on the workflow root along with the status, reason and failure class.

- `gc.outcome=fail` and `gc.build.code_review_status=failed`
- `gc.restart.review_report_path=<preserved review report path>`
- `gc.restart.handoff_path=<handoff path>`
- `gc.restart.entrypoint=manual`

Stop here. A person decides what happens next. If this lane took the item
branch, complete the context refresh for any fix commit and the verified
branch release below before the terminal close.

Close this child with `gc.outcome=fail`, the same failure class,
`code_review.verdict=iterate` and `code_review.report_path=<handoff path>`.
This terminal close takes precedence over the ordinary verdict instructions below.

Use implementation target {{implementation_target}} for any code changes. Read
the starter review synthesis. If all three review lanes approve, write a no-op
review summary. If required fixes or missing evidence remain, address them
with the smallest focused changes that close each class, run the relevant
proof commands, and write the review-fix summary under the build artifact
root.

Fix each required fix as a class: fix every case of its class in the change,
not only the case the review named, and add one test at the class level that
fails on any case of it. Name the class, its cases and that test in the
summary. A residual risk or a PRECONDITION item never forces another loop: fix
a residual risk only in a pass that already commits a required fix, when it is
cheap, and list it otherwise.

The review-fix summary carries one line per finding of this round, with
exactly one outcome: FIXED (with the commit, or for missing evidence the proof
command and its result), WRONG (the reviewer was mistaken, with the line that
shows it), INTENDED, or DOES NOT MATTER. A
PRECONDITION item and a residual risk left unfixed take DOES NOT MATTER, the
PRECONDITION with its precondition in one sentence. The only lines without an
outcome are a required fix this pass cannot settle, such as one with no
implementation worktree, and a blocking finding an earlier summary answered
that this pass neither fixes nor proves (below): each says why.

In a round after the first that the family comparison above did not stop,
read every review-fix summary listed under the review context's
`## Earlier Review Rounds` before you settle a finding. A blocking finding of
this round that an earlier summary answered (WRONG, INTENDED, DOES NOT
MATTER, or FIXED by a proof command without a commit) has been rejected,
whether or not a lane marked it ANSWER REJECTED. It takes one of three lines
and no other: FIXED with a commit; for missing evidence, FIXED with a proof
command that no earlier summary records for it; or a line without an outcome
that gives the reviewer's reason and says why this pass neither fixes nor
proves it. Never write WRONG, INTENDED or DOES NOT MATTER for it again, and
never again FIXED by the same proof command without a commit. Each of the
three sets `iterate` by the verdict rules below. An entry the synthesis carries as ANSWER ACCEPTED is not
a finding of this round and takes no line in the summary.

Record every review-fix summary this step writes, the no-op summary too, in
the review context file at `gc.build.code_review_context_path` before this
pass closes, whether or not the pass commits: under the heading
`## Earlier Review Rounds`, which you add at the end of the file when it is
not there yet, keep one line per round,
`- round <n>: <absolute path of that round's review-fix summary>`. Each
round's review-fix summary has a path of its own, keyed by workflow, review
loop and round like the input copies, and never overwrites an earlier
round's. A retry of the same round rewrites that round's line and never adds
a second. This write
needs no branch and no commit: the context file lies under the build artifact
root. The section is the loop's own, not a source anchor's record. A pass
that ends on a terminal exit without a review-fix summary records nothing.

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
(the source anchor has no `work_dir` and records `gc.work_branch`: the item
was handed over by branch, and a directory is per agent), the workspace for
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

This lane is the second WRITER in the item's lifecycle: a lane HOLDS the
item's branch only while it is writing to it, and git allows one worktree per
branch. The branch is free when you arrive because the implementation lane
released it on handoff, and the review lanes only ever inspected its commit
detached. Take it only when there is a fix to commit (a no-op summary needs no
switch). If the switch fails because another worktree still holds the branch
(git says "already checked out at <path>", or "already used by worktree at
<path>" in newer git; `git worktree list` names the holder: a writer that
crashed before releasing), fail this step closed with the holder's path in
the close reason and mail the mayor (`gc mail send mayor -s "branch held:
<gc.work_branch>" -m "<holder path>"`); the run operator (a human or the
mayor) releases it with `git -C <holder lane> switch --detach`.
Never enter that lane, never `--force`, never remove its checkout, never
release another agent's lane.

Refresh the review context after the fix commit and BEFORE releasing the
branch. The review setup ran ONCE, outside the review loop, and the three
review lanes inspect the commit it recorded; a context left at that commit
makes every later attempt review the ORIGINAL code and repeat the findings you
just resolved until the attempts run out. The record is PER SOURCE ANCHOR:
read the new commit id from the worktree you committed in, `git -C
"$WORKTREE" rev-parse --verify HEAD` (in the lane case `$WORKTREE` is
`$GC_DIR` on the branch you still hold, so it equals `git -C "$GC_DIR"
rev-parse --verify "refs/heads/<gc.work_branch>"`; in the `work_dir` case
the per-item worktree is detached at your commit and has no item branch to
read; never the bare branch name: git resolves a bare name tag-first, and a
tag with the branch's name would record the tag's commit, the base, as the
reviewed commit), rewrite the
commit id (and the changed-file list, when the context carries one) in the
record of the source anchor you committed on, in the review context file at
`gc.build.code_review_context_path` on the workflow root, then record the new
commit on that source anchor: `gc bd update "<source-anchor-id>"
--set-metadata 'gc.review_commit=<sha>'` (the review
lanes read that anchor's key first and fall back to that anchor's record in
the context file). Touch no other source anchor's record or key: the other
items sit on independent branches at their own commits, and there is no
workflow-wide review commit to update. The retry sequence is: fix, commit,
refresh the context, release the branch; then the loop re-runs the reviewers
on the new commit. Never leave the old commit in the context after a fix. The
refresh applies in the `work_dir` case too (a fix committed in the per-item
worktree): the commit id in the context and on the source anchor are
rewritten the same way; only the release below is the lane case.

After refreshing any fix commit and before any close after taking the item
branch (including exhaustion), release the branch from your lane: `git -C
"$GC_DIR" switch --detach` (HEAD stays at your commit; untracked files stay),
then verify `git -C "$GC_DIR" branch --show-current` prints nothing, and name
the commit id in this step's close reason. This release is inside the guarded
lane case above: it runs only after the boundary test passed and your switch
onto the branch succeeded. A `$GC_DIR` that failed the boundary test (the rig
root, a subdirectory of it, or a worktree of another repository) was never
switched and detaches nothing, because a detach there would move the human
checkout's HEAD; in the `work_dir` case the per-item worktree is already
detached and shared with no one, so there is nothing to release.

Set `code_review.verdict=done` only when this pass commits no fix and
acceptance, test evidence, and simplicity all approve; a lane that reported
only residual risks or PRECONDITION items counts as approving. Set
`code_review.verdict=iterate` when this pass commits a fix or supplies missing
evidence, so the review lanes see it, or when a required fix or missing
evidence remains. A required fix
settled WRONG, INTENDED or DOES NOT MATTER sets `iterate` too: the review
lanes, not this lane, retire a required fix. An unchanged root checkout is
never a required fix and takes no line in the summary. In the case above where
it is the only reported issue, it is the one exception to both rules: the
verdict stays `done`, whatever verdict the lane that reported it recorded.

For ordinary approval or iteration, close with `gc.outcome=pass`,
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
