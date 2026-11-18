# gp-hdn2 evidence: THE PR BODY LINES

The second note in `gascity/template-fragments/gc-standing-notes.template.md`,
its README sentence, and its tests. Base: main at `148e4a0f`. Code head:
`e65b23bf` (the commit after it adds only this directory).

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
locals dump are scrubbed to `<TMPDIR>` and `<GC_TEST_BIN>`.

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
  `e65b23bf`. It holds what follows the role text, both notes.
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
