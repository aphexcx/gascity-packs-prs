This is the `fix-loop-base` methodology contract re-review step.

Concrete methodology packs may override the review step. Preserve the
preceding report before reusing its path. After fixes, invoke the selected
`{{code_review_formula}}` and record its workflow ID. On retry, use that
workflow's result. Record the follow-up review report path on the workflow
root as `gc.build.review_report_path` before closing.

Read `gc.build.repair_status` from the invoked review workflow root before
using the verdict or counting attempts. If it is `family_stop` or `exhausted`,
open no further fix attempt. Forward its status, `gc.failure_class`,
`gc.restart.reason` and `gc.restart.handoff_path` unchanged to the fix-loop
root. Set `gc.restart.entrypoint=manual` and `gc.outcome=fail` there and
close this step with the same failure outcome and class. The pointer names
the original terminal handoff, including both rounds and findings for a family stop.

Count the caller's initial review as round 1. Carry forward the round numbers
and preserved reports from its preceding fixes, including across dispatches.
From round 3 onward, compare each blocking
finding with earlier rounds' blocking findings before applying the
`{{max_iterations}}` cap. If the findings share a class of fault and the earlier
class-level fix, stated in one sentence, would close both, stop. Treat an
uncertain comparison as the same family. On a match, write one handoff with
both findings, rounds and preserved report paths. Record
`gc.build.repair_status=family_stop`, `gc.failure_class=review_family_stop`
and `gc.restart.reason=review_family_stop`. If an unapproved review instead
exhausts the cap, write one handoff with the outstanding findings and last
report path. Record `gc.build.repair_status=exhausted`,
`gc.failure_class=review_repair_failed` and `gc.restart.reason=review_repair_exhausted`.
For either local exit, record those values and `gc.restart.handoff_path`
on the fix-loop root and use the same manual, failing close above. A retry
preserves the terminal result. Otherwise return the follow-up verdict.

Artifact validation: this step is gated by the pack-owned `build-artifact-valid.sh` check, which validates the report recorded at `gc.build.review_report_path` against schema `gc.build.review.v1`. On repair attempts (`gc.attempt` greater than 1), read the validator errors from `gc.attempt_log` on the validation loop control bead (the dependent of this step bead) and repair the report in place instead of rewriting it. Two bounded repair attempts follow the first failure; exhausting them closes this stage with `gc.outcome=fail` and machine-readable validation errors that block downstream stages. Never ask questions in headless mode; record unresolved ambiguity inside the report.
