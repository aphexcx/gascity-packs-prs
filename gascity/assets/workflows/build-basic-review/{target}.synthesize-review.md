Synthesize the build-basic starter factory review.

Read the acceptance, test evidence, and simplicity review reports. Deduplicate
findings, preserve the source review lane for each finding, and classify each
item as required fix, missing evidence, residual risk, or PRECONDITION.

Name the class of each required fix: the general fault, not the one case a
lane reported. List with it the other cases of that class you can see in the
change, each with its file and line, so the fix lane can close the whole class
in one pass. A finding that holds only under a runtime or provider that no
deployment of this change runs, or a right no one there has, is a
PRECONDITION, not a required fix: carry it with its precondition in one
sentence.

Also read `gc.build.code_review_context_path` from the workflow root bead. When
you carry a finding forward, include the source anchor and implementation
worktree from the context's `## Implementation Worktrees` section. If a finding
cites only a relative filename, resolve that filename relative to the
implementation worktree, never the launcher checkout. Required fixes must be
specific enough for the fix lane to act without guessing which worktree owns the
file.

Contract: `gc.work_dir` is the launcher rig root, not the implementation worktree.

Write one starter review synthesis under the build artifact root. The synthesis
must be short enough for a first-time factory user to scan, but concrete enough
for the fix lane to act without another planning pass.

Close with `gc.outcome=pass`,
`code_review.synthesis_path=<starter review synthesis path>`, and
`code_review.output_path=<starter review synthesis path>`.

Do not invoke provider-native subagents. Synthesis happens in this Gas City
fan-in lane.
