# Gate model migration verification, round 4

The base is `02825004b40a72453b69bc3c0490225eda56a48a`. The verified implementation is `475c6a10f389f83362a381fd7a2eba56bc714529`. The next commit adds the evidence. Its SHA and parsed trailers are in the handoff.

| Change | Commit |
| --- | --- |
| Add the probe tests (R1–R3, red) | `b54ab2c7a3b4a82b0553d75d3b25411beebdc9e9` |
| Run each caller against the checked stub (R1–R3, green) | `24ad1cfc73c3f341f2edba82341c00222338e869` |
| Add the hash and comment reader tests (R4, red) | `d7450532b6e2f2695aaec37bea682c56f9152cbc` |
| Record Bash argument vectors (R4, green) | `854402dbfa6eed924cda239dd2d32bdb7c35026d` |
| Check the stale report before the review prompt (R1) | `734effbebe4b39abca7e6227958ab4d7951dd53f` |
| Keep the recipe edit in one hunk (R2 and R6) | `475c6a10f389f83362a381fd7a2eba56bc714529` |

## Results by ruling

| Ruling | Red or rejection control | Verified result |
| --- | --- | --- |
| R1, caller identity and output | The previous recipe gave no worker report. Breaks 1–3 hid the stale model, replaced a real verdict, and leaked the scoped PATH, respectively. | The default and worker each reported OK for the current model. Only the worker reported STALE when given `previous-gate-model`, and the shell exited 1. Each named capture matched its report, and each call wrote a separate clean verdict. |
| R2, executable lookup | The old recipe called a fallback for an empty directory and a mode-0644 file, and called an exported function in the third case. Breaks 4 and 5 isolate the guards. | Each refusal produced one diagnostic, preserved the seeded record and log, created no scratch files and made zero stand-in calls. |
| R3, record path | The old recipe named zero records with CAPTURE unset. Break 6 reproduced the missing record. | The recipe set CAPTURE, printed its scratch path and recorded the default model. |
| R4, Bash arguments | The old reader accepted a literal `#old` suffix. Its new self-test failed with `DID NOT RAISE`. | Bash kept that suffix and discarded a separate comment. Both pin tests rejected the malformed arguments. See breaks 7 and 8. |
| R5, parsed trailers | All six round-three commits parsed only the co-author. | Each implementation commit parsed both the bead and the co-author. The evidence commit was checked after creation. |
| R6, edit scope | The first documentation edit produced three zero-context hunks. | One hunk, confined to original lines 120–126. The helper, shim, template, README and integration tests compare byte-for-byte with the base. |
| R7, public text | A synthetic private-path input triggered the publication scan. | Added lines, evidence, new messages and added PR text passed the scan and a manual read. Local paths in receipts are normalized. |
| R8, caller overrides | Both test versions rejected a stale review model and a missing exec model (breaks 9 and 10). | The original example commands and model explanation are byte-identical to the base. The example-pin test passed at the verified head. |

The exact pytest invocations, counts and exits are in [red.txt](red.txt) and [green.txt](green.txt). The probe tests failed five cases at the first commit and passed five at the second. The reader-test commit failed one case and passed three. All nine focused cases passed at the reader commit and at the verified implementation.

## Mutation runs

| # | Mutation | Round 4 | Previous test |
| ---: | --- | --- | --- |
| 1 | Fixed exec command ignores the worker pin | 1 failed, exit 1. Required STALE for the worker, observed OK for the default. | No corresponding probe |
| 2 | Write to the caller’s real output | 1 failed, exit 1. Seeded verdict was replaced. | No corresponding probe |
| 3 | Use braces for the probe body | 1 failed, exit 1. PATH differed after the function returned. | No corresponding probe |
| 4 | Remove the refusal guard | 3 failed, exit 1. Two calls to the fallback and one to the function. | No corresponding probe |
| 5 | Remove the resolved-file comparison | 1 failed, 2 passed, exit 1. Only the shadowing function received a call. | No corresponding probe |
| 6 | Remove the CAPTURE assignment | 1 failed, exit 1. Zero named records. | No corresponding probe |
| 7 | Append #old to the exec example model | 1 failed, exit 1. Exec example passed the wrong model argument. | 1 passed, exit 0 |
| 8 | Append #old to the worker model | 1 failed, exit 1. Worker passed the wrong model argument. | 1 passed, exit 0 |
| 9 | Use the previous model in the review example | 1 failed, exit 1. Review example passed the old ID. | 1 failed, exit 1 |
| 10 | Drop --model from the exec example | 1 failed, exit 1. Exec example supplied no model. | 1 failed, exit 1 |

Bash skips a missing or non-executable file during lookup. Keeping the identity comparison would therefore still reject these cases if only the explicit file and execute tests were removed. Break 4 removes the complete refusal guard. Break 5 removes only the resolved-file comparison. Each invocation, lookup and result is in [breaks.txt](breaks.txt).

## Verification

The toolchain directory was first on PATH. For the full suite, `GC_TEST_BIN` pointed to the installed `gc`. The gate tests used recording executables. Their environment was constructed from scratch, with the installed-helper path under the test’s own temporary directory.

| Command | Result | Exit |
| --- | --- | ---: |
| `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gascity/tests/test_codex_gate.py gascity/tests/test_codex_infisical_shim.py -q` | 75 passed | 0 |
| `make -C <clone> test-gascity` | 1,079 passed, 11 skipped, 10,518 subtests passed | 0 |
| `git grep -n gpt-6-astra HEAD -- . ':!assets/pr-evidence'` | Only the shim test at lines 264 and 265 | 0 |
| `git grep -n '0\.153\.3' HEAD -- . ':!assets/pr-evidence'` | 0 matches | 1 |
| `git grep -n gpt-7-astra HEAD -- . ':!assets/pr-evidence'` | 0 matches | 1 |
| `git grep -n previous-gate-model HEAD -- . ':!assets/pr-evidence'` | Only the gate test file | 0 |
| `git grep -n gate_probe HEAD -- docs/ops/citadel-codex-gates.md` | 6 matches, all inside the migration section | 0 |
| `git grep -nF 'PATH="${STUB_DIR:?}:$PATH" codex-gate.sh exec' HEAD -- docs/ops/citadel-codex-gates.md` | 0 matches | 1 |
| `git diff --stat 02825004b40a72453b69bc3c0490225eda56a48a HEAD` | Implementation only: 2 files, 169 insertions and 52 deletions. With evidence: 7 files, 453 insertions and 52 deletions. | 0 |
| `git diff --exit-code 02825004b40a72453b69bc3c0490225eda56a48a HEAD -- gascity/assets gascity/template-fragments gascity/README.md tests` | All protected files byte-identical | 0 |
| `git diff -U0 02825004b40a72453b69bc3c0490225eda56a48a HEAD -- docs/ops/citadel-codex-gates.md` | One hunk at `-120,7 +120,22`. The migration section occupies 34 lines. | 0 |
| `git diff --check 02825004b40a72453b69bc3c0490225eda56a48a HEAD` | Clean | 0 |

The previous and current parse outputs are in [trailers.txt](trailers.txt). The verification commit adds only the five files in this directory. The hosted checks run on the published SHA. [PR checks](https://github.com/aphexcx/gascity-packs-prs/pull/114/checks)

## STANDARD self-review

Reviewed this round’s diff, all ten mutations and the proposed PR text. The helper truncates the output at line 82 and redirects its CLI transcript at line 86. The probe’s subshell confines PATH and CAPTURE. Its file-type, executable and lookup checks precede scratch creation and the helper call. The two command readers route both the PATH helper and the rendered worker path through scratch, with a second recording CLI available for unexpected calls.

- **MINOR, SHOWN and fixed.** SHOWN at `gascity/tests/test_codex_gate.py:171` of `854402db`. With the fixed-command mutation, the review-prompt assertion failed on the first caller pair. The stale pair had not run. At `734effbe` the stale report fails first for that mutation. Its expected and observed IDs appear in `breaks.txt`, case 1.
- **NIT, SHOWN and fixed.** SHOWN at `docs/ops/citadel-codex-gates.md:120` of `854402db`. `git diff -U0` produced three hunk headers within the permitted range. At `475c6a10` it produces one. The resolved-file comparison is independently exercised by break 5. A CommonMark render returned one Bash fence with the exact contents executed by the tests.

Counts were 0 blocker, 0 major, 1 minor and 1 nit. Both findings were fixed and verified.
