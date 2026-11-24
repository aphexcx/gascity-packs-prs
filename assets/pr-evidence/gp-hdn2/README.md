# gp-hdn2 evidence: THE PR BODY LINES

The second note in `gascity/template-fragments/gc-standing-notes.template.md`,
its README sentence, and its tests. Base: main at `148e4a0f`. Round 1's code
head: `e65b23bf`. The gate fold's code head: `659265c0` (see "Round 2"
below). The commits after each add only this directory.

`<gc>` below is a dev build of gc (`gc version` prints `dev`); CI installs
gastownhall/gascity `cmd/gc@latest`. Every run is under `env -i` with only
PATH, HOME and TMPDIR kept, because a worker session's own GC_* variables
leak into one claim-command test.

## Commands and counts

The targeted run (the static fragment tests and the real-gc render tests):

```sh
env -i PATH="$PATH" HOME="$HOME" TMPDIR="$TMPDIR" GC_TEST_BIN=<gc> \
  uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest \
  tests/test_gc_standing_notes_fragment.py tests/test_gc_role_prompt_integration.py \
  -q -k "standing or fragment or city_lane or pr_body" -rf
```

| Run | Tree | Result |
| --- | --- | --- |
| Baseline, the targeted run | main `148e4a0f` | 26 passed, exit 0 |
| `red.txt`, the targeted run | `e745a672`'s tests on main's fragment and README (before the note was written) | 6 failed, 25 passed, exit 1 |
| `green.txt`, the targeted run | `e65b23bf` | 32 passed, exit 0 |
| `env -i PATH HOME TMPDIR make test` | a clean clone at `e65b23bf` | reachable: 93 tests, 92 pass, 0 fail, 1 skipped; Python: 3002 passed, 132 skipped, 0 failed; exit 0 |
| CI's gc step: `GC_TEST_BIN=<gc> python3 -m pytest tests/test_gc_role_prompt_integration.py tests/test_pack_composition_warnings.py -q` (through `uv run --with ...`) | a clean clone at `e65b23bf` | 103 passed, exit 0 |

The six reds are the new tests, each failing for the missing note or README
sentence (the `E` lines in `red.txt`). In `red.txt` two lines of pytest's
locals dump are scrubbed to `<TMPDIR>` and `<GC_TEST_BIN>`, and trailing
spaces are stripped from four lines so `git diff --check` passes.

A `make test` in the lane's own worktree also ran: 1 failed,
`tests/test_default_branch_resolution.py::DefaultBranchResolutionTest::test_the_superseded_pipeline_is_gone_from_every_asset`.
That test walks every file under the repository root, and the worktree's
ignored `.gc/` directory holds scratch clones from earlier work that carry
old copies of that test file. A clean clone has no `.gc/`: the same suite at
`e745a672` gave 3001 passed, 0 failed, and at `e65b23bf` 3002 passed, 0 failed.

## Files

- `red.txt`, `green.txt`: the targeted run, before and after.
- `rendered-notes.txt`: a real gc render (`gc prime --strict`) of a city lane
  whose prompt calls `gc-role-worker` and then `gc-standing-notes`, at
  `659265c0` (round 1's copy, at `e65b23bf`, differs only in the revert
  bullet). It holds what follows the role text, both notes.
- `mutations.txt`: the review folds shown to bite. The new tests fail on the
  text before each fold, and on mutations of the final text: the block split,
  a relative path, "last", the trailer clause gone, a clause hidden in a
  template comment.
- `codex-r1-2d6715dd.md`: the lane's codex QUICK round 1 at `2d6715dd`. Two
  header paths are replaced by `<hub home>` and `<lane worktree>`.

## Review rounds and outcomes

Self-review (a fresh reviewer subagent on the lane's model, at `e745a672`):
BLOCKER 0, MAJOR 1, MINOR 3, NIT 4.

| Finding | Outcome |
| --- | --- |
| MAJOR 1: the trailer allowance was pinned by no test | FIXED `2d6715dd`. The class is "a placement statement no test pins". All three cases are fixed: the trailer clause is pinned, "keeps them last" is gone, and the README sentence names the trailers. One test covers the class: `test_pr_body_lines_placement_leaves_room_for_the_closing_trailers` |
| MINOR 2: "keeps them last" contradicted the trailer allowance | FIXED `2d6715dd` |
| MINOR 3: the path checks missed relative paths | FIXED `2d6715dd`: any slash fails, in both tests |
| MINOR 4: the revert line did not say which commit | FIXED `2d6715dd`, then reworded at `e65b23bf` (codex MAJOR 2) |
| NIT 5: the README sentence left out the trailers | FIXED `2d6715dd` |
| NIT 6: with several tiers touched, which one counts | FIXED `2d6715dd`: the highest tier of any touched path |
| NIT 7: nothing required the three lines to be one block | FIXED `2d6715dd`: three consecutive lines, in both tests |
| NIT 8: the render test retypes three of the static test's patterns, and a written-out date passes | DOES NOT MATTER: the existing tests already duplicate `BEAD_ID` the same way (see the Deepening note in the PR body), and no note names a month |

Codex QUICK round 1 at `2d6715dd`: BLOCK (MAJOR 2, MINOR 2).

| Finding | Outcome |
| --- | --- |
| MAJOR 1: the testing line can copy a secret into the body | FIXED `e65b23bf`. The class is "a line copies a command into a public body", and one sentence covers all three lines: a secret is written as a placeholder. Pinned clause |
| MAJOR 2: the revert line asked for a commit that does not exist yet | FIXED `e65b23bf`: name the commit through the PR, since its merge or squash commit has no sha until the merge |
| MINOR 3: a tier list that no touched path is on | FIXED `e65b23bf`: `not tiered` and the highest-risk path |
| MINOR 4: a clause hidden in a template comment passed the clause tests | FIXED `e65b23bf`: the clause tests read the renderable text |

## Round 2: the mayor's gate round 1

The mayor's gate round 1 at `e4d27c78`: BLOCK (MAJOR 1 / MINOR 2). The gate
ordered one last step with no codex round: fix the MAJOR, fix MINOR 3 if it
is a few lines, leave MINOR 2 for a follow-up, self-review the delta.

| Finding | Outcome |
| --- | --- |
| MAJOR 1: the revert line assumes one commit is the whole PR, so a rebased or fast-forwarded PR is only partly undone by reverting the one sha the PR resolves to | FIXED `9ca68b17`, then `659265c0` (the self-review below). The revert instruction stays one bullet and names what the PR landed as: the squash commit when squashed; the first-parent change of the merge commit when merged with two parents; the whole landed range when rebased or fast-forwarded. The clause test pins the whole bullet as one clause and fails on the old wording |
| MINOR 3: swapping the text after two labels passed both block checks | FIXED `9ca68b17`: the static test and the real-gc render test match the three lines whole, each label with its own instruction |
| MINOR 2: an exact multi-line command (a heredoc) cannot sit on the one Testing line | NOT FIXED, by the gate's order: a follow-up |

Self-review of `e4d27c78..9ca68b17` (a fresh reviewer subagent on the lane's
model): BLOCKER 0, MAJOR 1, MINOR 3, NIT 3. Folded at `659265c0`.

| Finding | Outcome |
| --- | --- |
| MAJOR: "the whole landed range, oldest to newest" leads to three wrong commands | FIXED `659265c0`. `r2-git-revert-facts.sh` builds a scratch repository per case and `r2-git-revert-facts.txt` holds its output (git 2.54.0). `git revert A B` reverts in the order listed and stops when B touched A's lines. `A..B` leaves A out. A fast-forward of a branch that merged its base stops at the merge commit. A range from the commit the PR landed on, `git revert --no-merges <landed-on>..<newest>`, restores the tree in both shapes and reverts newest first. The note now gives that form |
| MINOR: a worker writing the line before the merge may not know how the PR will land | FIXED `659265c0`: it covers each way the repository allows |
| MINOR: `9ca68b17` made what a revert leaves behind an alternative to the command ("or") | FIXED `659265c0`: "also", as before the gate fold |
| MINOR: nothing pinned one revert instruction | FIXED `659265c0`: a second "On the revert line" fails the clause test |
| NIT: the list after "never a branch commit the merge may not keep:" read as a list of branch commits | FIXED `659265c0`: the clause moved after the list |
| NIT: "the first-parent change of the merge commit" is jargon; say `-m 1` | NOT CHANGED: the gate's own words for this case |
| NIT: the label-prefix assert compares two test constants, and the three lines are written in both test files | NOT CHANGED: the assert guards the constants against each other; the second copy is the duplication the PR's Deepening note names |

The targeted run, round 2:

| Run | Tree | Result |
| --- | --- | --- |
| `r2-red.txt` | `659265c0`'s tests on `e4d27c78`'s note | 1 failed, 31 passed, exit 1 |
| `r2-mutations.txt` | `659265c0`'s tests on `9ca68b17`'s note | 1 failed, 31 passed, exit 1 |
| `r2-green.txt` | a clean clone at `659265c0` | 32 passed, exit 0 |
| `r2-mutations.txt` | the swap at `e4d27c78`, and six mutations at `659265c0` | the swap passes the old tests; each of the six fails |
| `env -i PATH HOME TMPDIR make test` | a clean clone at `659265c0` | reachable: 93 tests, 92 pass, 0 fail, 1 skipped; Python: 3002 passed, 132 skipped, 0 failed; exit 0 |
| CI's gc step (as in round 1) | a clean clone at `659265c0` | 103 passed, exit 0 |
