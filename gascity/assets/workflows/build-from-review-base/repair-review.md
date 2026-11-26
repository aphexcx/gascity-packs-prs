This is the `build-from-review-base` repair-review stage.

Read the review report, review verdict, unresolved findings, fix attempt count,
review_mode={{review_mode}}, interaction_mode={{interaction_mode}},
review_fix_formula={{review_fix_formula}}, implementation_target={{implementation_target}},
implementation_formula={{implementation_formula}}, implementation_item_formula={{implementation_item_formula}},
and max_iterations={{max_iterations}} from the workflow root metadata and
artifacts.

Read `gc.build.repair_status` from the completed review workflow root before
using its verdict. After a fix, including on resume, check the last dispatched
fix-loop root first. If it is `family_stop` or `exhausted`, open no further
fix attempt. Forward its status, `gc.failure_class`, `gc.restart.reason` and
`gc.restart.handoff_path` unchanged to the continuation root. The handoff
contains both findings, rounds and preserved report paths for a family stop.
Record `gc.outcome=fail` and `gc.restart.entrypoint=build-from-review` on
this root and close this step with the same failure outcome and class.

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
- past round 3, every blocking finding left is NOT SHOWN;
- max_iterations is exhausted.

From round 3 onward, a blocking finding of the same family as an earlier
round's ends the loop with `gc.build.repair_status=family_stop`, never another
fix round. Past round 3, a finding is fixed only when it is reproduced (by a
failing test, or shown reachable in what this city runs) and sits in the
changes made since the previous round; the rest are listed as NOT SHOWN with
their file and line. Pass the fix loop only the findings to fix, with the NOT
SHOWN ones listed apart in its findings input, and carry them into any
terminal handoff. When none is left to fix, open no further fix attempt and
record the exhaustion exit below, with the NOT SHOWN findings as its
outstanding findings.

Count the initial review as round 1. Include the round numbers and preserved
report paths in each fix loop's findings input and record its workflow ID
for resume. If later rounds reuse a report path, save a round-specific copy
before it is overwritten. Before dispatching another fix or recording
exhaustion, compare each current blocking finding from round 3 onward with
blocking findings from all earlier rounds. Two findings are in the
same family when they concern the same class of fault and the earlier finding's
class-level fix, stated in one sentence, would close the current one too. A
different guard, input or invariant is a different fault, even in the same
file. If that comparison is uncertain, treat the pair as the same family.

On a local match, open no further fix attempt. Record both findings in one repair handoff,
with the round, report path and finding identifier or text for each. Record
`gc.build.repair_status=family_stop`, `gc.outcome=fail`,
`gc.failure_class=review_family_stop`, `gc.restart.entrypoint=build-from-review`
and `gc.restart.reason=review_family_stop` on the workflow root. Set
`gc.restart.handoff_path` to that artifact before closing. Close the
repair step with the same failure outcome and class.

Record every attempt on the workflow root metadata and in artifacts. On
approval, record `gc.build.repair_status=approved` and the final review report
path. On local exhaustion, write one handoff with the last review, fixes,
tests and outstanding findings. Record its path in `gc.restart.handoff_path`,
`gc.build.repair_status=exhausted`, `gc.restart.reason=review_repair_exhausted`,
`gc.outcome=fail`, `gc.failure_class=review_repair_failed` and
`gc.restart.entrypoint=build-from-review` on the workflow root. Close this
step with the same failure outcome and class. The continuation entrypoint
is for a person's decision. Both terminal outcomes end this run.

On blocked attempts, record `gc.build.repair_status=blocked`, `gc.outcome=fail`,
`gc.failure_class=review_repair_failed`, and restart metadata with
`gc.restart.entrypoint=build-from-review`.

If any prerequisite review artifact, implementation evidence, or selected
formula is missing, do not invent a pass. Record `gc.build.repair_status=blocked`,
`gc.outcome=fail`, `gc.failure_class=review_repair_blocked`,
`gc.restart.entrypoint=build-from-review`, and `gc.restart.reason` with the
machine-readable blocked reason.

Do not close the workflow root with `gc.outcome=pass` from this stage.
