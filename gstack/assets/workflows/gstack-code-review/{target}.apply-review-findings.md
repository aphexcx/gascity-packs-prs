Apply gstack code-review findings.

Before applying fixes, read the review findings for this round. Use this
child's `gc.attempt` as the round number (the first review is round 1).
Keep a copy of each input report under the build artifact root.
For a standalone review, use the directory containing the requested report
(`gc.var.report_path`).
Key the copies by workflow, review loop and round. Reuse them on retry.

From round 3 onward, compare each blocking finding with the blocking findings
in all earlier rounds of this loop. A pair belongs to the same family if it
concerns the same class of fault and the earlier class-level fix, stated in
one sentence, would close both findings. Treat an uncertain comparison as a
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

Stop here. A person decides what happens next.

Close this child with `gc.outcome=fail`, the same failure class,
`code_review.verdict=iterate` and `code_review.report_path=<handoff path>`.
This terminal close takes precedence over the ordinary verdict instructions below.

Use implementation target {{implementation_target}} for any code changes. Read
the review synthesis from workflow root `gc.build.code_review_report_path`. If
all required review lanes approve, write a no-op review-fix artifact. If
required fixes or missing evidence remain, make the smallest focused changes
and run proof commands. Write the review-fix artifact under the artifact root
and record it on the workflow root as `gc.build.review_fix_summary_path`.

Set `code_review.verdict=done` only when staff, QA evidence, security, and gap
analysis approve, and update workflow root metadata with
`gc.build.code_review_status=approved`. Set `code_review.verdict=iterate` when
required fixes remain, and update workflow root metadata with
`gc.build.code_review_status=draft`.

For ordinary approval or iteration, close with `gc.outcome=pass`,
`code_review.verdict=done|iterate`,
`code_review.report_path=<review-fix artifact path>`, and
`code_review.output_path=<review-fix artifact path>`.

Keep edited build artifacts schema-valid. Stage checks and the build gate's final artifact check validate these
Markdown build artifacts, so any edit you make to one must leave it valid:

- The implementation summary at workflow root metadata `gc.build.implementation_summary_path`, schema `gc.build.implementation-summary.v1`:
  front-matter `status` must be one of `draft`, `approved`, `blocked`, or `superseded`.
- The synthesized review report at workflow root metadata `gc.build.code_review_report_path`, schema `gc.build.review.v1`:
  front-matter `status` must be one of `draft`, `questions`, `approved`, `changes_required`, `blocked`, or `superseded`.

When you edit one of them:

- Edit it in place and preserve its YAML front matter: keep `schema`,
  `workflow`, `methodology`, `producer`, and `trace` intact, and keep
  `trace.coverage` and the Markdown `ID`/`Status` coverage table in sync.
- Use only the allowed `status` values listed above. Do not invent statuses
  such as `reviewed`; record review progress in the apply summary and bead
  metadata, not in the artifact's front matter.
- Before closing with `gc.outcome=pass`, from `$GC_RIG_ROOT`, re-run the
  validator on every build artifact you edited and fix every reported error:
  `python3 .gc/scripts/validate_build_artifact.py --schema gc.build.implementation-summary.v1 --path "<implementation summary path>"`.
  `python3 .gc/scripts/validate_build_artifact.py --schema gc.build.review.v1 --path "<review report path>"`.
- On a repeated attempt (`gc.attempt` greater than 1), first read the validator
  errors from `gc.attempt_log` on the loop control bead and repair the
  artifact in place.

Do not invoke provider-native subagents. This Gas City graph lane is the delegation
mechanism for fixes.
