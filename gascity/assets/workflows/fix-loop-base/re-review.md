This is the `fix-loop-base` methodology contract re-review step.

Concrete methodology packs override this step to call `{{code_review_formula}}`
after fixes. Preserve the preceding report before reusing its path. Record the
follow-up review report path on the workflow root as `gc.build.review_report_path`
before closing.

Count the caller's initial review as round 1. Carry forward the round numbers
and preserved reports from its preceding fixes, including across dispatches.
From round 3 onward, compare each blocking
finding with earlier rounds' blocking findings before applying the
`{{max_iterations}}` cap. If the findings share a class of fault and the earlier
class-level fix, stated in one sentence, would close both, stop. Treat an
uncertain comparison as the same family. On a match, or if the invoked review
already recorded a family stop, set `gc.build.repair_status=family_stop` on
this fix-loop workflow root before closing. Preserve both findings, their
round numbers and their report paths for the caller in the review report at
`gc.build.review_report_path`. A retry preserves this terminal result.
Open no further fix attempt. Otherwise continue only while the iteration
count is below the cap.

Artifact validation: this step is gated by the pack-owned `build-artifact-valid.sh` check, which validates the report recorded at `gc.build.review_report_path` against schema `gc.build.review.v1`. On repair attempts (`gc.attempt` greater than 1), read the validator errors from `gc.attempt_log` on the validation loop control bead (the dependent of this step bead) and repair the report in place instead of rewriting it. Two bounded repair attempts follow the first failure; exhausting them closes this stage with `gc.outcome=fail` and machine-readable validation errors that block downstream stages. Never ask questions in headless mode; record unresolved ambiguity inside the report.
