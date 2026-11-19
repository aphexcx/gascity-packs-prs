# Review loop verification

The base was `7876f9768281dc85036160c9dddd71173401b5e3`. Checks used the
repository's `verify` function, Python 3.12.13 and Node 24.21.0.

| Check | Revision | Result | Exit |
| --- | --- | --- | --- |
| `verify make test-gascity 'PYTEST_ARGS=-q -p no:cacheprovider'` | Base | 1062 passed, 128 skipped, 10518 subtests passed | 0 |
| `verify -C gascity python3 -m pytest tests/test_formula_assets.py -q -p no:cacheprovider` | Base with the new assertions | 14 failed, 135 passed, 6635 subtests passed | 1 |
| Same asset suite | Updated formulas and workflows | 137 passed, 6850 subtests passed | 0 |
| Same Gas City Make target | Updated formulas and workflows | 1064 passed, 128 skipped, 10530 subtests passed | 0 |
| `verify -C reachable/test node --test --test-skip-pattern '^review 4: archive replay advances in bounded chunks without dropping rows$'` | Updated formulas and workflows | 91 passed, 1 skipped | 0 |

The red run caught all three declarations that defaulted to ten, the inherited
continuation value, the prepare example, the starter check at six, and the
missing family-stop instructions in repair and re-review. The formula diff
changes four scalar values. BMAD, Compound Engineering, gstack and Superpowers
inherit the base limit. Finalize already admits only `approved` and
`not_needed` as passing repair statuses.
