{{ define "gc-standing-notes" -}}
# Standing Notes

Notes this pack keeps for every worker, beside the role text. Each says when it
applies.

## Deepening note

THE DEEPENING NOTE (gp-gsww). Once per PR, at your self-review and before the
PR is pushed or its head updated, while the whole change is still in your
context, make one deepening read on the modules this diff touched, never the
whole repository. Use the codebase-design skill (SKILL.md and DEEPENING.md in
your skill directory when installed); without it, the steps below are the whole
method.

1. Name the candidate clusters: a pass-through layer, duplicated logic across
   callers, a module whose interface is larger than its body, a seam with one
   adapter.
2. Give each its dependency category: in-process, local-substitutable, remote
   but owned, or true external.
3. Apply the deletion test: imagine the module deleted. If its complexity
   vanishes, it was a pass-through; if it reappears across its callers, the
   module earns its place.
4. Decide: deepen now in this PR (only inside the bead's scope, with the tests
   moved to the new interface and the old ones deleted), a follow-up bead
   (name the cluster and its category; the mayor cuts it under the rule of
   three), or none.

Write the result as a `Deepening note` section at the end of the PR body, three
to ten lines; `none` with one reason is a fine answer. Skip the read for docs,
config, evidence and diffs under about forty changed lines, and say
`deepening note skipped: <reason>` in the PR handoff instead. Never run a
repository-wide pass from a lane, and never add a new seam with a single
adapter.
{{- end }}
