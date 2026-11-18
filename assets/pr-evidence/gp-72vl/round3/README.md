# Gate model migration verification, round 3

The round starts at `4bec6afe538518d6a5eb521c89a31bb1eb7327eb`. The verified implementation head is `a24a5ed170b6c995bd5f9f8dae535e1a0515f929`. The following commit adds the verification record.

| Ruling | Change | Commit |
| --- | --- | --- |
| R2, R3, R4 | Compare the worker pin with the helper, ignore shell comments in command examples and compare complete CLI versions | `1f849cbcbd2805c1057f5edda493e8f8d0cc0451` |
| R1 | Run the documented command against a fake CLI with a seeded verdict and transcript | `6f8347647adb29429959c1eb30c2a3d05ea7ee96` |
| R1 | Give the stub invocation a fresh scratch output and set `PATH` for that command | `931cffff4fdb9997c716131becdb6fc097aa4485` |
| R2, R5 | State the override rule and check the template and freshly rendered review command | `cdbaa58a4b939aa36b6c883a7fcf21c40d0797c7` |
| Self-review | Require a second invocation in the same scratch directory to produce a distinct answer | `a24a5ed170b6c995bd5f9f8dae535e1a0515f929` |

## Executable examples

The first three scratch changes to the template model, the commented exec flag and a shim prerelease each failed their new assertion before the pin-test commit. The restored test file passed all 22 tests (exit 0). The example parser joins backslash-newline continuations and uses `shlex.split(..., comments=True)`. An unquoted `#` in the middle of a word also starts a comment in `shlex`. Bash keeps it there. The documented examples and template command contain no such word.

From the checkout root, the three named tests ran with

```sh
env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest 'gascity/tests/test_codex_gate.py::test_doc_stub_run_writes_only_a_fresh_output' 'gascity/tests/test_codex_gate.py::test_worker_prompt_gate_command_follows_the_gate_default' 'gascity/tests/test_codex_gate.py::test_doc_examples_and_cli_pins_follow_the_gate_default' -q
```

| Revision | Result | Exit |
| --- | --- | ---: |
| R1 test commit | The recipe had no bash fence. The stub test failed at the fence assertion and both pin tests passed. [Red transcript](red.txt) | 1 |
| R1 documentation commit | All 3 passed. [Green transcript](green.txt) | 0 |
| Verified implementation head | All 3 passed, including two stub invocations with separate answers. [Green transcript](green.txt) | 0 |

## Deliberate breaks

The archive used the implementation head, with each input restored before the next case. For comparisons, the complete round-two gate test file ran against the same modified input. Commands, assertion messages, counts and exits are in the [break transcripts](breaks.txt).

| # | Input changed | Round 3 (exit 1) | Round 2 |
| ---: | --- | --- | --- |
| 1 | Reuse the seeded review output | Detected replacement of the original verdict | No stub-run test |
| 2 | Export the stub directory on `PATH` | Detected the changed `PATH` after the command | No stub-run test |
| 3 | Drop the stub directory from the command | Refusing fake CLI exited 97 | No stub-run test |
| 4 | Change only the worker template model | Detected disagreement with the helper default | 21 passed, exit 0 |
| 5 | Change only the helper default | Detected disagreement with the worker command | 3 failed, 18 passed, exit 1 |
| 6 | Comment out the exec example's `--model` | Detected the missing flag | 21 passed, exit 0 |
| 7 | Add `-alpha.1` to one shim pin | Detected disagreement with the README | 21 passed, exit 0 |
| 8 | Add `-alpha.1` to the README pin | Detected disagreement with helper usage | 21 passed, exit 0 |

## Verification

The toolchain directory was first on `PATH`. `GC_TEST_BIN` pointed to the installed `gc` for the full suite. `<clone>` denotes the checkout root.

| Command | Observed result | Exit |
| --- | --- | ---: |
| `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gascity/tests/test_codex_gate.py gascity/tests/test_codex_infisical_shim.py -q` | 69 passed | 0 |
| `make -C <clone> test-gascity` | 1,073 passed, 11 skipped, 10,518 subtests passed | 0 |
| `git grep -n 'gpt-6-astra' HEAD -- . ':!assets/pr-evidence'` | Only `gascity/tests/test_codex_infisical_shim.py` lines 264 and 265 | 0 |
| `git grep -n '0\.153\.3' HEAD -- . ':!assets/pr-evidence'` | 0 matches | 1 |
| `git grep -n 'gpt-7-astra' HEAD -- . ':!assets/pr-evidence'` | 0 matches | 1 |
| `git grep -nF 'PATH="${STUB_DIR:?}:$PATH" codex-gate.sh exec' HEAD -- docs/ops/citadel-codex-gates.md` | Exactly one command at line 123 | 0 |
| `git grep -n -e 'any other layer' -e 'gc-role-worker.template.md' -e 'gc prime' HEAD -- docs/ops/citadel-codex-gates.md` | Lines 112, 116 and 117 | 0 |
| `git diff --exit-code 4bec6afe538518d6a5eb521c89a31bb1eb7327eb HEAD -- gascity/assets gascity/template-fragments gascity/README.md tests` | Byte-identical paths | 0 |
| `git diff -U0 4bec6afe538518d6a5eb521c89a31bb1eb7327eb HEAD -- docs/ops/citadel-codex-gates.md` | Only original lines 112, 115 and 118 changed. The migration section occupies 19 lines. | 0 |
| `git diff --stat 4bec6afe538518d6a5eb521c89a31bb1eb7327eb HEAD` | Implementation was 2 files, 85 insertions, 18 deletions. Including evidence, 6 files changed, 295 insertions and 18 deletions. | 0 |
| `git diff --check 4bec6afe538518d6a5eb521c89a31bb1eb7327eb HEAD` | Clean | 0 |

The post-push receipt records the head, draft state, mergeability and hosted Check result from `gh pr view 114 --repo aphexcx/gascity-packs-prs --json headRefOid,isDraft,mergeable,statusCheckRollup`. [PR 114 checks](https://github.com/aphexcx/gascity-packs-prs/pull/114/checks)

## STANDARD self-review

Reviewed the round's diff, eight scratch cases and proposed PR text. The helper truncates the output at line 82 and opens the log for truncation at line 86. The fallback command allocates the output with `mktemp` and uses `${STUB_DIR:?}`, which makes Bash reject an unset or empty stub directory before starting the helper. Its inline assignment scopes the stub search path to that invocation. The original review and exec examples retain their explicit model.

| Blocker | Major | Minor | Nit | Finding and outcome |
| ---: | ---: | ---: | ---: | --- |
| 0 | 0 | 1 | 0 | At `gascity/tests/test_codex_gate.py:95` of `cdbaa58a`, a fixed filename in `TMPDIR` passed the single-run check (1 passed, exit 0). Fixed at `a24a5ed1`. The same input now fails on the second run (1 failed, exit 1). The documented command passes both invocations. |
