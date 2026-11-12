# gp-xt33: the public-repository clean-up of packs main

This repository is public, and a pack's files ship to every city that imports it. Files merged
before the repository's public-content rule named a customer, the customer's city and its mayor, a
hosting partner and its platform, a competitor's product, one real email address and one private
host name. This change rewords them. It changes documents and evidence only: no code, and no history
(the older text stays reachable in the repository's history).

## Files changed, by directory

| Directory | Files | What changed |
| --- | ---: | --- |
| `assets/pr-evidence/gp-ecsi/` | 3 | a city's mayor, named, becomes "the customer city's mayor" |
| `assets/pr-evidence/gp-w4kz/` | 2 | a customer, its city, its mayor, a real email address, a hosting partner and its platform, each reworded; `pr-before.json` still parses |
| `assets/pr-evidence/gp-zbq3/` | 4 | a competitor's domain in captured data becomes `competitor.example`; the JSON still parses and the README's host table still matches it |
| `assets/pr-evidence/gp-zbq3/round-2/` | 3 | the same, in three replay logs |
| `docs/plans/` | 1 | a private tailnet host name becomes an invented one |
| `whatsapp/gateway/` | 1 | the README names a hosting partner by its role |
| `whatsapp/` | 1 | `CHANGELOG.md`: one docs-only line |
| `docs/design/` | 1 | `whatsapp-adapter-20261103.md` rewritten, at the same path, as a short technical design of record (281 lines and 163,501 bytes before; 299 lines after), checked against the pack's code at the base |

Nothing else in a reworded file changed: no number, sha, finding or sentence that names nobody.

## The proof, as counts

The names were searched with `git grep -n -i -E -f LIST` over the whole tree, where LIST is a
private list of 17 patterns (names, an email shape and a phone shape) kept off this repository.

| Search | At the base `28918336` | At the head |
| --- | ---: | ---: |
| Hits in the tree (lines / files) | 252 / 91 | 186 / 78 |
| Hits that are a real name, address or id (lines / files) | 68 / 15 | 0 |
| Hits in the delta's added lines | | 2 |
| Hits in the commit messages | | 1 per commit |

Every hit at the head is not a name: invented test addresses at example.com, example.test and
example.invalid and other test fixtures, an ordinary English word in vendored and copied skill texts,
GitHub's SSH remote address, tool noreply addresses, and a Google service account's documented
address. The two
hits in the added lines are the invented example.test address the rewording uses; the hits in the
commit messages are the Co-Authored-By noreply address.

## Tests

`make test` at `d2f866ff`, run under a clean environment (`env -i PATH HOME TMPDIR`): 2691 passed,
126 skipped, 10689 subtests passed, rc 0. The commit after it changes one sentence of the design
document and adds this README; no test reads either. An earlier run at `9269c0c8` had one failure,
`test_toolchain_shims.py::PnpmShimTests::test_concurrent_callers_wait_for_one_install_instead_of_racing_it`
(one caller of three waited where two were expected, under the full suite's load). The change touches
nothing under `gascity/`, and the test passed alone 3 of 3 at the head.

`make test PYTEST_DIRS=whatsapp/tests` (the pack's own suite, which CI runs and `make test` does
not): 57 passed, rc 0.
