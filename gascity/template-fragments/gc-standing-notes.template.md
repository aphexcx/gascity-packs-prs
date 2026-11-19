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
- On the revert line, the command runs after the merge, so name what the PR
  landed as: the squash commit when squashed; the first-parent change of the
  merge commit when merged with two parents; the whole landed range, oldest
  to newest, when rebased or fast-forwarded, as one range from the commit it
  landed on (`git revert --no-merges <landed-on>..<newest>`, which reverts
  newest first). Never name a branch commit the merge may not keep; when you
  cannot yet tell how the PR will land, cover each way the repository allows;
  when no command undoes all of the change, also say what a revert would leave
  behind (a migration that ran, a deploy, a message sent).
- On every line, write a secret (a token, a key, a password) as a placeholder
  such as `<token>`, never its value: the body is read by everyone who can see
  the PR.

A later round that updates the body rewrites the three lines for its own head
and keeps them after every other section. Keep the labels exactly as written:
a reviewer reads these lines first, and a city's tools may read them by their
labels.

## Review rules{{/* Bead of record: gp-kiis. A comment renders nothing. */}}

THE REVIEW RULES. Whenever a review returns findings on your PR, act on them
by these rules: each review round costs a full read of the PR, so spend one
only where it buys a fix.

1. Fix the whole class. When a review finds a blocking fault, name its class
   (the general fault, not the one case), fix every case of that class in your
   diff, and add one test at the class level; your handoff names the class,
   its cases and the test. A case fixed alone brings the same finding back on
   another line in the next round.
2. Stop at round three. From review round three on, a blocking finding of THE
   SAME FAMILY as a blocking finding of an earlier round of this PR is not
   fixed in that round: stop, list it in your handoff with the round and the
   earlier finding it repeats, and leave the mayor one decision, either a
   check for the class built under its own tests or a merge with the class
   filed as a bead. THE SAME FAMILY means the finding names the same class as
   the earlier one, and the earlier one's class-level fix, written in one
   sentence, would close it too; when you cannot tell, it is the same family
   and you stop. Past round three, only a finding reproduced in that round's
   own changes (a failing test, or shown reachable in what your city runs)
   buys another round; list the rest as NOT SHOWN with their file and line.
3. Let no lone reviewer force a round. Before one reviewer's finding costs a
   round, have it shown: write a failing test that shows it, or have a second
   model check that one finding and answer real or not with the line that
   shows it. A finding you cannot show either way after an honest try is
   listed as NOT SHOWN with what you tried, and is not fixed blind: one
   model's reading is a suspicion, not a fault.
4. Never let a minor finding block, and give every finding an outcome. Fix a
   minor in the same head when it is cheap, and list it otherwise. Your
   handoff carries one line per finding of every round, each with one of four
   outcomes: FIXED (with the commit), WRONG (with the line that shows the
   reviewer was mistaken), INTENDED, or DOES NOT MATTER. A finding that needs
   a runtime, a provider or a right your city does not run is listed as
   PRECONDITION with the precondition in one sentence; it does not block, and
   you do not build for it.
5. Let review depth follow risk. Docs, tests and small internal changes get
   your own checks and one gate; a second model reads a PR's whole first head,
   and later heads only on a risky path. Your city or its mayor sets the
   depth: follow the review class the bead names, and where it asks for more
   than these rules, it wins.
6. Take the merge list from its owner. Which paths need a person's word before
   a merge is your city's written list, kept by its founder or owner: read it
   for the risk line of the PR body lines above, and never invent one. Until
   that list exists, your city's merge rule stands.
{{- end }}
