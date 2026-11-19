# Review loop verification

The base was `7876f9768281dc85036160c9dddd71173401b5e3`. Checks used the
repository's `verify` function, Python 3.12.13 and Node 24.21.0.

| Check | Revision | Result | Exit |
| --- | --- | --- | --- |
| `verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` | Base | 1062 passed, 128 skipped, 10518 subtests passed | 0 |
| `verify -C gascity python3 -m pytest tests/test_formula_assets.py -q -p no:cacheprovider` | Base with the new assertions | 14 failed, 135 passed, 6635 subtests passed | 1 |
| Same asset suite | First revision | 137 passed, 6850 subtests passed | 0 |
| Same Gas City Make target | First revision | 1064 passed, 128 skipped, 10530 subtests passed | 0 |
| `verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'` | First revision | 91 passed, 1 skipped | 0 |
| `verify -C gascity python3 -m pytest tests -q -k full_build_review_controllers -p no:cacheprovider` | First revision with the controller assertions | 41 failed, 1 passed, 538 deselected, 9 subtests passed | 1 |
| Same asset suite | Full-build revision | 138 passed, 6905 subtests passed | 0 |
| Same Gas City Make target | Full-build revision | 1065 passed, 128 skipped, 10585 subtests passed | 0 |
| Same Reachable command | Full-build revision | 91 passed, 1 skipped | 0 |

The red run caught all three declarations that defaulted to ten, the inherited
continuation value, the prepare example, the starter check at six, and the
missing family-stop instructions in repair and re-review. The second red run
followed the resolved review step into each concrete expansion. The full builds
still had fixed caps of 3, 6, 8, 8 and 6, and their apply steps lacked the
family stop. On the second revision, all five checks allow three attempts
and each apply step compares the reports before applying fixes.

In a scratch pack, `gc formula show cap-probe --var max_iterations=5 --json`
accepted a check with `max_attempts = 3` (exit 0). Replacing just that value
with `"{{max_iterations}}"` failed during formula loading (exit 1) with
`cannot unmarshal string into Go struct field RalphSpec.max_attempts of type int`.
Compiling each of the five real builds with `gc formula show <build> --var
max_iterations=5 --json` succeeded. Every compiled code-review controller
recorded `gc.max_attempts=3`. A launch override still changes the continuation
repair and fix-loop limit. The build-from-review continuation's finalize step
admits only `approved` and `not_needed` as passing repair statuses.

## Terminal outcomes at `3452da2c`

At `10bdd0b4`, the two class tests reported 19 failing checks. The restart
graph omitted gstack QA and release readiness and Compound's resolution
expansion. All five full builds lacked an exhaustion record, and neither
fix-loop caller checked the child workflow's terminal metadata.

| Check | Revised result | Exit |
| --- | --- | --- |
| `verify -C gascity python3 -m pytest tests -q -k 'stage_complete_restart or exchange_the_family_stop' -p no:cacheprovider` | 2 passed, 68 subtests passed, 537 deselected | 0 |
| Same formula asset suite | 138 passed, 6918 subtests passed | 0 |
| Same Gas City Make target | 1065 passed, 128 skipped, 10598 subtests passed | 0 |
| Same Reachable command | 91 passed, 1 skipped | 0 |

At that revision the restart test followed transitive dependencies from review
in each resolved build and compared stage IDs and expansions with the recorded
restart. It also checked the standalone entrypoints that share four review
expansions. The caller test checked the producing step and both dispatching
prompts. In a scratch city, `gc formula show` also resolved the terminal
instructions for all five builds with `max_iterations=5` and all four
standalone reviews with `review_mode=agent`, including external prompt files.
These are graph and prompt checks. Live agent execution was not run.

## Terminal handoff with no automatic restart

On the prior revision the compiled prompts sent five builds back through
implementation and four standalone reviews back through review. The final
class tests found 31 failing subtests against that revision's assets.
The same two tests passed with 135 passing subtests on the revised assets using
`verify -C gascity python3 -m pytest tests -q -k terminal_review_handoff -p no:cacheprovider`.

| Full build | Recorded entry | Standalone review | Recorded entry |
| --- | --- | --- | --- |
| `build-basic` | `manual` | | |
| `bmad-build` | `manual` | `bmad-review` | `manual` |
| `compound-build` | `manual` | `compound-review` | `manual` |
| `gstack-build` | `manual` | `gstack-review` | `manual` |
| `superpowers-build` | `manual` | `superpowers-review` | `manual` |

The graph test compares the full runnable graph of each candidate with
the stages downstream of the originating review, including dependency
edges, expansion names and stage implementations. None of the nine has
an exact continuation. Substituting `build-basic` in the BMAD prompt
failed the assertion for both BMAD entries. All nine revised prompts
also compiled in a scratch city. The separate `build-from-review`
continuation records its own entrypoint for a person's decision.

For either terminal status the review root records the reason and the
path of one handoff. The GitHub adapter, review continuation and
fix-loop re-review check the producing root before spending another
attempt. The starter releases a branch it took before a failing close.

The Gas City Make target passed 1,065 tests with 128 skipped. Reachable
passed 91 with its configured skip. A standalone formula asset run passed
138 tests. A repeated asset run hit the two-second deadline in the unchanged
claim retry test while another suite ran. The graph and prompt checks do
not exercise a live provider-driven stop.
