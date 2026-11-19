# GPT-6.2 Astra gate verification

Base `920830934bf7d60e3ac15e5691e5f52d17db06c6`. Implementation head `768b0f175664473e68f18afc3a30f1709047c852`.

## Regression checks

Commands ran from the clone root with the project's toolchain first on PATH. For each row the command first ran with the new assertion against the original implementation, then again after the implementation edit.

| Command | RED (exit 1) | GREEN (exit 0) |
| --- | --- | --- |
| `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gascity/tests/test_codex_gate.py -k test_city_astra_invocation_and_stdin -q` | 2 failed at the model argument, 18 deselected | 2 passed, 18 deselected |
| `GC_TEST_BIN="$(command -v gc)" env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests/test_gc_role_prompt_integration.py -k test_role_prompts_render_public_worker_fragment -q` | 8 failed at the rendered model flag, 86 deselected | 8 passed, 86 deselected |
| `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gascity/tests/test_codex_infisical_shim.py -k test_readme_install_recipe_works_from_a_fresh_directory -q` | 1 failed at the installed pin, 45 deselected | 1 passed, 45 deselected |

## Rendered worker prompt

A scratch copy of the public-worker render test saved the real `gc prime --strict` output for the `gc` binding. All eight cases passed at both revisions. The two captures used the same command with the scratch test path, `GC_TEST_BIN` set as above and the capture destination in `PROMPT_CAPTURE`.

The [base passage](before.txt) and [head passage](after.txt) differ only in the two model IDs. [Unified diff](render.diff)

```diff
--- before.txt
+++ after.txt
@@ -3 +3 @@
-  --model gpt-6-astra
+  --model gpt-6.2-astra
@@ -6 +6 @@
-The helper invokes `codex -p city -m gpt-6-astra exec --skip-git-repo-check`
+The helper invokes `codex -p city -m gpt-6.2-astra exec --skip-git-repo-check`
```

## Head verification

| Command | Result | Exit |
| --- | --- | ---: |
| `env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest gascity/tests/test_codex_gate.py gascity/tests/test_codex_infisical_shim.py -q` | 66 passed, 0 skipped | 0 |
| `GC_TEST_BIN="$(command -v gc)" env -u GC_TEMPLATE uv run --with pytest --with pyyaml --with jsonschema python3 -m pytest tests/test_gc_role_prompt_integration.py -q` | 94 passed, 0 skipped | 0 |
| `make test-gascity` (with `GC_TEST_BIN` exported as above) | 1,070 passed, 11 skipped, 10,518 subtests passed | 0 |
| `git grep -n 'gpt-6-astra' HEAD -- docs/ops/citadel-codex-gates.md` | 0 matches | 1 |
| `git grep -n 'gpt-6-astra' HEAD -- . ':!assets/pr-evidence'` | 2 matches, both in the shim argv-passthrough test | 0 |
| `git grep -n '0\.153\.3' HEAD -- . ':!assets/pr-evidence'` | 0 matches | 1 |

OpenAI's catalog at commit `6c0cee502` lists `minimal_client_version` as `0.166.0` for this model. [Catalog entry source](https://github.com/openai/codex/blob/6c0cee502/codex-rs/models-manager/models.json)

## Scope

`git diff --numstat 920830934bf7d60e3ac15e5691e5f52d17db06c6 768b0f175664473e68f18afc3a30f1709047c852` produced the following implementation stat. The evidence commit adds this README, the two passages and their diff.

```text
17   8  docs/ops/citadel-codex-gates.md
 1   1  gascity/README.md
 3   2  gascity/assets/scripts/codex-gate.sh
 2   2  gascity/assets/scripts/codex-infisical-shim.sh
 2   2  gascity/template-fragments/gc-role-worker.template.md
 2   2  gascity/tests/test_codex_gate.py
 1   1  gascity/tests/test_codex_infisical_shim.py
 3   1  tests/test_gc_role_prompt_integration.py
```

The provider, agent and shim path retain the name `codex-astra`. The shim script delta consists of two comment lines.

## Worker reviews

| STANDARD review at the implementation head | Blocker | Major | Minor | Nit | Outcome |
| --- | ---: | ---: | ---: | ---: | --- |
| Self-review of the changes, evidence and draft PR body | 0 | 0 | 1 | 0 | Fixed the PR description's evidence link (line 47). Its relative URL resolved under `/pull/assets/`. The published link points to the evidence file on GitHub. |
| `codex review --base origin/main -c 'sandbox_workspace_write.network_access=true'` | 0 | 0 | 0 | 0 | Exit 0. The reviewer reported consistent gate and prompt model IDs, a compatible CLI pin and 74 passing targeted tests. |
