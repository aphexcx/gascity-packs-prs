
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
normalized interaction mode when the selected formula accepts them. Record
the review workflow ID for resume.

Read `gc.build.repair_status` from the completed review workflow root before
interpreting its verdict. After any fix, including on resume, check the last
dispatched fix-loop root first. If it is `family_stop` or `exhausted`, open
no further fix attempt. Forward that status, `gc.failure_class`,
`gc.restart.reason` and `gc.restart.handoff_path` from that root to the
adapter root. Keep the same handoff, including both findings, rounds and
preserved report paths for a family stop. Set `gc.blocked_reason` to the
forwarded reason and `gc.restart.entrypoint=manual`. Record `gc.outcome=fail`
on the root and close this build step with the same failure outcome and class.

Otherwise stop on approval. For an unresolved review with budget remaining,
run `{{review_fix_formula}}` with the numbered review history and preserved
report paths, counting the initial review as round 1. Record the dispatched
workflow ID for resume. If the adapter exhausts its own limit, write one
handoff with the outstanding findings and last report path. Record
`gc.build.repair_status=exhausted`, `gc.failure_class=review_repair_failed`,
`gc.restart.reason=review_repair_exhausted`, `gc.restart.handoff_path` and
`gc.blocked_reason=review_repair_exhausted` on the adapter root. Use the same
manual, failing exit. In `headless` interaction mode, never ask questions.
When required input is missing, stop blocked with a machine-readable
`gc.blocked_reason`.
