# Gate model migration verification, round 5

| Revision | Commit |
| --- | --- |
| Round 4 base | `d6568926494d8445a743903813163e0614b6a54e` |
| Remove the runnable recipe and its tests | `c92f2a3ee8d43e7d9d621156fd8c8952f6557335` |
| Add the two regression cases | `f1953d483c366351b9f59822a954ff98a9753bba` |
| Fix the fixtures (reviewed implementation head) | `7a6aa98b9df1a3e886a9b0717f0b17592902d3b7` |

The runnable model check could report OK after its stand-in replaced a real BLOCK verdict with CLEAN. It also checked the executable in the operator’s shell, while a different PATH in the helper process could send the full prompt to a different executable. The guide now keeps step 4’s `--dry-run` check and directs readers of the repository helper to its `model=` default, caller overrides and step 2’s freshly rendered worker command. The default model and the Bash argument checks remain in place. The round3/ and round4/ directories preserve earlier evidence for the removed recipe.

## Test coverage

Removed `test_gate_probe_reports_the_model_each_caller_sends`, `test_gate_probe_refuses_an_unusable_stub` and `test_gate_probe_names_its_own_record`, along with their helpers `run_probe`, `assert_original_record` and `probe_records`.

| Retained test | What it checks |
| --- | --- |
| `test_worker_prompt_gate_command_follows_the_gate_default` | The worker passes the exact model once. |
| `test_doc_examples_and_cli_pins_follow_the_gate_default` | Both documented invocations pass the gate default. |
| `test_pin_reader_sees_what_bash_passes` | Suffixes, comments and the unset model variable cannot satisfy the pin. |
| `test_city_astra_invocation_and_stdin` | Exec and review use the default for both model arguments. |
| `test_model_override_pins_review_model_too` | An explicit model reaches both arguments. |
| `test_only_one_exact_anchored_final_verdict_passes`, `test_cli_failure_cannot_pass_or_reuse_stale_verdict`, `test_review_rejects_empty_delta_before_calling_codex`, `test_review_rejects_uncommitted_work_before_calling_codex` | Verdict parsing, stale results and review refusals. |

At the regression-test commit the pin test accepted `--model "$NEW_MODEL"` and failed with `DID NOT RAISE`. The new `test_gate_fixture_runs_no_caller_git_hook` found two post-commit hook entries in the caller’s scratch home. Removing the model from the pin fixture and isolating Git configuration for setup and the helper made both cases pass. The gate fixture overrides inherited hook settings, including those passed through `git -c`. Exact invocations, failure messages, counts and exits are in [red.txt](red.txt) and [green.txt](green.txt).

## Mutation results

The example and worker pin tests ran together in one scratch archive of the implementation head, with the corresponding file restored between edits. The indicated test failed and its sibling passed in every case (exit 1). [Commands and assertion output](breaks.txt)

| Break | Edit | Test that rejected it |
| --- | --- | --- |
| 7 | Append `#old` to the exec model | Example |
| 8 | Append `#old` to the worker model | Worker |
| 9 | Use the previous model in the review example | Example |
| 10 | Remove the exec model flag | Example |
| 11 | Set the worker model to `"$NEW_MODEL"` | Worker |

## Scope checks

Commands below ran from the clone. `BASE` is the full round 4 SHA above. The greps exclude historical evidence.

| Command | Output | Exit |
| --- | --- | ---: |
| `git grep -n -E 'gate_probe\|STUB_DIR\|run_probe\|probe_records\|assert_original_record\|previous-gate-model\|gate-probe\|gate-stub\|test_doc_stub_run' HEAD -- . ':!assets/pr-evidence'` | Zero matches | 1 |
| `git grep -n NEW_MODEL HEAD -- . ':!assets/pr-evidence'` | The new self-test case at gate test line 82 | 0 |
| `git grep -n 'Moving a city' HEAD -- gascity/tests` | Zero matches | 1 |
| `git show HEAD:docs/ops/citadel-codex-gates.md` piped through `sed -n '/^## Moving a city/,/^## Local/p'` and a count of lines with three backticks | 0 fenced lines, 14 section lines including the trailing blank | 1 (grep) |
| `git grep -n gpt-6-astra HEAD -- . ':!assets/pr-evidence'` | Only shim test lines 264 and 265 | 0 |
| `git grep -n '0\.153\.3' HEAD -- . ':!assets/pr-evidence'` and `git grep -n gpt-7-astra HEAD -- . ':!assets/pr-evidence'` | Zero matches each | 1 each |
| `git diff --stat "$BASE" HEAD` | Implementation head: two files, 43 insertions and 123 deletions. This evidence adds four files. | 0 |
| `git diff --exit-code "$BASE" HEAD -- . ':!docs/ops/citadel-codex-gates.md' ':!gascity/tests/test_codex_gate.py' ':!assets/pr-evidence/gp-72vl/round5'` | Empty | 0 |
| `git diff -U0 "$BASE" HEAD -- docs/ops/citadel-codex-gates.md` | One hunk, `@@ -120,22 +120,2 @@` | 0 |
| `git diff --check "$BASE" HEAD` | Empty | 0 |

The fence count used `grep -c` on the migration section as follows (output `0`, grep exit 1).

~~~sh
git show HEAD:docs/ops/citadel-codex-gates.md | sed -n '/^## Moving a city/,/^## Local/p' | grep -c '```'
~~~

The guide’s example lines and the six original gate tests compare byte-for-byte with the round 4 base. The script, shim, worker template, README, other tests and earlier evidence all pass the excluded-path diff.

## Commit trailers

`git log -1 --format=%B "$commit" | git interpret-trailers --parse` returned the following two lines for each of the three implementation commits listed above (exit 0 for each). The final evidence commit’s SHA and trailer parse are recorded in the handoff.

```text
Bead: gp-wnmz
Co-Authored-By: Codex <noreply@openai.com>
```

## Reviews

**STANDARD self-review of the implementation head.** 0 blockers, 0 majors, 0 minors and 0 nits. The doc statements trace to the helper’s default, override, argument construction and redirections at lines 28, 42, 52, 54, 82 and 86. The five mutations, the two red cases, the retained assertions, the protected-path diff and the proposed PR text were checked.

**STANDARD Codex review of the same head.** The helper returned `VERDICT: BLOCK` (exit 1) with 1 major and 1 minor. It ran no tests. The review used the round 4 base, class STANDARD and model `gpt-6.2-astra`.

- **MAJOR, NOT SHOWN in the review, deferred.** At `gascity/tests/test_codex_gate.py:133` the fixture inherits Git repository-selection variables, including `GIT_INDEX_FILE`. The reviewer described possible writes to a caller-selected index. The base also inherited these variables. The fixture change isolates configuration and hooks. Repository selection and the two existing `rev-parse` calls were outside this round.
- **MINOR, NOT SHOWN in the review, deferred.** At `gascity/tests/test_codex_gate.py:79` the surviving worker-model test records the review command and asserts its exact model. The removed end-to-end recipe test also exercised required non-model arguments. The reviewer proposed a missing `--base` case but supplied no run. The five pin mutations cover model arguments. Checking every required worker argument remains a separate coverage gap.

Both findings remain open for the independent reviewers. The evidence commit contains verification material only.
