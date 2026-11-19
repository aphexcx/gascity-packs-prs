Process Superpowers code-review feedback.

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

Use the installed `receiving-code-review` skill as guidance for triaging review
and gap-analysis findings. Resolve required findings in this single lane and
preserve traceability to the reviewer, finding id, file anchors, and acceptance
criteria.

Read the code-review context from workflow root metadata
`gc.build.code_review_context_path`, the implementation review report from
`gc.build.code_review_report_path`, and the gap-analysis report from
`gc.build.gap_analysis_report_path`. Write the review fix summary to workflow
root metadata path `gc.build.review_fix_summary_path`, which should be
`<artifact_root>/review-fix-summary.md`.

Use implementation target {{implementation_target}} for any code changes.
Close this lane only after the review-fix artifact records resolved findings,
changed files, tests run, and blockers. If there are no required fixes, record a
no-op review-fix artifact instead of editing code.

If the review-fix artifact includes `gc.build.review.v1` front matter, its
`trace.coverage[].status` values and Markdown coverage table statuses must use
only schema allowed coverage statuses: `covered`, `blocked`, `deferred`,
`not_applicable`, `out_of_scope`, or `superseded`. Use `covered` for resolved
findings; do not use `resolved`, `violated`, `approved`, or `changes_required`
as coverage statuses. Include `rationale: <why this id is not covered>` on
every non-`covered` coverage row. The Markdown coverage table remains ID/status
only; the rationale belongs in YAML front matter.

If both review lanes approve, perform a no-op pass, update workflow root
metadata with `gc.build.code_review_status=approved`, and close with
`code_review.verdict=done`.

If either review lane returns `iterate`, process its required findings and
write the repair evidence. Even when the repair and focused tests pass, update
workflow root metadata with `gc.build.code_review_status=draft` and close with
`code_review.verdict=iterate`: the next attempt must independently re-review
the repaired implementation. This lane must not self-approve its own remediation.
Only a later attempt whose reviewer lanes both approve may close
with `code_review.verdict=done`.

An independently invoked review may have no requirements, plan, or
decomposition artifact. When the review context marks those inputs as not
provided and says `implementation_convoy_id: not provided`, record the absence
in the fix summary as non-blocking for this standalone review; it does not make
a required code finding remain open. Set `iterate` only for unresolved findings
in the reviewed implementation, a required artifact that is missing or invalid,
or a claimed resolution that cannot be verified.

For ordinary approval or iteration, close with `gc.outcome=pass`, `code_review.verdict=done|iterate`,
`code_review.report_path=<review fix summary path>`, and
`code_review.output_path=<review fix summary path>`.

Use the exact claimed bead id when updating metadata. Do not pass freeform notes
or additional positional arguments to `gc bd update`; unquoted words can resolve to
unrelated beads. Use this command shape:

```bash
gc bd update "$CLAIMED_BEAD_ID" \
  --set-metadata 'gc.outcome=pass' \
  --set-metadata 'code_review.verdict=done' \
  --set-metadata 'code_review.report_path=<review fix summary path>' \
  --set-metadata 'code_review.output_path=<review fix summary path>'
gc bd close "$CLAIMED_BEAD_ID" --reason 'Code-review feedback processed and approved.'
```

Do not invoke provider-native subagents. This graph lane is the delegation
mechanism.
