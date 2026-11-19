
Execute the durable implementation and post-implementation review loop for the
generated implementation convoy.

Selected methodology formulas:

- implementation_formula: {{implementation_formula}}
- implementation_item_formula: {{implementation_item_formula}}
- code_review_formula: {{code_review_formula}}
- review_fix_formula: {{review_fix_formula}}

Selector compatibility was validated at the snapshot gate; the effective
interaction mode is recorded as `gc.var.interaction_mode` on the workflow
root. The requested review mode is {{review_mode}}.

The GitHub issue-fix adapter owns run identity, status comments, human gates,
PR publication, and finalization. The selected methodology formulas own only
how implementation work is drained, how code review is produced, and how failed
review findings are fixed.

Start or reuse `{{implementation_formula}}` against the generated
implementation convoy, passing through drain policy {{drain_policy}},
implementation target {{implementation_target}},
implementation_item_formula {{implementation_item_formula}}, and the
normalized interaction mode when the selected formula accepts them. After
implementation completes, launch `{{code_review_formula}}` with the issue-fix
subject and report paths, passing through review_mode {{review_mode}} and the
normalized interaction mode when the selected formula accepts them. If the
review blocks the fix, run `{{review_fix_formula}}` with the numbered review
history and preserved report paths in its findings input, counting the
initial review as round 1. Record the dispatched workflow ID for resume.
Before the first fix check the completed review workflow, and on return or
resume check the last dispatched fix-loop workflow root for
`gc.build.repair_status=family_stop` before using its verdict, retrying or
checking the remaining budget. On that signal, open no further fix attempt.
Copy the paired findings and report paths into the adapter handoff, record
`gc.build.repair_status=family_stop`, `gc.blocked_reason=review_family_stop`
and `gc.outcome=fail` on the adapter root, and close this build step with
`gc.outcome=fail` and `gc.failure_class=review_family_stop`. Otherwise stop
on approval or exhaustion of the iteration limit. In `headless` interaction mode,
never ask questions; on missing required input, stop blocked with a
machine-readable `gc.blocked_reason` instead of waiting on a human.
