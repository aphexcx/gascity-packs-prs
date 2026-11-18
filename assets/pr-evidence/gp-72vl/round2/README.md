# Round 2 gate model verification

The round starts at `375073d7e8dbba245de7b41252cacd6ce97c935c`. The verified implementation head is `0c50c775afbf1ee5998cb6c8927fb898c629d3e1`. The following commit adds this evidence.

| Change | Commit |
| --- | --- |
| R3 regression test | `e965b1cf7df128cc274f6af8eff839ca10d3685f` |
| R1 explicit model in both examples and four-step migration recipe | `7ff00eb888c8320041556997613047f540073120` |
| R2 catalog placement and shared-home effects | `045071a482727447979f8c50d12277ebd2b26d01` |
| R3 CLI warning and conditional helper and provider defaults | `525649caed0a1de3bfd125be85e31add137695c5` |
| Self-review fix to limit the usage assertion's failure output | `0c50c775afbf1ee5998cb6c8927fb898c629d3e1` |

The warning matches the format string at line 1416 with the model ID substituted. [Codex CLI rust-v0.167.0 source](https://github.com/openai/codex/blob/rust-v0.167.0/codex-rs/core/src/session/turn_context.rs#L1416)

## Regression test

From the clone root, or the root of the archived scratch tree for each break, the command was

```sh
env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest 'gascity/tests/test_codex_gate.py::test_doc_examples_and_cli_pins_follow_the_gate_default' -q
```

| Tree | Result | Exit | Transcript |
| --- | --- | ---: | --- |
| Test commit | 1 failed on the review example's missing `--model` | 1 | [Red](red.txt) |
| R1 commit and final implementation head | 1 passed at each | 0 | [Green](green.txt) |
| Final implementation archive, first shim comment reverted to CLI 0.153.3 | 1 failed on the two shim pins | 1 | [Breaks](breaks.txt) |
| Same archive, restored, helper usage changed to CLI 0.153.3 | 1 failed on the usage pin | 1 | [Breaks](breaks.txt) |
| Same archive, restored, `--model` and its value removed from the exec example | 1 failed on the exec example | 1 | [Breaks](breaks.txt) |

## Verification

The city toolchain was first on `PATH`. The full suite had `GC_TEST_BIN="$(command -v gc)"` exported so the real-CLI integration tests could run. Paths in the commands below use `<clone>` for the checkout.

| Command | Result | Exit |
| --- | --- | ---: |
| `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gascity/tests/test_codex_gate.py gascity/tests/test_codex_infisical_shim.py -q` | 67 passed, 0 skipped | 0 |
| `make -C <clone> test-gascity` | 1,071 passed, 11 skipped, 10,518 subtests passed | 0 |
| `git -C <clone> grep -n 'gpt-6-astra' HEAD -- . ':!assets/pr-evidence'` | 2 matches in the existing shim argv test, lines 264–265 | 0 |
| `git -C <clone> grep -n '0\.153\.3' HEAD -- . ':!assets/pr-evidence'` | 0 matches | 1 |
| `git -C <clone> grep -nF 'Defaulting to fallback metadata; this can degrade performance and cause issues.' HEAD -- docs/ops/citadel-codex-gates.md` | 1 line, including the backticks around the model ID | 0 |
| `git -C <clone> grep -n -e 'top-level' -e 'city.config.toml' HEAD -- docs/ops/citadel-codex-gates.md` | 3 lines, including the city-only top-level key | 0 |
| `git -C <clone> diff --stat 375073d7e8dbba245de7b41252cacd6ce97c935c HEAD` | Implementation stat below | 0 |
| `git -C <clone> diff 375073d7e8dbba245de7b41252cacd6ce97c935c HEAD -- docs/ops/citadel-codex-gates.md` | Original lines 50–52, 64–65, 78–79 and 116 edited, recipe inserted before the next heading | 0 |
| `git -C <clone> diff --check 375073d7e8dbba245de7b41252cacd6ce97c935c HEAD` | 0 whitespace errors | 0 |

```text
 docs/ops/citadel-codex-gates.md  | 29 +++++++++++++++++++++--------
 gascity/tests/test_codex_gate.py | 36 ++++++++++++++++++++++++++++++++++++
 2 files changed, 57 insertions(+), 8 deletions(-)
```

Including this README and the three transcripts, the round changes six files with 272 insertions and eight deletions. The post-push readback uses `GH_CONFIG_DIR=<gh-config> gh pr view 114 --repo aphexcx/gascity-packs-prs --json headRefOid,isDraft,mergeable,statusCheckRollup`. The READY receipt records its exit code, published SHA, draft state, mergeability and Check result. [PR 114 checks](https://github.com/aphexcx/gascity-packs-prs/pull/114/checks)

## STANDARD self-review

Reviewed the round's diff, three breaks and draft PR text. The four migration steps cover the city profile, the installed helper and callers, the provider's launch and resume flags, and verification of the actual arguments. The catalog guidance matches the profile measurements and the quoted warning matches the tagged CLI source.

| Blocker | Major | Minor | Nit | Outcome |
| ---: | ---: | ---: | ---: | --- |
| 0 | 0 | 1 | 0 | Fixed. At `gascity/tests/test_codex_gate.py:41`, the usage regex also matched the script header, which pytest included in the deliberate usage-pin failure. Commit `0c50c775afbf1ee5998cb6c8927fb898c629d3e1` starts the match at the heredoc. |
