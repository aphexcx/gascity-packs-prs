# Review rules in the review loop steps: verification

The base was `e0b06b76d1d87493ae632651f5b5c24898c99ff2`. Checks used the
repository's `verify` function from `.claude/skills/verify/SKILL.md` at the
base, Python 3.12.13 and Node 24.21.0. Each run used a clean worktree, except
the red and green runs of single test rows, which ran in the lane at the
revision named.

## The asset test, red then green

The test is `test_review_loop_steps_state_the_review_rules` in
`gascity/tests/test_formula_assets.py`. It holds one row per sentence the
four step files must carry. It also checks that each step's own family-stop
exit paragraph records the value the family-stop sentence names, and that
the four files carry no date, ordinal or "this city".

| Check | Revision | Result | Exit |
| --- | --- | --- | --- |
| `verify -C gascity python3 -m pytest tests/test_formula_assets.py -q -p no:cacheprovider` | Base | 139 passed, 6996 subtests passed | 0 |
| Same, with the head's test file | Base | 26 failed (every sentence row), 140 passed, 7002 subtests passed | 1 |
| Same | `8d9af51a` (first test, base text) | 17 failed (every row of that test), 140 passed, 7000 subtests passed | 1 |
| Same | `f2f8a59c` | 140 passed, 7017 subtests passed | 0 |
| Same with `-k review_loop_steps_state_the_review_rules` | `9aa0b80f` (self-review rows) | 11 failed, 1 passed, 18 subtests passed | 1 |
| Same asset file | `02836db7` | 140 passed, 7025 subtests passed | 0 |
| Same with `-k review_loop_steps_state_the_review_rules` | `2522c20d` (codex round 1 rows) | 8 failed, 1 passed, 24 subtests passed | 1 |
| Same asset file | `4fa5f65e` | 140 passed, 7028 subtests passed | 0 |
| `verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` | `4fa5f65e` | 1084 passed, 128 skipped, 10735 subtests passed | 0 |

## The exit-line mutant

At `02836db7`, deleting `gc.build.repair_status=family_stop` from the repair
stage's family-stop exit line still passed the whole asset file: 140
passed, 7025 subtests passed, exit 0. The stage's new family-stop sentence
repeats the value in prose, so a substring check on the file still found
it. Run with the test file of `2522c20d`, the same mutant fails the per-step
exit check, which reads the value from the exit paragraph itself: the
`family-stop exit value` subtest of the repair stage failed (exit 1). The
run's 8 other failures were the codex round 1 rows, since the mutant kept
the text of `02836db7`.

## A checkout with ignored scratch copies

`verify make test-gascity` in a worktree whose ignored `.gc/` directory
held scratch clones of this repository failed
`test_the_superseded_pipeline_is_gone_from_every_asset`: the test reads
every file under the checkout, ignored ones included. The same target
passed in a clean worktree, as the table shows. The verify skill already
says to start from a clean checkout.
