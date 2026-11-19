This is the `build-from-review-base` repair-review stage.

Read the review report, review verdict, unresolved findings, fix attempt count,
review_mode={{review_mode}}, interaction_mode={{interaction_mode}},
review_fix_formula={{review_fix_formula}}, implementation_target={{implementation_target}},
implementation_formula={{implementation_formula}}, implementation_item_formula={{implementation_item_formula}},
and max_iterations={{max_iterations}} from the workflow root metadata and
artifacts.

If the review verdict is approved, do not mutate code. Record
`gc.build.repair_status=not_needed`, preserve the approved review metadata, and
close this step successfully.

If the review verdict is `changes_required` and review_mode=report, do not
mutate code. Write a repair handoff artifact that names the selected
review_fix_formula, affected requirements, target files or work items when
known, current review report path, and the exact continuation entrypoint to
restart from. Record at least:

- `gc.build.repair_status=repairable`
- `gc.restart.entrypoint=build-from-review`
- `gc.restart.reason=review_changes_required`
- `gc.restart.review_report_path=<review report path>`
- `gc.restart.review_fix_formula={{review_fix_formula}}`
- `gc.restart.implementation_target={{implementation_target}}`

Then close this step with failure metadata so the workflow cannot finalize as a
pass without an explicit restart.

If the review verdict is `changes_required` and review_mode is agent or
interactive, run or dispatch the selected review_fix_formula against the
recorded review findings and implementation evidence until one of these happens:

- the implementation is approved;
- review or repair returns blocked;
- a blocking finding repeats a family from an earlier review round (from round 3 onward);
- max_iterations is exhausted.

Count the initial review as round 1. If later rounds reuse a report path, save
a round-specific copy before it is overwritten. Before dispatching another
fix or recording exhaustion, compare each current blocking finding from round 3
onward with blocking findings from all earlier rounds. Two findings are in the
same family when they concern the same class of fault and the earlier finding's
class-level fix, stated in one sentence, would close the current one too. If
that comparison is uncertain, treat the pair as the same family.

On a match, including a `family_stop` returned by the selected fix loop, open
no further fix attempt. Record both findings in the repair handoff artifact,
with the round, report path and finding identifier or text for each. Record
`gc.build.repair_status=family_stop`, `gc.outcome=fail`,
`gc.failure_class=review_family_stop`, `gc.restart.entrypoint=build-from-review`
and `gc.restart.reason=review_family_stop` on the workflow root. Close the
repair step with the same failure outcome and class.

Record every attempt on the workflow root metadata and in artifacts. On
approval, record `gc.build.repair_status=approved` and the final review report
path. On blocked or exhausted attempts, record `gc.build.repair_status=blocked`
or `gc.build.repair_status=exhausted`, `gc.outcome=fail`,
`gc.failure_class=review_repair_failed`, and restart metadata with
`gc.restart.entrypoint=build-from-review`.

If any prerequisite review artifact, implementation evidence, or selected
formula is missing, do not invent a pass. Record `gc.build.repair_status=blocked`,
`gc.outcome=fail`, `gc.failure_class=review_repair_blocked`,
`gc.restart.entrypoint=build-from-review`, and `gc.restart.reason` with the
machine-readable blocked reason.

Do not close the workflow root with `gc.outcome=pass` from this stage.
