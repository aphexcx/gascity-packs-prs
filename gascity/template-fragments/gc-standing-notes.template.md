{{ define "gc-standing-notes" -}}
# Standing Notes

Notes this pack keeps for every worker, beside the role text. Each says when it
applies.

## Deepening note{{/* Bead of record: gp-gsww. A comment renders nothing. */}}

THE DEEPENING NOTE. Once per PR, at your self-review and before the
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
`deepening note skipped: <reason>` in the PR handoff instead (the handoff mail
when the bead names a recipient, else the close reason). Never run a
repository-wide pass from a lane, and never add a new seam with a single
adapter.

## PR body lines{{/* Bead of record: gp-hdn2. A comment renders nothing. */}}

THE PR BODY LINES. Whenever you open a PR or update its body, end the body
with three lines, each on its own line and starting with its label, after
every other section (a Deepening note included); only the body's closing
trailer or sign-off lines may follow them:

```text
Testing: <the exact commands you ran, each with its counts and exit code>
Risk: <the tier and the path that set it>
How to revert: <the one command that undoes the change once merged>
```

- On the testing line, write `none: <why>` when you ran none.
- On the risk line, the tier comes from your city's written list of paths by
  risk tier: the highest tier of any path the diff touches, and that path;
  without such a list, or when no path the diff touches is on it, write
  `not tiered` and the highest-risk path the diff touches.
- On the revert line, the command runs after the merge, so name the commit
  through the PR (the revert of this PR's merge or squash commit, which has no
  sha until the merge), never a branch commit the merge may not keep; when no
  command undoes all of the change, say what a revert would leave behind: a
  migration that ran, a deploy, a message sent.
- On every line, write a secret (a token, a key, a password) as a placeholder
  such as `<token>`, never its value: the body is read by everyone who can see
  the PR.

A later round that updates the body rewrites the three lines for its own head
and keeps them after every other section. Keep the labels exactly as written:
a reviewer reads these lines first, and a city's tools may read them by their
labels.
{{- end }}
