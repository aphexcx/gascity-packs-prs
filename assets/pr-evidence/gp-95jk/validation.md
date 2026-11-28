# Worker provenance verification

The Part P base was `ebc58be18c1665509d7fac4c5a5b2149ff336ee2`. Tests invoke the pack command against a fixture Beads store and use real Git repositories for artifact checks.

## Base reproduction

With the new tests and the base recorder, the following command from `gascity/` exited 1. Pytest reported 10 failures, 55 passes, 508 deselections and 216 passing subtests.

```sh
env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest -q tests -k 'artifact or invalid_deliverable_combinations or shipped_code_still_requires_commit or noop_reviews_report_latest_round or ran_and_fork_outcome'
```

| Behavior | Observation on the base |
| --- | --- |
| Artifact arguments | The CLI rejected both flags during parsing, including valid submissions, so the successful write and five path/checksum diagnostics failed. |
| Git file eligibility | The CLI could neither distinguish an ignored file from a tracked file nor identify the repository target of a symlink. |
| Reviewed no-op | The summary printed `PR 123 at unknown, gate unknown, read unknown` after two PASS verdicts. |
| Vocabulary | The recorder's outcome choices lacked `ran`. |
| Existing shipped form | The missing-commit refusal already passed. |

## After the fixes

The following command from `gascity/` passed all 20 selected cases on the code at `d5f6b8303c7b5f61093bf35eae8a3499865e8d3f` (556 deselected).

```sh
env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest -q tests -k 'test_worker_provenance and (artifact or invalid_deliverable_combinations or shipped_code_still_requires_commit or noop_reviews or ran_and_fork_outcome)'
```

Tests check the typed facts, compatibility keys, idempotent retry, zero store calls on rejection, latest-round verdicts, SHA conflicts and attribution across PRs.
