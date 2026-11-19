Apply required Compound Engineering code-review findings.

Before applying fixes, read the review findings for this round. Use this
child's `gc.attempt` as the round number (the first review is round 1).
Keep a copy of each input report under the build artifact root, keyed by
workflow, review loop and round. Reuse those copies if this step is retried.

From round 3 onward, compare each blocking finding with the blocking findings
in all earlier rounds of this loop. A pair belongs to the same family if it
concerns the same class of fault and the earlier class-level fix, stated in
one sentence, would close both findings. Treat an uncertain comparison as a
match. On a match, apply no fix. Write a handoff identifying both findings
and the round and preserved report path for each. On the workflow root set
`gc.build.code_review_status=failed`,
`gc.build.repair_status=family_stop`, `gc.outcome=fail`,
`gc.failure_class=review_family_stop`, `gc.restart.entrypoint=build-from-review`
and `gc.restart.reason=review_family_stop`. Close this child immediately with
`gc.outcome=fail`, `gc.failure_class=review_family_stop`,
`code_review.verdict=iterate` and `code_review.report_path=<handoff path>`.

Read the synthesized review report and resolve required findings in this single
lane. Preserve traceability to the reviewer, finding id, severity, file anchors,
and acceptance criteria.

Read the synthesized report from `gc.build.code_review_report_path`. Write the
review-fix summary to `gc.build.review_fix_summary_path`, which should be
`{{artifact_root}}/code-review/apply-summary.md`.

Use implementation target {{implementation_target}} for any code changes.
Close this lane only after the review-fix artifact records changed files, tests
run, resolved findings, and blockers. If there are no required fixes, record a
no-op review-fix artifact instead of editing code.

If the synthesized report approves the implementation with no required fixes,
perform a no-op pass, update workflow root metadata with
`gc.build.code_review_status=approved`, and close with
`code_review.verdict=done`. If required fixes remain after processing, update
workflow root metadata with `gc.build.code_review_status=draft` and close with
`code_review.verdict=iterate`.

For ordinary approval or iteration, close with `gc.outcome=pass`,
`code_review.report_path=<review fix summary path>`, and
`code_review.output_path=<review fix summary path>`.

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

Do not invoke provider-native subagents. This graph lane is the delegation
mechanism.
