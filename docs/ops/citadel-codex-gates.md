# Citadel Codex gates (gp-mj2q)

Afik approved the isolated profile and gate rollout on 2026-09-06
(message 1788737744.207359): “we should always use codex with astra”.
Use the city profile with `gpt-6.2-astra` and `xhigh` reasoning.
Existing default profile settings are preserved.

## Profile compatibility and measurements

GPT-6.2 Astra requires Codex CLI 0.166.0 or later. The profile was tested
with Codex CLI 0.167.0, which reads `city.config.toml` in the Codex home for `-p city`.
It rejects a same-name legacy `[profiles.city]` table in `config.toml`.
The requested legacy table was tested and removed; the base file is
byte-identical to its backup, `~/.codex/config.toml.bak-20260906-185000`.
See [OpenAI profile documentation](https://learn.chatgpt.com/docs/config-file/config-advanced#profiles)
and the installed CLI's `codex --help`.

The city overlay sets `model = "gpt-6.2-astra"` and
`model_reasoning_effort = "xhigh"`. It disables `features.plugins`,
`features.apps`, `features.recommended_plugins`, and `features.remote_plugin`,
and sets `enabled = false` for all eleven configured plugins and both configured
MCP servers (`node_repl` and `computer-use`). The system and user skills remain
available. Future explicitly configured MCP servers also need a disabled entry
in this overlay.

Measured in `/tmp` on 2026-09-06, before shortening ego-browser's description:

| Fixed prompt: `reply OK` | Default | City |
| --- | ---: | ---: |
| Preamble characters | 19,176 | 11,966 |
| Skills block characters | 7,210 | 3,554 |
| Plugin catalogue characters | 3,387 | 0 |
| Input tokens | 15,679 | 13,125 |
| Cached input tokens | 11,136 | 11,136 |

Both runs returned `OK`. Preamble size is the sum of model-visible text lengths
from `codex [-p city] -C /tmp debug prompt-input 'reply OK'`, excluding the last
user prompt, including the environment message. It excludes hidden model base
instructions and tool schemas. Tokens are the reported `turn.completed.usage`
from `codex [-p city] exec --skip-git-repo-check -C /tmp --json 'reply OK'` with
stdin redirected from `/dev/null`. The profile comparison also changes the model;
it is an observed before/after result, not a tokenizer-controlled benchmark.

Raw local evidence is in `~/city/assets/ops/gp-mj2q/`. `codex -p city mcp list`
confirmed both servers disabled. The rendered city prompt has no recommended
plugin catalogue or plugin skill entries.

## GPT-6.2 Astra until a Codex release bundles it

Set `model_catalog_json` to a local copy of OpenAI's `codex-rs/models-manager/models.json` from `openai/codex` commit `6c0cee502` or later.
Place it at the top level, above the file's first TOML table, because Codex silently ignores it under a table.
Use the city's `city.config.toml` for this top-level key so it applies only to `-p city`, or use `config.toml` in a Codex home dedicated to the city.
Keep the key out of a base `config.toml` shared with ordinary sessions.
Every session sharing that file would use the replacement catalog, changing instructions and metadata for existing models, and a missing catalog file would stop every command in that home.
Without the entry, the CLI warns ``Model metadata for `gpt-6.2-astra` not found. Defaulting to fallback metadata; this can degrade performance and cause issues.`` and uses generic metadata.
Add `gpt-6.2-astra` to the CLIProxyAPI hub's model catalog so it can route the ID. Otherwise it returns HTTP 400, “unknown provider for model gpt-6.2-astra”.
Remove these overrides once the Codex release bundles the metadata and the hub's upstream catalog lists the ID.

## Gate helper

Use the updated city helper (ci-cmju0n, 2026-10-23) at
`~/city/assets/ops/mayor-tools/codex-gate.sh`. The repository copy at
`gascity/assets/scripts/codex-gate.sh` lacks the options below; do not install
it over the city helper. Provision the city profile first.

```bash
codex-gate.sh review --base origin/main -C "$REPO" --output "$REVIEW_OUTPUT" --class "$REVIEW_CLASS" --model gpt-6.2-astra
codex-gate.sh exec "$PROMPT_FILE" -C "$REPO" --output "$REVIEW_OUTPUT" --model gpt-6.2-astra
```

Set `REVIEW_CLASS` to the bead's recorded class: QUICK, STANDARD, DEEP or SAFETY.
The class is never defaulted; a review without `--class` or `--bead` refuses
with rc 2. Alternatively, `--bead "$BEAD"` reads `gc.review_class` from the bead.
SAFETY returns CLEAN only with no finding at any severity, including MINORs,
as required by the repository's SAFETY review text. `--evidence-dir DIR` takes
an existing absolute directory for evidence outside the repository; this rig's
beads keep it under `/Users/tailor512/city/assets/ops/pr-evidence/<bead>/`.
`--raw` keeps Codex's bytes; the default strips trailing whitespace so a
committed `.out` passes `git diff --check`.

Both modes use `-p city -m MODEL`, read-only execution, `-C`, and `--skip-git-repo-check`. The prompt and the contract reach Codex on stdin.
`MODEL` comes from `--model` when supplied. Otherwise the helper passes its own `model=` value, so an older installation keeps its previous ID until updated to `gpt-6.2-astra` (see “Moving a city to a new gate model”).
`--model MODEL` explicitly overrides both the normal model and the optional
review model. Review mode
resolves REF and HEAD to commit IDs, computes their merge base, and asks
`codex exec` for a review of that immutable delta at the selected class.
Native `codex review` formats its own findings and did not honor the requested
VERDICT line in a live test, so both helper modes use exec's separate
final-answer file.
The caller's output file receives that answer; `FILE.log` holds the transcript.
The helper accepts exactly one final standalone `VERDICT: CLEAN` or
`VERDICT: BLOCK` line. Exit codes are 0 for CLEAN, 1 for BLOCK, 2 for a missing
or invalid verdict, a refused delta or a usage error, 3 if Codex crashes or exits
non-zero without a provider error (or the helper is terminated), and 4 for a
provider error or refusal, with details in `FILE.refusal` beside `FILE`.
When the helper exits without an answer, `FILE` holds one `FAILED:` or `REFUSED:`
line behind the audit header. For a real run with `FILE` established,
`FILE.reason` explains the exit status and `FILE.rc` is written last on every
exit path, so a waiter never hangs. Before a real run, old side files are removed
and `FILE.rc` publication is armed; no stale CLEAN survives an exit.
Review mode rejects empty committed deltas and reviews a pristine detached
worktree at the selected head. It ignores untracked files and refuses
uncommitted changes to tracked files unless `--head` is given, when the caller's
working tree is not consulted. Commit changes before review and keep
prompt/output files outside the reviewed worktree.

Codex QUICK reviews supplement the required Fable adversarial review for
Codex-built changes. Publishing a PR does not authorize merging it.

## Moving a city to a new gate model

An older model pin in any other layer the run uses can override a new default. Check the gate helper, its callers, the worker prompt and the provider command.
For example, an installed helper whose `model=` line still names the previous model passes that ID in both `-m` and `review_model`, overriding the city profile's new default.

1. Set `model` in `city.config.toml`. If the CLI does not yet bundle the model, set the catalog key as described above.
2. Search the installed gate helper, every script that calls it and the worker prompt template `gascity/template-fragments/gc-role-worker.template.md` for `model=`, `--model` and the previous ID. The template's review command passes `--model` explicitly.
   Update each pin, then check the review command in a freshly rendered worker prompt (`gc prime <lane>`).
3. In `city.toml`, check the `codex-astra` provider's model default and any `-m` or `--model` flags in its `command` and `resume_command`.
4. Run the installed helper with its callers' usual flags, adding `--dry-run` to print the Codex command without running it. Check for the new ID after `-m` and in `review_model=`.
   For a helper without `--dry-run`, use the executable `codex` written by the `gate` fixture in `gascity/tests/test_codex_gate.py`. Set `STUB_DIR` to its directory and `NEW_MODEL` to the intended ID. Run the following in Bash.

   ```bash
   gate_probe() (
     : "${STUB_DIR:?}" "${NEW_MODEL:?}"; PATH="$STUB_DIR:$PATH"
     if [[ ! -f $STUB_DIR/codex || ! -x $STUB_DIR/codex || $(type -t codex) != file || ! $(command -v codex) -ef $STUB_DIR/codex ]]; then
       printf "gate_probe: codex resolves to '%s', expected executable stub %s/codex\n" "$(command -v codex)" "$STUB_DIR" >&2; exit 2
     fi
     CAPTURE=$(mktemp "${TMPDIR:-/tmp}/gate-probe.XXXXXX") && out=$(mktemp "${TMPDIR:-/tmp}/gate-stub.XXXXXX") || exit 2
     export CAPTURE; args=("$@")
     for ((i=1; i+1<${#args[@]}; i++)); do if [[ ${args[i]} == --output ]]; then args[++i]=$out; fi; done
     rc=0; "${args[@]}" || rc=$?; printf "gate_probe: helper exit %s; the stub's record is %s\n" "$rc" "$CAPTURE"
     python3 -I -c 'import json, pathlib, sys
   record = pathlib.Path(sys.argv[1]).read_text()
   args = json.loads(record)["args"] if record else []
   models = [value for flag, value in zip(args, args[1:]) if flag == "-m"]
   reviews = [json.loads(arg.split("=", 1)[1]) for arg in args if arg.startswith("review_model=")]
   status = "NO CALL RECORDED" if not args else "OK" if models == reviews == [sys.argv[2]] else "STALE"
   print("gate_probe: -m %s, review_model %s: %s" % (",".join(models) or "none", ",".join(reviews) or "none", status)); sys.exit(status != "OK")' "$CAPTURE" "$NEW_MODEL"
   )
   gate_probe codex-gate.sh exec "$PROMPT_FILE" -C "$REPO" --output "$REVIEW_OUTPUT"
   ```
   The last line probes the helper default. Prefix each caller’s own command with `gate_probe`, including the freshly rendered worker command and each calling script’s helper line. Review needs a clean `REPO_DIR` with a commit after `REVIEW_BASE`. The lookup check prevents a fallback to the real CLI. Only the output destination changes in the caller’s arguments because the helper empties `--output` and its `.log` before calling Codex. A model mismatch reports `STALE`. If the helper refuses a review without `--class` or `--bead`, the probe reports `NO CALL RECORDED` and fails. Calling Codex through its home alone checks only the profile.

## Local ego-browser override and provider handoff

The ego-browser description is now `Use when a task needs a real browser session
on citadel`: 977 → 55 characters. `~/.agents/skills/ego-browser` is a local
directory with the edited SKILL.md and links to the app's other resources.
The body and ego app's source file are unchanged. The original directory symlink
was saved at `~/.agents/ego-browser.bak-20260906-185347`, outside skill discovery.
Claude-side skills, including superpowers, were not modified.

The mayor owns `city.toml` and sets the `codex-astra` provider's model default there when the city switches to GPT-6.2 Astra.
Its command and resume command still need `-p city` to select the new profile.
The plain `codex` provider also needs the explicit GPT-6.2 Astra model and city profile
if it is used for city work. No city.toml change is included in this task.
